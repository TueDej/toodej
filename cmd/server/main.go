// Command server is the entry point for the Toodej farm store e-commerce application.
// It wires together the database, handler, and Chi router, then starts the HTTP server.
package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"farmstore/internal/database"
	"farmstore/internal/handlers"
	"farmstore/internal/logutil"
	"farmstore/internal/payment"
)

func main() {
	// ── Database ──────────────────────────────────────
	dbPath := "farmstore.db"
	if p := os.Getenv("DB_PATH"); p != "" {
		dbPath = p
	}

	db, err := database.Init(dbPath)
	if err != nil {
		logutil.Fatal("database init failed", "err", err)
	}
	defer db.Close()

	// ── Payment ──────────────────────────────────────
	zarinpal := payment.NewFromEnv()
	baseURL := getEnv("APP_BASE_URL", "https://toodej.shop")

	// ── Handler ───────────────────────────────────────
	cartStore := handlers.NewCartStore()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	h, err := handlers.NewHandler(ctx, db, cartStore, zarinpal, baseURL)
	if err != nil {
		logutil.Fatal("handler init failed", "err", err)
	}

	// ── Router ────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(logutil.RequestLogger())
	r.Use(middleware.Recoverer)
	r.Use(handlers.SecurityHeaders)
	r.Use(handlers.SameOrigin)
	r.Use(handlers.CSRFMiddleware)

	// Admin credentials. In production the default admin/admin123 fallback must
	// never be used: fail fast unless explicit, non-default credentials (at least
	// 8 characters) are supplied. In DEV_MODE the defaults are accepted with a
	// warning so local development stays frictionless.
	adminUser := os.Getenv("ADMIN_USER")
	adminPass := os.Getenv("ADMIN_PASS")
	if os.Getenv("DEV_MODE") != "true" {
		if adminUser == "" || adminPass == "" || (adminUser == "admin" && adminPass == "admin123") {
			logutil.Fatal("refusing default admin credentials in production (set ADMIN_USER and ADMIN_PASS)")
		}
		if len(adminPass) < 8 {
			logutil.Fatal("ADMIN_PASS must be at least 8 characters in production")
		}
	} else {
		if adminUser == "" || adminPass == "" {
			adminUser, adminPass = "admin", "admin123"
		}
		logutil.Warn("using default admin credentials (DEV_MODE only)", "admin_user", adminUser)
	}

	// Rate limiter: per-IP budget for the admin panel.
	adminLimiter := handlers.NewRateLimiter(30, time.Minute)

	// Public routes
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	r.Get("/", h.Home)
	r.Get("/about", h.About)
	r.Get("/products/{category}", h.ProductsPage)
	r.Get("/cart/count", h.CartCount)
	r.Post("/cart/add", h.AddToCart)
	r.Post("/cart/update", h.UpdateCart)
	r.Post("/cart/remove", h.RemoveFromCart)
	r.Get("/cart", h.ViewCart)
	r.Handle("/assets/*", http.StripPrefix("/assets/", http.FileServer(http.Dir("assets"))))
	r.Get("/checkout", h.CheckoutForm)
	r.Post("/checkout/preview", h.PreviewCheckout)
	r.Post("/checkout", h.PlaceOrder)
	r.Get("/checkout/verify", h.VerifyPayment)
	r.Get("/checkout/confirmation/{id}", h.Confirmation)
	r.Get("/login", h.LoginPage)
	r.Post("/auth/send-otp", h.SendOTP)
	r.Post("/auth/verify-otp", h.VerifyOTP)
	r.Get("/logout", h.Logout)
	r.Get("/orders", h.UserOrders)
	r.Post("/orders/{id}/pay", h.ResumePayment)
	r.Get("/sitemap.xml", h.ServeSitemap)
	r.Get("/robots.txt", handlers.ServeRobotsTXT)

	// Admin routes (protected by HTTP Basic Auth)
	r.Route("/admin", func(r chi.Router) {
		r.Use(handlers.BasicAuth(adminUser, adminPass))
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

	// Admin-uploaded product/category images.
	uploadDir := os.Getenv("UPLOAD_DIR")
	if uploadDir == "" {
		uploadDir = "uploads"
	}
	r.Handle("/uploads/*", http.StripPrefix("/uploads/", http.FileServer(http.Dir(uploadDir))))

	// ── Start ─────────────────────────────────────────
	port := getEnv("PORT", "8080")
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           r,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	logutil.Info("server starting", "port", port)

	// ListenAndServe blocks until the server is shut down. Run it in a
	// goroutine so the main goroutine can wait for the interrupt signal.
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	// Block until SIGINT (Ctrl-C) arrives.
	<-ctx.Done()
	logutil.Info("shutting down...")

	// Give in-flight requests up to 10 seconds to complete.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logutil.Fatal("shutdown error", "err", err)
	}
	if err := <-errCh; err != nil && err != http.ErrServerClosed {
		logutil.Fatal("server stopped", "err", err)
	}
}

// getEnv returns the value of an environment variable, or a fallback default if
// the variable is empty or not set.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
