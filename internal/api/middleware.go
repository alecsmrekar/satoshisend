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

		// Skip logging for status polling endpoints to reduce noise
		if strings.HasSuffix(r.URL.Path, "/status") && strings.HasPrefix(r.URL.Path, "/api/file/") {
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

// ipRateLimiter manages per-IP rate limiters.
type ipRateLimiter struct {
	limiters sync.Map // map[string]*rate.Limiter
	rate     rate.Limit
	burst    int
}

func newIPRateLimiter(r float64, burst int) *ipRateLimiter {
	return &ipRateLimiter{
		rate:  rate.Limit(r),
		burst: burst,
	}
}

func (rl *ipRateLimiter) getLimiter(ip string) *rate.Limiter {
	if limiter, exists := rl.limiters.Load(ip); exists {
		return limiter.(*rate.Limiter)
	}

	limiter := rate.NewLimiter(rl.rate, rl.burst)
	rl.limiters.Store(ip, limiter)
	return limiter
}

// RateLimit creates a rate limiting middleware.
// It applies different limits for upload endpoints vs general API requests.
func RateLimit(cfg RateLimitConfig) func(http.Handler) http.Handler {
	generalLimiter := newIPRateLimiter(cfg.RequestsPerSecond, cfg.BurstSize)
	uploadLimiter := newIPRateLimiter(cfg.UploadRequestsPerMinute/60, cfg.UploadBurstSize)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := extractIP(r)

			// Use stricter limits for upload endpoint
			var limiter *rate.Limiter
			if r.Method == "POST" && r.URL.Path == "/api/upload" {
				limiter = uploadLimiter.getLimiter(ip)
			} else {
				limiter = generalLimiter.getLimiter(ip)
			}

			if !limiter.Allow() {
				logging.HTTP.Printf("rate limit exceeded for %s on %s %s", ip, r.Method, r.URL.Path)
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}

			next.ServeHTTP(w, r)
		})
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
