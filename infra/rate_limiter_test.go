package infra

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xxww0098/cpa-gateway/config"
	"github.com/xxww0098/cpa-gateway/testutil"
	"pgregory.net/rapid"
)

// Feature: billing-system-optimization, Property 10: Distributed Rate Limiter Atomicity
//
// Property 10: For any set of concurrent requests from the same user across
// multiple gateway instances sharing a Redis backend, the total number of
// allowed requests SHALL not exceed the configured per-user limit within any
// time window.
//
// **Validates: Requirements 5.3, 5.8**

func TestProperty10_DistributedRateLimiterAtomicity(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate random rate limit between 5 and 50 requests per minute.
		maxReq := rapid.IntRange(5, 50).Draw(rt, "maxRequestsPerMin")

		// Number of simulated gateway instances (goroutines): 2-10.
		numInstances := rapid.IntRange(2, 10).Draw(rt, "numInstances")

		// Total requests to attempt: 2x-4x the limit to ensure we exceed it.
		multiplier := rapid.IntRange(2, 4).Draw(rt, "requestMultiplier")
		totalRequests := maxReq * multiplier

		// Distribute requests across instances.
		requestsPerInstance := totalRequests / numInstances

		// Set up miniredis and rate limiter.
		client, _ := testutil.MustMiniRedis(t)

		cfg := config.RateLimitConfig{
			RequestsPerMin:   maxReq,
			TokensPerMin:     1000000, // High token limit so it doesn't interfere.
			MaxConcurrent:    1000,    // High concurrent limit so it doesn't interfere.
			BurstSize:        2,
			GlobalRequestCap: 100000, // High global cap so it doesn't interfere.
			GlobalTokenCap:   100000000,
		}

		rl := NewRateLimiter(client, cfg)

		identity := "test-user-property10"
		ctx := context.Background()

		// Launch N goroutines simulating multiple gateway instances.
		var allowed atomic.Int64
		var wg sync.WaitGroup

		for i := 0; i < numInstances; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < requestsPerInstance; j++ {
					ok, err := rl.Allow(ctx, identity, 1, "", nil)
					if err != nil {
						rt.Fatalf("unexpected error from Allow: %v", err)
						return
					}
					if ok {
						allowed.Add(1)
					}
				}
			}()
		}

		wg.Wait()

		totalAllowed := allowed.Load()
		if totalAllowed > int64(maxReq) {
			rt.Fatalf("Property 10 violated: allowed %d requests but limit is %d (instances=%d, totalAttempts=%d)",
				totalAllowed, maxReq, numInstances, totalRequests)
		}
	})
}

// Feature: billing-system-optimization, Property 11: Multi-Dimensional Rate Limiting

// TestProperty11_MultiDimensionalRateLimiting verifies that the rate limiter
// rejects a request if ANY of the three dimensions (request count, token
// consumption, concurrent requests) exceeds its configured limit, even if the
// other dimensions are within bounds.
//
// **Validates: Requirements 5.4**
func TestProperty11_MultiDimensionalRateLimiting(t *testing.T) {
	t.Run("request_count_exceeded_denies_even_if_token_and_concurrent_ok", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			// Generate a small request limit so we can exceed it quickly.
			maxReq := rapid.IntRange(1, 10).Draw(rt, "maxReq")
			// Token and concurrent limits are generous — they should NOT trigger.
			maxTok := int64(rapid.IntRange(100000, 1000000).Draw(rt, "maxTok"))
			maxConc := rapid.IntRange(100, 500).Draw(rt, "maxConc")

			client, _ := testutil.MustMiniRedis(t)

			cfg := config.RateLimitConfig{
				RequestsPerMin:   maxReq,
				TokensPerMin:     maxTok,
				MaxConcurrent:    maxConc,
				BurstSize:        2,
				GlobalRequestCap: 100000, // very high, won't trigger
				GlobalTokenCap:   100000000,
			}

			rl := NewRateLimiter(client, cfg)
			ctx := context.Background()
			identity := "user-prop11-req"

			// Use a small token count per request so token limit is never hit.
			tokenCount := int64(1)

			// Fill up the request count dimension to the limit.
			for i := 0; i < maxReq; i++ {
				allowed, err := rl.Allow(ctx, identity, tokenCount, "gpt-4o", nil)
				if err != nil {
					rt.Fatalf("unexpected error on request %d: %v", i, err)
				}
				if !allowed {
					rt.Fatalf("request %d should be allowed (limit=%d)", i, maxReq)
				}
			}

			// The next request should be denied due to request count, even though
			// token and concurrent limits are well within bounds.
			allowed, err := rl.Allow(ctx, identity, tokenCount, "gpt-4o", nil)
			if err != nil {
				rt.Fatalf("unexpected error on overflow request: %v", err)
			}
			if allowed {
				rt.Fatalf("request should be DENIED after exceeding request count limit (%d), but was allowed", maxReq)
			}
		})
	})

	t.Run("token_limit_exceeded_denies_even_if_request_count_and_concurrent_ok", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			// Token limit is small so we can exceed it in a few requests.
			maxTok := int64(rapid.IntRange(100, 1000).Draw(rt, "maxTok"))
			// Request count and concurrent limits are generous.
			maxReq := rapid.IntRange(1000, 10000).Draw(rt, "maxReq")
			maxConc := rapid.IntRange(100, 500).Draw(rt, "maxConc")

			client, _ := testutil.MustMiniRedis(t)

			cfg := config.RateLimitConfig{
				RequestsPerMin:   maxReq,
				TokensPerMin:     maxTok,
				MaxConcurrent:    maxConc,
				BurstSize:        2,
				GlobalRequestCap: 100000,
				GlobalTokenCap:   100000000,
			}

			rl := NewRateLimiter(client, cfg)
			ctx := context.Background()
			identity := "user-prop11-tok"

			// Each request consumes a large chunk of the token budget.
			// We'll use a token count that fills the budget in 2 requests.
			tokenPerReq := maxTok/2 + 1

			// First request should succeed (tokens used = tokenPerReq <= maxTok).
			allowed, err := rl.Allow(ctx, identity, tokenPerReq, "gpt-4o", nil)
			if err != nil {
				rt.Fatalf("unexpected error on first request: %v", err)
			}
			if !allowed {
				rt.Fatalf("first request should be allowed (tokens=%d, limit=%d)", tokenPerReq, maxTok)
			}

			// Second request should be denied because total tokens would exceed limit.
			allowed, err = rl.Allow(ctx, identity, tokenPerReq, "gpt-4o", nil)
			if err != nil {
				rt.Fatalf("unexpected error on second request: %v", err)
			}
			if allowed {
				rt.Fatalf("request should be DENIED after exceeding token limit (used=%d + new=%d > limit=%d), but was allowed",
					tokenPerReq, tokenPerReq, maxTok)
			}
		})
	})

	t.Run("concurrent_limit_exceeded_denies_even_if_request_count_and_token_ok", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			// Concurrent limit is small so we can exceed it.
			maxConc := rapid.IntRange(1, 10).Draw(rt, "maxConc")
			// Request count and token limits are generous.
			maxReq := rapid.IntRange(10000, 100000).Draw(rt, "maxReq")
			maxTok := int64(rapid.IntRange(1000000, 10000000).Draw(rt, "maxTok"))

			client, _ := testutil.MustMiniRedis(t)

			cfg := config.RateLimitConfig{
				RequestsPerMin:   maxReq,
				TokensPerMin:     maxTok,
				MaxConcurrent:    maxConc,
				BurstSize:        2,
				GlobalRequestCap: 1000000,
				GlobalTokenCap:   1000000000,
			}

			rl := NewRateLimiter(client, cfg)
			ctx := context.Background()
			identity := "user-prop11-conc"

			// Fill up the concurrent slots by making requests without releasing them.
			// Each Allow() adds the request to the concurrent set.
			for i := 0; i < maxConc; i++ {
				allowed, err := rl.Allow(ctx, identity, 1, "gpt-4o", nil)
				if err != nil {
					rt.Fatalf("unexpected error on request %d: %v", i, err)
				}
				if !allowed {
					rt.Fatalf("request %d should be allowed (concurrent limit=%d)", i, maxConc)
				}
			}

			// The next request should be denied due to concurrent limit, even though
			// request count and token limits are well within bounds.
			allowed, err := rl.Allow(ctx, identity, 1, "gpt-4o", nil)
			if err != nil {
				rt.Fatalf("unexpected error on overflow request: %v", err)
			}
			if allowed {
				rt.Fatalf("request should be DENIED after exceeding concurrent limit (%d), but was allowed", maxConc)
			}
		})
	})
}
