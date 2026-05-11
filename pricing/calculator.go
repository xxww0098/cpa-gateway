package pricing

// Calculator turns a model ID and a set of token counts into a USD cost,
// using the four per-1M price columns from model.ModelPrice and an optional
// rate multiplier applied on top.
//
// The cache dependency is read-only from Calculator's perspective — cache
// refresh is owned by admin flows via ModelPriceCache.Invalidate.
//
// A zero-value Calculator is still a valid type, kept as a safe default for
// wiring code that was written before the full implementation landed. In
// that state all methods return 0. Production callers must use
// NewCalculator so the cache and defaultPrice are populated.
type Calculator struct {
	cache        *ModelPriceCache
	defaultPrice float64
}

// UsageTokens carries the four token counts priced independently per
// ModelPrice column. Any field left at 0 simply contributes 0 to the cost.
//
// Units are raw tokens; the Compute math normalizes by 1,000,000 to match
// the "per 1M tokens" unit on ModelPrice.
type UsageTokens struct {
	Input     int64
	Output    int64
	Cached    int64
	Reasoning int64
}

// NewCalculator constructs a Calculator. defaultPricePer1M is the fallback
// used by Estimate when the requested model is missing from the cache; it is
// expressed in USD per 1M tokens so the unit matches ModelPrice.
//
// A nil cache is tolerated — every lookup will then miss and Estimate falls
// back to defaultPricePer1M. This keeps bootstrap code paths simple when the
// pricing table is empty.
func NewCalculator(cache *ModelPriceCache, defaultPricePer1M float64) *Calculator {
	return &Calculator{cache: cache, defaultPrice: defaultPricePer1M}
}

// estimatedTokens is the nominal token count Estimate reserves for a single
// request before the real response is seen. It is intentionally a small,
// generous constant rather than a per-model heuristic: Hold math only needs
// to be "close enough" — Settle reconciles to the true usage afterwards.
const estimatedTokens = 1000

// streamMultiplier scales the Estimate upward for streaming requests to
// give the Hold more headroom — streaming completions routinely generate
// more output tokens than single-shot calls, so we reserve roughly 2x.
const streamMultiplier = 2.0

// Estimate returns the USD cost we expect a request to incur, used by the
// Hold middleware to reserve funds up front. The value is deliberately an
// over-approximation; Settle will later refund any unused portion.
//
// Rules:
//   - Known model: price both input and output at estimatedTokens each, using
//     the model's per-1M rates.
//   - Unknown model: fall back to defaultPrice × estimatedTokens (still
//     treated as per-1M), applied to both input and output.
//   - Stream requests: multiply the estimate by streamMultiplier.
//   - rateMult: final linear scaling applied after the base estimate.
//
// A non-positive defaultPrice on the unknown-model path collapses to 0, and
// a zero rateMult collapses to 0 as well — both are deliberate: the caller
// controls whether to enforce a floor.
func (c *Calculator) Estimate(modelID string, stream bool, rateMult float64) float64 {
	if c == nil {
		return 0
	}

	var inputPer1M, outputPer1M float64
	if p, ok := c.lookup(modelID); ok {
		inputPer1M = p.InputPricePer1M
		outputPer1M = p.OutputPricePer1M
	} else {
		inputPer1M = c.defaultPrice
		outputPer1M = c.defaultPrice
	}

	tokens := float64(estimatedTokens)
	base := (inputPer1M*tokens + outputPer1M*tokens) / 1_000_000.0
	if stream {
		base *= streamMultiplier
	}
	return base * rateMult
}

// Compute returns the exact USD cost for a completed request, summing the
// four token columns against the corresponding per-1M prices and scaling by
// rateMult.
//
// Formula:
//
//	(inputPrice*Input + outputPrice*Output + cachedPrice*Cached +
//	 reasoningPrice*Reasoning) / 1_000_000 * rateMult
//
// Unknown models use defaultPrice for every column. Passing an all-zero
// UsageTokens yields exactly 0, regardless of price or rateMult.
func (c *Calculator) Compute(modelID string, tokens UsageTokens, rateMult float64) float64 {
	if c == nil {
		return 0
	}

	var inputPer1M, outputPer1M, cachedPer1M, reasoningPer1M float64
	if p, ok := c.lookup(modelID); ok {
		inputPer1M = p.InputPricePer1M
		outputPer1M = p.OutputPricePer1M
		cachedPer1M = p.CachedInputPricePer1M
		reasoningPer1M = p.ReasoningPricePer1M
	} else {
		inputPer1M = c.defaultPrice
		outputPer1M = c.defaultPrice
		cachedPer1M = c.defaultPrice
		reasoningPer1M = c.defaultPrice
	}

	raw := inputPer1M*float64(tokens.Input) +
		outputPer1M*float64(tokens.Output) +
		cachedPer1M*float64(tokens.Cached) +
		reasoningPer1M*float64(tokens.Reasoning)

	return raw / 1_000_000.0 * rateMult
}

// lookup resolves a model ID to a ModelPrice via the cache, tolerating a nil
// cache (e.g., in early bootstrap or in tests that pass the zero Calculator).
func (c *Calculator) lookup(modelID string) (*modelPriceView, bool) {
	if c.cache == nil {
		return nil, false
	}
	p, ok := c.cache.Get(modelID)
	if !ok || p == nil {
		return nil, false
	}
	return &modelPriceView{
		InputPricePer1M:       p.InputPricePer1M,
		OutputPricePer1M:      p.OutputPricePer1M,
		CachedInputPricePer1M: p.CachedInputPricePer1M,
		ReasoningPricePer1M:   p.ReasoningPricePer1M,
	}, true
}

// modelPriceView is a narrow, internal projection of model.ModelPrice.
// Using it keeps the exported Calculator API from leaking the full GORM
// struct (including timestamps and ID) into callers that only need the four
// price columns.
type modelPriceView struct {
	InputPricePer1M       float64
	OutputPricePer1M      float64
	CachedInputPricePer1M float64
	ReasoningPricePer1M   float64
}
