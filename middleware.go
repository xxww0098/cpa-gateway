package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/xxww0098/cpa-gateway/ledger"
)

const (
	billingCtxGinKey = "billingCtx"
	traceIDHeader    = "X-Trace-ID"
	authTypeAPIKey   = "api_key"
	authTypeJWT      = "jwt"

	defaultHoldAmount           = 0.01
	defaultRequestBodyLimit     = 1 << 20
	defaultHoldTTL              = 5 * time.Minute
	middlewareErrorUnauthorized = 1001
	middlewareErrorPayment      = 2001
	middlewareErrorInternal     = 5001
)

type billingContextKey struct{}

// BillingCtx is the authenticated request context shared between
// handler_proxy.go and the root-side BillingMiddleware. The api/ package
// mirrors this type (api.BillingCtx) so panel handlers do not depend on
// root-package identifiers. Wave 4 deletes this root copy entirely.
type BillingCtx struct {
	UserID    uint
	ApiKeyID  uint
	GroupID   *uint
	RateMult  float64
	AuthType  string
	Email     string
	Status    string
	RequestID string
}

// GlobalLedger is initialized by main.go when database/Redis are available.
var GlobalLedger *ledger.Ledger

// BillingContextFromContext returns a BillingCtx previously injected into request context.
func BillingContextFromContext(ctx context.Context) (*BillingCtx, bool) {
	bc, ok := ctx.Value(billingContextKey{}).(*BillingCtx)
	return bc, ok
}

// BillingMiddleware creates a short-lived balance hold for authenticated proxy
// requests. It remains at the root until Wave 4 (Task 21) deletes it in favour
// of the SDK Builder's HoldMiddleware.
func BillingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		bc, ok := billingContextFromGin(c)
		if !ok || bc.UserID == 0 {
			writeErrorJSON(c, http.StatusUnauthorized, middlewareErrorUnauthorized, "authentication context required")
			c.Abort()
			return
		}

		requestID := traceIDFromGin(c)
		bc.RequestID = requestID
		setBillingContext(c, bc)

		body, err := peekAndRestoreBody(c, defaultRequestBodyLimit)
		if err != nil {
			writeErrorJSON(c, http.StatusBadRequest, middlewareErrorInternal, "failed to read request body")
			c.Abort()
			return
		}

		estimatedCost := estimateRequestCost(body, bc.RateMult)
		l := GlobalLedger
		if l == nil {
			writeErrorJSON(c, http.StatusInternalServerError, middlewareErrorInternal, "billing ledger not initialized")
			c.Abort()
			return
		}

		if err := l.Hold(c.Request.Context(), bc.UserID, estimatedCost, requestID, holdTTL()); err != nil {
			if errors.Is(err, ErrInsufficientBalance) {
				writeErrorJSON(c, http.StatusPaymentRequired, middlewareErrorPayment, "insufficient balance")
			} else {
				writeErrorJSON(c, http.StatusInternalServerError, middlewareErrorInternal, "billing hold failed")
			}
			c.Abort()
			return
		}

		c.Next()
	}
}

// writeErrorJSON writes a minimal JSON error response matching the envelope
// used by api.Error so the proxy surface stays consistent with the panel.
func writeErrorJSON(c *gin.Context, httpStatus, code int, msg string) {
	c.JSON(httpStatus, gin.H{"code": code, "message": msg})
}

func setBillingContext(c *gin.Context, bc *BillingCtx) {
	c.Set(billingCtxGinKey, bc)
	ctx := context.WithValue(c.Request.Context(), billingContextKey{}, bc)
	c.Request = c.Request.WithContext(ctx)
}

func billingContextFromGin(c *gin.Context) (*BillingCtx, bool) {
	value, ok := c.Get(billingCtxGinKey)
	if !ok {
		return BillingContextFromContext(c.Request.Context())
	}
	bc, ok := value.(*BillingCtx)
	return bc, ok
}

func traceIDFromGin(c *gin.Context) string {
	if value, ok := c.Get(traceIDHeader); ok {
		if requestID, ok := value.(string); ok && requestID != "" {
			return requestID
		}
	}
	requestID := strings.TrimSpace(c.Writer.Header().Get(traceIDHeader))
	if requestID != "" {
		return requestID
	}
	requestID = strings.TrimSpace(c.GetHeader(traceIDHeader))
	if requestID != "" {
		return requestID
	}
	return uuid.NewString()
}

func peekAndRestoreBody(c *gin.Context, limit int64) ([]byte, error) {
	if c.Request.Body == nil {
		return nil, nil
	}
	reader := io.LimitReader(c.Request.Body, limit+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	c.Request.Body.Close()
	if int64(len(body)) > limit {
		body = body[:limit]
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func estimateRequestCost(body []byte, rateMult float64) float64 {
	cost := defaultHoldAmount
	if GlobalConfig != nil && GlobalConfig.Billing.HoldAmount > 0 {
		cost = float64(GlobalConfig.Billing.HoldAmount)
	}
	if len(body) > 0 {
		estimatedTokens := float64(len(body)+3) / 4
		price := 0.0
		if GlobalConfig != nil {
			price = GlobalConfig.Billing.DefaultPricePer1KTokens
		}
		if price > 0 {
			cost = maxFloat(cost, (estimatedTokens/1000)*price)
		}
	}
	if rateMult <= 0 {
		rateMult = 1.0
	}
	return cost * rateMult
}

func holdTTL() time.Duration {
	if GlobalConfig != nil && GlobalConfig.Billing.HoldTTLSeconds > 0 {
		return time.Duration(GlobalConfig.Billing.HoldTTLSeconds) * time.Second
	}
	return defaultHoldTTL
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
