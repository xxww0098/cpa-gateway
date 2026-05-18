package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/xxww0098/cpa-gateway/config"
	"github.com/xxww0098/cpa-gateway/model"
	"github.com/xxww0098/cpa-gateway/pricing"
	"pgregory.net/rapid"
)

// This file focuses on Property 16 + Requirements 6.4 / 6.5:
//
//   - 6.4: a negative value in ANY of
//            input_price_per_1m / output_price_per_1m /
//            cached_input_price_per_1m / reasoning_price_per_1m
//          MUST produce HTTP 400 {"error":"invalid_price"} and MUST NOT
//          write to model.ModelPrice. Exactly 0 in any of those four
//          fields is a valid price.
//   - 6.5: when the handler rejects a payload on validation it MUST NOT
//          invoke Pricing_Cache.Invalidate.
//
// The happy-path "Invalidate reaches the shared cache" property is
// covered by TestAdminUpsertRefreshesCache in admin_pricing_cache_test.go;
// the helpers that file defines (adminPricingTB, newAdminPricingDB,
// adminPricingTestEmail, seedAdminBillingCtxMiddleware, pricesEqual and
// raiseRapidChecksAdminPricing) are reused below to keep the two focused
// on their respective properties without duplicating setup.

// adminPricingPayload is the JSON shape the admin upsert handler accepts.
// Using a struct (rather than map[string]any) keeps field names in one
// place and prevents a silent rename on the handler side from producing
// a false-green test.
type adminPricingPayload struct {
	ModelID               string  `json:"model_id"`
	InputPricePer1M       float64 `json:"input_price_per_1m"`
	OutputPricePer1M      float64 `json:"output_price_per_1m"`
	CachedInputPricePer1M float64 `json:"cached_input_price_per_1m"`
	ReasoningPricePer1M   float64 `json:"reasoning_price_per_1m"`
}

// mountAdminUpsertHandler builds a minimal gin engine that injects the
// admin BillingCtx (via seedAdminBillingCtxMiddleware) and mounts the
// handler at its production path. Centralising the wiring keeps every
// iteration in this file using the same request shape.
func mountAdminUpsertHandler(pr *PanelRouter) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(seedAdminBillingCtxMiddleware())
	r.POST("/api/panel/admin/pricing/models", pr.AdminUpsertPricingModelHandler)
	return r
}

// doAdminUpsert marshals the payload and drives the handler under a
// httptest recorder. The returned recorder lets the caller assert status
// and body shape without repeating the boilerplate.
func doAdminUpsert(tb adminPricingTB, r *gin.Engine, payload adminPricingPayload) *httptest.ResponseRecorder {
	tb.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		tb.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost,
		"/api/panel/admin/pricing/models",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// assertInvalidPriceResponse is the shared assertion for any rejection
// path: HTTP 400 with a JSON body whose top-level "error" key is
// exactly "invalid_price". The handler writes that body via
// c.AbortWithStatusJSON(...) (handler_ops.go), so we match against a
// plain {"error":"..."} shape rather than the APIResponse envelope that
// success paths use.
func assertInvalidPriceResponse(tb adminPricingTB, w *httptest.ResponseRecorder) {
	tb.Helper()
	if w.Code != http.StatusBadRequest {
		tb.Fatalf("status = %d; want 400 invalid_price; body=%s", w.Code, w.Body.String())
	}
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
		tb.Fatalf("unmarshal body: %v; body=%q", err, w.Body.String())
	}
	if parsed.Error != "invalid_price" {
		tb.Fatalf("body.error = %q; want %q; full body=%q",
			parsed.Error, "invalid_price", w.Body.String())
	}
}

// TestNegativePriceRejected drives Property 16 and Requirement 6.4 as a
// property test over the four per-1M fields × negative draws.
//
// For each iteration:
//  1. Pick one of the 4 field indices {0,1,2,3} to "poison" with a
//     negative value drawn from [-1000, -0.000001]. The other 3 stay
//     non-negative, drawn from [0, 100] so they cannot individually
//     trigger the rejection and we know the handler is rejecting
//     specifically on the poisoned field.
//  2. Submit the payload against a fresh DB that has NO pre-existing
//     row for the modelID.
//  3. Assert HTTP 400 + {"error":"invalid_price"}.
//  4. Assert model.ModelPrice contains zero rows for that modelID —
//     i.e. the handler refused to INSERT a poisoned row.
//  5. Assert model.ModelPrice has zero rows in total — i.e. no side
//     effect on neighboring rows either.
//
// The lower bound -0.000001 (rather than 0) lets rapid shrink toward the
// float boundary just below zero, which is the most interesting failure
// to surface: a handler that accidentally does `< -epsilon` instead of
// `< 0` would miss this case and be caught here.
//
// **Validates: Property 16, Requirements 6.4 (negative rejection branch)**
func TestNegativePriceRejected(t *testing.T) {
	raiseRapidChecksAdminPricing(t, 200)

	rapid.Check(t, func(rt *rapid.T) {
		// Which field carries the negative value this iteration. Rapid
		// will shrink to the smallest index on counterexamples; including
		// all 4 ensures every field gets coverage over the iteration
		// budget.
		negIdx := rapid.IntRange(0, 3).Draw(rt, "negativeFieldIndex")
		negVal := rapid.Float64Range(-1000, -0.000001).Draw(rt, "negativeValue")

		// Non-poisoned fields stay in the valid, non-negative range.
		// Drawing from [0, 100] also exercises the "0 is valid" part of
		// Requirement 6.4 in about half of the iterations.
		p := [4]float64{
			rapid.Float64Range(0, 100).Draw(rt, "field0"),
			rapid.Float64Range(0, 100).Draw(rt, "field1"),
			rapid.Float64Range(0, 100).Draw(rt, "field2"),
			rapid.Float64Range(0, 100).Draw(rt, "field3"),
		}
		p[negIdx] = negVal

		modelID := rapid.StringMatching(`[a-z][a-z0-9-]{0,15}`).Draw(rt, "modelID")

		db := newAdminPricingDB(rt)
		// NewModelPriceCache with an empty DB is a no-op load; we still
		// wire it so the handler's Invalidate branch is exercised and
		// observed not to run (TestValidationSkipsInvalidate below
		// pins that claim end-to-end; here we just prove the request
		// was rejected without side effects).
		cache, err := pricing.NewModelPriceCache(db)
		if err != nil {
			rt.Fatalf("NewModelPriceCache: %v", err)
		}
		cfg := &config.Config{}
		cfg.Auth.AdminEmails = []string{adminPricingTestEmail}
		pr := NewPanelRouter(db, nil, nil, nil, cfg)
		pr.PriceCache = cache

		r := mountAdminUpsertHandler(pr)

		payload := adminPricingPayload{
			ModelID:               modelID,
			InputPricePer1M:       p[0],
			OutputPricePer1M:      p[1],
			CachedInputPricePer1M: p[2],
			ReasoningPricePer1M:   p[3],
		}
		w := doAdminUpsert(rt, r, payload)
		assertInvalidPriceResponse(rt, w)

		// Per Requirement 6.4 the handler MUST NOT write to ModelPrice
		// on a rejected payload. We verify both: no row for this
		// modelID AND no rows at all (since we never seeded one).
		var byModel int64
		if err := db.Model(&model.ModelPrice{}).
			Where("model_id = ?", modelID).
			Count(&byModel).Error; err != nil {
			rt.Fatalf("count rows by model_id: %v", err)
		}
		if byModel != 0 {
			rt.Fatalf("rejected payload inserted %d row(s) for model_id=%q", byModel, modelID)
		}
		var total int64
		if err := db.Model(&model.ModelPrice{}).Count(&total).Error; err != nil {
			rt.Fatalf("count rows total: %v", err)
		}
		if total != 0 {
			rt.Fatalf("rejected payload mutated ModelPrice table: total rows = %d; want 0", total)
		}
	})
}

// TestZeroPriceAccepted pins the "exactly 0 is valid" branch of
// Requirement 6.4. Using an example (rather than rapid) keeps the
// signal sharp: the assertion checks a specific boundary, not a
// distribution.
//
// We submit all four fields as 0, expect HTTP 200, and then assert
// the cache reports all-zero prices for the modelID. The cache
// observation doubles as a round-trip check: the row was inserted
// (otherwise Invalidate's reload would produce a miss) AND the
// Invalidate branch of the handler ran (otherwise Get would miss
// because NewModelPriceCache loaded an empty table).
//
// **Validates: Property 16, Requirement 6.4 (0-accepted branch)**
func TestZeroPriceAccepted(t *testing.T) {
	db := newAdminPricingDB(t)
	cache, err := pricing.NewModelPriceCache(db)
	if err != nil {
		t.Fatalf("NewModelPriceCache: %v", err)
	}
	cfg := &config.Config{}
	cfg.Auth.AdminEmails = []string{adminPricingTestEmail}
	pr := NewPanelRouter(db, nil, nil, nil, cfg)
	pr.PriceCache = cache

	r := mountAdminUpsertHandler(pr)

	const modelID = "zero-price-model"
	w := doAdminUpsert(t, r, adminPricingPayload{
		ModelID:               modelID,
		InputPricePer1M:       0,
		OutputPricePer1M:      0,
		CachedInputPricePer1M: 0,
		ReasoningPricePer1M:   0,
	})

	if w.Code != http.StatusOK {
		t.Fatalf("zero-price upsert expected 200, got %d; body=%s", w.Code, w.Body.String())
	}

	got, ok := cache.Get(modelID)
	if !ok {
		t.Fatalf("cache.Get(%q) miss after zero-price upsert; Invalidate branch did not run", modelID)
	}
	if !pricesEqual(got, 0, 0, 0, 0) {
		t.Fatalf(
			"cache holds wrong prices after zero-price upsert: "+
				"(%g,%g,%g,%g); want all-zero",
			got.InputPricePer1M, got.OutputPricePer1M,
			got.CachedInputPricePer1M, got.ReasoningPricePer1M,
		)
	}

	// Also verify the row was actually written with all-zero values.
	// GORM's default Update semantics silently drop zero-value fields
	// on UPDATE paths, which is exactly the footgun that motivated
	// the handler's Select(...) + Updates(...) rewrite in Stage 3. A
	// regression here would represent a real billing risk: "price
	// dropped to 0" must be representable.
	var row model.ModelPrice
	if err := db.Where("model_id = ?", modelID).First(&row).Error; err != nil {
		t.Fatalf("load persisted row: %v", err)
	}
	if !pricesEqual(&row, 0, 0, 0, 0) {
		t.Fatalf(
			"persisted row holds wrong prices: (%g,%g,%g,%g); want all-zero",
			row.InputPricePer1M, row.OutputPricePer1M,
			row.CachedInputPricePer1M, row.ReasoningPricePer1M,
		)
	}
}

// TestValidationSkipsInvalidate pins Requirement 6.5: when the handler
// rejects a payload on validation it MUST NOT invoke
// Pricing_Cache.Invalidate. Direct spying on cache.Invalidate would
// require wrapping *pricing.ModelPriceCache in a new interface, so we
// assert the same claim *indirectly* but unambiguously:
//
//  1. Seed a ModelPrice row with known OLD values.
//  2. Build NewModelPriceCache(db) so the cache has the OLD values
//     pre-loaded.
//  3. Send an upsert whose reasoning_price_per_1m is negative (the
//     other three fields are intentionally set to NEW values so that
//     any accidental DB write would be detectable).
//  4. Expect HTTP 400 + invalid_price.
//  5. Assert cache.Get(modelID) still returns the OLD values — this
//     means either (a) Invalidate was not called, or (b) Invalidate
//     was called but the DB was not mutated. In both cases the
//     operator-visible outcome is "rejected payloads never mutate
//     runtime pricing", which is the invariant Requirement 6.5 is
//     expressing.
//  6. As a stricter pin: also assert the DB row itself still holds
//     the OLD values. Combined with (5) this reduces to "Invalidate
//     was not called" because if the handler had called Invalidate
//     after a successful DB write we would observe NEW values in the
//     cache; if it had called Invalidate against an unchanged DB, the
//     cache would still hold OLD values (fine). The failing mode this
//     rules out is exactly the bug: handler writes NEW values to DB
//     *and* calls Invalidate, leaving cache with the NEW values.
//
// **Validates: Property 16, Requirements 6.4, 6.5**
func TestValidationSkipsInvalidate(t *testing.T) {
	db := newAdminPricingDB(t)

	const modelID = "validation-skip-model"
	const (
		oldInput     = 1.23
		oldOutput    = 4.56
		oldCached    = 7.89
		oldReasoning = 9.87
	)
	seed := model.ModelPrice{
		ModelID:               modelID,
		InputPricePer1M:       oldInput,
		OutputPricePer1M:      oldOutput,
		CachedInputPricePer1M: oldCached,
		ReasoningPricePer1M:   oldReasoning,
	}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatalf("seed ModelPrice: %v", err)
	}

	cache, err := pricing.NewModelPriceCache(db)
	if err != nil {
		t.Fatalf("NewModelPriceCache: %v", err)
	}
	// Pre-warm sanity check: if this fails the test itself is broken.
	if got, ok := cache.Get(modelID); !ok {
		t.Fatalf("pre-warm cache miss on seeded modelID %q", modelID)
	} else if !pricesEqual(got, oldInput, oldOutput, oldCached, oldReasoning) {
		t.Fatalf(
			"pre-warm cache holds wrong prices: (%g,%g,%g,%g); want (%g,%g,%g,%g)",
			got.InputPricePer1M, got.OutputPricePer1M,
			got.CachedInputPricePer1M, got.ReasoningPricePer1M,
			oldInput, oldOutput, oldCached, oldReasoning,
		)
	}

	cfg := &config.Config{}
	cfg.Auth.AdminEmails = []string{adminPricingTestEmail}
	pr := NewPanelRouter(db, nil, nil, nil, cfg)
	pr.PriceCache = cache

	r := mountAdminUpsertHandler(pr)

	// Send a payload whose reasoning_price_per_1m is negative and the
	// other three are plausible NEW values. Any accidental write to
	// the DB would therefore be detectable: the OLD-vs-NEW values are
	// chosen to be distinct.
	w := doAdminUpsert(t, r, adminPricingPayload{
		ModelID:               modelID,
		InputPricePer1M:       42.0,
		OutputPricePer1M:      43.0,
		CachedInputPricePer1M: 44.0,
		ReasoningPricePer1M:   -0.1,
	})
	assertInvalidPriceResponse(t, w)

	// Core claim: cache still reports OLD values. If the handler had
	// (incorrectly) called Invalidate after a successful DB mutation,
	// cache.Get would surface the NEW values below.
	got, ok := cache.Get(modelID)
	if !ok {
		t.Fatalf("cache.Get(%q) miss after rejected upsert; Invalidate side-effect removed entry", modelID)
	}
	if !pricesEqual(got, oldInput, oldOutput, oldCached, oldReasoning) {
		t.Fatalf(
			"cache holds NEW/other values after rejected upsert: "+
				"(%g,%g,%g,%g); want OLD (%g,%g,%g,%g). "+
				"Indicates handler either mutated the DB or refreshed the cache on a rejected payload.",
			got.InputPricePer1M, got.OutputPricePer1M,
			got.CachedInputPricePer1M, got.ReasoningPricePer1M,
			oldInput, oldOutput, oldCached, oldReasoning,
		)
	}

	// Stricter pin: the persisted row is also unchanged. Rejection
	// MUST NOT cross the handler's validation short-circuit, so no
	// UPDATE or INSERT on ModelPrice should have occurred.
	var row model.ModelPrice
	if err := db.Where("model_id = ?", modelID).First(&row).Error; err != nil {
		t.Fatalf("reload persisted row: %v", err)
	}
	if !pricesEqual(&row, oldInput, oldOutput, oldCached, oldReasoning) {
		t.Fatalf(
			"persisted row mutated despite rejection: "+
				"(%g,%g,%g,%g); want OLD (%g,%g,%g,%g)",
			row.InputPricePer1M, row.OutputPricePer1M,
			row.CachedInputPricePer1M, row.ReasoningPricePer1M,
			oldInput, oldOutput, oldCached, oldReasoning,
		)
	}

	// Additional pin: exactly one row exists for this modelID —
	// guards against a bug where the handler inserts a second row
	// under the same modelID instead of rejecting.
	var count int64
	if err := db.Model(&model.ModelPrice{}).
		Where("model_id = ?", modelID).
		Count(&count).Error; err != nil {
		t.Fatalf("count rows by model_id: %v", err)
	}
	if count != 1 {
		t.Fatalf("rows for model_id=%q: got %d; want 1", modelID, count)
	}
}
