package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xxww0098/cpa-gateway/authutil"
	"github.com/xxww0098/cpa-gateway/config"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const securityRegressionJWTSecret = "security-regression-secret"

func newSecurityRegressionRouter(t *testing.T) (*PanelRouter, *gorm.DB) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if sqlDB, derr := db.DB(); derr == nil {
		sqlDB.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := db.AutoMigrate(
		&model.User{},
		&model.ApiKey{},
		&model.BalanceLog{},
		&model.UsageLog{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}

	cfg := &config.Config{}
	cfg.Auth.JWT.Secret = securityRegressionJWTSecret
	cfg.Auth.JWT.ExpiryHours = 24
	cfg.Auth.AdminEmails = []string{"owner@example.test"}
	return NewPanelRouter(db, nil, nil, nil, cfg), db
}

func buildSecurityAuthEngine(pr *PanelRouter) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	panel := r.Group("/api/panel")
	pr.RegisterAuthRoutes(panel)
	authed := panel.Group("/", pr.AuthMiddleware())
	authed.GET("/admin/probe", func(c *gin.Context) {
		if !pr.requireAdmin(c) {
			return
		}
		Success(c, gin.H{"ok": true})
	})
	return r
}

func doJSON(t *testing.T, r http.Handler, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()

	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRegisterAdminEmailDoesNotGrantAdmin(t *testing.T) {
	pr, db := newSecurityRegressionRouter(t)
	r := buildSecurityAuthEngine(pr)

	w := doJSON(t, r, http.MethodPost, "/api/panel/auth/register", gin.H{
		"email":    "owner@example.test",
		"password": "password123",
	}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("register HTTP %d; body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Code int `json:"code"`
		Data struct {
			Token string `json:"token"`
			User  struct {
				ID   uint   `json:"id"`
				Role string `json:"role"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if resp.Data.User.Role != "user" {
		t.Fatalf("registered admin email role=%q, want user", resp.Data.User.Role)
	}

	w = doJSON(t, r, http.MethodGet, "/api/panel/admin/probe", nil, resp.Data.Token)
	if w.Code != http.StatusForbidden {
		t.Fatalf("registered admin email reached admin probe: HTTP %d body=%s", w.Code, w.Body.String())
	}

	if err := db.Model(&model.User{}).Where("id = ?", resp.Data.User.ID).Update("role", "admin").Error; err != nil {
		t.Fatalf("promote user for positive check: %v", err)
	}
	w = doJSON(t, r, http.MethodGet, "/api/panel/admin/probe", nil, resp.Data.Token)
	if w.Code != http.StatusOK {
		t.Fatalf("DB admin role rejected: HTTP %d body=%s", w.Code, w.Body.String())
	}
}

func TestForgedJWTEmailClaimDoesNotGrantAdmin(t *testing.T) {
	pr, db := newSecurityRegressionRouter(t)
	r := buildSecurityAuthEngine(pr)

	user := model.User{Email: "attacker@example.test", PasswordHash: "hash", Role: "user", Status: userStatusActive}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	token, err := authutil.GenerateJWT(user.ID, "owner@example.test", securityRegressionJWTSecret, 24)
	if err != nil {
		t.Fatalf("generate forged-claim jwt: %v", err)
	}

	w := doJSON(t, r, http.MethodGet, "/api/panel/admin/probe", nil, token)
	if w.Code != http.StatusForbidden {
		t.Fatalf("forged email claim reached admin probe: HTTP %d body=%s", w.Code, w.Body.String())
	}
}

func resetOpsStoreForTest(t *testing.T) {
	t.Helper()
	reset := func() {
		opsStore.nextAnnID = 2
		opsStore.nextRefundID = 1
		opsStore.nextRedeemID = 1
		opsStore.nextOrderID = 1
		opsStore.notifications = nil
		opsStore.announcements = nil
		opsStore.refunds = nil
		opsStore.orders = nil
		opsStore.redeemCodes = nil
	}
	opsStore.mu.Lock()
	reset()
	opsStore.mu.Unlock()
	t.Cleanup(func() {
		opsStore.mu.Lock()
		reset()
		opsStore.mu.Unlock()
	})
}

func seedSecurityUser(t *testing.T, db *gorm.DB, id uint, role string) {
	t.Helper()
	user := model.User{
		ID:           id,
		Email:        "security-user-" + strings.TrimSpace(role) + "-" + strconv.FormatUint(uint64(id), 10) + "@example.test",
		PasswordHash: "hash",
		Role:         role,
		Status:       userStatusActive,
	}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("seed %s user: %v", role, err)
	}
}

func runHandlerWithBillingCtx(t *testing.T, pr *PanelRouter, userID uint, method, path string, body any, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		setBillingContext(c, &BillingCtx{
			UserID:    userID,
			RateMult:  1,
			Status:    userStatusActive,
			AuthType:  authTypeJWT,
			RequestID: "security-regression",
		})
		c.Next()
	})
	routePath := path
	if idx := strings.Index(routePath, "?"); idx >= 0 {
		routePath = routePath[:idx]
	}
	r.Handle(method, routePath, handler)
	return doJSON(t, r, method, path, body, "")
}

func TestRedeemCodesAreRandomAndSequentialGuessFails(t *testing.T) {
	resetOpsStoreForTest(t)
	pr, db := newSecurityRegressionRouter(t)
	seedSecurityUser(t, db, 1, "admin")
	seedSecurityUser(t, db, 2, "user")

	w := runHandlerWithBillingCtx(t, pr, 1, http.MethodPost, "/admin/redeem-codes", gin.H{
		"count":  2,
		"amount": 5,
	}, pr.AdminCreateRedeemCodesHandler)
	if w.Code != http.StatusOK {
		t.Fatalf("create redeem codes HTTP %d; body=%s", w.Code, w.Body.String())
	}

	opsStore.mu.Lock()
	if got := len(opsStore.redeemCodes); got != 2 {
		opsStore.mu.Unlock()
		t.Fatalf("redeem code count=%d, want 2", got)
	}
	first := opsStore.redeemCodes[0].Code
	second := opsStore.redeemCodes[1].Code
	opsStore.mu.Unlock()

	if first == "RDM-000001" || second == "RDM-000002" {
		t.Fatalf("redeem codes are still sequential: %q %q", first, second)
	}
	if !strings.HasPrefix(first, "RDM-") || len(first) < 24 || first == second {
		t.Fatalf("unexpected redeem codes: %q %q", first, second)
	}

	w = runHandlerWithBillingCtx(t, pr, 2, http.MethodPost, "/user/redeem", gin.H{"code": "RDM-000001"}, pr.UserRedeemHandler)
	if w.Code != http.StatusNotFound {
		t.Fatalf("sequential guess redeemed: HTTP %d body=%s", w.Code, w.Body.String())
	}

	w = runHandlerWithBillingCtx(t, pr, 2, http.MethodPost, "/user/redeem", gin.H{"code": strings.ToLower(first)}, pr.UserRedeemHandler)
	if w.Code != http.StatusOK {
		t.Fatalf("actual random code rejected: HTTP %d body=%s", w.Code, w.Body.String())
	}
}

func TestPaymentStatusRequiresOrderOwnerOrAdmin(t *testing.T) {
	resetOpsStoreForTest(t)
	pr, db := newSecurityRegressionRouter(t)
	seedSecurityUser(t, db, 1, "user")
	seedSecurityUser(t, db, 2, "user")
	seedSecurityUser(t, db, 3, "admin")

	now := time.Now()
	opsStore.mu.Lock()
	opsStore.orders = append(opsStore.orders,
		paymentOrderRecord{ID: 1, UserID: 1, Provider: "alipay", AmountUSD: 10, Status: "pending", CreatedAt: now, UpdatedAt: now},
		paymentOrderRecord{ID: 2, UserID: 1, Provider: "wechat", AmountUSD: 20, Status: "pending", CreatedAt: now, UpdatedAt: now},
	)
	opsStore.mu.Unlock()

	cases := []struct {
		name   string
		userID uint
		method gin.HandlerFunc
		path   string
		want   int
	}{
		{name: "alipay other user", userID: 2, method: pr.PaymentAlipayStatusHandler, path: "/payment/alipay/status?order_id=alipay-000001", want: http.StatusNotFound},
		{name: "wechat other user", userID: 2, method: pr.PaymentWechatStatusHandler, path: "/payment/wechat/status?order_id=wechat-000002", want: http.StatusNotFound},
		{name: "alipay owner", userID: 1, method: pr.PaymentAlipayStatusHandler, path: "/payment/alipay/status?order_id=alipay-000001", want: http.StatusOK},
		{name: "wechat admin", userID: 3, method: pr.PaymentWechatStatusHandler, path: "/payment/wechat/status?order_id=wechat-000002", want: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := runHandlerWithBillingCtx(t, pr, tc.userID, http.MethodGet, tc.path, nil, tc.method)
			if w.Code != tc.want {
				t.Fatalf("HTTP %d, want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}
