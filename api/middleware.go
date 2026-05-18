package api

import (
	"context"
	"errors"
	"log/slog"
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
			// The JWT email claim is presentation data, not an authorization
			// source. Handlers that need trusted identity state must load it by
			// user_id from the database.
			bc.Status = "active"
			bc.AuthType = authTypeJWT
		}

		// User-status recheck (Requirement 4.3/4.4/4.7, Property 11). The
		// primary credential check may succeed against a stale APIKeyCache
		// entry or against a JWT issued before the owning user was
		// suspended/deleted. userIsActive re-confirms the DB-side status
		// via the shared UserStatusCache so a flip observed on /v1/* is
		// immediately honored on /api/panel/** and vice versa.
		//
		// API Key path: if the cached ApiKey.Status is already non-active
		// (e.g. admin disabled the key between cache populations), reject
		// with the same opaque invalid_credentials body before user-status
		// lookup — we do not need to hit the DB to know the key is dead.
		//
		// JWT path: the claims have no Status field, so we always go through
		// userIsActive.
		if bc.Status != "" && bc.Status != userStatusActive {
			slog.Info("panel_auth_rejected_inactive_api_key",
				"event", "user_inactive",
				"auth_type", bc.AuthType,
				"user_id", bc.UserID,
				"api_key_status", bc.Status,
			)
			Error(c, http.StatusUnauthorized, middlewareErrorUnauthorized, "invalid_credentials")
			c.Abort()
			return
		}
		if bc.AuthType == authTypeJWT {
			if !pr.userIsActive(c.Request.Context(), bc.UserID) {
				slog.Info("panel_auth_rejected_inactive_jwt",
					"event", "user_inactive",
					"auth_type", bc.AuthType,
					"user_id", bc.UserID,
				)
				Error(c, http.StatusUnauthorized, middlewareErrorUnauthorized, "invalid_credentials")
				c.Abort()
				return
			}
		}

		setBillingContext(c, bc)
		c.Next()
	}
}

// userIsActive reports whether the user row for userID currently has
// Status == userStatusActive. It mirrors sdk/access.go's userIsActive
// (they share the injected UserStatusCache instance) so a status flip
// observed by either surface shows up on the other within the TTL.
//
// Lookup order:
//  1. UserStatusCache — O(1) in-memory hit.
//  2. SELECT status FROM users WHERE id = ? — single-column DB read on miss.
//
// Negative results are cached alongside positive ones so a credential-
// guessing burst against the same bogus userID does not hammer the DB.
// Transient DB errors are not cached (return false without updating the
// cache) so a connectivity blip does not pin a live user into "inactive"
// for the full TTL.
func (pr *PanelRouter) userIsActive(ctx context.Context, userID uint) bool {
	if pr == nil || pr.DB == nil || userID == 0 {
		return false
	}

	if pr.UserStatusCache != nil {
		if us, ok := pr.UserStatusCache.Get(userID); ok {
			return us.Status == userStatusActive
		}
	}

	var status string
	err := pr.DB.WithContext(ctx).
		Model(&model.User{}).
		Select("status").
		Where("id = ?", userID).
		Limit(1).
		Scan(&status).Error
	if err != nil {
		return false
	}

	cached := status
	if cached == "" {
		cached = "missing"
	}
	if pr.UserStatusCache != nil {
		pr.UserStatusCache.Set(userID, cached, 5*time.Minute)
	}

	return status == userStatusActive
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

// invalidateUserCaches flushes both L1 caches for the given user. Called by
// admin paths that flip a user's status or delete a user so the next auth
// attempt re-reads the database instead of seeing a stale "active" entry.
//
// Order of operations (design.md §"Admin delete hook"):
//  1. Drop the UserStatusCache entry so JWT-path user-status rechecks fall
//     through to the DB on the next request.
//  2. Enumerate every api_keys row for the user (no status filter — already
//     deleted or suspended keys may still be cached) and drop each cached
//     entry keyed by key_hash.
//
// Both caches are nil-safe: if the router was constructed without a cache
// (tests that do not exercise recheck paths), the corresponding step is a
// no-op. A DB failure at step 2 is logged and swallowed; step 1 must still
// run so that at least the status recheck path is forced to re-read. This
// keeps the admin handler progressable even under transient DB errors.
func (pr *PanelRouter) invalidateUserCaches(ctx context.Context, userID uint) {
	if pr == nil {
		return
	}

	// Step 1: flush user-status cache unconditionally so that JWT recheck
	// (middleware) and sdk.AccessProvider both observe the DB-side change.
	if pr.UserStatusCache != nil {
		pr.UserStatusCache.InvalidateUser(userID)
	}

	// Step 2: flush every cached API-key entry that belongs to this user.
	// We purposefully SELECT all rows (no status='active' filter) so that
	// keys flipped to deleted/suspended prior to this call are also evicted.
	// The api_keys row itself is not modified here.
	if pr.APIKeyCache == nil || pr.DB == nil {
		return
	}

	var keyHashes []string
	if err := pr.DB.WithContext(ctx).
		Model(&model.ApiKey{}).
		Where("user_id = ?", userID).
		Pluck("key_hash", &keyHashes).Error; err != nil {
		slog.Warn("invalidate_user_caches_db_failed",
			"event", "invalidate_user_caches_db_failed",
			"user_id", userID,
			"err", err,
		)
		return
	}

	for _, h := range keyHashes {
		pr.APIKeyCache.Delete(h)
	}
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

func init() {
	// Periodically evict stale rate-limit buckets to prevent unbounded
	// memory growth from unique client identities.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		for range ticker.C {
			cutoff := time.Now().UTC().Truncate(time.Minute)
			userLimiters.Range(func(key, value any) bool {
				bucket, ok := value.(*rateLimitBucket)
				if !ok {
					userLimiters.Delete(key)
					return true
				}
				bucket.mu.Lock()
				stale := bucket.window.Before(cutoff)
				bucket.mu.Unlock()
				if stale {
					userLimiters.Delete(key)
				}
				return true
			})
		}
	}()
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
