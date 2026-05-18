package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// defaultIdempotencyTTL is the default TTL for cached idempotency responses.
	defaultIdempotencyTTL = 24 * time.Hour

	// idempotencyKeyPrefix is the Redis key prefix for idempotency entries.
	idempotencyKeyPrefix = "cpa-gateway:idempotency:"
)

// CachedResponse holds the response data cached for idempotent request replay.
type CachedResponse struct {
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"`
	Body       []byte            `json:"body"`
	Cost       float64           `json:"cost"`
	RequestID  string            `json:"request_id"`
}

// IdempotencyManager handles idempotent request deduplication using Redis.
type IdempotencyManager struct {
	redis *redis.Client
	ttl   time.Duration
}

// NewIdempotencyManager creates a new IdempotencyManager. If ttl <= 0, the
// default of 24 hours is used.
func NewIdempotencyManager(redisClient *redis.Client, ttl time.Duration) *IdempotencyManager {
	if ttl <= 0 {
		ttl = defaultIdempotencyTTL
	}
	return &IdempotencyManager{
		redis: redisClient,
		ttl:   ttl,
	}
}

// Check looks up a cached response for the given idempotency key.
// Returns (response, true, nil) if a cached entry exists,
// (nil, false, nil) if no entry is found, or (nil, false, err) on error.
func (im *IdempotencyManager) Check(ctx context.Context, key string) (*CachedResponse, bool, error) {
	redisKey := idempotencyKeyPrefix + key

	data, err := im.redis.Get(ctx, redisKey).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("idempotency check failed: %w", err)
	}

	var resp CachedResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, false, fmt.Errorf("idempotency unmarshal failed: %w", err)
	}

	return &resp, true, nil
}

// Store saves a response in the idempotency cache with the configured TTL.
func (im *IdempotencyManager) Store(ctx context.Context, key string, resp *CachedResponse) error {
	redisKey := idempotencyKeyPrefix + key

	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("idempotency marshal failed: %w", err)
	}

	if err := im.redis.Set(ctx, redisKey, data, im.ttl).Err(); err != nil {
		return fmt.Errorf("idempotency store failed: %w", err)
	}

	return nil
}
