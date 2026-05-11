package main

import (
	"encoding/base64"
	"html"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/gorm"
)

const maxTicketImageBytes = 4 << 20

func UserListTicketsHandler(c *gin.Context) {
	bc, ok := requireBillingCtx(c)
	if !ok {
		return
	}
	page := queryInt(c, "page", 1, 1, 1000000)
	pageSize := queryInt(c, "page_size", 20, 1, 100)
	status := strings.TrimSpace(c.Query("status"))

	q := GlobalDB.WithContext(c.Request.Context()).Model(&model.Ticket{}).Where("user_id = ?", bc.UserID)
	if status != "" && status != "all" {
		q = q.Where("status = ?", status)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count tickets")
		return
	}
	var rows []model.Ticket
	if err := q.Order("updated_at DESC, id DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&rows).Error; err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list tickets")
		return
	}
	items := make([]gin.H, 0, len(rows))
	for _, t := range rows {
		items = append(items, ticketPayload(t, ""))
	}
	Success(c, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func UserCreateTicketHandler(c *gin.Context) {
	bc, ok := requireBillingCtx(c)
	if !ok {
		return
	}
	var req struct {
		Title    string `json:"title"`
		Category string `json:"category"`
		Priority string `json:"priority"`
		Content  string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Content) == "" {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid ticket payload")
		return
	}
	ticket := model.Ticket{UserID: bc.UserID, Title: firstNonEmpty(req.Title, "在线咨询"), Category: firstNonEmpty(req.Category, "other"), Priority: firstNonEmpty(req.Priority, "medium"), Status: "open"}
	err := GlobalDB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&ticket).Error; err != nil {
			return err
		}
		return tx.Create(&model.TicketReply{TicketID: ticket.ID, UserID: bc.UserID, Content: strings.TrimSpace(req.Content)}).Error
	})
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to create ticket")
		return
	}
	Success(c, ticketPayload(ticket, strings.TrimSpace(req.Content)))
}

func UserGetTicketHandler(c *gin.Context) {
	bc, ok := requireBillingCtx(c)
	if !ok {
		return
	}
	ticket, ok := loadUserTicket(c, bc.UserID)
	if !ok {
		return
	}
	Success(c, ticketDetailPayload(c, ticket))
}

func UserCreateTicketReplyHandler(c *gin.Context) {
	bc, ok := requireBillingCtx(c)
	if !ok {
		return
	}
	ticket, ok := loadUserTicket(c, bc.UserID)
	if !ok {
		return
	}
	createTicketReply(c, ticket, bc.UserID, false)
}

func UserUploadTicketImageHandler(c *gin.Context) {
	if _, ok := requireBillingCtx(c); !ok {
		return
	}
	file, err := c.FormFile("image")
	if err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "image is required")
		return
	}
	if file.Size > maxTicketImageBytes {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "image exceeds 4MB")
		return
	}
	contentType := file.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "image/") {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "file must be an image")
		return
	}
	src, err := file.Open()
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to read image")
		return
	}
	defer src.Close()
	data, err := io.ReadAll(io.LimitReader(src, maxTicketImageBytes+1))
	if err != nil || len(data) > maxTicketImageBytes {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid image")
		return
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	alt := html.EscapeString(file.Filename)
	url := "data:" + contentType + ";base64," + encoded
	Success(c, gin.H{"url": url, "markdown": "![" + alt + "](" + url + ")"})
}

func AdminListTicketsHandler(c *gin.Context) {
	if !requireAdmin(c) { return }
	page := queryInt(c, "page", 1, 1, 1000000)
	pageSize := queryInt(c, "page_size", 20, 1, 100)
	status := strings.TrimSpace(c.Query("status"))
	q := GlobalDB.WithContext(c.Request.Context()).Model(&model.Ticket{})
	if status != "" && status != "all" { q = q.Where("status = ?", status) }
	var total int64
	if err := q.Count(&total).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to count tickets"); return }
	var rows []model.Ticket
	if err := q.Order("updated_at DESC, id DESC").Limit(pageSize).Offset((page-1)*pageSize).Find(&rows).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list tickets"); return }
	items := make([]gin.H, 0, len(rows))
	for _, t := range rows { items = append(items, ticketPayload(t, "")) }
	Success(c, gin.H{"items": items, "total": total, "page": page, "page_size": pageSize})
}

func AdminGetTicketHandler(c *gin.Context) {
	if !requireAdmin(c) { return }
	ticket, ok := loadAnyTicket(c)
	if !ok { return }
	Success(c, ticketDetailPayload(c, ticket))
}

func AdminCreateTicketReplyHandler(c *gin.Context) {
	bc, ok := requireBillingCtx(c)
	if !ok || !requireAdmin(c) { return }
	ticket, ok := loadAnyTicket(c)
	if !ok { return }
	createTicketReply(c, ticket, bc.UserID, true)
}

func AdminUpdateTicketStatusHandler(c *gin.Context) {
	if !requireAdmin(c) { return }
	ticket, ok := loadAnyTicket(c)
	if !ok { return }
	var req struct{ Status string `json:"status"` }
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Status) == "" { Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid status"); return }
	ticket.Status = strings.TrimSpace(req.Status)
	if err := GlobalDB.WithContext(c.Request.Context()).Save(&ticket).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to update ticket"); return }
	Success(c, ticketPayload(ticket, ""))
}

func AdminAssignTicketHandler(c *gin.Context) {
	bc, ok := requireBillingCtx(c)
	if !ok || !requireAdmin(c) { return }
	ticket, ok := loadAnyTicket(c)
	if !ok { return }
	var req struct{ AssigneeID *uint `json:"assignee_id"` }
	_ = c.ShouldBindJSON(&req)
	if req.AssigneeID == nil { ticket.AssigneeID = &bc.UserID } else { ticket.AssigneeID = req.AssigneeID }
	if err := GlobalDB.WithContext(c.Request.Context()).Save(&ticket).Error; err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to assign ticket"); return }
	Success(c, ticketPayload(ticket, ""))
}

func AdminTicketQuickRepliesGetHandler(c *gin.Context) {
	if !requireAdmin(c) { return }
	Success(c, gin.H{"items": []gin.H{{"id": 1, "title": "收到", "content": "您好，我们已收到您的反馈，将尽快处理。"}}})
}

func AdminTicketQuickRepliesSaveHandler(c *gin.Context) {
	if !requireAdmin(c) { return }
	Success(c, gin.H{"ok": true})
}

func loadUserTicket(c *gin.Context, userID uint) (model.Ticket, bool) {
	ticket, ok := loadAnyTicket(c)
	if !ok { return model.Ticket{}, false }
	if ticket.UserID != userID { Error(c, http.StatusNotFound, apiErrorNotFound, "ticket not found"); return model.Ticket{}, false }
	return ticket, true
}

func loadAnyTicket(c *gin.Context) (model.Ticket, bool) {
	id, err := parseUintParam(c, "id")
	if err != nil { Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid ticket id"); return model.Ticket{}, false }
	var ticket model.Ticket
	if err := GlobalDB.WithContext(c.Request.Context()).First(&ticket, id).Error; err != nil {
		if err == gorm.ErrRecordNotFound { Error(c, http.StatusNotFound, apiErrorNotFound, "ticket not found"); return model.Ticket{}, false }
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load ticket"); return model.Ticket{}, false
	}
	return ticket, true
}

func createTicketReply(c *gin.Context, ticket model.Ticket, userID uint, isAdmin bool) {
	var req struct{ Content string `json:"content"` }
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Content) == "" { Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid reply payload"); return }
	reply := model.TicketReply{TicketID: ticket.ID, UserID: userID, IsAdmin: isAdmin, Content: strings.TrimSpace(req.Content)}
	status := ticket.Status
	if isAdmin && status == "open" { status = "answered" }
	if !isAdmin && status == "answered" { status = "open" }
	err := GlobalDB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&reply).Error; err != nil { return err }
		return tx.Model(&model.Ticket{}).Where("id = ?", ticket.ID).Updates(map[string]any{"status": status}).Error
	})
	if err != nil { Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to create reply"); return }
	Success(c, replyPayload(reply))
}

func ticketDetailPayload(c *gin.Context, ticket model.Ticket) gin.H {
	var replies []model.TicketReply
	_ = GlobalDB.WithContext(c.Request.Context()).Where("ticket_id = ?", ticket.ID).Order("created_at ASC, id ASC").Find(&replies).Error
	items := make([]gin.H, 0, len(replies))
	for _, r := range replies { items = append(items, replyPayload(r)) }
	out := ticketPayload(ticket, "")
	out["replies"] = items
	return out
}

func ticketPayload(t model.Ticket, content string) gin.H {
	return gin.H{"id": t.ID, "user_id": t.UserID, "title": t.Title, "category": t.Category, "priority": t.Priority, "status": t.Status, "assignee_id": t.AssigneeID, "content": content, "created_at": t.CreatedAt, "updated_at": t.UpdatedAt}
}

func replyPayload(r model.TicketReply) gin.H {
	return gin.H{"id": r.ID, "ticket_id": r.TicketID, "user_id": r.UserID, "is_admin": r.IsAdmin, "content": r.Content, "created_at": r.CreatedAt}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values { if s := strings.TrimSpace(v); s != "" { return s } }
	return ""
}
