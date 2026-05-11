package main

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/gorm"
)

type notificationItem struct {
	ID               uint      `json:"id"`
	Title            string    `json:"title"`
	Content          string    `json:"content"`
	IsRead           bool      `json:"is_read"`
	NotificationType string    `json:"notification_type"`
	CreatedAt        time.Time `json:"created_at"`
}

type announcementRecord struct {
	ID        uint      `json:"id"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	Type      string    `json:"type"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

type refundRecord struct {
	ID             uint       `json:"id"`
	UserID         uint       `json:"user_id"`
	SubscriptionID uint       `json:"subscription_id"`
	Amount         float64    `json:"amount"`
	Reason         string     `json:"reason"`
	Status         string     `json:"status"`
	DaysUsed       int        `json:"days_used"`
	TotalDays      int        `json:"total_days"`
	DailyRate      float64    `json:"daily_rate"`
	ProcessedAt    *time.Time `json:"processed_at"`
	ProcessedBy    *uint      `json:"processed_by"`
	CreatedAt      time.Time  `json:"created_at"`
}

type paymentOrderRecord struct {
	ID            uint       `json:"id"`
	UserID        uint       `json:"user_id"`
	Provider      string     `json:"provider"`
	AmountUSD     float64    `json:"amount_usd"`
	AmountLocal   float64    `json:"amount_local"`
	Currency      string     `json:"currency"`
	Status        string     `json:"status"`
	TransactionID *string    `json:"transaction_id"`
	Metadata      *string    `json:"metadata"`
	PaidAt        *time.Time `json:"paid_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type redeemCodeRecord struct {
	ID        uint       `json:"id"`
	Code      string     `json:"code"`
	Amount    float64    `json:"amount"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
	UsedBy    *string    `json:"used_by,omitempty"`
}

type createAnnouncementRequest struct {
	Title    string `json:"title"`
	Content  string `json:"content"`
	Type     string `json:"type"`
	IsActive bool   `json:"is_active"`
}

type applyRefundRequest struct {
	SubscriptionID uint   `json:"subscription_id"`
	Reason         string `json:"reason"`
}

type createRedeemCodeRequest struct {
	Count  int     `json:"count"`
	Amount float64 `json:"amount"`
}

var opsStore = struct {
	mu            sync.Mutex
	nextAnnID     uint
	nextRefundID  uint
	nextRedeemID  uint
	nextOrderID   uint
	notifications []notificationItem
	announcements []announcementRecord
	refunds       []refundRecord
	orders        []paymentOrderRecord
	redeemCodes   []redeemCodeRecord
}{
	nextAnnID:    2,
	nextRefundID: 1,
	nextRedeemID: 1,
	nextOrderID:  1,
	notifications: []notificationItem{{
		ID:               1,
		Title:            "欢迎使用 CPA Gateway",
		Content:          "账户面板、API Key、计费和代理转发功能已就绪。",
		IsRead:           false,
		NotificationType: "system",
		CreatedAt:        time.Now(),
	}},
	announcements: []announcementRecord{{
		ID:        1,
		Title:     "欢迎使用 CPA Gateway",
		Content:   "系统维护中请关注通知。",
		Type:      "info",
		IsActive:  true,
		CreatedAt: time.Now(),
	}},
	orders: []paymentOrderRecord{},
}

func RegisterOpsRoutes(rg *gin.RouterGroup) {
	rg.GET("/user/notifications/unread-count", UserUnreadNotificationsHandler)
	rg.GET("/user/notifications", UserNotificationsHandler)
	rg.PUT("/user/notifications/:id/read", UserReadNotificationHandler)
	rg.PUT("/user/notifications/read-all", UserReadAllNotificationsHandler)
	rg.POST("/user/redeem", UserRedeemHandler)
	rg.GET("/refund/list", UserRefundListHandler)
	rg.POST("/refund/apply", UserRefundApplyHandler)

	rg.GET("/admin/announcements", AdminListAnnouncementsHandler)
	rg.POST("/admin/announcements", AdminCreateAnnouncementHandler)
	rg.PUT("/admin/announcements/:id", AdminUpdateAnnouncementHandler)
	rg.DELETE("/admin/announcements/:id", AdminDeleteAnnouncementHandler)
	rg.GET("/admin/orders", AdminListOrdersHandler)
	rg.GET("/admin/usage-logs", AdminUsageLogsHandler)
	rg.GET("/admin/redeem-codes", AdminListRedeemCodesHandler)
	rg.POST("/admin/redeem-codes", AdminCreateRedeemCodesHandler)
	rg.DELETE("/admin/redeem-codes/:id", AdminDeleteRedeemCodeHandler)
	rg.GET("/admin/pricing/groups", AdminListPricingGroupsHandler)
	rg.POST("/admin/pricing/groups", AdminUpsertPricingGroupHandler)
	rg.DELETE("/admin/pricing/groups/:name", AdminDeletePricingGroupHandler)
	rg.GET("/admin/model-catalog/models-url", AdminModelCatalogModelsURLGetHandler)
	rg.PUT("/admin/model-catalog/models-url", AdminModelCatalogModelsURLPutHandler)
	rg.POST("/admin/model-catalog/ensure-openai-channel", AdminModelCatalogEnsureOpenAIChannelHandler)
	rg.POST("/admin/model-catalog/openai-visibility", AdminModelCatalogOpenAIVisibilityHandler)
	rg.POST("/admin/pricing/models", AdminUpsertPricingModelHandler)
	rg.GET("/admin/refunds", AdminListRefundsHandler)
	rg.PUT("/admin/refund/:id/approve", AdminApproveRefundHandler)
	rg.PUT("/admin/refund/:id/reject", AdminRejectRefundHandler)

	rg.GET("/payment/stripe/config", PaymentStripeConfigHandler)
	rg.POST("/payment/stripe/create", PaymentStripeCreateHandler)
	rg.POST("/payment/alipay/create", PaymentAlipayCreateHandler)
	rg.GET("/payment/alipay/status", PaymentAlipayStatusHandler)
	rg.POST("/payment/wechat/create", PaymentWechatCreateHandler)
	rg.GET("/payment/wechat/status", PaymentWechatStatusHandler)
}

func UserUnreadNotificationsHandler(c *gin.Context) {
	opsStore.mu.Lock()
	defer opsStore.mu.Unlock()
	count := 0
	for _, item := range opsStore.notifications {
		if !item.IsRead {
			count++
		}
	}
	Success(c, gin.H{"unread_count": count})
}

func UserNotificationsHandler(c *gin.Context) {
	page := queryInt(c, "page", 1, 1, 1000000)
	pageSize := queryInt(c, "page_size", 10, 1, 100)
	opsStore.mu.Lock()
	items := append([]notificationItem(nil), opsStore.notifications...)
	opsStore.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	start := (page - 1) * pageSize
	if start > len(items) {
		start = len(items)
	}
	end := start + pageSize
	if end > len(items) {
		end = len(items)
	}
	Success(c, gin.H{"items": items[start:end], "total": len(items), "page": page, "page_size": pageSize})
}

func UserReadNotificationHandler(c *gin.Context) {
	id, err := parseUintParam(c, "id")
	if err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid notification id")
		return
	}
	opsStore.mu.Lock()
	defer opsStore.mu.Unlock()
	for i := range opsStore.notifications {
		if opsStore.notifications[i].ID == id {
			opsStore.notifications[i].IsRead = true
			Success(c, gin.H{"ok": true})
			return
		}
	}
	Error(c, http.StatusNotFound, apiErrorNotFound, "notification not found")
}

func UserReadAllNotificationsHandler(c *gin.Context) {
	opsStore.mu.Lock()
	defer opsStore.mu.Unlock()
	for i := range opsStore.notifications {
		opsStore.notifications[i].IsRead = true
	}
	Success(c, gin.H{"ok": true})
}

func UserRedeemHandler(c *gin.Context) {
	bc, ok := requireBillingCtx(c)
	if !ok {
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Code) == "" {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid redeem code")
		return
	}
	code := strings.TrimSpace(req.Code)

	opsStore.mu.Lock()
	idx := -1
	for i := range opsStore.redeemCodes {
		if opsStore.redeemCodes[i].Code == code {
			idx = i
			break
		}
	}
	if idx == -1 {
		opsStore.mu.Unlock()
		Error(c, http.StatusNotFound, apiErrorNotFound, "redeem code not found")
		return
	}
	if opsStore.redeemCodes[idx].Status != "unused" {
		opsStore.mu.Unlock()
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "redeem code already used")
		return
	}
	amount := opsStore.redeemCodes[idx].Amount
	now := time.Now()
	usedBy := bc.Email
	if usedBy == "" {
		usedBy = strconv.FormatUint(uint64(bc.UserID), 10)
	}
	// Mark used while holding the lock to prevent concurrent redemption.
	opsStore.redeemCodes[idx].Status = "used"
	opsStore.redeemCodes[idx].UsedAt = &now
	opsStore.redeemCodes[idx].UsedBy = &usedBy
	opsStore.mu.Unlock()

	if GlobalLedger != nil {
		if err := GlobalLedger.Credit(c.Request.Context(), bc.UserID, amount, "redeem:"+code); err != nil {
			opsStore.mu.Lock()
			// Only roll back if nobody else has reused this code in the interim.
			if idx < len(opsStore.redeemCodes) && opsStore.redeemCodes[idx].Status == "used" && opsStore.redeemCodes[idx].UsedBy != nil && *opsStore.redeemCodes[idx].UsedBy == usedBy {
				opsStore.redeemCodes[idx].Status = "unused"
				opsStore.redeemCodes[idx].UsedAt = nil
				opsStore.redeemCodes[idx].UsedBy = nil
			}
			opsStore.mu.Unlock()
			Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to apply redeem")
			return
		}
	}
	Success(c, gin.H{"amount": amount})
}

func UserRefundListHandler(c *gin.Context) {
	bc, ok := requireBillingCtx(c)
	if !ok {
		return
	}
	opsStore.mu.Lock()
	defer opsStore.mu.Unlock()
	items := make([]refundRecord, 0)
	for _, r := range opsStore.refunds {
		if r.UserID == bc.UserID {
			items = append(items, r)
		}
	}
	Success(c, gin.H{"items": items})
}

func UserRefundApplyHandler(c *gin.Context) {
	bc, ok := requireBillingCtx(c)
	if !ok {
		return
	}
	var req applyRefundRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.SubscriptionID == 0 {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid refund request")
		return
	}
	// Verify the subscription belongs to the authenticated user.
	var sub model.Subscription
	if err := GlobalDB.WithContext(c.Request.Context()).Where("id = ? AND user_id = ?", req.SubscriptionID, bc.UserID).First(&sub).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			Error(c, http.StatusNotFound, apiErrorNotFound, "subscription not found")
		} else {
			Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to look up subscription")
		}
		return
	}
	opsStore.mu.Lock()
	defer opsStore.mu.Unlock()
	now := time.Now()
	rec := refundRecord{ID: opsStore.nextRefundID, UserID: bc.UserID, SubscriptionID: req.SubscriptionID, Amount: 0, Reason: strings.TrimSpace(req.Reason), Status: "pending", DaysUsed: 0, TotalDays: 0, DailyRate: 0, CreatedAt: now}
	opsStore.nextRefundID++
	opsStore.refunds = append(opsStore.refunds, rec)
	Success(c, gin.H{"id": rec.ID, "status": rec.Status})
}

func AdminListAnnouncementsHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	opsStore.mu.Lock()
	defer opsStore.mu.Unlock()
	items := append([]announcementRecord(nil), opsStore.announcements...)
	sort.Slice(items, func(i, j int) bool { return items[i].ID > items[j].ID })
	Success(c, gin.H{"items": items})
}

func AdminCreateAnnouncementHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var req createAnnouncementRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.Content) == "" {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid announcement payload")
		return
	}
	opsStore.mu.Lock()
	defer opsStore.mu.Unlock()
	rec := announcementRecord{ID: opsStore.nextAnnID, Title: strings.TrimSpace(req.Title), Content: strings.TrimSpace(req.Content), Type: strings.TrimSpace(req.Type), IsActive: req.IsActive, CreatedAt: time.Now()}
	if rec.Type == "" {
		rec.Type = "info"
	}
	opsStore.nextAnnID++
	opsStore.announcements = append([]announcementRecord{rec}, opsStore.announcements...)
	Success(c, rec)
}

func AdminUpdateAnnouncementHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	id, err := parseUintParam(c, "id")
	if err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid id")
		return
	}
	var req createAnnouncementRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid payload")
		return
	}
	opsStore.mu.Lock()
	defer opsStore.mu.Unlock()
	for i := range opsStore.announcements {
		if opsStore.announcements[i].ID == id {
			opsStore.announcements[i].Title = strings.TrimSpace(req.Title)
			opsStore.announcements[i].Content = strings.TrimSpace(req.Content)
			if t := strings.TrimSpace(req.Type); t != "" {
				opsStore.announcements[i].Type = t
			}
			opsStore.announcements[i].IsActive = req.IsActive
			Success(c, opsStore.announcements[i])
			return
		}
	}
	Error(c, http.StatusNotFound, apiErrorNotFound, "announcement not found")
}

func AdminDeleteAnnouncementHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	id, err := parseUintParam(c, "id")
	if err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid id")
		return
	}
	opsStore.mu.Lock()
	defer opsStore.mu.Unlock()
	for i := range opsStore.announcements {
		if opsStore.announcements[i].ID == id {
			opsStore.announcements = append(opsStore.announcements[:i], opsStore.announcements[i+1:]...)
			Success(c, gin.H{"deleted": true})
			return
		}
	}
	Error(c, http.StatusNotFound, apiErrorNotFound, "announcement not found")
}

func AdminListOrdersHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	page := queryInt(c, "page", 1, 1, 1000000)
	pageSize := queryInt(c, "page_size", 15, 1, 100)
	provider := strings.TrimSpace(c.Query("provider"))
	status := strings.TrimSpace(c.Query("status"))
	userID := strings.TrimSpace(c.Query("user_id"))

	opsStore.mu.Lock()
	defer opsStore.mu.Unlock()
	filtered := make([]paymentOrderRecord, 0, len(opsStore.orders))
	for _, o := range opsStore.orders {
		if provider != "" && o.Provider != provider {
			continue
		}
		if status != "" && o.Status != status {
			continue
		}
		if userID != "" {
			uid, _ := strconv.ParseUint(userID, 10, 64)
			if uid == 0 || o.UserID != uint(uid) {
				continue
			}
		}
		filtered = append(filtered, o)
	}
	total := len(filtered)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	Success(c, gin.H{"items": filtered[start:end], "total": total, "page": page, "page_size": pageSize})
}

func AdminListRedeemCodesHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	opsStore.mu.Lock()
	defer opsStore.mu.Unlock()
	items := append([]redeemCodeRecord(nil), opsStore.redeemCodes...)
	sort.Slice(items, func(i, j int) bool { return items[i].ID > items[j].ID })
	Success(c, gin.H{"items": items})
}

func AdminCreateRedeemCodesHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var req createRedeemCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Count <= 0 || req.Count > 100 || req.Amount <= 0 {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid payload")
		return
	}
	opsStore.mu.Lock()
	defer opsStore.mu.Unlock()
	for i := 0; i < req.Count; i++ {
		id := opsStore.nextRedeemID
		opCode := fmt.Sprintf("RDM-%06d", id)
		opsStore.nextRedeemID++
		opsStore.redeemCodes = append(opsStore.redeemCodes, redeemCodeRecord{ID: id, Code: opCode, Amount: req.Amount, Status: "unused", CreatedAt: time.Now()})
	}
	Success(c, gin.H{"created": req.Count})
}

func AdminDeleteRedeemCodeHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	id, err := parseUintParam(c, "id")
	if err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid id")
		return
	}
	opsStore.mu.Lock()
	defer opsStore.mu.Unlock()
	for i := range opsStore.redeemCodes {
		if opsStore.redeemCodes[i].ID == id {
			opsStore.redeemCodes = append(opsStore.redeemCodes[:i], opsStore.redeemCodes[i+1:]...)
			Success(c, gin.H{"deleted": true})
			return
		}
	}
	Error(c, http.StatusNotFound, apiErrorNotFound, "redeem code not found")
}

func AdminListPricingGroupsHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var groups []model.Group
	if err := GlobalDB.WithContext(c.Request.Context()).Order("name ASC").Find(&groups).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list groups")
		return
	}
	items := make([]gin.H, 0, len(groups))
	for _, g := range groups {
		items = append(items, gin.H{"group_name": g.Name, "discount_rate": g.RateMultiplier})
	}
	Success(c, items)
}

func AdminUpsertPricingGroupHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var req struct {
		GroupName    string  `json:"group_name"`
		DiscountRate float64 `json:"discount_rate"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.GroupName) == "" || req.DiscountRate <= 0 {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid payload")
		return
	}
	name := strings.TrimSpace(req.GroupName)
	var g model.Group
	err := GlobalDB.WithContext(c.Request.Context()).Where("name = ?", name).First(&g).Error
	if err == nil {
		if e := GlobalDB.WithContext(c.Request.Context()).Model(&g).Update("rate_multiplier", req.DiscountRate).Error; e != nil {
			Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to update group")
			return
		}
		Success(c, gin.H{"group_name": name, "discount_rate": req.DiscountRate})
		return
	}
	g = model.Group{Name: name, RateMultiplier: req.DiscountRate}
	if e := GlobalDB.WithContext(c.Request.Context()).Create(&g).Error; e != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to create group")
		return
	}
	Success(c, gin.H{"group_name": name, "discount_rate": req.DiscountRate})
}

func AdminDeletePricingGroupHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	name := strings.TrimSpace(c.Param("name"))
	if name == "" || name == "default" {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid group name")
		return
	}
	res := GlobalDB.WithContext(c.Request.Context()).Where("name = ?", name).Delete(&model.Group{})
	if res.Error != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to delete group")
		return
	}
	if res.RowsAffected == 0 {
		Error(c, http.StatusNotFound, apiErrorNotFound, "group not found")
		return
	}
	Success(c, gin.H{"deleted": true})
}

func AdminListRefundsHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	page := queryInt(c, "page", 1, 1, 1000000)
	pageSize := queryInt(c, "page_size", 15, 1, 100)
	status := strings.TrimSpace(c.Query("status"))

	opsStore.mu.Lock()
	defer opsStore.mu.Unlock()
	filtered := make([]refundRecord, 0, len(opsStore.refunds))
	for _, r := range opsStore.refunds {
		if status != "" && r.Status != status {
			continue
		}
		filtered = append(filtered, r)
	}
	total := len(filtered)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	Success(c, gin.H{"items": filtered[start:end], "total": total, "page": page, "page_size": pageSize})
}

func AdminApproveRefundHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	id, err := parseUintParam(c, "id")
	if err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid id")
		return
	}
	bc, _ := requireBillingCtx(c)
	opsStore.mu.Lock()
	defer opsStore.mu.Unlock()
	for i := range opsStore.refunds {
		if opsStore.refunds[i].ID == id {
			now := time.Now()
			opsStore.refunds[i].Status = "approved"
			opsStore.refunds[i].ProcessedAt = &now
			if bc != nil {
				uid := bc.UserID
				opsStore.refunds[i].ProcessedBy = &uid
			}
			Success(c, opsStore.refunds[i])
			return
		}
	}
	Error(c, http.StatusNotFound, apiErrorNotFound, "refund not found")
}

func AdminRejectRefundHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	id, err := parseUintParam(c, "id")
	if err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid id")
		return
	}
	bc, _ := requireBillingCtx(c)
	opsStore.mu.Lock()
	defer opsStore.mu.Unlock()
	for i := range opsStore.refunds {
		if opsStore.refunds[i].ID == id {
			now := time.Now()
			opsStore.refunds[i].Status = "rejected"
			opsStore.refunds[i].ProcessedAt = &now
			if bc != nil {
				uid := bc.UserID
				opsStore.refunds[i].ProcessedBy = &uid
			}
			Success(c, opsStore.refunds[i])
			return
		}
	}
	Error(c, http.StatusNotFound, apiErrorNotFound, "refund not found")
}

func PaymentStripeConfigHandler(c *gin.Context) {
	Success(c, gin.H{"publishable_key": "", "mode": "sandbox", "enabled": false})
}

func PaymentStripeCreateHandler(c *gin.Context) {
	bc, ok := requireBillingCtx(c)
	if !ok {
		return
	}
	var req struct {
		Amount float64 `json:"amount"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Amount <= 0 {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid amount")
		return
	}
	opsStore.mu.Lock()
	id := opsStore.nextOrderID
	opsStore.nextOrderID++
	orderID := fmt.Sprintf("stripe-%06d", id)
	now := time.Now()
	opsStore.orders = append(opsStore.orders, paymentOrderRecord{ID: id, UserID: bc.UserID, Provider: "stripe", AmountUSD: req.Amount, AmountLocal: req.Amount, Currency: "USD", Status: "pending", CreatedAt: now, UpdatedAt: now})
	opsStore.mu.Unlock()
	paymentIntentID := fmt.Sprintf("pi_%06d", id)
	Success(c, gin.H{"client_secret": paymentIntentID + "_secret_local_mock", "order_id": orderID, "payment_intent_id": paymentIntentID, "amount_usd": req.Amount})
}

func PaymentAlipayCreateHandler(c *gin.Context) {
	bc, ok := requireBillingCtx(c)
	if !ok {
		return
	}
	var req struct {
		Amount float64 `json:"amount"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Amount <= 0 {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid amount")
		return
	}
	opsStore.mu.Lock()
	id := opsStore.nextOrderID
	opsStore.nextOrderID++
	orderID := fmt.Sprintf("alipay-%06d", id)
	now := time.Now()
	opsStore.orders = append(opsStore.orders, paymentOrderRecord{ID: id, UserID: bc.UserID, Provider: "alipay", AmountUSD: req.Amount, AmountLocal: req.Amount * 7.2, Currency: "CNY", Status: "pending", CreatedAt: now, UpdatedAt: now})
	opsStore.mu.Unlock()
	payURL := "cpa-gateway://payment/alipay/" + orderID
	Success(c, gin.H{"order_id": orderID, "pay_url": payURL, "qr_code": payURL, "amount_usd": req.Amount, "amount_local": req.Amount * 7.2, "currency": "CNY"})
}

func PaymentAlipayStatusHandler(c *gin.Context) {
	if _, ok := requireBillingCtx(c); !ok {
		return
	}
	orderID := strings.TrimSpace(c.Query("order_id"))
	if orderID == "" {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "order_id is required")
		return
	}
	order, ok := paymentOrderByPublicID(orderID)
	if !ok || order.Provider != "alipay" {
		Error(c, http.StatusNotFound, apiErrorNotFound, "order not found")
		return
	}
	Success(c, gin.H{"status": order.Status, "order_id": orderID, "amount": order.AmountUSD, "paid_at": order.PaidAt})
}

func PaymentWechatCreateHandler(c *gin.Context) {
	bc, ok := requireBillingCtx(c)
	if !ok {
		return
	}
	var req struct {
		Amount float64 `json:"amount"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Amount <= 0 {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid amount")
		return
	}
	opsStore.mu.Lock()
	id := opsStore.nextOrderID
	opsStore.nextOrderID++
	orderID := fmt.Sprintf("wechat-%06d", id)
	now := time.Now()
	opsStore.orders = append(opsStore.orders, paymentOrderRecord{ID: id, UserID: bc.UserID, Provider: "wechat", AmountUSD: req.Amount, AmountLocal: req.Amount * 7.2, Currency: "CNY", Status: "pending", CreatedAt: now, UpdatedAt: now})
	opsStore.mu.Unlock()
	Success(c, gin.H{"order_id": orderID, "code_url": orderID, "amount_usd": req.Amount, "amount_local": req.Amount * 7.2, "currency": "CNY"})
}

func PaymentWechatStatusHandler(c *gin.Context) {
	if _, ok := requireBillingCtx(c); !ok {
		return
	}
	orderID := strings.TrimSpace(c.Query("order_id"))
	if orderID == "" {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "order_id is required")
		return
	}
	order, ok := paymentOrderByPublicID(orderID)
	if !ok || order.Provider != "wechat" {
		Error(c, http.StatusNotFound, apiErrorNotFound, "order not found")
		return
	}
	Success(c, gin.H{"status": order.Status, "order_id": orderID, "amount": order.AmountUSD, "paid_at": order.PaidAt})
}

func paymentOrderByPublicID(orderID string) (paymentOrderRecord, bool) {
	parts := strings.Split(orderID, "-")
	if len(parts) != 2 {
		return paymentOrderRecord{}, false
	}
	id64, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return paymentOrderRecord{}, false
	}
	opsStore.mu.Lock()
	defer opsStore.mu.Unlock()
	for _, order := range opsStore.orders {
		if order.ID == uint(id64) && order.Provider == parts[0] {
			return order, true
		}
	}
	return paymentOrderRecord{}, false
}

func AdminUsageLogsHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	page := queryInt(c, "page", 1, 1, 1000000)
	pageSize := queryInt(c, "page_size", 30, 1, 100)
	offset := (page - 1) * pageSize
	modelQuery := strings.TrimSpace(c.Query("model"))
	status := strings.TrimSpace(c.Query("status"))
	startDate := strings.TrimSpace(c.Query("start_date"))
	endDate := strings.TrimSpace(c.Query("end_date"))

	base := GlobalDB.WithContext(c.Request.Context()).Model(&model.UsageLog{})
	if modelQuery != "" {
		base = base.Where("model = ?", modelQuery)
	}
	if status != "" {
		if status == "success" {
			base = base.Where("cost >= 0")
		} else if status == "failed" {
			base = base.Where("cost < 0")
		}
	}
	if startDate != "" {
		base = base.Where("DATE(created_at) >= ?", startDate)
	}
	if endDate != "" {
		base = base.Where("DATE(created_at) <= ?", endDate)
	}

	var total int64
	if err := base.Count(&total).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count usage logs")
		return
	}

	var rows []model.UsageLog
	if err := base.Order("created_at DESC").Limit(pageSize).Offset(offset).Find(&rows).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list usage logs")
		return
	}

	items := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		item := gin.H{
			"id":               row.ID,
			"request_id":       fmt.Sprintf("req-%d", row.ID),
			"user_id":          row.UserID,
			"api_key_id":       row.ApiKeyID,
			"model":            row.Model,
			"provider":         "openai",
			"input_tokens":     row.TokensIn,
			"output_tokens":    row.TokensOut,
			"reasoning_tokens": 0,
			"cached_tokens":    0,
			"input_cost":       row.Cost,
			"output_cost":      0,
			"total_cost":       row.Cost,
			"actual_cost":      row.Cost,
			"rate_multiplier":  1,
			"stream":           false,
			"duration_ms":      nil,
			"failed":           row.Cost < 0,
			"created_at":       row.CreatedAt,
		}
		items = append(items, item)
	}

	Success(c, gin.H{"items": items, "page": page, "page_size": pageSize, "total": total})
}

func AdminModelCatalogModelsURLGetHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	channelKey := strings.TrimSpace(c.Query("channel_key"))
	if channelKey == "" {
		Success(c, gin.H{"models_url": ""})
		return
	}
	var entry model.ModelCatalogEntry
	if err := GlobalDB.WithContext(c.Request.Context()).Where("channel_key = ? AND models_url <> ''", channelKey).First(&entry).Error; err != nil {
		Success(c, gin.H{"models_url": ""})
		return
	}
	Success(c, gin.H{"models_url": entry.ModelsURL})
}

func AdminModelCatalogModelsURLPutHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var req struct {
		ChannelKey string `json:"channel_key"`
		ModelsURL  string `json:"models_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.ChannelKey) == "" {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid models url payload")
		return
	}
	entry := model.ModelCatalogEntry{ChannelKey: strings.TrimSpace(req.ChannelKey), ModelID: "__models_url__", Visible: false, ModelsURL: strings.TrimSpace(req.ModelsURL)}
	if err := GlobalDB.WithContext(c.Request.Context()).Where("channel_key = ? AND model_id = ?", entry.ChannelKey, entry.ModelID).Assign(entry).FirstOrCreate(&entry).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to save models url")
		return
	}
	Success(c, gin.H{"ok": true, "models_url": entry.ModelsURL})
}

func AdminModelCatalogEnsureOpenAIChannelHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var req struct {
		ChannelKey string   `json:"channel_key"`
		ModelIDs   []string `json:"model_ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.ChannelKey) == "" {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid model catalog payload")
		return
	}
	created := 0
	for _, modelID := range req.ModelIDs {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			continue
		}
		entry := model.ModelCatalogEntry{ChannelKey: strings.TrimSpace(req.ChannelKey), ModelID: modelID, Visible: true}
		res := GlobalDB.WithContext(c.Request.Context()).Where("channel_key = ? AND model_id = ?", entry.ChannelKey, entry.ModelID).FirstOrCreate(&entry)
		if res.Error != nil {
			Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to ensure model catalog")
			return
		}
		if res.RowsAffected > 0 {
			created++
		}
	}
	Success(c, gin.H{"ok": true, "created": created})
}

func AdminModelCatalogOpenAIVisibilityHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var req struct {
		ChannelKey string `json:"channel_key"`
		ModelID    string `json:"model_id"`
		Visible    bool   `json:"visible"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.ChannelKey) == "" || strings.TrimSpace(req.ModelID) == "" {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid visibility payload")
		return
	}
	entry := model.ModelCatalogEntry{ChannelKey: strings.TrimSpace(req.ChannelKey), ModelID: strings.TrimSpace(req.ModelID), Visible: req.Visible}
	if err := GlobalDB.WithContext(c.Request.Context()).Where("channel_key = ? AND model_id = ?", entry.ChannelKey, entry.ModelID).Assign(map[string]any{"visible": req.Visible}).FirstOrCreate(&entry).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to save visibility")
		return
	}
	Success(c, gin.H{"ok": true, "channel_key": entry.ChannelKey, "model_id": entry.ModelID, "visible": req.Visible})
}

func AdminUpsertPricingModelHandler(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var req struct {
		ModelID               string  `json:"model_id"`
		InputPricePer1M       float64 `json:"input_price_per_1m"`
		OutputPricePer1M      float64 `json:"output_price_per_1m"`
		CachedInputPricePer1M float64 `json:"cached_input_price_per_1m"`
		ReasoningPricePer1M   float64 `json:"reasoning_price_per_1m"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.ModelID) == "" {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid model price payload")
		return
	}
	price := model.ModelPrice{ModelID: strings.TrimSpace(req.ModelID), InputPricePer1M: req.InputPricePer1M, OutputPricePer1M: req.OutputPricePer1M, CachedInputPricePer1M: req.CachedInputPricePer1M, ReasoningPricePer1M: req.ReasoningPricePer1M}
	if err := GlobalDB.WithContext(c.Request.Context()).Where("model_id = ?", price.ModelID).Assign(price).FirstOrCreate(&price).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to save model price")
		return
	}
	Success(c, price)
}
