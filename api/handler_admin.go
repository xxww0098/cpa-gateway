package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/gorm"
)

func (pr *PanelRouter) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/dashboard", pr.AdminDashboardHandler)
	rg.GET("/admin/usage/trend", pr.AdminUsageTrendHandler)
	rg.GET("/admin/usage/models", pr.AdminUsageModelsHandler)
	rg.GET("/admin/users", pr.AdminUsersListHandler)
	rg.POST("/admin/users", pr.AdminUsersCreateHandler)
	rg.PUT("/admin/users/:id", pr.AdminUsersUpdateHandler)
	rg.DELETE("/admin/users/:id", pr.AdminUsersDeleteHandler)
	rg.POST("/admin/users/:id/deposit", pr.AdminUsersDepositHandler)
	rg.GET("/admin/users/:id/api-keys", pr.AdminUsersAPIKeysHandler)
	rg.GET("/admin/users/:id/balance-history", pr.AdminUsersBalanceHistoryHandler)
	rg.GET("/admin/subscriptions", pr.AdminSubscriptionsListHandler)
	rg.POST("/admin/subscriptions", pr.AdminSubscriptionsCreateHandler)
	rg.PUT("/admin/subscriptions/:id/extend", pr.AdminSubscriptionsExtendHandler)
	rg.DELETE("/admin/subscriptions/:id", pr.AdminSubscriptionsRevokeHandler)
	rg.POST("/admin/subscriptions/:id/reactivate", pr.AdminSubscriptionsReactivateHandler)
	rg.PUT("/admin/subscriptions/:id/reactivate", pr.AdminSubscriptionsReactivateHandler)
	rg.POST("/admin/subscriptions/:id/reset-quota", pr.AdminSubscriptionsResetQuotaHandler)
	rg.GET("/admin/groups", pr.AdminGroupsListHandler)
	rg.POST("/admin/groups", pr.AdminGroupsCreateHandler)
	rg.PUT("/admin/groups/:id", pr.AdminGroupsUpdateHandler)
	rg.DELETE("/admin/groups/:id", pr.AdminGroupsDeleteHandler)
	rg.GET("/admin/tickets", pr.AdminListTicketsHandler)
	rg.GET("/admin/tickets/:id", pr.AdminGetTicketHandler)
	rg.POST("/admin/tickets/:id/replies", pr.AdminCreateTicketReplyHandler)
	rg.PUT("/admin/tickets/:id/status", pr.AdminUpdateTicketStatusHandler)
	rg.PUT("/admin/tickets/:id/assign", pr.AdminAssignTicketHandler)
	rg.GET("/admin/ticket-quick-replies", pr.AdminTicketQuickRepliesGetHandler)
	rg.POST("/admin/ticket-quick-replies", pr.AdminTicketQuickRepliesSaveHandler)
}

func (pr *PanelRouter) AdminDashboardHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}

	var userTotal int64
	if err := pr.DB.WithContext(c.Request.Context()).Model(&model.User{}).Count(&userTotal).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count users")
		return
	}

	var userActive int64
	if err := pr.DB.WithContext(c.Request.Context()).Model(&model.User{}).Where("status = ?", userStatusActive).Count(&userActive).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count active users")
		return
	}

	var keyTotal int64
	if err := pr.DB.WithContext(c.Request.Context()).Model(&model.ApiKey{}).Count(&keyTotal).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count api keys")
		return
	}

	var keyActive int64
	if err := pr.DB.WithContext(c.Request.Context()).Model(&model.ApiKey{}).Where("status = ?", "active").Count(&keyActive).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count active api keys")
		return
	}

	today := time.Now()
	todayStart := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())
	weekStart := todayStart.AddDate(0, 0, -6)

	var todayReq int64
	if err := pr.DB.WithContext(c.Request.Context()).Model(&model.UsageLog{}).Where("created_at >= ?", todayStart).Count(&todayReq).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load today usage")
		return
	}

	var todayCost float64
	if err := pr.DB.WithContext(c.Request.Context()).Model(&model.UsageLog{}).
		Select("COALESCE(SUM(cost), 0)").
		Where("created_at >= ?", todayStart).
		Scan(&todayCost).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load today usage cost")
		return
	}

	var weekReq int64
	if err := pr.DB.WithContext(c.Request.Context()).Model(&model.UsageLog{}).Where("created_at >= ?", weekStart).Count(&weekReq).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load week usage")
		return
	}

	Success(c, gin.H{
		"users":    gin.H{"total": userTotal, "active": userActive},
		"api_keys": gin.H{"total": keyTotal, "active": keyActive},
		"usage":    gin.H{"today_requests": todayReq, "today_cost": todayCost, "week_requests": weekReq},
	})
}

func (pr *PanelRouter) AdminUsageTrendHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	days := queryInt(c, "days", 7, 1, 30)
	points, err := pr.buildUsageTrend(c, 0, days)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load usage trend")
		return
	}
	Success(c, points)
}

func (pr *PanelRouter) AdminUsageModelsHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	days := queryInt(c, "days", 30, 1, 90)
	items, err := pr.buildUsageModels(c, 0, days)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load usage models")
		return
	}
	Success(c, items)
}

// requireAdmin verifies the authenticated user is an admin. Returns false and
// writes an error response when not.
func (pr *PanelRouter) requireAdmin(c *gin.Context) bool {
	bc, ok := pr.requireBillingCtx(c)
	if !ok {
		return false
	}

	isAdmin, err := pr.userHasAdminRole(c.Request.Context(), bc.UserID)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to verify admin permission")
		return false
	}
	if isAdmin {
		return true
	}

	Error(c, http.StatusForbidden, apiErrorUnauthorized, "admin permission required")
	return false
}

// isAdminCaller is a non-writing admin probe: it returns true if the caller's
// users row has Role=admin and Status=active. Unlike requireAdmin it never
// writes an error response — handlers that want to branch their output shape
// (e.g. AvailableGroupsHandler returning the full vs. entitlement-filtered group
// list per Requirement 3.3) use this to discover the caller role without
// terminating the request. Any DB failure is treated as "not admin" because
// this is only a filter predicate.
func (pr *PanelRouter) isAdminCaller(c *gin.Context, bc *BillingCtx) bool {
	if pr == nil || bc == nil {
		return false
	}
	isAdmin, err := pr.userHasAdminRole(c.Request.Context(), bc.UserID)
	return err == nil && isAdmin
}

func (pr *PanelRouter) userHasAdminRole(ctx context.Context, userID uint) (bool, error) {
	if pr == nil || pr.DB == nil || userID == 0 {
		return false, nil
	}

	var user model.User
	err := pr.DB.WithContext(ctx).
		Select("role", "status").
		First(&user, userID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return user.Status == userStatusActive && strings.EqualFold(strings.TrimSpace(user.Role), "admin"), nil
}
