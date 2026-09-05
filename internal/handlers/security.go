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
// policy, and a permissions policy. On secure requests (direct TLS or the
// trusted loopback TLS proxy) Strict-Transport-Security is added so browsers
// refuse to downgrade to plain HTTP.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy", cspHeader)
		if requestIsSecure(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
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

// SameOrigin is a CSRF mitigation for cookie-protected state-changing routes.
// When an Origin header is present (browsers send it on cross-site and, on
// modern browsers, same-site POSTs) it must match the request Host. When it is
// absent:
//   - a Referer header, if present, must reference the request Host;
//   - a request carrying neither header but presenting an ambient session or
//     admin cookie is rejected — a real browser always sends Origin or Referer
//     on a mutating navigation/XHR, so a cookie-carrying POST without either is
//     a stripped-header request, not a legitimate user;
//   - requests with no cookies at all are allowed (non-browser API clients
//     cannot carry ambient credentials from a victim).
//
// The double-submit CSRF token remains the primary defense; this middleware
// shrinks the attack surface where that token may have been planted.
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
			} else if referer := r.Header.Get("Referer"); referer != "" {
				u, err := url.Parse(referer)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
			} else if hasAmbientCredentials(r) {
				// Cookie-authenticated POST with no Origin and no Referer.
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// hasAmbientCredentials reports whether the request presents a session or admin
// cookie — i.e. credentials a browser attaches automatically and an attacker
// could ride. Cookie names cover both the plain-HTTP and the __Host- prefixed
// variants.
func hasAmbientCredentials(r *http.Request) bool {
	for _, name := range []string{"session", "__Host-session", adminCookieName} {
		if _, err := r.Cookie(name); err == nil {
			return true
		}
	}
	return false
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
