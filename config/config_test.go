package config

import (
	"os"
	"path/filepath"
	"testing"
)

// helper to write a YAML string to a temp file and return its path.
func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing temp YAML: %v", err)
	}
	return path
}

// --- YAML Parsing Tests ---

func TestYAML_BillingConfig_NewFields(t *testing.T) {
	yaml := `
billing:
  hold_amount: 5
  default_price_per_1k_tokens: 0.03
  hold_ttl_seconds: 300
  balance_cache_ttl_seconds: 45
  budget_token_multiplier: 15
  budget_token_ttl_seconds: 90
  low_balance_threshold_usd: 2.5
`
	cfg, err := Load(writeTempYAML(t, yaml))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Billing.HoldAmount != 5 {
		t.Errorf("HoldAmount = %d, want 5", cfg.Billing.HoldAmount)
	}
	if cfg.Billing.DefaultPricePer1KTokens != 0.03 {
		t.Errorf("DefaultPricePer1KTokens = %f, want 0.03", cfg.Billing.DefaultPricePer1KTokens)
	}
	if cfg.Billing.HoldTTLSeconds != 300 {
		t.Errorf("HoldTTLSeconds = %d, want 300", cfg.Billing.HoldTTLSeconds)
	}
	if cfg.Billing.BalanceCacheTTLSeconds != 45 {
		t.Errorf("BalanceCacheTTLSeconds = %d, want 45", cfg.Billing.BalanceCacheTTLSeconds)
	}
	if cfg.Billing.BudgetTokenMultiplier != 15 {
		t.Errorf("BudgetTokenMultiplier = %d, want 15", cfg.Billing.BudgetTokenMultiplier)
	}
	if cfg.Billing.BudgetTokenTTLSeconds != 90 {
		t.Errorf("BudgetTokenTTLSeconds = %d, want 90", cfg.Billing.BudgetTokenTTLSeconds)
	}
	if cfg.Billing.LowBalanceThresholdUSD != 2.5 {
		t.Errorf("LowBalanceThresholdUSD = %f, want 2.5", cfg.Billing.LowBalanceThresholdUSD)
	}
}

func TestYAML_RateLimitConfig(t *testing.T) {
	yaml := `
rate_limit:
  requests_per_min: 120
  tokens_per_min: 200000
  max_concurrent: 20
  burst_size: 4
  global_request_cap: 50000
  global_token_cap: 5000000
  group_overrides:
    "premium":
      requests_per_min: 300
      tokens_per_min: 500000
      max_concurrent: 50
      burst_size: 10
    "basic":
      requests_per_min: 30
      tokens_per_min: 50000
      max_concurrent: 5
      burst_size: 1
  model_token_limits:
    "claude-opus": 50000
    "gpt-4o": 80000
`
	cfg, err := Load(writeTempYAML(t, yaml))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	rl := cfg.RateLimit
	if rl.RequestsPerMin != 120 {
		t.Errorf("RequestsPerMin = %d, want 120", rl.RequestsPerMin)
	}
	if rl.TokensPerMin != 200000 {
		t.Errorf("TokensPerMin = %d, want 200000", rl.TokensPerMin)
	}
	if rl.MaxConcurrent != 20 {
		t.Errorf("MaxConcurrent = %d, want 20", rl.MaxConcurrent)
	}
	if rl.BurstSize != 4 {
		t.Errorf("BurstSize = %d, want 4", rl.BurstSize)
	}
	if rl.GlobalRequestCap != 50000 {
		t.Errorf("GlobalRequestCap = %d, want 50000", rl.GlobalRequestCap)
	}
	if rl.GlobalTokenCap != 5000000 {
		t.Errorf("GlobalTokenCap = %d, want 5000000", rl.GlobalTokenCap)
	}

	// GroupOverrides
	if len(rl.GroupOverrides) != 2 {
		t.Fatalf("GroupOverrides len = %d, want 2", len(rl.GroupOverrides))
	}
	premium, ok := rl.GroupOverrides["premium"]
	if !ok {
		t.Fatal("GroupOverrides missing 'premium' key")
	}
	if premium.RequestsPerMin != 300 {
		t.Errorf("premium.RequestsPerMin = %d, want 300", premium.RequestsPerMin)
	}
	if premium.TokensPerMin != 500000 {
		t.Errorf("premium.TokensPerMin = %d, want 500000", premium.TokensPerMin)
	}
	if premium.MaxConcurrent != 50 {
		t.Errorf("premium.MaxConcurrent = %d, want 50", premium.MaxConcurrent)
	}
	if premium.BurstSize != 10 {
		t.Errorf("premium.BurstSize = %d, want 10", premium.BurstSize)
	}

	basic, ok := rl.GroupOverrides["basic"]
	if !ok {
		t.Fatal("GroupOverrides missing 'basic' key")
	}
	if basic.RequestsPerMin != 30 {
		t.Errorf("basic.RequestsPerMin = %d, want 30", basic.RequestsPerMin)
	}

	// ModelTokenLimits
	if len(rl.ModelTokenLimits) != 2 {
		t.Fatalf("ModelTokenLimits len = %d, want 2", len(rl.ModelTokenLimits))
	}
	if rl.ModelTokenLimits["claude-opus"] != 50000 {
		t.Errorf("ModelTokenLimits[claude-opus] = %d, want 50000", rl.ModelTokenLimits["claude-opus"])
	}
	if rl.ModelTokenLimits["gpt-4o"] != 80000 {
		t.Errorf("ModelTokenLimits[gpt-4o] = %d, want 80000", rl.ModelTokenLimits["gpt-4o"])
	}
}

func TestYAML_CircuitBreakerConfig(t *testing.T) {
	yaml := `
circuit_breaker:
  failure_threshold: 0.7
  window_seconds: 60
  cooldown_seconds: 45
`
	cfg, err := Load(writeTempYAML(t, yaml))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	cb := cfg.CircuitBreaker
	if cb.FailureThreshold != 0.7 {
		t.Errorf("FailureThreshold = %f, want 0.7", cb.FailureThreshold)
	}
	if cb.WindowSeconds != 60 {
		t.Errorf("WindowSeconds = %d, want 60", cb.WindowSeconds)
	}
	if cb.CooldownSeconds != 45 {
		t.Errorf("CooldownSeconds = %d, want 45", cb.CooldownSeconds)
	}
}

// --- Env Override Tests ---

func TestEnvOverride_BillingNewFields(t *testing.T) {
	yaml := `
billing:
  hold_amount: 1
  balance_cache_ttl_seconds: 10
  budget_token_multiplier: 5
  budget_token_ttl_seconds: 30
  low_balance_threshold_usd: 0.5
`
	path := writeTempYAML(t, yaml)

	// Set env vars to override
	t.Setenv("BILLING_BALANCE_CACHE_TTL_SECONDS", "60")
	t.Setenv("BILLING_BUDGET_TOKEN_MULTIPLIER", "20")
	t.Setenv("BILLING_BUDGET_TOKEN_TTL_SECONDS", "120")
	t.Setenv("BILLING_LOW_BALANCE_THRESHOLD_USD", "5.0")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Billing.BalanceCacheTTLSeconds != 60 {
		t.Errorf("BalanceCacheTTLSeconds = %d, want 60 (env override)", cfg.Billing.BalanceCacheTTLSeconds)
	}
	if cfg.Billing.BudgetTokenMultiplier != 20 {
		t.Errorf("BudgetTokenMultiplier = %d, want 20 (env override)", cfg.Billing.BudgetTokenMultiplier)
	}
	if cfg.Billing.BudgetTokenTTLSeconds != 120 {
		t.Errorf("BudgetTokenTTLSeconds = %d, want 120 (env override)", cfg.Billing.BudgetTokenTTLSeconds)
	}
	if cfg.Billing.LowBalanceThresholdUSD != 5.0 {
		t.Errorf("LowBalanceThresholdUSD = %f, want 5.0 (env override)", cfg.Billing.LowBalanceThresholdUSD)
	}
}

func TestEnvOverride_RateLimitFields(t *testing.T) {
	yaml := `
rate_limit:
  requests_per_min: 60
  tokens_per_min: 100000
  max_concurrent: 10
  burst_size: 2
  global_request_cap: 10000
  global_token_cap: 10000000
`
	path := writeTempYAML(t, yaml)

	t.Setenv("RATE_LIMIT_REQUESTS_PER_MIN", "200")
	t.Setenv("RATE_LIMIT_TOKENS_PER_MIN", "500000")
	t.Setenv("RATE_LIMIT_MAX_CONCURRENT", "50")
	t.Setenv("RATE_LIMIT_BURST_SIZE", "8")
	t.Setenv("RATE_LIMIT_GLOBAL_REQUEST_CAP", "99999")
	t.Setenv("RATE_LIMIT_GLOBAL_TOKEN_CAP", "8888888")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	rl := cfg.RateLimit
	if rl.RequestsPerMin != 200 {
		t.Errorf("RequestsPerMin = %d, want 200 (env override)", rl.RequestsPerMin)
	}
	if rl.TokensPerMin != 500000 {
		t.Errorf("TokensPerMin = %d, want 500000 (env override)", rl.TokensPerMin)
	}
	if rl.MaxConcurrent != 50 {
		t.Errorf("MaxConcurrent = %d, want 50 (env override)", rl.MaxConcurrent)
	}
	if rl.BurstSize != 8 {
		t.Errorf("BurstSize = %d, want 8 (env override)", rl.BurstSize)
	}
	if rl.GlobalRequestCap != 99999 {
		t.Errorf("GlobalRequestCap = %d, want 99999 (env override)", rl.GlobalRequestCap)
	}
	if rl.GlobalTokenCap != 8888888 {
		t.Errorf("GlobalTokenCap = %d, want 8888888 (env override)", rl.GlobalTokenCap)
	}
}

func TestEnvOverride_CircuitBreakerFields(t *testing.T) {
	yaml := `
circuit_breaker:
  failure_threshold: 0.5
  window_seconds: 30
  cooldown_seconds: 30
`
	path := writeTempYAML(t, yaml)

	t.Setenv("CIRCUIT_BREAKER_FAILURE_THRESHOLD", "0.8")
	t.Setenv("CIRCUIT_BREAKER_WINDOW_SECONDS", "120")
	t.Setenv("CIRCUIT_BREAKER_COOLDOWN_SECONDS", "90")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	cb := cfg.CircuitBreaker
	if cb.FailureThreshold != 0.8 {
		t.Errorf("FailureThreshold = %f, want 0.8 (env override)", cb.FailureThreshold)
	}
	if cb.WindowSeconds != 120 {
		t.Errorf("WindowSeconds = %d, want 120 (env override)", cb.WindowSeconds)
	}
	if cb.CooldownSeconds != 90 {
		t.Errorf("CooldownSeconds = %d, want 90 (env override)", cb.CooldownSeconds)
	}
}

// --- Billing.StrictUsageMetadataMode ---

// TestStrictUsageMetadataModeDefault confirms that when no YAML override is
// provided and BILLING_STRICT_USAGE_METADATA_MODE is not set, the default
// value is false (conservative fallback settlement). See Requirement 1.6.
func TestStrictUsageMetadataModeDefault(t *testing.T) {
	// Force the env var to empty so this test is robust regardless of the
	// host shell. t.Setenv restores the prior state on test completion,
	// preventing leaks into other tests.
	t.Setenv("BILLING_STRICT_USAGE_METADATA_MODE", "")

	// Minimal YAML that does not mention strict_usage_metadata_mode, so the
	// zero value of the Go struct field is what we ultimately observe.
	cfg, err := Load(writeTempYAML(t, "billing:\n  hold_amount: 1\n"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Billing.StrictUsageMetadataMode {
		t.Errorf("StrictUsageMetadataMode = true, want false (default)")
	}
}

// TestStrictUsageMetadataModeEnvOverride exercises the BILLING_STRICT_USAGE_METADATA_MODE
// env-override for each documented input. "invalid" and "" must leave the
// default false unchanged. See Requirement 1.6.
func TestStrictUsageMetadataModeEnvOverride(t *testing.T) {
	// Baseline YAML has strict_usage_metadata_mode absent, so the struct
	// field starts at zero (false). Each sub-test asserts the env-applied
	// result directly.
	cases := []struct {
		envValue string
		want     bool
	}{
		{envValue: "true", want: true},
		{envValue: "1", want: true},
		{envValue: "false", want: false},
		{envValue: "0", want: false},
		{envValue: "invalid", want: false}, // ParseBool fails -> default preserved.
		{envValue: "", want: false},        // empty env -> override branch skipped.
	}

	for _, tc := range cases {
		tc := tc
		t.Run("env="+tc.envValue, func(t *testing.T) {
			t.Setenv("BILLING_STRICT_USAGE_METADATA_MODE", tc.envValue)

			cfg, err := Load(writeTempYAML(t, "billing:\n  hold_amount: 1\n"))
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}

			if cfg.Billing.StrictUsageMetadataMode != tc.want {
				t.Errorf("StrictUsageMetadataMode = %v, want %v (env=%q)",
					cfg.Billing.StrictUsageMetadataMode, tc.want, tc.envValue)
			}
		})
	}

	// Also confirm that when the baseline is already true (via YAML), the
	// "false"/"0" env values actually downgrade it, and "invalid"/""
	// preserve it. This proves overrides are bi-directional.
	yamlTrue := "billing:\n  strict_usage_metadata_mode: true\n"
	downCases := []struct {
		envValue string
		want     bool
	}{
		{envValue: "false", want: false},
		{envValue: "0", want: false},
		{envValue: "invalid", want: true}, // YAML value preserved.
		{envValue: "", want: true},        // YAML value preserved.
	}
	for _, tc := range downCases {
		tc := tc
		t.Run("yaml=true/env="+tc.envValue, func(t *testing.T) {
			t.Setenv("BILLING_STRICT_USAGE_METADATA_MODE", tc.envValue)

			cfg, err := Load(writeTempYAML(t, yamlTrue))
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}

			if cfg.Billing.StrictUsageMetadataMode != tc.want {
				t.Errorf("StrictUsageMetadataMode = %v, want %v (yaml=true, env=%q)",
					cfg.Billing.StrictUsageMetadataMode, tc.want, tc.envValue)
			}
		})
	}
}
