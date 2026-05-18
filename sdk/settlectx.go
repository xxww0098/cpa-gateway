package sdk

import "context"

// SettleCtx carries per-request billing state across the SDK pipeline.
//
// It is produced by HoldMiddleware after a successful balance hold and
// read back out by the UsagePlugin in HandleUsage to apply precise debits
// using the executor-reported token usage and the CPA ModelPrice table.
//
// This replaces an earlier design that relied on a request_id keyed
// global UsageCollector; see the "Metis Review" section of the plan for
// rationale. Passing the value through context.Context keeps the per-
// request state local to a single goroutine chain and avoids a shared
// mutable registry.
type SettleCtx struct {
	// RequestID uniquely identifies this request and matches the key
	// used by Ledger.Hold / Ledger.Settle / Ledger.Release.
	RequestID string

	// UserID is the authenticated tenant user.
	UserID uint

	// ApiKeyID is the ID of the API key used to authenticate this request.
	// Zero when the request is authenticated via JWT without an API key.
	ApiKeyID uint

	// GroupID is the group the authenticated API key belongs to.
	// Nil when the request has no associated group (e.g. JWT-only auth).
	GroupID *uint

	// RateMult is the group-configured rate multiplier (defaults to 1.0).
	RateMult float64

	// SubscriptionID points at the active subscription (if any) whose
	// quota counters should be incremented on Settle.
	SubscriptionID *uint

	// Model is the model identifier extracted from the request body,
	// used by the UsagePlugin when looking up ModelPrice rows.
	Model string

	// Stream is true when the client requested Server-Sent Events or
	// any other streaming transport. It is consumed by the pricing
	// Calculator to bias the Hold estimate upward.
	Stream bool

	// IPAddress is the client IP address extracted from the request context,
	// recorded in UsageLog for abuse investigation.
	IPAddress string

	// IdempotencyKey is the deduplication identifier from the client request
	// header (Idempotency-Key or X-Idempotency-Key), used to prevent
	// duplicate billing for retried requests.
	IdempotencyKey string
}

// settleCtxKey is an unexported context key type to prevent collisions
// with other packages using context.WithValue.
type settleCtxKey struct{}

// WithSettleCtx returns a derived context carrying sc. Callers that pass
// a nil sc get back the parent context unchanged.
func WithSettleCtx(ctx context.Context, sc *SettleCtx) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if sc == nil {
		return ctx
	}
	return context.WithValue(ctx, settleCtxKey{}, sc)
}

// SettleCtxFromContext extracts the SettleCtx previously stored with
// WithSettleCtx. The second return value reports whether a value was
// present and had the expected type.
func SettleCtxFromContext(ctx context.Context) (*SettleCtx, bool) {
	if ctx == nil {
		return nil, false
	}
	sc, ok := ctx.Value(settleCtxKey{}).(*SettleCtx)
	if !ok || sc == nil {
		return nil, false
	}
	return sc, true
}
