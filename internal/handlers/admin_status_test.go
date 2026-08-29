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
	if err := database.UpdateOrderStatus(context.Background(), h.db, orderID, "dispatched"); err == nil {
		t.Fatal("cancelled → dispatched accepted by DB")
	}
}

// TestAdminOrderDetailStatusControls checks the order-detail page renders a
// forward-only status control and that the badge endpoint's response keeps the
// controls container (badge + select) intact.
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
	if strings.Contains(strings.SplitN(c.body(), `id="order-status-controls"`, 2)[1], `value="awaiting_payment"`) {
		t.Fatal("pending order offers backward move to awaiting_payment")
	}

	// Forward move via the badge endpoint replaces the whole controls block.
	resp := c.post("/admin/orders/"+orderID+"/status-badge", url.Values{"status": {"preparing"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("badge preparing = %d", resp.StatusCode)
	}
	if !strings.Contains(c.body(), `id="order-status-controls"`) || !strings.Contains(c.body(), `selected value="preparing"`) && !strings.Contains(c.body(), `value="preparing" selected`) {
		t.Fatalf("badge response missing controls/preparing selection: %s", c.body())
	}

	// Backward via the badge endpoint is rejected with the toast header.
	resp = c.post("/admin/orders/"+orderID+"/status-badge", url.Values{"status": {"pending"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("badge preparing → pending = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("HX-Trigger"), "adminToast") {
		t.Fatalf("badge rejected transition missing adminToast, got %q", resp.Header.Get("HX-Trigger"))
	}
}
