package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xxww0098/cpa-gateway/authutil"
	"github.com/xxww0098/cpa-gateway/config"
	"github.com/xxww0098/cpa-gateway/infra"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"pgregory.net/rapid"
)

// inactiveMiddlewareIterCounter mints per-iteration DSN tags that are
// guaranteed unique across a rapid.Check invocation. Without this, two
// iterations that happen to draw the same (status, authType, userID)
// triple would share the same shared-cache sqlite DSN and collide on
// the seeded users.id primary key.
var inactiveMiddlewareIterCounter atomic.Int64

// Feature: billing-security-hardening, Task 13.3 — Panel AuthMiddleware
// user-status recheck (Property 11, Requirements 4.3 / 4.4 / 4.7).
//
// Acceptance criteria pinned by this file:
//
//  1. TestPanelMiddlewareInactiveUserRejected (rapid over status × auth):
//     a credential whose owning user is not in Status="active" MUST
//     be rejected with HTTP 401 BEFORE the downstream handler runs,
//     regardless of whether the credential is an API key (stale cache
//     path — bc.Status != "active" short-circuit) or a JWT (DB-side
//     recheck through pr.userIsActive). The property samples across
//     the observed non-active status values ({"deleted","suspended",
//     "disabled"}) so a future bug that inspects only "deleted" is
//     caught by the shrink.
//
//  2. TestPanelMiddlewareActiveUserPasses (example): a valid API-key
//     credential belonging to an Active_User MUST pass the middleware
//     and reach the terminal handler unmolested. This pins the
//     contrapositive so a regression that accidentally rejects active
//     users (too-aggressive recheck) does not slip through.
//
// Both tests use the real pr.AuthMiddleware() wired to a terminal
// handler whose hit counter is asserted — observing 401 alone is not
// enough to prove "rejection happened before downstream", so the
// counter assertion is what rules out a stray Next() call.
//
// Per the task sheet we do not pin the exact wire-body shape (the
// middleware emits {"code":1001,"message":"invalid_credentials"} via
// the shared Error helper but Property 11 is phrased as "401 +
// invalid_credentials"-family; pinning only the status and the no-
// downstream invariant keeps the test robust to a future body rename
// while still enforcing the security contract).
//
// **Validates: Property 11, Requirements 4.3, 4.4, 4.7**

// inactiveMiddlewareJWTSecret is the shared HS256 secret for JWTs
// issued by the tests. Any non-empty string works; a fixed literal
// keeps failure output stable across rapid iterations.
const inactiveMiddlewareJWTSecret = "inactive-user-test-secret"

// newInactiveMiddlewareRouter builds a PanelRouter backed by an
// in-memory sqlite DB with APIKeyCache + UserStatusCache wired so the
// production middleware's lookup paths fire end-to-end. tag is
// forwarded into a shared-cache DSN so rapid iterations do not
// observe each other's rows; passing "" falls back to ":memory:" for
// single-iteration example tests.
func newInactiveMiddlewareRouter(tb testing.TB, tag string) (*gorm.DB, *PanelRouter) {
	tb.Helper()

	dsn := ":memory:"
	if tag != "" {
		// Shared-cache mode lets the middleware's DB read (which runs
		// on the gin request goroutine) see the rows inserted by the
		// test goroutine. Per-tag DSN keeps iterations isolated.
		dsn = fmt.Sprintf("file:middleware_inactive_%s?mode=memory&cache=shared", tag)
	}

	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		tb.Fatalf("open sqlite: %v", err)
	}
	if sqlDB, derr := db.DB(); derr == nil {
		// Single connection so the DB view stays consistent across
		// goroutines during one iteration.
		sqlDB.SetMaxOpenConns(1)
		tb.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := db.AutoMigrate(&model.User{}, &model.ApiKey{}); err != nil {
		tb.Fatalf("automigrate: %v", err)
	}

	cfg := &config.Config{}
	cfg.Auth.JWT.Secret = inactiveMiddlewareJWTSecret
	pr := NewPanelRouter(db, nil, nil, nil, cfg)
	pr.APIKeyCache = infra.NewAPIKeyCache()
	pr.UserStatusCache = infra.NewUserStatusCache()
	return db, pr
}

// buildInactiveMiddlewareEngine wires a fresh gin engine with only
// AuthMiddleware and a terminal handler that flips *hit to true. The
// caller reads *hit after ServeHTTP to assert whether the middleware
// let the request through. We avoid registering TraceIDMiddleware /
// MetricsMiddleware / RateLimitMiddleware — they are orthogonal to
// the contract under test and would add noise on failure output.
func buildInactiveMiddlewareEngine(pr *PanelRouter, hit *bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	authed := r.Group("/", pr.AuthMiddleware())
	authed.GET("/probe", func(c *gin.Context) {
		*hit = true
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

// seedInactiveUser inserts a user row with the requested status.
// email is synthesised from userID to keep the unique-index happy
// across rapid iterations.
func seedInactiveUser(tb testing.TB, db *gorm.DB, userID uint, status string) {
	tb.Helper()
	u := &model.User{
		ID:           userID,
		Email:        fmt.Sprintf("inactive-%d-%d@test.local", userID, time.Now().UnixNano()),
		PasswordHash: "hash",
		Role:         "user",
		Status:       status,
	}
	if err := db.Create(u).Error; err != nil {
		tb.Fatalf("seed user: %v", err)
	}
}

// seedInactiveAPIKey inserts an ApiKey owned by userID and primes the
// APIKeyCache with the supplied cachedStatus so the middleware's
// stale-cache short-circuit can be exercised deterministically. The
// plaintext is returned so the caller can put it in the Bearer header.
func seedInactiveAPIKey(tb testing.TB, db *gorm.DB, cache *infra.APIKeyCache, userID uint, cachedStatus string) string {
	tb.Helper()
	pt, err := authutil.NewAPIKey()
	if err != nil {
		tb.Fatalf("new api key: %v", err)
	}
	ak := &model.ApiKey{
		UserID:    userID,
		KeyHash:   authutil.HashAPIKey(pt),
		KeyPrefix: authutil.APIKeyPrefix(pt),
		Name:      "probe",
		Status:    "active",
	}
	if err := db.Create(ak).Error; err != nil {
		tb.Fatalf("seed api key: %v", err)
	}
	// Prime the cache with the cachedStatus — the middleware's first
	// check (bc.Status != "" && bc.Status != "active") reads this
	// value without hitting the DB, which is exactly the stale-cache
	// path Requirement 4.6 / 4.7 guards against.
	cache.Set(ak.KeyHash, &infra.CachedKey{
		UserID:    userID,
		ApiKeyID:  ak.ID,
		GroupID:   nil,
		RateMult:  1.0,
		Status:    cachedStatus,
		ExpiresAt: time.Now().Add(5 * time.Minute),
	})
	return pt
}

// -----------------------------------------------------------------------------
// 1. rapid property: inactive user rejected across status × auth-type
// -----------------------------------------------------------------------------

// inactiveStatuses enumerates the non-active User.Status values the
// production code path is expected to treat as "reject". The rapid
// property draws from this slice so shrink output names the status
// that triggered the failure instead of shrinking into an empty or
// whitespace literal (both would be pathological seeds that the
// middleware is under no contract to reject).
var inactiveStatuses = []string{"deleted", "suspended", "disabled"}

// TestPanelMiddlewareInactiveUserRejected samples
// (status ∈ inactiveStatuses) × (auth-type ∈ {apikey, jwt}) and
// asserts the middleware rejects the request with HTTP 401 AND the
// terminal handler never runs. This is the rapid-driven property
// promised by task 13.3.
//
// Notes on the two auth paths:
//
//   - API key: the APIKeyCache is primed with the inactive status so
//     the middleware's first short-circuit (bc.Status != "" &&
//     bc.Status != "active") fires without any DB read. A separate
//     users row is still seeded with the same status so that, if a
//     future refactor moves the cache-status check behind the DB
//     recheck, this test still passes (the DB recheck would reject
//     the credential anyway).
//
//   - JWT: bc.Status is hard-set to "active" on the JWT branch (it
//     does not propagate from any cache), so the first short-circuit
//     cannot fire for this path. The rejection MUST come from
//     pr.userIsActive's DB lookup, which reads the seeded user row's
//     Status column. The UserStatusCache is left empty for this path
//     so the lookup is forced to go to the DB.
//
// **Validates: Property 11, Requirements 4.3, 4.4, 4.7**
func TestPanelMiddlewareInactiveUserRejected(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		status := rapid.SampledFrom(inactiveStatuses).Draw(rt, "status")
		authType := rapid.SampledFrom([]string{authTypeAPIKey, authTypeJWT}).Draw(rt, "authType")
		// Keep userID positive — GORM accepts explicit non-zero IDs on
		// insert, which makes failure output deterministic.
		userID := uint(rapid.IntRange(1, 1_000_000).Draw(rt, "userID"))

		// Per-iteration unique tag so two iterations that happen to
		// draw the same (status, authType, userID) triple cannot
		// collide on the shared-cache sqlite DSN or the users.id PK.
		tag := fmt.Sprintf("reject_%s_%s_%d_%d",
			status, authType, userID, inactiveMiddlewareIterCounter.Add(1))
		db, pr := newInactiveMiddlewareRouter(t, tag)

		seedInactiveUser(t, db, userID, status)

		var token string
		switch authType {
		case authTypeAPIKey:
			// Stale-cache path: prime APIKeyCache with the inactive
			// status. The middleware short-circuits on this value
			// alone; no DB read required.
			token = seedInactiveAPIKey(t, db, pr.APIKeyCache, userID, status)
		case authTypeJWT:
			var err error
			token, err = authutil.GenerateJWT(userID, "inactive@test.local",
				inactiveMiddlewareJWTSecret, 1)
			if err != nil {
				rt.Fatalf("generate JWT: %v", err)
			}
		}

		var terminalHit bool
		engine := buildInactiveMiddlewareEngine(pr, &terminalHit)

		req := httptest.NewRequest(http.MethodGet, "/probe", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		// Assertion 1: HTTP 401. Per task guidance we do NOT pin the
		// exact wire-body shape — the security contract is the status
		// code plus the no-downstream invariant asserted below.
		if w.Code != http.StatusUnauthorized {
			rt.Fatalf("status: got %d want 401; auth=%s status=%q body=%s",
				w.Code, authType, status, w.Body.String())
		}

		// Assertion 2: the terminal handler must never have run.
		// Observing 401 alone is not enough — a middleware that
		// Aborts AFTER the handler (or forgets c.Abort()) would still
		// 401 the response but also leak side effects. The hit
		// counter is the only unambiguous signal for "recheck fired
		// before downstream".
		if terminalHit {
			rt.Fatalf("terminal handler ran despite inactive user; auth=%s status=%q", authType, status)
		}
	})
}

// -----------------------------------------------------------------------------
// 2. example: active API-key credential passes
// -----------------------------------------------------------------------------

// TestPanelMiddlewareActiveUserPasses pins the positive branch of the
// user-status recheck contract. A credential whose cached status is
// "active" AND whose DB-side user row is "active" MUST traverse the
// middleware to the downstream handler. The terminal handler sets a
// hit flag so a too-aggressive recheck regression (e.g. one that
// ignores "active" or that mis-reads the cache) is caught immediately.
//
// We exercise the API-key path because the JWT path's active-user
// recheck is already covered by sibling tests in sdk/ (specifically
// sdk/access_inactive_user_test.go's negative side). Pinning the API-
// key positive path here keeps the test surface focused on the
// middleware's own cache-first logic.
//
// **Validates: Property 11 (contrapositive), Requirements 4.3, 4.4, 4.7**
func TestPanelMiddlewareActiveUserPasses(t *testing.T) {
	const userID uint = 4242

	db, pr := newInactiveMiddlewareRouter(t, "")

	// Seed an Active_User whose Status is the exact constant the
	// middleware compares against. Any drift between the seeded value
	// and userStatusActive would flip this test's meaning, so the
	// seed uses the shared constant deliberately.
	seedInactiveUser(t, db, userID, userStatusActive)

	// Prime APIKeyCache with the matching active status. The
	// middleware's first short-circuit (bc.Status != "active") must
	// NOT fire for this entry, and because the auth-type is api_key
	// the JWT-only userIsActive DB recheck is skipped.
	pt := seedInactiveAPIKey(t, db, pr.APIKeyCache, userID, userStatusActive)

	var terminalHit bool
	engine := buildInactiveMiddlewareEngine(pr, &terminalHit)

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+pt)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	if !terminalHit {
		t.Fatalf("terminal handler did not run for active user; body=%s", w.Body.String())
	}
}
