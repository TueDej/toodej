package handlers

import (
	"context"
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

	resp = c.get("/products/test")
	if resp.StatusCode != http.StatusOK || !strings.Contains(c.body(), "PRODUCTS-PAGE") {
		t.Fatalf("products/test = %d %q", resp.StatusCode, c.body())
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
	if _, err := database.GetUserByPhone(context.Background(), h.db, "12345"); err == nil {
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

	// Add to cart then drop stock to 0: refreshCartFromProducts removes the
	// unavailable item, and PlaceOrder rejects the order (409, "stock changed")
	// instead of silently redirecting — no order is created and nothing is oversold.
	c.addToCart(t, seedProductFig)
	if _, err := h.db.Exec("UPDATE products SET stock_quantity = 0 WHERE id = ?", seedProductFig); err != nil {
		t.Fatal(err)
	}
	resp = c.post("/checkout", validShipment())
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("zero-stock checkout = %d -> %q, want 409", resp.StatusCode, resp.Header.Get("Location"))
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

// TestResumePayment lets the owner of an awaiting_payment order retry payment:
// a fresh authority is requested, stored on the order, and the user is sent to
// the cashier. Another user (non-owner) gets a 404.
func TestResumePayment(t *testing.T) {
	r, h, gw := newTestRouter(t)
	c := newTestClient(t, r)
	c.login(t, h.db, "09121234567")
	c.addToCart(t, seedProductFig)

	resp := c.post("/checkout", validShipment())
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("place order = %d", resp.StatusCode)
	}
	order := lastOrder(t, h.db)
	if order.Status != "awaiting_payment" {
		t.Fatalf("order status = %q, want awaiting_payment", order.Status)
	}

	// Resuming mutates the order, so the route must be CSRF-protected: a POST
	// that omits the token the browser attaches is rejected before the handler
	// runs, and never reaches the gateway.
	_, hitsNoToken, _ := gw.snapshot()
	token := c.csrf()
	c.csrfToken = ""
	if resp := c.post("/orders/"+order.ID+"/pay", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("resume without CSRF token = %d, want 403", resp.StatusCode)
	}
	if _, hits, _ := gw.snapshot(); hits != hitsNoToken {
		t.Fatalf("token-less resume reached the gateway (hits %d)", hits)
	}
	c.csrfToken = token

	_, hitsBefore, _ := gw.snapshot()
	resp = c.post("/orders/"+order.ID+"/pay", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("resume payment = %d", resp.StatusCode)
	}
	_, hits, auth := gw.snapshot()
	if hits != hitsBefore+1 {
		t.Fatalf("gateway request hits = %d, want %d", hits, hitsBefore+1)
	}
	if loc := resp.Header.Get("Location"); loc != gw.server.URL+"/pg/StartPay/"+auth {
		t.Fatalf("resume redirect = %q", loc)
	}

	order = lastOrder(t, h.db)
	if order.PaymentAuthority != auth {
		t.Fatalf("order authority = %q, want %q", order.PaymentAuthority, auth)
	}

	// IDOR: another user cannot resume this order.
	c2 := newTestClient(t, r)
	c2.login(t, h.db, "09139998877")
	resp = c2.post("/orders/"+order.ID+"/pay", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("IDOR resume = %d, want 404", resp.StatusCode)
	}

	// A paid order cannot be resumed (no new gateway hit).
	resp = c.get("/checkout/verify?Authority=" + auth + "&Status=OK")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("verify callback = %d", resp.StatusCode)
	}
	_, hitsAfter, _ := gw.snapshot()
	resp = c.post("/orders/"+order.ID+"/pay", nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/orders" {
		t.Fatalf("resume paid order = %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, hitsFinal, _ := gw.snapshot(); hitsFinal != hitsAfter {
		t.Fatalf("paid order caused a new gateway request (hits %d)", hitsFinal)
	}
}

// TestPaymentReconciliation rescues an awaiting_payment order whose successful
// payment callback was lost: reconciliation asks the gateway, sees the payment
// succeeded, and moves the order to pending instead of letting the janitor
// cancel it.
func TestPaymentReconciliation(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.login(t, h.db, "09121234567")
	c.addToCart(t, seedProductFig)

	resp := c.post("/checkout", validShipment())
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("place order = %d", resp.StatusCode)
	}
	order := lastOrder(t, h.db)
	if order.Status != "awaiting_payment" {
		t.Fatalf("order status = %q, want awaiting_payment", order.Status)
	}

	h.reconcilePayments()

	order = lastOrder(t, h.db)
	if order.Status != "pending" {
		t.Fatalf("order status after reconciliation = %q, want pending", order.Status)
	}
	if order.PaymentRefID != 1002003004005 {
		t.Fatalf("payment ref id = %d, want 1002003004005", order.PaymentRefID)
	}
}

// TestPaymentReconciliationLeavesUnpaidOrdersAlone ensures reconciliation never
// confirms an order the gateway reports as unpaid; those are still left for the
// unpaid-order janitor to cancel.
func TestPaymentReconciliationLeavesUnpaidOrdersAlone(t *testing.T) {
	r, h, gw := newTestRouter(t)
	c := newTestClient(t, r)
	c.login(t, h.db, "09121234567")
	c.addToCart(t, seedProductFig)

	resp := c.post("/checkout", validShipment())
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("place order = %d", resp.StatusCode)
	}
	order := lastOrder(t, h.db)

	gw.setVerifyCode(102) // gateway reports the payment never happened
	h.reconcilePayments()

	order = lastOrder(t, h.db)
	if order.Status != "awaiting_payment" {
		t.Fatalf("order status = %q, want awaiting_payment", order.Status)
	}
}

// setCartQuantity inflates a cart line directly (bypassing the stock-capped add
// path) so tests can simulate a cart holding more units than remain in stock —
// the over-stock race this suite guards against.
func setCartQuantity(t *testing.T, h *Handler, c *testClient, productID int64, qty int) {
	t.Helper()
	sid, ok := c.cookies["session"]
	if !ok || sid.Value == "" {
		t.Fatal("no session cookie to locate cart")
	}
	cart := h.cartStore.Get(sid.Value)
	for i := range cart.Items {
		if cart.Items[i].ProductID == productID {
			cart.Items[i].Quantity = qty
			return
		}
	}
	t.Fatalf("product %d not in cart", productID)
}

// TestCheckoutOverStockStep1Warns verifies that when a cart line exceeds the
// remaining stock, the step-1 checkout form proactively surfaces an over-stock
// warning instead of letting the user reach the payment button unaware.
func TestCheckoutOverStockStep1Warns(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.login(t, h.db, "09121234567")
	c.addToCart(t, seedProductFig)

	// Drop the remaining stock below the cart quantity, then inflate the cart.
	if _, err := h.db.Exec("UPDATE products SET stock_quantity = 2 WHERE id = ?", seedProductFig); err != nil {
		t.Fatal(err)
	}
	setCartQuantity(t, h, c, seedProductFig, 5)

	resp := c.get("/checkout")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /checkout = %d", resp.StatusCode)
	}
	if !strings.Contains(c.body(), "OVERSTOCK=") {
		t.Fatalf("step 1 missing overstock warning: %q", c.body())
	}
	if !strings.Contains(c.body(), ":2;") {
		t.Fatalf("step 1 overstock warning missing available count: %q", c.body())
	}
}

// TestCheckoutOverStockPreviewWarns verifies the same proactive warning appears
// on step 2 (order review) before the user clicks "pay".
func TestCheckoutOverStockPreviewWarns(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.login(t, h.db, "09121234567")
	c.addToCart(t, seedProductFig)

	if _, err := h.db.Exec("UPDATE products SET stock_quantity = 2 WHERE id = ?", seedProductFig); err != nil {
		t.Fatal(err)
	}
	setCartQuantity(t, h, c, seedProductFig, 5)

	resp := c.post("/checkout/preview", validShipment())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preview = %d", resp.StatusCode)
	}
	if !strings.Contains(c.body(), "OVERSTOCK=") {
		t.Fatalf("preview missing overstock warning: %q", c.body())
	}
}

// TestCheckoutOverStockRejectedAtPlaceOrder ensures that even if a user ignores
// the warning and submits, PlaceOrder refuses the order (409) rather than
// overselling — closing the race between viewing the cart and paying.
func TestCheckoutOverStockRejectedAtPlaceOrder(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.login(t, h.db, "09121234567")
	c.addToCart(t, seedProductFig)

	if _, err := h.db.Exec("UPDATE products SET stock_quantity = 2 WHERE id = ?", seedProductFig); err != nil {
		t.Fatal(err)
	}
	setCartQuantity(t, h, c, seedProductFig, 5)

	resp := c.post("/checkout", validShipment())
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("overstock place order = %d -> %q, want 409 (body: %s)", resp.StatusCode, resp.Header.Get("Location"), c.body())
	}
	if !strings.Contains(c.body(), "OVERSTOCK=") {
		t.Fatalf("rejection missing overstock warning: %q", c.body())
	}
	if n := countOrders(t, h.db); n != 0 {
		t.Fatalf("overstock checkout created %d orders", n)
	}
	if got := productStock(t, h.db, seedProductFig); got != 2 {
		t.Fatalf("stock changed despite rejected order: %d, want 2", got)
	}
}

// TestCheckoutOverStockTwoUsers is the integration case: user 1 reserves stock
// first, leaving less than user 2's cart quantity. User 2's preview must still
// warn about the over-stock line even though user 2 never saw a stale "in stock"
// banner.
func TestCheckoutOverStockTwoUsers(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c1 := newTestClient(t, r)
	c1.login(t, h.db, "09121234567")
	c1.addToCart(t, seedProductFig)

	// User 1 places an order, reserving 1 unit of stock.
	stockBefore := productStock(t, h.db, seedProductFig)
	resp := c1.post("/checkout", validShipment())
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("user1 place order = %d", resp.StatusCode)
	}
	if got := productStock(t, h.db, seedProductFig); got != stockBefore-1 {
		t.Fatalf("user1 stock after order = %d, want %d", got, stockBefore-1)
	}

	// User 2 adds the same product and inflates the cart beyond what remains.
	c2 := newTestClient(t, r)
	c2.login(t, h.db, "09139998877")
	c2.addToCart(t, seedProductFig)
	remaining := productStock(t, h.db, seedProductFig)
	setCartQuantity(t, h, c2, seedProductFig, remaining+5)

	resp = c2.post("/checkout/preview", validShipment())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("user2 preview = %d", resp.StatusCode)
	}
	if !strings.Contains(c2.body(), "OVERSTOCK=") {
		t.Fatalf("user2 preview missing overstock warning: %q", c2.body())
	}
}

// TestVerifyPaymentNoFalseSuccessWhenConfirmFails ensures that when the gateway
// reports a successful payment but the order can no longer be confirmed (e.g. it
// was cancelled before the callback arrived), VerifyPayment does NOT show a
// "payment successful" confirmation. Showing success would lie to a customer
// whose order is actually cancelled — and whose stock was already handed to
// another shopper.
func TestVerifyPaymentNoFalseSuccessWhenConfirmFails(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.login(t, h.db, "09121234567")
	c.addToCart(t, seedProductFig)

	resp := c.post("/checkout", validShipment())
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("place order = %d", resp.StatusCode)
	}
	order := lastOrder(t, h.db)
	if order.Status != "awaiting_payment" || order.PaymentAuthority == "" {
		t.Fatalf("order not awaiting/authority: %+v", order)
	}

	// Simulate the order having been cancelled (e.g. abandoned) before the
	// gateway callback reaches us.
	if _, err := h.db.Exec("UPDATE orders SET status = 'cancelled' WHERE id = ?", order.ID); err != nil {
		t.Fatal(err)
	}

	// Gateway says paid, but confirmation cannot attach to a cancelled order.
	resp = c.get("/checkout/verify?Authority=" + order.PaymentAuthority + "&Status=OK")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("verify = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/cart?error=payment_failed" {
		t.Fatalf("verify redirected to %q, want /cart?error=payment_failed", loc)
	}
}

// TestUnpaidJanitorKeepsPaidOrders verifies the unpaid-order janitor reconciles
// (asks the gateway) before cancelling. A customer who paid but whose callback
// was delayed keeps their order and its reserved stock, instead of losing it —
// which would otherwise free the stock for a later shopper while the original
// customer is shown a successful payment.
func TestUnpaidJanitorKeepsPaidOrders(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	c.login(t, h.db, "09121234567")
	c.addToCart(t, seedProductFig)
	stockBefore := productStock(t, h.db, seedProductFig)

	resp := c.post("/checkout", validShipment())
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("place order = %d", resp.StatusCode)
	}
	order := lastOrder(t, h.db)
	if order.PaymentAuthority == "" {
		t.Fatal("order has no authority")
	}

	// Age the order past the TTL so it looks abandoned; the fake gateway still
	// reports it as paid.
	if _, err := h.db.Exec("UPDATE orders SET created_at = datetime('now','-20 minutes') WHERE id = ?", order.ID); err != nil {
		t.Fatal(err)
	}

	h.cancelExpiredUnpaidOrders()

	o, err := database.GetOrder(context.Background(), h.db, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != "pending" {
		t.Fatalf("paid expired order status = %q, want pending", o.Status)
	}
	if got := productStock(t, h.db, seedProductFig); got != stockBefore-1 {
		t.Fatalf("stock after janitor = %d, want %d (stock must stay reserved)", got, stockBefore-1)
	}
}

// TestUnpaidJanitorCancelsUnpaid verifies that an expired order the gateway
// confirms was never paid is still cancelled and its stock restored.
func TestUnpaidJanitorCancelsUnpaid(t *testing.T) {
	r, h, gw := newTestRouter(t)
	c := newTestClient(t, r)
	c.login(t, h.db, "09121234567")
	c.addToCart(t, seedProductPome)
	stockBefore := productStock(t, h.db, seedProductPome)

	resp := c.post("/checkout", validShipment())
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("place order = %d", resp.StatusCode)
	}
	order := lastOrder(t, h.db)

	gw.setVerifyCode(102) // gateway reports the payment never happened
	if _, err := h.db.Exec("UPDATE orders SET created_at = datetime('now','-20 minutes') WHERE id = ?", order.ID); err != nil {
		t.Fatal(err)
	}

	h.cancelExpiredUnpaidOrders()

	o, err := database.GetOrder(context.Background(), h.db, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != "cancelled" {
		t.Fatalf("unpaid expired order status = %q, want cancelled", o.Status)
	}
	if got := productStock(t, h.db, seedProductPome); got != stockBefore {
		t.Fatalf("stock after cancel = %d, want %d", got, stockBefore)
	}
}

// TestVerifyTransportErrorKeepsOrderAwaitingPayment ensures that when the
// gateway cannot give an authoritative verify answer (timeout, 5xx, garbage
// payload) AFTER the customer may have paid, the order is NOT cancelled and its
// stock is NOT restored — otherwise a paid charge would be left with no
// recoverable order. The order stays awaiting_payment so the payment
// reconciler can rescue it, and the customer is sent to /orders.
func TestVerifyTransportErrorKeepsOrderAwaitingPayment(t *testing.T) {
	r, h, gw := newTestRouter(t)
	c := newTestClient(t, r)
	c.login(t, h.db, "09121234567")
	c.addToCart(t, seedProductFig)
	stockBefore := productStock(t, h.db, seedProductFig)

	resp := c.post("/checkout", validShipment())
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("place order = %d", resp.StatusCode)
	}
	order := lastOrder(t, h.db)

	// Point only the verify endpoint at a dead port: the payment request step
	// already succeeded, but verification now fails as a transport error.
	h.zarinpal = payment.NewTestClient("merchant",
		gw.server.URL+"/request",
		"http://127.0.0.1:1/verify",
		gw.server.URL+"/pg/StartPay/",
		&http.Client{Timeout: time.Second})

	resp = c.get("/checkout/verify?Authority=" + order.PaymentAuthority + "&Status=OK")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("verify transport error = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/orders" {
		t.Fatalf("verify transport error redirect = %q, want /orders", loc)
	}

	order = lastOrder(t, h.db)
	if order.Status != "awaiting_payment" {
		t.Fatalf("order status = %q, want awaiting_payment (must not cancel on inconclusive verify)", order.Status)
	}
	if got := productStock(t, h.db, seedProductFig); got != stockBefore-1 {
		t.Fatalf("stock after inconclusive verify = %d, want %d (reservation must be kept)", got, stockBefore-1)
	}

	// The reconciler must be able to rescue the order once the gateway answers
	// definitively: restore a working gateway client and reconcile.
	h.zarinpal = gw.client()
	h.reconcilePayments()

	order = lastOrder(t, h.db)
	if order.Status != "pending" {
		t.Fatalf("order status after reconciliation = %q, want pending", order.Status)
	}
	if got := productStock(t, h.db, seedProductFig); got != stockBefore-1 {
		t.Fatalf("stock after reconciliation = %d, want %d", got, stockBefore-1)
	}
}

// TestVerifyTransportErrorUnpaidOrderStillCancelledByJanitor ensures the
// compensating case: if the inconclusive verify was for a genuinely UNPAID
// order, the unpaid-order janitor still reclaims the stock after the TTL.
func TestVerifyTransportErrorUnpaidOrderStillCancelledByJanitor(t *testing.T) {
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

	h.zarinpal = payment.NewTestClient("merchant",
		gw.server.URL+"/request",
		"http://127.0.0.1:1/verify",
		gw.server.URL+"/pg/StartPay/",
		&http.Client{Timeout: time.Second})
	resp = c.get("/checkout/verify?Authority=" + order.PaymentAuthority + "&Status=OK")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("verify transport error = %d", resp.StatusCode)
	}
	if lastOrder(t, h.db).Status != "awaiting_payment" {
		t.Fatal("order should remain awaiting_payment after inconclusive verify")
	}

	// The gateway never actually charged the customer. Age the order past the
	// TTL and run the janitor: it must cancel and restore the stock.
	if _, err := h.db.Exec("UPDATE orders SET created_at = datetime('now', '-1 hour') WHERE id = ?", order.ID); err != nil {
		t.Fatalf("age order: %v", err)
	}
	if _, err := database.CancelExpiredUnpaidOrders(context.Background(), h.db, unpaidOrderTTL); err != nil {
		t.Fatalf("cancel expired unpaid orders: %v", err)
	}

	order = lastOrder(t, h.db)
	if order.Status != "cancelled" {
		t.Fatalf("order status after janitor = %q, want cancelled", order.Status)
	}
	if got := productStock(t, h.db, seedProductJam); got != stockBefore {
		t.Fatalf("stock after janitor = %d, want %d", got, stockBefore)
	}
}
