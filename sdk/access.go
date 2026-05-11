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
}

// NewAccessProvider wires the provider's dependencies. Any of db /
// redis / apiKeyCache may be nil only in narrow test scenarios; the
// production Builder wiring (Task 20) always supplies all four.
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

	meta := baseMetadata(claims.UserID, 0, nil, 1.0)
	if claims.Email != "" {
		meta["email"] = claims.Email
	}
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
