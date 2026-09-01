package handlers

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"farmstore/internal/database"
)

// TestAdminStatusSelectNeverOffersBackwardMoves drives the HTMX endpoint the
// admin table's <select> posts to and asserts:
//   - backward transitions are rejected (400) with a Persian adminToast event,
//   - the success response re-renders the select with forward options only,
//   - a cancelled order is terminal: disabled select, no way out.
func TestAdminStatusSelectNeverOffersBackwardMoves(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	orderID := createOrderForTest(t, h) // starts "pending"

	// pending → preparing is a valid forward step.
	resp := c.post("/admin/orders/"+orderID+"/status", url.Values{"status": {"preparing"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pending → preparing = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(c.body(), `value="dispatched"`) || !strings.Contains(c.body(), `value="cancelled"`) {
		t.Fatalf("preparing select missing forward options: %s", c.body())
	}
	if strings.Contains(c.body(), `value="pending"`) {
		t.Fatalf("preparing select still offers backward move to pending: %s", c.body())
	}

	// preparing → pending is backward: rejected with the admin toast header.
	resp = c.post("/admin/orders/"+orderID+"/status", url.Values{"status": {"pending"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("preparing → pending = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("HX-Trigger"), "adminToast") {
		t.Fatalf("rejected transition missing adminToast trigger, got %q", resp.Header.Get("HX-Trigger"))
	}
	if got := orderStatus(t, h, orderID); got != "preparing" {
		t.Fatalf("status mutated by rejected transition: %q", got)
	}

	// Walk to dispatched, then cancel.
	for _, status := range []string{"dispatched", "cancelled"} {
		if resp := c.post("/admin/orders/"+orderID+"/status", url.Values{"status": {status}}); resp.StatusCode != http.StatusOK {
			t.Fatalf("→ %s = %d", status, resp.StatusCode)
		}
	}

	// cancelled is terminal: select disabled, only itself offered.
	if resp := c.post("/admin/orders/"+orderID+"/status", url.Values{"status": {"cancelled"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("cancelled re-render = %d", resp.StatusCode)
	}
	if !strings.Contains(c.body(), "disabled") {
		t.Fatalf("cancelled select not disabled: %s", c.body())
	}
	for _, banned := range []string{`value="pending"`, `value="preparing"`, `value="dispatched"`} {
		if strings.Contains(c.body(), banned) {
			t.Fatalf("cancelled select offers %s: %s", banned, c.body())
		}
	}
	// The DB must agree: any move out of cancelled is rejected.
	if _, err := database.UpdateOrderStatus(context.Background(), h.db, orderID, "dispatched", ""); err == nil {
		t.Fatal("cancelled → dispatched accepted by DB")
	}
}

// TestAdminOrderDetailStatusControls checks the order-detail page renders a
// status badge WITHOUT a status <select> (status switching lives in the admin
// panel's orders table only) and that the badge endpoint still accepts
// tracking-code edits for dispatched orders while rejecting backward moves.
func TestAdminOrderDetailStatusControls(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	orderID := createOrderForTest(t, h) // pending

	if resp := c.get("/admin/orders/" + orderID); resp.StatusCode != http.StatusOK {
		t.Fatalf("order detail = %d", resp.StatusCode)
	}
	if !strings.Contains(c.body(), `id="order-status-controls"`) {
		t.Fatal("order detail missing status controls container")
	}
	if strings.Contains(c.body(), `<select name="status"`) {
		t.Fatal("order detail should not offer a status select")
	}

	// The badge endpoint (used by the tracking-code edit button) re-renders the
	// controls block; a backward move from preparing is still rejected with the
	// toast header.
	if resp := c.post("/admin/orders/"+orderID+"/status", url.Values{"status": {"preparing"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("preparing = %d", resp.StatusCode)
	}
	resp := c.post("/admin/orders/"+orderID+"/status-badge", url.Values{"status": {"pending"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("badge preparing → pending rejected = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("HX-Trigger"), "adminToast") {
		t.Fatalf("badge rejected transition missing adminToast, got %q", resp.Header.Get("HX-Trigger"))
	}

	// Walk forward to dispatched, then edit the tracking code via the badge
	// endpoint exactly like the detail page's edit button does.
	if resp := c.post("/admin/orders/"+orderID+"/status", url.Values{"status": {"preparing"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("preparing = %d", resp.StatusCode)
	}
	if resp := c.post("/admin/orders/"+orderID+"/status", url.Values{"status": {"dispatched"}, "tracking_code": {"2468013579"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("dispatched = %d", resp.StatusCode)
	}
	resp = c.post("/admin/orders/"+orderID+"/status-badge", url.Values{"status": {"dispatched"}, "tracking_code": {"111222333"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("badge tracking edit = %d", resp.StatusCode)
	}
	if !strings.Contains(c.body(), `id="order-status-controls"`) || !strings.Contains(c.body(), `data-code="111222333"`) {
		t.Fatalf("badge response missing controls/updated code: %s", c.body())
	}
}

// TestAdminStatusEndpointStoresTrackingCode drives both admin status endpoints
// (the orders-table select and the order-detail badge controls) and asserts:
//   - moving to dispatched with a postal tracking code stores it,
//   - moving to dispatched with an empty code stores empty,
//   - the success responses render the tracking input back to the admin,
//   - a non-dispatched transition ignores the submitted code,
//   - an over-long code is rejected with 400.
func TestAdminStatusEndpointStoresTrackingCode(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	orderID := createOrderForTest(t, h) // pending

	// pending → preparing (code ignored), then → dispatched with a code.
	if resp := c.post("/admin/orders/"+orderID+"/status", url.Values{"status": {"preparing"}, "tracking_code": {"TYPED-EARLY"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("preparing = %d", resp.StatusCode)
	}
	resp := c.post("/admin/orders/"+orderID+"/status", url.Values{"status": {"dispatched"}, "tracking_code": {"2468013579"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dispatched = %d", resp.StatusCode)
	}
	var stored string
	if err := h.db.QueryRow("SELECT tracking_code FROM orders WHERE id = ?", orderID).Scan(&stored); err != nil {
		t.Fatalf("read tracking_code: %v", err)
	}
	if stored != "2468013579" {
		t.Fatalf("tracking_code = %q, want 2468013579", stored)
	}
	// The re-rendered <td> carries the code in the edit button's data attribute.
	if !strings.Contains(c.body(), `data-code="2468013579"`) {
		t.Fatalf("dispatch response missing tracking edit button: %s", c.body())
	}

	// Editing the code on a dispatched order via the same-status select.
	resp = c.post("/admin/orders/"+orderID+"/status", url.Values{"status": {"dispatched"}, "tracking_code": {"111222333"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dispatched edit = %d", resp.StatusCode)
	}
	if err := h.db.QueryRow("SELECT tracking_code FROM orders WHERE id = ?", orderID).Scan(&stored); err != nil {
		t.Fatalf("read tracking_code: %v", err)
	}
	if stored != "111222333" {
		t.Fatalf("tracking_code after edit = %q, want 111222333", stored)
	}

	// Badge endpoint (order-detail page) with an empty code stores empty.
	resp = c.post("/admin/orders/"+orderID+"/status-badge", url.Values{"status": {"dispatched"}, "tracking_code": {"  "}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("badge dispatched empty code = %d", resp.StatusCode)
	}
	if err := h.db.QueryRow("SELECT tracking_code FROM orders WHERE id = ?", orderID).Scan(&stored); err != nil {
		t.Fatalf("read tracking_code: %v", err)
	}
	if stored != "" {
		t.Fatalf("tracking_code after empty badge POST = %q, want empty", stored)
	}
	if !strings.Contains(c.body(), `js-edit-tracking`) || !strings.Contains(c.body(), `data-code=""`) {
		t.Fatalf("badge response missing tracking edit button: %s", c.body())
	}

	// Over-long code is rejected before any DB write.
	long := strings.Repeat("x", 65)
	resp = c.post("/admin/orders/"+orderID+"/status-badge", url.Values{"status": {"dispatched"}, "tracking_code": {long}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("badge over-long code = %d, want 400", resp.StatusCode)
	}
}
