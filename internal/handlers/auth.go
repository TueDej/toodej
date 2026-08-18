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
		loginAlert(w, "error", `شماره تماس معتبر وارد کنید؛ شماره باید با ۰۹ شروع شده و ۱۱ رقم باشد (مثلاً ۰۹۱۲۳۴۵۶۷۸۹).`)
		return
	}

	// A number serving a login cooldown cannot shake it off by asking for a
	// fresh code, otherwise the wrong-code budget would mean nothing.
	if lock := h.otpLockRemaining(phone, clientIP(r)); lock > 0 {
		lockoutAlert(w, lock)
		return
	}

	// Per-phone rate limit on top of the per-IP limit applied by the router.
	if !h.otpLimiter.Allow("phone:" + phone) {
		loginAlert(w, "warn", `درخواست‌های زیادی ارسال کرده‌اید؛ لطفاً کمی بعد دوباره تلاش کنید.`)
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
	<style>.otp-box{width:2.75rem;height:3.25rem;text-align:center;font-size:1.5rem;font-weight:600;border:1.5px solid var(--color-sand,#d1c4b0);border-radius:.625rem;background:var(--color-parchment,#faf8f5);outline:none;transition:border-color .15s,box-shadow .15s}.otp-box:focus{border-color:#8b6914;box-shadow:0 0 0 2px rgba(139,105,20,.15)}.otp-box:disabled{opacity:.5;cursor:not-allowed}</style>
	<!-- ERROR SLOT — wrong/expired codes and login cooldowns are retargeted here
	     (HX-Retarget), so the digit boxes survive a failed attempt. -->
	<div id="login-error"></div>
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
//
// Every rejection is retargeted into the form's #login-error slot instead of
// replacing the form: a wrong code costs one of maxOTPFailuresPerPhone attempts,
// not the digits the user typed. Spending the budget puts the number (and, at a
// much looser threshold, the client IP) on an otpLockoutDuration cooldown.
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

	ip := clientIP(r)

	if !ok || pl.phone == "" {
		loginAlert(w, "error", `درخواست ورود شما دیگر معتبر نیست؛ کد جدیدی بگیرید.`+markerResend)
		return
	}

	// A number on cooldown is turned away before its code is even looked at, so
	// the lockout cannot be brute-forced through.
	if lock := h.otpLockRemaining(pl.phone, ip); lock > 0 {
		lockoutAlert(w, lock)
		return
	}

	// An incomplete code is a slip rather than a guess, so it costs no attempt.
	if code == "" {
		loginAlert(w, "error", `کد ۵ رقمی را کامل وارد کنید.`+markerAttempt)
		return
	}

	if time.Now().After(pl.expiresAt) {
		h.sessionMu.Lock()
		delete(h.pendingLogins, oldSid)
		h.sessionMu.Unlock()
		loginAlert(w, "error", `کد منقضی شده است؛ با «ارسال دوباره کد» کد جدیدی بگیرید.`+markerResend)
		return
	}

	// Per-phone attempt limit on top of the per-IP limit applied by the router.
	if !h.otpVerifyLimiter.Allow("phone:" + pl.phone) {
		loginAlert(w, "warn", `تعداد تلاش‌ها زیاد است؛ کمی بعد دوباره تلاش کنید.`)
		return
	}

	valid, err := database.VerifyOTP(r.Context(), h.db, pl.phone, code)
	if err != nil {
		logutil.Error("verify otp", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if !valid {
		lock, remaining := h.recordOTPFailure(pl.phone, ip)
		if lock > 0 {
			logutil.Warn("otp login on cooldown after repeated wrong codes",
				"phone_suffix", phoneSuffix(pl.phone), "ip", ip, "cooldown", otpLockoutDuration)
			lockoutAlert(w, lock)
			return
		}
		loginAlert(w, "error", fmt.Sprintf(`کد وارد شده اشتباه است؛ %s تلاش دیگر باقی مانده است.`,
			toPersianDigits(fmt.Sprint(remaining)))+markerAttempt)
		return
	}

	// The code was right, so the number starts from a full budget next time.
	h.otpAttempts.reset("phone:" + pl.phone)

	user, err := database.GetUserByPhone(r.Context(), h.db, pl.phone)
	if err != nil {
		logutil.Error("get user", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Regenerate session ID to prevent session fixation: the pre-auth ID
	// (which an attacker may have planted) is discarded. regenerateSessionID
	// migrates the pending return-URL to the new ID, so we read it back from
	// newSid below (reading oldSid would be empty after the migration).
	newSid := h.regenerateSessionID(w, r)

	h.sessionMu.Lock()
	if h.userSessions == nil {
		h.userSessions = make(map[string]session)
	}
	h.userSessions[newSid] = session{userID: user.ID, expiresAt: time.Now().Add(sessionTTL)}
	delete(h.pendingLogins, newSid)
	next := ""
	if pr, ok := h.pendingNext[newSid]; ok {
		next = pr.url
	}
	delete(h.pendingNext, newSid)
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

// ── Login feedback fragments ──────────────────────────
//
// Rejections during the OTP exchange are written into the login page's
// #login-error slot (via HX-Retarget) instead of being swapped over the form.
// Both login forms carry a slot with that id, so the phone field or the digit
// boxes stay on screen — and stay filled — whatever the server answers.

// markerAttempt tags a recoverable error: the page clears the digit boxes and
// puts the cursor back in the first one so the user can simply retype.
const markerAttempt = `<span id="otp-attempt-error" hidden></span>`

// markerResend tags an error the user can only escape by fetching a new code:
// the page clears the boxes and re-enables the resend button immediately.
const markerResend = `<span id="otp-resend-now" hidden></span>`

// loginAlert writes an alert box into the #login-error slot. tone is "error" for
// a rejected input or "warn" for a limit the user has to wait out. bodyHTML is
// trusted markup assembled here, never raw user input.
func loginAlert(w http.ResponseWriter, tone, bodyHTML string) {
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("HX-Retarget", "#login-error")
	w.Header().Set("HX-Reswap", "innerHTML")
	w.Write([]byte(alertBox(tone, bodyHTML)))
}

// alertBox renders the login page's alert markup around an HTML body.
func alertBox(tone, bodyHTML string) string {
	palette := "border-rose/40 bg-[#FAE6E1] text-pomegranate"
	if tone == "warn" {
		palette = "border-[#C98A2C]/45 bg-[#F6E9CD] text-[#7A5A2E]"
	}
	return `<div class="flex items-start gap-3 rounded-[1.05rem_1rem_1rem_1.1rem] border ` + palette + ` px-4 py-3 text-sm leading-7" role="alert">
      <svg class="mt-1 h-4 w-4 flex-shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
        <path stroke-linecap="round" stroke-linejoin="round" d="M12 9v3.75m9-.75a9 9 0 11-18 0 9 9 0 0118 0zm-9 3.75h.008v.008H12v-.008z"/>
      </svg>
      <span>` + bodyHTML + `</span>
    </div>`
}

// lockoutAlert reports the remaining login cooldown. The login page ticks the
// countdown element down and keeps the inputs disabled for as long as it is on
// screen, re-enabling them when it reaches zero.
func lockoutAlert(w http.ResponseWriter, remaining time.Duration) {
	secs := int(remaining.Round(time.Second).Seconds())
	if secs < 1 {
		secs = 1
	}
	loginAlert(w, "warn", fmt.Sprintf(
		`تلاش‌های ناموفق زیادی انجام شده است؛ ورود با این شماره تا <span id="otp-lock-timer" data-seconds="%d" dir="ltr">%s</span> دیگر بسته است.`,
		secs, toPersianDigits(fmt.Sprintf("%d:%02d", secs/60, secs%60))))
}

// phoneSuffix returns the last four digits of a phone number, so an operational
// log line can identify a lockout without recording the whole number.
func phoneSuffix(phone string) string {
	if len(phone) <= 4 {
		return phone
	}
	return phone[len(phone)-4:]
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
