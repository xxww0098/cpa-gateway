package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/xxww0098/cpa-gateway/config"
	"github.com/xxww0098/cpa-gateway/model"
	"github.com/xxww0098/cpa-gateway/pricing"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"pgregory.net/rapid"
)

// raiseRapidChecksAdminPricing raises the -rapid.checks flag to at least
// minChecks for the duration of the current test. Mirrors the helper used
// by sibling property tests (see pricing/estimate_max_tokens_test.go and
// sdk/holdmw_preflight_test.go) so task 12.2's ≥ 200 iteration budget is
// honoured regardless of the caller's global rapid configuration.
func raiseRapidChecksAdminPricing(t *testing.T, minChecks int) {
	t.Helper()
	fl := flag.Lookup("rapid.checks")
	if fl == nil {
		return
	}
	orig := fl.Value.String()
	cur, err := strconv.Atoi(orig)
	if err != nil || cur >= minChecks {
		return
	}
	if setErr := flag.Set("rapid.checks", strconv.Itoa(minChecks)); setErr != nil {
		t.Fatalf("flag.Set rapid.checks: %v", setErr)
	}
	t.Cleanup(func() { _ = flag.Set("rapid.checks", orig) })
}

// adminPricingTB is the subset of *testing.T and *rapid.T that the
// per-iteration helpers need. Both concrete types satisfy it so the setup
// can be called from either the outer test or a rapid iteration.
type adminPricingTB interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// newAdminPricingDB opens a fresh in-memory sqlite DB with model.ModelPrice and
// a real admin principal. Admin handlers authorize from users.role, so the
// focused pricing tests still need the users table even though pricing itself
// only writes model prices.
func newAdminPricingDB(tb adminPricingTB) *gorm.DB {
	tb.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		tb.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.ModelPrice{}); err != nil {
		tb.Fatalf("automigrate: %v", err)
	}
	admin := &model.User{
		ID:           1,
		Email:        adminPricingTestEmail,
		PasswordHash: "hash",
		Role:         "admin",
		Status:       userStatusActive,
	}
	if err := db.Create(admin).Error; err != nil {
		tb.Fatalf("seed admin: %v", err)
	}
	if sqlDB, err := db.DB(); err == nil {
		// Single-connection keeps the in-memory DB view consistent across
		// any concurrent callers (the handler and the test assertions run
		// on the same goroutine here, but this is cheap insurance).
		sqlDB.SetMaxOpenConns(1)
		tb.Cleanup(func() { _ = sqlDB.Close() })
	}
	return db
}

// adminPricingTestEmail is the deterministic admin row email used by the
// BillingCtx shim and seeded users row.
const adminPricingTestEmail = "admin@admin-pricing-test.local"

// seedAdminBillingCtxMiddleware returns a gin middleware that populates a
// BillingCtx whose UserID points at the seeded admin row. Mirrors the shim
// middleware pattern in purchase_debt_block_test.go; the downstream
// requireAdmin check performs the same users.role lookup used in production.
func seedAdminBillingCtxMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		bc := &BillingCtx{
			UserID:    1,
			Email:     adminPricingTestEmail,
			RateMult:  1.0,
			AuthType:  authTypeJWT,
			Status:    userStatusActive,
			RequestID: "admin-pricing-test",
		}
		setBillingContext(c, bc)
		c.Next()
	}
}

// pricesEqual compares a cached *model.ModelPrice against the four per-1M
// expected values using a tiny epsilon so JSON round-trip representation
// quirks do not produce false failures. The epsilon mirrors the one used by
// ledger/active_hold_amount_test.go for the same reason.
func pricesEqual(p *model.ModelPrice, input, output, cached, reasoning float64) bool {
	const epsilon = 1e-9
	return math.Abs(p.InputPricePer1M-input) <= epsilon &&
		math.Abs(p.OutputPricePer1M-output) <= epsilon &&
		math.Abs(p.CachedInputPricePer1M-cached) <= epsilon &&
		math.Abs(p.ReasoningPricePer1M-reasoning) <= epsilon
}

// TestAdminUpsertRefreshesCache exercises task 12.2's property-based
// assertion for Property 15 and Requirements 6.1, 6.2, 6.3, 6.6:
//
// After AdminUpsertPricingModelHandler commits a model price upsert, the
// in-memory *pricing.ModelPriceCache that the Calculator reads from MUST
// reflect the new prices — without a process restart and without any
// secondary refresh step by the caller.
//
// Per-iteration flow:
//  1. Fresh sqlite (only model.ModelPrice migrated) + seeded
//     ModelPrice{modelID, old_prices}.
//  2. Build *pricing.ModelPriceCache via pricing.NewModelPriceCache(db),
//     which loads the seeded row, and inject it onto a fresh PanelRouter.
//     This mirrors main.go's wiring step validated by
//     router_pricecache_wiring_test.go.
//  3. Sanity check: cache.Get(modelID) returns the OLD prices. A failure
//     here is a test-setup bug, not a handler bug — we surface it with a
//     distinct message.
//  4. POST /api/panel/admin/pricing/models with the NEW prices, carrying
//     a BillingCtx whose user_id belongs to a seeded DB admin.
//  5. Assert HTTP 200 and cache.Get(modelID) now returns the NEW prices.
//     This is the observable contract Pricing_Calculator.Estimate and
//     .Compute both transitively read from (pricing/cache.go → Calculator
//     .lookup), so refreshing the cache is equivalent to refreshing every
//     downstream estimate/compute caller.
//
// Rapid inputs:
//   - modelID: short lower-case ASCII. The cache normalizes keys via
//     strings.ToLower(strings.TrimSpace(...)), so generating pre-normalized
//     IDs keeps the assertion direct (no need to re-normalize on the
//     expect side) and guarantees two distinct draws do not collide under
//     normalization.
//   - old_prices / new_prices: four non-negative floats per side.
//     Requirement 6.4 rejects negative values on the handler (exactly 0
//     is accepted), so constraining generators to [0, 100] keeps every
//     iteration on the valid-input path for the property we care about.
//
// Shrinking target: task description pins "modelID=\"gpt-5\"", all 1.0
// prices. Rapid's default shrink minimises toward short strings and small
// floats, which lands near that target naturally.
//
// Iteration budget: raiseRapidChecksAdminPricing pins -rapid.checks ≥ 200,
// matching the task-level budget called out in tasks.md §12.2.
//
// **Validates: Property 15, Requirements 6.1, 6.2, 6.3, 6.6**
func TestAdminUpsertRefreshesCache(t *testing.T) {
	raiseRapidChecksAdminPricing(t, 200)

	rapid.Check(t, func(rt *rapid.T) {
		// Short lower-case ASCII IDs avoid normalization collisions and
		// keep each iteration cheap. Length is bounded so DB inserts
		// stay well below the ModelID column's size=128 limit.
		modelID := rapid.StringMatching(`[a-z][a-z0-9-]{0,15}`).Draw(rt, "modelID")

		drawPrice := func(label string) float64 {
			return rapid.Float64Range(0, 100).Draw(rt, label)
		}
		oldInput := drawPrice("oldInput")
		oldOutput := drawPrice("oldOutput")
		oldCached := drawPrice("oldCached")
		oldReasoning := drawPrice("oldReasoning")

		newInput := drawPrice("newInput")
		newOutput := drawPrice("newOutput")
		newCached := drawPrice("newCached")
		newReasoning := drawPrice("newReasoning")

		// Per-iteration DB + seed. Building a fresh env per iteration
		// (rather than sharing across iterations with a per-iteration
		// modelID) eliminates cross-iteration interference in the cache
		// — each iteration's assertion is about THIS upsert refreshing
		// the cache, not about cache state accumulated from prior runs.
		db := newAdminPricingDB(rt)

		seed := model.ModelPrice{
			ModelID:               modelID,
			InputPricePer1M:       oldInput,
			OutputPricePer1M:      oldOutput,
			CachedInputPricePer1M: oldCached,
			ReasoningPricePer1M:   oldReasoning,
		}
		if err := db.Create(&seed).Error; err != nil {
			rt.Fatalf("seed ModelPrice: %v", err)
		}

		// NewModelPriceCache performs an initial full load from the DB,
		// so the cache observes the seeded OLD row immediately.
		cache, err := pricing.NewModelPriceCache(db)
		if err != nil {
			rt.Fatalf("NewModelPriceCache: %v", err)
		}

		cfg := &config.Config{}
		cfg.Auth.AdminEmails = []string{adminPricingTestEmail}
		pr := NewPanelRouter(db, nil, nil, nil, cfg)
		// Emulate main.go's wiring: the same cache instance the Calculator
		// would read from is pushed onto PanelRouter so the admin handler
		// can Invalidate it after the upsert commits.
		pr.PriceCache = cache

		// Pre-warm sanity guard. If this check fails the test itself is
		// broken (not the handler) — surface it with a distinct message.
		if got, ok := cache.Get(modelID); !ok {
			rt.Fatalf("pre-warm: cache miss on seeded modelID %q", modelID)
		} else if !pricesEqual(got, oldInput, oldOutput, oldCached, oldReasoning) {
			rt.Fatalf(
				"pre-warm: cache holds wrong prices for %q: "+
					"got (%g,%g,%g,%g) want (%g,%g,%g,%g)",
				modelID,
				got.InputPricePer1M, got.OutputPricePer1M,
				got.CachedInputPricePer1M, got.ReasoningPricePer1M,
				oldInput, oldOutput, oldCached, oldReasoning,
			)
		}

		// Build a minimal gin engine that injects the admin BillingCtx
		// and mounts the handler at its production path. Using the full
		// path doubles as a smoke check on the URL contract.
		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.Use(seedAdminBillingCtxMiddleware())
		r.POST("/api/panel/admin/pricing/models", pr.AdminUpsertPricingModelHandler)

		payload, err := json.Marshal(map[string]any{
			"model_id":                  modelID,
			"input_price_per_1m":        newInput,
			"output_price_per_1m":       newOutput,
			"cached_input_price_per_1m": newCached,
			"reasoning_price_per_1m":    newReasoning,
		})
		if err != nil {
			rt.Fatalf("marshal payload: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost,
			"/api/panel/admin/pricing/models",
			bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			rt.Fatalf(
				"upsert expected HTTP 200, got %d; body=%s (modelID=%q)",
				w.Code, w.Body.String(), modelID,
			)
		}

		// Core property: after the successful upsert returns, the cache
		// MUST reflect the new prices. Because Calculator.lookup reads
		// through this cache for both Estimate and Compute, asserting the
		// cache contents here is equivalent to asserting that subsequent
		// Estimate/Compute calls will also observe the new prices — which
		// is exactly what Requirements 6.1/6.2/6.3/6.6 mandate.
		got, ok := cache.Get(modelID)
		if !ok {
			rt.Fatalf(
				"post-upsert: cache miss on %q — Invalidate did not re-populate",
				modelID,
			)
		}
		if !pricesEqual(got, newInput, newOutput, newCached, newReasoning) {
			rt.Fatalf(
				"post-upsert: cache holds stale prices for %q: "+
					"got (%g,%g,%g,%g) want (%g,%g,%g,%g)",
				modelID,
				got.InputPricePer1M, got.OutputPricePer1M,
				got.CachedInputPricePer1M, got.ReasoningPricePer1M,
				newInput, newOutput, newCached, newReasoning,
			)
		}
	})
}
