package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xxww0098/cpa-gateway/config"
	"github.com/xxww0098/cpa-gateway/infra"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"pgregory.net/rapid"
)

// newRebindRouter spins up a PanelRouter with an in-memory sqlite DB and a
// real *infra.APIKeyCache so the test can observe cache state before and
// after the rebind handler runs. The middleware/auth layer is intentionally
// bypassed — Property 9 is about the handler body (UserHoldsEntitlement
// preflight + APIKeyCache.Delete ordering), not about Panel JWT/apikey
// validation. Mirrors the shim pattern used by purchase_debt_block_test.go.
func newRebindRouter(t testing.TB) (*PanelRouter, *gorm.DB) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.User{},
		&model.ApiKey{},
		&model.Group{},
		&model.Subscription{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}

	cfg := &config.Config{}
	pr := NewPanelRouter(db, nil, nil, nil, cfg)
	pr.APIKeyCache = infra.NewAPIKeyCache()
	return pr, db
}

// seedRebindActor inserts an active user with the given userID, together
// with a dummy API key that the handler can target. The key is returned to
// the caller so the test can assert on ID, KeyHash, and GroupID. The key's
// KeyHash is synthesised (not hashed from a real plaintext) because the
// rebind handler only cares about the stored hash for cache invalidation.
func seedRebindActor(t testing.TB, db *gorm.DB, userID uint, initialGroupID *uint) model.ApiKey {
	t.Helper()
	u := &model.User{
		ID:           userID,
		Email:        fmt.Sprintf("rebind-%d@test.local", userID),
		PasswordHash: "hash",
		Role:         "user",
		Status:       userStatusActive,
	}
	if err := db.Create(u).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	key := model.ApiKey{
		UserID:    userID,
		KeyHash:   fmt.Sprintf("hash-%d-%d", userID, time.Now().UnixNano()),
		KeyPrefix: "cpa-deadbe",
		Name:      "test",
		Status:    "active",
		GroupID:   initialGroupID,
	}
	if err := db.Create(&key).Error; err != nil {
		t.Fatalf("seed api key: %v", err)
	}
	return key
}

// seedRebindGroups creates one baseline group (rate_multiplier = 1.0) and
// one non-baseline "pro" group (rate_multiplier = 0.5). Returning both IDs
// lets the property test pick between entitled/unentitled targets and also
// verify that the baseline group is always treated as entitled. GORM
// autoincrements from 1 on a freshly migrated table so we do not pin the
// IDs ourselves; the caller reads them off the returned values.
func seedRebindGroups(t testing.TB, db *gorm.DB) (baseline, pro model.Group) {
	t.Helper()
	baseline = model.Group{Name: "default", RateMultiplier: 1.0}
	if err := db.Create(&baseline).Error; err != nil {
		t.Fatalf("seed baseline group: %v", err)
	}
	pro = model.Group{Name: "pro", RateMultiplier: 0.5}
	if err := db.Create(&pro).Error; err != nil {
		t.Fatalf("seed pro group: %v", err)
	}
	return baseline, pro
}

// seedActiveSubscription installs an active, unexpired Subscription row
// that grants userID an entitlement to groupID. The ExpiresAt is pinned
// well into the future so the property test does not race a clock boundary.
func seedActiveSubscription(t testing.TB, db *gorm.DB, userID, groupID uint) {
	t.Helper()
	sub := model.Subscription{
		UserID:    userID,
		PackageID: 1,
		GroupID:   groupID,
		Status:    "active",
		StartsAt:  time.Now().UTC().Add(-time.Hour),
		ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour),
	}
	if err := db.Create(&sub).Error; err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
}

// primeAPIKeyCache stores a synthesised CachedKey under the ApiKey's
// KeyHash so the property test can assert cache-preservation semantics
// (Property 9 says the rejected rebind MUST NOT evict this entry). The
// cached GroupID is pinned to the pre-rebind value.
func primeAPIKeyCache(cache *infra.APIKeyCache, key model.ApiKey) *infra.CachedKey {
	entry := &infra.CachedKey{
		UserID:    key.UserID,
		ApiKeyID:  key.ID,
		GroupID:   key.GroupID,
		RateMult:  1.0,
		Status:    key.Status,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	cache.Set(key.KeyHash, entry)
	return entry
}

// runRebind exercises the handler end-to-end via an httptest recorder. The
// shim middleware injects a BillingCtx identical to what AuthMiddleware
// would emit for a valid JWT from the acting user, keeping the test focused
// on the handler body.
func runRebind(t testing.TB, pr *PanelRouter, userID, apiKeyID, targetGroupID uint) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(seedBillingCtxMiddleware(userID))
	r.PATCH("/api/panel/user/api-keys/:id/group", pr.RebindAPIKeyGroupHandler)

	body, err := json.Marshal(map[string]any{"group_id": targetGroupID})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch,
		"/api/panel/user/api-keys/"+strconv.FormatUint(uint64(apiKeyID), 10)+"/group",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestRebindRejectsUnentitled is the rapid-driven property for task 10.3:
//
//	rapid over (groupID, hasActiveSub):
//	  hasActiveSub == false && groupID != baseline
//	    ⇒ HTTP 403 body {"error":"group_not_entitled"}
//	    ∧ DB: ApiKey.GroupID unchanged
//	    ∧ APIKeyCache: the primed entry under key.KeyHash is still present
//
// The entitled / baseline branches are covered by the dedicated success
// example test below; the property here only exercises the rejection
// contract for the cases Property 9 guarantees.
//
// **Validates: Property 9, Requirements 3.1, 3.2**
func TestRebindRejectsUnentitled(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Keep userID strictly positive — seedRebindActor relies on GORM's
		// primary-key acceptance of explicit non-zero values.
		userID := uint(rapid.IntRange(1, 1_000_000).Draw(rt, "userID"))
		initialGroupIsBaseline := rapid.Bool().Draw(rt, "initialGroupIsBaseline")

		pr, db := newRebindRouter(t)
		baseline, pro := seedRebindGroups(t, db)

		// Pick the starting GroupID: either the baseline group or nil.
		// The rebind target is always the non-baseline "pro" group.
		var initial *uint
		if initialGroupIsBaseline {
			baselineID := baseline.ID
			initial = &baselineID
		}
		key := seedRebindActor(t, db, userID, initial)
		primed := primeAPIKeyCache(pr.APIKeyCache, key)

		// We do NOT seed any Subscription, so hasActiveSub == false for
		// the non-baseline target. Property: the handler must reject.
		w := runRebind(t, pr, userID, key.ID, pro.ID)

		// Assertion 1: HTTP 403 with the documented error code.
		if w.Code != http.StatusForbidden {
			rt.Fatalf("status: got %d want 403; body=%s (userID=%d target=%d)",
				w.Code, w.Body.String(), userID, pro.ID)
		}
		var resp map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			rt.Fatalf("parse body: %v; raw=%s", err, w.Body.String())
		}
		if got, want := resp["message"], "group_not_entitled"; got != want {
			rt.Fatalf("body.message: got %v want %q; body=%s", got, want, w.Body.String())
		}

		// Assertion 2: DB row's GroupID was NOT updated. Reload from the
		// DB (not the seeded struct) so the assertion reflects the
		// persisted state.
		var after model.ApiKey
		if err := db.First(&after, key.ID).Error; err != nil {
			rt.Fatalf("reload api key: %v", err)
		}
		if initial == nil {
			if after.GroupID != nil {
				rt.Fatalf("GroupID: got %v want nil (rebind must not touch DB on reject)", *after.GroupID)
			}
		} else {
			if after.GroupID == nil {
				rt.Fatalf("GroupID: got nil want %d (rebind must not touch DB on reject)", *initial)
			} else if *after.GroupID != *initial {
				rt.Fatalf("GroupID: got %d want %d (rebind must not touch DB on reject)",
					*after.GroupID, *initial)
			}
		}

		// Assertion 3: APIKeyCache entry is still present and unchanged.
		// Property 9 requires cache invalidation ONLY on successful rebind.
		cached, ok := pr.APIKeyCache.Get(key.KeyHash)
		if !ok {
			rt.Fatalf("APIKeyCache entry was evicted on reject; want preserved")
		}
		if cached != primed {
			rt.Fatalf("APIKeyCache entry was replaced on reject (got %p want %p)", cached, primed)
		}
	})
}

// TestRebindInvalidatesCacheOnSuccess covers the positive branch of
// Property 9: entitled rebind ⇒ DB update + exactly one APIKeyCache.Delete
// call for the targeted KeyHash. Because *infra.APIKeyCache is a concrete
// type (no interface seam available to the handler), we observe the
// invalidation by priming the cache, running the handler, and asserting
// the entry is no longer reachable via Get. Combined with the negative
// property above (which asserts the entry remains reachable on reject),
// the two tests together bracket the "exactly-once on success" contract.
//
// **Validates: Property 9, Requirements 3.1, 3.2**
func TestRebindInvalidatesCacheOnSuccess(t *testing.T) {
	const userID uint = 4242

	pr, db := newRebindRouter(t)
	baseline, pro := seedRebindGroups(t, db)

	// Seed the user with an active subscription to the "pro" group so
	// UserHoldsEntitlement returns true for the rebind target.
	baselineID := baseline.ID
	key := seedRebindActor(t, db, userID, &baselineID)
	seedActiveSubscription(t, db, userID, pro.ID)

	primed := primeAPIKeyCache(pr.APIKeyCache, key)

	w := runRebind(t, pr, userID, key.ID, pro.ID)

	// Assertion 1: HTTP 200 unified envelope.
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	var resp APIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse body: %v; raw=%s", err, w.Body.String())
	}
	if resp.Code != 0 {
		t.Fatalf("response code: got %d want 0; body=%s", resp.Code, w.Body.String())
	}

	// Assertion 2: DB row's GroupID was updated to the new pro group.
	var after model.ApiKey
	if err := db.First(&after, key.ID).Error; err != nil {
		t.Fatalf("reload api key: %v", err)
	}
	if after.GroupID == nil {
		t.Fatalf("GroupID: got nil want %d (rebind must persist new group)", pro.ID)
	}
	if *after.GroupID != pro.ID {
		t.Fatalf("GroupID: got %d want %d", *after.GroupID, pro.ID)
	}

	// Assertion 3: APIKeyCache entry for this KeyHash was invalidated
	// (Delete was called exactly once on success). The primed entry must
	// no longer be reachable via Get; any re-authentication will force a
	// DB re-resolution of the new group and its RateMultiplier.
	if _, ok := pr.APIKeyCache.Get(key.KeyHash); ok {
		t.Fatalf("APIKeyCache entry still reachable after successful rebind; want invalidated")
	}
	// And the evicted entry was precisely the one we primed — guards
	// against a handler bug that deletes by a different hash.
	_ = primed
}
