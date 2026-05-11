package executor

import (
	"bytes"
	"encoding/json"
)

// UsageTokens is the provider-agnostic token tally extracted from an upstream
// response body. It is declared in the executor package (not in `pricing`) so
// that Task 15-19 integrations can import it without introducing a circular
// dependency between executor and pricing. Task 14 (`sdk/usage.go`) handles the
// conversion between executor.UsageTokens and pricing.UsageTokens via a simple
// field mapping.
type UsageTokens struct {
	Input     int64
	Output    int64
	Cached    int64
	Reasoning int64
}

// hasValues reports whether any of the four token counts are non-zero. A
// strict non-zero check is intentional: payloads that parse cleanly but carry
// all-zero counters are treated as "no usage info" so callers can fall back to
// heuristic accounting.
func (u UsageTokens) hasValues() bool {
	return u.Input != 0 || u.Output != 0 || u.Cached != 0 || u.Reasoning != 0
}

// trimmedJSON returns the payload with leading/trailing whitespace stripped,
// or nil when the result is empty. It also gives us a single place to reject
// obviously empty inputs before hitting json.Unmarshal.
func trimmedJSON(payload []byte) []byte {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil
	}
	return trimmed
}

// --- OpenAI -----------------------------------------------------------------

type openAIUsageEnvelope struct {
	Usage *openAIUsage `json:"usage"`
}

type openAIUsage struct {
	PromptTokens            int64                          `json:"prompt_tokens"`
	CompletionTokens        int64                          `json:"completion_tokens"`
	TotalTokens             int64                          `json:"total_tokens"`
	PromptTokensDetails     *openAIPromptTokensDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails *openAICompletionTokensDetails `json:"completion_tokens_details"`
}

type openAIPromptTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type openAICompletionTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// ParseOpenAIUsage extracts UsageTokens from an OpenAI (Chat Completions /
// Responses API) JSON payload. The envelope is:
//
//	{"usage": {"prompt_tokens": ..., "completion_tokens": ...,
//	           "prompt_tokens_details": {"cached_tokens": ...},
//	           "completion_tokens_details": {"reasoning_tokens": ...}}}
//
// Returns (UsageTokens{}, false) for nil, empty, malformed, or missing-usage
// payloads — never panics.
func ParseOpenAIUsage(payload []byte) (UsageTokens, bool) {
	data := trimmedJSON(payload)
	if data == nil {
		return UsageTokens{}, false
	}
	var env openAIUsageEnvelope
	if err := json.Unmarshal(data, &env); err != nil || env.Usage == nil {
		return UsageTokens{}, false
	}
	tokens := UsageTokens{
		Input:  env.Usage.PromptTokens,
		Output: env.Usage.CompletionTokens,
	}
	if env.Usage.PromptTokensDetails != nil {
		tokens.Cached = env.Usage.PromptTokensDetails.CachedTokens
	}
	if env.Usage.CompletionTokensDetails != nil {
		tokens.Reasoning = env.Usage.CompletionTokensDetails.ReasoningTokens
	}
	if !tokens.hasValues() {
		return UsageTokens{}, false
	}
	return tokens, true
}

// --- Claude -----------------------------------------------------------------

type claudeUsageEnvelope struct {
	Usage   *claudeUsage           `json:"usage"`
	Delta   *claudeDelta           `json:"delta"`
	Message *claudeEmbeddedMessage `json:"message"`
}

type claudeEmbeddedMessage struct {
	Usage *claudeUsage `json:"usage"`
}

type claudeDelta struct {
	Usage *claudeUsage `json:"usage"`
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// ParseClaudeUsage extracts UsageTokens from Anthropic Claude responses and
// streaming events. Claude can express usage in multiple shapes:
//
//   - message_start: {"type":"message_start","message":{"usage": {...}}}
//   - message_delta: {"type":"message_delta","delta":{"usage":{"output_tokens":N}},
//     "message":{"usage":{"input_tokens":M}}}
//   - non-streaming: {"type":"message","usage":{...}}
//
// cache_creation_input_tokens + cache_read_input_tokens both contribute to
// UsageTokens.Cached.
func ParseClaudeUsage(payload []byte) (UsageTokens, bool) {
	data := trimmedJSON(payload)
	if data == nil {
		return UsageTokens{}, false
	}
	var env claudeUsageEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return UsageTokens{}, false
	}

	tokens := UsageTokens{}
	merge := func(u *claudeUsage) {
		if u == nil {
			return
		}
		if u.InputTokens > tokens.Input {
			tokens.Input = u.InputTokens
		}
		if u.OutputTokens > tokens.Output {
			tokens.Output = u.OutputTokens
		}
		tokens.Cached += u.CacheCreationInputTokens + u.CacheReadInputTokens
	}
	merge(env.Usage)
	if env.Message != nil {
		merge(env.Message.Usage)
	}
	if env.Delta != nil {
		merge(env.Delta.Usage)
	}

	if !tokens.hasValues() {
		return UsageTokens{}, false
	}
	return tokens, true
}

// --- Gemini / Vertex --------------------------------------------------------

type geminiUsageEnvelope struct {
	UsageMetadata *geminiUsageMetadata `json:"usageMetadata"`
}

type geminiUsageMetadata struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	TotalTokenCount         int64 `json:"totalTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
}

func parseGeminiShapedUsage(payload []byte) (UsageTokens, bool) {
	data := trimmedJSON(payload)
	if data == nil {
		return UsageTokens{}, false
	}
	var env geminiUsageEnvelope
	if err := json.Unmarshal(data, &env); err != nil || env.UsageMetadata == nil {
		return UsageTokens{}, false
	}
	tokens := UsageTokens{
		Input:     env.UsageMetadata.PromptTokenCount,
		Output:    env.UsageMetadata.CandidatesTokenCount,
		Reasoning: env.UsageMetadata.ThoughtsTokenCount,
		Cached:    env.UsageMetadata.CachedContentTokenCount,
	}
	if !tokens.hasValues() {
		return UsageTokens{}, false
	}
	return tokens, true
}

// ParseGeminiUsage extracts UsageTokens from a Google AI Gemini
// generateContent / streamGenerateContent response.
//
//	{"usageMetadata": {
//	   "promptTokenCount": ..., "candidatesTokenCount": ...,
//	   "thoughtsTokenCount": ..., "cachedContentTokenCount": ...}}
func ParseGeminiUsage(payload []byte) (UsageTokens, bool) {
	return parseGeminiShapedUsage(payload)
}

// ParseVertexUsage extracts UsageTokens from a Vertex AI response. Vertex
// mirrors the Gemini usageMetadata envelope, so the shared implementation is
// reused directly.
func ParseVertexUsage(payload []byte) (UsageTokens, bool) {
	return parseGeminiShapedUsage(payload)
}

// --- Codex ------------------------------------------------------------------

// ParseCodexUsage extracts UsageTokens from a Codex/OpenAI-OAuth response
// body. Codex surfaces usage through the OpenAI-compatible `usage` field, so
// parsing is delegated to ParseOpenAIUsage. Header-only (`x-usage-*`) parsing
// is intentionally deferred to the executor layer where http.Header is
// available; this function focuses on the body path.
func ParseCodexUsage(payload []byte) (UsageTokens, bool) {
	return ParseOpenAIUsage(payload)
}
