package main

import (
	sdkconfig "github.com/xxww0098/cpa-gateway/config"
)

// Config aliases the shared configuration type so existing root-package code
// (auth.go, handler_proxy.go, seed.go, middleware.go) keeps compiling without
// importing the new `config` package everywhere at once.
type Config = sdkconfig.Config

// Type aliases used by main/root helpers and handler_proxy.go.
type (
	SDKConfig         = sdkconfig.SDKConfig
	SDKProviderConfig = sdkconfig.SDKProviderConfig
	ServerConfig      = sdkconfig.ServerConfig
	DatabaseConfig    = sdkconfig.DatabaseConfig
	RedisConfig       = sdkconfig.RedisConfig
	AuthConfig        = sdkconfig.AuthConfig
	JWTConfig         = sdkconfig.JWTConfig
	BillingConfig     = sdkconfig.BillingConfig
)

// LoadConfig reads a YAML config file and applies environment overrides.
// Thin wrapper over sdkconfig.Load preserving the legacy call site.
func LoadConfig(path string) (*Config, error) {
	return sdkconfig.Load(path)
}
