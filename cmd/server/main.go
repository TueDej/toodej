package main

import (
	"log"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"farmstore/internal/database"
	"farmstore/internal/handlers"
)

func main() {
	dbPath := "farmstore.db"
	if p := os.Getenv("DB_PATH"); p != "" {
		dbPath = p
	}

	db, err := database.Init(dbPath)
	if err != nil {
		log.Fatalf("database init: %v", err)
	}
	defer db.Close()

	cartStore := handlers.NewCartStore()
	h, err := handlers.NewHandler(db, cartStore)
	if err != nil {
		log.Fatalf("handler init: %v", err)
	}

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	adminUser := getEnv("ADMIN_USER", "admin")
	adminPass := getEnv("ADMIN_PASS", "admin123")

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	r.Get("/", h.Home)
	r.Get("/cart/count", h.CartCount)
	r.Post("/cart/add", h.AddToCart)
	r.Post("/cart/update", h.UpdateCart)
	r.Post("/cart/remove", h.RemoveFromCart)
	r.Get("/cart", h.ViewCart)
	r.Handle("/assets/*", http.StripPrefix("/assets/", http.FileServer(http.Dir("assets"))))
	r.Get("/checkout", h.CheckoutForm)
	r.Post("/checkout", h.PlaceOrder)
	r.Get("/checkout/confirmation/{id}", h.Confirmation)
	r.Get("/login", h.LoginPage)
	r.Post("/auth/send-otp", h.SendOTP)
	r.Post("/auth/verify-otp", h.VerifyOTP)
	r.Get("/logout", h.Logout)
	r.Get("/orders", h.UserOrders)

	r.Route("/admin", func(r chi.Router) {
		r.Use(handlers.BasicAuth(adminUser, adminPass))
		r.Get("/", h.AdminDashboard)
		r.Post("/orders/{id}/status", h.AdminUpdateOrderStatus)
		r.Post("/products/{id}/toggle", h.AdminToggleProduct)
		r.Post("/products/{id}", h.AdminUpdateProduct)
		r.Post("/products", h.AdminCreateProduct)
	})

	port := getEnv("PORT", "8080")
	log.Printf("server starting on :%s", port)
	if err := http.ListenAndServe(":"+port, r); err != nil {
		log.Fatal(err)
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
