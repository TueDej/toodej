package handlers

import (
	"net/http"

	"farmstore/internal/database"
	"farmstore/internal/logutil"
	"farmstore/internal/payment"
)

// UserOrders renders the authenticated user's order history page.
func (h *Handler) UserOrders(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireAuth(w, r)
	if !ok {
		return
	}

	summaries, err := database.GetUserOrdersWithItems(r.Context(), h.db, userID)
	if err != nil {
		logutil.Error("get user orders", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := h.mergeData(r, map[string]any{
		"Orders": summaries,
	}, w)
	h.render(w, "orders", data)
}

// ResumePayment lets an order owner retry payment for an order still in the
// awaiting_payment state: it requests a fresh authority from the gateway and
// redirects them to the cashier. Requests for an order owned by someone else or
// in any other state are rejected (IDOR guard), so order IDs cannot be probed.
//
// It is registered as POST, not GET: it mutates the order (a new gateway
// authority is stored) and so must sit behind the CSRF and same-origin
// middleware, which only guard mutating methods. The orders page submits it as
// a plain form so the browser follows the 303 to the gateway itself — an htmx
// request would try to swap the cross-origin response instead of navigating.
func (h *Handler) ResumePayment(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireAuth(w, r)
	if !ok {
		return
	}

	orderID := r.PathValue("id")
	if !validOrderID(orderID) {
		http.NotFound(w, r)
		return
	}

	order, err := database.GetOrder(r.Context(), h.db, orderID)
	if err != nil {
		logutil.Error("resume payment: get order", "err", err)
		http.NotFound(w, r)
		return
	}
	if order.UserID != userID {
		http.NotFound(w, r)
		return
	}
	if order.Status != "awaiting_payment" {
		http.Redirect(w, r, "/orders", http.StatusSeeOther)
		return
	}

	gatewayAmount, err := payment.TomanToRial(order.TotalAmount)
	if err != nil {
		logutil.Error("resume payment: convert amount", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	callbackURL := h.baseURL + "/checkout/verify"
	authority, err := h.zarinpal.RequestPayment(gatewayAmount, callbackURL, "سفارش تودج "+orderID)
	if err != nil {
		logutil.Error("resume payment: zarinpal request", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := database.SetPaymentAuthority(r.Context(), h.db, orderID, authority); err != nil {
		logutil.Error("resume payment: set authority", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, h.zarinpal.GatewayURL(authority), http.StatusSeeOther)
}
