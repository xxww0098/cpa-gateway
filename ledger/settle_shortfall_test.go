package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/xxww0098/cpa-gateway/model"
	"pgregory.net/rapid"
)

// Feature: billing-security-hardening, Property 6 — Partial-Settle conservation.
//
// TestSettleConservation asserts, over randomly generated
// (initial_balance, hold_amount, actual_cost) triples, three invariants that
// task 1.3's partial-debit rewrite of Ledger.Settle must uphold:
//
//  1. Conservation (corrected per spec patch, keeps Requirement 2.3 dimension-
//     ally consistent with Settle's partial-debit semantics):
//     final_balance - Σ(metadata.shortfall_usd) == initial_balance - actual_cost,
//     within a small float epsilon. Equivalently, final + debited == initial,
//     where debited = min(initial_balance, actual_cost) = actual_cost -
//     Σshortfall — the ledger never over-debits, and the recoverable debt
//     is tracked in `shortfall_usd` metadata.
//  2. A settle BalanceLog row whose Metadata carries shortfall_usd > 0
//     exists IFF actual_cost > initial_balance — the partial-debit branch
//     of Settle records the unpaid portion precisely when it exists, and
//     must not emit a spurious shortfall row when actual_cost is fully
//     covered by the persistent balance.
//  3. After Settle commits, ZSCORE holds:{userID} {requestID} does not
//     exist — the Redis hold is cleared only after the DB tx commits.
//
// Inputs are drawn from rapid.Float64Range(0, 1000) for each element of
// the triple per the task spec. Cases where Hold would fail (hold_amount
// == 0 or hold_amount > initial_balance) are filtered out with rt.SkipNow,
// so the Settle-conservation invariants are tested only on settle paths
// that followed a successful Hold — the Hold admission path has its own
// dedicated property tests in active_hold_amount_test.go and
// ledger_prop_test.go.
//
// The 5× outer loop yields ≥500 rapid iterations (100 per inner Check,
// rapid's default), matching task 1.4's iteration budget while preserving
// the per-Check shrinker. Shrink target per the task:
// balance=0.01, hold=0.005, actual=1.00 — the canonical partial-debit
// case where a $0.01 balance is fully consumed and a $0.99 shortfall is
// recorded.
//
// **Validates: Property 6, Requirements 2.2, 2.3, 2.4**
func TestSettleConservation(t *testing.T) {
	const userID = uint(1)
	const epsilon = 1e-9

	for outer := 0; outer < 5; outer++ {
		rapid.Check(t, func(rt *rapid.T) {
			initialBalance := rapid.Float64Range(0, 1000).Draw(rt, "initial_balance")
			holdAmount := rapid.Float64Range(0, 1000).Draw(rt, "hold_amount")
			actualCost := rapid.Float64Range(0, 1000).Draw(rt, "actual_cost")
			reqSuffix := rapid.IntRange(1, 1_000_000_000).Draw(rt, "reqSuffix")

			// Filter: Hold requires amount > 0 AND available balance >= amount.
			// Inputs that violate those preconditions belong to the Hold
			// admission tests, not to this Settle conservation property.
			if holdAmount <= 0 || holdAmount > initialBalance {
				rt.SkipNow()
			}

			requestID := fmt.Sprintf("settle-cons-req-%d", reqSuffix)

			ldg, _ := setupActiveHoldTestEnv(rt, userID, initialBalance)
			ctx := context.Background()

			if err := ldg.Hold(ctx, userID, holdAmount, requestID, 5*time.Minute); err != nil {
				rt.Fatalf("Hold failed (balance=%v hold=%v reqID=%s): %v",
					initialBalance, holdAmount, requestID, err)
			}
			if err := ldg.Settle(ctx, userID, requestID, actualCost); err != nil {
				rt.Fatalf("Settle failed (balance=%v actual=%v reqID=%s): %v",
					initialBalance, actualCost, requestID, err)
			}

			// Read post-settle balance from DB — the authoritative source.
			var user model.User
			if err := ldg.db.WithContext(ctx).First(&user, userID).Error; err != nil {
				rt.Fatalf("reload user after Settle: %v", err)
			}
			finalBalance := user.Balance

			// Aggregate shortfall_usd across every settle BalanceLog row
			// for the request. Rows without the key contribute 0 (per the
			// task-level guidance: "shortfall_usd (0 if absent)").
			var settleRows []model.BalanceLog
			if err := ldg.db.WithContext(ctx).
				Where("user_id = ? AND reference = ? AND type = ?",
					userID, requestID, balanceLogTypeSettle).
				Find(&settleRows).Error; err != nil {
				rt.Fatalf("query settle BalanceLog rows: %v", err)
			}

			var totalShortfall float64
			shortfallRowCount := 0
			for _, row := range settleRows {
				if len(row.Metadata) == 0 {
					continue
				}
				var meta map[string]interface{}
				if err := json.Unmarshal(row.Metadata, &meta); err != nil {
					rt.Fatalf("parse settle row metadata id=%d: %v", row.ID, err)
				}
				raw, ok := meta["shortfall_usd"]
				if !ok {
					continue
				}
				val, ok := raw.(float64)
				if !ok {
					rt.Fatalf("shortfall_usd has unexpected type %T in row id=%d",
						raw, row.ID)
				}
				if val > 0 {
					totalShortfall += val
					shortfallRowCount++
				}
			}

			// Invariant 1: conservation (corrected per spec patch).
			//   final_balance - Σ(shortfall_usd) == initial_balance - actual_cost
			// Equivalently:
			//   final_balance + debited == initial_balance
			// where debited = actual_cost - Σshortfall = min(initial, actual).
			lhs := finalBalance - totalShortfall
			rhs := initialBalance - actualCost
			if math.Abs(lhs-rhs) > epsilon {
				rt.Fatalf("conservation violated: final=%v Σshortfall=%v initial=%v actual=%v (lhs=%v rhs=%v diff=%v)",
					finalBalance, totalShortfall, initialBalance, actualCost, lhs, rhs, lhs-rhs)
			}

			// Invariant 2: shortfall row exists iff actual_cost > initial_balance.
			expectShortfall := actualCost > initialBalance
			hadShortfall := shortfallRowCount > 0
			if hadShortfall != expectShortfall {
				rt.Fatalf("shortfall presence mismatch: got=%t want=%t (initial=%v actual=%v shortfall_rows=%d)",
					hadShortfall, expectShortfall, initialBalance, actualCost, shortfallRowCount)
			}

			// Invariant 3: Redis hold must be cleared after a successful Settle.
			score, present, err := ldg.ActiveHoldAmount(ctx, userID, requestID)
			if err != nil {
				rt.Fatalf("ActiveHoldAmount after Settle returned error: %v", err)
			}
			if present {
				rt.Fatalf("ZSCORE holds:{%d} %s still present after Settle: got=%v",
					userID, requestID, score)
			}
		})
	}
}
