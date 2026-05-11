package pricing

import (
	"context"
	"strings"
	"sync"

	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/gorm"
)

// ModelPriceCache holds an in-memory snapshot of model.ModelPrice rows keyed
// by a normalized form of ModelID (trimmed + lower-case) so callers can look
// up prices without case/whitespace sensitivity.
//
// The cache is loaded once at startup via NewModelPriceCache and only
// refreshed explicitly via Invalidate — typically after an admin mutates the
// pricing table. There is no background timer.
//
// Concurrency: Get/List acquire the RWMutex for read only; Invalidate
// acquires it for write. Callers may safely share a single *ModelPriceCache
// across goroutines.
type ModelPriceCache struct {
	mu    sync.RWMutex
	items map[string]*model.ModelPrice
}

// NewModelPriceCache constructs a cache and performs an initial full load from
// the database. A non-nil *gorm.DB is required; passing nil returns an error.
func NewModelPriceCache(db *gorm.DB) (*ModelPriceCache, error) {
	c := &ModelPriceCache{items: make(map[string]*model.ModelPrice)}
	if err := c.Invalidate(db); err != nil {
		return nil, err
	}
	return c, nil
}

// Get returns the ModelPrice for the given model ID. The lookup trims
// whitespace and lower-cases the key, matching the normalization used when
// the cache was populated. ok is false when the model ID is not tracked.
func (c *ModelPriceCache) Get(modelID string) (*model.ModelPrice, bool) {
	key := normalizeModelKey(modelID)
	if key == "" {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.items[key]
	return p, ok
}

// Invalidate reloads the cache from the database in a single read. The
// in-memory map is swapped atomically under the write lock so concurrent
// readers either see the old snapshot or the new one — never a torn state.
//
// If db is nil, Invalidate returns an error and the existing snapshot is
// preserved.
func (c *ModelPriceCache) Invalidate(db *gorm.DB) error {
	if db == nil {
		return gorm.ErrInvalidDB
	}
	var rows []model.ModelPrice
	if err := db.WithContext(context.Background()).Find(&rows).Error; err != nil {
		return err
	}
	next := make(map[string]*model.ModelPrice, len(rows))
	for i := range rows {
		p := rows[i]
		key := normalizeModelKey(p.ModelID)
		if key == "" {
			continue
		}
		next[key] = &p
	}
	c.mu.Lock()
	c.items = next
	c.mu.Unlock()
	return nil
}

// List returns a snapshot slice of all cached ModelPrice entries. The order
// is unspecified. The returned pointers share storage with the cache; callers
// should treat them as read-only.
func (c *ModelPriceCache) List() []*model.ModelPrice {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*model.ModelPrice, 0, len(c.items))
	for _, p := range c.items {
		out = append(out, p)
	}
	return out
}

// normalizeModelKey converts a model ID into the canonical cache key form.
// Keeping this in one place ensures Get and Invalidate agree on normalization.
func normalizeModelKey(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}
