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
	if resp.StatusCode >= http.StatusBadRequest {
		return wrapped, &upstreamStatusError{status: resp.StatusCode, payload: payload}
	}
	return wrapped, nil
}

func (e *ClaudeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	credential, baseURL := e.resolveCredentials(auth)
	streamOpts := opts
	streamOpts.Stream = true
	resp, err := e.doMessagesRequest(ctx, req, streamOpts, credential, baseURL)
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
