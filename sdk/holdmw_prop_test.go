package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xxww0098/cpa-gateway/config"
	"github.com/xxww0098/cpa-gateway/infra"
	"github.com/xxww0098/cpa-gateway/model"
	"github.com/xxww0098/cpa-gateway/testutil"
	"pgregory.net/rapid"
)

// Feature: billing-system-optimization, Property 7: SettleCtx Field Propagation
//
// Property 7: For any request authenticated with an API key, the SettleCtx
// SHALL carry the ApiKeyID and GroupID values extracted from the access
// metadata, and these SHALL match the values in the metadata map.
//
// **Validates: Requirements 3.1, 3.5**

func TestProperty7_SettleCtxFieldPropagation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rapid.Check(t, func(rt *rapid.T) {
		// Generate random metadata values.
		userID := rapid.UintRange(1, 100000).Draw(rt, "userID")
		apiKeyID := rapid.UintRange(1, 50000).Draw(rt, "apiKeyID")
		groupID := rapid.UintRange(1, 10000).Draw(rt, "groupID")

		// Generate a random estimate value.
		estimateValue := float64(rapid.IntRange(1, 500).Draw(rt, "estimateCents")) / 100.0

		// Set up mock ledger (succeeds) and calculator.
		ldgr := &holdLedgerMock{}
		calc := &holdCalcMock{EstimateValue: estimateValue}
		db := newHoldTestDB(t)
		mw := NewHoldMiddleware(ldgr, calc, db, 5*time.Minute)

		// Build access metadata with api_key_id and group_id.
		meta := map[string]string{
			"user_id":    strconv.FormatUint(uint64(userID), 10),
			"rate_mult":  "1.0",
			"api_key_id": strconv.FormatUint(uint64(apiKeyID), 10),
			"group_id":   strconv.FormatUint(uint64(groupID), 10),
		}

		// Capture the SettleCtx from the downstream handler.
		var capturedSC *SettleCtx
		var scFound bool

		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set("accessMetadata", meta)
			c.Next()
		})
		r.Use(mw.Handle)
		r.Any("/*path", func(c *gin.Context) {
			capturedSC, scFound = SettleCtxFromContext(c.Request.Context())
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})

		// Build a /v1/ request with a model in the body.
		body := `{"model":"gpt-4o","stream":false}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		// Property: request must succeed (Hold succeeds).
		if w.Code != http.StatusOK {
			rt.Fatalf("expected status 200, got %d; body=%s", w.Code, w.Body.String())
		}

		// Property: SettleCtx must be present in the downstream context.
		if !scFound || capturedSC == nil {
			rt.Fatalf("SettleCtx not found in downstream handler context")
		}

		// Property: ApiKeyID in SettleCtx must match the metadata value.
		if capturedSC.ApiKeyID != uint(apiKeyID) {
			rt.Fatalf("SettleCtx.ApiKeyID=%d, want %d (from metadata)", capturedSC.ApiKeyID, apiKeyID)
		}

		// Property: GroupID in SettleCtx must match the metadata value.
		if capturedSC.GroupID == nil {
			rt.Fatalf("SettleCtx.GroupID is nil, want %d (from metadata)", groupID)
		}
		if *capturedSC.GroupID != uint(groupID) {
			rt.Fatalf("SettleCtx.GroupID=%d, want %d (from metadata)", *capturedSC.GroupID, groupID)
		}
	})
}

// TestProperty7_SettleCtxFieldPropagation_NoApiKey verifies that when the
// request is authenticated via JWT without an API key (no api_key_id or
// group_id in metadata), the SettleCtx carries ApiKeyID=0 and GroupID=nil.
//
// **Validates: Requirements 3.1, 3.5**
func TestProperty7_SettleCtxFieldPropagation_NoApiKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rapid.Check(t, func(rt *rapid.T) {
		// Generate random user ID (JWT-only auth, no API key).
		userID := rapid.UintRange(1, 100000).Draw(rt, "userID")
		estimateValue := float64(rapid.IntRange(1, 500).Draw(rt, "estimateCents")) / 100.0

		ldgr := &holdLedgerMock{}
		calc := &holdCalcMock{EstimateValue: estimateValue}
		db := newHoldTestDB(t)
		mw := NewHoldMiddleware(ldgr, calc, db, 5*time.Minute)

		// Metadata without api_key_id and group_id (JWT-only auth).
		meta := map[string]string{
			"user_id":   strconv.FormatUint(uint64(userID), 10),
			"rate_mult": "1.0",
		}

		var capturedSC *SettleCtx
		var scFound bool

		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set("accessMetadata", meta)
			c.Next()
		})
		r.Use(mw.Handle)
		r.Any("/*path", func(c *gin.Context) {
			capturedSC, scFound = SettleCtxFromContext(c.Request.Context())
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})

		body := `{"model":"gpt-4o","stream":false}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			rt.Fatalf("expected status 200, got %d; body=%s", w.Code, w.Body.String())
		}

		if !scFound || capturedSC == nil {
			rt.Fatalf("SettleCtx not found in downstream handler context")
		}

		// Property: ApiKeyID should be 0 when no api_key_id in metadata.
		if capturedSC.ApiKeyID != 0 {
			rt.Fatalf("SettleCtx.ApiKeyID=%d, want 0 (no API key in metadata)", capturedSC.ApiKeyID)
		}

		// Property: GroupID should be nil when no group_id in metadata.
		if capturedSC.GroupID != nil {
			rt.Fatalf("SettleCtx.GroupID=%v, want nil (no group in metadata)", capturedSC.GroupID)
		}
	})
}

// Feature: billing-system-optimization, Property 9: Subscription Quota Serialization
//
// Property 9: For any two requests checking the same subscription's quota where
// only one request's worth of quota remains, the database transaction SHALL
// serialize access such that exactly one request passes the quota check and the
// other is rejected.
//
// NOTE: SQLite does not support true row-level locking (FOR UPDATE is a no-op),
// so this test verifies the logic works correctly in a sequential scenario.
// True concurrent serialization requires PostgreSQL with FOR UPDATE row locks.
// The sequential test confirms that the quota accounting logic correctly rejects
// the second request when only one slot remains.
//
// **Validates: Requirements 4.1, 4.2, 4.4**

func TestProperty9_SubscriptionQuotaSerialization(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate random daily limit and cost such that exactly one request fits.
		// dailyLimit is the subscription's daily cap.
		// cost is the estimated cost per request.
		// We set dailyUsage = dailyLimit - cost (so exactly one more request fits).
		cost := rapid.Float64Range(0.01, 10.0).Draw(rt, "cost")
		// dailyLimit must be > cost so we can set usage = limit - cost
		dailyLimit := cost + rapid.Float64Range(0.01, 50.0).Draw(rt, "limitMargin")
		// Set usage so that exactly one more request of `cost` fits:
		// usage + cost <= dailyLimit (passes)
		// usage + 2*cost > dailyLimit (second would fail)
		dailyUsage := dailyLimit - cost // exactly one slot remaining

		// Ensure the second request would exceed: dailyUsage + cost <= dailyLimit
		// but dailyUsage + cost + cost > dailyLimit (since dailyUsage = dailyLimit - cost,
		// first: (dailyLimit - cost) + cost = dailyLimit <= dailyLimit ✓
		// second: dailyLimit + cost > dailyLimit ✓)

		// Set up in-memory SQLite DB.
		db := newHoldTestDB(t)

		// Create a subscription with the generated limits.
		future := time.Now().UTC().Add(24 * time.Hour)
		sub := &model.Subscription{
			UserID:         1,
			PackageID:      1,
			GroupID:        1,
			Status:         "active",
			StartsAt:       time.Now().UTC().Add(-24 * time.Hour),
			ExpiresAt:      future,
			DailyUsageUSD:  dailyUsage,
			DailyResetAt:   future, // far in the future, no reset
			WeeklyResetAt:  future,
			MonthlyResetAt: future,
			DailyLimitUSD:  &dailyLimit,
		}
		if err := db.Create(sub).Error; err != nil {
			rt.Fatalf("create subscription: %v", err)
		}

		// Create HoldMiddleware with the test DB.
		ldgr := &holdLedgerMock{}
		calc := &holdCalcMock{EstimateValue: cost}
		mw := NewHoldMiddleware(ldgr, calc, db, 5*time.Minute)

		// Call checkSubscriptionQuota twice sequentially (simulating serialized access).
		// First call: should pass (dailyUsage + cost <= dailyLimit).
		reason1, ok1 := mw.checkSubscriptionQuota(context.Background(), sub.ID, cost)

		// Second call: should be rejected (after first call, usage is at the limit).
		// Note: checkSubscriptionQuota does NOT increment usage — it only checks.
		// The actual usage increment happens in UsagePlugin after Settle.
		// However, the property we're testing is that the check itself correctly
		// evaluates the quota boundary. Since checkSubscriptionQuota only reads
		// and resets (no increment), we simulate the "second request" by manually
		// updating the usage to reflect that the first request consumed its slot.
		if ok1 {
			// Simulate the first request consuming its quota slot.
			db.Model(&model.Subscription{}).Where("id = ?", sub.ID).
				Update("daily_usage_usd", dailyUsage+cost)
		}

		reason2, ok2 := mw.checkSubscriptionQuota(context.Background(), sub.ID, cost)

		// Property: first call MUST pass.
		if !ok1 {
			rt.Fatalf("first quota check should pass but was rejected: %s (dailyUsage=%.6f, cost=%.6f, limit=%.6f)",
				reason1, dailyUsage, cost, dailyLimit)
		}

		// Property: second call MUST be rejected (quota exceeded).
		if ok2 {
			rt.Fatalf("second quota check should be rejected but passed (dailyUsage after first=%.6f, cost=%.6f, limit=%.6f)",
				dailyUsage+cost, cost, dailyLimit)
		}

		// Property: rejection reason should mention daily quota.
		if reason2 == "" {
			rt.Fatalf("expected non-empty rejection reason for second call")
		}
	})
}

// Feature: billing-system-optimization, Property 22: Circuit Breaker Hold Release
//
// Property 22: For any request to a circuit-broken provider, the HoldMiddleware
// SHALL return a 503 error without creating a hold. The ledger Hold method
// SHALL NOT be called when the circuit breaker rejects the provider.
//
// **Validates: Requirements 12.7**

func TestProperty22_CircuitBreakerHoldRelease(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate a random user ID.
		userID := rapid.UintRange(1, 10000).Draw(rt, "userID")

		// Generate a model name that maps to a known provider.
		modelNames := []string{
			"gpt-4o", "gpt-3.5-turbo", "gpt-4",
			"claude-3-opus", "claude-3-sonnet",
			"gemini-1.5-pro", "gemini-1.0-ultra",
		}
		modelIdx := rapid.IntRange(0, len(modelNames)-1).Draw(rt, "modelIdx")
		modelName := modelNames[modelIdx]

		// Generate a random estimate value.
		estimateValue := float64(rapid.IntRange(1, 1000).Draw(rt, "estimateCents")) / 100.0

		// Set up miniredis-backed circuit breaker.
		redisClient, _ := testutil.MustMiniRedis(t)

		cbCfg := config.CircuitBreakerConfig{
			FailureThreshold: 0.5,
			WindowSeconds:    30,
			CooldownSeconds:  60, // Long cooldown so it stays open during test.
		}
		cb := infra.NewCircuitBreaker(redisClient, cbCfg)

		// Trip the circuit breaker for the provider inferred from the model.
		provider := inferProvider(modelName)
		if provider == "" {
			rt.Fatalf("model %q should map to a known provider", modelName)
		}

		ctx := context.Background()
		// Record 5 failures (100% failure rate > 50% threshold) to trip the circuit.
		for i := 0; i < 5; i++ {
			if err := cb.RecordFailure(ctx, provider); err != nil {
				rt.Fatalf("unexpected error recording failure: %v", err)
			}
		}

		// Verify circuit is open before proceeding.
		state, err := cb.State(ctx, provider)
		if err != nil {
			rt.Fatalf("unexpected error getting state: %v", err)
		}
		if state != infra.CircuitOpen {
			rt.Fatalf("expected circuit to be open, got %s", state)
		}

		// Set up mock ledger — Hold should NOT be called.
		ldgr := &holdLedgerMock{}
		calc := &holdCalcMock{EstimateValue: estimateValue}
		db := newHoldTestDB(t)
		mw := NewHoldMiddleware(ldgr, calc, db, 5*time.Minute)
		mw.SetCircuitBreaker(cb)

		// Build gin engine with access metadata.
		gin.SetMode(gin.TestMode)
		engine := gin.New()

		meta := map[string]string{
			"user_id":   strconv.FormatUint(uint64(userID), 10),
			"rate_mult": "1.0",
		}

		// Inject access metadata like the SDK AuthMiddleware would.
		engine.Use(func(c *gin.Context) {
			c.Set("accessMetadata", meta)
			c.Next()
		})
		engine.Use(mw.Handle)

		// Terminal handler — should never be reached.
		handlerCalled := false
		engine.Any("/*path", func(c *gin.Context) {
			handlerCalled = true
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})

		// Build request with the model in the body.
		body := `{"model":"` + modelName + `","stream":false}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		// Assert: 503 Service Unavailable returned.
		if w.Code != http.StatusServiceUnavailable {
			rt.Fatalf("expected status 503, got %d; body=%s", w.Code, w.Body.String())
		}

		// Assert: response body contains expected error structure.
		var respBody map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &respBody); err != nil {
			rt.Fatalf("failed to parse response body: %v", err)
		}
		if respBody["error"] != "Service Unavailable" {
			rt.Fatalf("expected error='Service Unavailable', got %v", respBody["error"])
		}

		// Assert: Hold was NOT called (no hold created for circuit-broken provider).
		ldgr.mu.Lock()
		holdCount := len(ldgr.HoldCalls)
		ldgr.mu.Unlock()
		if holdCount != 0 {
			rt.Fatalf("expected 0 Hold calls when circuit is open, got %d", holdCount)
		}

		// Assert: downstream handler was NOT called.
		if handlerCalled {
			rt.Fatalf("downstream handler should not be called when circuit breaker rejects")
		}
	})
}

// Feature: billing-system-optimization, Property 25: Insufficient Balance Error Response

// insufficientBalanceLedgerMock is a mock BillingLedger that always returns
// an "insufficient balance" error from Hold. It also implements BalanceQuerier
// to provide the current balance in structured 402 responses.
type insufficientBalanceLedgerMock struct {
	balance float64
}

func (m *insufficientBalanceLedgerMock) Hold(_ context.Context, _ uint, _ float64, _ string, _ time.Duration) error {
	return errors.New("insufficient balance")
}

func (m *insufficientBalanceLedgerMock) Settle(_ context.Context, _ uint, _ string, _ float64) error {
	return nil
}

func (m *insufficientBalanceLedgerMock) Release(_ context.Context, _ uint, _ string) error {
	return nil
}

// ActiveHoldAmount satisfies the BillingLedger interface added in Stage 2
// (task 5.1). This mock always rejects Hold before any state is recorded,
// so there is never an active hold to report.
func (m *insufficientBalanceLedgerMock) ActiveHoldAmount(_ context.Context, _ uint, _ string) (float64, bool, error) {
	return 0, false, nil
}

// HasUnresolvedShortfall satisfies the BillingLedger interface extension
// added in task 1.6. This mock is used by the insufficient-balance test
// path which runs before any shortfall recording could occur; returning
// (false, nil) keeps the preflight chain moving so we can observe the
// intended Hold rejection.
func (m *insufficientBalanceLedgerMock) HasUnresolvedShortfall(_ context.Context, _ uint) (bool, error) {
	return false, nil
}

func (m *insufficientBalanceLedgerMock) GetBalance(_ context.Context, _ uint) (float64, error) {
	return m.balance, nil
}

// Compile-time check that the mock satisfies both interfaces.
var (
	_ BillingLedger  = (*insufficientBalanceLedgerMock)(nil)
	_ BalanceQuerier = (*insufficientBalanceLedgerMock)(nil)
)

// TestProperty25_InsufficientBalanceErrorResponse verifies that for any Hold
// rejection due to insufficient balance, the error response is a 402 JSON
// containing current_balance, required_amount, and top_up_url fields.
//
// The test uses a mock ledger that always returns ErrInsufficientBalance and
// implements BalanceQuerier to provide the current balance. It generates
// random user IDs, balance values, and estimated costs to verify the property
// holds universally.
//
// **Validates: Requirements 14.2, 14.5**
func TestProperty25_InsufficientBalanceErrorResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rapid.Check(t, func(rt *rapid.T) {
		// Generate random parameters
		userID := uint(rapid.IntRange(1, 100000).Draw(rt, "userID"))
		currentBalance := rapid.Float64Range(0.0, 100.0).Draw(rt, "currentBalance")
		estimatedCost := rapid.Float64Range(0.01, 500.0).Draw(rt, "estimatedCost")

		// Create a mock ledger that returns insufficient balance and implements BalanceQuerier
		ldgr := &insufficientBalanceLedgerMock{
			balance: currentBalance,
		}

		calc := &holdCalcMock{EstimateValue: estimatedCost}
		db := newHoldTestDB(t)
		mw := NewHoldMiddleware(ldgr, calc, db, 5*time.Minute)

		// Build gin engine with access metadata
		meta := map[string]string{
			"user_id":   strconv.FormatUint(uint64(userID), 10),
			"rate_mult": "1.0",
		}

		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set("accessMetadata", meta)
			c.Next()
		})
		r.Use(mw.Handle)
		r.Any("/*path", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})

		// Build a /v1/ request with a model
		modelName := fmt.Sprintf("gpt-%d", rapid.IntRange(1, 100).Draw(rt, "modelSuffix"))
		body := `{"model":"` + modelName + `","stream":false}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		// Property: response status MUST be 402
		if w.Code != http.StatusPaymentRequired {
			rt.Fatalf("expected status 402, got %d; body=%s", w.Code, w.Body.String())
		}

		// Property: response body MUST be valid JSON with required fields
		var respBody map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &respBody); err != nil {
			rt.Fatalf("response body is not valid JSON: %v; raw=%s", err, w.Body.String())
		}

		// Property: MUST contain "current_balance" field
		cbRaw, hasCB := respBody["current_balance"]
		if !hasCB {
			rt.Fatalf("response missing 'current_balance' field; body=%v", respBody)
		}
		cbVal, ok := cbRaw.(float64)
		if !ok {
			rt.Fatalf("'current_balance' is not a number: %T=%v", cbRaw, cbRaw)
		}
		// The current_balance should match what the BalanceQuerier returns
		const epsilon = 1e-9
		diff := cbVal - currentBalance
		if diff > epsilon || diff < -epsilon {
			rt.Fatalf("current_balance=%f, want %f (from BalanceQuerier)", cbVal, currentBalance)
		}

		// Property: MUST contain "required_amount" field
		raRaw, hasRA := respBody["required_amount"]
		if !hasRA {
			rt.Fatalf("response missing 'required_amount' field; body=%v", respBody)
		}
		raVal, ok := raRaw.(float64)
		if !ok {
			rt.Fatalf("'required_amount' is not a number: %T=%v", raRaw, raRaw)
		}
		// The required_amount should match the estimated cost
		diff = raVal - estimatedCost
		if diff > epsilon || diff < -epsilon {
			rt.Fatalf("required_amount=%f, want %f (estimatedCost)", raVal, estimatedCost)
		}

		// Property: MUST contain "top_up_url" field (non-empty string)
		tuRaw, hasTU := respBody["top_up_url"]
		if !hasTU {
			rt.Fatalf("response missing 'top_up_url' field; body=%v", respBody)
		}
		tuVal, ok := tuRaw.(string)
		if !ok {
			rt.Fatalf("'top_up_url' is not a string: %T=%v", tuRaw, tuRaw)
		}
		if tuVal == "" {
			rt.Fatalf("'top_up_url' is empty string")
		}
	})
}

// TestProperty25_InsufficientBalanceErrorResponse_WithoutQuerier verifies that
// even when the ledger does NOT implement BalanceQuerier, the 402 response
// still contains the required structured fields (current_balance defaults to 0).
//
// **Validates: Requirements 14.2, 14.5**
func TestProperty25_InsufficientBalanceErrorResponse_WithoutQuerier(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rapid.Check(t, func(rt *rapid.T) {
		userID := uint(rapid.IntRange(1, 100000).Draw(rt, "userID"))
		estimatedCost := rapid.Float64Range(0.01, 500.0).Draw(rt, "estimatedCost")

		// Use the holdLedgerMock which does NOT implement BalanceQuerier
		ldgr := &holdLedgerMock{HoldErr: errors.New("insufficient balance")}
		calc := &holdCalcMock{EstimateValue: estimatedCost}
		db := newHoldTestDB(t)
		mw := NewHoldMiddleware(ldgr, calc, db, 5*time.Minute)

		meta := map[string]string{
			"user_id":   strconv.FormatUint(uint64(userID), 10),
			"rate_mult": "1.0",
		}

		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set("accessMetadata", meta)
			c.Next()
		})
		r.Use(mw.Handle)
		r.Any("/*path", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})

		body := `{"model":"gpt-4o","stream":false}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		// Property: response status MUST be 402
		if w.Code != http.StatusPaymentRequired {
			rt.Fatalf("expected status 402, got %d; body=%s", w.Code, w.Body.String())
		}

		// Property: response body MUST be valid JSON with required fields
		var respBody map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &respBody); err != nil {
			rt.Fatalf("response body is not valid JSON: %v; raw=%s", err, w.Body.String())
		}

		// Property: MUST contain "current_balance" field (defaults to 0 without BalanceQuerier)
		cbRaw, hasCB := respBody["current_balance"]
		if !hasCB {
			rt.Fatalf("response missing 'current_balance' field; body=%v", respBody)
		}
		cbVal, ok := cbRaw.(float64)
		if !ok {
			rt.Fatalf("'current_balance' is not a number: %T=%v", cbRaw, cbRaw)
		}
		if cbVal != 0 {
			rt.Fatalf("current_balance=%f, want 0 (no BalanceQuerier)", cbVal)
		}

		// Property: MUST contain "required_amount" field
		raRaw, hasRA := respBody["required_amount"]
		if !hasRA {
			rt.Fatalf("response missing 'required_amount' field; body=%v", respBody)
		}
		raVal, ok := raRaw.(float64)
		if !ok {
			rt.Fatalf("'required_amount' is not a number: %T=%v", raRaw, raRaw)
		}
		const epsilon = 1e-9
		diff := raVal - estimatedCost
		if diff > epsilon || diff < -epsilon {
			rt.Fatalf("required_amount=%f, want %f", raVal, estimatedCost)
		}

		// Property: MUST contain "top_up_url" field (non-empty string)
		tuRaw, hasTU := respBody["top_up_url"]
		if !hasTU {
			rt.Fatalf("response missing 'top_up_url' field; body=%v", respBody)
		}
		tuVal, ok := tuRaw.(string)
		if !ok {
			rt.Fatalf("'top_up_url' is not a string: %T=%v", tuRaw, tuRaw)
		}
		if tuVal == "" {
			rt.Fatalf("'top_up_url' is empty string")
		}
	})
}
