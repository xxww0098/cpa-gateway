package ledger

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"gorm.io/gorm"
)

// Feature: billing-security-hardening, Property 7 — Settle never clears the
// Redis hold unless the persistent writes (debit + optional shortfall row)
// have been committed.
//
// These two fault-injection example tests lock down the strict ordering
// specified by Requirement 2.7:
//
//  1. If the DB transaction rolls back (any of the INSERTs into BalanceLog or
//     the UPDATE on users fails), Settle must return a non-nil error AND
//     leave the Redis hold entry intact for later reconciliation.
//  2. If the DB transaction commits but the post-commit ZRem/HDel against
//     Redis fails, Settle must return the Redis error AND leave the hold
//     entry intact (an acceptable degraded state — the hold is reclaimed by
//     TTL or manual reconciliation).
//
// Both tests are deliberately written as example tests rather than
// rapid-driven property tests because they exercise very specific failure
// injection points and do not benefit from shrinkable input generation;
// the invariant under test is categorical ("hold must survive"), not
// parametric.

// TestSettleTxRollbackKeepsHold verifies that when the BalanceLog INSERT
// inside Settle's transaction fails, Settle surfaces the error and the
// Redis hold sorted-set entry remains present with its original score.
//
// Injection point: a GORM Before("gorm:create") callback registered on the
// Ledger's *gorm.DB that returns an error whenever a BalanceLog row is the
// target of a Create. This causes tx.Create(&BalanceLog{...}) to fail,
// which aborts the surrounding transaction and triggers the error branch
// in Settle before ZRem/HDel are reached.
//
// **Validates: Property 7, Requirement 2.7**
func TestSettleTxRollbackKeepsHold(t *testing.T) {
	const (
		userID         uint    = 1
		initialBalance float64 = 100.0
		holdAmount     float64 = 5.0
		actualAmount   float64 = 3.0 // > 0 forces the partial-debit (tx) path
	)
	const reqID = "settle-fault-tx-1"

	ldg, srv := setupActiveHoldTestEnv(t, userID, initialBalance)
	ctx := context.Background()

	// Place the hold. The user row already exists from setupActiveHoldTestEnv,
	// and Hold does not itself go through GORM Create against BalanceLog in
	// its critical path (writeAuditLog is best-effort), so registering the
	// failing callback AFTER this step keeps the pre-condition clean.
	if err := ldg.Hold(ctx, userID, holdAmount, reqID, 5*time.Minute); err != nil {
		t.Fatalf("Hold failed (prep): %v", err)
	}

	// Sanity-check: hold is present with the expected score before injection.
	score, present, err := ldg.ActiveHoldAmount(ctx, userID, reqID)
	if err != nil {
		t.Fatalf("ActiveHoldAmount (pre): %v", err)
	}
	if !present {
		t.Fatalf("ActiveHoldAmount (pre): hold missing, want present=true")
	}
	if math.Abs(score-holdAmount) > 1e-9 {
		t.Fatalf("ActiveHoldAmount (pre): got %f, want %f", score, holdAmount)
	}

	// Register a Create-callback that errors whenever a BalanceLog row is
	// being inserted. The User row was already created during setup, so the
	// UPDATE inside Settle (which does not trigger the Create callback) and
	// the SELECT FOR UPDATE are unaffected; only the BalanceLog INSERT fails.
	const callbackName = "test:fail_balance_log_create"
	failErr := errors.New("simulated balance log insert failure")
	cb := func(d *gorm.DB) {
		if d.Statement.Schema != nil && d.Statement.Schema.Name == "BalanceLog" {
			_ = d.AddError(failErr)
		}
	}
	if err := ldg.db.Callback().Create().Before("gorm:create").Register(callbackName, cb); err != nil {
		t.Fatalf("register create callback: %v", err)
	}
	t.Cleanup(func() {
		_ = ldg.db.Callback().Create().Remove(callbackName)
	})

	// Attempt Settle. The BalanceLog INSERT inside the tx must fail,
	// rolling back the whole tx. Settle must return a non-nil error.
	settleErr := ldg.Settle(ctx, userID, reqID, actualAmount)
	if settleErr == nil {
		t.Fatalf("Settle expected non-nil error when BalanceLog create is rejected, got nil")
	}

	// The returned error should carry our injected reason (gorm wraps the
	// callback error, so we use errors.Is to tolerate wrapping).
	if !errors.Is(settleErr, failErr) && settleErr.Error() == "" {
		// Not strictly required by the contract, but helps future debugging
		// if the callback mechanics ever change.
		t.Logf("Settle returned %v (expected to wrap %v)", settleErr, failErr)
	}

	// Invariant: the Redis hold is still present with its original score.
	score, present, err = ldg.ActiveHoldAmount(ctx, userID, reqID)
	if err != nil {
		t.Fatalf("ActiveHoldAmount after failed Settle: %v", err)
	}
	if !present {
		t.Fatalf("ActiveHoldAmount after failed Settle: hold missing (tx rollback must NOT release the hold)")
	}
	if math.Abs(score-holdAmount) > 1e-9 {
		t.Fatalf("ActiveHoldAmount after failed Settle: got %f, want %f (hold amount must be unchanged)",
			score, holdAmount)
	}

	// Cross-check via miniredis directly so the invariant is visible at the
	// sorted-set layer, independent of the ActiveHoldAmount implementation.
	memberScore, zerr := srv.ZScore(holdsKey(userID), reqID)
	if zerr != nil {
		t.Fatalf("miniredis ZScore after failed Settle: %v", zerr)
	}
	if math.Abs(memberScore-holdAmount) > 1e-9 {
		t.Fatalf("miniredis ZScore after failed Settle: got %f, want %f", memberScore, holdAmount)
	}
}

// TestSettleRedisErrorKeepsHold verifies that when Settle's DB transaction
// commits successfully but the post-commit ZRem/HDel against Redis fails,
// Settle returns the Redis error and the hold entry remains in the sorted
// set. Clearing the injected error afterward lets the test observe the
// surviving hold via ActiveHoldAmount.
//
// Injection point: miniredis.SetError("outage") installs a pre-hook that
// forces every Redis command to return the given error message. We set it
// AFTER the Hold is in place (so the hold entry exists in Redis) and BEFORE
// calling Settle (so the ZRem/HDel inside Settle fail).
//
// **Validates: Property 7, Requirement 2.7**
func TestSettleRedisErrorKeepsHold(t *testing.T) {
	const (
		userID         uint    = 2
		initialBalance float64 = 100.0
		holdAmount     float64 = 7.5
		actualAmount   float64 = 4.0 // > 0 to force the partial-debit tx path
	)
	const reqID = "settle-fault-redis-1"

	ldg, srv := setupActiveHoldTestEnv(t, userID, initialBalance)
	ctx := context.Background()

	// Place the hold under normal (error-free) Redis conditions.
	if err := ldg.Hold(ctx, userID, holdAmount, reqID, 5*time.Minute); err != nil {
		t.Fatalf("Hold failed (prep): %v", err)
	}

	// Sanity-check the hold exists before injection.
	score, present, err := ldg.ActiveHoldAmount(ctx, userID, reqID)
	if err != nil {
		t.Fatalf("ActiveHoldAmount (pre): %v", err)
	}
	if !present || math.Abs(score-holdAmount) > 1e-9 {
		t.Fatalf("ActiveHoldAmount (pre): got (%f, %t), want (%f, true)", score, present, holdAmount)
	}

	// Inject the Redis outage. From here on every Redis command returns an
	// error, but the in-memory sorted-set state is NOT mutated by the
	// pre-hook, so the hold member survives the outage.
	srv.SetError("outage")

	// Cleanup is registered immediately so that an early t.Fatalf does not
	// leave the miniredis in an unusable state for later code in this test.
	t.Cleanup(func() { srv.SetError("") })

	// Settle's DB transaction (SQLite, unaffected by the Redis outage)
	// commits successfully, debiting 4.0 and writing the settle BalanceLog.
	// The post-commit ZRem then fails with "outage" — Settle must surface
	// that error. Per Requirement 2.7 the hold entry remains in place.
	settleErr := ldg.Settle(ctx, userID, reqID, actualAmount)
	if settleErr == nil {
		t.Fatalf("Settle expected non-nil error when Redis ZRem fails, got nil")
	}

	// Clear the injected error so ActiveHoldAmount (which also talks to
	// Redis) can observe the surviving hold entry.
	srv.SetError("")

	score, present, err = ldg.ActiveHoldAmount(ctx, userID, reqID)
	if err != nil {
		t.Fatalf("ActiveHoldAmount after Redis-error Settle: %v", err)
	}
	if !present {
		t.Fatalf("ActiveHoldAmount after Redis-error Settle: hold missing (Redis failure must NOT silently release the hold)")
	}
	if math.Abs(score-holdAmount) > 1e-9 {
		t.Fatalf("ActiveHoldAmount after Redis-error Settle: got %f, want %f (hold amount must be unchanged)",
			score, holdAmount)
	}

	// Cross-check via miniredis directly.
	memberScore, zerr := srv.ZScore(holdsKey(userID), reqID)
	if zerr != nil {
		t.Fatalf("miniredis ZScore after Redis-error Settle: %v", zerr)
	}
	if math.Abs(memberScore-holdAmount) > 1e-9 {
		t.Fatalf("miniredis ZScore after Redis-error Settle: got %f, want %f", memberScore, holdAmount)
	}
}
