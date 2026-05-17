package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// maxIPLimiters is the upper bound on tracked unique client IPs. When the
// limit is reached, the least-recently-seen entries are evicted to make room.
// 10 000 entries × ~200 bytes ≈ 2 MB — well within acceptable memory usage.
const maxIPLimiters = 10_000

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
	limiters := newIPLimiterStore(rps, burst, maxIPLimiters)
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

// ipEntry pairs a rate limiter with the time it was last accessed, enabling
// eviction of stale entries when the store reaches capacity.
type ipEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// ipLimiterStore holds one rate.Limiter per client IP address, bounded to
// maxEntries. When full, the least-recently-seen entries are evicted.
type ipLimiterStore struct {
	mu         sync.Mutex
	limiters   map[string]*ipEntry
	rps        rate.Limit
	burst      int
	maxEntries int
}

func newIPLimiterStore(rps float64, burst int, maxEntries int) *ipLimiterStore {
	return &ipLimiterStore{
		limiters:   make(map[string]*ipEntry),
		rps:        rate.Limit(rps),
		burst:      burst,
		maxEntries: maxEntries,
	}
}

func (s *ipLimiterStore) get(ip string) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()

	if entry, ok := s.limiters[ip]; ok {
		entry.lastSeen = time.Now()
		return entry.limiter
	}

	if len(s.limiters) >= s.maxEntries {
		s.evictOldest()
	}

	l := rate.NewLimiter(s.rps, s.burst)
	s.limiters[ip] = &ipEntry{limiter: l, lastSeen: time.Now()}
	return l
}

// evictOldest removes the entry with the oldest lastSeen time. Must be
// called with s.mu held.
func (s *ipLimiterStore) evictOldest() {
	var oldestIP string
	var oldestTime time.Time
	for ip, entry := range s.limiters {
		if oldestIP == "" || entry.lastSeen.Before(oldestTime) {
			oldestIP = ip
			oldestTime = entry.lastSeen
		}
	}
	if oldestIP != "" {
		delete(s.limiters, oldestIP)
	}
}
