package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"farmstore/internal/database"
)

// commonData returns template data that is shared across all pages — currently
// just the "LoggedIn" boolean used to show/hide login/logout/orders links.
// It also includes the CSRF token for forms and meta tags.
func (h *Handler) commonData(r *http.Request, w http.ResponseWriter) map[string]any {
	sid, err := r.Cookie("session")
	loggedIn := false
	cartCount := 0
	if err == nil && validSessionID(sid.Value) {
		h.sessionMu.RLock()
		if s, ok := h.userSessions[sid.Value]; ok && time.Now().Before(s.expiresAt) {
			loggedIn = true
		}
		h.sessionMu.RUnlock()
		cartCount = h.cartStore.Get(sid.Value).Count()
	}
	// Ensure CSRF token is set and include it in template data
	csrfToken := ensureCSRFToken(w, r)
	return map[string]any{
		"LoggedIn":  loggedIn,
		"CartCount": cartCount,
		"CSRFToken": csrfToken,
	}
}

// mergeData merges common template data into the page-specific data map.
// Page-specific keys take precedence over common keys.
func (h *Handler) mergeData(r *http.Request, data map[string]any, w http.ResponseWriter) map[string]any {
	if data == nil {
		data = make(map[string]any)
	}
	for k, v := range h.commonData(r, w) {
		if _, exists := data[k]; !exists {
			data[k] = v
		}
	}
	return data
}

// renderCenteredError is a utility for rendering error pages (unused currently
// but kept for future error-page rendering).
func (h *Handler) renderCenteredError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	data := map[string]any{
		"title":   http.StatusText(status),
		"message": msg,
	}
	h.renderTemplate(w, "index", "content", data)
}

// formatToman formats an integer price (in the smallest currency unit) as a
// human-readable Persian price string with thousand separators, Persian digits,
// and the "تومان" suffix.
func formatToman(cents int) string {
	return toPersianDigits(commaInt(cents)) + " تومان"
}

// toPersianDigits converts Western digits (0-9) in a string to their Persian
// Unicode equivalents (۰-۹). Non-digit runes pass through unchanged.
func toPersianDigits(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r - '0' + 0x06F0)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// refreshCartFromProducts refreshes cart items with latest product data from DB
// and adjusts quantities to not exceed available stock.
func (h *Handler) refreshCartFromProducts(ctx context.Context, cart *Cart) {
	items := cart.Snapshot()
	refreshed := make([]CartItem, 0, len(items))
	for _, item := range items {
		product, err := database.GetProduct(ctx, h.db, item.ProductID)
		if err != nil || !product.IsActive || product.StockQuantity <= 0 {
			continue
		}
		if item.Quantity > product.StockQuantity {
			item.Quantity = product.StockQuantity
		}
		if item.Quantity <= 0 {
			continue
		}
		item.Name = product.Name
		item.Price = product.Price
		item.Unit = product.Unit
		item.ImageURL = product.ImageURL
		refreshed = append(refreshed, item)
	}
	cart.ReplaceItems(refreshed)
}

// renderCartContent renders the "cart-content" template partial and fires a
// cart event so the badge and toast are updated on the client.
func (h *Handler) renderCartContent(w http.ResponseWriter, r *http.Request, sid, event string) {
	cart := h.cartStore.Get(sid)

	data := h.mergeData(r, map[string]any{
		"Items": cart.Snapshot(),
		"Total": cart.Total(),
	}, w)
	w.Header().Set("HX-Trigger", `{"cartUpdated":"", "cartEvent":"`+event+`"}`)
	h.renderTemplate(w, "cart", "cart-content", data)
}

// CartCount returns the total number of units in the cart as plain text (used
// by the cart badge in the navbar via HTMX).
func (h *Handler) CartCount(w http.ResponseWriter, r *http.Request) {
	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, toPersianDigits(strconv.Itoa(cart.Count())))
}
