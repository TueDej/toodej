package database

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"farmstore/internal/models"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Init(filepath.Join(t.TempDir(), "farmstore.db"))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func createTestUser(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	res, err := db.Exec("INSERT INTO users (phone_number) VALUES (?)", "09123456789")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("user id: %v", err)
	}
	return id
}

func TestCreateOrderPricesFromActiveProducts(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec("UPDATE products SET price = 120, stock_quantity = 5, is_active = 1 WHERE id = 1"); err != nil {
		t.Fatalf("update product: %v", err)
	}
	userID := createTestUser(t, db)

	order := &models.Order{
		CustomerName:    "Customer",
		CustomerPhone:   "09123456789",
		CustomerAddress: "Address",
		PostalCode:      "1234567890",
		TotalAmount:     999,
		Status:          "awaiting_payment",
		UserID:          userID,
	}
	orderID, err := CreateOrder(db, order, []models.OrderItem{{
		ProductID:    1,
		Quantity:     2,
		PricePerUnit: 1,
	}})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.TotalAmount != 240 {
		t.Fatalf("order total = %d, want 240", order.TotalAmount)
	}

	var storedTotal, storedPrice, stock int
	if err := db.QueryRow("SELECT total_amount FROM orders WHERE id = ?", orderID).Scan(&storedTotal); err != nil {
		t.Fatalf("query order: %v", err)
	}
	if storedTotal != 240 {
		t.Fatalf("stored total = %d, want 240", storedTotal)
	}
	if err := db.QueryRow("SELECT price_per_unit FROM order_items WHERE order_id = ? AND product_id = 1", orderID).Scan(&storedPrice); err != nil {
		t.Fatalf("query order item: %v", err)
	}
	if storedPrice != 120 {
		t.Fatalf("stored item price = %d, want 120", storedPrice)
	}
	if err := db.QueryRow("SELECT stock_quantity FROM products WHERE id = 1").Scan(&stock); err != nil {
		t.Fatalf("query stock: %v", err)
	}
	if stock != 3 {
		t.Fatalf("stock = %d, want 3", stock)
	}

	if _, err := db.Exec("UPDATE products SET is_active = 0 WHERE id = 1"); err != nil {
		t.Fatalf("deactivate product: %v", err)
	}
	_, err = CreateOrder(db, &models.Order{Status: "awaiting_payment", UserID: userID}, []models.OrderItem{{ProductID: 1, Quantity: 1}})
	if !errors.Is(err, ErrProductUnavailable) {
		t.Fatalf("CreateOrder inactive error = %v, want ErrProductUnavailable", err)
	}
}

func TestPaymentStateIsIdempotentAndDoesNotReviveCancelledOrders(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec("UPDATE products SET price = 100, stock_quantity = 5, is_active = 1 WHERE id = 1"); err != nil {
		t.Fatalf("update product: %v", err)
	}
	userID := createTestUser(t, db)

	order := &models.Order{
		CustomerName:    "Customer",
		CustomerPhone:   "09123456789",
		CustomerAddress: "Address",
		PostalCode:      "1234567890",
		Status:          "awaiting_payment",
		UserID:          userID,
	}
	orderID, err := CreateOrder(db, order, []models.OrderItem{{ProductID: 1, Quantity: 2}})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if err := SetPaymentAuthority(db, orderID, "A0001"); err != nil {
		t.Fatalf("SetPaymentAuthority: %v", err)
	}
	if err := ConfirmPayment(db, orderID, 123); err != nil {
		t.Fatalf("ConfirmPayment: %v", err)
	}
	if err := MarkPaymentFailed(db, orderID); err != nil {
		t.Fatalf("MarkPaymentFailed paid order: %v", err)
	}

	var status string
	var stock int
	if err := db.QueryRow("SELECT status FROM orders WHERE id = ?", orderID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "pending" {
		t.Fatalf("status after failed paid callback = %s, want pending", status)
	}
	if err := db.QueryRow("SELECT stock_quantity FROM products WHERE id = 1").Scan(&stock); err != nil {
		t.Fatalf("query stock: %v", err)
	}
	if stock != 3 {
		t.Fatalf("stock after failed paid callback = %d, want 3", stock)
	}

	if err := UpdateOrderStatus(db, orderID, "cancelled"); err != nil {
		t.Fatalf("cancel paid order: %v", err)
	}
	if err := ConfirmPayment(db, orderID, 456); !errors.Is(err, ErrInvalidOrderTransition) {
		t.Fatalf("ConfirmPayment cancelled error = %v, want ErrInvalidOrderTransition", err)
	}
	if err := UpdateOrderStatus(db, orderID, "pending"); !errors.Is(err, ErrInvalidOrderTransition) {
		t.Fatalf("uncancel error = %v, want ErrInvalidOrderTransition", err)
	}
	if err := db.QueryRow("SELECT stock_quantity FROM products WHERE id = 1").Scan(&stock); err != nil {
		t.Fatalf("query final stock: %v", err)
	}
	if stock != 5 {
		t.Fatalf("final stock = %d, want 5", stock)
	}
}

func TestStatusMigrationPreservesPaymentFieldsAndOrderItems(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		t.Fatalf("enable foreign keys: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE products (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			slug TEXT NOT NULL UNIQUE,
			category TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			price INTEGER NOT NULL,
			stock_quantity INTEGER NOT NULL DEFAULT 0,
			unit TEXT NOT NULL DEFAULT '',
			image_url TEXT NOT NULL DEFAULT '',
			is_active INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			phone_number TEXT NOT NULL UNIQUE,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE orders (
			id TEXT PRIMARY KEY,
			customer_name TEXT NOT NULL,
			customer_phone TEXT NOT NULL DEFAULT '',
			customer_address TEXT NOT NULL DEFAULT '',
			postal_code TEXT NOT NULL DEFAULT '',
			total_amount INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'pending'
				CHECK (status IN ('pending','processing','completed','cancelled')),
			payment_authority TEXT NOT NULL DEFAULT '',
			payment_ref_id INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE order_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			order_id TEXT NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
			product_id INTEGER NOT NULL REFERENCES products(id),
			quantity INTEGER NOT NULL DEFAULT 1,
			price_per_unit INTEGER NOT NULL,
			UNIQUE(order_id, product_id)
		);
		CREATE TABLE otp_codes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			phone_number TEXT NOT NULL,
			code TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			is_used INTEGER NOT NULL DEFAULT 0
		);
		INSERT INTO products (id, name, slug, category, price) VALUES (1, 'fig', 'fig', 'summer', 100);
		INSERT INTO orders (id, customer_name, status, payment_authority, payment_ref_id) VALUES ('TDJ-ABC123', 'Customer', 'processing', 'AUTH123', 987);
		INSERT INTO order_items (order_id, product_id, quantity, price_per_unit) VALUES ('TDJ-ABC123', 1, 2, 100);
	`)
	if err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var status, authority string
	var refID, itemCount int
	if err := db.QueryRow("SELECT status, payment_authority, payment_ref_id FROM orders WHERE id = 'TDJ-ABC123'").Scan(&status, &authority, &refID); err != nil {
		t.Fatalf("query migrated order: %v", err)
	}
	if status != "preparing" || authority != "AUTH123" || refID != 987 {
		t.Fatalf("migrated order = status %s authority %s ref %d", status, authority, refID)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM order_items WHERE order_id = 'TDJ-ABC123'").Scan(&itemCount); err != nil {
		t.Fatalf("query order items: %v", err)
	}
	if itemCount != 1 {
		t.Fatalf("item count = %d, want 1", itemCount)
	}

	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check returned violations")
	}
}

func TestGetOrder(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec("UPDATE products SET price = 100, stock_quantity = 5, is_active = 1 WHERE id = 1"); err != nil {
		t.Fatalf("update product: %v", err)
	}
	userID := createTestUser(t, db)

	order := &models.Order{
		CustomerName:    "Customer",
		CustomerPhone:   "09123456789",
		CustomerAddress: "Address",
		PostalCode:      "1234567890",
		Status:          "awaiting_payment",
		UserID:          userID,
	}
	orderID, err := CreateOrder(db, order, []models.OrderItem{{ProductID: 1, Quantity: 2}})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	got, err := GetOrder(db, orderID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if got.ID != orderID || got.UserID != userID || got.TotalAmount != order.TotalAmount || got.Status != "awaiting_payment" {
		t.Fatalf("GetOrder = %+v", got)
	}

	if _, err := GetOrder(db, "TDJ-NOPE00"); err == nil {
		t.Fatal("GetOrder on missing id succeeded")
	}
}

func TestGetAwaitingPaymentOrders(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec("UPDATE products SET price = 100, stock_quantity = 5, is_active = 1 WHERE id = 1"); err != nil {
		t.Fatalf("update product: %v", err)
	}
	userID := createTestUser(t, db)

	order := &models.Order{
		CustomerName:    "Customer",
		CustomerPhone:   "09123456789",
		CustomerAddress: "Address",
		PostalCode:      "1234567890",
		Status:          "awaiting_payment",
		UserID:          userID,
	}
	orderID, err := CreateOrder(db, order, []models.OrderItem{{ProductID: 1, Quantity: 2}})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	// No authority yet → nothing to reconcile.
	unpaid, err := GetAwaitingPaymentOrders(db)
	if err != nil {
		t.Fatalf("GetAwaitingPaymentOrders: %v", err)
	}
	if len(unpaid) != 0 {
		t.Fatalf("awaiting orders = %d, want 0", len(unpaid))
	}

	if err := SetPaymentAuthority(db, orderID, "AUTH123"); err != nil {
		t.Fatalf("SetPaymentAuthority: %v", err)
	}
	unpaid, err = GetAwaitingPaymentOrders(db)
	if err != nil {
		t.Fatalf("GetAwaitingPaymentOrders: %v", err)
	}
	if len(unpaid) != 1 || unpaid[0].ID != orderID || unpaid[0].Authority != "AUTH123" || unpaid[0].TotalAmount != order.TotalAmount {
		t.Fatalf("unpaid = %+v", unpaid)
	}

	// Paid orders are excluded.
	if err := ConfirmPayment(db, orderID, 123); err != nil {
		t.Fatalf("ConfirmPayment: %v", err)
	}
	unpaid, err = GetAwaitingPaymentOrders(db)
	if err != nil {
		t.Fatalf("GetAwaitingPaymentOrders: %v", err)
	}
	if len(unpaid) != 0 {
		t.Fatalf("awaiting after confirm = %d, want 0", len(unpaid))
	}
}

func TestCancelExpiredUnpaidOrders(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec("UPDATE products SET price = 100, stock_quantity = 10, is_active = 1 WHERE id = 1"); err != nil {
		t.Fatalf("update product: %v", err)
	}
	userID := createTestUser(t, db)

	order := &models.Order{
		CustomerName:    "Customer",
		CustomerPhone:   "09123456789",
		CustomerAddress: "Address",
		PostalCode:      "1234567890",
		Status:          "awaiting_payment",
		UserID:          userID,
	}
	orderID, err := CreateOrder(db, order, []models.OrderItem{{ProductID: 1, Quantity: 4}})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	var stock int
	if err := db.QueryRow("SELECT stock_quantity FROM products WHERE id = 1").Scan(&stock); err != nil {
		t.Fatalf("query stock: %v", err)
	}
	if stock != 6 {
		t.Fatalf("stock after order = %d, want 6", stock)
	}

	// Backdate created_at to 20 minutes ago so it exceeds the 15-minute TTL.
	if _, err := db.Exec("UPDATE orders SET created_at = datetime('now', '-20 minutes') WHERE id = ?", orderID); err != nil {
		t.Fatalf("backdate order: %v", err)
	}

	cancelledCount, err := CancelExpiredUnpaidOrders(db, 15*time.Minute)
	if err != nil {
		t.Fatalf("CancelExpiredUnpaidOrders: %v", err)
	}
	if cancelledCount != 1 {
		t.Fatalf("cancelledCount = %d, want 1", cancelledCount)
	}

	var status string
	if err := db.QueryRow("SELECT status FROM orders WHERE id = ?", orderID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "cancelled" {
		t.Fatalf("order status = %s, want cancelled", status)
	}

	if err := db.QueryRow("SELECT stock_quantity FROM products WHERE id = 1").Scan(&stock); err != nil {
		t.Fatalf("query restored stock: %v", err)
	}
	if stock != 10 {
		t.Fatalf("restored stock = %d, want 10", stock)
	}
}
