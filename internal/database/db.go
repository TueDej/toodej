// Package database provides SQLite database initialisation, schema migration,
// data seeding, and all query functions for products, orders, users, and OTP codes.
package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"farmstore/internal/logutil"
	"farmstore/internal/models"
)

// Category constants for the five product categories on the storefront.
const (
	CategorySpring    = "بهار"
	CategorySummer    = "تابستان"
	CategoryAutumn    = "پاییز"
	CategoryDried     = "خشکبار"
	CategoryProcessed = "سنتی"
)

// ErrInsufficientStock is returned by CreateOrder when an ordered quantity
// exceeds the remaining stock of a product. The enclosing transaction is rolled
// back, so no partial stock changes or order rows are persisted.
var ErrInsufficientStock = errors.New("insufficient stock")

// ErrProductUnavailable is returned by CreateOrder when a cart references a
// missing, inactive, or invalid product line.
var ErrProductUnavailable = errors.New("product unavailable")

// ErrInvalidOrderTransition is returned when an order status change would break
// the payment/inventory state machine.
var ErrInvalidOrderTransition = errors.New("invalid order status transition")

// Init opens (or creates) the SQLite database at dbPath, enables WAL mode and
// foreign keys, runs migrations, and seeds initial data if the database is empty.
func Init(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// WAL mode improves concurrent read performance and is safe in a single-server deployment.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("enable wal: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	if err := seed(db); err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}

	return db, nil
}

// migrate creates all tables if they do not already exist.
// It uses a best-effort ALTER TABLE to add the user_id column to orders for
// backward-compatibility with databases created before that column existed.
func migrate(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS products (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		name          TEXT    NOT NULL,
		slug          TEXT    NOT NULL UNIQUE,
		category      TEXT    NOT NULL,
		description   TEXT    NOT NULL DEFAULT '',
		price         INTEGER NOT NULL,
		stock_quantity INTEGER NOT NULL DEFAULT 0,
		unit          TEXT    NOT NULL DEFAULT '',
		image_url     TEXT    NOT NULL DEFAULT '',
		is_active     INTEGER NOT NULL DEFAULT 1,
		created_at    TEXT    NOT NULL DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS users (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		phone_number TEXT    NOT NULL UNIQUE,
		created_at   TEXT    NOT NULL DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS orders (
		id              TEXT    PRIMARY KEY,
		customer_name    TEXT    NOT NULL,
		customer_phone   TEXT    NOT NULL DEFAULT '',
		customer_address TEXT    NOT NULL DEFAULT '',
		postal_code      TEXT    NOT NULL DEFAULT '',
		total_amount    INTEGER NOT NULL DEFAULT 0,
		status          TEXT    NOT NULL DEFAULT 'pending'
			CHECK (status IN ('pending','preparing','dispatched','cancelled','awaiting_payment')),
		payment_authority TEXT   NOT NULL DEFAULT '',
		payment_ref_id   INTEGER NOT NULL DEFAULT 0,
		user_id        INTEGER REFERENCES users(id),
		created_at      TEXT    NOT NULL DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS order_items (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		order_id       TEXT    NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
		product_id     INTEGER NOT NULL REFERENCES products(id),
		quantity       INTEGER NOT NULL DEFAULT 1,
		price_per_unit INTEGER NOT NULL,
		UNIQUE(order_id, product_id)
	);

	CREATE TABLE IF NOT EXISTS otp_codes (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		phone_number TEXT    NOT NULL,
		code         TEXT    NOT NULL,
		expires_at   TEXT    NOT NULL,
		is_used      INTEGER NOT NULL DEFAULT 0
	);
	`
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}

	if err := ensureOrderColumn(db, "postal_code", "ALTER TABLE orders ADD COLUMN postal_code TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureOrderColumn(db, "payment_authority", "ALTER TABLE orders ADD COLUMN payment_authority TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureOrderColumn(db, "payment_ref_id", "ALTER TABLE orders ADD COLUMN payment_ref_id INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureOrderColumn(db, "user_id", "ALTER TABLE orders ADD COLUMN user_id INTEGER REFERENCES users(id)"); err != nil {
		return err
	}

	// Migration: update status CHECK constraint to include new statuses.
	// SQLite does not support ALTER TABLE for CHECK constraints, so we
	// recreate the orders table with the updated constraint.
	var checkSQL string
	err = db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='orders'").Scan(&checkSQL)
	if err == nil && !strings.Contains(checkSQL, "awaiting_payment") {
		if err := migrateOrderStatusConstraint(db); err != nil {
			return err
		}
	}

	return nil
}

func ensureOrderColumn(db *sql.DB, name, ddl string) error {
	exists, err := orderColumnExists(db, name)
	if err != nil {
		return fmt.Errorf("check orders.%s: %w", name, err)
	}
	if exists {
		return nil
	}
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("add orders.%s: %w", name, err)
	}
	return nil
}

func orderColumnExists(db *sql.DB, name string) (bool, error) {
	rows, err := db.Query("PRAGMA table_info(orders)")
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var colName, colType string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &colName, &colType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if colName == name {
			return true, nil
		}
	}
	return false, rows.Err()
}

func migrateOrderStatusConstraint(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get migration conn: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return fmt.Errorf("disable foreign keys: %w", err)
	}
	defer conn.ExecContext(ctx, "PRAGMA foreign_keys=ON")

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin status migration: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE orders_new (
		id              TEXT    PRIMARY KEY,
		customer_name    TEXT    NOT NULL,
		customer_phone   TEXT    NOT NULL DEFAULT '',
		customer_address TEXT    NOT NULL DEFAULT '',
		postal_code      TEXT    NOT NULL DEFAULT '',
		total_amount    INTEGER NOT NULL DEFAULT 0,
		status          TEXT    NOT NULL DEFAULT 'pending'
			CHECK (status IN ('pending','preparing','dispatched','cancelled','awaiting_payment')),
		payment_authority TEXT   NOT NULL DEFAULT '',
		payment_ref_id   INTEGER NOT NULL DEFAULT 0,
		user_id        INTEGER REFERENCES users(id),
		created_at      TEXT    NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return fmt.Errorf("create orders_new: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO orders_new (
			id, customer_name, customer_phone, customer_address, postal_code,
			total_amount, status, payment_authority, payment_ref_id, user_id, created_at
		)
		SELECT id, customer_name, customer_phone, customer_address, postal_code,
			total_amount,
			CASE status
				WHEN 'processing' THEN 'preparing'
				WHEN 'completed' THEN 'dispatched'
				ELSE status
			END,
			payment_authority, payment_ref_id, user_id, created_at
		FROM orders`); err != nil {
		return fmt.Errorf("copy orders: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `CREATE TABLE order_items_new (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		order_id       TEXT    NOT NULL REFERENCES orders_new(id) ON DELETE CASCADE,
		product_id     INTEGER NOT NULL REFERENCES products(id),
		quantity       INTEGER NOT NULL DEFAULT 1,
		price_per_unit INTEGER NOT NULL,
		UNIQUE(order_id, product_id)
	)`); err != nil {
		return fmt.Errorf("create order_items_new: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO order_items_new (id, order_id, product_id, quantity, price_per_unit)
		SELECT id, order_id, product_id, quantity, price_per_unit FROM order_items`); err != nil {
		return fmt.Errorf("copy order_items: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DROP TABLE order_items"); err != nil {
		return fmt.Errorf("drop old order_items: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DROP TABLE orders"); err != nil {
		return fmt.Errorf("drop old orders: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "ALTER TABLE orders_new RENAME TO orders"); err != nil {
		return fmt.Errorf("rename orders_new: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "ALTER TABLE order_items_new RENAME TO order_items"); err != nil {
		return fmt.Errorf("rename order_items_new: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_orders_user_id ON orders(user_id)"); err != nil {
		return fmt.Errorf("create orders user index: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit status migration: %w", err)
	}

	rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("foreign key check failed after orders migration")
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("foreign key check rows: %w", err)
	}
	return nil
}

// seed populates the products table with initial inventory when the table is empty.
// This runs once per fresh database so the storefront is never empty on first launch.
func seed(db *sql.DB) error {
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM products").Scan(&count); err != nil {
		return fmt.Errorf("count products: %w", err)
	}
	if count > 0 {
		return nil
	}

	products := []struct {
		Name, Slug, Category, Description string
		Price, Stock                      int
		Unit                              string
	}{
		{"انجیر تازه ارگانیک", "انجیر-تازه-ارگانیک", "تابستان", "شیرین، نرم و پر از فیبر.", 1299000, 50, "۱ کیلوگرم"},
		{"انار تازه ملس", "انار-تازه-ملس", "پاییز", "شیرین، آبدار و پر از آنتی‌اکسیدان.", 899000, 60, "۱ کیلوگرم"},
		{"مربای انجیر خانگی", "مربای-انجیر-خانگی", "سنتی", "آهسته‌پز با انجیر طبیعی و بدون افزودنی.", 950000, 30, "شیشه ۲۵۰ گرمی"},
		{"رب انار خالص", "رب-انار-خالص", "سنتی", "غلیظ و ترش و شیرین؛ عالی برای سس و خورشت.", 1200000, 25, "بطری ۵۰۰ میلی‌لیتر"},
		{"آب انار طبیعی", "آب-انار-طبیعی", "سنتی", "تازه و بدون شکر و مواد افزودنی.", 790000, 40, "بطری ۵۰۰ میلی‌لیتر"},
		{"انجیر خشک اعلی", "انجیر-خشک-اعلی", "خشکبار", "خشک شده زیر آفتاب، شیرین و طبیعی.", 1899000, 40, "۵۰۰ گرم"},
		{"پسته اکبری", "پسته-اکبری", "خشکبار", "مرغوب، خوش‌رنگ و خوش‌طعم.", 3490000, 20, "۲۵۰ گرم"},
		{"به‌لیمو تازه", "به‌لیمو-تازه", "بهار", "عطر دل‌انگیز بهاری، مناسب دمنوش و غذا.", 450000, 35, "۲۵۰ گرم"},
		{"نعناع تازه", "نعناع-تازه", "بهار", "سبز، خوش‌عطر و مناسب تزئین و دمنوش.", 350000, 50, "دسته"},
		{"هلو تازه", "هلو-تازه", "تابستان", "آبدار و شیرین، رسیده و خوش‌طعم.", 750000, 45, "۱ کیلوگرم"},
		{"سیب قرمز", "سیب-قرمز", "پاییز", "ترش و شیرین، ترد و تازه.", 650000, 60, "۱ کیلوگرم"},
		{"مغز گردو", "مغز-گردو", "خشکبار", "تازه و خوش‌طعم، بدون نمک.", 2100000, 30, "۲۵۰ گرم"},
		{"ترشی مخلوط", "ترشی-مخلوط", "سنتی", "خانگی و آهسته‌پز، طعم اصیل.", 680000, 25, "شیشه ۵۰۰ گرمی"},
		{"گیلاس تازه", "گیلاس-تازه", "بهار", "شیرین و رسیده، بهترین کیفیت فصل.", 1200000, 30, "۱ کیلوگرم"},
		{"طالبی", "طالبی", "تابستان", "شیرین و آبدار، مناسب سالاد و دسر.", 550000, 40, "۱ کیلوگرم"},
		{"کدو حلوایی", "کدو-حلوایی", "پاییز", "شیرین و مناسب پخت و پز.", 480000, 35, "۱ کیلوگرم"},
	}

	stmt, err := db.Prepare(`INSERT INTO products (name, slug, category, description, price, stock_quantity, unit) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()

	for _, p := range products {
		if _, err := stmt.Exec(p.Name, p.Slug, p.Category, p.Description, p.Price, p.Stock, p.Unit); err != nil {
			return fmt.Errorf("insert %q: %w", p.Name, err)
		}
		logutil.Info("seeded product", "name", p.Name)
	}

	return nil
}

// GetProducts returns active products, optionally filtered by category.
// An empty or "all" category returns every active product.
func GetProducts(ctx context.Context, db *sql.DB, category string) ([]models.Product, error) {
	query := "SELECT id, name, slug, category, description, price, stock_quantity, unit, image_url, is_active, created_at FROM products WHERE is_active = 1"
	args := []interface{}{}
	if category != "" && category != "all" {
		query += " AND category = ?"
		args = append(args, category)
	}
	query += " ORDER BY name"

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query products: %w", err)
	}
	defer rows.Close()

	var products []models.Product
	for rows.Next() {
		p, err := scanProductRow(rows)
		if err != nil {
			return nil, err
		}
		products = append(products, p)
	}
	return products, rows.Err()
}

// GetProduct returns a single product by its primary key.
func GetProduct(ctx context.Context, db *sql.DB, id int64) (*models.Product, error) {
	row := db.QueryRowContext(ctx, "SELECT id, name, slug, category, description, price, stock_quantity, unit, image_url, is_active, created_at FROM products WHERE id = ?", id)
	p, err := scanProductRow(row)
	if err != nil {
		return nil, fmt.Errorf("get product %d: %w", id, err)
	}
	return &p, nil
}

// GetProductsByIDs returns active products matching the given IDs in a single query.
// This eliminates N+1 query patterns when fetching products for order items or cart refresh.
func GetProductsByIDs(ctx context.Context, db *sql.DB, ids []int64) ([]models.Product, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf("SELECT id, name, slug, category, description, price, stock_quantity, unit, image_url, is_active, created_at FROM products WHERE id IN (%s)", strings.Join(placeholders, ","))

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query products by ids: %w", err)
	}
	defer rows.Close()

	var products []models.Product
	for rows.Next() {
		p, err := scanProductRow(rows)
		if err != nil {
			return nil, err
		}
		products = append(products, p)
	}
	return products, rows.Err()
}

// GetOrders returns all orders ordered by newest first.
func GetOrders(ctx context.Context, db *sql.DB) ([]models.Order, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, customer_name, customer_phone, customer_address, postal_code, total_amount, status, created_at FROM orders ORDER BY created_at DESC")
	if err != nil {
		return nil, fmt.Errorf("query orders: %w", err)
	}
	defer rows.Close()

	var orders []models.Order
	for rows.Next() {
		var o models.Order
		var createdAt string
		if err := rows.Scan(&o.ID, &o.CustomerName, &o.CustomerPhone, &o.CustomerAddress, &o.PostalCode, &o.TotalAmount, &o.Status, &createdAt); err != nil {
			return nil, fmt.Errorf("scan order: %w", err)
		}
		o.CreatedAt = parseTime(createdAt)
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

// UpdateOrderStatus changes the status of an order by its TDJ-XXXXXX ID.
// When transitioning to "cancelled", stock is atomically restored for all items.
func UpdateOrderStatus(ctx context.Context, db *sql.DB, orderID string, status string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Fetch current status to avoid double-restoring stock.
	var currentStatus string
	err = tx.QueryRowContext(ctx, "SELECT status FROM orders WHERE id = ?", orderID).Scan(&currentStatus)
	if err == sql.ErrNoRows {
		return fmt.Errorf("order %s not found", orderID)
	}
	if err != nil {
		return fmt.Errorf("query order %s: %w", orderID, err)
	}
	if !validOrderTransition(currentStatus, status) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidOrderTransition, currentStatus, status)
	}

	if _, err := tx.ExecContext(ctx, "UPDATE orders SET status = ? WHERE id = ?", status, orderID); err != nil {
		return fmt.Errorf("update order %s status: %w", orderID, err)
	}

	// Restore stock only when transitioning to cancelled from a non-cancelled state.
	if status == "cancelled" && currentStatus != "cancelled" {
		rows, err := tx.QueryContext(ctx, "SELECT product_id, quantity FROM order_items WHERE order_id = ?", orderID)
		if err != nil {
			return fmt.Errorf("query order items: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var productID, quantity int
			if err := rows.Scan(&productID, &quantity); err != nil {
				return fmt.Errorf("scan order item: %w", err)
			}
			if _, err := tx.ExecContext(ctx, "UPDATE products SET stock_quantity = stock_quantity + ? WHERE id = ?", quantity, productID); err != nil {
				return fmt.Errorf("restore stock for product %d: %w", productID, err)
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate order items: %w", err)
		}
	}

	return tx.Commit()
}

func validOrderTransition(currentStatus, nextStatus string) bool {
	if currentStatus == nextStatus {
		return true
	}
	switch currentStatus {
	case "awaiting_payment":
		return nextStatus == "pending" || nextStatus == "cancelled"
	case "pending":
		return nextStatus == "preparing" || nextStatus == "cancelled"
	case "preparing":
		return nextStatus == "dispatched" || nextStatus == "cancelled"
	case "dispatched":
		return nextStatus == "cancelled"
	case "cancelled":
		return false
	default:
		return false
	}
}

// SetPaymentAuthority stores the Zarinpal authority token on an order.
func SetPaymentAuthority(ctx context.Context, db *sql.DB, orderID, authority string) error {
	res, err := db.ExecContext(ctx, "UPDATE orders SET payment_authority = ? WHERE id = ? AND status = 'awaiting_payment'", authority, orderID)
	if err != nil {
		return fmt.Errorf("set payment authority for %s: %w", orderID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: cannot set payment authority for %s", ErrInvalidOrderTransition, orderID)
	}
	return nil
}

// GetOrderByAuthority looks up an order by its Zarinpal authority token.
func GetOrderByAuthority(ctx context.Context, db *sql.DB, authority string) (*models.Order, error) {
	var o models.Order
	var createdAt string
	var userID sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT id, customer_name, customer_phone, customer_address, postal_code,
		total_amount, status, payment_ref_id, user_id, created_at
		FROM orders WHERE payment_authority = ?`, authority).
		Scan(&o.ID, &o.CustomerName, &o.CustomerPhone, &o.CustomerAddress, &o.PostalCode,
			&o.TotalAmount, &o.Status, &o.PaymentRefID, &userID, &createdAt)
	if err != nil {
		return nil, fmt.Errorf("get order by authority: %w", err)
	}
	o.CreatedAt = parseTime(createdAt)
	o.UserID = userID.Int64
	return &o, nil
}

// ConfirmPayment marks an awaiting-payment order as pending (paid) and stores the
// Zarinpal ref ID. Already-paid orders are treated as idempotent callbacks.
func ConfirmPayment(ctx context.Context, db *sql.DB, orderID string, refID int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin confirm payment: %w", err)
	}
	defer tx.Rollback()

	var currentStatus string
	var currentRefID int64
	err = tx.QueryRowContext(ctx, "SELECT status, payment_ref_id FROM orders WHERE id = ?", orderID).Scan(&currentStatus, &currentRefID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("order %s not found", orderID)
	}
	if err != nil {
		return fmt.Errorf("query payment status for %s: %w", orderID, err)
	}

	switch currentStatus {
	case "awaiting_payment":
		if _, err := tx.ExecContext(ctx, "UPDATE orders SET status = 'pending', payment_ref_id = ? WHERE id = ? AND status = 'awaiting_payment'", refID, orderID); err != nil {
			return fmt.Errorf("confirm payment for %s: %w", orderID, err)
		}
	case "pending", "preparing", "dispatched":
		if currentRefID == 0 && refID != 0 {
			if _, err := tx.ExecContext(ctx, "UPDATE orders SET payment_ref_id = ? WHERE id = ? AND payment_ref_id = 0", refID, orderID); err != nil {
				return fmt.Errorf("store payment ref for %s: %w", orderID, err)
			}
		}
	default:
		return fmt.Errorf("%w: cannot confirm payment for %s in status %s", ErrInvalidOrderTransition, orderID, currentStatus)
	}

	return tx.Commit()
}

// MarkPaymentFailed sets order status to cancelled when payment verification fails.
// Stock is restored only for orders that are still awaiting payment.
func MarkPaymentFailed(ctx context.Context, db *sql.DB, orderID string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mark payment failed: %w", err)
	}
	defer tx.Rollback()

	var currentStatus string
	err = tx.QueryRowContext(ctx, "SELECT status FROM orders WHERE id = ?", orderID).Scan(&currentStatus)
	if err == sql.ErrNoRows {
		return fmt.Errorf("order %s not found", orderID)
	}
	if err != nil {
		return fmt.Errorf("query order %s: %w", orderID, err)
	}
	if currentStatus != "awaiting_payment" {
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, "UPDATE orders SET status = 'cancelled' WHERE id = ? AND status = 'awaiting_payment'", orderID); err != nil {
		return fmt.Errorf("cancel unpaid order %s: %w", orderID, err)
	}

	rows, err := tx.QueryContext(ctx, "SELECT product_id, quantity FROM order_items WHERE order_id = ?", orderID)
	if err != nil {
		return fmt.Errorf("query order items: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var productID, quantity int
		if err := rows.Scan(&productID, &quantity); err != nil {
			return fmt.Errorf("scan order item: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE products SET stock_quantity = stock_quantity + ? WHERE id = ?", quantity, productID); err != nil {
			return fmt.Errorf("restore stock for product %d: %w", productID, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate order items: %w", err)
	}

	return tx.Commit()
}

// CancelExpiredUnpaidOrders cancels all orders in 'awaiting_payment' state that were created
// older than ttl ago, and restores stock for all items in those orders.
func CancelExpiredUnpaidOrders(ctx context.Context, db *sql.DB, ttl time.Duration) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin cancel expired tx: %w", err)
	}
	defer tx.Rollback()

	cutoff := time.Now().Add(-ttl).Format("2006-01-02 15:04:05")

	rows, err := tx.QueryContext(ctx, "SELECT id FROM orders WHERE status = 'awaiting_payment' AND created_at < ?", cutoff)
	if err != nil {
		return 0, fmt.Errorf("query expired awaiting_payment orders: %w", err)
	}

	var expiredOrderIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired order id: %w", err)
		}
		expiredOrderIDs = append(expiredOrderIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if len(expiredOrderIDs) == 0 {
		return 0, nil
	}

	for _, orderID := range expiredOrderIDs {
		if _, err := tx.ExecContext(ctx, "UPDATE orders SET status = 'cancelled' WHERE id = ? AND status = 'awaiting_payment'", orderID); err != nil {
			return 0, fmt.Errorf("cancel expired order %s: %w", orderID, err)
		}

		itemRows, err := tx.QueryContext(ctx, "SELECT product_id, quantity FROM order_items WHERE order_id = ?", orderID)
		if err != nil {
			return 0, fmt.Errorf("query order items for %s: %w", orderID, err)
		}

		type stockRestore struct {
			productID int
			quantity  int
		}
		var restores []stockRestore
		for itemRows.Next() {
			var sr stockRestore
			if err := itemRows.Scan(&sr.productID, &sr.quantity); err != nil {
				itemRows.Close()
				return 0, fmt.Errorf("scan order item for %s: %w", orderID, err)
			}
			restores = append(restores, sr)
		}
		itemRows.Close()
		if err := itemRows.Err(); err != nil {
			return 0, err
		}

		for _, sr := range restores {
			if _, err := tx.ExecContext(ctx, "UPDATE products SET stock_quantity = stock_quantity + ? WHERE id = ?", sr.quantity, sr.productID); err != nil {
				return 0, fmt.Errorf("restore stock for product %d on order %s: %w", sr.productID, orderID, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit cancel expired: %w", err)
	}

	return len(expiredOrderIDs), nil
}

// GetAllProducts returns every product (including inactive ones), ordered by name.
// Used by the admin panel.
func GetAllProducts(ctx context.Context, db *sql.DB) ([]models.Product, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, name, slug, category, description, price, stock_quantity, unit, image_url, is_active, created_at FROM products ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("query all products: %w", err)
	}
	defer rows.Close()

	var products []models.Product
	for rows.Next() {
		p, err := scanProductRow(rows)
		if err != nil {
			return nil, err
		}
		products = append(products, p)
	}
	return products, rows.Err()
}

// UpdateProduct updates price, stock_quantity, and is_active for a given product.
func UpdateProduct(ctx context.Context, db *sql.DB, p *models.Product) error {
	active := 0
	if p.IsActive {
		active = 1
	}
	res, err := db.ExecContext(ctx, "UPDATE products SET price = ?, stock_quantity = ?, is_active = ? WHERE id = ?",
		p.Price, p.StockQuantity, active, p.ID)
	if err != nil {
		return fmt.Errorf("update product %d: %w", p.ID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("product %d not found", p.ID)
	}
	return nil
}

// CreateProduct inserts a new product and returns its auto-generated ID.
func CreateProduct(ctx context.Context, db *sql.DB, p *models.Product) (int64, error) {
	active := 0
	if p.IsActive {
		active = 1
	}
	res, err := db.ExecContext(ctx, `INSERT INTO products (name, slug, category, description, price, stock_quantity, unit, image_url, is_active) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.Slug, p.Category, p.Description, p.Price, p.StockQuantity, p.Unit, p.ImageURL, active)
	if err != nil {
		return 0, fmt.Errorf("create product: %w", err)
	}
	return res.LastInsertId()
}

// SlugifyName converts a product name into a URL-safe slug: lowercased with
// runs of whitespace collapsed into single dashes. Collapsing whitespace makes
// the mapping deterministic so names differing only in space runs/case map to
// the same base slug, which UniqueSlug then de-duplicates.
func SlugifyName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), "-"))
}

// UniqueSlug returns a slug for name that does not collide with any existing
// product slug. If the base slug is already taken it appends -2, -3, ... until
// a free one is found, so slug collisions (products.slug is UNIQUE) never
// surface as an insert error to the admin. excludeID, when non-zero, omits that
// product from the collision check.
func UniqueSlug(ctx context.Context, db *sql.DB, name string, excludeID int64) (string, error) {
	base := SlugifyName(name)
	if base == "" {
		return "", fmt.Errorf("cannot derive slug from empty name")
	}
	taken := func(candidate string) (bool, error) {
		var n int
		err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM products WHERE slug = ? AND id != ?", candidate, excludeID).Scan(&n)
		if err != nil {
			return false, fmt.Errorf("check slug %q: %w", candidate, err)
		}
		return n > 0, nil
	}
	if ok, err := taken(base); err != nil {
		return "", err
	} else if !ok {
		return base, nil
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if ok, err := taken(candidate); err != nil {
			return "", err
		} else if !ok {
			return candidate, nil
		}
	}
}

// scanProductRow is a helper that scans a product row from either a *sql.Row or *sql.Rows
// into a models.Product, converting the integer is_active flag to bool.
func scanProductRow(s interface {
	Scan(dest ...interface{}) error
}) (models.Product, error) {
	var p models.Product
	var isActive int
	var createdAt string
	if err := s.Scan(&p.ID, &p.Name, &p.Slug, &p.Category, &p.Description, &p.Price, &p.StockQuantity, &p.Unit, &p.ImageURL, &isActive, &createdAt); err != nil {
		return p, err
	}
	p.IsActive = isActive == 1
	p.CreatedAt = parseTime(createdAt)
	return p, nil
}

// parseTime parses an SQLite datetime string into time.Time.
// Returns time.Now() on parse failure so templates never receive a zero time.
func parseTime(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		return time.Now()
	}
	return t
}

// randomOrderID generates a cryptographically random order ID in the format
// TDJ-XXXXXX where each X is A-Z or 0-9. The ID is unpredictable (used crypto/rand)
// so customers cannot enumerate orders.
func randomOrderID() string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 6)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[n.Int64()]
	}
	return "TDJ-" + string(b)
}

// CreateOrder inserts an order and its items inside a single transaction.
// The order ID is auto-generated via randomOrderID.
func CreateOrder(ctx context.Context, db *sql.DB, o *models.Order, items []models.OrderItem) (string, error) {
	o.ID = randomOrderID()
	if len(items) == 0 {
		return "", ErrProductUnavailable
	}

	type pricedItem struct {
		productID    int64
		quantity     int
		pricePerUnit int
	}
	productOrder := make([]int64, 0, len(items))
	quantities := make(map[int64]int, len(items))
	maxInt := int(^uint(0) >> 1)
	for _, item := range items {
		if item.ProductID <= 0 || item.Quantity <= 0 {
			return "", fmt.Errorf("%w (product id %d)", ErrProductUnavailable, item.ProductID)
		}
		if _, ok := quantities[item.ProductID]; !ok {
			productOrder = append(productOrder, item.ProductID)
		}
		if item.Quantity > maxInt-quantities[item.ProductID] {
			return "", fmt.Errorf("quantity overflow for product id %d", item.ProductID)
		}
		quantities[item.ProductID] += item.Quantity
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Atomically reserve stock: the guarded UPDATE only decrements when enough
	// inventory remains, so concurrent orders cannot oversell a product or drive
	// stock below zero. Any failure rolls the whole order back.
	stockStmt, err := tx.PrepareContext(ctx, `UPDATE products SET stock_quantity = stock_quantity - ? WHERE id = ? AND is_active = 1 AND stock_quantity >= ?`)
	if err != nil {
		return "", fmt.Errorf("prepare stock update: %w", err)
	}
	defer stockStmt.Close()

	var pricedItems []pricedItem
	totalAmount := 0
	for _, productID := range productOrder {
		quantity := quantities[productID]
		var price int
		var isActive int
		err := tx.QueryRowContext(ctx, "SELECT price, is_active FROM products WHERE id = ?", productID).Scan(&price, &isActive)
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("%w (product id %d)", ErrProductUnavailable, productID)
		}
		if err != nil {
			return "", fmt.Errorf("query product %d: %w", productID, err)
		}
		if isActive != 1 {
			return "", fmt.Errorf("%w (product id %d)", ErrProductUnavailable, productID)
		}
		if price > maxInt/quantity {
			return "", fmt.Errorf("line total overflow for product id %d", productID)
		}
		lineTotal := price * quantity
		if lineTotal > maxInt-totalAmount {
			return "", fmt.Errorf("order total overflow")
		}

		res, err := stockStmt.Exec(quantity, productID, quantity)
		if err != nil {
			return "", fmt.Errorf("decrement stock: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return "", fmt.Errorf("%w (product id %d)", ErrInsufficientStock, productID)
		}

		totalAmount += lineTotal
		pricedItems = append(pricedItems, pricedItem{productID: productID, quantity: quantity, pricePerUnit: price})
	}
	o.TotalAmount = totalAmount

	_, err = tx.ExecContext(ctx, `INSERT INTO orders (id, customer_name, customer_phone, customer_address, postal_code, total_amount, status, user_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		o.ID, o.CustomerName, o.CustomerPhone, o.CustomerAddress, o.PostalCode, o.TotalAmount, o.Status, o.UserID)
	if err != nil {
		return "", fmt.Errorf("insert order: %w", err)
	}

	itemStmt, err := tx.PrepareContext(ctx, `INSERT INTO order_items (order_id, product_id, quantity, price_per_unit) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return "", fmt.Errorf("prepare order_items: %w", err)
	}
	defer itemStmt.Close()

	for _, item := range pricedItems {
		if _, err := itemStmt.Exec(o.ID, item.productID, item.quantity, item.pricePerUnit); err != nil {
			return "", fmt.Errorf("insert order_item: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit tx: %w", err)
	}
	return o.ID, nil
}

// GetOrder retrieves a single order by its TDJ-XXXXXX ID, including the owning
// user ID so callers can enforce ownership before acting on the order.
func GetOrder(ctx context.Context, db *sql.DB, orderID string) (*models.Order, error) {
	var o models.Order
	var createdAt string
	var userID sql.NullInt64
	err := db.QueryRowContext(ctx, "SELECT id, customer_name, customer_phone, customer_address, postal_code, total_amount, status, payment_ref_id, user_id, created_at FROM orders WHERE id = ?", orderID).
		Scan(&o.ID, &o.CustomerName, &o.CustomerPhone, &o.CustomerAddress, &o.PostalCode, &o.TotalAmount, &o.Status, &o.PaymentRefID, &userID, &createdAt)
	if err != nil {
		return nil, fmt.Errorf("get order %s: %w", orderID, err)
	}
	o.CreatedAt = parseTime(createdAt)
	o.UserID = userID.Int64
	return &o, nil
}

// PaymentOrder is the minimal order projection the payment reconciliation job
// needs to verify outstanding payments against the gateway.
type PaymentOrder struct {
	ID          string
	TotalAmount int
	Authority   string
}

// GetAwaitingPaymentOrders returns orders still in the awaiting_payment state
// that have a stored Zarinpal authority token. These are exactly the orders a
// reconciliation job must check with the gateway to rescue payments whose
// callback was lost.
func GetAwaitingPaymentOrders(ctx context.Context, db *sql.DB) ([]PaymentOrder, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, total_amount, payment_authority FROM orders WHERE status = 'awaiting_payment' AND payment_authority != ''")
	if err != nil {
		return nil, fmt.Errorf("query awaiting payment orders: %w", err)
	}
	defer rows.Close()

	var orders []PaymentOrder
	for rows.Next() {
		var o PaymentOrder
		if err := rows.Scan(&o.ID, &o.TotalAmount, &o.Authority); err != nil {
			return nil, fmt.Errorf("scan awaiting payment order: %w", err)
		}
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

// GetOrderWithItems retrieves a single order by ID along with its items and the
// corresponding product data. Product names are mapped back from products table.
func GetOrderWithItems(ctx context.Context, db *sql.DB, orderID string) (*models.Order, []models.OrderItem, []models.Product, error) {
	o, err := GetOrder(ctx, db, orderID)
	if err != nil {
		return nil, nil, nil, err
	}

	rows, err := db.QueryContext(ctx, "SELECT id, order_id, product_id, quantity, price_per_unit FROM order_items WHERE order_id = ?", orderID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get order_items: %w", err)
	}
	defer rows.Close()

	var items []models.OrderItem
	var productIDs []int64
	for rows.Next() {
		var item models.OrderItem
		if err := rows.Scan(&item.ID, &item.OrderID, &item.ProductID, &item.Quantity, &item.PricePerUnit); err != nil {
			return nil, nil, nil, fmt.Errorf("scan order_item: %w", err)
		}
		items = append(items, item)
		productIDs = append(productIDs, item.ProductID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}

	products, err := GetProductsByIDs(ctx, db, productIDs)
	if err != nil {
		return nil, nil, nil, err
	}

	return o, items, products, nil
}

// ── Users ────────────────────────────────────────────

// GetUserByPhone looks up a user by their phone number.
func GetUserByPhone(ctx context.Context, db *sql.DB, phone string) (*models.User, error) {
	var u models.User
	var createdAt string
	err := db.QueryRowContext(ctx, "SELECT id, phone_number, created_at FROM users WHERE phone_number = ?", phone).
		Scan(&u.ID, &u.PhoneNumber, &createdAt)
	if err != nil {
		return nil, err
	}
	u.CreatedAt = parseTime(createdAt)
	return &u, nil
}

// CreateUser inserts a new user with the given phone number.
func CreateUser(ctx context.Context, db *sql.DB, phone string) (*models.User, error) {
	res, err := db.ExecContext(ctx, "INSERT INTO users (phone_number) VALUES (?)", phone)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &models.User{ID: id, PhoneNumber: phone, CreatedAt: time.Now()}, nil
}

// GetOrCreateUser returns the existing user for a phone number or creates one.
// This avoids a separate registration step — users are auto-created on first OTP request.
func GetOrCreateUser(ctx context.Context, db *sql.DB, phone string) (*models.User, error) {
	user, err := GetUserByPhone(ctx, db, phone)
	if err == nil {
		return user, nil
	}
	return CreateUser(ctx, db, phone)
}

// ── OTP ──────────────────────────────────────────────

// CreateOTP stores a one-time password with a 2-minute expiry window.
// Old expired or used OTPs are purged on each call so the table stays bounded.
func CreateOTP(ctx context.Context, db *sql.DB, phone, code string, expiresAt time.Time) error {
	_, _ = db.ExecContext(ctx, "DELETE FROM otp_codes WHERE expires_at < datetime('now') OR is_used = 1")
	_, err := db.ExecContext(ctx, "INSERT INTO otp_codes (phone_number, code, expires_at) VALUES (?, ?, ?)", phone, code, expiresAt.Format("2006-01-02 15:04:05"))
	return err
}

// VerifyOTP checks that a code matches the latest unused OTP for the given phone
// and that it has not expired. On success the OTP is marked as used.
func VerifyOTP(ctx context.Context, db *sql.DB, phone, code string) (bool, error) {
	var id int64
	var expiresAt string
	err := db.QueryRowContext(ctx, "SELECT id, expires_at FROM otp_codes WHERE phone_number = ? AND code = ? AND is_used = 0 ORDER BY id DESC LIMIT 1", phone, code).
		Scan(&id, &expiresAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}

	expTime, err := time.Parse("2006-01-02 15:04:05", expiresAt)
	if err != nil {
		return false, err
	}

	if time.Now().After(expTime) {
		return false, nil
	}

	_, err = db.ExecContext(ctx, "UPDATE otp_codes SET is_used = 1 WHERE id = ?", id)
	return err == nil, err
}

// ── User Orders ──────────────────────────────────────

// GetOrdersByUser returns all orders placed by a specific user, newest first.
func GetOrdersByUser(ctx context.Context, db *sql.DB, userID int64) ([]models.Order, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, customer_name, customer_phone, customer_address, postal_code, total_amount, status, created_at FROM orders WHERE user_id = ? ORDER BY created_at DESC", userID)
	if err != nil {
		return nil, fmt.Errorf("query user orders: %w", err)
	}
	defer rows.Close()

	var orders []models.Order
	for rows.Next() {
		var o models.Order
		var createdAt string
		if err := rows.Scan(&o.ID, &o.CustomerName, &o.CustomerPhone, &o.CustomerAddress, &o.PostalCode, &o.TotalAmount, &o.Status, &createdAt); err != nil {
			return nil, fmt.Errorf("scan order: %w", err)
		}
		o.CreatedAt = parseTime(createdAt)
		o.UserID = userID
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

// GetUserOrdersWithItems returns a user's orders enriched with product names
// and computed subtotals. It batches all order IDs into a single IN query.
func GetUserOrdersWithItems(ctx context.Context, db *sql.DB, userID int64) ([]models.OrderSummary, error) {
	orders, err := GetOrdersByUser(ctx, db, userID)
	if err != nil {
		return nil, err
	}
	if len(orders) == 0 {
		return nil, nil
	}

	orderIDs := make([]string, len(orders))
	for i, o := range orders {
		orderIDs[i] = o.ID
	}

	placeholders := make([]string, len(orderIDs))
	args := make([]interface{}, len(orderIDs))
	for i, id := range orderIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	query := fmt.Sprintf("SELECT oi.order_id, oi.quantity, oi.price_per_unit, p.name, p.unit FROM order_items oi JOIN products p ON p.id = oi.product_id WHERE oi.order_id IN (%s) ORDER BY oi.order_id", strings.Join(placeholders, ","))
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query order items: %w", err)
	}
	defer rows.Close()

	itemsByOrder := make(map[string][]models.OrderItemView)
	for rows.Next() {
		var orderID string
		var quantity, price int
		var name, unit string
		if err := rows.Scan(&orderID, &quantity, &price, &name, &unit); err != nil {
			return nil, fmt.Errorf("scan item: %w", err)
		}
		itemsByOrder[orderID] = append(itemsByOrder[orderID], models.OrderItemView{
			Name:     name,
			Quantity: quantity,
			Price:    price,
			Subtotal: quantity * price,
			Unit:     unit,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	summaries := make([]models.OrderSummary, len(orders))
	for i, o := range orders {
		summaries[i] = models.OrderSummary{
			Order: o,
			Items: itemsByOrder[o.ID],
		}
	}
	return summaries, nil
}
