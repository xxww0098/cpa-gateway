package sdk

import (
	"context"
	"errors"
	"log"
	"time"

	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/xxww0098/cpa-gateway/model"
	"github.com/xxww0098/cpa-gateway/pricing"
	"gorm.io/gorm"
)

// UsagePlugin implements cliproxy's usage.Plugin and is the terminal hop
// of the Hold → Settle/Release billing pipeline.
//
// Flow (successful response):
//
//	HoldMiddleware  →  inject SettleCtx into ctx + ledger.Hold
//	executor        →  usage.PublishRecord(ctx, rec)
//	UsagePlugin     →  read SettleCtx from ctx
//	                   calc.Compute → actualCost
//	                   ledger.Settle(userID, requestID, actualCost)
//	                   INSERT UsageLog row
//	                   if subscription active: accumulate usage counters
//
// Flow (failed upstream):
//
//	UsagePlugin     →  ledger.Release(userID, requestID)
//	                   INSERT UsageLog row with Failed=true
//
// The plugin must never panic — the SDK runtime dispatches records from a
// shared background goroutine, and a panic there would tear down all
// subsequent deliveries. All errors are logged and swallowed.
type UsagePlugin struct {
	db     *gorm.DB
	ledger BillingLedger
	calc   PricingCalculator
}

// NewUsagePlugin wires the plugin with its three dependencies. All three
// are required; calling HandleUsage on a plugin constructed with any nil
// field is a programmer error (the plugin will log and return early).
func NewUsagePlugin(db *gorm.DB, ledger BillingLedger, calc PricingCalculator) *UsagePlugin {
	return &UsagePlugin{db: db, ledger: ledger, calc: calc}
}

// Compile-time assertion: UsagePlugin satisfies cliproxyusage.Plugin.
var _ cliproxyusage.Plugin = (*UsagePlugin)(nil)

// HandleUsage is invoked by the cliproxy usage manager for every
// published Record. See the type doc comment for the end-to-end flow.
func (p *UsagePlugin) HandleUsage(ctx context.Context, rec cliproxyusage.Record) {
	if p == nil {
		return
	}

	// No SettleCtx ⇒ the request bypassed HoldMiddleware (e.g., a non
	// /v1/ path or a probe). There is nothing to reconcile — return
	// without side effects so we do not accidentally write orphan
	// UsageLog rows.
	sc, ok := SettleCtxFromContext(ctx)
	if !ok || sc == nil {
		return
	}
	if p.ledger == nil || p.calc == nil || p.db == nil {
		log.Printf("sdk.UsagePlugin: missing dependency (ledger=%v calc=%v db=%v), skipping",
			p.ledger != nil, p.calc != nil, p.db != nil)
		return
	}

	tokens := pricing.UsageTokens{
		Input:     rec.Detail.InputTokens,
		Output:    rec.Detail.OutputTokens,
		Cached:    rec.Detail.CachedTokens,
		Reasoning: rec.Detail.ReasoningTokens,
	}
	actualCost := p.calc.Compute(rec.Model, tokens, sc.RateMult)

	// Detach the DB / ledger work from the request context because the
	// caller's ctx may already be cancelled (the client has hung up by
	// the time Settle runs). We still honor a bounded timeout so a stuck
	// DB does not wedge the dispatcher goroutine.
	bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if rec.Failed {
		if err := p.ledger.Release(bgCtx, sc.UserID, sc.RequestID); err != nil {
			log.Printf("sdk.UsagePlugin: ledger.Release failed user=%d request=%s: %v",
				sc.UserID, sc.RequestID, err)
		}
	} else {
		if err := p.ledger.Settle(bgCtx, sc.UserID, sc.RequestID, actualCost); err != nil {
			// Do not abort — we still want the UsageLog and subscription
			// counters to reflect what the upstream reported, even if
			// the settlement debit failed (e.g., ErrInsufficientBalance
			// after a race).
			log.Printf("sdk.UsagePlugin: ledger.Settle failed user=%d request=%s cost=%v: %v",
				sc.UserID, sc.RequestID, actualCost, err)
		}
	}

	p.writeUsageLog(bgCtx, sc, rec, tokens, actualCost)

	// Only successful responses accumulate quota — a failed request
	// was not actually billed so it must not consume subscription
	// allowance.
	if !rec.Failed && sc.SubscriptionID != nil && *sc.SubscriptionID != 0 && actualCost > 0 {
		p.accumulateSubscription(bgCtx, *sc.SubscriptionID, actualCost)
	}
}

// writeUsageLog inserts a single UsageLog row mirroring the record's key
// fields. Failures are logged but not returned — the plugin is best-
// effort: a missed log row must not block Settle or subscription
// accounting that has already happened.
func (p *UsagePlugin) writeUsageLog(ctx context.Context, sc *SettleCtx, rec cliproxyusage.Record, tokens pricing.UsageTokens, cost float64) {
	entry := &model.UsageLog{
		UserID:          sc.UserID,
		RequestID:       sc.RequestID,
		Model:           rec.Model,
		Provider:        rec.Provider,
		AuthID:          rec.AuthID,
		InputTokens:     int(tokens.Input),
		OutputTokens:    int(tokens.Output),
		CachedTokens:    int(tokens.Cached),
		ReasoningTokens: int(tokens.Reasoning),
		TokensIn:        int(tokens.Input),
		TokensOut:       int(tokens.Output),
		TotalCost:       cost,
		ActualCost:      cost,
		Cost:            cost,
		RateMultiplier:  sc.RateMult,
		Stream:          sc.Stream,
		DurationMs:      rec.Latency.Milliseconds(),
		Failed:          rec.Failed,
	}

	if err := p.db.WithContext(ctx).Create(entry).Error; err != nil {
		log.Printf("sdk.UsagePlugin: insert UsageLog failed user=%d request=%s: %v",
			sc.UserID, sc.RequestID, err)
	}
}

// accumulateSubscription increments the day/week/month usage counters on
// the active subscription by cost. The row is updated only if it still
// matches status=active and has not expired, which guards against racing
// with admin cancel/expire flows.
func (p *UsagePlugin) accumulateSubscription(ctx context.Context, subscriptionID uint, cost float64) {
	var sub model.Subscription
	err := p.db.WithContext(ctx).
		Where("id = ? AND status = ? AND expires_at > ?", subscriptionID, "active", time.Now().UTC()).
		First(&sub).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Printf("sdk.UsagePlugin: lookup subscription %d failed: %v", subscriptionID, err)
		}
		return
	}

	sub.DailyUsageUSD += cost
	sub.WeeklyUsageUSD += cost
	sub.MonthlyUsageUSD += cost

	if err := p.db.WithContext(ctx).Save(&sub).Error; err != nil {
		log.Printf("sdk.UsagePlugin: update subscription %d counters failed: %v", subscriptionID, err)
	}
}
