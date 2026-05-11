package sdk

import (
	"context"
	"time"

	"github.com/xxww0098/cpa-gateway/pricing"
)

// BillingLedger is the minimal ledger surface the SDK-facing middleware
// and plugins depend on. It is a narrowed view of *ledger.Ledger that
// exists so tests can inject mocks without spinning up Redis.
//
// The method signatures are a 1:1 match of the concrete ledger methods —
// see ledger.Ledger.Hold / ledger.Ledger.Settle / ledger.Ledger.Release
// for the authoritative semantics. Keeping them identical means the
// concrete ledger satisfies this interface without adapter shims.
type BillingLedger interface {
	// Hold reserves amount against userID for requestID, valid for ttl.
	Hold(ctx context.Context, userID uint, amount float64, requestID string, ttl time.Duration) error

	// Settle clears the outstanding hold for requestID and debits the
	// actual amount from the persistent balance. amount <= 0 only clears
	// the hold and does not debit.
	Settle(ctx context.Context, userID uint, requestID string, actualAmount float64) error

	// Release clears the hold for requestID without touching the
	// persistent balance. Used when the upstream request failed.
	Release(ctx context.Context, userID uint, requestID string) error
}

// PricingCalculator is the minimal pricing surface consumed by the Hold
// middleware and UsagePlugin. It mirrors the public methods of
// *pricing.Calculator so the concrete calculator satisfies the interface
// directly, while tests can substitute a lightweight fake.
type PricingCalculator interface {
	// Estimate returns an over-approximate USD cost used by Hold.
	Estimate(model string, stream bool, rateMult float64) float64

	// Compute returns the exact USD cost using per-column token counts,
	// used by Settle.
	Compute(model string, tokens pricing.UsageTokens, rateMult float64) float64
}
