package sdk

import (
	"context"
	"sync"
	"testing"
	"time"

	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/xxww0098/cpa-gateway/executor"
	"github.com/xxww0098/cpa-gateway/model"
	"github.com/xxww0098/cpa-gateway/pricing"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// -----------------------------------------------------------------------------
// test fakes
// -----------------------------------------------------------------------------

// fakeLedger records every ledger call so tests can assert exactly which
// path fired (Settle vs Release) and what arguments were passed. It never
// errors unless an error is pre-seeded via errOnSettle / errOnRelease.
type fakeLedger struct {
	mu sync.Mutex

	settleCalls  []settleCall
	releaseCalls []releaseCall
	holdCalls    []holdCall

	errOnSettle  error
	errOnRelease error
}

type settleCall struct {
	userID       uint
	requestID    string
	actualAmount float64
}

type releaseCall struct {
	userID    uint
	requestID string
}

type holdCall struct {
	userID    uint
	amount    float64
	requestID string
	ttl       time.Duration
}

func (f *fakeLedger) Hold(_ context.Context, userID uint, amount float64, requestID string, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holdCalls = append(f.holdCalls, holdCall{userID: userID, amount: amount, requestID: requestID, ttl: ttl})
	return nil
}

func (f *fakeLedger) Settle(_ context.Context, userID uint, requestID string, actualAmount float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settleCalls = append(f.settleCalls, settleCall{userID: userID, requestID: requestID, actualAmount: actualAmount})
	return f.errOnSettle
}

func (f *fakeLedger) Release(_ context.Context, userID uint, requestID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls = append(f.releaseCalls, releaseCall{userID: userID, requestID: requestID})
	return f.errOnRelease
}

// ActiveHoldAmount satisfies the BillingLedger interface added in Stage 2
// (task 5.1). The UsagePlugin tests in this file do not yet exercise the
// fallback-settlement path, so the mock returns (0, false, nil).
func (f *fakeLedger) ActiveHoldAmount(_ context.Context, _ uint, _ string) (float64, bool, error) {
	return 0, false, nil
}

// HasUnresolvedShortfall satisfies the BillingLedger interface extension
// added in task 1.6. The UsagePlugin tests in this file never construct an
// unresolved-shortfall scenario, so the stub returns (false, nil).
func (f *fakeLedger) HasUnresolvedShortfall(_ context.Context, _ uint) (bool, error) {
	return false, nil
}

// fakeCalculator returns a configurable cost from Compute so tests can pin
// the exact number that UsagePlugin writes into UsageLog and the balance
// counters. Estimate is unused by HandleUsage but satisfies the interface.
type fakeCalculator struct {
	computeCost float64
	// lastTokens captures the last tokens passed to Compute so tests can
	// assert the mapping from rec.Detail to pricing.UsageTokens.
	lastTokens   pricing.UsageTokens
	lastModel    string
	lastRateMult float64
	computed     bool
}

func (f *fakeCalculator) Estimate(_ string, _ bool, _ float64) float64 { return 0 }

// EstimateWithMaxTokens satisfies the PricingCalculator interface extension
// introduced in task 6.1. The UsagePlugin tests in this file do not exercise
// the preflight upper-bound path, so returning 0 matches the existing
// Estimate stub.
func (f *fakeCalculator) EstimateWithMaxTokens(_ string, _ int64, _ bool, _ float64) float64 {
	return 0
}

func (f *fakeCalculator) Compute(model string, tokens pricing.UsageTokens, rateMult float64) float64 {
	f.lastTokens = tokens
	f.lastModel = model
	f.lastRateMult = rateMult
	f.computed = true
	return f.computeCost
}

// newUsageTestDB builds a fresh in-memory SQLite database with only the
// tables UsagePlugin touches. It stays intentionally minimal so any failure
// surfacing as "no such table" signals a real regression in the plugin.
func newUsageTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:usage_test_" + t.Name() + "_?mode=memory&cache=shared"
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

	if err := db.AutoMigrate(
		&model.User{},
		&model.UsageLog{},
		&model.Subscription{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// newCtxWithSettle builds a context carrying a SettleCtx with the given
// identifiers. Tests use it to exercise the plugin's "happy path" that
// depends on an upstream HoldMiddleware injection.
//
// The returned context is also annotated with Usage_Detail_Present = true
// so HandleUsage's three-branch switch (see sdk/usage.go) takes the
// precise-settle path. Tests that want to exercise the fallback/strict
// branches construct their own context without this marker.
func newCtxWithSettle(sc *SettleCtx) context.Context {
	ctx := WithSettleCtx(context.Background(), sc)
	return executor.WithUsageDetailPresent(ctx, true)
}

// -----------------------------------------------------------------------------
// tests
// -----------------------------------------------------------------------------

// TestHandleUsage_SettleSuccess verifies the happy path: a successful
// upstream response with token counts in rec.Detail triggers a Settle for
// the computed cost and writes a UsageLog row populated from the record.
func TestHandleUsage_SettleSuccess(t *testing.T) {
	db := newUsageTestDB(t)
	led := &fakeLedger{}
	calc := &fakeCalculator{computeCost: 0.0123}
	plugin := NewUsagePlugin(db, led, calc)

	sc := &SettleCtx{
		RequestID: "req-settle-ok",
		UserID:    42,
		RateMult:  1.5,
		Model:     "gpt-4o",
		Stream:    false,
	}
	ctx := newCtxWithSettle(sc)

	rec := cliproxyusage.Record{
		Provider:    "openai",
		Model:       "gpt-4o",
		RequestedAt: time.Now(),
		Latency:     250 * time.Millisecond,
		Detail: cliproxyusage.Detail{
			InputTokens:     100,
			OutputTokens:    200,
			CachedTokens:    10,
			ReasoningTokens: 5,
			TotalTokens:     315,
		},
	}

	plugin.HandleUsage(ctx, rec)

	if len(led.settleCalls) != 1 {
		t.Fatalf("expected exactly 1 Settle call, got %d", len(led.settleCalls))
	}
	call := led.settleCalls[0]
	if call.userID != sc.UserID {
		t.Errorf("Settle.userID = %d, want %d", call.userID, sc.UserID)
	}
	if call.requestID != sc.RequestID {
		t.Errorf("Settle.requestID = %q, want %q", call.requestID, sc.RequestID)
	}
	if call.actualAmount != calc.computeCost {
		t.Errorf("Settle.actualAmount = %v, want %v", call.actualAmount, calc.computeCost)
	}
	if len(led.releaseCalls) != 0 {
		t.Errorf("expected no Release calls, got %d", len(led.releaseCalls))
	}

	// Assert the calculator saw the correct per-column tokens.
	if calc.lastTokens.Input != 100 || calc.lastTokens.Output != 200 ||
		calc.lastTokens.Cached != 10 || calc.lastTokens.Reasoning != 5 {
		t.Errorf("Compute tokens = %+v, want {Input:100 Output:200 Cached:10 Reasoning:5}", calc.lastTokens)
	}
	if calc.lastRateMult != sc.RateMult {
		t.Errorf("Compute rateMult = %v, want %v", calc.lastRateMult, sc.RateMult)
	}

	// Assert a UsageLog row was written with the right columns.
	var log model.UsageLog
	if err := db.Where("request_id = ?", sc.RequestID).First(&log).Error; err != nil {
		t.Fatalf("UsageLog not found: %v", err)
	}
	if log.UserID != sc.UserID {
		t.Errorf("UsageLog.UserID = %d, want %d", log.UserID, sc.UserID)
	}
	if log.Model != rec.Model {
		t.Errorf("UsageLog.Model = %q, want %q", log.Model, rec.Model)
	}
	if log.Provider != rec.Provider {
		t.Errorf("UsageLog.Provider = %q, want %q", log.Provider, rec.Provider)
	}
	if log.InputTokens != 100 || log.OutputTokens != 200 ||
		log.CachedTokens != 10 || log.ReasoningTokens != 5 {
		t.Errorf("UsageLog token columns = {I:%d O:%d C:%d R:%d}, want {100 200 10 5}",
			log.InputTokens, log.OutputTokens, log.CachedTokens, log.ReasoningTokens)
	}
	if log.Cost != calc.computeCost {
		t.Errorf("UsageLog.Cost = %v, want %v", log.Cost, calc.computeCost)
	}
	if log.Failed {
		t.Errorf("UsageLog.Failed = true, want false")
	}
}

// TestHandleUsage_Failed verifies that a failed upstream response triggers
// Release (not Settle) and still writes a UsageLog row marked Failed=true.
func TestHandleUsage_Failed(t *testing.T) {
	db := newUsageTestDB(t)
	led := &fakeLedger{}
	calc := &fakeCalculator{computeCost: 0.05}
	plugin := NewUsagePlugin(db, led, calc)

	sc := &SettleCtx{
		RequestID: "req-failed",
		UserID:    7,
		RateMult:  1.0,
		Model:     "gpt-4o-mini",
	}
	ctx := newCtxWithSettle(sc)

	rec := cliproxyusage.Record{
		Provider: "openai",
		Model:    "gpt-4o-mini",
		Failed:   true,
		Detail: cliproxyusage.Detail{
			InputTokens:  10,
			OutputTokens: 0,
		},
	}

	plugin.HandleUsage(ctx, rec)

	if len(led.releaseCalls) != 1 {
		t.Fatalf("expected 1 Release call, got %d", len(led.releaseCalls))
	}
	if got := led.releaseCalls[0]; got.userID != sc.UserID || got.requestID != sc.RequestID {
		t.Errorf("Release call = %+v, want {userID:%d requestID:%q}", got, sc.UserID, sc.RequestID)
	}
	if len(led.settleCalls) != 0 {
		t.Errorf("expected no Settle calls on failure, got %d", len(led.settleCalls))
	}

	// UsageLog is still recorded so admins can audit failed attempts.
	var log model.UsageLog
	if err := db.Where("request_id = ?", sc.RequestID).First(&log).Error; err != nil {
		t.Fatalf("UsageLog not found: %v", err)
	}
	if !log.Failed {
		t.Errorf("UsageLog.Failed = false, want true")
	}
}

// TestHandleUsage_WithSubscription verifies that when the SettleCtx carries
// an active subscription ID, HandleUsage accumulates the actualCost into
// the subscription's daily/weekly/monthly usage counters.
func TestHandleUsage_WithSubscription(t *testing.T) {
	db := newUsageTestDB(t)
	led := &fakeLedger{}
	calc := &fakeCalculator{computeCost: 0.25}
	plugin := NewUsagePlugin(db, led, calc)

	user := &model.User{Email: "sub-accum@example.com", PasswordHash: "x", Role: "user", Status: "active", Balance: 10}
	if err := db.Create(user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	now := time.Now().UTC()
	sub := &model.Subscription{
		UserID:          user.ID,
		PackageID:       1,
		GroupID:         1,
		Status:          "active",
		StartsAt:        now.Add(-24 * time.Hour),
		ExpiresAt:       now.Add(30 * 24 * time.Hour),
		DailyUsageUSD:   1.00,
		WeeklyUsageUSD:  2.00,
		MonthlyUsageUSD: 3.00,
		DailyResetAt:    now.Add(24 * time.Hour),
		WeeklyResetAt:   now.Add(7 * 24 * time.Hour),
		MonthlyResetAt:  now.Add(30 * 24 * time.Hour),
	}
	if err := db.Create(sub).Error; err != nil {
		t.Fatalf("create subscription: %v", err)
	}

	subID := sub.ID
	sc := &SettleCtx{
		RequestID:      "req-sub",
		UserID:         user.ID,
		RateMult:       1.0,
		SubscriptionID: &subID,
		Model:          "gpt-4o",
	}

	rec := cliproxyusage.Record{
		Provider: "openai",
		Model:    "gpt-4o",
		Detail: cliproxyusage.Detail{
			InputTokens:  50,
			OutputTokens: 100,
		},
	}

	plugin.HandleUsage(newCtxWithSettle(sc), rec)

	var reloaded model.Subscription
	if err := db.First(&reloaded, sub.ID).Error; err != nil {
		t.Fatalf("reload subscription: %v", err)
	}
	if got, want := reloaded.DailyUsageUSD, 1.00+calc.computeCost; got != want {
		t.Errorf("DailyUsageUSD = %v, want %v", got, want)
	}
	if got, want := reloaded.WeeklyUsageUSD, 2.00+calc.computeCost; got != want {
		t.Errorf("WeeklyUsageUSD = %v, want %v", got, want)
	}
	if got, want := reloaded.MonthlyUsageUSD, 3.00+calc.computeCost; got != want {
		t.Errorf("MonthlyUsageUSD = %v, want %v", got, want)
	}
}

// TestHandleUsage_NoSettleCtx verifies that when the context is missing a
// SettleCtx (e.g. a non /v1/ path or an untagged internal call), the plugin
// is a no-op: no Settle/Release, no UsageLog insert.
func TestHandleUsage_NoSettleCtx(t *testing.T) {
	db := newUsageTestDB(t)
	led := &fakeLedger{}
	calc := &fakeCalculator{computeCost: 0.99}
	plugin := NewUsagePlugin(db, led, calc)

	// plain context with no SettleCtx key
	plugin.HandleUsage(context.Background(), cliproxyusage.Record{
		Provider: "openai",
		Model:    "gpt-4o",
		Detail:   cliproxyusage.Detail{InputTokens: 1, OutputTokens: 1},
	})

	if len(led.settleCalls)+len(led.releaseCalls) != 0 {
		t.Errorf("expected no ledger calls, got settle=%d release=%d",
			len(led.settleCalls), len(led.releaseCalls))
	}
	if calc.computed {
		t.Errorf("expected Compute to not be called when no SettleCtx")
	}

	var count int64
	db.Model(&model.UsageLog{}).Count(&count)
	if count != 0 {
		t.Errorf("UsageLog count = %d, want 0", count)
	}
}

// TestHandleUsage_ZeroTokens verifies that when the record reports zero
// tokens (e.g. an upstream 204 or an empty response), Settle is still
// called with 0 to clear the hold and no Debit is issued by the ledger
// contract itself (the fake simply records the zero amount).
func TestHandleUsage_ZeroTokens(t *testing.T) {
	db := newUsageTestDB(t)
	led := &fakeLedger{}
	calc := &fakeCalculator{computeCost: 0} // zero tokens → zero cost
	plugin := NewUsagePlugin(db, led, calc)

	sc := &SettleCtx{
		RequestID: "req-zero",
		UserID:    11,
		RateMult:  1.0,
		Model:     "gpt-4o",
	}
	rec := cliproxyusage.Record{
		Provider: "openai",
		Model:    "gpt-4o",
		Detail:   cliproxyusage.Detail{}, // all zeros
	}

	plugin.HandleUsage(newCtxWithSettle(sc), rec)

	if len(led.settleCalls) != 1 {
		t.Fatalf("expected 1 Settle call, got %d", len(led.settleCalls))
	}
	if got := led.settleCalls[0].actualAmount; got != 0 {
		t.Errorf("Settle.actualAmount = %v, want 0 (zero tokens)", got)
	}
	if len(led.releaseCalls) != 0 {
		t.Errorf("expected no Release calls, got %d", len(led.releaseCalls))
	}
}
