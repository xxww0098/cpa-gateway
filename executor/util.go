package executor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Shared provider identifiers (mirrors handler_proxy.go).
const (
	providerOpenAI = "openai"
	providerClaude = "claude"
	providerGemini = "gemini"
	providerCodex  = "codex"
	providerVertex = "vertex"

	// ProviderOpenAI / ProviderClaude / ... are the exported aliases for use
	// by callers outside the executor package (e.g. main.go).
	ProviderOpenAI = providerOpenAI
	ProviderClaude = providerClaude
	ProviderGemini = providerGemini
	ProviderCodex  = providerCodex
	ProviderVertex = providerVertex

	defaultTimeout = 60 * time.Second
	// DefaultTimeout is the exported alias for external callers.
	DefaultTimeout = defaultTimeout
)

// ProviderConfig holds provider-specific SDK upstream settings.
type ProviderConfig struct {
	BaseURL string
	APIKey  string
	Enabled bool
}

// upstreamStatusError wraps a non-2xx response from an upstream provider.
type upstreamStatusError struct {
	status  int
	payload []byte
}

// UpstreamStatusError is the exported alias used by the root package for
// status-code extraction via errors.As.
type UpstreamStatusError = upstreamStatusError

func (e *upstreamStatusError) Error() string {
	if e == nil {
		return "upstream error"
	}
	return fmt.Sprintf("upstream status %d", e.status)
}

// StatusCode exposes the upstream status for root-side error translation.
func (e *upstreamStatusError) StatusCode() int {
	if e == nil {
		return http.StatusBadGateway
	}
	return e.status
}

// Payload exposes the raw response body captured when the upstream errored.
func (e *upstreamStatusError) Payload() []byte {
	if e == nil {
		return nil
	}
	return e.payload
}

func approximateTokensFromBytes(size int) int {
	if size <= 0 {
		return 0
	}
	return (size + 3) / 4
}

// ApproximateTokensFromBytes is the exported alias for callers outside the package.
func ApproximateTokensFromBytes(size int) int { return approximateTokensFromBytes(size) }

func copyOutboundHeaders(dst, src http.Header) {
	for key, vals := range src {
		if shouldSkipProxyHeader(key) {
			continue
		}
		for _, val := range vals {
			dst.Add(key, val)
		}
	}
}

// CopyOutboundHeaders is the exported alias for callers outside the package.
func CopyOutboundHeaders(dst, src http.Header) { copyOutboundHeaders(dst, src) }

func sanitizedProxyHeaders(headers http.Header) http.Header {
	cloned := make(http.Header)
	copyOutboundHeaders(cloned, headers)
	return cloned
}

// SanitizedProxyHeaders is the exported alias for callers outside the package.
func SanitizedProxyHeaders(headers http.Header) http.Header { return sanitizedProxyHeaders(headers) }

func shouldSkipProxyHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization", "connection", "content-length", "host",
		"keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

// stringFromMap / nestedString / stringFromJSONString / stringFromJSONBytes
// are shared helpers used by provider-specific credential resolution.

func stringFromMap(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	raw, ok := values[key]
	if !ok {
		return ""
	}
	switch typed := raw.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	case float64:
		if typed == float64(int64(typed)) {
			return fmt.Sprintf("%d", int64(typed))
		}
		return fmt.Sprintf("%v", typed)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		if data, err := json.Marshal(typed); err == nil {
			return string(data)
		}
		return ""
	}
}

func nestedString(values map[string]any, parent string, key string) string {
	if values == nil {
		return ""
	}
	raw, ok := values[parent]
	if !ok {
		return ""
	}
	switch typed := raw.(type) {
	case map[string]any:
		return stringFromMap(typed, key)
	case string:
		return stringFromJSONString(typed, key)
	case json.RawMessage:
		return stringFromJSONBytes(typed, key)
	case []byte:
		return stringFromJSONBytes(typed, key)
	default:
		return ""
	}
}

func stringFromJSONString(raw string, key string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return stringFromJSONBytes([]byte(raw), key)
}

func stringFromJSONBytes(data []byte, key string) string {
	if len(data) == 0 {
		return ""
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return ""
	}
	return stringFromMap(parsed, key)
}
