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
	claudeDefaultBaseURL           = "https://api.anthropic.com"
	claudeMessagesPath             = "/v1/messages"
	claudeAnthropicVersion         = "2023-06-01"
	claudeOAuthTokenURL            = "https://api.anthropic.com/v1/oauth/token"
	claudeOAuthClientID            = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeMetadataAPIKey           = "api_key"
	claudeMetadataAccessToken      = "access_token"
	claudeMetadataRefreshToken     = "refresh_token"
	claudeMetadataTokenData        = "token_data"
	claudeMetadataExpiresAt        = "expires_at"
	claudeMetadataExpired          = "expired"
	claudeMetadataLastRefresh      = "last_refresh"
	claudeCredentialSourceAPIKey   = "api_key"
	claudeCredentialSourceOAuthKey = "oauth_token"
)

// ClaudeExecutor implements cliproxyauth.ProviderExecutor for Anthropic Claude.
// It is intentionally independent of SDK internal Claude packages so CPA-Gateway
// keeps ownership of the HTTP lifecycle while using the SDK as a pure library.
type ClaudeExecutor struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

type claudeCredential struct {
	value  string
	source string
}

type claudeRefreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Account      struct {
		EmailAddress string `json:"email_address"`
	} `json:"account"`
}

// NewClaudeExecutor creates a ClaudeExecutor from the provided config.
func NewClaudeExecutor(cfg ProviderConfig, timeoutSeconds int) (*ClaudeExecutor, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = claudeDefaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid claude base_url")
	}

	timeout := defaultTimeout
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	return &ClaudeExecutor{
		baseURL: baseURL,
		apiKey:  strings.TrimSpace(cfg.APIKey),
		client:  &http.Client{Timeout: timeout},
	}, nil
}

// BaseURL exposes the configured upstream base URL.
func (e *ClaudeExecutor) BaseURL() string { return e.baseURL }

func (e *ClaudeExecutor) Identifier() string {
	return providerClaude
}

func (e *ClaudeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	credential, baseURL := e.resolveCredentials(auth)
	startedAt := time.Now().UTC()
	resp, err := e.doMessagesRequest(ctx, req, opts, credential, baseURL)
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
	tokens, ok := ParseClaudeUsage(payload)
	if ok {
		wrapped.Metadata["usage"] = tokens
	}
	e.publishUsage(ctx, auth, req, opts, tokens, ok, failed, resp.StatusCode, payload, startedAt)
	if failed {
		return wrapped, &upstreamStatusError{status: resp.StatusCode, payload: payload}
	}
	return wrapped, nil
}

func (e *ClaudeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	credential, baseURL := e.resolveCredentials(auth)
	streamOpts := opts
	streamOpts.Stream = true
	startedAt := time.Now().UTC()
	resp, err := e.doMessagesRequest(ctx, req, streamOpts, credential, baseURL)
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

func (e *ClaudeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, nil
	}
	clone := auth.Clone()
	refreshToken := e.resolveRefreshToken(clone)
	if refreshToken == "" {
		clone.Status = cliproxyauth.StatusActive
		clone.UpdatedAt = time.Now().UTC()
		return clone, nil
	}

	tokenResp, err := e.refreshOAuthToken(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if clone.Metadata == nil {
		clone.Metadata = make(map[string]any)
	}
	if tokenResp.AccessToken != "" {
		clone.Metadata[claudeMetadataAccessToken] = tokenResp.AccessToken
	}
	if tokenResp.RefreshToken != "" {
		clone.Metadata[claudeMetadataRefreshToken] = tokenResp.RefreshToken
	} else {
		clone.Metadata[claudeMetadataRefreshToken] = refreshToken
	}
	if tokenResp.Account.EmailAddress != "" {
		clone.Metadata["email"] = tokenResp.Account.EmailAddress
	}
	if tokenResp.ExpiresIn > 0 {
		expiresAt := now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
		clone.Metadata[claudeMetadataExpiresAt] = expiresAt
		clone.Metadata[claudeMetadataExpired] = expiresAt
	}
	clone.Metadata[claudeMetadataLastRefresh] = now.Format(time.RFC3339)
	clone.Status = cliproxyauth.StatusActive
	clone.UpdatedAt = now
	clone.LastRefreshedAt = now
	return clone, nil
}

func (e *ClaudeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
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

func (e *ClaudeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("http request is required")
	}
	credential, _ := e.resolveCredentials(auth)
	if credential.value == "" {
		return nil, fmt.Errorf("claude credential is required")
	}
	req = req.WithContext(ctx)
	e.injectCredentialHeaders(req.Header, credential)
	return e.client.Do(req)
}

func (e *ClaudeExecutor) doMessagesRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, credential claudeCredential, baseURL string) (*http.Response, error) {
	if credential.value == "" {
		return nil, fmt.Errorf("claude credential is required")
	}
	endpoint, err := e.messagesEndpoint(opts.Query, baseURL)
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
	e.injectCredentialHeaders(httpReq.Header, credential)
	return e.client.Do(httpReq)
}

func (e *ClaudeExecutor) injectCredentialHeaders(headers http.Header, credential claudeCredential) {
	headers.Set("x-api-key", credential.value)
	if headers.Get("anthropic-version") == "" {
		headers.Set("anthropic-version", claudeAnthropicVersion)
	}
}

func (e *ClaudeExecutor) resolveCredentials(auth *cliproxyauth.Auth) (claudeCredential, string) {
	credential := claudeCredential{value: e.apiKey, source: claudeCredentialSourceAPIKey}
	baseURL := e.baseURL
	if strings.TrimSpace(baseURL) == "" {
		baseURL = claudeDefaultBaseURL
	}
	if auth == nil {
		return credential, baseURL
	}
	if u, ok := auth.Attributes["base_url"]; ok && strings.TrimSpace(u) != "" {
		baseURL = strings.TrimRight(strings.TrimSpace(u), "/")
	} else if u, ok := auth.Attributes["base-url"]; ok && strings.TrimSpace(u) != "" {
		baseURL = strings.TrimRight(strings.TrimSpace(u), "/")
	}

	if value := stringFromMap(auth.Metadata, claudeMetadataAPIKey); value != "" {
		return claudeCredential{value: value, source: claudeCredentialSourceAPIKey}, baseURL
	}
	if value := nestedString(auth.Metadata, claudeMetadataTokenData, claudeMetadataAPIKey); value != "" {
		return claudeCredential{value: value, source: claudeCredentialSourceAPIKey}, baseURL
	}
	if value := stringFromMap(auth.Metadata, claudeMetadataAccessToken); value != "" {
		return claudeCredential{value: value, source: claudeCredentialSourceOAuthKey}, baseURL
	}
	if value := nestedString(auth.Metadata, claudeMetadataTokenData, claudeMetadataAccessToken); value != "" {
		return claudeCredential{value: value, source: claudeCredentialSourceOAuthKey}, baseURL
	}
	if value := e.resolveCredentialFromStorage(auth); value != "" {
		return claudeCredential{value: value, source: claudeCredentialSourceOAuthKey}, baseURL
	}
	return credential, baseURL
}

func (e *ClaudeExecutor) resolveRefreshToken(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if value := stringFromMap(auth.Metadata, claudeMetadataRefreshToken); value != "" {
		return value
	}
	if value := nestedString(auth.Metadata, claudeMetadataTokenData, claudeMetadataRefreshToken); value != "" {
		return value
	}
	if value := e.stringFieldFromStorage(auth, "RefreshToken", claudeMetadataRefreshToken); value != "" {
		return value
	}
	return ""
}

func (e *ClaudeExecutor) resolveCredentialFromStorage(auth *cliproxyauth.Auth) string {
	if value := e.stringFieldFromStorage(auth, "APIKey", claudeMetadataAPIKey); value != "" {
		return value
	}
	return e.stringFieldFromStorage(auth, "AccessToken", claudeMetadataAccessToken)
}

func (e *ClaudeExecutor) stringFieldFromStorage(auth *cliproxyauth.Auth, fieldName string, jsonName string) string {
	if auth == nil || auth.Storage == nil {
		return ""
	}
	data, err := json.Marshal(auth.Storage)
	if err != nil {
		return ""
	}
	var values map[string]any
	if err := json.Unmarshal(data, &values); err != nil {
		return ""
	}
	if value := stringFromMap(values, jsonName); value != "" {
		return value
	}
	return stringFromMap(values, fieldName)
}

func (e *ClaudeExecutor) refreshOAuthToken(ctx context.Context, refreshToken string) (*claudeRefreshResponse, error) {
	reqBody := map[string]any{
		"client_id":     claudeOAuthClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling claude refresh request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeOAuthTokenURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("creating claude refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude token refresh request failed: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading claude refresh response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, &upstreamStatusError{status: resp.StatusCode, payload: payload}
	}
	var tokenResp claudeRefreshResponse
	if err := json.Unmarshal(payload, &tokenResp); err != nil {
		return nil, fmt.Errorf("parsing claude refresh response: %w", err)
	}
	return &tokenResp, nil
}

func (e *ClaudeExecutor) messagesEndpoint(query url.Values, baseURL string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = claudeDefaultBaseURL
	}
	var endpoint string
	switch {
	case strings.HasSuffix(base, claudeMessagesPath):
		endpoint = base
	case strings.HasSuffix(base, "/v1"):
		endpoint = base + "/messages"
	default:
		endpoint = base + claudeMessagesPath
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid claude base_url")
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

// claudeRequestedModel resolves the upstream-facing model name for a request.
// It prefers the translated Request.Model and falls back to the
// `requested_model` hint stored by the SDK router on either Request or
// Options metadata.
func claudeRequestedModel(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	if strings.TrimSpace(req.Model) != "" {
		return strings.TrimSpace(req.Model)
	}
	if value := stringFromMap(req.Metadata, cliproxyexecutor.RequestedModelMetadataKey); value != "" {
		return value
	}
	return stringFromMap(opts.Metadata, cliproxyexecutor.RequestedModelMetadataKey)
}

// publishUsage emits a usage.Record to the SDK default manager. Claude
// surfaces usage via the `usage` envelope on non-stream responses and via
// `message_start.message.usage` + `message_delta.usage` on SSE streams;
// ParseClaudeUsage already normalizes both shapes into UsageTokens. When
// parsing fails the record is still published with zero Detail so downstream
// UsagePlugin can fall back to heuristic accounting.
func (e *ClaudeExecutor) publishUsage(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, tokens UsageTokens, parsed bool, failed bool, status int, payload []byte, startedAt time.Time) {
	rec := cliproxyusage.Record{
		Provider:    providerClaude,
		Model:       claudeRequestedModel(req, opts),
		Alias:       cliproxyusage.RequestedModelAliasFromContext(ctx),
		Source:      providerClaude,
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
			Body:       truncateClaudeFailureBody(payload),
		}
	}
	// Propagate the "did we parse a terminal upstream usage envelope?" signal
	// through the ctx the cliproxy manager hands to UsagePlugin.HandleUsage.
	// Claude's message_start / message_delta frames always carry a `usage`
	// envelope on happy-path streams, so `parsed` is typically true — but we
	// still thread the parser's bool through unmodified (Requirement 1.1).
	ctx = WithUsageDetailPresent(ctx, parsed)
	cliproxyusage.PublishRecord(ctx, rec)
}

// publishStreamUsage parses the accumulated SSE body and delegates to
// publishUsage. Claude streams the following frames:
//
//	event: message_start
//	data: {"type":"message_start","message":{"usage":{"input_tokens":N,...}}}
//
//	event: message_delta
//	data: {"type":"message_delta","delta":{...},"usage":{"output_tokens":M,...}}
//
//	event: message_stop
//	data: {"type":"message_stop"}
//
// The last message_delta event carries the final output_tokens total, so we
// scan all `data:` events and keep the last parse that yields any usage
// values. ParseClaudeUsage's per-event merge handles input / output / cache
// fields correctly regardless of which frame was last.
func (e *ClaudeExecutor) publishStreamUsage(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, body []byte, failed bool, status int, startedAt time.Time) {
	tokens, ok := UsageTokens{}, false
	if len(body) > 0 {
		tokens, ok = parseClaudeStreamUsage(body)
	}
	e.publishUsage(ctx, auth, req, opts, tokens, ok, failed, status, nil, startedAt)
}

// parseClaudeStreamUsage walks an Anthropic SSE stream buffer, extracts the
// `data: {json}` payloads, and merges usage across events so the final record
// carries the largest observed input_tokens + output_tokens + cache token
// totals. Non-SSE (plain JSON) bodies are tolerated via a fast path.
func parseClaudeStreamUsage(body []byte) (UsageTokens, bool) {
	// Fast path: plain JSON body with top-level `usage` / `message`.
	if tokens, ok := ParseClaudeUsage(body); ok {
		return tokens, true
	}

	var (
		merged UsageTokens
		got    bool
	)
	for _, line := range bytes.Split(body, []byte{'\n'}) {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(trimmed[len("data:"):])
		if len(payload) == 0 {
			continue
		}
		tokens, ok := ParseClaudeUsage(payload)
		if !ok {
			continue
		}
		if tokens.Input > merged.Input {
			merged.Input = tokens.Input
		}
		if tokens.Output > merged.Output {
			merged.Output = tokens.Output
		}
		if tokens.Reasoning > merged.Reasoning {
			merged.Reasoning = tokens.Reasoning
		}
		if tokens.Cached > merged.Cached {
			merged.Cached = tokens.Cached
		}
		got = true
	}
	return merged, got
}

// truncateClaudeFailureBody clips the failure payload so usage.Record.Fail.Body
// stays bounded. 4 KiB is more than enough for provider error envelopes.
func truncateClaudeFailureBody(payload []byte) string {
	const max = 4 * 1024
	if len(payload) <= max {
		return string(payload)
	}
	return string(payload[:max])
}
