package database

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "modernc.org/sqlite"

	"farmstore/internal/models"
)

// category constants for filtering
const (
	CategoryFresh   = "Fresh Fruits"
	CategoryDerived = "Derived Products"
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

	CREATE TABLE IF NOT EXISTS orders (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		customer_name    TEXT    NOT NULL,
		customer_phone   TEXT    NOT NULL DEFAULT '',
		customer_address TEXT    NOT NULL DEFAULT '',
		total_amount    INTEGER NOT NULL DEFAULT 0,
		status          TEXT    NOT NULL DEFAULT 'pending'
			CHECK (status IN ('pending','processing','completed','cancelled')),
		created_at      TEXT    NOT NULL DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS order_items (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		order_id       INTEGER NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
		product_id     INTEGER NOT NULL REFERENCES products(id),
		quantity       INTEGER NOT NULL DEFAULT 1,
		price_per_unit INTEGER NOT NULL,
		UNIQUE(order_id, product_id)
	);
	`
	_, err := db.Exec(schema)
	return err
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
		{"Fresh Organic Figs", "fresh-organic-figs", "Fresh Fruits", "Premium organic figs, hand-picked at peak ripeness.", 1299, 50, "1kg"},
		{"Fresh Pomegranates", "fresh-pomegranates", "Fresh Fruits", "Juicy, ruby-red pomegranates straight from the orchard.", 899, 60, "1kg"},
		{"Artisanal Fig Jam", "artisanal-fig-jam", "Derived Products", "Slow-cooked fig jam made with organic figs and a hint of lemon.", 949, 30, "250ml jar"},
		{"Pure Pomegranate Molasses", "pure-pomegranate-molasses", "Derived Products", "Thick, tangy pomegranate molasses — perfect for dressings and marinades.", 1199, 25, "500ml bottle"},
		{"Cold-Pressed Pomegranate Juice", "cold-pressed-pomegranate-juice", "Derived Products", "Fresh cold-pressed pomegranate juice, no added sugar.", 799, 40, "500ml bottle"},
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

func UpdateOrderStatus(db *sql.DB, orderID int64, status string) error {
	res, err := db.Exec("UPDATE orders SET status = ? WHERE id = ?", status, orderID)
	if err != nil {
		return fmt.Errorf("update order %d status: %w", orderID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("order %d not found", orderID)
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

func CreateOrder(db *sql.DB, o *models.Order, items []models.OrderItem) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`INSERT INTO orders (customer_name, customer_phone, customer_address, total_amount, status) VALUES (?, ?, ?, ?, ?)`,
		o.CustomerName, o.CustomerPhone, o.CustomerAddress, o.TotalAmount, o.Status)
	if err != nil {
		return 0, fmt.Errorf("insert order: %w", err)
	}

	orderID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}

	stmt, err := tx.Prepare(`INSERT INTO order_items (order_id, product_id, quantity, price_per_unit) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("prepare order_items: %w", err)
	}
	defer stmt.Close()

	for _, item := range items {
		if _, err := stmt.Exec(orderID, item.ProductID, item.Quantity, item.PricePerUnit); err != nil {
			return 0, fmt.Errorf("insert order_item: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit tx: %w", err)
	}
	return orderID, nil
}

func GetOrderWithItems(db *sql.DB, orderID int64) (*models.Order, []models.OrderItem, []models.Product, error) {
	var o models.Order
	var createdAt string
	err := db.QueryRow("SELECT id, customer_name, customer_phone, customer_address, total_amount, status, created_at FROM orders WHERE id = ?", orderID).
		Scan(&o.ID, &o.CustomerName, &o.CustomerPhone, &o.CustomerAddress, &o.TotalAmount, &o.Status, &createdAt)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get order %d: %w", orderID, err)
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
