package main

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type subscriptionPackageItem struct {
	ID                  uint     `json:"id"`
	Name                string   `json:"name"`
	Description         string   `json:"description,omitempty"`
	RateMultiplier      float64  `json:"rate_multiplier"`
	DefaultValidityDays int      `json:"default_validity_days"`
	DailyLimitUSD       *float64 `json:"daily_limit_usd,omitempty"`
	WeeklyLimitUSD      *float64 `json:"weekly_limit_usd,omitempty"`
	MonthlyLimitUSD     *float64 `json:"monthly_limit_usd,omitempty"`
	SubscriptionPriceUSD float64  `json:"subscription_price_usd"`
}

type subscriptionItem struct {
	ID             uint      `json:"id"`
	GroupID        uint      `json:"group_id"`
	GroupName      string    `json:"group_name"`
	Status         string    `json:"status"`
	StartsAt       time.Time `json:"starts_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	DailyUsageUSD  float64   `json:"daily_usage_usd"`
	WeeklyUsageUSD float64   `json:"weekly_usage_usd"`
	MonthlyUsageUSD float64   `json:"monthly_usage_usd"`
	DailyLimitUSD  *float64  `json:"daily_limit_usd,omitempty"`
	WeeklyLimitUSD *float64  `json:"weekly_limit_usd,omitempty"`
	MonthlyLimitUSD *float64  `json:"monthly_limit_usd,omitempty"`
}

type purchaseSubscriptionRequest struct {
	GroupID uint `json:"group_id"`
}

func RegisterSubscriptionRoutes(rg *gin.RouterGroup) {
	rg.GET("/user/subscription-packages", ListSubscriptionPackagesHandler)
	rg.GET("/user/subscriptions", ListSubscriptionsHandler)
	rg.POST("/user/subscriptions/purchase", PurchaseSubscriptionHandler)
}

func ListSubscriptionPackagesHandler(c *gin.Context) {
	if GlobalDB == nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "database not initialized")
		return
	}

	var pkgs []SubscriptionPackage
	if err := GlobalDB.WithContext(c.Request.Context()).Where("enabled = ?", true).Order("id ASC").Find(&pkgs).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list subscription packages")
		return
	}

	items := make([]subscriptionPackageItem, 0, len(pkgs))
	for _, pkg := range pkgs {
		items = append(items, subscriptionPackageItem{
			ID:                  pkg.GroupID,
			Name:                pkg.Name,
			Description:         pkg.Description,
			RateMultiplier:      pkg.RateMultiplier,
			DefaultValidityDays: pkg.DefaultValidityDays,
			DailyLimitUSD:       pkg.DailyLimitUSD,
			WeeklyLimitUSD:      pkg.WeeklyLimitUSD,
			MonthlyLimitUSD:     pkg.MonthlyLimitUSD,
			SubscriptionPriceUSD: pkg.SubscriptionPriceUSD,
		})
	}

	Success(c, items)
}

func ListSubscriptionsHandler(c *gin.Context) {
	bc, ok := requireBillingCtx(c)
	if !ok {
		return
	}

	var subs []Subscription
	if err := GlobalDB.WithContext(c.Request.Context()).Where("user_id = ?", bc.UserID).Order("created_at DESC").Find(&subs).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list subscriptions")
		return
	}

	items := make([]subscriptionItem, 0, len(subs))
	for _, sub := range subs {
		items = append(items, subscriptionItem{
			ID:             sub.ID,
			GroupID:        sub.GroupID,
			GroupName:      sub.GroupName,
			Status:         sub.Status,
			StartsAt:       sub.StartsAt,
			ExpiresAt:      sub.ExpiresAt,
			DailyUsageUSD:  sub.DailyUsageUSD,
			WeeklyUsageUSD: sub.WeeklyUsageUSD,
			MonthlyUsageUSD: sub.MonthlyUsageUSD,
			DailyLimitUSD:  sub.DailyLimitUSD,
			WeeklyLimitUSD: sub.WeeklyLimitUSD,
			MonthlyLimitUSD: sub.MonthlyLimitUSD,
		})
	}

	Success(c, items)
}

func PurchaseSubscriptionHandler(c *gin.Context) {
	bc, ok := requireBillingCtx(c)
	if !ok {
		return
	}

	var req purchaseSubscriptionRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.GroupID == 0 {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid request body")
		return
	}

	var pkg SubscriptionPackage
	if err := GlobalDB.WithContext(c.Request.Context()).Where("group_id = ? AND enabled = ?", req.GroupID, true).First(&pkg).Error; err != nil {
		Error(c, http.StatusNotFound, apiErrorNotFound, "subscription package not found")
		return
	}

	if GlobalLedger == nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "ledger not initialized")
		return
	}

	price := pkg.SubscriptionPriceUSD
	if price <= 0 {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid subscription package price")
		return
	}
	if err := GlobalLedger.Debit(c.Request.Context(), bc.UserID, price, "subscription_purchase"); err != nil {
		if err == ErrInsufficientBalance {
			Error(c, http.StatusBadRequest, apiErrorBadRequest, "insufficient balance")
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

	sub := Subscription{
		UserID:         bc.UserID,
		PackageID:      pkg.ID,
		GroupID:        pkg.GroupID,
		GroupName:      groupName,
		Status:         "active",
		StartsAt:       now,
		ExpiresAt:      expiresAt,
		DailyUsageUSD:  0,
		WeeklyUsageUSD: 0,
		MonthlyUsageUSD: 0,
		DailyLimitUSD:  pkg.DailyLimitUSD,
		WeeklyLimitUSD: pkg.WeeklyLimitUSD,
		MonthlyLimitUSD: pkg.MonthlyLimitUSD,
	}
	if err := GlobalDB.WithContext(c.Request.Context()).Create(&sub).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to create subscription")
		return
	}

	balance, err := GlobalLedger.GetBalance(c.Request.Context(), bc.UserID)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load balance")
		return
	}

	Success(c, gin.H{"subscription_id": sub.ID, "balance": balance})
}

func EnsureSubscriptionSeeds(db *gorm.DB) error {
	if db == nil {
		return nil
	}

	basicLimit := 20.0
	proLimit := 100.0
	enterpriseLimit := 500.0

	seeds := []SubscriptionPackage{
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
		if err := db.Model(&SubscriptionPackage{}).Where("group_id = ?", seed.GroupID).Updates(updates).Error; err != nil {
			return err
		}
		var count int64
		if err := db.Model(&SubscriptionPackage{}).Where("group_id = ?", seed.GroupID).Count(&count).Error; err != nil {
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
