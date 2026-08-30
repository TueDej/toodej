package database

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
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
	orderID, err := CreateOrder(context.Background(), db, order, []models.OrderItem{{
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
	_, err = CreateOrder(context.Background(), db, &models.Order{Status: "awaiting_payment", UserID: userID}, []models.OrderItem{{ProductID: 1, Quantity: 1}})
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
	orderID, err := CreateOrder(context.Background(), db, order, []models.OrderItem{{ProductID: 1, Quantity: 2}})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if err := SetPaymentAuthority(context.Background(), db, orderID, "A0001"); err != nil {
		t.Fatalf("SetPaymentAuthority: %v", err)
	}
	if err := ConfirmPayment(context.Background(), db, orderID, 123); err != nil {
		t.Fatalf("ConfirmPayment: %v", err)
	}
	if err := MarkPaymentFailed(context.Background(), db, orderID); err != nil {
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

	if err := UpdateOrderStatus(context.Background(), db, orderID, "cancelled"); err != nil {
		t.Fatalf("cancel paid order: %v", err)
	}
	if err := ConfirmPayment(context.Background(), db, orderID, 456); !errors.Is(err, ErrInvalidOrderTransition) {
		t.Fatalf("ConfirmPayment cancelled error = %v, want ErrInvalidOrderTransition", err)
	}
	if err := UpdateOrderStatus(context.Background(), db, orderID, "pending"); !errors.Is(err, ErrInvalidOrderTransition) {
		t.Fatalf("uncancel error = %v, want ErrInvalidOrderTransition", err)
	}
	if err := db.QueryRow("SELECT stock_quantity FROM products WHERE id = 1").Scan(&stock); err != nil {
		t.Fatalf("query final stock: %v", err)
	}
	if stock != 5 {
		t.Fatalf("final stock = %d, want 5", stock)
	}
}

// TestCategoryDescriptionMigrationBackfill verifies that migrating a legacy
// categories table (no description column) adds the column and backfills the
// default taglines for the seeded taxonomy, while leaving custom categories
// blank.
func TestCategoryDescriptionMigrationBackfill(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE categories (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			slug TEXT NOT NULL UNIQUE,
			label TEXT NOT NULL,
			is_enabled INTEGER NOT NULL DEFAULT 1
		);
		INSERT INTO categories (slug, label) VALUES
			('pomegranate','انار'), ('fig','انجیر'), ('custom','کاستم');
	`); err != nil {
		t.Fatalf("create legacy categories: %v", err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var pom, custom string
	if err := db.QueryRow("SELECT description FROM categories WHERE slug='pomegranate'").Scan(&pom); err != nil {
		t.Fatalf("read pomegranate: %v", err)
	}
	if pom != "انار تازه، آبدار و طبیعی." {
		t.Fatalf("pomegranate backfill = %q", pom)
	}
	if err := db.QueryRow("SELECT description FROM categories WHERE slug='custom'").Scan(&custom); err != nil {
		t.Fatalf("read custom: %v", err)
	}
	if custom != "" {
		t.Fatalf("custom category should stay blank, got %q", custom)
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

func TestSlugifyName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Apple", "apple"},
		{"Apple Sauce", "apple-sauce"},
		{"  Apple   Sauce  ", "apple-sauce"},
		{"A B  C\t \nD", "a-b-c-d"},
		{"انجیر تازه ارگانیک", "انجیر-تازه-ارگانیک"},
	}
	for _, c := range cases {
		if got := SlugifyName(c.in); got != c.want {
			t.Fatalf("SlugifyName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestUniqueSlug(t *testing.T) {
	db := testDB(t)

	slug, err := UniqueSlug(context.Background(), db, "Apple", 0)
	if err != nil {
		t.Fatalf("UniqueSlug: %v", err)
	}
	if slug != "apple" {
		t.Fatalf("first slug = %q, want apple", slug)
	}

	// A product already owns "apple": the next name mapping to the same base
	// slug must be de-duplicated with a numeric suffix.
	appleID, err := CreateProduct(context.Background(), db, &models.Product{Name: "Apple", Slug: "apple", Category: "x", Price: 1})
	if err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}
	slug, err = UniqueSlug(context.Background(), db, "Apple", 0)
	if err != nil {
		t.Fatalf("UniqueSlug: %v", err)
	}
	if slug != "apple-2" {
		t.Fatalf("second slug = %q, want apple-2", slug)
	}

	// The suffix itself is taken, so it must keep climbing.
	if _, err := CreateProduct(context.Background(), db, &models.Product{Name: "Apple", Slug: "apple-2", Category: "x", Price: 1}); err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}
	slug, err = UniqueSlug(context.Background(), db, "Apple", 0)
	if err != nil {
		t.Fatalf("UniqueSlug: %v", err)
	}
	if slug != "apple-3" {
		t.Fatalf("third slug = %q, want apple-3", slug)
	}

	// excludeID ignores a product's own slug so an update can keep it.
	slug, err = UniqueSlug(context.Background(), db, "Apple", appleID)
	if err != nil {
		t.Fatalf("UniqueSlug exclude: %v", err)
	}
	if slug != "apple" {
		t.Fatalf("excluded slug = %q, want apple", slug)
	}

	if _, err := UniqueSlug(context.Background(), db, "   ", 0); err == nil {
		t.Fatal("UniqueSlug on blank name succeeded")
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
	orderID, err := CreateOrder(context.Background(), db, order, []models.OrderItem{{ProductID: 1, Quantity: 2}})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	got, err := GetOrder(context.Background(), db, orderID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if got.ID != orderID || got.UserID != userID || got.TotalAmount != order.TotalAmount || got.Status != "awaiting_payment" {
		t.Fatalf("GetOrder = %+v", got)
	}

	if _, err := GetOrder(context.Background(), db, "TDJ-NOPE00"); err == nil {
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
	orderID, err := CreateOrder(context.Background(), db, order, []models.OrderItem{{ProductID: 1, Quantity: 2}})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	// No authority yet → nothing to reconcile.
	unpaid, err := GetAwaitingPaymentOrders(context.Background(), db)
	if err != nil {
		t.Fatalf("GetAwaitingPaymentOrders: %v", err)
	}
	if len(unpaid) != 0 {
		t.Fatalf("awaiting orders = %d, want 0", len(unpaid))
	}

	if err := SetPaymentAuthority(context.Background(), db, orderID, "AUTH123"); err != nil {
		t.Fatalf("SetPaymentAuthority: %v", err)
	}
	unpaid, err = GetAwaitingPaymentOrders(context.Background(), db)
	if err != nil {
		t.Fatalf("GetAwaitingPaymentOrders: %v", err)
	}
	if len(unpaid) != 1 || unpaid[0].ID != orderID || unpaid[0].Authority != "AUTH123" || unpaid[0].TotalAmount != order.TotalAmount {
		t.Fatalf("unpaid = %+v", unpaid)
	}

	// Paid orders are excluded.
	if err := ConfirmPayment(context.Background(), db, orderID, 123); err != nil {
		t.Fatalf("ConfirmPayment: %v", err)
	}
	unpaid, err = GetAwaitingPaymentOrders(context.Background(), db)
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
	orderID, err := CreateOrder(context.Background(), db, order, []models.OrderItem{{ProductID: 1, Quantity: 4}})
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

	cancelledCount, err := CancelExpiredUnpaidOrders(context.Background(), db, 15*time.Minute)
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

// TestCancelExpiredUnpaidOrdersKeepsFreshOrders guards against a timezone bug
// where the cutoff was formatted in the server's local time while created_at is
// stored in UTC (datetime('now')). In a non-UTC zone (e.g. +03:30) that made
// every freshly-created awaiting_payment order look hours old, so the janitor
// cancelled new orders within a single tick. The cutoff is now computed in
// SQLite's UTC clock, so a brand-new order must survive the sweep.
func TestCancelExpiredUnpaidOrdersKeepsFreshOrders(t *testing.T) {
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
	orderID, err := CreateOrder(context.Background(), db, order, []models.OrderItem{{ProductID: 1, Quantity: 4}})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	// Leave created_at at its default (now, UTC) — a brand-new order.

	cancelledCount, err := CancelExpiredUnpaidOrders(context.Background(), db, 15*time.Minute)
	if err != nil {
		t.Fatalf("CancelExpiredUnpaidOrders: %v", err)
	}
	if cancelledCount != 0 {
		t.Fatalf("cancelledCount = %d, want 0 (fresh order must survive)", cancelledCount)
	}

	var status string
	if err := db.QueryRow("SELECT status FROM orders WHERE id = ?", orderID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "awaiting_payment" {
		t.Fatalf("fresh order status = %s, want awaiting_payment", status)
	}

	// Stock must remain reserved (not restored) since nothing was cancelled.
	var stock int
	if err := db.QueryRow("SELECT stock_quantity FROM products WHERE id = 1").Scan(&stock); err != nil {
		t.Fatalf("query stock: %v", err)
	}
	if stock != 6 {
		t.Fatalf("stock = %d, want 6 (still reserved)", stock)
	}
}

func TestGetCategoriesReturnsSeed(t *testing.T) {
	db := testDB(t)
	cats, err := GetCategories(context.Background(), db)
	if err != nil {
		t.Fatalf("GetCategories: %v", err)
	}
	if len(cats) != 4 {
		t.Fatalf("category count = %d, want 4", len(cats))
	}
	enabled, err := GetEnabledCategories(context.Background(), db)
	if err != nil {
		t.Fatalf("GetEnabledCategories: %v", err)
	}
	if len(enabled) != 4 {
		t.Fatalf("enabled category count = %d, want 4", len(enabled))
	}
}

func TestGetCategoryBySlug(t *testing.T) {
	db := testDB(t)
	cat, err := GetCategoryBySlug(context.Background(), db, "fig")
	if err != nil {
		t.Fatalf("GetCategoryBySlug fig: %v", err)
	}
	if cat.Label != "انجیر" || !cat.IsEnabled {
		t.Fatalf("fig category = %+v", cat)
	}

	if _, err := GetCategoryBySlug(context.Background(), db, "nope"); err == nil {
		t.Fatal("GetCategoryBySlug missing slug succeeded")
	}
}

func TestCreateCategoryDuplicateRejected(t *testing.T) {
	db := testDB(t)
	if _, err := CreateCategory(context.Background(), db, "fresh", "تازه", ""); err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}
	// Duplicate slug must be rejected with ErrDuplicateCategory.
	if _, err := CreateCategory(context.Background(), db, "fresh", "تازه دیگر", ""); !errors.Is(err, ErrDuplicateCategory) {
		t.Fatalf("duplicate CreateCategory err = %v, want ErrDuplicateCategory", err)
	}
	// Empty slug/label must be rejected.
	if _, err := CreateCategory(context.Background(), db, "  ", "x", ""); err == nil {
		t.Fatal("CreateCategory accepted blank slug")
	}
}

// TestCategoryDescriptionRoundTrip verifies the storefront tagline is stored on
// the category, survives UpdateCategory, and is seeded for the default taxonomy.
func TestCategoryDescriptionRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// Seeded categories carry the default taglines.
	seeded, err := GetCategoryBySlug(ctx, db, "pomegranate")
	if err != nil {
		t.Fatalf("GetCategoryBySlug: %v", err)
	}
	if seeded.Description != "انار تازه، آبدار و طبیعی." {
		t.Fatalf("seeded description = %q", seeded.Description)
	}

	// Create with a description, then read it back.
	id, err := CreateCategory(ctx, db, "citrus", "مرکبات", "مرکبات تازه و آبدار.")
	if err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}
	got, err := GetCategoryBySlug(ctx, db, "citrus")
	if err != nil {
		t.Fatalf("read citrus: %v", err)
	}
	if got.ID != id || got.Description != "مرکبات تازه و آبدار." {
		t.Fatalf("created category = %+v", got)
	}

	// Update overwrites the description.
	if err := UpdateCategory(ctx, db, id, "citrus", "مرکبات", "بهاری تازه.", true); err != nil {
		t.Fatalf("UpdateCategory: %v", err)
	}
	got, _ = GetCategoryBySlug(ctx, db, "citrus")
	if got.Description != "بهاری تازه." {
		t.Fatalf("after update description = %q", got.Description)
	}
}

func TestUpdateCategoryEnabled(t *testing.T) {
	db := testDB(t)
	cat, err := GetCategoryBySlug(context.Background(), db, "test")
	if err != nil {
		t.Fatalf("get test category: %v", err)
	}
	if err := UpdateCategoryEnabled(context.Background(), db, cat.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	reload, err := GetCategoryBySlug(context.Background(), db, "test")
	if err != nil {
		t.Fatalf("reload test: %v", err)
	}
	if reload.IsEnabled {
		t.Fatal("category still enabled after disable")
	}
	enabled, err := GetEnabledCategories(context.Background(), db)
	if err != nil {
		t.Fatalf("GetEnabledCategories: %v", err)
	}
	if len(enabled) != 3 {
		t.Fatalf("enabled after disable = %d, want 3", len(enabled))
	}
}

func TestUpdateProductPersistsAllFields(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)

	p := &models.Product{
		Name:          "Fig",
		Slug:          "fig",
		Category:      "fresh",
		Description:   "sweet",
		Price:         100,
		StockQuantity: 5,
		Unit:          "1 kg",
		IsActive:      true,
	}
	id, err := CreateProduct(ctx, db, p)
	if err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}

	loaded, err := GetProduct(ctx, db, id)
	if err != nil {
		t.Fatalf("GetProduct: %v", err)
	}

	// Edit fields other than the name: everything must persist and the slug
	// must remain untouched so product URLs stay stable.
	loaded.Price = 200
	loaded.StockQuantity = 9
	loaded.Description = "extra sweet"
	loaded.Unit = "500 g"
	loaded.Category = "dried"
	if err := UpdateProduct(ctx, db, loaded); err != nil {
		t.Fatalf("UpdateProduct: %v", err)
	}
	got, err := GetProduct(ctx, db, id)
	if err != nil {
		t.Fatalf("GetProduct after update: %v", err)
	}
	if got.Price != 200 || got.StockQuantity != 9 || got.Description != "extra sweet" || got.Unit != "500 g" || got.Category != "dried" {
		t.Fatalf("fields not persisted: price=%d stock=%d desc=%q unit=%q category=%q",
			got.Price, got.StockQuantity, got.Description, got.Unit, got.Category)
	}
	if got.Slug != "fig" || got.Name != "Fig" {
		t.Fatalf("slug/name churned without rename: name=%q slug=%q", got.Name, got.Slug)
	}

	// Renaming the product must regenerate the slug.
	got.Name = "Dried Fig"
	if err := UpdateProduct(ctx, db, got); err != nil {
		t.Fatalf("UpdateProduct rename: %v", err)
	}
	renamed, err := GetProduct(ctx, db, id)
	if err != nil {
		t.Fatalf("GetProduct after rename: %v", err)
	}
	if renamed.Name != "Dried Fig" || renamed.Slug != "dried-fig" {
		t.Fatalf("rename not persisted: name=%q slug=%q", renamed.Name, renamed.Slug)
	}

	// Updating a missing product must fail.
	if err := UpdateProduct(ctx, db, &models.Product{ID: 99999, Name: "Ghost"}); err == nil {
		t.Fatal("UpdateProduct on missing product succeeded")
	}
}

// TestProductPositionOrderingAndReorder covers the drag-to-reorder backing:
// seeded products start in name order, a new product appends to the end (not by
// name), and SetProductOrder rewrites the display order for both the admin list
// and the storefront.
func TestProductPositionOrderingAndReorder(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	names := func() []string {
		ps, err := GetAllProducts(ctx, db)
		if err != nil {
			t.Fatalf("GetAllProducts: %v", err)
		}
		out := make([]string, 0, len(ps))
		for _, p := range ps {
			out = append(out, p.Name)
		}
		return out
	}
	idOf := func(name string) int64 {
		var id int64
		if err := db.QueryRowContext(ctx, "SELECT id FROM products WHERE name = ?", name).Scan(&id); err != nil {
			t.Fatalf("id of %q: %v", name, err)
		}
		return id
	}

	if got := names(); !reflect.DeepEqual(got, []string{"test1", "test2", "test3"}) {
		t.Fatalf("initial order = %v, want [test1 test2 test3]", got)
	}

	// A new product appends to the end even though its name sorts first.
	if _, err := CreateProduct(ctx, db, &models.Product{Name: "aaa", Slug: "aaa", Category: "test", Price: 100, IsActive: true}); err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}
	if got := names(); !reflect.DeepEqual(got, []string{"test1", "test2", "test3", "aaa"}) {
		t.Fatalf("order after create = %v, want new product last", got)
	}

	if err := SetProductOrder(ctx, db, []int64{idOf("aaa"), idOf("test3"), idOf("test1"), idOf("test2")}); err != nil {
		t.Fatalf("SetProductOrder: %v", err)
	}
	if got := names(); !reflect.DeepEqual(got, []string{"aaa", "test3", "test1", "test2"}) {
		t.Fatalf("order after reorder = %v", got)
	}

	ps, err := GetProducts(ctx, db, "test")
	if err != nil {
		t.Fatalf("GetProducts: %v", err)
	}
	sf := make([]string, 0, len(ps))
	for _, p := range ps {
		sf = append(sf, p.Name)
	}
	if !reflect.DeepEqual(sf, []string{"aaa", "test3", "test1", "test2"}) {
		t.Fatalf("storefront order = %v, want position order", sf)
	}
}
