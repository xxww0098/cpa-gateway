package sdk

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/xxww0098/cpa-gateway/model"
	"github.com/xxww0098/cpa-gateway/pricing"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// -----------------------------------------------------------------------------
// Mocks
// -----------------------------------------------------------------------------

// holdLedgerMock records every Hold / Settle / Release call so tests can
// assert the HoldMiddleware's interaction with the billing ledger.
//
// It is also programmable: HoldErr / ReleaseErr let a test force a
// specific error return from the corresponding method so we can exercise
// the 402 / upstream-failure branches of the middleware without standing
// up a real Redis or database.
//
// The type is named with a "hold" prefix so it does not clash with the
// usage_test.go fakeLedger which lives in the same package.
type holdLedgerMock struct {
	mu sync.Mutex

	HoldErr    error
	ReleaseErr error
	SettleErr  error

	HoldCalls    []holdCallMW
	SettleCalls  []settleCallMW
	ReleaseCalls []releaseCallMW
}

type holdCallMW struct {
	UserID    uint
	Amount    float64
	RequestID string
	TTL       time.Duration
}

type settleCallMW struct {
	UserID       uint
	RequestID    string
	ActualAmount float64
}

type releaseCallMW struct {
	UserID    uint
	RequestID string
}

func (m *holdLedgerMock) Hold(ctx context.Context, userID uint, amount float64, requestID string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.HoldCalls = append(m.HoldCalls, holdCallMW{UserID: userID, Amount: amount, RequestID: requestID, TTL: ttl})
	return m.HoldErr
}

func (m *holdLedgerMock) Settle(ctx context.Context, userID uint, requestID string, actualAmount float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.SettleCalls = append(m.SettleCalls, settleCallMW{UserID: userID, RequestID: requestID, ActualAmount: actualAmount})
	return m.SettleErr
}

func (m *holdLedgerMock) Release(ctx context.Context, userID uint, requestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ReleaseCalls = append(m.ReleaseCalls, releaseCallMW{UserID: userID, RequestID: requestID})
	return m.ReleaseErr
}

// ActiveHoldAmount satisfies the BillingLedger interface added in Stage 2
// (task 5.1). The HoldMiddleware tests in this file do not exercise the
// fallback-settlement path that consumes this value, so the mock returns
// (0, false, nil) — equivalent to "no hold recorded".
func (m *holdLedgerMock) ActiveHoldAmount(_ context.Context, _ uint, _ string) (float64, bool, error) {
	return 0, false, nil
}

// HasUnresolvedShortfall satisfies the BillingLedger interface extension
// added in task 1.6. The HoldMiddleware unit tests in this file do not
// populate any BalanceLog shortfall rows, so a (false, nil) stub is the
// correct default.
func (m *holdLedgerMock) HasUnresolvedShortfall(_ context.Context, _ uint) (bool, error) {
	return false, nil
}

// holdCalcMock returns a constant estimate so tests can reason precisely
// about quota arithmetic. Stream and rateMult are recorded for
// assertions but do not change the returned value.
type holdCalcMock struct {
	EstimateValue float64
	ComputeValue  float64

	mu           sync.Mutex
	EstimateArgs []estimateArgsMW
}

type estimateArgsMW struct {
	Model    string
	Stream   bool
	RateMult float64
}

func (m *holdCalcMock) Estimate(modelID string, stream bool, rateMult float64) float64 {
	m.mu.Lock()
	m.EstimateArgs = append(m.EstimateArgs, estimateArgsMW{Model: modelID, Stream: stream, RateMult: rateMult})
	m.mu.Unlock()
	return m.EstimateValue
}

// EstimateWithMaxTokens satisfies the PricingCalculator interface extension
// introduced in task 6.1. The HoldMiddleware preflight (task 6.4) calls this
// before every Hold, so the mock must return a stable value. Returning the
// same EstimateValue keeps the preflight upper bound equal to the single
// Estimate value these tests exercise — the existing assertions continue to
// hold.
func (m *holdCalcMock) EstimateWithMaxTokens(modelID string, _ int64, stream bool, rateMult float64) float64 {
	return m.EstimateValue
}

func (m *holdCalcMock) Compute(modelID string, tokens pricing.UsageTokens, rateMult float64) float64 {
	return m.ComputeValue
}

// -----------------------------------------------------------------------------
// Test fixtures
// -----------------------------------------------------------------------------

// newHoldTestDB builds an in-memory SQLite DB and migrates just the
// tables HoldMiddleware touches. Each test gets a fresh DB keyed by
// t.Name() so parallel tests don't collide.
func newHoldTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:holdmw_test_" + t.Name() + "_?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("raw db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(&model.Subscription{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// newHoldGinEngine wires a gin engine with a middleware that injects a
// synthetic access.Result into gin.Context followed by the hold
// middleware and a trivial terminal handler. responseStatus governs the
// final handler's status code so tests can simulate upstream success or
// failure.
func newHoldGinEngine(t *testing.T, mw *HoldMiddleware, meta map[string]string, responseStatus int) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()

	// Seed access.Result on context like the SDK AuthMiddleware would.
	r.Use(func(c *gin.Context) {
		if meta != nil {
			c.Set("accessProvider", accessProviderName)
			c.Set("accessMetadata", meta)
			if principal, ok := meta["user_id"]; ok {
				c.Set("userApiKey", principal)
			}
		}
		c.Next()
	})

	r.Use(mw.Handle)

	// Terminal handler — returns responseStatus so the deferred hook in
	// HoldMiddleware can observe upstream success / failure.
	r.Any("/*path", func(c *gin.Context) {
		if responseStatus >= 400 {
			c.JSON(responseStatus, gin.H{"error": "upstream error"})
			return
		}
		// Assert SettleCtx is available to downstream handlers on the success path.
		if _, ok := SettleCtxFromContext(c.Request.Context()); !ok {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "settle ctx missing"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

// buildV1Request constructs a POST /v1/chat/completions request with the
// given JSON body. Tests that need a different path or body shape can
// inline their own construction.
func buildV1Request(body string) *http.Request {
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// baseMeta returns a fresh metadata map identical to what AccessProvider
// would produce for a subscription-less user. Tests mutate the result
// in place to add subscription fields.
func baseMeta(userID uint) map[string]string {
	return map[string]string{
		"user_id":   strconv.FormatUint(uint64(userID), 10),
		"rate_mult": "1.0",
	}
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

// TestHold_SufficientBalance verifies the happy path: Hold succeeds, the
// downstream handler runs, and the handler sees a SettleCtx on the
// request context.
func TestHold_SufficientBalance(t *testing.T) {
	db := newHoldTestDB(t)
	ldgr := &holdLedgerMock{}
	calc := &holdCalcMock{EstimateValue: 0.02}
	mw := NewHoldMiddleware(ldgr, calc, db, 5*time.Minute)

	body := `{"model":"gpt-4o","stream":false}`
	engine := newHoldGinEngine(t, mw, baseMeta(42), http.StatusOK)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, buildV1Request(body))

	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d; body=%s", got, want, w.Body.String())
	}
	if len(ldgr.HoldCalls) != 1 {
		t.Fatalf("expected 1 Hold call, got %d", len(ldgr.HoldCalls))
	}
	if ldgr.HoldCalls[0].UserID != 42 {
		t.Errorf("Hold userID = %d, want 42", ldgr.HoldCalls[0].UserID)
	}
	if ldgr.HoldCalls[0].Amount != 0.02 {
		t.Errorf("Hold amount = %v, want 0.02", ldgr.HoldCalls[0].Amount)
	}
	if len(ldgr.ReleaseCalls) != 0 {
		t.Errorf("expected 0 Release calls on 2xx, got %d", len(ldgr.ReleaseCalls))
	}
}

// TestHold_InsufficientBalance verifies that a ledger Hold returning
// ErrInsufficientBalance causes the middleware to abort with HTTP 402
// and that the downstream handler is never called.
func TestHold_InsufficientBalance(t *testing.T) {
	db := newHoldTestDB(t)
	ldgr := &holdLedgerMock{HoldErr: errors.New("insufficient balance")}
	calc := &holdCalcMock{EstimateValue: 0.02}
	mw := NewHoldMiddleware(ldgr, calc, db, 5*time.Minute)

	body := `{"model":"gpt-4o","stream":false}`
	engine := newHoldGinEngine(t, mw, baseMeta(7), http.StatusOK)

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, buildV1Request(body))

	if got, want := w.Code, http.StatusPaymentRequired; got != want {
		t.Fatalf("status = %d, want %d; body=%s", got, want, w.Body.String())
	}
	if len(ldgr.HoldCalls) != 1 {
		t.Errorf("expected 1 Hold call, got %d", len(ldgr.HoldCalls))
	}
}

// TestHold_SubscriptionDailyQuotaExceeded verifies that a pre-check
// failure (daily quota would be exceeded by the estimated cost) returns
// 402 without consulting the ledger.
func TestHold_SubscriptionDailyQuotaExceeded(t *testing.T) {
	db := newHoldTestDB(t)
	ldgr := &holdLedgerMock{}
	calc := &holdCalcMock{EstimateValue: 5.0}
	mw := NewHoldMiddleware(ldgr, calc, db, 5*time.Minute)

	userID := uint(11)
	daily := 10.0
	future := time.Now().UTC().Add(12 * time.Hour)
	sub := &model.Subscription{
		UserID:         userID,
		PackageID:      1,
		GroupID:        1,
		Status:         "active",
		StartsAt:       time.Now().UTC().Add(-24 * time.Hour),
		ExpiresAt:      time.Now().UTC().Add(24 * time.Hour),
		DailyUsageUSD:  8.0, // 8 + 5 > 10 → reject
		DailyResetAt:   future,
		WeeklyResetAt:  future,
		MonthlyResetAt: future,
		DailyLimitUSD:  &daily,
	}
	if err := db.Create(sub).Error; err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	meta := baseMeta(userID)
	meta["subscription_id"] = strconv.FormatUint(uint64(sub.ID), 10)
	meta["daily_limit"] = "10"
	meta["daily_used"] = "8"

	engine := newHoldGinEngine(t, mw, meta, http.StatusOK)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, buildV1Request(`{"model":"gpt-4o","stream":false}`))

	if got, want := w.Code, http.StatusPaymentRequired; got != want {
		t.Fatalf("status = %d, want %d; body=%s", got, want, w.Body.String())
	}
	if len(ldgr.HoldCalls) != 0 {
		t.Errorf("expected 0 Hold calls when quota fails, got %d", len(ldgr.HoldCalls))
	}
}

// TestHold_SubscriptionQuotaReset verifies that when Subscription.DailyResetAt
// is in the past, the middleware resets the usage counter, advances the
// reset timestamp to the next UTC midnight, and allows the request to
// proceed.
func TestHold_SubscriptionQuotaReset(t *testing.T) {
	db := newHoldTestDB(t)
	ldgr := &holdLedgerMock{}
	calc := &holdCalcMock{EstimateValue: 0.02}
	mw := NewHoldMiddleware(ldgr, calc, db, 5*time.Minute)

	userID := uint(13)
	daily := 10.0
	pastReset := time.Now().UTC().Add(-1 * time.Hour)
	future := time.Now().UTC().Add(24 * time.Hour)
	sub := &model.Subscription{
		UserID:         userID,
		PackageID:      1,
		GroupID:        1,
		Status:         "active",
		StartsAt:       time.Now().UTC().Add(-24 * time.Hour),
		ExpiresAt:      time.Now().UTC().Add(24 * time.Hour),
		DailyUsageUSD:  9.99, // would fail without reset (9.99 + 0.02 > 10)
		DailyResetAt:   pastReset,
		WeeklyResetAt:  future,
		MonthlyResetAt: future,
		DailyLimitUSD:  &daily,
	}
	if err := db.Create(sub).Error; err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	meta := baseMeta(userID)
	meta["subscription_id"] = strconv.FormatUint(uint64(sub.ID), 10)

	engine := newHoldGinEngine(t, mw, meta, http.StatusOK)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, buildV1Request(`{"model":"gpt-4o","stream":false}`))

	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d; body=%s", got, want, w.Body.String())
	}

	// Reload the subscription and assert reset happened.
	var fresh model.Subscription
	if err := db.First(&fresh, sub.ID).Error; err != nil {
		t.Fatalf("reload subscription: %v", err)
	}
	if fresh.DailyUsageUSD != 0 {
		t.Errorf("DailyUsageUSD after reset = %v, want 0", fresh.DailyUsageUSD)
	}
	if !fresh.DailyResetAt.After(pastReset) {
		t.Errorf("DailyResetAt not advanced: was %v, now %v", pastReset, fresh.DailyResetAt)
	}
	if len(ldgr.HoldCalls) != 1 {
		t.Errorf("expected 1 Hold call after reset, got %d", len(ldgr.HoldCalls))
	}
}

// TestHold_ReleaseOnUpstreamError verifies that when the downstream
// handler responds with a non-2xx status, the middleware's deferred
// hook calls Release to free the hold.
func TestHold_ReleaseOnUpstreamError(t *testing.T) {
	db := newHoldTestDB(t)
	ldgr := &holdLedgerMock{}
	calc := &holdCalcMock{EstimateValue: 0.02}
	mw := NewHoldMiddleware(ldgr, calc, db, 5*time.Minute)

	engine := newHoldGinEngine(t, mw, baseMeta(99), http.StatusBadGateway)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, buildV1Request(`{"model":"gpt-4o","stream":false}`))

	if got, want := w.Code, http.StatusBadGateway; got != want {
		t.Fatalf("status = %d, want %d; body=%s", got, want, w.Body.String())
	}
	if len(ldgr.HoldCalls) != 1 {
		t.Errorf("expected 1 Hold call, got %d", len(ldgr.HoldCalls))
	}
	if len(ldgr.ReleaseCalls) != 1 {
		t.Fatalf("expected 1 Release call on upstream failure, got %d", len(ldgr.ReleaseCalls))
	}
	if ldgr.ReleaseCalls[0].UserID != 99 {
		t.Errorf("Release userID = %d, want 99", ldgr.ReleaseCalls[0].UserID)
	}
}

// TestHold_NonV1Path verifies that the middleware short-circuits for
// non /v1/* paths: no Hold is issued and the handler runs directly
// without a SettleCtx installed.
func TestHold_NonV1Path(t *testing.T) {
	db := newHoldTestDB(t)
	ldgr := &holdLedgerMock{}
	calc := &holdCalcMock{EstimateValue: 0.02}
	mw := NewHoldMiddleware(ldgr, calc, db, 5*time.Minute)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(mw.Handle)
	var hit bool
	var hadSettleCtx bool
	r.Any("/*path", func(c *gin.Context) {
		hit = true
		_, hadSettleCtx = SettleCtxFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !hit {
		t.Fatalf("terminal handler not called for non-/v1 path")
	}
	if hadSettleCtx {
		t.Errorf("SettleCtx should not be installed on non-/v1 paths")
	}
	if len(ldgr.HoldCalls) != 0 {
		t.Errorf("expected 0 Hold calls for non-/v1 path, got %d", len(ldgr.HoldCalls))
	}
}

// -----------------------------------------------------------------------------
// Compile-time assertions — the mocks must implement the SDK interfaces.
// -----------------------------------------------------------------------------

var (
	_ BillingLedger      = (*holdLedgerMock)(nil)
	_ PricingCalculator  = (*holdCalcMock)(nil)
	_ sdkaccess.Provider = (*AccessProvider)(nil)
)
