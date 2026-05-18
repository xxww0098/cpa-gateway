package sdk

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/xxww0098/cpa-gateway/authutil"
	"github.com/xxww0098/cpa-gateway/infra"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/gorm"
)

// accessProviderName is the identifier surfaced to the cliproxy SDK's
// access manager and written into Result.Provider so downstream
// middleware (HoldMiddleware, UsagePlugin) can assert the source.
const accessProviderName = "cpa-tenant"

// apiKeyTTL is the L1 cache lifetime for validated API key rows.
const apiKeyTTL = 5 * time.Minute

// AccessProvider implements cliproxy's access.Provider for CPA tenants.
//
// It accepts two credential shapes on the Authorization header:
//
//	Authorization: Bearer cpa-<64 hex>   → API key lookup against ApiKey table
//	Authorization: Bearer <jwt>          → HS256 JWT signed by jwtSecret
//
// On success it returns an access.Result whose Metadata carries every
// piece of billing state the downstream HoldMiddleware needs so that
// middleware can operate without a second DB round-trip. All metadata
// values are stringified because access.Result.Metadata is typed as
// map[string]string.
//
// This provider deliberately performs no Hold/Settle/Release side
// effects — those live in sdk/holdmw.go so that the access layer stays
// idempotent and can be retried safely by the SDK runtime.
type AccessProvider struct {
	db          *gorm.DB
	redis       *redis.Client
	apiKeyCache *infra.APIKeyCache
	jwtSecret   string

	// UserStatusCache memoizes the `users.status` column so that the
	// per-request user-status recheck (see userIsActive) does not hit
	// the DB on every authenticated call. Injected by main.go during
	// Stage 4 wiring; nil-safe so existing tests (which construct this
	// provider via NewAccessProvider) continue to work — a nil cache
	// simply means every call reads through to the DB.
	UserStatusCache *infra.UserStatusCache
}

// userStatusMissing is the sentinel value cached when a user row is
// absent from the DB entirely. It lets us suppress the DB lookup for
// the same bogus userID during a credential-guessing burst without
// conflating "missing" with any legitimate Status value.
const userStatusMissing = "missing"

// NewAccessProvider wires the provider's dependencies. Any of db /
// redis / apiKeyCache may be nil only in narrow test scenarios; the
// production Builder wiring (Task 20) always supplies all four.
//
// UserStatusCache is NOT part of this constructor: it is assigned
// directly on the returned value by main.go so the same cache
// instance can be shared with PanelRouter (see Stage 4). Callers that
// do not set it get a nil-safe pass-through to the DB.
func NewAccessProvider(db *gorm.DB, redisClient *redis.Client, apiKeyCache *infra.APIKeyCache, jwtSecret string) *AccessProvider {
	return &AccessProvider{
		db:          db,
		redis:       redisClient,
		apiKeyCache: apiKeyCache,
		jwtSecret:   jwtSecret,
	}
}

// Identifier returns the registry key used by the cliproxy access
// manager to route inbound requests to this provider.
func (p *AccessProvider) Identifier() string { return accessProviderName }

// Authenticate parses the Authorization header on r and resolves it to
// a CPA Gateway user. It returns an access.Result on success; on any
// validation failure it returns a typed *access.AuthError so the SDK
// can emit the right HTTP status.
func (p *AccessProvider) Authenticate(ctx context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if r == nil {
		return nil, sdkaccess.NewNoCredentialsError()
	}
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		return nil, sdkaccess.NewNoCredentialsError()
	}

	if strings.HasPrefix(token, "cpa-") {
		return p.authenticateAPIKey(ctx, token)
	}
	return p.authenticateJWT(ctx, token)
}

// authenticateAPIKey validates a "cpa-" prefixed key against the L1
// cache first and falls back to a DB lookup. On success it loads the
// group rate multiplier and any active subscription.
func (p *AccessProvider) authenticateAPIKey(ctx context.Context, plaintext string) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if p.db == nil {
		return nil, sdkaccess.NewInternalAuthError("database not initialized", nil)
	}

	keyHash := authutil.HashAPIKey(plaintext)

	var (
		userID   uint
		apiKeyID uint
		groupID  *uint
		rateMult = 1.0
		status   = "active"
	)

	if p.apiKeyCache != nil {
		if ck, found := p.apiKeyCache.Get(keyHash); found {
			// Cache hit still has to gate on the cached ApiKey.Status:
			// a DB-side deactivation that has been flushed via
			// APIKeyCache.Delete lands here as a miss, but a
			// deactivation that raced with a still-warm cache entry
			// shows up as Status != "active". The DB lookup branch
			// below already filters on status = "active", so the
			// symmetric check belongs here. The owning user's status
			// is re-validated below by userIsActive (Requirement 4.6).
			if ck.Status != "active" {
				return nil, sdkaccess.NewInvalidCredentialError()
			}
			userID = ck.UserID
			apiKeyID = ck.ApiKeyID
			groupID = ck.GroupID
			rateMult = ck.RateMult
			status = ck.Status
		}
	}

	if userID == 0 {
		var apiKey model.ApiKey
		err := p.db.WithContext(ctx).Where("key_hash = ? AND status = ?", keyHash, "active").First(&apiKey).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, sdkaccess.NewInvalidCredentialError()
			}
			return nil, sdkaccess.NewInternalAuthError("api key lookup failed", err)
		}

		if apiKey.GroupID != nil {
			var grp model.Group
			if lookupErr := p.db.WithContext(ctx).First(&grp, *apiKey.GroupID).Error; lookupErr == nil {
				if grp.RateMultiplier > 0 {
					rateMult = grp.RateMultiplier
				}
			}
		}

		userID = apiKey.UserID
		apiKeyID = apiKey.ID
		groupID = apiKey.GroupID
		status = apiKey.Status

		if p.apiKeyCache != nil {
			p.apiKeyCache.Set(keyHash, &infra.CachedKey{
				UserID:    userID,
				ApiKeyID:  apiKeyID,
				GroupID:   groupID,
				RateMult:  rateMult,
				Status:    status,
				ExpiresAt: time.Now().Add(apiKeyTTL),
			})
		}

		// Fire-and-forget last_used_at bump. Use a detached context so
		// request cancellation does not abort the write.
		p.touchAPIKeyAsync(apiKeyID)
	}

	// Second-stage gate (Requirement 4.1, 4.4, 4.6, 4.7): even when
	// the primary ApiKey lookup — whether from the L1 cache or a fresh
	// DB read — reports Status = "active", the owning user may have
	// been suspended or deleted since the key was cached. userIsActive
	// consults the UserStatusCache and falls back to a single-column
	// DB read on miss; either way the cached api-key entry can be
	// stale relative to the users table, so this check MUST fire in
	// both code paths before we hand back an access.Result. The
	// response shape matches the invalid-credential error used above
	// to avoid leaking which field (key vs. user) caused the rejection.
	if !p.userIsActive(ctx, userID) {
		return nil, sdkaccess.NewInvalidCredentialError()
	}

	// Entitlement filter (Requirements 3.4, 3.5 / Property 10): the
	// ApiKey row — whether read from the L1 cache or freshly loaded
	// above — may still point at a GroupID whose subscription has
	// since lapsed. The cache intentionally stores the key's BOUND
	// group (the DB source of truth), so re-evaluating entitlement on
	// every auth is what lets an expired subscription immediately
	// collapse the principal back to the baseline instead of waiting
	// for the cache TTL. When the user no longer holds the required
	// entitlement, we drop group_id from the returned metadata and
	// reset rate_mult to 1.0 so downstream HoldMiddleware cannot honor
	// a discounted rate that belongs to a revoked group.
	//
	// The helper is a small private duplicate of the same predicate
	// that api/entitlements.go exports as UserHoldsEntitlement; the
	// two copies exist because package dependency rules forbid
	// importing sdk internals from api. Any semantic change here MUST
	// be mirrored in api/entitlements.go.
	if groupID != nil && !p.accessControlsGroupEntitled(ctx, userID, *groupID) {
		groupID = nil
		rateMult = 1.0
	}

	meta := baseMetadata(userID, apiKeyID, groupID, rateMult)
	p.attachSubscriptionMetadata(ctx, userID, meta)

	return &sdkaccess.Result{
		Provider:  accessProviderName,
		Principal: formatUint(userID),
		Metadata:  meta,
	}, nil
}

// authenticateJWT validates an HS256 JWT and emits an access.Result with
// the tenant's subscription metadata attached.
func (p *AccessProvider) authenticateJWT(ctx context.Context, token string) (*sdkaccess.Result, *sdkaccess.AuthError) {
	claims, err := authutil.ValidateJWT(token, p.jwtSecret)
	if err != nil {
		return nil, sdkaccess.NewInvalidCredentialError()
	}
	if claims == nil || claims.UserID == 0 {
		return nil, sdkaccess.NewInvalidCredentialError()
	}

	// User-status recheck (Requirements 4.2, 4.4, 4.7): the JWT path
	// has no APIKeyCache entry to gate on, so every request MUST
	// re-confirm the claimed user is still active before we populate
	// any billing or subscription metadata. userIsActive is cache-
	// backed (see sdk/access.go `userIsActive` for TTL and fail-
	// closed semantics), so the DB hit is O(1) across a credential-
	// guessing burst. Placing the check BEFORE attachSubscriptionMetadata
	// ensures no subscription identifiers or quota values leak out
	// for a suspended or deleted user — the rejection response must
	// be indistinguishable from any other invalid-credential error to
	// avoid user-status oracles.
	if !p.userIsActive(ctx, claims.UserID) {
		return nil, sdkaccess.NewInvalidCredentialError()
	}

	meta := baseMetadata(claims.UserID, 0, nil, 1.0)
	p.attachSubscriptionMetadata(ctx, claims.UserID, meta)

	return &sdkaccess.Result{
		Provider:  accessProviderName,
		Principal: formatUint(claims.UserID),
		Metadata:  meta,
	}, nil
}

// attachSubscriptionMetadata looks up the newest active subscription for
// userID and, if found, writes its identifiers and quota state into meta.
// Missing or stale subscriptions are silently skipped — the caller is
// expected to enforce any "subscription required" policy elsewhere.
func (p *AccessProvider) attachSubscriptionMetadata(ctx context.Context, userID uint, meta map[string]string) {
	if p.db == nil || userID == 0 {
		return
	}

	var sub model.Subscription
	now := time.Now().UTC()
	err := p.db.WithContext(ctx).
		Where("user_id = ? AND status = ? AND expires_at > ?", userID, "active", now).
		Order("expires_at DESC").
		First(&sub).Error
	if err != nil {
		return
	}

	meta["subscription_id"] = formatUint(sub.ID)
	meta["subscription_group_id"] = formatUint(sub.GroupID)
	if sub.DailyLimitUSD != nil {
		meta["daily_limit"] = formatFloat(*sub.DailyLimitUSD)
	}
	if sub.WeeklyLimitUSD != nil {
		meta["weekly_limit"] = formatFloat(*sub.WeeklyLimitUSD)
	}
	if sub.MonthlyLimitUSD != nil {
		meta["monthly_limit"] = formatFloat(*sub.MonthlyLimitUSD)
	}
	meta["daily_used"] = formatFloat(sub.DailyUsageUSD)
	meta["weekly_used"] = formatFloat(sub.WeeklyUsageUSD)
	meta["monthly_used"] = formatFloat(sub.MonthlyUsageUSD)
}

// touchAPIKeyAsync updates last_used_at without blocking the request.
func (p *AccessProvider) touchAPIKeyAsync(apiKeyID uint) {
	if p.db == nil || apiKeyID == 0 {
		return
	}
	go func(id uint) {
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		p.db.WithContext(bg).Model(&model.ApiKey{}).Where("id = ?", id).Update("last_used_at", time.Now())
	}(apiKeyID)
}

// userIsActive reports whether the user row for userID currently has
// Status == "active". It is the second-stage gate used by both the
// API-key and JWT auth paths (see Requirement 4.1/4.2/4.6/4.7): the
// primary credential check may pass against a stale APIKeyCache entry,
// so this step re-confirms the owning user has not been suspended or
// deleted since the key was cached.
//
// Lookup order:
//  1. UserStatusCache — O(1) in-memory hit.
//  2. SELECT status FROM users WHERE id = ? — a single-column DB read
//     when the cache misses.
//
// Any result — active, inactive, or missing — is written back to the
// cache with the apiKeyTTL (clamped inside the cache). Caching
// negative results is intentional: without it, a credential-guessing
// burst against a suspended or non-existent user would hammer the DB
// once per request. A subsequent admin-driven state change calls
// UserStatusCache.InvalidateUser (Stage 3 / Task 14) to flush the
// entry so the next auth observes the new state.
//
// Fail-closed: when db is nil or userID is zero, this returns false;
// the caller then surfaces the standard invalid_credentials error.
// When the DB read itself errors (connectivity glitch, etc.), we
// return false without caching, so transient failures do not pin a
// user into "inactive" for the TTL window.
func (p *AccessProvider) userIsActive(ctx context.Context, userID uint) bool {
	if userID == 0 || p.db == nil {
		return false
	}

	if p.UserStatusCache != nil {
		if us, ok := p.UserStatusCache.Get(userID); ok {
			return us.Status == "active"
		}
	}

	var status string
	err := p.db.WithContext(ctx).
		Model(&model.User{}).
		Select("status").
		Where("id = ?", userID).
		Limit(1).
		Scan(&status).Error
	if err != nil {
		// Do NOT cache transient DB errors; let the next request retry.
		return false
	}

	cached := status
	if cached == "" {
		// Row absent: cache the sentinel so repeated guesses at the
		// same ID don't re-hit the DB within the TTL.
		cached = userStatusMissing
	}
	if p.UserStatusCache != nil {
		p.UserStatusCache.Set(userID, cached, apiKeyTTL)
	}

	return status == "active"
}

// accessControlsGroupEntitled reports whether userID currently holds
// an entitlement for groupID, i.e. whether the cpa- API key bound to
// that group should be honored at its configured RateMultiplier on
// this request. See api/entitlements.go's UserHoldsEntitlement for
// the exported sibling used by Panel handlers; both copies exist to
// respect the project's package dependency direction
// (main → sdk/api/infra → model/...; api MUST NOT import sdk and
// sdk's unexported helpers cannot leak into api), and any semantic
// change here MUST be mirrored there.
//
// Predicate:
//   - db / userID / groupID zero → false (fail-closed). Zero IDs
//     cannot legitimately resolve to an entitlement.
//   - Group row missing → false. If the admin UI has since deleted
//     the group the key points at, we treat the key as unbindable
//     rather than 5xx the auth path.
//   - Group.RateMultiplier == 1.0 (baseline group) → true. Every
//     Active_User implicitly holds this entitlement, so we do not
//     need to consult the subscriptions table.
//   - Otherwise → true iff there is at least one Subscription row
//     with user_id=userID, group_id=groupID, status='active', and
//     expires_at > NOW() (UTC).
//
// Transient errors (Group lookup / Subscription existence query) are
// treated as "not entitled" rather than propagated: the hot auth path
// prefers to project the principal to the conservative baseline over
// 5xx'ing a valid request. The DB error is swallowed here because
// attachSubscriptionMetadata — the very next call — will observe the
// same outage and log it through the standard plugin path.
func (p *AccessProvider) accessControlsGroupEntitled(ctx context.Context, userID uint, groupID uint) bool {
	if p.db == nil || userID == 0 || groupID == 0 {
		return false
	}

	var grp model.Group
	if err := p.db.WithContext(ctx).First(&grp, groupID).Error; err != nil {
		// Missing or transient — deny the multiplier; baseline wins.
		return false
	}
	if grp.RateMultiplier == 1.0 {
		return true
	}

	var count int64
	err := p.db.WithContext(ctx).
		Model(&model.Subscription{}).
		Where("user_id = ? AND group_id = ? AND status = ? AND expires_at > ?",
			userID, groupID, "active", time.Now().UTC()).
		Limit(1).
		Count(&count).Error
	if err != nil {
		return false
	}
	return count > 0
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

// bearerToken returns the token portion of "Bearer <token>" (case-
// insensitive on the scheme). It rejects malformed headers.
func bearerToken(header string) (string, bool) {
	parts := strings.Fields(strings.TrimSpace(header))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

// baseMetadata allocates the result metadata map and populates the
// fields common to both API key and JWT flows.
func baseMetadata(userID, apiKeyID uint, groupID *uint, rateMult float64) map[string]string {
	meta := map[string]string{
		"user_id":   formatUint(userID),
		"rate_mult": formatFloat(rateMult),
	}
	if apiKeyID != 0 {
		meta["api_key_id"] = formatUint(apiKeyID)
	}
	if groupID != nil {
		meta["group_id"] = formatUint(*groupID)
	}
	return meta
}

func formatUint(u uint) string {
	return strconv.FormatUint(uint64(u), 10)
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
