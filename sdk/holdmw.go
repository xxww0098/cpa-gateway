package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/xxww0098/cpa-gateway/infra"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

// defaultTopUpURL is the default top-up URL hint returned in structured
// 402 responses when the user has insufficient balance.
const defaultTopUpURL = "/api/panel/billing/topup"

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

	// Optional components — nil-safe. When nil, the corresponding check
	// is skipped, preserving backward compatibility with existing tests
	// and deployments that haven't wired these up yet.
	rateLimiter      *infra.RateLimiter
	circuitBreaker   *infra.CircuitBreaker
	idempotencyMgr   *IdempotencyManager
	budgetTokenStore *BudgetTokenStore
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

// SetRateLimiter injects an optional distributed rate limiter.
func (m *HoldMiddleware) SetRateLimiter(rl *infra.RateLimiter) {
	m.rateLimiter = rl
}

// SetCircuitBreaker injects an optional per-provider circuit breaker.
func (m *HoldMiddleware) SetCircuitBreaker(cb *infra.CircuitBreaker) {
	m.circuitBreaker = cb
}

// SetIdempotencyManager injects an optional idempotency deduplication manager.
func (m *HoldMiddleware) SetIdempotencyManager(im *IdempotencyManager) {
	m.idempotencyMgr = im
}

// SetBudgetTokenStore injects an optional process-local budget token store.
func (m *HoldMiddleware) SetBudgetTokenStore(bts *BudgetTokenStore) {
	m.budgetTokenStore = bts
}

// Handle is the gin.HandlerFunc entry point.
//
// The flow is:
//  1. Skip non /v1/* paths.
//  2. Read the auth metadata produced by the SDK AuthMiddleware.
//  3. Peek the body to extract model + stream.
//  4. RateLimiter check (fail-open on Redis error).
//  5. IdempotencyManager check (return cached response if duplicate).
//  6. CircuitBreaker check (503 if provider broken).
//  7. If a subscription is attached, reset stale usage counters and
//     enforce daily / weekly / monthly limits against the estimate.
//  8. BudgetToken flow: TryDeduct local → fallback to Redis Hold.
//  9. Inject the SettleCtx and run the downstream handler.
//  10. On non-2xx response, Release the hold in a deferred callback.
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

	// Extract extended metadata fields for SettleCtx.
	apiKeyID := parseOptionalUintValue(meta, "api_key_id")
	groupID := parseOptionalUintMetadata(meta, "group_id")

	modelName, streaming, maxTokens, _ := parseRequestModelStream(c)

	requestID := traceIDFromRequest(c)

	// Extract IP address from request.
	ipAddress := extractIPAddress(c)

	// Extract idempotency key from request headers.
	idempotencyKey := extractIdempotencyKey(c)

	// --- Rate Limiter Check (fail-open on Redis error) ---
	if m.rateLimiter != nil {
		identity := strconv.FormatUint(uint64(userID), 10)
		// Use a default token count estimate for rate limiting purposes.
		// The actual token count is unknown at this point; use 1 as a
		// request-count proxy.
		allowed, _ := m.rateLimiter.Allow(c.Request.Context(), identity, 1, modelName, groupID)
		if !allowed {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":   "Too Many Requests",
				"message": "rate limit exceeded",
			})
			return
		}
	}

	// --- Idempotency Check (return cached response if duplicate) ---
	if m.idempotencyMgr != nil && idempotencyKey != "" {
		cached, found, checkErr := m.idempotencyMgr.Check(c.Request.Context(), idempotencyKey)
		if checkErr != nil {
			// Log but don't block — idempotency is best-effort on error.
			slog.Warn("HoldMiddleware: idempotency check error",
				"error", checkErr, "key", idempotencyKey)
		} else if found && cached != nil {
			// Return the cached response directly without creating a new Hold.
			for k, v := range cached.Headers {
				c.Header(k, v)
			}
			c.Data(cached.StatusCode, "application/json", cached.Body)
			c.Abort()
			return
		}
	}

	// --- Circuit Breaker Check ---
	// Extract provider from the request path (e.g., /v1/chat/completions → infer from model).
	// For simplicity, we use the model name as the provider identifier for circuit breaking.
	if m.circuitBreaker != nil && modelName != "" {
		provider := inferProvider(modelName)
		if provider != "" {
			allowed, _ := m.circuitBreaker.Allow(c.Request.Context(), provider)
			if !allowed {
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
					"error":   "Service Unavailable",
					"message": "provider temporarily unavailable",
				})
				return
			}
		}
	}

	// --- Outstanding-Debt Preflight ---
	// A user carrying an unresolved shortfall (a settle row whose
	// metadata.shortfall_usd has no matching shortfall_resolve: credit)
	// must not be permitted to accumulate more billable work until the
	// debt is cleared (Requirement 2.5 / 2.6). An error from the ledger
	// lookup fails closed: we refuse the request so a transient DB hiccup
	// cannot let a debtor slip through. No Redis hold is created.
	if outstanding, err := m.ledger.HasUnresolvedShortfall(c.Request.Context(), userID); err != nil {
		slog.Warn("hold_middleware_shortfall_lookup_failed",
			"event", "shortfall_lookup_failed",
			"user_id", userID,
			"path", c.Request.URL.Path,
			"err", err,
		)
		c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{"error": "outstanding_debt"})
		return
	} else if outstanding {
		slog.Warn("hold_middleware_outstanding_debt_block",
			"event", "outstanding_debt_block",
			"user_id", userID,
			"path", c.Request.URL.Path,
		)
		c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{"error": "outstanding_debt"})
		return
	}

	// Subscription quota handling: reset any stale period counters and
	// reject requests whose estimated cost would exceed the limit.
	estimatedCost := m.calc.Estimate(modelName, streaming, rateMult)
	if subscriptionID != nil && m.db != nil {
		if reason, ok := m.checkSubscriptionQuota(c.Request.Context(), *subscriptionID, estimatedCost); !ok {
			abortJSON(c, http.StatusPaymentRequired, reason)
			return
		}
	}

	// --- Upper-Bound Preflight (Requirement 2.1) ---
	// Compute a conservative upper bound on what the request could cost
	// and reject before creating a Redis hold when the user's available
	// balance cannot cover even the worst case. The reserved hold amount
	// remains estimatedCost (the generous streaming estimate embeds a 2x
	// multiplier); the upper bound is only used for the balance gate.
	//
	// upperBound = max( holdAmount, EstimateWithMaxTokens, Estimate(stream=true) )
	//
	// EstimateWithMaxTokens tightens the bound when the client supplies a
	// max_tokens / max_completion_tokens cap; Estimate(..., true, ...)
	// guards the case where the cap is absent / absurdly large.
	estWithMax := m.calc.EstimateWithMaxTokens(modelName, maxTokens, streaming, rateMult)
	estStream := m.calc.Estimate(modelName, true, rateMult)
	upperBound := math.Max(estimatedCost, math.Max(estWithMax, estStream))

	if querier, ok := m.ledger.(BalanceQuerier); ok {
		avail, _ := querier.GetBalance(c.Request.Context(), userID)
		if avail < upperBound {
			slog.Warn("hold_middleware_preflight_insufficient_balance",
				"event", "preflight_insufficient_balance",
				"user_id", userID,
				"model", modelName,
				"upper_bound", upperBound,
				"avail", avail,
			)
			// Structured 402 mirrors abortInsufficientBalance so clients
			// see a uniform error shape whether the rejection happens at
			// preflight (no Redis hold) or inside Ledger.Hold.
			c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{
				"error":           "insufficient_balance",
				"message":         "insufficient balance",
				"current_balance": avail,
				"required_amount": upperBound,
				"top_up_url":      defaultTopUpURL,
			})
			return
		}
	}

	// --- Budget Token + Hold Flow ---
	// Try local BudgetToken deduction first; fall back to Redis Hold.
	usedBudgetToken := false
	if m.budgetTokenStore != nil {
		if m.budgetTokenStore.TryDeduct(userID, estimatedCost) {
			usedBudgetToken = true
		}
	}

	if !usedBudgetToken {
		if err := m.ledger.Hold(c.Request.Context(), userID, estimatedCost, requestID, m.ttl); err != nil {
			// Return structured 402 JSON with current_balance, required_amount, top_up_url
			// if the error is due to insufficient balance.
			m.abortInsufficientBalance(c, err, userID, estimatedCost)
			return
		}
	}

	sc := &SettleCtx{
		RequestID:      requestID,
		UserID:         userID,
		ApiKeyID:       apiKeyID,
		GroupID:        groupID,
		RateMult:       rateMult,
		SubscriptionID: subscriptionID,
		Model:          modelName,
		Stream:         streaming,
		IPAddress:      ipAddress,
		IdempotencyKey: idempotencyKey,
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
			if !usedBudgetToken {
				_ = m.ledger.Release(releaseCtx, userID, requestID)
			}
			released = true
		}
	}()

	c.Next()
}

// abortInsufficientBalance writes a structured 402 JSON response with
// current_balance, required_amount, and top_up_url when the Hold fails
// due to insufficient balance. For other Hold errors, it falls back to
// a generic payment required message.
func (m *HoldMiddleware) abortInsufficientBalance(c *gin.Context, holdErr error, userID uint, requiredAmount float64) {
	msg := holdErr.Error()
	isInsufficient := strings.Contains(strings.ToLower(msg), "insufficient")

	if isInsufficient {
		// Try to get the current balance for the structured response.
		var currentBalance float64
		if querier, ok := m.ledger.(BalanceQuerier); ok {
			bal, balErr := querier.GetBalance(c.Request.Context(), userID)
			if balErr == nil {
				currentBalance = bal
			}
		}

		c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{
			"error":           "insufficient_balance",
			"message":         "insufficient balance",
			"current_balance": currentBalance,
			"required_amount": requiredAmount,
			"top_up_url":      defaultTopUpURL,
		})
		return
	}

	abortJSON(c, http.StatusPaymentRequired, paymentErrorMessage(holdErr))
}

// checkSubscriptionQuota loads the subscription, rotates any stale
// period counters to their next reset boundary, and returns false along
// with a human-readable reason if the estimated cost would exceed any
// limit. A missing subscription row is treated as permissive (the
// quota system is opt-in; a user without an active subscription is
// billed purely from their balance).
//
// The entire check runs inside an explicit database transaction so that
// the FOR UPDATE row lock serializes concurrent requests against the
// same subscription. Only the quota read/reset/check lives inside the
// transaction to minimize lock hold duration (Requirement 4.5).
func (m *HoldMiddleware) checkSubscriptionQuota(ctx context.Context, subID uint, estimatedCost float64) (string, bool) {
	var reason string
	allowed := true

	txErr := m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var sub model.Subscription
		// SELECT ... FOR UPDATE within the transaction serializes
		// concurrent quota checks for the same subscription row.
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&sub, subID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				// Missing subscription is permissive — allow the request.
				return nil
			}
			reason = "subscription lookup failed"
			allowed = false
			return err
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
			// Persist the rotated counters within the same transaction
			// so the reset and quota evaluation are atomic.
			if err := tx.Save(&sub).Error; err != nil {
				reason = "subscription counter reset failed"
				allowed = false
				return err
			}
		}

		// Evaluate quota limits.
		if sub.DailyLimitUSD != nil && sub.DailyUsageUSD+estimatedCost > *sub.DailyLimitUSD {
			reason = "subscription daily quota exceeded"
			allowed = false
			return nil
		}
		if sub.WeeklyLimitUSD != nil && sub.WeeklyUsageUSD+estimatedCost > *sub.WeeklyLimitUSD {
			reason = "subscription weekly quota exceeded"
			allowed = false
			return nil
		}
		if sub.MonthlyLimitUSD != nil && sub.MonthlyUsageUSD+estimatedCost > *sub.MonthlyLimitUSD {
			reason = "subscription monthly quota exceeded"
			allowed = false
			return nil
		}

		return nil
	})

	if txErr != nil && allowed {
		// Transaction itself failed (e.g. DB connectivity issue) but we
		// haven't set a specific reason yet — fail closed.
		return "subscription quota check failed", false
	}

	return reason, allowed
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
// extracts the top-level "model", "stream", and output-token-limit
// fields. All are optional: a missing model returns an empty string
// (the pricing calculator will fall back to its default) and a missing
// stream defaults to false. maxTokens is resolved in the order
// max_tokens → max_completion_tokens → 0; a non-positive value means
// "no upper bound" so the preflight logic can fall back to the default
// streaming estimate.
//
// The final ok flag reports whether a JSON payload was successfully
// parsed. On JSON decode failure (or when the body is missing / not
// JSON / unreadable) the function returns the zero values
// (model="", stream=false, maxTokens=0, ok=false), matching the
// existing permissive failure semantics — the billing layer must not
// reject a request merely because the payload cannot be peeked.
//
// The body is restored to c.Request.Body so the downstream handler
// sees it unchanged.
func parseRequestModelStream(c *gin.Context) (model string, stream bool, maxTokens int64, ok bool) {
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
		return "", false, 0, false
	}
	// Only bother peeking when the content type looks like JSON.
	ct := strings.ToLower(c.GetHeader("Content-Type"))
	if ct != "" && !strings.Contains(ct, "json") {
		return "", false, 0, false
	}

	reader := io.LimitReader(c.Request.Body, holdRequestBodyLimit+1)
	body, err := io.ReadAll(reader)
	_ = c.Request.Body.Close()
	if err != nil {
		// Restore an empty body so gin does not choke later.
		c.Request.Body = io.NopCloser(bytes.NewReader(nil))
		return "", false, 0, false
	}
	if int64(len(body)) > holdRequestBodyLimit {
		body = body[:holdRequestBodyLimit]
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))

	if len(body) == 0 {
		return "", false, 0, false
	}

	// Use a permissive decoder: unknown / unexpected fields must not
	// cause the billing layer to reject a valid request.
	var payload struct {
		Model               string `json:"model"`
		Stream              bool   `json:"stream"`
		MaxTokens           int64  `json:"max_tokens"`
		MaxCompletionTokens int64  `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", false, 0, false
	}

	// Parse order: max_tokens → max_completion_tokens → 0. A
	// non-positive value means "unset", so we fall through to the
	// secondary field or return 0 so callers can use the default
	// streaming estimate as a safe upper bound.
	resolved := payload.MaxTokens
	if resolved <= 0 {
		resolved = payload.MaxCompletionTokens
	}
	if resolved < 0 {
		resolved = 0
	}
	return strings.TrimSpace(payload.Model), payload.Stream, resolved, true
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

// parseOptionalUintValue returns a uint from meta[key] or 0 if the
// entry is absent or malformed.
func parseOptionalUintValue(meta map[string]string, key string) uint {
	raw := strings.TrimSpace(meta[key])
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0
	}
	return uint(v)
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

// extractIPAddress extracts the client IP address from the request.
// It checks X-Forwarded-For, X-Real-IP headers first, then falls back
// to RemoteAddr.
func extractIPAddress(c *gin.Context) string {
	// X-Forwarded-For: first entry is the original client IP.
	if xff := c.GetHeader("X-Forwarded-For"); xff != "" {
		parts := strings.SplitN(xff, ",", 2)
		if ip := strings.TrimSpace(parts[0]); ip != "" {
			return ip
		}
	}
	// X-Real-IP header (set by some reverse proxies).
	if xri := c.GetHeader("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	// Fall back to RemoteAddr (host:port format).
	if c.Request.RemoteAddr != "" {
		host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
		if err == nil {
			return host
		}
		return c.Request.RemoteAddr
	}
	return ""
}

// extractIdempotencyKey reads the idempotency key from request headers.
// It checks both "Idempotency-Key" and "X-Idempotency-Key" headers.
func extractIdempotencyKey(c *gin.Context) string {
	if key := strings.TrimSpace(c.GetHeader("Idempotency-Key")); key != "" {
		return key
	}
	if key := strings.TrimSpace(c.GetHeader("X-Idempotency-Key")); key != "" {
		return key
	}
	return ""
}

// inferProvider maps a model name to a provider identifier for circuit
// breaking purposes. Returns empty string if the provider cannot be
// determined.
func inferProvider(modelName string) string {
	lower := strings.ToLower(modelName)
	switch {
	case strings.HasPrefix(lower, "gpt-") || strings.HasPrefix(lower, "o1") || strings.HasPrefix(lower, "o3") || strings.HasPrefix(lower, "o4"):
		return "openai"
	case strings.HasPrefix(lower, "claude-"):
		return "anthropic"
	case strings.HasPrefix(lower, "gemini-"):
		return "google"
	case strings.Contains(lower, "codex"):
		return "codex"
	default:
		return ""
	}
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
