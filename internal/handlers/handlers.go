package handlers

import (
	"database/sql"
	"fmt"
	"html/template"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"farmstore/internal/payment"
	"farmstore/internal/utils"
)

// Handler is the central HTTP handler. It wires together the database connection,
// HTML template store, in-memory cart store, and session management state.
type Handler struct {
	db               *sql.DB
	templates        *TemplateStore
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

// templateFuncs returns the shared template function map used by every page
// (formatPrice, persianDate, statusColor, etc.).
func templateFuncs() template.FuncMap {
	return template.FuncMap{
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
		"now":       time.Now,
		"hasSuffix": strings.HasSuffix,
	}
}

// NewHandler initialises the Handler, parsing all HTML templates with a shared
// function map (formatPrice, persianDate, etc.) from the templates/ directory.
func NewHandler(db *sql.DB, cartStore *CartStore, zarinpal *payment.Zarinpal, baseURL string) (*Handler, error) {
	funcMap := templateFuncs()

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

	// In dev mode, templates can be hot-reloaded from disk on every render and
	// a broken template at startup logs a warning instead of crashing the boot,
	// so syntax errors during development no longer require a manual restart.
	devMode := os.Getenv("DEV_MODE") == "true"
	templates := newTemplateStore(funcMap, layoutFiles, pages, devMode)
	if err := templates.load(); err != nil {
		if devMode {
			log.Printf("template parse warning (server will start and retry on each render): %v", err)
		} else {
			return nil, fmt.Errorf("parse templates: %w", err)
		}
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
	h.startUnpaidOrderJanitor()
	h.startPaymentReconciler()
	return h, nil
}
