package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"time"

	"farmstore/internal/database"
	"farmstore/internal/payment"
)

// sessionTTL is how long an authenticated session lives (server-side), matching
// the session cookie's 7-day MaxAge. Pending-login (OTP) bindings and saved
// post-login destinations expire on a shorter timer so stale entries cannot
// accumulate in the in-memory maps indefinitely.
const (
	sessionTTL = 7 * 24 * time.Hour
	otpTTL     = 2 * time.Minute
)

// session is an authenticated session entry with its server-side expiry.
type session struct {
	userID    int64
	expiresAt time.Time
}

// pendingLogin binds a phone number to a session during the OTP exchange.
type pendingLogin struct {
	phone     string
	expiresAt time.Time
}

// pendingReturn is a sanitized post-login destination saved against a session.
type pendingReturn struct {
	url       string
	expiresAt time.Time
}

// startSessionJanitor launches a background goroutine that periodically purges
// expired session, pending-login, and pending-return entries so the in-memory
// maps cannot grow without bound.
func (h *Handler) startSessionJanitor() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			h.purgeExpiredSessions(time.Now())
		}
	}()
}

// unpaidOrderTTL is how long an order may remain in 'awaiting_payment' before
// the janitor cancels it and restores its reserved stock.
const unpaidOrderTTL = 15 * time.Minute

// startUnpaidOrderJanitor launches a background goroutine that periodically
// cancels orders stuck in 'awaiting_payment' for longer than unpaidOrderTTL and
// restores their reserved stock. This prevents inventory leaks when a customer
// abandons the Zarinpal payment gateway without returning to the callback URL.
func (h *Handler) startUnpaidOrderJanitor() {
	go func() {
		// Sweep once on startup so a server restart immediately reclaims
		// stock from orders abandoned before the process came back up.
		h.cancelExpiredUnpaidOrders()

		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			h.cancelExpiredUnpaidOrders()
		}
	}()
}

func (h *Handler) cancelExpiredUnpaidOrders() {
	n, err := database.CancelExpiredUnpaidOrders(h.db, unpaidOrderTTL)
	if err != nil {
		log.Printf("cancel expired unpaid orders: %v", err)
		return
	}
	if n > 0 {
		log.Printf("cancelled %d expired awaiting_payment orders (older than %s)", n, unpaidOrderTTL)
	}
}

// paymentReconcileInterval is how often the payment reconciliation job sweeps
// orders stuck in awaiting_payment to ask the gateway whether they were actually
// paid.
const paymentReconcileInterval = time.Minute

// startPaymentReconciler launches a background goroutine that periodically asks
// Zarinpal whether orders stuck in awaiting_payment were actually completed.
// This is the payment reconciliation job: a successful payment whose callback
// was lost (e.g. the browser closed right after paying) otherwise stays in
// awaiting_payment until the unpaid-order janitor cancels it. Reconciliation
// rescues such orders by confirming them, so stock is never needlessly restored
// and the customer is not shown a cancelled order they already paid for.
func (h *Handler) startPaymentReconciler() {
	go func() {
		h.reconcilePayments()

		ticker := time.NewTicker(paymentReconcileInterval)
		defer ticker.Stop()
		for range ticker.C {
			h.reconcilePayments()
		}
	}()
}

// reconcilePayments verifies every awaiting_payment order with a stored
// authority against the gateway in rial. Orders whose payment actually succeeded
// are moved to pending via ConfirmPayment; orders that were never paid are left
// untouched so the unpaid-order janitor reclaims their stock after the TTL.
func (h *Handler) reconcilePayments() {
	orders, err := database.GetAwaitingPaymentOrders(h.db)
	if err != nil {
		log.Printf("payment reconciliation: list orders: %v", err)
		return
	}
	for _, o := range orders {
		amount, err := payment.TomanToRial(o.TotalAmount)
		if err != nil {
			log.Printf("payment reconciliation: convert amount for %s: %v", o.ID, err)
			continue
		}
		result, err := h.zarinpal.VerifyPayment(amount, o.Authority)
		if err != nil {
			log.Printf("payment reconciliation: verify order %s: %v", o.ID, err)
			continue
		}
		if !result.OK {
			continue
		}
		if err := database.ConfirmPayment(h.db, o.ID, result.RefID); err != nil {
			log.Printf("payment reconciliation: confirm order %s: %v", o.ID, err)
			continue
		}
		log.Printf("payment reconciliation: order %s confirmed paid (ref %d)", o.ID, result.RefID)
	}
}

// purgeExpiredSessions removes every entry whose expiry has passed.
func (h *Handler) purgeExpiredSessions(now time.Time) {
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	for sid, s := range h.userSessions {
		if now.After(s.expiresAt) {
			delete(h.userSessions, sid)
		}
	}
	for sid, pl := range h.pendingLogins {
		if now.After(pl.expiresAt) {
			delete(h.pendingLogins, sid)
		}
	}
	for sid, pr := range h.pendingNext {
		if now.After(pr.expiresAt) {
			delete(h.pendingNext, sid)
		}
	}
}

// getUserID returns the authenticated user ID for the current request, or 0 if
// the user is not logged in. It reads the session cookie and looks up the
// in-memory session map.
func (h *Handler) getUserID(r *http.Request) int64 {
	cookie, err := r.Cookie("session")
	if err != nil || !validSessionID(cookie.Value) {
		return 0
	}
	h.sessionMu.RLock()
	defer h.sessionMu.RUnlock()
	s, ok := h.userSessions[cookie.Value]
	if !ok || time.Now().After(s.expiresAt) {
		return 0
	}
	return s.userID
}

// getOrCreateSessionID returns the existing session cookie value for this request,
// or creates a new session, sets the cookie, and returns the new ID.
func (h *Handler) getOrCreateSessionID(w http.ResponseWriter, r *http.Request) string {
	cookie, err := r.Cookie("session")
	if err == nil && validSessionID(cookie.Value) {
		// Ensure CSRF token is set for existing session
		_ = ensureCSRFToken(w, r)
		return cookie.Value
	}
	sid := generateSessionID()
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	// Ensure CSRF token is set for new session
	_ = ensureCSRFToken(w, r)
	return sid
}

// generateSessionID creates a cryptographically random 32-hex-character session
// identifier using crypto/rand.
func generateSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
