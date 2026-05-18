package ledger

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"pgregory.net/rapid"
)

// ledgerTestTB is the common subset of *testing.T and *rapid.T that this file
// needs. Both concrete types satisfy it, so the setup helper can be shared
// between the example test and the rapid-driven property test without
// duplicating boilerplate.
type ledgerTestTB interface {
	Helper()
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
	Cleanup(func())
}

// setupActiveHoldTestEnv builds a real *ledger.Ledger backed by a fresh
// miniredis server plus an in-memory SQLite database, seeded with a user
// whose balance is sufficient for the supplied hold amount.
//
// The helper mirrors the construction pattern used elsewhere in the ledger
// package's property tests (see TestProperty12_HoldSortedSetLifecycleInvariant)
// so the Round-trip test exercises the real Hold / Settle / Release code
// paths instead of a mock.
func setupActiveHoldTestEnv(rt ledgerTestTB, userID uint, balance float64) (*Ledger, *miniredis.Miniredis) {
	rt.Helper()

	srv := miniredis.RunT(rt)

	rClient := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	rt.Cleanup(func() { _ = rClient.Close() })

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		rt.Fatalf("failed to open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.BalanceLog{}); err != nil {
		rt.Fatalf("failed to migrate: %v", err)
	}

	user := model.User{
		ID:           userID,
		Email:        fmt.Sprintf("active-hold-%d@test.local", userID),
		PasswordHash: "hash",
		Balance:      balance,
	}
	if err := db.Create(&user).Error; err != nil {
		rt.Fatalf("failed to create user: %v", err)
	}

	ldg := NewWithConfig(db, rClient, 30*time.Second, 5*time.Minute)

	// Pre-cache the balance in Redis so the Hold Lua script observes it.
	if err := ldg.refreshBalanceCache(context.Background(), userID); err != nil {
		rt.Fatalf("failed to refresh balance cache: %v", err)
	}

	return ldg, srv
}

// Feature: billing-security-hardening, Property 1 — Fallback settle lower-bound.
//
// TestActiveHoldAmountRoundTrip asserts the round-trip contract that the
// fallback-settle path in UsagePlugin relies on:
//
//  1. After Hold(userID, amount, requestID), ActiveHoldAmount returns
//     (amount, true, nil).
//  2. After Release(userID, requestID), ActiveHoldAmount returns
//     (0, false, nil).
//  3. After Settle(userID, requestID, actual) with any actual >= 0,
//     ActiveHoldAmount returns (0, false, nil).
//
// This is the sole Active_Hold_Amount invariant that Requirement 1.5 and
// Requirement 2.2 both transitively depend on: the fallback-settle lower
// bound is `max(Active_Hold_Amount, Estimate(...))`, so a drifted reading
// would corrupt every downstream settlement calculation.
//
// The outer 2× loop is used because `rapid.Check` defaults to 100 internal
// iterations; two successive invocations yield >= 200 total checks while
// preserving rapid's shrinking behaviour per invocation.
//
// **Validates: Property 1, Requirement 2.2**
func TestActiveHoldAmountRoundTrip(t *testing.T) {
	for outer := 0; outer < 2; outer++ {
		rapid.Check(t, func(rt *rapid.T) {
			// Generate inputs. Amounts are bounded away from zero and below
			// the seeded balance so Hold is guaranteed to succeed; userID is
			// kept small but strictly positive to match GORM semantics.
			amount := rapid.Float64Range(0.01, 100.0).Draw(rt, "amount")
			userID := uint(rapid.IntRange(1, 1_000_000).Draw(rt, "userID"))
			reqSuffix := rapid.IntRange(1, 1_000_000_000).Draw(rt, "reqSuffix")
			requestID := fmt.Sprintf("active-hold-req-%d", reqSuffix)

			// Choose which teardown path to exercise: Release OR Settle(actual).
			// Settle with a non-negative actual (including 0) is valid and
			// must also clear the hold.
			useSettle := rapid.Bool().Draw(rt, "useSettle")
			actualAmount := rapid.Float64Range(0.0, amount).Draw(rt, "actualAmount")

			// Seed balance generously so Settle never fails with
			// ErrInsufficientBalance. That failure mode is covered by
			// the partial-Settle tests (see tasks 1.3 – 1.5).
			ldg, _ := setupActiveHoldTestEnv(rt, userID, amount*10+1.0)
			ctx := context.Background()

			// Step 1: Hold → ActiveHoldAmount returns (amount, true, nil).
			if err := ldg.Hold(ctx, userID, amount, requestID, 5*time.Minute); err != nil {
				rt.Fatalf("Hold failed (userID=%d amount=%f reqID=%s): %v",
					userID, amount, requestID, err)
			}

			got, present, err := ldg.ActiveHoldAmount(ctx, userID, requestID)
			if err != nil {
				rt.Fatalf("ActiveHoldAmount after Hold returned error: %v", err)
			}
			if !present {
				rt.Fatalf("ActiveHoldAmount after Hold: present=false, want true (userID=%d reqID=%s)",
					userID, requestID)
			}
			// Redis scores are stored as float64; the hold amount is
			// serialised via strconv.FormatFloat(..., -1, 64) so round-trip
			// precision should be exact. Use a tiny epsilon to tolerate
			// float comparison quirks without hiding real drift.
			const epsilon = 1e-9
			if math.Abs(got-amount) > epsilon {
				rt.Fatalf("ActiveHoldAmount after Hold: got=%f want=%f (userID=%d reqID=%s)",
					got, amount, userID, requestID)
			}

			// Step 2: Release OR Settle → ActiveHoldAmount returns (0, false, nil).
			if useSettle {
				if err := ldg.Settle(ctx, userID, requestID, actualAmount); err != nil {
					rt.Fatalf("Settle failed (userID=%d reqID=%s actual=%f): %v",
						userID, requestID, actualAmount, err)
				}
			} else {
				if err := ldg.Release(ctx, userID, requestID); err != nil {
					rt.Fatalf("Release failed (userID=%d reqID=%s): %v",
						userID, requestID, err)
				}
			}

			got, present, err = ldg.ActiveHoldAmount(ctx, userID, requestID)
			if err != nil {
				action := "Release"
				if useSettle {
					action = "Settle"
				}
				rt.Fatalf("ActiveHoldAmount after %s returned error: %v", action, err)
			}
			if present {
				action := "Release"
				if useSettle {
					action = "Settle"
				}
				rt.Fatalf("ActiveHoldAmount after %s: present=true, want false (userID=%d reqID=%s got=%f)",
					action, userID, requestID, got)
			}
			if got != 0 {
				action := "Release"
				if useSettle {
					action = "Settle"
				}
				rt.Fatalf("ActiveHoldAmount after %s: got=%f, want 0 (userID=%d reqID=%s)",
					action, got, userID, requestID)
			}
		})
	}
}

// TestActiveHoldAmountEmptyRequestIDError covers the guard clause in
// Ledger.ActiveHoldAmount: an empty requestID is a programmer error and must
// surface as a non-nil error instead of silently returning the zero state
// (which would be indistinguishable from "no hold present" and could mask a
// miswired call site in UsagePlugin's fallback branch).
//
// **Validates: Property 1, Requirement 2.2**
func TestActiveHoldAmountEmptyRequestIDError(t *testing.T) {
	srv := miniredis.RunT(t)
	rClient := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = rClient.Close() })

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.BalanceLog{}); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	ldg := NewWithConfig(db, rClient, 30*time.Second, 5*time.Minute)

	got, present, err := ldg.ActiveHoldAmount(context.Background(), 42, "")
	if err == nil {
		t.Fatalf("ActiveHoldAmount with empty requestID: want error, got nil (got=%f present=%t)",
			got, present)
	}
	if present {
		t.Fatalf("ActiveHoldAmount with empty requestID: present=true, want false (got=%f err=%v)",
			got, err)
	}
	if got != 0 {
		t.Fatalf("ActiveHoldAmount with empty requestID: got=%f, want 0 (err=%v)", got, err)
	}
}
