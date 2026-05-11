package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/dashboard", AdminDashboardHandler)
	rg.GET("/admin/usage/trend", AdminUsageTrendHandler)
	rg.GET("/admin/usage/models", AdminUsageModelsHandler)
	rg.GET("/admin/users", AdminUsersListHandler)
	rg.POST("/admin/users", AdminUsersCreateHandler)
	rg.PUT("/admin/users/:id", AdminUsersUpdateHandler)
	rg.DELETE("/admin/users/:id", AdminUsersDeleteHandler)
	rg.POST("/admin/users/:id/deposit", AdminUsersDepositHandler)
	rg.GET("/admin/users/:id/api-keys", AdminUsersAPIKeysHandler)
	rg.GET("/admin/users/:id/balance-history", AdminUsersBalanceHistoryHandler)
	rg.GET("/admin/subscriptions", AdminSubscriptionsListHandler)
	rg.POST("/admin/subscriptions", AdminSubscriptionsCreateHandler)
	rg.PUT("/admin/subscriptions/:id/extend", AdminSubscriptionsExtendHandler)
	rg.DELETE("/admin/subscriptions/:id", AdminSubscriptionsRevokeHandler)
	rg.POST("/admin/subscriptions/:id/reactivate", AdminSubscriptionsReactivateHandler)
	rg.PUT("/admin/subscriptions/:id/reactivate", AdminSubscriptionsReactivateHandler)
	rg.POST("/admin/subscriptions/:id/reset-quota", AdminSubscriptionsResetQuotaHandler)
	rg.GET("/admin/groups", AdminGroupsListHandler)
	rg.POST("/admin/groups", AdminGroupsCreateHandler)
	rg.PUT("/admin/groups/:id", AdminGroupsUpdateHandler)
	rg.DELETE("/admin/groups/:id", AdminGroupsDeleteHandler)
	rg.GET("/admin/tickets", AdminListTicketsHandler)
	rg.GET("/admin/tickets/:id", AdminGetTicketHandler)
	rg.POST("/admin/tickets/:id/replies", AdminCreateTicketReplyHandler)
	rg.PUT("/admin/tickets/:id/status", AdminUpdateTicketStatusHandler)
	rg.PUT("/admin/tickets/:id/assign", AdminAssignTicketHandler)
	rg.GET("/admin/ticket-quick-replies", AdminTicketQuickRepliesGetHandler)
	rg.POST("/admin/ticket-quick-replies", AdminTicketQuickRepliesSaveHandler)
}

func AdminDashboardHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}

	var userTotal int64
	if err := GlobalDB.WithContext(c.Request.Context()).Model(&User{}).Count(&userTotal).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count users")
		return
	}

	var userActive int64
	if err := GlobalDB.WithContext(c.Request.Context()).Model(&User{}).Where("status = ?", userStatusActive).Count(&userActive).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count active users")
		return
	}

	var keyTotal int64
	if err := GlobalDB.WithContext(c.Request.Context()).Model(&ApiKey{}).Count(&keyTotal).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count api keys")
		return
	}

	var keyActive int64
	if err := GlobalDB.WithContext(c.Request.Context()).Model(&ApiKey{}).Where("status = ?", "active").Count(&keyActive).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count active api keys")
		return
	}

	today := time.Now()
	todayStart := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())
	weekStart := todayStart.AddDate(0, 0, -6)

	var todayReq int64
	if err := GlobalDB.WithContext(c.Request.Context()).Model(&UsageLog{}).Where("created_at >= ?", todayStart).Count(&todayReq).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load today usage")
		return
	}

	var todayCost float64
	if err := GlobalDB.WithContext(c.Request.Context()).Model(&UsageLog{}).
		Select("COALESCE(SUM(cost), 0)").
		Where("created_at >= ?", todayStart).
		Scan(&todayCost).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load today usage cost")
		return
	}

	var weekReq int64
	if err := GlobalDB.WithContext(c.Request.Context()).Model(&UsageLog{}).Where("created_at >= ?", weekStart).Count(&weekReq).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load week usage")
		return
	}

	Success(c, gin.H{
		"users":    gin.H{"total": userTotal, "active": userActive},
		"api_keys": gin.H{"total": keyTotal, "active": keyActive},
		"usage":    gin.H{"today_requests": todayReq, "today_cost": todayCost, "week_requests": weekReq},
	})
}

func AdminUsageTrendHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	days := queryInt(c, "days", 7, 1, 30)
	points, err := buildUsageTrend(c, 0, days)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load usage trend")
		return
	}
	Success(c, points)
}

func AdminUsageModelsHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	days := queryInt(c, "days", 30, 1, 90)
	items, err := buildUsageModels(c, 0, days)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load usage models")
		return
	}
	Success(c, items)
}

func requireAdmin(c *gin.Context) bool {
	bc, ok := requireBillingCtx(c)
	if !ok {
		return false
	}
	if isAdminEmail(bc.Email) {
		return true
	}

	var user User
	if err := GlobalDB.WithContext(c.Request.Context()).First(&user, bc.UserID).Error; err == nil && isAdminEmail(user.Email) {
		return true
	}

	Error(c, http.StatusForbidden, apiErrorUnauthorized, "admin permission required")
	return false
}

// isAdminEmail checks whether email exactly matches any entry in the configured
// admin email list (case-insensitive). If no admin emails are configured,
// access is denied.
func isAdminEmail(email string) bool {
	if GlobalConfig == nil || len(GlobalConfig.Auth.AdminEmails) == 0 {
		return false
	}
	e := strings.ToLower(strings.TrimSpace(email))
	if e == "" {
		return false
	}
	for _, ae := range GlobalConfig.Auth.AdminEmails {
		if strings.ToLower(strings.TrimSpace(ae)) == e {
			return true
		}
	}
	return false
}
