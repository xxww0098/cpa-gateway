package pricing

import (
	"flag"
	"strconv"
	"testing"

	"github.com/xxww0098/cpa-gateway/model"
	"pgregory.net/rapid"
)

// raiseRapidChecks raises the -rapid.checks iteration count to the
// requested minimum for the duration of the current test. It is a no-op
// when the caller already configured a higher value via -rapid.checks or
// RAPID_CHECKS. The original flag value is restored on test cleanup so
// neighboring tests observe the user-supplied configuration.
func raiseRapidChecks(t *testing.T, minChecks int) {
	t.Helper()
	fl := flag.Lookup("rapid.checks")
	if fl == nil {
		// rapid did not register the flag in this binary; fall back to the
		// package default rather than failing the test — the default of 100
		// is still a useful check even if it's below our requested floor.
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

// TestEstimateWithMaxTokensUpperBound validates the HoldMiddleware
// preflight upper-bound invariant: when a client-supplied max_output_tokens
// exceeds the Calculator's nominal estimatedTokens floor (1000), the
// max-tokens-aware estimate MUST be at least as large as the default
// streaming Estimate. Hold_Middleware uses this to compute the
// Upper_Bound_Request_Cost for Requirement 2.1's balance preflight.
//
// **Validates: Property 5, Requirements 2.1**
func TestEstimateWithMaxTokensUpperBound(t *testing.T) {
	raiseRapidChecks(t, 200)

	rapid.Check(t, func(rt *rapid.T) {
		// Per-1M prices are non-negative per Requirement 6.4 (admin handler
		// rejects negatives; 0 is accepted).
		inputPer1M := rapid.Float64Range(0, 100).Draw(rt, "inputPer1M")
		outputPer1M := rapid.Float64Range(0, 100).Draw(rt, "outputPer1M")
		// maxTokens strictly greater than the estimatedTokens floor (1000)
		// so the upper-bound property is well-defined. Upper bound
		// 2_000_000 stays far below any float precision concerns once
		// multiplied by per-1M prices.
		maxTokens := rapid.Int64Range(estimatedTokens+1, 2_000_000).Draw(rt, "maxTokens")
		// rateMult spans baseline (1.0) plus tier discounts/surcharges. A
		// value of 0 is admissible — both sides collapse to 0 and the `>=`
		// check still holds trivially.
		rateMult := rapid.Float64Range(0, 10).Draw(rt, "rateMult")

		const modelID = "rapid-upper-bound-model"
		cache := newCacheWithPrices(model.ModelPrice{
			ModelID:          modelID,
			InputPricePer1M:  inputPer1M,
			OutputPricePer1M: outputPer1M,
		})
		calc := NewCalculator(cache, 0)

		// Baseline streaming Estimate that HoldMiddleware already uses today.
		base := calc.Estimate(modelID, true, rateMult)
		// Max-tokens-aware estimate — never lower than the baseline when
		// maxTokens > estimatedTokens.
		upper := calc.EstimateWithMaxTokens(modelID, maxTokens, true, rateMult)

		if upper < base {
			rt.Fatalf(
				"EstimateWithMaxTokens=%g must be >= Estimate(stream=true)=%g "+
					"(input=%g output=%g maxTokens=%d rateMult=%g)",
				upper, base, inputPer1M, outputPer1M, maxTokens, rateMult,
			)
		}
	})
}

// TestEstimateWithMaxTokensZeroOrNegativeFallback covers the
// maxOutputTokens <= 0 fallback branch documented on EstimateWithMaxTokens:
// when the caller cannot parse a positive client cap, the result must be
// exactly Estimate(...). This is the conservative posture spelled out in
// the method's godoc — clients that omit or malform max_tokens do not
// benefit from a tighter bound, they pay the streaming Estimate instead.
//
// **Validates: Property 5, Requirements 2.1**
func TestEstimateWithMaxTokensZeroOrNegativeFallback(t *testing.T) {
	raiseRapidChecks(t, 200)

	rapid.Check(t, func(rt *rapid.T) {
		inputPer1M := rapid.Float64Range(0, 100).Draw(rt, "inputPer1M")
		outputPer1M := rapid.Float64Range(0, 100).Draw(rt, "outputPer1M")
		// Whole range of "non-positive" values maxTokens can take.
		maxTokens := rapid.Int64Range(-1_000_000, 0).Draw(rt, "maxTokens")
		rateMult := rapid.Float64Range(0, 10).Draw(rt, "rateMult")
		streaming := rapid.Bool().Draw(rt, "streaming")

		const modelID = "rapid-fallback-model"
		cache := newCacheWithPrices(model.ModelPrice{
			ModelID:          modelID,
			InputPricePer1M:  inputPer1M,
			OutputPricePer1M: outputPer1M,
		})
		calc := NewCalculator(cache, 0)

		base := calc.Estimate(modelID, streaming, rateMult)
		fallback := calc.EstimateWithMaxTokens(modelID, maxTokens, streaming, rateMult)

		// Expect bit-for-bit equality: the fallback branch delegates to
		// Estimate with the same args, so no arithmetic divergence is
		// tolerated.
		if fallback != base {
			rt.Fatalf(
				"fallback=%g must equal Estimate=%g when maxTokens=%d <= 0 "+
					"(input=%g output=%g rateMult=%g streaming=%v)",
				fallback, base, maxTokens, inputPer1M, outputPer1M, rateMult, streaming,
			)
		}
	})
}
