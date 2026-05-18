package executor

import (
	"bytes"
	"encoding/json"
	"testing"

	"pgregory.net/rapid"
)

// Feature: billing-security-hardening, Property 4: Streaming requests force
// include_usage=true.
//
// EnsureIncludeUsage rewrites an OpenAI-compatible chat.completions payload so
// that streaming responses always carry a terminal usage envelope. The tests
// below cover the three invariants called out in design.md Property 4 and
// Requirement 1.2:
//
//  1. When the payload is valid JSON with `stream == true`, exactly one or
//     two rewrites leave `stream_options.include_usage == true`.
//  2. When the payload is valid JSON with `stream` missing or `stream == false`,
//     the payload is returned unchanged byte-for-byte (the executor does not
//     force usage metadata on non-streaming requests).
//  3. When the payload is not parseable JSON, the function falls through and
//     returns the input unchanged so UsagePlugin's fallback path handles it.

// genChatCompletionsPayload produces a well-formed JSON body that resembles a
// chat.completions request. The generator intentionally covers:
//
//   - `stream`: true / false / absent
//   - `stream_options`: absent / empty object / pre-existing `include_usage`
//     (true or false) / additional sibling keys preserved alongside
//     `include_usage`
//
// Additional unrelated request fields (model, max_tokens, messages) are
// sprinkled in to ensure the rewrite does not clobber unrelated payload
// content.
func genChatCompletionsPayload(rt *rapid.T) []byte {
	body := make(map[string]any)

	if rapid.Bool().Draw(rt, "has_model") {
		body["model"] = rapid.StringMatching(`[a-zA-Z0-9][a-zA-Z0-9._\-]{0,15}`).Draw(rt, "model")
	}
	if rapid.Bool().Draw(rt, "has_max_tokens") {
		body["max_tokens"] = rapid.IntRange(1, 4096).Draw(rt, "max_tokens")
	}
	if rapid.Bool().Draw(rt, "has_messages") {
		body["messages"] = []map[string]string{
			{"role": "user", "content": rapid.String().Draw(rt, "content")},
		}
	}

	// stream field: true (0), false (1), absent (2)
	switch rapid.IntRange(0, 2).Draw(rt, "stream_choice") {
	case 0:
		body["stream"] = true
	case 1:
		body["stream"] = false
	case 2:
		// absent
	}

	// stream_options variants: absent / empty / {include_usage:true} /
	// {include_usage:false} / {include_usage: ?, include_input_tokens: ?}.
	switch rapid.IntRange(0, 4).Draw(rt, "so_choice") {
	case 0:
		// absent
	case 1:
		body["stream_options"] = map[string]any{}
	case 2:
		body["stream_options"] = map[string]any{"include_usage": true}
	case 3:
		body["stream_options"] = map[string]any{"include_usage": false}
	case 4:
		body["stream_options"] = map[string]any{
			"include_usage":        rapid.Bool().Draw(rt, "incl_usage"),
			"include_input_tokens": rapid.Bool().Draw(rt, "incl_input"),
		}
	}

	out, err := json.Marshal(body)
	if err != nil {
		rt.Fatalf("generator marshal failed: %v", err)
	}
	return out
}

// TestEnsureIncludeUsageIdempotent verifies Property 4's core invariants:
//   - Streaming payloads (stream=true) end up with
//     stream_options.include_usage == true after one rewrite, and a second
//     rewrite produces byte-identical output.
//   - Non-streaming payloads (stream=false or absent) are returned unchanged.
//
// The property runs twice the rapid default (100 * 2 = 200) to satisfy the
// ≥ 200 iteration requirement spelled out in tasks.md 4.8.
//
// **Validates: Requirements 1.2**
func TestEnsureIncludeUsageIdempotent(t *testing.T) {
	// rapid.Check defaults to 100 iterations; loop twice to satisfy the
	// ≥ 200 target. A tighter budget can still be driven through
	// -rapid.checks at CI time; the loop is a floor, not a ceiling.
	for i := 0; i < 2; i++ {
		rapid.Check(t, func(rt *rapid.T) {
			payload := genChatCompletionsPayload(rt)

			// Classify the generated payload so we know which invariant
			// to enforce: streaming payloads must end with include_usage
			// flipped on, non-streaming payloads must be untouched.
			var parsed map[string]any
			if err := json.Unmarshal(payload, &parsed); err != nil {
				rt.Fatalf("generator produced invalid JSON: %v (%s)", err, string(payload))
			}
			streamTrue := false
			if raw, ok := parsed["stream"]; ok {
				if b, isBool := raw.(bool); isBool && b {
					streamTrue = true
				}
			}

			first := EnsureIncludeUsage(payload)
			second := EnsureIncludeUsage(first)

			if streamTrue {
				// Both rewrites must yield a JSON object whose
				// stream_options.include_usage is the literal boolean true.
				for idx, buf := range [][]byte{first, second} {
					var body map[string]any
					if err := json.Unmarshal(buf, &body); err != nil {
						rt.Fatalf("rewrite %d produced invalid JSON: %v (%s)", idx+1, err, string(buf))
					}
					opts, ok := body["stream_options"].(map[string]any)
					if !ok {
						rt.Fatalf("rewrite %d missing stream_options object: %s", idx+1, string(buf))
					}
					raw, present := opts["include_usage"]
					if !present {
						rt.Fatalf("rewrite %d missing stream_options.include_usage: %s", idx+1, string(buf))
					}
					val, isBool := raw.(bool)
					if !isBool || !val {
						rt.Fatalf("rewrite %d stream_options.include_usage != true (got %#v): %s", idx+1, raw, string(buf))
					}
				}
			} else {
				// Non-streaming payload: rewrite must return the input
				// unchanged (byte-for-byte). The helper's contract is that
				// it only mutates when stream is the literal boolean true.
				if !bytes.Equal(first, payload) {
					rt.Fatalf("non-streaming payload altered by rewrite\ninput=%s\nfirst=%s", string(payload), string(first))
				}
			}

			// Idempotence: a second rewrite over the first output must be a
			// no-op at the byte level. This holds because both branches of
			// the rewrite end in json.Marshal over a map[string]any, whose
			// key order is deterministic (alphabetical).
			if !bytes.Equal(first, second) {
				rt.Fatalf("rewrite is not idempotent\nfirst =%s\nsecond=%s", string(first), string(second))
			}
		})
	}
}

// TestEnsureIncludeUsageMalformedFallthrough locks in the defensive fallback
// branch: when the payload is not parseable JSON (or is JSON but not a
// top-level object), EnsureIncludeUsage MUST return the input bytes
// unchanged. Design.md and Requirement 1.2 both treat the rewrite as
// advisory — the UsagePlugin fallback path compensates for missing upstream
// usage data, so blocking a malformed request here would regress on-spec
// behaviour.
//
// **Validates: Requirements 1.2**
func TestEnsureIncludeUsageMalformedFallthrough(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"random bytes", []byte{0xff, 0x00, 0xab, 0x7f, 0x10}},
		{"plain text", []byte("hello, not json")},
		{"unopened brace", []byte(`"stream":true}`)},
		{"truncated object", []byte(`{"stream":true`)},
		{"truncated nested", []byte(`{"stream":true,"stream_options":`)},
		{"unclosed string", []byte(`{"stream":"tr`)},
		{"invalid token", []byte("{not valid json}")},
		{"partial array", []byte("[1,2,")},
		{"json array top-level", []byte("[1,2,3]")},
		{"json primitive number", []byte("42")},
		{"json primitive string", []byte(`"hello"`)},
		{"json null", []byte("null")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := EnsureIncludeUsage(tc.input)
			if !bytes.Equal(out, tc.input) {
				t.Errorf("EnsureIncludeUsage returned modified bytes for %s\ninput =%q\noutput=%q", tc.name, tc.input, out)
			}
		})
	}
}
