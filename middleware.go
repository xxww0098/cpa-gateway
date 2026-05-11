package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	billingCtxGinKey = "billingCtx"
	traceIDHeader    = "X-Trace-ID"
	authTypeAPIKey   = "api_key"
	authTypeJWT      = "jwt"

	defaultHoldAmount           = 0.01
	defaultRateLimitPerMin      = 60
	defaultRequestBodyLimit     = 1 << 20
	defaultHoldTTL              = 5 * time.Minute
	middlewareErrorUnauthorized = 1001
	middlewareErrorPayment      = 2001
	middlewareErrorRateLimit    = 3001
	middlewareErrorInternal     = 5001
)

type billingContextKey struct{}

// BillingCtx is the authenticated request context shared by proxy and billing handlers.
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

// GlobalLedger is initialized by main.go when database/Redis are available.
var GlobalLedger *Ledger

var requestMetrics = &metricsStore{startedAt: time.Now()}
var userLimiters sync.Map

type metricsStore struct {
	mu              sync.RWMutex
	startedAt       time.Time
	totalRequests   uint64
	inFlight        int64
	statusCounts    map[int]uint64
	pathCounts      map[string]uint64
	totalLatencySum time.Duration
}

type rateLimitBucket struct {
	mu       sync.Mutex
	window   time.Time
	used     int
	capacity int
}

// BillingContextFromContext returns a BillingCtx previously injected into request context.
func BillingContextFromContext(ctx context.Context) (*BillingCtx, bool) {
	bc, ok := ctx.Value(billingContextKey{}).(*BillingCtx)
	return bc, ok
}

// AuthMiddleware validates Bearer JWT/API-key credentials and injects BillingCtx.
func AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			Error(c, http.StatusUnauthorized, middlewareErrorUnauthorized, "missing or invalid authorization bearer token")
			c.Abort()
			return
		}

		bc := &BillingCtx{RateMult: 1.0, RequestID: traceIDFromGin(c)}
		if strings.HasPrefix(token, "cpa-") {
			cached, err := ValidateAPIKey(token)
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
			claims, err := ValidateJWT(token)
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

// BillingMiddleware creates a short-lived balance hold for authenticated proxy requests.
func BillingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		bc, ok := billingContextFromGin(c)
		if !ok || bc.UserID == 0 {
			Error(c, http.StatusUnauthorized, middlewareErrorUnauthorized, "authentication context required")
			c.Abort()
			return
		}

		requestID := traceIDFromGin(c)
		bc.RequestID = requestID
		setBillingContext(c, bc)

		body, err := peekAndRestoreBody(c, defaultRequestBodyLimit)
		if err != nil {
			Error(c, http.StatusBadRequest, middlewareErrorInternal, "failed to read request body")
			c.Abort()
			return
		}

		estimatedCost := estimateRequestCost(body, bc.RateMult)
		ledger := GlobalLedger
		if ledger == nil {
			Error(c, http.StatusInternalServerError, middlewareErrorInternal, "billing ledger not initialized")
			c.Abort()
			return
		}

		if err := ledger.Hold(c.Request.Context(), bc.UserID, estimatedCost, requestID, holdTTL()); err != nil {
			if errors.Is(err, ErrInsufficientBalance) {
				Error(c, http.StatusPaymentRequired, middlewareErrorPayment, "insufficient balance")
			} else {
				Error(c, http.StatusInternalServerError, middlewareErrorInternal, "billing hold failed")
			}
			c.Abort()
			return
		}

		c.Next()
	}
}

// TraceIDMiddleware passes through or generates X-Trace-ID and makes it available to handlers.
func TraceIDMiddleware() gin.HandlerFunc {
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

// MetricsMiddleware records in-memory request counters for later /metrics exposure.
func MetricsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		requestMetrics.start(c.FullPath())
		defer func() {
			requestMetrics.finish(c.Writer.Status(), time.Since(start))
		}()
		c.Next()
	}
}

// RateLimitMiddleware applies a simple per-user/IP requests-per-minute limit.
func RateLimitMiddleware() gin.HandlerFunc {
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

// SetupMiddleware registers global middleware. Auth/Billing are exposed for route-group use.
func SetupMiddleware(r *gin.Engine) {
	r.Use(TraceIDMiddleware(), MetricsMiddleware(), RateLimitMiddleware())
}

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
	requestID := strings.TrimSpace(c.Writer.Header().Get(traceIDHeader))
	if requestID != "" {
		return requestID
	}
	requestID = strings.TrimSpace(c.GetHeader(traceIDHeader))
	if requestID != "" {
		return requestID
	}
	return uuid.NewString()
}

func peekAndRestoreBody(c *gin.Context, limit int64) ([]byte, error) {
	if c.Request.Body == nil {
		return nil, nil
	}
	reader := io.LimitReader(c.Request.Body, limit+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	c.Request.Body.Close()
	if int64(len(body)) > limit {
		body = body[:limit]
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func estimateRequestCost(body []byte, rateMult float64) float64 {
	cost := defaultHoldAmount
	if GlobalConfig != nil && GlobalConfig.Billing.HoldAmount > 0 {
		cost = float64(GlobalConfig.Billing.HoldAmount)
	}
	if len(body) > 0 {
		// MVP approximation: one token ~= four request bytes, priced per 1K tokens.
		estimatedTokens := float64(len(body)+3) / 4
		price := 0.0
		if GlobalConfig != nil {
			price = GlobalConfig.Billing.DefaultPricePer1KTokens
		}
		if price > 0 {
			cost = maxFloat(cost, (estimatedTokens/1000)*price)
		}
	}
	if rateMult <= 0 {
		rateMult = 1.0
	}
	return cost * rateMult
}

func holdTTL() time.Duration {
	if GlobalConfig != nil && GlobalConfig.Billing.HoldTTLSeconds > 0 {
		return time.Duration(GlobalConfig.Billing.HoldTTLSeconds) * time.Second
	}
	return defaultHoldTTL
}

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

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
