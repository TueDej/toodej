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
	h.refreshCartFromProducts(r.Context(), cart)
	if cart.Count() == 0 {
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
		"Total":      cart.Total(),
		"Phone":      phone,
		"Step":       1,
		"Name":       r.URL.Query().Get("name"),
		"Address":    r.URL.Query().Get("address"),
		"PostalCode": r.URL.Query().Get("postal_code"),
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
			"Error": "اطلاعات تماس، آدرس و کد پستی را به‌درستی وارد کنید.",
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
	h.refreshCartFromProducts(r.Context(), cart)
	if cart.Count() == 0 {
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}

	items := cart.Snapshot()

	data := h.mergeData(r, map[string]any{
		"Step":       2,
		"Total":      cart.Total(),
		"Items":      items,
		"Name":       name,
		"Phone":      phone,
		"Address":    address,
		"PostalCode": postalCode,
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
			"Error": "اطلاعات تماس، آدرس و کد پستی را به‌درستی وارد کنید.",
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
	h.refreshCartFromProducts(r.Context(), cart)

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

	cart.Clear()
	w.Header().Set("HX-Trigger", "cartUpdated")
	http.Redirect(w, r, gatewayURL, http.StatusSeeOther)
}

// VerifyPayment handles the Zarinpal callback after the user completes (or cancels)
// the payment. It verifies the transaction and updates the order status accordingly.
func (h *Handler) VerifyPayment(w http.ResponseWriter, r *http.Request) {
	authority := r.URL.Query().Get("Authority")
	status := r.URL.Query().Get("Status")

	if authority == "" || status != "OK" {
		// Payment was cancelled or failed — cancel the order and restore stock.
		if authority != "" {
		if order, err := database.GetOrderByAuthority(r.Context(), h.db, authority); err == nil {
			database.MarkPaymentFailed(r.Context(), h.db, order.ID)
			}
		}
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}

	order, err := database.GetOrderByAuthority(r.Context(), h.db, authority)
	if err != nil {
		logutil.Error("verify: order not found for authority", "err", err)
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
		logutil.Error("zarinpal verify", "err", err)
		database.MarkPaymentFailed(r.Context(), h.db, order.ID)
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}

	if result.OK {
		if err := database.ConfirmPayment(r.Context(), h.db, order.ID, result.RefID); err != nil {
			logutil.Error("confirm payment", "err", err)
		}
		http.Redirect(w, r, fmt.Sprintf("/checkout/confirmation/%s", order.ID), http.StatusSeeOther)
		return
	} else {
		logutil.Warn("payment not verified", "order_id", order.ID, "message", result.Message)
		database.MarkPaymentFailed(r.Context(), h.db, order.ID)
		http.Redirect(w, r, "/cart?error=payment_failed", http.StatusSeeOther)
		return
	}
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

	var itemViews []itemView
	for i, item := range items {
		name := fmt.Sprintf("Product #%d", item.ProductID)
		unit := ""
		if i < len(products) {
			name = products[i].Name
			unit = products[i].Unit
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
