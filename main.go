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
	"github.com/xxww0098/cpa-gateway/config"
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
	cfg, err := config.Load(configPath)
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
	if err := infra.RunCustomMigrations(db); err != nil {
		return err
	}

	rdb := infra.InitRedis(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)

	if err := infra.SeedModelPrices(db); err != nil {
		slog.Warn("failed to seed model prices; continuing startup", "error", err)
	}
	if err := api.EnsureSubscriptionSeeds(db); err != nil {
		return err
	}
	if err := infra.EnsureSDKManagementSeeds(db, cfg); err != nil {
		return err
	}

	// Core billing + pricing dependencies.
	ldgr := ledger.New(db, rdb)
	priceCache, err := pricing.NewModelPriceCache(db)
	if err != nil {
		return err
	}
	// Config stores the default price as "per 1K tokens" but the Calculator
	// operates in "per 1M tokens" units (matching ModelPrice columns).
	defaultPricePer1M := cfg.Billing.DefaultPricePer1KTokens * 1000
	calc := pricing.NewCalculator(priceCache, defaultPricePer1M)

	apiKeyCache := infra.NewAPIKeyCache()
	ctx := context.Background()

	// Distributed infrastructure components.
	rateLimiter := infra.NewRateLimiter(rdb, cfg.RateLimit)
	circuitBreaker := infra.NewCircuitBreaker(rdb, cfg.CircuitBreaker)
	idempotencyMgr := sdk.NewIdempotencyManager(rdb, 0) // 0 → default 24h TTL
	budgetTokenStore := sdk.NewBudgetTokenStore()

	// SDK interface implementations (access / usage / hold).
	store := sdk.NewStore(db)
	accessProvider := sdk.NewAccessProvider(db, rdb, apiKeyCache, cfg.Auth.JWT.Secret)
	// Shared user-status cache: the same instance must be visible to both
	// sdk.AccessProvider (for /v1/* auth rechecks) and api.PanelRouter (for
	// /api/panel/** JWT rechecks and admin invalidation hooks) so a status
	// flip observed on one surface is reflected on the other. Populated as a
	// post-construction field to keep NewAccessProvider / NewPanelRouter
	// signatures untouched.
	userStatusCache := infra.NewUserStatusCache()
	accessProvider.UserStatusCache = userStatusCache
	usagePlugin := sdk.NewUsagePlugin(db, ldgr, calc)
	usagePlugin.SetBudgetTokenStore(budgetTokenStore)
	usagePlugin.SetLowBalanceThreshold(cfg.Billing.LowBalanceThresholdUSD)
	// Wire the strict-usage-metadata toggle from config (Task 16.3). The
	// default config value is `false` — conservative fallback settlement —
	// and ops can flip to `true` via YAML / BILLING_STRICT_USAGE_METADATA_MODE
	// to suspend billing on upstream responses that strip the usage envelope.
	// See design.md "Observation recipe for ops" for the roll-out procedure.
	usagePlugin.SetStrictUsageMetadataMode(cfg.Billing.StrictUsageMetadataMode)

	holdMW := sdk.NewHoldMiddleware(ldgr, calc, db, holdMiddlewareTTL(cfg))
	holdMW.SetRateLimiter(rateLimiter)
	holdMW.SetCircuitBreaker(circuitBreaker)
	holdMW.SetIdempotencyManager(idempotencyMgr)
	holdMW.SetBudgetTokenStore(budgetTokenStore)

	// Core auth manager — load the persisted auth store, then register the
	// five runtime-only upstream executors sourced from CPA-Gateway config.
	authMgr := cliproxyauth.NewManager(store, &cliproxyauth.RoundRobinSelector{}, cliproxyauth.NoopHook{})
	if err := authMgr.Load(ctx); err != nil {
		return err
	}
	if err := sdk.RegisterRuntimeAuths(authMgr, cfg); err != nil {
		return err
	}

	// Access manager: the CPA tenant provider authenticates /v1/* callers.
	accessMgr := sdkaccess.NewManager()
	accessMgr.SetProviders([]sdkaccess.Provider{accessProvider})

	// Panel router owns the /api/panel/** surface.
	panelRouter := api.NewPanelRouter(db, rdb, ldgr, calc, cfg)
	panelRouter.AuthStore = store
	panelRouter.AuthManager = authMgr
	panelRouter.OAuthTokenRequester = sdkapi.NewManagementTokenRequester(buildSDKConfig(cfg), authMgr)
	panelRouter.APIKeyCache = apiKeyCache
	// Share the same ModelPriceCache instance used by the Calculator so that
	// admin price upserts (handler_ops) can Invalidate the cache that the
	// Calculator actually reads from. Constructing a second cache here would
	// leave the Calculator's cache stale after invalidation.
	panelRouter.PriceCache = priceCache
	// Share the same UserStatusCache instance with AccessProvider so the Panel
	// middleware and /v1/* auth see identical (userID → status) state. The
	// sweeper goroutine is launched once for this shared instance; APIKeyCache
	// uses its own sweeper started by StartCacheCleanup below.
	panelRouter.UserStatusCache = userStatusCache
	go userStatusCache.Start(ctx)
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
// surface through RegisterRuntimeAuths and the SDK management API routes
// on /api/panel/admin/sdk-management.
func buildSDKConfig(cfg *config.Config) *sdkconfig.Config {
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
func holdMiddlewareTTL(cfg *config.Config) time.Duration {
	if cfg != nil && cfg.Billing.HoldTTLSeconds > 0 {
		return time.Duration(cfg.Billing.HoldTTLSeconds) * time.Second
	}
	return 5 * time.Minute
}
