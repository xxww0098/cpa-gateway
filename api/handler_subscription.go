package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/xxww0098/cpa-gateway/ledger"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/gorm"
)

type subscriptionPackageItem struct {
	ID                   uint     `json:"id"`
	Name                 string   `json:"name"`
	Description          string   `json:"description,omitempty"`
	RateMultiplier       float64  `json:"rate_multiplier"`
	DefaultValidityDays  int      `json:"default_validity_days"`
	DailyLimitUSD        *float64 `json:"daily_limit_usd,omitempty"`
	WeeklyLimitUSD       *float64 `json:"weekly_limit_usd,omitempty"`
	MonthlyLimitUSD      *float64 `json:"monthly_limit_usd,omitempty"`
	SubscriptionPriceUSD float64  `json:"subscription_price_usd"`
}

type subscriptionItem struct {
	ID              uint      `json:"id"`
	GroupID         uint      `json:"group_id"`
	GroupName       string    `json:"group_name"`
	Status          string    `json:"status"`
	StartsAt        time.Time `json:"starts_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	DailyUsageUSD   float64   `json:"daily_usage_usd"`
	WeeklyUsageUSD  float64   `json:"weekly_usage_usd"`
	MonthlyUsageUSD float64   `json:"monthly_usage_usd"`
	DailyLimitUSD   *float64  `json:"daily_limit_usd,omitempty"`
	WeeklyLimitUSD  *float64  `json:"weekly_limit_usd,omitempty"`
	MonthlyLimitUSD *float64  `json:"monthly_limit_usd,omitempty"`
}

type purchaseSubscriptionRequest struct {
	GroupID uint `json:"group_id"`
}

func (pr *PanelRouter) RegisterSubscriptionRoutes(rg *gin.RouterGroup) {
	rg.GET("/user/subscription-packages", pr.ListSubscriptionPackagesHandler)
	rg.GET("/user/subscriptions", pr.ListSubscriptionsHandler)
	rg.POST("/user/subscriptions/purchase", pr.PurchaseSubscriptionHandler)
}

func (pr *PanelRouter) ListSubscriptionPackagesHandler(c *gin.Context) {
	if pr.DB == nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "database not initialized")
		return
	}

	var pkgs []model.SubscriptionPackage
	if err := pr.DB.WithContext(c.Request.Context()).Where("enabled = ?", true).Order("id ASC").Find(&pkgs).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list subscription packages")
		return
	}

	items := make([]subscriptionPackageItem, 0, len(pkgs))
	for _, pkg := range pkgs {
		items = append(items, subscriptionPackageItem{
			ID:                   pkg.GroupID,
			Name:                 pkg.Name,
			Description:          pkg.Description,
			RateMultiplier:       pkg.RateMultiplier,
			DefaultValidityDays:  pkg.DefaultValidityDays,
			DailyLimitUSD:        pkg.DailyLimitUSD,
			WeeklyLimitUSD:       pkg.WeeklyLimitUSD,
			MonthlyLimitUSD:      pkg.MonthlyLimitUSD,
			SubscriptionPriceUSD: pkg.SubscriptionPriceUSD,
		})
	}

	Success(c, items)
}

func (pr *PanelRouter) ListSubscriptionsHandler(c *gin.Context) {
	bc, ok := pr.requireBillingCtx(c)
	if !ok {
		return
	}

	var subs []model.Subscription
	if err := pr.DB.WithContext(c.Request.Context()).Where("user_id = ?", bc.UserID).Order("created_at DESC").Find(&subs).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list subscriptions")
		return
	}

	items := make([]subscriptionItem, 0, len(subs))
	for _, sub := range subs {
		items = append(items, subscriptionItem{
			ID:              sub.ID,
			GroupID:         sub.GroupID,
			GroupName:       sub.GroupName,
			Status:          sub.Status,
			StartsAt:        sub.StartsAt,
			ExpiresAt:       sub.ExpiresAt,
			DailyUsageUSD:   sub.DailyUsageUSD,
			WeeklyUsageUSD:  sub.WeeklyUsageUSD,
			MonthlyUsageUSD: sub.MonthlyUsageUSD,
			DailyLimitUSD:   sub.DailyLimitUSD,
			WeeklyLimitUSD:  sub.WeeklyLimitUSD,
			MonthlyLimitUSD: sub.MonthlyLimitUSD,
		})
	}

	Success(c, items)
}

// PurchaseSubscriptionHandler executes the debit -> create-subscription flow
// atomically (Requirement 5). The sequence is:
//
//  1. Outstanding-debt preflight (Requirement 2.5): if the user has any
//     unresolved shortfall in balance_logs, refuse with HTTP 402
//     outstanding_debt before any write. This mirrors the HoldMiddleware
//     preflight so a debtor cannot park their debt behind a subscription
//     purchase.
//  2. Load the requested subscription package.
//  3. Generate a unique debit reference of the form
//     "subscription_purchase:<package_id>:<nonce>" using uuid. The reference
//     prefix (Requirement 5.3) lets operators pair the debit row with any
//     later compensating credit by scanning Reference LIKE
//     'subscription_purchase:<package_id>:%'.
//  4. Debit the price from the user's balance. ErrInsufficientBalance
//     surfaces as HTTP 400 with body {"error":"insufficient balance"} and
//     guarantees no BalanceLog / Subscription row is written (Requirement
//     5.5). Any other error is a 500.
//  5. Create the Subscription row. If the insert fails, issue a
//     compensating Credit with Reference
//     "subscription_purchase:<package_id>:compensate:<debit_ref>"
//     (Requirement 5.2, 5.3) so the user's balance is restored and
//     operators can correlate the refund to the failed attempt. The
//     compensation itself is best-effort: a compErr is logged at Error
//     level (needs manual intervention) while the outer response still
//     returns 500 to the caller.
//  6. Success returns the existing payload.
//
// Log events emitted from this handler:
//   - subscription_create_failed (Warn) — insert failure, includes ref.
//   - subscription_compensation_failed (Error) — compensating credit also
//     failed; operators must reconcile manually.
//   - subscription_purchase_debt_block (Warn, via preflight) — user blocked.
func (pr *PanelRouter) PurchaseSubscriptionHandler(c *gin.Context) {
	bc, ok := pr.requireBillingCtx(c)
	if !ok {
		return
	}

	var req purchaseSubscriptionRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.GroupID == 0 {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid request body")
		return
	}

	if pr.Ledger == nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "ledger not initialized")
		return
	}

	ctx := c.Request.Context()

	// (1) Outstanding-debt preflight. An error from the ledger is treated
	// as "unknown" and fails closed: we refuse the purchase so a transient
	// DB hiccup does not let a debtor charge through the block. The public
	// body mirrors the HoldMiddleware preflight.
	if outstanding, err := pr.Ledger.HasUnresolvedShortfall(ctx, bc.UserID); err != nil {
		slog.Warn("subscription_purchase_shortfall_lookup_failed",
			"event", "subscription_purchase_shortfall_lookup_failed",
			"user_id", bc.UserID,
			"err", err,
		)
		c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{"error": "outstanding_debt"})
		return
	} else if outstanding {
		slog.Warn("subscription_purchase_debt_block",
			"event", "subscription_purchase_debt_block",
			"user_id", bc.UserID,
			"group_id", req.GroupID,
		)
		c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{"error": "outstanding_debt"})
		return
	}

	// (2) Load the package row that corresponds to the requested group.
	var pkg model.SubscriptionPackage
	if err := pr.DB.WithContext(ctx).Where("group_id = ? AND enabled = ?", req.GroupID, true).First(&pkg).Error; err != nil {
		Error(c, http.StatusNotFound, apiErrorNotFound, "subscription package not found")
		return
	}

	price := pkg.SubscriptionPriceUSD
	if price <= 0 {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid subscription package price")
		return
	}

	// (3) Build the debit reference. The package_id + nonce layout is the
	// contract for pairing with the compensating credit on rollback; see
	// design.md "BalanceLog.Reference prefix conventions".
	ref := fmt.Sprintf("subscription_purchase:%d:%s", pkg.ID, uuid.NewString())

	// (4) Debit the price. ErrInsufficientBalance is the only case where
	// Debit returns without writing a BalanceLog row (see ledger.Debit's
	// transaction body), so the "no subscription, no balance log" invariant
	// in Requirement 5.5 holds simply by refusing here.
	if err := pr.Ledger.Debit(ctx, bc.UserID, price, ref); err != nil {
		if errors.Is(err, ledger.ErrInsufficientBalance) {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "insufficient balance"})
			return
		}
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to purchase subscription")
		return
	}

	groupName := pkg.Name
	if groupName == "" {
		groupName = "Plan"
	}
	now := time.Now()
	days := pkg.DefaultValidityDays
	if days < 1 {
		days = 1
	}
	expiresAt := now.AddDate(0, 0, days)

	sub := model.Subscription{
		UserID:          bc.UserID,
		PackageID:       pkg.ID,
		GroupID:         pkg.GroupID,
		GroupName:       groupName,
		Status:          "active",
		StartsAt:        now,
		ExpiresAt:       expiresAt,
		DailyUsageUSD:   0,
		DailyResetAt:    model.NextDailyResetAfter(now),
		WeeklyUsageUSD:  0,
		WeeklyResetAt:   model.NextWeeklyResetAfter(now),
		MonthlyUsageUSD: 0,
		MonthlyResetAt:  model.NextMonthlyResetAfter(now),
		DailyLimitUSD:   pkg.DailyLimitUSD,
		WeeklyLimitUSD:  pkg.WeeklyLimitUSD,
		MonthlyLimitUSD: pkg.MonthlyLimitUSD,
	}
	// (5) Create the Subscription row. On failure we immediately compensate
	// the debit so the user's balance returns to its pre-request value; the
	// refund reference embeds the original debit ref so operators can pair
	// them deterministically.
	if err := pr.DB.WithContext(ctx).Create(&sub).Error; err != nil {
		compRef := fmt.Sprintf("subscription_purchase:%d:compensate:%s", pkg.ID, ref)
		compErr := pr.Ledger.Credit(ctx, bc.UserID, price, compRef)

		slog.Warn("subscription_create_failed",
			"event", "subscription_create_failed",
			"user_id", bc.UserID,
			"package_id", pkg.ID,
			"ref", ref,
			"err", err,
		)
		if compErr != nil {
			// The outer Debit row is still on the books and the user is
			// charged; ops must issue a manual shortfall_resolve credit.
			slog.Error("subscription_compensation_failed",
				"event", "subscription_compensation_failed",
				"user_id", bc.UserID,
				"package_id", pkg.ID,
				"ref", ref,
				"compensate_ref", compRef,
				"err", compErr,
			)
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "subscription create failed"})
		return
	}

	// (6) Success. Return the same payload the pre-hardening handler did so
	// front-end contracts stay intact.
	balance, err := pr.Ledger.GetBalance(ctx, bc.UserID)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load balance")
		return
	}

	Success(c, gin.H{"subscription_id": sub.ID, "balance": balance})
}

// EnsureSubscriptionSeeds seeds the default subscription packages if they are absent.
// Exported for main.go so it can be invoked during startup.
func EnsureSubscriptionSeeds(db *gorm.DB) error {
	if db == nil {
		return nil
	}

	basicLimit := 20.0
	proLimit := 100.0
	enterpriseLimit := 500.0

	seeds := []model.SubscriptionPackage{
		{
			Name:                 "Basic",
			Description:          "适合个人和轻量开发",
			GroupID:              1,
			RateMultiplier:       1.0,
			DefaultValidityDays:  30,
			MonthlyLimitUSD:      &basicLimit,
			SubscriptionPriceUSD: 9.9,
			Enabled:              true,
		},
		{
			Name:                 "Pro",
			Description:          "适合中等负载与团队协作",
			GroupID:              2,
			RateMultiplier:       0.95,
			DefaultValidityDays:  30,
			MonthlyLimitUSD:      &proLimit,
			SubscriptionPriceUSD: 29.9,
			Enabled:              true,
		},
		{
			Name:                 "Enterprise",
			Description:          "适合高负载与企业场景",
			GroupID:              3,
			RateMultiplier:       0.9,
			DefaultValidityDays:  30,
			MonthlyLimitUSD:      &enterpriseLimit,
			SubscriptionPriceUSD: 99.9,
			Enabled:              true,
		},
	}

	for _, seed := range seeds {
		updates := map[string]any{
			"name":                   seed.Name,
			"description":            seed.Description,
			"rate_multiplier":        seed.RateMultiplier,
			"default_validity_days":  seed.DefaultValidityDays,
			"monthly_limit_usd":      seed.MonthlyLimitUSD,
			"subscription_price_usd": seed.SubscriptionPriceUSD,
			"enabled":                seed.Enabled,
		}
		if err := db.Model(&model.SubscriptionPackage{}).Where("group_id = ?", seed.GroupID).Updates(updates).Error; err != nil {
			return err
		}
		var count int64
		if err := db.Model(&model.SubscriptionPackage{}).Where("group_id = ?", seed.GroupID).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			if err := db.Create(&seed).Error; err != nil {
				return err
			}
		}
	}

	return nil
}
