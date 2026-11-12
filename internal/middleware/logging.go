package middleware

import (
	"errors"
	"strings"
	"time"

	"github.com/dana-team/capp-backend/internal/apierrors"
	"github.com/dana-team/capp-backend/internal/cluster"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// causeSafeToLog lists the APIError codes whose constructor already
// sanitizes its cause before storing it (dropping tokens/URLs), so it is
// safe to put in server logs. Every other APIError's cause is excluded by
// default: NewInternal, for example, wraps whatever error a lower layer
// returned, which can carry credentials or other request-specific detail.
var causeSafeToLog = map[string]bool{
	apierrors.CodeGitopsFailed:          true,
	apierrors.CodePrometheusQueryFailed: true,
	apierrors.CodePrometheusTimeout:     true,
}

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

		// Include handler errors stored via c.Error() by the error response
		// layer so that sanitized client responses can still be debugged.
		// APIError.Error() deliberately omits its cause, since for most codes
		// (NewInternal in particular) it can carry request-specific detail
		// unsafe even for server logs. Only unwrap the codes in
		// causeSafeToLog, whose constructors already sanitize the cause.
		if len(c.Errors) > 0 {
			fields = append(fields, zap.String("error", c.Errors.String()))
			var causes []string
			for _, ginErr := range c.Errors {
				var apiErr *apierrors.APIError
				if errors.As(ginErr.Err, &apiErr) && causeSafeToLog[apiErr.Code] {
					if cause := errors.Unwrap(apiErr); cause != nil {
						causes = append(causes, cause.Error())
					}
				}
			}
			if len(causes) > 0 {
				fields = append(fields, zap.String("errorCause", strings.Join(causes, "; ")))
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
