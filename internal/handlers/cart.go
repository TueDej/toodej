package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
)

type CartItem struct {
	ProductID int64  `json:"product_id"`
	Name      string `json:"name"`
	Price     int    `json:"price"`
	Unit      string `json:"unit"`
	Quantity  int    `json:"quantity"`
	ImageURL  string `json:"image_url"`
}

type Cart struct {
	mu    sync.Mutex
	Items []CartItem
}

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

func (c *Cart) Total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, item := range c.Items {
		total += item.Price * item.Quantity
	}
	return total
}

func (c *Cart) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, item := range c.Items {
		count += item.Quantity
	}
	return count
}

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

func (c *Cart) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Items = nil
}

type CartStore struct {
	mu    sync.RWMutex
	carts map[string]*Cart
}

func NewCartStore() *CartStore {
	return &CartStore{carts: make(map[string]*Cart)}
}

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

func generateSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
