package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const (
	codexDefaultBaseURL       = "https://api.openai.com"
	codexChatCompletionsPath  = "/v1/chat/completions"
	codexOAuthTokenURL        = "https://auth.openai.com/oauth/token"
	codexOAuthClientID        = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexMetadataAPIKey       = "api_key"
	CodexMetadataAccessToken  = "access_token"
	codexMetadataRefreshToken = "refresh_token"
	codexMetadataTokenData    = "token_data"
	codexMetadataExpiresAt    = "expires_at"
	codexMetadataExpired      = "expired"
	codexMetadataLastRefresh  = "last_refresh"
	codexMetadataIDToken      = "id_token"
)

// CodexExecutor implements cliproxyauth.ProviderExecutor for Codex/OpenAI OAuth
// credentials. It avoids SDK internal OAuth packages so CPA-Gateway keeps HTTP
// lifecycle ownership while using SDK auth records as data only.
type CodexExecutor struct {
	baseURL     string
	accessToken string
	client      *http.Client
}

type codexRefreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// NewCodexExecutor creates a CodexExecutor from the provided config.
func NewCodexExecutor(cfg ProviderConfig, timeoutSeconds int) (*CodexExecutor, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = codexDefaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid codex base_url")
	}

	timeout := defaultTimeout
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	return &CodexExecutor{
		baseURL:     baseURL,
		accessToken: strings.TrimSpace(cfg.APIKey),
		client:      &http.Client{Timeout: timeout},
	}, nil
}

// BaseURL exposes the configured upstream base URL.
func (e *CodexExecutor) BaseURL() string { return e.baseURL }

// AccessToken exposes the config-level access token used to seed auth records.
func (e *CodexExecutor) AccessToken() string { return e.accessToken }

func (e *CodexExecutor) Identifier() string { return providerCodex }

func (e *CodexExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	accessToken, baseURL := e.resolveCredentials(auth)
	resp, err := e.doChatCompletionsRequest(ctx, req, opts, accessToken, baseURL)
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

func (e *CodexExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	accessToken, baseURL := e.resolveCredentials(auth)
	streamOpts := opts
	streamOpts.Stream = true
	resp, err := e.doChatCompletionsRequest(ctx, req, streamOpts, accessToken, baseURL)
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

func (e *CodexExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
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
	if tokenResp.RefreshToken == "" {
		tokenResp.RefreshToken = refreshToken
	}
	if tokenResp.AccessToken != "" {
		clone.Metadata[CodexMetadataAccessToken] = tokenResp.AccessToken
	}
	clone.Metadata[codexMetadataRefreshToken] = tokenResp.RefreshToken
	if tokenResp.IDToken != "" {
		clone.Metadata[codexMetadataIDToken] = tokenResp.IDToken
	}
	if tokenResp.ExpiresIn > 0 {
		expiresAt := now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
		clone.Metadata[codexMetadataExpiresAt] = expiresAt
		clone.Metadata[codexMetadataExpired] = expiresAt
	}
	clone.Metadata[codexMetadataLastRefresh] = now.Format(time.RFC3339)
	clone.Metadata[codexMetadataTokenData] = e.updatedTokenData(clone.Metadata[codexMetadataTokenData], tokenResp, refreshToken, now)
	clone.Status = cliproxyauth.StatusActive
	clone.UpdatedAt = now
	clone.LastRefreshedAt = now
	return clone, nil
}

func (e *CodexExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
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

func (e *CodexExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("http request is required")
	}
	accessToken, _ := e.resolveCredentials(auth)
	if accessToken == "" {
		return nil, fmt.Errorf("codex access token is required")
	}
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	return e.client.Do(req)
}

func (e *CodexExecutor) doChatCompletionsRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, accessToken string, baseURL string) (*http.Response, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("codex access token is required")
	}
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
	httpReq.Header.Set("Authorization", "Bearer "+accessToken)
	return e.client.Do(httpReq)
}

func (e *CodexExecutor) resolveCredentials(auth *cliproxyauth.Auth) (accessToken, baseURL string) {
	accessToken = e.accessToken
	baseURL = e.baseURL
	if strings.TrimSpace(baseURL) == "" {
		baseURL = codexDefaultBaseURL
	}
	if auth == nil {
		return strings.TrimSpace(accessToken), baseURL
	}
	if u, ok := auth.Attributes["base_url"]; ok && strings.TrimSpace(u) != "" {
		baseURL = strings.TrimRight(strings.TrimSpace(u), "/")
	} else if u, ok := auth.Attributes["base-url"]; ok && strings.TrimSpace(u) != "" {
		baseURL = strings.TrimRight(strings.TrimSpace(u), "/")
	}

	if value := stringFromMap(auth.Metadata, CodexMetadataAccessToken); value != "" {
		return value, baseURL
	}
	if value := nestedString(auth.Metadata, codexMetadataTokenData, CodexMetadataAccessToken); value != "" {
		return value, baseURL
	}
	if value := stringFromMap(auth.Metadata, codexMetadataAPIKey); value != "" {
		return value, baseURL
	}
	if value := nestedString(auth.Metadata, codexMetadataTokenData, codexMetadataAPIKey); value != "" {
		return value, baseURL
	}
	if value := e.stringFieldFromStorage(auth, "AccessToken", CodexMetadataAccessToken); value != "" {
		return value, baseURL
	}
	if value := e.stringFieldFromStorage(auth, "APIKey", codexMetadataAPIKey); value != "" {
		return value, baseURL
	}
	return strings.TrimSpace(accessToken), baseURL
}

func (e *CodexExecutor) resolveRefreshToken(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if value := stringFromMap(auth.Metadata, codexMetadataRefreshToken); value != "" {
		return value
	}
	if value := nestedString(auth.Metadata, codexMetadataTokenData, codexMetadataRefreshToken); value != "" {
		return value
	}
	return e.stringFieldFromStorage(auth, "RefreshToken", codexMetadataRefreshToken)
}

func (e *CodexExecutor) stringFieldFromStorage(auth *cliproxyauth.Auth, fieldName string, jsonName string) string {
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

func (e *CodexExecutor) refreshOAuthToken(ctx context.Context, refreshToken string) (*codexRefreshResponse, error) {
	form := url.Values{
		"client_id":     {codexOAuthClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {"openid profile email"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("creating codex refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex token refresh request failed: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading codex refresh response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("codex token refresh failed with upstream status %d", resp.StatusCode)
	}
	var tokenResp codexRefreshResponse
	if err := json.Unmarshal(payload, &tokenResp); err != nil {
		return nil, fmt.Errorf("parsing codex refresh response: %w", err)
	}
	return &tokenResp, nil
}

func (e *CodexExecutor) updatedTokenData(raw any, tokenResp *codexRefreshResponse, previousRefreshToken string, now time.Time) map[string]any {
	tokenData := map[string]any{}
	switch v := raw.(type) {
	case map[string]any:
		maps.Copy(tokenData, v)
	case map[string]string:
		for key, value := range v {
			tokenData[key] = value
		}
	case json.RawMessage:
		_ = json.Unmarshal(v, &tokenData)
	case []byte:
		_ = json.Unmarshal(v, &tokenData)
	case string:
		_ = json.Unmarshal([]byte(strings.TrimSpace(v)), &tokenData)
	default:
		if raw != nil {
			data, err := json.Marshal(raw)
			if err == nil {
				_ = json.Unmarshal(data, &tokenData)
			}
		}
	}
	if tokenResp.AccessToken != "" {
		tokenData[CodexMetadataAccessToken] = tokenResp.AccessToken
	}
	refreshToken := tokenResp.RefreshToken
	if refreshToken == "" {
		refreshToken = previousRefreshToken
	}
	if refreshToken != "" {
		tokenData[codexMetadataRefreshToken] = refreshToken
	}
	if tokenResp.IDToken != "" {
		tokenData[codexMetadataIDToken] = tokenResp.IDToken
	}
	if tokenResp.ExpiresIn > 0 {
		tokenData[codexMetadataExpired] = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	tokenData[codexMetadataLastRefresh] = now.Format(time.RFC3339)
	return tokenData
}

func (e *CodexExecutor) chatCompletionsEndpoint(query url.Values, baseURL string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = codexDefaultBaseURL
	}
	var endpoint string
	switch {
	case strings.HasSuffix(base, codexChatCompletionsPath):
		endpoint = base
	case strings.HasSuffix(base, "/v1"):
		endpoint = base + "/chat/completions"
	default:
		endpoint = base + codexChatCompletionsPath
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid codex base_url")
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
