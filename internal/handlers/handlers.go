package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"farmstore/internal/database"
	"farmstore/internal/models"
	"farmstore/internal/payment"
	"farmstore/internal/utils"
)

// sessionTTL is how long an authenticated session lives (server-side), matching
// the session cookie's 7-day MaxAge. Pending-login (OTP) bindings and saved
// post-login destinations expire on a shorter timer so stale entries cannot
// accumulate in the in-memory maps indefinitely.
const (
	sessionTTL = 7 * 24 * time.Hour
	otpTTL     = 2 * time.Minute
)

// session is an authenticated session entry with its server-side expiry.
type session struct {
	userID    int64
	expiresAt time.Time
}

// pendingLogin binds a phone number to a session during the OTP exchange.
type pendingLogin struct {
	phone     string
	expiresAt time.Time
}

// pendingReturn is a sanitized post-login destination saved against a session.
type pendingReturn struct {
	url       string
	expiresAt time.Time
}

// Handler is the central HTTP handler. It wires together the database connection,
// HTML template map, in-memory cart store, and session management state.
type Handler struct {
	db               *sql.DB
	templates        map[string]*template.Template
	cartStore        *CartStore
	zarinpal         *payment.Zarinpal
	baseURL          string
	userSessions     map[string]session       // session ID → authenticated session
	pendingLogins    map[string]pendingLogin  // session ID → phone during OTP flow
	pendingNext      map[string]pendingReturn // session ID → post-login destination
	otpLimiter       *RateLimiter             // per-phone cap on OTP sends
	otpVerifyLimiter *RateLimiter             // per-phone cap on OTP verification attempts
	sessionMu        sync.RWMutex
}

// NewHandler initialises the Handler, parsing all HTML templates with a shared
// function map (formatPrice, persianDate, etc.) from the templates/ directory.
func NewHandler(db *sql.DB, cartStore *CartStore, zarinpal *payment.Zarinpal, baseURL string) (*Handler, error) {
	funcMap := template.FuncMap{
		"formatPrice": func(cents int) string {
			return formatToman(cents)
		},
		"persianDigits": func(v any) string {
			return toPersianDigits(fmt.Sprint(v))
		},
		"persianDate": func(t time.Time) string {
			return utils.FormatPersianDate(t)
		},
		"persianDateTime": func(t time.Time) string {
			return utils.FormatPersianDateTime(t)
		},
		"comma": func(v int) string {
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
		},
		"multiply": func(a, b int) int {
			return a * b
		},
		"statusColor": func(s string) string {
			switch s {
			case "pending":
				return "text-[#7A5A2E]"
			case "preparing":
				return "text-[#5B3A5C]"
			case "dispatched":
				return "text-[#2F5D33]"
			case "cancelled":
				return "text-[#9E2A2B]"
			case "awaiting_payment":
				return "text-[#C98A2C]"
			}
			return "text-clay"
		},
		"now": time.Now,
		"hasSuffix": strings.HasSuffix,
	}

	layoutFiles := []string{"templates/layout.html"}
	pages := map[string][]string{
		"index":        {"templates/index.html"},
		"products":     {"templates/products.html"},
		"about":        {"templates/about.html"},
		"cart":         {"templates/cart.html"},
		"checkout":     {"templates/checkout.html"},
		"confirmation": {"templates/confirmation.html"},
		"admin":        {"templates/admin.html"},
		"order-detail": {"templates/order-detail.html"},
		"login":        {"templates/login.html"},
		"orders":       {"templates/orders.html"},
	}

	templates := make(map[string]*template.Template, len(pages))
	for name, files := range pages {
		t, err := template.New("layout.html").Funcs(funcMap).ParseFiles(append(layoutFiles, files...)...)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		templates[name] = t
	}

	h := &Handler{
		db:               db,
		templates:        templates,
		cartStore:        cartStore,
		zarinpal:         zarinpal,
		baseURL:          baseURL,
		userSessions:     make(map[string]session),
		pendingLogins:    make(map[string]pendingLogin),
		pendingNext:      make(map[string]pendingReturn),
		otpLimiter:       NewRateLimiter(5, time.Minute),
		otpVerifyLimiter: NewRateLimiter(10, time.Minute),
	}
	h.startSessionJanitor()
	return h, nil
}

// getUserID returns the authenticated user ID for the current request, or 0 if
// the user is not logged in. It reads the session cookie and looks up the
// in-memory session map.
func (h *Handler) getUserID(r *http.Request) int64 {
	cookie, err := r.Cookie("session")
	if err != nil || !validSessionID(cookie.Value) {
		return 0
	}
	h.sessionMu.RLock()
	defer h.sessionMu.RUnlock()
	s, ok := h.userSessions[cookie.Value]
	if !ok || time.Now().After(s.expiresAt) {
		return 0
	}
	return s.userID
}

// commonData returns template data that is shared across all pages — currently
// just the "LoggedIn" boolean used to show/hide login/logout/orders links.
func (h *Handler) commonData(r *http.Request) map[string]any {
	sid, err := r.Cookie("session")
	loggedIn := false
	cartCount := 0
	if err == nil && validSessionID(sid.Value) {
		h.sessionMu.RLock()
		if s, ok := h.userSessions[sid.Value]; ok && time.Now().Before(s.expiresAt) {
			loggedIn = true
		}
		h.sessionMu.RUnlock()
		cartCount = h.cartStore.Get(sid.Value).Count()
	}
	return map[string]any{
		"LoggedIn":  loggedIn,
		"CartCount": cartCount,
	}
}

// startSessionJanitor launches a background goroutine that periodically purges
// expired session, pending-login, and pending-return entries so the in-memory
// maps cannot grow without bound.
func (h *Handler) startSessionJanitor() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			h.purgeExpiredSessions(time.Now())
		}
	}()
}

// purgeExpiredSessions removes every entry whose expiry has passed.
func (h *Handler) purgeExpiredSessions(now time.Time) {
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	for sid, s := range h.userSessions {
		if now.After(s.expiresAt) {
			delete(h.userSessions, sid)
		}
	}
	for sid, pl := range h.pendingLogins {
		if now.After(pl.expiresAt) {
			delete(h.pendingLogins, sid)
		}
	}
	for sid, pr := range h.pendingNext {
		if now.After(pr.expiresAt) {
			delete(h.pendingNext, sid)
		}
	}
}

// mergeData merges common template data into the page-specific data map.
// Page-specific keys take precedence over common keys.
func (h *Handler) mergeData(r *http.Request, data map[string]any) map[string]any {
	if data == nil {
		data = make(map[string]any)
	}
	for k, v := range h.commonData(r) {
		if _, exists := data[k]; !exists {
			data[k] = v
		}
	}
	return data
}

// getOrCreateSessionID returns the existing session cookie value for this request,
// or creates a new session, sets the cookie, and returns the new ID.
func (h *Handler) getOrCreateSessionID(w http.ResponseWriter, r *http.Request) string {
	cookie, err := r.Cookie("session")
	if err == nil && validSessionID(cookie.Value) {
		return cookie.Value
	}
	sid := generateSessionID()
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	return sid
}

// ── Storefront ────────────────────────────────────────

// catInfo is the lightweight per-category metadata used to render the home page
// showcase tiles — the Persian label plus a slug used in the URL, a widescreen
// orchard photo for the tile.
type catInfo struct {
	Slug    string
	Label   string
	Image   string // CSS background image URL for the tile
	Season  string // season key for matching (spring, summer, autumn, or empty)
	IsSVG   bool   // true if Image is an SVG icon (small centered) vs photo (cover)
}

// seasonInfo carries the copy + accent used by the seasonal banner on Home.
// It flips between fig season and pomegranate season through the year.
type seasonInfo struct {
	Key              string // "fig" or "pomegranate"
	Label            string
	Tag              string // small stamp label
	Heading          string // Alyamama headline
	Tagline          string
	Accent           string // underline bar colour
	AccentQuoteColor string // text colour used in the tag stamp
	Image            string
	Target           string // category link for the season's produce
	CTA              string
}

// currentSeason decides the seasonal banner based on the Gregorian month.
func currentSeason() seasonInfo {
	m := time.Now().Month()
	switch {
	case m >= 3 && m <= 5:
		return seasonInfo{
			Key:              "spring",
			Label:            "فصل بهار",
			Tag:              "تازه و سبز",
			Heading:          "محصولات تازه‌ی بهاری",
			Tagline:          "سبزی و میوه‌ی بهاری، مستقیم از باغ.",
			Accent:           "#3F5D42",
			AccentQuoteColor: "#5A8A60",
			Image:            "/assets/toodej.webp",
			Target:           "/products/spring",
			CTA:              "محصولات بهار را ببین",
		}
	case m >= 6 && m <= 8:
		return seasonInfo{
			Key:              "summer",
			Label:            "فصل تابستان",
			Tag:              "ویژه این فصل",
			Heading:          "انجیر خشک درجه یک، خوشمزه و طبیعی",
			Tagline:          "خشک‌شده زیر آفتاب و با کیفیت. سرشار از فیبر، آنتی‌اکسیدان و مواد معدنی.",
			Accent:           "#C98A2C",
			AccentQuoteColor: "#E3B65C",
			Image:            "/assets/fig-showcase.webp",
			Target:           "/products/summer",
			CTA:              "محصولات این فصل را ببین",
		}
	case m >= 9 && m <= 11:
		return seasonInfo{
			Key:              "autumn",
			Label:            "فصل پاییز",
			Tag:              "برداشت پاییز",
			Heading:          "انار یاقوتی، آبدار و پر از آنتی‌اکسیدان",
			Tagline:          "از دانه‌ی تازه تا رب و آب‌انار؛ بدون هیچ افزودنی.",
			Accent:           "#C97064",
			AccentQuoteColor: "#D98C80",
			Image:            "/assets/pomegranate-showcase.webp",
			Target:           "/products/autumn",
			CTA:              "محصولات پاییز را ببین",
		}
	default:
		return seasonInfo{
			Key:              "dried",
			Label:            "خشکبار",
			Tag:              "همیشه موجود",
			Heading:          "خشکبار با کیفیت، همیشه تازه",
			Tagline:          "انجیر خشک، پسته، گردو و دیگر خشکبار اعلی.",
			Accent:           "#8C6F5E",
			AccentQuoteColor: "#A68B7B",
			Image:            "/assets/fig-showcase.webp",
			Target:           "/products/dried",
			CTA:              "خشکبار را ببین",
		}
	}
}

// featuredProducts flattens a small mixed selection of active products from
// all categories for the storefront "منتخب این فصل" row.
func (h *Handler) featuredProducts() []models.Product {
	const max = 5
	var out []models.Product
	for _, cat := range []string{database.CategorySpring, database.CategorySummer, database.CategoryAutumn, database.CategoryDried, database.CategoryProcessed} {
		ps, err := database.GetProducts(h.db, cat)
		if err != nil {
			continue
		}
		for _, p := range ps {
			if len(out) >= max {
				return out
			}
			out = append(out, p)
		}
	}
	return out
}

// Home renders the main storefront page — hero, story strip, featured products,
// seasonal banner, and the five category showcase tiles.
func (h *Handler) Home(w http.ResponseWriter, r *http.Request) {
	cats := []catInfo{
		{
			Slug:   "spring",
			Label:  database.CategorySpring,
			Image:  "/assets/blossoms-and-sky.webp",
			Season: "spring",
		},
		{
			Slug:   "summer",
			Label:  database.CategorySummer,
			Image:  "/assets/summer.webp",
			Season: "summer",
		},
		{
			Slug:   "autumn",
			Label:  database.CategoryAutumn,
			Image:  "/assets/autumn.webp",
			Season: "autumn",
		},
		{
			Slug:  "dried",
			Label: database.CategoryDried,
			Image: "/assets/fig.svg?v=2",
			IsSVG: true,
		},
		{
			Slug:  "processed",
			Label: database.CategoryProcessed,
			Image: "/assets/leaf.svg?v=2",
			IsSVG: true,
		},
	}

	data := h.mergeData(r, map[string]any{
		"Categories":    cats,
		"Featured":      h.featuredProducts(),
		"Season":        currentSeason(),
		"CurrentSeason": currentSeason().Key,
	})

	if err := h.templates["index"].Execute(w, data); err != nil {
		log.Printf("render home: %v", err)
	}
}

// ProductsPage renders the listing for a single category — reusing the same
// product card markup as before.
func (h *Handler) ProductsPage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("category")

	var category, currentFilter, label string
	switch slug {
	case "spring":
		category = database.CategorySpring
		currentFilter = "spring"
		label = database.CategorySpring
	case "summer":
		category = database.CategorySummer
		currentFilter = "summer"
		label = database.CategorySummer
	case "autumn":
		category = database.CategoryAutumn
		currentFilter = "autumn"
		label = database.CategoryAutumn
	case "dried":
		category = database.CategoryDried
		currentFilter = "dried"
		label = database.CategoryDried
	case "processed":
		category = database.CategoryProcessed
		currentFilter = "processed"
		label = database.CategoryProcessed
	default:
		http.NotFound(w, r)
		return
	}

	products, err := database.GetProducts(h.db, category)
	if err != nil {
		log.Printf("products page: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := h.mergeData(r, map[string]any{
		"Products":      products,
		"CurrentFilter": currentFilter,
		"CategoryLabel": label,
		"CategorySlug":  currentFilter,
	})

	if err := h.templates["products"].Execute(w, data); err != nil {
		log.Printf("render products page: %v", err)
	}
}

// ── Cart ──────────────────────────────────────────────

// CartCount returns the total number of units in the cart as plain text (used
// by the cart badge in the navbar via HTMX).
func (h *Handler) CartCount(w http.ResponseWriter, r *http.Request) {
	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, toPersianDigits(strconv.Itoa(cart.Count())))
}

// AddToCart adds a product to the cart (or increments its quantity by 1). It
// triggers a cartUpdated event on the client so the cart badge updates.
func (h *Handler) AddToCart(w http.ResponseWriter, r *http.Request) {
	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)

	productIDStr := r.FormValue("product_id")
	productID, err := strconv.ParseInt(productIDStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid product", http.StatusBadRequest)
		return
	}

	product, err := database.GetProduct(h.db, productID)
	if err != nil {
		http.Error(w, "product not found", http.StatusNotFound)
		return
	}

	// Check if adding one more would exceed stock.
	cart.mu.Lock()
	currentQty := 0
	for _, item := range cart.Items {
		if item.ProductID == productID {
			currentQty = item.Quantity
			break
		}
	}
	cart.mu.Unlock()

	if currentQty >= product.StockQuantity {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("HX-Trigger", `{"cartUpdated":"", "stockError":""}`)
		fmt.Fprint(w, toPersianDigits(strconv.Itoa(cart.Count())))
		return
	}

	cart.AddItem(CartItem{
		ProductID: product.ID,
		Name:      product.Name,
		Price:     product.Price,
		Unit:      product.Unit,
		Quantity:  1,
		ImageURL:  product.ImageURL,
	})

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("HX-Trigger", `{"cartUpdated":"", "cartEvent":"added"}`)
	fmt.Fprint(w, cart.Count())
}

// UpdateCart adjusts the quantity of a cart item by a positive or negative delta.
// If the resulting quantity is zero or negative the item is removed.
func (h *Handler) UpdateCart(w http.ResponseWriter, r *http.Request) {
	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)

	productIDStr := r.FormValue("product_id")
	deltaStr := r.FormValue("delta")
	if productIDStr == "" {
		http.Error(w, "missing product_id", http.StatusBadRequest)
		return
	}
	productID, err := strconv.ParseInt(productIDStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid product_id", http.StatusBadRequest)
		return
	}
	delta := 1
	if deltaStr != "" {
		d, err := strconv.Atoi(deltaStr)
		if err == nil {
			delta = d
		}
	}

	cart.UpdateQuantity(productID, delta)

	// When increasing quantity, check stock and revert if overflow.
	if delta > 0 {
		product, err := database.GetProduct(h.db, productID)
		if err == nil {
			cart.mu.Lock()
			var newQty int
			found := false
			for _, item := range cart.Items {
				if item.ProductID == productID {
					newQty = item.Quantity
					found = true
					break
				}
			}
			cart.mu.Unlock()
			if found && newQty > product.StockQuantity {
				cart.UpdateQuantity(productID, -1)
				cart.mu.Lock()
				items := make([]CartItem, len(cart.Items))
				copy(items, cart.Items)
				cart.mu.Unlock()
				data := h.mergeData(r, map[string]any{
					"Items": items,
					"Total": cart.Total(),
				})
				w.Header().Set("Content-Type", "text/html")
				w.Header().Set("HX-Trigger", `{"cartUpdated":"", "stockError":""}`)
				h.templates["cart"].ExecuteTemplate(w, "cart-content", data)
				return
			}
		}
	}

	event := "added"
	if delta < 0 {
		event = "removed"
	}
	h.renderCartContent(w, r, event)
}

// RemoveFromCart removes a product line from the cart entirely.
func (h *Handler) RemoveFromCart(w http.ResponseWriter, r *http.Request) {
	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)

	productIDStr := r.FormValue("product_id")
	if productIDStr == "" {
		http.Error(w, "missing product_id", http.StatusBadRequest)
		return
	}
	productID, err := strconv.ParseInt(productIDStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid product_id", http.StatusBadRequest)
		return
	}

	cart.RemoveItem(productID)

	h.renderCartContent(w, r, "removed")
}

// renderCartContent renders the "cart-content" template partial and fires a
// cart event so the badge and toast are updated on the client.
func (h *Handler) renderCartContent(w http.ResponseWriter, r *http.Request, event string) {
	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)

	cart.mu.Lock()
	items := make([]CartItem, len(cart.Items))
	copy(items, cart.Items)
	cart.mu.Unlock()

	data := h.mergeData(r, map[string]any{
		"Items": items,
		"Total": cart.Total(),
	})
	w.Header().Set("HX-Trigger", `{"cartUpdated":"", "cartEvent":"`+event+`"}`)
	if err := h.templates["cart"].ExecuteTemplate(w, "cart-content", data); err != nil {
		log.Printf("render cart-content: %v", err)
	}
}

// ViewCart renders the full cart page.
func (h *Handler) ViewCart(w http.ResponseWriter, r *http.Request) {
	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)

	cart.mu.Lock()
	items := make([]CartItem, len(cart.Items))
	copy(items, cart.Items)
	cart.mu.Unlock()

	data := h.mergeData(r, map[string]any{
		"Items": items,
		"Total": cart.Total(),
	})
	if err := h.templates["cart"].Execute(w, data); err != nil {
		log.Printf("render cart: %v", err)
	}
}

// ── Checkout ──────────────────────────────────────────

// CheckoutForm renders the checkout page. Requires authentication and a non-empty
// cart; otherwise redirects to login or cart respectively.
func (h *Handler) CheckoutForm(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireAuth(w, r)
	if !ok {
		return
	}

	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)
	if cart.Count() == 0 {
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}

	phone := ""
	rows, err := h.db.Query("SELECT phone_number FROM users WHERE id = ?", userID)
	if err == nil {
		if rows.Next() {
			rows.Scan(&phone)
		}
		rows.Close()
	}

	data := h.mergeData(r, map[string]any{
		"Total":      cart.Total(),
		"Phone":      phone,
		"Step":       1,
		"Name":       r.URL.Query().Get("name"),
		"Address":    r.URL.Query().Get("address"),
		"PostalCode": r.URL.Query().Get("postal_code"),
	})
	if err := h.templates["checkout"].Execute(w, data); err != nil {
		log.Printf("render checkout: %v", err)
	}
}

// PreviewCheckout validates the shipping form and renders step 2 (order review).
func (h *Handler) PreviewCheckout(w http.ResponseWriter, r *http.Request) {
	_, ok := h.requireAuth(w, r)
	if !ok {
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	phone := strings.TrimSpace(r.FormValue("phone"))
	address := strings.ReplaceAll(strings.TrimSpace(r.FormValue("address")), "\n", " ")
	address = strings.ReplaceAll(address, "\r", "")
	postalCode := strings.TrimSpace(r.FormValue("postal_code"))

	if name == "" || len(name) > 80 || !validIranianPhone(phone) || len(address) < 5 || len(address) > 300 || !validPostalCode(postalCode) {
		sid := h.getOrCreateSessionID(w, r)
		cart := h.cartStore.Get(sid)
		data := h.mergeData(r, map[string]any{
			"Error":    "اطلاعات تماس، آدرس و کد پستی را به‌درستی وارد کنید.",
			"Total":    cart.Total(),
			"Phone":    phone,
			"Step":     1,
		})
		w.WriteHeader(http.StatusBadRequest)
		if err := h.templates["checkout"].Execute(w, data); err != nil {
			log.Printf("render checkout error: %v", err)
		}
		return
	}

	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)
	if cart.Count() == 0 {
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}

	cart.mu.Lock()
	items := make([]CartItem, len(cart.Items))
	copy(items, cart.Items)
	cart.mu.Unlock()

	data := h.mergeData(r, map[string]any{
		"Step":      2,
		"Total":     cart.Total(),
		"Items":     items,
		"Name":      name,
		"Phone":     phone,
		"Address":   address,
		"PostalCode": postalCode,
	})
	if err := h.templates["checkout"].Execute(w, data); err != nil {
		log.Printf("render checkout preview: %v", err)
	}
}

// PlaceOrder validates the checkout form, creates an order and order items in a
// database transaction, clears the cart, and redirects to the confirmation page.
func (h *Handler) PlaceOrder(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireAuth(w, r)
	if !ok {
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	phone := strings.TrimSpace(r.FormValue("phone"))
	address := strings.ReplaceAll(strings.TrimSpace(r.FormValue("address")), "\n", " ")
	address = strings.ReplaceAll(address, "\r", "")
	postalCode := strings.TrimSpace(r.FormValue("postal_code"))

	if name == "" || len(name) > 80 || !validIranianPhone(phone) || len(address) < 5 || len(address) > 300 || !validPostalCode(postalCode) {
		sid := h.getOrCreateSessionID(w, r)
		cart := h.cartStore.Get(sid)
		data := h.mergeData(r, map[string]any{
			"Error": "اطلاعات تماس، آدرس و کد پستی را به‌درستی وارد کنید.",
			"Total": cart.Total(),
			"Phone": phone,
			"Step":  1,
		})
		w.WriteHeader(http.StatusBadRequest)
		if err := h.templates["checkout"].Execute(w, data); err != nil {
			log.Printf("render checkout error: %v", err)
		}
		return
	}

	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)

	cart.mu.Lock()
	items := make([]CartItem, len(cart.Items))
	copy(items, cart.Items)
	cart.mu.Unlock()

	if len(items) == 0 {
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}

	totalAmount := cart.Total()

	order := &models.Order{
		CustomerName:    name,
		CustomerPhone:   phone,
		CustomerAddress: address,
		PostalCode:      postalCode,
		TotalAmount:     totalAmount,
		Status:          "awaiting_payment",
		UserID:          userID,
	}

	var orderItems []models.OrderItem
	for _, ci := range items {
		orderItems = append(orderItems, models.OrderItem{
			ProductID:    ci.ProductID,
			Quantity:     ci.Quantity,
			PricePerUnit: ci.Price,
		})
	}

	orderID, err := database.CreateOrder(h.db, order, orderItems)
	if err != nil {
		if errors.Is(err, database.ErrInsufficientStock) {
			sid := h.getOrCreateSessionID(w, r)
			cart := h.cartStore.Get(sid)
			data := h.mergeData(r, map[string]any{
				"Error":    "موجودی برخی محصولات کافی نیست؛ لطفاً سبد را به‌روز کنید.",
				"Total":    cart.Total(),
				"Phone":    phone,
				"Step":     2,
				"Name":     name,
				"Address":  address,
				"PostalCode": postalCode,
			})
			w.WriteHeader(http.StatusConflict)
			if err := h.templates["checkout"].Execute(w, data); err != nil {
				log.Printf("render checkout error: %v", err)
			}
			return
		}
		log.Printf("create order: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Initiate Zarinpal payment.
	callbackURL := h.baseURL + "/checkout/verify"
	authority, err := h.zarinpal.RequestPayment(totalAmount, callbackURL, "سفارش تودج "+orderID)
	if err != nil {
		log.Printf("zarinpal request payment: %v", err)
		// Cancel the order and restore stock so the user can retry.
		database.MarkPaymentFailed(h.db, orderID)
		sid := h.getOrCreateSessionID(w, r)
		cart := h.cartStore.Get(sid)

		cart.mu.Lock()
		cartItems := make([]CartItem, len(cart.Items))
		copy(cartItems, cart.Items)
		cart.mu.Unlock()

		data := h.mergeData(r, map[string]any{
			"Error":    "خطا در اتصال به درگاه پرداخت؛ لطفاً دوباره تلاش کنید.",
			"Total":    cart.Total(),
			"Phone":    phone,
			"Step":     2,
			"Name":     name,
			"Address":  address,
			"PostalCode": postalCode,
			"Items":    cartItems,
		})
		w.WriteHeader(http.StatusBadGateway)
		if err := h.templates["checkout"].Execute(w, data); err != nil {
			log.Printf("render checkout error: %v", err)
		}
		return
	}

	if err := database.SetPaymentAuthority(h.db, orderID, authority); err != nil {
		log.Printf("set payment authority: %v", err)
	}

	gatewayURL := h.zarinpal.GatewayURL(authority)
	log.Printf("order %s: redirecting to payment gateway: %s", orderID, gatewayURL)

	cart.Clear()
	w.Header().Set("HX-Trigger", "cartUpdated")
	http.Redirect(w, r, gatewayURL, http.StatusSeeOther)
}

// VerifyPayment handles the Zarinpal callback after the user completes (or cancels)
// the payment. It verifies the transaction and updates the order status accordingly.
func (h *Handler) VerifyPayment(w http.ResponseWriter, r *http.Request) {
	authority := r.URL.Query().Get("Authority")
	status := r.URL.Query().Get("Status")

	if authority == "" || status != "OK" {
		// Payment was cancelled or failed — cancel the order and restore stock.
		if authority != "" {
			if order, err := database.GetOrderByAuthority(h.db, authority); err == nil {
				database.MarkPaymentFailed(h.db, order.ID)
			}
		}
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}

	order, err := database.GetOrderByAuthority(h.db, authority)
	if err != nil {
		log.Printf("verify: order not found for authority: %v", err)
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}

	result, err := h.zarinpal.VerifyPayment(order.TotalAmount, authority)
	if err != nil {
		log.Printf("zarinpal verify: %v", err)
		database.MarkPaymentFailed(h.db, order.ID)
		http.Redirect(w, r, "/cart", http.StatusSeeOther)
		return
	}

	if result.OK {
		if err := database.ConfirmPayment(h.db, order.ID, result.RefID); err != nil {
			log.Printf("confirm payment: %v", err)
		}
	} else {
		log.Printf("payment not verified for order %s: %s", order.ID, result.Message)
		database.MarkPaymentFailed(h.db, order.ID)
	}

	http.Redirect(w, r, fmt.Sprintf("/checkout/confirmation/%s", order.ID), http.StatusSeeOther)
}

// Confirmation displays the order confirmation page after a successful checkout.
// Access is restricted to the authenticated user who owns the order (IDOR guard):
// unauthenticated visitors are redirected to login and requests for another
// user's order return 404 so order IDs cannot be probed or enumerated.
func (h *Handler) Confirmation(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireAuth(w, r)
	if !ok {
		return
	}

	orderID := r.PathValue("id")
	if !validOrderID(orderID) {
		http.NotFound(w, r)
		return
	}

	order, items, products, err := database.GetOrderWithItems(h.db, orderID)
	if err != nil {
		log.Printf("get order: %v", err)
		http.NotFound(w, r)
		return
	}

	if order.UserID != userID {
		http.NotFound(w, r)
		return
	}

	type itemView struct {
		Name     string
		Quantity int
		Price    int
		Subtotal int
		Unit     string
	}

	var itemViews []itemView
	for i, item := range items {
		name := fmt.Sprintf("Product #%d", item.ProductID)
		unit := ""
		if i < len(products) {
			name = products[i].Name
			unit = products[i].Unit
		}
		itemViews = append(itemViews, itemView{
			Name:     name,
			Quantity: item.Quantity,
			Price:    item.PricePerUnit,
			Subtotal: item.Quantity * item.PricePerUnit,
			Unit:     unit,
		})
	}

	data := h.mergeData(r, map[string]any{
		"Order": order,
		"Items": itemViews,
	})
	if err := h.templates["confirmation"].Execute(w, data); err != nil {
		log.Printf("render confirmation: %v", err)
	}
}

// ── User Orders ───────────────────────────────────────

// UserOrders renders the authenticated user's order history page.
func (h *Handler) UserOrders(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireAuth(w, r)
	if !ok {
		return
	}

	summaries, err := database.GetUserOrdersWithItems(h.db, userID)
	if err != nil {
		log.Printf("get user orders: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := h.mergeData(r, map[string]any{
		"Orders": summaries,
	})
	if err := h.templates["orders"].Execute(w, data); err != nil {
		log.Printf("render orders: %v", err)
	}
}

// About renders the about-us page with a short introduction to the farm.
func (h *Handler) About(w http.ResponseWriter, r *http.Request) {
	data := h.mergeData(r, nil)
	if err := h.templates["about"].Execute(w, data); err != nil {
		log.Printf("render about: %v", err)
	}
}

// ── Helpers ───────────────────────────────────────────

// formatToman formats an integer price (in the smallest currency unit) as a
// human-readable Persian price string with thousand separators, Persian digits,
// and the "تومان" suffix.
func formatToman(cents int) string {
	s := strconv.Itoa(cents)
	n := len(s)
	var parts []string
	for i := n; i > 0; i -= 3 {
		start := i - 3
		if start < 0 {
			start = 0
		}
		parts = append([]string{s[start:i]}, parts...)
	}
	return toPersianDigits(strings.Join(parts, ",")) + " تومان"
}

// toPersianDigits converts Western digits (0-9) in a string to their Persian
// Unicode equivalents (۰-۹). Non-digit runes pass through unchanged.
func toPersianDigits(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r - '0' + 0x06F0)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// renderCenteredError is a utility for rendering error pages (unused currently
// but kept for future error-page rendering).
func (h *Handler) renderCenteredError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	data := map[string]any{
		"title":   http.StatusText(status),
		"message": msg,
	}
	if err := h.templates["index"].ExecuteTemplate(w, "content", data); err != nil {
		http.Error(w, msg, status)
	}
}
