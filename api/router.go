package api

import (
	"time"

	"github.com/gin-gonic/gin"
	sdkapi "github.com/router-for-me/CLIProxyAPI/v7/sdk/api"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/redis/go-redis/v9"
	"github.com/xxww0098/cpa-gateway/config"
	"github.com/xxww0098/cpa-gateway/infra"
	"github.com/xxww0098/cpa-gateway/ledger"
	"github.com/xxww0098/cpa-gateway/pricing"
	"gorm.io/gorm"
)

// PanelRouter bundles the dependencies needed by the panel HTTP handlers and
// exposes a single RegisterPanelRoutes entrypoint.
//
// The SDK integration fields (AuthManager, AuthStore) are populated by main.go
// and used by SDK-management handlers. Business handlers only touch the core
// DB/Redis/Ledger/Calc/Config surface.
type PanelRouter struct {
	DB          *gorm.DB
	Redis       *redis.Client
	Ledger      *ledger.Ledger
	Calc        *pricing.Calculator
	Config      *config.Config
	APIKeyCache *infra.APIKeyCache

	// PriceCache is the in-memory ModelPrice cache shared with *pricing.Calculator.
	// Populated by main.go after cache construction; nil-safe so callers (e.g. admin
	// price upsert handlers) must check before invoking Invalidate. The constructor
	// deliberately does not take this field to avoid changing NewPanelRouter's
	// signature; tests that do not exercise pricing-cache invalidation leave it nil.
	PriceCache *pricing.ModelPriceCache

	// UserStatusCache memoizes the `users.status` column so that Panel JWT
	// user-status rechecks (api/middleware.go, api/handler_admin.go) do not hit
	// the DB on every request. Populated by main.go after construction with the
	// same instance shared by sdk.AccessProvider.UserStatusCache so invalidation
	// on one side is observed by the other; nil-safe so tests that do not
	// exercise status-recheck paths can leave it unset and fall through to the
	// DB (see sdk.AccessProvider.userIsActive for the same pattern).
	UserStatusCache *infra.UserStatusCache

	// SDK integration surface (populated by main.go; may be nil during tests).
	AuthManager *cliproxyauth.Manager
	AuthStore   cliproxyauth.Store

	// OAuthTokenRequester delegates Antigravity/Kimi OAuth to the embedded SDK
	// management handlers (device-flow + localhost callback). Nil in unit tests.
	OAuthTokenRequester sdkapi.ManagementTokenRequester

	// startedAt is captured once for metrics output.
	startedAt time.Time
	metrics   *metricsStore
}

// NewPanelRouter constructs a PanelRouter with the provided dependencies.
// AuthManager and AuthStore should be set separately by main.go after the
// SDK auth manager has been built.
func NewPanelRouter(
	db *gorm.DB,
	rdb *redis.Client,
	l *ledger.Ledger,
	calc *pricing.Calculator,
	cfg *config.Config,
) *PanelRouter {
	pr := &PanelRouter{
		DB:        db,
		Redis:     rdb,
		Ledger:    l,
		Calc:      calc,
		Config:    cfg,
		startedAt: time.Now(),
		metrics:   newMetricsStore(),
	}
	return pr
}

// RegisterPanelRoutes wires all panel and helper routes onto the provided
// gin router. Global middleware (trace-id, metrics, rate-limit) is applied
// via pr.SetupGlobalMiddleware; callers are free to call that separately.
func (pr *PanelRouter) RegisterPanelRoutes(r gin.IRouter) {
	healthHandler := func(c *gin.Context) {
		Success(c, gin.H{"status": "ok"})
	}
	// /healthz is already registered by the SDK's internal server; only
	// register /api/health to avoid a duplicate-route panic.
	r.GET("/api/health", healthHandler)
	r.GET("/metrics", pr.MetricsHandler)

	panel := r.Group("/api/panel")
	pr.RegisterAuthRoutes(panel)

	authed := panel.Group("/", pr.AuthMiddleware())
	pr.RegisterUserRoutes(authed)
	pr.RegisterSubscriptionRoutes(authed)
	pr.RegisterAdminRoutes(authed)
	pr.RegisterOpsRoutes(authed)

	sdkMgmt := authed.Group("/admin/sdk-management")
	pr.RegisterSDKManagementRoutes(sdkMgmt)

	authed.PATCH("/admin/sdk-config", pr.SDKMgmtSDKConfigPatchHandler)
}

// SetupGlobalMiddleware registers TraceID/Metrics/RateLimit middleware on the engine.
// main.go calls this before RegisterPanelRoutes when running panel routes outside
// the SDK Builder's router configurator.
func (pr *PanelRouter) SetupGlobalMiddleware(r *gin.Engine) {
	r.Use(pr.TraceIDMiddleware(), pr.MetricsMiddleware(), pr.RateLimitMiddleware())
}
