package handlers

import (
	"net/http"
	"net/url"
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

	before, err := database.GetProduct(h.db, seedProductFig)
	if err != nil {
		t.Fatal(err)
	}
	resp := c.post("/admin/products/1/toggle", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("toggle = %d", resp.StatusCode)
	}
	after, err := database.GetProduct(h.db, seedProductFig)
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
	restored, _ := database.GetProduct(h.db, seedProductFig)
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
	p, err := database.GetProduct(h.db, seedProductFig)
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

// createOrderForTest inserts an order directly into the DB (plus one item) so
// admin status flows can be exercised without the payment gateway.
func createOrderForTest(t *testing.T, h *Handler) string {
	t.Helper()
	user, err := database.GetOrCreateUser(h.db, "09121234567")
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
	id, err := database.CreateOrder(h.db, order, []models.OrderItem{{ProductID: seedProductFig, Quantity: 1}})
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
	if err := database.UpdateOrderStatus(h.db, orderID, "dispatched"); err == nil {
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
