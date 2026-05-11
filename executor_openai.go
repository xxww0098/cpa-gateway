package main

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
)

// openAICompatibleExecutor implements cliproxyauth.ProviderExecutor for any
// OpenAI-compatible API. It supports auth-aware credential resolution from
// provider pool auth records in addition to its own config-level credentials.
type openAICompatibleExecutor struct {
	provider string
	baseURL  string
	apiKey   string
	client   *http.Client
}

// newOpenAICompatibleExecutor creates a new executor from SDK provider config.
func newOpenAICompatibleExecutor(cfg SDKProviderConfig, timeoutSeconds int) (*openAICompatibleExecutor, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	apiKey := strings.TrimSpace(cfg.APIKey)
	if !cfg.Enabled || baseURL == "" || apiKey == "" {
		return nil, fmt.Errorf("sdk base_url and api_key are required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid sdk base_url")
	}

	timeout := proxyDefaultTimeout
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	return &openAICompatibleExecutor{
		provider: proxyProviderOpenAI,
		baseURL:  baseURL,
		apiKey:   apiKey,
		client:   &http.Client{Timeout: timeout},
	}, nil
}

// Identifier returns the provider key handled by this executor.
func (e *openAICompatibleExecutor) Identifier() string {
	return e.provider
}

// Execute handles non-streaming execution to the OpenAI-compatible upstream.
func (e *openAICompatibleExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	apiKey, baseURL := e.resolveCredentials(auth)
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
	if resp.StatusCode >= http.StatusBadRequest {
		return wrapped, &upstreamStatusError{status: resp.StatusCode, payload: payload}
	}
	return wrapped, nil
}

// ExecuteStream handles streaming execution to the OpenAI-compatible upstream.
func (e *openAICompatibleExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	apiKey, baseURL := e.resolveCredentials(auth)
	resp, err := e.doChatCompletionsRequest(ctx, req, opts, apiKey, baseURL)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		defer resp.Body.Close()
		payload, _ := io.ReadAll(resp.Body)
		return nil, &upstreamStatusError{status: resp.StatusCode, payload: payload}
	}

	chunks := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(chunks)
		defer resp.Body.Close()

		buf := make([]byte, 32*1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				payload := make([]byte, n)
				copy(payload, buf[:n])
				select {
				case <-ctx.Done():
					chunks <- cliproxyexecutor.StreamChunk{Err: ctx.Err()}
					return
				case chunks <- cliproxyexecutor.StreamChunk{Payload: payload}:
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					chunks <- cliproxyexecutor.StreamChunk{Err: err}
				}
				return
			}
		}
	}()

	return &cliproxyexecutor.StreamResult{Headers: resp.Header.Clone(), Chunks: chunks}, nil
}

// Refresh returns a refreshed active clone for API key credentials (no-op).
func (e *openAICompatibleExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	_ = ctx
	if auth == nil {
		return nil, nil
	}
	clone := auth.Clone()
	clone.Status = cliproxyauth.StatusActive
	clone.UpdatedAt = time.Now().UTC()
	return clone, nil
}

// CountTokens returns an approximate token count for the request payload.
func (e *openAICompatibleExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = auth
	_ = opts
	tokens := approximateTokensFromBytes(len(req.Payload))
	payload, _ := json.Marshal(map[string]any{"total_tokens": tokens})
	return cliproxyexecutor.Response{
		Payload: payload,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// HttpRequest injects credentials into the supplied HTTP request and executes it.
// Uses auth-specific API key when present, falling back to executor config key.
func (e *openAICompatibleExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("http request is required")
	}
	apiKey, _ := e.resolveCredentials(auth)
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return e.client.Do(req)
}

// resolveCredentials resolves the effective API key and base URL for a given auth record.
// Provider pool credentials (from Task 5 CRUD) take precedence over executor config defaults:
//   - API key from auth.Metadata["api_key"]
//   - Base URL from auth.Attributes["base_url"] or auth.Attributes["base-url"]
//
// Never log or return the resolved key in full.
func (e *openAICompatibleExecutor) resolveCredentials(auth *cliproxyauth.Auth) (apiKey, baseURL string) {
	apiKey = e.apiKey
	baseURL = e.baseURL
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

// doChatCompletionsRequest builds and sends an OpenAI-compatible /v1/chat/completions
// HTTP request using the supplied apiKey and baseURL (which may be auth-resolved).
func (e *openAICompatibleExecutor) doChatCompletionsRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, apiKey, baseURL string) (*http.Response, error) {
	endpoint, err := e.chatCompletionsEndpoint(opts.Query, baseURL)
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
	if opts.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else if httpReq.Header.Get("Accept") == "" {
		httpReq.Header.Set("Accept", "application/json")
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	return e.client.Do(httpReq)
}

// chatCompletionsEndpoint builds the full /v1/chat/completions URL from the
// given base URL, appending query parameters when present.
func (e *openAICompatibleExecutor) chatCompletionsEndpoint(query url.Values, baseURL string) (string, error) {
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
