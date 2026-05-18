package sdk

import (
	"context"
	"flag"
	"fmt"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	"github.com/xxww0098/cpa-gateway/authutil"
	"github.com/xxww0098/cpa-gateway/infra"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"pgregory.net/rapid"
)

// Feature: billing-security-hardening, Property 11 — Inactive-user
// credentials are rejected uniformly without side effects.
//
// The two tests in this file exercise the AccessProvider authentication
// surface from two angles:
//
//   1. TestInactiveUserAuthRejectsWithInvalidCredentials drives the fresh
//      DB-read path across every combination of status ∈ {"deleted",
//      "suspended", "disabled"} and credential shape ∈ {apikey, jwt}.
//      Because no cache is pre-populated, userIsActive always goes to the
//      DB, and the assertion is that the rejection is uniform — same
//      invalid_credential AuthError — regardless of which inactive label
//      the user wears or which credential shape is presented. It also
//      proves the rejection leaves zero trace: no BalanceLog row, no
//      UsageLog row, no Redis hold entry.
//
//   2. TestStaleCacheStatusRejected drives the stale-cache path: it
//      warms APIKeyCache with Status="active" from a legitimate DB read,
//      then mutates the user row's status directly (bypassing the admin
//      invalidation hook that production uses). userIsActive is forced
//      to re-consult the DB because the UserStatusCache is empty —
//      which is exactly the coherence guarantee Requirement 4.6 makes.
//      The rejection MUST happen even though the cached ApiKey entry
//      still says "active".
//
// Together these tests pin Property 11's invariants: the rejection code
// path is observationally identical across statuses, across credential
// shapes, and across cache-hot/cache-cold conditions, and it never
// writes billable side effects.

// -----------------------------------------------------------------------------
// test helpers
// -----------------------------------------------------------------------------

// raiseRapidChecksInactiveUser raises the -rapid.checks flag so the two
// rapid.Check properties below hit a ≥ 200 iteration budget, restoring the
// caller's original value on cleanup. Mirrors raiseRapidChecksPreflight in
// sdk/holdmw_preflight_test.go so test runners that pass an explicit
// -rapid.checks value still get the stronger guarantee we want here.
func raiseRapidChecksInactiveUser(t *testing.T, minChecks int) {
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

// inactiveUserEnv bundles the full dependency set for one iteration: an
// in-memory sqlite DB migrated with every table the assertions inspect
// (User, ApiKey, Group, Subscription, BalanceLog, UsageLog), a miniredis
// instance backing the APIKeyCache contract, and the wired AccessProvider.
// Each env is isolated by its own DSN so rapid iterations cannot bleed
// state into one another.
type inactiveUserEnv struct {
	db       *gorm.DB
	srv      *miniredis.Miniredis
	client   *redis.Client
	cache    *infra.APIKeyCache
	provider *AccessProvider
}

// newInactiveUserEnv spins up a fresh environment. The shared-cache sqlite
// DSN is parameterised on the iteration tag so concurrent rapid shrinks
// (which rapid supports but this test does not use) would still not race
// on the same in-memory DB. A single max-open connection keeps the
// in-memory database visible across goroutines (GORM opens per-statement
// connections otherwise).
func newInactiveUserEnv(t testing.TB, tag string) *inactiveUserEnv {
	t.Helper()

	dsn := fmt.Sprintf("file:inactive_user_%s?mode=memory&cache=shared", tag)
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

	// Migrate both the credential tables (User, ApiKey, Group, Subscription)
	// that AccessProvider reads, and the two billing tables whose emptiness
	// the property asserts (BalanceLog, UsageLog). Production migrations
	// cover these from infra/; here we migrate locally to keep the test
	// hermetic.
	if err := db.AutoMigrate(
		&model.User{},
		&model.ApiKey{},
		&model.Group{},
		&model.Subscription{},
		&model.BalanceLog{},
		&model.UsageLog{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}

	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	cache := infra.NewAPIKeyCache()
	provider := NewAccessProvider(db, client, cache, testJWTSecret)

	return &inactiveUserEnv{
		db:       db,
		srv:      srv,
		client:   client,
		cache:    cache,
		provider: provider,
	}
}

// holdsZSetKeyForInactive mirrors ledger.holdsKey — the ledger keeps its
// key helper unexported, so the test reproduces the format directly. If
// the ledger ever changes its scheme, this test fails loudly rather than
// silently skipping its hold-absence assertion.
func holdsZSetKeyForInactive(userID uint) string {
	return fmt.Sprintf("cpa-gateway:billing:holds:%d", userID)
}

// assertNoBillingSideEffects confirms Requirement 4.7 for the given user:
// no BalanceLog row, no UsageLog row, and no member in holds:{userID}.
// The caller is responsible for pinning the (userID, requestID) pair to
// something unique per iteration so assertions do not alias across
// rapid iterations that share a DB.
func assertNoBillingSideEffects(rt *rapid.T, env *inactiveUserEnv, userID uint) {
	rt.Helper()

	var balanceRows int64
	if err := env.db.Model(&model.BalanceLog{}).Where("user_id = ?", userID).Count(&balanceRows).Error; err != nil {
		rt.Fatalf("count balance_logs for user %d: %v", userID, err)
	}
	if balanceRows != 0 {
		rt.Fatalf("expected 0 balance_logs rows for rejected user %d, got %d", userID, balanceRows)
	}

	var usageRows int64
	if err := env.db.Model(&model.UsageLog{}).Where("user_id = ?", userID).Count(&usageRows).Error; err != nil {
		rt.Fatalf("count usage_logs for user %d: %v", userID, err)
	}
	if usageRows != 0 {
		rt.Fatalf("expected 0 usage_logs rows for rejected user %d, got %d", userID, usageRows)
	}

	// Redis hold: the sorted set may legitimately not exist at all on
	// miniredis (ZCard on a missing key returns 0). Either "key missing"
	// or "key present but empty" satisfies the invariant; anything else
	// is a violation.
	if exists := env.srv.Exists(holdsZSetKeyForInactive(userID)); exists {
		members, _ := env.srv.ZMembers(holdsZSetKeyForInactive(userID))
		if len(members) != 0 {
			rt.Fatalf("expected zero members in holds:{%d}, got %v", userID, members)
		}
	}
}

// seedInactiveUserWithAPIKey inserts a user with the requested status and
// an associated ApiKey. It returns the user ID and the plaintext key the
// test will pass in the Authorization header. The function does NOT create
// a Group: the path under test is the user-status recheck that runs before
// any entitlement evaluation, so leaving GroupID=nil keeps the test focused.
func seedInactiveUserWithAPIKey(rt *rapid.T, db *gorm.DB, email string, status string) (uint, string) {
	rt.Helper()

	user := &model.User{
		Email:        email,
		PasswordHash: "x",
		Role:         "user",
		Status:       status,
		Balance:      100,
	}
	if err := db.Create(user).Error; err != nil {
		rt.Fatalf("create user status=%s: %v", status, err)
	}

	plaintext, err := authutil.NewAPIKey()
	if err != nil {
		rt.Fatalf("new api key: %v", err)
	}
	ak := &model.ApiKey{
		UserID:    user.ID,
		KeyHash:   authutil.HashAPIKey(plaintext),
		KeyPrefix: authutil.APIKeyPrefix(plaintext),
		Name:      "inactive-probe",
		// The ApiKey row itself is "active" — the inactive status lives
		// on the User row. Requirement 4.1/4.2 requires the second-stage
		// user-status recheck to reject even when the ApiKey is healthy.
		Status: "active",
	}
	if err := db.Create(ak).Error; err != nil {
		rt.Fatalf("create api key: %v", err)
	}
	return user.ID, plaintext
}

// issueInactiveJWT builds a signed JWT for a user. The function exists so
// the test can issue tokens for a user row that has already been flipped
// inactive — authutil.GenerateJWT does not consult the DB, so it signs
// happily regardless of the row's current Status. This mirrors the
// real-world threat model: a token issued while the user was active is
// still presented after the user has been deactivated.
func issueInactiveJWT(rt *rapid.T, userID uint, email string) string {
	rt.Helper()
	tok, err := authutil.GenerateJWT(userID, email, testJWTSecret, 1)
	if err != nil {
		rt.Fatalf("issue jwt for user=%d: %v", userID, err)
	}
	return tok
}

// -----------------------------------------------------------------------------
// property: inactive credentials are rejected uniformly
// -----------------------------------------------------------------------------

// TestInactiveUserAuthRejectsWithInvalidCredentials property-tests the
// invariant documented as Property 11: for any credential (API key or
// panel JWT) whose owning model.User.Status is NOT "active", the
// AccessProvider surface for /v1/* SHALL return an InvalidCredential
// AuthError, the rejection SHALL be observationally indistinguishable
// from a completely bogus credential (same AuthError code), AND no
// balance / usage / hold side effect SHALL occur for that user.
//
// The generator enumerates the full matrix:
//
//	status    ∈ {"deleted", "suspended", "disabled"}
//	auth_type ∈ {"apikey", "jwt"}
//
// Each iteration exercises a fresh DB + miniredis + APIKeyCache so
// state from prior iterations cannot leak into the assertion. The test
// intentionally does NOT pre-warm any cache — this forces userIsActive
// onto the DB-read path, which is the slow path production sees when a
// user is freshly suspended and no prior request has populated the
// UserStatusCache. The complementary stale-cache path is covered by
// TestStaleCacheStatusRejected below.
//
// **Validates: Property 11, Requirements 4.1, 4.2, 4.4, 4.7**
func TestInactiveUserAuthRejectsWithInvalidCredentials(t *testing.T) {
	raiseRapidChecksInactiveUser(t, 200)

	// Monotonic counter across rapid iterations so seeded emails do not
	// collide with the User's unique email index. sync.Mutex guards the
	// counter because rapid can invoke the body concurrently during
	// shrinking on some versions.
	var (
		counter int64
		mu      sync.Mutex
	)
	nextTag := func() string {
		mu.Lock()
		defer mu.Unlock()
		counter++
		return fmt.Sprintf("%d_%d", time.Now().UnixNano(), counter)
	}

	rapid.Check(t, func(rt *rapid.T) {
		// Inactive-status alphabet. All three must round-trip to the
		// same opaque invalid_credential AuthError regardless of which
		// label the admin picked — Requirement 4.4 forbids the body
		// from distinguishing "deleted" from "suspended" because doing
		// so would leak a user-status oracle.
		status := rapid.SampledFrom([]string{"deleted", "suspended", "disabled"}).Draw(rt, "status")
		// Credential shape. API key exercises the cache+DB path, JWT
		// exercises the claims-only path; both must hit the same
		// userIsActive gate.
		authType := rapid.SampledFrom([]string{"apikey", "jwt"}).Draw(rt, "auth_type")

		tag := nextTag()
		env := newInactiveUserEnv(t, tag)

		// Seed a user in the requested inactive status.
		email := fmt.Sprintf("inactive-%s-%s@test.local", status, tag)
		userID, plaintext := seedInactiveUserWithAPIKey(rt, env.db, email, status)

		// Build the Authorization header matching the chosen credential
		// shape. The JWT is signed with the same secret the provider
		// was constructed with — it is cryptographically valid; the
		// only reason to reject is the user's non-active status.
		req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		var bearer string
		switch authType {
		case "apikey":
			bearer = "Bearer " + plaintext
		case "jwt":
			bearer = "Bearer " + issueInactiveJWT(rt, userID, email)
		default:
			rt.Fatalf("unexpected auth_type %q", authType)
		}
		req.Header.Set("Authorization", bearer)

		result, authErr := env.provider.Authenticate(context.Background(), req)

		// --- Assertion 1: uniform invalid_credential rejection --------
		if result != nil {
			rt.Fatalf("expected nil result for inactive user (status=%s, auth_type=%s, user=%d), got %+v",
				status, authType, userID, result)
		}
		if authErr == nil {
			rt.Fatalf("expected AuthError for inactive user (status=%s, auth_type=%s, user=%d)", status, authType, userID)
		}
		if authErr.Code != sdkaccess.AuthErrorCodeInvalidCredential {
			rt.Fatalf("AuthError.Code = %q, want %q (status=%s, auth_type=%s, user=%d)",
				authErr.Code, sdkaccess.AuthErrorCodeInvalidCredential, status, authType, userID)
		}
		// Requirement 4.4 also specifies HTTP 401. NewInvalidCredentialError
		// wires StatusCode=401 in the SDK; we assert so a downstream SDK
		// change that drops the status would be caught here rather than
		// leaking through to production as a different public error surface.
		if authErr.StatusCode != 401 {
			rt.Fatalf("AuthError.StatusCode = %d, want 401 (status=%s, auth_type=%s)",
				authErr.StatusCode, status, authType)
		}

		// --- Assertion 2: no billing side effects for the user -------
		// The API-key path passes through ApiKey -> userIsActive; the
		// JWT path passes through ValidateJWT -> userIsActive. Neither
		// path should touch balance_logs, usage_logs, or the Redis
		// holds sorted set — Requirement 4.7's "before any Hold,
		// UsageRecord, UsageLog, or BalanceLog row is written" clause.
		assertNoBillingSideEffects(rt, env, userID)
	})
}

// -----------------------------------------------------------------------------
// property: stale APIKeyCache entry is caught by userIsActive
// -----------------------------------------------------------------------------

// TestStaleCacheStatusRejected property-tests the coherence guarantee
// that Property 11 / Requirement 4.6 make: even when the APIKeyCache
// still carries Status="active" from a prior successful authentication,
// a user row that has since flipped to an inactive status MUST be
// rejected by the very next Authenticate call. userIsActive is the
// gate that makes this possible — it re-reads the users table (via
// UserStatusCache on the hot path, via DB on a miss) on every request.
//
// Test sequence, per iteration:
//
//  1. Seed an active user + active API key.
//  2. Call Authenticate(apiKey) once. Expect success. This warms the
//     APIKeyCache entry keyed on the key hash with Status="active".
//  3. Mutate the user row directly to the target inactive status.
//     Critically, we do NOT invalidate any cache — we are simulating
//     the worst case where an admin transition bypassed the
//     invalidateUserCaches hook for any reason (e.g., direct DB
//     surgery during an incident).
//  4. Re-read the APIKeyCache to confirm it still reports "active" —
//     this is the stale condition the test is pinning.
//  5. Re-call Authenticate(apiKey). Expect the same invalid_credential
//     AuthError as TestInactiveUserAuthRejectsWithInvalidCredentials.
//
// Because the AccessProvider in this test is constructed without a
// UserStatusCache (UserStatusCache is nil), userIsActive falls through
// to the DB read, observes the flipped status, and correctly rejects —
// which is exactly the behaviour Requirement 4.6 requires.
//
// **Validates: Property 11, Requirements 4.1, 4.6, 4.7**
func TestStaleCacheStatusRejected(t *testing.T) {
	raiseRapidChecksInactiveUser(t, 200)

	var (
		counter int64
		mu      sync.Mutex
	)
	nextTag := func() string {
		mu.Lock()
		defer mu.Unlock()
		counter++
		return fmt.Sprintf("stale_%d_%d", time.Now().UnixNano(), counter)
	}

	rapid.Check(t, func(rt *rapid.T) {
		status := rapid.SampledFrom([]string{"deleted", "suspended", "disabled"}).Draw(rt, "flipped_status")

		tag := nextTag()
		env := newInactiveUserEnv(t, tag)

		email := fmt.Sprintf("stale-active-%s@test.local", tag)

		// Step 1 + 2: seed ACTIVE user + active API key, warm the L1
		// cache via a legitimate Authenticate call.
		userID, plaintext := seedInactiveUserWithAPIKey(rt, env.db, email, "active")
		keyHash := authutil.HashAPIKey(plaintext)

		req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer "+plaintext)

		if result, authErr := env.provider.Authenticate(context.Background(), req); authErr != nil || result == nil {
			rt.Fatalf("warm-up Authenticate failed: result=%+v err=%+v", result, authErr)
		}

		// Confirm the cache did warm. A missing cache entry here means
		// the provider was constructed without an APIKeyCache or the
		// cache TTL is already 0 — either is a test-setup bug, not a
		// production invariant failure, so we bail early rather than
		// silently pass a vacuous property.
		warm, ok := env.cache.Get(keyHash)
		if !ok {
			rt.Fatalf("APIKeyCache did not warm after successful authentication; cache setup is broken")
		}
		if warm.Status != "active" {
			rt.Fatalf("APIKeyCache entry status = %q, want %q after active-user auth", warm.Status, "active")
		}

		// Step 3: flip user row to inactive directly. Using a raw
		// Update (not an admin handler) deliberately bypasses the
		// production invalidateUserCaches hook so the APIKeyCache
		// entry remains stale — precisely the race condition
		// Requirement 4.6 protects against.
		if err := env.db.Model(&model.User{}).Where("id = ?", userID).Update("status", status).Error; err != nil {
			rt.Fatalf("flip user status to %q: %v", status, err)
		}

		// Step 4: verify staleness BEFORE the rejection check. If this
		// ever fails, the test is no longer exercising the stale-cache
		// path it advertises.
		stale, ok := env.cache.Get(keyHash)
		if !ok {
			rt.Fatalf("APIKeyCache entry disappeared between warm-up and re-auth (tag=%s)", tag)
		}
		if stale.Status != "active" {
			rt.Fatalf("APIKeyCache entry no longer stale: Status=%q (expected still %q)", stale.Status, "active")
		}

		// Step 5: re-authenticate. userIsActive should consult the DB
		// (UserStatusCache is nil on env.provider, so there is no
		// in-memory shortcut) and observe the flipped Status — the
		// rejection must be indistinguishable from the fresh-DB
		// inactive-user path.
		reReq := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		reReq.Header.Set("Authorization", "Bearer "+plaintext)

		result, authErr := env.provider.Authenticate(context.Background(), reReq)
		if result != nil {
			rt.Fatalf("expected nil result after status flip (status=%s, user=%d), got %+v",
				status, userID, result)
		}
		if authErr == nil {
			rt.Fatalf("expected AuthError after status flip (status=%s, user=%d)", status, userID)
		}
		if authErr.Code != sdkaccess.AuthErrorCodeInvalidCredential {
			rt.Fatalf("AuthError.Code = %q, want %q after status flip (status=%s, user=%d)",
				authErr.Code, sdkaccess.AuthErrorCodeInvalidCredential, status, userID)
		}

		// The stale-cache rejection must also produce no billing side
		// effects. The warm-up authenticate above did not touch these
		// tables either, so a non-zero count here would be a true
		// regression rather than an artifact of the warm-up.
		assertNoBillingSideEffects(rt, env, userID)
	})
}

// -----------------------------------------------------------------------------
// end
// -----------------------------------------------------------------------------
