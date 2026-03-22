package main

import (
	"context"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultThrottleAfter = 3                // start throttling after this many failures
	defaultDecayInterval = 1 * time.Minute  // remove one strike per interval of no activity
	defaultCleanupEvery  = 5 * time.Minute
	maxTrackedIPs        = 100000          // hard cap to prevent memory exhaustion
	maxThrottleDelay     = 2 * time.Second // above this, just reject immediately
)

type ipRecord struct {
	failures    int
	lastFailure time.Time
}

type RateLimiter struct {
	mu sync.Mutex
	ips map[string]*ipRecord

	throttleAfter int
	decayInterval time.Duration

	// trustedProxies are CIDR ranges that are allowed to set X-Real-IP / X-Forwarded-For.
	trustedProxies []*net.IPNet

	cancel context.CancelFunc
}

func NewRateLimiter(trustedProxies []string) *RateLimiter {
	var nets []*net.IPNet
	for _, cidr := range trustedProxies {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			// Try as single IP.
			ip := net.ParseIP(cidr)
			if ip != nil {
				if ip.To4() != nil {
					_, ipNet, _ = net.ParseCIDR(cidr + "/32")
				} else {
					_, ipNet, _ = net.ParseCIDR(cidr + "/128")
				}
			}
		}
		if ipNet != nil {
			nets = append(nets, ipNet)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	rl := &RateLimiter{
		ips:            make(map[string]*ipRecord),
		throttleAfter:  defaultThrottleAfter,
		decayInterval:  defaultDecayInterval,
		trustedProxies: nets,
		cancel:         cancel,
	}

	go rl.cleanup(ctx)
	return rl
}

// Close stops the background cleanup goroutine.
func (rl *RateLimiter) Close() {
	rl.cancel()
}

// RecordFailure registers a failed auth attempt from an IP.
func (rl *RateLimiter) RecordFailure(ip string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Hard cap: if we're at capacity and this is a new IP, reject silently.
	rec, ok := rl.ips[ip]
	if !ok {
		if len(rl.ips) >= maxTrackedIPs {
			slog.Warn("rate limiter at capacity, dropping new entry", "ip", ip)
			return
		}
		rec = &ipRecord{}
		rl.ips[ip] = rec
	}

	rec.failures++
	rec.lastFailure = time.Now()

	if rec.failures >= rl.throttleAfter {
		slog.Warn("IP throttled",
			"ip", ip,
			"failures", rec.failures,
		)
	}
}

// Check returns the action to take for a given IP.
func (rl *RateLimiter) Check(ip string) (allowed bool) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rec, ok := rl.ips[ip]
	if !ok {
		return true
	}

	// Apply decay: reduce failures based on time since last failure.
	if rl.decayInterval > 0 && rec.failures > 0 {
		elapsed := time.Since(rec.lastFailure)
		decay := int(elapsed / rl.decayInterval)
		if decay > 0 {
			rec.failures = max(0, rec.failures-decay)
			if rec.failures == 0 {
				delete(rl.ips, ip)
				return true
			}
		}
	}

	// If in throttle range, reject once exponential delay exceeds the threshold.
	if rec.failures >= rl.throttleAfter {
		exponent := rec.failures - rl.throttleAfter
		if 1000*math.Pow(2, float64(exponent)) > float64(maxThrottleDelay/time.Millisecond) {
			return false
		}
	}

	return true
}

// cleanup periodically removes stale IP records until ctx is cancelled.
func (rl *RateLimiter) cleanup(ctx context.Context) {
	ticker := time.NewTicker(defaultCleanupEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rl.mu.Lock()
			now := time.Now()
			for ip, rec := range rl.ips {
				if now.Sub(rec.lastFailure) > rl.decayInterval*time.Duration(rec.failures+1) {
					delete(rl.ips, ip)
				}
			}
			rl.mu.Unlock()
		}
	}
}

// RateLimitMiddleware throttles IPs that send bad auth. Valid keys are never throttled.
func RateLimitMiddleware(rl *RateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := rl.clientIP(r)

		if !rl.Check(ip) {
			// Return 429 with Retry-After instead of holding a goroutine sleeping.
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "too many requests")
			return
		}

		// Wrap the response writer to detect auth failures.
		rw := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rw, r)

		// Only record failures for bad auth — valid keys are never penalized.
		if rw.status == http.StatusUnauthorized {
			rl.RecordFailure(ip)
		}
	})
}

// clientIP extracts the real client IP. Only trusts X-Real-IP / X-Forwarded-For
// from connections originating from configured trusted proxies.
func (rl *RateLimiter) clientIP(r *http.Request) string {
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}

	// Only honor proxy headers if the direct connection is from a trusted proxy.
	if rl.isTrustedProxy(remoteHost) {
		if ip := r.Header.Get("X-Real-IP"); ip != "" {
			return ip
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}

	return remoteHost
}

func (rl *RateLimiter) isTrustedProxy(host string) bool {
	if len(rl.trustedProxies) == 0 {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, cidr := range rl.trustedProxies {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// statusRecorder captures the HTTP status code written by downstream handlers.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (sr *statusRecorder) WriteHeader(code int) {
	if !sr.wroteHeader {
		sr.status = code
		sr.wroteHeader = true
	}
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if !sr.wroteHeader {
		sr.status = http.StatusOK
		sr.wroteHeader = true
	}
	return sr.ResponseWriter.Write(b)
}

// Flush supports streaming through the rate limit middleware.
func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
