package handlers

import (
	"fmt"
	"net/http"
	"strconv"

	"farmstore/internal/database"
)

// AddToCart adds a product to the cart (or increments its quantity by 1). It
// triggers a cartUpdated event on the client so the cart badge updates.
func (h *Handler) AddToCart(w http.ResponseWriter, r *http.Request) {
	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)

	productIDStr := r.FormValue("product_id")
	productID, err := strconv.ParseInt(productIDStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid product", http.StatusBadRequest)
		return
	}

	product, err := database.GetProduct(r.Context(), h.db, productID)
	if err != nil || !product.IsActive {
		http.Error(w, "product not found", http.StatusNotFound)
		return
	}

	added := cart.AddItemLimited(CartItem{
		ProductID: product.ID,
		Name:      product.Name,
		Price:     product.Price,
		Unit:      product.Unit,
		Quantity:  1,
		ImageURL:  product.ImageURL,
	}, product.StockQuantity)
	if !added {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("HX-Trigger", `{"cartUpdated":"", "stockError":""}`)
		fmt.Fprint(w, toPersianDigits(strconv.Itoa(cart.Count())))
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("HX-Trigger", `{"cartUpdated":"", "cartEvent":"added"}`)
	fmt.Fprint(w, cart.Count())
}

// UpdateCart adjusts the quantity of a cart item by a positive or negative delta.
// If the resulting quantity is zero or negative the item is removed.
func (h *Handler) UpdateCart(w http.ResponseWriter, r *http.Request) {
	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)

	productIDStr := r.FormValue("product_id")
	deltaStr := r.FormValue("delta")
	if productIDStr == "" {
		http.Error(w, "missing product_id", http.StatusBadRequest)
		return
	}
	productID, err := strconv.ParseInt(productIDStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid product_id", http.StatusBadRequest)
		return
	}
	delta := 1
	if deltaStr != "" {
		d, err := strconv.Atoi(deltaStr)
		if err != nil || (d != 1 && d != -1) {
			http.Error(w, "invalid delta", http.StatusBadRequest)
			return
		}
		delta = d
	}

	if delta > 0 {
		product, err := database.GetProduct(r.Context(), h.db, productID)
		if err != nil || !product.IsActive {
			http.Error(w, "product not found", http.StatusNotFound)
			return
		}
		if !cart.UpdateQuantityLimited(productID, delta, product.StockQuantity) {
			data := h.mergeData(r, map[string]any{
				"Items": cart.Snapshot(),
				"Total": cart.Total(),
			}, w)
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("HX-Trigger", `{"cartUpdated":"", "stockError":""}`)
			h.renderTemplate(w, "cart", "cart-content", data)
			return
		}
	} else {
		cart.UpdateQuantity(productID, delta)
	}

	event := "added"
	if delta < 0 {
		event = "removed"
	}
	h.renderCartContent(w, r, sid, event)
}

// RemoveFromCart removes a product line from the cart entirely.
func (h *Handler) RemoveFromCart(w http.ResponseWriter, r *http.Request) {
	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)

	productIDStr := r.FormValue("product_id")
	if productIDStr == "" {
		http.Error(w, "missing product_id", http.StatusBadRequest)
		return
	}
	productID, err := strconv.ParseInt(productIDStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid product_id", http.StatusBadRequest)
		return
	}

	cart.RemoveItem(productID)

	h.renderCartContent(w, r, sid, "removed")
}

// ViewCart renders the full cart page.
func (h *Handler) ViewCart(w http.ResponseWriter, r *http.Request) {
	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)

	data := h.mergeData(r, map[string]any{
		"Items": cart.Snapshot(),
		"Total": cart.Total(),
	}, w)
	h.render(w, "cart", data)
}
