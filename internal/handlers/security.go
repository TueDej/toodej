package handlers

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// SecurityHeaders attaches a baseline set of HTTP security headers to every
// response: a Content-Security-Policy restricted to the known CDNs used by the
// templates, clickjacking protection, MIME-sniffing prevention, a referrer
// policy, and a permissions policy.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy", cspHeader)
		next.ServeHTTP(w, r)
	})
}

// cspHeader restricts script/style/image loading to first-party resources and
// the exact CDNs the templates depend on (Tailwind, HTMX, Google Fonts, and the
// e-Namad trust seal). 'unsafe-inline'/'unsafe-eval' are required by the inline
// template scripts and the Tailwind Play CDN; vendoring those assets would allow
// removing them. Frame-ancestors and base-uri are locked down to block common
// injection payloads. form-action is omitted because the checkout flow uses a
// 303 redirect to an external payment gateway (Zarinpal); SameOrigin middleware
// provides CSRF protection instead.
const cspHeader = "" +
	"default-src 'self'; " +
	"script-src 'self' 'unsafe-inline' 'unsafe-eval' https://cdn.tailwindcss.com https://unpkg.com https://trustseal.enamad.ir; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com https://cdn.tailwindcss.com; " +
	"font-src 'self' data: https://fonts.gstatic.com; " +
	"img-src 'self' data: https://trustseal.enamad.ir; " +
	"connect-src 'self' https://cdn.tailwindcss.com https://fonts.googleapis.com; " +
	"frame-src https://trustseal.enamad.ir; " +
	"worker-src 'self' blob:; " +
	"base-uri 'self'; " +
	"frame-ancestors 'none'; " +
	"object-src 'none';"

// SameOrigin is a lightweight CSRF mitigation for cookie- and Basic-Auth-
// protected state-changing routes. Browsers attach an Origin header to
// cross-site state-changing requests; when present it must match the request
// Host or the request is rejected. Non-browser clients that omit Origin are
// allowed (they cannot carry ambient cookie/Basic credentials from a victim).
func SameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut ||
			r.Method == http.MethodPatch || r.Method == http.MethodDelete {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// remoteIP returns the client's IP from the TCP connection.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// requestIsSecure reports whether the request arrived over TLS, so that the
// session cookie's Secure flag can be set only when it will not break the
// plain-HTTP development flow. Direct TLS is detected from the connection;
// behind the trusted loopback proxy (Caddy), X-Forwarded-Proto is used.
func requestIsSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	ip := remoteIP(r)
	if ip == "127.0.0.1" || ip == "::1" {
		return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	}
	return false
}
