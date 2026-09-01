// Package models defines the core domain types shared across the application:
// Product, Order, OrderItem, and User.
package models

import "time"

// Product represents a sellable item in the farm store. Price is stored as an
// integer amount in Iranian toman to match the admin UI and storefront display.
type Product struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	Slug          string    `json:"slug"`
	Category      string    `json:"category"`
	Description   string    `json:"description"`
	Price         int       `json:"price"`
	StockQuantity int       `json:"stock_quantity"`
	Unit          string    `json:"unit"`
	ImageURL      string    `json:"image_url"`
	IsActive      bool      `json:"is_active"`
	CreatedAt     time.Time `json:"created_at"`

	// Images is a display-only gallery (ordered paths) populated by handlers
	// via the images table; it is never persisted through Product itself.
	Images []string `json:"images,omitempty"`
}

// Order represents a customer order with a TDJ-XXXXXX ID, customer details,
// total amount, and current processing status.
type Order struct {
	ID              string    `json:"id"`
	CustomerName    string    `json:"customer_name"`
	CustomerPhone   string    `json:"customer_phone"`
	CustomerAddress string    `json:"customer_address"`
	PostalCode      string    `json:"postal_code"`
	TotalAmount     int       `json:"total_amount"`
	Status          string    `json:"status"`
	PaymentRefID    int64     `json:"payment_ref_id"`
	// TrackingCode is the optional postal tracking number the admin enters
	// when marking an order dispatched (ارسال شد). Customers see it on their
	// orders page to follow the shipment.
	TrackingCode string    `json:"tracking_code"`
	CreatedAt    time.Time `json:"created_at"`
	UserID       int64     `json:"user_id"`
}

// OrderItem maps a product to its quantity and price-at-purchase within an order.
type OrderItem struct {
	ID           int64  `json:"id"`
	OrderID      string `json:"order_id"`
	ProductID    int64  `json:"product_id"`
	Quantity     int    `json:"quantity"`
	PricePerUnit int    `json:"price_per_unit"`
}

// OrderItemView is a read-only projection that joins order_items with product names
// and includes a pre-computed subtotal.
type OrderItemView struct {
	Name     string
	Quantity int
	Price    int
	Subtotal int
	Unit     string
}

// OrderSummary pairs an Order with its human-readable OrderItemViews.
type OrderSummary struct {
	Order Order
	Items []OrderItemView
}

// Category is an admin-manageable product grouping. The storefront filters
// products by the category's Label (a free-text key held on products.category),
// while the URL slug is the stable, English identifier used in routes.
type Category struct {
	ID          int64
	Slug        string
	Label       string
	Description string
	IsEnabled   bool
}

// User represents an authenticated customer identified by their phone number.
type User struct {
	ID          int64     `json:"id"`
	PhoneNumber string    `json:"phone_number"`
	CreatedAt   time.Time `json:"created_at"`
}
