package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/xxww0098/cpa-gateway/api"
	"github.com/xxww0098/cpa-gateway/infra"
	"github.com/xxww0098/cpa-gateway/ledger"
	"github.com/xxww0098/cpa-gateway/pricing"
	"github.com/xxww0098/cpa-gateway/sdk"
)

// GlobalStore persists SDK auth records in PostgreSQL. Set by run() once
// the database is ready. It is consumed by handler_proxy.go's InitSDK.
var GlobalStore cliproxyauth.Store

const appVersion = "0.1.0"

// globalPanel is the PanelRouter instance created during startup. Root-package
// code (handler_proxy.go's AuthMiddleware/BillingMiddleware, etc.) delegates
// API-key validation to it so the in-memory cache stays shared across the
// panel and the /v1/* proxy. It is populated by run() before any requests
// are served.
var globalPanel *api.PanelRouter

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

func run(configPath string) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	GlobalConfig = cfg

	db, err := InitDB(cfg)
	if err != nil {
		return err
	}
	GlobalDB = db
	if err := AutoMigrate(db); err != nil {
		return err
	}
	GlobalStore = sdk.NewStore(db)
	if err := api.EnsureSubscriptionSeeds(db); err != nil {
		return err
	}
	if err := EnsureSDKManagementSeeds(db, cfg); err != nil {
		return err
	}
	if err := SeedModelPrices(db); err != nil {
		slog.Warn("failed to seed model prices; continuing startup", "error", err)
	}

	redisClient := infra.InitRedis(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	GlobalLedger = ledger.New(db, redisClient)

	if err := InitSDK(cfg); err != nil {
		return err
	}

	// Construct the PanelRouter. The pricing.Calculator is a Wave-2
	// placeholder; a fully-wired Calculator lands in Task 8.
	panelRouter := api.NewPanelRouter(db, redisClient, GlobalLedger, &pricing.Calculator{}, cfg)
	panelRouter.AuthStore = GlobalStore
	panelRouter.AuthManager = authManager
	panelRouter.StartCacheCleanup(context.Background())
	globalPanel = panelRouter

	r := gin.Default()
	panelRouter.RegisterPanelRoutes(r)

	// Proxy routes remain in the root package until Wave 4 removes them.
	proxy := r.Group("/v1", panelRouter.AuthMiddleware(), BillingMiddleware())
	proxy.POST("/chat/completions", ProxyChatHandler)
	proxy.GET("/models", ProxyModelsHandler)

	addr := serverAddr(cfg)
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	slog.Info("starting CPA-Gateway HTTP server", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func serverAddr(cfg *Config) string {
	host := cfg.Server.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := cfg.Server.Port
	if port == 0 {
		port = 8888
	}
	return host + ":" + strconv.Itoa(port)
}
