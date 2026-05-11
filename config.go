package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds all configuration sections.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Redis    RedisConfig    `yaml:"redis"`
	SDK      SDKConfig      `yaml:"sdk"`
	Auth     AuthConfig     `yaml:"auth"`
	Billing  BillingConfig  `yaml:"billing"`
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

func (s SDKConfig) openAIProviderConfig() SDKProviderConfig {
	provider := s.OpenAI
	if !provider.configured() && s.OpenAICompatible.configured() {
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

func (s SDKConfig) pendingProviderConfigs() map[string]SDKProviderConfig {
	return map[string]SDKProviderConfig{
		"claude": s.Claude,
		"gemini": s.Gemini,
		"codex":  s.Codex,
		"vertex": s.Vertex,
	}
}

func (p SDKProviderConfig) configured() bool {
	return p.Enabled || strings.TrimSpace(p.BaseURL) != "" || strings.TrimSpace(p.APIKey) != ""
}

func (p SDKProviderConfig) complete() bool {
	return p.Enabled && strings.TrimSpace(p.BaseURL) != "" && strings.TrimSpace(p.APIKey) != ""
}

// AuthConfig holds authentication settings.
type AuthConfig struct {
	JWT         JWTConfig `yaml:"jwt"`
	AdminEmails []string  `yaml:"admin_emails"`
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
}

// LoadConfig reads a YAML config file and applies environment overrides.
// At minimum, SERVER_PORT overrides server.port.
func LoadConfig(path string) (*Config, error) {
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
//   - ADMIN_EMAILS: auth.admin_emails (comma-separated)
//   - SDK_BASE_URL, SDK_API_KEY, SDK_TIMEOUT_SECONDS: sdk
//   - BILLING_HOLD_AMOUNT, BILLING_DEFAULT_PRICE_PER_1K_TOKENS, BILLING_HOLD_TTL_SECONDS: billing
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
}
