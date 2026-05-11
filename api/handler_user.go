package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/gorm"
)

type apiKeyCreateRequest struct {
	Name string `json:"name"`
}

type apiKeyListItem struct {
	ID         uint       `json:"id"`
	Name       string     `json:"name"`
	Key        string     `json:"key"`
	KeyPrefix  string     `json:"key_prefix"`
	Status     string     `json:"status"`
	GroupID    *uint      `json:"group_id,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

type apiKeyCreateResponse struct {
	ID        uint      `json:"id"`
	Name      string    `json:"name"`
	Key       string    `json:"key"`
	KeyPrefix string    `json:"key_prefix"`
	CreatedAt time.Time `json:"created_at"`
}

type balanceHistoryItem struct {
	ID            uint      `json:"id"`
	UserID        uint      `json:"user_id"`
	Amount        float64   `json:"amount"`
	Type          string    `json:"type"`
	Kind          string    `json:"kind"`
	Reference     string    `json:"reference"`
	Note          string    `json:"note"`
	BalanceBefore float64   `json:"balance_before"`
	BalanceAfter  float64   `json:"balance_after"`
	OperatorEmail string    `json:"operator_email"`
	CreatedAt     time.Time `json:"created_at"`
}

// RegisterUserRoutes wires authenticated panel user endpoints onto a Gin router group.
func (pr *PanelRouter) RegisterUserRoutes(rg *gin.RouterGroup) {
	rg.GET("/user/profile", pr.UserProfileHandler)
	rg.GET("/user/api-keys", pr.ListAPIKeysHandler)
	rg.POST("/user/api-keys", pr.CreateAPIKeyHandler)
	rg.DELETE("/user/api-keys/:id", pr.DeleteAPIKeyHandler)
	rg.PATCH("/user/api-keys/:id/group", pr.RebindAPIKeyGroupHandler)
	rg.GET("/user/available-groups", pr.AvailableGroupsHandler)
	rg.GET("/user/usage", pr.UsageHandler)
	rg.GET("/user/usage/detail", pr.UsageDetailHandler)
	rg.GET("/user/usage/stats", pr.UsageStatsHandler)
	rg.GET("/user/usage/trend", pr.UserUsageTrendHandler)
	rg.GET("/user/usage/models", pr.UserUsageModelsHandler)
	rg.GET("/user/balance-history", pr.BalanceHistoryHandler)
	rg.GET("/user/announcements", pr.UserAnnouncementsHandler)
	rg.GET("/user/models", pr.ModelsHandler)
	rg.GET("/user/tickets", pr.UserListTicketsHandler)
	rg.POST("/user/tickets", pr.UserCreateTicketHandler)
	rg.GET("/user/tickets/:id", pr.UserGetTicketHandler)
	rg.POST("/user/tickets/:id/replies", pr.UserCreateTicketReplyHandler)
	rg.POST("/user/ticket-images", pr.UserUploadTicketImageHandler)
}

type rebindGroupRequest struct {
	GroupID *uint `json:"group_id"`
}

type availableGroupItem struct {
	ID                  uint     `json:"id"`
	Name                string   `json:"name"`
	Description         string   `json:"description"`
	SubscriptionType    string   `json:"subscription_type"`
	RateMultiplier      float64  `json:"rate_multiplier"`
	DailyLimitUSD       *float64 `json:"daily_limit_usd"`
	WeeklyLimitUSD      *float64 `json:"weekly_limit_usd"`
	MonthlyLimitUSD     *float64 `json:"monthly_limit_usd"`
	DefaultValidityDays int      `json:"default_validity_days"`
}

type trendPoint struct {
	Date     string  `json:"date"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
}

type modelPoint struct {
	Model    string  `json:"model"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost"`
}

type usageDetailStats struct {
	TotalRequests     int64   `json:"total_requests"`
	TotalInputTokens  int64   `json:"total_input_tokens"`
	TotalOutputTokens int64   `json:"total_output_tokens"`
	TotalTokens       int64   `json:"total_tokens"`
	TotalCost         float64 `json:"total_cost"`
	TotalActualCost   float64 `json:"total_actual_cost"`
	SuccessCount      int64   `json:"success_count"`
	FailCount         int64   `json:"fail_count"`
	AvgDurationMs     float64 `json:"avg_duration_ms"`
}

func (pr *PanelRouter) UserProfileHandler(c *gin.Context) {
	bc, ok := pr.requireBillingCtx(c)
	if !ok {
		return
	}
	if pr.DB == nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "database not initialized")
		return
	}

	var user model.User
	if err := pr.DB.WithContext(c.Request.Context()).First(&user, bc.UserID).Error; err != nil {
		handleRecordError(c, err, "failed to load user")
		return
	}

	if pr.isAdminEmail(user.Email) && user.Role != "admin" {
		pr.DB.WithContext(c.Request.Context()).Model(&user).Update("role", "admin")
		user.Role = "admin"
	}

	available := user.Balance
	if pr.Ledger != nil {
		balance, err := pr.Ledger.GetBalance(c.Request.Context(), user.ID)
		if err != nil {
			Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load balance")
			return
		}
		available = balance
	}

	Success(c, gin.H{"user": authUserFromModel(user), "available_balance": available})
}

func (pr *PanelRouter) ListAPIKeysHandler(c *gin.Context) {
	bc, ok := pr.requireBillingCtx(c)
	if !ok {
		return
	}

	var keys []model.ApiKey
	if err := pr.DB.WithContext(c.Request.Context()).Where("user_id = ? AND status <> ?", bc.UserID, "revoked").Order("created_at DESC").Find(&keys).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list API keys")
		return
	}

	items := make([]apiKeyListItem, 0, len(keys))
	for _, key := range keys {
		items = append(items, apiKeyListItem{
			ID:         key.ID,
			Name:       key.Name,
			Key:        key.KeyPrefix + "****",
			KeyPrefix:  key.KeyPrefix,
			Status:     key.Status,
			GroupID:    key.GroupID,
			LastUsedAt: key.LastUsedAt,
			CreatedAt:  key.CreatedAt,
		})
	}

	Success(c, gin.H{
		"items":       items,
		"page":        1,
		"page_size":   len(items),
		"total":       len(items),
		"total_pages": 1,
	})
}

func (pr *PanelRouter) CreateAPIKeyHandler(c *gin.Context) {
	bc, ok := pr.requireBillingCtx(c)
	if !ok {
		return
	}

	var req apiKeyCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid request body")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "name is required")
		return
	}

	plaintext, apiKey, err := pr.GenerateAPIKey(bc.UserID, name, nil)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to create API key")
		return
	}

	Success(c, apiKeyCreateResponse{
		ID:        apiKey.ID,
		Name:      apiKey.Name,
		Key:       plaintext,
		KeyPrefix: apiKey.KeyPrefix,
		CreatedAt: apiKey.CreatedAt,
	})
}

func (pr *PanelRouter) DeleteAPIKeyHandler(c *gin.Context) {
	bc, ok := pr.requireBillingCtx(c)
	if !ok {
		return
	}

	id, err := parseUintParam(c, "id")
	if err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid API key id")
		return
	}

	res := pr.DB.WithContext(c.Request.Context()).Model(&model.ApiKey{}).Where("id = ? AND user_id = ?", id, bc.UserID).Update("status", "revoked")
	if res.Error != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to revoke API key")
		return
	}
	if res.RowsAffected == 0 {
		Error(c, http.StatusNotFound, apiErrorNotFound, "API key not found")
		return
	}

	Success(c, gin.H{"revoked": true})
}

func (pr *PanelRouter) AvailableGroupsHandler(c *gin.Context) {
	if _, ok := pr.requireBillingCtx(c); !ok {
		return
	}

	var groups []model.Group
	if err := pr.DB.WithContext(c.Request.Context()).Order("id ASC").Find(&groups).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list groups")
		return
	}

	items := make([]availableGroupItem, 0, len(groups))
	for _, g := range groups {
		items = append(items, availableGroupItem{
			ID:                  g.ID,
			Name:                g.Name,
			Description:         "",
			SubscriptionType:    "standard",
			RateMultiplier:      g.RateMultiplier,
			DailyLimitUSD:       nil,
			WeeklyLimitUSD:      nil,
			MonthlyLimitUSD:     nil,
			DefaultValidityDays: 30,
		})
	}

	Success(c, items)
}

func (pr *PanelRouter) RebindAPIKeyGroupHandler(c *gin.Context) {
	bc, ok := pr.requireBillingCtx(c)
	if !ok {
		return
	}

	id, err := parseUintParam(c, "id")
	if err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid API key id")
		return
	}

	var req rebindGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid request body")
		return
	}

	var key model.ApiKey
	if err := pr.DB.WithContext(c.Request.Context()).Where("id = ? AND user_id = ?", id, bc.UserID).First(&key).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			Error(c, http.StatusNotFound, apiErrorNotFound, "API key not found")
			return
		}
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load API key")
		return
	}

	if req.GroupID != nil {
		var group model.Group
		if err := pr.DB.WithContext(c.Request.Context()).First(&group, *req.GroupID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				Error(c, http.StatusBadRequest, apiErrorBadRequest, "group not found")
				return
			}
			Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to validate group")
			return
		}
	}

	if err := pr.DB.WithContext(c.Request.Context()).Model(&key).Update("group_id", req.GroupID).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to update API key group")
		return
	}
	key.GroupID = req.GroupID

	Success(c, gin.H{
		"id":       key.ID,
		"group_id": key.GroupID,
	})
}

func (pr *PanelRouter) UsageHandler(c *gin.Context) {
	bc, ok := pr.requireBillingCtx(c)
	if !ok {
		return
	}

	page := queryInt(c, "page", 1, 1, 1000000)
	pageSize := queryInt(c, "page_size", 20, 1, 100)
	offset := (page - 1) * pageSize

	var total int64
	base := pr.DB.WithContext(c.Request.Context()).Model(&model.UsageLog{}).Where("user_id = ?", bc.UserID)
	if err := base.Count(&total).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count usage")
		return
	}

	var logs []model.UsageLog
	if err := base.Order("created_at DESC").Limit(pageSize).Offset(offset).Find(&logs).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list usage")
		return
	}

	Success(c, gin.H{
		"items":       logs,
		"page":        page,
		"page_size":   pageSize,
		"total":       total,
		"total_pages": int(math.Ceil(float64(total) / float64(pageSize))),
	})
}

func (pr *PanelRouter) UsageDetailHandler(c *gin.Context) {
	bc, ok := pr.requireBillingCtx(c)
	if !ok {
		return
	}

	page := queryInt(c, "page", 1, 1, 1000000)
	pageSize := queryInt(c, "page_size", 20, 1, 100)
	offset := (page - 1) * pageSize

	base := pr.DB.WithContext(c.Request.Context()).Model(&model.UsageLog{}).Where("usage_logs.user_id = ?", bc.UserID)
	base, valid := pr.applyUsageDetailFilters(c, base, bc.UserID)
	if !valid {
		return
	}

	var total int64
	if err := base.Count(&total).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count usage details")
		return
	}

	var logs []model.UsageLog
	if err := base.Order("usage_logs.created_at DESC").Limit(pageSize).Offset(offset).Find(&logs).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list usage details")
		return
	}

	stats, err := usageDetailStatsForQuery(base, total)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load usage detail stats")
		return
	}

	keyNames, err := pr.apiKeyNamesForUsageLogs(c, bc.UserID, logs)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load API key names")
		return
	}

	items := make([]gin.H, 0, len(logs))
	for _, log := range logs {
		apiKeyName := keyNames[log.ApiKeyID]
		items = append(items, gin.H{
			"id":               log.ID,
			"request_id":       log.RequestID,
			"provider":         log.Provider,
			"model":            log.Model,
			"api_key_id":       log.ApiKeyID,
			"api_key_name":     apiKeyName,
			"input_tokens":     usageInputTokens(log),
			"output_tokens":    usageOutputTokens(log),
			"reasoning_tokens": log.ReasoningTokens,
			"cached_tokens":    log.CachedTokens,
			"input_cost":       log.InputCost,
			"output_cost":      log.OutputCost,
			"total_cost":       usageTotalCost(log),
			"actual_cost":      usageChargedCost(log),
			"cost":             log.Cost,
			"rate_multiplier":  usageRateMultiplier(log),
			"stream":           log.Stream,
			"duration_ms":      log.DurationMs,
			"failed":           log.Failed,
			"created_at":       log.CreatedAt,
		})
	}

	Success(c, gin.H{
		"items":       items,
		"page":        page,
		"page_size":   pageSize,
		"total":       total,
		"total_pages": int(math.Ceil(float64(total) / float64(pageSize))),
		"stats":       stats,
	})
}

func (pr *PanelRouter) UsageStatsHandler(c *gin.Context) {
	bc, ok := pr.requireBillingCtx(c)
	if !ok {
		return
	}

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	week := today.AddDate(0, 0, -6)
	month := today.AddDate(0, 0, -29)

	todayStats, err := pr.usageStatsSince(c, bc.UserID, today)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load today usage stats")
		return
	}
	weekStats, err := pr.usageStatsSince(c, bc.UserID, week)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load week usage stats")
		return
	}
	monthStats, err := pr.usageStatsSince(c, bc.UserID, month)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load month usage stats")
		return
	}

	Success(c, gin.H{"today": todayStats, "week": weekStats, "month": monthStats})
}

func (pr *PanelRouter) UserUsageTrendHandler(c *gin.Context) {
	bc, ok := pr.requireBillingCtx(c)
	if !ok {
		return
	}
	days := queryInt(c, "days", 7, 1, 30)
	points, err := pr.buildUsageTrend(c, bc.UserID, days)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load usage trend")
		return
	}
	Success(c, points)
}

func (pr *PanelRouter) UserUsageModelsHandler(c *gin.Context) {
	bc, ok := pr.requireBillingCtx(c)
	if !ok {
		return
	}
	days := queryInt(c, "days", 30, 1, 90)
	items, err := pr.buildUsageModels(c, bc.UserID, days)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load usage models")
		return
	}
	Success(c, items)
}

func (pr *PanelRouter) UserAnnouncementsHandler(c *gin.Context) {
	if _, ok := pr.requireBillingCtx(c); !ok {
		return
	}
	Success(c, []gin.H{{
		"id":    1,
		"title": "欢迎使用 CPA Gateway",
		"type":  "info",
	}})
}

// visibleCatalogModelIDsSorted returns distinct visible catalog model IDs
// (excluding the models_url sentinel row), sorted.
func (pr *PanelRouter) visibleCatalogModelIDsSorted(ctx context.Context) ([]string, error) {
	if pr.DB == nil {
		return nil, errors.New("database not initialized")
	}
	var entries []model.ModelCatalogEntry
	if err := pr.DB.WithContext(ctx).
		Where("visible = ? AND model_id <> ?", true, "__models_url__").
		Find(&entries).Error; err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	for _, e := range entries {
		id := strings.TrimSpace(e.ModelID)
		if id == "" {
			continue
		}
		seen[id] = struct{}{}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// listPanelModelCatalog loads visible models with per-million token prices.
func (pr *PanelRouter) listPanelModelCatalog(ctx context.Context) ([]gin.H, error) {
	ids, err := pr.visibleCatalogModelIDsSorted(ctx)
	if err != nil {
		return nil, err
	}
	var prices []model.ModelPrice
	if len(ids) > 0 {
		if err := pr.DB.WithContext(ctx).Where("model_id IN ?", ids).Find(&prices).Error; err != nil {
			return nil, err
		}
	}
	byID := make(map[string]model.ModelPrice, len(prices))
	for _, p := range prices {
		byID[p.ModelID] = p
	}
	out := make([]gin.H, 0, len(ids))
	for _, id := range ids {
		p := byID[id]
		out = append(out, gin.H{
			"id":                        id,
			"input_price_per_1m":        p.InputPricePer1M,
			"output_price_per_1m":       p.OutputPricePer1M,
			"cached_input_price_per_1m": p.CachedInputPricePer1M,
			"reasoning_price_per_1m":    p.ReasoningPricePer1M,
		})
	}
	return out, nil
}

func (pr *PanelRouter) ModelsHandler(c *gin.Context) {
	bc, ok := pr.requireBillingCtx(c)
	if !ok {
		return
	}
	models, err := pr.listPanelModelCatalog(c.Request.Context())
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load model catalog")
		return
	}
	Success(c, gin.H{
		"models":          models,
		"rate_multiplier": bc.RateMult,
	})
}

func (pr *PanelRouter) BalanceHistoryHandler(c *gin.Context) {
	bc, ok := pr.requireBillingCtx(c)
	if !ok {
		return
	}

	page := queryInt(c, "page", 1, 1, 1000000)
	pageSize := queryInt(c, "page_size", 20, 1, 100)
	kind := strings.TrimSpace(c.Query("kind"))

	db := pr.DB.WithContext(c.Request.Context()).Model(&model.BalanceLog{}).Where("user_id = ?", bc.UserID)
	if kind != "" {
		db = db.Where("type = ?", kind)
	}

	var total int64
	if err := db.Count(&total).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count balance logs")
		return
	}

	// Load all matching logs ASC to compute deterministic running balances.
	var allLogs []model.BalanceLog
	if err := db.Order("created_at ASC, id ASC").Find(&allLogs).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list balance logs")
		return
	}

	type enriched struct {
		BalanceBefore float64
		BalanceAfter  float64
		Log           model.BalanceLog
	}
	allItems := make([]enriched, 0, len(allLogs))
	var running float64
	for _, log := range allLogs {
		before := running
		running += log.Amount
		allItems = append(allItems, enriched{BalanceBefore: before, BalanceAfter: running, Log: log})
	}
	for i, j := 0, len(allItems)-1; i < j; i, j = i+1, j-1 {
		allItems[i], allItems[j] = allItems[j], allItems[i]
	}

	offset := (page - 1) * pageSize
	if offset > len(allItems) {
		offset = len(allItems)
	}
	end := offset + pageSize
	if end > len(allItems) {
		end = len(allItems)
	}
	pageItems := allItems[offset:end]

	items := make([]balanceHistoryItem, 0, len(pageItems))
	for _, e := range pageItems {
		items = append(items, balanceHistoryItem{
			ID:            e.Log.ID,
			UserID:        e.Log.UserID,
			Amount:        e.Log.Amount,
			Type:          e.Log.Type,
			Kind:          e.Log.Type,
			Reference:     e.Log.Reference,
			Note:          e.Log.Reference,
			BalanceBefore: e.BalanceBefore,
			BalanceAfter:  e.BalanceAfter,
			OperatorEmail: "",
			CreatedAt:     e.Log.CreatedAt,
		})
	}

	Success(c, gin.H{
		"items":       items,
		"page":        page,
		"page_size":   pageSize,
		"total":       total,
		"total_pages": int(math.Ceil(float64(total) / float64(pageSize))),
	})
}

func (pr *PanelRouter) requireBillingCtx(c *gin.Context) (*BillingCtx, bool) {
	bc, ok := billingContextFromGin(c)
	if !ok || bc.UserID == 0 {
		Error(c, http.StatusUnauthorized, apiErrorUnauthorized, "authentication context required")
		return nil, false
	}
	if pr.DB == nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "database not initialized")
		return nil, false
	}
	return bc, true
}

func parseUintParam(c *gin.Context, name string) (uint, error) {
	value, err := strconv.ParseUint(c.Param(name), 10, 64)
	if err != nil || value == 0 {
		return 0, err
	}
	return uint(value), nil
}

func queryInt(c *gin.Context, name string, def, min, max int) int {
	value, err := strconv.Atoi(c.Query(name))
	if err != nil {
		value = def
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func (pr *PanelRouter) applyUsageDetailFilters(c *gin.Context, q *gorm.DB, userID uint) (*gorm.DB, bool) {
	if apiKeyIDRaw := strings.TrimSpace(c.Query("api_key_id")); apiKeyIDRaw != "" {
		apiKeyID, err := strconv.ParseUint(apiKeyIDRaw, 10, 64)
		if err != nil || apiKeyID == 0 {
			Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid api_key_id")
			return nil, false
		}
		var count int64
		if err := pr.DB.WithContext(c.Request.Context()).Model(&model.ApiKey{}).Where("id = ? AND user_id = ?", uint(apiKeyID), userID).Count(&count).Error; err != nil {
			Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to validate API key")
			return nil, false
		}
		if count == 0 {
			Error(c, http.StatusBadRequest, apiErrorBadRequest, "api_key_id does not belong to current user")
			return nil, false
		}
		q = q.Where("usage_logs.api_key_id = ?", uint(apiKeyID))
	}

	if modelFilter := strings.TrimSpace(c.Query("model")); modelFilter != "" {
		q = q.Where("usage_logs.model ILIKE ?", "%"+modelFilter+"%")
	}

	if startDate := strings.TrimSpace(c.Query("start_date")); startDate != "" {
		start, err := time.ParseInLocation("2006-01-02", startDate, time.Local)
		if err != nil {
			Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid start_date")
			return nil, false
		}
		q = q.Where("usage_logs.created_at >= ?", start)
	}

	if endDate := strings.TrimSpace(c.Query("end_date")); endDate != "" {
		end, err := time.ParseInLocation("2006-01-02", endDate, time.Local)
		if err != nil {
			Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid end_date")
			return nil, false
		}
		q = q.Where("usage_logs.created_at < ?", end.AddDate(0, 0, 1))
	}

	switch status := strings.TrimSpace(c.Query("status")); status {
	case "", "all":
	case "success":
		q = q.Where("usage_logs.failed = ?", false)
	case "failed":
		q = q.Where("usage_logs.failed = ?", true)
	default:
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid status")
		return nil, false
	}

	return q, true
}

func usageDetailStatsForQuery(q *gorm.DB, total int64) (usageDetailStats, error) {
	type row struct {
		InputTokens     int64
		OutputTokens    int64
		TotalCost       float64
		TotalActualCost float64
		SuccessCount    int64
		FailCount       int64
		AvgDurationMs   float64
	}
	var r row
	err := q.Session(&gorm.Session{}).Select(`
		COALESCE(SUM(CASE WHEN input_tokens > 0 THEN input_tokens ELSE tokens_in END), 0) AS input_tokens,
		COALESCE(SUM(CASE WHEN output_tokens > 0 THEN output_tokens ELSE tokens_out END), 0) AS output_tokens,
		COALESCE(SUM(CASE WHEN total_cost > 0 THEN total_cost ELSE cost END), 0) AS total_cost,
		COALESCE(SUM(CASE WHEN actual_cost > 0 THEN actual_cost ELSE cost END), 0) AS total_actual_cost,
		COALESCE(SUM(CASE WHEN failed = false THEN 1 ELSE 0 END), 0) AS success_count,
		COALESCE(SUM(CASE WHEN failed = true THEN 1 ELSE 0 END), 0) AS fail_count,
		COALESCE(AVG(NULLIF(duration_ms, 0)), 0) AS avg_duration_ms
	`).Scan(&r).Error
	if err != nil {
		return usageDetailStats{}, err
	}
	return usageDetailStats{
		TotalRequests:     total,
		TotalInputTokens:  r.InputTokens,
		TotalOutputTokens: r.OutputTokens,
		TotalTokens:       r.InputTokens + r.OutputTokens,
		TotalCost:         r.TotalCost,
		TotalActualCost:   r.TotalActualCost,
		SuccessCount:      r.SuccessCount,
		FailCount:         r.FailCount,
		AvgDurationMs:     r.AvgDurationMs,
	}, nil
}

func (pr *PanelRouter) apiKeyNamesForUsageLogs(c *gin.Context, userID uint, logs []model.UsageLog) (map[uint]string, error) {
	ids := make([]uint, 0, len(logs))
	seen := map[uint]bool{}
	for _, log := range logs {
		if log.ApiKeyID == 0 || seen[log.ApiKeyID] {
			continue
		}
		seen[log.ApiKeyID] = true
		ids = append(ids, log.ApiKeyID)
	}
	if len(ids) == 0 {
		return map[uint]string{}, nil
	}

	var keys []model.ApiKey
	if err := pr.DB.WithContext(c.Request.Context()).Where("user_id = ? AND id IN ?", userID, ids).Find(&keys).Error; err != nil {
		return nil, err
	}
	names := make(map[uint]string, len(keys))
	for _, key := range keys {
		names[key.ID] = key.Name
	}
	return names, nil
}

func usageInputTokens(log model.UsageLog) int {
	if log.InputTokens > 0 {
		return log.InputTokens
	}
	return log.TokensIn
}

func usageOutputTokens(log model.UsageLog) int {
	if log.OutputTokens > 0 {
		return log.OutputTokens
	}
	return log.TokensOut
}

func usageTotalCost(log model.UsageLog) float64 {
	if log.TotalCost > 0 {
		return log.TotalCost
	}
	return log.Cost
}

func usageChargedCost(log model.UsageLog) float64 {
	if log.ActualCost > 0 {
		return log.ActualCost
	}
	return log.Cost
}

func usageRateMultiplier(log model.UsageLog) float64 {
	if log.RateMultiplier > 0 {
		return log.RateMultiplier
	}
	return 1.0
}

func (pr *PanelRouter) usageStatsSince(c *gin.Context, userID uint, since time.Time) (gin.H, error) {
	type statsRow struct {
		Requests  int64
		TokensIn  int
		TokensOut int
		Cost      float64
	}

	var row statsRow
	err := pr.DB.WithContext(c.Request.Context()).Model(&model.UsageLog{}).
		Select("COUNT(*) AS requests, COALESCE(SUM(CASE WHEN input_tokens > 0 THEN input_tokens ELSE tokens_in END), 0) AS tokens_in, COALESCE(SUM(CASE WHEN output_tokens > 0 THEN output_tokens ELSE tokens_out END), 0) AS tokens_out, COALESCE(SUM(CASE WHEN actual_cost > 0 THEN actual_cost ELSE cost END), 0) AS cost").
		Where("user_id = ? AND created_at >= ?", userID, since).
		Scan(&row).Error
	if err != nil {
		return nil, err
	}

	return gin.H{
		"requests":   row.Requests,
		"tokens_in":  row.TokensIn,
		"tokens_out": row.TokensOut,
		"tokens":     row.TokensIn + row.TokensOut,
		"cost":       row.Cost,
	}, nil
}

func (pr *PanelRouter) buildUsageTrend(c *gin.Context, userID uint, days int) ([]trendPoint, error) {
	since := time.Now().AddDate(0, 0, -(days - 1))
	start := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.Local)

	q := pr.DB.WithContext(c.Request.Context()).Model(&model.UsageLog{}).Where("created_at >= ?", start)
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	}

	var logs []model.UsageLog
	if err := q.Find(&logs).Error; err != nil {
		return nil, err
	}

	bucket := make(map[string]*trendPoint, days)
	for i := 0; i < days; i++ {
		d := start.AddDate(0, 0, i).Format("2006-01-02")
		bucket[d] = &trendPoint{Date: d}
	}
	for _, row := range logs {
		d := row.CreatedAt.In(time.Local).Format("2006-01-02")
		p, ok := bucket[d]
		if !ok {
			continue
		}
		p.Requests++
		p.Tokens += int64(usageInputTokens(row) + usageOutputTokens(row))
		p.Cost += usageChargedCost(row)
	}

	points := make([]trendPoint, 0, len(bucket))
	for i := 0; i < days; i++ {
		d := start.AddDate(0, 0, i).Format("2006-01-02")
		points = append(points, *bucket[d])
	}
	return points, nil
}

func (pr *PanelRouter) buildUsageModels(c *gin.Context, userID uint, days int) ([]modelPoint, error) {
	since := time.Now().AddDate(0, 0, -(days - 1))
	start := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.Local)

	q := pr.DB.WithContext(c.Request.Context()).Model(&model.UsageLog{}).Where("created_at >= ?", start)
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	}

	var logs []model.UsageLog
	if err := q.Find(&logs).Error; err != nil {
		return nil, err
	}

	table := map[string]*modelPoint{}
	for _, row := range logs {
		name := strings.TrimSpace(row.Model)
		if name == "" {
			name = "unknown"
		}
		cur, ok := table[name]
		if !ok {
			cur = &modelPoint{Model: name}
			table[name] = cur
		}
		cur.Requests++
		cur.Tokens += int64(usageInputTokens(row) + usageOutputTokens(row))
		cur.Cost += usageChargedCost(row)
	}

	items := make([]modelPoint, 0, len(table))
	for _, v := range table {
		items = append(items, *v)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Requests == items[j].Requests {
			return items[i].Model < items[j].Model
		}
		return items[i].Requests > items[j].Requests
	})
	if len(items) > 20 {
		items = items[:20]
	}
	return items, nil
}

func handleRecordError(c *gin.Context, err error, fallback string) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		Error(c, http.StatusNotFound, apiErrorNotFound, "user not found")
		return
	}
	Error(c, http.StatusInternalServerError, apiErrorInternal, fallback)
}
