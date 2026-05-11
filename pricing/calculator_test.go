package pricing

import (
	"math"
	"testing"

	"github.com/xxww0098/cpa-gateway/model"
)

// newCacheWithPrices builds a ModelPriceCache directly from in-memory
// ModelPrice values, bypassing the gorm.DB load path. This keeps calculator
// tests hermetic (no database, no miniredis) while still exercising the real
// Get/normalize logic the production Calculator depends on.
func newCacheWithPrices(prices ...model.ModelPrice) *ModelPriceCache {
	items := make(map[string]*model.ModelPrice, len(prices))
	for i := range prices {
		p := prices[i]
		key := normalizeModelKey(p.ModelID)
		if key == "" {
			continue
		}
		items[key] = &p
	}
	return &ModelPriceCache{items: items}
}

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9
}

// TestEstimate_NonStream verifies that a known model produces a strictly
// positive estimated cost for a non-stream request. This is the baseline
// happy-path: cache hit + input/output prices set means cost must not be 0.
func TestEstimate_NonStream(t *testing.T) {
	cache := newCacheWithPrices(model.ModelPrice{
		ModelID:          "gpt-4o",
		InputPricePer1M:  2.50,
		OutputPricePer1M: 10.00,
	})
	calc := NewCalculator(cache, 1.0)

	got := calc.Estimate("gpt-4o", false, 1.0)
	if got <= 0 {
		t.Fatalf("Estimate(non-stream) = %v, want > 0", got)
	}
}

// TestEstimate_Stream verifies that enabling stream mode yields a strictly
// larger estimate than the equivalent non-stream call. HoldMiddleware uses
// this headroom to avoid under-debiting long-running streaming responses.
func TestEstimate_Stream(t *testing.T) {
	cache := newCacheWithPrices(model.ModelPrice{
		ModelID:          "gpt-4o",
		InputPricePer1M:  2.50,
		OutputPricePer1M: 10.00,
	})
	calc := NewCalculator(cache, 1.0)

	nonStream := calc.Estimate("gpt-4o", false, 1.0)
	streamed := calc.Estimate("gpt-4o", true, 1.0)

	if !(streamed > nonStream) {
		t.Fatalf("Estimate(stream)=%v should be > Estimate(non-stream)=%v", streamed, nonStream)
	}
}

// TestEstimate_UnknownModel verifies the unknown-model branch falls back to
// the Calculator's defaultPrice rather than returning 0 or panicking. The
// fallback lets HoldMiddleware still reserve funds for models we haven't
// priced explicitly.
func TestEstimate_UnknownModel(t *testing.T) {
	cache := newCacheWithPrices() // empty cache
	const defaultPrice = 2.0
	calc := NewCalculator(cache, defaultPrice)

	got := calc.Estimate("does-not-exist", false, 1.0)
	if got <= 0 {
		t.Fatalf("Estimate(unknown, default=%v) = %v, want > 0 (fallback to default price)", defaultPrice, got)
	}

	// Also confirm a zero default collapses the fallback to 0 — proving the
	// non-zero result above really did come from defaultPrice.
	zeroCalc := NewCalculator(cache, 0)
	if zg := zeroCalc.Estimate("does-not-exist", false, 1.0); zg != 0 {
		t.Fatalf("Estimate(unknown, default=0) = %v, want 0", zg)
	}
}

// TestCompute_AllTokenTypes verifies Compute multiplies each of the four
// token columns by its matching per-1M price. Each column is exercised with
// 1,000,000 tokens so the expected contribution equals the price directly.
func TestCompute_AllTokenTypes(t *testing.T) {
	cache := newCacheWithPrices(model.ModelPrice{
		ModelID:               "o3",
		InputPricePer1M:       10.0,
		OutputPricePer1M:      40.0,
		CachedInputPricePer1M: 2.5,
		ReasoningPricePer1M:   60.0,
	})
	calc := NewCalculator(cache, 0)

	tokens := UsageTokens{
		Input:     1_000_000,
		Output:    1_000_000,
		Cached:    1_000_000,
		Reasoning: 1_000_000,
	}
	got := calc.Compute("o3", tokens, 1.0)

	want := 10.0 + 40.0 + 2.5 + 60.0 // 112.5
	if !approxEqual(got, want) {
		t.Fatalf("Compute all-token-types = %v, want %v", got, want)
	}
}

// TestCompute_RateMult verifies rateMult scales the final cost linearly. A
// rateMult of 2.0 must produce exactly 2x the cost of rateMult 1.0 for the
// same token counts.
func TestCompute_RateMult(t *testing.T) {
	cache := newCacheWithPrices(model.ModelPrice{
		ModelID:          "gpt-4o",
		InputPricePer1M:  2.5,
		OutputPricePer1M: 10.0,
	})
	calc := NewCalculator(cache, 0)

	tokens := UsageTokens{Input: 1_000_000, Output: 500_000}
	single := calc.Compute("gpt-4o", tokens, 1.0)
	doubled := calc.Compute("gpt-4o", tokens, 2.0)

	if !approxEqual(doubled, single*2.0) {
		t.Fatalf("Compute doubled=%v, want %v (single=%v)", doubled, single*2.0, single)
	}
	if single <= 0 {
		t.Fatalf("baseline Compute single = %v, want > 0", single)
	}
}

// TestCompute_ZeroTokens verifies that a fully-zero UsageTokens yields a
// cost of exactly 0 regardless of the model price or default fallback. This
// matters for Settle paths when a request failed before any tokens were
// consumed.
func TestCompute_ZeroTokens(t *testing.T) {
	cache := newCacheWithPrices(model.ModelPrice{
		ModelID:               "gpt-4o",
		InputPricePer1M:       2.5,
		OutputPricePer1M:      10.0,
		CachedInputPricePer1M: 0.1,
		ReasoningPricePer1M:   5.0,
	})
	calc := NewCalculator(cache, 5.0)

	got := calc.Compute("gpt-4o", UsageTokens{}, 1.0)
	if got != 0 {
		t.Fatalf("Compute(zero tokens) = %v, want 0", got)
	}
}
