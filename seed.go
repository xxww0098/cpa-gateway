package main

import (
	"encoding/json"
	"log/slog"

	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/gorm"
)

func EnsureSDKManagementSeeds(db *gorm.DB, cfg *Config) error {
	if db == nil {
		return nil
	}

	var providerCount int64
	if err := db.Model(&model.ProviderConfig{}).
		Where("provider = ?", "sdk_config").
		Count(&providerCount).Error; err != nil {
		return err
	}

	if providerCount == 0 {
		data, err := buildSDKConfigData(cfg.SDK)
		if err != nil {
			return err
		}
		pc := model.ProviderConfig{
			Provider:   "sdk_config",
			ConfigData: data,
		}
		if err := db.Create(&pc).Error; err != nil {
			return err
		}
		slog.Info("seeded sdk_config provider config")
	}

	var ampcodeCount int64
	if err := db.Model(&model.AmpcodeConfig{}).Count(&ampcodeCount).Error; err != nil {
		return err
	}
	if ampcodeCount == 0 {
		ac := model.AmpcodeConfig{
			ID:         1,
			ConfigData: json.RawMessage("{}"),
		}
		if err := db.Create(&ac).Error; err != nil {
			return err
		}
		slog.Info("seeded default ampcode config")
	}

	// AuthRecord seeding is intentionally skipped: config-backed credentials
	// (OpenAI-compatible, Claude, Gemini, Codex, Vertex) are registered as
	// runtime-only auths by InitSDK with runtime_only=true.  PostgresAuthStore.Save
	// skips runtime-only auths, so persisting them here would create records
	// overwritten by InitSDK's manager.Register on next restart.
	// Credentials added via SDK management API handlers are persisted via
	// GlobalStore.Save() at creation time and don't need a startup seed.
	slog.Info("auth record seeding skipped: config-backed credentials are runtime-only registrations by InitSDK")

	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

type sdkProviderConfigJSON struct {
	BaseURL string `json:"base_url"`
	Enabled bool   `json:"enabled"`
}

type sdkConfigJSON struct {
	BaseURL        string                           `json:"base_url"`
	TimeoutSeconds int                              `json:"timeout_seconds"`
	Providers      map[string]sdkProviderConfigJSON `json:"providers"`
}

func buildSDKConfigData(sdk SDKConfig) (json.RawMessage, error) {
	cfg := sdkConfigJSON{
		BaseURL:        sdk.BaseURL,
		TimeoutSeconds: sdk.TimeoutSeconds,
		Providers: map[string]sdkProviderConfigJSON{
			"openai": {
				BaseURL: sdk.OpenAI.BaseURL,
				Enabled: sdk.OpenAI.Enabled,
			},
			"openai_compatible": {
				BaseURL: sdk.OpenAICompatible.BaseURL,
				Enabled: sdk.OpenAICompatible.Enabled,
			},
			"claude": {
				BaseURL: sdk.Claude.BaseURL,
				Enabled: sdk.Claude.Enabled,
			},
			"gemini": {
				BaseURL: sdk.Gemini.BaseURL,
				Enabled: sdk.Gemini.Enabled,
			},
			"codex": {
				BaseURL: sdk.Codex.BaseURL,
				Enabled: sdk.Codex.Enabled,
			},
			"vertex": {
				BaseURL: sdk.Vertex.BaseURL,
				Enabled: sdk.Vertex.Enabled,
			},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}
