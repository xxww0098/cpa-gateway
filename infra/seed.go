package infra

import (
	"encoding/json"
	"log/slog"

	"github.com/xxww0098/cpa-gateway/config"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// EnsureSDKManagementSeeds creates initial SDK management records if they
// don't already exist (provider_config for sdk_config, default ampcode_config).
func EnsureSDKManagementSeeds(db *gorm.DB, cfg *config.Config) error {
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

func buildSDKConfigData(sdk config.SDKConfig) (json.RawMessage, error) {
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

// ─────────────────────────────────────────────────────────────────────────────
// Model price seeds
//
// NOTE: 以下价格为占位值，生产环境需按各家厂商官方定价对齐后再启用。
// 单位：USD per 1M tokens。调用方应以幂等方式写入：已存在的 ModelID
// 记录不会被覆盖（OnConflict DoNothing），以便运维手动维护生产价目。
// ─────────────────────────────────────────────────────────────────────────────

// SeedModelPrices 幂等预置常用模型的占位价格。若对应 ModelID 已存在，
// 则保留现有记录，不做覆盖。
//
// Seeded model IDs (12 entries; placeholder prices):
//
//	gpt-4o
//	gpt-4o-mini
//	o3
//	o3-mini
//	o4-mini
//	claude-sonnet-4-20250514
//	claude-opus-4-20250514
//	claude-haiku-3-5-20241022
//	gemini-2.5-pro
//	gemini-2.5-flash
//	codex-mini
//	vertex-gemini-2.5-pro
func SeedModelPrices(db *gorm.DB) error {
	if db == nil {
		return nil
	}

	prices := []model.ModelPrice{
		// OpenAI
		{ModelID: "gpt-4o", InputPricePer1M: 2.50, OutputPricePer1M: 10.00, CachedInputPricePer1M: 1.25, ReasoningPricePer1M: 0},
		{ModelID: "gpt-4o-mini", InputPricePer1M: 0.15, OutputPricePer1M: 0.60, CachedInputPricePer1M: 0.075, ReasoningPricePer1M: 0},
		{ModelID: "o3", InputPricePer1M: 10.00, OutputPricePer1M: 40.00, CachedInputPricePer1M: 2.50, ReasoningPricePer1M: 60.00},
		{ModelID: "o3-mini", InputPricePer1M: 1.10, OutputPricePer1M: 4.40, CachedInputPricePer1M: 0.55, ReasoningPricePer1M: 4.40},
		{ModelID: "o4-mini", InputPricePer1M: 1.10, OutputPricePer1M: 4.40, CachedInputPricePer1M: 0.55, ReasoningPricePer1M: 4.40},
		// Anthropic Claude
		{ModelID: "claude-sonnet-4-20250514", InputPricePer1M: 3.00, OutputPricePer1M: 15.00, CachedInputPricePer1M: 0.30, ReasoningPricePer1M: 0},
		{ModelID: "claude-opus-4-20250514", InputPricePer1M: 15.00, OutputPricePer1M: 75.00, CachedInputPricePer1M: 1.50, ReasoningPricePer1M: 0},
		{ModelID: "claude-haiku-3-5-20241022", InputPricePer1M: 0.80, OutputPricePer1M: 4.00, CachedInputPricePer1M: 0.08, ReasoningPricePer1M: 0},
		// Google Gemini
		{ModelID: "gemini-2.5-pro", InputPricePer1M: 1.25, OutputPricePer1M: 10.00, CachedInputPricePer1M: 0.3125, ReasoningPricePer1M: 0},
		{ModelID: "gemini-2.5-flash", InputPricePer1M: 0.15, OutputPricePer1M: 0.60, CachedInputPricePer1M: 0.0375, ReasoningPricePer1M: 0.35},
		// Codex
		{ModelID: "codex-mini", InputPricePer1M: 1.50, OutputPricePer1M: 6.00, CachedInputPricePer1M: 0.375, ReasoningPricePer1M: 0},
		// Vertex AI
		{ModelID: "vertex-gemini-2.5-pro", InputPricePer1M: 1.25, OutputPricePer1M: 10.00, CachedInputPricePer1M: 0.3125, ReasoningPricePer1M: 0},
	}

	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "model_id"}},
		DoNothing: true,
	}).Create(&prices).Error; err != nil {
		return err
	}

	slog.Info("seeded model prices (placeholder values; align with vendor pricing before production)", "count", len(prices))
	return nil
}
