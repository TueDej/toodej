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
	// pendingReturnTTL bounds how long a saved post-login destination lives.
	// The previous code reused sessionTTL (7 days) here, letting bot traffic
	// mint week-long redirect state; 30 minutes is plenty for a real login.
	pendingReturnTTL = 30 * time.Minute
)

// Session cookie naming. On secure requests the cookie uses the __Host-
// prefix, which browsers enforce as Secure + Path=/ + no Domain attribute —
// a sibling subdomain can no longer plant a shadowing session cookie. Plain
// HTTP development keeps the legacy name (the prefix requires a secure
// context and would break the local flow).
const (
	legacySessionCookieName = "session"
	secureSessionCookieName = "__Host-session"
)

// sessionCookieNameFor returns the session cookie name for the given security
// level.
func sessionCookieNameFor(secure bool) string {
	if secure {
		return secureSessionCookieName
	}
	return legacySessionCookieName
}

// sessionCookie returns the request's session cookie for its security level,
// or an error when absent.
func sessionCookie(r *http.Request) (*http.Cookie, error) {
	return r.Cookie(sessionCookieNameFor(requestIsSecure(r)))
}

// sessionCookieAny returns the request's session cookie checking BOTH names
// (secure __Host- and legacy). A secure/plain flip between login and a later
// request must not orphan server state: the entry keyed by the other variant
// would otherwise linger until the janitor while the user appears logged out.
func sessionCookieAny(r *http.Request) (*http.Cookie, error) {
	if c, err := r.Cookie(secureSessionCookieName); err == nil && validSessionID(c.Value) {
		return c, nil
	}
	if c, err := r.Cookie(legacySessionCookieName); err == nil && validSessionID(c.Value) {
		return c, nil
	}
	return nil, http.ErrNoCookie
}

// sessionSIDs returns every session id presented by the request across both
// cookie variants, so logout/teardown can drop server state for all of them.
func sessionSIDs(r *http.Request) []string {
	var out []string
	seen := map[string]bool{}
	for _, name := range []string{secureSessionCookieName, legacySessionCookieName} {
		if c, err := r.Cookie(name); err == nil && validSessionID(c.Value) && !seen[c.Value] {
			seen[c.Value] = true
			out = append(out, c.Value)
		}
	}
	return out
}

// sessionCookie builds (with the given name helper context) the session
// Set-Cookie value.
func newSessionCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	secure := requestIsSecure(r)
	return &http.Cookie{
		Name:     sessionCookieNameFor(secure),
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

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

// reconcilePayments verifies every awaiting_payment order against the gateway
// in rial — the active authority first, then any superseded authority kept in
// the payment_authorities history (a resume-payment replaces the active token
// while the customer may still complete payment on the old one). Orders whose
// payment actually succeeded are moved to pending via ConfirmPayment; orders
// that were never paid are left untouched so the unpaid-order janitor
// reclaims their stock after the TTL.
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

		authorities := []string{o.Authority}
		if history, err := database.GetAuthoritiesForOrder(context.Background(), h.db, o.ID); err == nil {
			for _, a := range history {
				if a != o.Authority {
					authorities = append(authorities, a)
				}
			}
		} else {
			logutil.Error("payment reconciliation: list authorities", "order_id", o.ID, "err", err)
		}

		for _, auth := range authorities {
			result, err := h.zarinpal.VerifyPayment(amount, auth)
			if err != nil {
				// No authoritative answer from the gateway: stop verifying this
				// order this tick and let the next sweep retry.
				logutil.Error("payment reconciliation: verify order", "order_id", o.ID, "err", err)
				break
			}
			if !result.OK {
				continue
			}
			transitioned, err := database.ConfirmPayment(context.Background(), h.db, o.ID, result.RefID)
			if err != nil {
				logutil.Error("payment reconciliation: confirm order", "order_id", o.ID, "err", err)
				break
			}
			logutil.Info("payment reconciliation: order confirmed paid", "order_id", o.ID, "ref_id", result.RefID)
			if transitioned {
				h.notifyOrderConfirmedAsync(o.ID, o.TotalAmount)
			}
			break
		}
	}
	h.detectOrphanedPayments()
}

// orphanScanWindow bounds how far back the orphaned-payment scan looks for
// cancelled orders. It comfortably exceeds unpaidOrderTTL, so every order the
// janitor cancelled (or a failed-verify path cancelled) is re-checked with the
// gateway long enough for a late success to surface.
const orphanScanWindow = 30 * time.Minute

// detectOrphanedPayments re-verifies recently cancelled orders that carry an
// authority and no recorded ref. If the gateway reports the charge as
// successful, the money was taken for an order that no longer exists —
// un-cancelling is unsafe (its stock was already handed back), so the ref is
// recorded on the order and the discrepancy is logged at error level for a
// manual refund.
func (h *Handler) detectOrphanedPayments() {
	orders, err := database.GetRecentlyCancelledPaymentOrders(context.Background(), h.db, orphanScanWindow)
	if err != nil {
		logutil.Error("orphaned payment scan: list cancelled orders", "err", err)
		return
	}
	for _, o := range orders {
		amount, err := payment.TomanToRial(o.TotalAmount)
		if err != nil {
			continue
		}
		result, err := h.zarinpal.VerifyPayment(amount, o.Authority)
		if err != nil || !result.OK {
			continue
		}
		if err := database.MarkOrphanedPayment(context.Background(), h.db, o.ID, result.RefID); err != nil {
			logutil.Error("orphaned payment scan: record ref", "order_id", o.ID, "err", err)
			continue
		}
		logutil.Error("ORPHANED PAYMENT: gateway reports a cancelled order was paid — manual refund required",
			"order_id", o.ID, "authority", o.Authority, "ref_id", result.RefID, "amount", o.TotalAmount)
	}
}

// purgeExpiredSessions removes every entry whose expiry has passed, plus any
// carts idle for longer than the session cookie's lifetime.
func (h *Handler) purgeExpiredSessions(now time.Time) {
	h.sessionMu.Lock()
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
	for sid, expiresAt := range h.adminSessions {
		if now.After(expiresAt) {
			delete(h.adminSessions, sid)
		}
	}
	h.sessionMu.Unlock()

	// Evict carts that have gone untouched for longer than the session cookie's
	// lifetime. Without this, the cart map grows without bound: every visitor
	// without a cookie is minted a fresh session (and cart) on first request.
	// cartStore is nil in some tests that construct a bare Handler.
	if h.cartStore != nil {
		h.cartStore.PurgeIdle(sessionTTL)
	}
}

// getUserID returns the authenticated user ID for the current request, or 0 if
// the user is not logged in. It reads the session cookie and looks up the
// in-memory session map.
func (h *Handler) getUserID(r *http.Request) int64 {
	cookie, err := sessionCookieAny(r)
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
	if cookie, err := sessionCookieAny(r); err == nil && validSessionID(cookie.Value) {
		// Ensure CSRF token is set for existing session
		_ = ensureCSRFToken(w, r)
		return cookie.Value
	}
	sid, err := generateSessionID()
	if err != nil {
		logutil.Error("generate session id", "err", err)
		// Fail closed: return a value that fails validSessionID so the maps
		// treat the request as unauthenticated and no two callers share it
		// as a real session.
		return "entropy-error"
	}
	http.SetCookie(w, newSessionCookie(r, sid, int(sessionTTL.Seconds())))
	// Ensure CSRF token is set for new session
	_ = ensureCSRFToken(w, r)
	return sid
}

// generateSessionID creates a cryptographically random 32-hex-character session
// identifier using crypto/rand. Entropy failure is returned instead of
// panicking: panicking inside a handler goroutine turns a transient kernel
// entropy hiccup into a 500/crash, while an error lets the caller fail closed.
func generateSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// regenerateSessionID issues a new session cookie and returns the new ID.
// Any existing session data (authenticated user, pending login, pending redirect)
// is migrated to the new ID so the login state is preserved. Called on login to
// prevent session fixation: the pre-auth session ID is discarded and cannot be
// reused by an attacker who planted it.
func (h *Handler) regenerateSessionID(w http.ResponseWriter, r *http.Request) string {
	oldSid := ""
	if cookie, err := sessionCookieAny(r); err == nil && validSessionID(cookie.Value) {
		oldSid = cookie.Value
	}

	newSid, err := generateSessionID()
	if err != nil {
		logutil.Error("regenerate session id", "err", err)
		// Keep the old session rather than logging the user out on an
		// entropy hiccup; fixation protection is best-effort here.
		if oldSid != "" {
			return oldSid
		}
		return "entropy-error"
	}
	http.SetCookie(w, newSessionCookie(r, newSid, int(sessionTTL.Seconds())))
	// Expire the non-current variant so a flip-flopped client does not keep
	// sending the stale cookie alongside the fresh one.
	for _, name := range []string{legacySessionCookieName, secureSessionCookieName} {
		if name == sessionCookieNameFor(requestIsSecure(r)) {
			continue
		}
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true, Secure: requestIsSecure(r), SameSite: http.SameSiteLaxMode, MaxAge: -1})
	}
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
		if h.cartStore != nil {
			h.cartStore.MigrateSession(oldSid, newSid)
		}
	}

	return newSid
}
