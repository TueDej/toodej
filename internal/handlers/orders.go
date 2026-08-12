package handlers

import (
	"log"
	"net/http"

	"farmstore/internal/database"
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
