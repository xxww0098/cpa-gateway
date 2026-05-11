package executor

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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
	vertexDefaultLocation         = "us-central1"
	vertexDefaultTokenURI         = "https://oauth2.googleapis.com/token"
	vertexCloudPlatformScope      = "https://www.googleapis.com/auth/cloud-platform"
	vertexMetadataAccessToken     = "access_token"
	vertexMetadataExpiresAt       = "expires_at"
	vertexMetadataExpired         = "expired"
	vertexMetadataLastRefresh     = "last_refresh"
	vertexMetadataServiceAccount  = "service_account"
	vertexMetadataTokenData       = "token_data"
	vertexRefreshSkew             = 2 * time.Minute
	vertexTokenFallbackExpiration = time.Hour

	// VertexMetadataServiceAccount is the exported metadata key holding
	// the raw service_account JSON on a cliproxyauth.Auth record.
	VertexMetadataServiceAccount = vertexMetadataServiceAccount
)

// vertexExecutor implements cliproxyauth.ProviderExecutor for Vertex AI Gemini.
// It keeps OAuth and HTTP lifecycle in CPA-Gateway and treats SDK auth records as data only.
type VertexExecutor struct {
	baseURL            string
	serviceAccountJSON string
	client             *http.Client
}

type vertexServiceAccount struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
	ProjectID   string `json:"project_id"`
}

type vertexTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// BaseURL exposes the configured upstream base URL (empty string means
// derive from default Vertex host).
func (e *VertexExecutor) BaseURL() string { return e.baseURL }

// ServiceAccountJSON exposes the raw JSON service_account string used to
// seed persisted auth records.
func (e *VertexExecutor) ServiceAccountJSON() string { return e.serviceAccountJSON }

func NewVertexExecutor(cfg ProviderConfig, timeoutSeconds int) (*VertexExecutor, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL != "" {
		parsed, err := url.Parse(baseURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("invalid vertex base_url")
		}
	}

	timeout := defaultTimeout
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	return &VertexExecutor{
		baseURL:            baseURL,
		serviceAccountJSON: strings.TrimSpace(cfg.APIKey),
		client:             &http.Client{Timeout: timeout},
	}, nil
}

func (e *VertexExecutor) Identifier() string { return providerVertex }

func (e *VertexExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	accessToken, baseURL, project, location, err := e.credentialsForRequest(ctx, auth)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	resp, err := e.doGenerateContentRequest(ctx, req, opts, accessToken, baseURL, project, location, false)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	wrapped := cliproxyexecutor.Response{
		Payload:  payload,
		Headers:  resp.Header.Clone(),
		Metadata: map[string]any{"status_code": resp.StatusCode},
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return wrapped, &upstreamStatusError{status: resp.StatusCode, payload: payload}
	}
	return wrapped, nil
}

func (e *VertexExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	accessToken, baseURL, project, location, err := e.credentialsForRequest(ctx, auth)
	if err != nil {
		return nil, err
	}
	streamOpts := opts
	streamOpts.Stream = true
	resp, err := e.doGenerateContentRequest(ctx, req, streamOpts, accessToken, baseURL, project, location, true)
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

func (e *VertexExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, nil
	}
	clone := auth.Clone()
	serviceAccount, err := e.resolveServiceAccount(clone)
	if err != nil {
		return nil, err
	}
	tokenResp, err := e.refreshServiceAccountToken(ctx, serviceAccount)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	expiresAt := now.Add(vertexTokenFallbackExpiration)
	if tokenResp.ExpiresIn > 0 {
		expiresAt = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	}

	if clone.Metadata == nil {
		clone.Metadata = make(map[string]any)
	}
	clone.Metadata[vertexMetadataAccessToken] = tokenResp.AccessToken
	clone.Metadata[vertexMetadataExpiresAt] = expiresAt.Format(time.RFC3339)
	clone.Metadata[vertexMetadataExpired] = expiresAt.Format(time.RFC3339)
	clone.Metadata[vertexMetadataLastRefresh] = now.Format(time.RFC3339)
	clone.Metadata[vertexMetadataTokenData] = e.updatedTokenData(clone.Metadata[vertexMetadataTokenData], tokenResp.AccessToken, expiresAt, now)
	clone.Status = cliproxyauth.StatusActive
	clone.UpdatedAt = now
	clone.LastRefreshedAt = now
	return clone, nil
}

func (e *VertexExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = auth
	_ = opts
	tokens := approximateTokensFromBytes(len(req.Payload))
	payload, _ := json.Marshal(map[string]any{"total_tokens": tokens})
	return cliproxyexecutor.Response{Payload: payload, Headers: http.Header{"Content-Type": []string{"application/json"}}}, nil
}

func (e *VertexExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("http request is required")
	}
	accessToken, _, _, _, err := e.credentialsForRequest(ctx, auth)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	return e.client.Do(req)
}

func (e *VertexExecutor) credentialsForRequest(ctx context.Context, auth *cliproxyauth.Auth) (accessToken, baseURL, project, location string, err error) {
	serviceAccount, saErr := e.resolveServiceAccount(auth)
	baseURL, project, location = e.resolveEndpointSettings(auth, serviceAccount)
	if project == "" {
		return "", "", "", "", fmt.Errorf("vertex project is required")
	}
	if accessToken, ok := e.cachedAccessToken(auth, time.Now().UTC()); ok {
		return accessToken, baseURL, project, location, nil
	}
	if saErr != nil {
		return "", "", "", "", saErr
	}
	refreshed, err := e.Refresh(ctx, auth)
	if err != nil {
		return "", "", "", "", err
	}
	if accessToken, ok := e.cachedAccessToken(refreshed, time.Now().UTC()); ok {
		e.copyRefreshedMetadata(auth, refreshed)
		return accessToken, baseURL, project, location, nil
	}
	return "", "", "", "", fmt.Errorf("vertex access token refresh did not return a usable token")
}

func (e *VertexExecutor) copyRefreshedMetadata(dst *cliproxyauth.Auth, src *cliproxyauth.Auth) {
	if dst == nil || src == nil || src.Metadata == nil {
		return
	}
	if dst.Metadata == nil {
		dst.Metadata = make(map[string]any)
	}
	for _, key := range []string{
		vertexMetadataAccessToken,
		vertexMetadataExpiresAt,
		vertexMetadataExpired,
		vertexMetadataLastRefresh,
		vertexMetadataTokenData,
	} {
		if value, ok := src.Metadata[key]; ok {
			dst.Metadata[key] = cloneSafeVertexMetadataValue(value)
		}
	}
}

func (e *VertexExecutor) doGenerateContentRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, accessToken string, baseURL string, project string, location string, stream bool) (*http.Response, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("vertex access token is required")
	}
	model := vertexRequestedModel(req, opts)
	if model == "" {
		return nil, fmt.Errorf("vertex model is required")
	}
	endpoint, err := e.generateContentEndpoint(opts.Query, baseURL, project, location, model, stream)
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
	httpReq.Header.Set("Authorization", "Bearer "+accessToken)
	return e.client.Do(httpReq)
}

func (e *VertexExecutor) generateContentEndpoint(query url.Values, baseURL string, project string, location string, model string, stream bool) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = fmt.Sprintf("https://%s-aiplatform.googleapis.com", location)
	}
	action := "generateContent"
	if stream {
		action = "streamGenerateContent"
	}
	model = strings.TrimPrefix(strings.TrimSpace(model), providerVertex+"/")
	endpoint := base + "/v1/projects/" + url.PathEscape(project) + "/locations/" + url.PathEscape(location) + "/publishers/google/models/" + url.PathEscape(model) + ":" + action
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid vertex base_url")
	}
	values := parsed.Query()
	for key, vals := range query {
		for _, val := range vals {
			values.Add(key, val)
		}
	}
	parsed.RawQuery = values.Encode()
	return parsed.String(), nil
}

func (e *VertexExecutor) resolveServiceAccount(auth *cliproxyauth.Auth) (*vertexServiceAccount, error) {
	raw := ""
	if auth != nil {
		raw = serviceAccountStringFromAny(auth.Metadata[vertexMetadataServiceAccount])
		if raw == "" {
			raw = serviceAccountStringFromAny(nestedAny(auth.Metadata, vertexMetadataTokenData, vertexMetadataServiceAccount))
		}
		if raw == "" {
			raw = serviceAccountStringFromAny(auth.Storage)
		}
	}
	if raw == "" {
		raw = e.serviceAccountJSON
	}
	if raw == "" {
		return nil, fmt.Errorf("vertex service_account is required")
	}

	var sa vertexServiceAccount
	if err := json.Unmarshal([]byte(raw), &sa); err != nil {
		return nil, fmt.Errorf("vertex service_account must be valid JSON")
	}
	if strings.TrimSpace(sa.ClientEmail) == "" {
		return nil, fmt.Errorf("vertex service_account missing client_email")
	}
	if strings.TrimSpace(sa.PrivateKey) == "" {
		return nil, fmt.Errorf("vertex service_account missing private_key")
	}
	if strings.TrimSpace(sa.TokenURI) == "" {
		sa.TokenURI = vertexDefaultTokenURI
	}
	return &sa, nil
}

func (e *VertexExecutor) resolveEndpointSettings(auth *cliproxyauth.Auth, sa *vertexServiceAccount) (baseURL, project, location string) {
	baseURL = strings.TrimRight(strings.TrimSpace(e.baseURL), "/")
	location = vertexDefaultLocation
	if auth != nil {
		if u := strings.TrimSpace(auth.Attributes["base_url"]); u != "" {
			baseURL = strings.TrimRight(u, "/")
		} else if u := strings.TrimSpace(auth.Attributes["base-url"]); u != "" {
			baseURL = strings.TrimRight(u, "/")
		}
		if value := strings.TrimSpace(auth.Attributes["project"]); value != "" {
			project = value
		} else if value := strings.TrimSpace(auth.Attributes["project_id"]); value != "" {
			project = value
		}
		if value := strings.TrimSpace(auth.Attributes["location"]); value != "" {
			location = value
		} else if value := strings.TrimSpace(auth.Attributes["region"]); value != "" {
			location = value
		}
	}
	if project == "" && sa != nil {
		project = strings.TrimSpace(sa.ProjectID)
	}
	if baseURL == "" {
		baseURL = fmt.Sprintf("https://%s-aiplatform.googleapis.com", location)
	}
	return baseURL, project, location
}

func (e *VertexExecutor) cachedAccessToken(auth *cliproxyauth.Auth, now time.Time) (string, bool) {
	if auth == nil {
		return "", false
	}
	if token := stringFromMap(auth.Metadata, vertexMetadataAccessToken); token != "" && e.tokenStillValid(auth.Metadata, now) {
		return token, true
	}
	if token := nestedString(auth.Metadata, vertexMetadataTokenData, vertexMetadataAccessToken); token != "" && e.nestedTokenStillValid(auth.Metadata, now) {
		return token, true
	}
	return "", false
}

func (e *VertexExecutor) tokenStillValid(metadata map[string]any, now time.Time) bool {
	return metadataExpiryValid(metadata, vertexMetadataExpiresAt, now) || metadataExpiryValid(metadata, vertexMetadataExpired, now)
}

func (e *VertexExecutor) nestedTokenStillValid(metadata map[string]any, now time.Time) bool {
	tokenData := mapFromAny(nestedAny(metadata, vertexMetadataTokenData, ""))
	if tokenData == nil {
		return false
	}
	return metadataExpiryValid(tokenData, vertexMetadataExpiresAt, now) || metadataExpiryValid(tokenData, vertexMetadataExpired, now)
}

func (e *VertexExecutor) refreshServiceAccountToken(ctx context.Context, sa *vertexServiceAccount) (*vertexTokenResponse, error) {
	jwtAssertion, err := vertexSignedJWT(sa)
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {jwtAssertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(sa.TokenURI), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("creating vertex token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vertex token refresh request failed")
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading vertex token response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("vertex token refresh failed with upstream status %d", resp.StatusCode)
	}
	var tokenResp vertexTokenResponse
	if err := json.Unmarshal(payload, &tokenResp); err != nil {
		return nil, fmt.Errorf("parsing vertex token response: %w", err)
	}
	if strings.TrimSpace(tokenResp.AccessToken) == "" {
		return nil, fmt.Errorf("vertex token response missing access_token")
	}
	return &tokenResp, nil
}

func (e *VertexExecutor) updatedTokenData(raw any, accessToken string, expiresAt time.Time, now time.Time) map[string]any {
	tokenData := mapFromAny(raw)
	if tokenData == nil {
		tokenData = map[string]any{}
	} else {
		tokenData = maps.Clone(tokenData)
	}
	tokenData[vertexMetadataAccessToken] = accessToken
	tokenData[vertexMetadataExpiresAt] = expiresAt.Format(time.RFC3339)
	tokenData[vertexMetadataExpired] = expiresAt.Format(time.RFC3339)
	tokenData[vertexMetadataLastRefresh] = now.Format(time.RFC3339)
	return tokenData
}

func vertexSignedJWT(sa *vertexServiceAccount) (string, error) {
	privateKey, err := parseVertexPrivateKey(sa.PrivateKey)
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iss":   strings.TrimSpace(sa.ClientEmail),
		"scope": vertexCloudPlatformScope,
		"aud":   strings.TrimSpace(sa.TokenURI),
		"iat":   now,
		"exp":   now + 3600,
	}
	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)
	unsigned := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("signing vertex service_account assertion failed")
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func parseVertexPrivateKey(raw string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(raw)))
	if block == nil {
		return nil, fmt.Errorf("vertex service_account private_key must be PEM encoded")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("vertex service_account private_key parse failed")
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("vertex service_account private_key must be RSA")
	}
	return rsaKey, nil
}

func vertexRequestedModel(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	model := geminiRequestedModel(req, opts)
	return strings.TrimPrefix(strings.TrimSpace(model), providerVertex+"/")
}

func serviceAccountStringFromAny(raw any) string {
	switch v := raw.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case json.RawMessage:
		return strings.TrimSpace(string(v))
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		var wrapper map[string]any
		if err := json.Unmarshal(data, &wrapper); err == nil {
			if nested := serviceAccountStringFromAny(wrapper[vertexMetadataServiceAccount]); nested != "" {
				return nested
			}
		}
		return strings.TrimSpace(string(data))
	}
}

func nestedAny(values map[string]any, parent string, key string) any {
	if values == nil {
		return nil
	}
	raw := values[parent]
	if key == "" {
		return raw
	}
	nested := mapFromAny(raw)
	if nested == nil {
		return nil
	}
	return nested[key]
}

func cloneSafeVertexMetadataValue(raw any) any {
	switch v := raw.(type) {
	case map[string]any:
		clone := make(map[string]any, len(v))
		for key, value := range v {
			if key == vertexMetadataServiceAccount || key == "private_key" || key == "client_email" {
				continue
			}
			clone[key] = value
		}
		return clone
	case map[string]string:
		clone := make(map[string]any, len(v))
		for key, value := range v {
			if key == vertexMetadataServiceAccount || key == "private_key" || key == "client_email" {
				continue
			}
			clone[key] = value
		}
		return clone
	default:
		return raw
	}
}

func mapFromAny(raw any) map[string]any {
	switch v := raw.(type) {
	case nil:
		return nil
	case map[string]any:
		return v
	case map[string]string:
		out := make(map[string]any, len(v))
		for key, value := range v {
			out[key] = value
		}
		return out
	case json.RawMessage:
		return mapFromJSONBytes(v)
	case []byte:
		return mapFromJSONBytes(v)
	case string:
		return mapFromJSONBytes([]byte(strings.TrimSpace(v)))
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return mapFromJSONBytes(data)
	}
}

func mapFromJSONBytes(data []byte) map[string]any {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	var values map[string]any
	if err := json.Unmarshal(data, &values); err != nil {
		return nil
	}
	return values
}

func metadataExpiryValid(values map[string]any, key string, now time.Time) bool {
	value := stringFromMap(values, key)
	if value == "" {
		return false
	}
	expiresAt, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return false
	}
	return expiresAt.After(now.Add(vertexRefreshSkew))
}
