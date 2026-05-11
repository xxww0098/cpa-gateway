package api

import (
	"encoding/json"
	"fmt"
	"strings"
)

// stringFromMap returns a trimmed string value for the given key from a
// free-form map typically produced by JSON decoding. Non-string, non-Stringer
// values yield the empty string (matching the prior in-executor behavior so
// callers like handler_sdk_mgmt continue to work unchanged).
func stringFromMap(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	raw, ok := values[key]
	if !ok {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	default:
		return ""
	}
}

// nestedString reads values[parent][key] coping with parent being a map,
// JSON-encoded string, or raw bytes. Mirrors the helper previously defined
// alongside the root-package executors.
func nestedString(values map[string]any, parent string, key string) string {
	if values == nil {
		return ""
	}
	raw, ok := values[parent]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case map[string]any:
		return stringFromMap(v, key)
	case map[string]string:
		return strings.TrimSpace(v[key])
	case json.RawMessage:
		return stringFromJSONBytes(v, key)
	case []byte:
		return stringFromJSONBytes(v, key)
	case string:
		return stringFromJSONString(v, key)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return stringFromJSONBytes(data, key)
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
	var values map[string]any
	if err := json.Unmarshal(data, &values); err != nil {
		return ""
	}
	return stringFromMap(values, key)
}
