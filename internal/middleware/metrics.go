package middleware

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// httpRequestsTotal counts every completed HTTP request, labelled by method,
	// route template (not the actual path — to avoid high cardinality from
	// variable segments like namespace names), and status code.
	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "capp_backend_http_requests_total",
		Help: "Total number of HTTP requests handled by capp-backend.",
	}, []string{"method", "route", "status"})

	// httpRequestDuration records the full handler latency per route.
	httpRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "capp_backend_http_request_duration_seconds",
		Help:    "Latency distribution of HTTP requests handled by capp-backend.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})
)

// Metrics returns a Gin middleware that records Prometheus counters and
// histograms for every HTTP request. Metrics are recorded after the handler
// chain completes so the final status code is known.
//
// The "route" label uses c.FullPath() (the Gin route pattern, e.g.
// /api/v1/clusters/:cluster/namespaces/:namespace/capps) rather than the
// actual request path. This keeps metric cardinality low regardless of how
// many clusters, namespaces, or app names exist.
func Metrics() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		route := c.FullPath()
		if route == "" {
			// Unmatched routes (404s) group under a single label to avoid cardinality explosion.
			route = "unmatched"
		}

		status := strconv.Itoa(c.Writer.Status())
		elapsed := time.Since(start).Seconds()

		httpRequestsTotal.WithLabelValues(c.Request.Method, route, status).Inc()
		httpRequestDuration.WithLabelValues(c.Request.Method, route).Observe(elapsed)
	}
}
