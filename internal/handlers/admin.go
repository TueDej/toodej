package handlers

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"farmstore/internal/database"
	"farmstore/internal/models"
)

// ── Dashboard ─────────────────────────────────────────

// AdminDashboard renders the admin panel showing all orders and all products
// (including inactive ones) for management.
func (h *Handler) AdminDashboard(w http.ResponseWriter, r *http.Request) {
	orders, err := database.GetOrders(h.db)
	if err != nil {
		log.Printf("admin orders: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	products, err := database.GetAllProducts(h.db)
	if err != nil {
		log.Printf("admin products: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := map[string]any{
		"Orders":   orders,
		"Products": products,
	}
	if err := h.templates["admin"].Execute(w, data); err != nil {
		log.Printf("render admin: %v", err)
	}
}

// ── Order Management ──────────────────────────────────

// AdminUpdateOrderStatus updates the status of an order via an HTMX POST from
// the admin panel's inline <select>. It returns the new <td> with the updated
// <select> so the page does not need a full reload.
func (h *Handler) AdminUpdateOrderStatus(w http.ResponseWriter, r *http.Request) {
	orderID := r.PathValue("id")
	if orderID == "" {
		http.Error(w, "invalid order id", http.StatusBadRequest)
		return
	}

	status := r.FormValue("status")
	valid := map[string]bool{"pending": true, "processing": true, "completed": true, "cancelled": true}
	if !valid[status] {
		http.Error(w, "invalid status", http.StatusBadRequest)
		return
	}

	if err := database.UpdateOrderStatus(h.db, orderID, status); err != nil {
		log.Printf("update order status: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	tmpl := fmt.Sprintf(`<td id="order-%s-status" class="px-4 py-3">
    <select name="status" class="rounded-md border border-gray-300 bg-white px-2 py-1 text-xs font-medium shadow-sm focus:border-garnet focus:ring-1 focus:ring-garnet %%s"
      hx-post="/admin/orders/%s/status" hx-trigger="change" hx-target="#order-%s-status" hx-swap="outerHTML">
      <option value="pending" style="color:black" %%s>در انتظار بررسی</option>
      <option value="processing" style="color:black" %%s>در حال پردازش</option>
      <option value="completed" style="color:black" %%s>تکمیل شده</option>
      <option value="cancelled" style="color:black" %%s>لغو شده</option>
    </select>
  </td>`, orderID, orderID, orderID)

	statusColors := map[string]string{
		"pending":    "text-yellow-600",
		"processing": "text-blue-600",
		"completed":  "text-green-600",
		"cancelled":  "text-red-600",
	}

	color := statusColors[status]
	sel := map[string]string{
		"pending":    "",
		"processing": "",
		"completed":  "",
		"cancelled":  "",
	}
	sel[status] = `selected="selected"`

	tmpl = fmt.Sprintf(tmpl, color, sel["pending"], sel["processing"], sel["completed"], sel["cancelled"])
	fmt.Fprint(w, tmpl)
}

// ── Product Management ────────────────────────────────

// AdminToggleProduct toggles the active/inactive state of a product and re-renders
// its table row via HTMX.
func (h *Handler) AdminToggleProduct(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	productID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid product id", http.StatusBadRequest)
		return
	}

	product, err := database.GetProduct(h.db, productID)
	if err != nil {
		http.Error(w, "product not found", http.StatusNotFound)
		return
	}

	product.IsActive = !product.IsActive
	if err := database.UpdateProduct(h.db, product); err != nil {
		log.Printf("toggle product: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.renderProductRow(w, *product)
}

// AdminUpdateProduct updates the price and/or stock quantity of a product via
// HTMX inline editing and re-renders its table row.
func (h *Handler) AdminUpdateProduct(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	productID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid product id", http.StatusBadRequest)
		return
	}

	product, err := database.GetProduct(h.db, productID)
	if err != nil {
		http.Error(w, "product not found", http.StatusNotFound)
		return
	}

	if priceStr := r.FormValue("price"); priceStr != "" {
		priceStr = strings.ReplaceAll(priceStr, ",", "")
		price, err := strconv.Atoi(priceStr)
		if err == nil && price >= 0 {
			product.Price = price
		}
	}
	if stockStr := r.FormValue("stock_quantity"); stockStr != "" {
		stockStr = strings.ReplaceAll(stockStr, ",", "")
		stock, err := strconv.Atoi(stockStr)
		if err == nil && stock >= 0 {
			product.StockQuantity = stock
		}
	}

	if err := database.UpdateProduct(h.db, product); err != nil {
		log.Printf("update product: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.renderProductRow(w, *product)
}

// AdminCreateProduct creates a new product from the admin form and prepends its
// row to the products table via HTMX.
func (h *Handler) AdminCreateProduct(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	category := strings.TrimSpace(r.FormValue("category"))
	priceStr := strings.TrimSpace(r.FormValue("price"))
	stockStr := strings.TrimSpace(r.FormValue("stock_quantity"))
	unit := strings.TrimSpace(r.FormValue("unit"))
	description := strings.TrimSpace(r.FormValue("description"))

	if name == "" || category == "" || priceStr == "" {
		http.Error(w, "name, category, and price are required", http.StatusBadRequest)
		return
	}

	priceStr = strings.ReplaceAll(priceStr, ",", "")
	price, err := strconv.Atoi(priceStr)
	if err != nil || price < 0 {
		http.Error(w, "invalid price", http.StatusBadRequest)
		return
	}

	stock := 0
	if stockStr != "" {
		stockStr = strings.ReplaceAll(stockStr, ",", "")
		stock, _ = strconv.Atoi(stockStr)
	}

	slug := strings.ToLower(strings.ReplaceAll(name, " ", "-"))

	product := &models.Product{
		Name:          name,
		Slug:          slug,
		Category:      category,
		Description:   description,
		Price:         price,
		StockQuantity: stock,
		Unit:          unit,
		IsActive:      true,
	}

	id, err := database.CreateProduct(h.db, product)
	if err != nil {
		log.Printf("create product: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	product.ID = id
	h.renderProductRow(w, *product)
}

// renderProductRow returns the HTML for a single <tr> in the admin products table.
// This is used as the HTMX response for all product CRUD operations.
func (h *Handler) renderProductRow(w http.ResponseWriter, p models.Product) {
	w.Header().Set("Content-Type", "text/html")
	inactiveClass := ""
	if !p.IsActive {
		inactiveClass = "opacity-50"
	}

	row := fmt.Sprintf(`<tr id="product-%d" class="border-b border-gray-100 %s hover:bg-gray-50 transition">
    <td class="px-4 py-3 text-sm text-gray-500 font-mono tracking-wider">%d</td>
    <td class="px-4 py-3 text-sm font-medium text-gray-900">%s</td>
    <td class="px-4 py-3 text-sm text-gray-600">%s</td>
    <td class="px-4 py-3">
      <input type="text" inputmode="numeric" name="price" value="%s"
        class="w-40 rounded-md border border-gray-300 px-2 py-1 text-sm shadow-sm focus:border-garnet focus:ring-1 focus:ring-garnet thousand-sep"
        hx-post="/admin/products/%d" hx-trigger="change" hx-target="#product-%d" hx-swap="outerHTML">
    </td>
    <td class="px-4 py-3">
      <input type="number" name="stock_quantity" value="%d" min="0"
        class="w-20 rounded-md border border-gray-300 px-2 py-1 text-sm shadow-sm focus:border-garnet focus:ring-1 focus:ring-garnet"
        hx-post="/admin/products/%d" hx-trigger="change" hx-target="#product-%d" hx-swap="outerHTML">
    </td>
    <td class="px-4 py-3">
      <button dir="ltr" hx-post="/admin/products/%d/toggle" hx-target="#product-%d" hx-swap="outerHTML"
        class="relative inline-flex h-6 w-11 flex-shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-200 ease-in-out focus:outline-none %s">
        <span class="pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow ring-0 transition duration-200 ease-in-out %s"></span>
      </button>
    </td>
  </tr>`,
		p.ID, inactiveClass,
		p.ID, htmlEscape(p.Name), htmlEscape(p.Category),
		commaInt(p.Price), p.ID, p.ID,
		p.StockQuantity, p.ID, p.ID,
		p.ID, p.ID,
		toggleBg(p.IsActive), toggleTranslate(p.IsActive))

	fmt.Fprint(w, row)
}

// toggleBg returns the background colour class for the toggle switch based on active state.
func toggleBg(active bool) string {
	if active {
		return "bg-garnet"
	}
	return "bg-gray-300"
}

// toggleTranslate returns the translate-x class for the toggle switch knob.
func toggleTranslate(active bool) string {
	if active {
		return "translate-x-5"
	}
	return "translate-x-0"
}

// commaInt formats an integer with comma thousand separators (e.g., 129900 → "129,900").
func commaInt(v int) string {
	s := strconv.Itoa(v)
	n := len(s)
	var parts []string
	for i := n; i > 0; i -= 3 {
		start := i - 3
		if start < 0 {
			start = 0
		}
		parts = append([]string{s[start:i]}, parts...)
	}
	return strings.Join(parts, ",")
}

// htmlEscape performs standard HTML entity escaping to prevent XSS in admin templates.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&#39;")
	return s
}
