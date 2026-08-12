package handlers

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"

	"html/template"
)

// CSRF token configuration
const (
	csrfCookieName = "csrf_token"
	csrfHeaderName = "X-CSRF-Token"
	csrfFormField  = "csrf_token"
	csrfTokenBytes = 32 // 256-bit token
)

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
// 1. Form field (for regular form submissions)
// 2. Header (for HTMX/AJAX requests)
// 3. Cookie (fallback)
func extractToken(r *http.Request) string {
	// Check form value first
	if token := r.FormValue(csrfFormField); token != "" {
		return token
	}
	// Check header (used by HTMX)
	if token := r.Header.Get(csrfHeaderName); token != "" {
		return token
	}
	// Check cookie as fallback
	if cookie, err := r.Cookie(csrfCookieName); err == nil {
		return cookie.Value
	}
	return ""
}

// cookieToken returns the CSRF token from the cookie, or empty string if not found.
func cookieToken(r *http.Request) string {
	if cookie, err := r.Cookie(csrfCookieName); err == nil {
		return cookie.Value
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

// ensureCSRFToken ensures a CSRF token cookie is set for the response.
// Returns the token value (new or existing).
func ensureCSRFToken(w http.ResponseWriter, r *http.Request) string {
	// If cookie already exists, return its value (no need to set new cookie)
	if cookie, err := r.Cookie(csrfCookieName); err == nil {
		return cookie.Value
	}

	// Generate new token
	token := GenerateToken()

	// Always set cookie if we have a ResponseWriter; if nil, skip setting
	// but still return the token so the caller can embed it in HTML.
	if w != nil {
		http.SetCookie(w, &http.Cookie{
			Name:     csrfCookieName,
			Value:    token,
			Path:     "/",
			HttpOnly: true, // Keep it out of JavaScript reach to mitigate XSS + CSRF token theft
			Secure:   requestIsSecure(r),
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(sessionTTL.Seconds()), // Match session lifetime
		})
	}

	return token
}

// csrfTokenFunc is a template function that returns a valid CSRF token.
// It ensures the token cookie is set.
func csrfTokenFunc(h *Handler) func(http.ResponseWriter, *http.Request) string {
	return func(w http.ResponseWriter, r *http.Request) string {
		return ensureCSRFToken(w, r)
	}
}

// csrfFieldFunc is a template function that returns a hidden input field with the CSRF token.
func csrfFieldFunc(h *Handler) func() template.HTML {
	return func() template.HTML {
		// We need a request to generate the token, but template funcs don't have direct access.
		// Instead, we'll rely on the token being set via middleware or commonData.
		// This will be populated in commonData instead.
		return template.HTML(`<input type="hidden" name="csrf_token" value="PLACEHOLDER">`)
	}
}
