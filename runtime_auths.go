package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/xxww0098/cpa-gateway/executor"
)

// providerConfigFromSDK translates a root-level SDKProviderConfig into the
// executor.ProviderConfig struct expected by the migrated executor package.
func providerConfigFromSDK(cfg SDKProviderConfig) executor.ProviderConfig {
	return executor.ProviderConfig{
		BaseURL: cfg.BaseURL,
		APIKey:  cfg.APIKey,
		Enabled: cfg.Enabled,
	}
}

// registerRuntimeAuths wires the five provider executors onto mgr and
// registers a runtime-only cliproxyauth.Auth for each credential supplied
// via CPA-Gateway configuration.
//
// This is the non-gin-routing subset of the legacy InitSDK: executors are
// attached to the auth manager so downstream SDK handlers can execute
// against CPA-Gateway-configured upstreams, but no HTTP routes are mounted
// here (the SDK Builder owns that surface now).
//
// The manager is mutated in place. Errors from executor construction or
// auth registration are returned immediately without attempting to roll
// back earlier registrations — main.go treats the failure as fatal.
func registerRuntimeAuths(mgr *cliproxyauth.Manager, cfg *Config) error {
	if mgr == nil {
		return fmt.Errorf("auth manager is required")
	}
	if cfg == nil {
		return fmt.Errorf("config is required")
	}

	// OpenAI-compatible upstream.
	openAIConfig := cfg.SDK.OpenAIProviderConfig()
	if openAIConfig.Complete() {
		exec, err := executor.NewOpenAICompatibleExecutor(providerConfigFromSDK(openAIConfig), cfg.SDK.TimeoutSeconds)
		if err != nil {
			return err
		}
		mgr.RegisterExecutor(exec)
		now := time.Now().UTC()
		if _, err := mgr.Register(context.Background(), &cliproxyauth.Auth{
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
		}); err != nil {
			return fmt.Errorf("registering OpenAI runtime auth: %w", err)
		}
	} else {
		slog.Warn("CLIProxyAPI SDK OpenAI-compatible proxy disabled: sdk.openai/openai_compatible or legacy sdk.base_url/api_key is missing")
	}

	// Claude upstream.
	claudeExec, err := executor.NewClaudeExecutor(providerConfigFromSDK(cfg.SDK.Claude), cfg.SDK.TimeoutSeconds)
	if err != nil {
		return err
	}
	mgr.RegisterExecutor(claudeExec)
	if cfg.SDK.Claude.Complete() {
		now := time.Now().UTC()
		if _, err := mgr.Register(context.Background(), &cliproxyauth.Auth{
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
		}); err != nil {
			return fmt.Errorf("registering Claude runtime auth: %w", err)
		}
	} else {
		slog.Info("CLIProxyAPI SDK Claude executor registered without config credential; persisted claude auths may still be used")
	}

	// Gemini upstream.
	geminiExec, err := executor.NewGeminiExecutor(providerConfigFromSDK(cfg.SDK.Gemini), cfg.SDK.TimeoutSeconds)
	if err != nil {
		return err
	}
	mgr.RegisterExecutor(geminiExec)
	if cfg.SDK.Gemini.Complete() {
		now := time.Now().UTC()
		if _, err := mgr.Register(context.Background(), &cliproxyauth.Auth{
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
		}); err != nil {
			return fmt.Errorf("registering Gemini runtime auth: %w", err)
		}
	} else {
		slog.Info("CLIProxyAPI SDK Gemini executor registered without config credential; persisted gemini auths may still be used")
	}

	// Codex upstream (config access token optional).
	codexExec, err := executor.NewCodexExecutor(providerConfigFromSDK(cfg.SDK.Codex), cfg.SDK.TimeoutSeconds)
	if err != nil {
		return err
	}
	mgr.RegisterExecutor(codexExec)
	if cfg.SDK.Codex.Configured() && strings.TrimSpace(cfg.SDK.Codex.APIKey) != "" {
		now := time.Now().UTC()
		if _, err := mgr.Register(context.Background(), &cliproxyauth.Auth{
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
		}); err != nil {
			return fmt.Errorf("registering Codex runtime auth: %w", err)
		}
	} else {
		slog.Info("CLIProxyAPI SDK Codex executor registered without config access token; persisted codex auths may still be used")
	}

	// Vertex upstream.
	vertexExec, err := executor.NewVertexExecutor(providerConfigFromSDK(cfg.SDK.Vertex), cfg.SDK.TimeoutSeconds)
	if err != nil {
		return err
	}
	mgr.RegisterExecutor(vertexExec)
	if cfg.SDK.Vertex.Configured() && strings.TrimSpace(cfg.SDK.Vertex.APIKey) != "" {
		now := time.Now().UTC()
		if _, err := mgr.Register(context.Background(), &cliproxyauth.Auth{
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
		}); err != nil {
			return fmt.Errorf("registering Vertex runtime auth: %w", err)
		}
	} else {
		slog.Info("CLIProxyAPI SDK Vertex executor registered without config service account; persisted vertex auths may still be used")
	}

	return nil
}
