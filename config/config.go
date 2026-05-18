// Package config defines the gateway's YAML-driven configuration surface.
//
// It is deliberately dependency-light (standard library + yaml.v3) so that
// every other package (api/, sdk/, infra/, main) can import it without
// creating an import cycle with the main package.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds all configuration sections.
type Config struct {
	Server         ServerConfig         `yaml:"server"`
	Database       DatabaseConfig       `yaml:"database"`
	Redis          RedisConfig          `yaml:"redis"`
	SDK            SDKConfig            `yaml:"sdk"`
	Auth           AuthConfig           `yaml:"auth"`
	Billing        BillingConfig        `yaml:"billing"`
	RateLimit      RateLimitConfig      `yaml:"rate_limit"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// DatabaseConfig holds PostgreSQL connection settings.
type DatabaseConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
	SSLMode  string `yaml:"sslmode"`
}

// DSN returns the PostgreSQL connection string.
func (d DatabaseConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		d.Host, d.Port, d.User, d.Password, d.DBName, d.SSLMode,
	)
}

// RedisConfig holds Redis connection settings.
type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

// SDKConfig holds CLIProxyAPI SDK settings.
type SDKConfig struct {
	BaseURL          string            `yaml:"base_url"`
	APIKey           string            `yaml:"api_key"`
	TimeoutSeconds   int               `yaml:"timeout_seconds"`
	OpenAI           SDKProviderConfig `yaml:"openai"`
	OpenAICompatible SDKProviderConfig `yaml:"openai_compatible"`
	Claude           SDKProviderConfig `yaml:"claude"`
	Gemini           SDKProviderConfig `yaml:"gemini"`
	Codex            SDKProviderConfig `yaml:"codex"`
	Vertex           SDKProviderConfig `yaml:"vertex"`
}

// SDKProviderConfig holds provider-specific SDK upstream settings.
type SDKProviderConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	Enabled bool   `yaml:"enabled"`
}

// OpenAIProviderConfig returns the resolved OpenAI-compatible provider config,
// falling back to legacy top-level fields when nested configuration is absent.
func (s SDKConfig) OpenAIProviderConfig() SDKProviderConfig {
	provider := s.OpenAI
	if !provider.Configured() && s.OpenAICompatible.Configured() {
		provider = s.OpenAICompatible
	}

	if strings.TrimSpace(provider.BaseURL) == "" {
		provider.BaseURL = s.BaseURL
	}
	if strings.TrimSpace(provider.APIKey) == "" {
		provider.APIKey = s.APIKey
	}
	if !provider.Enabled && strings.TrimSpace(provider.BaseURL) != "" && strings.TrimSpace(provider.APIKey) != "" {
		provider.Enabled = true
	}

	return provider
}

// PendingProviderConfigs returns the non-OpenAI provider configs keyed by provider name.
func (s SDKConfig) PendingProviderConfigs() map[string]SDKProviderConfig {
	return map[string]SDKProviderConfig{
		"claude": s.Claude,
		"gemini": s.Gemini,
		"codex":  s.Codex,
		"vertex": s.Vertex,
	}
}

// Configured reports whether the provider has any meaningful value set.
func (p SDKProviderConfig) Configured() bool {
	return p.Enabled || strings.TrimSpace(p.BaseURL) != "" || strings.TrimSpace(p.APIKey) != ""
}

// Complete reports whether the provider has enough configuration to be used.
func (p SDKProviderConfig) Complete() bool {
	return p.Enabled && strings.TrimSpace(p.BaseURL) != "" && strings.TrimSpace(p.APIKey) != ""
}

// AuthConfig holds authentication settings.
type AuthConfig struct {
	JWT JWTConfig `yaml:"jwt"`
	// AdminEmails is retained for backward-compatible config parsing only.
	// Runtime admin authorization is stored in users.role and never granted by
	// matching an email claim or public registration input.
	AdminEmails []string `yaml:"admin_emails"`
}

// JWTConfig holds JWT-specific settings.
type JWTConfig struct {
	Secret      string `yaml:"secret"`
	ExpiryHours int    `yaml:"expiry_hours"`
}

// BillingConfig holds billing/precharge settings.
type BillingConfig struct {
	HoldAmount              int     `yaml:"hold_amount"`
	DefaultPricePer1KTokens float64 `yaml:"default_price_per_1k_tokens"`
	HoldTTLSeconds          int     `yaml:"hold_ttl_seconds"`
	BalanceCacheTTLSeconds  int     `yaml:"balance_cache_ttl_seconds"`  // default: 30
	BudgetTokenMultiplier   int     `yaml:"budget_token_multiplier"`    // default: 10
	BudgetTokenTTLSeconds   int     `yaml:"budget_token_ttl_seconds"`   // default: 60
	LowBalanceThresholdUSD  float64 `yaml:"low_balance_threshold_usd"`  // default: 1.0
	StrictUsageMetadataMode bool    `yaml:"strict_usage_metadata_mode"` // default: false (conservative fallback settlement)
}

// RateLimitOverride holds per-group rate limit overrides.
type RateLimitOverride struct {
	RequestsPerMin int   `yaml:"requests_per_min"`
	TokensPerMin   int64 `yaml:"tokens_per_min"`
	MaxConcurrent  int   `yaml:"max_concurrent"`
	BurstSize      int   `yaml:"burst_size"`
}

// RateLimitConfig holds rate limiting settings.
type RateLimitConfig struct {
	RequestsPerMin   int                          `yaml:"requests_per_min"`   // default: 60
	TokensPerMin     int64                        `yaml:"tokens_per_min"`     // default: 100000
	MaxConcurrent    int                          `yaml:"max_concurrent"`     // default: 10
	BurstSize        int                          `yaml:"burst_size"`         // default: 2
	GlobalRequestCap int                          `yaml:"global_request_cap"` // default: 10000
	GlobalTokenCap   int64                        `yaml:"global_token_cap"`   // default: 10000000
	GroupOverrides   map[string]RateLimitOverride `yaml:"group_overrides"`
	ModelTokenLimits map[string]int64             `yaml:"model_token_limits"`
}

// CircuitBreakerConfig holds circuit breaker settings.
type CircuitBreakerConfig struct {
	FailureThreshold float64 `yaml:"failure_threshold"` // default: 0.5
	WindowSeconds    int     `yaml:"window_seconds"`    // default: 30
	CooldownSeconds  int     `yaml:"cooldown_seconds"`  // default: 30
}

// Load reads a YAML config file and applies environment overrides.
// At minimum, SERVER_PORT overrides server.port.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}

	applyEnvOverrides(&cfg)

	return &cfg, nil
}

// applyEnvOverrides applies environment variable overrides to config.
// Supported env vars:
//   - SERVER_PORT: server port
//   - DB_HOST, DB_PORT, DB_USER, DB_PASSWORD, DB_NAME, DB_SSLMODE: database
//   - REDIS_ADDR, REDIS_PASSWORD, REDIS_DB: redis
//   - JWT_SECRET, JWT_EXPIRY_HOURS: auth.jwt
//   - ADMIN_EMAILS: auth.admin_emails (comma-separated, legacy/non-authorizing)
//   - SDK_BASE_URL, SDK_API_KEY, SDK_TIMEOUT_SECONDS: sdk
//   - BILLING_HOLD_AMOUNT, BILLING_DEFAULT_PRICE_PER_1K_TOKENS, BILLING_HOLD_TTL_SECONDS: billing
//   - BILLING_STRICT_USAGE_METADATA_MODE: billing (bool, default false)
func applyEnvOverrides(cfg *Config) {
	// Server
	if v := os.Getenv("SERVER_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = port
		}
	}

	// Database
	if v := os.Getenv("DB_HOST"); v != "" {
		cfg.Database.Host = v
	}
	if v := os.Getenv("DB_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.Database.Port = port
		}
	}
	if v := os.Getenv("DB_USER"); v != "" {
		cfg.Database.User = v
	}
	if v := os.Getenv("DB_PASSWORD"); v != "" {
		cfg.Database.Password = v
	}
	if v := os.Getenv("DB_NAME"); v != "" {
		cfg.Database.DBName = v
	}
	if v := os.Getenv("DB_SSLMODE"); v != "" {
		cfg.Database.SSLMode = v
	}

	// Redis
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		cfg.Redis.Addr = v
	}
	if v := os.Getenv("REDIS_PASSWORD"); v != "" {
		cfg.Redis.Password = v
	}
	if v := os.Getenv("REDIS_DB"); v != "" {
		if db, err := strconv.Atoi(v); err == nil {
			cfg.Redis.DB = db
		}
	}

	// Auth/JWT
	if v := os.Getenv("JWT_SECRET"); v != "" {
		cfg.Auth.JWT.Secret = v
	}
	if v := os.Getenv("JWT_EXPIRY_HOURS"); v != "" {
		if h, err := strconv.Atoi(v); err == nil {
			cfg.Auth.JWT.ExpiryHours = h
		}
	}
	if v := os.Getenv("ADMIN_EMAILS"); v != "" {
		parts := strings.Split(v, ",")
		emails := make([]string, 0, len(parts))
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				emails = append(emails, trimmed)
			}
		}
		cfg.Auth.AdminEmails = emails
	}

	// SDK
	if v := os.Getenv("SDK_BASE_URL"); v != "" {
		cfg.SDK.BaseURL = v
	}
	if v := os.Getenv("SDK_API_KEY"); v != "" {
		cfg.SDK.APIKey = v
	}
	if v := os.Getenv("SDK_TIMEOUT_SECONDS"); v != "" {
		if t, err := strconv.Atoi(v); err == nil {
			cfg.SDK.TimeoutSeconds = t
		}
	}

	// Billing
	if v := os.Getenv("BILLING_HOLD_AMOUNT"); v != "" {
		if amt, err := strconv.Atoi(v); err == nil {
			cfg.Billing.HoldAmount = amt
		}
	}
	if v := os.Getenv("BILLING_DEFAULT_PRICE_PER_1K_TOKENS"); v != "" {
		if price, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Billing.DefaultPricePer1KTokens = price
		}
	}
	if v := os.Getenv("BILLING_HOLD_TTL_SECONDS"); v != "" {
		if ttl, err := strconv.Atoi(v); err == nil {
			cfg.Billing.HoldTTLSeconds = ttl
		}
	}
	if v := os.Getenv("BILLING_BALANCE_CACHE_TTL_SECONDS"); v != "" {
		if ttl, err := strconv.Atoi(v); err == nil {
			cfg.Billing.BalanceCacheTTLSeconds = ttl
		}
	}
	if v := os.Getenv("BILLING_BUDGET_TOKEN_MULTIPLIER"); v != "" {
		if mult, err := strconv.Atoi(v); err == nil {
			cfg.Billing.BudgetTokenMultiplier = mult
		}
	}
	if v := os.Getenv("BILLING_BUDGET_TOKEN_TTL_SECONDS"); v != "" {
		if ttl, err := strconv.Atoi(v); err == nil {
			cfg.Billing.BudgetTokenTTLSeconds = ttl
		}
	}
	if v := os.Getenv("BILLING_LOW_BALANCE_THRESHOLD_USD"); v != "" {
		if threshold, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Billing.LowBalanceThresholdUSD = threshold
		}
	}
	if v := os.Getenv("BILLING_STRICT_USAGE_METADATA_MODE"); v != "" {
		if strict, err := strconv.ParseBool(v); err == nil {
			cfg.Billing.StrictUsageMetadataMode = strict
		}
	}

	// Rate Limit
	if v := os.Getenv("RATE_LIMIT_REQUESTS_PER_MIN"); v != "" {
		if rpm, err := strconv.Atoi(v); err == nil {
			cfg.RateLimit.RequestsPerMin = rpm
		}
	}
	if v := os.Getenv("RATE_LIMIT_TOKENS_PER_MIN"); v != "" {
		if tpm, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.RateLimit.TokensPerMin = tpm
		}
	}
	if v := os.Getenv("RATE_LIMIT_MAX_CONCURRENT"); v != "" {
		if mc, err := strconv.Atoi(v); err == nil {
			cfg.RateLimit.MaxConcurrent = mc
		}
	}
	if v := os.Getenv("RATE_LIMIT_BURST_SIZE"); v != "" {
		if bs, err := strconv.Atoi(v); err == nil {
			cfg.RateLimit.BurstSize = bs
		}
	}
	if v := os.Getenv("RATE_LIMIT_GLOBAL_REQUEST_CAP"); v != "" {
		if cap, err := strconv.Atoi(v); err == nil {
			cfg.RateLimit.GlobalRequestCap = cap
		}
	}
	if v := os.Getenv("RATE_LIMIT_GLOBAL_TOKEN_CAP"); v != "" {
		if cap, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.RateLimit.GlobalTokenCap = cap
		}
	}

	// Circuit Breaker
	if v := os.Getenv("CIRCUIT_BREAKER_FAILURE_THRESHOLD"); v != "" {
		if ft, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.CircuitBreaker.FailureThreshold = ft
		}
	}
	if v := os.Getenv("CIRCUIT_BREAKER_WINDOW_SECONDS"); v != "" {
		if ws, err := strconv.Atoi(v); err == nil {
			cfg.CircuitBreaker.WindowSeconds = ws
		}
	}
	if v := os.Getenv("CIRCUIT_BREAKER_COOLDOWN_SECONDS"); v != "" {
		if cs, err := strconv.Atoi(v); err == nil {
			cfg.CircuitBreaker.CooldownSeconds = cs
		}
	}
}
