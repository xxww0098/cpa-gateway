package sdk

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"sync/atomic"
	"time"

	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/xxww0098/cpa-gateway/executor"
	"github.com/xxww0098/cpa-gateway/model"
	"github.com/xxww0098/cpa-gateway/pricing"
	"gorm.io/gorm"
)

// UsagePlugin implements cliproxy's usage.Plugin and is the terminal hop
// of the Hold → Settle/Release billing pipeline.
//
// Flow (successful response, precise path — Usage_Detail_Present = true):
//
//	HoldMiddleware  →  inject SettleCtx into ctx + ledger.Hold
//	executor        →  usage.PublishRecord(ctx, rec) with
//	                   executor.WithUsageDetailPresent(ctx, true)
//	UsagePlugin     →  read SettleCtx from ctx
//	                   calc.Compute → actualCost
//	                   PG Transaction:
//	                     ledger.Settle(userID, requestID, actualCost)
//	                     query BalanceLog shortfall rows for requestID
//	                     INSERT UsageLog row (with optional shortfall_usd)
//	                     if subscription active: accumulate usage counters
//	                   BudgetToken.DeductSettle (local token)
//	                   Check low-balance / balance-depleted events
//
// Flow (successful response, fallback path — Usage_Detail_Present = false,
// Strict_Usage_Metadata_Mode = false):
//
//	UsagePlugin     →  actualCost = max(ActiveHoldAmount, Estimate(model,true,rateMult))
//	                   ledger.Settle(userID, requestID, actualCost)
//	                   INSERT UsageLog row with
//	                     RawMetadata.billing_fallback.reason = "missing_upstream_usage"
//
// Flow (successful response, strict path — Usage_Detail_Present = false,
// Strict_Usage_Metadata_Mode = true):
//
//	UsagePlugin     →  DO NOT Settle, DO NOT Release (Hold expires naturally)
//	                   INSERT UsageLog row with Failed=true, ActualCost=0,
//	                     RawMetadata.reason = "missing_upstream_usage_strict"
//
// Flow (failed upstream):
//
//	UsagePlugin     →  ledger.Release(userID, requestID)
//	                   INSERT UsageLog row with Failed=true (no subscription accumulation)
//
// The plugin must never panic — the SDK runtime dispatches records from a
// shared background goroutine, and a panic there would tear down all
// subsequent deliveries. All errors are logged and swallowed.
type UsagePlugin struct {
	db                  *gorm.DB
	ledger              BillingLedger
	calc                PricingCalculator
	budgetTokenStore    *BudgetTokenStore
	lowBalanceThreshold float64
	strictUsageMetadata atomic.Bool
}

// NewUsagePlugin wires the plugin with its three required dependencies.
// All three are required; calling HandleUsage on a plugin constructed with
// any nil field is a programmer error (the plugin will log and return early).
func NewUsagePlugin(db *gorm.DB, ledger BillingLedger, calc PricingCalculator) *UsagePlugin {
	return &UsagePlugin{db: db, ledger: ledger, calc: calc}
}

// SetBudgetTokenStore attaches a BudgetTokenStore to the plugin so that
// successful Settle operations deduct from the local budget token.
func (p *UsagePlugin) SetBudgetTokenStore(store *BudgetTokenStore) {
	if p != nil {
		p.budgetTokenStore = store
	}
}

// SetLowBalanceThreshold sets the threshold below which a
// low_balance_warning event is recorded in BalanceLog after Settle.
func (p *UsagePlugin) SetLowBalanceThreshold(threshold float64) {
	if p != nil {
		p.lowBalanceThreshold = threshold
	}
}

// SetStrictUsageMetadataMode controls whether the plugin requires
// structurally valid usage metadata from upstream executors. When strict
// mode is enabled, records lacking required fields are treated as failed
// (no Settle, no Release — Hold expires naturally; UsageLog Failed=true)
// rather than settled via the fallback path. The field is stored
// atomically so it can be toggled safely at runtime.
func (p *UsagePlugin) SetStrictUsageMetadataMode(strict bool) {
	if p == nil {
		return
	}
	p.strictUsageMetadata.Store(strict)
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
		// Failed upstream: Release the hold and write UsageLog with Failed=true.
		// No subscription accumulation.
		if err := p.ledger.Release(bgCtx, sc.UserID, sc.RequestID); err != nil {
			log.Printf("sdk.UsagePlugin: ledger.Release failed user=%d request=%s: %v",
				sc.UserID, sc.RequestID, err)
		}
		p.writeUsageLog(bgCtx, sc, rec, tokens, actualCost, true, "upstream request failed")
		return
	}

	// Successful upstream: branch on whether the executor parsed an
	// explicit terminal usage envelope (see Requirements 1.1, 1.3, 1.4,
	// 1.5, 1.7). A missing ctx marker is treated as "not present" per
	// the contract of executor.UsageDetailPresentFromContext.
	present, _ := executor.UsageDetailPresentFromContext(ctx)

	// Strict branch: the operator has opted into a default-deny posture.
	// We do NOT Settle, do NOT Release — the Redis Hold expires via its
	// natural TTL. A Failed=true UsageLog row captures the event for
	// out-of-band reconciliation. See Requirement 1.4 / 1.7.
	if !present && p.strictUsageMetadata.Load() {
		p.writeStrictMissingUsageLog(bgCtx, sc, rec, tokens)
		return
	}

	// Fallback branch: compute a conservative actualCost so the tenant
	// is billed at least the hold (no free upstream output). The final
	// UsageLog will carry RawMetadata.billing_fallback.reason so ops
	// can alert on volume. See Requirement 1.3 / 1.5.
	var fallbackReason string
	if !present {
		holdAmt, _, err := p.ledger.ActiveHoldAmount(bgCtx, sc.UserID, sc.RequestID)
		if err != nil {
			// ActiveHoldAmount error: we cannot compute a safe
			// lower-bound. Do NOT call Settle (that would zero-cost
			// the request). Write a Failed=true UsageLog flagged
			// with event=active_hold_lookup_failed so ops can
			// investigate. The Hold is left to TTL like strict mode.
			log.Printf("sdk.UsagePlugin: active_hold_lookup_failed user=%d request=%s: %v",
				sc.UserID, sc.RequestID, err)
			p.writeActiveHoldLookupFailedLog(bgCtx, sc, rec, tokens, err)
			return
		}
		est := p.calc.Estimate(rec.Model, true, sc.RateMult)
		actualCost = math.Max(holdAmt, est)
		fallbackReason = "missing_upstream_usage"
	}

	// Settle path (fallback or precise). Settle now always succeeds
	// (post-task-1.3) for logical outcomes; any returned error is an
	// infrastructure failure. On error we write a Failed=true UsageLog
	// and do NOT accumulate subscription counters.
	var balanceBefore, balanceAfter float64
	var shortfall float64
	settleErr := p.db.WithContext(bgCtx).Transaction(func(tx *gorm.DB) error {
		if err := p.ledger.Settle(bgCtx, sc.UserID, sc.RequestID, actualCost); err != nil {
			return err
		}

		// Read balance after settle for low-balance / depletion checks.
		var user model.User
		if err := tx.Select("balance").First(&user, sc.UserID).Error; err == nil {
			balanceAfter = user.Balance
			balanceBefore = user.Balance + actualCost // approximate pre-settle balance
		}

		// Inspect BalanceLog rows keyed by this request for any
		// shortfall marker written by ledger.Settle's partial-debit
		// path. The shortfall_usd is surfaced through UsageLog so
		// downstream reporting distinguishes "free request" from
		// "partially paid request" (Requirement 2.4).
		shortfall = lookupShortfall(tx, sc.RequestID)

		entry := p.buildUsageLogEntry(sc, rec, tokens, actualCost, false, "")
		annotateSettleMetadata(entry, fallbackReason, shortfall)
		if err := tx.Create(entry).Error; err != nil {
			return err
		}

		// Accumulate subscription counters within the same transaction.
		if sc.SubscriptionID != nil && *sc.SubscriptionID != 0 && actualCost > 0 {
			result := tx.
				Model(&model.Subscription{}).
				Where("id = ? AND status = ? AND expires_at > ?", *sc.SubscriptionID, "active", time.Now().UTC()).
				Updates(map[string]interface{}{
					"daily_usage_usd":   gorm.Expr("daily_usage_usd + ?", actualCost),
					"weekly_usage_usd":  gorm.Expr("weekly_usage_usd + ?", actualCost),
					"monthly_usage_usd": gorm.Expr("monthly_usage_usd + ?", actualCost),
				})
			if result.Error != nil {
				return result.Error
			}
		}

		return nil
	})

	if settleErr != nil {
		// Settle failed: write UsageLog with Failed=true OUTSIDE the failed
		// transaction. Do NOT accumulate subscription.
		log.Printf("sdk.UsagePlugin: settle transaction failed user=%d request=%s cost=%v: %v",
			sc.UserID, sc.RequestID, actualCost, settleErr)
		p.writeUsageLog(bgCtx, sc, rec, tokens, actualCost, true, settleErr.Error())
		return
	}

	// Post-commit: BudgetToken deduction (process-local, non-transactional).
	if p.budgetTokenStore != nil {
		p.budgetTokenStore.DeductSettle(sc.UserID, actualCost)
	}

	// Post-commit: check low-balance and balance-depleted events.
	p.checkBalanceEvents(bgCtx, sc, balanceBefore, balanceAfter)
}

// buildUsageLogEntry constructs a UsageLog model populated from SettleCtx
// and the usage record. Fields ApiKeyID, GroupID, IPAddress, and
// IdempotencyKey are sourced from SettleCtx.
func (p *UsagePlugin) buildUsageLogEntry(sc *SettleCtx, rec cliproxyusage.Record, tokens pricing.UsageTokens, cost float64, failed bool, failReason string) *model.UsageLog {
	entry := &model.UsageLog{
		UserID:          sc.UserID,
		ApiKeyID:        sc.ApiKeyID,
		GroupID:         sc.GroupID,
		RequestID:       sc.RequestID,
		IdempotencyKey:  sc.IdempotencyKey,
		IPAddress:       sc.IPAddress,
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
		Failed:          failed,
	}

	if failed && failReason != "" {
		metadata := map[string]interface{}{
			"reason":    failReason,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		}
		if data, err := json.Marshal(metadata); err == nil {
			entry.RawMetadata = data
		}
	}

	return entry
}

// writeUsageLog inserts a single UsageLog row outside of any transaction.
// Used for failure paths where the main transaction has already rolled back.
func (p *UsagePlugin) writeUsageLog(ctx context.Context, sc *SettleCtx, rec cliproxyusage.Record, tokens pricing.UsageTokens, cost float64, failed bool, failReason string) {
	entry := p.buildUsageLogEntry(sc, rec, tokens, cost, failed, failReason)
	if err := p.db.WithContext(ctx).Create(entry).Error; err != nil {
		log.Printf("sdk.UsagePlugin: insert UsageLog failed user=%d request=%s: %v",
			sc.UserID, sc.RequestID, err)
	}
}

// writeStrictMissingUsageLog writes a Failed=true UsageLog row marking the
// strict-mode "upstream usage missing" case. The Hold is intentionally left
// in place — it expires via its natural Redis TTL, so downstream
// reconciliation can match the UsageLog reason against the abandoned Hold.
// See Requirement 1.4 / 1.7.
func (p *UsagePlugin) writeStrictMissingUsageLog(ctx context.Context, sc *SettleCtx, rec cliproxyusage.Record, tokens pricing.UsageTokens) {
	entry := p.buildUsageLogEntry(sc, rec, tokens, 0, true, "")
	entry.ActualCost = 0
	entry.TotalCost = 0
	entry.Cost = 0
	metadata := map[string]interface{}{
		"reason":    "missing_upstream_usage_strict",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}
	if data, err := json.Marshal(metadata); err == nil {
		entry.RawMetadata = data
	}
	if err := p.db.WithContext(ctx).Create(entry).Error; err != nil {
		log.Printf("sdk.UsagePlugin: insert strict UsageLog failed user=%d request=%s: %v",
			sc.UserID, sc.RequestID, err)
	}
}

// writeActiveHoldLookupFailedLog writes a Failed=true UsageLog row when
// ActiveHoldAmount could not be resolved. We deliberately skip the Settle
// call because we cannot bound the cost safely without knowing the hold —
// a zero-cost Settle would violate Requirement 1.5.
func (p *UsagePlugin) writeActiveHoldLookupFailedLog(ctx context.Context, sc *SettleCtx, rec cliproxyusage.Record, tokens pricing.UsageTokens, cause error) {
	entry := p.buildUsageLogEntry(sc, rec, tokens, 0, true, "")
	entry.ActualCost = 0
	entry.TotalCost = 0
	entry.Cost = 0
	metadata := map[string]interface{}{
		"event":     "active_hold_lookup_failed",
		"reason":    cause.Error(),
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}
	if data, err := json.Marshal(metadata); err == nil {
		entry.RawMetadata = data
	}
	if err := p.db.WithContext(ctx).Create(entry).Error; err != nil {
		log.Printf("sdk.UsagePlugin: insert active_hold_lookup_failed UsageLog user=%d request=%s: %v",
			sc.UserID, sc.RequestID, err)
	}
}

// lookupShortfall sums BalanceLog{type=settle}.Metadata.shortfall_usd for
// the given requestID. Returns 0 when no shortfall row exists (the normal
// case) or when metadata cannot be parsed. Called inside the Settle
// transaction so it observes the rows just written by ledger.Settle.
func lookupShortfall(tx *gorm.DB, requestID string) float64 {
	var logs []model.BalanceLog
	if err := tx.
		Where("reference = ? AND type = ?", requestID, "settle").
		Find(&logs).Error; err != nil {
		return 0
	}
	var total float64
	for _, bl := range logs {
		if len(bl.Metadata) == 0 {
			continue
		}
		var meta map[string]interface{}
		if err := json.Unmarshal(bl.Metadata, &meta); err != nil {
			continue
		}
		if v, ok := meta["shortfall_usd"].(float64); ok && v > 0 {
			total += v
		}
	}
	return total
}

// annotateSettleMetadata attaches billing_fallback.reason and shortfall_usd
// to the UsageLog entry's RawMetadata when applicable. A no-op when neither
// annotation applies, so precise/no-shortfall entries keep RawMetadata nil.
func annotateSettleMetadata(entry *model.UsageLog, fallbackReason string, shortfall float64) {
	if fallbackReason == "" && shortfall <= 0 {
		return
	}
	annotations := map[string]interface{}{}
	if fallbackReason != "" {
		annotations["billing_fallback"] = map[string]interface{}{
			"reason": fallbackReason,
		}
	}
	if shortfall > 0 {
		annotations["shortfall_usd"] = shortfall
	}
	if data, err := json.Marshal(annotations); err == nil {
		entry.RawMetadata = data
	}
}

// checkBalanceEvents records low_balance_warning and balance_depleted events
// in BalanceLog when the user's balance crosses relevant thresholds.
func (p *UsagePlugin) checkBalanceEvents(ctx context.Context, sc *SettleCtx, balanceBefore, balanceAfter float64) {
	threshold := p.lowBalanceThreshold
	if threshold <= 0 {
		threshold = 1.0 // default $1
	}

	// Low balance warning: balance dropped below threshold.
	if balanceBefore >= threshold && balanceAfter < threshold && balanceAfter > 0 {
		p.writeBalanceEvent(ctx, sc, "low_balance_warning", balanceAfter)
	}

	// Balance depleted: balance went from positive to zero (or negative).
	if balanceBefore > 0 && balanceAfter <= 0 {
		p.writeBalanceEvent(ctx, sc, "balance_depleted", balanceAfter)
	}
}

// writeBalanceEvent writes a BalanceLog record for balance threshold events.
func (p *UsagePlugin) writeBalanceEvent(ctx context.Context, sc *SettleCtx, eventType string, currentBalance float64) {
	metadata := map[string]interface{}{
		"user_id":         sc.UserID,
		"current_balance": currentBalance,
		"model":           sc.Model,
		"timestamp":       time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		log.Printf("sdk.UsagePlugin: marshal balance event metadata: %v", err)
		return
	}

	entry := &model.BalanceLog{
		UserID:    sc.UserID,
		Amount:    0,
		Type:      eventType,
		Reference: sc.RequestID,
		Metadata:  data,
	}
	if err := p.db.WithContext(ctx).Create(entry).Error; err != nil {
		log.Printf("sdk.UsagePlugin: write %s event failed user=%d: %v", eventType, sc.UserID, err)
	}
}
