package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/xxww0098/cpa-gateway/authutil"
	"github.com/xxww0098/cpa-gateway/model"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type authRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type authUserResponse struct {
	ID        uint    `json:"id"`
	Email     string  `json:"email"`
	Role      string  `json:"role"`
	Balance   float64 `json:"balance"`
	Status    string  `json:"status"`
	CreatedAt string  `json:"created_at"`
}

// RegisterAuthRoutes wires panel authentication endpoints onto a Gin router group.
func (pr *PanelRouter) RegisterAuthRoutes(rg *gin.RouterGroup) {
	rg.POST("/auth/register", pr.RegisterHandler)
	rg.POST("/auth/login", pr.LoginHandler)
}

// RegisterHandler creates a user, applies the initial credit, and returns a JWT.
func (pr *PanelRouter) RegisterHandler(c *gin.Context) {
	var req authRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid request body")
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !validAuthInput(email, req.Password) {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "email and password are required")
		return
	}
	if pr.DB == nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "database not initialized")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to hash password")
		return
	}

	role := "user"
	if pr.isAdminEmail(email) {
		role = "admin"
	}
	user := model.User{Email: email, PasswordHash: string(hash), Role: role, Status: userStatusActive}
	if err := pr.createUserWithInitialCredit(c, &user); err != nil {
		if isUniqueConstraintError(err) {
			Error(c, http.StatusConflict, apiErrorConflict, "email already registered")
			return
		}
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to create user")
		return
	}

	token, err := authutil.GenerateJWT(user.ID, user.Email, pr.Config.Auth.JWT.Secret)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to generate token")
		return
	}

	Success(c, gin.H{"token": token, "user": authUserFromModel(user)})
}

// LoginHandler validates credentials and returns a JWT.
func (pr *PanelRouter) LoginHandler(c *gin.Context) {
	var req authRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "invalid request body")
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !validAuthInput(email, req.Password) {
		Error(c, http.StatusBadRequest, apiErrorBadRequest, "email and password are required")
		return
	}
	if pr.DB == nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "database not initialized")
		return
	}

	var user model.User
	if err := pr.DB.WithContext(c.Request.Context()).Where("email = ? AND status = ?", email, userStatusActive).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			Error(c, http.StatusUnauthorized, apiErrorUnauthorized, "invalid email or password")
			return
		}
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to load user")
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		Error(c, http.StatusUnauthorized, apiErrorUnauthorized, "invalid email or password")
		return
	}

	// Auto-promote admin role based on admin_emails config if DB role is stale.
	if pr.isAdminEmail(user.Email) && user.Role != "admin" {
		pr.DB.WithContext(c.Request.Context()).Model(&user).Update("role", "admin")
		user.Role = "admin"
	}

	token, err := authutil.GenerateJWT(user.ID, user.Email, pr.Config.Auth.JWT.Secret)
	if err != nil {
		Error(c, http.StatusInternalServerError, apiErrorInternal, "failed to generate token")
		return
	}

	Success(c, gin.H{"token": token, "user": authUserFromModel(user)})
}

func validAuthInput(email, password string) bool {
	return email != "" && strings.Contains(email, "@") && len(password) >= 8
}

func (pr *PanelRouter) createUserWithInitialCredit(c *gin.Context, user *model.User) error {
	if pr.Ledger != nil {
		if err := pr.DB.WithContext(c.Request.Context()).Create(user).Error; err != nil {
			return err
		}
		if err := pr.Ledger.Credit(c.Request.Context(), user.ID, initialRegisterCredit, "initial_register_credit"); err != nil {
			return err
		}
		user.Balance = initialRegisterCredit
		return nil
	}

	return pr.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		user.Balance = initialRegisterCredit
		if err := tx.Create(user).Error; err != nil {
			return err
		}
		return tx.Create(&model.BalanceLog{
			UserID:    user.ID,
			Amount:    initialRegisterCredit,
			Type:      "credit",
			Reference: "initial_register_credit",
		}).Error
	})
}

func authUserFromModel(user model.User) authUserResponse {
	return authUserResponse{
		ID:        user.ID,
		Email:     user.Email,
		Role:      user.Role,
		Balance:   user.Balance,
		Status:    user.Status,
		CreatedAt: user.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

func isUniqueConstraintError(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "UNIQUE constraint"))
}

// isAdminEmail reports whether the supplied email matches a configured admin
// address (case-insensitive, trimmed). Returns false when no admin emails
// are configured.
func (pr *PanelRouter) isAdminEmail(email string) bool {
	if pr == nil || pr.Config == nil {
		return false
	}
	normalized := strings.ToLower(strings.TrimSpace(email))
	if normalized == "" {
		return false
	}
	return slices.ContainsFunc(pr.Config.Auth.AdminEmails, func(ae string) bool {
		return strings.EqualFold(strings.TrimSpace(ae), normalized)
	})
}

// StartCacheCleanup launches the in-memory API-key cache sweeper.
func (pr *PanelRouter) StartCacheCleanup(ctx context.Context) {
	if pr == nil || pr.APIKeyCache == nil {
		return
	}
	pr.APIKeyCache.Start(ctx)
}
