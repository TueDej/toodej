package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"farmstore/internal/database"
	"farmstore/internal/logutil"
	"farmstore/internal/models"
	"farmstore/internal/payment"
)

// CheckoutForm renders the checkout page. Requires authentication and a non-empty
// cart; otherwise redirects to login or cart respectively.
func (h *Handler) CheckoutForm(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireAuth(w, r)
	if !ok {
		return
	}

	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)
	removed, overStock := h.refreshCartFromProducts(r.Context(), cart)
	// If items were dropped because they became unavailable, surface the change
	// to the user instead of silently redirecting them away from checkout.
	if cart.Count() == 0 && len(removed) == 0 && len(overStock) == 0 {
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}

	phone := ""
	rows, err := h.db.Query("SELECT phone_number FROM users WHERE id = ?", userID)
	if err == nil {
		defer rows.Close()
		if rows.Next() {
			rows.Scan(&phone)
		}
	}

	data := h.mergeData(r, map[string]any{
		"Total":          cart.Total(),
		"Phone":          phone,
		"Step":           1,
		"Name":           r.URL.Query().Get("name"),
		"Address":        r.URL.Query().Get("address"),
		"PostalCode":     r.URL.Query().Get("postal_code"),
		"RemovedItems":   removed,
		"OverStockItems": overStock,
	}, w)
	h.render(w, "checkout", data)
}

// PreviewCheckout validates the shipping form and renders step 2 (order review).
func (h *Handler) PreviewCheckout(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requireAuth(w, r)
	if !ok {
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	phone := strings.TrimSpace(r.FormValue("phone"))
	address := strings.ReplaceAll(strings.TrimSpace(r.FormValue("address")), "\n", " ")
	address = strings.ReplaceAll(address, "\r", "")
	postalCode := strings.TrimSpace(r.FormValue("postal_code"))

	if name == "" || len(name) > 80 || !validIranianPhone(phone) || len(address) < 5 || len(address) > 300 || !validPostalCode(postalCode) {
		sid := h.getOrCreateSessionID(w, r)
		cart := h.cartStore.Get(sid)
		data := h.mergeData(r, map[string]any{
			"Error": "اطلاعات تماس، آدرس و کد پستی را به‌درستی وارد کنید. دقت کنید ارقام انگلیسی باشند.",
			"Total": cart.Total(),
			"Phone": phone,
			"Step":  1,
		}, w)
		w.WriteHeader(http.StatusBadRequest)
		h.render(w, "checkout", data)
		return
	}

	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)
	removed, overStock := h.refreshCartFromProducts(r.Context(), cart)
	if cart.Count() == 0 {
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}

	items := cart.Snapshot()

	data := h.mergeData(r, map[string]any{
		"Step":           2,
		"Total":          cart.Total(),
		"Items":          items,
		"RemovedItems":   removed,
		"OverStockItems": overStock,
		"Name":           name,
		"Phone":          phone,
		"Address":        address,
		"PostalCode":     postalCode,
	}, w)
	h.render(w, "checkout", data)
}

// PlaceOrder validates the checkout form, creates an order and order items in a
// database transaction, clears the cart, and redirects to the confirmation page.
func (h *Handler) PlaceOrder(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireAuth(w, r)
	if !ok {
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	phone := strings.TrimSpace(r.FormValue("phone"))
	address := strings.ReplaceAll(strings.TrimSpace(r.FormValue("address")), "\n", " ")
	address = strings.ReplaceAll(address, "\r", "")
	postalCode := strings.TrimSpace(r.FormValue("postal_code"))

	if name == "" || len(name) > 80 || !validIranianPhone(phone) || len(address) < 5 || len(address) > 300 || !validPostalCode(postalCode) {
		sid := h.getOrCreateSessionID(w, r)
		cart := h.cartStore.Get(sid)
		data := h.mergeData(r, map[string]any{
			"Error": "اطلاعات تماس، آدرس و کد پستی را به‌درستی وارد کنید. دقت کنید ارقام انگلیسی باشند.",
			"Total": cart.Total(),
			"Phone": phone,
			"Step":  1,
		}, w)
		w.WriteHeader(http.StatusBadRequest)
		h.render(w, "checkout", data)
		return
	}

	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)
	removed, overStock := h.refreshCartFromProducts(r.Context(), cart)
	if len(removed) > 0 || len(overStock) > 0 {
		// Some cart items became unavailable, or the cart quantity exceeds the
		// remaining stock, between viewing the cart and paying. Reject the order
		// rather than silently dropping lines or overselling; the user must
		// review the cart before trying again.
		data := h.mergeData(r, map[string]any{
			"Total":          cart.Total(),
			"Phone":          phone,
			"Step":           1,
			"Name":           name,
			"Address":        address,
			"PostalCode":     postalCode,
			"RemovedItems":   removed,
			"OverStockItems": overStock,
		}, w)
		w.WriteHeader(http.StatusConflict)
		h.render(w, "checkout", data)
		return
	}

	items := cart.Snapshot()

	if len(items) == 0 {
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}

	order := &models.Order{
		CustomerName:    name,
		CustomerPhone:   phone,
		CustomerAddress: address,
		PostalCode:      postalCode,
		Status:          "awaiting_payment",
		UserID:          userID,
	}

	var orderItems []models.OrderItem
	for _, ci := range items {
		orderItems = append(orderItems, models.OrderItem{
			ProductID: ci.ProductID,
			Quantity:  ci.Quantity,
		})
	}

	orderID, err := database.CreateOrder(r.Context(), h.db, order, orderItems)
	if err != nil {
		if errors.Is(err, database.ErrInsufficientStock) || errors.Is(err, database.ErrProductUnavailable) {
			sid := h.getOrCreateSessionID(w, r)
			cart := h.cartStore.Get(sid)
			message := "موجودی برخی محصولات کافی نیست؛ لطفاً سبد را به‌روز کنید."
			if errors.Is(err, database.ErrProductUnavailable) {
				message = "برخی محصولات سبد دیگر قابل سفارش نیستند؛ لطفاً سبد را به‌روز کنید."
			}
			data := h.mergeData(r, map[string]any{
				"Error":      message,
				"Total":      cart.Total(),
				"Phone":      phone,
				"Step":       2,
				"Name":       name,
				"Address":    address,
				"PostalCode": postalCode,
				"Items":      cart.Snapshot(),
			}, w)
			w.WriteHeader(http.StatusConflict)
			h.render(w, "checkout", data)
			return
		}
		logutil.Error("create order", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	totalAmount := order.TotalAmount

	// Inform the admins about the new order submission without blocking the
	// redirect to the payment gateway.
	h.notifyAdminOrderAsync(orderID, phone)

	// Initiate Zarinpal payment.
	callbackURL := h.baseURL + "/checkout/verify"
	gatewayAmount, err := payment.TomanToRial(totalAmount)
	if err != nil {
		logutil.Error("convert payment amount", "err", err)
		database.MarkPaymentFailed(r.Context(), h.db, orderID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	authority, err := h.zarinpal.RequestPayment(gatewayAmount, callbackURL, "سفارش تودج "+orderID)
	if err != nil {
		logutil.Error("zarinpal request payment", "err", err)
		// Cancel the order and restore stock so the user can retry.
		database.MarkPaymentFailed(r.Context(), h.db, orderID)
		sid := h.getOrCreateSessionID(w, r)
		cart := h.cartStore.Get(sid)

		cart.mu.Lock()
		cartItems := make([]CartItem, len(cart.Items))
		copy(cartItems, cart.Items)
		cart.mu.Unlock()

		data := h.mergeData(r, map[string]any{
			"Error":      "خطا در اتصال به درگاه پرداخت؛ لطفاً دوباره تلاش کنید.",
			"Total":      cart.Total(),
			"Phone":      phone,
			"Step":       2,
			"Name":       name,
			"Address":    address,
			"PostalCode": postalCode,
			"Items":      cartItems,
		}, w)
		w.WriteHeader(http.StatusBadGateway)
		h.render(w, "checkout", data)
		return
	}

	if err := database.SetPaymentAuthority(r.Context(), h.db, orderID, authority); err != nil {
		logutil.Error("set payment authority", "err", err)
		database.MarkPaymentFailed(r.Context(), h.db, orderID)
		data := h.mergeData(r, map[string]any{
			"Error":      "خطا در ثبت اطلاعات پرداخت؛ لطفاً دوباره تلاش کنید.",
			"Total":      cart.Total(),
			"Phone":      phone,
			"Step":       2,
			"Name":       name,
			"Address":    address,
			"PostalCode": postalCode,
			"Items":      cart.Snapshot(),
		}, w)
		w.WriteHeader(http.StatusInternalServerError)
		h.render(w, "checkout", data)
		return
	}

	gatewayURL := h.zarinpal.GatewayURL(authority)
	logutil.Info("redirecting to payment gateway", "order_id", orderID)

	// NOTE: the cart is intentionally NOT cleared here. Clearing it before the
	// gateway redirect would lose the user's cart if the 303 redirect failed
	// (network error, closed connection) with no confirmed order. The cart is
	// cleared only after a *successful* payment verification in VerifyPayment.
	http.Redirect(w, r, gatewayURL, http.StatusSeeOther)
}

// VerifyPayment handles the Zarinpal callback after the user completes (or
// cancels) the payment. The gateway's verify answer — not the user-supplied
// Status hint — decides the outcome:
//
//   - transport error / unreadable payload: inconclusive. The customer may
//     have ALREADY paid, so the order must not be cancelled here (that would
//     restore stock and hide a paid charge). The order stays awaiting_payment
//     and the payment reconciler re-verifies it every minute.
//   - verified (code 100/101): ConfirmPayment; on transition the customer is
//     notified once and the cart is cleared.
//   - verified as unpaid: safe to cancel and restore stock — the gateway has
//     confirmed no money moved, so the user-supplied Authority alone can no
//     longer cancel a pending order via a forged <img>/link.
func (h *Handler) VerifyPayment(w http.ResponseWriter, r *http.Request) {
	authority := r.URL.Query().Get("Authority")
	status := r.URL.Query().Get("Status")

	if authority == "" {
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}

	order, err := database.GetOrderByAuthority(r.Context(), h.db, authority)
	if err != nil {
		logutil.Error("verify: order not found for authority", "err", err, "status_hint", status)
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}

	gatewayAmount, err := payment.TomanToRial(order.TotalAmount)
	if err != nil {
		logutil.Error("convert verify amount", "err", err)
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}

	result, err := h.zarinpal.VerifyPayment(gatewayAmount, authority)
	if err != nil {
		// The gateway did not give an authoritative answer (timeout, 5xx,
		// unreadable payload). The customer may have ALREADY paid, so the order
		// must not be cancelled here: MarkPaymentFailed would restore stock and
		// move the order out of awaiting_payment, leaving a paid charge with no
		// recoverable order. Keep the order in awaiting_payment — the payment
		// reconciler re-verifies it every minute until the gateway returns a
		// definitive verdict (paid → confirmed; unpaid → janitor cancels after
		// the TTL and restores stock). Send the customer to their orders page,
		// where the order shows as awaiting payment and can be retried.
		logutil.Error("zarinpal verify (gateway answer inconclusive; order left awaiting_payment)",
			"err", err, "order_id", order.ID)
		http.Redirect(w, r, "/orders", http.StatusSeeOther)
		return
	}

	if result.OK {
		transitioned, err := database.ConfirmPayment(r.Context(), h.db, order.ID, result.RefID)
		if err != nil {
			// The gateway reports the payment succeeded, but we could not attach
			// it to the order (it was cancelled, or already finalized). Showing a
			// "payment successful" confirmation here would lie to the customer —
			// their order is gone while the gateway took their money. Send them
			// back to the cart so the paid-but-unconfirmed order can be resolved,
			// and keep their cart intact rather than clearing it.
			logutil.Error("confirm payment failed despite gateway success", "err", err, "order_id", order.ID)
			http.Redirect(w, r, "/cart?error=payment_failed", http.StatusSeeOther)
			return
		}
		// Fire the customer's "order confirmed" SMS exactly once — on the call
		// that actually transitioned the order (idempotent replays skip it).
		if transitioned {
			h.notifyOrderConfirmedAsync(order.ID, order.TotalAmount)
		}
		// Cart is cleared only now that the payment is confirmed, so a failed
		// gateway redirect can never drop the user's cart without an order.
		sid := h.getOrCreateSessionID(w, r)
		h.cartStore.Get(sid).Clear()
		http.Redirect(w, r, fmt.Sprintf("/checkout/confirmation/%s", order.ID), http.StatusSeeOther)
		return
	}

	// The gateway confirms no payment happened (the user cancelled at the
	// cashier, or the transaction failed). Cancelling is now backed by the
	// gateway's own verdict instead of the user-supplied Status parameter.
	logutil.Warn("payment not verified; cancelling order", "order_id", order.ID, "message", result.Message)
	database.MarkPaymentFailed(r.Context(), h.db, order.ID)
	http.Redirect(w, r, "/cart?error=payment_failed", http.StatusSeeOther)
}

// Confirmation displays the order confirmation page after a successful checkout.
// Access is restricted to the authenticated user who owns the order (IDOR guard):
// unauthenticated visitors are redirected to login and requests for another
// user's order return 404 so order IDs cannot be probed or enumerated.
func (h *Handler) Confirmation(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireAuth(w, r)
	if !ok {
		return
	}

	orderID := r.PathValue("id")
	if !validOrderID(orderID) {
		http.NotFound(w, r)
		return
	}

	order, items, products, err := database.GetOrderWithItems(r.Context(), h.db, orderID)
	if err != nil {
		logutil.Error("get order", "err", err)
		http.NotFound(w, r)
		return
	}

	if order.UserID != userID {
		http.NotFound(w, r)
		return
	}

	type itemView struct {
		Name     string
		Quantity int
		Price    int
		Subtotal int
		Unit     string
	}

	productByID := make(map[int64]models.Product, len(products))
	for _, p := range products {
		productByID[p.ID] = p
	}

	var itemViews []itemView
	for _, item := range items {
		name := fmt.Sprintf("Product #%d", item.ProductID)
		unit := ""
		if p, ok := productByID[item.ProductID]; ok {
			name = p.Name
			unit = p.Unit
		}
		itemViews = append(itemViews, itemView{
			Name:     name,
			Quantity: item.Quantity,
			Price:    item.PricePerUnit,
			Subtotal: item.Quantity * item.PricePerUnit,
			Unit:     unit,
		})
	}

	data := h.mergeData(r, map[string]any{
		"Order": order,
		"Items": itemViews,
	}, w)
	h.render(w, "confirmation", data)
}
