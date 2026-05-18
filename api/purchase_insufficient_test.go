package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/xxww0098/cpa-gateway/model"
	"pgregory.net/rapid"
)

// TestPurchaseInsufficientBalanceNoWrites is the rapid-driven property for
// task 11.3. It pins Requirement 5.5: when the ledger's Debit surfaces
// ErrInsufficientBalance, the purchase handler must
//
//  1. respond with HTTP 400 and body {"error":"insufficient balance"},
//  2. leave the balance_logs table untouched (no debit / credit / settle
//     rows), and
//  3. not create a Subscription row.
//
// Setup per iteration:
//   - Fresh in-memory sqlite + miniredis-backed *ledger.Ledger (via
//     newPurchaseAtomicityRouter; its Subscription-create failure hook is
//     disabled because Debit fails first and Create is never reached).
//   - User seeded with balance = 0 and Status = "active" so the only path
//     that can gate the request is the insufficient-balance check inside
//     ledger.Debit itself. In particular no shortfall row is seeded, so
//     the outstanding-debt preflight is guaranteed to pass and the request
//     reaches the Debit call.
//   - SubscriptionPackage priced in (0.01, 10] so every iteration has a
//     strictly positive price that exceeds the zero balance.
//
// **Validates: Property 14, Requirement 5.5**
func TestPurchaseInsufficientBalanceNoWrites(t *testing.T) {
	raiseRapidChecksAdminPricing(t, 100)

	var iter atomic.Int64

	rapid.Check(t, func(rt *rapid.T) {
		// Per-iteration unique userID so any leaked rows can be attributed
		// back to the draw and sqlite primary-key constraints never
		// collide across iterations on the same process-wide DB.
		userID := uint(iter.Add(1))

		// Price strictly positive and bounded per the task spec
		// (balance=0, price in [0.01, 10]).
		price := rapid.Float64Range(0.01, 10.0).Draw(rt, "price")

		// failFlag is forced false: the atomicity fixture's Subscription
		// create callback must not fire. Debit fails first on a zero
		// balance, so the Create path is unreachable, but pinning the
		// flag keeps the intent explicit for future readers.
		var failFlag atomic.Bool
		failFlag.Store(false)

		pr, db := newPurchaseAtomicityRouter(rt, &failFlag)

		// Seed: user with balance = 0. We deliberately bypass
		// seedPurchaseAtomicityWorld here because that helper pads the
		// balance to 2*price+10, which would pass the Debit check and
		// invalidate the property under test.
		user := model.User{
			ID:           userID,
			Email:        fmt.Sprintf("purchase-insufficient-%d@test.local", userID),
			PasswordHash: "hash",
			Balance:      0,
			Status:       userStatusActive,
		}
		if err := db.Create(&user).Error; err != nil {
			rt.Fatalf("seed user: %v", err)
		}

		pkg := model.SubscriptionPackage{
			Name:                 "PropertyTestPkg",
			GroupID:              7,
			RateMultiplier:       0.9,
			DefaultValidityDays:  30,
			SubscriptionPriceUSD: price,
			Enabled:              true,
		}
		if err := db.Create(&pkg).Error; err != nil {
			rt.Fatalf("seed package: %v", err)
		}

		// Drive the handler.
		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.Use(seedBillingCtxMiddleware(userID))
		r.POST("/api/panel/user/subscriptions/purchase", pr.PurchaseSubscriptionHandler)

		body, err := json.Marshal(map[string]any{"group_id": pkg.GroupID})
		if err != nil {
			rt.Fatalf("marshal body: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost,
			"/api/panel/user/subscriptions/purchase",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		// Assertion 1: HTTP 400 with the documented body. A neutral
		// 500 or a 402 would mean the handler took a different branch
		// (e.g. shortfall preflight, internal error) — both break
		// Requirement 5.5.
		if w.Code != http.StatusBadRequest {
			rt.Fatalf("status: got %d want 400 (price=%v); body=%s",
				w.Code, price, w.Body.String())
		}
		var resp map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			rt.Fatalf("unmarshal body: %v (raw=%s)", err, w.Body.String())
		}
		if got, want := resp["error"], "insufficient balance"; got != want {
			rt.Fatalf("error: got %q want %q (price=%v, raw=%s)",
				got, want, price, w.Body.String())
		}

		// Assertion 2: no balance_logs rows for this user. Requirement
		// 5.5 is stricter than "no debit row" — it mandates that NO
		// BalanceLog row references this request, so the count check
		// covers every ledger row type (debit / credit / settle / hold
		// / release).
		var balanceLogCount int64
		if err := db.Model(&model.BalanceLog{}).
			Where("user_id = ?", userID).
			Count(&balanceLogCount).Error; err != nil {
			rt.Fatalf("count balance logs: %v", err)
		}
		if balanceLogCount != 0 {
			var rows []model.BalanceLog
			_ = db.Where("user_id = ?", userID).Find(&rows).Error
			rt.Fatalf("balance_logs rows: got %d want 0 (price=%v, rows=%+v)",
				balanceLogCount, price, rows)
		}

		// Assertion 3: no Subscription row for this user. Debit failed
		// before the handler ever reached Create, so this must be zero.
		var subCount int64
		if err := db.Model(&model.Subscription{}).
			Where("user_id = ?", userID).
			Count(&subCount).Error; err != nil {
			rt.Fatalf("count subs: %v", err)
		}
		if subCount != 0 {
			rt.Fatalf("subscriptions rows: got %d want 0 (price=%v)", subCount, price)
		}

		// Sanity: the user's balance stayed exactly 0. Debit must not
		// have mutated the persistent balance on the ErrInsufficientBalance
		// path (ledger.Debit's transaction rolls back on that branch).
		var after model.User
		if err := db.First(&after, userID).Error; err != nil {
			rt.Fatalf("reload user: %v", err)
		}
		if after.Balance != 0 {
			rt.Fatalf("balance: got %v want 0 (price=%v)", after.Balance, price)
		}
	})
}
