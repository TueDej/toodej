// Package database provides SQLite database initialisation, schema migration,
// data seeding, and all query functions for products, orders, users, and OTP codes.
package database

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"farmstore/internal/logutil"
	"farmstore/internal/models"
)

// ErrDuplicateCategory is returned by CreateCategory when the requested slug
// already belongs to an existing category.
var ErrDuplicateCategory = errors.New("duplicate category slug")

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
	// Without a busy timeout a concurrent write (HTTP handlers plus the three
	// background janitors) makes SQLite return SQLITE_BUSY immediately, which
	// surfaces to customers as intermittent 500s. Wait up to 5s for the lock.
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return nil, fmt.Errorf("enable busy timeout: %w", err)
	}

	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	if err := seed(db); err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}

	if err := seedCategories(db); err != nil {
		return nil, fmt.Errorf("seed categories: %w", err)
	}

	if err := initProductPositions(db); err != nil {
		return nil, fmt.Errorf("init product positions: %w", err)
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
		position      INTEGER NOT NULL DEFAULT 0,
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
		tracking_code    TEXT    NOT NULL DEFAULT '',
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
		code         TEXT    NOT NULL, -- SHA-256 hash of (phone|code), never the plaintext
		expires_at   TEXT    NOT NULL,
		is_used      INTEGER NOT NULL DEFAULT 0
	);

	CREATE TABLE IF NOT EXISTS payment_authorities (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		order_id   TEXT NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
		authority  TEXT NOT NULL UNIQUE,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);

	CREATE INDEX IF NOT EXISTS idx_payment_authorities_order ON payment_authorities(order_id);

	CREATE TABLE IF NOT EXISTS categories (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		slug        TEXT    NOT NULL UNIQUE,
		label       TEXT    NOT NULL,
		is_enabled  INTEGER NOT NULL DEFAULT 1,
		description TEXT    NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS images (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		owner_type TEXT    NOT NULL CHECK (owner_type IN ('product','category')),
		owner_id   INTEGER NOT NULL,
		path       TEXT    NOT NULL,
		position   INTEGER NOT NULL DEFAULT 0,
		created_at TEXT    NOT NULL DEFAULT (datetime('now'))
	);

	CREATE INDEX IF NOT EXISTS idx_images_owner ON images(owner_type, owner_id, position);
	`
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}

	// OTPs are now stored as SHA-256 hashes (see HashOTP). Any pre-existing
	// rows hold plaintext from the old format and can never verify again.
	// Codes are ephemeral (2-minute TTL) and the in-memory pending-login state
	// dies with the process on every restart, so wiping the table on migrate
	// loses nothing.
	if _, err := db.Exec("DELETE FROM otp_codes"); err != nil {
		return fmt.Errorf("purge legacy otp rows: %w", err)
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
	// tracking_code holds the postal tracking number the admin enters when an
	// order is marked dispatched (ارسال شد). Optional: it may stay empty.
	if err := ensureOrderColumn(db, "tracking_code", "ALTER TABLE orders ADD COLUMN tracking_code TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureOrderColumn(db, "user_id", "ALTER TABLE orders ADD COLUMN user_id INTEGER REFERENCES users(id)"); err != nil {
		return err
	}

	// Migration: products.position backs the admin's drag-to-reorder. Databases
	// created before it existed get the column here; positions are seeded to a
	// sensible order (by name) in initProductPositions once the table has rows.
	if err := ensureProductColumn(db, "position", "ALTER TABLE products ADD COLUMN position INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}

	// Migration: categories.description holds the editable storefront tagline
	// (e.g. "انار تازه، آبدار و طبیعی.") shown under the category page header.
	// It replaces the taglines that used to be hardcoded in products.html.
	// The backfill runs only when the column was just added, so an admin who
	// later clears a description on purpose is never overwritten on restart.
	descAdded, err := ensureCategoryColumn(db, "description", "ALTER TABLE categories ADD COLUMN description TEXT NOT NULL DEFAULT ''")
	if err != nil {
		return err
	}
	if descAdded {
		if _, err := db.Exec(`UPDATE categories SET description = CASE slug
			WHEN 'fig' THEN 'انجیر تازه و خشک، مستقیم از باغ.'
			WHEN 'pomegranate' THEN 'انار تازه، آبدار و طبیعی.'
			WHEN 'traditional' THEN 'محصولات سنتی و خانگی، با طعم اصیل.'
			ELSE description END
			WHERE slug IN ('fig','pomegranate','traditional')`); err != nil {
			return fmt.Errorf("backfill category descriptions: %w", err)
		}
	}

	// Payment callbacks and the payment reconciler look orders up by their
	// Zarinpal authority token; without an index every lookup is a full scan.
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_orders_payment_authority ON orders(payment_authority)"); err != nil {
		return fmt.Errorf("create payment_authority index: %w", err)
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

// ensureProductColumn adds a column to products if it does not already exist.
func ensureProductColumn(db *sql.DB, name, ddl string) error {
	exists, err := productColumnExists(db, name)
	if err != nil {
		return fmt.Errorf("check products.%s: %w", name, err)
	}
	if exists {
		return nil
	}
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("add products.%s: %w", name, err)
	}
	return nil
}

func productColumnExists(db *sql.DB, name string) (bool, error) {
	rows, err := db.Query("PRAGMA table_info(products)")
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

// ensureCategoryColumn adds a column to categories if it does not already
// exist, reporting whether it was added so callers can backfill fresh values
// exactly once.
func ensureCategoryColumn(db *sql.DB, name, ddl string) (bool, error) {
	exists, err := categoryColumnExists(db, name)
	if err != nil {
		return false, fmt.Errorf("check categories.%s: %w", name, err)
	}
	if exists {
		return false, nil
	}
	if _, err := db.Exec(ddl); err != nil {
		return false, fmt.Errorf("add categories.%s: %w", name, err)
	}
	return true, nil
}

func categoryColumnExists(db *sql.DB, name string) (bool, error) {
	rows, err := db.Query("PRAGMA table_info(categories)")
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

// initProductPositions gives products a stable display order the first time the
// position column is used. It assigns positions 0..n-1 in name order (matching
// the storefront's previous alphabetical listing) so existing databases keep
// their visual order across the upgrade. It is a no-op once any product has a
// non-zero position — i.e. once the admin has reordered — so it never clobbers a
// custom order on later startups.
func initProductPositions(db *sql.DB) error {
	var total, customized int
	if err := db.QueryRow("SELECT COUNT(*), COALESCE(SUM(position <> 0), 0) FROM products").Scan(&total, &customized); err != nil {
		return fmt.Errorf("count products for position init: %w", err)
	}
	if total <= 1 || customized > 0 {
		return nil
	}

	rows, err := db.Query("SELECT id FROM products ORDER BY name, id")
	if err != nil {
		return fmt.Errorf("list products for position init: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan product id for position init: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate products for position init: %w", err)
	}
	rows.Close()

	stmt, err := db.Prepare("UPDATE products SET position = ? WHERE id = ?")
	if err != nil {
		return fmt.Errorf("prepare position init: %w", err)
	}
	defer stmt.Close()
	for i, id := range ids {
		if _, err := stmt.Exec(i, id); err != nil {
			return fmt.Errorf("init position for product %d: %w", id, err)
		}
	}
	return nil
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
		tracking_code    TEXT    NOT NULL DEFAULT '',
		user_id        INTEGER REFERENCES users(id),
		created_at      TEXT    NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return fmt.Errorf("create orders_new: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO orders_new (
			id, customer_name, customer_phone, customer_address, postal_code,
			total_amount, status, payment_authority, payment_ref_id, tracking_code, user_id, created_at
		)
		SELECT id, customer_name, customer_phone, customer_address, postal_code,
			total_amount,
			CASE status
				WHEN 'processing' THEN 'preparing'
				WHEN 'completed' THEN 'dispatched'
				ELSE status
			END,
			payment_authority, payment_ref_id, tracking_code, user_id, created_at
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
		{"test1", "test1", "test", "", 1299000, 50, "عدد"},
		{"test2", "test2", "test", "", 899000, 60, "عدد"},
		{"test3", "test3", "test", "", 950000, 30, "عدد"},
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

// seedCategories populates the categories table with the default taxonomy when
// it is empty. This runs once per fresh database.
func seedCategories(db *sql.DB) error {
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM categories").Scan(&count); err != nil {
		return fmt.Errorf("count categories: %w", err)
	}
	if count > 0 {
		return nil
	}

	rows := []struct {
		Slug        string
		Label       string
		Description string
		Enable      bool
	}{
		{"fig", "انجیر", "انجیر تازه و خشک، مستقیم از باغ.", true},
		{"traditional", "محصولات سنتی/خانگی", "محصولات سنتی و خانگی، با طعم اصیل.", true},
		{"pomegranate", "انار", "انار تازه، آبدار و طبیعی.", true},
		{"test", "test", "test", true},
	}

	stmt, err := db.Prepare(`INSERT INTO categories (slug, label, description, is_enabled) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare category insert: %w", err)
	}
	defer stmt.Close()

	for _, c := range rows {
		enabled := 0
		if c.Enable {
			enabled = 1
		}
		if _, err := stmt.Exec(c.Slug, c.Label, c.Description, enabled); err != nil {
			return fmt.Errorf("insert category %q: %w", c.Slug, err)
		}
		logutil.Info("seeded category", "slug", c.Slug, "label", c.Label)
	}

	return nil
}

// GetCategories returns every category ordered by id.
func GetCategories(ctx context.Context, db *sql.DB) ([]models.Category, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, slug, label, is_enabled, description FROM categories ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("query categories: %w", err)
	}
	defer rows.Close()

	var cats []models.Category
	for rows.Next() {
		c, err := scanCategoryRow(rows)
		if err != nil {
			return nil, err
		}
		cats = append(cats, c)
	}
	return cats, rows.Err()
}

// GetEnabledCategories returns only the categories that are currently enabled.
// A nil slice is returned (never error) when there are none, so callers can
// range over it safely without nil-panic guards.
func GetEnabledCategories(ctx context.Context, db *sql.DB) ([]models.Category, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, slug, label, is_enabled, description FROM categories WHERE is_enabled = 1 ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("query enabled categories: %w", err)
	}
	defer rows.Close()

	var cats []models.Category
	for rows.Next() {
		c, err := scanCategoryRow(rows)
		if err != nil {
			return nil, err
		}
		cats = append(cats, c)
	}
	return cats, rows.Err()
}

// GetCategoryBySlug returns the category with the given slug, or sql.ErrNoRows
// if no such category exists.
func GetCategoryBySlug(ctx context.Context, db *sql.DB, slug string) (*models.Category, error) {
	row := db.QueryRowContext(ctx, "SELECT id, slug, label, is_enabled, description FROM categories WHERE slug = ?", slug)
	c, err := scanCategoryRow(row)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// CreateCategory inserts a new category after trimming and validating its slug
// and label. The description is the optional storefront tagline. A duplicate
// slug returns ErrDuplicateCategory.
func CreateCategory(ctx context.Context, db *sql.DB, slug, label, description string) (int64, error) {
	slug = strings.TrimSpace(slug)
	label = strings.TrimSpace(label)
	description = strings.TrimSpace(description)
	if slug == "" || label == "" {
		return 0, fmt.Errorf("category slug and label are required")
	}

	var existing int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM categories WHERE slug = ?", slug).Scan(&existing); err != nil {
		return 0, fmt.Errorf("check category slug: %w", err)
	}
	if existing > 0 {
		return 0, ErrDuplicateCategory
	}

	res, err := db.ExecContext(ctx, "INSERT INTO categories (slug, label, description, is_enabled) VALUES (?, ?, ?, 1)", slug, label, description)
	if err != nil {
		return 0, fmt.Errorf("create category: %w", err)
	}
	return res.LastInsertId()
}

// UpdateCategoryEnabled flips the enabled flag for a category by id.
// UpdateCategoryEnabled flips the enabled flag of a category.
func UpdateCategoryEnabled(ctx context.Context, db *sql.DB, id int64, enabled bool) error {
	on := 0
	if enabled {
		on = 1
	}
	res, err := db.ExecContext(ctx, "UPDATE categories SET is_enabled = ? WHERE id = ?", on, id)
	if err != nil {
		return fmt.Errorf("update category enabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("category %d not found", id)
	}
	return nil
}

// scanCategoryRow scans a category row from either a *sql.Row or *sql.Rows.
// UpdateCategory changes a category's slug, label, description, and enabled
// flag. A slug that collides with a different category is rejected with
// ErrDuplicateCategory so the UNIQUE constraint never surfaces as a 500.
// Products reference their category by free-text label (no FK), so a label
// rename also migrates products.category in the same transaction — otherwise
// renamed products vanish from every category page except /all.
func UpdateCategory(ctx context.Context, db *sql.DB, id int64, slug, label, description string, enabled bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin update category %d: %w", id, err)
	}
	defer tx.Rollback()

	var oldLabel string
	if err := tx.QueryRowContext(ctx, "SELECT label FROM categories WHERE id = ?", id).Scan(&oldLabel); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("category %d not found", id)
		}
		return fmt.Errorf("lookup category %d: %w", id, err)
	}
	res, err := tx.ExecContext(ctx,
		"UPDATE categories SET slug = ?, label = ?, description = ?, is_enabled = ? WHERE id = ?",
		slug, label, strings.TrimSpace(description), boolToInt(enabled), id)
	if err != nil {
		if isUniqueConstraintError(err) {
			return ErrDuplicateCategory
		}
		return fmt.Errorf("update category %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("category %d not found", id)
	}
	if label != oldLabel {
		if _, err := tx.ExecContext(ctx, "UPDATE products SET category = ? WHERE category = ?", label, oldLabel); err != nil {
			return fmt.Errorf("migrate products to renamed category: %w", err)
		}
	}
	return tx.Commit()
}

// boolToInt converts a bool to its SQLite integer representation.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func scanCategoryRow(s interface {
	Scan(dest ...interface{}) error
}) (models.Category, error) {
	var c models.Category
	var isEnabled int
	if err := s.Scan(&c.ID, &c.Slug, &c.Label, &isEnabled, &c.Description); err != nil {
		return c, err
	}
	c.IsEnabled = isEnabled == 1
	return c, nil
}

// GetProducts returns active products, optionally filtered by category.
// An empty category returns every active product. "all" returns every active
// product whose category is not disabled — disabling a category must hide its
// products from /all too, otherwise the toggle is cosmetic (the products stay
// buyable via the all-listing).
func GetProducts(ctx context.Context, db *sql.DB, category string) ([]models.Product, error) {
	query := "SELECT id, name, slug, category, description, price, stock_quantity, unit, image_url, is_active, created_at FROM products WHERE is_active = 1"
	args := []interface{}{}
	if category != "" && category != "all" {
		query += " AND category = ?"
		args = append(args, category)
	} else if category == "all" {
		query += " AND category NOT IN (SELECT label FROM categories WHERE is_enabled = 0)"
	}
	query += " ORDER BY position, id"

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
	rows, err := db.QueryContext(ctx, "SELECT id, customer_name, customer_phone, customer_address, postal_code, total_amount, status, payment_ref_id, tracking_code, created_at FROM orders ORDER BY created_at DESC")
	if err != nil {
		return nil, fmt.Errorf("query orders: %w", err)
	}
	defer rows.Close()

	var orders []models.Order
	for rows.Next() {
		var o models.Order
		var createdAt string
		if err := rows.Scan(&o.ID, &o.CustomerName, &o.CustomerPhone, &o.CustomerAddress, &o.PostalCode, &o.TotalAmount, &o.Status, &o.PaymentRefID, &o.TrackingCode, &createdAt); err != nil {
			return nil, fmt.Errorf("scan order: %w", err)
		}
		o.CreatedAt = parseTime(createdAt)
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

// UpdateOrderStatus changes the status of an order by its TDJ-XXXXXX ID.
// When transitioning to "cancelled", stock is atomically restored for all items.
// trackingCode is the optional postal tracking number; it is stored only when
// the order moves to "dispatched" (ارسال شد) and may be empty. Any other
// transition leaves the stored tracking code untouched (e.g. cancelling a
// dispatched order keeps its code for the record).
//
// The returned bool reports whether the order's status actually changed
// (same-status calls — e.g. re-posting dispatched to edit its tracking code —
// return false) so callers can fire one-time customer notifications only on
// real transitions.
func UpdateOrderStatus(ctx context.Context, db *sql.DB, orderID string, status string, trackingCode string) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Fetch current status to avoid double-restoring stock.
	var currentStatus string
	err = tx.QueryRowContext(ctx, "SELECT status FROM orders WHERE id = ?", orderID).Scan(&currentStatus)
	if err == sql.ErrNoRows {
		return false, fmt.Errorf("order %s not found", orderID)
	}
	if err != nil {
		return false, fmt.Errorf("query order %s: %w", orderID, err)
	}
	if !validOrderTransition(currentStatus, status) {
		return false, fmt.Errorf("%w: %s -> %s", ErrInvalidOrderTransition, currentStatus, status)
	}

	changed := currentStatus != status
	if status == "dispatched" {
		if _, err := tx.ExecContext(ctx, "UPDATE orders SET status = ?, tracking_code = ? WHERE id = ?", status, trackingCode, orderID); err != nil {
			return false, fmt.Errorf("update order %s status: %w", orderID, err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, "UPDATE orders SET status = ? WHERE id = ?", status, orderID); err != nil {
			return false, fmt.Errorf("update order %s status: %w", orderID, err)
		}
	}

	// Restore stock only when transitioning to cancelled from a non-cancelled state.
	if status == "cancelled" && currentStatus != "cancelled" {
		rows, err := tx.QueryContext(ctx, "SELECT product_id, quantity FROM order_items WHERE order_id = ?", orderID)
		if err != nil {
			return false, fmt.Errorf("query order items: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var productID, quantity int
			if err := rows.Scan(&productID, &quantity); err != nil {
				return false, fmt.Errorf("scan order item: %w", err)
			}
			if _, err := tx.ExecContext(ctx, "UPDATE products SET stock_quantity = stock_quantity + ? WHERE id = ?", quantity, productID); err != nil {
				return false, fmt.Errorf("restore stock for product %d: %w", productID, err)
			}
		}
		if err := rows.Err(); err != nil {
			return false, fmt.Errorf("iterate order items: %w", err)
		}
	}

	return changed, tx.Commit()
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

// orderLifecycle lists the non-cancelled statuses in the order they are
// progressed through. "cancelled" sits outside this sequence: it is reachable
// from any active status, but — like time — only ever moves forward.
var orderLifecycle = []string{"awaiting_payment", "pending", "preparing", "dispatched"}

// ValidOrderStatusOptions returns the statuses an order in `current` may be
// moved to, in display order, derived directly from validOrderTransition — so
// the admin UI can never drift from what the database enforces: the status
// itself, adjacent forward steps, and "cancelled" while still active. A
// cancelled order is terminal — it offers only itself.
func ValidOrderStatusOptions(current string) []string {
	ordered := append([]string{}, orderLifecycle...)
	ordered = append(ordered, "cancelled")
	var opts []string
	for _, s := range ordered {
		if validOrderTransition(current, s) {
			opts = append(opts, s)
		}
	}
	return opts
}

// SetPaymentAuthority stores the Zarinpal authority token on an order and
// records it in the payment_authorities history table. History matters: a
// resume-payment overwrites the order's active authority, and a customer may
// still complete payment on the superseded token — the callback and the
// reconciler must still be able to resolve it to this order.
func SetPaymentAuthority(ctx context.Context, db *sql.DB, orderID, authority string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set payment authority for %s: %w", orderID, err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, "UPDATE orders SET payment_authority = ? WHERE id = ? AND status = 'awaiting_payment'", authority, orderID)
	if err != nil {
		return fmt.Errorf("set payment authority for %s: %w", orderID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: cannot set payment authority for %s", ErrInvalidOrderTransition, orderID)
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO payment_authorities (order_id, authority) VALUES (?, ?)", orderID, authority); err != nil {
		return fmt.Errorf("record authority history for %s: %w", orderID, err)
	}
	return tx.Commit()
}

// GetAuthoritiesForOrder returns every authority ever issued for an order,
// newest first. The active one lives on orders.payment_authority; the rest are
// superseded tokens kept so their payments can still be matched.
func GetAuthoritiesForOrder(ctx context.Context, db *sql.DB, orderID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT authority FROM payment_authorities WHERE order_id = ? ORDER BY id DESC", orderID)
	if err != nil {
		return nil, fmt.Errorf("query authorities for %s: %w", orderID, err)
	}
	defer rows.Close()

	var authorities []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		authorities = append(authorities, a)
	}
	return authorities, rows.Err()
}

// GetOrderByAuthority looks up an order by its Zarinpal authority token. The
// active authority is tried first; if it does not match, the history table is
// consulted so payments completed on a superseded (resume-replaced) token are
// still resolved to their order instead of orphaning the charge.
func GetOrderByAuthority(ctx context.Context, db *sql.DB, authority string) (*models.Order, error) {
	o, err := getOrderByAuthorityColumn(ctx, db, "orders.payment_authority = ?", authority)
	if err == nil {
		return o, nil
	}
	o, err = getOrderByAuthorityColumn(ctx, db,
		"id IN (SELECT order_id FROM payment_authorities WHERE authority = ?)", authority)
	if err != nil {
		return nil, fmt.Errorf("get order by authority: %w", err)
	}
	return o, nil
}

func getOrderByAuthorityColumn(ctx context.Context, db *sql.DB, where string, authority string) (*models.Order, error) {
	var o models.Order
	var createdAt string
	var userID sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT id, customer_name, customer_phone, customer_address, postal_code,
		total_amount, status, payment_authority, payment_ref_id, user_id, created_at
		FROM orders WHERE `+where, authority).
		Scan(&o.ID, &o.CustomerName, &o.CustomerPhone, &o.CustomerAddress, &o.PostalCode,
			&o.TotalAmount, &o.Status, &o.Authority, &o.PaymentRefID, &userID, &createdAt)
	if err != nil {
		return nil, err
	}
	o.CreatedAt = parseTime(createdAt)
	o.UserID = userID.Int64
	return &o, nil
}

// ConfirmPayment marks an awaiting-payment order as pending (paid) and stores the
// Zarinpal ref ID. Already-paid orders are treated as idempotent callbacks.
// The returned bool reports whether THIS call performed the awaiting_payment
// → pending transition (idempotent replays report false) so callers fire the
// customer's "order confirmed" SMS exactly once.
func ConfirmPayment(ctx context.Context, db *sql.DB, orderID string, refID int64) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin confirm payment: %w", err)
	}
	defer tx.Rollback()

	var currentStatus string
	var currentRefID int64
	err = tx.QueryRowContext(ctx, "SELECT status, payment_ref_id FROM orders WHERE id = ?", orderID).Scan(&currentStatus, &currentRefID)
	if err == sql.ErrNoRows {
		return false, fmt.Errorf("order %s not found", orderID)
	}
	if err != nil {
		return false, fmt.Errorf("query payment status for %s: %w", orderID, err)
	}

	transitioned := false
	switch currentStatus {
	case "awaiting_payment":
		res, err := tx.ExecContext(ctx, "UPDATE orders SET status = 'pending', payment_ref_id = ? WHERE id = ? AND status = 'awaiting_payment'", refID, orderID)
		if err != nil {
			return false, fmt.Errorf("confirm payment for %s: %w", orderID, err)
		}
		// The status may have changed between the SELECT above and this UPDATE
		// (e.g. the unpaid-order janitor cancelled the order and restored its
		// stock). If no row was actually updated we must not report success, or
		// the caller would treat a paid order as confirmed while its stock was
		// already returned — leaking inventory.
		if n, _ := res.RowsAffected(); n == 0 {
			return false, fmt.Errorf("%w: order %s was already transitioned before confirmation", ErrInvalidOrderTransition, orderID)
		}
		transitioned = true
	case "pending", "preparing", "dispatched":
		if currentRefID == 0 && refID != 0 {
			if _, err := tx.ExecContext(ctx, "UPDATE orders SET payment_ref_id = ? WHERE id = ? AND payment_ref_id = 0", refID, orderID); err != nil {
				return false, fmt.Errorf("store payment ref for %s: %w", orderID, err)
			}
		}
	default:
		return false, fmt.Errorf("%w: cannot confirm payment for %s in status %s", ErrInvalidOrderTransition, orderID, currentStatus)
	}

	return transitioned, tx.Commit()
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

	res, err := tx.ExecContext(ctx, "UPDATE orders SET status = 'cancelled' WHERE id = ? AND status = 'awaiting_payment'", orderID)
	if err != nil {
		return fmt.Errorf("cancel unpaid order %s: %w", orderID, err)
	}
	// Only restore stock if THIS transaction actually moved the order out of
	// awaiting_payment. Another caller (the unpaid-order janitor, the payment
	// reconciler's ConfirmPayment, or a concurrent request) may have already
	// transitioned it; if so RowsAffected is 0 and we must NOT restore, or
	// inventory would be returned to stock twice — or, worse, for an order that
	// was actually paid (reconciler confirmed it) — leaking stock.
	if n, _ := res.RowsAffected(); n == 0 {
		return tx.Commit()
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
// older than ttl ago, and restores stock for all items in those orders. It returns the number
// of orders this call actually cancelled, which can be fewer than the number it selected if
// another path transitioned one of them in the meantime.
func CancelExpiredUnpaidOrders(ctx context.Context, db *sql.DB, ttl time.Duration) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin cancel expired tx: %w", err)
	}
	defer tx.Rollback()

	// Compute the cutoff in SQLite's own clock (UTC), matching how created_at is
	// stored via datetime('now'). Building the cutoff in Go and formatting it in
	// the server's local timezone would compare against UTC-stored timestamps and
	// treat every freshly-created awaiting_payment order as hours old — the local
	// vs UTC offset (e.g. +03:30) makes the janitor cancel new orders within a
	// single tick instead of after the intended TTL.
	minutes := int(ttl.Minutes())
	if minutes < 0 {
		minutes = 0
	}
	cutoffMod := fmt.Sprintf("-%d minutes", minutes)

	rows, err := tx.QueryContext(ctx, "SELECT id FROM orders WHERE status = 'awaiting_payment' AND datetime(created_at) < datetime('now', '"+cutoffMod+"')")
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

	cancelled := 0
	for _, orderID := range expiredOrderIDs {
		res, err := tx.ExecContext(ctx, "UPDATE orders SET status = 'cancelled' WHERE id = ? AND status = 'awaiting_payment'", orderID)
		if err != nil {
			return 0, fmt.Errorf("cancel expired order %s: %w", orderID, err)
		}
		// Skip stock restore if another path already transitioned this order out
		// of awaiting_payment (e.g. the reconciler confirmed payment). Without
		// this guard a paid order's inventory could be wrongly returned to stock.
		// Such an order is also not counted, so the reported total reflects the
		// orders this call really cancelled.
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		cancelled++

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

	return cancelled, nil
}

// GetAllProducts returns every product (including inactive ones), ordered by name.
// Used by the admin panel.
func GetAllProducts(ctx context.Context, db *sql.DB) ([]models.Product, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, name, slug, category, description, price, stock_quantity, unit, image_url, is_active, created_at FROM products ORDER BY position, id")
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
// UpdateProduct persists all editable product fields: name, slug, category,
// description, unit, price, stock quantity, and active flag. Callers must pass
// a fully-populated product (load it with GetProduct first). When the name
// changes, the URL slug is regenerated to match while remaining unique;
// unrelated edits leave the slug untouched so product URLs stay stable.
func UpdateProduct(ctx context.Context, db *sql.DB, p *models.Product) error {
	var currentName string
	err := db.QueryRowContext(ctx, "SELECT name FROM products WHERE id = ?", p.ID).Scan(&currentName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("product %d not found", p.ID)
		}
		return fmt.Errorf("load product %d for update: %w", p.ID, err)
	}
	if p.Name != currentName {
		slug, err := UniqueSlug(ctx, db, p.Name, p.ID)
		if err != nil {
			return fmt.Errorf("regenerate slug for product %d: %w", p.ID, err)
		}
		p.Slug = slug
	}

	active := 0
	if p.IsActive {
		active = 1
	}
	res, err := db.ExecContext(ctx, `UPDATE products SET
		name = ?, slug = ?, category = ?, description = ?, unit = ?,
		price = ?, stock_quantity = ?, is_active = ?
		WHERE id = ?`,
		p.Name, p.Slug, p.Category, p.Description, p.Unit,
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
	res, err := db.ExecContext(ctx, `INSERT INTO products (name, slug, category, description, price, stock_quantity, unit, image_url, is_active, position) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM products))`,
		p.Name, p.Slug, p.Category, p.Description, p.Price, p.StockQuantity, p.Unit, p.ImageURL, active)
	if err != nil {
		return 0, fmt.Errorf("create product: %w", err)
	}
	return res.LastInsertId()
}

// SetProductOrder rewrites products.position to match the supplied id order:
// the first id gets position 0, the next 1, and so on. The storefront and the
// admin list both order by position, so this is what drag-to-reorder persists.
// Unknown ids are rejected (rather than silently updating zero rows, which
// previously let partial/ghost orders create duplicate positions).
func SetProductOrder(ctx context.Context, db *sql.DB, ids []int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reorder tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, "UPDATE products SET position = ? WHERE id = ?")
	if err != nil {
		return fmt.Errorf("prepare reorder stmt: %w", err)
	}
	defer stmt.Close()

	for i, id := range ids {
		res, err := stmt.ExecContext(ctx, i, id)
		if err != nil {
			return fmt.Errorf("set position %d for product %d: %w", i, id, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("reorder: product %d not found", id)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reorder: %w", err)
	}
	return nil
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

// parseTime parses an SQLite datetime string into time.Time. Timestamps are
// stored by SQLite as UTC (datetime('now')), so the string is parsed in the UTC
// location to recover the correct instant. Returns time.Now() on parse failure
// so templates never receive a zero time.
func parseTime(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC)
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

// pricedItem is a product line resolved during order creation: its ID, the
// quantity ordered, and the price captured at purchase time.
type pricedItem struct {
	productID    int64
	quantity     int
	pricePerUnit int
}

// maxOrderIDAttempts bounds the retry loop used when a randomly generated order
// ID collides with an existing one. Collisions are vanishingly rare for TDJ-XXXXXX
// (a 36^6 space) but not impossible; without a retry the unique-constraint insert
// would fail and the customer's order would be lost.
const maxOrderIDAttempts = 5

// CreateOrder inserts an order and its items inside a single transaction.
// The order ID is auto-generated via randomOrderID. If a generated ID collides
// with an existing order (primary-key/unique violation), a fresh ID is tried up
// to maxOrderIDAttempts times before giving up, so a collision can never drop the
// order.
func CreateOrder(ctx context.Context, db *sql.DB, o *models.Order, items []models.OrderItem) (string, error) {
	if len(items) == 0 {
		return "", ErrProductUnavailable
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

	var lastErr error
	for attempt := 0; attempt < maxOrderIDAttempts; attempt++ {
		o.ID = randomOrderID()
		id, err := createOrderTx(ctx, db, o, productOrder, quantities, maxInt)
		if err == nil {
			return id, nil
		}
		lastErr = err
		// Only a primary-key collision on the random ID is worth retrying; any
		// other error (insufficient stock, DB failure, ...) is returned at once.
		if !isUniqueConstraintError(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("create order: order id generation exhausted after %d attempts: %w", maxOrderIDAttempts, lastErr)
}

// createOrderTx performs the stock reservation and row inserts for a single order
// using the caller-assigned o.ID. The whole operation runs in one transaction so
// a failure rolls back cleanly, including any stock reserved before a colliding
// insert forces a retry. It returns the order ID on success.
func createOrderTx(ctx context.Context, db *sql.DB, o *models.Order, productOrder []int64, quantities map[int64]int, maxInt int) (string, error) {
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

// isUniqueConstraintError reports whether err is a SQLite UNIQUE constraint
// violation — used to retry order creation when a randomly generated TDJ-XXXXXX
// ID happens to already exist. modernc.org/sqlite surfaces the standard SQLite
// "UNIQUE constraint failed" message, which we match.
func isUniqueConstraintError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// GetOrder retrieves a single order by its TDJ-XXXXXX ID, including the owning
// user ID so callers can enforce ownership before acting on the order.
func GetOrder(ctx context.Context, db *sql.DB, orderID string) (*models.Order, error) {
	var o models.Order
	var createdAt string
	var userID sql.NullInt64
	err := db.QueryRowContext(ctx, "SELECT id, customer_name, customer_phone, customer_address, postal_code, total_amount, status, payment_authority, payment_ref_id, tracking_code, user_id, created_at FROM orders WHERE id = ?", orderID).
		Scan(&o.ID, &o.CustomerName, &o.CustomerPhone, &o.CustomerAddress, &o.PostalCode, &o.TotalAmount, &o.Status, &o.Authority, &o.PaymentRefID, &o.TrackingCode, &userID, &createdAt)
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

// GetRecentlyCancelledPaymentOrders returns orders cancelled within the given
// window (measured from order creation) that carry an authority token and have
// no recorded payment ref. The payment reconciler verifies these with the
// gateway: a charge that succeeded after the cancel must be surfaced for a
// manual refund instead of vanishing silently.
func GetRecentlyCancelledPaymentOrders(ctx context.Context, db *sql.DB, window time.Duration) ([]PaymentOrder, error) {
	minutes := int(window.Minutes())
	if minutes < 1 {
		minutes = 1
	}
	rows, err := db.QueryContext(ctx,
		"SELECT id, total_amount, payment_authority FROM orders WHERE status = 'cancelled' AND payment_authority != '' AND payment_ref_id = 0 AND datetime(created_at) >= datetime('now', ?)",
		fmt.Sprintf("-%d minutes", minutes))
	if err != nil {
		return nil, fmt.Errorf("query recently cancelled payment orders: %w", err)
	}
	defer rows.Close()

	var orders []PaymentOrder
	for rows.Next() {
		var o PaymentOrder
		if err := rows.Scan(&o.ID, &o.TotalAmount, &o.Authority); err != nil {
			return nil, fmt.Errorf("scan recently cancelled payment order: %w", err)
		}
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

// MarkOrphanedPayment records the gateway ref of a charge that succeeded for a
// cancelled order. The row is only written once (payment_ref_id must still be
// 0) so the reconciler's repeated scans do not re-report the same orphan.
func MarkOrphanedPayment(ctx context.Context, db *sql.DB, orderID string, refID int64) error {
	res, err := db.ExecContext(ctx,
		"UPDATE orders SET payment_ref_id = ? WHERE id = ? AND status = 'cancelled' AND payment_ref_id = 0", refID, orderID)
	if err != nil {
		return fmt.Errorf("mark orphaned payment for %s: %w", orderID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("order %s is not an unrecorded cancelled order", orderID)
	}
	return nil
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
// The insert uses OR IGNORE so two concurrent first-OTP requests for the same
// number do not race into a UNIQUE violation (one would previously 500).
func GetOrCreateUser(ctx context.Context, db *sql.DB, phone string) (*models.User, error) {
	if _, err := db.ExecContext(ctx, "INSERT OR IGNORE INTO users (phone_number) VALUES (?)", phone); err != nil {
		return nil, fmt.Errorf("ensure user %s: %w", phone, err)
	}
	return GetUserByPhone(ctx, db, phone)
}

// ── OTP ──────────────────────────────────────────────

// HashOTP derives the value stored in otp_codes.code: a SHA-256 hash over the
// phone number and the code. Hashing at rest means a database or backup leak
// does not expose live OTPs in plaintext; the phone is mixed in so the same
// code issued to two numbers hashes differently. Exported so tests can derive
// the stored value for a known code.
func HashOTP(phone, code string) string {
	sum := sha256.Sum256([]byte(phone + "|" + code))
	return hex.EncodeToString(sum[:])
}

// CreateOTP stores a one-time password with a 2-minute expiry window. The code
// is stored hashed (HashOTP), never in plaintext.
//
// Any previous OTP row for the same phone is deleted first, so exactly one
// live code exists per number: an intercepted code is invalidated by a resend,
// and the guess space stays 1-in-100000 instead of growing with every resend.
// Expired or used rows for other phones are purged on each call so the table
// stays bounded.
//
// The expiry is stored in UTC: VerifyOTP parses this column as UTC, and the
// purge below compares it with datetime('now') (also UTC). Storing the local
// wall clock here would shift the effective expiry by the server's UTC offset —
// on a UTC+3:30 host an OTP would stay valid for hours instead of minutes.
func CreateOTP(ctx context.Context, db *sql.DB, phone, code string, expiresAt time.Time) error {
	_, err := db.ExecContext(ctx,
		"DELETE FROM otp_codes WHERE phone_number = ? OR expires_at < datetime('now') OR is_used = 1",
		phone)
	if err != nil {
		return fmt.Errorf("purge prior otp for %s: %w", phone, err)
	}
	_, err = db.ExecContext(ctx, "INSERT INTO otp_codes (phone_number, code, expires_at) VALUES (?, ?, ?)",
		phone, HashOTP(phone, code), expiresAt.UTC().Format("2006-01-02 15:04:05"))
	return err
}

// VerifyOTP checks that a code matches the single live OTP for the given phone
// and that it has not expired. The consume is atomic (guarded UPDATE), so two
// concurrent verifications of the same code cannot both succeed. On success all
// rows for the phone are removed, leaving no reusable hash material behind.
func VerifyOTP(ctx context.Context, db *sql.DB, phone, code string) (bool, error) {
	var id int64
	var expiresAt string
	err := db.QueryRowContext(ctx, "SELECT id, expires_at FROM otp_codes WHERE phone_number = ? AND code = ? AND is_used = 0 ORDER BY id DESC LIMIT 1", phone, HashOTP(phone, code)).
		Scan(&id, &expiresAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}

	expTime, err := time.ParseInLocation("2006-01-02 15:04:05", expiresAt, time.UTC)
	if err != nil {
		return false, err
	}

	// expTime is a UTC instant (CreateOTP stores UTC); time.Now() compares
	// instants regardless of zone, so this is offset-safe.
	if time.Now().After(expTime) {
		return false, nil
	}

	// Atomic consume: only the first concurrent caller flips is_used 0→1.
	res, err := db.ExecContext(ctx, "UPDATE otp_codes SET is_used = 1 WHERE id = ? AND is_used = 0", id)
	if err != nil {
		return false, fmt.Errorf("consume otp: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Another request consumed this code between the SELECT and the UPDATE.
		return false, nil
	}
	// Drop the rows entirely: a consumed code must leave nothing behind.
	if _, err := db.ExecContext(ctx, "DELETE FROM otp_codes WHERE phone_number = ?", phone); err != nil {
		return true, fmt.Errorf("delete consumed otp: %w", err)
	}
	return true, nil
}

// ── User Orders ──────────────────────────────────────

// GetOrdersByUser returns all orders placed by a specific user, newest first.
func GetOrdersByUser(ctx context.Context, db *sql.DB, userID int64) ([]models.Order, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, customer_name, customer_phone, customer_address, postal_code, total_amount, status, payment_ref_id, tracking_code, created_at FROM orders WHERE user_id = ? ORDER BY created_at DESC", userID)
	if err != nil {
		return nil, fmt.Errorf("query user orders: %w", err)
	}
	defer rows.Close()

	var orders []models.Order
	for rows.Next() {
		var o models.Order
		var createdAt string
		if err := rows.Scan(&o.ID, &o.CustomerName, &o.CustomerPhone, &o.CustomerAddress, &o.PostalCode, &o.TotalAmount, &o.Status, &o.PaymentRefID, &o.TrackingCode, &createdAt); err != nil {
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
