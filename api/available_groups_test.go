package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xxww0098/cpa-gateway/config"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"pgregory.net/rapid"
)

// newAvailableGroupsRouter builds a PanelRouter backed by an in-memory
// sqlite DB for the AvailableGroupsHandler property test. It migrates the
// tables the handler actually touches (User, Group, Subscription) plus
// model.User so that requireBillingCtx does not trip if a future change
// adds a user-row read. Mirrors the shim pattern used by
// apikey_rebind_entitlement_test.go / purchase_debt_block_test.go — the
// auth layer is bypassed because the property under test lives in the
// handler body (entitlement filter), not in credential validation.
func newAvailableGroupsRouter(tb testing.TB) (*PanelRouter, *gorm.DB) {
	tb.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		tb.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.User{},
		&model.Group{},
		&model.Subscription{},
	); err != nil {
		tb.Fatalf("automigrate: %v", err)
	}

	// Config with no AdminEmails so isAdminCaller always returns false
	// (non-admin caller is the path this property exercises).
	cfg := &config.Config{}
	pr := NewPanelRouter(db, nil, nil, nil, cfg)
	return pr, db
}

// seedAvailableGroupsUser inserts a plain non-admin Active user under the
// supplied userID. The handler does not actually load the user row
// (requireBillingCtx trusts the BillingCtx shim) but seeding one keeps
// the DB state honest and defends the test against a future
// refactor that tightens isAdminCaller to consult users.Role.
func seedAvailableGroupsUser(tb testing.TB, db *gorm.DB, userID uint) {
	tb.Helper()
	u := &model.User{
		ID:           userID,
		Email:        fmt.Sprintf("available-groups-%d@test.local", userID),
		PasswordHash: "hash",
		Role:         "user",
		Status:       userStatusActive,
	}
	if err := db.Create(u).Error; err != nil {
		tb.Fatalf("seed user: %v", err)
	}
}

// seedAvailableGroupsCatalog writes one baseline group (RateMultiplier=1.0)
// and nonBaselineCount non-baseline groups (RateMultiplier=0.5). The
// returned slice of non-baseline IDs lets the rapid generator pick which
// groups the acting user subscribes to. Uses successive Create calls (not
// batch insert) so GORM assigns deterministic autoincrement IDs starting
// at 1; the test never hard-codes the IDs, it reads them back.
func seedAvailableGroupsCatalog(tb testing.TB, db *gorm.DB, nonBaselineCount int) (baselineID uint, nonBaselineIDs []uint) {
	tb.Helper()
	baseline := model.Group{Name: "default", RateMultiplier: 1.0}
	if err := db.Create(&baseline).Error; err != nil {
		tb.Fatalf("seed baseline group: %v", err)
	}
	nonBaselineIDs = make([]uint, 0, nonBaselineCount)
	for i := 0; i < nonBaselineCount; i++ {
		g := model.Group{
			Name:           fmt.Sprintf("tier-%d", i),
			RateMultiplier: 0.5,
		}
		if err := db.Create(&g).Error; err != nil {
			tb.Fatalf("seed tier-%d group: %v", i, err)
		}
		nonBaselineIDs = append(nonBaselineIDs, g.ID)
	}
	return baseline.ID, nonBaselineIDs
}

// subscribeUserToGroups installs an active, unexpired Subscription row
// for each groupID passed in. Mirrors seedActiveSubscription from
// apikey_rebind_entitlement_test.go but pulls multiple rows in one call
// so the property generator can drive arbitrary subscription-set shapes.
func subscribeUserToGroups(tb testing.TB, db *gorm.DB, userID uint, groupIDs []uint) {
	tb.Helper()
	now := time.Now().UTC()
	for _, gid := range groupIDs {
		sub := model.Subscription{
			UserID:    userID,
			PackageID: 1,
			GroupID:   gid,
			Status:    "active",
			StartsAt:  now.Add(-time.Hour),
			ExpiresAt: now.Add(30 * 24 * time.Hour),
		}
		if err := db.Create(&sub).Error; err != nil {
			tb.Fatalf("seed subscription for group %d: %v", gid, err)
		}
	}
}

// callAvailableGroups drives the handler end-to-end via a gin test
// recorder and returns the parsed group IDs. Using an engine + shim
// middleware (rather than invoking the handler directly) keeps the test
// aligned with the production invocation path: the middleware injects
// a BillingCtx into both the gin and request contexts, then the handler
// reads it back through requireBillingCtx.
func callAvailableGroups(tb testing.TB, pr *PanelRouter, userID uint) []uint {
	tb.Helper()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(seedBillingCtxMiddleware(userID))
	r.GET("/api/panel/user/available-groups", pr.AvailableGroupsHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/panel/user/available-groups", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		tb.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Code    int                  `json:"code"`
		Message string               `json:"message"`
		Data    []availableGroupItem `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		tb.Fatalf("parse body: %v; raw=%s", err, w.Body.String())
	}
	if resp.Code != 0 {
		tb.Fatalf("response code: got %d want 0; body=%s", resp.Code, w.Body.String())
	}
	ids := make([]uint, 0, len(resp.Data))
	for _, g := range resp.Data {
		ids = append(ids, g.ID)
	}
	return ids
}

// sortedUintSet returns a fresh sorted copy with duplicates removed. Used
// so the property assertion compares IDs as order-independent sets
// without mutating the original slices.
func sortedUintSet(in []uint) []uint {
	seen := make(map[uint]struct{}, len(in))
	out := make([]uint, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TestAvailableGroupsFiltersByEntitlement is the rapid-driven property
// for task 10.5:
//
//	rapid over a randomly chosen subset of non-baseline groups:
//	  the handler's non-admin response group-ID set ==
//	    {baseline.ID} ∪ {active-subscribed group IDs}
//
// The property covers every boundary explicitly:
//   - empty subscription set → only the baseline group is returned.
//   - full subscription set → baseline plus every non-baseline group.
//   - any interior subset → exactly those groups plus baseline.
//
// A single baseline group plus N non-baseline groups is seeded per
// iteration (N in [0, 6] so the generator's subset search stays tractable
// while still exercising the full power-set at small cardinalities). The
// rapid.checks budget is pinned to ≥ 100 per the task guidance.
//
// **Validates: Property 10 (Note 附带), Requirement 3.3**
func TestAvailableGroupsFiltersByEntitlement(t *testing.T) {
	raiseRapidChecksAdminPricing(t, 100)

	rapid.Check(t, func(rt *rapid.T) {
		// userID is strictly positive — GORM accepts non-zero explicit
		// primary keys, and requireBillingCtx rejects userID == 0.
		userID := uint(rapid.IntRange(1, 1_000_000).Draw(rt, "userID"))

		// nonBaselineCount ∈ [0, 6]. 0 exercises the "no tiers at all"
		// edge case (only baseline exists). 6 keeps the 2^N subset
		// search under 64 shapes per iteration so the test runs fast
		// even at 200+ rapid.checks.
		nonBaselineCount := rapid.IntRange(0, 6).Draw(rt, "nonBaselineCount")

		pr, db := newAvailableGroupsRouter(t)
		seedAvailableGroupsUser(t, db, userID)
		baselineID, nonBaselineIDs := seedAvailableGroupsCatalog(t, db, nonBaselineCount)

		// For each non-baseline group, independently decide whether the
		// user is subscribed. This yields a uniformly random subset of
		// the non-baseline catalog, including the empty and full sets.
		subscribedIDs := make([]uint, 0, nonBaselineCount)
		for i, gid := range nonBaselineIDs {
			if rapid.Bool().Draw(rt, fmt.Sprintf("subscribed_%d", i)) {
				subscribedIDs = append(subscribedIDs, gid)
			}
		}
		subscribeUserToGroups(t, db, userID, subscribedIDs)

		got := sortedUintSet(callAvailableGroups(t, pr, userID))
		want := sortedUintSet(append([]uint{baselineID}, subscribedIDs...))

		if len(got) != len(want) {
			rt.Fatalf("len: got %d want %d; got=%v want=%v (subscribed=%v)",
				len(got), len(want), got, want, subscribedIDs)
		}
		for i := range got {
			if got[i] != want[i] {
				rt.Fatalf("mismatch at %d: got %d want %d; got=%v want=%v (subscribed=%v)",
					i, got[i], want[i], got, want, subscribedIDs)
			}
		}
	})
}
