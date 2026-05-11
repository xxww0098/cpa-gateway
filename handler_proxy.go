package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/xxww0098/cpa-gateway/executor"
	"github.com/xxww0098/cpa-gateway/model"
)

const (
	proxyProviderOpenAI      = executor.ProviderOpenAI
	proxyProviderClaude      = executor.ProviderClaude
	proxyProviderGemini      = executor.ProviderGemini
	proxyProviderCodex       = executor.ProviderCodex
	proxyProviderVertex      = executor.ProviderVertex
	proxyMaxBodyBytes        = 8 << 20
	proxyErrorInvalidRequest = "invalid_request_error"
	proxyErrorServer         = "server_error"
	proxyErrorUpstream       = "upstream_error"
)

// authManager is the SDK cliproxy execution manager used by CPA-Gateway-owned Gin handlers.
var authManager *cliproxyauth.Manager

type openAIChatCompletionRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// providerConfigFromSDK translates a root-level SDKProviderConfig into the
// executor.ProviderConfig struct expected by the migrated executor package.
func providerConfigFromSDK(cfg SDKProviderConfig) executor.ProviderConfig {
	return executor.ProviderConfig{
		BaseURL: cfg.BaseURL,
		APIKey:  cfg.APIKey,
		Enabled: cfg.Enabled,
	}
}

// InitSDK prepares the SDK execution manager as a pure library dependency.
// It does not build, run, or register any SDK HTTP routes.
// The SDK Manager uses GlobalStore for PostgreSQL-backed auth records. CPA-Gateway's
// OpenAI-compatible upstream credential still comes from process config as a runtime-only
// auth, and no SDK HTTP lifecycle APIs are invoked.
func InitSDK(cfg *Config) error {
	if cfg == nil {
		authManager = nil
		return fmt.Errorf("config is required")
	}

	manager := cliproxyauth.NewManager(GlobalStore, &cliproxyauth.RoundRobinSelector{}, cliproxyauth.NoopHook{})
	if err := manager.Load(context.Background()); err != nil {
		authManager = nil
		return fmt.Errorf("loading SDK auth store: %w", err)
	}

	openAIConfig := cfg.SDK.openAIProviderConfig()
	if openAIConfig.complete() {
		exec, err := executor.NewOpenAICompatibleExecutor(providerConfigFromSDK(openAIConfig), cfg.SDK.TimeoutSeconds)
		if err != nil {
			authManager = nil
			return err
		}
		manager.RegisterExecutor(exec)
		now := time.Now().UTC()
		_, err = manager.Register(context.Background(), &cliproxyauth.Auth{
			ID:        "cpa-gateway-openai-compatible",
			Provider:  exec.Identifier(),
			Label:     "CPA-Gateway OpenAI-compatible upstream",
			Status:    cliproxyauth.StatusActive,
			CreatedAt: now,
			UpdatedAt: now,
			Attributes: map[string]string{
				"runtime_only": "true",
				"source":       "cpa-gateway-config",
				"base_url":     exec.BaseURL,
			},
		})
		if err != nil {
			authManager = nil
			return fmt.Errorf("registering SDK auth: %w", err)
		}
	} else {
		slog.Warn("CLIProxyAPI SDK OpenAI-compatible proxy disabled: sdk.openai/openai_compatible or legacy sdk.base_url/api_key is missing")
	}

	claudeConfig := cfg.SDK.Claude
	claudeExec, err := executor.NewClaudeExecutor(providerConfigFromSDK(claudeConfig), cfg.SDK.TimeoutSeconds)
	if err != nil {
		authManager = nil
		return err
	}
	manager.RegisterExecutor(claudeExec)
	if claudeConfig.complete() {
		now := time.Now().UTC()
		_, err = manager.Register(context.Background(), &cliproxyauth.Auth{
			ID:        "cpa-gateway-claude",
			Provider:  claudeExec.Identifier(),
			Label:     "CPA-Gateway Claude upstream",
			Status:    cliproxyauth.StatusActive,
			CreatedAt: now,
			UpdatedAt: now,
			Attributes: map[string]string{
				"runtime_only": "true",
				"source":       "cpa-gateway-config",
				"base_url":     claudeExec.BaseURL(),
			},
		})
		if err != nil {
			authManager = nil
			return fmt.Errorf("registering Claude SDK auth: %w", err)
		}
	} else {
		slog.Info("CLIProxyAPI SDK Claude executor registered without config credential; persisted claude auths may still be used")
	}

	geminiConfig := cfg.SDK.Gemini
	geminiExec, err := executor.NewGeminiExecutor(providerConfigFromSDK(geminiConfig), cfg.SDK.TimeoutSeconds)
	if err != nil {
		authManager = nil
		return err
	}
	manager.RegisterExecutor(geminiExec)
	if geminiConfig.complete() {
		now := time.Now().UTC()
		_, err = manager.Register(context.Background(), &cliproxyauth.Auth{
			ID:        "cpa-gateway-gemini",
			Provider:  geminiExec.Identifier(),
			Label:     "CPA-Gateway Gemini upstream",
			Status:    cliproxyauth.StatusActive,
			CreatedAt: now,
			UpdatedAt: now,
			Attributes: map[string]string{
				"runtime_only": "true",
				"source":       "cpa-gateway-config",
				"base_url":     geminiExec.BaseURL(),
			},
		})
		if err != nil {
			authManager = nil
			return fmt.Errorf("registering Gemini SDK auth: %w", err)
		}
	} else {
		slog.Info("CLIProxyAPI SDK Gemini executor registered without config credential; persisted gemini auths may still be used")
	}

	codexConfig := cfg.SDK.Codex
	codexExec, err := executor.NewCodexExecutor(providerConfigFromSDK(codexConfig), cfg.SDK.TimeoutSeconds)
	if err != nil {
		authManager = nil
		return err
	}
	manager.RegisterExecutor(codexExec)
	if codexConfig.configured() && strings.TrimSpace(codexConfig.APIKey) != "" {
		now := time.Now().UTC()
		_, err = manager.Register(context.Background(), &cliproxyauth.Auth{
			ID:        "cpa-gateway-codex",
			Provider:  codexExec.Identifier(),
			Label:     "CPA-Gateway Codex upstream",
			Status:    cliproxyauth.StatusActive,
			CreatedAt: now,
			UpdatedAt: now,
			Attributes: map[string]string{
				"runtime_only": "true",
				"source":       "cpa-gateway-config",
				"base_url":     codexExec.BaseURL(),
			},
			Metadata: map[string]any{
				executor.CodexMetadataAccessToken: codexExec.AccessToken(),
			},
		})
		if err != nil {
			authManager = nil
			return fmt.Errorf("registering Codex SDK auth: %w", err)
		}
	} else {
		slog.Info("CLIProxyAPI SDK Codex executor registered without config access token; persisted codex auths may still be used")
	}

	vertexConfig := cfg.SDK.Vertex
	vertexExec, err := executor.NewVertexExecutor(providerConfigFromSDK(vertexConfig), cfg.SDK.TimeoutSeconds)
	if err != nil {
		authManager = nil
		return err
	}
	manager.RegisterExecutor(vertexExec)
	if vertexConfig.configured() && strings.TrimSpace(vertexConfig.APIKey) != "" {
		now := time.Now().UTC()
		_, err = manager.Register(context.Background(), &cliproxyauth.Auth{
			ID:        "cpa-gateway-vertex",
			Provider:  vertexExec.Identifier(),
			Label:     "CPA-Gateway Vertex upstream",
			Status:    cliproxyauth.StatusActive,
			CreatedAt: now,
			UpdatedAt: now,
			Attributes: map[string]string{
				"runtime_only": "true",
				"source":       "cpa-gateway-config",
				"base_url":     vertexExec.BaseURL(),
			},
			Metadata: map[string]any{
				executor.VertexMetadataServiceAccount: vertexExec.ServiceAccountJSON(),
			},
		})
		if err != nil {
			authManager = nil
			return fmt.Errorf("registering Vertex SDK auth: %w", err)
		}
	} else {
		slog.Info("CLIProxyAPI SDK Vertex executor registered without config service account; persisted vertex auths may still be used")
	}

	logPendingProviderExecutors(cfg.SDK)
	authManager = manager
	return nil
}

func logPendingProviderExecutors(cfg SDKConfig) {
	for provider, providerConfig := range cfg.pendingProviderConfigs() {
		if provider == proxyProviderClaude || provider == proxyProviderGemini || provider == proxyProviderCodex || provider == proxyProviderVertex {
			continue
		}
		if providerConfig.configured() {
			slog.Warn("CLIProxyAPI SDK provider configured but executor is pending implementation", "provider", provider)
		}
	}
}

// ProxyChatHandler handles POST /v1/chat/completions with CPA-Gateway billing and SDK execution.
func ProxyChatHandler(c *gin.Context) {
	bc, ok := requireProxyBillingCtx(c)
	if !ok {
		return
	}

	rawJSON, err := readAndRestoreProxyBody(c, proxyMaxBodyBytes)
	if err != nil {
		releaseProxyHold(c.Request.Context(), bc)
		writeOpenAIError(c, http.StatusBadRequest, proxyErrorInvalidRequest, "invalid_request_body", err.Error())
		return
	}

	var reqBody openAIChatCompletionRequest
	if err := json.Unmarshal(rawJSON, &reqBody); err != nil {
		releaseProxyHold(c.Request.Context(), bc)
		writeOpenAIError(c, http.StatusBadRequest, proxyErrorInvalidRequest, "invalid_json", "request body must be valid OpenAI-compatible JSON")
		return
	}

	modelName := strings.TrimSpace(reqBody.Model)
	if modelName == "" {
		releaseProxyHold(c.Request.Context(), bc)
		writeOpenAIError(c, http.StatusBadRequest, proxyErrorInvalidRequest, "missing_model", "request body must include a non-empty model")
		return
	}

	if authManager == nil {
		releaseProxyHold(c.Request.Context(), bc)
		writeOpenAIError(c, http.StatusServiceUnavailable, proxyErrorServer, "sdk_not_configured", "CLIProxyAPI SDK auth manager is not initialized")
		return
	}

	execReq, opts := buildProxyExecutionRequest(c, modelName, reqBody.Stream, rawJSON)
	if reqBody.Stream {
		executeProxyStream(c, bc, modelName, rawJSON, execReq, opts)
		return
	}
	executeProxyNonStream(c, bc, modelName, rawJSON, execReq, opts)
}

// ProxyModelsHandler handles GET /v1/models with an OpenAI-compatible model list response.
func ProxyModelsHandler(c *gin.Context) {
	if GlobalDB == nil {
		writeOpenAIError(c, http.StatusInternalServerError, proxyErrorServer, "database_unavailable", "database not initialized")
		return
	}
	ids, err := visibleCatalogModelIDsSorted(c.Request.Context())
	if err != nil {
		slog.Error("failed to list visible catalog models", "error", err)
		writeOpenAIError(c, http.StatusInternalServerError, proxyErrorServer, "catalog_load_failed", "failed to load model catalog")
		return
	}
	now := time.Now().Unix()
	models := make([]gin.H, 0, len(ids))
	for _, id := range ids {
		models = append(models, gin.H{"id": id, "object": "model", "created": now, "owned_by": "cpa-gateway"})
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": models})
}

func executeProxyNonStream(c *gin.Context, bc *BillingCtx, modelName string, rawJSON []byte, execReq cliproxyexecutor.Request, opts cliproxyexecutor.Options) {
	started := time.Now()
	providers := proxyProvidersForModel(modelName)
	provider := providers[0]
	resp, err := authManager.Execute(c.Request.Context(), providers, execReq, opts)
	if err != nil {
		releaseProxyHold(c.Request.Context(), bc)
		writeOpenAIError(c, statusCodeFromError(err), proxyErrorUpstream, "sdk_execute_failed", err.Error())
		return
	}

	if err := settleAndLogProxyUsage(c.Request.Context(), bc, provider, modelName, rawJSON, resp.Payload, len(resp.Payload), false, time.Since(started)); err != nil {
		releaseProxyHold(c.Request.Context(), bc)
		slog.Error("failed to settle proxy usage", "request_id", bc.RequestID, "user_id", bc.UserID, "error", err)
		writeOpenAIError(c, http.StatusInternalServerError, proxyErrorServer, "billing_settlement_failed", "failed to settle proxy usage")
		return
	}

	writeUpstreamHeaders(c, resp.Headers, "application/json")
	c.Data(http.StatusOK, contentTypeOrDefault(resp.Headers, "application/json"), resp.Payload)
}

func executeProxyStream(c *gin.Context, bc *BillingCtx, modelName string, rawJSON []byte, execReq cliproxyexecutor.Request, opts cliproxyexecutor.Options) {
	started := time.Now()
	providers := proxyProvidersForModel(modelName)
	provider := providers[0]
	result, err := authManager.ExecuteStream(c.Request.Context(), providers, execReq, opts)
	if err != nil {
		releaseProxyHold(c.Request.Context(), bc)
		writeOpenAIError(c, statusCodeFromError(err), proxyErrorUpstream, "sdk_stream_failed", err.Error())
		return
	}
	if result == nil || result.Chunks == nil {
		releaseProxyHold(c.Request.Context(), bc)
		writeOpenAIError(c, http.StatusBadGateway, proxyErrorUpstream, "sdk_stream_empty", "SDK returned an empty stream")
		return
	}

	writeUpstreamHeaders(c, result.Headers, "text/event-stream")
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Status(http.StatusOK)

	outputBytes := 0
	var streamErr error
	c.Stream(func(w io.Writer) bool {
		select {
		case <-c.Request.Context().Done():
			streamErr = c.Request.Context().Err()
			return false
		case chunk, ok := <-result.Chunks:
			if !ok {
				return false
			}
			if chunk.Err != nil {
				streamErr = chunk.Err
				writeProxySSEError(w, chunk.Err)
				return false
			}
			if len(chunk.Payload) == 0 {
				return true
			}
			if _, err := w.Write(chunk.Payload); err != nil {
				streamErr = err
				return false
			}
			outputBytes += len(chunk.Payload)
			return true
		}
	})

	if streamErr != nil {
		releaseProxyHold(c.Request.Context(), bc)
		slog.Warn("proxy stream failed", "request_id", bc.RequestID, "user_id", bc.UserID, "error", streamErr)
		return
	}

	if err := settleAndLogProxyUsage(c.Request.Context(), bc, provider, modelName, rawJSON, nil, outputBytes, true, time.Since(started)); err != nil {
		releaseProxyHold(c.Request.Context(), bc)
		slog.Error("failed to settle streaming proxy usage", "request_id", bc.RequestID, "user_id", bc.UserID, "error", err)
	}
}

func buildProxyExecutionRequest(c *gin.Context, modelName string, stream bool, rawJSON []byte) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	metadata := map[string]any{
		cliproxyexecutor.RequestedModelMetadataKey: modelName,
		cliproxyexecutor.RequestPathMetadataKey:    c.Request.URL.Path,
		"cpa-gateway_trace_id":                     traceIDFromGin(c),
	}

	return cliproxyexecutor.Request{
			Model:    modelName,
			Payload:  rawJSON,
			Format:   sdktranslator.FormatOpenAI,
			Metadata: metadata,
		}, cliproxyexecutor.Options{
			Stream:          stream,
			Headers:         executor.SanitizedProxyHeaders(c.Request.Header),
			Query:           cloneURLValues(c.Request.URL.Query()),
			OriginalRequest: rawJSON,
			SourceFormat:    sdktranslator.FormatOpenAI,
			Metadata:        metadata,
		}
}

func proxyProvidersForModel(modelName string) []string {
	return []string{proxyProviderForModel(modelName)}

}

func proxyProviderForModel(modelName string) string {
	normalized := strings.ToLower(strings.TrimSpace(modelName))
	if strings.HasPrefix(normalized, proxyProviderClaude) || strings.Contains(normalized, proxyProviderClaude+"-") {
		return proxyProviderClaude
	}
	if strings.HasPrefix(normalized, proxyProviderGemini) || strings.Contains(normalized, proxyProviderGemini+"-") {
		return proxyProviderGemini
	}
	if strings.HasPrefix(normalized, "gpt-5-codex") || strings.HasPrefix(normalized, "gpt-5.3-codex") || strings.Contains(normalized, proxyProviderCodex) {
		return proxyProviderCodex
	}
	if strings.HasPrefix(normalized, proxyProviderVertex+"/") || strings.HasPrefix(normalized, proxyProviderVertex+":") || strings.Contains(normalized, proxyProviderVertex) {
		return proxyProviderVertex
	}
	return proxyProviderOpenAI
}

func requireProxyBillingCtx(c *gin.Context) (*BillingCtx, bool) {
	bc, ok := billingContextFromGin(c)
	if !ok || bc == nil || bc.UserID == 0 {
		writeOpenAIError(c, http.StatusUnauthorized, proxyErrorInvalidRequest, "authentication_required", "authentication context required")
		return nil, false
	}
	if GlobalDB == nil {
		releaseProxyHold(c.Request.Context(), bc)
		writeOpenAIError(c, http.StatusInternalServerError, proxyErrorServer, "database_not_initialized", "database not initialized")
		return nil, false
	}
	if GlobalLedger == nil {
		writeOpenAIError(c, http.StatusInternalServerError, proxyErrorServer, "ledger_not_initialized", "billing ledger not initialized")
		return nil, false
	}
	return bc, true
}

func readAndRestoreProxyBody(c *gin.Context, limit int64) ([]byte, error) {
	if c.Request.Body == nil {
		return nil, fmt.Errorf("request body is required")
	}
	reader := io.LimitReader(c.Request.Body, limit+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	_ = c.Request.Body.Close()
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("request body exceeds %d bytes", limit)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, fmt.Errorf("request body is required")
	}
	return body, nil
}

func releaseProxyHold(ctx context.Context, bc *BillingCtx) {
	if bc == nil || bc.UserID == 0 || bc.RequestID == "" || GlobalLedger == nil {
		return
	}
	if err := GlobalLedger.Release(ctx, bc.UserID, bc.RequestID); err != nil {
		slog.Warn("failed to release proxy billing hold", "request_id", bc.RequestID, "user_id", bc.UserID, "error", err)
	}
}

func settleAndLogProxyUsage(ctx context.Context, bc *BillingCtx, provider string, modelName string, requestPayload []byte, responsePayload []byte, responseBytes int, stream bool, duration time.Duration) error {
	if bc == nil || bc.UserID == 0 {
		return fmt.Errorf("billing context required")
	}
	if GlobalLedger == nil {
		return fmt.Errorf("billing ledger not initialized")
	}
	if GlobalDB == nil {
		return fmt.Errorf("database not initialized")
	}

	tokensIn, tokensOut, inputCost, outputCost, totalCost, actualCost, rateMult := calculateProxyUsage(bc, requestPayload, responsePayload, responseBytes)
	if err := GlobalLedger.Settle(ctx, bc.UserID, bc.RequestID, actualCost); err != nil {
		return err
	}
	if strings.TrimSpace(provider) == "" {
		provider = proxyProviderOpenAI
	}

	return GlobalDB.WithContext(ctx).Create(&model.UsageLog{
		UserID:         bc.UserID,
		ApiKeyID:       bc.ApiKeyID,
		GroupID:        bc.GroupID,
		RequestID:      bc.RequestID,
		Model:          modelName,
		Provider:       provider,
		TokensIn:       tokensIn,
		TokensOut:      tokensOut,
		InputTokens:    tokensIn,
		OutputTokens:   tokensOut,
		InputCost:      inputCost,
		OutputCost:     outputCost,
		TotalCost:      totalCost,
		ActualCost:     actualCost,
		Cost:           actualCost,
		RateMultiplier: rateMult,
		Stream:         stream,
		DurationMs:     duration.Milliseconds(),
		Failed:         false,
	}).Error
}

func calculateProxyUsage(bc *BillingCtx, requestPayload []byte, responsePayload []byte, responseBytes int) (int, int, float64, float64, float64, float64, float64) {
	tokensIn, tokensOut, ok := openAIUsageTokens(responsePayload)
	if !ok {
		tokensIn = executor.ApproximateTokensFromBytes(len(requestPayload))
		tokensOut = executor.ApproximateTokensFromBytes(responseBytes)
	}

	price := 0.0
	if GlobalConfig != nil {
		price = GlobalConfig.Billing.DefaultPricePer1KTokens
	}
	rateMult := 1.0
	if bc != nil && bc.RateMult > 0 {
		rateMult = bc.RateMult
	}
	inputCost := (float64(tokensIn) / 1000.0) * price
	outputCost := (float64(tokensOut) / 1000.0) * price
	totalCost := inputCost + outputCost
	actualCost := totalCost * rateMult
	return tokensIn, tokensOut, inputCost, outputCost, totalCost, actualCost, rateMult
}

func openAIUsageTokens(payload []byte) (int, int, bool) {
	if len(payload) == 0 {
		return 0, 0, false
	}
	var envelope struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Usage == nil {
		return 0, 0, false
	}
	in := maxInt(envelope.Usage.PromptTokens, 0)
	out := maxInt(envelope.Usage.CompletionTokens, 0)
	if in == 0 && out == 0 && envelope.Usage.TotalTokens > 0 {
		out = envelope.Usage.TotalTokens
	}
	return in, out, in > 0 || out > 0 || envelope.Usage.TotalTokens > 0
}

func writeUpstreamHeaders(c *gin.Context, headers http.Header, defaultContentType string) {
	for key, vals := range headers {
		if shouldSkipProxyHeader(key) || strings.EqualFold(key, "Content-Encoding") {
			continue
		}
		for _, val := range vals {
			c.Writer.Header().Add(key, val)
		}
	}
	if c.Writer.Header().Get("Content-Type") == "" && defaultContentType != "" {
		c.Header("Content-Type", defaultContentType)
	}
}

func shouldSkipProxyHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization", "connection", "content-length", "host", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func contentTypeOrDefault(headers http.Header, fallback string) string {
	if value := strings.TrimSpace(headers.Get("Content-Type")); value != "" {
		return value
	}
	return fallback
}

func cloneURLValues(values url.Values) url.Values {
	cloned := make(url.Values, len(values))
	for key, vals := range values {
		cloned[key] = append([]string(nil), vals...)
	}
	return cloned
}

func statusCodeFromError(err error) int {
	type statusCoder interface{ StatusCode() int }
	for current := err; current != nil; current = errors.Unwrap(current) {
		if status, ok := current.(statusCoder); ok {
			code := status.StatusCode()
			if code >= http.StatusBadRequest && code <= http.StatusNetworkAuthenticationRequired {
				return code
			}
		}
	}
	return http.StatusBadGateway
}

func writeOpenAIError(c *gin.Context, status int, typ string, code string, message string) {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if typ == "" {
		typ = proxyErrorServer
	}
	if message == "" {
		message = http.StatusText(status)
	}
	c.JSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    typ,
			"code":    code,
		},
	})
}

func writeProxySSEError(w io.Writer, err error) {
	payload, _ := json.Marshal(gin.H{
		"error": gin.H{
			"message": err.Error(),
			"type":    proxyErrorUpstream,
			"code":    "sdk_stream_failed",
		},
	})
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
