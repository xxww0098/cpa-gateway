package executor

import (
	"context"
	"testing"
)

// TestParseOpenAIUsage_Full validates that ParseOpenAIUsage extracts
// prompt/completion tokens together with cached and reasoning fields when
// the upstream payload contains the full `usage` envelope.
func TestParseOpenAIUsage_Full(t *testing.T) {
	payload := []byte(`{
		"id": "chatcmpl-123",
		"usage": {
			"prompt_tokens": 120,
			"completion_tokens": 80,
			"total_tokens": 200,
			"prompt_tokens_details": {"cached_tokens": 40},
			"completion_tokens_details": {"reasoning_tokens": 30}
		}
	}`)

	tokens, ok := ParseOpenAIUsage(payload)
	if !ok {
		t.Fatalf("expected ok=true for full OpenAI usage payload")
	}
	if tokens.Input != 120 {
		t.Errorf("Input = %d, want 120", tokens.Input)
	}
	if tokens.Output != 80 {
		t.Errorf("Output = %d, want 80", tokens.Output)
	}
	if tokens.Cached != 40 {
		t.Errorf("Cached = %d, want 40", tokens.Cached)
	}
	if tokens.Reasoning != 30 {
		t.Errorf("Reasoning = %d, want 30", tokens.Reasoning)
	}
}

// TestParseOpenAIUsage_Empty confirms that nil / empty / invalid payloads
// yield the zero value with ok=false and do not panic.
func TestParseOpenAIUsage_Empty(t *testing.T) {
	cases := map[string][]byte{
		"nil":            nil,
		"empty":          {},
		"malformed":      []byte(`{not-json`),
		"missing-usage":  []byte(`{"id":"x"}`),
		"zero-usage":     []byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`),
		"null-usage":     []byte(`{"usage":null}`),
		"wrong-top-type": []byte(`[]`),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			tokens, ok := ParseOpenAIUsage(payload)
			if ok {
				t.Fatalf("expected ok=false, got tokens=%+v", tokens)
			}
			if tokens != (UsageTokens{}) {
				t.Fatalf("expected zero UsageTokens, got %+v", tokens)
			}
		})
	}
}

// TestParseOpenAIUsage_Reasoning specifically covers the o1 / o3 series
// reasoning-token breakdown in completion_tokens_details.
func TestParseOpenAIUsage_Reasoning(t *testing.T) {
	payload := []byte(`{
		"usage": {
			"prompt_tokens": 10,
			"completion_tokens": 500,
			"completion_tokens_details": {"reasoning_tokens": 480}
		}
	}`)
	tokens, ok := ParseOpenAIUsage(payload)
	if !ok {
		t.Fatalf("expected ok=true, got false")
	}
	if tokens.Reasoning != 480 {
		t.Errorf("Reasoning = %d, want 480", tokens.Reasoning)
	}
	if tokens.Output != 500 {
		t.Errorf("Output = %d, want 500", tokens.Output)
	}
}

// TestParseClaudeUsage_MessageDelta validates the Claude streaming
// message_delta shape where output_tokens lives under delta.usage and
// input_tokens (if present) lives under message.usage.
func TestParseClaudeUsage_MessageDelta(t *testing.T) {
	payload := []byte(`{
		"type": "message_delta",
		"message": {"usage": {"input_tokens": 150}},
		"delta": {"usage": {"output_tokens": 75}}
	}`)
	tokens, ok := ParseClaudeUsage(payload)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if tokens.Input != 150 {
		t.Errorf("Input = %d, want 150", tokens.Input)
	}
	if tokens.Output != 75 {
		t.Errorf("Output = %d, want 75", tokens.Output)
	}
}

// TestParseClaudeUsage_CacheCreation ensures cache_creation_input_tokens
// maps onto UsageTokens.Cached and that cache_read_input_tokens also
// contributes to the cached total.
func TestParseClaudeUsage_CacheCreation(t *testing.T) {
	payload := []byte(`{
		"type": "message",
		"usage": {
			"input_tokens": 200,
			"output_tokens": 60,
			"cache_creation_input_tokens": 45,
			"cache_read_input_tokens": 15
		}
	}`)
	tokens, ok := ParseClaudeUsage(payload)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if tokens.Input != 200 {
		t.Errorf("Input = %d, want 200", tokens.Input)
	}
	if tokens.Output != 60 {
		t.Errorf("Output = %d, want 60", tokens.Output)
	}
	if tokens.Cached != 60 {
		t.Errorf("Cached = %d, want 60 (creation+read)", tokens.Cached)
	}
}

// TestParseClaudeUsage_Empty guarantees graceful handling of empty /
// malformed payloads.
func TestParseClaudeUsage_Empty(t *testing.T) {
	for name, payload := range map[string][]byte{
		"nil":       nil,
		"empty":     {},
		"malformed": []byte(`{{`),
		"no-usage":  []byte(`{"type":"message_stop"}`),
	} {
		t.Run(name, func(t *testing.T) {
			tokens, ok := ParseClaudeUsage(payload)
			if ok {
				t.Fatalf("expected ok=false, got tokens=%+v", tokens)
			}
			if tokens != (UsageTokens{}) {
				t.Fatalf("expected zero UsageTokens, got %+v", tokens)
			}
		})
	}
}

// TestParseGeminiUsage_WithThoughts validates the Gemini usageMetadata
// shape including the thoughtsTokenCount reasoning field.
func TestParseGeminiUsage_WithThoughts(t *testing.T) {
	payload := []byte(`{
		"usageMetadata": {
			"promptTokenCount": 300,
			"candidatesTokenCount": 90,
			"thoughtsTokenCount": 120,
			"cachedContentTokenCount": 50
		}
	}`)
	tokens, ok := ParseGeminiUsage(payload)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if tokens.Input != 300 {
		t.Errorf("Input = %d, want 300", tokens.Input)
	}
	if tokens.Output != 90 {
		t.Errorf("Output = %d, want 90", tokens.Output)
	}
	if tokens.Reasoning != 120 {
		t.Errorf("Reasoning = %d, want 120", tokens.Reasoning)
	}
	if tokens.Cached != 50 {
		t.Errorf("Cached = %d, want 50", tokens.Cached)
	}
}

// TestParseGeminiUsage_Empty ensures missing/invalid payloads return the
// zero value without panicking.
func TestParseGeminiUsage_Empty(t *testing.T) {
	for name, payload := range map[string][]byte{
		"nil":        nil,
		"empty":      {},
		"malformed":  []byte(`garbage`),
		"no-meta":    []byte(`{"candidates":[]}`),
		"empty-meta": []byte(`{"usageMetadata":{}}`),
	} {
		t.Run(name, func(t *testing.T) {
			tokens, ok := ParseGeminiUsage(payload)
			if ok {
				t.Fatalf("expected ok=false, got tokens=%+v", tokens)
			}
			if tokens != (UsageTokens{}) {
				t.Fatalf("expected zero UsageTokens, got %+v", tokens)
			}
		})
	}
}

// TestParseCodexUsage_Body validates that Codex bodies (OpenAI-compatible
// usage envelope) are parsed identically to OpenAI payloads.
func TestParseCodexUsage_Body(t *testing.T) {
	payload := []byte(`{
		"id": "cx-1",
		"usage": {
			"prompt_tokens": 42,
			"completion_tokens": 128,
			"prompt_tokens_details": {"cached_tokens": 12},
			"completion_tokens_details": {"reasoning_tokens": 64}
		}
	}`)
	tokens, ok := ParseCodexUsage(payload)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if tokens.Input != 42 {
		t.Errorf("Input = %d, want 42", tokens.Input)
	}
	if tokens.Output != 128 {
		t.Errorf("Output = %d, want 128", tokens.Output)
	}
	if tokens.Cached != 12 {
		t.Errorf("Cached = %d, want 12", tokens.Cached)
	}
	if tokens.Reasoning != 64 {
		t.Errorf("Reasoning = %d, want 64", tokens.Reasoning)
	}

	// Empty codex payload must not panic.
	if _, ok := ParseCodexUsage(nil); ok {
		t.Fatal("expected ok=false for nil payload")
	}
}

// TestParseVertexUsage covers the Vertex AI response shape, which mirrors
// the Gemini usageMetadata envelope.
func TestParseVertexUsage(t *testing.T) {
	payload := []byte(`{
		"usageMetadata": {
			"promptTokenCount": 500,
			"candidatesTokenCount": 250,
			"thoughtsTokenCount": 80
		}
	}`)
	tokens, ok := ParseVertexUsage(payload)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if tokens.Input != 500 {
		t.Errorf("Input = %d, want 500", tokens.Input)
	}
	if tokens.Output != 250 {
		t.Errorf("Output = %d, want 250", tokens.Output)
	}
	if tokens.Reasoning != 80 {
		t.Errorf("Reasoning = %d, want 80", tokens.Reasoning)
	}

	// Empty vertex payload must not panic.
	if _, ok := ParseVertexUsage(nil); ok {
		t.Fatal("expected ok=false for nil payload")
	}
	if _, ok := ParseVertexUsage([]byte(`not-json`)); ok {
		t.Fatal("expected ok=false for malformed JSON")
	}
}

// --- UsageDetailPresent context carrier ------------------------------------

// TestUsageDetailPresentRoundTrip verifies that a value written with
// WithUsageDetailPresent is observable via UsageDetailPresentFromContext
// with the expected (present, ok) pair. Both true and false markers must
// round-trip, so the signal can distinguish "parsed usage envelope" from
// "explicitly marked as missing".
func TestUsageDetailPresentRoundTrip(t *testing.T) {
	base := context.Background()

	for _, want := range []bool{true, false} {
		ctx := WithUsageDetailPresent(base, want)
		got, ok := UsageDetailPresentFromContext(ctx)
		if !ok {
			t.Fatalf("present=%v: expected ok=true after WithUsageDetailPresent", want)
		}
		if got != want {
			t.Fatalf("present=%v: got %v", want, got)
		}
	}
}

// TestUsageDetailPresentMissingKey enforces the critical safety default
// documented in Requirement 1: a context that never carried the marker must
// yield (false, false), so consumers treating ok=false as "not presented"
// stay on the fallback / strict code path.
func TestUsageDetailPresentMissingKey(t *testing.T) {
	got, ok := UsageDetailPresentFromContext(context.Background())
	if ok {
		t.Fatalf("expected ok=false for bare context, got (%v, true)", got)
	}
	if got {
		t.Fatalf("expected present=false for bare context, got true")
	}
}

// TestUsageDetailPresentNilContext confirms that passing a nil context is
// non-fatal in both directions. The carrier is crossing a boundary between
// executor publishers and the SDK consumer, so defensive handling of nil
// prevents panics if an intermediate layer forgets to forward ctx.
func TestUsageDetailPresentNilContext(t *testing.T) {
	// Read from nil context.
	got, ok := UsageDetailPresentFromContext(nil) //nolint:staticcheck // intentional nil check
	if ok || got {
		t.Fatalf("expected (false,false) from nil context, got (%v,%v)", got, ok)
	}

	// Write to nil context: must upgrade to context.Background and succeed.
	ctx := WithUsageDetailPresent(nil, true) //nolint:staticcheck // intentional nil check
	if ctx == nil {
		t.Fatalf("WithUsageDetailPresent(nil, true) returned nil ctx")
	}
	got, ok = UsageDetailPresentFromContext(ctx)
	if !ok || !got {
		t.Fatalf("expected (true,true) after WithUsageDetailPresent(nil,true), got (%v,%v)", got, ok)
	}
}

// TestUsageDetailPresentOverwrite documents the last-write-wins semantics
// that the sdk/usage plugin relies on: if an executor layer wraps the ctx
// twice (e.g. retry stacking), the innermost marker governs.
func TestUsageDetailPresentOverwrite(t *testing.T) {
	ctx := WithUsageDetailPresent(context.Background(), false)
	ctx = WithUsageDetailPresent(ctx, true)

	got, ok := UsageDetailPresentFromContext(ctx)
	if !ok || !got {
		t.Fatalf("expected overwrite to stick, got (%v,%v)", got, ok)
	}
}
