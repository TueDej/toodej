package handlers

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// requireAuth is the central authentication guard for session-authenticated
// routes. It returns the authenticated user ID for the current request and true.
// If the user is not logged in it preserves the requested URL (path, query, and
// fragment) as the post-login destination and redirects to the login page,
// returning false.
func (h *Handler) requireAuth(w http.ResponseWriter, r *http.Request) (int64, bool) {
	userID := h.getUserID(r)
	if userID != 0 {
		return userID, true
	}
	h.redirectToLogin(w, r)
	return 0, false
}

// redirectToLogin preserves the current request URL as the user's intended
// destination and redirects them to the login page. The destination is passed to
// the login page as the "next" query parameter and stored server-side against
// the session by LoginPage so it survives the OTP exchange.
func (h *Handler) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	returnURL := sanitizeReturnURL(r.URL.RequestURI())
	loginURL := "/login"
	if returnURL != "" && returnURL != "/" {
		loginURL += "?next=" + url.QueryEscape(returnURL)
	}
	http.Redirect(w, r, loginURL, http.StatusSeeOther)
}

// takeReturnURL returns and clears the post-login destination recorded for the
// current session, or "" if none was set.
func (h *Handler) takeReturnURL(w http.ResponseWriter, r *http.Request) string {
	sid := h.getOrCreateSessionID(w, r)
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	pr, ok := h.pendingNext[sid]
	delete(h.pendingNext, sid)
	if !ok || time.Now().After(pr.expiresAt) {
		return ""
	}
	return pr.url
}

// sanitizeReturnURL validates a post-login redirect target and returns a safe
// internal destination. Only relative URLs that begin with a single leading
// slash are accepted, which prevents open redirects: absolute URLs, protocol-
// relative URLs ("//host"), schemes such as "javascript:", and backslash tricks
// are all reduced to "/". An empty input yields "" so callers can fall back to
// the default homepage.
func sanitizeReturnURL(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}

	u, err := url.Parse(target)
	if err != nil {
		return "/"
	}
	if u.IsAbs() || u.Host != "" || u.User != nil {
		return "/"
	}

	p := u.Path
	if !strings.HasPrefix(p, "/") ||
		strings.HasPrefix(p, "//") ||
		strings.HasPrefix(p, "/\\") ||
		strings.ContainsAny(p, "\\\x00") {
		return "/"
	}

	return target
}

// secureEqual compares two strings in constant time, preventing timing
// side-channel attacks that plain == comparisons would expose on the admin
// login credentials.
func secureEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
