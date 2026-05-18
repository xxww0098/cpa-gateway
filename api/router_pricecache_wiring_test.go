package api

import (
	"testing"

	"github.com/xxww0098/cpa-gateway/config"
	"github.com/xxww0098/cpa-gateway/model"
	"github.com/xxww0098/cpa-gateway/pricing"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// newWiringTestDB opens a fresh in-memory SQLite DB with only the tables
// needed for the pricing-cache wiring test. Keeping the migration scope
// narrow avoids pulling unrelated model dependencies into this smoke test.
func newWiringTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.ModelPrice{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// TestPanelRouterPriceCacheWired verifies that when main.go injects a
// *pricing.ModelPriceCache onto PanelRouter, business handlers can reach
// through the router to invalidate the shared cache. This is the
// infrastructure precondition for Property 15 (admin upsert refreshes the
// cache that the Calculator actually reads from).
func TestPanelRouterPriceCacheWired(t *testing.T) {
	db := newWiringTestDB(t)

	cache, err := pricing.NewModelPriceCache(db)
	if err != nil {
		t.Fatalf("NewModelPriceCache: %v", err)
	}
	calc := pricing.NewCalculator(cache, 0)

	cfg := &config.Config{}
	pr := NewPanelRouter(db, nil, nil, calc, cfg)
	// Emulate main.go's wiring step that pushes the same cache instance
	// used by the Calculator onto the PanelRouter.
	pr.PriceCache = cache

	if pr.PriceCache == nil {
		t.Fatalf("pr.PriceCache == nil after wiring; want non-nil")
	}
	if pr.PriceCache != cache {
		t.Fatalf("pr.PriceCache is a different instance than the Calculator's cache; invalidation would be useless")
	}

	// Invalidate must not panic and must not return an error against a
	// valid DB — this mirrors the call handler_ops will make after an
	// admin upsert in Stage 3 (task 12.1).
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("pr.PriceCache.Invalidate(pr.DB) panicked: %v", r)
			}
		}()
		if err := pr.PriceCache.Invalidate(pr.DB); err != nil {
			t.Fatalf("Invalidate: %v", err)
		}
	}()
}

// TestPanelRouterPriceCacheNil verifies that leaving PriceCache unset does
// not break PanelRouter construction. The nil-safe handler branch that
// emits `pricing_cache_not_wired` logs is implemented as part of task 12.1
// (Stage 3). Once that lands, this test should grow a slog capture to
// assert the log line; for now we only confirm the router itself stays
// usable so Stage 1 can ship without blocking on Stage 3.
//
// TODO(12.1): extend to capture slog and assert `pricing_cache_not_wired`.
func TestPanelRouterPriceCacheNil(t *testing.T) {
	db := newWiringTestDB(t)

	cache, err := pricing.NewModelPriceCache(db)
	if err != nil {
		t.Fatalf("NewModelPriceCache: %v", err)
	}
	calc := pricing.NewCalculator(cache, 0)

	cfg := &config.Config{}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("NewPanelRouter with PriceCache left nil panicked: %v", r)
			}
		}()
		pr := NewPanelRouter(db, nil, nil, calc, cfg)
		if pr.PriceCache != nil {
			t.Fatalf("default PriceCache = %v, want nil (constructor must not populate it)", pr.PriceCache)
		}
	}()
}
