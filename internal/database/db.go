package database

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"farmstore/internal/models"
)

// category constants for filtering
const (
	CategoryFresh   = "میوه تازه"
	CategoryDerived = "محصولات فرآوری‌شده"
)

func Init(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

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
		total_amount    INTEGER NOT NULL DEFAULT 0,
		status          TEXT    NOT NULL DEFAULT 'pending'
			CHECK (status IN ('pending','processing','completed','cancelled')),
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

	_, err = db.Exec("ALTER TABLE orders ADD COLUMN user_id INTEGER REFERENCES users(id)")
	if err != nil {
		// column may already exist on re-deploy — ignore
	}

	return nil
}

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
		{"انجیر تازه ارگانیک", "انجیر-تازه-ارگانیک", "میوه تازه", "انجیر ارگانیک درجه یک، چیده‌شده در اوج رسیدگی.", 1299, 50, "۱ کیلوگرم"},
		{"انار تازه ملس", "انار-تازه-ملس", "میوه تازه", "انارهای آبدار و یاقوتی مستقیماً از باغ.", 899, 60, "۱ کیلوگرم"},
		{"مربای انجیر خانگی", "مربای-انجیر-خانگی", "محصولات فرآوری‌شده", "مربای انجیر آرام‌پز شده با انجیر ارگانیک و کمی لیمو.", 949, 30, "شیشه ۲۵۰ گرمی"},
		{"رب انار خالص", "رب-انار-خالص", "محصولات فرآوری‌شده", "رب انار غلیظ و ترش - عالی برای سس و ماریناد.", 1199, 25, "بطری ۵۰۰ میلی‌لیتر"},
		{"آب انار طبیعی", "آب-انار-طبیعی", "محصولات فرآوری‌شده", "آب انار تازه و طبیعی، بدون شکر افزوده.", 799, 40, "بطری ۵۰۰ میلی‌لیتر"},
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
		log.Printf("seeded product: %s", p.Name)
	}

	return nil
}

func GetProducts(db *sql.DB, category string) ([]models.Product, error) {
	query := "SELECT id, name, slug, category, description, price, stock_quantity, unit, image_url, is_active, created_at FROM products WHERE is_active = 1"
	args := []interface{}{}
	if category != "" && category != "all" {
		query += " AND category = ?"
		args = append(args, category)
	}
	query += " ORDER BY name"

	rows, err := db.Query(query, args...)
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

func GetProduct(db *sql.DB, id int64) (*models.Product, error) {
	row := db.QueryRow("SELECT id, name, slug, category, description, price, stock_quantity, unit, image_url, is_active, created_at FROM products WHERE id = ?", id)
	p, err := scanProductRow(row)
	if err != nil {
		return nil, fmt.Errorf("get product %d: %w", id, err)
	}
	return &p, nil
}

func GetOrders(db *sql.DB) ([]models.Order, error) {
	rows, err := db.Query("SELECT id, customer_name, customer_phone, customer_address, total_amount, status, created_at FROM orders ORDER BY created_at DESC")
	if err != nil {
		return nil, fmt.Errorf("query orders: %w", err)
	}
	defer rows.Close()

	var orders []models.Order
	for rows.Next() {
		var o models.Order
		var createdAt string
		if err := rows.Scan(&o.ID, &o.CustomerName, &o.CustomerPhone, &o.CustomerAddress, &o.TotalAmount, &o.Status, &createdAt); err != nil {
			return nil, fmt.Errorf("scan order: %w", err)
		}
		o.CreatedAt = parseTime(createdAt)
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

func UpdateOrderStatus(db *sql.DB, orderID string, status string) error {
	res, err := db.Exec("UPDATE orders SET status = ? WHERE id = ?", status, orderID)
	if err != nil {
		return fmt.Errorf("update order %s status: %w", orderID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("order %s not found", orderID)
	}
	return nil
}

func GetAllProducts(db *sql.DB) ([]models.Product, error) {
	rows, err := db.Query("SELECT id, name, slug, category, description, price, stock_quantity, unit, image_url, is_active, created_at FROM products ORDER BY name")
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

func UpdateProduct(db *sql.DB, p *models.Product) error {
	active := 0
	if p.IsActive {
		active = 1
	}
	res, err := db.Exec("UPDATE products SET price = ?, stock_quantity = ?, is_active = ? WHERE id = ?",
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

func CreateProduct(db *sql.DB, p *models.Product) (int64, error) {
	active := 0
	if p.IsActive {
		active = 1
	}
	res, err := db.Exec(`INSERT INTO products (name, slug, category, description, price, stock_quantity, unit, image_url, is_active) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.Slug, p.Category, p.Description, p.Price, p.StockQuantity, p.Unit, p.ImageURL, active)
	if err != nil {
		return 0, fmt.Errorf("create product: %w", err)
	}
	return res.LastInsertId()
}

func scanProductRow(s interface{ Scan(dest ...interface{}) error }) (models.Product, error) {
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

func parseTime(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		return time.Now()
	}
	return t
}

func randomOrderID() string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 6)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[n.Int64()]
	}
	return "TDJ-" + string(b)
}

func CreateOrder(db *sql.DB, o *models.Order, items []models.OrderItem) (string, error) {
	o.ID = randomOrderID()

	tx, err := db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(`INSERT INTO orders (id, customer_name, customer_phone, customer_address, total_amount, status, user_id) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		o.ID, o.CustomerName, o.CustomerPhone, o.CustomerAddress, o.TotalAmount, o.Status, o.UserID)
	if err != nil {
		return "", fmt.Errorf("insert order: %w", err)
	}

	stmt, err := tx.Prepare(`INSERT INTO order_items (order_id, product_id, quantity, price_per_unit) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return "", fmt.Errorf("prepare order_items: %w", err)
	}
	defer stmt.Close()

	for _, item := range items {
		if _, err := stmt.Exec(o.ID, item.ProductID, item.Quantity, item.PricePerUnit); err != nil {
			return "", fmt.Errorf("insert order_item: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit tx: %w", err)
	}
	return o.ID, nil
}

func GetOrderWithItems(db *sql.DB, orderID string) (*models.Order, []models.OrderItem, []models.Product, error) {
	var o models.Order
	var createdAt string
	err := db.QueryRow("SELECT id, customer_name, customer_phone, customer_address, total_amount, status, created_at FROM orders WHERE id = ?", orderID).
		Scan(&o.ID, &o.CustomerName, &o.CustomerPhone, &o.CustomerAddress, &o.TotalAmount, &o.Status, &createdAt)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get order %s: %w", orderID, err)
	}
	o.CreatedAt = parseTime(createdAt)

	rows, err := db.Query("SELECT id, order_id, product_id, quantity, price_per_unit FROM order_items WHERE order_id = ?", orderID)
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

	products := make([]models.Product, 0, len(productIDs))
	for _, pid := range productIDs {
		p, err := GetProduct(db, pid)
		if err != nil {
			return nil, nil, nil, err
		}
		products = append(products, *p)
	}

	return &o, items, products, nil
}

// ── Users ────────────────────────────────────────────

func GetUserByPhone(db *sql.DB, phone string) (*models.User, error) {
	var u models.User
	var createdAt string
	err := db.QueryRow("SELECT id, phone_number, created_at FROM users WHERE phone_number = ?", phone).
		Scan(&u.ID, &u.PhoneNumber, &createdAt)
	if err != nil {
		return nil, err
	}
	u.CreatedAt = parseTime(createdAt)
	return &u, nil
}

func CreateUser(db *sql.DB, phone string) (*models.User, error) {
	res, err := db.Exec("INSERT INTO users (phone_number) VALUES (?)", phone)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &models.User{ID: id, PhoneNumber: phone, CreatedAt: time.Now()}, nil
}

func GetOrCreateUser(db *sql.DB, phone string) (*models.User, error) {
	user, err := GetUserByPhone(db, phone)
	if err == nil {
		return user, nil
	}
	return CreateUser(db, phone)
}

// ── OTP ──────────────────────────────────────────────

func CreateOTP(db *sql.DB, phone, code string, expiresAt time.Time) error {
	_, err := db.Exec("INSERT INTO otp_codes (phone_number, code, expires_at) VALUES (?, ?, ?)", phone, code, expiresAt.Format("2006-01-02 15:04:05"))
	return err
}

func VerifyOTP(db *sql.DB, phone, code string) (bool, error) {
	var id int64
	var expiresAt string
	err := db.QueryRow("SELECT id, expires_at FROM otp_codes WHERE phone_number = ? AND code = ? AND is_used = 0 ORDER BY id DESC LIMIT 1", phone, code).
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

	_, err = db.Exec("UPDATE otp_codes SET is_used = 1 WHERE id = ?", id)
	return err == nil, err
}

// ── User Orders ──────────────────────────────────────

func GetOrdersByUser(db *sql.DB, userID int64) ([]models.Order, error) {
	rows, err := db.Query("SELECT id, customer_name, customer_phone, customer_address, total_amount, status, created_at FROM orders WHERE user_id = ? ORDER BY created_at DESC", userID)
	if err != nil {
		return nil, fmt.Errorf("query user orders: %w", err)
	}
	defer rows.Close()

	var orders []models.Order
	for rows.Next() {
		var o models.Order
		var createdAt string
		if err := rows.Scan(&o.ID, &o.CustomerName, &o.CustomerPhone, &o.CustomerAddress, &o.TotalAmount, &o.Status, &createdAt); err != nil {
			return nil, fmt.Errorf("scan order: %w", err)
		}
		o.CreatedAt = parseTime(createdAt)
		o.UserID = userID
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

func GetUserOrdersWithItems(db *sql.DB, userID int64) ([]models.OrderSummary, error) {
	orders, err := GetOrdersByUser(db, userID)
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

	query := fmt.Sprintf("SELECT oi.order_id, oi.quantity, oi.price_per_unit, p.name FROM order_items oi JOIN products p ON p.id = oi.product_id WHERE oi.order_id IN (%s) ORDER BY oi.order_id", strings.Join(placeholders, ","))
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query order items: %w", err)
	}
	defer rows.Close()

	itemsByOrder := make(map[string][]models.OrderItemView)
	for rows.Next() {
		var orderID string
		var quantity, price int
		var name string
		if err := rows.Scan(&orderID, &quantity, &price, &name); err != nil {
			return nil, fmt.Errorf("scan item: %w", err)
		}
		itemsByOrder[orderID] = append(itemsByOrder[orderID], models.OrderItemView{
			Name:     name,
			Quantity: quantity,
			Price:    price,
			Subtotal: quantity * price,
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
