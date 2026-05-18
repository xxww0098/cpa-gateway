package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xxww0098/cpa-gateway/model"
)

const (
	opSourcePanel   = "panel"
	opSourceSDK     = "sdk"
	opSourceBalance = "balance"
)

// recordOperation persists a single panel-side OperationLog row.
//
// It is intentionally best-effort: failures are logged and swallowed because
// the audit trail must never block the business operation that triggered it.
// Pass `actor=nil` for unauthenticated events (login attempts before JWT is
// minted, register). `extras` is JSON-marshalled into Metadata; nil-safe.
func (pr *PanelRouter) recordOperation(c *gin.Context, actor *BillingCtx, action, target string, statusCode int, extras map[string]any) {
	if pr == nil || pr.DB == nil {
		return
	}
	row := model.OperationLog{
		Source:     opSourcePanel,
		Action:     action,
		Target:     target,
		StatusCode: statusCode,
	}
	if c != nil {
		row.Method = c.Request.Method
		row.Path = c.FullPath()
		if row.Path == "" {
			row.Path = c.Request.URL.Path
		}
		row.IPAddress = c.ClientIP()
		row.RequestID = traceIDFromGin(c)
	}
	if actor != nil {
		row.ActorID = actor.UserID
		row.ActorEmail = actor.Email
	}
	if extras != nil {
		if data, err := json.Marshal(extras); err == nil {
			row.Metadata = data
		}
	}

	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	if err := pr.DB.WithContext(ctx).Create(&row).Error; err != nil {
		slog.Warn("record_operation_failed",
			"event", "operation_log_write_failed",
			"action", action,
			"target", target,
			"error", err,
		)
	}
}

type auditLogEntry struct {
	ID         string         `json:"id"`
	Source     string         `json:"source"`
	ActorID    uint           `json:"actor_id"`
	ActorEmail string         `json:"actor_email,omitempty"`
	Action     string         `json:"action"`
	Target     string         `json:"target,omitempty"`
	Method     string         `json:"method,omitempty"`
	Path       string         `json:"path,omitempty"`
	StatusCode int            `json:"status_code,omitempty"`
	IPAddress  string         `json:"ip_address,omitempty"`
	RequestID  string         `json:"request_id,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

// AdminListAuditLogsHandler is the unified operations console feed.
//
// Sources (selectable via `?source=panel|sdk|balance|all`):
//   - panel   → operation_logs (二次开发的 panel/admin 操作)
//   - sdk     → usage_logs (cliproxyapi SDK /v1/* 调用，已由 UsagePlugin 写入)
//   - balance → balance_logs (Hold/Settle/Release/Credit/Debit 流水)
//   - all (default) → union, ordered by created_at DESC.
//
// Admin-only. Filters: source, action, user_id, start_date, end_date, q (free-text).
func (pr *PanelRouter) AdminListAuditLogsHandler(c *gin.Context) {
	if !pr.requireAdmin(c) {
		return
	}
	page := queryInt(c, "page", 1, 1, 1000000)
	pageSize := queryInt(c, "page_size", 30, 1, 200)
	source := strings.ToLower(strings.TrimSpace(c.Query("source")))
	action := strings.TrimSpace(c.Query("action"))
	userIDStr := strings.TrimSpace(c.Query("user_id"))
	startDate := strings.TrimSpace(c.Query("start_date"))
	endDate := strings.TrimSpace(c.Query("end_date"))
	q := strings.TrimSpace(c.Query("q"))

	var userID uint
	if userIDStr != "" {
		if v, err := strconv.ParseUint(userIDStr, 10, 64); err == nil {
			userID = uint(v)
		}
	}

	// Collect candidates per requested source. We over-fetch from each table
	// (limit = page*pageSize) then merge + sort in-memory. This avoids a
	// SQL UNION ALL across heterogeneous tables and is fine for the volumes
	// the operations console queries (top N most recent).
	fetchLimit := page * pageSize
	if fetchLimit > 1000 {
		fetchLimit = 1000
	}

	want := func(s string) bool {
		return source == "" || source == "all" || source == s
	}

	entries := make([]auditLogEntry, 0, fetchLimit*2)
	ctx := c.Request.Context()

	if want(opSourcePanel) {
		base := pr.DB.WithContext(ctx).Model(&model.OperationLog{})
		if action != "" {
			base = base.Where("action = ?", action)
		}
		if userID != 0 {
			base = base.Where("actor_id = ?", userID)
		}
		if startDate != "" {
			base = base.Where("created_at >= ?", startDate)
		}
		if endDate != "" {
			base = base.Where("created_at <= ?", endDate+" 23:59:59")
		}
		if q != "" {
			like := "%" + q + "%"
			base = base.Where("action ILIKE ? OR target ILIKE ? OR actor_email ILIKE ?", like, like, like)
		}
		var rows []model.OperationLog
		if err := base.Order("created_at DESC").Limit(fetchLimit).Find(&rows).Error; err != nil {
			Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list panel logs")
			return
		}
		for _, r := range rows {
			entries = append(entries, panelLogToEntry(r))
		}
	}

	if want(opSourceSDK) {
		base := pr.DB.WithContext(ctx).Model(&model.UsageLog{})
		if userID != 0 {
			base = base.Where("user_id = ?", userID)
		}
		if action != "" && strings.HasPrefix(action, "sdk:") {
			base = base.Where("provider = ?", strings.TrimPrefix(action, "sdk:"))
		}
		if startDate != "" {
			base = base.Where("created_at >= ?", startDate)
		}
		if endDate != "" {
			base = base.Where("created_at <= ?", endDate+" 23:59:59")
		}
		if q != "" {
			like := "%" + q + "%"
			base = base.Where("model ILIKE ? OR provider ILIKE ? OR request_id ILIKE ?", like, like, like)
		}
		var rows []model.UsageLog
		if err := base.Order("created_at DESC").Limit(fetchLimit).Find(&rows).Error; err != nil {
			Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list sdk logs")
			return
		}
		for _, r := range rows {
			entries = append(entries, usageLogToEntry(r))
		}
	}

	if want(opSourceBalance) {
		base := pr.DB.WithContext(ctx).Model(&model.BalanceLog{})
		if userID != 0 {
			base = base.Where("user_id = ?", userID)
		}
		if action != "" && strings.HasPrefix(action, "balance:") {
			base = base.Where("type = ?", strings.TrimPrefix(action, "balance:"))
		}
		if startDate != "" {
			base = base.Where("created_at >= ?", startDate)
		}
		if endDate != "" {
			base = base.Where("created_at <= ?", endDate+" 23:59:59")
		}
		if q != "" {
			like := "%" + q + "%"
			base = base.Where("type ILIKE ? OR reference ILIKE ?", like, like)
		}
		var rows []model.BalanceLog
		if err := base.Order("created_at DESC").Limit(fetchLimit).Find(&rows).Error; err != nil {
			Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to list balance logs")
			return
		}
		for _, r := range rows {
			entries = append(entries, balanceLogToEntry(r))
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].CreatedAt.After(entries[j].CreatedAt) })

	total := len(entries)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}

	Success(c, gin.H{
		"items":     entries[start:end],
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"source":    source,
	})
}

func panelLogToEntry(r model.OperationLog) auditLogEntry {
	return auditLogEntry{
		ID:         "panel-" + strconv.FormatUint(uint64(r.ID), 10),
		Source:     opSourcePanel,
		ActorID:    r.ActorID,
		ActorEmail: r.ActorEmail,
		Action:     r.Action,
		Target:     r.Target,
		Method:     r.Method,
		Path:       r.Path,
		StatusCode: r.StatusCode,
		IPAddress:  r.IPAddress,
		RequestID:  r.RequestID,
		Metadata:   decodeJSONMap(r.Metadata),
		CreatedAt:  r.CreatedAt,
	}
}

func usageLogToEntry(r model.UsageLog) auditLogEntry {
	action := "sdk:" + r.Provider
	if r.Provider == "" {
		action = "sdk:request"
	}
	target := r.Model
	if target == "" {
		target = "request:" + r.RequestID
	}
	meta := map[string]any{
		"model":          r.Model,
		"provider":       r.Provider,
		"tokens_in":      r.TokensIn,
		"tokens_out":     r.TokensOut,
		"input_cost":     r.InputCost,
		"output_cost":    r.OutputCost,
		"total_cost":     r.TotalCost,
		"actual_cost":    r.ActualCost,
		"stream":         r.Stream,
		"duration_ms":    r.DurationMs,
		"failed":         r.Failed,
		"api_key_id":     r.ApiKeyID,
		"idempotency":    r.IdempotencyKey,
	}
	status := http.StatusOK
	if r.Failed {
		status = http.StatusBadGateway
	}
	return auditLogEntry{
		ID:         "sdk-" + strconv.FormatUint(uint64(r.ID), 10),
		Source:     opSourceSDK,
		ActorID:    r.UserID,
		Action:     action,
		Target:     target,
		StatusCode: status,
		IPAddress:  r.IPAddress,
		RequestID:  r.RequestID,
		Metadata:   meta,
		CreatedAt:  r.CreatedAt,
	}
}

func balanceLogToEntry(r model.BalanceLog) auditLogEntry {
	meta := decodeJSONMap(r.Metadata)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["amount"] = r.Amount
	meta["reference"] = r.Reference
	return auditLogEntry{
		ID:        "balance-" + strconv.FormatUint(uint64(r.ID), 10),
		Source:    opSourceBalance,
		ActorID:   r.UserID,
		Action:    "balance:" + r.Type,
		Target:    r.Reference,
		Metadata:  meta,
		CreatedAt: r.CreatedAt,
	}
}

func decodeJSONMap(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}
