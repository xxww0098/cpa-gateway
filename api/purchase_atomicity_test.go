package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/xxww0098/cpa-gateway/config"
	"github.com/xxww0098/cpa-gateway/ledger"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"pgregory.net/rapid"
)

// purchaseAtomicityTB is the subset of *testing.T / *rapid.T that the
// per-iteration setup helpers need. Both concrete types satisfy it, and it
// also conforms to miniredis.Tester so the test can spin miniredis per
// rapid iteration without a stand-alone *testing.T.
type purchaseAtomicityTB interface {
	Helper()
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
	Cleanup(func())
}

// newPurchaseAtomicityRouter builds a PanelRouter with a real *ledger.Ledger
// (sqlite + miniredis) and a Subscription-create callback that fails when
// failFlag.Load() is true. This is the "mock ledger + gorm hook" seam
// called out in tasks.md §11.2: the ledger itself is real (so Debit/Credit
// invariants are enforced by production code), while the Subscription
// INSERT is selectively failed through a Before("gorm:create") callback
// keyed on Statement.Schema.Name == "Subscription".
//
// The callback is registered with a stable name per iteration; the test
// cleanup removes it before the next iteration so the hook never leaks
// between rapid invocations.
func newPurchaseAtomicityRouter(tb purchaseAtomicityTB, failFlag *atomic.Bool) (*PanelRouter, *gorm.DB) {
	tb.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		tb.Fatalf("open sqlite: %v", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		// One connection keeps the in-memory DB visible to every goroutine
		// the handler spawns (the ledger's tx + the handler's Create).
		sqlDB.SetMaxOpenConns(1)
		tb.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := db.AutoMigrate(
		&model.User{},
		&model.Subscription{},
		&model.SubscriptionPackage{},
		&model.BalanceLog{},
	); err != nil {
		tb.Fatalf("automigrate: %v", err)
	}

	const callbackName = "test:fail_subscription_create"
	cb := func(d *gorm.DB) {
		if !failFlag.Load() {
			return
		}
		if d.Statement.Schema != nil && d.Statement.Schema.Name == "Subscription" {
			_ = d.AddError(errors.New("injected subscription create failure"))
		}
	}
	if err := db.Callback().Create().Before("gorm:create").Register(callbackName, cb); err != nil {
		tb.Fatalf("register create callback: %v", err)
	}
	tb.Cleanup(func() {
		_ = db.Callback().Create().Remove(callbackName)
	})

	// Use a miniredis-backed ledger so the Debit/Credit path exercises the
	// real production ledger (the handler's contract is about THAT
	// ledger's BalanceLog rows, not an interface mock). The miniredis
	// instance is scoped to this iteration via RunT, which accepts any
	// miniredis.Tester — our purchaseAtomicityTB alias satisfies that
	// contract so a rapid.T can drive the spin-up directly without
	// needing a top-level *testing.T.
	srv := miniredis.RunT(tb)
	rClient := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	tb.Cleanup(func() { _ = rClient.Close() })
	ldg := ledger.New(db, rClient)

	cfg := &config.Config{}
	pr := NewPanelRouter(db, rClient, ldg, nil, cfg)
	return pr, db
}

// seedPurchaseAtomicityWorld installs one User (with a generous starting
// balance so Debit always succeeds when the test allows it) and one
// SubscriptionPackage priced at `price`. Returns the package so the caller
// knows the GroupID to POST and the package ID used in the ref prefix.
func seedPurchaseAtomicityWorld(tb purchaseAtomicityTB, db *gorm.DB, userID uint, price float64) model.SubscriptionPackage {
	tb.Helper()

	user := model.User{
		ID:           userID,
		Email:        fmt.Sprintf("purchase-atomic-%d@test.local", userID),
		PasswordHash: "hash",
		// Balance is always >= 2x price so Debit never surfaces
		// ErrInsufficientBalance inside the property; the insufficient-
		// balance contract is exercised by purchase_insufficient_test.go
		// and is out of scope for this atomicity test.
		Balance: price*2 + 10,
		Status:  userStatusActive,
	}
	if err := db.Create(&user).Error; err != nil {
		tb.Fatalf("seed user: %v", err)
	}

	pkg := model.SubscriptionPackage{
		Name:                 "PropertyTestPkg",
		GroupID:              7,
		RateMultiplier:       0.9,
		DefaultValidityDays:  30,
		SubscriptionPriceUSD: price,
		Enabled:              true,
	}
	if err := db.Create(&pkg).Error; err != nil {
		tb.Fatalf("seed package: %v", err)
	}
	return pkg
}

// runPurchase invokes PurchaseSubscriptionHandler through a test gin engine
// seeded with BillingCtx for userID. Mirrors the shim pattern used by
// purchase_debt_block_test.go and apikey_rebind_entitlement_test.go.
func runPurchase(tb purchaseAtomicityTB, pr *PanelRouter, userID, groupID uint) *httptest.ResponseRecorder {
	tb.Helper()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(seedBillingCtxMiddleware(userID))
	r.POST("/api/panel/user/subscriptions/purchase", pr.PurchaseSubscriptionHandler)

	body, err := json.Marshal(map[string]any{"group_id": groupID})
	if err != nil {
		tb.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost,
		"/api/panel/user/subscriptions/purchase",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestPurchaseConservation is the rapid-driven conservation property for
// task 11.2. For every (price, inject_create_failure) draw it runs a full
// purchase against a real ledger + sqlite, then asserts the atomicity
// invariant called out in Requirement 5.4:
//
//	Σ debit_amount_for_ref_prefix - Σ credit_amount_for_ref_prefix ==
//	  (price if a Subscription row exists else 0)
//
// and, on the rollback branch, asserts that the compensating credit row
// carries the documented reference prefix (Requirement 5.3):
//
//	Reference == "subscription_purchase:<pkgID>:compensate:<debitRef>"
//
// with amount == price.
//
// The amount convention in the ledger's BalanceLog: Debit writes
// Amount = -price (money leaving), Credit writes Amount = +price. We
// translate back to "debited dollars" by negating the debit row's Amount
// before summing so the invariant reads naturally.
//
// **Validates: Property 13, Requirements 5.1, 5.2, 5.3, 5.4, 5.6**
func TestPurchaseConservation(t *testing.T) {
	raiseRapidChecksAdminPricing(t, 200)

	var iter atomic.Int64

	rapid.Check(t, func(rt *rapid.T) {
		// Per-iteration unique userID keeps the seed helpers side-effect
		// free and lets any leaked rows be attributed back to the draw.
		userID := uint(iter.Add(1))

		// Draws: price (strictly positive, bounded so the BigDecimal
		// balance stays finite) and whether this iteration should inject
		// a Subscription-create failure.
		price := rapid.Float64Range(0.01, 100.0).Draw(rt, "price")
		injectFailure := rapid.Bool().Draw(rt, "injectSubscriptionCreateFailure")

		var failFlag atomic.Bool
		failFlag.Store(injectFailure)

		pr, db := newPurchaseAtomicityRouter(rt, &failFlag)
		pkg := seedPurchaseAtomicityWorld(rt, db, userID, price)

		w := runPurchase(rt, pr, userID, pkg.GroupID)

		// Fetch every BalanceLog row whose Reference starts with the
		// package-qualified prefix. Requirement 5.3 guarantees BOTH the
		// debit and the compensating credit carry this prefix, so the
		// single query captures the entire conservation basket for the
		// request.
		prefix := fmt.Sprintf("subscription_purchase:%d:", pkg.ID)
		var rows []model.BalanceLog
		if err := db.Where("user_id = ? AND reference LIKE ?", userID, prefix+"%").
			Find(&rows).Error; err != nil {
			rt.Fatalf("list balance logs: %v", err)
		}

		// Compute the debited-minus-credited dollars for the request.
		// Debit rows store Amount as -price (money out), so the
		// contribution to "debited dollars" is -Amount. Credit rows
		// store Amount as +price, so they subtract as +Amount.
		var debited, credited float64
		var debitRef, compRef string
		var debitAmount, creditAmount float64
		for _, row := range rows {
			switch row.Type {
			case "debit":
				debited += -row.Amount
				debitRef = row.Reference
				debitAmount = -row.Amount
			case "credit":
				credited += row.Amount
				compRef = row.Reference
				creditAmount = row.Amount
			}
		}
		net := debited - credited

		// Does a Subscription row exist for this user/package?
		var subCount int64
		if err := db.Model(&model.Subscription{}).
			Where("user_id = ? AND package_id = ?", userID, pkg.ID).
			Count(&subCount).Error; err != nil {
			rt.Fatalf("count subs: %v", err)
		}
		hasSub := subCount > 0

		// Invariant: net ledger movement equals the subscribed price iff
		// the Subscription row exists.
		expected := 0.0
		if hasSub {
			expected = price
		}
		if diff := net - expected; diff < -1e-9 || diff > 1e-9 {
			rt.Fatalf("conservation: Σ debit - Σ credit = %v, want %v (hasSub=%v, price=%v, rows=%+v, status=%d, body=%s)",
				net, expected, hasSub, price, rows, w.Code, w.Body.String())
		}

		if injectFailure {
			// Rollback branch assertions.
			// 1. Response must surface as 500 with the documented body,
			//    so the caller knows the purchase did NOT persist.
			if w.Code != http.StatusInternalServerError {
				rt.Fatalf("status: got %d want 500 (inject=true, price=%v); body=%s",
					w.Code, price, w.Body.String())
			}
			// 2. The debit row MUST have been written (it fires before
			//    Subscription Create) and its amount must equal the price.
			if debitRef == "" {
				rt.Fatalf("expected debit row on rollback branch; rows=%+v", rows)
			}
			if diff := debitAmount - price; diff < -1e-9 || diff > 1e-9 {
				rt.Fatalf("debit amount: got %v want %v", debitAmount, price)
			}
			// 3. The compensating credit must reference the precise
			//    debitRef per Requirement 5.3's "compensate:<debitRef>"
			//    convention, and its amount must equal the price so
			//    conservation gives net 0.
			wantCompRef := fmt.Sprintf("subscription_purchase:%d:compensate:%s", pkg.ID, debitRef)
			if compRef != wantCompRef {
				rt.Fatalf("compensate reference: got %q want %q (rows=%+v)",
					compRef, wantCompRef, rows)
			}
			if diff := creditAmount - price; diff < -1e-9 || diff > 1e-9 {
				rt.Fatalf("compensate amount: got %v want %v", creditAmount, price)
			}
			// 4. No Subscription row should exist.
			if hasSub {
				rt.Fatalf("subscription row should NOT exist on rollback branch")
			}
		} else {
			// Happy path: 200, debit present, no compensating credit,
			// subscription row persisted.
			if w.Code != http.StatusOK {
				rt.Fatalf("status: got %d want 200 (inject=false, price=%v); body=%s",
					w.Code, price, w.Body.String())
			}
			if !hasSub {
				rt.Fatalf("subscription row should exist on happy path")
			}
			if compRef != "" {
				rt.Fatalf("unexpected compensating credit on happy path: %q (amount=%v)",
					compRef, creditAmount)
			}
		}
	})
}

// TestCompensationLogEmitted pins the structured-log contract called out in
// Requirement 5.6: when Subscription creation fails after a successful
// Debit, the handler MUST emit a warn-level slog entry with
// msg=subscription_create_failed carrying the ref field (and the matching
// event attribute). This example test captures the log stream through a
// JSON handler attached to slog.SetDefault and asserts on the decoded
// record.
//
// Non-goals: we do NOT assert on the compErr path here — that is covered by
// the subscription_compensation_failed Error log, which only fires when
// both the Subscription create AND the compensating credit fail. The
// conservation property already exercises the successful compensation
// branch.
//
// **Validates: Property 13, Requirements 5.6**
func TestCompensationLogEmitted(t *testing.T) {
	const (
		userID uint    = 9999
		price  float64 = 12.34
	)

	var failFlag atomic.Bool
	failFlag.Store(true)

	pr, db := newPurchaseAtomicityRouter(t, &failFlag)
	pkg := seedPurchaseAtomicityWorld(t, db, userID, price)

	// Redirect slog to a JSON handler over a bytes.Buffer for the
	// duration of the test. The JSON envelope gives us a structured
	// payload we can decode and assert on without scraping free-form
	// text. We restore the previous default on cleanup so concurrent or
	// follow-up tests in this file are unaffected.
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w := runPurchase(t, pr, userID, pkg.GroupID)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500; body=%s", w.Code, w.Body.String())
	}

	// Scan the JSON log stream for the subscription_create_failed record.
	// Each slog JSON line is a complete JSON object; split on newlines
	// and decode. We expect exactly one matching record per invocation.
	found := false
	var record map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		// slog's JSONHandler emits the top-level `msg` key for the
		// event message; the explicit `event` attribute is emitted
		// alongside it per the handler call.
		if msg, _ := m["msg"].(string); msg == "subscription_create_failed" {
			found = true
			record = m
			break
		}
	}
	if !found {
		t.Fatalf("expected slog record with msg=subscription_create_failed; got stream:\n%s", buf.String())
	}

	// The handler passes the event attribute explicitly so downstream
	// log pipelines that key off `event` (rather than `msg`) still see
	// the right value.
	if evt, _ := record["event"].(string); evt != "subscription_create_failed" {
		t.Fatalf("event attr: got %v want subscription_create_failed (record=%v)", evt, record)
	}

	// The ref field must be present and must match the documented
	// "subscription_purchase:<pkgID>:<nonce>" prefix so operators can
	// pair it with the compensating credit row later. A blank / missing
	// ref breaks the pairing contract.
	ref, _ := record["ref"].(string)
	wantPrefix := fmt.Sprintf("subscription_purchase:%d:", pkg.ID)
	if !strings.HasPrefix(ref, wantPrefix) {
		t.Fatalf("ref: got %q want prefix %q (record=%v)", ref, wantPrefix, record)
	}

	// user_id and package_id must also be present — the handler logs
	// them explicitly so ops can scope alerts per tenant/plan.
	if uid, _ := record["user_id"].(float64); uint(uid) != userID {
		t.Fatalf("user_id: got %v want %d", record["user_id"], userID)
	}
	if pid, _ := record["package_id"].(float64); uint(pid) != pkg.ID {
		t.Fatalf("package_id: got %v want %d", record["package_id"], pkg.ID)
	}
}
