package ledger

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"pgregory.net/rapid"
)

// setupTestDB creates an in-memory SQLite database with the User and BalanceLog tables.
func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.BalanceLog{}); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}
	return db
}

// Feature: billing-system-optimization, Property 1: Atomic Hold Balance Computation

// TestProperty1_AtomicHoldBalanceComputation verifies that for any user with
// cached balance B and holds H₁..Hₙ, Hold of amount A succeeds iff B - Σ(Hᵢ) ≥ A.
//
// **Validates: Requirements 1.1, 1.4, 6.2**
func TestProperty1_AtomicHoldBalanceComputation(t *testing.T) {
	f := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))

		// Generate random balance B (positive, between 1.0 and 1000.0)
		balance := math.Round((rng.Float64()*999.0+1.0)*100) / 100

		// Generate random number of existing holds (0 to 8)
		numHolds := rng.Intn(9)
		existingHolds := make([]float64, 0, numHolds)

		// Generate hold amounts that are individually valid (each < balance)
		for i := 0; i < numHolds; i++ {
			maxAmt := balance / float64(numHolds+1)
			if maxAmt < 0.01 {
				maxAmt = 0.01
			}
			amt := math.Round((rng.Float64()*maxAmt+0.01)*100) / 100
			existingHolds = append(existingHolds, amt)
		}

		// Generate new hold amount (between 0.01 and balance to test both outcomes)
		newAmount := math.Round((rng.Float64()*balance+0.01)*100) / 100

		// Set up miniredis
		srv, err := miniredis.Run()
		if err != nil {
			t.Logf("failed to start miniredis: %v", err)
			return false
		}
		defer srv.Close()

		rClient := redis.NewClient(&redis.Options{Addr: srv.Addr()})
		defer rClient.Close()

		// Set up SQLite with user having balance B
		db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
			Logger: logger.Discard,
		})
		if err != nil {
			t.Logf("failed to open sqlite: %v", err)
			return false
		}
		if err := db.AutoMigrate(&model.User{}, &model.BalanceLog{}); err != nil {
			t.Logf("failed to migrate: %v", err)
			return false
		}

		user := model.User{
			ID:           1,
			Email:        "test@example.com",
			PasswordHash: "hash",
			Balance:      balance,
		}
		if err := db.Create(&user).Error; err != nil {
			t.Logf("failed to create user: %v", err)
			return false
		}

		// Create ledger with long holdTTL so nothing expires during the test
		ldg := NewWithConfig(db, rClient, 30*time.Second, 5*time.Minute)
		ctx := context.Background()

		// Pre-cache the balance in Redis
		if err := ldg.refreshBalanceCache(ctx, user.ID); err != nil {
			t.Logf("failed to refresh balance cache: %v", err)
			return false
		}

		// Add existing holds to the sorted set via the Hold API
		actualHolds := make([]float64, 0, len(existingHolds))
		for i, holdAmt := range existingHolds {
			reqID := fmt.Sprintf("existing-hold-%d", i)
			err := ldg.Hold(ctx, user.ID, holdAmt, reqID, 5*time.Minute)
			if err != nil {
				// Hold failed because sum of holds exceeded balance — stop adding
				break
			}
			actualHolds = append(actualHolds, holdAmt)
		}

		// Compute expected available balance
		sumHolds := 0.0
		for _, h := range actualHolds {
			sumHolds += h
		}
		available := balance - sumHolds

		// Attempt the new hold
		newReqID := "new-hold-request"
		err = ldg.Hold(ctx, user.ID, newAmount, newReqID, 5*time.Minute)

		// Property: Hold succeeds iff available >= newAmount
		// Use a small epsilon for floating point comparison in the Lua script
		const epsilon = 1e-9
		shouldSucceed := available-newAmount >= -epsilon

		if shouldSucceed && err != nil {
			t.Logf("FAIL: expected hold to succeed (available=%.6f, amount=%.6f, balance=%.6f, sumHolds=%.6f) but got error: %v",
				available, newAmount, balance, sumHolds, err)
			return false
		}
		if !shouldSucceed && err == nil {
			t.Logf("FAIL: expected hold to fail (available=%.6f, amount=%.6f, balance=%.6f, sumHolds=%.6f) but it succeeded",
				available, newAmount, balance, sumHolds)
			return false
		}

		// If hold succeeded, verify the sorted set contains the new entry with correct score
		if err == nil {
			score, scoreErr := rClient.ZScore(ctx, holdsKey(user.ID), newReqID).Result()
			if scoreErr != nil {
				t.Logf("FAIL: hold succeeded but requestID not found in sorted set: %v", scoreErr)
				return false
			}
			if math.Abs(score-newAmount) > epsilon {
				t.Logf("FAIL: sorted set score %.6f != hold amount %.6f", score, newAmount)
				return false
			}
		}

		return true
	}

	cfg := &quick.Config{
		MaxCount: 200,
		Values: func(values []reflect.Value, rng *rand.Rand) {
			values[0] = reflect.ValueOf(rng.Int63())
		},
	}

	if err := quick.Check(f, cfg); err != nil {
		t.Errorf("Property 1 (Atomic Hold Balance Computation) failed: %v", err)
	}
}

// Feature: billing-system-optimization, Property 12: Hold Sorted Set Lifecycle Invariant

// TestProperty12_HoldSortedSetLifecycleInvariant verifies that after a Hold
// operation, the user's sorted set contains the request ID with the hold amount
// as score; and after Settle or Release, the request ID is no longer a member.
//
// **Validates: Requirements 6.3, 6.4**
func TestProperty12_HoldSortedSetLifecycleInvariant(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate random test parameters
		balance := rapid.Float64Range(10.0, 10000.0).Draw(rt, "balance")
		holdAmount := rapid.Float64Range(0.01, balance*0.5).Draw(rt, "holdAmount")
		reqSuffix := rapid.IntRange(1, 999999999).Draw(rt, "reqSuffix")
		requestID := fmt.Sprintf("req-lifecycle-%d", reqSuffix)
		useSettle := rapid.Bool().Draw(rt, "useSettle")

		// Set up infrastructure: fresh miniredis + SQLite per iteration
		srv, err := miniredis.Run()
		if err != nil {
			rt.Fatalf("failed to start miniredis: %v", err)
		}
		defer srv.Close()

		rClient := redis.NewClient(&redis.Options{Addr: srv.Addr()})
		defer rClient.Close()

		db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
		if err != nil {
			rt.Fatalf("failed to open sqlite: %v", err)
		}
		if err := db.AutoMigrate(&model.User{}, &model.BalanceLog{}); err != nil {
			rt.Fatalf("failed to migrate: %v", err)
		}

		// Create user with sufficient balance
		user := model.User{
			ID:           1,
			Email:        "lifecycle@test.com",
			PasswordHash: "hash",
			Balance:      balance,
		}
		if err := db.Create(&user).Error; err != nil {
			rt.Fatalf("failed to create user: %v", err)
		}

		ctx := context.Background()

		// Create ledger
		ldg := NewWithConfig(db, rClient, 30*time.Second, 5*time.Minute)

		// Pre-cache the balance in Redis
		if err := ldg.refreshBalanceCache(ctx, user.ID); err != nil {
			rt.Fatalf("failed to refresh balance cache: %v", err)
		}

		// Execute Hold
		err = ldg.Hold(ctx, user.ID, holdAmount, requestID, 5*time.Minute)
		if err != nil {
			rt.Fatalf("Hold failed: %v", err)
		}

		// Assert: sorted set contains the requestID after Hold
		holdSetKey := holdsKey(user.ID)
		members, _ := srv.ZMembers(holdSetKey)
		found := false
		for _, m := range members {
			if m == requestID {
				found = true
				break
			}
		}
		if !found {
			rt.Fatalf("requestID %q not found in sorted set after Hold; members: %v", requestID, members)
		}

		// Verify score matches hold amount
		score, err := srv.ZScore(holdSetKey, requestID)
		if err != nil {
			rt.Fatalf("failed to get ZScore: %v", err)
		}
		if diff := score - holdAmount; diff > 0.001 || diff < -0.001 {
			rt.Fatalf("score mismatch: got %f, want %f", score, holdAmount)
		}

		// Randomly choose Settle or Release
		if useSettle {
			// Settle with a random actual amount <= holdAmount
			actualAmount := rapid.Float64Range(0.001, holdAmount).Draw(rt, "actualAmount")
			err = ldg.Settle(ctx, user.ID, requestID, actualAmount)
			if err != nil {
				rt.Fatalf("Settle failed: %v", err)
			}
		} else {
			err = ldg.Release(ctx, user.ID, requestID)
			if err != nil {
				rt.Fatalf("Release failed: %v", err)
			}
		}

		// Assert: sorted set no longer contains the requestID
		members, _ = srv.ZMembers(holdSetKey)
		for _, m := range members {
			if m == requestID {
				action := "Release"
				if useSettle {
					action = "Settle"
				}
				rt.Fatalf("requestID %q still in sorted set after %s; members: %v", requestID, action, members)
			}
		}

		// Also verify the timestamp hash no longer contains the requestID
		tsHashKey := holdsTSKey(user.ID)
		tsVal := srv.HGet(tsHashKey, requestID)
		if tsVal != "" {
			action := "Release"
			if useSettle {
				action = "Settle"
			}
			rt.Fatalf("requestID %q still in timestamp hash after %s", requestID, action)
		}
	})
}

// Feature: billing-system-optimization, Property 13: Orphan Hold TTL Cleanup

// TestProperty13_OrphanHoldTTLCleanup verifies that holds exceeding TTL are
// removed before balance computation, effectively reclaiming frozen amounts.
//
// **Validates: Requirements 9.3, 9.4**
func TestProperty13_OrphanHoldTTLCleanup(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate test parameters
		balance := rapid.Float64Range(10.0, 10000.0).Draw(rt, "balance")
		holdTTLSec := rapid.IntRange(60, 600).Draw(rt, "holdTTLSec")
		holdTTL := time.Duration(holdTTLSec) * time.Second

		numExpired := rapid.IntRange(1, 10).Draw(rt, "numExpired")
		numFresh := rapid.IntRange(0, 10).Draw(rt, "numFresh")

		expiredAmounts := make([]float64, numExpired)
		for i := range expiredAmounts {
			expiredAmounts[i] = rapid.Float64Range(0.01, 5.0).Draw(rt, fmt.Sprintf("expiredAmount_%d", i))
		}

		freshAmounts := make([]float64, numFresh)
		for i := range freshAmounts {
			freshAmounts[i] = rapid.Float64Range(0.01, 5.0).Draw(rt, fmt.Sprintf("freshAmount_%d", i))
		}

		// Setup ledger with the generated holdTTL
		mr, err := miniredis.Run()
		if err != nil {
			rt.Fatalf("failed to start miniredis: %v", err)
		}
		defer mr.Close()

		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		defer rdb.Close()

		db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
		if err != nil {
			rt.Fatalf("failed to open sqlite: %v", err)
		}
		if err := db.AutoMigrate(&model.User{}, &model.BalanceLog{}); err != nil {
			rt.Fatalf("failed to migrate: %v", err)
		}

		ldg := NewWithConfig(db, rdb, 30*time.Second, holdTTL)

		// Create user with the generated balance
		userID := uint(1)
		user := model.User{ID: userID, Email: "test@test.com", PasswordHash: "hash", Balance: balance}
		if err := db.Create(&user).Error; err != nil {
			rt.Fatalf("failed to create user: %v", err)
		}

		ctx := context.Background()

		// Cache the balance in Redis
		bKey := balanceKey(userID)
		rdb.Set(ctx, bKey, strconv.FormatFloat(balance, 'f', -1, 64), 30*time.Second)

		// Manually insert expired holds using miniredis direct commands.
		// Expired holds have timestamps older than (now - holdTTL).
		now := time.Now().Unix()
		hKey := holdsKey(userID)
		tsKey := holdsTSKey(userID)

		var expiredTotal float64
		for i := 0; i < numExpired; i++ {
			reqID := fmt.Sprintf("expired-req-%d", i)
			amount := expiredAmounts[i]
			expiredTotal += amount

			// ZADD to sorted set
			mr.ZAdd(hKey, amount, reqID)
			// HSET timestamp that is older than TTL
			expiredTS := now - int64(holdTTLSec) - int64(rapid.IntRange(1, 3600).Draw(rt, fmt.Sprintf("expiredOffset_%d", i)))
			mr.HSet(tsKey, reqID, strconv.FormatInt(expiredTS, 10))
		}

		// Insert fresh holds (within TTL)
		var freshTotal float64
		for i := 0; i < numFresh; i++ {
			reqID := fmt.Sprintf("fresh-req-%d", i)
			amount := freshAmounts[i]
			freshTotal += amount

			// ZADD to sorted set
			mr.ZAdd(hKey, amount, reqID)
			// HSET timestamp that is within TTL (recent)
			freshTS := now - int64(rapid.IntRange(0, holdTTLSec-1).Draw(rt, fmt.Sprintf("freshOffset_%d", i)))
			mr.HSet(tsKey, reqID, strconv.FormatInt(freshTS, 10))
		}

		// Call GetBalance to trigger the cleanup
		available, err := ldg.GetBalance(ctx, userID)
		if err != nil {
			rt.Fatalf("GetBalance failed: %v", err)
		}

		// Assert: available balance correctly excludes only fresh holds (not expired ones)
		expectedAvailable := balance - freshTotal
		tolerance := 0.0001
		if diff := available - expectedAvailable; diff > tolerance || diff < -tolerance {
			rt.Fatalf("available balance mismatch: got %f, want %f (balance=%f, freshTotal=%f, expiredTotal=%f)",
				available, expectedAvailable, balance, freshTotal, expiredTotal)
		}

		// Assert: expired holds are removed from the sorted set
		for i := 0; i < numExpired; i++ {
			reqID := fmt.Sprintf("expired-req-%d", i)
			members, _ := mr.ZMembers(hKey)
			for _, m := range members {
				if m == reqID {
					rt.Fatalf("expired hold %q should have been removed from sorted set", reqID)
				}
			}
			// Also check timestamp hash
			if mr.HGet(tsKey, reqID) != "" {
				rt.Fatalf("expired hold %q should have been removed from timestamp hash", reqID)
			}
		}

		// Assert: fresh holds remain in the sorted set
		for i := 0; i < numFresh; i++ {
			reqID := fmt.Sprintf("fresh-req-%d", i)
			members, _ := mr.ZMembers(hKey)
			found := false
			for _, m := range members {
				if m == reqID {
					found = true
					break
				}
			}
			if !found {
				rt.Fatalf("fresh hold %q should still be in sorted set", reqID)
			}
			// Also check timestamp hash
			if mr.HGet(tsKey, reqID) == "" {
				rt.Fatalf("fresh hold %q should still be in timestamp hash", reqID)
			}
		}
	})
}

// TestProperty13_OrphanHoldTTLCleanup_ViaHold verifies that the Hold operation
// also cleans up expired holds before computing available balance.
//
// **Validates: Requirements 9.3, 9.4**
func TestProperty13_OrphanHoldTTLCleanup_ViaHold(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		balance := rapid.Float64Range(100.0, 10000.0).Draw(rt, "balance")
		holdTTLSec := rapid.IntRange(60, 600).Draw(rt, "holdTTLSec")
		holdTTL := time.Duration(holdTTLSec) * time.Second

		numExpired := rapid.IntRange(1, 5).Draw(rt, "numExpired")
		numFresh := rapid.IntRange(0, 5).Draw(rt, "numFresh")

		// Setup
		mr, err := miniredis.Run()
		if err != nil {
			rt.Fatalf("failed to start miniredis: %v", err)
		}
		defer mr.Close()

		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		defer rdb.Close()

		db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
		if err != nil {
			rt.Fatalf("failed to open sqlite: %v", err)
		}
		if err := db.AutoMigrate(&model.User{}, &model.BalanceLog{}); err != nil {
			rt.Fatalf("failed to migrate: %v", err)
		}

		ldg := NewWithConfig(db, rdb, 30*time.Second, holdTTL)

		userID := uint(1)
		user := model.User{ID: userID, Email: "test@test.com", PasswordHash: "hash", Balance: balance}
		if err := db.Create(&user).Error; err != nil {
			rt.Fatalf("failed to create user: %v", err)
		}

		ctx := context.Background()

		// Cache balance
		bKey := balanceKey(userID)
		rdb.Set(ctx, bKey, strconv.FormatFloat(balance, 'f', -1, 64), 30*time.Second)

		now := time.Now().Unix()
		hKey := holdsKey(userID)
		tsKey := holdsTSKey(userID)

		// Insert expired holds with large amounts to "freeze" balance
		for i := 0; i < numExpired; i++ {
			reqID := fmt.Sprintf("expired-hold-%d", i)
			amount := rapid.Float64Range(1.0, 10.0).Draw(rt, fmt.Sprintf("expAmt_%d", i))

			mr.ZAdd(hKey, amount, reqID)
			expiredTS := now - int64(holdTTLSec) - int64(rapid.IntRange(1, 3600).Draw(rt, fmt.Sprintf("expOff_%d", i)))
			mr.HSet(tsKey, reqID, strconv.FormatInt(expiredTS, 10))
		}

		// Insert fresh holds
		var freshTotal float64
		for i := 0; i < numFresh; i++ {
			reqID := fmt.Sprintf("fresh-hold-%d", i)
			amount := rapid.Float64Range(0.01, 2.0).Draw(rt, fmt.Sprintf("frAmt_%d", i))
			freshTotal += amount

			mr.ZAdd(hKey, amount, reqID)
			freshTS := now - int64(rapid.IntRange(0, holdTTLSec-1).Draw(rt, fmt.Sprintf("frOff_%d", i)))
			mr.HSet(tsKey, reqID, strconv.FormatInt(freshTS, 10))
		}

		// New hold amount that should succeed because expired holds are cleaned up
		// Available after cleanup = balance - freshTotal
		availableAfterCleanup := balance - freshTotal
		if availableAfterCleanup <= 0.01 {
			// Skip this iteration if fresh holds already consume all balance
			return
		}
		newHoldAmount := rapid.Float64Range(0.01, availableAfterCleanup*0.5).Draw(rt, "newHoldAmount")

		// Execute Hold — this should trigger cleanup of expired holds
		newReqID := "new-hold-request"
		err = ldg.Hold(ctx, userID, newHoldAmount, newReqID, holdTTL)
		if err != nil {
			rt.Fatalf("Hold should succeed after cleaning expired holds: %v (balance=%f, freshTotal=%f, newAmount=%f)",
				err, balance, freshTotal, newHoldAmount)
		}

		// Assert: expired holds are removed
		for i := 0; i < numExpired; i++ {
			reqID := fmt.Sprintf("expired-hold-%d", i)
			members, _ := mr.ZMembers(hKey)
			for _, m := range members {
				if m == reqID {
					rt.Fatalf("expired hold %q should have been cleaned up by Hold", reqID)
				}
			}
			if mr.HGet(tsKey, reqID) != "" {
				rt.Fatalf("expired hold %q timestamp should have been cleaned up", reqID)
			}
		}

		// Assert: fresh holds remain
		for i := 0; i < numFresh; i++ {
			reqID := fmt.Sprintf("fresh-hold-%d", i)
			members, _ := mr.ZMembers(hKey)
			found := false
			for _, m := range members {
				if m == reqID {
					found = true
					break
				}
			}
			if !found {
				rt.Fatalf("fresh hold %q should still exist after Hold cleanup", reqID)
			}
		}

		// Assert: new hold is present
		members, _ := mr.ZMembers(hKey)
		foundNew := false
		for _, m := range members {
			if m == newReqID {
				foundNew = true
				break
			}
		}
		if !foundNew {
			rt.Fatalf("new hold %q should be in sorted set after successful Hold", newReqID)
		}
	})
}


// Feature: billing-system-optimization, Property 2: Concurrent Hold Mutual Exclusion

// TestProperty2_ConcurrentHoldMutualExclusion verifies that when a user's
// available balance is sufficient for exactly one of two concurrent Hold
// requests, exactly one succeeds and the other fails with ErrInsufficientBalance.
//
// **Validates: Requirements 1.2**
func TestProperty2_ConcurrentHoldMutualExclusion(t *testing.T) {
	const iterations = 200
	for i := 0; i < iterations; i++ {
		func() {
			rng := rand.New(rand.NewSource(int64(i)))
			// Generate a random positive balance between 1.0 and 10000.0
			balance := math.Round((rng.Float64()*9999.0+1.0)*100) / 100

			// Set up infrastructure: fresh miniredis + SQLite per iteration
			srv, err := miniredis.Run()
			if err != nil {
				t.Fatalf("failed to start miniredis: %v", err)
			}
			defer srv.Close()

			rClient := redis.NewClient(&redis.Options{Addr: srv.Addr()})
			defer rClient.Close()

			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
				Logger: logger.Discard,
			})
			if err != nil {
				t.Fatalf("failed to open sqlite: %v", err)
			}
			if err := db.AutoMigrate(&model.User{}, &model.BalanceLog{}); err != nil {
				t.Fatalf("failed to migrate: %v", err)
			}

			// Create user with the generated balance
			user := model.User{
				ID:           1,
				Email:        "test@example.com",
				PasswordHash: "hash",
				Balance:      balance,
			}
			if err := db.Create(&user).Error; err != nil {
				t.Fatalf("failed to create user: %v", err)
			}

			ctx := context.Background()

			// Create ledger
			ldg := NewWithConfig(db, rClient, 30*time.Second, 5*time.Minute)

			// Pre-cache the balance in Redis
			if err := ldg.refreshBalanceCache(ctx, user.ID); err != nil {
				t.Fatalf("failed to refresh balance cache: %v", err)
			}

			// Both goroutines attempt to Hold the full balance amount.
			// Only one should succeed since the balance covers exactly one hold.
			holdAmount := balance
			reqID1 := fmt.Sprintf("req-1-%d", i)
			reqID2 := fmt.Sprintf("req-2-%d", i)

			var wg sync.WaitGroup
			var err1, err2 error

			wg.Add(2)
			go func() {
				defer wg.Done()
				err1 = ldg.Hold(ctx, user.ID, holdAmount, reqID1, 5*time.Minute)
			}()
			go func() {
				defer wg.Done()
				err2 = ldg.Hold(ctx, user.ID, holdAmount, reqID2, 5*time.Minute)
			}()
			wg.Wait()

			// Exactly one must succeed and one must fail with ErrInsufficientBalance
			oneSucceeded := (err1 == nil && errors.Is(err2, ErrInsufficientBalance)) ||
				(err2 == nil && errors.Is(err1, ErrInsufficientBalance))

			if !oneSucceeded {
				t.Fatalf("iteration %d: expected exactly one success and one ErrInsufficientBalance, got err1=%v, err2=%v (balance=%f)",
					i, err1, err2, balance)
			}
		}()
	}
}

// Feature: billing-system-optimization, Property 3: Cache Invalidation on Balance Mutation

// TestProperty3_CacheInvalidationOnBalanceMutation verifies that after any
// Credit or Debit operation that modifies a user's persistent balance, the
// Redis cached balance key is deleted, ensuring subsequent Hold operations
// read the updated value.
//
// **Validates: Requirements 1.7**
func TestProperty3_CacheInvalidationOnBalanceMutation(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate random test parameters
		balance := rapid.Float64Range(10.0, 10000.0).Draw(rt, "balance")
		mutationAmount := rapid.Float64Range(0.01, balance*0.5).Draw(rt, "mutationAmount")
		useCredit := rapid.Bool().Draw(rt, "useCredit")

		// Set up miniredis
		srv, err := miniredis.Run()
		if err != nil {
			rt.Fatalf("failed to start miniredis: %v", err)
		}
		defer srv.Close()

		rClient := redis.NewClient(&redis.Options{Addr: srv.Addr()})
		defer rClient.Close()

		// Set up SQLite with a user having balance B
		db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
			Logger: logger.Discard,
		})
		if err != nil {
			rt.Fatalf("failed to open sqlite: %v", err)
		}
		if err := db.AutoMigrate(&model.User{}, &model.BalanceLog{}); err != nil {
			rt.Fatalf("failed to migrate: %v", err)
		}

		userID := uint(1)
		user := model.User{
			ID:           userID,
			Email:        "cache-test@example.com",
			PasswordHash: "hash",
			Balance:      balance,
		}
		if err := db.Create(&user).Error; err != nil {
			rt.Fatalf("failed to create user: %v", err)
		}

		// Create ledger
		ldg := NewWithConfig(db, rClient, 30*time.Second, 5*time.Minute)
		ctx := context.Background()

		// Pre-populate the Redis balance cache key with the current balance
		bKey := balanceKey(userID)
		rClient.Set(ctx, bKey, strconv.FormatFloat(balance, 'f', -1, 64), 30*time.Second)

		// Verify the cache key exists before mutation
		if !srv.Exists(bKey) {
			rt.Fatalf("balance cache key should exist before mutation")
		}

		// Call Credit or Debit (randomly chosen)
		ref := fmt.Sprintf("prop3-test-%v", useCredit)
		if useCredit {
			err = ldg.Credit(ctx, userID, mutationAmount, ref)
		} else {
			err = ldg.Debit(ctx, userID, mutationAmount, ref)
		}
		if err != nil {
			rt.Fatalf("balance mutation failed: %v (useCredit=%v, balance=%f, amount=%f)",
				err, useCredit, balance, mutationAmount)
		}

		// Assert: the Redis balance cache key no longer exists after the operation
		if srv.Exists(bKey) {
			action := "Debit"
			if useCredit {
				action = "Credit"
			}
			rt.Fatalf("balance cache key %q should be deleted after %s (balance=%f, amount=%f)",
				bKey, action, balance, mutationAmount)
		}
	})
}
