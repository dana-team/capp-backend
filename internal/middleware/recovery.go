package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Recovery returns a Gin middleware that catches panics in downstream handlers,
// logs the stack trace, and returns a 500 Internal Server Error response.
// Without this middleware, a panic in a handler brings down the entire goroutine.
func Recovery(logger *zap.Logger) gin.HandlerFunc {
	return gin.RecoveryWithWriter(nil, func(c *gin.Context, err any) {
		logger.Error("panic recovered",
			zap.Any("error", err),
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
		)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"code":    "INTERNAL_ERROR",
				"message": "an unexpected error occurred",
				"status":  http.StatusInternalServerError,
			},
		})
	})
}
