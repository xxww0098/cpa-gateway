package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/xxww0098/cpa-gateway/model"
)

// insertShortfallSettleRowHoldMW inserts a settle-type BalanceLog row
// whose Metadata carries a positive shortfall_usd. The row is unresolved
// because no matching "shortfall_resolve:<ref>:<id>" credit is written,
// mirroring the shape the partial-debit branch of ledger.Settle would
// have produced on a real under-funded settlement.
//
// The name is suffixed with "HoldMW" to avoid a package-level collision
// with the equivalent helper in api/purchase_debt_block_test.go (even
// though they live in different packages, keeping the name distinct
// makes grep / IDE navigation unambiguous).
func insertShortfallSettleRowHoldMW(t *testing.T, env *preflightEnv, userID uint, ref string, shortfall float64) model.BalanceLog {
	t.Helper()

	metadata, err := json.Marshal(map[string]interface{}{
		"user_id":       userID,
		"shortfall_usd": shortfall,
		"actual_cost":   shortfall,
	})
	if err != nil {
		t.Fatalf("marshal shortfall metadata: %v", err)
	}

	row := model.BalanceLog{
		UserID:    userID,
		Amount:    0,
		Type:      "settle",
		Reference: ref,
		Metadata:  metadata,
	}
	if err := env.db.Create(&row).Error; err != nil {
		t.Fatalf("insert shortfall settle row: %v", err)
	}
	return row
}

// insertShortfallResolveCreditHoldMW writes a credit-type BalanceLog
// whose Reference follows the "shortfall_resolve:<debitRef>:<debitID>"
// convention enforced by Ledger.HasUnresolvedShortfall. After this row
// is inserted, the user should be unblocked on the next preflight.
func insertShortfallResolveCreditHoldMW(t *testing.T, env *preflightEnv, userID uint, debitRef string, debitID uint, amount float64) {
	t.Helper()

	row := model.BalanceLog{
		UserID:    userID,
		Amount:    amount,
		Type:      "credit",
		Reference: fmt.Sprintf("shortfall_resolve:%s:%d", debitRef, debitID),
	}
	if err := env.db.Create(&row).Error; err != nil {
		t.Fatalf("insert shortfall_resolve credit row: %v", err)
	}
}

// TestHoldMiddlewareBlockedOnShortfall is task 6.6's example regression
// for Property 8 (Requirements 2.5, 2.6):
//
//   - A user carrying an unresolved shortfall row must be refused at the
//     HoldMiddleware preflight BEFORE any Redis hold is created. The
//     response is HTTP 402 with body {"error":"outstanding_debt"}, the
//     downstream handler never runs, and no hold appears in miniredis.
//   - Once a matching "shortfall_resolve:<debitRef>:<debitID>" credit is
//     written, the next preflight must admit the request: handler runs,
//     ledger.Hold creates the expected Redis entry.
//
// The test uses the same (miniredis + sqlite + real *ledger.Ledger)
// triple that holdmw_preflight_test.go exercises, reusing the helper
// types from that file so the two tests share a consistent setup
// surface. The pricing stub is deliberately tiny so the upper-bound
// preflight (Requirement 2.1) never interferes with the debt-block
// preflight under test — the admission branch in the second request is
// still exercised end-to-end because Hold is invoked on the real
// ledger.
//
// **Validates: Property 8, Requirements 2.5, 2.6**
func TestHoldMiddlewareBlockedOnShortfall(t *testing.T) {
	const (
		userID          = uint(101)
		startingBalance = 5.0
		shortfallUSD    = 0.50
		debitRef        = "req-x"
	)

	env := newPreflightEnv(t, userID, startingBalance)

	// --- Seed: one unresolved shortfall settle row. ---
	debitRow := insertShortfallSettleRowHoldMW(t, env, userID, debitRef, shortfallUSD)

	// Sanity: Ledger.HasUnresolvedShortfall agrees the user is blocked.
	// A drift in the seeded metadata shape vs. the predicate in
	// ledger.go would otherwise surface only as a confusing test
	// failure further down.
	blocked, err := env.ldg.HasUnresolvedShortfall(context.Background(), userID)
	if err != nil {
		t.Fatalf("HasUnresolvedShortfall (pre-block): %v", err)
	}
	if !blocked {
		t.Fatalf("HasUnresolvedShortfall (pre-block): got false, want true")
	}

	// Pricing stub with tiny estimates so the upper-bound preflight
	// cannot interfere. Both Estimate and EstimateWithMaxTokens return
	// the same value — the property under test is the debt block, not
	// the upper-bound gate.
	calc := &preflightCalcStub{
		estimate:             0.0001,
		estimateWithMaxValue: 0.0001,
	}

	// ----- Request 1: must be rejected by the debt-block preflight -----
	w1, handlerReached1, reqID1 := runPreflightHandler(t, env, calc, userID, 0)

	if w1.Code != http.StatusPaymentRequired {
		t.Fatalf("request 1 status: got %d want %d; body=%s",
			w1.Code, http.StatusPaymentRequired, w1.Body.String())
	}
	var resp1 map[string]string
	if err := json.Unmarshal(w1.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("request 1 unmarshal body: %v; raw=%s", err, w1.Body.String())
	}
	if got, want := resp1["error"], "outstanding_debt"; got != want {
		t.Fatalf("request 1 error: got %q want %q; body=%s", got, want, w1.Body.String())
	}
	if handlerReached1 {
		t.Fatalf("request 1 downstream handler must NOT run on debt block")
	}
	// A hold must NOT exist for this reqID. ZScore on a missing member
	// (or a missing key) returns a non-nil error on miniredis, so a
	// nil error is the violation signal.
	if _, err := env.srv.ZScore(holdsZSetKeyFor(userID), reqID1); err == nil {
		t.Fatalf("request 1 must not create a Redis hold, but ZSCORE holds:{%d} %s returned no error",
			userID, reqID1)
	}

	// No balance should have been debited or credited by the preflight
	// path — only the seeded settle row should exist.
	var writeCount int64
	if err := env.db.Model(&model.BalanceLog{}).
		Where("user_id = ? AND type IN (?, ?)", userID, "debit", "credit").
		Count(&writeCount).Error; err != nil {
		t.Fatalf("request 1 balance_logs count: %v", err)
	}
	if writeCount != 0 {
		t.Fatalf("request 1 unexpected debit/credit rows: got %d want 0", writeCount)
	}

	// ----- Resolve the shortfall via a matching credit. -----
	insertShortfallResolveCreditHoldMW(t, env, userID, debitRef, debitRow.ID, shortfallUSD)

	// Sanity: the predicate now reports unblocked.
	blocked, err = env.ldg.HasUnresolvedShortfall(context.Background(), userID)
	if err != nil {
		t.Fatalf("HasUnresolvedShortfall (post-resolve): %v", err)
	}
	if blocked {
		t.Fatalf("HasUnresolvedShortfall (post-resolve): got true, want false")
	}

	// ----- Request 2: must be admitted; ledger.Hold creates a hold -----
	w2, handlerReached2, reqID2 := runPreflightHandler(t, env, calc, userID, 0)

	if w2.Code != http.StatusOK {
		t.Fatalf("request 2 status: got %d want 200; body=%s", w2.Code, w2.Body.String())
	}
	if !handlerReached2 {
		t.Fatalf("request 2 downstream handler must run after shortfall is resolved; body=%s",
			w2.Body.String())
	}
	if _, err := env.srv.ZScore(holdsZSetKeyFor(userID), reqID2); err != nil {
		t.Fatalf("request 2 must create a Redis hold, but ZSCORE holds:{%d} %s returned err=%v",
			userID, reqID2, err)
	}
}
