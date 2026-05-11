package main

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func AdminUsersListHandler(c *gin.Context) {
	if !requireAdmin(c) { return }
	page := queryInt(c, "page", 1, 1, 1000000)
	pageSize := queryInt(c, "page_size", 15, 1, 100)
	q := strings.TrimSpace(c.Query("q"))
	role := strings.TrimSpace(c.Query("role"))
	status := strings.TrimSpace(c.Query("status"))
	db := GlobalDB.WithContext(c.Request.Context()).Model(&User{})
	if q != "" { db = db.Where("email ILIKE ? OR username ILIKE ?", "%"+q+"%", "%"+q+"%") }
	if role != "" { db = db.Where("role = ?", role) }
	if status != "" { db = db.Where("status = ?", status) }
	var total int64
	if err := db.Count(&total).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count users"); return }
	var users []User
	if err := db.Order("id DESC").Limit(pageSize).Offset((page-1)*pageSize).Find(&users).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list users"); return }
	items := make([]gin.H, 0, len(users))
	for _, u := range users { items = append(items, adminUserPayload(u)) }
	Success(c, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func AdminUsersCreateHandler(c *gin.Context) {
	if !requireAdmin(c) { return }
	var req struct { Email, Password, Role, Username string; Balance float64 `json:"balance"` }
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Email) == "" || req.Password == "" { Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid user payload"); return }
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to hash password"); return }
	role := firstNonEmpty(req.Role, "user")
	u := User{Email: strings.ToLower(strings.TrimSpace(req.Email)), PasswordHash: string(hash), Role: role, Username: strings.TrimSpace(req.Username), Balance: req.Balance, Status: userStatusActive, Concurrency: 1}
	if err := GlobalDB.WithContext(c.Request.Context()).Create(&u).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to create user"); return }
	Success(c, adminUserPayload(u))
}

func AdminUsersUpdateHandler(c *gin.Context) {
	if !requireAdmin(c) { return }
	u, ok := loadAdminUser(c); if !ok { return }
	var req struct { Role string `json:"role"`; Balance float64 `json:"balance"`; Concurrency int `json:"concurrency"`; Status string `json:"status"`; Username *string `json:"username"`; Password string `json:"password"` }
	if err := c.ShouldBindJSON(&req); err != nil { Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid user payload"); return }
	u.Role = firstNonEmpty(req.Role, u.Role, "user"); u.Balance = req.Balance; u.Concurrency = req.Concurrency; u.Status = firstNonEmpty(req.Status, u.Status, userStatusActive)
	if req.Username != nil { u.Username = strings.TrimSpace(*req.Username) }
	if strings.TrimSpace(req.Password) != "" { hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost); if err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to hash password"); return }; u.PasswordHash = string(hash) }
	if err := GlobalDB.WithContext(c.Request.Context()).Save(&u).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to update user"); return }
	Success(c, adminUserPayload(u))
}

func AdminUsersDeleteHandler(c *gin.Context) {
	if !requireAdmin(c) { return }
	id, err := parseUintParam(c, "id"); if err != nil { Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid user id"); return }
	if err := GlobalDB.WithContext(c.Request.Context()).Model(&User{}).Where("id = ?", id).Update("status", "deleted").Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to delete user"); return }
	Success(c, gin.H{"deleted": true})
}

func AdminUsersDepositHandler(c *gin.Context) {
	if !requireAdmin(c) { return }
	u, ok := loadAdminUser(c); if !ok { return }
	var req struct{ Amount float64 `json:"amount"`; Note string `json:"note"` }
	if err := c.ShouldBindJSON(&req); err != nil || req.Amount == 0 { Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid deposit payload"); return }
	err := GlobalDB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error { if err := tx.Model(&u).Update("balance", gorm.Expr("balance + ?", req.Amount)).Error; err != nil { return err }; return tx.Create(&BalanceLog{UserID: u.ID, Amount: req.Amount, Type: "admin_deposit", Reference: strings.TrimSpace(req.Note)}).Error })
	if err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to deposit balance"); return }
	Success(c, gin.H{"ok": true})
}

func AdminUsersAPIKeysHandler(c *gin.Context) {
	if !requireAdmin(c) { return }
	id, err := parseUintParam(c, "id"); if err != nil { Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid user id"); return }
	var keys []ApiKey
	if err := GlobalDB.WithContext(c.Request.Context()).Where("user_id = ?", id).Order("id DESC").Find(&keys).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list api keys"); return }
	items := make([]gin.H, 0, len(keys)); for _, k := range keys { items = append(items, gin.H{"id": k.ID, "name": k.Name, "prefix": k.KeyPrefix, "status": k.Status, "quota": 0, "quota_used": 0, "created_at": k.CreatedAt}) }
	Success(c, items)
}

func AdminUsersBalanceHistoryHandler(c *gin.Context) {
	if !requireAdmin(c) { return }
	id, err := parseUintParam(c, "id"); if err != nil { Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid user id"); return }
	var logs []BalanceLog
	if err := GlobalDB.WithContext(c.Request.Context()).Where("user_id = ?", id).Order("created_at DESC, id DESC").Limit(100).Find(&logs).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list balance history"); return }
	items := make([]gin.H, 0, len(logs)); for _, l := range logs { items = append(items, gin.H{"id": l.ID, "kind": l.Type, "amount": l.Amount, "balance_before": 0, "balance_after": 0, "operator_email": nil, "note": l.Reference, "created_at": l.CreatedAt}) }
	Success(c, gin.H{"entries": items})
}

func AdminGroupsListHandler(c *gin.Context) {
	if !requireAdmin(c) { return }
	var pkgs []SubscriptionPackage
	if err := GlobalDB.WithContext(c.Request.Context()).Order("id ASC").Find(&pkgs).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list groups"); return }
	items := make([]gin.H, 0, len(pkgs)); for _, p := range pkgs { items = append(items, groupPayload(p)) }
	Success(c, items)
}

func AdminGroupsCreateHandler(c *gin.Context) { if !requireAdmin(c) { return }; saveAdminGroup(c, 0) }
func AdminGroupsUpdateHandler(c *gin.Context) { if !requireAdmin(c) { return }; id, err := parseUintParam(c, "id"); if err != nil { Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid group id"); return }; saveAdminGroup(c, id) }
func AdminGroupsDeleteHandler(c *gin.Context) { if !requireAdmin(c) { return }; id, err := parseUintParam(c, "id"); if err != nil { Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid group id"); return }; if err := GlobalDB.WithContext(c.Request.Context()).Delete(&SubscriptionPackage{}, id).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to delete group"); return }; Success(c, gin.H{"deleted": true}) }

func AdminSubscriptionsListHandler(c *gin.Context) {
	if !requireAdmin(c) { return }
	page := queryInt(c, "page", 1, 1, 1000000); pageSize := queryInt(c, "page_size", 20, 1, 100)
	var total int64
	if err := GlobalDB.WithContext(c.Request.Context()).Model(&Subscription{}).Count(&total).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count subscriptions"); return }
	var subs []Subscription
	if err := GlobalDB.WithContext(c.Request.Context()).Order("id DESC").Limit(pageSize).Offset((page-1)*pageSize).Find(&subs).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list subscriptions"); return }
	items := make([]gin.H, 0, len(subs)); for _, s := range subs { items = append(items, subscriptionAdminPayload(c, s)) }
	Success(c, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func AdminSubscriptionsCreateHandler(c *gin.Context) {
	if !requireAdmin(c) { return }
	var req struct { UserID, GroupID uint; ValidityDays int `json:"validity_days"`; Notes, FundingSource, FundingReference string; PricePaidUSD float64 `json:"price_paid_usd"` }
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == 0 || req.GroupID == 0 { Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid subscription payload"); return }
	var pkg SubscriptionPackage; if err := GlobalDB.WithContext(c.Request.Context()).Where("group_id = ? OR id = ?", req.GroupID, req.GroupID).First(&pkg).Error; err != nil { Error(c, http.StatusNotFound, apiErrorNotFound, "group not found"); return }
	days := req.ValidityDays; if days <= 0 { days = pkg.DefaultValidityDays }; if days <= 0 { days = 30 }
	now := time.Now(); sub := Subscription{UserID: req.UserID, PackageID: pkg.ID, GroupID: pkg.GroupID, GroupName: pkg.Name, Status: "active", StartsAt: now, ExpiresAt: now.AddDate(0,0,days), DailyLimitUSD: pkg.DailyLimitUSD, WeeklyLimitUSD: pkg.WeeklyLimitUSD, MonthlyLimitUSD: pkg.MonthlyLimitUSD, FundingSource: req.FundingSource, FundingReference: req.FundingReference, PricePaidUSD: req.PricePaidUSD, Notes: req.Notes}
	if err := GlobalDB.WithContext(c.Request.Context()).Create(&sub).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to create subscription"); return }
	Success(c, subscriptionAdminPayload(c, sub))
}

func AdminSubscriptionsExtendHandler(c *gin.Context) { if !requireAdmin(c) { return }; sub, ok := loadSubscription(c); if !ok { return }; var req struct{ Days int `json:"days"` }; if err := c.ShouldBindJSON(&req); err != nil || req.Days <= 0 { Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid days"); return }; sub.ExpiresAt = sub.ExpiresAt.AddDate(0,0,req.Days); if err := GlobalDB.WithContext(c.Request.Context()).Save(&sub).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to extend subscription"); return }; Success(c, subscriptionAdminPayload(c, sub)) }
func AdminSubscriptionsRevokeHandler(c *gin.Context) { if !requireAdmin(c) { return }; sub, ok := loadSubscription(c); if !ok { return }; sub.Status = "revoked"; if err := GlobalDB.WithContext(c.Request.Context()).Save(&sub).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to revoke subscription"); return }; Success(c, subscriptionAdminPayload(c, sub)) }
func AdminSubscriptionsReactivateHandler(c *gin.Context) { if !requireAdmin(c) { return }; sub, ok := loadSubscription(c); if !ok { return }; sub.Status = "active"; if sub.ExpiresAt.Before(time.Now()) { sub.ExpiresAt = time.Now().AddDate(0,0,30) }; if err := GlobalDB.WithContext(c.Request.Context()).Save(&sub).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to reactivate subscription"); return }; Success(c, subscriptionAdminPayload(c, sub)) }
func AdminSubscriptionsResetQuotaHandler(c *gin.Context) { if !requireAdmin(c) { return }; sub, ok := loadSubscription(c); if !ok { return }; sub.DailyUsageUSD = 0; sub.WeeklyUsageUSD = 0; sub.MonthlyUsageUSD = 0; if err := GlobalDB.WithContext(c.Request.Context()).Save(&sub).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to reset quota"); return }; Success(c, subscriptionAdminPayload(c, sub)) }

func adminUserPayload(u User) gin.H { return gin.H{"id": u.ID, "email": u.Email, "username": nullableString(u.Username), "role": firstNonEmpty(u.Role, "user"), "balance": u.Balance, "status": firstNonEmpty(u.Status, userStatusActive), "concurrency": u.Concurrency, "created_at": u.CreatedAt, "updated_at": u.UpdatedAt} }
func nullableString(s string) any { if strings.TrimSpace(s) == "" { return nil }; return s }
func loadAdminUser(c *gin.Context) (User, bool) { id, err := parseUintParam(c, "id"); if err != nil { Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid user id"); return User{}, false }; var u User; if err := GlobalDB.WithContext(c.Request.Context()).First(&u, id).Error; err != nil { if errors.Is(err, gorm.ErrRecordNotFound) { Error(c, http.StatusNotFound, apiErrorNotFound, "user not found") } else { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load user") }; return User{}, false }; return u, true }

func saveAdminGroup(c *gin.Context, id uint) { var req struct { Name string `json:"name"`; RateMultiplier float64 `json:"rate_multiplier"`; DailyLimitUSD, WeeklyLimitUSD, MonthlyLimitUSD *float64; DefaultValidityDays int `json:"default_validity_days"`; SubscriptionPriceUSD float64 `json:"subscription_price_usd"` }; if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Name) == "" { Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid group payload"); return }; pkg := SubscriptionPackage{}; if id != 0 { if err := GlobalDB.WithContext(c.Request.Context()).First(&pkg, id).Error; err != nil { Error(c, http.StatusNotFound, apiErrorNotFound, "group not found"); return } } else { var maxID uint; _ = GlobalDB.WithContext(c.Request.Context()).Model(&SubscriptionPackage{}).Select("COALESCE(MAX(group_id),0)").Scan(&maxID).Error; pkg.GroupID = maxID + 1; pkg.Enabled = true }; pkg.Name = strings.TrimSpace(req.Name); pkg.RateMultiplier = req.RateMultiplier; if pkg.RateMultiplier <= 0 { pkg.RateMultiplier = 1 }; pkg.DefaultValidityDays = req.DefaultValidityDays; if pkg.DefaultValidityDays <= 0 { pkg.DefaultValidityDays = 30 }; pkg.DailyLimitUSD = req.DailyLimitUSD; pkg.WeeklyLimitUSD = req.WeeklyLimitUSD; pkg.MonthlyLimitUSD = req.MonthlyLimitUSD; pkg.SubscriptionPriceUSD = req.SubscriptionPriceUSD; if err := GlobalDB.WithContext(c.Request.Context()).Save(&pkg).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to save group"); return }; Success(c, groupPayload(pkg)) }
func groupPayload(p SubscriptionPackage) gin.H { return gin.H{"id": p.ID, "name": p.Name, "subscription_type": "subscription", "rate_multiplier": p.RateMultiplier, "daily_limit_usd": p.DailyLimitUSD, "weekly_limit_usd": p.WeeklyLimitUSD, "monthly_limit_usd": p.MonthlyLimitUSD, "default_validity_days": p.DefaultValidityDays, "subscription_price_usd": p.SubscriptionPriceUSD} }

func loadSubscription(c *gin.Context) (Subscription, bool) { id, err := parseUintParam(c, "id"); if err != nil { Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid subscription id"); return Subscription{}, false }; var sub Subscription; if err := GlobalDB.WithContext(c.Request.Context()).First(&sub, id).Error; err != nil { Error(c, http.StatusNotFound, apiErrorNotFound, "subscription not found"); return Subscription{}, false }; return sub, true }
func subscriptionAdminPayload(c *gin.Context, s Subscription) gin.H { var u User; _ = GlobalDB.WithContext(c.Request.Context()).First(&u, s.UserID).Error; return gin.H{"id": s.ID, "user_id": s.UserID, "group_id": s.GroupID, "email": u.Email, "username": nullableString(u.Username), "group_name": s.GroupName, "status": s.Status, "starts_at": s.StartsAt, "expires_at": s.ExpiresAt, "daily_usage_usd": s.DailyUsageUSD, "weekly_usage_usd": s.WeeklyUsageUSD, "monthly_usage_usd": s.MonthlyUsageUSD, "daily_limit_usd": s.DailyLimitUSD, "weekly_limit_usd": s.WeeklyLimitUSD, "monthly_limit_usd": s.MonthlyLimitUSD, "created_at": s.CreatedAt, "funding_source": s.FundingSource, "funding_reference": s.FundingReference, "price_paid": s.PricePaidUSD, "notes": nullableString(s.Notes)} }
