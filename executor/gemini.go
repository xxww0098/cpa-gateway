package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const (
	geminiDefaultBaseURL  = "https://generativelanguage.googleapis.com"
	geminiMetadataAPIKey  = "api_key"
	geminiTokenData       = "token_data"
	geminiAccessToken     = "access_token"
	geminiCredentialQuery = "key"
)

// GeminiExecutor implements cliproxyauth.ProviderExecutor for Google AI Gemini.
// It intentionally avoids SDK internal packages; CPA-Gateway owns all HTTP IO.
type GeminiExecutor struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewGeminiExecutor creates a GeminiExecutor from the provided config.
func NewGeminiExecutor(cfg ProviderConfig, timeoutSeconds int) (*GeminiExecutor, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = geminiDefaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid gemini base_url")
	}

	timeout := defaultTimeout
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	return &GeminiExecutor{
		baseURL: baseURL,
		apiKey:  strings.TrimSpace(cfg.APIKey),
		client:  &http.Client{Timeout: timeout},
	}, nil
}

// BaseURL exposes the configured upstream base URL.
func (e *GeminiExecutor) BaseURL() string { return e.baseURL }

func (e *GeminiExecutor) Identifier() string { return providerGemini }

func (e *GeminiExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	apiKey, baseURL := e.resolveCredentials(auth)
	startedAt := time.Now().UTC()
	resp, err := e.doGenerateContentRequest(ctx, req, opts, apiKey, baseURL, false)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	wrapped := cliproxyexecutor.Response{
		Payload: payload,
		Headers: resp.Header.Clone(),
		Metadata: map[string]any{"status_code": resp.StatusCode},
	}
	failed := resp.StatusCode >= http.StatusBadRequest
	tokens, ok := ParseGeminiUsage(payload)
	if ok {
		wrapped.Metadata["usage"] = tokens
	}
	e.publishUsage(ctx, auth, req, opts, tokens, ok, failed, resp.StatusCode, payload, startedAt)
	if failed {
		return wrapped, &upstreamStatusError{status: resp.StatusCode, payload: payload}
	}
	return wrapped, nil
}

func (e *GeminiExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	apiKey, baseURL := e.resolveCredentials(auth)
	streamOpts := opts
	streamOpts.Stream = true
	startedAt := time.Now().UTC()
	resp, err := e.doGenerateContentRequest(ctx, req, streamOpts, apiKey, baseURL, true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		defer resp.Body.Close()
		payload, _ := io.ReadAll(resp.Body)
		e.publishUsage(ctx, auth, req, streamOpts, UsageTokens{}, false, true, resp.StatusCode, payload, startedAt)
		return nil, &upstreamStatusError{status: resp.StatusCode, payload: payload}
	}

	chunks := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(chunks)
		defer resp.Body.Close()

		var accumulator bytes.Buffer
		var streamErr error
		buf := make([]byte, 32*1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				payload := make([]byte, n)
				copy(payload, buf[:n])
				accumulator.Write(payload)
				select {
				case <-ctx.Done():
					streamErr = ctx.Err()
					chunks <- cliproxyexecutor.StreamChunk{Err: streamErr}
					e.publishStreamUsage(ctx, auth, req, streamOpts, accumulator.Bytes(), true, 0, startedAt)
					return
				case chunks <- cliproxyexecutor.StreamChunk{Payload: payload}:
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					streamErr = err
					chunks <- cliproxyexecutor.StreamChunk{Err: err}
				}
				e.publishStreamUsage(ctx, auth, req, streamOpts, accumulator.Bytes(), streamErr != nil, resp.StatusCode, startedAt)
				return
			}
		}
	}()

	return &cliproxyexecutor.StreamResult{Headers: resp.Header.Clone(), Chunks: chunks}, nil
}

// Refresh is a no-op for Gemini API-key auth. OAuth-style records are kept active
// so startup and persisted credential loading do not fail; OAuth refresh is handled
// by future management tasks rather than this provider executor.
func (e *GeminiExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	_ = ctx
	if auth == nil {
		return nil, nil
	}
	clone := auth.Clone()
	clone.Status = cliproxyauth.StatusActive
	clone.UpdatedAt = time.Now().UTC()
	return clone, nil
}

func (e *GeminiExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = auth
	_ = opts
	tokens := approximateTokensFromBytes(len(req.Payload))
	payload, _ := json.Marshal(map[string]any{"total_tokens": tokens})
	return cliproxyexecutor.Response{Payload: payload, Headers: http.Header{"Content-Type": []string{"application/json"}}}, nil
}

func (e *GeminiExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("http request is required")
	}
	apiKey, _ := e.resolveCredentials(auth)
	req = req.WithContext(ctx)
	e.injectAPIKey(req, apiKey)
	return e.client.Do(req)
}

func (e *GeminiExecutor) doGenerateContentRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, apiKey string, baseURL string, stream bool) (*http.Response, error) {
	model := geminiRequestedModel(req, opts)
	if model == "" {
		return nil, fmt.Errorf("gemini model is required")
	}
	endpoint, err := e.generateContentEndpoint(opts.Query, baseURL, model, stream)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Payload))
	if err != nil {
		return nil, err
	}
	copyOutboundHeaders(httpReq.Header, opts.Headers)
	if httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else if httpReq.Header.Get("Accept") == "" {
		httpReq.Header.Set("Accept", "application/json")
	}
	e.injectAPIKey(httpReq, apiKey)
	return e.client.Do(httpReq)
}

func (e *GeminiExecutor) injectAPIKey(req *http.Request, apiKey string) {
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("x-goog-api-key", strings.TrimSpace(apiKey))
		return
	}
	if req.URL != nil && req.URL.Query().Get(geminiCredentialQuery) != "" {
		return
	}
}

func (e *GeminiExecutor) resolveCredentials(auth *cliproxyauth.Auth) (apiKey string, baseURL string) {
	apiKey = strings.TrimSpace(e.apiKey)
	baseURL = strings.TrimRight(strings.TrimSpace(e.baseURL), "/")
	if baseURL == "" {
		baseURL = geminiDefaultBaseURL
	}
	if auth == nil {
		return apiKey, baseURL
	}
	if u, ok := auth.Attributes["base_url"]; ok && strings.TrimSpace(u) != "" {
		baseURL = strings.TrimRight(strings.TrimSpace(u), "/")
	} else if u, ok := auth.Attributes["base-url"]; ok && strings.TrimSpace(u) != "" {
		baseURL = strings.TrimRight(strings.TrimSpace(u), "/")
	}

	if value := stringFromMap(auth.Metadata, geminiMetadataAPIKey); value != "" {
		return value, baseURL
	}
	if value := nestedString(auth.Metadata, geminiTokenData, geminiMetadataAPIKey); value != "" {
		return value, baseURL
	}
	if value := stringFromMap(auth.Metadata, geminiAccessToken); value != "" {
		return value, baseURL
	}
	if value := nestedString(auth.Metadata, geminiTokenData, geminiAccessToken); value != "" {
		return value, baseURL
	}
	return apiKey, baseURL
}

func (e *GeminiExecutor) generateContentEndpoint(query url.Values, baseURL string, model string, stream bool) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = geminiDefaultBaseURL
	}
	action := "generateContent"
	if stream {
		action = "streamGenerateContent"
	}
	parsed, err := url.Parse(base + "/v1beta/models/" + url.PathEscape(model) + ":" + action)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid gemini base_url")
	}
	values := parsed.Query()
	for key, vals := range query {
		for _, val := range vals {
			values.Add(key, val)
		}
	}
	if stream {
		values.Set("alt", "sse")
	}
	parsed.RawQuery = values.Encode()
	return parsed.String(), nil
}

func geminiRequestedModel(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	if strings.TrimSpace(req.Model) != "" {
		return strings.TrimSpace(req.Model)
	}
	if value := stringFromMap(req.Metadata, cliproxyexecutor.RequestedModelMetadataKey); value != "" {
		return value
	}
	return stringFromMap(opts.Metadata, cliproxyexecutor.RequestedModelMetadataKey)
}

// publishUsage emits a usage.Record to the SDK default manager. Gemini's
// usageMetadata envelope maps promptTokenCount → Input, candidatesTokenCount →
// Output, thoughtsTokenCount → Reasoning, and cachedContentTokenCount →
// Cached. When parsing fails the record is still published with zero Detail so
// downstream UsagePlugin can fall back to heuristic accounting.
func (e *GeminiExecutor) publishUsage(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, tokens UsageTokens, parsed bool, failed bool, status int, payload []byte, startedAt time.Time) {
	rec := cliproxyusage.Record{
		Provider:    providerGemini,
		Model:       geminiRequestedModel(req, opts),
		Alias:       cliproxyusage.RequestedModelAliasFromContext(ctx),
		Source:      providerGemini,
		RequestedAt: startedAt,
		Latency:     time.Since(startedAt),
		Failed:      failed,
	}
	if auth != nil {
		rec.AuthID = auth.ID
		rec.AuthIndex = auth.Index
		rec.AuthType = auth.Provider
	}
	if parsed {
		rec.Detail = cliproxyusage.Detail{
			InputTokens:     tokens.Input,
			OutputTokens:    tokens.Output,
			ReasoningTokens: tokens.Reasoning,
			CachedTokens:    tokens.Cached,
			TotalTokens:     tokens.Input + tokens.Output,
		}
	}
	if failed {
		rec.Fail = cliproxyusage.Failure{
			StatusCode: status,
			Body:       truncateGeminiFailureBody(payload),
		}
	}
	cliproxyusage.PublishRecord(ctx, rec)
}

// publishStreamUsage parses the accumulated streaming body and delegates to
// publishUsage. Gemini streamGenerateContent yields either SSE-framed `data:
// {json}` events (when `alt=sse` is set, as this executor does) or newline-
// separated JSON chunks. The final chunk carries the aggregate `usageMetadata`
// envelope, so we scan all events and keep the last successful parse.
func (e *GeminiExecutor) publishStreamUsage(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, body []byte, failed bool, status int, startedAt time.Time) {
	tokens, ok := UsageTokens{}, false
	if len(body) > 0 {
		tokens, ok = parseGeminiStreamUsage(body)
	}
	e.publishUsage(ctx, auth, req, opts, tokens, ok, failed, status, nil, startedAt)
}

// parseGeminiStreamUsage scans a streamGenerateContent response body for the
// last chunk that carries usageMetadata. Two shapes are tolerated:
//
//  1. SSE framing (`alt=sse`): every chunk is prefixed with `data: ` and
//     separated by a blank line. The final non-`[DONE]` data event carries
//     the aggregate usageMetadata.
//  2. NDJSON-ish framing: JSON objects separated by `\n\n`. Same semantics as
//     above minus the `data: ` prefix.
//
// A plain JSON body is also tolerated (fast path) for safety. Falls back to
// (zero, false) when no chunk yields parseable usage.
func parseGeminiStreamUsage(body []byte) (UsageTokens, bool) {
	// Fast path: plain JSON body with top-level usageMetadata.
	if tokens, ok := ParseGeminiUsage(body); ok {
		return tokens, true
	}

	var (
		last UsageTokens
		got  bool
	)

	// SSE path: walk every `data: ` payload and keep the last successful parse.
	for _, line := range bytes.Split(body, []byte{'\n'}) {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(trimmed[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if tokens, ok := ParseGeminiUsage(payload); ok {
			last = tokens
			got = true
		}
	}
	if got {
		return last, true
	}

	// NDJSON / `\n\n`-separated path: split on blank lines and try each chunk.
	for _, chunk := range bytes.Split(body, []byte("\n\n")) {
		trimmed := bytes.TrimSpace(chunk)
		if len(trimmed) == 0 {
			continue
		}
		if tokens, ok := ParseGeminiUsage(trimmed); ok {
			last = tokens
			got = true
		}
	}
	return last, got
}

// truncateGeminiFailureBody clips the failure payload so usage.Record.Fail.Body
// stays bounded. 4 KiB is more than enough for provider error envelopes.
func truncateGeminiFailureBody(payload []byte) string {
	const max = 4 * 1024
	if len(payload) <= max {
		return string(payload)
	}
	return string(payload[:max])
}
