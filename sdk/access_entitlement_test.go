package sdk

import (
	"context"
	"flag"
	"fmt"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xxww0098/cpa-gateway/authutil"
	"github.com/xxww0098/cpa-gateway/infra"
	"github.com/xxww0098/cpa-gateway/model"
	"github.com/xxww0098/cpa-gateway/testutil"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"pgregory.net/rapid"
)

// raiseRapidChecksAccess raises the -rapid.checks iteration count to the
// requested minimum for the duration of the current test. It mirrors the
// pattern established in pricing/estimate_max_tokens_test.go and
// sdk/holdmw_preflight_test.go so this file stays self-contained: sdk
// package tests cannot depend on the pricing package's test-only helpers
// (the dependency direction forbids an api/sdk → pricing _test import
// chain, and pricing/*_test.go symbols are not exported to other packages
// regardless).
//
// The override is a no-op when the caller already configured a higher
// value via -rapid.checks or the RAPID_CHECKS env var so CI runners with
// a tighter budget still get the floor they asked for. The original flag
// value is restored on test cleanup so neighboring tests observe the
// user-supplied configuration.
func raiseRapidChecksAccess(t *testing.T, minChecks int) {
	t.Helper()
	fl := flag.Lookup("rapid.checks")
	if fl == nil {
		return
	}
	orig := fl.Value.String()
	cur, err := strconv.Atoi(orig)
	if err != nil || cur >= minChecks {
		return
	}
	if setErr := flag.Set("rapid.checks", strconv.Itoa(minChecks)); setErr != nil {
		t.Fatalf("flag.Set rapid.checks: %v", setErr)
	}
	t.Cleanup(func() { _ = flag.Set("rapid.checks", orig) })
}

// newEntitlementTestDB opens a fresh in-memory SQLite database with the
// minimum schema AccessProvider.Authenticate touches on the API-key auth
// path (users, api_keys, groups, subscriptions). A single max-open
// connection keeps the :memory: database visible across the goroutines
// that touchAPIKeyAsync backgrounds, matching the pattern established in
// sdk/access_test.go and sdk/usage_fallback_settle_test.go.
func newEntitlementTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:access_entitlement_%s?mode=memory&cache=shared", t.Name())
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
		&model.ApiKey{},
		&model.Group{},
		&model.Subscription{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// TestAccessRateMultDefaultsOnRevokedEntitlement is the property-based
// regression for Requirements 3.4 + 3.5 (Property 10). It asserts the
// runtime enforcement of API-key group entitlements inside
// sdk.AccessProvider.Authenticate: when a cpa- API key is bound to a
// non-baseline group (RateMultiplier != 1.0), the returned
// access.Result.Metadata SHALL honor the group's RateMultiplier AND
// surface group_id only when the owning user still holds an active,
// unexpired subscription for that group. Otherwise — which is the
// revocation branch this feature introduces — rate_mult SHALL collapse
// to the baseline 1.0 AND group_id SHALL NOT appear in the metadata, so
// downstream HoldMiddleware cannot apply a discounted multiplier the
// tenant no longer owns.
//
// Generator shape (per the task sheet):
//
//	groupID, hasActiveSub, rateMult
//
// where rateMult is derived from the sampled group (so rateMult tracks
// the group's RateMultiplier by construction). The baseline group is
// deliberately excluded from the sampling pool because the task sheet
// requires groupID != baseline — otherwise the predicate
// Group.RateMultiplier == 1.0 short-circuits accessControlsGroupEntitled
// to true and hides the revocation branch this test exists to cover.
//
// Assertions:
//
//   - hasActiveSub == true
//     ⇒ metadata.rate_mult == formatFloat(grp.RateMultiplier)
//     AND metadata.group_id == formatUint(groupID)
//
//   - hasActiveSub == false && groupID != baseline
//     ⇒ metadata.rate_mult == "1"         (formatFloat(1.0))
//     AND metadata.group_id is ABSENT
//
// Fixture strategy:
//
//   - Seed the AccessProvider once, outside the rapid loop, so the
//     APIKeyCache and Redis + SQLite handles remain hot across iterations
//     (each iteration generates a fresh key plaintext, so there is no
//     cache cross-contamination).
//   - Seed one baseline group (RateMultiplier=1.0) and three non-baseline
//     groups with distinct multipliers (1.5, 2.5, 0.5). Non-1.0 multipliers
//     are mandatory: a 1.0 multiplier would short-circuit the entitlement
//     predicate through the baseline branch and the revocation case would
//     never fire.
//   - Use a fresh User + ApiKey (+ optional Subscription) per iteration.
//     The user email / key hash are unique-indexed in the schema; a
//     monotonic iteration counter guarantees collision-free inserts
//     even when rapid retries shrunken inputs.
//
// The bearer format follows sdk/access.go's dispatch rule
// (strings.HasPrefix(token, "cpa-")): authutil.NewAPIKey() produces a
// "cpa-" + 64 hex plaintext, which is the exact shape Authenticate
// matches to route into authenticateAPIKey.
//
// **Validates: Property 10, Requirements 3.4, 3.5**
func TestAccessRateMultDefaultsOnRevokedEntitlement(t *testing.T) {
	raiseRapidChecksAccess(t, 200)

	db := newEntitlementTestDB(t)
	rdb, _ := testutil.MustMiniRedis(t)
	cache := infra.NewAPIKeyCache()
	provider := NewAccessProvider(db, rdb, cache, testJWTSecret)

	// Baseline group — RateMultiplier=1.0 is the marker
	// accessControlsGroupEntitled uses to short-circuit the subscription
	// check: every Active_User implicitly holds this entitlement. We
	// seed it for completeness (a real gateway always has a baseline
	// row) but never pick it as the ApiKey's bound group because the
	// task sheet constrains groupID != baseline so the revocation
	// branch is observable.
	baseline := &model.Group{Name: "baseline-entitlement-test", RateMultiplier: 1.0}
	if err := db.Create(baseline).Error; err != nil {
		t.Fatalf("seed baseline group: %v", err)
	}

	// Non-baseline groups, each with a distinct RateMultiplier != 1.0.
	// A 1.0 multiplier here would defeat the test's purpose because
	// accessControlsGroupEntitled would bypass the subscription check.
	// We cover both > 1.0 (premium) and < 1.0 (discounted) so the
	// property holds symmetrically across the pricing spectrum.
	nonBaselineGroups := make([]*model.Group, 0, 3)
	for idx, mult := range []float64{1.5, 2.5, 0.5} {
		g := &model.Group{
			Name:           fmt.Sprintf("non-baseline-%d", idx),
			RateMultiplier: mult,
		}
		if err := db.Create(g).Error; err != nil {
			t.Fatalf("seed non-baseline group %d: %v", idx, err)
		}
		nonBaselineGroups = append(nonBaselineGroups, g)
	}

	// Monotonic iteration counter for collision-free user emails and
	// API-key plaintexts. A stale rapid retry could otherwise reinsert
	// the same email twice and trip the unique index.
	var iter atomic.Int64

	rapid.Check(t, func(rt *rapid.T) {
		i := iter.Add(1)

		// --- Generate (groupID, hasActiveSub, rateMult). ---
		// rateMult is derived from the sampled group so it always
		// matches the group's RateMultiplier by construction; keeping
		// rateMult as an independent generator would decouple it from
		// the data the production code actually reads, which would make
		// the assertion non-faithful.
		idx := rapid.IntRange(0, len(nonBaselineGroups)-1).Draw(rt, "groupIndex")
		grp := nonBaselineGroups[idx]
		hasActiveSub := rapid.Bool().Draw(rt, "hasActiveSub")

		// --- Fresh user. Status="active" is mandatory so userIsActive
		// passes; otherwise the auth path rejects before the
		// entitlement check fires and we cannot observe group_id /
		// rate_mult at all. Balance is irrelevant for auth but the
		// column is non-nullable.
		user := &model.User{
			Email:        fmt.Sprintf("entitlement-%d@test.local", i),
			PasswordHash: "x",
			Role:         "user",
			Status:       "active",
			Balance:      100,
		}
		if err := db.Create(user).Error; err != nil {
			rt.Fatalf("create user (i=%d): %v", i, err)
		}

		// --- Optional subscription. When hasActiveSub is true we seed
		// an active, unexpired subscription that EXACTLY matches the
		// sampled group so accessControlsGroupEntitled's
		// existence-query resolves to true. The reset / expires fields
		// are in the far future so the subscription stays valid for the
		// full test runtime.
		if hasActiveSub {
			now := time.Now().UTC()
			sub := &model.Subscription{
				UserID:         user.ID,
				PackageID:      1,
				GroupID:        grp.ID,
				GroupName:      grp.Name,
				Status:         "active",
				StartsAt:       now.Add(-time.Hour),
				ExpiresAt:      now.Add(30 * 24 * time.Hour),
				DailyResetAt:   now.Add(24 * time.Hour),
				WeeklyResetAt:  now.Add(7 * 24 * time.Hour),
				MonthlyResetAt: now.Add(30 * 24 * time.Hour),
			}
			if err := db.Create(sub).Error; err != nil {
				rt.Fatalf("create subscription (i=%d): %v", i, err)
			}
		}

		// --- Fresh API key bound to the sampled non-baseline group.
		// authutil.NewAPIKey() yields a "cpa-" + 64 hex plaintext,
		// which is the exact bearer shape
		// sdk/access.go's Authenticate dispatches on via
		// strings.HasPrefix(token, "cpa-"). The KeyHash is SHA-256 of
		// the plaintext, matching the production hashing scheme.
		plaintext, err := authutil.NewAPIKey()
		if err != nil {
			rt.Fatalf("new api key (i=%d): %v", i, err)
		}
		groupID := grp.ID
		ak := &model.ApiKey{
			UserID:    user.ID,
			KeyHash:   authutil.HashAPIKey(plaintext),
			KeyPrefix: authutil.APIKeyPrefix(plaintext),
			Name:      fmt.Sprintf("entitlement-key-%d", i),
			Status:    "active",
			GroupID:   &groupID,
		}
		if err := db.Create(ak).Error; err != nil {
			rt.Fatalf("create api key (i=%d): %v", i, err)
		}

		// --- Authenticate. Request path is irrelevant to the access
		// provider — Authenticate only reads the Authorization header —
		// but we use /v1/chat/completions for symmetry with the
		// production call site.
		req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer "+plaintext)

		result, authErr := provider.Authenticate(context.Background(), req)
		if authErr != nil {
			rt.Fatalf(
				"unexpected auth error (i=%d hasActiveSub=%v groupID=%d mult=%g): %+v",
				i, hasActiveSub, groupID, grp.RateMultiplier, authErr,
			)
		}
		if result == nil {
			rt.Fatalf(
				"nil access result (i=%d hasActiveSub=%v groupID=%d)",
				i, hasActiveSub, groupID,
			)
		}

		// --- Assert the metadata predicate. ---
		gotRateMult := result.Metadata["rate_mult"]
		gotGroupID, hasGroupID := result.Metadata["group_id"]

		if hasActiveSub {
			// Entitlement holds: the bound group's RateMultiplier MUST
			// be surfaced verbatim (formatFloat rounds to the shortest
			// round-tripping representation, matching access.go's
			// baseMetadata writer) and group_id MUST carry the ApiKey's
			// bound group id.
			wantRateMult := strconv.FormatFloat(grp.RateMultiplier, 'f', -1, 64)
			if gotRateMult != wantRateMult {
				rt.Fatalf(
					"rate_mult = %q, want %q (hasActiveSub=true groupID=%d mult=%g)",
					gotRateMult, wantRateMult, groupID, grp.RateMultiplier,
				)
			}
			wantGroupID := strconv.FormatUint(uint64(groupID), 10)
			if !hasGroupID {
				rt.Fatalf(
					"group_id missing, want %q (hasActiveSub=true groupID=%d)",
					wantGroupID, groupID,
				)
			}
			if gotGroupID != wantGroupID {
				rt.Fatalf(
					"group_id = %q, want %q (hasActiveSub=true groupID=%d)",
					gotGroupID, wantGroupID, groupID,
				)
			}
			return
		}

		// hasActiveSub == false AND groupID != baseline: the
		// entitlement check must reject the group, which collapses
		// rate_mult to the baseline 1.0 and drops group_id from the
		// returned metadata so HoldMiddleware charges the baseline.
		wantRateMult := strconv.FormatFloat(1.0, 'f', -1, 64)
		if gotRateMult != wantRateMult {
			rt.Fatalf(
				"rate_mult = %q, want %q (hasActiveSub=false groupID=%d mult=%g)",
				gotRateMult, wantRateMult, groupID, grp.RateMultiplier,
			)
		}
		if hasGroupID {
			rt.Fatalf(
				"group_id = %q, want absent (hasActiveSub=false groupID=%d mult=%g)",
				gotGroupID, groupID, grp.RateMultiplier,
			)
		}
	})
}
