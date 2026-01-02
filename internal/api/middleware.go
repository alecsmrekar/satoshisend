package api

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"satoshisend/internal/logging"
)

// Logger wraps a handler with request logging.
func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(wrapped, r)

		// Only log API requests
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			return
		}

		// Skip logging for status polling endpoints to reduce noise
		if strings.HasSuffix(r.URL.Path, "/status") {
			return
		}

		logging.HTTP.Printf("%s %s %d %s", r.Method, r.URL.Path, wrapped.status, time.Since(start))
	})
}

// CORSConfig holds CORS middleware configuration.
type CORSConfig struct {
	AllowedOrigins []string // Empty or nil means allow all (development mode)
}

// CORS adds CORS headers with configurable origin restrictions.
// In production, set AllowedOrigins to restrict which domains can access the API.
func CORS(cfg CORSConfig) func(http.Handler) http.Handler {
	allowAll := len(cfg.AllowedOrigins) == 0

	// Build a set for O(1) lookup
	allowedSet := make(map[string]bool)
	for _, origin := range cfg.AllowedOrigins {
		allowedSet[origin] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			if allowAll {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if origin != "" && allowedSet[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			}

			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, HEAD, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// RateLimitConfig holds rate limiting configuration.
type RateLimitConfig struct {
	// RequestsPerSecond is the rate limit for general API requests per IP
	RequestsPerSecond float64
	// BurstSize is the maximum burst size allowed
	BurstSize int
	// UploadRequestsPerMinute is the rate limit for upload requests per IP
	UploadRequestsPerMinute float64
	// UploadBurstSize is the maximum burst for uploads
	UploadBurstSize int
}

// DefaultRateLimitConfig returns sensible defaults for rate limiting.
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		RequestsPerSecond:       10,  // 10 requests per second for general API
		BurstSize:               20,  // Allow bursts up to 20
		UploadRequestsPerMinute: 10,  // 10 uploads per minute
		UploadBurstSize:         3,   // Allow burst of 3 uploads
	}
}

// trackedLimiter wraps a rate.Limiter with last-access tracking.
type trackedLimiter struct {
	limiter  *rate.Limiter
	lastSeen int64 // Unix timestamp, updated atomically
}

// ipRateLimiter manages per-IP rate limiters with automatic cleanup.
type ipRateLimiter struct {
	limiters sync.Map // map[string]*trackedLimiter
	rate     rate.Limit
	burst    int
	ttl      time.Duration // How long to keep idle limiters
	stopCh   chan struct{}
	stopped  bool
	mu       sync.Mutex
}

func newIPRateLimiter(r float64, burst int) *ipRateLimiter {
	return newIPRateLimiterWithTTL(r, burst, 10*time.Minute)
}

func newIPRateLimiterWithTTL(r float64, burst int, ttl time.Duration) *ipRateLimiter {
	rl := &ipRateLimiter{
		rate:   rate.Limit(r),
		burst:  burst,
		ttl:    ttl,
		stopCh: make(chan struct{}),
	}
	go rl.cleanupLoop()
	return rl
}

func (rl *ipRateLimiter) getLimiter(ip string) *rate.Limiter {
	now := time.Now().Unix()

	if val, exists := rl.limiters.Load(ip); exists {
		tracked := val.(*trackedLimiter)
		// Update last seen time (atomic via sync.Map reload pattern is fine here,
		// slight races are acceptable for TTL tracking)
		tracked.lastSeen = now
		return tracked.limiter
	}

	tracked := &trackedLimiter{
		limiter:  rate.NewLimiter(rl.rate, rl.burst),
		lastSeen: now,
	}
	// Use LoadOrStore to handle race conditions
	actual, _ := rl.limiters.LoadOrStore(ip, tracked)
	return actual.(*trackedLimiter).limiter
}

// cleanupLoop periodically removes stale rate limiters.
func (rl *ipRateLimiter) cleanupLoop() {
	ticker := time.NewTicker(rl.ttl / 2) // Cleanup at half the TTL interval
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rl.cleanup()
		case <-rl.stopCh:
			return
		}
	}
}

// cleanup removes limiters that haven't been used within the TTL.
func (rl *ipRateLimiter) cleanup() {
	cutoff := time.Now().Add(-rl.ttl).Unix()
	var removed int

	rl.limiters.Range(func(key, value any) bool {
		tracked := value.(*trackedLimiter)
		if tracked.lastSeen < cutoff {
			rl.limiters.Delete(key)
			removed++
		}
		return true
	})

	if removed > 0 {
		logging.Internal.Printf("rate limiter cleanup: removed %d stale entries", removed)
	}
}

// Stop halts the cleanup goroutine. Call this during graceful shutdown.
func (rl *ipRateLimiter) Stop() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if !rl.stopped {
		close(rl.stopCh)
		rl.stopped = true
	}
}

// RateLimiterMiddleware wraps rate limiting middleware with cleanup management.
type RateLimiterMiddleware struct {
	generalLimiter *ipRateLimiter
	uploadLimiter  *ipRateLimiter
}

// Middleware returns the HTTP middleware function.
func (rlm *RateLimiterMiddleware) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := extractIP(r)

		// Use stricter limits for upload endpoint
		var limiter *rate.Limiter
		if r.Method == "POST" && r.URL.Path == "/api/upload" {
			limiter = rlm.uploadLimiter.getLimiter(ip)
		} else {
			limiter = rlm.generalLimiter.getLimiter(ip)
		}

		if !limiter.Allow() {
			logging.HTTP.Printf("rate limit exceeded for %s on %s %s", ip, r.Method, r.URL.Path)
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Stop halts all cleanup goroutines. Call this during graceful shutdown.
func (rlm *RateLimiterMiddleware) Stop() {
	rlm.generalLimiter.Stop()
	rlm.uploadLimiter.Stop()
}

// NewRateLimiter creates a rate limiting middleware with automatic cleanup.
// Call Stop() on the returned middleware during graceful shutdown.
func NewRateLimiter(cfg RateLimitConfig) *RateLimiterMiddleware {
	return &RateLimiterMiddleware{
		generalLimiter: newIPRateLimiter(cfg.RequestsPerSecond, cfg.BurstSize),
		uploadLimiter:  newIPRateLimiter(cfg.UploadRequestsPerMinute/60, cfg.UploadBurstSize),
	}
}

// extractIP gets the client IP from the request, checking X-Forwarded-For for proxied requests.
func extractIP(r *http.Request) string {
	// Check X-Forwarded-For header (set by reverse proxies)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the first IP (original client)
		if idx := strings.Index(xff, ","); idx != -1 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}

	// Check X-Real-IP header
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	// Fall back to RemoteAddr
	// RemoteAddr is in the form "IP:port", so strip the port
	addr := r.RemoteAddr
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		return addr[:idx]
	}
	return addr
}
