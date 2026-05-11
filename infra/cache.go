package infra

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// CachedKey holds the cached API key validation result with TTL metadata.
type CachedKey struct {
	UserID    uint
	ApiKeyID  uint
	GroupID   *uint
	RateMult  float64
	Status    string
	ExpiresAt time.Time
}

// String returns a human-readable representation of the cached key.
func (c *CachedKey) String() string {
	return fmt.Sprintf("CachedKey{UserID:%d ApiKeyID:%d GroupID:%v RateMult:%.2f Status:%s ExpiresAt:%s}",
		c.UserID, c.ApiKeyID, c.GroupID, c.RateMult, c.Status, c.ExpiresAt.Format(time.RFC3339))
}

// APIKeyCache is an in-memory L1 cache for validated API keys, keyed by
// the SHA-256 hash of the plaintext key. Entries carry their own
// ExpiresAt and are periodically swept by Start.
type APIKeyCache struct {
	m sync.Map
}

// NewAPIKeyCache returns an empty APIKeyCache.
func NewAPIKeyCache() *APIKeyCache {
	return &APIKeyCache{}
}

// Get returns the CachedKey for the given hash if present and not expired.
// Expired entries are removed from the cache as a side effect.
func (c *APIKeyCache) Get(hash string) (*CachedKey, bool) {
	if c == nil {
		return nil, false
	}
	value, ok := c.m.Load(hash)
	if !ok {
		return nil, false
	}
	ck, ok := value.(*CachedKey)
	if !ok {
		c.m.Delete(hash)
		return nil, false
	}
	if time.Now().After(ck.ExpiresAt) {
		c.m.Delete(hash)
		return nil, false
	}
	return ck, true
}

// Set stores v under the given hash.
func (c *APIKeyCache) Set(hash string, v *CachedKey) {
	if c == nil || v == nil {
		return
	}
	c.m.Store(hash, v)
}

// Delete removes the entry under the given hash.
func (c *APIKeyCache) Delete(hash string) {
	if c == nil {
		return
	}
	c.m.Delete(hash)
}

// Start runs a background goroutine that periodically removes expired
// entries. It returns when ctx is canceled.
func (c *APIKeyCache) Start(ctx context.Context) {
	if c == nil {
		return
	}
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			c.m.Range(func(key, value any) bool {
				ck, ok := value.(*CachedKey)
				if !ok {
					c.m.Delete(key)
					return true
				}
				if now.After(ck.ExpiresAt) {
					c.m.Delete(key)
				}
				return true
			})
		}
	}
}
