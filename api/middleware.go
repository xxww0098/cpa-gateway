package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/xxww0098/cpa-gateway/authutil"
	"github.com/xxww0098/cpa-gateway/infra"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/gorm"
)

const (
	billingCtxGinKey = "billingCtx"
	traceIDHeader    = "X-Trace-ID"
	authTypeAPIKey   = "api_key"
	authTypeJWT      = "jwt"

	defaultRateLimitPerMin      = 60
	middlewareErrorUnauthorized = 1001
	middlewareErrorRateLimit    = 3001
	middlewareErrorInternal     = 5001
)

type billingContextKey struct{}

// BillingCtx carries authenticated-request metadata shared across handlers.
type BillingCtx struct {
	UserID    uint
	ApiKeyID  uint
	GroupID   *uint
	RateMult  float64
	AuthType  string
	Email     string
	Status    string
	RequestID string
}

// BillingContextFromContext returns a BillingCtx previously injected into the request context.
func BillingContextFromContext(ctx context.Context) (*BillingCtx, bool) {
	bc, ok := ctx.Value(billingContextKey{}).(*BillingCtx)
	return bc, ok
}

// AuthMiddleware validates Bearer JWT/API-key credentials and injects BillingCtx
// into both the gin context and the request context.
func (pr *PanelRouter) AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			Error(c, http.StatusUnauthorized, middlewareErrorUnauthorized, "missing or invalid authorization bearer token")
			c.Abort()
			return
		}

		bc := &BillingCtx{RateMult: 1.0, RequestID: traceIDFromGin(c)}
		if strings.HasPrefix(token, "cpa-") {
			cached, err := pr.validateAPIKey(c.Request.Context(), token)
			if err != nil {
				Error(c, http.StatusUnauthorized, middlewareErrorUnauthorized, "invalid API key")
				c.Abort()
				return
			}
			bc.UserID = cached.UserID
			bc.ApiKeyID = cached.ApiKeyID
			bc.GroupID = cached.GroupID
			bc.RateMult = cached.RateMult
			bc.Status = cached.Status
			bc.AuthType = authTypeAPIKey
		} else {
			claims, err := authutil.ValidateJWT(token, pr.Config.Auth.JWT.Secret)
			if err != nil {
				Error(c, http.StatusUnauthorized, middlewareErrorUnauthorized, "invalid JWT")
				c.Abort()
				return
			}
			bc.UserID = claims.UserID
			bc.Email = claims.Email
			bc.Status = "active"
			bc.AuthType = authTypeJWT
		}

		setBillingContext(c, bc)
		c.Next()
	}
}

// TraceIDMiddleware passes through or generates X-Trace-ID for every request.
func (pr *PanelRouter) TraceIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := strings.TrimSpace(c.GetHeader(traceIDHeader))
		if requestID == "" {
			requestID = uuid.NewString()
		}
		c.Set(traceIDHeader, requestID)
		c.Writer.Header().Set(traceIDHeader, requestID)
		ctx := context.WithValue(c.Request.Context(), traceIDHeader, requestID)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// MetricsMiddleware records per-request counters on the PanelRouter's metrics store.
func (pr *PanelRouter) MetricsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		pr.metrics.start(c.FullPath())
		defer func() {
			pr.metrics.finish(c.Writer.Status(), time.Since(start))
		}()
		c.Next()
	}
}

// RateLimitMiddleware enforces a simple per-user/IP requests-per-minute cap.
func (pr *PanelRouter) RateLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := c.ClientIP()
		if bc, ok := billingContextFromGin(c); ok && bc.UserID != 0 {
			identity = bc.AuthType + ":" + strconv.FormatUint(uint64(bc.UserID), 10)
		}
		if !allowRequest(identity, defaultRateLimitPerMin) {
			Error(c, http.StatusTooManyRequests, middlewareErrorRateLimit, "rate limit exceeded")
			c.Abort()
			return
		}
		c.Next()
	}
}

// validateAPIKey validates a plaintext key through the router's in-memory cache
// and falls back to a DB lookup on miss. It mirrors the behaviour previously
// implemented by the root-package ValidateAPIKey helper.
func (pr *PanelRouter) validateAPIKey(ctx context.Context, plaintext string) (*infra.CachedKey, error) {
	keyHash := authutil.HashAPIKey(plaintext)

	if pr.APIKeyCache != nil {
		if ck, found := pr.APIKeyCache.Get(keyHash); found {
			return ck, nil
		}
	}
	if pr.DB == nil {
		return nil, errors.New("database not initialized")
	}

	var apiKey model.ApiKey
	if err := pr.DB.WithContext(ctx).Where("key_hash = ? AND status = ?", keyHash, "active").First(&apiKey).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("invalid API key")
		}
		return nil, err
	}

	rateMult := 1.0
	if apiKey.GroupID != nil {
		var group model.Group
		if err := pr.DB.WithContext(ctx).First(&group, *apiKey.GroupID).Error; err == nil {
			rateMult = group.RateMultiplier
		}
	}

	cached := &infra.CachedKey{
		UserID:    apiKey.UserID,
		ApiKeyID:  apiKey.ID,
		GroupID:   apiKey.GroupID,
		RateMult:  rateMult,
		Status:    apiKey.Status,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	if pr.APIKeyCache != nil {
		pr.APIKeyCache.Set(keyHash, cached)
	}

	// Update LastUsedAt asynchronously (fire-and-forget).
	go func(id uint) {
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if pr.DB != nil {
			pr.DB.WithContext(bg).Model(&model.ApiKey{}).Where("id = ?", id).Update("last_used_at", time.Now())
		}
	}(apiKey.ID)

	return cached, nil
}

// -----------------------------------------------------------------------------
// Shared helpers
// -----------------------------------------------------------------------------

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(strings.TrimSpace(header))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func setBillingContext(c *gin.Context, bc *BillingCtx) {
	c.Set(billingCtxGinKey, bc)
	ctx := context.WithValue(c.Request.Context(), billingContextKey{}, bc)
	c.Request = c.Request.WithContext(ctx)
}

func billingContextFromGin(c *gin.Context) (*BillingCtx, bool) {
	value, ok := c.Get(billingCtxGinKey)
	if !ok {
		return BillingContextFromContext(c.Request.Context())
	}
	bc, ok := value.(*BillingCtx)
	return bc, ok
}

func traceIDFromGin(c *gin.Context) string {
	if value, ok := c.Get(traceIDHeader); ok {
		if requestID, ok := value.(string); ok && requestID != "" {
			return requestID
		}
	}
	if requestID := strings.TrimSpace(c.Writer.Header().Get(traceIDHeader)); requestID != "" {
		return requestID
	}
	if requestID := strings.TrimSpace(c.GetHeader(traceIDHeader)); requestID != "" {
		return requestID
	}
	return uuid.NewString()
}

// -----------------------------------------------------------------------------
// Rate-limit bucket (process-wide; keyed by identity)
// -----------------------------------------------------------------------------

type rateLimitBucket struct {
	mu       sync.Mutex
	window   time.Time
	used     int
	capacity int
}

var userLimiters sync.Map

func allowRequest(identity string, capacity int) bool {
	if identity == "" {
		identity = "anonymous"
	}
	if capacity <= 0 {
		capacity = defaultRateLimitPerMin
	}
	now := time.Now().UTC().Truncate(time.Minute)
	value, _ := userLimiters.LoadOrStore(identity, &rateLimitBucket{window: now, capacity: capacity})
	bucket := value.(*rateLimitBucket)

	bucket.mu.Lock()
	defer bucket.mu.Unlock()
	if bucket.window != now {
		bucket.window = now
		bucket.used = 0
		bucket.capacity = capacity
	}
	if bucket.used >= bucket.capacity {
		return false
	}
	bucket.used++
	return true
}

// -----------------------------------------------------------------------------
// Metrics store (per PanelRouter instance)
// -----------------------------------------------------------------------------

type metricsStore struct {
	mu              sync.RWMutex
	startedAt       time.Time
	totalRequests   uint64
	inFlight        int64
	statusCounts    map[int]uint64
	pathCounts      map[string]uint64
	totalLatencySum time.Duration
}

func newMetricsStore() *metricsStore {
	return &metricsStore{startedAt: time.Now()}
}

func (m *metricsStore) start(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.totalRequests++
	m.inFlight++
	if m.pathCounts == nil {
		m.pathCounts = make(map[string]uint64)
	}
	m.pathCounts[metricPath(path)]++
}

func (m *metricsStore) finish(status int, latency time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inFlight > 0 {
		m.inFlight--
	}
	if m.statusCounts == nil {
		m.statusCounts = make(map[int]uint64)
	}
	m.statusCounts[status]++
	m.totalLatencySum += latency
}

func metricPath(path string) string {
	if path == "" {
		return "unmatched"
	}
	return path
}

// MetricsHandler returns a JSON snapshot of the routed-request metrics.
func (pr *PanelRouter) MetricsHandler(c *gin.Context) {
	m := pr.metrics
	m.mu.RLock()
	defer m.mu.RUnlock()

	statusCounts := make(map[int]uint64, len(m.statusCounts))
	for k, v := range m.statusCounts {
		statusCounts[k] = v
	}
	pathCounts := make(map[string]uint64, len(m.pathCounts))
	for k, v := range m.pathCounts {
		pathCounts[k] = v
	}

	avg := 0.0
	if m.totalRequests > 0 {
		avg = float64(m.totalLatencySum.Milliseconds()) / float64(m.totalRequests)
	}

	Success(c, gin.H{
		"started_at":         m.startedAt.UTC().Format(time.RFC3339),
		"uptime_seconds":     int64(time.Since(m.startedAt).Seconds()),
		"total_requests":     m.totalRequests,
		"in_flight":          m.inFlight,
		"status_counts":      statusCounts,
		"path_counts":        pathCounts,
		"total_latency_ms":   m.totalLatencySum.Milliseconds(),
		"average_latency_ms": avg,
	})
}
