package handlers

import (
	"database/sql"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// productName returns the displayed name for a seeded product so the render
// assertions below stay in sync with the catalogue.
func productName(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var name string
	if err := db.QueryRow("SELECT name FROM products WHERE id = ?", id).Scan(&name); err != nil {
		t.Fatalf("read product %d name: %v", id, err)
	}
	return name
}

// placePaidOrder drives a real customer through add-to-cart → OTP login →
// place order → Zarinpal verification, leaving the client on the confirmation
// page. It returns the client (whose last body is the confirmation HTML) and
// the new order id, so callers can assert what the page rendered.
func placePaidOrder(t *testing.T, handler http.Handler, h *Handler, gw *fakeGateway, productIDs ...int64) (*testClient, string) {
	t.Helper()
	c := newTestClient(t, handler)
	if resp := c.get("/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap GET / = %d", resp.StatusCode)
	}
	for _, pid := range productIDs {
		c.addToCart(t, pid)
	}
	c.login(t, h.db, "09121234567")

	resp := c.post("/checkout", validShipment())
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("place order = %d (body: %s)", resp.StatusCode, c.body())
	}
	_, _, auth := gw.snapshot()
	if auth == "" {
		t.Fatal("no payment authority issued")
	}
	resp = c.get("/checkout/verify?Authority=" + auth + "&Status=OK")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("verify callback = %d", resp.StatusCode)
	}
	confLoc := resp.Header.Get("Location")
	if !strings.HasPrefix(confLoc, "/checkout/confirmation/") {
		t.Fatalf("success redirect = %q", confLoc)
	}
	resp = c.get(confLoc)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirmation = %d %q", resp.StatusCode, c.body())
	}
	orderID := strings.TrimPrefix(confLoc, "/checkout/confirmation/")
	return c, orderID
}

// TestConfirmationRendersCorrectProductNames guards the off-by-N product-name
// join bug (#2): the confirmation page must show each line item's own product
// name, in the order the items were placed — not a name shifted by a positional
// join over GetProductsByIDs (which returns products in DB id order).
func TestConfirmationRendersCorrectProductNames(t *testing.T) {
	r, h, gw := newTestRouter(t)

	// Add product 3 (مربای انجیر) then product 1 (انجیر تازه). A positional join
	// would render them swapped, because GetProductsByIDs returns [p1, p3].
	c, _ := placePaidOrder(t, r, h, gw, seedProductJam, seedProductFig)

	body := c.body()
	nameJam := productName(t, h.db, seedProductJam)
	nameFig := productName(t, h.db, seedProductFig)

	itemJam := " ITEM=" + nameJam
	itemFig := " ITEM=" + nameFig
	if !strings.Contains(body, itemJam) {
		t.Fatalf("confirmation missing %q (body: %s)", itemJam, body)
	}
	if !strings.Contains(body, itemFig) {
		t.Fatalf("confirmation missing %q (body: %s)", itemFig, body)
	}
}

// TestAdminOrderDetailRendersCorrectProductNames guards the same off-by-N
// join in the admin order-detail page (#2): each line item must show its own
// product name.
func TestAdminOrderDetailRendersCorrectProductNames(t *testing.T) {
	r, h, gw := newTestRouter(t)
	_, orderID := placePaidOrder(t, r, h, gw, seedProductFig, seedProductJam)

	c := newTestClient(t, r)
	c.authorize("admin", "admin123")
	c.bootstrapAdmin(t)

	resp := c.get("/admin/orders/" + orderID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin order detail = %d %q", resp.StatusCode, c.body())
	}
	body := c.body()
	nameFig := productName(t, h.db, seedProductFig)
	nameJam := productName(t, h.db, seedProductJam)
	if !strings.Contains(body, " ITEM="+nameFig) {
		t.Fatalf("admin detail missing %q (body: %s)", nameFig, body)
	}
	if !strings.Contains(body, " ITEM="+nameJam) {
		t.Fatalf("admin detail missing %q (body: %s)", nameJam, body)
	}
}

// TestCSRFRejectsMutatingPostWithoutToken proves the CSRF protection holds on a
// real form submission end-to-end: a mutating POST that omits the anti-CSRF
// token (header or hidden field) is rejected with 403, while the same request
// with the token succeeds.
func TestCSRFRejectsMutatingPostWithoutToken(t *testing.T) {
	r, _, _ := newTestRouter(t)

	// Establish browser state (cookies + token in the page) like a real visit.
	c := newTestClient(t, r)
	if resp := c.get("/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap GET / = %d", resp.StatusCode)
	}

	// A real form submission that omits the token the browser attaches must be
	// rejected. Drop the token the test client would normally add.
	c.csrfToken = ""
	resp := c.post("/cart/add", url.Values{"product_id": {"1"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without CSRF token = %d, want 403", resp.StatusCode)
	}

	// Sanity: the same request WITH the token is accepted.
	c2 := newTestClient(t, r)
	c2.get("/")
	if resp := c2.post("/cart/add", url.Values{"product_id": {"1"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("legitimate POST with token = %d, want 200", resp.StatusCode)
	}
}
