package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"farmstore/internal/database"
	"farmstore/internal/logutil"
	"farmstore/internal/models"
)

// ── Dashboard ─────────────────────────────────────────

// AdminDashboard renders the admin panel showing all orders and all products
// (including inactive ones) for management.
func (h *Handler) AdminDashboard(w http.ResponseWriter, r *http.Request) {
	orders, err := database.GetOrders(r.Context(), h.db)
	if err != nil {
		logutil.Error("admin orders", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	products, err := database.GetAllProducts(r.Context(), h.db)
	if err != nil {
		logutil.Error("admin products", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	categories, err := database.GetCategories(r.Context(), h.db)
	if err != nil {
		logutil.Error("admin categories", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := h.mergeData(r, map[string]any{
		"Orders":     orders,
		"Products":   products,
		"Categories": categories,
	}, w)
	h.render(w, "admin", data)
}

// ── Order Management ──────────────────────────────────

// AdminOrderDetail renders the full detail page for a single order.
func (h *Handler) AdminOrderDetail(w http.ResponseWriter, r *http.Request) {
	orderID := r.PathValue("id")
	if !validOrderID(orderID) {
		http.Error(w, "invalid order id", http.StatusBadRequest)
		return
	}

	order, items, products, err := database.GetOrderWithItems(r.Context(), h.db, orderID)
	if err != nil {
		logutil.Error("admin order detail", "err", err)
		http.NotFound(w, r)
		return
	}

	// Build OrderItemView slice with product names and subtotals.
	// Look products up by ID rather than by position: GetProductsByIDs returns
	// products in DB id order (not item order) and may omit deleted products,
	// so a positional join would mismatch names/units or drop lines.
	productByID := make(map[int64]models.Product, len(products))
	for _, p := range products {
		productByID[p.ID] = p
	}

	var itemViews []models.OrderItemView
	for _, item := range items {
		name := ""
		unit := ""
		if p, ok := productByID[item.ProductID]; ok {
			name = p.Name
			unit = p.Unit
		}
		itemViews = append(itemViews, models.OrderItemView{
			Name:     name,
			Quantity: item.Quantity,
			Price:    item.PricePerUnit,
			Subtotal: item.PricePerUnit * item.Quantity,
			Unit:     unit,
		})
	}

	data := h.mergeData(r, map[string]any{
		"Order": order,
		"Items": itemViews,
	}, w)
	h.render(w, "order-detail", data)
}

// isKnownOrderStatus reports whether s is one of the five order statuses.
func isKnownOrderStatus(s string) bool {
	_, ok := statusLabels[s]
	return ok
}

// orderTransitionToast sets an HX-Trigger header carrying an adminToast event
// so the admin panel shows a Persian explanation of why the status change was
// rejected. HTMX skips the swap for non-2xx responses, so the header (plus the
// page's htmx:responseError fallback listener) is the only feedback channel.
func orderTransitionToast(w http.ResponseWriter, message string) {
	payload, _ := json.Marshal(map[string]string{"message": message})
	w.Header().Set("HX-Trigger", `{"adminToast":`+string(payload)+`}`)
}

// orderStatusSelectHTML renders the status <select> for an order, offering only
// the current status and the forward (and cancel) options that
// database.ValidOrderStatusOptions permits. A cancelled order is terminal: the
// select is disabled with an explanatory tooltip so it can never be moved back.
func (h *Handler) orderStatusSelectHTML(w http.ResponseWriter, r *http.Request, orderID, current string) string {
	disabled := ""
	title := ""
	if current == "cancelled" {
		disabled = "disabled"
		title = `title="سفارش لغو شده است؛ این وضعیت نهایی است و تغییر نمی‌کند."`
	}

	var opts strings.Builder
	for _, s := range database.ValidOrderStatusOptions(current) {
		sel := ""
		if s == current {
			sel = `selected `
		}
		fmt.Fprintf(&opts, `<option value="%s" %s>%s</option>`, s, sel, statusLabels[s])
	}

	return fmt.Sprintf(`<input type="hidden" name="csrf_token" value="%s">
    <select name="status" class="field-inline w-40 status-select" data-color="%s" %s %s
      hx-post="/admin/orders/%s/status" hx-trigger="change" hx-target="#order-%s-status" hx-swap="outerHTML" hx-include="closest td">
      %s
    </select>`,
		ensureCSRFToken(w, r), statusVar(current), disabled, title, orderID, orderID, opts.String())
}

// orderStatusControlsHTML renders the order-detail page's status controls: the
// colored badge plus a forward-only select. For a cancelled (terminal) order
// only the badge is shown.
func (h *Handler) orderStatusControlsHTML(r *http.Request, orderID, current string) string {
	badge := fmt.Sprintf(`<span class="rounded-full border border-dashed px-3 py-1 text-xs font-semibold" style="background:var(--surface-warm);color:%s">%s</span>`,
		statusVar(current), statusLabels[current])

	if current == "cancelled" || current == "awaiting_payment" {
		// No manual control: cancelled is final, awaiting_payment is managed
		// by the payment flow itself.
		return `<div id="order-status-controls" class="flex items-center gap-3">` + badge + `</div>`
	}

	var opts strings.Builder
	for _, s := range database.ValidOrderStatusOptions(current) {
		sel := ""
		if s == current {
			sel = `selected `
		}
		fmt.Fprintf(&opts, `<option value="%s" %s>%s</option>`, s, sel, statusLabels[s])
	}

	return fmt.Sprintf(`<div id="order-status-controls" class="flex items-center gap-3">%s
  <select name="status" class="field-inline status-select" data-color="%s" style="color:%s"
    hx-post="/admin/orders/%s/status-badge" hx-trigger="change" hx-target="#order-status-controls" hx-swap="outerHTML">
    %s
  </select>
</div>`, badge, statusVar(current), statusVar(current), orderID, opts.String())
}

// AdminUpdateOrderStatusBadge returns the order-detail page's status controls
// (badge + forward-only select) as the HTMX target after a status change.
func (h *Handler) AdminUpdateOrderStatusBadge(w http.ResponseWriter, r *http.Request) {
	orderID := r.PathValue("id")
	if !validOrderID(orderID) {
		http.Error(w, "invalid order id", http.StatusBadRequest)
		return
	}

	status := r.FormValue("status")
	if !isKnownOrderStatus(status) {
		http.Error(w, "invalid status", http.StatusBadRequest)
		return
	}

	if err := database.UpdateOrderStatus(r.Context(), h.db, orderID, status); err != nil {
		if errors.Is(err, database.ErrInvalidOrderTransition) {
			orderTransitionToast(w, "تغییر وضعیت سفارش به عقب مجاز نیست؛ وضعیت فقط رو به جلو تغییر می‌کند.")
			http.Error(w, "invalid status transition", http.StatusBadRequest)
			return
		}
		logutil.Error("update order status badge", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, h.orderStatusControlsHTML(r, orderID, status))
}

// AdminUpdateOrderStatus updates the status of an order via an HTMX POST from
// the admin panel's inline <select>. It returns the new <td> with an updated
// <select> that offers only the statuses the order may still move to
// (forward-only; cancelled is terminal). Backward transitions are rejected by
// the database's state machine and surface as a Persian toast via HX-Trigger.
func (h *Handler) AdminUpdateOrderStatus(w http.ResponseWriter, r *http.Request) {
	orderID := r.PathValue("id")
	if !validOrderID(orderID) {
		http.Error(w, "invalid order id", http.StatusBadRequest)
		return
	}

	status := r.FormValue("status")
	if !isKnownOrderStatus(status) {
		http.Error(w, "invalid status", http.StatusBadRequest)
		return
	}

	if err := database.UpdateOrderStatus(r.Context(), h.db, orderID, status); err != nil {
		if errors.Is(err, database.ErrInvalidOrderTransition) {
			orderTransitionToast(w, "تغییر وضعیت سفارش به عقب مجاز نیست؛ وضعیت فقط رو به جلو تغییر می‌کند.")
			http.Error(w, "invalid status transition", http.StatusBadRequest)
			return
		}
		logutil.Error("update order status", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<td id="order-%s-status" class="px-4 py-3" onclick="event.stopPropagation()">
    %s
  </td>`, orderID, h.orderStatusSelectHTML(w, r, orderID, status))
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

	product, err := database.GetProduct(r.Context(), h.db, productID)
	if err != nil {
		http.Error(w, "product not found", http.StatusNotFound)
		return
	}

	product.IsActive = !product.IsActive
	if err := database.UpdateProduct(r.Context(), h.db, product); err != nil {
		logutil.Error("toggle product", "err", err)
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

	product, err := database.GetProduct(r.Context(), h.db, productID)
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

	if err := database.UpdateProduct(r.Context(), h.db, product); err != nil {
		logutil.Error("update product", "err", err)
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

	slug, err := database.UniqueSlug(r.Context(), h.db, name, 0)
	if err != nil {
		logutil.Error("create product slug", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

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

	id, err := database.CreateProduct(r.Context(), h.db, product)
	if err != nil {
		logutil.Error("create product", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	product.ID = id
	w.Header().Set("Content-Type", "text/html")
	h.renderProductRow(w, *product)
	// Out-of-band swap: re-open the modal in edit mode for the just-created
	// product, where the image gallery is now live.
	fmt.Fprint(w, renderModalOOB(h.renderProductEditModal(r, product)))
}

// AdminCreateCategory creates a new category from the admin form and prepends
// its row to the categories table via HTMX.
func (h *Handler) AdminCreateCategory(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	slug := strings.TrimSpace(r.FormValue("slug"))
	label := strings.TrimSpace(r.FormValue("label"))
	if slug == "" || label == "" {
		http.Error(w, "slug and label are required", http.StatusBadRequest)
		return
	}

	id, err := database.CreateCategory(r.Context(), h.db, slug, label)
	if err != nil {
		if errors.Is(err, database.ErrDuplicateCategory) {
			http.Error(w, "duplicate category slug", http.StatusBadRequest)
			return
		}
		logutil.Error("create category", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	newCat := models.Category{ID: id, Slug: slug, Label: label, IsEnabled: true}
	w.Header().Set("Content-Type", "text/html")
	h.renderCategoryRow(w, newCat)
	// Out-of-band swap: re-open the modal in edit mode for the just-created
	// category, where the image gallery is now live.
	fmt.Fprint(w, renderModalOOB(h.renderCategoryEditModal(r, newCat)))
}

// AdminToggleCategory flips the enabled state of a category and re-renders its
// table row via HTMX.
func (h *Handler) AdminToggleCategory(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	catID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid category id", http.StatusBadRequest)
		return
	}

	row, err := h.loadCategory(r, catID)
	if err != nil {
		http.Error(w, "category not found", http.StatusNotFound)
		return
	}

	row.IsEnabled = !row.IsEnabled
	if err := database.UpdateCategoryEnabled(r.Context(), h.db, row.ID, row.IsEnabled); err != nil {
		logutil.Error("toggle category", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.renderCategoryRow(w, *row)
}

// loadCategory fetches a single category by id for the toggle handler.
func (h *Handler) loadCategory(r *http.Request, id int64) (*models.Category, error) {
	row := h.db.QueryRowContext(r.Context(), "SELECT id, slug, label, is_enabled FROM categories WHERE id = ?", id)
	var c models.Category
	var isEnabled int
	if err := row.Scan(&c.ID, &c.Slug, &c.Label, &isEnabled); err != nil {
		return nil, err
	}
	c.IsEnabled = isEnabled == 1
	return &c, nil
}

// renderCategoryRow returns the HTML for a single <tr> in the admin categories
// table. This is used as the HTMX response for category create/toggle/update.
func (h *Handler) renderCategoryRow(w http.ResponseWriter, c models.Category) {
	w.Header().Set("Content-Type", "text/html")
	row := fmt.Sprintf(`<tr id="category-%d" class="border-b border-line/70 transition hover:bg-sand/40">
    <td class="px-4 py-3 text-sm text-clay font-mono tracking-wider">%d</td>
    <td class="px-4 py-3 text-sm font-medium text-walnut">%s</td>
    <td class="px-4 py-3 text-sm text-clay">%s</td>
    <td class="px-4 py-3">
      <button dir="ltr" hx-post="/admin/categories/%d/toggle" hx-target="#category-%d" hx-swap="outerHTML"
        class="relative inline-flex h-6 w-11 flex-shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-200 ease-in-out focus:outline-none %s">
        <span class="pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow-sm ring-0 transition duration-200 ease-in-out %s"></span>
      </button>
    </td>
    <td class="px-4 py-3">
      <button type="button" hx-get="/admin/categories/%d/edit" hx-target="#admin-modal" hx-swap="innerHTML"
        class="rounded-full border border-line px-3 py-1 text-xs text-clay transition hover:border-fig hover:text-fig"
        title="ویرایش دسته‌بندی">ویرایش</button>
    </td>
  </tr>`,
		c.ID, c.ID, htmlEscape(c.Label), htmlEscape(c.Slug),
		c.ID, c.ID,
		toggleBg(c.IsEnabled), toggleTranslate(c.IsEnabled),
		c.ID)

	fmt.Fprint(w, row)
}

// renderProductRow returns the HTML for a single <tr> in the admin products table.
// This is used as the HTMX response for all product CRUD operations.
func (h *Handler) renderProductRow(w http.ResponseWriter, p models.Product) {
	w.Header().Set("Content-Type", "text/html")
	inactiveClass := ""
	if !p.IsActive {
		inactiveClass = "opacity-50"
	}

	row := fmt.Sprintf(`<tr id="product-%d" class="border-b border-line/70 %s transition hover:bg-sand/40">
    <td class="px-4 py-3 text-sm text-clay font-mono tracking-wider">%d</td>
    <td class="px-4 py-3 text-sm font-medium text-walnut">%s</td>
    <td class="px-4 py-3 text-sm text-clay">%s</td>
    <td class="px-4 py-3">
      <input type="text" inputmode="numeric" name="price" value="%s"
        class="field-inline w-40 thousand-sep"
        hx-post="/admin/products/%d" hx-trigger="change" hx-target="#product-%d" hx-swap="outerHTML">
    </td>
    <td class="px-4 py-3">
      <input type="number" name="stock_quantity" value="%d" min="0"
        class="field-inline w-20"
        hx-post="/admin/products/%d" hx-trigger="change" hx-target="#product-%d" hx-swap="outerHTML">
    </td>
    <td class="px-4 py-3">
      <button dir="ltr" hx-post="/admin/products/%d/toggle" hx-target="#product-%d" hx-swap="outerHTML"
        class="relative inline-flex h-6 w-11 flex-shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-200 ease-in-out focus:outline-none %s">
        <span class="pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow-sm ring-0 transition duration-200 ease-in-out %s"></span>
      </button>
    </td>
    <td class="px-4 py-3">
      <button type="button" hx-get="/admin/products/%d/edit" hx-target="#admin-modal" hx-swap="innerHTML"
        class="rounded-full border border-line px-3 py-1 text-xs text-clay transition hover:border-fig hover:text-fig"
        title="ویرایش محصول">ویرایش</button>
    </td>
  </tr>`,
		p.ID, inactiveClass,
		p.ID, htmlEscape(p.Name), htmlEscape(p.Category),
		commaInt(p.Price), p.ID, p.ID,
		p.StockQuantity, p.ID, p.ID,
		p.ID, p.ID,
		toggleBg(p.IsActive), toggleTranslate(p.IsActive),
		p.ID)

	fmt.Fprint(w, row)
}

// toggleBg returns the background colour class for the toggle switch based on active state.
func toggleBg(active bool) string {
	if active {
		return "bg-pomegranate"
	}
	return "bg-line"
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
