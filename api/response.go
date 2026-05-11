package api

import "github.com/gin-gonic/gin"

// APIResponse is the unified JSON response envelope shared by all panel handlers.
type APIResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Success writes a successful unified JSON response.
func Success(c *gin.Context, data any) {
	c.JSON(200, APIResponse{
		Code:    0,
		Message: "ok",
		Data:    data,
	})
}

// Error writes a failed unified JSON response.
func Error(c *gin.Context, httpStatus int, code int, msg string) {
	c.JSON(httpStatus, APIResponse{
		Code:    code,
		Message: msg,
	})
}
