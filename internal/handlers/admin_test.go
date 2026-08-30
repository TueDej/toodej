package handlers

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"farmstore/internal/database"
	"farmstore/internal/models"
)

// TestAdminAuthProtection ensures every admin endpoint rejects unauthenticated
// requests and the BasicAuth realm is advertised, while known credentials pass.
func TestAdminAuthProtection(t *testing.T) {
	r, _, _ := newTestRouter(t)

	anon := newTestClient(t, r)
	resp := anon.get("/admin/")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admin without creds = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate on 401")
	}

	wrong := newTestClient(t, r)
	wrong.authorize("admin", "nope")
	resp = wrong.get("/admin/")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admin wrong creds = %d, want 401", resp.StatusCode)
	}

	ok := newTestClient(t, r)
	ok.authorize("admin", "admin123")
	resp = ok.get("/admin/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(ok.body(), "ADMIN-PANEL") {
		t.Fatalf("admin with creds = %d %q", resp.StatusCode, ok.body())
	}
}

func TestAdminProductToggle(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	before, err := database.GetProduct(context.Background(), h.db, seedProductFig)
	if err != nil {
		t.Fatal(err)
	}
	resp := c.post("/admin/products/1/toggle", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("toggle = %d", resp.StatusCode)
	}
	after, err := database.GetProduct(context.Background(), h.db, seedProductFig)
	if err != nil {
		t.Fatal(err)
	}
	if after.IsActive == before.IsActive {
		t.Fatalf("product active state unchanged: %v", after.IsActive)
	}
	resp = c.post("/admin/products/1/toggle", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("toggle back = %d", resp.StatusCode)
	}
	restored, _ := database.GetProduct(context.Background(), h.db, seedProductFig)
	if restored.IsActive != before.IsActive {
		t.Fatalf("product state not restored")
	}
}

func TestAdminUpdateProduct(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	form := url.Values{"price": {"9,900"}, "stock_quantity": {"37"}}
	resp := c.post("/admin/products/1", form)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update product = %d", resp.StatusCode)
	}
	p, err := database.GetProduct(context.Background(), h.db, seedProductFig)
	if err != nil {
		t.Fatal(err)
	}
	if p.Price != 9900 || p.StockQuantity != 37 {
		t.Fatalf("updated product = price %d stock %d, want 9900/37", p.Price, p.StockQuantity)
	}
}

func TestAdminCreateProduct(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	form := url.Values{
		"name":           {"آب نارگیل تازه"},
		"category":       {"تابستان"},
		"price":          {"45,000"},
		"stock_quantity": {"10"},
		"unit":           {"بطری ۱ لیتری"},
	}
	resp := c.post("/admin/products", form)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create product = %d (body: %.80s)", resp.StatusCode, c.body())
	}

	var price int
	err := h.db.QueryRow("SELECT price FROM products WHERE name = ?", "آب نارگیل تازه").Scan(&price)
	if err != nil {
		t.Fatalf("created product not found: %v", err)
	}
	if price != 45000 {
		t.Fatalf("created price = %d, want 45000", price)
	}
}

// TestAdminCreateProductSlugCollision ensures two products whose names map to
// the same slug (e.g. differing only in white space/case) are both created with
// unique slugs instead of one failing on the UNIQUE constraint.
func TestAdminCreateProductSlugCollision(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	form := func(name string) url.Values {
		return url.Values{
			"name":     {name},
			"category": {"تابستان"},
			"price":    {"10000"},
		}
	}

	if resp := c.post("/admin/products", form("Apple Sauce")); resp.StatusCode != http.StatusOK {
		t.Fatalf("create first product = %d (body: %.80s)", resp.StatusCode, c.body())
	}
	if resp := c.post("/admin/products", form("Apple  Sauce")); resp.StatusCode != http.StatusOK {
		t.Fatalf("create second product = %d (body: %.80s)", resp.StatusCode, c.body())
	}

	var slugs []string
	rows, err := h.db.Query("SELECT slug FROM products WHERE name IN ('Apple Sauce', 'Apple  Sauce') ORDER BY slug")
	if err != nil {
		t.Fatalf("query slugs: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan slug: %v", err)
		}
		slugs = append(slugs, s)
	}
	if rows.Err() != nil {
		t.Fatalf("iterate slugs: %v", rows.Err())
	}
	if len(slugs) != 2 || slugs[0] != "apple-sauce" || slugs[1] != "apple-sauce-2" {
		t.Fatalf("slugs = %v, want [apple-sauce apple-sauce-2]", slugs)
	}
}

// createOrderForTest inserts an order directly into the DB (plus one item) so
// admin status flows can be exercised without the payment gateway.
func createOrderForTest(t *testing.T, h *Handler) string {
	t.Helper()
	user, err := database.GetOrCreateUser(context.Background(), h.db, "09121234567")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	order := &models.Order{
		CustomerName:    "مدیر تست",
		CustomerPhone:   "09121234567",
		CustomerAddress: "تهران، تست",
		PostalCode:      "1234567890",
		Status:          "pending",
		UserID:          user.ID,
	}
	id, err := database.CreateOrder(context.Background(), h.db, order, []models.OrderItem{{ProductID: seedProductFig, Quantity: 1}})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	return id
}

func orderStatus(t *testing.T, h *Handler, id string) string {
	t.Helper()
	var s string
	if err := h.db.QueryRow("SELECT status FROM orders WHERE id = ?", id).Scan(&s); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return s
}

func TestAdminOrderStatusTransitions(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	orderID := createOrderForTest(t, h) // starts "pending"

	for _, status := range []string{"preparing", "dispatched", "cancelled"} {
		resp := c.post("/admin/orders/"+orderID+"/status", url.Values{"status": {status}})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("transition to %s = %d", status, resp.StatusCode)
		}
		if got := orderStatus(t, h, orderID); got != status {
			t.Fatalf("db status = %q after transition to %q", got, status)
		}
	}
}

func TestAdminOrderStatusMachineEnforced(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	orderID := createOrderForTest(t, h) // pending

	// Direct DB rejects an impossible jump: pending → dispatched.
	if err := database.UpdateOrderStatus(context.Background(), h.db, orderID, "dispatched"); err == nil {
		t.Fatal("pending → dispatched accepted")
	}
	// Valid step via the endpoint.
	resp := c.post("/admin/orders/"+orderID+"/status", url.Values{"status": {"preparing"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preparing = %d", resp.StatusCode)
	}
	// Invalid step via the endpoint: preparing → pending is not allowed.
	resp = c.post("/admin/orders/"+orderID+"/status", url.Values{"status": {"pending"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("preparing → pending = %d, want 400", resp.StatusCode)
	}
	// Invalid status value.
	resp = c.post("/admin/orders/"+orderID+"/status", url.Values{"status": {"hacked"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid status = %d, want 400", resp.StatusCode)
	}
	// Malformed order ID.
	resp = c.post("/admin/orders/not-an-id/status", url.Values{"status": {"pending"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad order id = %d, want 400", resp.StatusCode)
	}

	// Badge endpoint also follows the state machine.
	resp = c.post("/admin/orders/"+orderID+"/status-badge", url.Values{"status": {"dispatched"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("badge dispatched = %d", resp.StatusCode)
	}
	resp = c.post("/admin/orders/"+orderID+"/status-badge", url.Values{"status": {"preparing"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("badge invalid transition = %d, want 400", resp.StatusCode)
	}
}

func TestAdminOrderDetail(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	orderID := createOrderForTest(t, h)
	resp := c.get("/admin/orders/" + orderID)
	if resp.StatusCode != http.StatusOK || !strings.Contains(c.body(), "ORDER-DETAIL-PAGE") {
		t.Fatalf("order detail = %d %q", resp.StatusCode, c.body())
	}

	bad := newTestClient(t, r)
	resp = bad.get("/admin/orders/" + orderID)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("order detail unauth = %d, want 401", resp.StatusCode)
	}
}

func TestAdminDashboardListsData(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	orderID := createOrderForTest(t, h)
	resp := c.get("/admin/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(c.body(), "ADMIN-PANEL") {
		t.Fatalf("dashboard = %d", resp.StatusCode)
	}
	if !strings.Contains(c.body(), orderID) {
		t.Fatalf("dashboard missing order %q", orderID)
	}
}

func TestAdminCreateCategoryUnauthenticated(t *testing.T) {
	r, _, _ := newTestRouter(t)
	anon := newTestClient(t, r)
	resp := anon.post("/admin/categories", url.Values{"slug": {"x"}, "label": {"x"}})
	// Unauthenticated requests must be rejected (BasicAuth → 401, or the
	// CSRF middleware that sits ahead of it → 403). Either proves the
	// endpoint is not openly writable.
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("unauth create category = %d, want rejected", resp.StatusCode)
	}
}

func TestAdminCreateCategory(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	resp := c.post("/admin/categories", url.Values{"slug": {"newcat"}, "label": {"جدید"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create category = %d (body: %.80s)", resp.StatusCode, c.body())
	}

	var id int64
	var slug, label string
	var enabled int
	err := h.db.QueryRow("SELECT id, slug, label, is_enabled FROM categories WHERE slug = ?", "newcat").
		Scan(&id, &slug, &label, &enabled)
	if err != nil {
		t.Fatalf("created category not found: %v", err)
	}
	if slug != "newcat" || label != "جدید" || enabled != 1 {
		t.Fatalf("created category = %q %q enabled=%d", slug, label, enabled)
	}
}

func TestAdminCreateCategoryDuplicate(t *testing.T) {
	r, _, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	// "fig" is a seeded slug — a second insert must be rejected with 400.
	resp := c.post("/admin/categories", url.Values{"slug": {"fig"}, "label": {"تکراری"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate category = %d, want 400", resp.StatusCode)
	}
}

func TestAdminToggleCategory(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	var id int64
	var beforeEnabled int
	if err := h.db.QueryRow("SELECT id, is_enabled FROM categories WHERE slug = ?", "test").Scan(&id, &beforeEnabled); err != nil {
		t.Fatalf("read test category: %v", err)
	}

	resp := c.post("/admin/categories/"+strconv.FormatInt(id, 10)+"/toggle", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("toggle category = %d", resp.StatusCode)
	}

	var afterEnabled int
	if err := h.db.QueryRow("SELECT is_enabled FROM categories WHERE id = ?", id).Scan(&afterEnabled); err != nil {
		t.Fatalf("read toggled category: %v", err)
	}
	if afterEnabled == beforeEnabled {
		t.Fatalf("category enabled state unchanged: %d", afterEnabled)
	}
}

// TestAdminReorderProducts posts a new product order (as the drag-and-drop UI
// does) and confirms it is persisted and reflected by GetAllProducts.
func TestAdminReorderProducts(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	ctx := context.Background()
	before, err := database.GetAllProducts(ctx, h.db)
	if err != nil || len(before) < 2 {
		t.Fatalf("seeded products: %v (%d)", err, len(before))
	}
	rev := make([]string, len(before))
	for i, p := range before {
		rev[len(before)-1-i] = strconv.FormatInt(p.ID, 10)
	}

	resp := c.post("/admin/products/reorder", url.Values{"order": {strings.Join(rev, ",")}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reorder = %d (body: %.80s)", resp.StatusCode, c.body())
	}

	after, err := database.GetAllProducts(ctx, h.db)
	if err != nil {
		t.Fatalf("GetAllProducts: %v", err)
	}
	for i := range after {
		want, _ := strconv.ParseInt(rev[i], 10, 64)
		if after[i].ID != want {
			t.Fatalf("order[%d] = %d, want %d", i, after[i].ID, want)
		}
	}
}

// TestAdminReorderProductsRejectsBadInput: empty or non-numeric ids are 400s.
func TestAdminReorderProductsRejectsBadInput(t *testing.T) {
	r, _, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	if resp := c.post("/admin/products/reorder", url.Values{"order": {""}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty order = %d, want 400", resp.StatusCode)
	}
	if resp := c.post("/admin/products/reorder", url.Values{"order": {"1,abc"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-numeric order = %d, want 400", resp.StatusCode)
	}
}

// TestAdminReorderProductsUnauthenticated: the endpoint is not openly writable.
func TestAdminReorderProductsUnauthenticated(t *testing.T) {
	r, _, _ := newTestRouter(t)
	anon := newTestClient(t, r)
	resp := anon.post("/admin/products/reorder", url.Values{"order": {"1,2"}})
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("unauth reorder = %d, want rejected", resp.StatusCode)
	}
}
