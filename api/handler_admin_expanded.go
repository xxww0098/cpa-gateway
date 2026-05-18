package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xxww0098/cpa-gateway/model"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func (pr *PanelRouter) AdminUsersListHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	page := queryInt(c, "page", 1, 1, 1000000)
	pageSize := queryInt(c, "page_size", 15, 1, 100)
	q := strings.TrimSpace(c.Query("q"))
	role := strings.TrimSpace(c.Query("role"))
	status := strings.TrimSpace(c.Query("status"))
	db := pr.DB.WithContext(c.Request.Context()).Model(&model.User{})
	if q != "" {
		db = db.Where("email ILIKE ? OR username ILIKE ?", "%"+q+"%", "%"+q+"%")
	}
	if role != "" {
		db = db.Where("role = ?", role)
	}
	if status != "" {
		db = db.Where("status = ?", status)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count users")
		return
	}
	var users []model.User
	if err := db.Order("id DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&users).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list users")
		return
	}
	items := make([]gin.H, 0, len(users))
	for _, u := range users {
		items = append(items, adminUserPayload(u))
	}
	Success(c, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func (pr *PanelRouter) AdminUsersCreateHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	var req struct {
		Email, Password, Role, Username string
		Balance                         float64 `json:"balance"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Email) == "" || req.Password == "" {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid user payload")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to hash password")
		return
	}
	role := firstNonEmpty(req.Role, "user")
	u := model.User{
		Email:        strings.ToLower(strings.TrimSpace(req.Email)),
		PasswordHash: string(hash),
		Role:         role,
		Username:     strings.TrimSpace(req.Username),
		Balance:      req.Balance,
		Status:       userStatusActive,
		Concurrency:  1,
	}
	if err := pr.DB.WithContext(c.Request.Context()).Create(&u).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to create user")
		return
	}
	actor, _ := pr.requireBillingCtx(c)
	pr.recordOperation(c, actor, "admin.user.create", "user:"+strconv.FormatUint(uint64(u.ID), 10), http.StatusOK, map[string]any{"email": u.Email, "role": u.Role})
	Success(c, adminUserPayload(u))
}

func (pr *PanelRouter) AdminUsersUpdateHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	u, ok := pr.loadAdminUser(c)
	if !ok {
		return
	}
	var req struct {
		Role        string  `json:"role"`
		Balance     float64 `json:"balance"`
		Concurrency int     `json:"concurrency"`
		Status      string  `json:"status"`
		Username    *string `json:"username"`
		Password    string  `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid user payload")
		return
	}
	u.Role = firstNonEmpty(req.Role, u.Role, "user")
	u.Balance = req.Balance
	u.Concurrency = req.Concurrency
	previousStatus := u.Status
	u.Status = firstNonEmpty(req.Status, u.Status, userStatusActive)
	if req.Username != nil {
		u.Username = strings.TrimSpace(*req.Username)
	}
	if strings.TrimSpace(req.Password) != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to hash password")
			return
		}
		u.PasswordHash = string(hash)
	}
	if err := pr.DB.WithContext(c.Request.Context()).Save(&u).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to update user")
		return
	}
	// If the admin flipped the user's status away from "active" (suspend /
	// disable / deleted via generic update path), flush caches AFTER the
	// commit so subsequent auth attempts re-read the DB. The equivalent of
	// the AdminUsersDeleteHandler hook, kept here for any admin route that
	// mutates User.Status. A no-op when the status did not change or when
	// the new status is still active.
	if previousStatus != u.Status && u.Status != userStatusActive {
		pr.invalidateUserCaches(c.Request.Context(), u.ID)
	}
	actor, _ := pr.requireBillingCtx(c)
	pr.recordOperation(c, actor, "admin.user.update", "user:"+strconv.FormatUint(uint64(u.ID), 10), http.StatusOK, map[string]any{"status": u.Status, "role": u.Role, "previous_status": previousStatus})
	Success(c, adminUserPayload(u))
}

func (pr *PanelRouter) AdminUsersDeleteHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	id, err := parseUintParam(c, "id")
	if err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid user id")
		return
	}
	if err := pr.DB.WithContext(c.Request.Context()).Model(&model.User{}).Where("id = ?", id).Update("status", "deleted").Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to delete user")
		return
	}
	// Flush UserStatusCache + APIKeyCache AFTER the status=deleted commit so
	// subsequent /v1/* and /api/panel/** requests that arrive via a stale
	// cached "active" entry re-read the DB and reject the credential. Failure
	// path above returns early, preserving atomicity (no cache flush on DB
	// failure — the DB row is still "active").
	pr.invalidateUserCaches(c.Request.Context(), id)
	actor, _ := pr.requireBillingCtx(c)
	pr.recordOperation(c, actor, "admin.user.delete", "user:"+strconv.FormatUint(uint64(id), 10), http.StatusOK, nil)
	Success(c, gin.H{"deleted": true})
}

func (pr *PanelRouter) AdminUsersDepositHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	u, ok := pr.loadAdminUser(c)
	if !ok {
		return
	}
	var req struct {
		Amount float64 `json:"amount"`
		Note   string  `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Amount == 0 {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid deposit payload")
		return
	}
	err := pr.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&u).Update("balance", gorm.Expr("balance + ?", req.Amount)).Error; err != nil {
			return err
		}
		return tx.Create(&model.BalanceLog{UserID: u.ID, Amount: req.Amount, Type: "admin_deposit", Reference: strings.TrimSpace(req.Note)}).Error
	})
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to deposit balance")
		return
	}
	actor, _ := pr.requireBillingCtx(c)
	pr.recordOperation(c, actor, "admin.user.deposit", "user:"+strconv.FormatUint(uint64(u.ID), 10), http.StatusOK, map[string]any{"amount": req.Amount, "note": strings.TrimSpace(req.Note)})
	Success(c, gin.H{"ok": true})
}

func (pr *PanelRouter) AdminUsersAPIKeysHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	id, err := parseUintParam(c, "id")
	if err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid user id")
		return
	}
	var keys []model.ApiKey
	if err := pr.DB.WithContext(c.Request.Context()).Where("user_id = ?", id).Order("id DESC").Find(&keys).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list api keys")
		return
	}
	items := make([]gin.H, 0, len(keys))
	for _, k := range keys {
		items = append(items, gin.H{"id": k.ID, "name": k.Name, "prefix": k.KeyPrefix, "status": k.Status, "quota": 0, "quota_used": 0, "created_at": k.CreatedAt})
	}
	Success(c, items)
}

func (pr *PanelRouter) AdminUsersBalanceHistoryHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	id, err := parseUintParam(c, "id")
	if err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid user id")
		return
	}
	var logs []model.BalanceLog
	if err := pr.DB.WithContext(c.Request.Context()).Where("user_id = ?", id).Order("created_at DESC, id DESC").Limit(100).Find(&logs).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list balance history")
		return
	}
	items := make([]gin.H, 0, len(logs))
	for _, l := range logs {
		items = append(items, gin.H{"id": l.ID, "kind": l.Type, "amount": l.Amount, "balance_before": 0, "balance_after": 0, "operator_email": nil, "note": l.Reference, "created_at": l.CreatedAt})
	}
	Success(c, gin.H{"entries": items})
}

func (pr *PanelRouter) AdminGroupsListHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	var pkgs []model.SubscriptionPackage
	if err := pr.DB.WithContext(c.Request.Context()).Order("id ASC").Find(&pkgs).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list groups")
		return
	}
	items := make([]gin.H, 0, len(pkgs))
	for _, p := range pkgs {
		items = append(items, groupPayload(p))
	}
	Success(c, items)
}

func (pr *PanelRouter) AdminGroupsCreateHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	pr.saveAdminGroup(c, 0)
}

func (pr *PanelRouter) AdminGroupsUpdateHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	id, err := parseUintParam(c, "id")
	if err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid group id")
		return
	}
	pr.saveAdminGroup(c, id)
}

func (pr *PanelRouter) AdminGroupsDeleteHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	id, err := parseUintParam(c, "id")
	if err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid group id")
		return
	}
	if err := pr.DB.WithContext(c.Request.Context()).Delete(&model.SubscriptionPackage{}, id).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to delete group")
		return
	}
	Success(c, gin.H{"deleted": true})
}

func (pr *PanelRouter) AdminSubscriptionsListHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	page := queryInt(c, "page", 1, 1, 1000000)
	pageSize := queryInt(c, "page_size", 20, 1, 100)
	var total int64
	if err := pr.DB.WithContext(c.Request.Context()).Model(&model.Subscription{}).Count(&total).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count subscriptions")
		return
	}
	var subs []model.Subscription
	if err := pr.DB.WithContext(c.Request.Context()).Order("id DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&subs).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list subscriptions")
		return
	}
	items := make([]gin.H, 0, len(subs))
	for _, s := range subs {
		items = append(items, pr.subscriptionAdminPayload(c, s))
	}
	Success(c, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func (pr *PanelRouter) AdminSubscriptionsCreateHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	var req struct {
		UserID, GroupID  uint
		ValidityDays     int `json:"validity_days"`
		Notes            string
		FundingSource    string
		FundingReference string
		PricePaidUSD     float64 `json:"price_paid_usd"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == 0 || req.GroupID == 0 {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid subscription payload")
		return
	}
	var pkg model.SubscriptionPackage
	if err := pr.DB.WithContext(c.Request.Context()).Where("group_id = ? OR id = ?", req.GroupID, req.GroupID).First(&pkg).Error; err != nil {
		Error(c, http.StatusNotFound, apiErrorNotFound, "group not found")
		return
	}
	days := req.ValidityDays
	if days <= 0 {
		days = pkg.DefaultValidityDays
	}
	if days <= 0 {
		days = 30
	}
	now := time.Now()
	sub := model.Subscription{
		UserID:           req.UserID,
		PackageID:        pkg.ID,
		GroupID:          pkg.GroupID,
		GroupName:        pkg.Name,
		Status:           "active",
		StartsAt:         now,
		ExpiresAt:        now.AddDate(0, 0, days),
		DailyResetAt:     model.NextDailyResetAfter(now),
		WeeklyResetAt:    model.NextWeeklyResetAfter(now),
		MonthlyResetAt:   model.NextMonthlyResetAfter(now),
		DailyLimitUSD:    pkg.DailyLimitUSD,
		WeeklyLimitUSD:   pkg.WeeklyLimitUSD,
		MonthlyLimitUSD:  pkg.MonthlyLimitUSD,
		FundingSource:    req.FundingSource,
		FundingReference: req.FundingReference,
		PricePaidUSD:     req.PricePaidUSD,
		Notes:            req.Notes,
	}
	if err := pr.DB.WithContext(c.Request.Context()).Create(&sub).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to create subscription")
		return
	}
	Success(c, pr.subscriptionAdminPayload(c, sub))
}

func (pr *PanelRouter) AdminSubscriptionsExtendHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	sub, ok := pr.loadSubscription(c)
	if !ok {
		return
	}
	var req struct {
		Days int `json:"days"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Days <= 0 {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid days")
		return
	}
	sub.ExpiresAt = sub.ExpiresAt.AddDate(0, 0, req.Days)
	if err := pr.DB.WithContext(c.Request.Context()).Save(&sub).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to extend subscription")
		return
	}
	Success(c, pr.subscriptionAdminPayload(c, sub))
}

func (pr *PanelRouter) AdminSubscriptionsRevokeHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	sub, ok := pr.loadSubscription(c)
	if !ok {
		return
	}
	sub.Status = "revoked"
	if err := pr.DB.WithContext(c.Request.Context()).Save(&sub).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to revoke subscription")
		return
	}
	Success(c, pr.subscriptionAdminPayload(c, sub))
}

func (pr *PanelRouter) AdminSubscriptionsReactivateHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	sub, ok := pr.loadSubscription(c)
	if !ok {
		return
	}
	sub.Status = "active"
	if sub.ExpiresAt.Before(time.Now()) {
		sub.ExpiresAt = time.Now().AddDate(0, 0, 30)
	}
	if err := pr.DB.WithContext(c.Request.Context()).Save(&sub).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to reactivate subscription")
		return
	}
	Success(c, pr.subscriptionAdminPayload(c, sub))
}

func (pr *PanelRouter) AdminSubscriptionsResetQuotaHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	sub, ok := pr.loadSubscription(c)
	if !ok {
		return
	}
	sub.DailyUsageUSD = 0
	sub.WeeklyUsageUSD = 0
	sub.MonthlyUsageUSD = 0
	if err := pr.DB.WithContext(c.Request.Context()).Save(&sub).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to reset quota")
		return
	}
	Success(c, pr.subscriptionAdminPayload(c, sub))
}

func adminUserPayload(u model.User) gin.H {
	return gin.H{
		"id":          u.ID,
		"email":       u.Email,
		"username":    nullableString(u.Username),
		"role":        firstNonEmpty(u.Role, "user"),
		"balance":     u.Balance,
		"status":      firstNonEmpty(u.Status, userStatusActive),
		"concurrency": u.Concurrency,
		"created_at":  u.CreatedAt,
		"updated_at":  u.UpdatedAt,
	}
}

func nullableString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func (pr *PanelRouter) loadAdminUser(c *gin.Context) (model.User, bool) {
	id, err := parseUintParam(c, "id")
	if err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid user id")
		return model.User{}, false
	}
	var u model.User
	if err := pr.DB.WithContext(c.Request.Context()).First(&u, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			Error(c, http.StatusNotFound, apiErrorNotFound, "user not found")
		} else {
			Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load user")
		}
		return model.User{}, false
	}
	return u, true
}

func (pr *PanelRouter) saveAdminGroup(c *gin.Context, id uint) {
	var req struct {
		Name                 string  `json:"name"`
		RateMultiplier       float64 `json:"rate_multiplier"`
		DailyLimitUSD        *float64
		WeeklyLimitUSD       *float64
		MonthlyLimitUSD      *float64
		DefaultValidityDays  int     `json:"default_validity_days"`
		SubscriptionPriceUSD float64 `json:"subscription_price_usd"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid group payload")
		return
	}
	pkg := model.SubscriptionPackage{}
	if id != 0 {
		if err := pr.DB.WithContext(c.Request.Context()).First(&pkg, id).Error; err != nil {
			Error(c, http.StatusNotFound, apiErrorNotFound, "group not found")
			return
		}
	} else {
		var maxID uint
		_ = pr.DB.WithContext(c.Request.Context()).Model(&model.SubscriptionPackage{}).Select("COALESCE(MAX(group_id),0)").Scan(&maxID).Error
		pkg.GroupID = maxID + 1
		pkg.Enabled = true
	}
	pkg.Name = strings.TrimSpace(req.Name)
	pkg.RateMultiplier = req.RateMultiplier
	if pkg.RateMultiplier <= 0 {
		pkg.RateMultiplier = 1
	}
	pkg.DefaultValidityDays = req.DefaultValidityDays
	if pkg.DefaultValidityDays <= 0 {
		pkg.DefaultValidityDays = 30
	}
	pkg.DailyLimitUSD = req.DailyLimitUSD
	pkg.WeeklyLimitUSD = req.WeeklyLimitUSD
	pkg.MonthlyLimitUSD = req.MonthlyLimitUSD
	pkg.SubscriptionPriceUSD = req.SubscriptionPriceUSD
	if err := pr.DB.WithContext(c.Request.Context()).Save(&pkg).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to save group")
		return
	}
	Success(c, groupPayload(pkg))
}

func groupPayload(p model.SubscriptionPackage) gin.H {
	return gin.H{
		"id":                     p.ID,
		"name":                   p.Name,
		"subscription_type":      "subscription",
		"rate_multiplier":        p.RateMultiplier,
		"daily_limit_usd":        p.DailyLimitUSD,
		"weekly_limit_usd":       p.WeeklyLimitUSD,
		"monthly_limit_usd":      p.MonthlyLimitUSD,
		"default_validity_days":  p.DefaultValidityDays,
		"subscription_price_usd": p.SubscriptionPriceUSD,
	}
}

func (pr *PanelRouter) loadSubscription(c *gin.Context) (model.Subscription, bool) {
	id, err := parseUintParam(c, "id")
	if err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid subscription id")
		return model.Subscription{}, false
	}
	var sub model.Subscription
	if err := pr.DB.WithContext(c.Request.Context()).First(&sub, id).Error; err != nil {
		Error(c, http.StatusNotFound, apiErrorNotFound, "subscription not found")
		return model.Subscription{}, false
	}
	return sub, true
}

func (pr *PanelRouter) subscriptionAdminPayload(c *gin.Context, s model.Subscription) gin.H {
	var u model.User
	_ = pr.DB.WithContext(c.Request.Context()).First(&u, s.UserID).Error
	return gin.H{
		"id":                s.ID,
		"user_id":           s.UserID,
		"group_id":          s.GroupID,
		"email":             u.Email,
		"username":          nullableString(u.Username),
		"group_name":        s.GroupName,
		"status":            s.Status,
		"starts_at":         s.StartsAt,
		"expires_at":        s.ExpiresAt,
		"daily_usage_usd":   s.DailyUsageUSD,
		"weekly_usage_usd":  s.WeeklyUsageUSD,
		"monthly_usage_usd": s.MonthlyUsageUSD,
		"daily_limit_usd":   s.DailyLimitUSD,
		"weekly_limit_usd":  s.WeeklyLimitUSD,
		"monthly_limit_usd": s.MonthlyLimitUSD,
		"created_at":        s.CreatedAt,
		"funding_source":    s.FundingSource,
		"funding_reference": s.FundingReference,
		"price_paid":        s.PricePaidUSD,
		"notes":             nullableString(s.Notes),
	}
}
