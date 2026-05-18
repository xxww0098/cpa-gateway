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

// UserStatus holds the cached user status lookup with TTL metadata.
type UserStatus struct {
	Status    string
	ExpiresAt time.Time
}

// maxUserStatusTTL caps the per-entry lifetime so the cache never serves a
// stale status for longer than the APIKeyCache default TTL (5 minutes). The
// cap is kept local to infra/ to preserve the `sdk → infra` dependency
// direction (sdk owns `apiKeyTTL` and must not be imported from here).
const maxUserStatusTTL = 5 * time.Minute

// UserStatusCache is an in-memory L1 cache for the `users.status` column,
// keyed by user ID. Entries carry their own ExpiresAt and are periodically
// swept by Start. The cache is designed for the user-status recheck hot path
// shared by sdk.AccessProvider and api.AuthMiddleware.
type UserStatusCache struct {
	m sync.Map
}

// NewUserStatusCache returns an empty UserStatusCache.
func NewUserStatusCache() *UserStatusCache {
	return &UserStatusCache{}
}

// Get returns the cached UserStatus for userID if present and not expired.
// Expired entries are removed from the cache as a side effect. The returned
// UserStatus is a value (not a pointer) to keep the hot path lock-free for
// readers.
func (c *UserStatusCache) Get(userID uint) (UserStatus, bool) {
	if c == nil {
		return UserStatus{}, false
	}
	value, ok := c.m.Load(userID)
	if !ok {
		return UserStatus{}, false
	}
	us, ok := value.(UserStatus)
	if !ok {
		c.m.Delete(userID)
		return UserStatus{}, false
	}
	if time.Now().After(us.ExpiresAt) {
		c.m.Delete(userID)
		return UserStatus{}, false
	}
	return us, true
}

// Set stores the given status under userID with the requested TTL. A ttl of
// zero or negative duration is a no-op (callers should not poison the cache
// with already-expired entries). The TTL is clamped at maxUserStatusTTL so
// that no entry outlives the APIKeyCache default.
func (c *UserStatusCache) Set(userID uint, status string, ttl time.Duration) {
	if c == nil {
		return
	}
	if ttl <= 0 {
		return
	}
	if ttl > maxUserStatusTTL {
		ttl = maxUserStatusTTL
	}
	c.m.Store(userID, UserStatus{
		Status:    status,
		ExpiresAt: time.Now().Add(ttl),
	})
}

// InvalidateUser removes the cached entry for userID. Called by admin paths
// that flip a user's status so the next auth attempt re-reads the database.
func (c *UserStatusCache) InvalidateUser(userID uint) {
	if c == nil {
		return
	}
	c.m.Delete(userID)
}

// Start runs a background goroutine that periodically removes expired
// entries. It returns when ctx is canceled. The sweep interval mirrors
// APIKeyCache.Start so the two caches share a predictable steady-state cost.
func (c *UserStatusCache) Start(ctx context.Context) {
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
				us, ok := value.(UserStatus)
				if !ok {
					c.m.Delete(key)
					return true
				}
				if now.After(us.ExpiresAt) {
					c.m.Delete(key)
				}
				return true
			})
		}
	}
}
