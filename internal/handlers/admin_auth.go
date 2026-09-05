package handlers

import (
	"net/http"
	"time"

	"farmstore/internal/logutil"
)

// Admin panel session configuration. The admin panel is protected by a
// cookie-based login (see AdminLoginPage / AdminLoginPOST / RequireAdmin)
// instead of HTTP Basic Auth. The session cookie is scoped to /admin so it is
// never sent with storefront requests, and it is stored in a separate map from
// customer sessions so an admin login can never be confused with a customer
// login (and vice versa).
const (
	adminCookieName   = "admin_session"
	adminSessionTTL   = 12 * time.Hour
	adminLoginPath    = "/admin/login"
	adminDashboardURL = "/admin/"
)

// SetAdminCredentials stores the admin username/password (from ADMIN_USER /
// ADMIN_PASS) on the handler for the login form to check against. It also
// initialises the admin session map and the per-IP login-attempt limiter.
func (h *Handler) SetAdminCredentials(username, password string) {
	h.adminUser = username
	h.adminPass = password
	if h.adminSessions == nil {
		h.adminSessions = make(map[string]time.Time)
	}
	if h.adminLoginLimiter == nil {
		h.adminLoginLimiter = NewRateLimiter(5, time.Minute)
	}
}

// isAdminAuthed reports whether the request carries a valid, unexpired admin
// session cookie.
func (h *Handler) isAdminAuthed(r *http.Request) bool {
	cookie, err := r.Cookie(adminCookieName)
	if err != nil || !validSessionID(cookie.Value) {
		return false
	}
	h.sessionMu.RLock()
	defer h.sessionMu.RUnlock()
	expiresAt, ok := h.adminSessions[cookie.Value]
	return ok && time.Now().Before(expiresAt)
}

// RequireAdmin guards the admin panel routes. Authenticated requests pass
// through; unauthenticated browser navigations are redirected to the login
// page, and unauthenticated HTMX requests get a bare 401 so the panel's
// in-place swaps surface an error instead of embedding the login form.
func (h *Handler) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.isAdminAuthed(r) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("HX-Request") == "true" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, adminLoginPath, http.StatusSeeOther)
	})
}

// AdminLoginPage renders the admin login form. Already-authenticated admins
// are sent straight to the dashboard.
func (h *Handler) AdminLoginPage(w http.ResponseWriter, r *http.Request) {
	if h.isAdminAuthed(r) {
		http.Redirect(w, r, adminDashboardURL, http.StatusSeeOther)
		return
	}
	data := h.mergeData(r, map[string]any{}, w)
	h.render(w, "admin-login", data)
}

// AdminLoginPOST validates the submitted credentials against ADMIN_USER /
// ADMIN_PASS in constant time and, on success, mints a fresh admin session ID
// (never derived from any pre-login cookie, so session fixation is avoided)
// and sets the /admin-scoped session cookie. Failures re-render the form with
// a generic error; the per-IP limiter caps brute-force attempts.
func (h *Handler) AdminLoginPOST(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if h.adminLoginLimiter != nil && !h.adminLoginLimiter.Allow("admin:"+clientIP(r)) {
		h.renderAdminLoginError(w, r, "تلاش‌های زیاد؛ لطفاً چند دقیقه بعد دوباره امتحان کنید.")
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")
	credsOK := h.adminUser != "" && h.adminPass != "" &&
		secureEqual(username, h.adminUser) && secureEqual(password, h.adminPass)

	if !credsOK {
		if h.adminUser != "" {
			logutil.Warn("failed admin login attempt", "ip", clientIP(r), "username", username)
		}
		h.renderAdminLoginError(w, r, "نام کاربری یا رمز عبور نادرست است.")
		return
	}

	sid := generateSessionID()
	h.sessionMu.Lock()
	if h.adminSessions == nil {
		h.adminSessions = make(map[string]time.Time)
	}
	h.adminSessions[sid] = time.Now().Add(adminSessionTTL)
	h.sessionMu.Unlock()

	// Rotate the CSRF token on admin login so a pre-authentication token (and
	// any value planted beside it) never carries into the authenticated panel.
	rotateCSRFToken(w, r)

	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    sid,
		Path:     "/admin",
		HttpOnly: true,
		Secure:   requestIsSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(adminSessionTTL.Seconds()),
	})
	http.Redirect(w, r, adminDashboardURL, http.StatusSeeOther)
}

// renderAdminLoginError re-renders the login form with an error message.
func (h *Handler) renderAdminLoginError(w http.ResponseWriter, r *http.Request, msg string) {
	w.WriteHeader(http.StatusUnauthorized)
	data := h.mergeData(r, map[string]any{"Error": msg}, w)
	h.render(w, "admin-login", data)
}

// AdminLogout destroys the current admin session and clears its cookie.
func (h *Handler) AdminLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(adminCookieName); err == nil && validSessionID(cookie.Value) {
		h.sessionMu.Lock()
		delete(h.adminSessions, cookie.Value)
		h.sessionMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/admin",
		HttpOnly: true,
		Secure:   requestIsSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	http.Redirect(w, r, adminLoginPath, http.StatusSeeOther)
}
