package infra

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/xxww0098/cpa-gateway/config"
	"github.com/xxww0098/cpa-gateway/testutil"
	"pgregory.net/rapid"
)

// Feature: billing-system-optimization, Property 21: Circuit Breaker State Transitions
//
// Property 21: For any provider, when the failure rate within the configured
// window exceeds the threshold, the circuit SHALL transition to open; while
// open, all requests SHALL be rejected without upstream calls; after the
// cooldown period, the circuit SHALL transition to half-open.
//
// **Validates: Requirements 12.2, 12.3, 12.4**

func TestProperty21_CircuitBreakerStateTransitions(t *testing.T) {
	t.Run("failure_rate_above_threshold_transitions_to_open", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			// Generate a failure threshold between 0.3 and 0.8.
			threshold := float64(rapid.IntRange(30, 80).Draw(rt, "thresholdPct")) / 100.0

			// We need at least 5 samples. Generate total between 5 and 20.
			total := rapid.IntRange(5, 20).Draw(rt, "totalRequests")

			// Calculate the minimum number of failures needed to exceed threshold.
			minFailures := int(float64(total)*threshold) + 1
			if minFailures > total {
				minFailures = total
			}

			// Number of successes = total - failures.
			successes := total - minFailures

			client, _ := testutil.MustMiniRedis(t)

			cfg := config.CircuitBreakerConfig{
				FailureThreshold: threshold,
				WindowSeconds:    30,
				CooldownSeconds:  30,
			}

			cb := NewCircuitBreaker(client, cfg)
			ctx := context.Background()
			provider := "test-provider-open"

			// Record successes first.
			for i := 0; i < successes; i++ {
				if err := cb.RecordSuccess(ctx, provider); err != nil {
					rt.Fatalf("unexpected error recording success: %v", err)
				}
			}

			// Record failures to exceed threshold.
			for i := 0; i < minFailures; i++ {
				if err := cb.RecordFailure(ctx, provider); err != nil {
					rt.Fatalf("unexpected error recording failure: %v", err)
				}
			}

			// Verify circuit is now open.
			state, err := cb.State(ctx, provider)
			if err != nil {
				rt.Fatalf("unexpected error getting state: %v", err)
			}
			if state != CircuitOpen {
				rt.Fatalf("expected circuit to be open after failure rate %.2f > threshold %.2f (failures=%d, total=%d), got state=%s",
					float64(minFailures)/float64(total), threshold, minFailures, total, state)
			}
		})
	})

	t.Run("open_state_rejects_all_requests", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			// Number of requests to attempt while circuit is open.
			numRequests := rapid.IntRange(1, 20).Draw(rt, "numRequests")

			client, _ := testutil.MustMiniRedis(t)

			cfg := config.CircuitBreakerConfig{
				FailureThreshold: 0.5,
				WindowSeconds:    30,
				CooldownSeconds:  60, // Long cooldown so it stays open.
			}

			cb := NewCircuitBreaker(client, cfg)
			ctx := context.Background()
			provider := "test-provider-reject"

			// Trip the circuit: record 5 failures (100% failure rate > 50% threshold).
			for i := 0; i < 5; i++ {
				if err := cb.RecordFailure(ctx, provider); err != nil {
					rt.Fatalf("unexpected error recording failure: %v", err)
				}
			}

			// Verify circuit is open.
			state, err := cb.State(ctx, provider)
			if err != nil {
				rt.Fatalf("unexpected error getting state: %v", err)
			}
			if state != CircuitOpen {
				rt.Fatalf("expected circuit to be open, got %s", state)
			}

			// All subsequent Allow() calls should return false.
			for i := 0; i < numRequests; i++ {
				allowed, err := cb.Allow(ctx, provider)
				if err != nil {
					rt.Fatalf("unexpected error on Allow: %v", err)
				}
				if allowed {
					rt.Fatalf("request %d should be REJECTED while circuit is open, but was allowed", i)
				}
			}
		})
	})

	t.Run("after_cooldown_transitions_to_half_open", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			// Generate a cooldown period between 1 and 10 seconds.
			cooldownSec := rapid.IntRange(1, 10).Draw(rt, "cooldownSeconds")

			client, _ := testutil.MustMiniRedis(t)

			cfg := config.CircuitBreakerConfig{
				FailureThreshold: 0.5,
				WindowSeconds:    30,
				CooldownSeconds:  cooldownSec,
			}

			cb := NewCircuitBreaker(client, cfg)
			ctx := context.Background()
			provider := "test-provider-halfopen"

			// Trip the circuit: record 5 failures (100% failure rate).
			for i := 0; i < 5; i++ {
				if err := cb.RecordFailure(ctx, provider); err != nil {
					rt.Fatalf("unexpected error recording failure: %v", err)
				}
			}

			// Verify circuit is open.
			state, err := cb.State(ctx, provider)
			if err != nil {
				rt.Fatalf("unexpected error getting state: %v", err)
			}
			if state != CircuitOpen {
				rt.Fatalf("expected circuit to be open, got %s", state)
			}

			// Before cooldown: should still reject.
			allowed, err := cb.Allow(ctx, provider)
			if err != nil {
				rt.Fatalf("unexpected error on Allow before cooldown: %v", err)
			}
			if allowed {
				rt.Fatalf("request should be rejected before cooldown expires")
			}

			// Simulate cooldown expiry by backdating the opened_at timestamp.
			// The circuit breaker checks time.Since(opened_at) >= cooldownPeriod,
			// so we set opened_at to (now - cooldown - 1s) to simulate expiry.
			key := circuitKey(provider)
			pastTime := time.Now().Add(-time.Duration(cooldownSec)*time.Second - time.Second).Unix()
			client.HSet(ctx, key, "opened_at", strconv.FormatInt(pastTime, 10))

			// After cooldown: Allow() should transition to half-open and return true.
			allowed, err = cb.Allow(ctx, provider)
			if err != nil {
				rt.Fatalf("unexpected error on Allow after cooldown: %v", err)
			}
			if !allowed {
				rt.Fatalf("request should be ALLOWED after cooldown (half-open probe), but was rejected")
			}

			// Verify state is now half-open.
			state, err = cb.State(ctx, provider)
			if err != nil {
				rt.Fatalf("unexpected error getting state after cooldown: %v", err)
			}
			if state != CircuitHalfOpen {
				rt.Fatalf("expected circuit to be half_open after cooldown, got %s", state)
			}
		})
	})

	t.Run("record_success_in_half_open_transitions_to_closed", func(t *testing.T) {
		rapid.Check(t, func(rt *rapid.T) {
			cooldownSec := rapid.IntRange(1, 5).Draw(rt, "cooldownSeconds")

			client, _ := testutil.MustMiniRedis(t)

			cfg := config.CircuitBreakerConfig{
				FailureThreshold: 0.5,
				WindowSeconds:    30,
				CooldownSeconds:  cooldownSec,
			}

			cb := NewCircuitBreaker(client, cfg)
			ctx := context.Background()
			provider := fmt.Sprintf("test-provider-recovery-%d", cooldownSec)

			// Trip the circuit.
			for i := 0; i < 5; i++ {
				if err := cb.RecordFailure(ctx, provider); err != nil {
					rt.Fatalf("unexpected error recording failure: %v", err)
				}
			}

			// Simulate cooldown expiry by backdating the opened_at timestamp.
			key := circuitKey(provider)
			pastTime := time.Now().Add(-time.Duration(cooldownSec)*time.Second - time.Second).Unix()
			client.HSet(ctx, key, "opened_at", strconv.FormatInt(pastTime, 10))

			// Allow the probe request (transitions to half-open).
			allowed, err := cb.Allow(ctx, provider)
			if err != nil {
				rt.Fatalf("unexpected error on Allow: %v", err)
			}
			if !allowed {
				rt.Fatalf("probe request should be allowed in half-open state")
			}

			// Record success — should transition back to closed.
			if err := cb.RecordSuccess(ctx, provider); err != nil {
				rt.Fatalf("unexpected error recording success: %v", err)
			}

			// Verify state is now closed.
			state, err := cb.State(ctx, provider)
			if err != nil {
				rt.Fatalf("unexpected error getting state: %v", err)
			}
			if state != CircuitClosed {
				rt.Fatalf("expected circuit to be closed after successful probe, got %s", state)
			}

			// Verify requests are allowed again.
			allowed, err = cb.Allow(ctx, provider)
			if err != nil {
				rt.Fatalf("unexpected error on Allow after recovery: %v", err)
			}
			if !allowed {
				rt.Fatalf("requests should be allowed after circuit recovers to closed state")
			}
		})
	})
}
