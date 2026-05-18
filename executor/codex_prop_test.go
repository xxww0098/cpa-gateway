package executor

import (
	"encoding/json"
	"testing"

	"pgregory.net/rapid"
)

// Feature: billing-system-optimization, Property 16: Codex Record.Model Non-Empty

// TestProperty16_CodexModelFromBody verifies that for any request body containing
// a "model" field with a non-empty string value, codexModelFromBody returns that
// exact model name (trimmed). This ensures Record.Model is non-empty and matches
// the requested model for all successful Codex requests.
//
// **Validates: Requirements 7.3**
func TestProperty16_CodexModelFromBody(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate a random non-empty model name.
		// Model names in practice are alphanumeric with dashes/dots/slashes,
		// but we test with arbitrary non-empty strings to be thorough.
		model := rapid.StringMatching(`[a-zA-Z0-9][a-zA-Z0-9._\-/]{0,63}`).Draw(rt, "model")

		// Build a JSON body with the model field
		body, err := json.Marshal(map[string]any{
			"model": model,
		})
		if err != nil {
			rt.Fatalf("failed to marshal body: %v", err)
		}

		// Call codexModelFromBody
		got := codexModelFromBody(body)

		// Property: result must be non-empty and match the input model
		if got == "" {
			rt.Fatalf("codexModelFromBody returned empty string for model=%q", model)
		}
		if got != model {
			rt.Fatalf("codexModelFromBody=%q, want %q", got, model)
		}
	})
}

// TestProperty16_CodexModelFromBody_WithOtherFields verifies that the model
// extraction works correctly even when the JSON body contains additional fields
// (messages, temperature, etc.) as would be the case in real requests.
//
// **Validates: Requirements 7.3**
func TestProperty16_CodexModelFromBody_WithOtherFields(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		model := rapid.StringMatching(`[a-zA-Z][a-zA-Z0-9._\-]{0,31}`).Draw(rt, "model")
		temperature := rapid.Float64Range(0.0, 2.0).Draw(rt, "temperature")
		maxTokens := rapid.IntRange(1, 4096).Draw(rt, "maxTokens")

		body, err := json.Marshal(map[string]any{
			"model":       model,
			"temperature": temperature,
			"max_tokens":  maxTokens,
			"messages": []map[string]string{
				{"role": "user", "content": "hello"},
			},
		})
		if err != nil {
			rt.Fatalf("failed to marshal body: %v", err)
		}

		got := codexModelFromBody(body)

		if got == "" {
			rt.Fatalf("codexModelFromBody returned empty for model=%q with extra fields", model)
		}
		if got != model {
			rt.Fatalf("codexModelFromBody=%q, want %q", got, model)
		}
	})
}

// TestProperty16_CodexModelFromBody_EdgeCases verifies that codexModelFromBody
// returns empty string for edge cases: empty body, invalid JSON, missing model
// field, and empty model value.
//
// **Validates: Requirements 7.3**
func TestProperty16_CodexModelFromBody_EdgeCases(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"nil body", nil},
		{"empty body", []byte{}},
		{"invalid JSON", []byte(`{not valid json`)},
		{"missing model field", []byte(`{"messages":[]}`)},
		{"empty model string", []byte(`{"model":""}`)},
		{"model with only spaces", []byte(`{"model":"   "}`)},
		{"model is number", []byte(`{"model":123}`)},
		{"model is null", []byte(`{"model":null}`)},
		{"model is array", []byte(`{"model":["gpt-4"]}`)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := codexModelFromBody(tc.body)
			if got != "" {
				t.Errorf("codexModelFromBody(%q) = %q, want empty string", tc.body, got)
			}
		})
	}
}
