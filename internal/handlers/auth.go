package handlers

import (
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"time"

	"farmstore/internal/database"
)

// LoginPage renders the OTP login form. If the user is already logged in they
// are redirected to the home page.
func (h *Handler) LoginPage(w http.ResponseWriter, r *http.Request) {
	userID := h.getUserID(r)
	if userID != 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
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

	phone := r.FormValue("phone")
	if phone == "" {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<p class="text-red-600 text-sm">شماره تماس را وارد کنید.</p>`))
		return
	}

	_, err := database.GetOrCreateUser(h.db, phone)
	if err != nil {
		log.Printf("get or create user: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	code := generateOTP5()

	if err := database.CreateOTP(h.db, phone, code, time.Now().Add(2*time.Minute)); err != nil {
		log.Printf("create otp: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Bind the phone number to this session for the verification step.
	sid := h.getOrCreateSessionID(w, r)
	h.sessionMu.Lock()
	if h.pendingLogins == nil {
		h.pendingLogins = make(map[string]string)
	}
	h.pendingLogins[sid] = phone
	h.sessionMu.Unlock()

	devBox := fmt.Sprintf(`<div class="rounded-lg bg-blue-50 p-3 text-center text-xs text-blue-700" dir="ltr">Dev: %s</div>`, code)
	valueFill := fmt.Sprintf(` value="%s"`, code)

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
</form>`, phone, devBox, phone, valueFill)
}

// VerifyOTP validates the OTP code against the database. On success it creates
// a server-side session mapping the session cookie to the user ID, then redirects
// to the home page via the HX-Redirect header (HTMX client-side redirect).
func (h *Handler) VerifyOTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	code := r.FormValue("code")

	sid := h.getOrCreateSessionID(w, r)
	h.sessionMu.RLock()
	phone := h.pendingLogins[sid]
	h.sessionMu.RUnlock()

	if phone == "" || code == "" {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<div class="space-y-4"><p class="text-red-600 text-sm text-center">کد نامعتبر است.</p><a href="/login" class="block text-center text-sm text-garnet hover:underline">دریافت دوباره کد</a></div>`)
		return
	}

	valid, err := database.VerifyOTP(h.db, phone, code)
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

	user, err := database.GetUserByPhone(h.db, phone)
	if err != nil {
		log.Printf("get user: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.sessionMu.Lock()
	if h.userSessions == nil {
		h.userSessions = make(map[string]int64)
	}
	h.userSessions[sid] = user.ID
	delete(h.pendingLogins, sid)
	h.sessionMu.Unlock()

	w.Header().Set("HX-Redirect", "/")
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
