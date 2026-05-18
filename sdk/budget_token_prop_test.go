package sdk

import (
	"fmt"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// Feature: billing-system-optimization, Property 18: Budget Token Local Deduction

// TestProperty18_BudgetTokenLocalDeduction verifies that for any valid
// (non-expired) Budget Token with remaining budget B, calling TryDeduct with
// amount A succeeds iff B >= A, and after success Remaining == B - A.
// This confirms that local deduction works without needing a Redis Hold.
//
// **Validates: Requirements 11.2**
func TestProperty18_BudgetTokenLocalDeduction(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate random budget B (positive, between 0.01 and 10000.0)
		budget := rapid.Float64Range(0.01, 10000.0).Draw(rt, "budget")

		// Generate random deduction amount A (positive, between 0.001 and 20000.0)
		// We intentionally allow A > B to test the rejection case
		amount := rapid.Float64Range(0.001, 20000.0).Draw(rt, "amount")

		// Generate a TTL that ensures the token is NOT expired during the test
		ttl := time.Duration(rapid.IntRange(10, 3600).Draw(rt, "ttlSeconds")) * time.Second

		// Create a fresh store and acquire a token with budget B
		store := NewBudgetTokenStore()
		userID := uint(rapid.IntRange(1, 100000).Draw(rt, "userID"))
		batchID := fmt.Sprintf("batch-p18-%d", rapid.IntRange(1, 999999).Draw(rt, "batchSuffix"))

		store.Acquire(userID, batchID, budget, ttl)

		// Call TryDeduct with amount A
		ok := store.TryDeduct(userID, amount)

		// Property: succeeds iff budget >= amount (token is not expired)
		shouldSucceed := budget >= amount

		if shouldSucceed && !ok {
			rt.Fatalf("TryDeduct should succeed: budget=%f >= amount=%f, but returned false",
				budget, amount)
		}
		if !shouldSucceed && ok {
			rt.Fatalf("TryDeduct should fail: budget=%f < amount=%f, but returned true",
				budget, amount)
		}

		// Property: after success, Remaining == B - A
		if ok {
			store.mu.RLock()
			token := store.tokens[userID]
			store.mu.RUnlock()

			token.mu.Lock()
			remaining := token.Remaining
			token.mu.Unlock()

			expected := budget - amount
			const epsilon = 1e-9
			diff := remaining - expected
			if diff > epsilon || diff < -epsilon {
				rt.Fatalf("after successful deduction: Remaining=%f, want %f (budget=%f, amount=%f)",
					remaining, expected, budget, amount)
			}
		}
	})
}

// TestProperty18_BudgetTokenLocalDeduction_Expired verifies that an expired
// token always rejects deduction regardless of remaining budget.
//
// **Validates: Requirements 11.2**
func TestProperty18_BudgetTokenLocalDeduction_Expired(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate random budget B (always sufficient)
		budget := rapid.Float64Range(100.0, 10000.0).Draw(rt, "budget")

		// Generate a small deduction amount that would succeed if token were valid
		amount := rapid.Float64Range(0.001, budget*0.5).Draw(rt, "amount")

		// Create a store and acquire a token with a TTL of 0 (immediately expired)
		store := NewBudgetTokenStore()
		userID := uint(rapid.IntRange(1, 100000).Draw(rt, "userID"))

		store.Acquire(userID, "batch-expired-p18", budget, 0)

		// Call TryDeduct — should always fail because token is expired
		ok := store.TryDeduct(userID, amount)

		if ok {
			rt.Fatalf("TryDeduct should fail for expired token: budget=%f, amount=%f, but returned true",
				budget, amount)
		}
	})
}

// Feature: billing-system-optimization, Property 19: Budget Token Renewal

// TestProperty19_BudgetTokenRenewal verifies that when a Budget Token's
// remaining budget is insufficient or the token has expired, the system
// releases the old token's remaining balance and can acquire a new batch
// Hold from Redis.
//
// **Validates: Requirements 11.3, 11.4**
func TestProperty19_BudgetTokenRenewal_Insufficient(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate random budget B and deduction amount A > B (insufficient)
		budget := rapid.Float64Range(0.01, 1000.0).Draw(rt, "budget")
		// Amount must be strictly greater than budget to trigger insufficient
		extra := rapid.Float64Range(0.01, 500.0).Draw(rt, "extra")
		amount := budget + extra

		userID := uint(rapid.IntRange(1, 100000).Draw(rt, "userID"))
		batchID := fmt.Sprintf("batch-%d", rapid.IntRange(1, 999999).Draw(rt, "batchSuffix"))
		ttl := time.Duration(rapid.IntRange(30, 300).Draw(rt, "ttlSec")) * time.Second

		store := NewBudgetTokenStore()

		// Step 1: Acquire a token with budget B
		token := store.Acquire(userID, batchID, budget, ttl)
		if token == nil {
			rt.Fatalf("Acquire returned nil")
		}
		if token.Remaining != budget {
			rt.Fatalf("token.Remaining = %f, want %f", token.Remaining, budget)
		}

		// Step 2: TryDeduct with amount A > B → should fail (insufficient)
		ok := store.TryDeduct(userID, amount)
		if ok {
			rt.Fatalf("TryDeduct should fail: amount %f > budget %f", amount, budget)
		}

		// Step 3: Release → should return batchID and remaining budget
		releasedBatchID, remaining := store.Release(userID)
		if releasedBatchID != batchID {
			rt.Fatalf("Release batchID = %q, want %q", releasedBatchID, batchID)
		}
		if remaining != budget {
			rt.Fatalf("Release remaining = %f, want %f (budget unchanged since TryDeduct failed)", remaining, budget)
		}

		// Step 4: Acquire with new batchID and new budget → should succeed
		newBatchID := fmt.Sprintf("new-batch-%d", rapid.IntRange(1, 999999).Draw(rt, "newBatchSuffix"))
		newBudget := rapid.Float64Range(amount, amount+1000.0).Draw(rt, "newBudget")
		newToken := store.Acquire(userID, newBatchID, newBudget, ttl)
		if newToken == nil {
			rt.Fatalf("new Acquire returned nil")
		}
		if newToken.BatchRequestID != newBatchID {
			rt.Fatalf("new token BatchRequestID = %q, want %q", newToken.BatchRequestID, newBatchID)
		}
		if newToken.Remaining != newBudget {
			rt.Fatalf("new token Remaining = %f, want %f", newToken.Remaining, newBudget)
		}

		// Step 5: TryDeduct with original amount should now succeed on new token
		ok = store.TryDeduct(userID, amount)
		if !ok {
			rt.Fatalf("TryDeduct should succeed on new token: amount %f <= newBudget %f", amount, newBudget)
		}

		// Verify remaining is correctly reduced
		store.mu.RLock()
		currentToken := store.tokens[userID]
		store.mu.RUnlock()

		currentToken.mu.Lock()
		expectedRemaining := newBudget - amount
		diff := currentToken.Remaining - expectedRemaining
		if diff > 0.0001 || diff < -0.0001 {
			rt.Fatalf("after deduct, Remaining = %f, want %f", currentToken.Remaining, expectedRemaining)
		}
		currentToken.mu.Unlock()
	})
}

// TestProperty19_BudgetTokenRenewal_Expired verifies that when a Budget Token
// has expired, TryDeduct fails and the token can be released and renewed.
//
// **Validates: Requirements 11.3, 11.4**
func TestProperty19_BudgetTokenRenewal_Expired(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate random parameters
		budget := rapid.Float64Range(1.0, 1000.0).Draw(rt, "budget")
		amount := rapid.Float64Range(0.01, budget).Draw(rt, "amount") // amount <= budget (would succeed if not expired)
		userID := uint(rapid.IntRange(1, 100000).Draw(rt, "userID"))
		batchID := fmt.Sprintf("batch-exp-%d", rapid.IntRange(1, 999999).Draw(rt, "batchSuffix"))

		store := NewBudgetTokenStore()

		// Acquire with very short TTL (1ms)
		token := store.Acquire(userID, batchID, budget, 1*time.Millisecond)
		if token == nil {
			rt.Fatalf("Acquire returned nil")
		}

		// Wait for token to expire
		time.Sleep(5 * time.Millisecond)

		// TryDeduct should fail because token is expired
		ok := store.TryDeduct(userID, amount)
		if ok {
			rt.Fatalf("TryDeduct should fail on expired token (budget=%f, amount=%f)", budget, amount)
		}

		// Release the expired token → should still return batchID and remaining
		releasedBatchID, remaining := store.Release(userID)
		if releasedBatchID != batchID {
			rt.Fatalf("Release batchID = %q, want %q", releasedBatchID, batchID)
		}
		if remaining != budget {
			rt.Fatalf("Release remaining = %f, want %f (no deductions occurred)", remaining, budget)
		}

		// Acquire a new token with fresh TTL
		newBatchID := fmt.Sprintf("new-batch-exp-%d", rapid.IntRange(1, 999999).Draw(rt, "newBatchSuffix"))
		newBudget := rapid.Float64Range(amount, amount+500.0).Draw(rt, "newBudget")
		newTTL := time.Duration(rapid.IntRange(30, 300).Draw(rt, "newTTLSec")) * time.Second
		newToken := store.Acquire(userID, newBatchID, newBudget, newTTL)
		if newToken == nil {
			rt.Fatalf("new Acquire returned nil")
		}
		if newToken.BatchRequestID != newBatchID {
			rt.Fatalf("new token BatchRequestID = %q, want %q", newToken.BatchRequestID, newBatchID)
		}

		// TryDeduct should now succeed on the fresh token
		ok = store.TryDeduct(userID, amount)
		if !ok {
			rt.Fatalf("TryDeduct should succeed on fresh token: amount %f <= newBudget %f", amount, newBudget)
		}
	})
}
