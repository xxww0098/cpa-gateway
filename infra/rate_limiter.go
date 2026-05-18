package infra

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/xxww0098/cpa-gateway/config"
)

// rateLimitScript is an atomic Lua script that performs sliding window +
// token bucket + concurrent counter checks in a single round-trip.
//
// KEYS[1] = cpa-gateway:ratelimit:req:{identity}   (sorted set: sliding window request count)
// KEYS[2] = cpa-gateway:ratelimit:tok:{identity}   (sorted set: sliding window token count)
// KEYS[3] = cpa-gateway:ratelimit:conc:{identity}  (set: concurrent request tracking)
// KEYS[4] = cpa-gateway:ratelimit:global:req       (sorted set: global request count)
// KEYS[5] = cpa-gateway:ratelimit:global:tok       (sorted set: global token count)
//
// ARGV[1] = current timestamp ms
// ARGV[2] = window size ms
// ARGV[3] = max requests per window
// ARGV[4] = token count for this request
// ARGV[5] = max tokens per window
// ARGV[6] = max concurrent
// ARGV[7] = request ID (unique member for sorted sets + concurrent set)
// ARGV[8] = global max requests per window (0 = disabled)
// ARGV[9] = global max tokens per window (0 = disabled)
//
// Returns: "ALLOWED" or "DENIED:{dimension}"
var rateLimitLua = `
local req_key   = KEYS[1]
local tok_key   = KEYS[2]
local conc_key  = KEYS[3]
local greq_key  = KEYS[4]
local gtok_key  = KEYS[5]

local now_ms       = tonumber(ARGV[1])
local window_ms    = tonumber(ARGV[2])
local max_req      = tonumber(ARGV[3])
local token_count  = tonumber(ARGV[4])
local max_tok      = tonumber(ARGV[5])
local max_conc     = tonumber(ARGV[6])
local request_id   = ARGV[7]
local global_max_req = tonumber(ARGV[8])
local global_max_tok = tonumber(ARGV[9])

local window_start = now_ms - window_ms
local expire_sec   = math.ceil(window_ms / 1000) + 60

-- 1. Clean expired entries from all sliding windows via ZREMRANGEBYSCORE.
--    All sorted sets use timestamp (ms) as score for uniform expiration.
redis.call('ZREMRANGEBYSCORE', req_key, '-inf', window_start)
redis.call('ZREMRANGEBYSCORE', tok_key, '-inf', window_start)
redis.call('ZREMRANGEBYSCORE', greq_key, '-inf', window_start)
redis.call('ZREMRANGEBYSCORE', gtok_key, '-inf', window_start)

-- 2. Check per-identity request count (sliding window).
local current_req = redis.call('ZCARD', req_key)
if current_req >= max_req then
    return "DENIED:request_count"
end

-- 3. Check per-identity token consumption (sliding window).
--    Token sorted set members are formatted as "requestID:tokenCount".
--    Score is the timestamp for expiration. We parse token counts from members.
local tok_members = redis.call('ZRANGEBYSCORE', tok_key, window_start, '+inf')
local current_tok = 0
for i = 1, #tok_members do
    local sep = string.find(tok_members[i], ":", 37)
    if sep then
        current_tok = current_tok + tonumber(string.sub(tok_members[i], sep + 1))
    end
end
if (current_tok + token_count) > max_tok then
    return "DENIED:token_limit"
end

-- 4. Check concurrent requests.
local current_conc = redis.call('SCARD', conc_key)
if current_conc >= max_conc then
    return "DENIED:concurrent"
end

-- 5. Check global request count (if enabled).
if global_max_req > 0 then
    local global_req = redis.call('ZCARD', greq_key)
    if global_req >= global_max_req then
        return "DENIED:global_request_count"
    end
end

-- 6. Check global token consumption (if enabled).
if global_max_tok > 0 then
    local gtok_members = redis.call('ZRANGEBYSCORE', gtok_key, window_start, '+inf')
    local global_tok = 0
    for i = 1, #gtok_members do
        local sep = string.find(gtok_members[i], ":", 37)
        if sep then
            global_tok = global_tok + tonumber(string.sub(gtok_members[i], sep + 1))
        end
    end
    if (global_tok + token_count) > global_max_tok then
        return "DENIED:global_token_limit"
    end
end

-- 7. All checks passed — record the request atomically.

-- Per-identity request window: member=requestID, score=timestamp_ms
redis.call('ZADD', req_key, now_ms, request_id)
redis.call('EXPIRE', req_key, expire_sec)

-- Per-identity token window: member="requestID:tokenCount", score=timestamp_ms
local tok_member = request_id .. ":" .. tostring(token_count)
redis.call('ZADD', tok_key, now_ms, tok_member)
redis.call('EXPIRE', tok_key, expire_sec)

-- Concurrent set: add request ID with 10-minute TTL for stale cleanup.
redis.call('SADD', conc_key, request_id)
redis.call('EXPIRE', conc_key, 600)

-- Global request window: member=requestID, score=timestamp_ms
redis.call('ZADD', greq_key, now_ms, request_id)
redis.call('EXPIRE', greq_key, expire_sec)

-- Global token window: member="requestID:tokenCount", score=timestamp_ms
redis.call('ZADD', gtok_key, now_ms, tok_member)
redis.call('EXPIRE', gtok_key, expire_sec)

return "ALLOWED"
`

// RateLimiter enforces multi-dimensional rate limits using Redis.
// It combines sliding window (request count + token consumption) with
// concurrent request tracking, all checked atomically via a Lua script.
type RateLimiter struct {
	redis  *redis.Client
	config config.RateLimitConfig
	script *redis.Script
}

// NewRateLimiter creates a RateLimiter backed by the given Redis client.
// cfg provides default limits and per-group/per-model overrides.
func NewRateLimiter(redisClient *redis.Client, cfg config.RateLimitConfig) *RateLimiter {
	// Apply defaults for zero-value config fields.
	if cfg.RequestsPerMin <= 0 {
		cfg.RequestsPerMin = 60
	}
	if cfg.TokensPerMin <= 0 {
		cfg.TokensPerMin = 100000
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 10
	}
	if cfg.BurstSize <= 0 {
		cfg.BurstSize = 2
	}
	if cfg.GlobalRequestCap <= 0 {
		cfg.GlobalRequestCap = 10000
	}
	if cfg.GlobalTokenCap <= 0 {
		cfg.GlobalTokenCap = 10000000
	}

	return &RateLimiter{
		redis:  redisClient,
		config: cfg,
		script: redis.NewScript(rateLimitLua),
	}
}

// Allow checks whether a request from the given identity is permitted under
// the configured rate limits. It resolves effective limits by checking
// per-group overrides and per-model token limits, then executes the atomic
// Lua script.
//
// Parameters:
//   - identity: unique identifier for the rate limit subject (e.g. userID, apiKeyID)
//   - tokenCount: estimated token consumption for this request
//   - model: the model being requested (used for per-model token limits)
//   - groupID: optional group ID for per-group override lookup
//
// Returns (allowed bool, err error). On Redis unavailability, returns (true, nil)
// with a logged warning (fail-open).
func (rl *RateLimiter) Allow(ctx context.Context, identity string, tokenCount int64, model string, groupID *uint) (bool, error) {
	if rl.redis == nil {
		slog.Warn("RateLimiter: Redis client is nil, allowing request (fail-open)")
		return true, nil
	}

	// Resolve effective limits.
	maxReq := rl.config.RequestsPerMin
	maxTok := rl.config.TokensPerMin
	maxConc := rl.config.MaxConcurrent

	// Per-group overrides.
	if groupID != nil && rl.config.GroupOverrides != nil {
		groupKey := strconv.FormatUint(uint64(*groupID), 10)
		if override, ok := rl.config.GroupOverrides[groupKey]; ok {
			if override.RequestsPerMin > 0 {
				maxReq = override.RequestsPerMin
			}
			if override.TokensPerMin > 0 {
				maxTok = override.TokensPerMin
			}
			if override.MaxConcurrent > 0 {
				maxConc = override.MaxConcurrent
			}
		}
	}

	// Per-model token limit override.
	if model != "" && rl.config.ModelTokenLimits != nil {
		if modelLimit, ok := rl.config.ModelTokenLimits[model]; ok && modelLimit > 0 {
			maxTok = modelLimit
		}
	}

	// Generate a unique request ID for this check.
	requestID := uuid.New().String()

	nowMs := time.Now().UnixMilli()
	windowMs := int64(60000) // 1 minute window in milliseconds

	keys := []string{
		fmt.Sprintf("cpa-gateway:ratelimit:req:%s", identity),
		fmt.Sprintf("cpa-gateway:ratelimit:tok:%s", identity),
		fmt.Sprintf("cpa-gateway:ratelimit:conc:%s", identity),
		"cpa-gateway:ratelimit:global:req",
		"cpa-gateway:ratelimit:global:tok",
	}

	args := []interface{}{
		nowMs,                       // ARGV[1]
		windowMs,                    // ARGV[2]
		maxReq,                      // ARGV[3]
		tokenCount,                  // ARGV[4]
		maxTok,                      // ARGV[5]
		maxConc,                     // ARGV[6]
		requestID,                   // ARGV[7]
		rl.config.GlobalRequestCap,  // ARGV[8]
		rl.config.GlobalTokenCap,    // ARGV[9]
	}

	result, err := rl.script.Run(ctx, rl.redis, keys, args...).Result()
	if err != nil {
		// Fail-open: if Redis is unavailable, allow the request.
		slog.Warn("RateLimiter: Redis Lua script error, allowing request (fail-open)",
			"error", err, "identity", identity)
		return true, nil
	}

	resultStr, ok := result.(string)
	if !ok {
		slog.Warn("RateLimiter: unexpected Lua script result type, allowing request (fail-open)",
			"result", result, "identity", identity)
		return true, nil
	}

	if resultStr == "ALLOWED" {
		return true, nil
	}

	// Request denied — extract the dimension for logging/metrics.
	if strings.HasPrefix(resultStr, "DENIED:") {
		dimension := strings.TrimPrefix(resultStr, "DENIED:")
		slog.Info("RateLimiter: request denied",
			"identity", identity, "dimension", dimension,
			"model", model, "tokenCount", tokenCount)
	}

	return false, nil
}

// ReleaseConc removes a request from the concurrent request tracking set.
// This should be called when a request completes (success or failure) to
// free up a concurrent slot.
func (rl *RateLimiter) ReleaseConc(ctx context.Context, identity string, requestID string) error {
	if rl.redis == nil {
		return nil
	}

	key := fmt.Sprintf("cpa-gateway:ratelimit:conc:%s", identity)
	err := rl.redis.SRem(ctx, key, requestID).Err()
	if err != nil {
		slog.Warn("RateLimiter: failed to release concurrent slot",
			"error", err, "identity", identity, "requestID", requestID)
		return fmt.Errorf("releasing concurrent slot: %w", err)
	}
	return nil
}
