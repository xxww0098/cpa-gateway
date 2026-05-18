package sdk

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/xxww0098/cpa-gateway/executor"
	"github.com/xxww0098/cpa-gateway/ledger"
	"github.com/xxww0098/cpa-gateway/model"
	"github.com/xxww0098/cpa-gateway/testutil"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"pgregory.net/rapid"
)

// Feature: billing-security-hardening, Property 6 — UsageLog shortfall
// annotation mirrors the BalanceLog shortfall row.
//
// TestUsageLogShortfallAnnotation is the property-based regression for
// Requirement 2.4: when UsagePlugin.HandleUsage drives Settle on a
// successful upstream response whose computed actualCost exceeds the
// user's persistent balance, the ledger's partial-debit branch writes
// a settle BalanceLog row with Metadata.shortfall_usd > 0, and the
// plugin MUST copy that exact shortfall amount into the paired
// UsageLog.RawMetadata.shortfall_usd so downstream reporting /
// alerting can distinguish "free request" from "partially paid
// request" without joining two tables.
//
// The property under test:
//
//	For all (initial_balance, actualCost) with balance > 0 AND
//	actualCost > balance, after HandleUsage returns:
//	  - exactly one UsageLog row exists for the request,
//	  - exactly one BalanceLog{type=settle} row with shortfall_usd > 0
//	    exists for the request,
//	  - UsageLog.RawMetadata.shortfall_usd == BalanceLog.Metadata.shortfall_usd
//	    (within a small float epsilon to tolerate the JSON encode / decode
//	    round-trip through the sqlite driver).
//
// Inputs use rapid.Float64Range over the task-prescribed low-balance
// region (0.001, 0.5) so every iteration is guaranteed to hit the
// partial-debit branch of ledger.Settle. actualCost is drawn from a
// strictly-greater range so shortfall is always positive; the separation
// also keeps the shrink target (balance=0.001, actualCost=0.5) close to
// the "minimum observable shortfall" case.
//
// The test uses the real *ledger.Ledger + miniredis + sqlite triple
// (the same triple used by holdmw_preflight_test.go), not a mock: the
// property covers the contract between the ledger's shortfall row
// format and the plugin's UsageLog annotation, which only the real
// ledger emits. A mock ledger would hide a regression in the metadata
// key ("shortfall_usd"), the row type ("settle"), or the ordering
// constraint that the shortfall row must be visible to the plugin's
// in-tx lookupShortfall call BEFORE the UsageLog INSERT.
//
// The flag.Lookup("rapid.checks") raise mirrors the helper in
// holdmw_preflight_test.go — task 5.6 requires ≥ 200 iterations, so we
// pin a floor even when the caller invokes go test with a smaller
// default.
//
// **Validates: Property 6 (UsageLog annotation), Requirement 2.4**
func TestUsageLogShortfallAnnotation(t *testing.T) {
	raiseRapidChecksShortfall(t, 200)

	// Shared iteration counter so each rapid draw gets a unique user
	// email + request ID even across rapid's internal retries. Using an
	// int64 keeps the counter safe under concurrent rapid workers (rapid
	// does not parallelise by default, but being defensive here costs
	// nothing).
	var iter int64
	var iterMu sync.Mutex
	nextIter := func() int64 {
		iterMu.Lock()
		defer iterMu.Unlock()
		iter++
		return iter
	}

	rapid.Check(t, func(rt *rapid.T) {
		i := nextIter()

		// Task-prescribed generators. Balance < actualCost is guaranteed
		// by construction: balance ∈ (0.001, 0.5] and actualCost ∈
		// (0.5, 10]. The overlap at 0.5 is excluded by strict-greater
		// sampling — rapid.Float64Range is inclusive on both ends, so we
		// use (0.5001, …) for actualCost to keep the partial-debit
		// branch the only reachable code path.
		balance := rapid.Float64Range(0.001, 0.5).Draw(rt, "balance")
		actualCost := rapid.Float64Range(0.5001, 10.0).Draw(rt, "actual_cost")

		// Hold amount is small enough to pass the Hold Lua script's
		// admission check (needs holdAmount <= cachedBalance). Settle's
		// partial-debit path reads user.Balance from PG directly, so the
		// hold value does not influence the shortfall calculation — we
		// just need a valid hold so ZRem has a member to remove.
		holdAmount := balance * 0.1
		if holdAmount < 0.0001 {
			holdAmount = 0.0001
		}

		requestID := fmt.Sprintf("req-shortfall-log-%d", i)
		userID := uint(1) // single-user fresh DB per iteration

		env := newShortfallTestEnv(t, userID, balance, i)

		ctx := context.Background()
		if err := env.ldg.Hold(ctx, userID, holdAmount, requestID, 5*time.Minute); err != nil {
			rt.Fatalf("Hold failed (balance=%v holdAmount=%v): %v",
				balance, holdAmount, err)
		}

		// fakeCalculator (defined in usage_test.go) returns its
		// configured computeCost from Compute, which is exactly what
		// HandleUsage uses as actualCost on the precise-settle branch.
		calc := &fakeCalculator{computeCost: actualCost}
		plugin := NewUsagePlugin(env.db, env.ldg, calc)

		sc := &SettleCtx{
			RequestID: requestID,
			UserID:    userID,
			RateMult:  1.0,
			Model:     "gpt-4o",
			ApiKeyID:  7,
			IPAddress: "10.0.0.1",
		}
		// WithUsageDetailPresent(ctx, true) selects the precise-settle
		// branch (Requirement 2.4 is scoped to precise settles where
		// the ledger records shortfall_usd; the fallback branch has its
		// own separate annotation assertion in
		// usage_fallback_settle_test.go).
		ctx = executor.WithUsageDetailPresent(WithSettleCtx(ctx, sc), true)

		rec := cliproxyusage.Record{
			Provider: "openai",
			Model:    "gpt-4o",
			Failed:   false,
			Latency:  50 * time.Millisecond,
			Detail: cliproxyusage.Detail{
				InputTokens:  100,
				OutputTokens: 200,
			},
		}

		plugin.HandleUsage(ctx, rec)

		// ---- Invariant 1: exactly one settle BalanceLog row carries
		// a positive shortfall_usd for this request. Ledger.Settle's
		// partial-debit branch writes two rows (the debit + the
		// zero-amount shortfall marker); we locate the marker row by
		// probing Metadata.shortfall_usd. ------------------------------
		var settleRows []model.BalanceLog
		if err := env.db.
			Where("user_id = ? AND reference = ? AND type = ?",
				userID, requestID, "settle").
			Find(&settleRows).Error; err != nil {
			rt.Fatalf("query settle BalanceLog rows: %v", err)
		}
		if len(settleRows) == 0 {
			rt.Fatalf("no settle BalanceLog rows for request=%s (balance=%v actual=%v)",
				requestID, balance, actualCost)
		}

		var balanceShortfall float64
		shortfallRowCount := 0
		for _, row := range settleRows {
			if len(row.Metadata) == 0 {
				continue
			}
			var meta map[string]interface{}
			if err := json.Unmarshal(row.Metadata, &meta); err != nil {
				rt.Fatalf("parse BalanceLog.Metadata id=%d: %v", row.ID, err)
			}
			v, ok := meta["shortfall_usd"].(float64)
			if !ok {
				continue
			}
			if v > 0 {
				balanceShortfall = v
				shortfallRowCount++
			}
		}
		if shortfallRowCount == 0 {
			rt.Fatalf("no BalanceLog{type=settle} row with shortfall_usd > 0 for request=%s (balance=%v actual=%v rows=%d)",
				requestID, balance, actualCost, len(settleRows))
		}
		if shortfallRowCount > 1 {
			rt.Fatalf("expected exactly 1 shortfall BalanceLog row, got %d (request=%s)",
				shortfallRowCount, requestID)
		}

		// ---- Invariant 2: exactly one UsageLog row exists for the
		// request and its RawMetadata.shortfall_usd equals the
		// BalanceLog shortfall. ----------------------------------------
		var usageLogs []model.UsageLog
		if err := env.db.Where("request_id = ?", requestID).
			Find(&usageLogs).Error; err != nil {
			rt.Fatalf("query UsageLog: %v", err)
		}
		if len(usageLogs) != 1 {
			rt.Fatalf("expected exactly 1 UsageLog row for request=%s, got %d",
				requestID, len(usageLogs))
		}
		ulog := usageLogs[0]
		if ulog.Failed {
			rt.Fatalf("UsageLog.Failed = true for successful partial-settle (request=%s)",
				requestID)
		}
		if len(ulog.RawMetadata) == 0 {
			rt.Fatalf("UsageLog.RawMetadata empty, want shortfall_usd annotation (request=%s balance=%v actual=%v)",
				requestID, balance, actualCost)
		}
		var rawMeta map[string]interface{}
		if err := json.Unmarshal(ulog.RawMetadata, &rawMeta); err != nil {
			rt.Fatalf("parse UsageLog.RawMetadata: %v (raw=%s)",
				err, string(ulog.RawMetadata))
		}
		rawShortfall, ok := rawMeta["shortfall_usd"].(float64)
		if !ok {
			rt.Fatalf("UsageLog.RawMetadata.shortfall_usd missing or wrong type: %v (raw=%s)",
				rawMeta["shortfall_usd"], string(ulog.RawMetadata))
		}

		// ---- Invariant 3: the two shortfall values agree. The JSON
		// round-trip through sqlite preserves float64 bit-for-bit in
		// practice (encoding/json uses strconv.FormatFloat with -1
		// precision), but we allow a tight epsilon so a future driver
		// change that quantises differently still passes if the values
		// agree to sub-cent precision. ---------------------------------
		diff := rawShortfall - balanceShortfall
		if diff < 0 {
			diff = -diff
		}
		if diff > 1e-9 {
			rt.Fatalf("UsageLog.RawMetadata.shortfall_usd = %.12f, BalanceLog.Metadata.shortfall_usd = %.12f (diff=%.12g request=%s balance=%v actual=%v)",
				rawShortfall, balanceShortfall, rawShortfall-balanceShortfall,
				requestID, balance, actualCost)
		}
	})
}

// -----------------------------------------------------------------------------
// test infrastructure
// -----------------------------------------------------------------------------

// shortfallTestEnv bundles the real (miniredis + sqlite + ledger)
// triple the property drives per iteration. A fresh env per iteration
// keeps rapid's shrinker focused on the (balance, actualCost) generator
// state rather than leaking BalanceLog rows across attempts.
type shortfallTestEnv struct {
	db  *gorm.DB
	ldg *ledger.Ledger
}

// newShortfallTestEnv spins up a fresh sqlite in-memory DB and a
// miniredis-backed *ledger.Ledger seeded with a single user whose
// persistent balance equals `balance`. Iteration index `i` is mixed
// into the DSN so concurrent rapid workers (if any) never collide on
// the shared-cache SQLite instance.
//
// SQLite configuration notes:
//   - cache=shared lets the ledger's goroutine (Settle runs on a
//     detached bgCtx inside HandleUsage) see the same in-memory
//     database the test goroutine wrote to.
//   - _journal_mode=WAL avoids the "database is locked" error that
//     SQLite emits when the outer UsagePlugin transaction is still
//     open while ledger.Settle's inner transaction opens another
//     connection on the partial-debit path.
//   - SetMaxOpenConns(2) matches the pattern used by
//     TestProperty14_BillingOperationAuditCompleteness_SettleFailed in
//     usage_prop_test.go (which exercises the same nested-tx shape).
func newShortfallTestEnv(t testing.TB, userID uint, balance float64, iterID int64) *shortfallTestEnv {
	t.Helper()

	dsn := fmt.Sprintf("file:usage_shortfall_log_%d?mode=memory&cache=shared&_journal_mode=WAL", iterID)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("raw db: %v", err)
	}
	sqlDB.SetMaxOpenConns(2)

	if err := db.AutoMigrate(
		&model.User{},
		&model.UsageLog{},
		&model.BalanceLog{},
		&model.Subscription{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}

	u := &model.User{
		ID:           userID,
		Email:        fmt.Sprintf("shortfall-log-%d@test.local", iterID),
		PasswordHash: "x",
		Role:         "user",
		Status:       "active",
		Balance:      balance,
	}
	if err := db.Create(u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	// testutil.MustMiniRedis returns a live miniredis server plus a
	// redis.Client wired to it. The server lifetime is bound to the
	// outer *testing.T via t.Cleanup, so each rapid iteration's env is
	// torn down automatically when the test function returns.
	//
	// The *testing.T assertion is safe — rapid drives this test with a
	// *rapid.T which does NOT satisfy *testing.T. We therefore accept a
	// testing.TB here and demand the caller supply the outer *testing.T;
	// the only caller is TestUsageLogShortfallAnnotation, which passes
	// its t directly.
	outerT, ok := t.(*testing.T)
	if !ok {
		t.Fatalf("newShortfallTestEnv requires *testing.T, got %T", t)
	}
	redisClient, _ := testutil.MustMiniRedis(outerT)

	ldg := ledger.NewWithConfig(db, redisClient, 30*time.Second, 5*time.Minute)

	return &shortfallTestEnv{db: db, ldg: ldg}
}

// raiseRapidChecksShortfall raises the -rapid.checks flag to the
// requested minimum for the duration of the current test and restores
// the original on cleanup. Task 5.6 mandates ≥ 200 iterations, so we
// pin a floor whenever the caller invokes go test with a smaller
// default.
//
// Named with a "Shortfall" suffix to avoid colliding with the
// equivalent helper in holdmw_preflight_test.go — both live in package
// sdk and would otherwise trigger a redeclaration compile error.
func raiseRapidChecksShortfall(t *testing.T, minChecks int) {
	t.Helper()
	fl := flag.Lookup("rapid.checks")
	if fl == nil {
		return
	}
	orig := fl.Value.String()
	cur, err := strconv.Atoi(orig)
	if err != nil || cur >= minChecks {
		return
	}
	if setErr := flag.Set("rapid.checks", strconv.Itoa(minChecks)); setErr != nil {
		t.Fatalf("flag.Set rapid.checks: %v", setErr)
	}
	t.Cleanup(func() { _ = flag.Set("rapid.checks", orig) })
}
