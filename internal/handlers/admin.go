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

// maxTrackingCodeLen caps the optional postal tracking code entered by the
// admin; Iranian postal tracking numbers are well under this length.
const maxTrackingCodeLen = 64

// trackingCodeFromForm extracts the optional postal tracking code (کد رهگیری
// پستی) that accompanies an admin status change. Empty is allowed.
func trackingCodeFromForm(r *http.Request) (string, error) {
	code := strings.TrimSpace(r.FormValue("tracking_code"))
	if len(code) > maxTrackingCodeLen {
		return "", fmt.Errorf("tracking code longer than %d characters", maxTrackingCodeLen)
	}
	return code, nil
}

// dispatchEditButtonHTML renders the small button shown for an already
// dispatched order. Clicking it re-opens the dispatch prompt (layout.html's
// #dispatch-confirm-tpl) so the postal tracking code can be viewed or changed;
// confirming posts the same endpoint with status=dispatched, which the
// database state machine accepts as a same-status update.
func dispatchEditButtonHTML(postURL, targetID, code string) string {
	label := "ثبت کد رهگیری پستی"
	if code != "" {
		label = fmt.Sprintf(`<span dir="ltr" class="font-mono tracking-wider">%s</span>`, htmlEscape(code))
	}
	return fmt.Sprintf(`<button type="button" class="js-edit-tracking mt-1.5 block rounded-full border border-line px-3 py-1 text-xs text-clay transition hover:border-fig hover:text-fig" data-post="%s" data-target="%s" data-code="%s" title="مشاهده / ویرایش کد رهگیری پستی">%s</button>`,
		postURL, targetID, htmlEscape(code), label)
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
// Selecting dispatched (ارسال شد) is intercepted client-side: the layout's
// dispatch-confirm prompt asks for the postal tracking code before the
// POST goes out. For already-dispatched orders an edit button re-opens the
// prompt instead.
func (h *Handler) orderStatusSelectHTML(w http.ResponseWriter, r *http.Request, orderID, current, trackingCode string) string {
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

	trackingButton := ""
	if current == "dispatched" {
		postURL := "/admin/orders/" + orderID + "/status"
		trackingButton = dispatchEditButtonHTML(postURL, "#order-"+orderID+"-status", trackingCode)
	}

	return fmt.Sprintf(`<input type="hidden" name="csrf_token" value="%s">
    <select name="status" class="field-inline w-40 status-select" data-color="%s" %s %s
      hx-post="/admin/orders/%s/status" hx-trigger="change" hx-target="#order-%s-status" hx-swap="outerHTML" hx-include="closest td">
      %s
    </select>%s`,
		ensureCSRFToken(w, r), statusVar(current), disabled, title, orderID, orderID, opts.String(), trackingButton)
}

// orderStatusControlsHTML renders the order-detail page's status controls: the
// colored badge plus — for a dispatched order — the button that re-opens the
// dispatch prompt to view/edit the postal tracking code. The order-detail page
// deliberately offers no status <select>; status switching lives in the admin
// panel's orders table only. The status-badge endpoint re-renders these
// controls after a tracking-code edit.
func (h *Handler) orderStatusControlsHTML(r *http.Request, orderID, current, trackingCode string) string {
	badge := fmt.Sprintf(`<span class="rounded-full border border-dashed px-3 py-1 text-xs font-semibold" style="background:var(--surface-warm);color:%s">%s</span>`,
		statusVar(current), statusLabels[current])

	controls := badge
	if current == "dispatched" {
		controls += dispatchEditButtonHTML("/admin/orders/"+orderID+"/status-badge", "#order-status-controls", trackingCode)
	}

	return `<div id="order-status-controls" class="flex flex-wrap items-center gap-3">` + controls + `</div>`
}

// AdminUpdateOrderStatusBadge returns the order-detail page's status controls
// (badge + forward-only select) as the HTMX target after a status change.
// When the order moves to dispatched (ارسال شد) the optional postal tracking
// code submitted alongside is stored on the order.
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

	trackingCode, err := trackingCodeFromForm(r)
	if err != nil {
		http.Error(w, "invalid tracking code", http.StatusBadRequest)
		return
	}

	changed, err := database.UpdateOrderStatus(r.Context(), h.db, orderID, status, trackingCode)
	if err != nil {
		if errors.Is(err, database.ErrInvalidOrderTransition) {
			orderTransitionToast(w, "تغییر وضعیت سفارش به عقب مجاز نیست؛ وضعیت فقط رو به جلو تغییر می‌کند.")
			http.Error(w, "invalid status transition", http.StatusBadRequest)
			return
		}
		logutil.Error("update order status badge", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// One-time customer SMS on real transitions only (same-status tracking-code
	// edits are silent). Dispatched with an empty code notifies without one.
	if changed {
		h.notifyOrderStatusAsync(orderID, status, trackingCode)
	}

	// The tracking code only ever changes when the order is dispatched; render
	// the stored value back into the input for any other status.
	renderedCode := ""
	if status == "dispatched" {
		renderedCode = trackingCode
	}
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, h.orderStatusControlsHTML(r, orderID, status, renderedCode))
}

// AdminUpdateOrderStatus updates the status of an order via an HTMX POST from
// the admin panel's inline <select>. It returns the new <td> with an updated
// <select> that offers only the statuses the order may still move to
// (forward-only; cancelled is terminal). Backward transitions are rejected by
// the database's state machine and surface as a Persian toast via HX-Trigger.
// When the order moves to dispatched (ارسال شد) the optional postal tracking
// code submitted alongside is stored on the order.
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

	trackingCode, err := trackingCodeFromForm(r)
	if err != nil {
		http.Error(w, "invalid tracking code", http.StatusBadRequest)
		return
	}

	changed, err := database.UpdateOrderStatus(r.Context(), h.db, orderID, status, trackingCode)
	if err != nil {
		if errors.Is(err, database.ErrInvalidOrderTransition) {
			orderTransitionToast(w, "تغییر وضعیت سفارش به عقب مجاز نیست؛ وضعیت فقط رو به جلو تغییر می‌کند.")
			http.Error(w, "invalid status transition", http.StatusBadRequest)
			return
		}
		logutil.Error("update order status", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// One-time customer SMS on real transitions only (same-status tracking-code
	// edits are silent). Dispatched with an empty code notifies without one.
	if changed {
		h.notifyOrderStatusAsync(orderID, status, trackingCode)
	}

	renderedCode := ""
	if status == "dispatched" {
		renderedCode = trackingCode
	}
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<td id="order-%s-status" class="px-4 py-3" onclick="event.stopPropagation()">
    %s
  </td>`, orderID, h.orderStatusSelectHTML(w, r, orderID, status, renderedCode))
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
	description := strings.TrimSpace(r.FormValue("description"))
	if slug == "" || label == "" {
		http.Error(w, "slug and label are required", http.StatusBadRequest)
		return
	}

	id, err := database.CreateCategory(r.Context(), h.db, slug, label, description)
	if err != nil {
		if errors.Is(err, database.ErrDuplicateCategory) {
			http.Error(w, "duplicate category slug", http.StatusBadRequest)
			return
		}
		logutil.Error("create category", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	newCat := models.Category{ID: id, Slug: slug, Label: label, Description: description, IsEnabled: true}
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
	row := h.db.QueryRowContext(r.Context(), "SELECT id, slug, label, is_enabled, description FROM categories WHERE id = ?", id)
	var c models.Category
	var isEnabled int
	if err := row.Scan(&c.ID, &c.Slug, &c.Label, &isEnabled, &c.Description); err != nil {
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

	row := fmt.Sprintf(`<tr id="product-%d" draggable="true" class="border-b border-line/70 %s transition hover:bg-sand/40">
    <td class="px-2 py-3 text-center text-clay/50">
      <span class="drag-handle inline-flex cursor-grab touch-none select-none items-center justify-center" title="برای تغییر ترتیب، بکشید و رها کنید" aria-hidden="true">
        <svg width="10" height="16" viewBox="0 0 10 16" fill="currentColor"><circle cx="3" cy="3" r="1.4"/><circle cx="7" cy="3" r="1.4"/><circle cx="3" cy="8" r="1.4"/><circle cx="7" cy="8" r="1.4"/><circle cx="3" cy="13" r="1.4"/><circle cx="7" cy="13" r="1.4"/></svg>
      </span>
    </td>
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

// AdminReorderProducts persists the admin's drag-and-drop ordering of products.
// The new order arrives as a comma-separated list of product ids in the `order`
// field (first id = shown first); positions are rewritten 0..n-1 so the
// storefront lists products in exactly that order.
func (h *Handler) AdminReorderProducts(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	raw := strings.TrimSpace(r.FormValue("order"))
	if raw == "" {
		http.Error(w, "order is required", http.StatusBadRequest)
		return
	}
	parts := strings.Split(raw, ",")
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "invalid product id in order", http.StatusBadRequest)
			return
		}
		ids = append(ids, id)
	}
	if err := database.SetProductOrder(r.Context(), h.db, ids); err != nil {
		logutil.Error("reorder products", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
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
