package handlers

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestClientIPRightmostForwardedHop verifies that behind the trusted loopback
// proxy only the RIGHTMOST X-Forwarded-For entry — the one Caddy appends — is
// trusted. Trusting the leftmost entry would let any client rotate fake IPs and
// bypass the per-IP rate limiters.
func TestClientIPRightmostForwardedHop(t *testing.T) {
	tests := []struct {
		name   string
		peer   string
		xff    string
		wantIP string
	}{
		{
			name:   "spoofed first hop is ignored, proxy-appended hop wins",
			peer:   "127.0.0.1:5555",
			xff:    "6.6.6.6, 203.0.113.7",
			wantIP: "203.0.113.7",
		},
		{
			name:   "single spoofed hop cannot hide the real client",
			peer:   "127.0.0.1:5555",
			xff:    "6.6.6.6, 198.51.100.9, 203.0.113.7",
			wantIP: "203.0.113.7",
		},
		{
			name:   "clean single-hop proxy header",
			peer:   "127.0.0.1:5555",
			xff:    "203.0.113.7",
			wantIP: "203.0.113.7",
		},
		{
			name:   "trailing empty entries skipped",
			peer:   "127.0.0.1:5555",
			xff:    "203.0.113.7, ",
			wantIP: "203.0.113.7",
		},
		{
			name:   "unparseable rightmost hop falls back to TCP peer",
			peer:   "127.0.0.1:5555",
			xff:    "203.0.113.7, not-an-ip",
			wantIP: "127.0.0.1",
		},
		{
			name:   "no XFF header falls back to TCP peer",
			peer:   "127.0.0.1:5555",
			xff:    "",
			wantIP: "127.0.0.1",
		},
		{
			name:   "direct non-loopback client: XFF never trusted",
			peer:   "198.51.100.9:1234",
			xff:    "203.0.113.7",
			wantIP: "198.51.100.9",
		},
		{
			name:   "IPv6 loopback peer with IPv6 forwarded hop",
			peer:   "[::1]:5555",
			xff:    "2001:db8::1",
			wantIP: "2001:db8::1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.peer
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := clientIP(r); got != tc.wantIP {
				t.Fatalf("clientIP() = %q, want %q", got, tc.wantIP)
			}
		})
	}
}

// TestClientIPSpoofCannotEvadeAdminLimiter simulates the attack from the audit:
// a client behind the proxy sending a fresh fake XFF per request must stay
// pinned to its real (proxy-observed) IP, so the admin login limiter counts all
// attempts against one bucket.
func TestClientIPSpoofCannotEvadeAdminLimiter(t *testing.T) {
	rl := NewRateLimiter(3, time.Minute) // 3 requests per window

	for i := 0; i < 3; i++ {
		r := httptest.NewRequest("POST", "/admin/login", nil)
		r.RemoteAddr = "127.0.0.1:5555"
		r.Header.Set("X-Forwarded-For", "10.0.0."+string(rune('a'+i))) // fresh fake IP each time
		if got := clientIP(r); got != "127.0.0.1" {
			t.Fatalf("attempt %d: clientIP() = %q, want TCP peer fallback for unparseable hop", i+1, got)
		}
		if !rl.Allow(clientIP(r)) {
			t.Fatalf("attempt %d: limiter rejected early", i+1)
		}
	}

	// The 4th attempt from the same bucket must be rejected.
	r := httptest.NewRequest("POST", "/admin/login", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "10.0.0.z")
	if rl.Allow(clientIP(r)) {
		t.Fatal("4th attempt allowed: spoofed XFF evaded the rate limiter")
	}
}
