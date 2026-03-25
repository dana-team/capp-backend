package middleware

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// RateLimit returns a Gin middleware that enforces a per-client-IP token-bucket
// rate limit. Each unique client IP gets its own independent limiter, so a
// single slow client cannot starve others.
//
// Requests that exceed the limit receive a 429 Too Many Requests response with
// a Retry-After: 1 header. Requests from clients that have never been seen
// before always pass (the limiter starts with a full bucket).
//
// rps is the sustained request rate (tokens/second). burst is the maximum
// burst capacity above that rate.
func RateLimit(rps float64, burst int) gin.HandlerFunc {
	limiters := newIPLimiterStore(rps, burst)
	return func(c *gin.Context) {
		ip := c.ClientIP()
		limiter := limiters.get(ip)
		if !limiter.Allow() {
			c.Header("Retry-After", "1")
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": gin.H{
					"code":    "RATE_LIMITED",
					"message": "too many requests",
					"status":  http.StatusTooManyRequests,
				},
			})
			return
		}
		c.Next()
	}
}

// ipLimiterStore holds one rate.Limiter per client IP address.
// New IPs are lazily initialised on first request.
type ipLimiterStore struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	rps      rate.Limit
	burst    int
}

func newIPLimiterStore(rps float64, burst int) *ipLimiterStore {
	return &ipLimiterStore{
		limiters: make(map[string]*rate.Limiter),
		rps:      rate.Limit(rps),
		burst:    burst,
	}
}

func (s *ipLimiterStore) get(ip string) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.limiters[ip]
	if !ok {
		l = rate.NewLimiter(s.rps, s.burst)
		s.limiters[ip] = l
	}
	return l
}
