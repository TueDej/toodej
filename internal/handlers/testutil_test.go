package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"farmstore/internal/database"
	"farmstore/internal/payment"
)

// testDB opens a fresh SQLite database in a temp directory seeded with the
// standard catalogue, so tests start from a known state.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Init(filepath.Join(t.TempDir(), "farmstore.db"))
	if err != nil {
		t.Fatalf("database.Init: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fakeGateway implements the two Zarinpal v4 endpoints the app talks to, so the
// full payment flow runs against an in-process server instead of the internet.
type fakeGateway struct {
	server        *httptest.Server
	mu            sync.Mutex
	authoritySeq  int
	requestedAmt  int // last /request amount (rial)
	verifyCode    int // code returned by /verify (default 100 = success)
	requestHits   int // number of /request calls
	verifyHits    int // number of /verify calls
	lastAuthority string
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	g := &fakeGateway{verifyCode: 100}
	mux := http.NewServeMux()
	mux.HandleFunc("/request", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Amount int `json:"amount"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		g.mu.Lock()
		g.requestHits++
		g.authoritySeq++
		g.lastAuthority = fmt.Sprintf("AUTH%04d", g.authoritySeq)
		g.requestedAmt = req.Amount
		auth := g.lastAuthority
		g.mu.Unlock()
		writeJSON(w, 200, map[string]any{
			"data": map[string]any{"authority": auth, "code": 100, "message": "success"},
		})
	})
	mux.HandleFunc("/verify", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Authority string `json:"authority"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		g.mu.Lock()
		g.verifyHits++
		code := g.verifyCode
		g.mu.Unlock()
		if code == 100 || code == 101 {
			writeJSON(w, 200, map[string]any{
				"data": map[string]any{"code": code, "message": "success", "ref_id": 1002003004005, "card_pan": "6037****1234"},
			})
			return
		}
		writeJSON(w, 200, map[string]any{
			"data": map[string]any{"code": code, "message": "verification failed"},
		})
	})
	g.server = httptest.NewServer(mux)
	t.Cleanup(g.server.Close)
	return g
}

func (g *fakeGateway) client() *payment.Zarinpal {
	return payment.NewTestClient("test-merchant",
		g.server.URL+"/request",
		g.server.URL+"/verify",
		g.server.URL+"/pg/StartPay/",
		g.server.Client())
}

func (g *fakeGateway) setVerifyCode(code int) {
	g.mu.Lock()
	g.verifyCode = code
	g.mu.Unlock()
}

func (g *fakeGateway) snapshot() (amt int, hits int, authority string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requestedAmt, g.requestHits, g.lastAuthority
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// testdataDir points at the bundled test templates dir (relative to internal/handlers).
const testdataDir = "testdata/templates"

// newTestHandler wires a Handler with a test database, test templates, and a
// fake Zarinpal gateway. The templates are validated at construction so a
// template typo fails the test immediately rather than at first render.
func newTestHandler(t *testing.T) (*Handler, *fakeGateway) {
	t.Helper()
	db := testDB(t)

	layoutFiles := []string{filepath.Join(testdataDir, "layout.html")}
	pages := map[string][]string{
		"index":        {filepath.Join(testdataDir, "index.html")},
		"products":     {filepath.Join(testdataDir, "products.html")},
		"about":        {filepath.Join(testdataDir, "about.html")},
		"cart":         {filepath.Join(testdataDir, "cart.html")},
		"checkout":     {filepath.Join(testdataDir, "checkout.html")},
		"confirmation": {filepath.Join(testdataDir, "confirmation.html")},
		"admin":        {filepath.Join(testdataDir, "admin.html")},
		"admin-login":  {filepath.Join(testdataDir, "admin-login.html")},
		"order-detail": {filepath.Join(testdataDir, "order-detail.html")},
		"login":        {filepath.Join(testdataDir, "login.html")},
		"orders":       {filepath.Join(testdataDir, "orders.html")},
	}
	store := newTemplateStore(templateFuncs(), layoutFiles, pages, false)
	if err := store.load(); err != nil {
		t.Fatalf("load test templates: %v", err)
	}

	gw := newFakeGateway(t)
	h := &Handler{
		db:                db,
		templates:         store,
		cartStore:         NewCartStore(),
		zarinpal:          gw.client(),
		baseURL:           "http://127.0.0.1",
		uploadDir:         t.TempDir(),
		userSessions:      make(map[string]session),
		pendingLogins:     make(map[string]pendingLogin),
		pendingNext:       make(map[string]pendingReturn),
		adminSessions:     make(map[string]time.Time),
		adminUser:         "admin",
		adminPass:         "admin123",
		adminLoginLimiter: NewRateLimiter(1000, time.Minute),
		otpLimiter:        NewRateLimiter(100, time.Minute),
		otpVerifyLimiter:  NewRateLimiter(100, time.Minute),
		otpAttempts:       newAttemptTracker(),
	}
	return h, gw
}

// newTestRouter builds the same routing/middleware stack the server uses in
// production (SecurityHeaders, SameOrigin, CSRF, rate limiters, admin session
// auth) around a test handler.
func newTestRouter(t *testing.T) (*chi.Mux, *Handler, *fakeGateway) {
	t.Helper()
	h, gw := newTestHandler(t)
	return routerFor(h), h, gw
}

// routerFor wires the production routing/middleware stack around an existing
// handler, mirroring cmd/server/main.go. Tests that tweak the handler (e.g. a
// broken payment gateway) rebuild the router with this helper.
func routerFor(h *Handler) *chi.Mux {
	r := chi.NewRouter()
	r.Use(SecurityHeaders)
	r.Use(SameOrigin)
	r.Use(CSRFMiddleware)

	r.Get("/", h.Home)
	r.Get("/about", h.About)
	r.Get("/products/{category}", h.ProductsPage)
	r.Get("/cart/count", h.CartCount)
	r.Post("/cart/add", h.AddToCart)
	r.Post("/cart/update", h.UpdateCart)
	r.Post("/cart/remove", h.RemoveFromCart)
	r.Get("/cart", h.ViewCart)
	r.Get("/checkout", h.CheckoutForm)
	r.Post("/checkout/preview", h.PreviewCheckout)
	r.Post("/checkout", h.PlaceOrder)
	r.Get("/checkout/verify", h.VerifyPayment)
	r.Get("/checkout/confirmation/{id}", h.Confirmation)
	r.Get("/login", h.LoginPage)
	r.Post("/auth/send-otp", h.SendOTP)
	r.Post("/auth/verify-otp", h.VerifyOTP)
	r.Post("/logout", h.Logout)
	r.Get("/orders", h.UserOrders)
	r.Post("/orders/{id}/pay", h.ResumePayment)

	adminLimiter := NewRateLimiter(1000, time.Minute)
	r.Route("/admin", func(r chi.Router) {
		r.Get("/login", h.AdminLoginPage)
		r.Post("/login", h.AdminLoginPOST)
		r.Get("/logout", h.AdminLogout)
		r.Group(func(r chi.Router) {
			r.Use(h.RequireAdmin)
			r.Use(adminLimiter.Middleware)
			r.Get("/", h.AdminDashboard)
			r.Get("/orders/{id}", h.AdminOrderDetail)
			r.Post("/orders/{id}/status", h.AdminUpdateOrderStatus)
			r.Post("/orders/{id}/status-badge", h.AdminUpdateOrderStatusBadge)
			r.Get("/products/new", h.AdminNewProduct)
			r.Get("/products/{id}/edit", h.AdminEditProduct)
			r.Post("/products/{id}/update", h.AdminUpdateProductFull)
			r.Post("/products/{id}/toggle", h.AdminToggleProduct)
			r.Post("/products/{id}", h.AdminUpdateProduct)
			r.Post("/products", h.AdminCreateProduct)
			r.Post("/products/reorder", h.AdminReorderProducts)
			r.Post("/images", h.AdminUploadImage)
			r.Post("/images/{id}/remove", h.AdminRemoveImage)
			r.Post("/images/{id}/move", h.AdminMoveImage)
			r.Get("/categories/new", h.AdminNewCategory)
			r.Get("/categories/{id}/edit", h.AdminEditCategory)
			r.Post("/categories/{id}/update", h.AdminUpdateCategoryFull)
			r.Post("/categories/{id}/toggle", h.AdminToggleCategory)
			r.Post("/categories", h.AdminCreateCategory)
		})
	})

	return r
}

var csrfMetaRe = regexp.MustCompile(`name="csrf-token" content="([^"]+)"`)

// testClient performs requests against the test server, maintaining cookies
// across calls like a browser, tracking the CSRF token emitted in the pages,
// and disabling automatic redirect following so Location headers can be asserted.
type testClient struct {
	t         *testing.T
	srv       *httptest.Server
	http      *http.Client
	cookies   map[string]*http.Cookie
	csrfToken string
	lastBody  []byte
}

func newTestClient(t *testing.T, handler http.Handler) *testClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &testClient{
		t:       t,
		srv:     srv,
		http:    &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		cookies: map[string]*http.Cookie{},
	}
}

// do performs a request, records cookies and the CSRF token from the response
// body, and returns the response with its body still readable.
func (c *testClient) do(method, path string, form url.Values) *http.Response {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, c.srv.URL+path, body)
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	// Mimic the browser's htmx:configRequest handler (layout.html): a real
	// browser attaches the CSRF token from the meta tag as the X-CSRF-Token
	// header on every mutating request. Without this, even legitimate tests
	// would be rejected by the CSRF middleware after the cookie-fallback was
	// removed.
	if isMutating(method) && c.csrfToken != "" {
		req.Header.Set(csrfHeaderName, c.csrfToken)
	}
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}

	for _, ck := range resp.Cookies() {
		c.cookies[ck.Name] = ck
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(data))
	c.lastBody = data
	if m := csrfMetaRe.FindSubmatch(data); len(m) == 2 {
		c.csrfToken = string(m[1])
	}
	return resp
}

func (c *testClient) get(path string) *http.Response {
	return c.do(http.MethodGet, path, nil)
}

func (c *testClient) post(path string, form url.Values) *http.Response {
	return c.do(http.MethodPost, path, form)
}

func (c *testClient) csrf() string {
	return c.csrfToken
}

// authorize logs in through the admin login form (GET the page to establish
// the CSRF cookie/token, then POST the credentials) so the returned client
// carries the /admin-scoped session cookie like a real browser would.
func (c *testClient) authorize(user, pass string) {
	c.t.Helper()
	if resp := c.get("/admin/login"); resp.StatusCode != http.StatusOK {
		c.t.Fatalf("admin login page = %d", resp.StatusCode)
	}
	resp := c.post("/admin/login", url.Values{
		"username":   {user},
		"password":   {pass},
		"csrf_token": {c.csrfToken},
	})
	if resp.StatusCode != http.StatusSeeOther {
		c.t.Fatalf("admin login = %d, want 303 (body: %.120s)", resp.StatusCode, c.body())
	}
}

// bootstrapAdmin fetches the (authenticated) admin dashboard once so the CSRF
// cookie and token are established before the first mutating admin request.
func (c *testClient) bootstrapAdmin(t *testing.T) {
	t.Helper()
	if resp := c.get("/admin/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin bootstrap GET /admin/ = %d", resp.StatusCode)
	}
}

func (c *testClient) body() string {
	return string(c.lastBody)
}

func (c *testClient) hasCookie(name string) bool {
	_, ok := c.cookies[name]
	return ok
}

// login performs the full OTP login flow for a phone number and returns the
// session cookie value. It reads the generated code from the test database.
func (c *testClient) login(t *testing.T, db *sql.DB, phone string) {
	t.Helper()
	if !c.hasCookie("csrf_token") {
		if resp := c.get("/"); resp.StatusCode != http.StatusOK {
			t.Fatalf("csrf bootstrap GET / = %d", resp.StatusCode)
		}
	}
	resp := c.post("/auth/send-otp", url.Values{"phone": {phone}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send-otp status = %d", resp.StatusCode)
	}
	code := otpCode(t, db, phone)
	resp = c.post("/auth/verify-otp", url.Values{"phone": {phone}, "code": {code}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("verify-otp status = %d (body: %s)", resp.StatusCode, c.body())
	}
	if got := resp.Header.Get("HX-Redirect"); got == "" {
		t.Fatalf("verify-otp did not set HX-Redirect")
	}
}

// otpCode returns the most recent unused OTP for a phone number.
func otpCode(t *testing.T, db *sql.DB, phone string) string {
	t.Helper()
	var code string
	err := db.QueryRow(`SELECT code FROM otp_codes WHERE phone_number = ? AND is_used = 0 ORDER BY id DESC LIMIT 1`, phone).Scan(&code)
	if err != nil {
		t.Fatalf("read otp for %s: %v", phone, err)
	}
	return code
}

// addToCart adds a product to the caller's cart (matching the client's session
// cookie) and asserts the response is 200.
func (c *testClient) addToCart(t *testing.T, productID int64) {
	t.Helper()
	resp := c.post("/cart/add", url.Values{"product_id": {fmt.Sprint(productID)}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cart/add status = %d (body: %s)", resp.StatusCode, c.body())
	}
}

// lastOrder fetches the most recently created order from the database.
func lastOrder(t *testing.T, db *sql.DB) *OrderRow {
	t.Helper()
	row := db.QueryRow(`SELECT id, status, total_amount, payment_authority, payment_ref_id, user_id
		FROM orders ORDER BY created_at DESC, rowid DESC LIMIT 1`)
	return scanOrderRow(t, row)
}

// database.OrderRow is a lightweight projection used by tests to inspect order
// state after the flows run.
type scanRow interface {
	Scan(dest ...any) error
}

// OrderRow mirrors the subset of order columns tests assert against.
type OrderRow struct {
	ID               string
	Status           string
	TotalAmount      int
	PaymentAuthority string
	PaymentRefID     int64
	UserID           int64
}

func scanOrderRow(t *testing.T, row scanRow) *OrderRow {
	t.Helper()
	var o OrderRow
	var userID sql.NullInt64
	err := row.Scan(&o.ID, &o.Status, &o.TotalAmount, &o.PaymentAuthority, &o.PaymentRefID, &userID)
	if err != nil {
		t.Fatalf("scan order: %v", err)
	}
	o.UserID = userID.Int64
	return &o
}

// productStock returns the current stock_quantity for a product.
func productStock(t *testing.T, db *sql.DB, productID int64) int {
	t.Helper()
	var s int
	if err := db.QueryRow(`SELECT stock_quantity FROM products WHERE id = ?`, productID).Scan(&s); err != nil {
		t.Fatalf("read stock for %d: %v", productID, err)
	}
	return s
}
