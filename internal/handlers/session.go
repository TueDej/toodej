package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"farmstore/internal/database"
	"farmstore/internal/logutil"
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
func (h *Handler) startSessionJanitor(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				h.purgeExpiredSessions(now)
			}
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
func (h *Handler) startUnpaidOrderJanitor(ctx context.Context) {
	go func() {
		// Sweep once on startup so a server restart immediately reclaims
		// stock from orders abandoned before the process came back up.
		h.cancelExpiredUnpaidOrders()

		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.cancelExpiredUnpaidOrders()
			}
		}
	}()
}

func (h *Handler) cancelExpiredUnpaidOrders() {
	// Reconcile BEFORE reclaiming stock: a customer who is still in the Zarinpal
	// gateway — or whose payment callback was delayed — may have already paid.
	// If we cancelled their awaiting_payment order here we would return its
	// reserved stock, letting another shopper grab it while the original
	// customer's payment succeeds but their order is marked cancelled. Asking the
	// gateway first keeps paid orders (and their stock) intact; only genuinely
	// unpaid orders are then cancelled by CancelExpiredUnpaidOrders below.
	h.reconcilePayments()

	n, err := database.CancelExpiredUnpaidOrders(context.Background(), h.db, unpaidOrderTTL)
	if err != nil {
		logutil.Error("cancel expired unpaid orders", "err", err)
		return
	}
	if n > 0 {
		logutil.Info("cancelled expired awaiting_payment orders", "count", n, "ttl", unpaidOrderTTL)
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
func (h *Handler) startPaymentReconciler(ctx context.Context) {
	go func() {
		h.reconcilePayments()

		ticker := time.NewTicker(paymentReconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.reconcilePayments()
			}
		}
	}()
}

// reconcilePayments verifies every awaiting_payment order with a stored
// authority against the gateway in rial. Orders whose payment actually succeeded
// are moved to pending via ConfirmPayment; orders that were never paid are left
// untouched so the unpaid-order janitor reclaims their stock after the TTL.
func (h *Handler) reconcilePayments() {
	orders, err := database.GetAwaitingPaymentOrders(context.Background(), h.db)
	if err != nil {
		logutil.Error("payment reconciliation: list orders", "err", err)
		return
	}
	for _, o := range orders {
		amount, err := payment.TomanToRial(o.TotalAmount)
		if err != nil {
			logutil.Error("payment reconciliation: convert amount", "order_id", o.ID, "err", err)
			continue
		}
		result, err := h.zarinpal.VerifyPayment(amount, o.Authority)
		if err != nil {
			logutil.Error("payment reconciliation: verify order", "order_id", o.ID, "err", err)
			continue
		}
		if !result.OK {
			continue
		}
		if err := database.ConfirmPayment(context.Background(), h.db, o.ID, result.RefID); err != nil {
			logutil.Error("payment reconciliation: confirm order", "order_id", o.ID, "err", err)
			continue
		}
		logutil.Info("payment reconciliation: order confirmed paid", "order_id", o.ID, "ref_id", result.RefID)
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

// regenerateSessionID issues a new session cookie and returns the new ID.
// Any existing session data (authenticated user, pending login, pending redirect)
// is migrated to the new ID so the login state is preserved. Called on login to
// prevent session fixation: the pre-auth session ID is discarded and cannot be
// reused by an attacker who planted it.
func (h *Handler) regenerateSessionID(w http.ResponseWriter, r *http.Request) string {
	cookie, err := r.Cookie("session")
	oldSid := ""
	if err == nil && validSessionID(cookie.Value) {
		oldSid = cookie.Value
	}

	newSid := generateSessionID()
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    newSid,
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	_ = ensureCSRFToken(w, r)

	// Migrate existing session data to the new ID so login state is preserved.
	if oldSid != "" && oldSid != newSid {
		h.sessionMu.Lock()
		if s, ok := h.userSessions[oldSid]; ok {
			h.userSessions[newSid] = s
			delete(h.userSessions, oldSid)
		}
		if pl, ok := h.pendingLogins[oldSid]; ok {
			h.pendingLogins[newSid] = pl
			delete(h.pendingLogins, oldSid)
		}
		if pr, ok := h.pendingNext[oldSid]; ok {
			h.pendingNext[newSid] = pr
			delete(h.pendingNext, oldSid)
		}
		h.sessionMu.Unlock()
		h.cartStore.MigrateSession(oldSid, newSid)
	}

	return newSid
}
