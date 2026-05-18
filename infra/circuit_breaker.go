package infra

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/xxww0098/cpa-gateway/config"
)

// CircuitState represents the state of a circuit breaker.
type CircuitState string

const (
	// CircuitClosed means the circuit is healthy and requests are allowed.
	CircuitClosed CircuitState = "closed"
	// CircuitOpen means the circuit has tripped and requests are rejected.
	CircuitOpen CircuitState = "open"
	// CircuitHalfOpen means the circuit is testing recovery with a probe request.
	CircuitHalfOpen CircuitState = "half_open"
)

// CircuitBreaker implements a per-provider circuit breaker backed by Redis Hash.
// Each provider's state is stored in a Redis hash at key
// "cpa-gateway:circuit:{provider}" with fields: state, failures, successes,
// total, opened_at.
type CircuitBreaker struct {
	redis            *redis.Client
	failureThreshold float64       // failure rate threshold to trip (0.0–1.0)
	windowSize       time.Duration // observation window for failure rate
	cooldownPeriod   time.Duration // time to wait in open state before half-open
}

// NewCircuitBreaker creates a CircuitBreaker backed by the given Redis client.
// Zero-value config fields are replaced with sensible defaults.
func NewCircuitBreaker(redisClient *redis.Client, cfg config.CircuitBreakerConfig) *CircuitBreaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 0.5
	}
	if cfg.WindowSeconds <= 0 {
		cfg.WindowSeconds = 30
	}
	if cfg.CooldownSeconds <= 0 {
		cfg.CooldownSeconds = 30
	}

	return &CircuitBreaker{
		redis:            redisClient,
		failureThreshold: cfg.FailureThreshold,
		windowSize:       time.Duration(cfg.WindowSeconds) * time.Second,
		cooldownPeriod:   time.Duration(cfg.CooldownSeconds) * time.Second,
	}
}

// circuitKey returns the Redis hash key for a provider's circuit breaker state.
func circuitKey(provider string) string {
	return fmt.Sprintf("cpa-gateway:circuit:%s", provider)
}

// Allow checks whether a request to the given provider is permitted.
//
// State logic:
//   - closed: always allow
//   - open: check if cooldown has expired; if so, transition to half_open and allow one probe
//   - half_open: allow (single probe already in progress)
//
// On Redis failure, returns (true, nil) — fail-open for availability.
func (cb *CircuitBreaker) Allow(ctx context.Context, provider string) (bool, error) {
	if cb.redis == nil {
		return true, nil
	}

	key := circuitKey(provider)

	vals, err := cb.redis.HGetAll(ctx, key).Result()
	if err != nil {
		slog.Warn("CircuitBreaker.Allow: Redis error, allowing request (fail-open)",
			"error", err, "provider", provider)
		return true, nil
	}

	// No state stored yet — treat as closed.
	if len(vals) == 0 {
		return true, nil
	}

	state := CircuitState(vals["state"])

	switch state {
	case CircuitOpen:
		// Check if cooldown has expired.
		openedAtStr := vals["opened_at"]
		if openedAtStr == "" {
			// Malformed state — reset to closed.
			cb.resetState(ctx, key)
			return true, nil
		}
		openedAt, parseErr := strconv.ParseInt(openedAtStr, 10, 64)
		if parseErr != nil {
			cb.resetState(ctx, key)
			return true, nil
		}
		elapsed := time.Since(time.Unix(openedAt, 0))
		if elapsed >= cb.cooldownPeriod {
			// Transition to half-open, allow probe.
			pipe := cb.redis.TxPipeline()
			pipe.HSet(ctx, key, "state", string(CircuitHalfOpen))
			ttl := cb.cooldownPeriod + cb.windowSize
			pipe.Expire(ctx, key, ttl)
			if _, pipeErr := pipe.Exec(ctx); pipeErr != nil {
				slog.Warn("CircuitBreaker.Allow: failed to transition to half_open",
					"error", pipeErr, "provider", provider)
			}
			return true, nil
		}
		// Still within cooldown — reject.
		return false, nil

	case CircuitHalfOpen:
		// Allow the probe request through.
		return true, nil

	default:
		// closed or unknown — allow.
		return true, nil
	}
}

// RecordSuccess records a successful request to the provider.
//
//   - In half_open state: transitions to closed and resets counters.
//   - In closed state: increments the success counter.
func (cb *CircuitBreaker) RecordSuccess(ctx context.Context, provider string) error {
	if cb.redis == nil {
		return nil
	}

	key := circuitKey(provider)

	vals, err := cb.redis.HGetAll(ctx, key).Result()
	if err != nil {
		slog.Warn("CircuitBreaker.RecordSuccess: Redis error", "error", err, "provider", provider)
		return fmt.Errorf("circuit breaker record success: %w", err)
	}

	state := CircuitState(vals["state"])

	if state == CircuitHalfOpen {
		// Probe succeeded — transition to closed, reset counters.
		cb.resetState(ctx, key)
		return nil
	}

	// Closed state: increment success and total counters.
	pipe := cb.redis.TxPipeline()
	pipe.HIncrBy(ctx, key, "successes", 1)
	pipe.HIncrBy(ctx, key, "total", 1)
	ttl := cb.cooldownPeriod + cb.windowSize
	pipe.Expire(ctx, key, ttl)
	if _, pipeErr := pipe.Exec(ctx); pipeErr != nil {
		slog.Warn("CircuitBreaker.RecordSuccess: pipeline error", "error", pipeErr, "provider", provider)
		return fmt.Errorf("circuit breaker record success pipeline: %w", pipeErr)
	}

	return nil
}

// RecordFailure records a failed request to the provider.
// It increments the failure and total counters, then checks whether the
// failure rate exceeds the threshold. If so, the circuit transitions to open.
func (cb *CircuitBreaker) RecordFailure(ctx context.Context, provider string) error {
	if cb.redis == nil {
		return nil
	}

	key := circuitKey(provider)

	pipe := cb.redis.TxPipeline()
	failCmd := pipe.HIncrBy(ctx, key, "failures", 1)
	totalCmd := pipe.HIncrBy(ctx, key, "total", 1)
	ttl := cb.cooldownPeriod + cb.windowSize
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Warn("CircuitBreaker.RecordFailure: pipeline error", "error", err, "provider", provider)
		return fmt.Errorf("circuit breaker record failure pipeline: %w", err)
	}

	failures := failCmd.Val()
	total := totalCmd.Val()

	// Need a minimum sample size before tripping.
	if total < 5 {
		return nil
	}

	failureRate := float64(failures) / float64(total)
	if failureRate >= cb.failureThreshold {
		// Trip the circuit — transition to open.
		now := time.Now().Unix()
		setErr := cb.redis.HSet(ctx, key,
			"state", string(CircuitOpen),
			"opened_at", strconv.FormatInt(now, 10),
		).Err()
		if setErr != nil {
			slog.Warn("CircuitBreaker.RecordFailure: failed to open circuit",
				"error", setErr, "provider", provider)
			return fmt.Errorf("circuit breaker open transition: %w", setErr)
		}
		slog.Warn("CircuitBreaker: circuit opened",
			"provider", provider, "failureRate", failureRate, "total", total)
	}

	return nil
}

// State returns the current circuit breaker state for the given provider.
// If no state is stored, returns CircuitClosed.
func (cb *CircuitBreaker) State(ctx context.Context, provider string) (CircuitState, error) {
	if cb.redis == nil {
		return CircuitClosed, nil
	}

	key := circuitKey(provider)

	stateStr, err := cb.redis.HGet(ctx, key, "state").Result()
	if err == redis.Nil {
		return CircuitClosed, nil
	}
	if err != nil {
		slog.Warn("CircuitBreaker.State: Redis error", "error", err, "provider", provider)
		return CircuitClosed, fmt.Errorf("circuit breaker state: %w", err)
	}

	if stateStr == "" {
		return CircuitClosed, nil
	}

	return CircuitState(stateStr), nil
}

// resetState resets the circuit breaker hash to a clean closed state.
func (cb *CircuitBreaker) resetState(ctx context.Context, key string) {
	pipe := cb.redis.TxPipeline()
	pipe.Del(ctx, key)
	pipe.HSet(ctx, key,
		"state", string(CircuitClosed),
		"failures", "0",
		"successes", "0",
		"total", "0",
		"opened_at", "",
	)
	ttl := cb.cooldownPeriod + cb.windowSize
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		slog.Warn("CircuitBreaker.resetState: pipeline error", "error", err, "key", key)
	}
}
