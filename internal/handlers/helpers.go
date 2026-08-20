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

// StockProblem describes a cart line that can no longer be ordered as-is.
// It is surfaced to the user (and rejected at order placement) rather than
// silently mutating the cart behind their back.
//
//   - Removed items: the product is missing, inactive, or fully out of stock.
//     Available is 0, and the item is dropped from the cart entirely.
//   - OverStock items: the product is in stock but the cart quantity exceeds the
//     remaining stock. Available holds the stock that is actually left, and the
//     item is kept in the cart so the user can reduce the quantity themselves.
type StockProblem struct {
	ProductID int64
	Name      string
	Quantity  int // requested quantity currently in the cart
	Available int // remaining stock (0 for items that became unavailable)
}

// refreshCartFromProducts refreshes cart items with the latest product display
// data (name, price, unit, image) and returns two classes of stock problems:
//
//   - removed: items that are no longer purchasable (missing, inactive, or out of
//     stock) — these are dropped from the cart.
//   - overStock: items still in stock but whose cart quantity exceeds the
//     remaining stock — these are kept so the user can reduce the quantity.
//
// Quantities are intentionally NOT capped: an over-stock quantity is surfaced to
// the user and rejected at order placement (ErrInsufficientStock) so the cart is
// never silently mutated. Callers must warn the user and must not place an order
// while either slice is non-empty.
func (h *Handler) refreshCartFromProducts(ctx context.Context, cart *Cart) (removed, overStock []StockProblem) {
	items := cart.Snapshot()
	refreshed := make([]CartItem, 0, len(items))
	for _, item := range items {
		product, err := database.GetProduct(ctx, h.db, item.ProductID)
		if err != nil || !product.IsActive || product.StockQuantity <= 0 {
			removed = append(removed, StockProblem{
				ProductID: item.ProductID,
				Name:      item.Name,
				Quantity:  item.Quantity,
				Available: 0,
			})
			continue
		}
		item.Name = product.Name
		item.Price = product.Price
		item.Unit = product.Unit
		item.ImageURL = product.ImageURL
		refreshed = append(refreshed, item)
		// Disabled cart lines (cap bypassed) show up here: the requested
		// quantity is greater than what is actually left in stock.
		if item.Quantity > product.StockQuantity {
			overStock = append(overStock, StockProblem{
				ProductID: item.ProductID,
				Name:      product.Name,
				Quantity:  item.Quantity,
				Available: product.StockQuantity,
			})
		}
	}
	cart.ReplaceItems(refreshed)
	return removed, overStock
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
