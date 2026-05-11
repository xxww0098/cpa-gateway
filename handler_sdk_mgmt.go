package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/gorm"
)

var sdkMgmtProviderEndpoints = map[string]string{
	"openai-compatibility": "openai",
	"claude-api-key":       "claude",
	"gemini-api-key":       "gemini",
	"codex-api-key":        "codex",
	"vertex-api-key":       "vertex",
}

const (
	sdkMgmtOAuthSessionTTL = 10 * time.Minute

	sdkMgmtOAuthProviderGemini    = "gemini"
	sdkMgmtOAuthProviderClaude    = "claude"
	sdkMgmtOAuthProviderAnthropic = "anthropic"
	sdkMgmtOAuthProviderCodex     = "codex"

	sdkMgmtGeminiAuthURL     = "https://accounts.google.com/o/oauth2/auth"
	sdkMgmtGeminiTokenURL    = "https://oauth2.googleapis.com/token"
	sdkMgmtGeminiUserInfoURL = "https://www.googleapis.com/oauth2/v1/userinfo?alt=json"
	sdkMgmtGeminiClientID    = "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com"

	sdkMgmtClaudeAuthURL  = "https://claude.ai/oauth/authorize"
	sdkMgmtClaudeTokenURL = "https://api.anthropic.com/v1/oauth/token"
	sdkMgmtClaudeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	sdkMgmtCodexAuthURL  = "https://auth.openai.com/oauth/authorize"
	sdkMgmtCodexTokenURL = "https://auth.openai.com/oauth/token"
	sdkMgmtCodexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

var sdkMgmtAuthURLProviders = map[string]string{
	"gemini-cli-auth-url": sdkMgmtOAuthProviderGemini,
	"anthropic-auth-url":  sdkMgmtOAuthProviderClaude,
	"codex-auth-url":      sdkMgmtOAuthProviderCodex,
}

type sdkMgmtOAuthSessionConfig struct {
	Provider            string `json:"provider"`
	ProviderAlias       string `json:"provider_alias,omitempty"`
	EndpointKey         string `json:"endpoint_key"`
	State               string `json:"state"`
	RedirectURI         string `json:"redirect_uri"`
	CodeVerifier        string `json:"code_verifier,omitempty"`
	CodeChallengeMethod string `json:"code_challenge_method,omitempty"`
	CreatedAt           string `json:"created_at"`
	ExpiresAt           string `json:"expires_at"`
}

type sdkMgmtOAuthTokenResponse struct {
	AccessToken  string         `json:"access_token"`
	RefreshToken string         `json:"refresh_token"`
	IDToken      string         `json:"id_token"`
	TokenType    string         `json:"token_type"`
	ExpiresIn    int            `json:"expires_in"`
	Scope        string         `json:"scope"`
	Raw          map[string]any `json:"-"`
}

func loadAmpcodeConfig(ctx context.Context) (map[string]any, error) {
	var cfg model.AmpcodeConfig
	err := GlobalDB.WithContext(ctx).First(&cfg, 1).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return make(map[string]any), nil
		}
		return nil, err
	}
	m := make(map[string]any)
	if len(cfg.ConfigData) > 0 {
		if err := json.Unmarshal(cfg.ConfigData, &m); err != nil {
			m = make(map[string]any)
		}
	}
	return m, nil
}

func saveAmpcodeConfig(ctx context.Context, m map[string]any) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	db := GlobalDB.WithContext(ctx)
	result := db.Model(&model.AmpcodeConfig{}).Where("id = 1").Update("config_data", data)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return db.Create(&model.AmpcodeConfig{ID: 1, ConfigData: data}).Error
	}
	return nil
}

var ampcodeKnownKeyPairs = map[string]string{
	"upstream-url":          "upstream_url",
	"upstream-api-key":      "upstream_api_key",
	"upstream-api-keys":     "upstream_api_keys",
	"force-model-mappings":  "force_model_mappings",
	"model-mappings":        "model_mappings",
}

func normalizeAmpcodeInputKeys(m map[string]any) {
	for hyphen, snake := range ampcodeKnownKeyPairs {
		if v, ok := m[snake]; ok {
			if _, exists := m[hyphen]; !exists {
				m[hyphen] = v
			}
			delete(m, snake)
		}
	}
}

func normalizeAmpcodeResponse(m map[string]any) map[string]any {
	if m == nil {
		m = make(map[string]any)
	}
	for hyphen, snake := range ampcodeKnownKeyPairs {
		if v, ok := m[hyphen]; ok {
			if _, exists := m[snake]; !exists {
				m[snake] = v
			}
		} else if v, ok := m[snake]; ok {
			m[hyphen] = v
		}
	}
	return m
}

// RegisterSDKManagementRoutes registers all SDK management route stubs.
// All routes are mounted under the authenticated panel router group
// at /api/panel/admin/sdk-management (via authedPanel.Group).
// Static routes are registered before parameterized /:provider routes
// so that Gin's radix tree matches them first.
func RegisterSDKManagementRoutes(rg *gin.RouterGroup) {
	// ── Static routes (register before /:provider) ──
	rg.GET("/api-key-usage", SDKMgmtAPIKeyUsageHandler)

	rg.GET("/auth-files", SDKMgmtAuthFilesListHandler)
	rg.POST("/auth-files", SDKMgmtAuthFilesCreateHandler)
	rg.PUT("/auth-files", SDKMgmtAuthFilesUpdateHandler)
	rg.DELETE("/auth-files", SDKMgmtAuthFilesDeleteHandler)
	rg.GET("/auth-files/quota", SDKMgmtAuthFilesQuotaHandler)
	rg.GET("/auth-files/models", SDKMgmtAuthFilesModelsHandler)

	rg.GET("/oauth-sessions", SDKMgmtOAuthSessionsHandler)
	rg.GET("/get-auth-status", SDKMgmtOAuthStatusHandler)

	rg.POST("/oauth-callback/:provider", SDKMgmtOAuthCallbackHandler)

	rg.GET("/ampcode", SDKMgmtAmpcodeGetHandler)
	rg.PUT("/ampcode", SDKMgmtAmpcodePutHandler)

	rg.GET("/ampcode/model-mappings", SDKMgmtAmpcodeModelMappingsGetHandler)
	rg.PUT("/ampcode/model-mappings", SDKMgmtAmpcodeModelMappingsPutHandler)
	rg.DELETE("/ampcode/model-mappings", SDKMgmtAmpcodeModelMappingsDeleteHandler)

	rg.GET("/ampcode/upstream-api-keys", SDKMgmtAmpcodeUpstreamAPIKeysGetHandler)
	rg.PUT("/ampcode/upstream-api-keys", SDKMgmtAmpcodeUpstreamAPIKeysPutHandler)
	rg.DELETE("/ampcode/upstream-api-keys", SDKMgmtAmpcodeUpstreamAPIKeysDeleteHandler)

	rg.PUT("/ampcode/upstream-url", SDKMgmtAmpcodeUpstreamURLPutHandler)
	rg.DELETE("/ampcode/upstream-url", SDKMgmtAmpcodeUpstreamURLDeleteHandler)

	rg.PUT("/ampcode/upstream-api-key", SDKMgmtAmpcodeUpstreamAPIKeyPutHandler)
	rg.DELETE("/ampcode/upstream-api-key", SDKMgmtAmpcodeUpstreamAPIKeyDeleteHandler)

	// ── SDK Config ──
	rg.GET("/config", SDKMgmtConfigGetHandler)
	rg.PUT("/config", SDKMgmtConfigPutHandler)

	// ── Convenience config key endpoints ──
	rg.GET("/debug", sdkMgmtConfigGetHandlerFn("debug"))
	rg.PUT("/debug", sdkMgmtConfigSetHandlerFn("debug"))
	rg.GET("/routing/strategy", sdkMgmtConfigGetRoutingStrategyFn())
	rg.PUT("/routing/strategy", sdkMgmtConfigSetHandlerFn("routing-strategy"))
	rg.GET("/force-model-prefix", sdkMgmtConfigGetForceModelPrefixFn())
	rg.PUT("/force-model-prefix", sdkMgmtConfigSetHandlerFn("force-model-prefix"))
	rg.GET("/logs-max-total-size-mb", sdkMgmtConfigGetLogsMaxSizeFn())
	rg.PUT("/logs-max-total-size-mb", sdkMgmtConfigSetHandlerFn("logs-max-total-size-mb"))
	rg.GET("/request-retry", sdkMgmtConfigGetHandlerFn("request-retry"))
	rg.PUT("/request-retry", sdkMgmtConfigSetHandlerFn("request-retry"))
	rg.GET("/max-retry-interval", sdkMgmtConfigGetHandlerFn("max-retry-interval"))
	rg.PUT("/max-retry-interval", sdkMgmtConfigSetHandlerFn("max-retry-interval"))
	rg.PUT("/proxy-url", sdkMgmtConfigSetHandlerFn("proxy-url"))
	rg.DELETE("/proxy-url", sdkMgmtConfigDeleteHandlerFn("proxy-url"))
	rg.GET("/request-log", sdkMgmtConfigGetHandlerFn("request-log"))
	rg.PUT("/request-log", sdkMgmtConfigSetHandlerFn("request-log"))
	rg.GET("/logging-to-file", sdkMgmtConfigGetHandlerFn("logging-to-file"))
	rg.PUT("/logging-to-file", sdkMgmtConfigSetHandlerFn("logging-to-file"))
	rg.GET("/ws-auth", sdkMgmtConfigGetHandlerFn("ws-auth"))
	rg.PUT("/ws-auth", sdkMgmtConfigSetHandlerFn("ws-auth"))
	rg.GET("/usage-statistics-enabled", sdkMgmtConfigGetHandlerFn("usage-statistics-enabled"))
	rg.PUT("/usage-statistics-enabled", sdkMgmtConfigSetHandlerFn("usage-statistics-enabled"))

	// ── Logs ──
	rg.GET("/logs", SDKMgmtLogsHandler)
	rg.DELETE("/logs", SDKMgmtLogsDeleteHandler)
	rg.GET("/request-error-logs", SDKMgmtRequestErrorLogsHandler)
	rg.DELETE("/request-error-logs", SDKMgmtRequestErrorLogsDeleteHandler)

	// ── Model Definitions ──
	rg.GET("/model-definitions/:channel", SDKMgmtModelDefinitionsHandler)

	// ── Parameterized provider routes (static routes registered above) ──
	rg.GET("/:provider", SDKMgmtProviderGetHandler)
	rg.POST("/:provider", SDKMgmtProviderPostHandler) // also handles :provider-auth-url
	rg.PUT("/:provider", SDKMgmtProviderPutHandler)
	rg.DELETE("/:provider", SDKMgmtProviderDeleteHandler)
}

// ── Provider API Key Pool Handlers ──

func SDKMgmtProviderGetHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	endpoint, provider, ok := sdkMgmtProviderFromRequest(c)
	if !ok {
		return
	}
	auths, ok := sdkMgmtProviderAuths(c, provider)
	if !ok {
		return
	}
	Success(c, gin.H{endpoint: sdkMgmtSerializeAuths(auths)})
}

func SDKMgmtProviderPostHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	provider := c.Param("provider")
	// POST /:provider with -auth-url suffix → OAuth URL generation (single-route
	// dispatch avoids Gin parameter conflict between /:provider and /:provider-auth-url).
	if strings.HasSuffix(provider, "-auth-url") {
		sdkMgmtOAuthAuthURLHandler(c, provider)
		return
	}
	_, sdkProvider, ok := sdkMgmtProviderFromRequest(c)
	if !ok {
		return
	}
	if !sdkMgmtEnsureManager(c) {
		return
	}
	items, ok := sdkMgmtParseProviderPayload(c)
	if !ok {
		return
	}
	created := make([]gin.H, 0, len(items))
	for _, item := range items {
		if !sdkMgmtHasRawAPIKey(item) {
			continue
		}
		auth := sdkMgmtAuthFromPayload(sdkProvider, item, nil)
		registered, err := authManager.Register(c.Request.Context(), auth)
		if err != nil {
			Error(c, http.StatusInternalServerError, 5001, "failed to register provider API key")
			return
		}
		created = append(created, sdkMgmtSerializeAuth(registered, len(created)))
	}
	if len(created) == 0 {
		Error(c, http.StatusBadRequest, 4001, "api key is required")
		return
	}
	Success(c, gin.H{"items": created, "message": "created"})
}

func SDKMgmtProviderPutHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	_, sdkProvider, ok := sdkMgmtProviderFromRequest(c)
	if !ok {
		return
	}
	if !sdkMgmtEnsureManager(c) {
		return
	}
	items, ok := sdkMgmtParseProviderPayload(c)
	if !ok {
		return
	}
	updated := make([]gin.H, 0, len(items))
	for index, item := range items {
		existing, found := sdkMgmtFindProviderAuth(sdkProvider, item, index)
		if !found {
			continue
		}
		next := sdkMgmtAuthFromPayload(sdkProvider, item, existing)
		saved, err := authManager.Update(c.Request.Context(), next)
		if err != nil {
			Error(c, http.StatusInternalServerError, 5002, "failed to update provider API key")
			return
		}
		updated = append(updated, sdkMgmtSerializeAuth(saved, index))
	}
	if len(updated) == 0 {
		Error(c, http.StatusNotFound, 4041, "provider API key not found")
		return
	}
	Success(c, gin.H{"items": updated, "message": "updated"})
}

func SDKMgmtProviderDeleteHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	_, sdkProvider, ok := sdkMgmtProviderFromRequest(c)
	if !ok {
		return
	}
	if !sdkMgmtEnsureManager(c) {
		return
	}
	items, isArray, ok := sdkMgmtParseProviderDeletePayload(c)
	if !ok {
		return
	}
	deleted, tombstoned := sdkMgmtDeleteProviderAuths(c.Request.Context(), sdkProvider, items, isArray)
	if len(deleted) == 0 && len(tombstoned) == 0 {
		Error(c, http.StatusNotFound, 4042, "provider API key not found")
		return
	}
	Success(c, gin.H{
		"deleted":             deleted,
		"tombstoned":          tombstoned,
		"in_memory_filtered":  tombstoned,
		"direct_remove":       false,
		"message":             "deleted",
		"manager_remove_note": "SDK manager has no public remove method; tombstoned credentials are omitted from GET and usage until reload",
	})
}

// ── API Key Usage ──

func SDKMgmtAPIKeyUsageHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	if !sdkMgmtEnsureManager(c) {
		return
	}
	usage := gin.H{}
	for _, auth := range authManager.List() {
		if auth == nil || sdkMgmtAuthDeleted(auth) {
			continue
		}
		endpoint := sdkMgmtEndpointForProvider(auth.Provider)
		if endpoint == "" {
			continue
		}
		bucket := sdkMgmtUsageBucket(usage, endpoint)
		providerBucket := sdkMgmtUsageBucket(usage, auth.Provider)
		baseURL := sdkMgmtAttr(auth, "base_url", "base-url")
		maskedKey := sdkMgmtMaskSecret(sdkMgmtAuthAPIKey(auth))
		entryKey := baseURL + "|" + maskedKey
		entry := gin.H{
			"success":         auth.Success,
			"failed":          auth.Failed,
			"recent_requests": sdkMgmtRecentRequests(auth),
		}
		bucket[entryKey] = entry
		providerBucket[entryKey] = entry
	}
	Success(c, usage)
}

func sdkMgmtProviderFromRequest(c *gin.Context) (string, string, bool) {
	endpoint := strings.TrimSpace(c.Param("provider"))
	provider, ok := sdkMgmtProviderEndpoints[endpoint]
	if !ok {
		Error(c, http.StatusNotFound, 4040, "unknown provider")
		return "", "", false
	}
	return endpoint, provider, true
}

func sdkMgmtAuthURLProviderSupported(c *gin.Context, authURLProvider string) bool {
	if _, ok := sdkMgmtAuthURLProviders[strings.TrimSpace(authURLProvider)]; !ok {
		Error(c, http.StatusNotFound, 4040, "unknown auth-url provider")
		return false
	}
	return true
}

func sdkMgmtEndpointForProvider(provider string) string {
	for endpoint, sdkProvider := range sdkMgmtProviderEndpoints {
		if sdkProvider == provider {
			return endpoint
		}
	}
	return ""
}

func sdkMgmtUsageBucket(usage gin.H, key string) gin.H {
	bucket, _ := usage[key].(gin.H)
	if bucket == nil {
		bucket = gin.H{}
		usage[key] = bucket
	}
	return bucket
}

func sdkMgmtEnsureManager(c *gin.Context) bool {
	if authManager == nil {
		Error(c, http.StatusServiceUnavailable, 5031, "SDK auth manager is not initialized")
		return false
	}
	return true
}

func sdkMgmtProviderAuths(c *gin.Context, provider string) ([]*cliproxyauth.Auth, bool) {
	if !sdkMgmtEnsureManager(c) {
		return nil, false
	}
	auths := make([]*cliproxyauth.Auth, 0)
	for _, auth := range authManager.List() {
		if auth != nil && auth.Provider == provider && !sdkMgmtAuthDeleted(auth) {
			auths = append(auths, auth)
		}
	}
	sort.SliceStable(auths, func(i, j int) bool {
		left := auths[i]
		right := auths[j]
		if left.CreatedAt.Equal(right.CreatedAt) {
			return left.ID < right.ID
		}
		return left.CreatedAt.Before(right.CreatedAt)
	})
	return auths, true
}

func sdkMgmtSerializeAuths(auths []*cliproxyauth.Auth) []gin.H {
	items := make([]gin.H, 0, len(auths))
	for index, auth := range auths {
		items = append(items, sdkMgmtSerializeAuth(auth, index))
	}
	return items
}

func sdkMgmtSerializeAuth(auth *cliproxyauth.Auth, index int) gin.H {
	if auth == nil {
		return gin.H{}
	}
	return gin.H{
		"id":                       auth.ID,
		"auth_id":                  auth.ID,
		"index":                    index,
		"name":                     sdkMgmtAuthName(auth, index),
		"api-key":                  sdkMgmtMaskSecret(sdkMgmtAuthAPIKey(auth)),
		"base-url":                 sdkMgmtAttr(auth, "base_url", "base-url"),
		"models-url":               sdkMgmtAttr(auth, "models_url", "models-url"),
		"proxy-url":                sdkMgmtProxyURL(auth),
		"prefix":                   auth.Prefix,
		"priority":                 sdkMgmtAttrNumber(auth, "priority"),
		"disabled":                 auth.Disabled || auth.Status == cliproxyauth.StatusDisabled,
		"headers":                  sdkMgmtMetadata(auth, "headers"),
		"models":                   sdkMgmtMetadata(auth, "models"),
		"excluded-models":          sdkMgmtMetadata(auth, "excluded_models"),
		"websockets":               sdkMgmtAttrBool(auth, "websockets"),
		"experimental-cch-signing": sdkMgmtAttrBool(auth, "experimental_cch_signing"),
		"status":                   string(auth.Status),
		"unavailable":              auth.Unavailable,
		"success":                  auth.Success,
		"failed":                   auth.Failed,
		"created_at":               auth.CreatedAt,
		"updated_at":               auth.UpdatedAt,
	}
}

func sdkMgmtParseProviderPayload(c *gin.Context) ([]map[string]any, bool) {
	var raw any
	if err := c.ShouldBindJSON(&raw); err != nil {
		Error(c, http.StatusBadRequest, 4000, "invalid JSON payload")
		return nil, false
	}
	items := sdkMgmtPayloadItems(raw)
	if len(items) == 0 {
		Error(c, http.StatusBadRequest, 4000, "provider payload is required")
		return nil, false
	}
	return items, true
}

func sdkMgmtPayloadItems(raw any) []map[string]any {
	switch value := raw.(type) {
	case []any:
		return sdkMgmtExpandProviderRecords(sdkMgmtRecordsFromArray(value))
	case map[string]any:
		for _, key := range []string{"value", "keys", "items"} {
			if array, ok := value[key].([]any); ok {
				return sdkMgmtExpandProviderRecords(sdkMgmtRecordsFromArray(array))
			}
		}
		return sdkMgmtExpandProviderRecords([]map[string]any{value})
	default:
		return nil
	}
}

func sdkMgmtParseProviderDeletePayload(c *gin.Context) ([]map[string]any, bool, bool) {
	var raw any
	if err := c.ShouldBindJSON(&raw); err != nil {
		Error(c, http.StatusBadRequest, 4000, "invalid JSON payload")
		return nil, false, false
	}
	switch value := raw.(type) {
	case []any:
		return sdkMgmtExpandProviderRecords(sdkMgmtRecordsFromArray(value)), true, true
	case map[string]any:
		for _, key := range []string{"value", "keys", "items"} {
			if array, ok := value[key].([]any); ok {
				return sdkMgmtExpandProviderRecords(sdkMgmtRecordsFromArray(array)), true, true
			}
		}
		return sdkMgmtExpandProviderRecords([]map[string]any{value}), false, true
	default:
		Error(c, http.StatusBadRequest, 4000, "provider payload is required")
		return nil, false, false
	}
}

func sdkMgmtRecordsFromArray(values []any) []map[string]any {
	items := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if item, ok := value.(map[string]any); ok {
			items = append(items, item)
		}
	}
	return items
}

func sdkMgmtExpandProviderRecords(items []map[string]any) []map[string]any {
	expanded := make([]map[string]any, 0, len(items))
	for _, item := range items {
		entries, ok := item["api-key-entries"].([]any)
		if !ok || len(entries) == 0 {
			expanded = append(expanded, item)
			continue
		}
		for _, entryValue := range entries {
			entry, ok := entryValue.(map[string]any)
			if !ok {
				continue
			}
			merged := make(map[string]any, len(item)+len(entry))
			for key, value := range item {
				if key != "api-key-entries" {
					merged[key] = value
				}
			}
			maps.Copy(merged, entry)
			expanded = append(expanded, merged)
		}
	}
	return expanded
}

func sdkMgmtAuthFromPayload(provider string, item map[string]any, existing *cliproxyauth.Auth) *cliproxyauth.Auth {
	now := time.Now().UTC()
	auth := &cliproxyauth.Auth{
		ID:         uuid.NewString(),
		Provider:   provider,
		Status:     cliproxyauth.StatusActive,
		CreatedAt:  now,
		UpdatedAt:  now,
		Attributes: map[string]string{},
		Metadata:   map[string]any{},
	}
	if existing != nil {
		auth = existing.Clone()
		if auth.Attributes == nil {
			auth.Attributes = map[string]string{}
		}
		if auth.Metadata == nil {
			auth.Metadata = map[string]any{}
		}
		auth.UpdatedAt = now
		if auth.CreatedAt.IsZero() {
			auth.CreatedAt = now
		}
	}
	if value := sdkMgmtString(item, "id", "auth_id", "_id"); value != "" && existing == nil {
		auth.ID = value
	}
	if name := sdkMgmtString(item, "name", "label"); name != "" {
		auth.Label = name
	}
	if prefix := sdkMgmtString(item, "prefix"); prefix != "" || sdkMgmtHasKey(item, "prefix") {
		auth.Prefix = prefix
	}
	if proxyURL := sdkMgmtString(item, "proxy-url", "proxy_url"); proxyURL != "" || sdkMgmtHasAnyKey(item, "proxy-url", "proxy_url") {
		auth.ProxyURL = proxyURL
		auth.Attributes["proxy_url"] = proxyURL
	}
	for _, field := range []struct{ jsonKey, attrKey string }{
		{"base-url", "base_url"},
		{"models-url", "models_url"},
		{"priority", "priority"},
		{"websockets", "websockets"},
		{"experimental-cch-signing", "experimental_cch_signing"},
	} {
		if value, ok := sdkMgmtPayloadString(item, field.jsonKey); ok {
			auth.Attributes[field.attrKey] = value
		}
	}
	if rawKey := sdkMgmtString(item, "api-key", "api_key", "apiKey"); rawKey != "" && !sdkMgmtLooksMasked(rawKey) {
		auth.Metadata["api_key"] = rawKey
	}
	for _, field := range []struct{ jsonKey, metadataKey string }{
		{"headers", "headers"},
		{"models", "models"},
		{"excluded-models", "excluded_models"},
	} {
		if value, ok := item[field.jsonKey]; ok {
			auth.Metadata[field.metadataKey] = value
		}
	}
	if disabled, ok := sdkMgmtPayloadBool(item, "disabled"); ok {
		auth.Disabled = disabled
		if disabled {
			auth.Status = cliproxyauth.StatusDisabled
		} else if auth.Status == cliproxyauth.StatusDisabled {
			auth.Status = cliproxyauth.StatusActive
		}
	}
	return auth
}

func sdkMgmtFindProviderAuth(provider string, item map[string]any, index int) (*cliproxyauth.Auth, bool) {
	if authManager == nil {
		return nil, false
	}
	if id := sdkMgmtString(item, "id", "auth_id"); id != "" {
		if auth, ok := authManager.GetByID(id); ok && auth.Provider == provider && !sdkMgmtAuthDeleted(auth) {
			return auth, true
		}
	}
	auths := authManager.List()
	sort.SliceStable(auths, func(i, j int) bool { return auths[i].CreatedAt.Before(auths[j].CreatedAt) })
	name := sdkMgmtString(item, "name", "label")
	for _, auth := range auths {
		if auth == nil || auth.Provider != provider || sdkMgmtAuthDeleted(auth) {
			continue
		}
		if name != "" && (auth.Label == name || auth.ID == name) {
			return auth, true
		}
	}
	providerIndex := 0
	for _, auth := range auths {
		if auth == nil || auth.Provider != provider || sdkMgmtAuthDeleted(auth) {
			continue
		}
		if providerIndex == index {
			return auth, true
		}
		providerIndex++
	}
	return nil, false
}

func sdkMgmtDeleteProviderAuths(ctx context.Context, provider string, items []map[string]any, desiredStateArray bool) ([]string, []string) {
	deleteIDs := map[string]bool{}
	if desiredStateArray {
		remaining := map[string]bool{}
		for index, item := range items {
			if auth, ok := sdkMgmtFindProviderAuth(provider, item, index); ok {
				remaining[auth.ID] = true
			}
		}
		for _, auth := range authManager.List() {
			if auth != nil && auth.Provider == provider && !sdkMgmtAuthDeleted(auth) && !remaining[auth.ID] {
				deleteIDs[auth.ID] = true
			}
		}
	} else {
		for index, item := range items {
			if auth, ok := sdkMgmtFindProviderAuth(provider, item, index); ok {
				deleteIDs[auth.ID] = true
			}
		}
	}
	deleted := make([]string, 0, len(deleteIDs))
	disabled := make([]string, 0)
	for id := range deleteIDs {
		if auth, ok := authManager.GetByID(id); ok {
			if auth.Attributes == nil {
				auth.Attributes = map[string]string{}
			}
			if auth.Metadata == nil {
				auth.Metadata = map[string]any{}
			}
			auth.Disabled = true
			auth.Status = cliproxyauth.StatusDisabled
			auth.Attributes["deleted"] = "true"
			auth.Metadata["deleted"] = true
			auth.Metadata["deleted_at"] = time.Now().UTC().Format(time.RFC3339)
			auth.UpdatedAt = time.Now().UTC()
			_, _ = authManager.Update(ctx, auth)
			disabled = append(disabled, id)
		}
		if GlobalStore != nil {
			_ = GlobalStore.Delete(ctx, id)
		}
		deleted = append(deleted, id)
	}
	sort.Strings(deleted)
	sort.Strings(disabled)
	return deleted, disabled
}

func sdkMgmtHasRawAPIKey(item map[string]any) bool {
	value := sdkMgmtString(item, "api-key", "api_key", "apiKey")
	return value != "" && !sdkMgmtLooksMasked(value)
}

func sdkMgmtAuthDeleted(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	if strings.EqualFold(sdkMgmtAttr(auth, "deleted"), "true") {
		return true
	}
	if auth.Metadata == nil {
		return false
	}
	switch value := auth.Metadata["deleted"].(type) {
	case bool:
		return value
	case string:
		return strings.EqualFold(value, "true")
	default:
		return false
	}
}

func sdkMgmtAuthAPIKey(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	return fmt.Sprint(auth.Metadata["api_key"])
}

func sdkMgmtAuthName(auth *cliproxyauth.Auth, index int) string {
	if auth.Label != "" {
		return auth.Label
	}
	return fmt.Sprintf("Channel-%d", index+1)
}

func sdkMgmtProxyURL(auth *cliproxyauth.Auth) string {
	if auth.ProxyURL != "" {
		return auth.ProxyURL
	}
	return sdkMgmtAttr(auth, "proxy_url", "proxy-url")
}

func sdkMgmtAttr(auth *cliproxyauth.Auth, keys ...string) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	for _, key := range keys {
		if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
			return value
		}
	}
	return ""
}

func sdkMgmtAttrNumber(auth *cliproxyauth.Auth, key string) any {
	value := sdkMgmtAttr(auth, key)
	if value == "" {
		return nil
	}
	var number json.Number
	if err := json.Unmarshal([]byte(value), &number); err == nil {
		if i, err := number.Int64(); err == nil {
			return i
		}
		if f, err := number.Float64(); err == nil {
			return f
		}
	}
	return value
}

func sdkMgmtAttrBool(auth *cliproxyauth.Auth, key string) any {
	value := sdkMgmtAttr(auth, key)
	if value == "" {
		return nil
	}
	return value == "true"
}

func sdkMgmtMetadata(auth *cliproxyauth.Auth, key string) any {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	return auth.Metadata[key]
}

func sdkMgmtMaskSecret(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	if len(secret) <= 8 {
		return secret[:1] + "..." + secret[len(secret)-1:]
	}
	return secret[:4] + "..." + secret[len(secret)-4:]
}

func sdkMgmtLooksMasked(value string) bool {
	value = strings.TrimSpace(value)
	return strings.Contains(value, "...") || strings.Contains(value, "••") || strings.Contains(value, "***")
}

func sdkMgmtString(item map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := sdkMgmtPayloadString(item, key); ok {
			return value
		}
	}
	return ""
}

func sdkMgmtPayloadString(item map[string]any, key string) (string, bool) {
	value, ok := item[key]
	if !ok {
		return "", false
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed), true
	case float64, bool, json.Number:
		return fmt.Sprint(typed), true
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return "", true
		}
		return string(encoded), true
	}
}

func sdkMgmtPayloadBool(item map[string]any, key string) (bool, bool) {
	value, ok := item[key]
	if !ok {
		return false, false
	}
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		return strings.EqualFold(typed, "true"), true
	default:
		return false, true
	}
}

func sdkMgmtHasKey(item map[string]any, key string) bool {
	_, ok := item[key]
	return ok
}

func sdkMgmtHasAnyKey(item map[string]any, keys ...string) bool {
	for _, key := range keys {
		if sdkMgmtHasKey(item, key) {
			return true
		}
	}
	return false
}

func sdkMgmtRecentRequests(auth *cliproxyauth.Auth) []gin.H {
	buckets := auth.RecentRequestsSnapshot(time.Now().UTC())
	out := make([]gin.H, 0, len(buckets))
	for _, bucket := range buckets {
		out = append(out, gin.H{"start": bucket.Time, "success": bucket.Success, "failed": bucket.Failed})
	}
	return out
}

// ── Auth Files ──

func SDKMgmtAuthFilesListHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	if !sdkMgmtEnsureManager(c) {
		return
	}
	items := sdkMgmtFilteredAuthFiles(c)
	Success(c, gin.H{"files": items, "total": len(items)})
}

func SDKMgmtAuthFilesCreateHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	if !sdkMgmtEnsureManager(c) {
		return
	}
	form, err := c.MultipartForm()
	if err != nil || form == nil {
		Error(c, http.StatusBadRequest, 4001, "multipart json auth files are required")
		return
	}
	files := sdkMgmtUploadedAuthFiles(form)
	if len(files) == 0 {
		Error(c, http.StatusBadRequest, 4001, "at least one .json auth file is required")
		return
	}

	created := make([]gin.H, 0, len(files))
	for _, fileHeader := range files {
		auth, err := sdkMgmtAuthFromUpload(fileHeader)
		if err != nil {
			Error(c, http.StatusBadRequest, 4001, err.Error())
			return
		}
		registered, err := authManager.Register(c.Request.Context(), auth)
		if err != nil {
			Error(c, http.StatusInternalServerError, 5003, "failed to register auth file")
			return
		}
		created = append(created, sdkMgmtSerializeAuthFile(registered, len(created)))
	}
	Success(c, gin.H{"message": "created", "created": created, "count": len(created)})
}

func SDKMgmtAuthFilesUpdateHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	if !sdkMgmtEnsureManager(c) {
		return
	}
	var payload map[string]any
	if err := c.ShouldBindJSON(&payload); err != nil {
		Error(c, http.StatusBadRequest, 4001, "invalid JSON body")
		return
	}
	action := strings.ToLower(sdkMgmtString(payload, "action"))
	disabled, ok := map[string]bool{"disable": true, "enable": false}[action]
	if !ok {
		Error(c, http.StatusBadRequest, 4001, "action must be disable or enable")
		return
	}
	names := sdkMgmtPayloadStringSlice(payload, "names", "name", "ids", "id", "auth_ids", "auth_id")
	if len(names) == 0 {
		Error(c, http.StatusBadRequest, 4001, "names are required")
		return
	}
	updated, missing := sdkMgmtToggleAuthFiles(c.Request.Context(), names, disabled)
	Success(c, gin.H{"message": "updated", "updated": updated, "missing": missing})
}

func SDKMgmtAuthFilesDeleteHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	if !sdkMgmtEnsureManager(c) {
		return
	}
	ids := sdkMgmtDeleteAuthFileTargets(c)
	if len(ids) == 0 {
		Error(c, http.StatusBadRequest, 4001, "id, name, or auth_id is required")
		return
	}
	deleted, disabled, missing := sdkMgmtDeleteAuthFiles(c.Request.Context(), ids)
	Success(c, gin.H{"message": "deleted", "deleted": deleted, "disabled": disabled, "missing": missing})
}

func SDKMgmtAuthFilesQuotaHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	if !sdkMgmtEnsureManager(c) {
		return
	}
	items := make([]gin.H, 0)
	for index, auth := range sdkMgmtSortedAuths() {
		if auth == nil || sdkMgmtAuthDeleted(auth) {
			continue
		}
		if !sdkMgmtAuthFileMatchesQuery(c, auth, index) {
			continue
		}
		items = append(items, sdkMgmtSerializeAuthQuota(auth, index))
	}
	Success(c, gin.H{"quota": items, "items": items, "total": len(items)})
}

func SDKMgmtAuthFilesModelsHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	if !sdkMgmtEnsureManager(c) {
		return
	}
	items := make([]gin.H, 0)
	for index, auth := range sdkMgmtSortedAuths() {
		if auth == nil || sdkMgmtAuthDeleted(auth) {
			continue
		}
		if !sdkMgmtAuthFileMatchesQuery(c, auth, index) {
			continue
		}
		items = append(items, sdkMgmtSerializeAuthModels(auth, index)...)
	}
	Success(c, gin.H{"models": items, "total": len(items)})
}

func sdkMgmtFilteredAuthFiles(c *gin.Context) []gin.H {
	items := make([]gin.H, 0)
	for index, auth := range sdkMgmtSortedAuths() {
		if auth == nil || sdkMgmtAuthDeleted(auth) {
			continue
		}
		if !sdkMgmtAuthFileMatchesQuery(c, auth, index) {
			continue
		}
		items = append(items, sdkMgmtSerializeAuthFile(auth, index))
	}
	return items
}

func sdkMgmtSortedAuths() []*cliproxyauth.Auth {
	if authManager == nil {
		return nil
	}
	auths := authManager.List()
	sort.SliceStable(auths, func(i, j int) bool {
		if auths[i] == nil || auths[j] == nil {
			return auths[j] != nil
		}
		if auths[i].Provider != auths[j].Provider {
			return auths[i].Provider < auths[j].Provider
		}
		return sdkMgmtAuthStableName(auths[i], i) < sdkMgmtAuthStableName(auths[j], j)
	})
	return auths
}

func sdkMgmtAuthFileMatchesQuery(c *gin.Context, auth *cliproxyauth.Auth, index int) bool {
	provider := strings.TrimSpace(c.Query("provider"))
	if provider != "" && !strings.EqualFold(auth.Provider, provider) {
		return false
	}
	status := strings.TrimSpace(c.Query("status"))
	if status != "" && !strings.EqualFold(string(auth.Status), status) {
		return false
	}
	if rawDisabled := strings.TrimSpace(c.Query("disabled")); rawDisabled != "" {
		expected, err := strconv.ParseBool(rawDisabled)
		if err != nil || auth.Disabled != expected {
			return false
		}
	}
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))
	if q == "" {
		return true
	}
	haystack := strings.ToLower(strings.Join([]string{
		auth.ID,
		auth.Provider,
		auth.Label,
		sdkMgmtAuthStableName(auth, index),
		sdkMgmtSafeMetadataString(auth, "email"),
		sdkMgmtSafeMetadataString(auth, "account_id"),
		sdkMgmtAttr(auth, "project_id"),
		sdkMgmtAttr(auth, "location"),
		sdkMgmtAttr(auth, "base_url", "base-url"),
	}, "\n"))
	return strings.Contains(haystack, q)
}

func sdkMgmtSerializeAuthFile(auth *cliproxyauth.Auth, index int) gin.H {
	name := sdkMgmtAuthStableName(auth, index)
	models := sdkMgmtAuthModels(auth)
	item := gin.H{
		"id":                  auth.ID,
		"auth_id":             auth.ID,
		"name":                name,
		"label":               auth.Label,
		"provider":            auth.Provider,
		"type":                auth.Provider,
		"status":              string(auth.Status),
		"status_message":      auth.StatusMessage,
		"disabled":            auth.Disabled,
		"unavailable":         auth.Unavailable,
		"email":               sdkMgmtSafeMetadataString(auth, "email"),
		"runtime_only":        strings.EqualFold(sdkMgmtAttr(auth, "runtime_only"), "true"),
		"oauth":               strings.EqualFold(sdkMgmtAttr(auth, "oauth"), "true"),
		"has_api_key":         sdkMgmtHasMetadata(auth, "api_key"),
		"has_access_token":    sdkMgmtHasMetadata(auth, "access_token"),
		"has_refresh_token":   sdkMgmtHasMetadata(auth, "refresh_token"),
		"has_service_account": sdkMgmtHasMetadata(auth, "service_account"),
		"prefix":              auth.Prefix,
		"proxy_url":           sdkMgmtProxyURL(auth),
		"base_url":            sdkMgmtAttr(auth, "base_url", "base-url"),
		"project_id":          sdkMgmtAttr(auth, "project_id"),
		"location":            sdkMgmtAttr(auth, "location"),
		"created_at":          sdkMgmtTimeString(auth.CreatedAt),
		"updated_at":          sdkMgmtTimeString(auth.UpdatedAt),
		"last_refresh":        sdkMgmtTimeString(auth.LastRefreshedAt),
		"success":             auth.Success,
		"failed":              auth.Failed,
		"recent_requests":     sdkMgmtRecentRequests(auth),
		"quota_exceeded":      auth.Quota.Exceeded,
		"next_recover_at":     sdkMgmtTimeString(auth.Quota.NextRecoverAt),
		"models":              models,
		"model_count":         len(models),
	}
	if key := sdkMgmtAuthAPIKey(auth); key != "" {
		item["api_key_preview"] = sdkMgmtMaskSecret(key)
	}
	if token := sdkMgmtSafeMetadataString(auth, "access_token"); token != "" {
		item["access_token_preview"] = sdkMgmtMaskSecret(token)
	}
	if token := sdkMgmtSafeMetadataString(auth, "refresh_token"); token != "" {
		item["refresh_token_preview"] = sdkMgmtMaskSecret(token)
	}
	if accountID := sdkMgmtSafeMetadataString(auth, "account_id"); accountID != "" {
		item["account_id"] = accountID
		item["chatgpt_account_id"] = accountID
	}
	return item
}

func sdkMgmtSerializeAuthQuota(auth *cliproxyauth.Auth, index int) gin.H {
	return gin.H{
		"id":              auth.ID,
		"auth_id":         auth.ID,
		"name":            sdkMgmtAuthStableName(auth, index),
		"provider":        auth.Provider,
		"exceeded":        auth.Quota.Exceeded,
		"Exceeded":        auth.Quota.Exceeded,
		"reason":          auth.Quota.Reason,
		"next_recover_at": sdkMgmtTimeString(auth.Quota.NextRecoverAt),
		"NextRecoverAt":   sdkMgmtTimeString(auth.Quota.NextRecoverAt),
		"backoff_level":   auth.Quota.BackoffLevel,
	}
}

func sdkMgmtSerializeAuthModels(auth *cliproxyauth.Auth, index int) []gin.H {
	models := sdkMgmtAuthModels(auth)
	items := make([]gin.H, 0, len(models)+len(auth.ModelStates))
	seen := map[string]bool{}
	for _, model := range models {
		seen[model] = true
		items = append(items, gin.H{"id": auth.ID, "auth_id": auth.ID, "name": sdkMgmtAuthStableName(auth, index), "provider": auth.Provider, "model": model, "status": string(auth.Status), "disabled": auth.Disabled})
	}
	for model, state := range auth.ModelStates {
		if state == nil || seen[model] {
			continue
		}
		items = append(items, gin.H{"id": auth.ID, "auth_id": auth.ID, "name": sdkMgmtAuthStableName(auth, index), "provider": auth.Provider, "model": model, "status": string(state.Status), "status_message": state.StatusMessage, "unavailable": state.Unavailable, "next_retry_after": sdkMgmtTimeString(state.NextRetryAfter), "quota_exceeded": state.Quota.Exceeded, "next_recover_at": sdkMgmtTimeString(state.Quota.NextRecoverAt), "updated_at": sdkMgmtTimeString(state.UpdatedAt)})
	}
	return items
}

func sdkMgmtUploadedAuthFiles(form *multipart.Form) []*multipart.FileHeader {
	var files []*multipart.FileHeader
	for _, field := range []string{"file", "files", "auth_file", "auth_files"} {
		files = append(files, form.File[field]...)
	}
	return files
}

func sdkMgmtAuthFromUpload(fileHeader *multipart.FileHeader) (*cliproxyauth.Auth, error) {
	if fileHeader == nil || !strings.HasSuffix(strings.ToLower(fileHeader.Filename), ".json") {
		return nil, fmt.Errorf("auth file must be .json")
	}
	file, err := fileHeader.Open()
	if err != nil {
		return nil, fmt.Errorf("failed to open auth file")
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read auth file")
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("invalid auth JSON")
	}
	provider, err := sdkMgmtProviderFromAuthJSON(payload)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	auth := &cliproxyauth.Auth{ID: uuid.NewString(), Provider: provider, Status: cliproxyauth.StatusActive, CreatedAt: now, UpdatedAt: now, Attributes: map[string]string{}, Metadata: map[string]any{}}
	if id := sdkMgmtString(payload, "id", "auth_id"); id != "" {
		auth.ID = id
	}
	if label := sdkMgmtString(payload, "label", "name", "email"); label != "" {
		auth.Label = label
	} else {
		auth.Label = strings.TrimSuffix(fileHeader.Filename, ".json")
	}
	if sdkMgmtIsGoogleServiceAccount(payload) {
		auth.Metadata["service_account"] = payload
	}
	for _, field := range []string{"email", "account_id"} {
		if value := sdkMgmtString(payload, field); value != "" {
			auth.Metadata[field] = value
		}
	}
	for _, field := range []string{"api_key", "api-key", "x-api-key", "access_token", "refresh_token", "id_token", "token_data", "service_account"} {
		if value, ok := payload[field]; ok {
			auth.Metadata[sdkMgmtCanonicalAuthKey(field)] = value
		}
	}
	for _, field := range []string{"project_id", "location", "base_url", "base-url", "proxy_url", "proxy-url", "prefix"} {
		if value := sdkMgmtString(payload, field); value != "" {
			key := sdkMgmtCanonicalAuthKey(field)
			switch key {
			case "proxy_url":
				auth.ProxyURL = value
			case "prefix":
				auth.Prefix = value
			}
			auth.Attributes[key] = value
		}
	}
	if tokenData, ok := auth.Metadata["token_data"].(map[string]any); ok {
		for _, key := range []string{"access_token", "refresh_token", "id_token", "email", "account_id"} {
			if _, exists := auth.Metadata[key]; !exists {
				if value := strings.TrimSpace(fmt.Sprint(tokenData[key])); value != "" && value != "<nil>" {
					auth.Metadata[key] = value
				}
			}
		}
	}
	return auth, nil
}

func sdkMgmtProviderFromAuthJSON(payload map[string]any) (string, error) {
	provider := strings.ToLower(sdkMgmtString(payload, "provider", "type"))
	if sdkMgmtIsGoogleServiceAccount(payload) {
		if provider == "" || provider == "service_account" || provider == "google_service_account" {
			return "vertex", nil
		}
	}
	switch provider {
	case "anthropic":
		provider = sdkMgmtOAuthProviderClaude
	case "openai-compatibility", "openai_compatibility", "openai-compatible":
		provider = "openai"
	}
	if provider != "" {
		return provider, nil
	}
	if _, ok := payload["service_account"]; ok {
		return "vertex", nil
	}
	if sdkMgmtString(payload, "api_key", "api-key", "x-api-key") != "" {
		return "openai", nil
	}
	if _, ok := payload["token_data"]; ok || sdkMgmtString(payload, "access_token") != "" {
		return "", fmt.Errorf("provider is required for OAuth token auth JSON")
	}
	return "", fmt.Errorf("provider is required")
}

func sdkMgmtIsGoogleServiceAccount(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	return strings.EqualFold(sdkMgmtString(payload, "type"), "service_account") && sdkMgmtString(payload, "private_key") != "" && sdkMgmtString(payload, "client_email") != ""
}

func sdkMgmtCanonicalAuthKey(key string) string {
	key = strings.TrimSpace(strings.ToLower(strings.ReplaceAll(key, "-", "_")))
	if key == "x_api_key" {
		return "api_key"
	}
	return key
}

func sdkMgmtPayloadStringSlice(item map[string]any, keys ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, key := range keys {
		value, ok := item[key]
		if !ok {
			continue
		}
		appendValue := func(raw any) {
			text := strings.TrimSpace(fmt.Sprint(raw))
			if text == "" || text == "<nil>" || seen[text] {
				return
			}
			seen[text] = true
			out = append(out, text)
		}
		switch typed := value.(type) {
		case []any:
			for _, entry := range typed {
				appendValue(entry)
			}
		case []string:
			for _, entry := range typed {
				appendValue(entry)
			}
		default:
			appendValue(typed)
		}
	}
	return out
}

func sdkMgmtDeleteAuthFileTargets(c *gin.Context) []string {
	seen := map[string]bool{}
	var out []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		out = append(out, value)
	}
	for _, key := range []string{"id", "name", "auth_id"} {
		add(c.Query(key))
	}
	var payload map[string]any
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&payload); err == nil {
			for _, value := range sdkMgmtPayloadStringSlice(payload, "ids", "id", "names", "name", "auth_ids", "auth_id") {
				add(value)
			}
		}
	}
	return out
}

func sdkMgmtToggleAuthFiles(ctx context.Context, names []string, disabled bool) ([]string, []string) {
	updated := make([]string, 0, len(names))
	missing := make([]string, 0)
	for _, name := range names {
		auth, ok := sdkMgmtFindAuthFile(name)
		if !ok {
			missing = append(missing, name)
			continue
		}
		auth.Disabled = disabled
		if disabled {
			auth.Status = cliproxyauth.StatusDisabled
		} else if auth.Status == cliproxyauth.StatusDisabled {
			auth.Status = cliproxyauth.StatusActive
		}
		auth.UpdatedAt = time.Now().UTC()
		if saved, err := authManager.Update(ctx, auth); err == nil && saved != nil {
			updated = append(updated, sdkMgmtAuthStableName(saved, len(updated)))
		}
	}
	return updated, missing
}

func sdkMgmtDeleteAuthFiles(ctx context.Context, names []string) ([]string, []string, []string) {
	deleted := make([]string, 0, len(names))
	disabled := make([]string, 0, len(names))
	missing := make([]string, 0)
	for _, name := range names {
		auth, ok := sdkMgmtFindAuthFile(name)
		if !ok {
			missing = append(missing, name)
			continue
		}
		if GlobalStore != nil {
			_ = GlobalStore.Delete(ctx, auth.ID)
		}
		if auth.Attributes == nil {
			auth.Attributes = map[string]string{}
		}
		if auth.Metadata == nil {
			auth.Metadata = map[string]any{}
		}
		auth.Disabled = true
		auth.Status = cliproxyauth.StatusDisabled
		auth.Attributes["deleted"] = "true"
		auth.Metadata["deleted"] = true
		auth.Metadata["deleted_at"] = time.Now().UTC().Format(time.RFC3339)
		auth.UpdatedAt = time.Now().UTC()
		_, _ = authManager.Update(ctx, auth)
		deleted = append(deleted, auth.ID)
		disabled = append(disabled, sdkMgmtAuthStableName(auth, len(disabled)))
	}
	return deleted, disabled, missing
}

func sdkMgmtFindAuthFile(target string) (*cliproxyauth.Auth, bool) {
	target = strings.TrimSpace(target)
	if target == "" || authManager == nil {
		return nil, false
	}
	if auth, ok := authManager.GetByID(target); ok && !sdkMgmtAuthDeleted(auth) {
		return auth, true
	}
	for index, auth := range sdkMgmtSortedAuths() {
		if auth == nil || sdkMgmtAuthDeleted(auth) {
			continue
		}
		if target == auth.ID || target == auth.Label || target == sdkMgmtAuthStableName(auth, index) {
			return auth, true
		}
	}
	return nil, false
}

func sdkMgmtAuthStableName(auth *cliproxyauth.Auth, index int) string {
	if auth == nil {
		return ""
	}
	if auth.Label != "" {
		return auth.Label
	}
	if email := sdkMgmtSafeMetadataString(auth, "email"); email != "" {
		return email
	}
	if auth.ID != "" {
		return auth.ID
	}
	return sdkMgmtAuthName(auth, index)
}

func sdkMgmtTimeString(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func sdkMgmtHasMetadata(auth *cliproxyauth.Auth, key string) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	value, ok := auth.Metadata[key]
	if !ok || value == nil {
		return false
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text) != ""
	}
	return true
}

func sdkMgmtSafeMetadataString(auth *cliproxyauth.Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	value := auth.Metadata[key]
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number, float64, bool:
		return fmt.Sprint(typed)
	default:
		return ""
	}
}

func sdkMgmtAuthModels(auth *cliproxyauth.Auth) []string {
	seen := map[string]bool{}
	var models []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		models = append(models, value)
	}
	if auth != nil {
		for model := range auth.ModelStates {
			add(model)
		}
		if raw := sdkMgmtMetadata(auth, "models"); raw != nil {
			switch typed := raw.(type) {
			case []any:
				for _, item := range typed {
					add(fmt.Sprint(item))
				}
			case []string:
				for _, item := range typed {
					add(item)
				}
			case string:
				for item := range strings.SplitSeq(typed, ",") {
					add(item)
				}
			}
		}
	}
	sort.Strings(models)
	return models
}

// ── OAuth ──

func SDKMgmtOAuthSessionsHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	if !sdkMgmtOAuthDBReady(c) {
		return
	}
	sdkMgmtCleanupExpiredOAuthSessions(c.Request.Context())

	var sessions []model.OAuthSession
	if err := GlobalDB.WithContext(c.Request.Context()).Where("expires_at > ? OR status IN ?", time.Now().UTC(), []string{"completed", "failed"}).Order("created_at DESC").Find(&sessions).Error; err != nil {
		Error(c, http.StatusInternalServerError, 5003, "failed to list OAuth sessions")
		return
	}

	items := make([]gin.H, 0, len(sessions))
	for _, session := range sessions {
		items = append(items, sdkMgmtSerializeOAuthSession(session))
	}
	Success(c, gin.H{"sessions": items})
}

func SDKMgmtOAuthCallbackHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	if !sdkMgmtOAuthDBReady(c) || !sdkMgmtEnsureManager(c) {
		return
	}

	provider, ok := sdkMgmtCanonicalOAuthProvider(c.Param("provider"))
	if !ok {
		Error(c, http.StatusNotFound, 4040, "unsupported OAuth provider")
		return
	}
	code, state := sdkMgmtOAuthCallbackParams(c)
	if code == "" || state == "" {
		Error(c, http.StatusBadRequest, 4001, "OAuth code and state are required")
		return
	}

	session, cfg, ok := sdkMgmtLoadPendingOAuthSession(c, provider, state)
	if !ok {
		return
	}
	token, email, accountID, err := sdkMgmtExchangeOAuthToken(c.Request.Context(), provider, code, cfg)
	if err != nil {
		sdkMgmtMarkOAuthSession(c.Request.Context(), &session, "failed", nil)
		Error(c, http.StatusBadGateway, 5021, "OAuth token exchange failed")
		return
	}

	auth := sdkMgmtOAuthAuthRecord(provider, token, email, accountID)
	registered, err := authManager.Register(c.Request.Context(), auth)
	if err != nil {
		sdkMgmtMarkOAuthSession(c.Request.Context(), &session, "failed", nil)
		Error(c, http.StatusInternalServerError, 5001, "failed to register OAuth auth")
		return
	}
	sdkMgmtMarkOAuthSession(c.Request.Context(), &session, "completed", &registered.ID)
	Success(c, gin.H{"message": "OAuth completed", "provider": provider, "auth_id": registered.ID})
}

func SDKMgmtOAuthStatusHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	if !sdkMgmtOAuthDBReady(c) {
		return
	}
	state := strings.TrimSpace(c.Query("state"))
	if state == "" {
		Error(c, http.StatusBadRequest, 4001, "state is required")
		return
	}

	var session model.OAuthSession
	err := GlobalDB.WithContext(c.Request.Context()).Where("state = ?", state).First(&session).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			Success(c, gin.H{"status": "missing"})
			return
		}
		Error(c, http.StatusInternalServerError, 5003, "failed to load OAuth session")
		return
	}
	if session.Status == "pending" && time.Now().UTC().After(session.ExpiresAt) {
		sdkMgmtMarkOAuthSession(c.Request.Context(), &session, "failed", nil)
		Success(c, gin.H{"status": "error", "message": "OAuth session expired"})
		return
	}
	switch session.Status {
	case "completed":
		Success(c, gin.H{"status": "success", "provider": session.Provider, "auth_id": session.AuthID})
	case "failed":
		Success(c, gin.H{"status": "error", "provider": session.Provider})
	default:
		Success(c, gin.H{"status": "wait", "provider": session.Provider})
	}
}

func sdkMgmtOAuthDBReady(c *gin.Context) bool {
	if GlobalDB == nil {
		Error(c, http.StatusServiceUnavailable, 5032, "database is not initialized")
		return false
	}
	return true
}

func sdkMgmtCanonicalOAuthProvider(provider string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case sdkMgmtOAuthProviderGemini:
		return sdkMgmtOAuthProviderGemini, true
	case sdkMgmtOAuthProviderClaude:
		return sdkMgmtOAuthProviderClaude, true
	case sdkMgmtOAuthProviderCodex:
		return sdkMgmtOAuthProviderCodex, true
	default:
		return "", false
	}
}

func sdkMgmtOAuthRedirectURI(c *gin.Context, provider string) string {
	scheme := "http"
	if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := c.Request.Host
	if forwarded := strings.TrimSpace(c.GetHeader("X-Forwarded-Host")); forwarded != "" {
		host = forwarded
	}
	return fmt.Sprintf("%s://%s/api/panel/admin/sdk-management/oauth-callback/%s", scheme, host, provider)
}

func sdkMgmtBuildOAuthAuthURL(provider string, state string, cfg *sdkMgmtOAuthSessionConfig) (string, error) {
	switch provider {
	case sdkMgmtOAuthProviderGemini:
		values := url.Values{
			"client_id":     {sdkMgmtGeminiClientID},
			"response_type": {"code"},
			"redirect_uri":  {cfg.RedirectURI},
			"scope":         {"https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile"},
			"state":         {state},
			"access_type":   {"offline"},
			"prompt":        {"consent"},
		}
		return sdkMgmtGeminiAuthURL + "?" + values.Encode(), nil
	case sdkMgmtOAuthProviderClaude:
		verifier, challenge, err := sdkMgmtGeneratePKCE()
		if err != nil {
			return "", err
		}
		cfg.CodeVerifier = verifier
		cfg.CodeChallengeMethod = "S256"
		values := url.Values{
			"code":                  {"true"},
			"client_id":             {sdkMgmtClaudeClientID},
			"response_type":         {"code"},
			"redirect_uri":          {cfg.RedirectURI},
			"scope":                 {"user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
			"state":                 {state},
		}
		return sdkMgmtClaudeAuthURL + "?" + values.Encode(), nil
	case sdkMgmtOAuthProviderCodex:
		verifier, challenge, err := sdkMgmtGeneratePKCE()
		if err != nil {
			return "", err
		}
		cfg.CodeVerifier = verifier
		cfg.CodeChallengeMethod = "S256"
		values := url.Values{
			"client_id":                  {sdkMgmtCodexClientID},
			"response_type":              {"code"},
			"redirect_uri":               {cfg.RedirectURI},
			"scope":                      {"openid email profile offline_access"},
			"state":                      {state},
			"code_challenge":             {challenge},
			"code_challenge_method":      {"S256"},
			"prompt":                     {"login"},
			"id_token_add_organizations": {"true"},
			"codex_cli_simplified_flow":  {"true"},
		}
		return sdkMgmtCodexAuthURL + "?" + values.Encode(), nil
	default:
		return "", fmt.Errorf("unsupported OAuth provider")
	}
}

func sdkMgmtGeneratePKCE() (string, string, error) {
	randomBytes := make([]byte, 96)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", "", err
	}
	verifier := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(randomBytes)
	hash := sha256.Sum256([]byte(verifier))
	challenge := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(hash[:])
	return verifier, challenge, nil
}

func sdkMgmtOAuthCallbackParams(c *gin.Context) (string, string) {
	code := strings.TrimSpace(c.Query("code"))
	state := strings.TrimSpace(c.Query("state"))
	if code != "" && state != "" {
		return code, state
	}
	_ = c.Request.ParseForm()
	if code == "" {
		code = strings.TrimSpace(c.Request.FormValue("code"))
	}
	if state == "" {
		state = strings.TrimSpace(c.Request.FormValue("state"))
	}
	if code != "" && state != "" {
		return code, state
	}
	var body struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := c.ShouldBindJSON(&body); err == nil {
		if code == "" {
			code = strings.TrimSpace(body.Code)
		}
		if state == "" {
			state = strings.TrimSpace(body.State)
		}
	}
	return code, state
}

func sdkMgmtLoadPendingOAuthSession(c *gin.Context, provider string, state string) (model.OAuthSession, sdkMgmtOAuthSessionConfig, bool) {
	var session model.OAuthSession
	err := GlobalDB.WithContext(c.Request.Context()).Where("state = ?", state).First(&session).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			Error(c, http.StatusBadRequest, 4002, "OAuth session not found")
			return session, sdkMgmtOAuthSessionConfig{}, false
		}
		Error(c, http.StatusInternalServerError, 5003, "failed to load OAuth session")
		return session, sdkMgmtOAuthSessionConfig{}, false
	}
	var cfg sdkMgmtOAuthSessionConfig
	if len(session.ConfigData) > 0 {
		_ = json.Unmarshal(session.ConfigData, &cfg)
	}
	if session.Provider != provider || cfg.Provider != provider {
		Error(c, http.StatusBadRequest, 4002, "OAuth session provider mismatch")
		return session, cfg, false
	}
	if session.Status != "pending" {
		Error(c, http.StatusBadRequest, 4002, "OAuth session is not pending")
		return session, cfg, false
	}
	if time.Now().UTC().After(session.ExpiresAt) {
		sdkMgmtMarkOAuthSession(c.Request.Context(), &session, "failed", nil)
		Error(c, http.StatusBadRequest, 4002, "OAuth session expired")
		return session, cfg, false
	}
	if strings.TrimSpace(cfg.RedirectURI) == "" {
		Error(c, http.StatusBadRequest, 4002, "OAuth session is incomplete")
		return session, cfg, false
	}
	return session, cfg, true
}

func sdkMgmtExchangeOAuthToken(ctx context.Context, provider string, code string, cfg sdkMgmtOAuthSessionConfig) (sdkMgmtOAuthTokenResponse, string, string, error) {
	switch provider {
	case sdkMgmtOAuthProviderGemini:
		form := url.Values{
			"grant_type":   {"authorization_code"},
			"client_id":    {sdkMgmtGeminiClientID},
			"code":         {code},
			"redirect_uri": {cfg.RedirectURI},
		}
		token, err := sdkMgmtPostFormToken(ctx, sdkMgmtGeminiTokenURL, form)
		if err != nil {
			return token, "", "", err
		}
		email := sdkMgmtFetchOAuthEmail(ctx, sdkMgmtGeminiUserInfoURL, token.AccessToken, "email")
		return token, email, "", nil
	case sdkMgmtOAuthProviderClaude:
		if strings.TrimSpace(cfg.CodeVerifier) == "" {
			return sdkMgmtOAuthTokenResponse{}, "", "", fmt.Errorf("missing PKCE verifier")
		}
		body := map[string]any{
			"code":          sdkMgmtClaudeCallbackCode(code),
			"state":         cfg.State,
			"grant_type":    "authorization_code",
			"client_id":     sdkMgmtClaudeClientID,
			"redirect_uri":  cfg.RedirectURI,
			"code_verifier": cfg.CodeVerifier,
		}
		if _, callbackState := sdkMgmtClaudeCodeAndState(code); callbackState != "" {
			body["state"] = callbackState
		}
		token, err := sdkMgmtPostJSONToken(ctx, sdkMgmtClaudeTokenURL, body)
		if err != nil {
			return token, "", "", err
		}
		email := nestedString(token.Raw, "account", "email_address")
		return token, email, stringFromMap(token.Raw, "organization_uuid"), nil
	case sdkMgmtOAuthProviderCodex:
		if strings.TrimSpace(cfg.CodeVerifier) == "" {
			return sdkMgmtOAuthTokenResponse{}, "", "", fmt.Errorf("missing PKCE verifier")
		}
		form := url.Values{
			"grant_type":    {"authorization_code"},
			"client_id":     {sdkMgmtCodexClientID},
			"code":          {code},
			"redirect_uri":  {cfg.RedirectURI},
			"code_verifier": {cfg.CodeVerifier},
		}
		token, err := sdkMgmtPostFormToken(ctx, sdkMgmtCodexTokenURL, form)
		if err != nil {
			return token, "", "", err
		}
		email, accountID := sdkMgmtClaimsFromJWT(token.IDToken)
		return token, email, accountID, nil
	default:
		return sdkMgmtOAuthTokenResponse{}, "", "", fmt.Errorf("unsupported OAuth provider")
	}
}

func sdkMgmtPostFormToken(ctx context.Context, endpoint string, form url.Values) (sdkMgmtOAuthTokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return sdkMgmtOAuthTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return sdkMgmtDoTokenRequest(req)
}

func sdkMgmtPostJSONToken(ctx context.Context, endpoint string, body map[string]any) (sdkMgmtOAuthTokenResponse, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return sdkMgmtOAuthTokenResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(encoded)))
	if err != nil {
		return sdkMgmtOAuthTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return sdkMgmtDoTokenRequest(req)
}

func sdkMgmtDoTokenRequest(req *http.Request) (sdkMgmtOAuthTokenResponse, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return sdkMgmtOAuthTokenResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return sdkMgmtOAuthTokenResponse{}, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return sdkMgmtOAuthTokenResponse{}, fmt.Errorf("token endpoint returned status %d", resp.StatusCode)
	}
	var token sdkMgmtOAuthTokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return sdkMgmtOAuthTokenResponse{}, err
	}
	_ = json.Unmarshal(body, &token.Raw)
	return token, nil
}

func sdkMgmtFetchOAuthEmail(ctx context.Context, endpoint string, accessToken string, key string) string {
	if strings.TrimSpace(accessToken) == "" {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return ""
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return ""
	}
	return stringFromMap(payload, key)
}

func sdkMgmtOAuthAuthRecord(provider string, token sdkMgmtOAuthTokenResponse, email string, accountID string) *cliproxyauth.Auth {
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(token.ExpiresIn) * time.Second)
	if token.ExpiresIn <= 0 {
		expiresAt = time.Time{}
	}
	metadata := map[string]any{
		"access_token": token.AccessToken,
		"last_refresh": now.Format(time.RFC3339),
		"oauth":        true,
		"token_data": map[string]any{
			"access_token": token.AccessToken,
		},
	}
	if token.RefreshToken != "" {
		metadata["refresh_token"] = token.RefreshToken
		metadata["token_data"].(map[string]any)["refresh_token"] = token.RefreshToken
	}
	if token.IDToken != "" {
		metadata["id_token"] = token.IDToken
		metadata["token_data"].(map[string]any)["id_token"] = token.IDToken
	}
	if !expiresAt.IsZero() {
		metadata["expires_at"] = expiresAt.Format(time.RFC3339)
		metadata["expired"] = expiresAt.Format(time.RFC3339)
		metadata["token_data"].(map[string]any)["expires_at"] = expiresAt.Format(time.RFC3339)
		metadata["token_data"].(map[string]any)["expired"] = expiresAt.Format(time.RFC3339)
	}
	if email != "" {
		metadata["email"] = email
		metadata["token_data"].(map[string]any)["email"] = email
	}
	if accountID != "" {
		metadata["account_id"] = accountID
		metadata["token_data"].(map[string]any)["account_id"] = accountID
	}
	if provider == sdkMgmtOAuthProviderGemini {
		metadata["token"] = sdkMgmtGeminiTokenMetadata(token)
	}
	label := provider + " OAuth"
	if email != "" {
		label = label + " (" + email + ")"
	}
	return &cliproxyauth.Auth{ID: uuid.NewString(), Provider: provider, Label: label, Status: cliproxyauth.StatusActive, Attributes: map[string]string{"oauth": "true"}, Metadata: metadata, CreatedAt: now, UpdatedAt: now, LastRefreshedAt: now}
}

func sdkMgmtGeminiTokenMetadata(token sdkMgmtOAuthTokenResponse) map[string]any {
	values := map[string]any{
		"access_token":    token.AccessToken,
		"refresh_token":   token.RefreshToken,
		"token_type":      token.TokenType,
		"expiry":          time.Now().UTC().Add(time.Duration(token.ExpiresIn) * time.Second).Format(time.RFC3339),
		"token_uri":       sdkMgmtGeminiTokenURL,
		"client_id":       sdkMgmtGeminiClientID,
		"scopes":          []string{"https://www.googleapis.com/auth/cloud-platform", "https://www.googleapis.com/auth/userinfo.email", "https://www.googleapis.com/auth/userinfo.profile"},
		"universe_domain": "googleapis.com",
	}
	if token.ExpiresIn <= 0 {
		delete(values, "expiry")
	}
	return values
}

func sdkMgmtMarkOAuthSession(ctx context.Context, session *model.OAuthSession, status string, authID *string) {
	if GlobalDB == nil || session == nil || session.ID == 0 {
		return
	}
	updates := map[string]any{"status": status}
	if authID != nil {
		updates["auth_id"] = *authID
	}
	_ = GlobalDB.WithContext(ctx).Model(session).Updates(updates).Error
}

func sdkMgmtCleanupExpiredOAuthSessions(ctx context.Context) {
	if GlobalDB == nil {
		return
	}
	_ = GlobalDB.WithContext(ctx).Model(&model.OAuthSession{}).Where("status = ? AND expires_at <= ?", "pending", time.Now().UTC()).Update("status", "failed").Error
}

func sdkMgmtSerializeOAuthSession(session model.OAuthSession) gin.H {
	status := session.Status
	if status == "pending" && time.Now().UTC().After(session.ExpiresAt) {
		status = "failed"
	}
	return gin.H{"id": session.ID, "provider": session.Provider, "status": status, "auth_id": session.AuthID, "created_at": session.CreatedAt.UTC().Format(time.RFC3339), "expires_at": session.ExpiresAt.UTC().Format(time.RFC3339)}
}

func sdkMgmtClaudeCodeAndState(code string) (string, string) {
	parts := strings.SplitN(code, "#", 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	}
	return strings.TrimSpace(code), ""
}

func sdkMgmtClaudeCallbackCode(code string) string {
	parsed, _ := sdkMgmtClaudeCodeAndState(code)
	return parsed
}

func sdkMgmtClaimsFromJWT(token string) (string, string) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", ""
	}
	email := stringFromMap(claims, "email")
	accountID := ""
	if authClaims, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		accountID = stringFromMap(authClaims, "account_id")
	}
	if accountID == "" {
		accountID = stringFromMap(claims, "account_id")
	}
	if accountID == "" {
		accountID = stringFromMap(claims, "sub")
	}
	return email, accountID
}

func sdkMgmtOAuthAuthURLHandler(c *gin.Context, endpoint string) {
	if !sdkMgmtAuthURLProviderSupported(c, endpoint) {
		return
	}
	if !sdkMgmtOAuthDBReady(c) {
		return
	}
	provider := sdkMgmtAuthURLProviders[endpoint]
	state := uuid.NewString()
	redirectURI := sdkMgmtOAuthRedirectURI(c, provider)
	cfg := sdkMgmtOAuthSessionConfig{
		Provider:    provider,
		EndpointKey: endpoint,
		State:       state,
		RedirectURI: redirectURI,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		ExpiresAt:   time.Now().UTC().Add(sdkMgmtOAuthSessionTTL).Format(time.RFC3339),
	}
	if endpoint == "anthropic-auth-url" {
		cfg.ProviderAlias = sdkMgmtOAuthProviderAnthropic
	}

	authURL, err := sdkMgmtBuildOAuthAuthURL(provider, state, &cfg)
	if err != nil {
		Error(c, http.StatusInternalServerError, 5004, "failed to create OAuth URL")
		return
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		Error(c, http.StatusInternalServerError, 5004, "failed to create OAuth session")
		return
	}
	session := model.OAuthSession{Provider: provider, State: state, AuthURL: authURL, Status: "pending", ConfigData: encoded, ExpiresAt: time.Now().UTC().Add(sdkMgmtOAuthSessionTTL)}
	if err := GlobalDB.WithContext(c.Request.Context()).Create(&session).Error; err != nil {
		Error(c, http.StatusInternalServerError, 5004, "failed to store OAuth session")
		return
	}
	Success(c, gin.H{"auth_url": authURL, "url": authURL, "state": state})
}

// ── Ampcode ──

func SDKMgmtAmpcodeGetHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	m, err := loadAmpcodeConfig(c.Request.Context())
	if err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to load ampcode config")
		return
	}
	Success(c, normalizeAmpcodeResponse(m))
}

func SDKMgmtAmpcodePutHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var body map[string]any
	if err := c.ShouldBindJSON(&body); err != nil {
		Error(c, http.StatusBadRequest, 4000, "invalid request body")
		return
	}
	payload := body
	if v, ok := body["ampcode"].(map[string]any); ok {
		payload = v
	} else if _, exists := body["ampcode"]; exists {
		Error(c, http.StatusBadRequest, 4000, "invalid ampcode wrapper: expected object")
		return
	}
	normalizeAmpcodeInputKeys(payload)
	m, err := loadAmpcodeConfig(c.Request.Context())
	if err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to load ampcode config")
		return
	}
	maps.Copy(m, payload)
	if err := saveAmpcodeConfig(c.Request.Context(), m); err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to save ampcode config")
		return
	}
	Success(c, normalizeAmpcodeResponse(m))
}

// ── /ampcode/model-mappings ──

func SDKMgmtAmpcodeModelMappingsGetHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	m, err := loadAmpcodeConfig(c.Request.Context())
	if err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to load ampcode config")
		return
	}
	var mappings []any
	if v, ok := m["model-mappings"]; ok {
		if arr, ok := v.([]any); ok {
			mappings = arr
		}
	}
	if mappings == nil {
		mappings = []any{}
	}
	Success(c, gin.H{"model-mappings": mappings, "mappings": mappings})
}

func SDKMgmtAmpcodeModelMappingsPutHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	raw, err := c.GetRawData()
	if err != nil {
		Error(c, http.StatusBadRequest, 4000, "cannot read request body")
		return
	}
	var mappings []any
	var wrapped struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Value != nil {
		if err := json.Unmarshal(wrapped.Value, &mappings); err != nil {
			Error(c, http.StatusBadRequest, 4000, "invalid model-mappings: value must be an array")
			return
		}
	} else if err := json.Unmarshal(raw, &mappings); err != nil {
		Error(c, http.StatusBadRequest, 4000, "invalid model-mappings: expected array or {value:array}")
		return
	}
	m, err := loadAmpcodeConfig(c.Request.Context())
	if err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to load ampcode config")
		return
	}
	m["model-mappings"] = mappings
	if err := saveAmpcodeConfig(c.Request.Context(), m); err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to save ampcode config")
		return
	}
	Success(c, gin.H{"model-mappings": mappings, "mappings": mappings})
}

func SDKMgmtAmpcodeModelMappingsDeleteHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	m, err := loadAmpcodeConfig(c.Request.Context())
	if err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to load ampcode config")
		return
	}
	delete(m, "model-mappings")
	if err := saveAmpcodeConfig(c.Request.Context(), m); err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to save ampcode config")
		return
	}
	Success(c, gin.H{"model-mappings": []any{}, "mappings": []any{}})
}

// ── /ampcode/upstream-api-keys ──

func SDKMgmtAmpcodeUpstreamAPIKeysGetHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	m, err := loadAmpcodeConfig(c.Request.Context())
	if err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to load ampcode config")
		return
	}
	var keys []any
	if v, ok := m["upstream-api-keys"]; ok {
		if arr, ok := v.([]any); ok {
			keys = arr
		}
	}
	if keys == nil {
		keys = []any{}
	}
	Success(c, gin.H{"upstream-api-keys": keys})
}

func SDKMgmtAmpcodeUpstreamAPIKeysPutHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	raw, err := c.GetRawData()
	if err != nil {
		Error(c, http.StatusBadRequest, 4000, "cannot read request body")
		return
	}
	var entries []any
	var wrapped struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Value != nil {
		if err := json.Unmarshal(wrapped.Value, &entries); err != nil {
			Error(c, http.StatusBadRequest, 4000, "invalid upstream-api-keys: value must be an array")
			return
		}
	} else if err := json.Unmarshal(raw, &entries); err != nil {
		Error(c, http.StatusBadRequest, 4000, "invalid upstream-api-keys: expected array or {value:array}")
		return
	}
	m, err := loadAmpcodeConfig(c.Request.Context())
	if err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to load ampcode config")
		return
	}
	m["upstream-api-keys"] = entries
	if err := saveAmpcodeConfig(c.Request.Context(), m); err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to save ampcode config")
		return
	}
	Success(c, gin.H{"upstream-api-keys": entries})
}

func SDKMgmtAmpcodeUpstreamAPIKeysDeleteHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var body struct {
		Value []string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.Value) == 0 {
		Error(c, http.StatusBadRequest, 4000, "invalid request: expected {value:[...upstream-key...]}")
		return
	}
	remove := make(map[string]bool, len(body.Value))
	for _, k := range body.Value {
		remove[k] = true
	}
	m, err := loadAmpcodeConfig(c.Request.Context())
	if err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to load ampcode config")
		return
	}
	existing, _ := m["upstream-api-keys"].([]any)
	filtered := make([]any, 0, len(existing))
	for _, entry := range existing {
		if e, ok := entry.(map[string]any); ok {
			key, _ := e["upstream-api-key"].(string)
			if remove[key] {
				continue
			}
		}
		filtered = append(filtered, entry)
	}
	m["upstream-api-keys"] = filtered
	if err := saveAmpcodeConfig(c.Request.Context(), m); err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to save ampcode config")
		return
	}
	Success(c, gin.H{"upstream-api-keys": filtered})
}

// ── /ampcode/upstream-url ──

func SDKMgmtAmpcodeUpstreamURLPutHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		Error(c, http.StatusBadRequest, 4000, "invalid request body")
		return
	}
	m, err := loadAmpcodeConfig(c.Request.Context())
	if err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to load ampcode config")
		return
	}
	m["upstream-url"] = body.Value
	if err := saveAmpcodeConfig(c.Request.Context(), m); err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to save ampcode config")
		return
	}
	Success(c, normalizeAmpcodeResponse(m))
}

func SDKMgmtAmpcodeUpstreamURLDeleteHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	m, err := loadAmpcodeConfig(c.Request.Context())
	if err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to load ampcode config")
		return
	}
	delete(m, "upstream-url")
	if err := saveAmpcodeConfig(c.Request.Context(), m); err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to save ampcode config")
		return
	}
	Success(c, normalizeAmpcodeResponse(m))
}

// ── /ampcode/upstream-api-key (singular) ──

func SDKMgmtAmpcodeUpstreamAPIKeyPutHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		Error(c, http.StatusBadRequest, 4000, "invalid request body")
		return
	}
	m, err := loadAmpcodeConfig(c.Request.Context())
	if err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to load ampcode config")
		return
	}
	m["upstream-api-key"] = body.Value
	if err := saveAmpcodeConfig(c.Request.Context(), m); err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to save ampcode config")
		return
	}
	Success(c, normalizeAmpcodeResponse(m))
}

func SDKMgmtAmpcodeUpstreamAPIKeyDeleteHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	m, err := loadAmpcodeConfig(c.Request.Context())
	if err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to load ampcode config")
		return
	}
	delete(m, "upstream-api-key")
	if err := saveAmpcodeConfig(c.Request.Context(), m); err != nil {
		Error(c, http.StatusInternalServerError, 5000, "failed to save ampcode config")
		return
	}
	Success(c, normalizeAmpcodeResponse(m))
}

// ── SDK Config ──

// ── SDK Config Persistence ──

func sdkMgmtReadConfig() (map[string]any, error) {
	var pc model.ProviderConfig
	err := GlobalDB.Where("provider = ?", "sdk_config").First(&pc).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return make(map[string]any), nil
		}
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal(pc.ConfigData, &data); err != nil || data == nil {
		return make(map[string]any), nil
	}
	return data, nil
}

func sdkMgmtWriteConfig(data map[string]any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return GlobalDB.Where("provider = ?", "sdk_config").
		Assign(model.ProviderConfig{ConfigData: raw}).
		FirstOrCreate(&model.ProviderConfig{Provider: "sdk_config", ConfigData: raw}).Error
}

// ── Config Handlers ──

func SDKMgmtConfigGetHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	config, err := sdkMgmtReadConfig()
	if err != nil {
		Error(c, http.StatusInternalServerError, 5004, "failed to read SDK config")
		return
	}
	Success(c, sdkMgmtExpandConfigAliases(config))
}

func SDKMgmtConfigPutHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var incoming map[string]any
	if err := c.ShouldBindJSON(&incoming); err != nil {
		Error(c, http.StatusBadRequest, 4000, "invalid JSON body")
		return
	}
	normalized := sdkMgmtNormalizeConfigMap(incoming)
	config, err := sdkMgmtReadConfig()
	if err != nil {
		Error(c, http.StatusInternalServerError, 5004, "failed to read SDK config")
		return
	}
	maps.Copy(config, normalized)
	if err := sdkMgmtWriteConfig(config); err != nil {
		Error(c, http.StatusInternalServerError, 5005, "failed to save SDK config")
		return
	}
	Success(c, gin.H{"message": "updated"})
}

// ── Convenience Config Key Handlers ──

func sdkMgmtConfigGetHandlerFn(key string, aliases ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireAdmin(c) {
			return
		}
		config, err := sdkMgmtReadConfig()
		if err != nil {
			Error(c, http.StatusInternalServerError, 5004, "failed to read SDK config")
			return
		}
		val := config[key]
		result := gin.H{key: val}
		for _, alias := range aliases {
			result[alias] = val
		}
		Success(c, result)
	}
}

func sdkMgmtConfigSetHandlerFn(key string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireAdmin(c) {
			return
		}
		var body struct {
			Value any `json:"value"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			Error(c, http.StatusBadRequest, 4000, `invalid JSON body: {"value": ...} expected`)
			return
		}
		config, err := sdkMgmtReadConfig()
		if err != nil {
			Error(c, http.StatusInternalServerError, 5004, "failed to read SDK config")
			return
		}
		config[key] = body.Value
		if err := sdkMgmtWriteConfig(config); err != nil {
			Error(c, http.StatusInternalServerError, 5005, "failed to save SDK config")
			return
		}
		Success(c, gin.H{"message": "updated"})
	}
}

func sdkMgmtConfigDeleteHandlerFn(key string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireAdmin(c) {
			return
		}
		config, err := sdkMgmtReadConfig()
		if err != nil {
			Error(c, http.StatusInternalServerError, 5004, "failed to read SDK config")
			return
		}
		delete(config, key)
		if err := sdkMgmtWriteConfig(config); err != nil {
			Error(c, http.StatusInternalServerError, 5005, "failed to save SDK config")
			return
		}
		Success(c, gin.H{"message": "deleted"})
	}
}

func sdkMgmtConfigGetRoutingStrategyFn() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireAdmin(c) {
			return
		}
		config, err := sdkMgmtReadConfig()
		if err != nil {
			Error(c, http.StatusInternalServerError, 5004, "failed to read SDK config")
			return
		}
		val, ok := config["routing-strategy"]
		if !ok || val == nil {
			val = "round-robin"
		}
		Success(c, gin.H{
			"strategy":         val,
			"routing-strategy": val,
			"routingStrategy":  val,
		})
	}
}

func sdkMgmtConfigGetForceModelPrefixFn() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireAdmin(c) {
			return
		}
		config, err := sdkMgmtReadConfig()
		if err != nil {
			Error(c, http.StatusInternalServerError, 5004, "failed to read SDK config")
			return
		}
		val, _ := config["force-model-prefix"]
		Success(c, gin.H{
			"force-model-prefix": val,
			"forceModelPrefix":   val,
		})
	}
}

func sdkMgmtConfigGetLogsMaxSizeFn() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireAdmin(c) {
			return
		}
		config, err := sdkMgmtReadConfig()
		if err != nil {
			Error(c, http.StatusInternalServerError, 5004, "failed to read SDK config")
			return
		}
		val, _ := config["logs-max-total-size-mb"]
		if val == nil {
			val = 100
		}
		Success(c, gin.H{
			"logs-max-total-size-mb": val,
			"logsMaxTotalSizeMb":     val,
		})
	}
}

func sdkMgmtCamelToHyphen(s string) string {
	if s == "" {
		return s
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch >= 'A' && ch <= 'Z' {
			if i > 0 {
				out = append(out, '-')
			}
			out = append(out, ch+32) // to lower
		} else {
			out = append(out, ch)
		}
	}
	return string(out)
}

func sdkMgmtNormalizeConfigMap(data map[string]any) map[string]any {
	out := make(map[string]any, len(data))
	for k, v := range data {
		nk := sdkMgmtNormalizeConfigKey(k)
		if sub, ok := v.(map[string]any); ok {
			out[nk] = sdkMgmtNormalizeConfigMap(sub)
		} else {
			out[nk] = v
		}
	}
	return out
}

func sdkMgmtNormalizeConfigKey(key string) string {
	h := sdkMgmtCamelToHyphen(key)
	if h != key {
		return h
	}
	return strings.ReplaceAll(key, "_", "-")
}

// Hyphen-to-camel state machine: "proxy-url" → "proxyUrl"
func sdkMgmtHyphenToCamel(s string) string {
	var out []byte
	upper := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '-' {
			upper = true
		} else if upper {
			if ch >= 'a' && ch <= 'z' {
				out = append(out, ch-32)
			} else {
				out = append(out, ch)
			}
			upper = false
		} else {
			out = append(out, ch)
		}
	}
	return string(out)
}

// Expands hyphenated keys with camelCase and snake_case aliases so frontend
// configValue() finds values regardless of stored format.
func sdkMgmtExpandConfigAliases(config map[string]any) map[string]any {
	if len(config) == 0 {
		return config
	}
	result := make(map[string]any, len(config)*2)
	for k, v := range config {
		result[k] = v
		if strings.Contains(k, "-") {
			camel := sdkMgmtHyphenToCamel(k)
			if camel != k {
				result[camel] = v
			}
			snake := strings.ReplaceAll(k, "-", "_")
			if snake != k {
				result[snake] = v
			}
		}
	}
	return result
}

func SDKMgmtSDKConfigPatchHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var incoming map[string]any
	if err := c.ShouldBindJSON(&incoming); err != nil {
		Error(c, http.StatusBadRequest, 4000, "invalid JSON body")
		return
	}
	normalized := sdkMgmtNormalizeConfigMap(incoming)
	config, err := sdkMgmtReadConfig()
	if err != nil {
		Error(c, http.StatusInternalServerError, 5004, "failed to read SDK config")
		return
	}
	maps.Copy(config, normalized)
	if err := sdkMgmtWriteConfig(config); err != nil {
		Error(c, http.StatusInternalServerError, 5005, "failed to save SDK config")
		return
	}
	Success(c, gin.H{"message": "updated"})
}

// ── Logs ──

func SDKMgmtLogsHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	limit := 50
	if l, err := strconv.Atoi(c.DefaultQuery("limit", "50")); err == nil && l > 0 {
		limit = min(l, 200)
	}
	level := c.Query("level")

	q := GlobalDB.Model(&model.UsageLog{}).Order("created_at DESC").Limit(limit)
	switch level {
	case "error":
		q = q.Where("failed = ?", true)
	case "info":
		q = q.Where("failed = ?", false)
	}

	var logs []model.UsageLog
	if err := q.Find(&logs).Error; err != nil {
		Error(c, http.StatusInternalServerError, 5006, "failed to query usage logs")
		return
	}

	items := make([]gin.H, 0, len(logs))
	for _, l := range logs {
		entryLevel := "info"
		if l.Failed {
			entryLevel = "error"
		}
		items = append(items, gin.H{
			"id":          l.ID,
			"request_id":  l.RequestID,
			"model":       l.Model,
			"provider":    l.Provider,
			"tokens_in":   l.TokensIn,
			"tokens_out":  l.TokensOut,
			"total_cost":  l.TotalCost,
			"duration_ms": l.DurationMs,
			"failed":      l.Failed,
			"level":       entryLevel,
			"ip_address":  l.IPAddress,
			"created_at":  l.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	Success(c, gin.H{"logs": items})
}

func SDKMgmtLogsDeleteHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	Success(c, gin.H{"message": "logs clear not supported on UsageLog-backed endpoint"})
}

func SDKMgmtRequestErrorLogsHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	limit := 50
	if l, err := strconv.Atoi(c.DefaultQuery("limit", "50")); err == nil && l > 0 {
		limit = min(l, 200)
	}

	var logs []model.UsageLog
	if err := GlobalDB.Model(&model.UsageLog{}).Where("failed = ?", true).
		Order("created_at DESC").Limit(limit).Find(&logs).Error; err != nil {
		Error(c, http.StatusInternalServerError, 5006, "failed to query error logs")
		return
	}

	items := make([]gin.H, 0, len(logs))
	for _, l := range logs {
		items = append(items, gin.H{
			"id":          l.ID,
			"request_id":  l.RequestID,
			"model":       l.Model,
			"provider":    l.Provider,
			"tokens_in":   l.TokensIn,
			"tokens_out":  l.TokensOut,
			"total_cost":  l.TotalCost,
			"duration_ms": l.DurationMs,
			"failed":      true,
			"level":       "error",
			"ip_address":  l.IPAddress,
			"created_at":  l.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	Success(c, gin.H{"logs": items})
}

func SDKMgmtRequestErrorLogsDeleteHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	Success(c, gin.H{"message": "error logs clear not supported on UsageLog-backed endpoint"})
}

// ── Model Definitions ──

func SDKMgmtModelDefinitionsHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	channel := c.Param("channel")

	// Attempt to read catalog entries from model_catalog_entries table
	var catalog []model.ModelCatalogEntry
	GlobalDB.Where("channel_key = ?", channel).Find(&catalog)

	var models []gin.H
	if len(catalog) > 0 {
		for _, entry := range catalog {
			models = append(models, gin.H{
				"id":         entry.ModelID,
				"model":      entry.ModelID,
				"provider":   channel,
				"name":       entry.ModelID,
				"visible":    entry.Visible,
				"models_url": entry.ModelsURL,
			})
		}
	} else {
		// Fallback to static model lists for known providers
		switch channel {
		case "openai":
			models = sdkMgmtStaticModels("openai", []string{
				"gpt-4o", "gpt-4o-mini", "gpt-4-turbo", "gpt-4", "gpt-3.5-turbo",
				"o1", "o1-mini", "o3-mini",
			})
		case "claude":
			models = sdkMgmtStaticModels("claude", []string{
				"claude-sonnet-4-20250514", "claude-sonnet-4", "claude-3-opus-latest",
				"claude-3-sonnet-latest", "claude-3-haiku-latest",
				"claude-3-5-sonnet-latest", "claude-3-5-haiku-latest",
			})
		case "gemini":
			models = sdkMgmtStaticModels("gemini", []string{
				"gemini-2.5-pro-exp-03-25", "gemini-2.0-flash", "gemini-2.0-flash-lite",
				"gemini-1.5-pro", "gemini-1.5-flash",
			})
		case "codex":
			models = sdkMgmtStaticModels("codex", []string{
				"o1", "o1-mini", "o3-mini", "gpt-4o", "gpt-4o-mini",
			})
		case "vertex":
			models = sdkMgmtStaticModels("vertex", []string{
				"claude-sonnet-4-20250514", "claude-3-opus-latest", "claude-3-sonnet-latest",
				"claude-3-haiku-latest", "gemini-2.0-flash", "gemini-1.5-pro",
			})
		default:
			Error(c, http.StatusNotFound, 4040, "unknown channel: "+channel)
			return
		}
	}
	Success(c, gin.H{"models": models})
}

func sdkMgmtStaticModels(provider string, modelIDs []string) []gin.H {
	items := make([]gin.H, 0, len(modelIDs))
	for _, id := range modelIDs {
		items = append(items, gin.H{
			"id":       id,
			"model":    id,
			"provider": provider,
			"name":     id,
		})
	}
	return items
}
