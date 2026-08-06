package handlers

import (
	"crypto/rand"
	"fmt"
	"html"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"farmstore/internal/database"
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

	data := h.mergeData(r, map[string]any{})
	if err := h.templates["login"].Execute(w, data); err != nil {
		log.Printf("render login: %v", err)
	}
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
		w.Write([]byte(`<div class="flex items-start gap-2 rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700" role="alert">
      <svg class="mt-0.5 h-4 w-4 flex-shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
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
		w.Write([]byte(`<div class="flex items-start gap-2 rounded-lg border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-700" role="alert">
      <svg class="mt-0.5 h-4 w-4 flex-shrink-0" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
        <path stroke-linecap="round" stroke-linejoin="round" d="M12 9v3.75m9-.75a9 9 0 11-18 0 9 9 0 0118 0zm-9 3.75h.008v.008H12v-.008z"/>
      </svg>
      <span>درخواستهای زیادی ارسال کردهاید؛ لطفاً کمی بعد دوباره تلاش کنید.</span>
    </div>`))
		return
	}

	_, err := database.GetOrCreateUser(h.db, phone)
	if err != nil {
		log.Printf("get or create user: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	code := generateOTP5()

	if err := database.CreateOTP(h.db, phone, code, time.Now().Add(otpTTL)); err != nil {
		log.Printf("create otp: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := services.SendOTP(phone, code); err != nil {
		log.Printf("send otp: %v", err)
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
	valueFill := ""
	if os.Getenv("DEV_MODE") == "true" {
		devBox = fmt.Sprintf(`<div class="rounded-lg bg-blue-50 p-3 text-center text-xs text-blue-700" dir="ltr">Dev: %s</div>`, code)
		valueFill = fmt.Sprintf(` value="%s"`, code)
	}

	// phone is validated to ^09\d{9}$, and is additionally HTML-escaped before
	// reflection into the fragment to defeat any injection attempt.
	escPhone := html.EscapeString(phone)

	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<form id="login-form" class="space-y-4" method="post" action="/auth/verify-otp"
	hx-post="/auth/verify-otp" hx-target="#login-form" hx-swap="outerHTML">
	<p class="text-sm text-gray-600 text-center">کد تایید به شماره %s ارسال شد.</p>%s
	<input type="hidden" name="phone" value="%s">
	<div>
		<label class="block text-sm font-medium text-gray-700">کد تایید</label>
		<input type="text" name="code" maxlength="5" inputmode="numeric" pattern="[0-9]{5}" required autocomplete="one-time-code"%s
			class="mt-1 block w-full rounded-lg border border-gray-300 bg-white px-4 py-2.5 text-center text-2xl tracking-widest text-gray-900 shadow-sm focus:border-garnet focus:ring-1 focus:ring-garnet">
	</div>
	<button type="submit"
		class="w-full rounded-lg bg-garnet px-6 py-3 font-semibold text-white shadow-sm hover:bg-garnet/90 transition">
		تایید کد
	</button>
	<p class="text-xs text-gray-400 text-center">کد ۵ رقمی را وارد کنید.</p>
</form>`, escPhone, devBox, escPhone, valueFill)
}

// VerifyOTP validates the OTP code against the database. On success it creates
// a server-side session mapping the session cookie to the user ID, then redirects
// to the home page via the HX-Redirect header (HTMX client-side redirect).
func (h *Handler) VerifyOTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	code := strings.TrimSpace(r.FormValue("code"))

	sid := h.getOrCreateSessionID(w, r)
	h.sessionMu.RLock()
	pl, ok := h.pendingLogins[sid]
	h.sessionMu.RUnlock()

	if !ok || pl.phone == "" || code == "" {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<div class="space-y-4"><p class="text-red-600 text-sm text-center">کد نامعتبر است.</p><a href="/login" class="block text-center text-sm text-garnet hover:underline">دریافت دوباره کد</a></div>`)
		return
	}

	if time.Now().After(pl.expiresAt) {
		h.sessionMu.Lock()
		delete(h.pendingLogins, sid)
		h.sessionMu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<div class="space-y-4"><p class="text-red-600 text-sm text-center">کد منقضی شده است.</p><a href="/login" class="block text-center text-sm text-garnet hover:underline">دریافت دوباره کد</a></div>`)
		return
	}

	// Per-phone attempt limit on top of the per-IP limit applied by the router.
	if !h.otpVerifyLimiter.Allow("phone:" + pl.phone) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<div class="space-y-4"><p class="text-red-600 text-sm text-center">تعداد تلاشها زیاد است؛ کمی بعد دوباره تلاش کنید.</p></div>`)
		return
	}

	valid, err := database.VerifyOTP(h.db, pl.phone, code)
	if err != nil {
		log.Printf("verify otp: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if !valid {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<div class="space-y-4"><p class="text-red-600 text-sm text-center">کد اشتباه است یا منقضی شده.</p><a href="/login" class="block text-center text-sm text-garnet hover:underline">دریافت دوباره کد</a></div>`)
		return
	}

	user, err := database.GetUserByPhone(h.db, pl.phone)
	if err != nil {
		log.Printf("get user: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.sessionMu.Lock()
	if h.userSessions == nil {
		h.userSessions = make(map[string]session)
	}
	h.userSessions[sid] = session{userID: user.ID, expiresAt: time.Now().Add(sessionTTL)}
	delete(h.pendingLogins, sid)
	next := ""
	if pr, ok := h.pendingNext[sid]; ok {
		next = pr.url
	}
	delete(h.pendingNext, sid)
	h.sessionMu.Unlock()

	dest := sanitizeReturnURL(next)
	if dest == "" {
		dest = "/"
	}

	w.Header().Set("HX-Redirect", dest)
	w.WriteHeader(http.StatusOK)
}

// Logout removes the session mapping and redirects to the home page.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	sid := h.getOrCreateSessionID(w, r)
	h.sessionMu.Lock()
	delete(h.userSessions, sid)
	h.sessionMu.Unlock()
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
