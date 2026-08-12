package handlers

import (
	"log"
	"net/http"

	"farmstore/internal/database"
	"farmstore/internal/payment"
)

// UserOrders renders the authenticated user's order history page.
func (h *Handler) UserOrders(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireAuth(w, r)
	if !ok {
		return
	}

	summaries, err := database.GetUserOrdersWithItems(h.db, userID)
	if err != nil {
		log.Printf("get user orders: %v", err)
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

	order, err := database.GetOrder(h.db, orderID)
	if err != nil {
		log.Printf("resume payment: get order: %v", err)
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
		log.Printf("resume payment: convert amount: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	callbackURL := h.baseURL + "/checkout/verify"
	authority, err := h.zarinpal.RequestPayment(gatewayAmount, callbackURL, "سفارش تودج "+orderID)
	if err != nil {
		log.Printf("resume payment: zarinpal request: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := database.SetPaymentAuthority(h.db, orderID, authority); err != nil {
		log.Printf("resume payment: set authority: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, h.zarinpal.GatewayURL(authority), http.StatusSeeOther)
}
