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

// OpenAICompatibleExecutor implements cliproxyauth.ProviderExecutor for any
// OpenAI-compatible API.
type OpenAICompatibleExecutor struct {
	provider string
	BaseURL  string
	apiKey   string
	client   *http.Client
}

// NewOpenAICompatibleExecutor creates a new executor from provider config.
func NewOpenAICompatibleExecutor(cfg ProviderConfig, timeoutSeconds int) (*OpenAICompatibleExecutor, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	apiKey := strings.TrimSpace(cfg.APIKey)
	if !cfg.Enabled || baseURL == "" || apiKey == "" {
		return nil, fmt.Errorf("sdk base_url and api_key are required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid sdk base_url")
	}

	timeout := DefaultTimeout
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	return &OpenAICompatibleExecutor{
		provider: ProviderOpenAI,
		BaseURL:  baseURL,
		apiKey:   apiKey,
		client:   &http.Client{Timeout: timeout},
	}, nil
}

func (e *OpenAICompatibleExecutor) Identifier() string {
	return e.provider
}

func (e *OpenAICompatibleExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	apiKey, baseURL := e.resolveCredentials(auth)
	startedAt := time.Now().UTC()
	resp, err := e.doChatCompletionsRequest(ctx, req, opts, apiKey, baseURL)
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
		Metadata: map[string]any{
			"status_code": resp.StatusCode,
		},
	}
	failed := resp.StatusCode >= http.StatusBadRequest
	tokens, ok := ParseOpenAIUsage(payload)
	if ok {
		wrapped.Metadata["usage"] = tokens
	}
	e.publishUsage(ctx, auth, req, opts, tokens, ok, failed, resp.StatusCode, payload, startedAt)
	if failed {
		return wrapped, &upstreamStatusError{status: resp.StatusCode, payload: payload}
	}
	return wrapped, nil
}

func (e *OpenAICompatibleExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	apiKey, baseURL := e.resolveCredentials(auth)
	streamOpts := opts
	streamOpts.Stream = true
	startedAt := time.Now().UTC()
	resp, err := e.doChatCompletionsRequest(ctx, req, streamOpts, apiKey, baseURL)
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

func (e *OpenAICompatibleExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	_ = ctx
	if auth == nil {
		return nil, nil
	}
	clone := auth.Clone()
	clone.Status = cliproxyauth.StatusActive
	clone.UpdatedAt = time.Now().UTC()
	return clone, nil
}

func (e *OpenAICompatibleExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = auth
	_ = opts
	tokens := ApproximateTokensFromBytes(len(req.Payload))
	payload, _ := json.Marshal(map[string]any{"total_tokens": tokens})
	return cliproxyexecutor.Response{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func (e *OpenAICompatibleExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("http request is required")
	}
	apiKey, _ := e.resolveCredentials(auth)
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return e.client.Do(req)
}

func (e *OpenAICompatibleExecutor) resolveCredentials(auth *cliproxyauth.Auth) (apiKey, baseURL string) {
	apiKey = e.apiKey
	baseURL = e.BaseURL
	if auth == nil {
		return
	}
	if raw, ok := auth.Metadata["api_key"]; ok {
		if s, ok := raw.(string); ok && s != "" {
			apiKey = s
		}
	}
	if u, ok := auth.Attributes["base_url"]; ok && u != "" {
		baseURL = u
	} else if u, ok := auth.Attributes["base-url"]; ok && u != "" {
		baseURL = u
	}
	return
}

func (e *OpenAICompatibleExecutor) doChatCompletionsRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, apiKey, baseURL string) (*http.Response, error) {
	endpoint, err := e.chatCompletionsEndpoint(opts.Query, baseURL)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(req.Payload))
	if err != nil {
		return nil, err
	}
	CopyOutboundHeaders(httpReq.Header, opts.Headers)
	if httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if opts.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else if httpReq.Header.Get("Accept") == "" {
		httpReq.Header.Set("Accept", "application/json")
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	return e.client.Do(httpReq)
}

func (e *OpenAICompatibleExecutor) chatCompletionsEndpoint(query url.Values, baseURL string) (string, error) {
	base := strings.TrimRight(baseURL, "/")
	var endpoint string
	switch {
	case strings.HasSuffix(base, "/v1/chat/completions"):
		endpoint = base
	case strings.HasSuffix(base, "/v1"):
		endpoint = base + "/chat/completions"
	default:
		endpoint = base + "/v1/chat/completions"
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	if len(query) > 0 {
		values := parsed.Query()
		for key, vals := range query {
			for _, val := range vals {
				values.Add(key, val)
			}
		}
		parsed.RawQuery = values.Encode()
	}
	return parsed.String(), nil
}

// openAIRequestedModel resolves the upstream-facing model name for a request.
// It prefers the translated Request.Model and falls back to the
// `requested_model` hint stored by the SDK router on either Request or
// Options metadata.
func openAIRequestedModel(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	if strings.TrimSpace(req.Model) != "" {
		return strings.TrimSpace(req.Model)
	}
	if value := stringFromMap(req.Metadata, cliproxyexecutor.RequestedModelMetadataKey); value != "" {
		return value
	}
	return stringFromMap(opts.Metadata, cliproxyexecutor.RequestedModelMetadataKey)
}

// publishUsage emits a usage.Record to the SDK default manager. OpenAI
// Chat Completions surface usage through the top-level `usage` envelope on
// non-stream responses and on the terminal streaming chunk when
// `stream_options.include_usage=true`. ParseOpenAIUsage normalizes both
// shapes into UsageTokens. When parsing fails the record is still published
// with zero Detail so downstream UsagePlugin can fall back to heuristic
// accounting.
func (e *OpenAICompatibleExecutor) publishUsage(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, tokens UsageTokens, parsed bool, failed bool, status int, payload []byte, startedAt time.Time) {
	rec := cliproxyusage.Record{
		Provider:    e.provider,
		Model:       openAIRequestedModel(req, opts),
		Alias:       cliproxyusage.RequestedModelAliasFromContext(ctx),
		Source:      e.provider,
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
			Body:       truncateOpenAIFailureBody(payload),
		}
	}
	cliproxyusage.PublishRecord(ctx, rec)
}

// publishStreamUsage parses the accumulated SSE body and delegates to
// publishUsage. OpenAI streams end with `data: [DONE]`; the penultimate
// `data: {json}` event carries the aggregate `usage` envelope when the
// client opts into `stream_options.include_usage=true`. We walk every
// `data: {json}` line, parse each via ParseOpenAIUsage, and keep the last
// successful parse so the final usage totals win.
func (e *OpenAICompatibleExecutor) publishStreamUsage(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, body []byte, failed bool, status int, startedAt time.Time) {
	tokens, ok := UsageTokens{}, false
	if len(body) > 0 {
		tokens, ok = parseOpenAIStreamUsage(body)
	}
	e.publishUsage(ctx, auth, req, opts, tokens, ok, failed, status, nil, startedAt)
}

// parseOpenAIStreamUsage scans an OpenAI-style SSE buffer for the last
// `data: {...}` event that carries a non-nil `usage` envelope. The stream
// format is:
//
//	data: {"choices":[...]}\n\n
//	data: {"choices":[...],"usage":{...}}\n\n
//	data: [DONE]\n\n
//
// Only the final usage-bearing JSON event matters; earlier deltas either
// omit usage entirely or carry partial counts. Falls back to parsing the
// whole buffer (non-stream path) when no SSE framing is detected.
func parseOpenAIStreamUsage(body []byte) (UsageTokens, bool) {
	// Fast path: plain JSON body with top-level usage (non-SSE).
	if tokens, ok := ParseOpenAIUsage(body); ok {
		return tokens, true
	}
	var (
		last UsageTokens
		got  bool
	)
	for _, line := range bytes.Split(body, []byte{'\n'}) {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(trimmed[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if tokens, ok := ParseOpenAIUsage(payload); ok {
			last = tokens
			got = true
		}
	}
	return last, got
}

// truncateOpenAIFailureBody clips the failure payload so usage.Record.Fail.Body
// stays bounded. 4 KiB is more than enough for provider error envelopes.
func truncateOpenAIFailureBody(payload []byte) string {
	const max = 4 * 1024
	if len(payload) <= max {
		return string(payload)
	}
	return string(payload[:max])
}
