package handlers

import "sync"

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

// AddItemLimited increments or appends a cart item only when it would not exceed
// the latest stock quantity known by the caller. It also refreshes display fields
// from the current product row.
func (c *Cart) AddItemLimited(item CartItem, maxQuantity int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if maxQuantity <= 0 {
		return false
	}
	item.Quantity = 1
	for i, existing := range c.Items {
		if existing.ProductID == item.ProductID {
			if existing.Quantity >= maxQuantity {
				return false
			}
			c.Items[i].Name = item.Name
			c.Items[i].Price = item.Price
			c.Items[i].Unit = item.Unit
			c.Items[i].ImageURL = item.ImageURL
			c.Items[i].Quantity++
			return true
		}
	}
	c.Items = append(c.Items, item)
	return true
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

// UpdateQuantityLimited adjusts quantity by one step while enforcing a maximum.
// It returns false when the product is not present or the increment would exceed stock.
func (c *Cart) UpdateQuantityLimited(productID int64, delta int, maxQuantity int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if delta != 1 && delta != -1 {
		return false
	}
	for i, item := range c.Items {
		if item.ProductID != productID {
			continue
		}
		if delta > 0 && item.Quantity >= maxQuantity {
			return false
		}
		c.Items[i].Quantity += delta
		if c.Items[i].Quantity <= 0 {
			c.Items = append(c.Items[:i], c.Items[i+1:]...)
		}
		return true
	}
	return false
}

// Snapshot returns a stable copy of the cart items for rendering or checkout.
func (c *Cart) Snapshot() []CartItem {
	c.mu.Lock()
	defer c.mu.Unlock()
	items := make([]CartItem, len(c.Items))
	copy(items, c.Items)
	return items
}

// ReplaceItems swaps the cart contents with a caller-built validated snapshot.
func (c *Cart) ReplaceItems(items []CartItem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Items = items
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
	if ok {
		return c
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Double-check after acquiring write lock.
	if c, ok = s.carts[sessionID]; ok {
		return c
	}
	c = &Cart{}
	s.carts[sessionID] = c
	return c
}

// MigrateSession moves the cart from oldID to newID so session regeneration
// on login doesn't lose the user's cart.
func (s *CartStore) MigrateSession(oldID, newID string) {
	if oldID == newID {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.carts[oldID]; ok {
		s.carts[newID] = c
		delete(s.carts, oldID)
	}
}
