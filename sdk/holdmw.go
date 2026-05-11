package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/gorm"
)

// holdRequestBodyLimit caps the number of bytes HoldMiddleware will read
// from the request body during pre-flight parsing. 1 MiB is plenty for
// an OpenAI-style chat completion payload and defends the server from
// arbitrarily large requests driving the middleware to OOM.
const holdRequestBodyLimit = 1 << 20

// v1PathPrefix is the prefix HoldMiddleware enforces billing on. Any
// other path (panel, health checks, static assets, etc.) skips the
// middleware entirely.
const v1PathPrefix = "/v1/"

// holdTraceHeader is the canonical HTTP trace header. HoldMiddleware
// honors an inbound value (so a caller can correlate the Hold with an
// existing request ID) and otherwise generates a UUID.
const holdTraceHeader = "X-Trace-ID"

// accessMetadataKey is the gin.Context key the CLIProxyAPI SDK's
// AuthMiddleware uses to expose access.Result.Metadata. Keep this in
// sync with internal/api/server.go:AuthMiddleware on the SDK side.
const accessMetadataKey = "accessMetadata"

// HoldMiddleware enforces pre-flight balance reservation and
// subscription quota checks for every /v1/* request. On success it
// installs a SettleCtx on the request context so that the executor and
// UsagePlugin can perform precise Settle/Release accounting later.
//
// The middleware is intentionally interface-driven (see BillingLedger
// and PricingCalculator in interfaces.go) so unit tests can exercise
// the 402, quota-reset, and upstream-error branches with in-memory fakes.
type HoldMiddleware struct {
	ledger BillingLedger
	calc   PricingCalculator
	db     *gorm.DB
	ttl    time.Duration
}

// NewHoldMiddleware constructs a HoldMiddleware. A non-positive ttl is
// replaced with a 5-minute default — holds must always expire to
// prevent a stuck goroutine from starving a user's balance indefinitely.
func NewHoldMiddleware(ledger BillingLedger, calc PricingCalculator, db *gorm.DB, ttl time.Duration) *HoldMiddleware {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &HoldMiddleware{ledger: ledger, calc: calc, db: db, ttl: ttl}
}

// Handle is the gin.HandlerFunc entry point.
//
// The flow is:
//  1. Skip non /v1/* paths.
//  2. Read the auth metadata produced by the SDK AuthMiddleware.
//  3. Peek the body to extract model + stream.
//  4. If a subscription is attached, reset stale usage counters and
//     enforce daily / weekly / monthly limits against the estimate.
//  5. Ask the ledger to Hold the estimated cost.
//  6. Inject the SettleCtx and run the downstream handler.
//  7. On non-2xx response, Release the hold in a deferred callback.
func (m *HoldMiddleware) Handle(c *gin.Context) {
	if !strings.HasPrefix(c.Request.URL.Path, v1PathPrefix) {
		c.Next()
		return
	}

	meta, ok := extractAccessMetadata(c)
	if !ok {
		// The SDK AuthMiddleware runs before HoldMiddleware; a missing
		// metadata value means authentication was skipped or failed in
		// an unexpected way. Fail closed so we never bill an
		// un-authenticated request.
		abortJSON(c, http.StatusUnauthorized, "authentication context required")
		return
	}

	userID, err := parseUintMetadata(meta, "user_id")
	if err != nil || userID == 0 {
		abortJSON(c, http.StatusUnauthorized, "invalid user_id in access metadata")
		return
	}

	rateMult := parseFloatMetadata(meta, "rate_mult", 1.0)
	if rateMult <= 0 {
		rateMult = 1.0
	}
	subscriptionID := parseOptionalUintMetadata(meta, "subscription_id")

	modelName, streaming := parseRequestModelStream(c)

	requestID := traceIDFromRequest(c)

	// Subscription quota handling: reset any stale period counters and
	// reject requests whose estimated cost would exceed the limit.
	estimatedCost := m.calc.Estimate(modelName, streaming, rateMult)
	if subscriptionID != nil && m.db != nil {
		if reason, ok := m.checkSubscriptionQuota(c.Request.Context(), *subscriptionID, estimatedCost); !ok {
			abortJSON(c, http.StatusPaymentRequired, reason)
			return
		}
	}

	if err := m.ledger.Hold(c.Request.Context(), userID, estimatedCost, requestID, m.ttl); err != nil {
		// We deliberately collapse all Hold errors to 402. Insufficient
		// balance is the dominant case; duplicate request IDs and
		// transient Redis errors are rare, and surfacing internal
		// detail on the billing surface is undesirable.
		abortJSON(c, http.StatusPaymentRequired, paymentErrorMessage(err))
		return
	}

	sc := &SettleCtx{
		RequestID:      requestID,
		UserID:         userID,
		RateMult:       rateMult,
		SubscriptionID: subscriptionID,
		Model:          modelName,
		Stream:         streaming,
	}
	c.Request = c.Request.WithContext(WithSettleCtx(c.Request.Context(), sc))

	// Register the release hook before running the handler so that a
	// panic in the downstream chain still frees the hold.
	released := false
	defer func() {
		if released {
			return
		}
		if c.Writer.Status() >= http.StatusBadRequest {
			// Fire-and-forget release. The original request context may
			// already be cancelled, so use a detached context with a
			// short timeout.
			releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = m.ledger.Release(releaseCtx, userID, requestID)
			released = true
		}
	}()

	c.Next()
}

// checkSubscriptionQuota loads the subscription, rotates any stale
// period counters to their next reset boundary, and returns false along
// with a human-readable reason if the estimated cost would exceed any
// limit. A missing subscription row is treated as permissive (the
// quota system is opt-in; a user without an active subscription is
// billed purely from their balance).
func (m *HoldMiddleware) checkSubscriptionQuota(ctx context.Context, subID uint, estimatedCost float64) (string, bool) {
	var sub model.Subscription
	if err := m.db.WithContext(ctx).First(&sub, subID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", true
		}
		return "subscription lookup failed", false
	}

	now := time.Now().UTC()
	dirty := false

	if !sub.DailyResetAt.IsZero() && now.After(sub.DailyResetAt) {
		sub.DailyUsageUSD = 0
		sub.DailyResetAt = model.NextDailyResetAfter(now)
		dirty = true
	}
	if !sub.WeeklyResetAt.IsZero() && now.After(sub.WeeklyResetAt) {
		sub.WeeklyUsageUSD = 0
		sub.WeeklyResetAt = model.NextWeeklyResetAfter(now)
		dirty = true
	}
	if !sub.MonthlyResetAt.IsZero() && now.After(sub.MonthlyResetAt) {
		sub.MonthlyUsageUSD = 0
		sub.MonthlyResetAt = model.NextMonthlyResetAfter(now)
		dirty = true
	}

	if dirty {
		// Persist the rotated counters before evaluating the quota.
		// Ignoring the error is deliberate: the quota check below is
		// safe against a partial write, and a DB flake should not turn
		// every in-flight request into a 402.
		_ = m.db.WithContext(ctx).Save(&sub).Error
	}

	if sub.DailyLimitUSD != nil && sub.DailyUsageUSD+estimatedCost > *sub.DailyLimitUSD {
		return "subscription daily quota exceeded", false
	}
	if sub.WeeklyLimitUSD != nil && sub.WeeklyUsageUSD+estimatedCost > *sub.WeeklyLimitUSD {
		return "subscription weekly quota exceeded", false
	}
	if sub.MonthlyLimitUSD != nil && sub.MonthlyUsageUSD+estimatedCost > *sub.MonthlyLimitUSD {
		return "subscription monthly quota exceeded", false
	}
	return "", true
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

// extractAccessMetadata pulls the access metadata map the SDK's
// AuthMiddleware installs on gin.Context.
func extractAccessMetadata(c *gin.Context) (map[string]string, bool) {
	raw, exists := c.Get(accessMetadataKey)
	if !exists {
		return nil, false
	}
	meta, ok := raw.(map[string]string)
	if !ok || meta == nil {
		return nil, false
	}
	return meta, true
}

// parseRequestModelStream reads the request body (with a size cap) and
// extracts the top-level "model" and "stream" fields. Both are
// optional: a missing model returns an empty string (the pricing
// calculator will fall back to its default) and a missing stream
// defaults to false. The body is restored to c.Request.Body so the
// downstream handler sees it unchanged.
func parseRequestModelStream(c *gin.Context) (string, bool) {
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
		return "", false
	}
	// Only bother peeking when the content type looks like JSON.
	ct := strings.ToLower(c.GetHeader("Content-Type"))
	if ct != "" && !strings.Contains(ct, "json") {
		return "", false
	}

	reader := io.LimitReader(c.Request.Body, holdRequestBodyLimit+1)
	body, err := io.ReadAll(reader)
	_ = c.Request.Body.Close()
	if err != nil {
		// Restore an empty body so gin does not choke later.
		c.Request.Body = io.NopCloser(bytes.NewReader(nil))
		return "", false
	}
	if int64(len(body)) > holdRequestBodyLimit {
		body = body[:holdRequestBodyLimit]
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))

	if len(body) == 0 {
		return "", false
	}

	// Use a permissive decoder: unknown / unexpected fields must not
	// cause the billing layer to reject a valid request.
	var payload struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", false
	}
	return strings.TrimSpace(payload.Model), payload.Stream
}

// traceIDFromRequest returns a stable request ID for use as the
// ledger Hold key. Preference order: existing gin key, request header,
// response header, generated UUID.
func traceIDFromRequest(c *gin.Context) string {
	if value, ok := c.Get(holdTraceHeader); ok {
		if id, ok := value.(string); ok && strings.TrimSpace(id) != "" {
			return id
		}
	}
	if id := strings.TrimSpace(c.GetHeader(holdTraceHeader)); id != "" {
		return id
	}
	if id := strings.TrimSpace(c.Writer.Header().Get(holdTraceHeader)); id != "" {
		return id
	}
	return uuid.NewString()
}

// parseUintMetadata parses a required uint value from the metadata
// map. An empty or malformed entry returns an error so callers can
// decide whether to hard-fail.
func parseUintMetadata(meta map[string]string, key string) (uint, error) {
	raw := strings.TrimSpace(meta[key])
	if raw == "" {
		return 0, errors.New("missing " + key)
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	return uint(v), nil
}

// parseOptionalUintMetadata returns a *uint from meta[key] or nil if
// the entry is absent or malformed. Absence is the common case for
// users without an active subscription.
func parseOptionalUintMetadata(meta map[string]string, key string) *uint {
	raw := strings.TrimSpace(meta[key])
	if raw == "" {
		return nil
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return nil
	}
	u := uint(v)
	return &u
}

// parseFloatMetadata reads a float from meta[key], falling back to
// defaultValue when the entry is missing or malformed.
func parseFloatMetadata(meta map[string]string, key string, defaultValue float64) float64 {
	raw := strings.TrimSpace(meta[key])
	if raw == "" {
		return defaultValue
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return defaultValue
	}
	return v
}

// abortJSON writes a JSON error payload and aborts the gin chain so no
// downstream handler runs.
func abortJSON(c *gin.Context, status int, msg string) {
	c.AbortWithStatusJSON(status, gin.H{
		"error":   http.StatusText(status),
		"message": msg,
	})
}

// paymentErrorMessage picks a user-facing message for a Hold error.
// We keep it short and avoid leaking Redis / Postgres diagnostics.
func paymentErrorMessage(err error) string {
	if err == nil {
		return "payment required"
	}
	msg := err.Error()
	if strings.Contains(strings.ToLower(msg), "insufficient") {
		return "insufficient balance"
	}
	return "payment required"
}
