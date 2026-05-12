package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	sdkapi "github.com/router-for-me/CLIProxyAPI/v7/sdk/api"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	cliproxy "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/xxww0098/cpa-gateway/api"
	"github.com/xxww0098/cpa-gateway/infra"
	"github.com/xxww0098/cpa-gateway/ledger"
	"github.com/xxww0098/cpa-gateway/pricing"
	"github.com/xxww0098/cpa-gateway/sdk"
)

const appVersion = "0.1.0"

func main() {
	configPath := flag.String("config", "config.example.yaml", "path to YAML config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		slog.Info("cpa-gateway", "version", appVersion)
		return
	}

	if err := run(*configPath); err != nil {
		slog.Error("cpa-gateway stopped", "error", err)
		os.Exit(1)
	}
}

// run wires the gateway around the CLIProxyAPI SDK Builder.
//
// The Builder owns the Gin engine and the /v1/* surface (SDK native handlers),
// runs HoldMiddleware (pre-flight balance reservation) before every /v1/*
// request, mounts the panel routes via WithRouterConfigurator so /api/panel/**
// still lives in this repo's api package, and dispatches usage records to
// UsagePlugin for Settle/Release accounting.
func run(configPath string) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}

	db, err := infra.InitDB(cfg.Database.DSN())
	if err != nil {
		return err
	}
	if err := infra.AutoMigrate(db); err != nil {
		return err
	}

	rdb := infra.InitRedis(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)

	if err := SeedModelPrices(db); err != nil {
		slog.Warn("failed to seed model prices; continuing startup", "error", err)
	}
	if err := api.EnsureSubscriptionSeeds(db); err != nil {
		return err
	}
	if err := EnsureSDKManagementSeeds(db, cfg); err != nil {
		return err
	}

	// Core billing + pricing dependencies.
	ldgr := ledger.New(db, rdb)
	priceCache, err := pricing.NewModelPriceCache(db)
	if err != nil {
		return err
	}
	calc := pricing.NewCalculator(priceCache, cfg.Billing.DefaultPricePer1KTokens)

	apiKeyCache := infra.NewAPIKeyCache()
	ctx := context.Background()
	go apiKeyCache.Start(ctx)

	// SDK interface implementations (access / usage / hold).
	store := sdk.NewStore(db)
	accessProvider := sdk.NewAccessProvider(db, rdb, apiKeyCache, cfg.Auth.JWT.Secret)
	usagePlugin := sdk.NewUsagePlugin(db, ldgr, calc)
	holdMW := sdk.NewHoldMiddleware(ldgr, calc, db, holdMiddlewareTTL(cfg))

	// Core auth manager — load the persisted auth store, then register the
	// five runtime-only upstream executors sourced from CPA-Gateway config.
	authMgr := cliproxyauth.NewManager(store, &cliproxyauth.RoundRobinSelector{}, cliproxyauth.NoopHook{})
	if err := authMgr.Load(ctx); err != nil {
		return err
	}
	if err := registerRuntimeAuths(authMgr, cfg); err != nil {
		return err
	}

	// Access manager: the CPA tenant provider authenticates /v1/* callers.
	accessMgr := sdkaccess.NewManager()
	accessMgr.SetProviders([]sdkaccess.Provider{accessProvider})

	// Panel router owns the /api/panel/** surface.
	panelRouter := api.NewPanelRouter(db, rdb, ldgr, calc, cfg)
	panelRouter.AuthStore = store
	panelRouter.AuthManager = authMgr
	panelRouter.APIKeyCache = apiKeyCache
	panelRouter.StartCacheCleanup(ctx)

	// Build the SDK Service with HoldMiddleware and panel routes attached.
	svc, err := cliproxy.NewBuilder().
		WithConfig(buildSDKConfig(cfg)).
		WithConfigPath(configPath).
		WithCoreAuthManager(authMgr).
		WithRequestAccessManager(accessMgr).
		WithServerOptions(
			sdkapi.WithMiddleware(holdMW.Handle),
			sdkapi.WithRouterConfigurator(func(e *gin.Engine, _ *handlers.BaseAPIHandler, _ *sdkconfig.Config) {
				panelRouter.RegisterPanelRoutes(e)
			}),
		).
		Build()
	if err != nil {
		return err
	}

	// UsagePlugin registration must happen after Build — the Builder does
	// not expose a WithUsagePlugin hook (the SDK's usage manager is a
	// process-global registry set up during Service.Run).
	svc.RegisterUsagePlugin(usagePlugin)

	slog.Info("starting CPA-Gateway via SDK Builder",
		"host", cfg.Server.Host, "port", cfg.Server.Port)
	return svc.Run(ctx)
}

// buildSDKConfig adapts the CPA-Gateway configuration to the minimal
// sdkconfig.Config that the CLIProxyAPI Builder requires. We only set
// the fields the Service itself reads at startup — host/port for the HTTP
// bind, and AuthDir for the file-backed token store.
//
// Upstream credentials are NOT mirrored here: CPA-Gateway owns that
// surface through registerRuntimeAuths and the SDK management API routes
// on /api/panel/admin/sdk-management.
func buildSDKConfig(cfg *Config) *sdkconfig.Config {
	host := cfg.Server.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := cfg.Server.Port
	if port == 0 {
		port = 8888
	}
	authDir := ".cpa-gateway-sdk-auth"
	sdkCfg := &sdkconfig.Config{
		Host:    host,
		Port:    port,
		AuthDir: authDir,
	}
	return sdkCfg
}

// holdMiddlewareTTL derives the Hold key TTL from the billing config,
// defaulting to 5 minutes when unset.
func holdMiddlewareTTL(cfg *Config) time.Duration {
	if cfg != nil && cfg.Billing.HoldTTLSeconds > 0 {
		return time.Duration(cfg.Billing.HoldTTLSeconds) * time.Second
	}
	return 5 * time.Minute
}
