package handlers

import (
	"database/sql"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"farmstore/internal/database"
	"farmstore/internal/payment"
)

// product IDs from the standard seed catalogue.
const (
	seedProductFig  = int64(1) // انجیر تازه، ۱٬۲۹۹٬۰۰۰ تومان، stock 50
	seedProductPome = int64(2) // انار تازه، ۸۹۹٬۰۰۰ تومان، stock 60
	seedProductJam  = int64(3) // مربای انجیر، stock 30
)

// TestUserJourneyEndToEnd walks a real customer through the whole flow over the
// fully-wired router: browse storefront → add to cart → OTP login → checkout →
// place order with stock reservation → Zarinpal gateway → payment verification →
// confirmation → order history.
func TestUserJourneyEndToEnd(t *testing.T) {
	r, h, gw := newTestRouter(t)
	c := newTestClient(t, r)

	// ── Storefront ──────────────────────────────────────────
	resp := c.get("/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d", resp.StatusCode)
	}
	if !strings.Contains(c.body(), "INDEX-PAGE") {
		t.Fatalf("home body missing marker: %q", c.body())
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security header missing on home")
	}

	resp = c.get("/products/summer")
	if resp.StatusCode != http.StatusOK || !strings.Contains(c.body(), "PRODUCTS-PAGE") {
		t.Fatalf("products/summer = %d %q", resp.StatusCode, c.body())
	}
	resp = c.get("/products/unknown")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown category = %d, want 404", resp.StatusCode)
	}
	resp = c.get("/about")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /about = %d", resp.StatusCode)
	}

	// ── Cart ─────────────────────────────────────────────────
	if !c.hasCookie("csrf_token") {
		t.Fatal("no csrf cookie after first page view")
	}
	if c.csrf() == "" {
		t.Fatal("no csrf token in page meta")
	}

	c.addToCart(t, seedProductFig)
	c.addToCart(t, seedProductPome)
	c.addToCart(t, seedProductFig)

	resp = c.get("/cart/count")
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(c.body()) != "۳" {
		t.Fatalf("cart count = %q, want ۳", c.body())
	}

	// update: +1 fig, −1 pomegranate (removes it), invalid delta rejected
	resp = c.post("/cart/update", url.Values{"product_id": {"1"}, "delta": {"1"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cart/update +1 = %d", resp.StatusCode)
	}
	resp = c.post("/cart/update", url.Values{"product_id": {"2"}, "delta": {"-1"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cart/update -1 = %d", resp.StatusCode)
	}
	resp = c.post("/cart/update", url.Values{"product_id": {"2"}, "delta": {"5"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("cart/update invalid delta = %d, want 400", resp.StatusCode)
	}

	resp = c.get("/cart")
	if resp.StatusCode != http.StatusOK || !strings.Contains(c.body(), "CART-PAGE") {
		t.Fatalf("GET /cart = %d %q", resp.StatusCode, c.body())
	}

	// ── Auth guard: checkout requires login ──────────────────
	resp = c.get("/checkout")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /checkout unauth = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "/login") {
		t.Fatalf("checkout redirect = %q, want login", loc)
	}
	resp = c.get("/orders")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /orders unauth = %d, want 303", resp.StatusCode)
	}

	// ── OTP login ────────────────────────────────────────────
	phone := "09121234567"
	resp = c.post("/auth/send-otp", url.Values{"phone": {phone}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send-otp = %d", resp.StatusCode)
	}
	// Invalid phone must not create a user or reach the SMS step.
	bad := c.post("/auth/send-otp", url.Values{"phone": {"12345"}})
	if bad.StatusCode != http.StatusOK || strings.Contains(c.body(), "ارسال شد") {
		t.Fatalf("invalid phone send-otp unexpectedly succeeded")
	}
	if _, err := database.GetUserByPhone(h.db, "12345"); err == nil {
		t.Fatal("invalid phone created a user")
	}

	c.login(t, h.db, phone)

	resp = c.get("/checkout")
	if resp.StatusCode != http.StatusOK || !strings.Contains(c.body(), "CHECKOUT-PAGE") {
		t.Fatalf("logged-in /checkout = %d %q", resp.StatusCode, c.body())
	}
	if !c.hasCookie("session") {
		t.Fatal("session cookie not set after login")
	}

	// ── Checkout step 2 preview ──────────────────────────────
	ship := validShipment()
	resp = c.post("/checkout/preview", ship)
	if resp.StatusCode != http.StatusOK || !strings.Contains(c.body(), "CHECKOUT-PAGE") {
		t.Fatalf("checkout preview = %d %q", resp.StatusCode, c.body())
	}

	// Invalid shipping info → 400 with the form shown again.
	badShip := url.Values{"name": {"ر"}, "phone": {"12345"}, "address": {"x"}, "postal_code": {"0"}}
	resp = c.post("/checkout/preview", badShip)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad checkout preview = %d, want 400", resp.StatusCode)
	}

	// ── Place order (stock reservation + gateway redirect) ───
	stockBefore := productStock(t, h.db, seedProductFig)
	resp = c.post("/checkout", ship)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("place order = %d (body: %s)", resp.StatusCode, c.body())
	}
	order := lastOrder(t, h.db)
	if order.Status != "awaiting_payment" {
		t.Fatalf("order status = %q, want awaiting_payment", order.Status)
	}
	if order.TotalAmount != 3*1299000 {
		t.Fatalf("order total = %d, want %d", order.TotalAmount, 3*1299000)
	}
	gotAmt, hits, auth := gw.snapshot()
	if hits != 1 || auth == "" {
		t.Fatalf("gateway/request hits=%d authority=%q", hits, auth)
	}
	if gotAmt != order.TotalAmount*payment.RialPerToman {
		t.Fatalf("gateway amount = %d, want %d", gotAmt, order.TotalAmount*payment.RialPerToman)
	}
	if order.PaymentAuthority != auth {
		t.Fatalf("order authority = %q, want %q", order.PaymentAuthority, auth)
	}
	if got := productStock(t, h.db, seedProductFig); got != stockBefore-3 {
		t.Fatalf("stock after order = %d, want %d", got, stockBefore-3)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, gw.server.URL+"/pg/StartPay/") {
		t.Fatalf("gateway redirect = %q", loc)
	}

	// ── Payment callback (success) ───────────────────────────
	resp = c.get("/checkout/verify?Authority=" + auth + "&Status=OK")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("verify callback = %d", resp.StatusCode)
	}
	confLoc := resp.Header.Get("Location")
	if !strings.HasPrefix(confLoc, "/checkout/confirmation/") {
		t.Fatalf("success redirect = %q", confLoc)
	}
	orderID := strings.TrimPrefix(confLoc, "/checkout/confirmation/")

	order = lastOrder(t, h.db)
	if order.Status != "pending" {
		t.Fatalf("order status after payment = %q, want pending", order.Status)
	}
	if order.PaymentRefID != 1002003004005 {
		t.Fatalf("payment ref id = %d, want 1002003004005", order.PaymentRefID)
	}

	// ── Confirmation page (owner only) ───────────────────────
	resp = c.get(confLoc)
	if resp.StatusCode != http.StatusOK || !strings.Contains(c.body(), "id="+orderID) {
		t.Fatalf("confirmation = %d %q", resp.StatusCode, c.body())
	}

	// ── Order history ────────────────────────────────────────
	resp = c.get("/orders")
	if resp.StatusCode != http.StatusOK || !strings.Contains(c.body(), orderID) {
		t.Fatalf("orders page missing order %q: %d %q", orderID, resp.StatusCode, c.body())
	}

	// ── IDOR: another user must not see this order ───────────
	c2 := newTestClient(t, r)
	c2.login(t, h.db, "09139998877")
	resp = c2.get(confLoc)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("IDOR: other user sees confirmation = %d, want 404", resp.StatusCode)
	}
	resp = c2.get("/orders")
	if strings.Contains(c2.body(), orderID) {
		t.Fatalf("IDOR: other user sees order in history")
	}

	// ── Logout ───────────────────────────────────────────────
	resp = c.get("/logout")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout = %d", resp.StatusCode)
	}
	resp = c.get("/orders")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("orders after logout = %d, want redirect", resp.StatusCode)
	}
}

// TestPaymentCancelledCallback ensures a customer who abandons the gateway
// (Status != OK) gets the order cancelled and stock restored.
func TestPaymentCancelledCallback(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.login(t, h.db, "09121234567")
	c.addToCart(t, seedProductPome)
	stockBefore := productStock(t, h.db, seedProductPome)

	resp := c.post("/checkout", validShipment())
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("place order = %d", resp.StatusCode)
	}
	order := lastOrder(t, h.db)
	if order.Status != "awaiting_payment" {
		t.Fatalf("order status = %q", order.Status)
	}

	resp = c.get("/checkout/verify?Authority=" + order.PaymentAuthority + "&Status=FAIL")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/cart" {
		t.Fatalf("cancelled callback = %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	order = lastOrder(t, h.db)
	if order.Status != "cancelled" {
		t.Fatalf("order status after cancel = %q, want cancelled", order.Status)
	}
	if got := productStock(t, h.db, seedProductPome); got != stockBefore {
		t.Fatalf("stock after cancel = %d, want %d", got, stockBefore)
	}
}

// TestPaymentGatewayRejectsVerification covers the gateway returning a failure
// code during verification: the order is cancelled, stock restored, and the
// user is bounced back to the cart.
func TestPaymentGatewayRejectsVerification(t *testing.T) {
	r, h, gw := newTestRouter(t)
	c := newTestClient(t, r)
	c.login(t, h.db, "09121234567")
	c.addToCart(t, seedProductJam)
	stockBefore := productStock(t, h.db, seedProductJam)

	resp := c.post("/checkout", validShipment())
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("place order = %d", resp.StatusCode)
	}
	order := lastOrder(t, h.db)

	gw.setVerifyCode(102)
	resp = c.get("/checkout/verify?Authority=" + order.PaymentAuthority + "&Status=OK")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("verify = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/cart?error=payment_failed" {
		t.Fatalf("failed verify redirect = %q", loc)
	}

	order = lastOrder(t, h.db)
	if order.Status != "cancelled" {
		t.Fatalf("order status = %q, want cancelled", order.Status)
	}
	if got := productStock(t, h.db, seedProductJam); got != stockBefore {
		t.Fatalf("stock after failed verify = %d, want %d", got, stockBefore)
	}
}

// TestPlaceOrderGatewayRequestFails verifies that when the payment gateway
// itself is unreachable the order is cancelled with stock restored and the user
// is shown a gateway error, not silently left with a dangling order.
func TestPlaceOrderGatewayRequestFails(t *testing.T) {
	h, _ := newTestHandler(t)
	// Point the payment client at a closed port so RequestPayment fails fast.
	h.zarinpal = payment.NewTestClient("merchant",
		"http://127.0.0.1:1/request",
		"http://127.0.0.1:1/verify",
		"http://127.0.0.1:1/pg/",
		&http.Client{Timeout: time.Second})
	r := routerFor(h)
	c := newTestClient(t, r)

	c.login(t, h.db, "09121234567")
	c.addToCart(t, seedProductFig)

	resp := c.post("/checkout", validShipment())
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("place order with dead gateway = %d, want 502 (body: %.60s)", resp.StatusCode, c.body())
	}

	order := lastOrder(t, h.db)
	if order.Status != "cancelled" {
		t.Fatalf("order status = %q, want cancelled", order.Status)
	}
	if got := productStock(t, h.db, seedProductFig); got != 50 {
		t.Fatalf("stock after dead gateway = %d, want 50", got)
	}
}

// TestCheckoutEdgeCases exercises empty-cart and out-of-stock ordering paths:
// both must fail without creating an order or overselling.
func TestCheckoutEdgeCases(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)

	c.login(t, h.db, "09121234567")

	// Empty cart places no order — must redirect to /cart.
	resp := c.post("/checkout", validShipment())
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/cart" {
		t.Fatalf("empty-cart checkout = %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if n := countOrders(t, h.db); n != 0 {
		t.Fatalf("empty-cart created %d orders", n)
	}

	// Add to cart then drop stock to 0: the cart is refreshed at checkout so
	// the item is dropped and the user is sent back instead of overselling.
	c.addToCart(t, seedProductFig)
	if _, err := h.db.Exec("UPDATE products SET stock_quantity = 0 WHERE id = ?", seedProductFig); err != nil {
		t.Fatal(err)
	}
	resp = c.post("/checkout", validShipment())
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/cart" {
		t.Fatalf("zero-stock checkout = %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if n := countOrders(t, h.db); n != 0 {
		t.Fatalf("zero-stock checkout created %d orders", n)
	}
}

func countOrders(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM orders").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func validShipment() url.Values {
	return url.Values{
		"name":        {"رضا محمدی"},
		"phone":       {"09121234567"},
		"address":     {"تهران، خیابان بهار، کوچه ۱۲، پلاک ۴"},
		"postal_code": {"1234567890"},
	}
}
