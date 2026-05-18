package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/xxww0098/cpa-gateway/executor"
	"github.com/xxww0098/cpa-gateway/ledger"
	"github.com/xxww0098/cpa-gateway/model"
	"github.com/xxww0098/cpa-gateway/pricing"
	"github.com/xxww0098/cpa-gateway/testutil"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"pgregory.net/rapid"
)

// -----------------------------------------------------------------------------
// Test helpers for property-based tests
// -----------------------------------------------------------------------------

// propTestDB creates a fresh in-memory SQLite database with all tables
// needed by UsagePlugin property tests (User, UsageLog, BalanceLog, Subscription).
func propTestDB(t *testing.T, suffix string) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:usage_prop_%s_%s?mode=memory&cache=shared", t.Name(), suffix)
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
		&model.BalanceLog{},
		&model.Subscription{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// propFakeLedger is a ledger fake for property tests that can be configured
// to fail on Settle to test the "none persist" side of atomicity.
type propFakeLedger struct {
	mu           sync.Mutex
	errOnSettle  error
	settleCalls  int
	releaseCalls int
}

func (f *propFakeLedger) Hold(_ context.Context, _ uint, _ float64, _ string, _ time.Duration) error {
	return nil
}

func (f *propFakeLedger) Settle(_ context.Context, _ uint, _ string, _ float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settleCalls++
	return f.errOnSettle
}

func (f *propFakeLedger) Release(_ context.Context, _ uint, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls++
	return nil
}

// ActiveHoldAmount satisfies the BillingLedger interface added in Stage 2
// (task 5.1). No property test in this file constructs the "missing upstream
// usage" branch, so a (0, false, nil) stub is sufficient.
func (f *propFakeLedger) ActiveHoldAmount(_ context.Context, _ uint, _ string) (float64, bool, error) {
	return 0, false, nil
}

// HasUnresolvedShortfall satisfies the BillingLedger interface extension
// added in task 1.6. None of the property tests in this file arrange an
// unresolved shortfall, so the stub returns (false, nil).
func (f *propFakeLedger) HasUnresolvedShortfall(_ context.Context, _ uint) (bool, error) {
	return false, nil
}

// propFakeCalc returns a configurable cost from Compute.
type propFakeCalc struct {
	cost float64
}

func (f *propFakeCalc) Estimate(_ string, _ bool, _ float64) float64 { return 0 }

// EstimateWithMaxTokens satisfies the PricingCalculator interface extension
// introduced in task 6.1. Property tests in this file do not exercise the
// preflight upper-bound path, so returning 0 matches the Estimate stub.
func (f *propFakeCalc) EstimateWithMaxTokens(_ string, _ int64, _ bool, _ float64) float64 {
	return 0
}
func (f *propFakeCalc) Compute(_ string, _ pricing.UsageTokens, _ float64) float64 {
	return f.cost
}

// =============================================================================
// Feature: billing-system-optimization, Property 4: Settle Transactional Atomicity
// =============================================================================

// TestProperty4_SettleTransactionalAtomicity_AllCommit verifies that for any
// successful Settle operation, the balance debit (via ledger.Settle), UsageLog
// insertion, Subscription counter accumulation, and BalanceLog creation all
// commit within a single database transaction — either all succeed or none persist.
//
// **Validates: Requirements 2.1**
func TestProperty4_SettleTransactionalAtomicity_AllCommit(t *testing.T) {
	db := propTestDB(t, "allcommit")
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		// Generate random but valid inputs.
		userBalance := rapid.Float64Range(1.0, 1000.0).Draw(rt, "userBalance")
		cost := rapid.Float64Range(0.001, userBalance).Draw(rt, "cost")
		inputTokens := rapid.Int64Range(1, 10000).Draw(rt, "inputTokens")
		outputTokens := rapid.Int64Range(1, 10000).Draw(rt, "outputTokens")
		cachedTokens := rapid.Int64Range(0, 1000).Draw(rt, "cachedTokens")
		reasoningTokens := rapid.Int64Range(0, 1000).Draw(rt, "reasoningTokens")
		rateMult := rapid.Float64Range(0.5, 3.0).Draw(rt, "rateMult")
		hasSubscription := rapid.Bool().Draw(rt, "hasSubscription")
		requestID := fmt.Sprintf("req-prop4-ok-%d", iter)

		// Create user with sufficient balance.
		user := &model.User{
			Email:        fmt.Sprintf("prop4-ok-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      userBalance,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		// Optionally create a subscription.
		var subID *uint
		var initialDaily, initialWeekly, initialMonthly float64
		if hasSubscription {
			initialDaily = rapid.Float64Range(0, 10.0).Draw(rt, "initialDaily")
			initialWeekly = rapid.Float64Range(0, 50.0).Draw(rt, "initialWeekly")
			initialMonthly = rapid.Float64Range(0, 200.0).Draw(rt, "initialMonthly")
			now := time.Now().UTC()
			sub := &model.Subscription{
				UserID:          user.ID,
				PackageID:       1,
				GroupID:         1,
				Status:          "active",
				StartsAt:        now.Add(-24 * time.Hour),
				ExpiresAt:       now.Add(30 * 24 * time.Hour),
				DailyUsageUSD:   initialDaily,
				WeeklyUsageUSD:  initialWeekly,
				MonthlyUsageUSD: initialMonthly,
				DailyResetAt:    now.Add(24 * time.Hour),
				WeeklyResetAt:   now.Add(7 * 24 * time.Hour),
				MonthlyResetAt:  now.Add(30 * 24 * time.Hour),
			}
			if err := db.Create(sub).Error; err != nil {
				rt.Fatalf("create subscription: %v", err)
			}
			id := sub.ID
			subID = &id
		}

		// Configure plugin with a ledger that succeeds.
		led := &propFakeLedger{}
		calc := &propFakeCalc{cost: cost}
		plugin := NewUsagePlugin(db, led, calc)

		sc := &SettleCtx{
			RequestID:      requestID,
			UserID:         user.ID,
			RateMult:       rateMult,
			SubscriptionID: subID,
			Model:          "gpt-4o",
			ApiKeyID:       42,
			IPAddress:      "10.0.0.1",
		}
		ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

		rec := cliproxyusage.Record{
			Provider: "openai",
			Model:    "gpt-4o",
			Failed:   false,
			Latency:  100 * time.Millisecond,
			Detail: cliproxyusage.Detail{
				InputTokens:     inputTokens,
				OutputTokens:    outputTokens,
				CachedTokens:    cachedTokens,
				ReasoningTokens: reasoningTokens,
			},
		}

		plugin.HandleUsage(ctx, rec)

		// Verify ALL artifacts committed together:

		// 1. Ledger.Settle was called exactly once.
		led.mu.Lock()
		settleCount := led.settleCalls
		led.mu.Unlock()
		if settleCount != 1 {
			rt.Fatalf("expected 1 Settle call, got %d", settleCount)
		}

		// 2. UsageLog with Failed=false exists.
		var usageLog model.UsageLog
		if err := db.Where("request_id = ?", requestID).First(&usageLog).Error; err != nil {
			rt.Fatalf("UsageLog not found after successful Settle: %v", err)
		}
		if usageLog.Failed {
			rt.Fatalf("UsageLog.Failed = true, want false for successful Settle")
		}
		if usageLog.UserID != user.ID {
			rt.Fatalf("UsageLog.UserID = %d, want %d", usageLog.UserID, user.ID)
		}

		// 3. Subscription counters accumulated (if subscription present).
		if hasSubscription && subID != nil && cost > 0 {
			var reloaded model.Subscription
			if err := db.First(&reloaded, *subID).Error; err != nil {
				rt.Fatalf("reload subscription: %v", err)
			}
			// Counters should have increased by cost.
			if reloaded.DailyUsageUSD < initialDaily+cost-0.0001 {
				rt.Fatalf("DailyUsageUSD = %v, want >= %v", reloaded.DailyUsageUSD, initialDaily+cost)
			}
			if reloaded.WeeklyUsageUSD < initialWeekly+cost-0.0001 {
				rt.Fatalf("WeeklyUsageUSD = %v, want >= %v", reloaded.WeeklyUsageUSD, initialWeekly+cost)
			}
			if reloaded.MonthlyUsageUSD < initialMonthly+cost-0.0001 {
				rt.Fatalf("MonthlyUsageUSD = %v, want >= %v", reloaded.MonthlyUsageUSD, initialMonthly+cost)
			}
		}
	})
}

// TestProperty4_SettleTransactionalAtomicity_NoneCommit verifies the "none"
// side of atomicity: when ledger.Settle fails, no UsageLog with Failed=false
// is written and no Subscription counters are accumulated.
//
// **Validates: Requirements 2.1**
func TestProperty4_SettleTransactionalAtomicity_NoneCommit(t *testing.T) {
	db := propTestDB(t, "nonecommit")
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		cost := rapid.Float64Range(0.001, 100.0).Draw(rt, "cost")
		inputTokens := rapid.Int64Range(1, 10000).Draw(rt, "inputTokens")
		outputTokens := rapid.Int64Range(1, 10000).Draw(rt, "outputTokens")
		hasSubscription := rapid.Bool().Draw(rt, "hasSubscription")
		requestID := fmt.Sprintf("req-prop4-fail-%d", iter)

		// Create user (balance doesn't matter since we force Settle to fail).
		user := &model.User{
			Email:        fmt.Sprintf("prop4fail-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      100.0,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		var subID *uint
		var initialDaily, initialWeekly, initialMonthly float64
		if hasSubscription {
			initialDaily = rapid.Float64Range(0, 10.0).Draw(rt, "initialDaily")
			initialWeekly = rapid.Float64Range(0, 50.0).Draw(rt, "initialWeekly")
			initialMonthly = rapid.Float64Range(0, 200.0).Draw(rt, "initialMonthly")
			now := time.Now().UTC()
			sub := &model.Subscription{
				UserID:          user.ID,
				PackageID:       1,
				GroupID:         1,
				Status:          "active",
				StartsAt:        now.Add(-24 * time.Hour),
				ExpiresAt:       now.Add(30 * 24 * time.Hour),
				DailyUsageUSD:   initialDaily,
				WeeklyUsageUSD:  initialWeekly,
				MonthlyUsageUSD: initialMonthly,
				DailyResetAt:    now.Add(24 * time.Hour),
				WeeklyResetAt:   now.Add(7 * 24 * time.Hour),
				MonthlyResetAt:  now.Add(30 * 24 * time.Hour),
			}
			if err := db.Create(sub).Error; err != nil {
				rt.Fatalf("create subscription: %v", err)
			}
			id := sub.ID
			subID = &id
		}

		// Configure plugin with a ledger that FAILS on Settle.
		led := &propFakeLedger{errOnSettle: errors.New("insufficient balance")}
		calc := &propFakeCalc{cost: cost}
		plugin := NewUsagePlugin(db, led, calc)

		sc := &SettleCtx{
			RequestID:      requestID,
			UserID:         user.ID,
			RateMult:       1.0,
			SubscriptionID: subID,
			Model:          "gpt-4o",
		}
		ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

		rec := cliproxyusage.Record{
			Provider: "openai",
			Model:    "gpt-4o",
			Failed:   false, // upstream succeeded, but Settle will fail
			Detail: cliproxyusage.Detail{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
			},
		}

		plugin.HandleUsage(ctx, rec)

		// Verify NONE of the success artifacts persist:

		// 1. No UsageLog with Failed=false should exist.
		var successLogs int64
		db.Model(&model.UsageLog{}).Where("request_id = ? AND failed = ?", requestID, false).Count(&successLogs)
		if successLogs != 0 {
			rt.Fatalf("found %d UsageLog records with Failed=false after failed Settle, want 0", successLogs)
		}

		// 2. A UsageLog with Failed=true should exist (failure audit).
		var failedLogs int64
		db.Model(&model.UsageLog{}).Where("request_id = ? AND failed = ?", requestID, true).Count(&failedLogs)
		if failedLogs != 1 {
			rt.Fatalf("expected 1 UsageLog with Failed=true after failed Settle, got %d", failedLogs)
		}

		// Verify the failure reason is recorded in RawMetadata.
		var failedLog model.UsageLog
		db.Where("request_id = ? AND failed = ?", requestID, true).First(&failedLog)
		if len(failedLog.RawMetadata) > 0 {
			var meta map[string]interface{}
			if err := json.Unmarshal(failedLog.RawMetadata, &meta); err == nil {
				if reason, ok := meta["reason"].(string); !ok || reason == "" {
					rt.Fatalf("RawMetadata missing 'reason' field for failed Settle")
				}
			}
		}

		// 3. Subscription counters NOT accumulated.
		if hasSubscription && subID != nil {
			var reloaded model.Subscription
			if err := db.First(&reloaded, *subID).Error; err != nil {
				rt.Fatalf("reload subscription: %v", err)
			}
			// Counters should remain unchanged.
			if reloaded.DailyUsageUSD != initialDaily {
				rt.Fatalf("DailyUsageUSD = %v, want %v (unchanged after failed Settle)", reloaded.DailyUsageUSD, initialDaily)
			}
			if reloaded.WeeklyUsageUSD != initialWeekly {
				rt.Fatalf("WeeklyUsageUSD = %v, want %v (unchanged after failed Settle)", reloaded.WeeklyUsageUSD, initialWeekly)
			}
			if reloaded.MonthlyUsageUSD != initialMonthly {
				rt.Fatalf("MonthlyUsageUSD = %v, want %v (unchanged after failed Settle)", reloaded.MonthlyUsageUSD, initialMonthly)
			}
		}
	})
}

// =============================================================================
// Feature: billing-system-optimization, Property 5: Failed Settle Prevents Success Artifacts
// =============================================================================

// TestProperty5_FailedSettlePreventsSuccessArtifacts verifies that for any
// Settle operation that fails (due to insufficient balance or any other error),
// no UsageLog record with Failed=false exists for that request, and no
// Subscription usage counters are incremented.
//
// This property generates random failure scenarios (various error messages,
// different cost levels, with/without subscriptions) and asserts the invariant
// holds universally.
//
// **Validates: Requirements 2.2, 2.3**
func TestProperty5_FailedSettlePreventsSuccessArtifacts(t *testing.T) {
	db := propTestDB(t, "prop5")
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		// Generate random inputs.
		cost := rapid.Float64Range(0.001, 500.0).Draw(rt, "cost")
		inputTokens := rapid.Int64Range(1, 50000).Draw(rt, "inputTokens")
		outputTokens := rapid.Int64Range(1, 50000).Draw(rt, "outputTokens")
		cachedTokens := rapid.Int64Range(0, 5000).Draw(rt, "cachedTokens")
		reasoningTokens := rapid.Int64Range(0, 5000).Draw(rt, "reasoningTokens")
		rateMult := rapid.Float64Range(0.1, 5.0).Draw(rt, "rateMult")
		hasSubscription := rapid.Bool().Draw(rt, "hasSubscription")

		// Generate a random settle error reason to simulate various failure modes.
		errReasons := []string{
			"insufficient balance",
			"database timeout",
			"deadlock detected",
			"connection refused",
			"balance below zero",
		}
		errIdx := rapid.IntRange(0, len(errReasons)-1).Draw(rt, "errIdx")
		settleError := errors.New(errReasons[errIdx])

		requestID := fmt.Sprintf("req-prop5-%d", iter)

		// Create user.
		user := &model.User{
			Email:        fmt.Sprintf("prop5-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      50.0,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		// Optionally create a subscription with known initial counters.
		var subID *uint
		var initialDaily, initialWeekly, initialMonthly float64
		if hasSubscription {
			initialDaily = rapid.Float64Range(0, 100.0).Draw(rt, "initialDaily")
			initialWeekly = rapid.Float64Range(0, 500.0).Draw(rt, "initialWeekly")
			initialMonthly = rapid.Float64Range(0, 2000.0).Draw(rt, "initialMonthly")
			now := time.Now().UTC()
			sub := &model.Subscription{
				UserID:          user.ID,
				PackageID:       1,
				GroupID:         1,
				Status:          "active",
				StartsAt:        now.Add(-24 * time.Hour),
				ExpiresAt:       now.Add(30 * 24 * time.Hour),
				DailyUsageUSD:   initialDaily,
				WeeklyUsageUSD:  initialWeekly,
				MonthlyUsageUSD: initialMonthly,
				DailyResetAt:    now.Add(24 * time.Hour),
				WeeklyResetAt:   now.Add(7 * 24 * time.Hour),
				MonthlyResetAt:  now.Add(30 * 24 * time.Hour),
			}
			if err := db.Create(sub).Error; err != nil {
				rt.Fatalf("create subscription: %v", err)
			}
			id := sub.ID
			subID = &id
		}

		// Configure plugin with a ledger that FAILS on Settle.
		led := &propFakeLedger{errOnSettle: settleError}
		calc := &propFakeCalc{cost: cost}
		plugin := NewUsagePlugin(db, led, calc)

		sc := &SettleCtx{
			RequestID:      requestID,
			UserID:         user.ID,
			RateMult:       rateMult,
			SubscriptionID: subID,
			Model:          "gpt-4o",
			ApiKeyID:       uint(rapid.IntRange(0, 100).Draw(rt, "apiKeyID")),
			IPAddress:      "192.168.1.1",
		}
		ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

		// The upstream request succeeded (rec.Failed=false), but Settle will fail.
		rec := cliproxyusage.Record{
			Provider: "openai",
			Model:    "gpt-4o",
			Failed:   false,
			Latency:  time.Duration(rapid.Int64Range(10, 5000).Draw(rt, "latencyMs")) * time.Millisecond,
			Detail: cliproxyusage.Detail{
				InputTokens:     inputTokens,
				OutputTokens:    outputTokens,
				CachedTokens:    cachedTokens,
				ReasoningTokens: reasoningTokens,
			},
		}

		plugin.HandleUsage(ctx, rec)

		// PROPERTY ASSERTION 1: No UsageLog with Failed=false shall exist.
		var successCount int64
		db.Model(&model.UsageLog{}).Where("request_id = ? AND failed = ?", requestID, false).Count(&successCount)
		if successCount != 0 {
			rt.Fatalf("Property 5 violated: found %d UsageLog records with Failed=false for request %s after failed Settle (error: %s)",
				successCount, requestID, settleError)
		}

		// PROPERTY ASSERTION 2: No Subscription usage counters shall be incremented.
		if hasSubscription && subID != nil {
			var reloaded model.Subscription
			if err := db.First(&reloaded, *subID).Error; err != nil {
				rt.Fatalf("reload subscription: %v", err)
			}
			if reloaded.DailyUsageUSD != initialDaily {
				rt.Fatalf("Property 5 violated: DailyUsageUSD changed from %v to %v after failed Settle",
					initialDaily, reloaded.DailyUsageUSD)
			}
			if reloaded.WeeklyUsageUSD != initialWeekly {
				rt.Fatalf("Property 5 violated: WeeklyUsageUSD changed from %v to %v after failed Settle",
					initialWeekly, reloaded.WeeklyUsageUSD)
			}
			if reloaded.MonthlyUsageUSD != initialMonthly {
				rt.Fatalf("Property 5 violated: MonthlyUsageUSD changed from %v to %v after failed Settle",
					initialMonthly, reloaded.MonthlyUsageUSD)
			}
		}
	})
}

// =============================================================================
// Feature: billing-system-optimization, Property 6: Failed Operation Audit Logging
// =============================================================================

// TestProperty6_FailedSettleAuditLogging verifies that for any Settle operation
// that fails, a UsageLog record with Failed=true is written and the RawMetadata
// contains the failure reason.
//
// **Validates: Requirements 2.4, 2.5**
func TestProperty6_FailedSettleAuditLogging(t *testing.T) {
	db := propTestDB(t, "prop6settle")
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		cost := rapid.Float64Range(0.001, 500.0).Draw(rt, "cost")
		inputTokens := rapid.Int64Range(1, 20000).Draw(rt, "inputTokens")
		outputTokens := rapid.Int64Range(1, 20000).Draw(rt, "outputTokens")
		cachedTokens := rapid.Int64Range(0, 5000).Draw(rt, "cachedTokens")
		reasoningTokens := rapid.Int64Range(0, 5000).Draw(rt, "reasoningTokens")
		rateMult := rapid.Float64Range(0.1, 5.0).Draw(rt, "rateMult")

		// Generate a random failure reason.
		errReasons := []string{
			"insufficient balance",
			"database timeout",
			"deadlock detected",
			"connection refused",
			"balance below zero",
			"transaction rollback",
			"constraint violation",
		}
		errIdx := rapid.IntRange(0, len(errReasons)-1).Draw(rt, "errIdx")
		settleErrMsg := errReasons[errIdx]

		requestID := fmt.Sprintf("req-prop6-settle-%d", iter)

		// Create user.
		user := &model.User{
			Email:        fmt.Sprintf("prop6settle-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      100.0,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		// Configure plugin with a ledger that FAILS on Settle.
		led := &propFakeLedger{errOnSettle: errors.New(settleErrMsg)}
		calc := &propFakeCalc{cost: cost}
		plugin := NewUsagePlugin(db, led, calc)

		sc := &SettleCtx{
			RequestID: requestID,
			UserID:    user.ID,
			RateMult:  rateMult,
			Model:     "gpt-4o",
			ApiKeyID:  uint(rapid.IntRange(0, 100).Draw(rt, "apiKeyID")),
			IPAddress: "10.0.0.1",
		}
		ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

		// Upstream succeeded but Settle will fail.
		rec := cliproxyusage.Record{
			Provider: "openai",
			Model:    "gpt-4o",
			Failed:   false,
			Latency:  time.Duration(rapid.Int64Range(10, 5000).Draw(rt, "latencyMs")) * time.Millisecond,
			Detail: cliproxyusage.Detail{
				InputTokens:     inputTokens,
				OutputTokens:    outputTokens,
				CachedTokens:    cachedTokens,
				ReasoningTokens: reasoningTokens,
			},
		}

		plugin.HandleUsage(ctx, rec)

		// PROPERTY ASSERTION 1: A UsageLog with Failed=true SHALL exist.
		var failedLog model.UsageLog
		err := db.Where("request_id = ? AND failed = ?", requestID, true).First(&failedLog).Error
		if err != nil {
			rt.Fatalf("Property 6 violated: no UsageLog with Failed=true found for failed Settle (request=%s, error=%s): %v",
				requestID, settleErrMsg, err)
		}

		// PROPERTY ASSERTION 2: RawMetadata SHALL contain the failure reason.
		if len(failedLog.RawMetadata) == 0 {
			rt.Fatalf("Property 6 violated: RawMetadata is empty for failed Settle (request=%s, error=%s)",
				requestID, settleErrMsg)
		}
		var meta map[string]interface{}
		if err := json.Unmarshal(failedLog.RawMetadata, &meta); err != nil {
			rt.Fatalf("Property 6 violated: RawMetadata is not valid JSON for failed Settle (request=%s): %v",
				requestID, err)
		}
		reason, ok := meta["reason"].(string)
		if !ok || reason == "" {
			rt.Fatalf("Property 6 violated: RawMetadata missing 'reason' field for failed Settle (request=%s, meta=%s)",
				requestID, string(failedLog.RawMetadata))
		}
		if reason != settleErrMsg {
			rt.Fatalf("Property 6 violated: RawMetadata reason=%q, want %q (request=%s)",
				reason, settleErrMsg, requestID)
		}
	})
}

// TestProperty6_ReleaseAuditLogging verifies that for any Release operation
// (failed upstream request), a UsageLog record with Failed=true is written
// without accumulating Subscription counters.
//
// **Validates: Requirements 2.4, 2.5**
func TestProperty6_ReleaseAuditLogging(t *testing.T) {
	db := propTestDB(t, "prop6release")
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		inputTokens := rapid.Int64Range(0, 20000).Draw(rt, "inputTokens")
		outputTokens := rapid.Int64Range(0, 20000).Draw(rt, "outputTokens")
		cachedTokens := rapid.Int64Range(0, 5000).Draw(rt, "cachedTokens")
		reasoningTokens := rapid.Int64Range(0, 5000).Draw(rt, "reasoningTokens")
		rateMult := rapid.Float64Range(0.1, 5.0).Draw(rt, "rateMult")
		hasSubscription := rapid.Bool().Draw(rt, "hasSubscription")

		requestID := fmt.Sprintf("req-prop6-release-%d", iter)

		// Create user.
		user := &model.User{
			Email:        fmt.Sprintf("prop6release-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      200.0,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		// Optionally create a subscription with known initial counters.
		var subID *uint
		var initialDaily, initialWeekly, initialMonthly float64
		if hasSubscription {
			initialDaily = rapid.Float64Range(0, 50.0).Draw(rt, "initialDaily")
			initialWeekly = rapid.Float64Range(0, 200.0).Draw(rt, "initialWeekly")
			initialMonthly = rapid.Float64Range(0, 1000.0).Draw(rt, "initialMonthly")
			now := time.Now().UTC()
			sub := &model.Subscription{
				UserID:          user.ID,
				PackageID:       1,
				GroupID:         1,
				Status:          "active",
				StartsAt:        now.Add(-24 * time.Hour),
				ExpiresAt:       now.Add(30 * 24 * time.Hour),
				DailyUsageUSD:   initialDaily,
				WeeklyUsageUSD:  initialWeekly,
				MonthlyUsageUSD: initialMonthly,
				DailyResetAt:    now.Add(24 * time.Hour),
				WeeklyResetAt:   now.Add(7 * 24 * time.Hour),
				MonthlyResetAt:  now.Add(30 * 24 * time.Hour),
			}
			if err := db.Create(sub).Error; err != nil {
				rt.Fatalf("create subscription: %v", err)
			}
			id := sub.ID
			subID = &id
		}

		// Configure plugin with a ledger that succeeds on Release.
		led := &propFakeLedger{}
		calc := &propFakeCalc{cost: rapid.Float64Range(0.001, 100.0).Draw(rt, "cost")}
		plugin := NewUsagePlugin(db, led, calc)

		sc := &SettleCtx{
			RequestID:      requestID,
			UserID:         user.ID,
			RateMult:       rateMult,
			SubscriptionID: subID,
			Model:          "claude-3-opus",
			ApiKeyID:       uint(rapid.IntRange(0, 100).Draw(rt, "apiKeyID")),
			IPAddress:      "172.16.0.1",
		}
		ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

		// The upstream request FAILED (rec.Failed=true) → triggers Release path.
		rec := cliproxyusage.Record{
			Provider: "anthropic",
			Model:    "claude-3-opus",
			Failed:   true,
			Latency:  time.Duration(rapid.Int64Range(10, 5000).Draw(rt, "latencyMs")) * time.Millisecond,
			Detail: cliproxyusage.Detail{
				InputTokens:     inputTokens,
				OutputTokens:    outputTokens,
				CachedTokens:    cachedTokens,
				ReasoningTokens: reasoningTokens,
			},
		}

		plugin.HandleUsage(ctx, rec)

		// PROPERTY ASSERTION 1: A UsageLog with Failed=true SHALL exist.
		var failedLog model.UsageLog
		err := db.Where("request_id = ? AND failed = ?", requestID, true).First(&failedLog).Error
		if err != nil {
			rt.Fatalf("Property 6 violated: no UsageLog with Failed=true found for Release (request=%s): %v",
				requestID, err)
		}

		// PROPERTY ASSERTION 2: ledger.Release was called (not Settle).
		led.mu.Lock()
		releaseCount := led.releaseCalls
		settleCount := led.settleCalls
		led.mu.Unlock()
		if releaseCount < 1 {
			rt.Fatalf("Property 6 violated: ledger.Release not called for failed upstream (request=%s)", requestID)
		}
		if settleCount != 0 {
			rt.Fatalf("Property 6 violated: ledger.Settle called %d times for failed upstream (request=%s), want 0",
				settleCount, requestID)
		}

		// PROPERTY ASSERTION 3: Subscription counters NOT accumulated.
		if hasSubscription && subID != nil {
			var reloaded model.Subscription
			if err := db.First(&reloaded, *subID).Error; err != nil {
				rt.Fatalf("reload subscription: %v", err)
			}
			if reloaded.DailyUsageUSD != initialDaily {
				rt.Fatalf("Property 6 violated: DailyUsageUSD changed from %v to %v after Release",
					initialDaily, reloaded.DailyUsageUSD)
			}
			if reloaded.WeeklyUsageUSD != initialWeekly {
				rt.Fatalf("Property 6 violated: WeeklyUsageUSD changed from %v to %v after Release",
					initialWeekly, reloaded.WeeklyUsageUSD)
			}
			if reloaded.MonthlyUsageUSD != initialMonthly {
				rt.Fatalf("Property 6 violated: MonthlyUsageUSD changed from %v to %v after Release",
					initialMonthly, reloaded.MonthlyUsageUSD)
			}
		}
	})
}

// =============================================================================
// Feature: billing-system-optimization, Property 8: UsageLog Field Completeness
// =============================================================================

// TestProperty8_UsageLogFieldCompleteness verifies that for any UsageLog record
// written by the UsagePlugin, the ApiKeyID, GroupID, IPAddress, and IdempotencyKey
// fields are populated from the corresponding SettleCtx fields.
//
// This property generates random SettleCtx values (including edge cases like
// zero ApiKeyID, nil GroupID, empty IPAddress/IdempotencyKey) and asserts that
// the written UsageLog record faithfully reflects those values for both
// successful and failed Settle paths.
//
// **Validates: Requirements 3.2, 3.3, 10.6**
func TestProperty8_UsageLogFieldCompleteness_SuccessPath(t *testing.T) {
	db := propTestDB(t, "prop8ok")
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		// Generate random SettleCtx field values.
		// Use UintRange to stay within SQLite int64 positive range.
		apiKeyID := uint(rapid.UintRange(0, 1<<31-1).Draw(rt, "apiKeyID"))
		hasGroup := rapid.Bool().Draw(rt, "hasGroup")
		var groupID *uint
		if hasGroup {
			gid := uint(rapid.UintRange(1, 1<<31-1).Draw(rt, "groupID"))
			groupID = &gid
		}
		ipAddress := rapid.StringMatching(`[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}`).Draw(rt, "ipAddress")
		idempotencyKey := rapid.StringMatching(`[a-z0-9\-]{0,64}`).Draw(rt, "idempotencyKey")

		cost := rapid.Float64Range(0.001, 50.0).Draw(rt, "cost")
		inputTokens := rapid.Int64Range(1, 5000).Draw(rt, "inputTokens")
		outputTokens := rapid.Int64Range(1, 5000).Draw(rt, "outputTokens")
		requestID := fmt.Sprintf("req-prop8-ok-%d", iter)

		// Create user with sufficient balance.
		user := &model.User{
			Email:        fmt.Sprintf("prop8ok-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      1000.0,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		// Configure plugin with a ledger that succeeds.
		led := &propFakeLedger{}
		calc := &propFakeCalc{cost: cost}
		plugin := NewUsagePlugin(db, led, calc)

		sc := &SettleCtx{
			RequestID:      requestID,
			UserID:         user.ID,
			ApiKeyID:       apiKeyID,
			GroupID:        groupID,
			RateMult:       1.0,
			Model:          "gpt-4o",
			IPAddress:      ipAddress,
			IdempotencyKey: idempotencyKey,
		}
		ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

		rec := cliproxyusage.Record{
			Provider: "openai",
			Model:    "gpt-4o",
			Failed:   false,
			Latency:  100 * time.Millisecond,
			Detail: cliproxyusage.Detail{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
			},
		}

		plugin.HandleUsage(ctx, rec)

		// Verify UsageLog fields match SettleCtx.
		var usageLog model.UsageLog
		if err := db.Where("request_id = ?", requestID).First(&usageLog).Error; err != nil {
			rt.Fatalf("UsageLog not found: %v", err)
		}

		// PROPERTY ASSERTION: ApiKeyID matches SettleCtx.ApiKeyID.
		if usageLog.ApiKeyID != apiKeyID {
			rt.Fatalf("Property 8 violated: UsageLog.ApiKeyID = %d, want %d (from SettleCtx)",
				usageLog.ApiKeyID, apiKeyID)
		}

		// PROPERTY ASSERTION: GroupID matches SettleCtx.GroupID.
		if groupID == nil {
			if usageLog.GroupID != nil {
				rt.Fatalf("Property 8 violated: UsageLog.GroupID = %v, want nil (from SettleCtx)",
					usageLog.GroupID)
			}
		} else {
			if usageLog.GroupID == nil {
				rt.Fatalf("Property 8 violated: UsageLog.GroupID = nil, want %d (from SettleCtx)",
					*groupID)
			}
			if *usageLog.GroupID != *groupID {
				rt.Fatalf("Property 8 violated: UsageLog.GroupID = %d, want %d (from SettleCtx)",
					*usageLog.GroupID, *groupID)
			}
		}

		// PROPERTY ASSERTION: IPAddress matches SettleCtx.IPAddress.
		if usageLog.IPAddress != ipAddress {
			rt.Fatalf("Property 8 violated: UsageLog.IPAddress = %q, want %q (from SettleCtx)",
				usageLog.IPAddress, ipAddress)
		}

		// PROPERTY ASSERTION: IdempotencyKey matches SettleCtx.IdempotencyKey.
		if usageLog.IdempotencyKey != idempotencyKey {
			rt.Fatalf("Property 8 violated: UsageLog.IdempotencyKey = %q, want %q (from SettleCtx)",
				usageLog.IdempotencyKey, idempotencyKey)
		}
	})
}

// TestProperty8_UsageLogFieldCompleteness_FailedPath verifies that even when
// the Settle operation fails (or the upstream request fails), the UsageLog
// record written with Failed=true still carries the correct ApiKeyID, GroupID,
// IPAddress, and IdempotencyKey from SettleCtx.
//
// **Validates: Requirements 3.2, 3.3, 10.6**
func TestProperty8_UsageLogFieldCompleteness_FailedPath(t *testing.T) {
	db := propTestDB(t, "prop8fail")
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		// Generate random SettleCtx field values.
		// Use UintRange to stay within SQLite int64 positive range.
		apiKeyID := uint(rapid.UintRange(0, 1<<31-1).Draw(rt, "apiKeyID"))
		hasGroup := rapid.Bool().Draw(rt, "hasGroup")
		var groupID *uint
		if hasGroup {
			gid := uint(rapid.UintRange(1, 1<<31-1).Draw(rt, "groupID"))
			groupID = &gid
		}
		ipAddress := rapid.StringMatching(`[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}`).Draw(rt, "ipAddress")
		idempotencyKey := rapid.StringMatching(`[a-z0-9\-]{0,64}`).Draw(rt, "idempotencyKey")

		// Choose failure mode: upstream failed OR settle failed.
		upstreamFailed := rapid.Bool().Draw(rt, "upstreamFailed")

		cost := rapid.Float64Range(0.001, 50.0).Draw(rt, "cost")
		inputTokens := rapid.Int64Range(1, 5000).Draw(rt, "inputTokens")
		outputTokens := rapid.Int64Range(1, 5000).Draw(rt, "outputTokens")
		requestID := fmt.Sprintf("req-prop8-fail-%d", iter)

		// Create user.
		user := &model.User{
			Email:        fmt.Sprintf("prop8fail-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      100.0,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		var led *propFakeLedger
		if upstreamFailed {
			led = &propFakeLedger{} // Release path
		} else {
			led = &propFakeLedger{errOnSettle: errors.New("insufficient balance")} // Settle failure path
		}
		calc := &propFakeCalc{cost: cost}
		plugin := NewUsagePlugin(db, led, calc)

		sc := &SettleCtx{
			RequestID:      requestID,
			UserID:         user.ID,
			ApiKeyID:       apiKeyID,
			GroupID:        groupID,
			RateMult:       1.0,
			Model:          "gpt-4o",
			IPAddress:      ipAddress,
			IdempotencyKey: idempotencyKey,
		}
		ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

		rec := cliproxyusage.Record{
			Provider: "openai",
			Model:    "gpt-4o",
			Failed:   upstreamFailed,
			Latency:  50 * time.Millisecond,
			Detail: cliproxyusage.Detail{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
			},
		}

		plugin.HandleUsage(ctx, rec)

		// Verify UsageLog with Failed=true has correct fields from SettleCtx.
		var usageLog model.UsageLog
		if err := db.Where("request_id = ? AND failed = ?", requestID, true).First(&usageLog).Error; err != nil {
			rt.Fatalf("UsageLog (Failed=true) not found: %v", err)
		}

		// PROPERTY ASSERTION: ApiKeyID matches SettleCtx.ApiKeyID.
		if usageLog.ApiKeyID != apiKeyID {
			rt.Fatalf("Property 8 violated (failed path): UsageLog.ApiKeyID = %d, want %d",
				usageLog.ApiKeyID, apiKeyID)
		}

		// PROPERTY ASSERTION: GroupID matches SettleCtx.GroupID.
		if groupID == nil {
			if usageLog.GroupID != nil {
				rt.Fatalf("Property 8 violated (failed path): UsageLog.GroupID = %v, want nil",
					usageLog.GroupID)
			}
		} else {
			if usageLog.GroupID == nil {
				rt.Fatalf("Property 8 violated (failed path): UsageLog.GroupID = nil, want %d",
					*groupID)
			}
			if *usageLog.GroupID != *groupID {
				rt.Fatalf("Property 8 violated (failed path): UsageLog.GroupID = %d, want %d",
					*usageLog.GroupID, *groupID)
			}
		}

		// PROPERTY ASSERTION: IPAddress matches SettleCtx.IPAddress.
		if usageLog.IPAddress != ipAddress {
			rt.Fatalf("Property 8 violated (failed path): UsageLog.IPAddress = %q, want %q",
				usageLog.IPAddress, ipAddress)
		}

		// PROPERTY ASSERTION: IdempotencyKey matches SettleCtx.IdempotencyKey.
		if usageLog.IdempotencyKey != idempotencyKey {
			rt.Fatalf("Property 8 violated (failed path): UsageLog.IdempotencyKey = %q, want %q",
				usageLog.IdempotencyKey, idempotencyKey)
		}
	})
}

// =============================================================================
// Feature: billing-system-optimization, Property 15: Audit Linkage via RequestID
// =============================================================================

// propAuditLedger is a ledger fake that records Settle calls so that after
// HandleUsage returns, the test can write BalanceLog records (simulating the
// real Ledger behavior) and verify cross-table correlation between
// BalanceLog.Reference and UsageLog.RequestID.
//
// Note: We cannot write BalanceLog inside Settle because HandleUsage wraps
// the call in a SQLite transaction (single-connection), which would deadlock.
// Instead we record the call and write the BalanceLog after HandleUsage returns,
// which mirrors the real system where Ledger.Settle writes BalanceLog in its
// own PG transaction.
type propAuditLedger struct {
	mu           sync.Mutex
	settleUserID uint
	settleReqID  string
	settleAmount float64
	settled      bool
}

func (f *propAuditLedger) Hold(_ context.Context, _ uint, _ float64, _ string, _ time.Duration) error {
	return nil
}

func (f *propAuditLedger) Settle(_ context.Context, userID uint, requestID string, actualAmount float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settleUserID = userID
	f.settleReqID = requestID
	f.settleAmount = actualAmount
	f.settled = true
	return nil
}

func (f *propAuditLedger) Release(_ context.Context, _ uint, _ string) error {
	return nil
}

// ActiveHoldAmount satisfies the BillingLedger interface added in Stage 2
// (task 5.1). The audit-linkage tests always provide a fresh SettleCtx and
// non-nil usage records, so the fallback path is never taken.
func (f *propAuditLedger) ActiveHoldAmount(_ context.Context, _ uint, _ string) (float64, bool, error) {
	return 0, false, nil
}

// HasUnresolvedShortfall satisfies the BillingLedger interface extension
// added in task 1.6. Audit-linkage property tests exercise fully-funded
// flows without any shortfall bookkeeping, so the stub returns
// (false, nil).
func (f *propAuditLedger) HasUnresolvedShortfall(_ context.Context, _ uint) (bool, error) {
	return false, nil
}

// TestProperty15_AuditLinkageViaRequestID verifies that for any completed
// billing flow (Hold → Settle), the BalanceLog.Reference and UsageLog.RequestID
// contain the same value, enabling cross-table correlation for audit.
//
// This property generates random valid billing flows with varying request IDs,
// user IDs, costs, and token counts, then asserts that after HandleUsage
// completes successfully, both a BalanceLog record (with Reference=requestID)
// and a UsageLog record (with RequestID=requestID) exist and can be correlated.
//
// **Validates: Requirements 10.7**
func TestProperty15_AuditLinkageViaRequestID(t *testing.T) {
	db := propTestDB(t, "prop15")
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		// Generate random but valid inputs.
		userBalance := rapid.Float64Range(10.0, 10000.0).Draw(rt, "userBalance")
		cost := rapid.Float64Range(0.001, userBalance/2).Draw(rt, "cost")
		inputTokens := rapid.Int64Range(1, 50000).Draw(rt, "inputTokens")
		outputTokens := rapid.Int64Range(1, 50000).Draw(rt, "outputTokens")
		cachedTokens := rapid.Int64Range(0, 5000).Draw(rt, "cachedTokens")
		reasoningTokens := rapid.Int64Range(0, 5000).Draw(rt, "reasoningTokens")
		rateMult := rapid.Float64Range(0.5, 3.0).Draw(rt, "rateMult")

		// Generate a unique request ID (the key field for audit linkage).
		requestID := fmt.Sprintf("req-prop15-%d-%s", iter,
			rapid.StringMatching(`[a-z0-9]{8}`).Draw(rt, "reqSuffix"))

		// Create user with sufficient balance.
		user := &model.User{
			Email:        fmt.Sprintf("prop15-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      userBalance,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		// Use propAuditLedger which records Settle calls.
		led := &propAuditLedger{}
		calc := &propFakeCalc{cost: cost}
		plugin := NewUsagePlugin(db, led, calc)

		sc := &SettleCtx{
			RequestID: requestID,
			UserID:    user.ID,
			RateMult:  rateMult,
			Model:     "gpt-4o",
			ApiKeyID:  uint(rapid.UintRange(0, 1000).Draw(rt, "apiKeyID")),
			IPAddress: "10.0.0.1",
		}
		ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

		rec := cliproxyusage.Record{
			Provider: "openai",
			Model:    "gpt-4o",
			Failed:   false, // Successful upstream → triggers Settle path
			Latency:  100 * time.Millisecond,
			Detail: cliproxyusage.Detail{
				InputTokens:     inputTokens,
				OutputTokens:    outputTokens,
				CachedTokens:    cachedTokens,
				ReasoningTokens: reasoningTokens,
			},
		}

		plugin.HandleUsage(ctx, rec)

		// Verify Settle was called and record the BalanceLog (simulating real Ledger).
		led.mu.Lock()
		settled := led.settled
		settleReqID := led.settleReqID
		settleUserID := led.settleUserID
		settleAmount := led.settleAmount
		led.mu.Unlock()

		if !settled {
			rt.Fatalf("Property 15: ledger.Settle was not called for request %s", requestID)
		}

		// Write BalanceLog record simulating what the real Ledger does during Settle.
		// In production, Ledger.Settle writes this inside its own PG transaction.
		balanceLogEntry := &model.BalanceLog{
			UserID:    settleUserID,
			Amount:    -settleAmount,
			Type:      "settle",
			Reference: settleReqID,
		}
		if err := db.Create(balanceLogEntry).Error; err != nil {
			rt.Fatalf("write simulated BalanceLog: %v", err)
		}

		// PROPERTY ASSERTION 1: A UsageLog record with RequestID == requestID SHALL exist.
		var usageLog model.UsageLog
		err := db.Where("request_id = ? AND failed = ?", requestID, false).First(&usageLog).Error
		if err != nil {
			rt.Fatalf("Property 15 violated: no UsageLog found with RequestID=%q after successful Settle: %v",
				requestID, err)
		}

		// PROPERTY ASSERTION 2: A BalanceLog record with Reference == requestID SHALL exist.
		var balanceLog model.BalanceLog
		err = db.Where("reference = ? AND type = ?", requestID, "settle").First(&balanceLog).Error
		if err != nil {
			rt.Fatalf("Property 15 violated: no BalanceLog found with Reference=%q and Type='settle' after successful Settle: %v",
				requestID, err)
		}

		// PROPERTY ASSERTION 3: The linkage holds — BalanceLog.Reference == UsageLog.RequestID.
		if balanceLog.Reference != usageLog.RequestID {
			rt.Fatalf("Property 15 violated: BalanceLog.Reference=%q != UsageLog.RequestID=%q (should be equal for audit linkage)",
				balanceLog.Reference, usageLog.RequestID)
		}

		// PROPERTY ASSERTION 4: Both records reference the same user.
		if balanceLog.UserID != usageLog.UserID {
			rt.Fatalf("Property 15 violated: BalanceLog.UserID=%d != UsageLog.UserID=%d for RequestID=%q",
				balanceLog.UserID, usageLog.UserID, requestID)
		}

		// PROPERTY ASSERTION 5: The RequestID passed to ledger.Settle matches
		// the SettleCtx.RequestID (ensuring the Ledger receives the correct ID
		// for writing its BalanceLog.Reference in production).
		if settleReqID != requestID {
			rt.Fatalf("Property 15 violated: ledger.Settle received requestID=%q, want %q",
				settleReqID, requestID)
		}
	})
}

// =============================================================================
// Feature: billing-system-optimization, Property 14: Billing Operation Audit Completeness
// =============================================================================

// TestProperty14_BillingOperationAuditCompleteness_Settle verifies that for any
// successful Settle operation, a BalanceLog record is created with Type="settle",
// Amount=-actualCost, Reference=requestID, and Metadata JSON containing user_id,
// model, provider, and timestamp.
//
// This test uses a real Ledger (with miniredis + SQLite) to exercise the full
// audit trail through UsagePlugin → Ledger → BalanceLog.
//
// **Validates: Requirements 10.1, 10.2, 10.3, 10.4, 10.5**
func TestProperty14_BillingOperationAuditCompleteness_Settle(t *testing.T) {
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		// Generate random inputs.
		userBalance := rapid.Float64Range(10.0, 1000.0).Draw(rt, "userBalance")
		cost := rapid.Float64Range(0.01, userBalance/2).Draw(rt, "cost")
		inputTokens := rapid.Int64Range(1, 10000).Draw(rt, "inputTokens")
		outputTokens := rapid.Int64Range(1, 10000).Draw(rt, "outputTokens")
		rateMult := rapid.Float64Range(0.5, 3.0).Draw(rt, "rateMult")
		modelName := rapid.SampledFrom([]string{"gpt-4o", "claude-3-opus", "gemini-pro", "codex-mini"}).Draw(rt, "model")
		provider := rapid.SampledFrom([]string{"openai", "anthropic", "google", "codex"}).Draw(rt, "provider")
		requestID := fmt.Sprintf("req-prop14-settle-%d", iter)

		// Fresh DB for each iteration to avoid cross-contamination.
		// Use WAL mode and allow multiple connections so nested transactions
		// (UsagePlugin tx → Ledger.Settle tx) don't deadlock on SQLite.
		dsn := fmt.Sprintf("file:prop14_settle_%d?mode=memory&cache=shared&_journal_mode=WAL", iter)
		db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Silent),
		})
		if err != nil {
			rt.Fatalf("open sqlite: %v", err)
		}
		sqlDB, _ := db.DB()
		sqlDB.SetMaxOpenConns(2)
		if err := db.AutoMigrate(&model.User{}, &model.UsageLog{}, &model.BalanceLog{}, &model.Subscription{}); err != nil {
			rt.Fatalf("automigrate: %v", err)
		}

		// Create user with sufficient balance.
		user := &model.User{
			Email:        fmt.Sprintf("prop14settle-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      userBalance,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		// Create a real Ledger backed by miniredis.
		redisClient, _ := testutil.MustMiniRedis(t)
		ldg := ledger.NewWithConfig(db, redisClient, 30*time.Second, 5*time.Minute)

		// Pre-hold so that Settle can find and remove the hold entry.
		if err := ldg.Hold(context.Background(), user.ID, cost*2, requestID, 5*time.Minute); err != nil {
			rt.Fatalf("pre-hold: %v", err)
		}

		// Configure UsagePlugin with the real ledger.
		calc := &propFakeCalc{cost: cost}
		plugin := NewUsagePlugin(db, ldg, calc)

		sc := &SettleCtx{
			RequestID: requestID,
			UserID:    user.ID,
			RateMult:  rateMult,
			Model:     modelName,
			ApiKeyID:  42,
			IPAddress: "10.0.0.1",
		}
		ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

		rec := cliproxyusage.Record{
			Provider: provider,
			Model:    modelName,
			Failed:   false,
			Latency:  100 * time.Millisecond,
			Detail: cliproxyusage.Detail{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
			},
		}

		plugin.HandleUsage(ctx, rec)

		// PROPERTY ASSERTION: A BalanceLog with Type="settle" SHALL exist for this requestID.
		var settleLog model.BalanceLog
		err = db.Where("reference = ? AND type = ?", requestID, "settle").First(&settleLog).Error
		if err != nil {
			rt.Fatalf("Property 14 violated: no BalanceLog with Type='settle' found for request=%s: %v",
				requestID, err)
		}

		// PROPERTY ASSERTION: Amount == -actualCost.
		expectedAmount := -cost
		if settleLog.Amount-expectedAmount > 0.0001 || settleLog.Amount-expectedAmount < -0.0001 {
			rt.Fatalf("Property 14 violated: BalanceLog.Amount = %v, want %v (request=%s)",
				settleLog.Amount, expectedAmount, requestID)
		}

		// PROPERTY ASSERTION: Reference == requestID.
		if settleLog.Reference != requestID {
			rt.Fatalf("Property 14 violated: BalanceLog.Reference = %q, want %q",
				settleLog.Reference, requestID)
		}

		// PROPERTY ASSERTION: UserID matches.
		if settleLog.UserID != user.ID {
			rt.Fatalf("Property 14 violated: BalanceLog.UserID = %d, want %d",
				settleLog.UserID, user.ID)
		}

		// PROPERTY ASSERTION: Metadata JSON contains user_id and timestamp.
		if len(settleLog.Metadata) == 0 {
			rt.Fatalf("Property 14 violated: BalanceLog.Metadata is empty for settle (request=%s)", requestID)
		}
		var meta map[string]interface{}
		if err := json.Unmarshal(settleLog.Metadata, &meta); err != nil {
			rt.Fatalf("Property 14 violated: BalanceLog.Metadata is not valid JSON: %v", err)
		}
		if _, ok := meta["user_id"]; !ok {
			rt.Fatalf("Property 14 violated: Metadata missing 'user_id' (request=%s, meta=%s)",
				requestID, string(settleLog.Metadata))
		}
		if _, ok := meta["timestamp"]; !ok {
			rt.Fatalf("Property 14 violated: Metadata missing 'timestamp' (request=%s, meta=%s)",
				requestID, string(settleLog.Metadata))
		}
	})
}

// TestProperty14_BillingOperationAuditCompleteness_SettleFailed verifies that
// when the tenant's persistent balance is insufficient for the computed
// actualCost, `ledger.Settle` records the partial debit and a shortfall
// marker BalanceLog row (rather than the legacy settle_failed row), and
// `UsagePlugin` surfaces the shortfall through `UsageLog.RawMetadata`.
//
// The pre-task-1.3 semantics of `ErrInsufficientBalance` no longer apply —
// Settle now always succeeds for logical outcomes and shortfall reconciliation
// happens out-of-band via `HasUnresolvedShortfall`. See Requirements 2.2, 2.3,
// 2.4 and design.md "Data Model Changes" / "Unresolved shortfall query
// predicate" for the shortfall contract.
//
// **Validates: Requirements 2.2, 2.3, 2.4**
func TestProperty14_BillingOperationAuditCompleteness_SettleFailed(t *testing.T) {
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		// User has very low balance so Settle takes the partial-debit path:
		// debited = userBalance; shortfall = cost - userBalance.
		userBalance := rapid.Float64Range(0.01, 1.0).Draw(rt, "userBalance")
		cost := rapid.Float64Range(userBalance+1.0, userBalance+100.0).Draw(rt, "cost")
		inputTokens := rapid.Int64Range(1, 10000).Draw(rt, "inputTokens")
		outputTokens := rapid.Int64Range(1, 10000).Draw(rt, "outputTokens")
		modelName := rapid.SampledFrom([]string{"gpt-4o", "claude-3-opus", "gemini-pro"}).Draw(rt, "model")
		provider := rapid.SampledFrom([]string{"openai", "anthropic", "google"}).Draw(rt, "provider")
		requestID := fmt.Sprintf("req-prop14-sfail-%d", iter)

		// Fresh DB for each iteration.
		// Use WAL mode and allow multiple connections so nested transactions
		// (UsagePlugin tx → Ledger.Settle tx) don't deadlock on SQLite.
		dsn := fmt.Sprintf("file:prop14_sfail_%d?mode=memory&cache=shared&_journal_mode=WAL", iter)
		db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Silent),
		})
		if err != nil {
			rt.Fatalf("open sqlite: %v", err)
		}
		sqlDB, _ := db.DB()
		sqlDB.SetMaxOpenConns(2)
		if err := db.AutoMigrate(&model.User{}, &model.UsageLog{}, &model.BalanceLog{}, &model.Subscription{}); err != nil {
			rt.Fatalf("automigrate: %v", err)
		}

		// Create user with insufficient balance for the cost.
		user := &model.User{
			Email:        fmt.Sprintf("prop14sfail-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      userBalance,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		// Create a real Ledger backed by miniredis.
		redisClient, _ := testutil.MustMiniRedis(t)
		ldg := ledger.NewWithConfig(db, redisClient, 30*time.Second, 5*time.Minute)

		// Pre-hold with a small amount so the Hold Lua script passes. The
		// hold value is unrelated to the partial-debit path we are
		// validating here — ledger.Settle reads `user.Balance` directly.
		holdAmount := userBalance * 0.5
		if holdAmount <= 0 {
			holdAmount = 0.001
		}
		_ = ldg.Hold(context.Background(), user.ID, holdAmount, requestID, 5*time.Minute)

		// Configure UsagePlugin with the real ledger.
		calc := &propFakeCalc{cost: cost}
		plugin := NewUsagePlugin(db, ldg, calc)

		sc := &SettleCtx{
			RequestID: requestID,
			UserID:    user.ID,
			RateMult:  1.0,
			Model:     modelName,
			ApiKeyID:  10,
			IPAddress: "192.168.1.1",
		}
		ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

		rec := cliproxyusage.Record{
			Provider: provider,
			Model:    modelName,
			Failed:   false, // upstream succeeded; balance < cost triggers shortfall
			Latency:  50 * time.Millisecond,
			Detail: cliproxyusage.Detail{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
			},
		}

		plugin.HandleUsage(ctx, rec)

		// PROPERTY ASSERTION 1: A BalanceLog with Type="settle" and
		// Metadata.shortfall_usd > 0 SHALL exist for this request.
		var settleRows []model.BalanceLog
		if err := db.Where("reference = ? AND type = ?", requestID, "settle").
			Find(&settleRows).Error; err != nil {
			rt.Fatalf("Property 14 violated: query settle rows: %v", err)
		}
		if len(settleRows) == 0 {
			rt.Fatalf("Property 14 violated: no BalanceLog with type='settle' found for request=%s", requestID)
		}

		var shortfallRow *model.BalanceLog
		for i := range settleRows {
			if len(settleRows[i].Metadata) == 0 {
				continue
			}
			var meta map[string]interface{}
			if err := json.Unmarshal(settleRows[i].Metadata, &meta); err != nil {
				continue
			}
			if v, ok := meta["shortfall_usd"].(float64); ok && v > 0 {
				shortfallRow = &settleRows[i]
				break
			}
		}
		if shortfallRow == nil {
			rt.Fatalf("Property 14 violated: no BalanceLog settle row with shortfall_usd > 0 for request=%s (rows=%d)",
				requestID, len(settleRows))
		}

		// PROPERTY ASSERTION 2: shortfall row Metadata contains the
		// shortfall amount plus the audit fields from buildAuditMetadata
		// (user_id, timestamp).
		var meta map[string]interface{}
		if err := json.Unmarshal(shortfallRow.Metadata, &meta); err != nil {
			rt.Fatalf("Property 14 violated: BalanceLog.Metadata is not valid JSON: %v", err)
		}
		shortfall, _ := meta["shortfall_usd"].(float64)
		if shortfall <= 0 {
			rt.Fatalf("Property 14 violated: shortfall_usd=%v, want > 0 (request=%s)", shortfall, requestID)
		}
		if _, ok := meta["user_id"]; !ok {
			rt.Fatalf("Property 14 violated: Metadata missing 'user_id' for shortfall row (request=%s)", requestID)
		}
		if _, ok := meta["timestamp"]; !ok {
			rt.Fatalf("Property 14 violated: Metadata missing 'timestamp' for shortfall row (request=%s)", requestID)
		}

		// PROPERTY ASSERTION 3: the matching UsageLog row carries the
		// same shortfall_usd in RawMetadata (Requirement 2.4).
		var usageLog model.UsageLog
		if err := db.Where("request_id = ?", requestID).First(&usageLog).Error; err != nil {
			rt.Fatalf("Property 14 violated: no UsageLog for request=%s: %v", requestID, err)
		}
		if usageLog.Failed {
			rt.Fatalf("Property 14 violated: UsageLog.Failed=true on partial settle (request=%s)", requestID)
		}
		if len(usageLog.RawMetadata) == 0 {
			rt.Fatalf("Property 14 violated: UsageLog.RawMetadata empty on partial settle (request=%s)", requestID)
		}
		var rawMeta map[string]interface{}
		if err := json.Unmarshal(usageLog.RawMetadata, &rawMeta); err != nil {
			rt.Fatalf("Property 14 violated: UsageLog.RawMetadata not valid JSON: %v", err)
		}
		rawShortfall, _ := rawMeta["shortfall_usd"].(float64)
		// Allow a small epsilon for float round-trip through JSON.
		if diff := rawShortfall - shortfall; diff < -0.0001 || diff > 0.0001 {
			rt.Fatalf("Property 14 violated: UsageLog.RawMetadata.shortfall_usd=%v, BalanceLog.shortfall_usd=%v (request=%s)",
				rawShortfall, shortfall, requestID)
		}
	})
}

// TestProperty14_BillingOperationAuditCompleteness_Release verifies that for any
// Release operation (failed upstream), a BalanceLog record is created with
// Type="release", Amount=0, Reference=requestID, and Metadata containing user_id
// and timestamp.
//
// **Validates: Requirements 10.1, 10.2, 10.3, 10.4, 10.5**
func TestProperty14_BillingOperationAuditCompleteness_Release(t *testing.T) {
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		userBalance := rapid.Float64Range(10.0, 1000.0).Draw(rt, "userBalance")
		inputTokens := rapid.Int64Range(0, 10000).Draw(rt, "inputTokens")
		outputTokens := rapid.Int64Range(0, 10000).Draw(rt, "outputTokens")
		modelName := rapid.SampledFrom([]string{"gpt-4o", "claude-3-opus", "gemini-pro", "codex-mini"}).Draw(rt, "model")
		provider := rapid.SampledFrom([]string{"openai", "anthropic", "google", "codex"}).Draw(rt, "provider")
		requestID := fmt.Sprintf("req-prop14-release-%d", iter)

		// Fresh DB for each iteration.
		dsn := fmt.Sprintf("file:prop14_release_%d?mode=memory&cache=shared&_journal_mode=WAL", iter)
		db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Silent),
		})
		if err != nil {
			rt.Fatalf("open sqlite: %v", err)
		}
		sqlDB, _ := db.DB()
		sqlDB.SetMaxOpenConns(2)
		if err := db.AutoMigrate(&model.User{}, &model.UsageLog{}, &model.BalanceLog{}, &model.Subscription{}); err != nil {
			rt.Fatalf("automigrate: %v", err)
		}

		// Create user.
		user := &model.User{
			Email:        fmt.Sprintf("prop14release-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      userBalance,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		// Create a real Ledger backed by miniredis.
		redisClient, _ := testutil.MustMiniRedis(t)
		ldg := ledger.NewWithConfig(db, redisClient, 30*time.Second, 5*time.Minute)

		// Pre-hold so that Release can find and remove the hold entry.
		holdAmount := rapid.Float64Range(0.01, userBalance/2).Draw(rt, "holdAmount")
		if err := ldg.Hold(context.Background(), user.ID, holdAmount, requestID, 5*time.Minute); err != nil {
			rt.Fatalf("pre-hold: %v", err)
		}

		// Configure UsagePlugin with the real ledger.
		calc := &propFakeCalc{cost: 0.01}
		plugin := NewUsagePlugin(db, ldg, calc)

		sc := &SettleCtx{
			RequestID: requestID,
			UserID:    user.ID,
			RateMult:  1.0,
			Model:     modelName,
			ApiKeyID:  5,
			IPAddress: "172.16.0.1",
		}
		ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

		// The upstream request FAILED → triggers Release path.
		rec := cliproxyusage.Record{
			Provider: provider,
			Model:    modelName,
			Failed:   true,
			Latency:  50 * time.Millisecond,
			Detail: cliproxyusage.Detail{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
			},
		}

		plugin.HandleUsage(ctx, rec)

		// PROPERTY ASSERTION: A BalanceLog with Type="release" SHALL exist for this requestID.
		var releaseLog model.BalanceLog
		err = db.Where("reference = ? AND type = ?", requestID, "release").First(&releaseLog).Error
		if err != nil {
			rt.Fatalf("Property 14 violated: no BalanceLog with Type='release' found for request=%s: %v",
				requestID, err)
		}

		// PROPERTY ASSERTION: Amount == 0.
		if releaseLog.Amount != 0 {
			rt.Fatalf("Property 14 violated: BalanceLog.Amount = %v, want 0 for release (request=%s)",
				releaseLog.Amount, requestID)
		}

		// PROPERTY ASSERTION: Reference == requestID.
		if releaseLog.Reference != requestID {
			rt.Fatalf("Property 14 violated: BalanceLog.Reference = %q, want %q",
				releaseLog.Reference, requestID)
		}

		// PROPERTY ASSERTION: UserID matches.
		if releaseLog.UserID != user.ID {
			rt.Fatalf("Property 14 violated: BalanceLog.UserID = %d, want %d",
				releaseLog.UserID, user.ID)
		}

		// PROPERTY ASSERTION: Metadata JSON contains user_id and timestamp.
		if len(releaseLog.Metadata) == 0 {
			rt.Fatalf("Property 14 violated: BalanceLog.Metadata is empty for release (request=%s)", requestID)
		}
		var meta map[string]interface{}
		if err := json.Unmarshal(releaseLog.Metadata, &meta); err != nil {
			rt.Fatalf("Property 14 violated: BalanceLog.Metadata is not valid JSON: %v", err)
		}
		if _, ok := meta["user_id"]; !ok {
			rt.Fatalf("Property 14 violated: Metadata missing 'user_id' for release (request=%s, meta=%s)",
				requestID, string(releaseLog.Metadata))
		}
		if _, ok := meta["timestamp"]; !ok {
			rt.Fatalf("Property 14 violated: Metadata missing 'timestamp' for release (request=%s, meta=%s)",
				requestID, string(releaseLog.Metadata))
		}
	})
}

// TestProperty14_BillingOperationAuditCompleteness_Hold verifies that for any
// Hold operation, a BalanceLog record is created with Type="hold", Amount equal
// to the hold amount, Reference=requestID, and Metadata containing user_id and
// timestamp.
//
// **Validates: Requirements 10.1, 10.2, 10.3, 10.4, 10.5**
func TestProperty14_BillingOperationAuditCompleteness_Hold(t *testing.T) {
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		userBalance := rapid.Float64Range(10.0, 1000.0).Draw(rt, "userBalance")
		holdAmount := rapid.Float64Range(0.01, userBalance/2).Draw(rt, "holdAmount")
		requestID := fmt.Sprintf("req-prop14-hold-%d", iter)

		// Fresh DB for each iteration.
		dsn := fmt.Sprintf("file:prop14_hold_%d?mode=memory&cache=shared&_journal_mode=WAL", iter)
		db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Silent),
		})
		if err != nil {
			rt.Fatalf("open sqlite: %v", err)
		}
		sqlDB, _ := db.DB()
		sqlDB.SetMaxOpenConns(2)
		if err := db.AutoMigrate(&model.User{}, &model.UsageLog{}, &model.BalanceLog{}, &model.Subscription{}); err != nil {
			rt.Fatalf("automigrate: %v", err)
		}

		// Create user with sufficient balance.
		user := &model.User{
			Email:        fmt.Sprintf("prop14hold-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      userBalance,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		// Create a real Ledger backed by miniredis.
		redisClient, _ := testutil.MustMiniRedis(t)
		ldg := ledger.NewWithConfig(db, redisClient, 30*time.Second, 5*time.Minute)

		// Execute Hold operation.
		err = ldg.Hold(context.Background(), user.ID, holdAmount, requestID, 5*time.Minute)
		if err != nil {
			rt.Fatalf("Hold failed: %v", err)
		}

		// PROPERTY ASSERTION: A BalanceLog with Type="hold" SHALL exist for this requestID.
		var holdLog model.BalanceLog
		err = db.Where("reference = ? AND type = ?", requestID, "hold").First(&holdLog).Error
		if err != nil {
			rt.Fatalf("Property 14 violated: no BalanceLog with Type='hold' found for request=%s: %v",
				requestID, err)
		}

		// PROPERTY ASSERTION: Amount == holdAmount.
		if holdLog.Amount-holdAmount > 0.0001 || holdLog.Amount-holdAmount < -0.0001 {
			rt.Fatalf("Property 14 violated: BalanceLog.Amount = %v, want %v for hold (request=%s)",
				holdLog.Amount, holdAmount, requestID)
		}

		// PROPERTY ASSERTION: Reference == requestID.
		if holdLog.Reference != requestID {
			rt.Fatalf("Property 14 violated: BalanceLog.Reference = %q, want %q",
				holdLog.Reference, requestID)
		}

		// PROPERTY ASSERTION: UserID matches.
		if holdLog.UserID != user.ID {
			rt.Fatalf("Property 14 violated: BalanceLog.UserID = %d, want %d",
				holdLog.UserID, user.ID)
		}

		// PROPERTY ASSERTION: Metadata JSON contains user_id and timestamp.
		if len(holdLog.Metadata) == 0 {
			rt.Fatalf("Property 14 violated: BalanceLog.Metadata is empty for hold (request=%s)", requestID)
		}
		var meta map[string]interface{}
		if err := json.Unmarshal(holdLog.Metadata, &meta); err != nil {
			rt.Fatalf("Property 14 violated: BalanceLog.Metadata is not valid JSON: %v", err)
		}
		if _, ok := meta["user_id"]; !ok {
			rt.Fatalf("Property 14 violated: Metadata missing 'user_id' for hold (request=%s, meta=%s)",
				requestID, string(holdLog.Metadata))
		}
		if _, ok := meta["timestamp"]; !ok {
			rt.Fatalf("Property 14 violated: Metadata missing 'timestamp' for hold (request=%s, meta=%s)",
				requestID, string(holdLog.Metadata))
		}
	})
}

// =============================================================================
// Feature: billing-system-optimization, Property 20: Budget Token Settle Deduction
// =============================================================================

// TestProperty20_BudgetTokenSettleDeduction verifies that for any Settle
// operation where a BudgetToken is active for the user, the actual cost SHALL
// be deducted from the BudgetToken's remaining budget rather than performing
// an individual Redis Settle.
//
// This property generates random valid billing flows with varying costs,
// initial token budgets, and token TTLs, then asserts that after a successful
// Settle, the BudgetToken's Remaining field is reduced by exactly the actual
// cost computed by the pricing calculator.
//
// **Validates: Requirements 11.6**
func TestProperty20_BudgetTokenSettleDeduction(t *testing.T) {
	db := propTestDB(t, "prop20")
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		// Generate random inputs.
		initialBudget := rapid.Float64Range(1.0, 500.0).Draw(rt, "initialBudget")
		cost := rapid.Float64Range(0.001, initialBudget).Draw(rt, "cost")
		inputTokens := rapid.Int64Range(1, 10000).Draw(rt, "inputTokens")
		outputTokens := rapid.Int64Range(1, 10000).Draw(rt, "outputTokens")
		cachedTokens := rapid.Int64Range(0, 2000).Draw(rt, "cachedTokens")
		reasoningTokens := rapid.Int64Range(0, 2000).Draw(rt, "reasoningTokens")
		rateMult := rapid.Float64Range(0.5, 3.0).Draw(rt, "rateMult")
		userBalance := rapid.Float64Range(cost+1.0, 10000.0).Draw(rt, "userBalance")
		requestID := fmt.Sprintf("req-prop20-%d", iter)

		// Create user with sufficient balance.
		user := &model.User{
			Email:        fmt.Sprintf("prop20-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      userBalance,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		// Create a BudgetTokenStore and acquire a token for the user.
		store := NewBudgetTokenStore()
		batchID := fmt.Sprintf("batch-prop20-%d", iter)
		store.Acquire(user.ID, batchID, initialBudget, 60*time.Second)

		// Configure plugin with a ledger that succeeds and attach the budget token store.
		led := &propFakeLedger{}
		calc := &propFakeCalc{cost: cost}
		plugin := NewUsagePlugin(db, led, calc)
		plugin.SetBudgetTokenStore(store)

		sc := &SettleCtx{
			RequestID: requestID,
			UserID:    user.ID,
			RateMult:  rateMult,
			Model:     "gpt-4o",
			ApiKeyID:  uint(rapid.UintRange(0, 100).Draw(rt, "apiKeyID")),
			IPAddress: "10.0.0.1",
		}
		ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

		rec := cliproxyusage.Record{
			Provider: "openai",
			Model:    "gpt-4o",
			Failed:   false,
			Latency:  100 * time.Millisecond,
			Detail: cliproxyusage.Detail{
				InputTokens:     inputTokens,
				OutputTokens:    outputTokens,
				CachedTokens:    cachedTokens,
				ReasoningTokens: reasoningTokens,
			},
		}

		plugin.HandleUsage(ctx, rec)

		// PROPERTY ASSERTION: The BudgetToken's Remaining SHALL be reduced by
		// exactly the actual cost after a successful Settle.
		store.mu.RLock()
		token, ok := store.tokens[user.ID]
		store.mu.RUnlock()
		if !ok {
			rt.Fatalf("Property 20 violated: BudgetToken not found for user %d after Settle", user.ID)
		}

		token.mu.Lock()
		remaining := token.Remaining
		token.mu.Unlock()

		expectedRemaining := initialBudget - cost
		diff := remaining - expectedRemaining
		if diff > 0.0001 || diff < -0.0001 {
			rt.Fatalf("Property 20 violated: BudgetToken.Remaining = %v, want %v (initial=%v, cost=%v, user=%d)",
				remaining, expectedRemaining, initialBudget, cost, user.ID)
		}
	})
}

// TestProperty20_BudgetTokenSettleDeduction_NoTokenNoop verifies that when no
// BudgetToken is active for the user, the Settle operation completes normally
// without any budget token deduction (no panic, no error).
//
// **Validates: Requirements 11.6**
func TestProperty20_BudgetTokenSettleDeduction_NoTokenNoop(t *testing.T) {
	db := propTestDB(t, "prop20noop")
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		cost := rapid.Float64Range(0.001, 100.0).Draw(rt, "cost")
		inputTokens := rapid.Int64Range(1, 5000).Draw(rt, "inputTokens")
		outputTokens := rapid.Int64Range(1, 5000).Draw(rt, "outputTokens")
		userBalance := rapid.Float64Range(cost+1.0, 5000.0).Draw(rt, "userBalance")
		requestID := fmt.Sprintf("req-prop20-noop-%d", iter)

		// Create user with sufficient balance.
		user := &model.User{
			Email:        fmt.Sprintf("prop20noop-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      userBalance,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		// Create a BudgetTokenStore but do NOT acquire a token for this user.
		store := NewBudgetTokenStore()

		// Configure plugin with a ledger that succeeds and attach the empty store.
		led := &propFakeLedger{}
		calc := &propFakeCalc{cost: cost}
		plugin := NewUsagePlugin(db, led, calc)
		plugin.SetBudgetTokenStore(store)

		sc := &SettleCtx{
			RequestID: requestID,
			UserID:    user.ID,
			RateMult:  1.0,
			Model:     "gpt-4o",
			ApiKeyID:  1,
			IPAddress: "10.0.0.1",
		}
		ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

		rec := cliproxyusage.Record{
			Provider: "openai",
			Model:    "gpt-4o",
			Failed:   false,
			Latency:  50 * time.Millisecond,
			Detail: cliproxyusage.Detail{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
			},
		}

		// Should not panic or error even without a token.
		plugin.HandleUsage(ctx, rec)

		// Verify Settle was still called (the ledger path works normally).
		led.mu.Lock()
		settleCount := led.settleCalls
		led.mu.Unlock()
		if settleCount != 1 {
			rt.Fatalf("Property 20 (no-token): expected 1 Settle call, got %d", settleCount)
		}

		// Verify no token was created as a side effect.
		store.mu.RLock()
		_, exists := store.tokens[user.ID]
		store.mu.RUnlock()
		if exists {
			rt.Fatalf("Property 20 (no-token): BudgetToken unexpectedly created for user %d", user.ID)
		}
	})
}

// TestProperty20_BudgetTokenSettleDeduction_MultipleSettles verifies that
// multiple sequential Settle operations each deduct their respective actual
// costs from the same BudgetToken, and the cumulative remaining is correct.
//
// **Validates: Requirements 11.6**
func TestProperty20_BudgetTokenSettleDeduction_MultipleSettles(t *testing.T) {
	db := propTestDB(t, "prop20multi")
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		numSettles := rapid.IntRange(2, 5).Draw(rt, "numSettles")
		initialBudget := rapid.Float64Range(10.0, 1000.0).Draw(rt, "initialBudget")
		userBalance := rapid.Float64Range(initialBudget+100.0, 10000.0).Draw(rt, "userBalance")

		// Create user with sufficient balance.
		user := &model.User{
			Email:        fmt.Sprintf("prop20multi-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      userBalance,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		// Create a BudgetTokenStore and acquire a token.
		store := NewBudgetTokenStore()
		batchID := fmt.Sprintf("batch-prop20-multi-%d", iter)
		store.Acquire(user.ID, batchID, initialBudget, 60*time.Second)

		// Generate costs that sum to less than initialBudget (so remaining stays positive).
		maxPerSettle := initialBudget / float64(numSettles+1)
		var totalCost float64
		costs := make([]float64, numSettles)
		for i := 0; i < numSettles; i++ {
			c := rapid.Float64Range(0.001, maxPerSettle).Draw(rt, fmt.Sprintf("cost_%d", i))
			costs[i] = c
			totalCost += c
		}

		// Execute multiple Settle operations sequentially.
		for i := 0; i < numSettles; i++ {
			requestID := fmt.Sprintf("req-prop20-multi-%d-%d", iter, i)

			led := &propFakeLedger{}
			calc := &propFakeCalc{cost: costs[i]}
			plugin := NewUsagePlugin(db, led, calc)
			plugin.SetBudgetTokenStore(store)

			sc := &SettleCtx{
				RequestID: requestID,
				UserID:    user.ID,
				RateMult:  1.0,
				Model:     "gpt-4o",
				ApiKeyID:  1,
				IPAddress: "10.0.0.1",
			}
			ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

			rec := cliproxyusage.Record{
				Provider: "openai",
				Model:    "gpt-4o",
				Failed:   false,
				Latency:  50 * time.Millisecond,
				Detail: cliproxyusage.Detail{
					InputTokens:  100,
					OutputTokens: 50,
				},
			}

			plugin.HandleUsage(ctx, rec)
		}

		// PROPERTY ASSERTION: After all Settles, the BudgetToken's Remaining
		// SHALL equal initialBudget - sum(costs).
		store.mu.RLock()
		token, ok := store.tokens[user.ID]
		store.mu.RUnlock()
		if !ok {
			rt.Fatalf("Property 20 (multi): BudgetToken not found for user %d after %d Settles",
				user.ID, numSettles)
		}

		token.mu.Lock()
		remaining := token.Remaining
		token.mu.Unlock()

		expectedRemaining := initialBudget - totalCost
		diff := remaining - expectedRemaining
		if diff > 0.001 || diff < -0.001 {
			rt.Fatalf("Property 20 (multi) violated: BudgetToken.Remaining = %v, want %v (initial=%v, totalCost=%v, numSettles=%d)",
				remaining, expectedRemaining, initialBudget, totalCost, numSettles)
		}
	})
}

// =============================================================================
// Feature: billing-system-optimization, Property 24: Low Balance Warning Event
// =============================================================================

// TestProperty24_LowBalanceWarningEvent verifies that for any Settle operation
// that causes the user's available balance to drop below the configured warning
// threshold (LowBalanceThresholdUSD), a BalanceLog record with
// Type="low_balance_warning" is created.
//
// The property generates random scenarios where:
//   - The user's balance before Settle is above the threshold
//   - The actual cost causes the balance to drop below the threshold (but remain positive)
//
// After HandleUsage completes, the test asserts that a BalanceLog record with
// Type="low_balance_warning" exists for the request.
//
// **Validates: Requirements 14.1**
func TestProperty24_LowBalanceWarningEvent(t *testing.T) {
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++

		// Generate a configurable threshold.
		threshold := rapid.Float64Range(0.5, 10.0).Draw(rt, "threshold")

		// User balance must be above threshold before Settle.
		// After Settle, balance must be below threshold but still positive.
		// So: balanceAfter = balanceBefore - cost, where balanceAfter in (0, threshold)
		// and balanceBefore >= threshold.
		balanceAfter := rapid.Float64Range(0.01, threshold-0.001).Draw(rt, "balanceAfter")
		cost := rapid.Float64Range(0.01, 100.0).Draw(rt, "cost")
		balanceBefore := balanceAfter + cost

		// Ensure balanceBefore >= threshold (it should be since balanceAfter < threshold
		// and cost > 0, so balanceBefore = balanceAfter + cost > balanceAfter).
		// But we also need balanceBefore >= threshold explicitly.
		if balanceBefore < threshold {
			// This can happen if cost is very small and balanceAfter is close to threshold.
			// Adjust cost to ensure the crossing.
			cost = threshold - balanceAfter + rapid.Float64Range(0.01, 5.0).Draw(rt, "extraCost")
			balanceBefore = balanceAfter + cost
		}

		inputTokens := rapid.Int64Range(1, 10000).Draw(rt, "inputTokens")
		outputTokens := rapid.Int64Range(1, 10000).Draw(rt, "outputTokens")
		modelName := rapid.SampledFrom([]string{"gpt-4o", "claude-3-opus", "gemini-pro", "codex-mini"}).Draw(rt, "model")
		provider := rapid.SampledFrom([]string{"openai", "anthropic", "google", "codex"}).Draw(rt, "provider")
		requestID := fmt.Sprintf("req-prop24-%d", iter)

		// Fresh DB for each iteration with WAL mode to avoid deadlocks.
		dsn := fmt.Sprintf("file:prop24_%d?mode=memory&cache=shared&_journal_mode=WAL", iter)
		db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Silent),
		})
		if err != nil {
			rt.Fatalf("open sqlite: %v", err)
		}
		sqlDB, _ := db.DB()
		sqlDB.SetMaxOpenConns(2)
		if err := db.AutoMigrate(&model.User{}, &model.UsageLog{}, &model.BalanceLog{}, &model.Subscription{}); err != nil {
			rt.Fatalf("automigrate: %v", err)
		}

		// Create user with balance = balanceBefore (above threshold).
		user := &model.User{
			Email:        fmt.Sprintf("prop24-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      balanceBefore,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		// Create a real Ledger backed by miniredis.
		redisClient, _ := testutil.MustMiniRedis(t)
		ldg := ledger.NewWithConfig(db, redisClient, 30*time.Second, 5*time.Minute)

		// Pre-hold with an amount the user can afford (must be <= balanceBefore).
		// The hold amount just needs to exist so Settle can remove it.
		holdAmount := cost
		if holdAmount > balanceBefore*0.9 {
			holdAmount = balanceBefore * 0.9
		}
		if err := ldg.Hold(context.Background(), user.ID, holdAmount, requestID, 5*time.Minute); err != nil {
			rt.Fatalf("pre-hold: %v (holdAmount=%v, balanceBefore=%v)", err, holdAmount, balanceBefore)
		}

		// Configure UsagePlugin with the real ledger and the threshold.
		calc := &propFakeCalc{cost: cost}
		plugin := NewUsagePlugin(db, ldg, calc)
		plugin.SetLowBalanceThreshold(threshold)

		sc := &SettleCtx{
			RequestID: requestID,
			UserID:    user.ID,
			RateMult:  1.0,
			Model:     modelName,
			ApiKeyID:  uint(rapid.UintRange(1, 100).Draw(rt, "apiKeyID")),
			IPAddress: "10.0.0.1",
		}
		ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

		rec := cliproxyusage.Record{
			Provider: provider,
			Model:    modelName,
			Failed:   false,
			Latency:  100 * time.Millisecond,
			Detail: cliproxyusage.Detail{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
			},
		}

		plugin.HandleUsage(ctx, rec)

		// PROPERTY ASSERTION: A BalanceLog with Type="low_balance_warning" SHALL exist.
		var warningLog model.BalanceLog
		err = db.Where("reference = ? AND type = ?", requestID, "low_balance_warning").First(&warningLog).Error
		if err != nil {
			rt.Fatalf("Property 24 violated: no BalanceLog with Type='low_balance_warning' found for request=%s "+
				"(balanceBefore=%v, cost=%v, balanceAfter=%v, threshold=%v): %v",
				requestID, balanceBefore, cost, balanceAfter, threshold, err)
		}

		// PROPERTY ASSERTION: UserID matches.
		if warningLog.UserID != user.ID {
			rt.Fatalf("Property 24 violated: BalanceLog.UserID = %d, want %d",
				warningLog.UserID, user.ID)
		}

		// PROPERTY ASSERTION: Reference == requestID.
		if warningLog.Reference != requestID {
			rt.Fatalf("Property 24 violated: BalanceLog.Reference = %q, want %q",
				warningLog.Reference, requestID)
		}

		// PROPERTY ASSERTION: Metadata contains user_id and current_balance.
		if len(warningLog.Metadata) == 0 {
			rt.Fatalf("Property 24 violated: BalanceLog.Metadata is empty for low_balance_warning (request=%s)",
				requestID)
		}
		var meta map[string]interface{}
		if err := json.Unmarshal(warningLog.Metadata, &meta); err != nil {
			rt.Fatalf("Property 24 violated: BalanceLog.Metadata is not valid JSON: %v", err)
		}
		if _, ok := meta["user_id"]; !ok {
			rt.Fatalf("Property 24 violated: Metadata missing 'user_id' (request=%s)", requestID)
		}
		if _, ok := meta["current_balance"]; !ok {
			rt.Fatalf("Property 24 violated: Metadata missing 'current_balance' (request=%s)", requestID)
		}
		if _, ok := meta["timestamp"]; !ok {
			rt.Fatalf("Property 24 violated: Metadata missing 'timestamp' (request=%s)", requestID)
		}
	})
}

// TestProperty24_LowBalanceWarningEvent_NoWarningAboveThreshold verifies the
// negative case: when a Settle does NOT cause the balance to drop below the
// threshold, no low_balance_warning BalanceLog record is created.
//
// **Validates: Requirements 14.1**
func TestProperty24_LowBalanceWarningEvent_NoWarningAboveThreshold(t *testing.T) {
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++

		// Generate a threshold.
		threshold := rapid.Float64Range(1.0, 10.0).Draw(rt, "threshold")

		// User balance stays well above threshold after Settle.
		// Use a margin to avoid floating-point boundary issues.
		balanceAfter := rapid.Float64Range(threshold+0.5, threshold+100.0).Draw(rt, "balanceAfter")
		cost := rapid.Float64Range(0.01, 50.0).Draw(rt, "cost")
		balanceBefore := balanceAfter + cost

		inputTokens := rapid.Int64Range(1, 5000).Draw(rt, "inputTokens")
		outputTokens := rapid.Int64Range(1, 5000).Draw(rt, "outputTokens")
		requestID := fmt.Sprintf("req-prop24-no-%d", iter)

		// Fresh DB for each iteration.
		dsn := fmt.Sprintf("file:prop24_no_%d?mode=memory&cache=shared&_journal_mode=WAL", iter)
		db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Silent),
		})
		if err != nil {
			rt.Fatalf("open sqlite: %v", err)
		}
		sqlDB, _ := db.DB()
		sqlDB.SetMaxOpenConns(2)
		if err := db.AutoMigrate(&model.User{}, &model.UsageLog{}, &model.BalanceLog{}, &model.Subscription{}); err != nil {
			rt.Fatalf("automigrate: %v", err)
		}

		// Create user with balance above threshold even after cost deduction.
		user := &model.User{
			Email:        fmt.Sprintf("prop24no-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      balanceBefore,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		// Create a real Ledger backed by miniredis.
		redisClient, _ := testutil.MustMiniRedis(t)
		ldg := ledger.NewWithConfig(db, redisClient, 30*time.Second, 5*time.Minute)

		// Pre-hold with an amount the user can afford.
		holdAmount := cost
		if holdAmount > balanceBefore*0.9 {
			holdAmount = balanceBefore * 0.9
		}
		if err := ldg.Hold(context.Background(), user.ID, holdAmount, requestID, 5*time.Minute); err != nil {
			rt.Fatalf("pre-hold: %v (holdAmount=%v, balanceBefore=%v)", err, holdAmount, balanceBefore)
		}

		// Configure UsagePlugin with the real ledger and the threshold.
		calc := &propFakeCalc{cost: cost}
		plugin := NewUsagePlugin(db, ldg, calc)
		plugin.SetLowBalanceThreshold(threshold)

		sc := &SettleCtx{
			RequestID: requestID,
			UserID:    user.ID,
			RateMult:  1.0,
			Model:     "gpt-4o",
			ApiKeyID:  1,
			IPAddress: "10.0.0.1",
		}
		ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

		rec := cliproxyusage.Record{
			Provider: "openai",
			Model:    "gpt-4o",
			Failed:   false,
			Latency:  100 * time.Millisecond,
			Detail: cliproxyusage.Detail{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
			},
		}

		plugin.HandleUsage(ctx, rec)

		// PROPERTY ASSERTION: No BalanceLog with Type="low_balance_warning" SHALL exist.
		var count int64
		db.Model(&model.BalanceLog{}).Where("reference = ? AND type = ?", requestID, "low_balance_warning").Count(&count)
		if count != 0 {
			rt.Fatalf("Property 24 violated (negative case): found %d BalanceLog records with Type='low_balance_warning' "+
				"when balance stayed above threshold (balanceBefore=%v, cost=%v, balanceAfter=%v, threshold=%v)",
				count, balanceBefore, cost, balanceAfter, threshold)
		}
	})
}

// =============================================================================
// Feature: billing-system-optimization, Property 26: Balance Depletion Event
// =============================================================================

// TestProperty26_BalanceDepletionEvent verifies that for any Settle operation
// that causes the user's balance to transition from positive to zero (or
// negative), a BalanceLog record with Type="balance_depleted" SHALL be created.
//
// The property generates random scenarios where the user's initial balance is
// positive and the settle cost is >= the balance, ensuring the balance drops
// to zero or below. It then asserts that a "balance_depleted" BalanceLog entry
// exists with the correct UserID, Reference (requestID), and Metadata.
//
// **Validates: Requirements 14.4**
func TestProperty26_BalanceDepletionEvent(t *testing.T) {
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		// Generate a positive initial balance. The cost will equal the balance
		// so that after Settle the balance drops to exactly zero (depletion).
		// Ledger.Settle requires user.Balance >= actualAmount to succeed,
		// so cost == userBalance is the exact boundary that triggers depletion.
		userBalance := rapid.Float64Range(0.01, 100.0).Draw(rt, "userBalance")
		cost := userBalance // balance goes to exactly zero after settle
		inputTokens := rapid.Int64Range(1, 10000).Draw(rt, "inputTokens")
		outputTokens := rapid.Int64Range(1, 10000).Draw(rt, "outputTokens")
		rateMult := rapid.Float64Range(0.5, 3.0).Draw(rt, "rateMult")
		modelName := rapid.SampledFrom([]string{"gpt-4o", "claude-3-opus", "gemini-pro", "codex-mini"}).Draw(rt, "model")
		provider := rapid.SampledFrom([]string{"openai", "anthropic", "google", "codex"}).Draw(rt, "provider")
		requestID := fmt.Sprintf("req-prop26-%d", iter)

		// Fresh DB for each iteration to avoid cross-contamination.
		// Use WAL mode and allow multiple connections so nested transactions
		// (UsagePlugin tx → Ledger.Settle tx) don't deadlock on SQLite.
		dsn := fmt.Sprintf("file:prop26_%d?mode=memory&cache=shared&_journal_mode=WAL", iter)
		db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Silent),
		})
		if err != nil {
			rt.Fatalf("open sqlite: %v", err)
		}
		sqlDB, _ := db.DB()
		sqlDB.SetMaxOpenConns(2)
		if err := db.AutoMigrate(&model.User{}, &model.UsageLog{}, &model.BalanceLog{}, &model.Subscription{}); err != nil {
			rt.Fatalf("automigrate: %v", err)
		}

		// Create user with positive balance that will be depleted by the settle cost.
		user := &model.User{
			Email:        fmt.Sprintf("prop26-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      userBalance,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		// Create a real Ledger backed by miniredis.
		redisClient, _ := testutil.MustMiniRedis(t)
		ldg := ledger.NewWithConfig(db, redisClient, 30*time.Second, 5*time.Minute)

		// Pre-hold with the user's full balance (hold amount <= available balance).
		// The hold amount just needs to be <= userBalance for the Hold to succeed.
		holdAmount := userBalance
		if err := ldg.Hold(context.Background(), user.ID, holdAmount, requestID, 5*time.Minute); err != nil {
			rt.Fatalf("pre-hold: %v", err)
		}

		// Configure UsagePlugin with the real ledger.
		calc := &propFakeCalc{cost: cost}
		plugin := NewUsagePlugin(db, ldg, calc)

		sc := &SettleCtx{
			RequestID: requestID,
			UserID:    user.ID,
			RateMult:  rateMult,
			Model:     modelName,
			ApiKeyID:  42,
			IPAddress: "10.0.0.1",
		}
		ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

		rec := cliproxyusage.Record{
			Provider: provider,
			Model:    modelName,
			Failed:   false, // Successful upstream → triggers Settle path
			Latency:  100 * time.Millisecond,
			Detail: cliproxyusage.Detail{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
			},
		}

		plugin.HandleUsage(ctx, rec)

		// PROPERTY ASSERTION: A BalanceLog with Type="balance_depleted" SHALL exist.
		var depletedLog model.BalanceLog
		err = db.Where("reference = ? AND type = ?", requestID, "balance_depleted").First(&depletedLog).Error
		if err != nil {
			rt.Fatalf("Property 26 violated: no BalanceLog with Type='balance_depleted' found for request=%s (userBalance=%v, cost=%v): %v",
				requestID, userBalance, cost, err)
		}

		// PROPERTY ASSERTION: UserID matches.
		if depletedLog.UserID != user.ID {
			rt.Fatalf("Property 26 violated: BalanceLog.UserID = %d, want %d",
				depletedLog.UserID, user.ID)
		}

		// PROPERTY ASSERTION: Reference == requestID.
		if depletedLog.Reference != requestID {
			rt.Fatalf("Property 26 violated: BalanceLog.Reference = %q, want %q",
				depletedLog.Reference, requestID)
		}

		// PROPERTY ASSERTION: Metadata JSON contains user_id and current_balance.
		if len(depletedLog.Metadata) == 0 {
			rt.Fatalf("Property 26 violated: BalanceLog.Metadata is empty for balance_depleted (request=%s)", requestID)
		}
		var meta map[string]interface{}
		if err := json.Unmarshal(depletedLog.Metadata, &meta); err != nil {
			rt.Fatalf("Property 26 violated: BalanceLog.Metadata is not valid JSON: %v", err)
		}
		if _, ok := meta["user_id"]; !ok {
			rt.Fatalf("Property 26 violated: Metadata missing 'user_id' (request=%s, meta=%s)",
				requestID, string(depletedLog.Metadata))
		}
		if _, ok := meta["timestamp"]; !ok {
			rt.Fatalf("Property 26 violated: Metadata missing 'timestamp' (request=%s, meta=%s)",
				requestID, string(depletedLog.Metadata))
		}
		if _, ok := meta["current_balance"]; !ok {
			rt.Fatalf("Property 26 violated: Metadata missing 'current_balance' (request=%s, meta=%s)",
				requestID, string(depletedLog.Metadata))
		}
		// Verify current_balance is <= 0 (balance depleted).
		if cb, ok := meta["current_balance"].(float64); ok {
			if cb > 0 {
				rt.Fatalf("Property 26 violated: Metadata current_balance = %v, want <= 0 (request=%s)",
					cb, requestID)
			}
		}
	})
}

// TestProperty26_BalanceDepletionEvent_NotFiredWhenBalanceStaysPositive verifies
// the negative case: when a Settle operation does NOT cause the balance to drop
// to zero (balance remains positive after settle), no "balance_depleted" event
// SHALL be created.
//
// **Validates: Requirements 14.4**
func TestProperty26_BalanceDepletionEvent_NotFiredWhenBalanceStaysPositive(t *testing.T) {
	var iter int

	rapid.Check(t, func(rt *rapid.T) {
		iter++
		// Generate a balance that is significantly larger than the cost.
		cost := rapid.Float64Range(0.01, 50.0).Draw(rt, "cost")
		userBalance := rapid.Float64Range(cost+1.0, cost+500.0).Draw(rt, "userBalance")
		inputTokens := rapid.Int64Range(1, 10000).Draw(rt, "inputTokens")
		outputTokens := rapid.Int64Range(1, 10000).Draw(rt, "outputTokens")
		modelName := rapid.SampledFrom([]string{"gpt-4o", "claude-3-opus", "gemini-pro"}).Draw(rt, "model")
		provider := rapid.SampledFrom([]string{"openai", "anthropic", "google"}).Draw(rt, "provider")
		requestID := fmt.Sprintf("req-prop26-nofire-%d", iter)

		// Fresh DB for each iteration.
		dsn := fmt.Sprintf("file:prop26_nofire_%d?mode=memory&cache=shared&_journal_mode=WAL", iter)
		db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Silent),
		})
		if err != nil {
			rt.Fatalf("open sqlite: %v", err)
		}
		sqlDB, _ := db.DB()
		sqlDB.SetMaxOpenConns(2)
		if err := db.AutoMigrate(&model.User{}, &model.UsageLog{}, &model.BalanceLog{}, &model.Subscription{}); err != nil {
			rt.Fatalf("automigrate: %v", err)
		}

		// Create user with balance that will remain positive after settle.
		user := &model.User{
			Email:        fmt.Sprintf("prop26nofire-%d@test.com", iter),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      userBalance,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user: %v", err)
		}

		// Create a real Ledger backed by miniredis.
		redisClient, _ := testutil.MustMiniRedis(t)
		ldg := ledger.NewWithConfig(db, redisClient, 30*time.Second, 5*time.Minute)

		// Pre-hold with cost amount (must be <= userBalance, which is guaranteed
		// since userBalance > cost + 1.0).
		holdAmount := cost
		if err := ldg.Hold(context.Background(), user.ID, holdAmount, requestID, 5*time.Minute); err != nil {
			rt.Fatalf("pre-hold: %v", err)
		}

		// Configure UsagePlugin with the real ledger.
		calc := &propFakeCalc{cost: cost}
		plugin := NewUsagePlugin(db, ldg, calc)

		sc := &SettleCtx{
			RequestID: requestID,
			UserID:    user.ID,
			RateMult:  1.0,
			Model:     modelName,
			ApiKeyID:  10,
			IPAddress: "192.168.1.1",
		}
		ctx := executor.WithUsageDetailPresent(WithSettleCtx(context.Background(), sc), true)

		rec := cliproxyusage.Record{
			Provider: provider,
			Model:    modelName,
			Failed:   false,
			Latency:  80 * time.Millisecond,
			Detail: cliproxyusage.Detail{
				InputTokens:  inputTokens,
				OutputTokens: outputTokens,
			},
		}

		plugin.HandleUsage(ctx, rec)

		// PROPERTY ASSERTION: No BalanceLog with Type="balance_depleted" SHALL exist.
		var count int64
		db.Model(&model.BalanceLog{}).Where("reference = ? AND type = ?", requestID, "balance_depleted").Count(&count)
		if count != 0 {
			rt.Fatalf("Property 26 violated (negative case): found %d BalanceLog with Type='balance_depleted' when balance stays positive (userBalance=%v, cost=%v, remaining=%v)",
				count, userBalance, cost, userBalance-cost)
		}
	})
}
