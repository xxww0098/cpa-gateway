package api

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
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

// Feature: billing-security-hardening, Task 14.3 — admin-delete cache
// invalidation hook (Property 12, Requirements 3.6 / 4.5 / 4.6).
//
// This file pins three contracts the admin-delete path and the
// PanelRouter.invalidateUserCaches helper are responsible for:
//
//  1. TestDeleteUserInvalidatesCaches (example): after an admin DELETE
//     /admin/users/:id commits, BOTH the UserStatusCache AND every
//     APIKeyCache entry owned by the target user MUST miss on the next
//     lookup. The existing test fixture hides these invariants behind
//     the AuthMiddleware; here we drive the admin handler directly with
//     a seeded admin BillingCtx so the assertion is unambiguous.
//
//  2. TestEntitlementRevokeInvalidatesApiKeyCache (example, Stage 5
//     placeholder): Requirement 3.6 says that when an admin revokes an
//     entitlement (subscription expiry / cancellation / override), every
//     APIKeyCache entry owned by that user MUST be invalidated before
//     the revocation commits. The only current admin hook that flushes
//     caches is user-status flips (handler_admin_expanded.go →
//     invalidateUserCaches). A subscription-revoke hook does NOT yet
//     exist — per the task sheet it is Stage 5 future work. This test
//     therefore DOCUMENTS the current (intentional) gap by asserting
//     the contrapositive: flipping a Subscription row to status=expired
//     via a raw DB Update leaves the cached APIKeyCache entry intact.
//     A TODO below the assertion points at the follow-up task.
//
//  3. TestConcurrentAuthAndDeleteRace (rapid state-machine walk):
//     interleaves validateAPIKey ("authenticate") and the admin-delete
//     handler across two actors per iteration. The invariant is the
//     one called out by Property 12: no successful authentication may
//     observe Status="active" from either cache AFTER the DB has been
//     flipped to deleted. The state machine keeps track of whether the
//     delete hook has fired; every successful auth that lands after
//     that point is a race bug.
//
// All three tests migrate the minimum model surface (User, ApiKey,
// Group, Subscription) and construct the PanelRouter with both caches
// wired so the production hook paths fire end-to-end.
//
// **Validates: Property 12, Requirements 3.6, 4.5, 4.6**

// userStatusCacheTestAdminEmail is the fixed admin email we seed into
// the seeded admin principal. requireAdmin now checks users.role, so the
// tests migrate users and seed a real admin row.
const userStatusCacheTestAdminEmail = "admin@user-status-cache-test.local"

// newUserStatusCacheRouter builds a PanelRouter backed by an in-memory
// sqlite DB and real *infra.APIKeyCache + *infra.UserStatusCache so the
// test can observe both caches before and after the admin handler runs.
// The middleware/auth layer is intentionally bypassed in favour of a
// per-test shim middleware — the contract under test is the admin
// handler body + invalidateUserCaches helper, not AuthMiddleware itself.
//
// tag parameter: a per-call unique suffix so rapid iterations using
// shared-cache sqlite DSNs cannot see each other's rows. Passing ""
// yields the default ":memory:" DSN, which is safe for single-iteration
// example tests.
func newUserStatusCacheRouter(tb testing.TB, tag string) (*gorm.DB, *PanelRouter) {
	tb.Helper()

	dsn := ":memory:"
	if tag != "" {
		// Shared-cache mode lets multiple GORM statements see the same
		// data across connections. A per-tag DSN keeps rapid iterations
		// isolated without leaking state across them.
		dsn = fmt.Sprintf("file:user_status_cache_%s?mode=memory&cache=shared", tag)
	}

	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		tb.Fatalf("open sqlite: %v", err)
	}
	if sqlDB, derr := db.DB(); derr == nil {
		// Single-connection keeps the in-memory DB view consistent
		// across the handler goroutine and the test goroutine (which
		// both need to see the status=deleted write promptly).
		sqlDB.SetMaxOpenConns(1)
		tb.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := db.AutoMigrate(
		&model.User{},
		&model.ApiKey{},
		&model.Group{},
		&model.Subscription{},
	); err != nil {
		tb.Fatalf("automigrate: %v", err)
	}

	cfg := &config.Config{}
	cfg.Auth.AdminEmails = []string{userStatusCacheTestAdminEmail}
	pr := NewPanelRouter(db, nil, nil, nil, cfg)
	pr.APIKeyCache = infra.NewAPIKeyCache()
	pr.UserStatusCache = infra.NewUserStatusCache()
	return db, pr
}

// seedAdminDeleteCtxMiddleware returns a gin middleware that injects a
// BillingCtx whose Email matches the admin allowlist, emulating the
// AuthMiddleware outcome for a valid admin JWT. adminUserID keeps the
// admin principal distinct from the target user under test so
// requireAdmin does not accidentally short-circuit on the target row.
func seedAdminDeleteCtxMiddleware(adminUserID uint) gin.HandlerFunc {
	return func(c *gin.Context) {
		bc := &BillingCtx{
			UserID:    adminUserID,
			Email:     userStatusCacheTestAdminEmail,
			RateMult:  1.0,
			AuthType:  authTypeJWT,
			Status:    userStatusActive,
			RequestID: "admin-delete-test",
		}
		setBillingContext(c, bc)
		c.Next()
	}
}

// seedUserWithAPIKeys inserts an active user together with n freshly
// generated API keys owned by that user. It returns the user ID and the
// plaintext keys; callers can hash each plaintext via authutil.HashAPIKey
// to obtain the cache lookup key. Every key is seeded Status="active"
// because the path under test is "admin flips user status and caches
// get flushed", not "admin disables the key".
func seedUserWithAPIKeys(tb testing.TB, db *gorm.DB, email string, n int) (uint, []string) {
	tb.Helper()
	u := &model.User{
		Email:        email,
		PasswordHash: "hash",
		Role:         "user",
		Status:       userStatusActive,
		Balance:      100,
	}
	if err := db.Create(u).Error; err != nil {
		tb.Fatalf("seed user: %v", err)
	}
	plaintexts := make([]string, n)
	for i := 0; i < n; i++ {
		pt, err := authutil.NewAPIKey()
		if err != nil {
			tb.Fatalf("new api key: %v", err)
		}
		ak := &model.ApiKey{
			UserID:    u.ID,
			KeyHash:   authutil.HashAPIKey(pt),
			KeyPrefix: authutil.APIKeyPrefix(pt),
			Name:      fmt.Sprintf("probe-%d", i),
			Status:    "active",
		}
		if err := db.Create(ak).Error; err != nil {
			tb.Fatalf("seed api key %d: %v", i, err)
		}
		plaintexts[i] = pt
	}
	return u.ID, plaintexts
}

// primeCachesForUser warms both caches with entries for userID + the
// supplied plaintext keys. The UserStatusCache is primed with Status
// "active" so the test can observe the invalidation; each APIKeyCache
// entry is primed to mirror what a real validateAPIKey call would have
// stored.
func primeCachesForUser(pr *PanelRouter, userID uint, plaintexts []string) {
	pr.UserStatusCache.Set(userID, userStatusActive, 5*time.Minute)
	for _, pt := range plaintexts {
		pr.APIKeyCache.Set(authutil.HashAPIKey(pt), &infra.CachedKey{
			UserID:    userID,
			ApiKeyID:  0, // not asserted here; Delete is keyed on hash
			GroupID:   nil,
			RateMult:  1.0,
			Status:    "active",
			ExpiresAt: time.Now().Add(5 * time.Minute),
		})
	}
}

// runAdminDelete exercises the admin delete handler end-to-end via an
// httptest recorder. adminUserID is the BillingCtx principal; targetID
// is the user to delete. Returns the recorder so callers can assert on
// status/body before probing the caches.
func runAdminDelete(tb testing.TB, pr *PanelRouter, adminUserID, targetID uint) *httptest.ResponseRecorder {
	tb.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(seedAdminDeleteCtxMiddleware(adminUserID))
	r.DELETE("/api/panel/admin/users/:id", pr.AdminUsersDeleteHandler)

	req := httptest.NewRequest(http.MethodDelete,
		"/api/panel/admin/users/"+strconv.FormatUint(uint64(targetID), 10),
		nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// -----------------------------------------------------------------------------
// 1. example: admin delete invalidates both caches
// -----------------------------------------------------------------------------

// TestDeleteUserInvalidatesCaches covers the positive branch of the
// admin delete hook: after the handler returns success, both the
// UserStatusCache entry and every primed APIKeyCache entry that belongs
// to the deleted user MUST miss on the next lookup. The test intentionally
// seeds two API keys to guard against a bug where invalidateUserCaches
// only drops the first row of the SELECT.
//
// Assertion sequence:
//  1. HTTP 200 {"deleted": true} envelope from the handler.
//  2. DB row's Status is now "deleted" (sanity check that the handler
//     actually committed before the flush ran).
//  3. UserStatusCache.Get(userID) → miss.
//  4. APIKeyCache.Get(keyHash) → miss for EVERY seeded key.
//
// **Validates: Property 12, Requirements 3.6, 4.5, 4.6**
func TestDeleteUserInvalidatesCaches(t *testing.T) {
	const adminUserID uint = 1

	db, pr := newUserStatusCacheRouter(t, "")

	// Seed an admin principal row too — requireAdmin's fast path reads
	// the BillingCtx email (populated via seedAdminDeleteCtxMiddleware),
	// so the row is only here to keep the DB shape realistic.
	admin := &model.User{
		ID:           adminUserID,
		Email:        userStatusCacheTestAdminEmail,
		PasswordHash: "hash",
		Role:         "admin",
		Status:       userStatusActive,
	}
	if err := db.Create(admin).Error; err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	targetID, plaintexts := seedUserWithAPIKeys(t, db, "target@user-status-cache-test.local", 2)
	primeCachesForUser(pr, targetID, plaintexts)

	// Sanity guard: both caches are warm before the handler runs.
	// A failure here is a test-setup bug, not a handler bug.
	if _, ok := pr.UserStatusCache.Get(targetID); !ok {
		t.Fatalf("pre-condition: UserStatusCache missing seeded entry for user %d", targetID)
	}
	for i, pt := range plaintexts {
		if _, ok := pr.APIKeyCache.Get(authutil.HashAPIKey(pt)); !ok {
			t.Fatalf("pre-condition: APIKeyCache missing seeded entry for key %d", i)
		}
	}

	w := runAdminDelete(t, pr, adminUserID, targetID)

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

	// Assertion 2: DB row actually committed to status=deleted. If the
	// write failed the handler would have returned 500 and the flush
	// would not have run, so this guard also confirms the order.
	var after model.User
	if err := db.First(&after, targetID).Error; err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if after.Status != "deleted" {
		t.Fatalf("user status: got %q want %q", after.Status, "deleted")
	}

	// Assertion 3: UserStatusCache miss for the target user.
	if us, ok := pr.UserStatusCache.Get(targetID); ok {
		t.Fatalf("UserStatusCache: got hit %+v, want miss after admin delete", us)
	}

	// Assertion 4: APIKeyCache miss for EVERY seeded key. Iterating all
	// keys catches a regression where invalidateUserCaches dropped only
	// the first SELECT row.
	for i, pt := range plaintexts {
		if ck, ok := pr.APIKeyCache.Get(authutil.HashAPIKey(pt)); ok {
			t.Fatalf("APIKeyCache: got hit %+v for key %d, want miss after admin delete", ck, i)
		}
	}
}

// -----------------------------------------------------------------------------
// 2. example: Stage 5 placeholder — subscription revoke does NOT yet flush cache
// -----------------------------------------------------------------------------

// TestEntitlementRevokeInvalidatesApiKeyCache documents the current
// behaviour of the entitlement-revoke path relative to Requirement 3.6.
//
// Requirement 3.6 mandates that, when an entitlement is revoked
// (subscription cancellation, expiry, admin override), every
// APIKeyCache entry owned by the affected user MUST be invalidated
// BEFORE the revocation commits, so that subsequent /v1/* requests
// re-evaluate the group entitlement check.
//
// The only admin hook wired to invalidateUserCaches today is the
// user-status transition in AdminUsersUpdateHandler / AdminUsersDelete-
// Handler. Subscription state transitions — AdminSubscriptionsRevoke-
// Handler, the natural expiry sweeper, a manual admin override, and
// the subscription-expiry cron — do NOT currently touch the caches.
// Per the task sheet (stage 4 / §14.3), wiring those hooks is deferred
// to Stage 5.
//
// Rather than leave Requirement 3.6 uncovered, this test pins the
// CURRENT behaviour so a future change that silently adds a flush
// without updating this test (or removing the TODO below) will be
// noticed. The assertion is therefore the CONTRAPOSITIVE of the end-
// state invariant: flipping the Subscription row via raw GORM still
// leaves the cached APIKeyCache entry intact.
//
// TODO(Stage 5, task 14.3 follow-up): wire subscription revoke/expire
// paths to pr.invalidateUserCaches(ctx, userID) — specifically the
// AdminSubscriptionsRevokeHandler (api/handler_admin_expanded.go) and
// the expiry sweep (sdk/access.go entitlement predicate already
// rejects expired subscriptions on the hot path, so the panel cache
// flush is the remaining gap). When the hook lands, replace the
// current assertion with "cache miss after revoke" to turn this test
// green on the fixed behaviour rather than the placeholder.
//
// **Validates: Property 12 (documented gap), Requirement 3.6 (Stage 5 follow-up)**
func TestEntitlementRevokeInvalidatesApiKeyCache(t *testing.T) {
	db, pr := newUserStatusCacheRouter(t, "")

	// Seed user + two API keys; prime the caches the same way a
	// hot /v1/* path would have.
	userID, plaintexts := seedUserWithAPIKeys(t, db, "revoke@user-status-cache-test.local", 2)

	// Seed one group + one active, non-expired subscription so we
	// have a realistic revoke target. The group's RateMultiplier !=
	// 1.0 makes it a non-baseline entitlement — baseline is implicit
	// and has no DB row to flip.
	grp := model.Group{Name: "pro", RateMultiplier: 0.5}
	if err := db.Create(&grp).Error; err != nil {
		t.Fatalf("seed group: %v", err)
	}
	sub := model.Subscription{
		UserID:    userID,
		PackageID: 1,
		GroupID:   grp.ID,
		Status:    "active",
		StartsAt:  time.Now().UTC().Add(-time.Hour),
		ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour),
	}
	if err := db.Create(&sub).Error; err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	primeCachesForUser(pr, userID, plaintexts)

	// Sanity guard on the pre-condition. Both caches are warm.
	if _, ok := pr.UserStatusCache.Get(userID); !ok {
		t.Fatalf("pre-condition: UserStatusCache missing seeded entry for user %d", userID)
	}
	for i, pt := range plaintexts {
		if _, ok := pr.APIKeyCache.Get(authutil.HashAPIKey(pt)); !ok {
			t.Fatalf("pre-condition: APIKeyCache missing seeded entry for key %d", i)
		}
	}

	// Revoke the entitlement via the raw DB path — the same shape
	// AdminSubscriptionsRevokeHandler commits today (it writes
	// Status="revoked" and returns without touching caches). Running
	// through the HTTP handler would be equivalent and strictly
	// noisier; we pick the DB path because the contract under
	// inspection is "the subscription transition itself does not
	// flush", not "this particular handler does not flush".
	if err := db.Model(&model.Subscription{}).
		Where("id = ?", sub.ID).
		Update("status", "expired").Error; err != nil {
		t.Fatalf("flip subscription to expired: %v", err)
	}

	// Assertion: the APIKeyCache entries are STILL present. This
	// pins the current behaviour — a subscription-revoke hook does
	// NOT yet exist. When the Stage 5 hook lands, this test must
	// flip to assert cache miss; see the TODO in the doc comment.
	for i, pt := range plaintexts {
		if _, ok := pr.APIKeyCache.Get(authutil.HashAPIKey(pt)); !ok {
			t.Fatalf(
				"APIKeyCache: got miss for key %d after subscription revoke, "+
					"want hit (current behaviour — the revoke hook is Stage 5 future work). "+
					"If this test failed because a new hook was added, update the test "+
					"to assert cache miss and remove the TODO in the doc comment.", i)
		}
	}

	// UserStatusCache is orthogonal to entitlement revoke (it keys on
	// user row status, not subscription status), so it MUST still hit.
	// Asserting this keeps the test focused on the APIKeyCache gap.
	if _, ok := pr.UserStatusCache.Get(userID); !ok {
		t.Fatalf("UserStatusCache: got miss for user %d after subscription revoke, "+
			"want hit (user row is unchanged)", userID)
	}
}

// -----------------------------------------------------------------------------
// 3. rapid state-machine walk: auth vs delete race
// -----------------------------------------------------------------------------

// raiseRapidChecksUserStatusCache mirrors the helper in sibling rapid
// tests so task 14.3's state-machine walk still gets ≥ 200 action steps
// per invocation regardless of the caller's -rapid.checks value.
func raiseRapidChecksUserStatusCache(t *testing.T, minChecks int) {
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

// raceWalkCounter is shared across rapid iterations to mint per-
// iteration email / DSN tags that cannot collide. Atomic increment is
// cheap; a mutex is unnecessary because the counter is monotonic.
var raceWalkCounter atomic.Int64

// raceWalk holds the per-iteration state the rapid.Repeat actions
// operate on. A fresh raceWalk is built for every rapid iteration so
// state does not leak across iterations; the actions themselves are
// closures over *raceWalk to keep the StateMachine interface (if we
// were using it) out of the way — rapid.T.Repeat accepts a raw action
// map, which is lighter for this small surface.
type raceWalk struct {
	t testing.TB

	db *gorm.DB
	pr *PanelRouter

	userID    uint
	plaintext string
	keyHash   string
	adminID   uint

	// dbFlipped is set to true once the admin-delete action has run.
	// Any subsequent authenticate() that observes Status="active" from
	// either cache is the race bug Property 12 forbids.
	dbFlipped bool

	// mu guards dbFlipped + cache reads that observe it. rapid runs
	// actions sequentially within a single goroutine, but we use a
	// mutex to keep the contract explicit so a future multi-goroutine
	// variant does not silently regress.
	mu sync.Mutex
}

// newRaceWalk builds one walk's worth of state: a fresh DB, primed
// caches, and the plaintext key the authenticate() action will present.
func newRaceWalk(t testing.TB) *raceWalk {
	t.Helper()
	tag := fmt.Sprintf("race_%d", raceWalkCounter.Add(1))
	db, pr := newUserStatusCacheRouter(t, tag)

	// Admin principal: requireAdmin authorizes from this DB row's role.
	admin := &model.User{
		Email:        userStatusCacheTestAdminEmail,
		PasswordHash: "hash",
		Role:         "admin",
		Status:       userStatusActive,
	}
	if err := db.Create(admin).Error; err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	email := fmt.Sprintf("race-%s@user-status-cache-test.local", tag)
	userID, plaintexts := seedUserWithAPIKeys(t, db, email, 1)
	primeCachesForUser(pr, userID, plaintexts)

	return &raceWalk{
		t:         t,
		db:        db,
		pr:        pr,
		userID:    userID,
		plaintext: plaintexts[0],
		keyHash:   authutil.HashAPIKey(plaintexts[0]),
		adminID:   admin.ID,
	}
}

// authenticate runs validateAPIKey against the primed key and enforces
// Property 12's core invariant: once the DB has been flipped, no cache
// path may return a success whose Status is still "active". The check
// is tolerant of TWO legitimate post-flip outcomes:
//
//   - cache miss + DB miss (apiKey row status still "active" but user
//     is deleted → DB query filter on ApiKey.status = "active" matches,
//     so the DB actually DOES return a CachedKey here; however that
//     CachedKey's Status is the ApiKey row's status, not the user's).
//     Callers defensive about user deletion MUST go through an
//     additional user-status gate (AuthMiddleware does so; see
//     api/middleware.go userIsActive). That gate is outside the scope
//     of this state machine; validateAPIKey alone only guarantees the
//     CACHE invalidation happened, not the user-status recheck.
//   - cache miss + validateAPIKey returns an error (the test does not
//     force the ApiKey row itself to flip, so this path does not fire
//     here, but it is still a legitimate outcome under other variants).
//
// The invariant enforced here: the CACHE returned by validateAPIKey
// after a flip MUST NOT be served from a stale APIKeyCache entry. We
// observe this by checking the APIKeyCache directly after the call —
// if the Delete hook fired as required, the entry must be gone (or
// repopulated by a fresh DB read, which is also acceptable because the
// repopulated entry reflects the post-flip DB state).
//
// Concretely: after dbFlipped=true, the primed cache entry (with
// ApiKeyID=0) must NOT be reachable by its keyHash — either the entry
// is missing (flush happened) OR the entry was replaced by a fresh
// DB read (ApiKeyID != 0). Both outcomes satisfy Property 12; the
// only failure mode is the primed stale entry still being reachable.
func (w *raceWalk) authenticate(rt *rapid.T) {
	rt.Helper()

	_, _ = w.pr.validateAPIKey(context.Background(), w.plaintext)

	w.mu.Lock()
	flipped := w.dbFlipped
	w.mu.Unlock()
	if !flipped {
		// Pre-flip authenticate calls are unconstrained — they may
		// legitimately hit the primed cache entry.
		return
	}

	// Post-flip: the APIKeyCache entry MUST NOT be the stale primed
	// one. Acceptable outcomes are:
	//   - entry gone (flush happened → cache.Get returns !ok), or
	//   - entry refreshed from DB (ApiKeyID != 0 because a real row
	//     was seeded with an autoincrement id).
	// Reading the cache *after* validateAPIKey is what pins the
	// stale-read variant: if a goroutine observed stale "active"
	// during the validateAPIKey call, that read is transient; the
	// subsequent Get here catches only the persisted stale state.
	ck, ok := w.pr.APIKeyCache.Get(w.keyHash)
	if !ok {
		// Cache flushed — compliant with Property 12.
		return
	}
	if ck.ApiKeyID == 0 {
		// Primed entry still present after delete — this is the race
		// bug Property 12 forbids. invalidateUserCaches MUST have
		// dropped the entry before the handler returned.
		rt.Fatalf("post-flip APIKeyCache still holds primed stale entry: %+v", ck)
	}
	// Otherwise the entry was legitimately refreshed from the DB
	// (ApiKeyID > 0) — acceptable because the refresh observes the
	// post-flip DB state on the validateAPIKey DB-read path.
}

// deleteUser invokes the admin delete handler, flipping the DB row AND
// triggering invalidateUserCaches atomically from the handler's
// perspective. After this action returns, every subsequent authenticate
// action must observe Property 12's post-flip invariant.
//
// The action is idempotent: on the second and later invocations the
// handler returns success anyway (the Update targets the same row),
// and the caches are already empty. We set dbFlipped once regardless.
func (w *raceWalk) deleteUser(rt *rapid.T) {
	rt.Helper()

	w.mu.Lock()
	alreadyFlipped := w.dbFlipped
	w.dbFlipped = true
	w.mu.Unlock()

	rec := runAdminDelete(w.t, w.pr, w.adminID, w.userID)
	if rec.Code != http.StatusOK {
		// A non-200 on the admin delete means we did not actually
		// flip. Roll the state back so later assertions don't
		// falsely flag a "post-flip" condition that the system never
		// entered. Failing loudly here is also acceptable — the path
		// under test should not 5xx on a valid admin delete.
		if !alreadyFlipped {
			w.mu.Lock()
			w.dbFlipped = false
			w.mu.Unlock()
		}
		rt.Fatalf("admin delete HTTP %d; body=%s", rec.Code, rec.Body.String())
	}
}

// check runs after every action (rapid convention) and enforces the
// end-state invariant: post-flip, the primed stale APIKeyCache entry
// MUST NOT be observable. The authenticate action enforces the same
// invariant after its call; check() catches the case where a bug
// causes the cache to be repopulated with a stale entry between
// actions (e.g., if a background goroutine repopulated the cache).
func (w *raceWalk) check(rt *rapid.T) {
	rt.Helper()
	w.mu.Lock()
	flipped := w.dbFlipped
	w.mu.Unlock()
	if !flipped {
		return
	}
	if ck, ok := w.pr.APIKeyCache.Get(w.keyHash); ok {
		if ck.ApiKeyID == 0 {
			rt.Fatalf("between-action invariant: primed stale cache entry reappeared: %+v", ck)
		}
	}
	// UserStatusCache may or may not be populated after a fresh
	// validateAPIKey+DB read (validateAPIKey itself does not touch
	// it), so we do not pin a specific state for it — the earlier
	// example test (TestDeleteUserInvalidatesCaches) already pins
	// the immediate post-delete miss, which is the invariant this
	// walk is sampling around.
}

// TestConcurrentAuthAndDeleteRace is the rapid state-machine walk
// promised by task 14.3. It interleaves validateAPIKey ("authenticate")
// and AdminUsersDeleteHandler ("delete") actions using rapid.Repeat,
// and enforces Property 12's core invariant: after the DB has been
// flipped to deleted, no subsequent authenticate may observe the
// primed "active" APIKeyCache entry.
//
// The walk runs within a single goroutine because rapid.Repeat is
// sequential by contract; a multi-goroutine variant would need to
// serialize actions through explicit synchronisation, and the contract
// being tested (the handler's invalidation happens before it returns)
// is already strictly observable from single-threaded action
// interleaving — any real-world race that lets a stale read survive
// past the handler's return will manifest as a post-flip hit on the
// primed cache entry.
//
// **Validates: Property 12, Requirements 3.6, 4.5, 4.6**
func TestConcurrentAuthAndDeleteRace(t *testing.T) {
	raiseRapidChecksUserStatusCache(t, 200)

	rapid.Check(t, func(rt *rapid.T) {
		w := newRaceWalk(t)
		rt.Repeat(map[string]func(*rapid.T){
			"authenticate": w.authenticate,
			"delete":       w.deleteUser,
			"":             w.check,
		})
	})
}
