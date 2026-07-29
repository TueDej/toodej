package handlers

import (
	"database/sql"
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
	"farmstore/internal/utils"
)

type Handler struct {
	db            *sql.DB
	templates     map[string]*template.Template
	cartStore     *CartStore
	userSessions  map[string]int64
	pendingLogins map[string]string
	sessionMu     sync.RWMutex
}

func NewHandler(db *sql.DB, cartStore *CartStore) (*Handler, error) {
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
				return "text-yellow-600"
			case "processing":
				return "text-blue-600"
			case "completed":
				return "text-green-600"
			case "cancelled":
				return "text-red-600"
			}
			return "text-gray-600"
		},
		"now": time.Now,
	}

	layouts := []string{"templates/layout.html"}
	pages := map[string][]string{
		"index":        {"templates/index.html"},
		"cart":         {"templates/cart.html"},
		"checkout":     {"templates/checkout.html"},
		"confirmation": {"templates/confirmation.html"},
		"admin":        {"templates/admin.html"},
		"login":        {"templates/login.html"},
		"orders":       {"templates/orders.html"},
	}

	templates := make(map[string]*template.Template, len(pages))
	for name, files := range pages {
		t, err := template.New("layout.html").Funcs(funcMap).ParseFiles(append(layouts, files...)...)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		templates[name] = t
	}

	return &Handler{
		db:            db,
		templates:     templates,
		cartStore:     cartStore,
		userSessions:  make(map[string]int64),
		pendingLogins: make(map[string]string),
	}, nil
}

func (h *Handler) getUserID(r *http.Request) int64 {
	cookie, err := r.Cookie("session")
	if err != nil {
		return 0
	}
	h.sessionMu.RLock()
	defer h.sessionMu.RUnlock()
	return h.userSessions[cookie.Value]
}

func (h *Handler) commonData(r *http.Request) map[string]any {
	sid, err := r.Cookie("session")
	loggedIn := false
	if err == nil {
		h.sessionMu.RLock()
		_, loggedIn = h.userSessions[sid.Value]
		h.sessionMu.RUnlock()
	}
	return map[string]any{
		"LoggedIn": loggedIn,
	}
}

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

func (h *Handler) getOrCreateSessionID(w http.ResponseWriter, r *http.Request) string {
	cookie, err := r.Cookie("session")
	if err == nil && cookie.Value != "" {
		return cookie.Value
	}
	sid := generateSessionID()
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   86400 * 7,
	})
	return sid
}

func (h *Handler) Home(w http.ResponseWriter, r *http.Request) {
	category := r.URL.Query().Get("category")
	if category == "fresh" {
		category = database.CategoryFresh
	} else if category == "derived" {
		category = database.CategoryDerived
	} else {
		category = ""
	}

	products, err := database.GetProducts(h.db, category)
	if err != nil {
		log.Printf("home: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	currentFilter := "all"
	if category == database.CategoryFresh {
		currentFilter = "fresh"
	} else if category == database.CategoryDerived {
		currentFilter = "derived"
	}

	data := h.mergeData(r, map[string]any{
		"Products":      products,
		"CurrentFilter": currentFilter,
	})

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		if err := h.templates["index"].ExecuteTemplate(w, "product-section", data); err != nil {
			log.Printf("render product grid: %v", err)
		}
		return
	}

	if err := h.templates["index"].Execute(w, data); err != nil {
		log.Printf("render home: %v", err)
	}
}

func (h *Handler) CartCount(w http.ResponseWriter, r *http.Request) {
	sid := h.getOrCreateSessionID(w, r)
	cart := h.cartStore.Get(sid)
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, toPersianDigits(strconv.Itoa(cart.Count())))
}

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

	event := "added"
	if delta < 0 {
		event = "removed"
	}
	h.renderCartContent(w, r, event)
}

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

func (h *Handler) CheckoutForm(w http.ResponseWriter, r *http.Request) {
	userID := h.getUserID(r)
	if userID == 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
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
		"Total": cart.Total(),
		"Phone": phone,
	})
	if err := h.templates["checkout"].Execute(w, data); err != nil {
		log.Printf("render checkout: %v", err)
	}
}

func (h *Handler) PlaceOrder(w http.ResponseWriter, r *http.Request) {
	userID := h.getUserID(r)
	if userID == 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	name := r.FormValue("name")
	phone := r.FormValue("phone")
	address := r.FormValue("address")

	if name == "" || phone == "" || address == "" {
		sid := h.getOrCreateSessionID(w, r)
		cart := h.cartStore.Get(sid)
		data := h.mergeData(r, map[string]any{
			"Error": "تمامی فیلدها الزامی هستند.",
			"Total": cart.Total(),
			"Phone": phone,
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
		TotalAmount:     totalAmount,
		Status:          "pending",
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
		log.Printf("create order: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	cart.Clear()
	w.Header().Set("HX-Trigger", "cartUpdated")
	http.Redirect(w, r, fmt.Sprintf("/checkout/confirmation/%s", orderID), http.StatusSeeOther)
}

func (h *Handler) Confirmation(w http.ResponseWriter, r *http.Request) {
	orderID := r.PathValue("id")
	if orderID == "" {
		http.NotFound(w, r)
		return
	}

	order, items, products, err := database.GetOrderWithItems(h.db, orderID)
	if err != nil {
		log.Printf("get order: %v", err)
		http.NotFound(w, r)
		return
	}

	type itemView struct {
		Name     string
		Quantity int
		Price    int
		Subtotal int
	}

	var itemViews []itemView
	for i, item := range items {
		name := fmt.Sprintf("Product #%d", item.ProductID)
		if i < len(products) {
			name = products[i].Name
		}
		itemViews = append(itemViews, itemView{
			Name:     name,
			Quantity: item.Quantity,
			Price:    item.PricePerUnit,
			Subtotal: item.Quantity * item.PricePerUnit,
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

func (h *Handler) UserOrders(w http.ResponseWriter, r *http.Request) {
	userID := h.getUserID(r)
	if userID == 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
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


