package handlers

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RateLimiter is a simple in-memory fixed-window counter keyed by an arbitrary
// string (client IP, phone number, ...). It is safe for concurrent use. Entries
// are lazily expired on access, with a periodic sweep once the table grows large
// enough that memory growth could otherwise become unbounded.
type RateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string]*rateEntry
	// lastSweep throttles the full-map sweep: without it, every request past
	// 10k keys pays O(n), turning a key-flood into CPU-DoS.
	lastSweep time.Time
}

type rateEntry struct {
	count int
	reset time.Time
}

// NewRateLimiter returns a RateLimiter allowing up to limit requests per window.
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	if limit <= 0 {
		limit = 1
	}
	if window <= 0 {
		window = time.Minute
	}
	return &RateLimiter{limit: limit, window: window, entries: make(map[string]*rateEntry)}
}

// Allow records a request for key and reports whether it is within the limit.
func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()

	e, ok := rl.entries[key]
	if !ok || now.After(e.reset) {
		rl.entries[key] = &rateEntry{count: 1, reset: now.Add(rl.window)}
	} else {
		e.count++
		if e.count > rl.limit {
			return false
		}
	}

	if len(rl.entries) > 10000 && now.Sub(rl.lastSweep) > time.Minute {
		rl.lastSweep = now
		for k, en := range rl.entries {
			if now.After(en.reset) {
				delete(rl.entries, k)
			}
		}
	}
	return true
}

// Middleware limits requests by client IP.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.Allow(clientIP(r)) {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(rl.window.Seconds())))
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP returns the caller's IP. Behind the trusted loopback reverse proxy
// (Caddy on the same host, per deploy.sh) the X-Forwarded-For list is used;
// otherwise the TCP peer address. Remote addresses other than loopback are never
// trusted to supply X-Forwarded-For, so off-host clients cannot spoof their key.
//
// The RIGHTMOST XFF entry is used, not the first. Caddy appends the connecting
// client's address to whatever XFF header the client supplied, so the rightmost
// hop is always the real client IP as observed by the trusted proxy — an
// attacker cannot remove or overwrite it. Taking the first (leftmost) entry
// would trust a value the client fully controls, defeating the per-IP rate
// limiters (admin login attempts, OTP IP lockout). The rightmost entry must
// parse as an IP address; anything else (a misconfigured proxy chain) falls
// back to the TCP peer so limiter keys can never be attacker-chosen.
func clientIP(r *http.Request) string {
	ip := remoteIP(r)
	if ip == "127.0.0.1" || ip == "::1" {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			for i := len(parts) - 1; i >= 0; i-- {
				candidate := strings.TrimSpace(parts[i])
				if candidate == "" {
					continue
				}
				if net.ParseIP(candidate) != nil {
					return candidate
				}
				// Unparseable rightmost hop: the proxy chain is not behaving
				// as expected, so do not trust any part of the header.
				break
			}
		}
	}
	return ip
}
