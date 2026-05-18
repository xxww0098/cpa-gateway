package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/xxww0098/cpa-gateway/config"
	"github.com/xxww0098/cpa-gateway/ledger"
	"github.com/xxww0098/cpa-gateway/model"
	"github.com/xxww0098/cpa-gateway/testutil"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// newPurchaseDebtBlockRouter builds a PanelRouter backed by an in-memory
// sqlite DB and a real *ledger.Ledger wired to miniredis. The router is
// intentionally constructed without the production AuthMiddleware so the
// test can seed BillingCtx directly via a shim middleware — the contract
// under test (Property 8 / Requirement 2.5) is purely about the handler's
// preflight behavior, not the auth layer.
//
// Migrates only the tables the purchase handler actually touches:
// User / Subscription / SubscriptionPackage / BalanceLog. Keeping the
// migration scope narrow avoids dragging unrelated GORM models into this
// focused example test.
func newPurchaseDebtBlockRouter(t *testing.T) (*PanelRouter, *gorm.DB) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.User{},
		&model.Subscription{},
		&model.SubscriptionPackage{},
		&model.BalanceLog{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}

	rdb, _ := testutil.MustMiniRedis(t)
	ldg := ledger.New(db, rdb)

	cfg := &config.Config{}
	pr := NewPanelRouter(db, rdb, ldg, nil, cfg)
	return pr, db
}

// seedBillingCtxMiddleware returns a gin middleware that injects a
// BillingCtx into both the gin context and the request context, exactly
// as AuthMiddleware would after successfully validating a JWT or API key.
// This lets the purchase handler's requireBillingCtx check succeed without
// running the real auth path, which is out of scope for this test.
func seedBillingCtxMiddleware(userID uint) gin.HandlerFunc {
	return func(c *gin.Context) {
		bc := &BillingCtx{
			UserID:    userID,
			RateMult:  1.0,
			AuthType:  authTypeJWT,
			Status:    userStatusActive,
			RequestID: "test-request",
		}
		setBillingContext(c, bc)
		c.Next()
	}
}

// insertShortfallSettleRow inserts a settle-type BalanceLog row whose
// Metadata carries a positive shortfall_usd. The row is unresolved because
// no matching "shortfall_resolve:<ref>:<id>" credit is written. This
// mirrors the shape that the partial-debit branch of ledger.Settle would
// have produced on a real under-funded settle.
func insertShortfallSettleRow(t *testing.T, db *gorm.DB, userID uint, ref string, shortfall float64) {
	t.Helper()

	metadata, err := json.Marshal(map[string]interface{}{
		"user_id":       userID,
		"shortfall_usd": shortfall,
		"actual_cost":   shortfall,
	})
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}

	row := model.BalanceLog{
		UserID:    userID,
		Amount:    0,
		Type:      "settle",
		Reference: ref,
		Metadata:  metadata,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("insert shortfall settle row: %v", err)
	}
}

// TestPurchaseBlockedOnShortfall exercises task 11.4's acceptance case:
// a user with positive balance and a seeded SubscriptionPackage must
// still be blocked from purchasing while an unresolved shortfall row
// exists on their BalanceLog. The handler's preflight
// (pr.Ledger.HasUnresolvedShortfall) must short-circuit BEFORE any
// ledger.Debit / ledger.Credit / Subscription insert runs, returning
// HTTP 402 with body {"error":"outstanding_debt"}.
//
// Assertions (all required by Property 8 / Requirement 2.5):
//  1. HTTP 402 and body {"error":"outstanding_debt"}.
//  2. No BalanceLog row of type "debit" created for this request.
//  3. No BalanceLog row of type "credit" created for this request.
//     (The only balance_logs row is the seeded shortfall settle row.)
//  4. No Subscription row created.
//  5. User's balance unchanged.
//
// **Validates: Property 8, Requirement 2.5**
func TestPurchaseBlockedOnShortfall(t *testing.T) {
	const (
		userID          = uint(42)
		startingBalance = 500.0
		shortfallUSD    = 0.5
	)

	pr, db := newPurchaseDebtBlockRouter(t)

	// Seed user with a positive balance so the block is NOT attributable
	// to insufficient funds — it must be the preflight that refuses.
	user := model.User{
		ID:           userID,
		Email:        "debtor@test.local",
		PasswordHash: "hash",
		Balance:      startingBalance,
		Status:       userStatusActive,
	}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// Seed an unresolved shortfall row. The reference "req-x" and the
	// positive shortfall_usd make HasUnresolvedShortfall report true until
	// a matching "shortfall_resolve:req-x:<id>" credit is written.
	insertShortfallSettleRow(t, db, userID, "req-x", shortfallUSD)

	// Sanity check: the ledger agrees the user is blocked before we even
	// hit the handler. Failing this guard means the seed shape drifted
	// from the Settle partial-debit convention.
	blocked, err := pr.Ledger.HasUnresolvedShortfall(context.Background(), userID)
	if err != nil {
		t.Fatalf("HasUnresolvedShortfall sanity: %v", err)
	}
	if !blocked {
		t.Fatalf("HasUnresolvedShortfall sanity: got false, want true")
	}

	// Seed a SubscriptionPackage the test will target. GroupID is the
	// public identifier the handler accepts in the POST body, so we pin
	// it to a value distinct from the primary key to catch accidental
	// by-ID lookups.
	pkg := model.SubscriptionPackage{
		Name:                 "Pro",
		GroupID:              7,
		RateMultiplier:       0.95,
		DefaultValidityDays:  30,
		SubscriptionPriceUSD: 29.9,
		Enabled:              true,
	}
	if err := db.Create(&pkg).Error; err != nil {
		t.Fatalf("seed package: %v", err)
	}

	// Build the gin engine with the seed middleware followed by the real
	// purchase handler. We register the route at the same path the
	// production router exposes so the test doubles as a smoke check on
	// the URL contract.
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(seedBillingCtxMiddleware(userID))
	r.POST("/api/panel/user/subscriptions/purchase", pr.PurchaseSubscriptionHandler)

	body, err := json.Marshal(map[string]any{"group_id": pkg.GroupID})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost,
		"/api/panel/user/subscriptions/purchase",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	// Assertion 1: 402 outstanding_debt.
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status: got %d want %d; body=%s", w.Code, http.StatusPaymentRequired, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal body: %v (raw=%s)", err, w.Body.String())
	}
	if got, want := resp["error"], "outstanding_debt"; got != want {
		t.Fatalf("error: got %q want %q", got, want)
	}

	// Assertion 2 & 3: no debit or credit BalanceLog rows for this user.
	// The only row should be the seeded shortfall-settle row.
	var debitCount int64
	if err := db.Model(&model.BalanceLog{}).
		Where("user_id = ? AND type = ?", userID, "debit").
		Count(&debitCount).Error; err != nil {
		t.Fatalf("count debit rows: %v", err)
	}
	if debitCount != 0 {
		t.Fatalf("unexpected debit rows: got %d want 0", debitCount)
	}

	var creditCount int64
	if err := db.Model(&model.BalanceLog{}).
		Where("user_id = ? AND type = ?", userID, "credit").
		Count(&creditCount).Error; err != nil {
		t.Fatalf("count credit rows: %v", err)
	}
	if creditCount != 0 {
		t.Fatalf("unexpected credit rows: got %d want 0", creditCount)
	}

	// Assertion 4: no Subscription row created.
	var subCount int64
	if err := db.Model(&model.Subscription{}).
		Where("user_id = ?", userID).
		Count(&subCount).Error; err != nil {
		t.Fatalf("count subscription rows: %v", err)
	}
	if subCount != 0 {
		t.Fatalf("unexpected subscription rows: got %d want 0", subCount)
	}

	// Assertion 5: balance unchanged. The preflight must not mutate the
	// user's persistent balance.
	var after model.User
	if err := db.First(&after, userID).Error; err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if after.Balance != startingBalance {
		t.Fatalf("balance: got %v want %v", after.Balance, startingBalance)
	}
}
