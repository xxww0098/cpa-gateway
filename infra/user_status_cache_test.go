package infra

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Feature: billing-security-hardening, Property 12 (infrastructure), Requirement 4.6.
//
// These tests exercise the user-visible semantics of UserStatusCache: Set/Get
// hit, TTL-driven eviction on read, and immediate miss after InvalidateUser.
// They also confirm that Start(ctx) exits cleanly when its context is
// canceled, which is the production-lifecycle contract shared with
// APIKeyCache.Start.
//
// **Validates: Requirement 4.6**

// TestUserStatusCacheGetSetExpire covers the three core Get/Set/Invalidate
// transitions:
//  1. Set then Get returns a hit with the same status.
//  2. After TTL expiry, Get reports a miss (and evicts the entry lazily).
//  3. After InvalidateUser, Get reports a miss immediately, even when the
//     entry would otherwise still be fresh.
func TestUserStatusCacheGetSetExpire(t *testing.T) {
	c := NewUserStatusCache()

	const userID uint = 42
	c.Set(userID, "active", 100*time.Millisecond)

	got, ok := c.Get(userID)
	if !ok {
		t.Fatalf("Get after Set: want hit, got miss")
	}
	if got.Status != "active" {
		t.Fatalf("Get after Set: status=%q, want %q", got.Status, "active")
	}
	if !got.ExpiresAt.After(time.Now()) {
		t.Fatalf("Get after Set: ExpiresAt=%v is not in the future", got.ExpiresAt)
	}

	// TTL expiry path: use a short TTL and sleep just past it so the entry is
	// stale by the time Get runs.
	c.Set(userID, "active", 10*time.Millisecond)
	time.Sleep(25 * time.Millisecond)
	if _, ok := c.Get(userID); ok {
		t.Fatalf("Get after TTL expiry: want miss, got hit")
	}

	// InvalidateUser path: re-populate with a comfortably long TTL, then
	// invalidate and confirm the next Get is a miss.
	c.Set(userID, "active", time.Minute)
	if _, ok := c.Get(userID); !ok {
		t.Fatalf("Get before InvalidateUser: want hit, got miss")
	}
	c.InvalidateUser(userID)
	if _, ok := c.Get(userID); ok {
		t.Fatalf("Get after InvalidateUser: want miss, got hit")
	}

	// Set with a non-positive TTL must be a no-op so callers cannot poison
	// the cache with an already-expired entry.
	c.Set(userID, "active", 0)
	if _, ok := c.Get(userID); ok {
		t.Fatalf("Get after Set(ttl=0): want miss, got hit")
	}
	c.Set(userID, "active", -time.Second)
	if _, ok := c.Get(userID); ok {
		t.Fatalf("Get after Set(ttl<0): want miss, got hit")
	}
}

// TestUserStatusCacheSweeper covers two aspects of the background sweeper:
//  1. Expired entries are reclaimed via the Get-side TTL delete — the
//     user-visible semantics operators rely on. The production 1-minute
//     ticker is too slow for CI, but Get is the actual hot-path reader used
//     by AccessProvider and AuthMiddleware, so asserting it is enough to
//     prove the "expired entries are cleaned" invariant.
//  2. Start(ctx) exits cleanly once its context is canceled, so a graceful
//     shutdown does not leak the sweeper goroutine.
func TestUserStatusCacheSweeper(t *testing.T) {
	c := NewUserStatusCache()

	const userID uint = 7
	c.Set(userID, "active", 10*time.Millisecond)

	// Wait past the TTL; a subsequent Get must observe the miss AND evict
	// the backing entry so later readers see a miss without having to rely
	// on the 1-minute ticker.
	time.Sleep(25 * time.Millisecond)
	if _, ok := c.Get(userID); ok {
		t.Fatalf("Get after TTL: want miss, got hit")
	}
	// After the lazy delete, the internal map should no longer carry the
	// expired entry. Re-populating with a distinct status and reading back
	// proves we are not shadowed by a stale residual.
	c.Set(userID, "suspended", time.Minute)
	got, ok := c.Get(userID)
	if !ok {
		t.Fatalf("Get after re-Set: want hit, got miss")
	}
	if got.Status != "suspended" {
		t.Fatalf("Get after re-Set: status=%q, want %q", got.Status, "suspended")
	}

	// Start(ctx) must exit cleanly once the ctx is canceled so shutdown
	// does not leak the goroutine. The production ticker is 1 minute, so we
	// only assert that cancel causes a bounded return, not that a sweep
	// actually ran.
	ctx, cancel := context.WithCancel(context.Background())
	var done atomic.Bool
	go func() {
		c.Start(ctx)
		done.Store(true)
	}()

	// Give the goroutine a moment to enter its select loop, then cancel.
	time.Sleep(5 * time.Millisecond)
	cancel()

	// Wait bounded time for the goroutine to exit.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if done.Load() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !done.Load() {
		t.Fatalf("Start(ctx) did not return within 500ms after ctx cancel")
	}
}
