package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"farmstore/internal/database"
	"farmstore/internal/logutil"
	"farmstore/internal/models"
)

// ── Admin create/edit modals ─────────────────────────
//
// Products and categories are created and edited through a single modal
// (#admin-modal in admin.html) fetched over HTMX. The create forms post to the
// existing create endpoints, which respond with the table row (prepended by
// HTMX) plus an out-of-band swap that re-opens the modal in edit mode — where
// the image gallery becomes live, since an image needs a saved owner.

// adminCloseModalScript is the shared JS snippet that empties the modal host.
const adminCloseModalScript = `document.getElementById('admin-modal').innerHTML=''`

// renderModalShell wraps modal content in the fixed overlay. Clicking the
// backdrop or the × button closes it.
func renderModalShell(title, inner string) string {
	return fmt.Sprintf(`<div class="fixed inset-0 z-50 overflow-y-auto bg-walnut/50 p-4" onclick="if(event.target===this) %s">
  <div class="paper-card mx-auto my-8 w-full max-w-2xl p-6">
    <div class="mb-4 flex items-center justify-between">
      <h3 class="font-display text-lg font-semibold text-walnut">%s</h3>
      <button type="button" onclick="%s" title="بستن"
        class="flex h-8 w-8 items-center justify-center rounded-full border border-line text-clay transition hover:border-rose hover:text-pomegranate">×</button>
    </div>
    %s
  </div>
</div>`, adminCloseModalScript, title, adminCloseModalScript, inner)
}

// renderModalOOB wraps a modal fragment for out-of-band swapping into
// #admin-modal, alongside a main HTMX swap target response.
func renderModalOOB(inner string) string {
	return fmt.Sprintf(`<div id="admin-modal" hx-swap-oob="innerHTML">%s</div>`, inner)
}

// renderModalClearOOB emits an out-of-band swap that empties #admin-modal;
// appended to row responses so a successful edit closes the modal.
func renderModalClearOOB() string {
	return `<div id="admin-modal" hx-swap-oob="innerHTML"></div>`
}

// renderCategoryOptions renders <option> elements for the product form's
// category select, marking the product's current category.
func renderCategoryOptions(categories []models.Category, current string) string {
	var b strings.Builder
	for _, c := range categories {
		if !c.IsEnabled {
			continue
		}
		sel := ""
		if c.Label == current {
			sel = "selected"
		}
		fmt.Fprintf(&b, `<option value="%s" %s>%s</option>`, htmlEscape(c.Label), sel, htmlEscape(c.Label))
	}
	return b.String()
}

// galleryPlaceholder is shown in create modals, where the owner does not exist
// yet and images cannot be attached.
const galleryPlaceholder = `<div class="mt-2 rounded-xl border border-dashed border-line bg-sand/40 px-4 py-3 text-xs leading-6 text-clay">پس از ذخیره می‌توانید تصاویر را اینجا اضافه کنید.</div>`

// renderProductFormInner renders the shared fields of the product create/edit
// form. action/target/swap control where HTMX posts and what it swaps.
func renderProductFormInner(r *http.Request, p *models.Product, categories []models.Category, action, target, swap, afterSwap string) string {
	isActiveChecked := ""
	if p.IsActive {
		isActiveChecked = "checked"
	}
	return fmt.Sprintf(`
<form hx-post="%s" hx-target="%s" hx-swap="%s"%s class="space-y-4">
  <input type="hidden" name="csrf_token" value="%s">
  <div class="grid gap-4 sm:grid-cols-2">
    <div>
      <label class="lbl block text-xs">نام *</label>
      <input type="text" name="name" required value="%s" class="field mt-1.5 text-sm">
    </div>
    <div>
      <label class="lbl block text-xs">دسته‌بندی *</label>
      <select name="category" required class="field mt-1.5 text-sm">%s</select>
    </div>
    <div>
      <label class="lbl block text-xs">قیمت (تومان) *</label>
      <input type="text" inputmode="numeric" name="price" required value="%s" class="field mt-1.5 text-sm thousand-sep">
    </div>
    <div>
      <label class="lbl block text-xs">موجودی</label>
      <input type="number" name="stock_quantity" min="0" value="%d" class="field mt-1.5 text-sm">
    </div>
    <div>
      <label class="lbl block text-xs">واحد</label>
      <input type="text" name="unit" placeholder="مثلاً ۱ کیلوگرم" value="%s" class="field mt-1.5 text-sm">
    </div>
    <div class="flex flex-col">
      <span class="lbl block text-xs opacity-0 select-none" aria-hidden="true">واحد</span>
      <label class="mt-1.5 flex flex-1 cursor-pointer items-center gap-2 text-sm text-walnut">
        <input type="checkbox" name="is_active" value="1" %s class="h-4 w-4 accent-pomegranate">
        فعال (نمایش در فروشگاه)
      </label>
    </div>
    <div class="sm:col-span-2">
      <label class="lbl block text-xs">توضیحات</label>
      <textarea name="description" rows="3" class="field mt-1.5 text-sm">%s</textarea>
    </div>
  </div>
  <div class="flex gap-2">
    <button type="submit" class="btn btn-forest px-5 py-2.5 text-sm">ذخیره</button>
    <button type="button" onclick="%s" class="btn btn-ghost px-5 py-2.5 text-sm">انصراف</button>
  </div>
</form>`,
		action, target, swap, afterSwap,
		ensureCSRFToken(nil, r),
		htmlEscape(p.Name), renderCategoryOptions(categories, p.Category),
		commaInt(p.Price), p.StockQuantity,
		htmlEscape(p.Unit), isActiveChecked, htmlEscape(p.Description),
		adminCloseModalScript)
}

// renderCategoryFormInner renders the shared fields of the category
// create/edit form.
func renderCategoryFormInner(r *http.Request, c models.Category, action, target, swap, afterSwap string) string {
	return fmt.Sprintf(`
<form hx-post="%s" hx-target="%s" hx-swap="%s"%s class="space-y-4">
  <input type="hidden" name="csrf_token" value="%s">
  <div class="grid gap-4 sm:grid-cols-2">
    <div>
      <label class="lbl block text-xs">نامک (انگلیسی) *</label>
      <input type="text" name="slug" required placeholder="مثلاً fig" value="%s" class="field mt-1.5 text-sm ltr">
    </div>
    <div>
      <label class="lbl block text-xs">برچسب *</label>
      <input type="text" name="label" required placeholder="مثلاً انجیر" value="%s" class="field mt-1.5 text-sm">
    </div>
  </div>
  <div class="flex gap-2">
    <button type="submit" class="btn btn-forest px-5 py-2.5 text-sm">ذخیره</button>
    <button type="button" onclick="%s" class="btn btn-ghost px-5 py-2.5 text-sm">انصراف</button>
  </div>
</form>`,
		action, target, swap, afterSwap,
		ensureCSRFToken(nil, r),
		htmlEscape(c.Slug), htmlEscape(c.Label),
		adminCloseModalScript)
}

// renderProductEditModal renders the full edit modal for a saved product,
// including its live image gallery.
func (h *Handler) renderProductEditModal(r *http.Request, p *models.Product) string {
	categories, err := database.GetEnabledCategories(r.Context(), h.db)
	if err != nil {
		categories = nil
	}
	gallery := galleryPlaceholder
	if p.ID != 0 {
		if g, err := h.renderImageGalleryString(r, database.ImageOwnerProduct, p.ID, ""); err == nil {
			gallery = g
		}
	}
	form := renderProductFormInner(r, p, categories,
		"/admin/products/"+strconv.FormatInt(p.ID, 10)+"/update",
		"#product-"+strconv.FormatInt(p.ID, 10), "outerHTML",
		` hx-on::after-swap="`+adminCloseModalScript+`"`)
	inner := form + fmt.Sprintf(`
<div class="mt-5 border-t border-line/70 pt-4">
  <h4 class="font-display text-sm font-semibold text-walnut">تصاویر محصول</h4>
  %s
</div>`, gallery)
	return renderModalShell("ویرایش محصول <span class='font-mono text-sm text-clay'>#"+strconv.FormatInt(p.ID, 10)+"</span>", inner)
}

// renderCategoryEditModal renders the full edit modal for a saved category.
func (h *Handler) renderCategoryEditModal(r *http.Request, c models.Category) string {
	gallery := galleryPlaceholder
	if c.ID != 0 {
		if g, err := h.renderImageGalleryString(r, database.ImageOwnerCategory, c.ID, ""); err == nil {
			gallery = g
		}
	}
	form := renderCategoryFormInner(r, c,
		"/admin/categories/"+strconv.FormatInt(c.ID, 10)+"/update",
		"#category-"+strconv.FormatInt(c.ID, 10), "outerHTML",
		` hx-on::after-swap="`+adminCloseModalScript+`"`)
	inner := form + fmt.Sprintf(`
<div class="mt-5 border-t border-line/70 pt-4">
  <h4 class="font-display text-sm font-semibold text-walnut">تصاویر دسته‌بندی</h4>
  %s
</div>`, gallery)
	return renderModalShell("ویرایش دسته‌بندی <span class='font-mono text-sm text-clay'>#"+strconv.FormatInt(c.ID, 10)+"</span>", inner)
}

// ── Product create/edit endpoints ────────────────────

// AdminNewProduct serves the create-product modal (HTMX GET).
func (h *Handler) AdminNewProduct(w http.ResponseWriter, r *http.Request) {
	categories, err := database.GetEnabledCategories(r.Context(), h.db)
	if err != nil {
		categories = nil
	}
	p := &models.Product{IsActive: true}
	w.Header().Set("Content-Type", "text/html")
	form := renderProductFormInner(r, p, categories, "/admin/products", "#products-tbody", "afterbegin", "")
	inner := form + fmt.Sprintf(`
<div class="mt-5 border-t border-line/70 pt-4">
  <h4 class="font-display text-sm font-semibold text-walnut">تصاویر محصول</h4>
  %s
</div>`, galleryPlaceholder)
	fmt.Fprint(w, renderModalShell("محصول جدید", inner))
}

// AdminEditProduct serves the edit-product modal (HTMX GET).
func (h *Handler) AdminEditProduct(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid product id", http.StatusBadRequest)
		return
	}
	p, err := database.GetProduct(r.Context(), h.db, id)
	if err != nil {
		http.Error(w, "product not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, h.renderProductEditModal(r, p))
}

// parseProductForm extracts and validates the shared product form fields.
// It returns the parsed values and whether validation passed; on failure it
// has already written the error response.
func parseProductForm(w http.ResponseWriter, r *http.Request) (name, category, unit, description string, price, stock int, isActive bool, ok bool) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return "", "", "", "", 0, 0, false, false
	}
	name = strings.TrimSpace(r.FormValue("name"))
	category = strings.TrimSpace(r.FormValue("category"))
	unit = strings.TrimSpace(r.FormValue("unit"))
	description = strings.TrimSpace(r.FormValue("description"))
	isActive = r.FormValue("is_active") == "1"

	priceStr := strings.ReplaceAll(strings.TrimSpace(r.FormValue("price")), ",", "")
	price, err := strconv.Atoi(priceStr)
	if name == "" || category == "" || err != nil || price < 0 {
		http.Error(w, "name, category, and a valid price are required", http.StatusBadRequest)
		return "", "", "", "", 0, 0, false, false
	}
	stockStr := strings.ReplaceAll(strings.TrimSpace(r.FormValue("stock_quantity")), ",", "")
	if stockStr != "" {
		stock, _ = strconv.Atoi(stockStr)
		if stock < 0 {
			stock = 0
		}
	}
	return name, category, unit, description, price, stock, isActive, true
}

// AdminUpdateProductFull applies the edit modal's full product form and
// responds with the updated table row plus an out-of-band swap that closes
// the modal.
func (h *Handler) AdminUpdateProductFull(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid product id", http.StatusBadRequest)
		return
	}
	p, err := database.GetProduct(r.Context(), h.db, id)
	if err != nil {
		http.Error(w, "product not found", http.StatusNotFound)
		return
	}

	name, category, unit, description, price, stock, isActive, ok := parseProductForm(w, r)
	if !ok {
		return
	}
	p.Name, p.Category, p.Unit, p.Description = name, category, unit, description
	p.Price, p.StockQuantity, p.IsActive = price, stock, isActive

	if err := database.UpdateProduct(r.Context(), h.db, p); err != nil {
		logutil.Error("update product full", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	h.renderProductRow(w, *p)
	fmt.Fprint(w, renderModalClearOOB())
}

// ── Category create/edit endpoints ───────────────────

// AdminNewCategory serves the create-category modal (HTMX GET).
func (h *Handler) AdminNewCategory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	form := renderCategoryFormInner(r, models.Category{}, "/admin/categories", "#categories-tbody", "afterbegin", "")
	inner := form + fmt.Sprintf(`
<div class="mt-5 border-t border-line/70 pt-4">
  <h4 class="font-display text-sm font-semibold text-walnut">تصاویر دسته‌بندی</h4>
  %s
</div>`, galleryPlaceholder)
	fmt.Fprint(w, renderModalShell("دسته‌بندی جدید", inner))
}

// AdminEditCategory serves the edit-category modal (HTMX GET).
func (h *Handler) AdminEditCategory(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid category id", http.StatusBadRequest)
		return
	}
	row, err := h.categoryByID(r, id)
	if err != nil {
		http.Error(w, "category not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, h.renderCategoryEditModal(r, *row))
}

// categoryByID fetches one category by id.
func (h *Handler) categoryByID(r *http.Request, id int64) (*models.Category, error) {
	categories, err := database.GetCategories(r.Context(), h.db)
	if err != nil {
		return nil, err
	}
	for i := range categories {
		if categories[i].ID == id {
			return &categories[i], nil
		}
	}
	return nil, database.ErrImageOwnerNotFound
}

// AdminUpdateCategoryFull applies the edit modal's category form and responds
// with the updated row plus an out-of-band modal clear.
func (h *Handler) AdminUpdateCategoryFull(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid category id", http.StatusBadRequest)
		return
	}
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

	current, err := h.categoryByID(r, id)
	if err != nil {
		http.Error(w, "category not found", http.StatusNotFound)
		return
	}
	if err := database.UpdateCategory(r.Context(), h.db, id, slug, label, current.IsEnabled); err != nil {
		if errors.Is(err, database.ErrDuplicateCategory) {
			http.Error(w, "duplicate category slug", http.StatusBadRequest)
			return
		}
		logutil.Error("update category full", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	h.renderCategoryRow(w, models.Category{ID: id, Slug: slug, Label: label, IsEnabled: current.IsEnabled})
	fmt.Fprint(w, renderModalClearOOB())
}
