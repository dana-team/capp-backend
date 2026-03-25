package middleware

import (
	"time"

	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Logging returns a Gin middleware that emits one structured log entry per
// request after the handler chain completes. Fields included:
//
//   - requestId  — the X-Request-ID header value, or a generated UUID
//   - method     — HTTP method
//   - path       — request path (with query string)
//   - status     — HTTP response status code
//   - latencyMs  — handler duration in milliseconds
//   - ip         — client IP address
//   - userAgent  — User-Agent header
//
// Log level is determined by the response status:
//   - 5xx → error
//   - 4xx → warn
//   - otherwise → info
func Logging(logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		c.Next()

		latency := time.Since(start)
		status := c.Writer.Status()

		fields := []zap.Field{
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.RequestURI()),
			zap.Int("status", status),
			zap.Duration("latency", latency),
			zap.String("ip", c.ClientIP()),
			zap.String("userAgent", c.Request.UserAgent()),
		}

		// Include the cluster name when it was resolved by the cluster middleware.
		if meta, ok := c.Get(string(ClusterMetaKey)); ok {
			if m, ok := meta.(cluster.ClusterMeta); ok {
				fields = append(fields, zap.String("cluster", m.Name))
			}
		}

		switch {
		case status >= 500:
			logger.Error("request completed", fields...)
		case status >= 400:
			logger.Warn("request completed", fields...)
		default:
			logger.Info("request completed", fields...)
		}
	}
}
