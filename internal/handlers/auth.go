package handlers

import (
	"crypto/rand"
	"fmt"
	"html"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"farmstore/internal/database"
	"farmstore/internal/logutil"
	"farmstore/internal/services"
)

// LoginPage renders the OTP login form. If the user is already logged in they
// are redirected to their saved destination (if any) or the home page. The
// "next" query parameter, set by the central auth guard, is validated and stored
// against the session so the destination survives the OTP exchange.
func (h *Handler) LoginPage(w http.ResponseWriter, r *http.Request) {
	userID := h.getUserID(r)
	if userID != 0 {
		if next := h.takeReturnURL(w, r); next != "" {
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		if next := sanitizeReturnURL(r.URL.Query().Get("next")); next != "" {
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	if next := r.URL.Query().Get("next"); next != "" {
		sid := h.getOrCreateSessionID(w, r)
		h.sessionMu.Lock()
		if h.pendingNext == nil {
			h.pendingNext = make(map[string]pendingReturn)
		}
		h.pendingNext[sid] = pendingReturn{url: sanitizeReturnURL(next), expiresAt: time.Now().Add(sessionTTL)}
		h.sessionMu.Unlock()
	}

	data := h.mergeData(r, map[string]any{}, w)
	h.render(w, "login", data)
}

// SendOTP handles the first step of OTP authentication. It creates or retrieves
// the user, generates a random 5-digit code, stores it in the database with a
// 2-minute expiry, and sends it via Kavenegar Verify.Lookup (or stdout in DEV_MODE).
//
// The response replaces the login form with a code-input form via HTMX outerHTML swap.
func (h *Handler) SendOTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	phone := strings.TrimSpace(r.FormValue("phone"))
	if !validIranianPhone(phone) {
		w.Header().Set("Content-Type", "text/html")
		// Retarget into the #login-error slot so the form stays on screen.
		w.Header().Set("HX-Retarget", "#login-error")
		w.Header().Set("HX-Reswap", "innerHTML")
		w.Write([]byte(`<div class="flex items-start gap-3 rounded-[1.05rem_1rem_1rem_1.1rem] border border-rose/40 bg-[#FAE6E1] px-4 py-3 text-sm leading-7 text-pomegranate" role="alert">
      <svg class="mt-1 h-4 w-4 flex-shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
        <path stroke-linecap="round" stroke-linejoin="round" d="M12 9v3.75m9-.75a9 9 0 11-18 0 9 9 0 0118 0zm-9 3.75h.008v.008H12v-.008z"/>
      </svg>
      <span>شماره تماس معتبر وارد کنید؛ شماره باید با ۰۹ شروع شده و ۱۱ رقم باشد (مثلاً ۰۹۱۲۳۴۵۶۷۸۹).</span>
    </div>`))
		return
	}

	// Per-phone rate limit on top of the per-IP limit applied by the router.
	if !h.otpLimiter.Allow("phone:" + phone) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("HX-Retarget", "#login-error")
		w.Header().Set("HX-Reswap", "innerHTML")
		w.Write([]byte(`<div class="flex items-start gap-3 rounded-[1.05rem_1rem_1rem_1.1rem] border border-[#C98A2C]/45 bg-[#F6E9CD] px-4 py-3 text-sm leading-7 text-[#7A5A2E]" role="alert">
      <svg class="mt-1 h-4 w-4 flex-shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
        <path stroke-linecap="round" stroke-linejoin="round" d="M12 9v3.75m9-.75a9 9 0 11-18 0 9 9 0 0118 0zm-9 3.75h.008v.008H12v-.008z"/>
      </svg>
      <span>درخواست‌های زیادی ارسال کرده‌اید؛ لطفاً کمی بعد دوباره تلاش کنید.</span>
    </div>`))
		return
	}

	_, err := database.GetOrCreateUser(r.Context(), h.db, phone)
	if err != nil {
		logutil.Error("get or create user", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	code := generateOTP5()

	if err := database.CreateOTP(r.Context(), h.db, phone, code, time.Now().Add(otpTTL)); err != nil {
		logutil.Error("create otp", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := services.SendOTP(phone, code); err != nil {
		logutil.Error("send otp", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Bind the phone number to this session for the verification step.
	sid := h.getOrCreateSessionID(w, r)
	h.sessionMu.Lock()
	if h.pendingLogins == nil {
		h.pendingLogins = make(map[string]pendingLogin)
	}
	h.pendingLogins[sid] = pendingLogin{phone: phone, expiresAt: time.Now().Add(otpTTL)}
	h.sessionMu.Unlock()

	// The dev-only code box and pre-filled value must never be rendered in
	// production: doing so would leak the OTP to anyone with access to the
	// client side.
	devBox := ""
	if os.Getenv("DEV_MODE") == "true" {
		devBox = fmt.Sprintf(`<div class="rounded-lg bg-sand px-3 py-2 text-center text-xs text-clay" dir="ltr">Dev: %s</div>
		<div id="otp-dev-code" data-code="%s" hidden></div>`, code, code)
	}

	// phone is validated to ^09\d{9}$, and is additionally HTML-escaped before
	// reflection into the fragment to defeat any injection attempt.
	escPhone := html.EscapeString(phone)

	// Get CSRF token for the verification form
	csrfToken := ensureCSRFToken(w, r)

	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<form id="login-form" class="space-y-4" method="post" action="/auth/verify-otp"
	hx-post="/auth/verify-otp" hx-target="#login-form" hx-swap="outerHTML">
	<input type="hidden" name="csrf_token" value="%s">
	<p class="text-center text-sm leading-7 text-clay">کد تایید به شماره %s ارسال شد.</p>%s
	<input type="hidden" name="phone" value="%s">
	<label class="lbl block text-center">کد تایید</label>
	<input type="hidden" name="code" id="otp-hidden">
	<div class="flex justify-center gap-2 mt-2" dir="ltr">
		<input type="text" name="c0" maxlength="1" inputmode="numeric" pattern="[0-9]" class="otp-box" data-idx="0" autofocus>
		<input type="text" name="c1" maxlength="1" inputmode="numeric" pattern="[0-9]" class="otp-box" data-idx="1">
		<input type="text" name="c2" maxlength="1" inputmode="numeric" pattern="[0-9]" class="otp-box" data-idx="2">
		<input type="text" name="c3" maxlength="1" inputmode="numeric" pattern="[0-9]" class="otp-box" data-idx="3">
		<input type="text" name="c4" maxlength="1" inputmode="numeric" pattern="[0-9]" class="otp-box" data-idx="4">
	</div>
	<style>.otp-box{width:2.75rem;height:3.25rem;text-align:center;font-size:1.5rem;font-weight:600;border:1.5px solid var(--color-sand,#d1c4b0);border-radius:.625rem;background:var(--color-parchment,#faf8f5);outline:none;transition:border-color .15s,box-shadow .15s}.otp-box:focus{border-color:#8b6914;box-shadow:0 0 0 2px rgba(139,105,20,.15)}</style>
	<button type="submit" class="btn btn-primary w-full">
		تایید کد
	</button>
	<div class="flex items-center justify-center gap-2 pt-1">
		<button type="button" id="resend-btn" class="text-sm text-fig underline-offset-4 hover:underline disabled:opacity-40 disabled:cursor-not-allowed" disabled
			hx-post="/auth/send-otp" hx-vals='{"phone":"%s","csrf_token":"%s"}' hx-target="#login-form" hx-swap="outerHTML">
			ارسال دوباره کد
		</button>
		<span id="resend-timer" class="text-xs text-clay"></span>
	</div>
	<p class="text-center text-xs text-clay">کد ۵ رقمی را وارد کنید.</p>
	</form>
	<p id="login-desc" hx-swap-oob="true"></p>`, csrfToken, escPhone, devBox, escPhone, escPhone, csrfToken)
}

// VerifyOTP validates the OTP code against the database. On success it creates
// a server-side session mapping the session cookie to the user ID, then redirects
// to the home page via the HX-Redirect header (HTMX client-side redirect).
// The session ID is regenerated on login to prevent session fixation.
func (h *Handler) VerifyOTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	code := strings.TrimSpace(r.FormValue("code"))
	if code == "" {
		code = r.FormValue("c0") + r.FormValue("c1") + r.FormValue("c2") + r.FormValue("c3") + r.FormValue("c4")
	}

	oldSid := h.getOrCreateSessionID(w, r)
	h.sessionMu.RLock()
	pl, ok := h.pendingLogins[oldSid]
	h.sessionMu.RUnlock()

	if !ok || pl.phone == "" || code == "" {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<div class="space-y-3"><p class="text-sm leading-7 text-pomegranate text-center">کد نامعتبر است.</p><a href="/login" class="block text-center text-sm font-semibold text-fig underline-offset-4 hover:underline">دریافت دوباره کد</a></div>`)
		return
	}

	if time.Now().After(pl.expiresAt) {
		h.sessionMu.Lock()
		delete(h.pendingLogins, oldSid)
		h.sessionMu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<div class="space-y-3"><p class="text-sm leading-7 text-pomegranate text-center">کد منقضی شده است.</p><a href="/login" class="block text-center text-sm font-semibold text-fig underline-offset-4 hover:underline">دریافت دوباره کد</a></div>`)
		return
	}

	// Per-phone attempt limit on top of the per-IP limit applied by the router.
	if !h.otpVerifyLimiter.Allow("phone:" + pl.phone) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<div class="space-y-3"><p class="text-sm leading-7 text-pomegranate text-center">تعداد تلاش‌ها زیاد است؛ کمی بعد دوباره تلاش کنید.</p><a href="/login" class="block text-center text-sm font-semibold text-fig underline-offset-4 hover:underline">دریافت دوباره کد</a></div>`)
		return
	}

	valid, err := database.VerifyOTP(r.Context(), h.db, pl.phone, code)
	if err != nil {
		logutil.Error("verify otp", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if !valid {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<div class="space-y-3"><p class="text-sm leading-7 text-pomegranate text-center">کد اشتباه است یا منقضی شده.</p><a href="/login" class="block text-center text-sm font-semibold text-fig underline-offset-4 hover:underline">دریافت دوباره کد</a></div>`)
		return
	}

	user, err := database.GetUserByPhone(r.Context(), h.db, pl.phone)
	if err != nil {
		logutil.Error("get user", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Regenerate session ID to prevent session fixation: the pre-auth ID
	// (which an attacker may have planted) is discarded.
	newSid := h.regenerateSessionID(w, r)

	h.sessionMu.Lock()
	if h.userSessions == nil {
		h.userSessions = make(map[string]session)
	}
	h.userSessions[newSid] = session{userID: user.ID, expiresAt: time.Now().Add(sessionTTL)}
	delete(h.pendingLogins, oldSid)
	next := ""
	if pr, ok := h.pendingNext[oldSid]; ok {
		next = pr.url
	}
	delete(h.pendingNext, oldSid)
	h.sessionMu.Unlock()

	dest := sanitizeReturnURL(next)
	if dest == "" {
		dest = "/"
	}

	w.Header().Set("HX-Redirect", dest)
	w.WriteHeader(http.StatusOK)
}

// Logout removes the session mapping, clears both session and CSRF cookies,
// and redirects to the home page.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	sid := h.getOrCreateSessionID(w, r)
	h.sessionMu.Lock()
	delete(h.userSessions, sid)
	h.sessionMu.Unlock()

	// Expire the session cookie in the browser.
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	// Expire the CSRF token cookie as well.
	http.SetCookie(w, &http.Cookie{
		Name:     "csrf_token",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// generateOTP5 returns a cryptographically random 5-digit zero-padded string.
// If crypto/rand fails it falls back to "12345" rather than crashing.
func generateOTP5() string {
	n, err := rand.Int(rand.Reader, big.NewInt(100000))
	if err != nil {
		return "12345"
	}
	return fmt.Sprintf("%05d", n.Int64())
}
