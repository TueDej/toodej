package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"farmstore/internal/database"
	"farmstore/internal/logutil"
	"farmstore/internal/payment"
	"farmstore/internal/utils"
)

// Handler is the central HTTP handler. It wires together the database connection,
// HTML template store, in-memory cart store, and session management state.
type Handler struct {
	db                *sql.DB
	templates         *TemplateStore
	cartStore         *CartStore
	zarinpal          *payment.Zarinpal
	baseURL           string
	uploadDir         string // directory for admin-uploaded product/category images
	ctx               context.Context
	cancel            context.CancelFunc
	userSessions      map[string]session       // session ID → authenticated session
	pendingLogins     map[string]pendingLogin  // session ID → phone during OTP flow
	pendingNext       map[string]pendingReturn // session ID → post-login destination
	adminSessions     map[string]time.Time     // admin session ID → expiry (cookie-based admin login)
	adminUser         string                   // ADMIN_USER, checked by the admin login form
	adminPass         string                   // ADMIN_PASS, checked by the admin login form
	adminLoginLimiter *RateLimiter             // per-IP cap on admin login attempts
	otpLimiter        *RateLimiter             // per-phone cap on OTP sends
	otpSendIPLimiter  *RateLimiter             // per-IP cap on OTP sends (stops number-cycling SMS pumping)
	otpGlobalLimiter  *RateLimiter             // global cap on OTP sends (guards the SMS budget)
	otpVerifyLimiter  *RateLimiter             // per-phone cap on OTP verification attempts
	otpAttempts       *attemptTracker          // wrong-code budget + login cooldown per phone/IP
	sessionMu         sync.RWMutex
}

// statusLabels is the single source of truth for order-status display text.
// Keep this in sync with statusColors and the DB CHECK constraint.
var statusLabels = map[string]string{
	"pending":          "در انتظار بررسی",
	"preparing":        "آماده‌سازی برای ارسال",
	"dispatched":       "ارسال شد",
	"cancelled":        "لغو شده",
	"awaiting_payment": "در انتظار پرداخت",
}

// statusColors maps each order status to the design-system color token used
// consistently across the customer-facing views and the admin panel.
var statusColors = map[string]string{
	"pending":          "saffron",
	"preparing":        "fig",
	"dispatched":       "forest",
	"cancelled":        "pomegranate",
	"awaiting_payment": "saffron",
}

// statusVar returns the CSS custom-property color for a status (e.g.
// "var(--forest)"), used by the admin panel's inline status markup.
func statusVar(s string) string {
	if c, ok := statusColors[s]; ok {
		return "var(--" + c + ")"
	}
	return "var(--clay)"
}

// envDefault returns the value of an environment variable or fallback.
func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
		"comma": commaInt,
		"multiply": func(a, b int) int {
			return a * b
		},
		"statusColor": func(s string) string {
			if c, ok := statusColors[s]; ok {
				return "text-" + c
			}
			return "text-clay"
		},
		"statusLabel": func(s string) string {
			if l, ok := statusLabels[s]; ok {
				return l
			}
			return s
		},
		// statusOptions lists the statuses an order may still move to from its
		// current state (forward-only; cancelled is terminal). Used by the
		// admin panel's status <select> so backward moves are never offered.
		"statusOptions": database.ValidOrderStatusOptions,
		"statusVar":     statusVar,
		"now":           time.Now,
		"hasSuffix":     strings.HasSuffix,
	}
}

// NewHandler initialises the Handler, parsing all HTML templates with a shared
// function map (formatPrice, persianDate, etc.) from the templates/ directory.
func NewHandler(ctx context.Context, db *sql.DB, cartStore *CartStore, zarinpal *payment.Zarinpal, baseURL string) (*Handler, error) {
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
		"admin-login":  {"templates/admin-login.html"},
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
			logutil.Warn("template parse warning (server will retry on each render)", "err", err)
		} else {
			return nil, fmt.Errorf("parse templates: %w", err)
		}
	}

	ctx, cancel := context.WithCancel(ctx)

	uploadDir := sanitizeUploadDir(envDefault("UPLOAD_DIR", "uploads"))

	h := &Handler{
		db:                db,
		templates:         templates,
		cartStore:         cartStore,
		zarinpal:          zarinpal,
		baseURL:           baseURL,
		uploadDir:         uploadDir,
		ctx:               ctx,
		cancel:            cancel,
		userSessions:      make(map[string]session),
		pendingLogins:     make(map[string]pendingLogin),
		pendingNext:       make(map[string]pendingReturn),
		adminSessions:     make(map[string]time.Time),
		adminLoginLimiter: NewRateLimiter(5, time.Minute),
		otpLimiter:        NewRateLimiter(5, time.Minute),
		otpSendIPLimiter:  NewRateLimiter(10, time.Minute),
		otpGlobalLimiter:  NewRateLimiter(300, time.Minute),
		otpVerifyLimiter:  NewRateLimiter(10, time.Minute),
		otpAttempts:       newAttemptTracker(),
	}
	h.startSessionJanitor(ctx)
	h.startUnpaidOrderJanitor(ctx)
	h.startPaymentReconciler(ctx)
	return h, nil
}

// sanitizeUploadDir validates UPLOAD_DIR so a misconfigured environment cannot
// turn the public /uploads/* FileServer into a system-file reader
// (UPLOAD_DIR=/etc would otherwise serve /etc/*). Absolute system paths and
// parent-directory escapes fall back to the default "uploads" with a warning.
func sanitizeUploadDir(dir string) string {
	cleaned := filepath.Clean(strings.TrimSpace(dir))
	if cleaned == "" || cleaned == "." {
		return "uploads"
	}
	if filepath.IsAbs(cleaned) {
		for _, blocked := range []string{"/", "/etc", "/usr", "/bin", "/sbin", "/proc", "/sys", "/dev", "/var/run"} {
			if cleaned == blocked || strings.HasPrefix(cleaned, blocked+"/") {
				logutil.Warn("refusing unsafe UPLOAD_DIR; using default", "upload_dir", dir)
				return "uploads"
			}
		}
		return cleaned
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		logutil.Warn("refusing escaping UPLOAD_DIR; using default", "upload_dir", dir)
		return "uploads"
	}
	return cleaned
}
