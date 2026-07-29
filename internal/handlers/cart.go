package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// CartItem represents a single product line in a shopping cart (product, quantity, unit price).
type CartItem struct {
	ProductID int64  `json:"product_id"`
	Name      string `json:"name"`
	Price     int    `json:"price"`
	Unit      string `json:"unit"`
	Quantity  int    `json:"quantity"`
	ImageURL  string `json:"image_url"`
}

// Cart is a per-session, in-memory collection of CartItems.
// Cart is NOT persisted across server restarts — it is suitable for single-server
// deployments only.
type Cart struct {
	mu    sync.Mutex
	Items []CartItem
}

// AddItem increments quantity if the product already exists in the cart;
// otherwise appends a new line item with quantity 1.
func (c *Cart) AddItem(item CartItem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, existing := range c.Items {
		if existing.ProductID == item.ProductID {
			c.Items[i].Quantity++
			return
		}
	}
	c.Items = append(c.Items, item)
}

// Total returns the sum of (price · quantity) for every item in the cart.
func (c *Cart) Total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, item := range c.Items {
		total += item.Price * item.Quantity
	}
	return total
}

// Count returns the total number of units (sum of quantities) across all items.
func (c *Cart) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, item := range c.Items {
		count += item.Quantity
	}
	return count
}

// UpdateQuantity adjusts the quantity of a product by delta. If the resulting
// quantity is ≤ 0 the item is removed from the cart.
func (c *Cart) UpdateQuantity(productID int64, delta int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, item := range c.Items {
		if item.ProductID == productID {
			c.Items[i].Quantity += delta
			if c.Items[i].Quantity <= 0 {
				c.Items = append(c.Items[:i], c.Items[i+1:]...)
			}
			return
		}
	}
}

// RemoveItem deletes a product line from the cart entirely.
func (c *Cart) RemoveItem(productID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, item := range c.Items {
		if item.ProductID == productID {
			c.Items = append(c.Items[:i], c.Items[i+1:]...)
			return
		}
	}
}

// Clear removes all items from the cart.
func (c *Cart) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Items = nil
}

// CartStore holds a map of session-ID → Cart, protected by an RWMutex.
// Each session gets its own cart automatically on first access.
type CartStore struct {
	mu    sync.RWMutex
	carts map[string]*Cart
}

// NewCartStore creates a new empty CartStore.
func NewCartStore() *CartStore {
	return &CartStore{carts: make(map[string]*Cart)}
}

// Get retrieves (or lazily creates) the Cart for a given session ID.
func (s *CartStore) Get(sessionID string) *Cart {
	s.mu.RLock()
	c, ok := s.carts[sessionID]
	s.mu.RUnlock()
	if !ok {
		c = &Cart{}
		s.mu.Lock()
		s.carts[sessionID] = c
		s.mu.Unlock()
	}
	return c
}

// generateSessionID creates a cryptographically random 32-hex-character session
// identifier using crypto/rand.
func generateSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
