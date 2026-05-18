package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/xxww0098/cpa-gateway/model"
	"github.com/xxww0098/cpa-gateway/pricing"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"pgregory.net/rapid"
)

// Feature: billing-security-hardening, Property 1 — Fallback Settle lower-bound.
//
// TestFallbackSettleLowerBound is the property-based regression for
// Requirements 1.3 and 1.5: when the executor does NOT publish a
// Usage_Detail_Present marker (i.e. upstream usage metadata was stripped
// or absent) AND strict mode is disabled, UsagePlugin.HandleUsage MUST
//
//  1. invoke Billing_Ledger.Settle with actualAmount equal to
//     max(Active_Hold_Amount, Pricing_Calculator.Estimate(model, true, rateMult)),
//     so the tenant is never billed below the conservative lower bound;
//     and
//  2. write exactly one UsageLog row whose RawMetadata carries
//     billing_fallback.reason = "missing_upstream_usage", so downstream
//     reporting / alerting can distinguish fallback settlements from
//     precise settlements.
//
// Inputs are generated over rapid.Float64Range(0, 100) for the hold and
// estimate amounts, which straddles the shrink target called out in the
// task sheet (Active_Hold = 0.01, Estimate = 0.02). rateMult and model
// name are also varied so the assertion holds independent of pricing
// inputs. The two outer iterations around rapid.Check(…) bring the total
// property iteration count to ≥ 200 while preserving rapid's per-Check
// shrinker — a failure shrinks inside the invocation it fires in, which
// lands closer to the documented target than flattening the loop into a
// single 200-count rapid.Check would.
//
// The test uses fallbackLedgerMock and fallbackCalcMock (file-local fakes)
// so each iteration can stamp a unique Active_Hold_Amount and Estimate
// pair without cross-talk. Compute is pinned to 0 because the fallback
// branch overwrites actualCost BEFORE calling Settle — see
// sdk/usage.go's `case !present:` branch. Pinning Compute to 0 therefore
// also doubles as a negative assertion: if Settle ever receives 0, the
// fallback rewrite did not fire and the test fails on the lower-bound
// check (0 is only >= max(hold, est) when both are 0, which is a valid
// shrink-floor case covered by the >=0 ranges).
//
// **Validates: Property 1, Requirements 1.3, 1.5**
func TestFallbackSettleLowerBound(t *testing.T) {
	db := newFallbackTestDB(t)

	// Shared iteration counter so generated user emails (which are
	// uniquely indexed by the User model) do not collide across rapid's
	// internal retries.
	var iter int64
	var iterMu sync.Mutex
	nextIter := func() int64 {
		iterMu.Lock()
		defer iterMu.Unlock()
		iter++
		return iter
	}

	// 2× rapid.Check(…) ⇒ 200 total iterations (rapid's default is 100
	// per invocation). The outer loop also keeps each Check's shrinker
	// focused on a single counterexample class, which makes the shrink
	// target (hold=0.01, est=0.02) reachable.
	for outer := 0; outer < 2; outer++ {
		rapid.Check(t, func(rt *rapid.T) {
			i := nextIter()

			// Active_Hold_Amount and Estimate are both drawn from [0, 100].
			// 0 is deliberately included because the fallback path must
			// also cope with a stale / missing hold row without zero-cost
			// settling — Settle's argument stays >= 0 trivially.
			activeHold := rapid.Float64Range(0, 100).Draw(rt, "active_hold_amount")
			estimate := rapid.Float64Range(0, 100).Draw(rt, "estimate")
			rateMult := rapid.Float64Range(0.5, 5.0).Draw(rt, "rate_mult")
			modelName := rapid.SampledFrom([]string{
				"gpt-4o",
				"gpt-4o-mini",
				"claude-3-opus",
				"gemini-1.5-pro",
				"o1",
			}).Draw(rt, "model")

			requestID := fmt.Sprintf("req-fallback-lb-%d", i)

			// Seed a user; the fallback Settle path in UsagePlugin.HandleUsage
			// reads user.Balance post-settle for low-balance events, so the
			// row must exist even though our fake ledger never mutates it.
			user := &model.User{
				Email:        fmt.Sprintf("fallback-lb-%d@test.local", i),
				PasswordHash: "x",
				Role:         "user",
				Status:       "active",
				Balance:      1000.0,
			}
			if err := db.Create(user).Error; err != nil {
				rt.Fatalf("create user: %v", err)
			}

			led := &fallbackLedgerMock{
				activeHoldAmount: activeHold,
				activeHoldOK:     true,
			}
			calc := &fallbackCalcMock{estimate: estimate, computeCost: 0}
			plugin := NewUsagePlugin(db, led, calc)
			// Strict mode left at its zero value (false) — no SetStrictUsageMetadataMode.

			sc := &SettleCtx{
				RequestID: requestID,
				UserID:    user.ID,
				RateMult:  rateMult,
				Model:     modelName,
				ApiKeyID:  7,
				IPAddress: "10.0.0.1",
			}

			// Critical: the ctx carries SettleCtx but DOES NOT carry a
			// usage_detail_present marker. Per
			// executor.UsageDetailPresentFromContext, a missing key is
			// treated as (false, false) by HandleUsage, which is exactly
			// the fallback-branch precondition we want to exercise.
			ctx := WithSettleCtx(context.Background(), sc)

			rec := cliproxyusage.Record{
				Provider: "openai",
				Model:    modelName,
				Failed:   false,
				Latency:  50 * time.Millisecond,
				// Detail is intentionally left zero-valued: the
				// executor published a "success without usage" record,
				// which is the stripping-attack shape Requirement 1.3
				// protects against.
				Detail: cliproxyusage.Detail{},
			}

			plugin.HandleUsage(ctx, rec)

			// ---- Assertion 1: Settle was called exactly once with the
			// fallback lower-bound amount. --------------------------------
			calls := led.settleSnapshot()
			if len(calls) != 1 {
				rt.Fatalf("expected exactly 1 Settle call for request %s, got %d (calls=%+v)",
					requestID, len(calls), calls)
			}
			call := calls[0]
			if call.userID != user.ID {
				rt.Fatalf("Settle userID = %d, want %d", call.userID, user.ID)
			}
			if call.requestID != requestID {
				rt.Fatalf("Settle requestID = %q, want %q", call.requestID, requestID)
			}

			// Lower-bound: settleArg >= max(Active_Hold_Amount, Estimate).
			// We compare against the exact same math.Max the production
			// code uses so float ordering is bit-faithful (no epsilon
			// fudge required).
			want := math.Max(activeHold, estimate)
			if call.actualAmount < want {
				rt.Fatalf("Settle actualAmount = %.12f, want >= max(hold=%.12f, estimate=%.12f) = %.12f (request=%s, rateMult=%.6f, model=%s)",
					call.actualAmount, activeHold, estimate, want, requestID, rateMult, modelName)
			}

			// ---- Assertion 2: a single UsageLog row was written with
			// the billing_fallback.reason annotation. --------------------
			var logs []model.UsageLog
			if err := db.Where("request_id = ?", requestID).Find(&logs).Error; err != nil {
				rt.Fatalf("query UsageLog: %v", err)
			}
			if len(logs) != 1 {
				rt.Fatalf("expected exactly 1 UsageLog row for request %s, got %d", requestID, len(logs))
			}
			ulog := logs[0]
			if ulog.Failed {
				rt.Fatalf("UsageLog.Failed = true, want false for non-strict fallback settle (request=%s)", requestID)
			}
			if len(ulog.RawMetadata) == 0 {
				rt.Fatalf("UsageLog.RawMetadata empty, want billing_fallback annotation (request=%s)", requestID)
			}
			var meta map[string]interface{}
			if err := json.Unmarshal(ulog.RawMetadata, &meta); err != nil {
				rt.Fatalf("unmarshal UsageLog.RawMetadata: %v (raw=%s)", err, string(ulog.RawMetadata))
			}
			bf, ok := meta["billing_fallback"].(map[string]interface{})
			if !ok {
				rt.Fatalf("UsageLog.RawMetadata.billing_fallback missing or wrong type: %v (raw=%s)",
					meta["billing_fallback"], string(ulog.RawMetadata))
			}
			reason, ok := bf["reason"].(string)
			if !ok {
				rt.Fatalf("UsageLog.RawMetadata.billing_fallback.reason missing or not a string: %v (raw=%s)",
					bf["reason"], string(ulog.RawMetadata))
			}
			if reason != "missing_upstream_usage" {
				rt.Fatalf("UsageLog.RawMetadata.billing_fallback.reason = %q, want %q (request=%s)",
					reason, "missing_upstream_usage", requestID)
			}

			// ---- Assertion 3: Release was NOT called. The fallback path
			// is a Settle path, not a Release path; a Release here would
			// silently return hold funds without any billing record. ----
			if r := led.releaseCount(); r != 0 {
				rt.Fatalf("expected 0 Release calls for fallback settle, got %d (request=%s)", r, requestID)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// file-local mocks
// -----------------------------------------------------------------------------

// fallbackLedgerMock is a BillingLedger fake tailored for the fallback-
// settle property: it records every Settle invocation so the test can
// assert the argument lower-bound, and it returns a configurable
// (amount, true, nil) from ActiveHoldAmount to stand in for a live Redis
// hold. Hold, Release and HasUnresolvedShortfall are no-ops because the
// fallback branch under test does not invoke them.
type fallbackLedgerMock struct {
	mu sync.Mutex

	settleCalls  []settleCallFallback
	releaseCalls int

	activeHoldAmount float64
	activeHoldOK     bool
	activeHoldErr    error
}

type settleCallFallback struct {
	userID       uint
	requestID    string
	actualAmount float64
}

func (m *fallbackLedgerMock) Hold(_ context.Context, _ uint, _ float64, _ string, _ time.Duration) error {
	return nil
}

func (m *fallbackLedgerMock) Settle(_ context.Context, userID uint, requestID string, actualAmount float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settleCalls = append(m.settleCalls, settleCallFallback{
		userID:       userID,
		requestID:    requestID,
		actualAmount: actualAmount,
	})
	return nil
}

func (m *fallbackLedgerMock) Release(_ context.Context, _ uint, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseCalls++
	return nil
}

func (m *fallbackLedgerMock) ActiveHoldAmount(_ context.Context, _ uint, _ string) (float64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeHoldAmount, m.activeHoldOK, m.activeHoldErr
}

func (m *fallbackLedgerMock) HasUnresolvedShortfall(_ context.Context, _ uint) (bool, error) {
	return false, nil
}

func (m *fallbackLedgerMock) settleSnapshot() []settleCallFallback {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]settleCallFallback, len(m.settleCalls))
	copy(out, m.settleCalls)
	return out
}

func (m *fallbackLedgerMock) releaseCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.releaseCalls
}

// fallbackCalcMock returns a configurable Estimate and a Compute pinned
// to 0. The fallback branch rewrites actualCost = max(hold, estimate)
// BEFORE Settle runs, so Compute's return value must not be observable
// at the Settle call site — pinning it to 0 makes that observability
// contract explicit: any non-zero Settle argument must come from the
// max(hold, est) rewrite.
type fallbackCalcMock struct {
	estimate    float64
	computeCost float64
}

func (c *fallbackCalcMock) Estimate(_ string, _ bool, _ float64) float64 {
	return c.estimate
}

func (c *fallbackCalcMock) EstimateWithMaxTokens(_ string, _ int64, _ bool, _ float64) float64 {
	// Not exercised by HandleUsage (HoldMiddleware owns this path); the
	// stub exists to satisfy the PricingCalculator interface.
	return 0
}

func (c *fallbackCalcMock) Compute(_ string, _ pricing.UsageTokens, _ float64) float64 {
	return c.computeCost
}

// newFallbackTestDB builds a fresh in-memory SQLite database with the
// tables UsagePlugin touches on the fallback branch (User, UsageLog,
// BalanceLog, Subscription). A shared-cache DSN + a single max-open
// connection avoids SQLite's "database is locked" under the
// Settle-transaction path.
func newFallbackTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:fallback_settle_%s?mode=memory&cache=shared", t.Name())
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
