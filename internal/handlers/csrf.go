package handlers

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
)

// CSRF token configuration
const (
	csrfCookieName = "csrf_token" // plain-HTTP (development) cookie name
	// csrfCookieNameSecure is the __Host- prefixed name used when the request
	// is secure. The __Host- prefix makes the browser reject the cookie unless
	// it is Secure, Path=/, and Domain-less, so a sibling subdomain (or a
	// plain-HTTP page on the same site) cannot plant a shadowing value and
	// defeat the double-submit comparison.
	csrfCookieNameSecure = "__Host-csrf"
	csrfHeaderName       = "X-CSRF-Token"
	csrfFormField        = "csrf_token"
	csrfTokenBytes       = 32 // 256-bit token
)

// csrfCookieNameFor returns the CSRF cookie name for a request of the given
// security level. Production (behind TLS or the trusted TLS proxy) gets the
// __Host- prefix; plain-HTTP development keeps the legacy name so the local
// flow is not broken by prefix rules that require a secure context.
func csrfCookieNameFor(secure bool) string {
	if secure {
		return csrfCookieNameSecure
	}
	return csrfCookieName
}

// GenerateToken returns a cryptographically random CSRF token.
// Panics if crypto/rand fails (fail closed for security).
func GenerateToken() string {
	b := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// extractToken extracts the CSRF token from request, checking:
// 1. POST body form field (for regular form submissions)
// 2. Header (for HTMX/AJAX requests)
//
// The URL query string is deliberately NOT consulted: r.FormValue merges query
// and body, and a token accepted from the query can leak into Referer headers
// and shared/bookmarked URLs. The cookie is also not a valid source — the
// double-submit cookie pattern proves the request is first-party because the
// submitted token (form/header) must match the cookie, but the cookie itself is
// auto-sent by the browser on every request — including forged cross-site
// ones. Reading the cookie as the submitted token would make any cross-site
// POST trivially pass CSRF, so the token must come from a channel an attacker
// cannot set cross-origin.
func extractToken(r *http.Request) string {
	// Check POST body value only (never the URL query).
	if token := r.PostFormValue(csrfFormField); token != "" {
		return token
	}
	// Check header (used by HTMX)
	if token := r.Header.Get(csrfHeaderName); token != "" {
		return token
	}
	return ""
}

// cookieToken returns the CSRF token from the cookie, or empty string if not found.
// Both variants are checked (secure first): after a secure/plain flip the
// client may present the other name, and the comparison must still succeed
// against the token the page was rendered with.
func cookieToken(r *http.Request) string {
	for _, name := range []string{csrfCookieNameSecure, csrfCookieName} {
		if cookie, err := r.Cookie(name); err == nil && cookie.Value != "" {
			return cookie.Value
		}
	}
	return ""
}

// isMutating reports whether the HTTP method is mutating (requires CSRF protection).
func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// CSRFMiddleware validates the double-submit cookie pattern on mutating requests.
// It exempts certain paths that are handled by external services or have other protections.
func CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip CSRF check for non-mutating methods
		if !isMutating(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		// Skip CSRF check for specific endpoints that are handled externally
		switch r.URL.Path {
		case "/checkout/verify": // Zarinpal callback (external service)
			next.ServeHTTP(w, r)
			return
		}

		// Extract tokens from request and cookie
		requestToken := extractToken(r)
		cookieToken := cookieToken(r)

		// Validate presence and match (constant-time comparison)
		if requestToken == "" || cookieToken == "" ||
			subtle.ConstantTimeCompare([]byte(requestToken), []byte(cookieToken)) != 1 {
			http.Error(w, "CSRF token validation failed", http.StatusForbidden)
			return
		}

		// Token is valid, proceed with request
		next.ServeHTTP(w, r)
	})
}

// csrfCookie builds the Set-Cookie value for a CSRF token with the security
// attributes pinned by TestCSRFTokenCookieAttributes.
func csrfCookie(r *http.Request, token string) *http.Cookie {
	secure := requestIsSecure(r)
	return &http.Cookie{
		Name:     csrfCookieNameFor(secure),
		Value:    token,
		Path:     "/",
		HttpOnly: true, // Keep it out of JavaScript reach to mitigate XSS + CSRF token theft
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()), // Match session lifetime
	}
}

// ensureCSRFToken ensures a CSRF token cookie is set for the response.
// Returns the token value (new or existing).
func ensureCSRFToken(w http.ResponseWriter, r *http.Request) string {
	// If either variant cookie already exists, return its value (no need to set new cookie)
	if token := cookieToken(r); token != "" {
		return token
	}
	return rotateCSRFToken(w, r)
}

// rotateCSRFToken mints a fresh CSRF token and sets it as the cookie. Called
// on login (customer and admin) so a token that may have been observed before
// authentication is never carried into the authenticated session.
func rotateCSRFToken(w http.ResponseWriter, r *http.Request) string {
	token := GenerateToken()
	if w != nil {
		http.SetCookie(w, csrfCookie(r, token))
	}
	return token
}
