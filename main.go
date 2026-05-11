package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
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
	GlobalStore = NewPostgresAuthStore(db)
	if err := EnsureSubscriptionSeeds(db); err != nil {
		return err
	}
	if err := EnsureSDKManagementSeeds(db, cfg); err != nil {
		return err
	}

	redisClient := initRedis(cfg)
	GlobalLedger = NewLedger(db, redisClient)
	go startCacheCleanup(context.Background())

	if err := InitSDK(cfg); err != nil {
		return err
	}

	r := gin.Default()
	registerRoutes(r)

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

func initRedis(cfg *Config) *redis.Client {
	addr := cfg.Redis.Addr
	if addr == "" {
		slog.Warn("Redis disabled: redis.addr is empty")
		return nil
	}

	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		slog.Warn("Redis unavailable; continuing without Redis-backed holds", "error", err)
		if closeErr := client.Close(); closeErr != nil {
			slog.Warn("failed to close unavailable Redis client", "error", closeErr)
		}
		return nil
	}

	slog.Info("Redis connection established", "addr", addr, "db", cfg.Redis.DB)
	return client
}

func registerRoutes(r *gin.Engine) {
	SetupMiddleware(r)

	healthHandler := func(c *gin.Context) {
		Success(c, gin.H{"status": "ok"})
	}
	r.GET("/healthz", healthHandler)
	r.GET("/api/health", healthHandler)
	r.GET("/metrics", MetricsHandler)

	panel := r.Group("/api/panel")
	RegisterAuthRoutes(panel)
	authedPanel := panel.Group("/", AuthMiddleware())
	RegisterUserRoutes(authedPanel)
	RegisterSubscriptionRoutes(authedPanel)
	RegisterAdminRoutes(authedPanel)
	RegisterOpsRoutes(authedPanel)

	sdkMgmt := authedPanel.Group("/admin/sdk-management")
	RegisterSDKManagementRoutes(sdkMgmt)

	authedPanel.PATCH("/admin/sdk-config", SDKMgmtSDKConfigPatchHandler)

	proxy := r.Group("/v1", AuthMiddleware(), BillingMiddleware())
	proxy.POST("/chat/completions", ProxyChatHandler)
	proxy.GET("/models", ProxyModelsHandler)
}

func MetricsHandler(c *gin.Context) {
	requestMetrics.mu.RLock()
	defer requestMetrics.mu.RUnlock()

	statusCounts := make(map[int]uint64, len(requestMetrics.statusCounts))
	maps.Copy(statusCounts, requestMetrics.statusCounts)
	pathCounts := make(map[string]uint64, len(requestMetrics.pathCounts))
	maps.Copy(pathCounts, requestMetrics.pathCounts)

	Success(c, gin.H{
		"started_at":         requestMetrics.startedAt.UTC().Format(time.RFC3339),
		"uptime_seconds":     int64(time.Since(requestMetrics.startedAt).Seconds()),
		"total_requests":     requestMetrics.totalRequests,
		"in_flight":          requestMetrics.inFlight,
		"status_counts":      statusCounts,
		"path_counts":        pathCounts,
		"total_latency_ms":   requestMetrics.totalLatencySum.Milliseconds(),
		"average_latency_ms": averageLatencyMillisLocked(),
	})
}

func averageLatencyMillisLocked() float64 {
	if requestMetrics.totalRequests == 0 {
		return 0
	}
	return float64(requestMetrics.totalLatencySum.Milliseconds()) / float64(requestMetrics.totalRequests)
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
