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

func (e *OpenAICompatibleExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
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
