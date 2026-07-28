package models

import "time"

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
}

type Order struct {
	ID              int64     `json:"id"`
	CustomerName    string    `json:"customer_name"`
	CustomerPhone   string    `json:"customer_phone"`
	CustomerAddress string    `json:"customer_address"`
	TotalAmount     int       `json:"total_amount"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
}

type OrderItem struct {
	ID           int64 `json:"id"`
	OrderID      int64 `json:"order_id"`
	ProductID    int64 `json:"product_id"`
	Quantity     int   `json:"quantity"`
	PricePerUnit int   `json:"price_per_unit"`
}
