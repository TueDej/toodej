package handlers

import (
	"testing"
	"time"
)

// TestCartStorePurgeIdle guards the unbounded-growth bug: carts minted for
// cookie-less visitors must be evicted once idle past the session lifetime.
func TestCartStorePurgeIdle(t *testing.T) {
	s := NewCartStore()

	old := s.Get("old-session") // never touched again after backdating
	fresh := s.Get("fresh-session")
	fresh.AddItemLimited(CartItem{ProductID: 1, Quantity: 1}, 5)

	// Backdate one cart past the session lifetime, then touch the other.
	old.mu.Lock()
	old.lastAccess = time.Now().Add(-sessionTTL - time.Minute)
	old.mu.Unlock()
	s.Get("fresh-session") // refreshes lastAccess

	removed := s.PurgeIdle(sessionTTL)
	if removed != 1 {
		t.Fatalf("PurgeIdle removed = %d, want 1", removed)
	}
	if _, ok := s.carts["old-session"]; ok {
		t.Fatal("stale cart was not evicted")
	}
	if _, ok := s.carts["fresh-session"]; !ok {
		t.Fatal("active cart was evicted")
	}

	// A Get after eviction lazily recreates the cart.
	s.Get("old-session")
	if _, ok := s.carts["old-session"]; !ok {
		t.Fatal("Get after purge did not recreate the cart")
	}
}

func TestCartLimitedMutations(t *testing.T) {
	cart := &Cart{}
	item := CartItem{ProductID: 1, Name: "fig", Price: 100, Quantity: 1}

	if !cart.AddItemLimited(item, 2) {
		t.Fatal("first add failed")
	}
	if !cart.AddItemLimited(item, 2) {
		t.Fatal("second add failed")
	}
	if cart.AddItemLimited(item, 2) {
		t.Fatal("third add exceeded stock")
	}
	if got := cart.Count(); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}

	if cart.UpdateQuantityLimited(1, 100, 2) {
		t.Fatal("accepted invalid delta")
	}
	if cart.UpdateQuantityLimited(1, 1, 2) {
		t.Fatal("increment exceeded stock")
	}
	if !cart.UpdateQuantityLimited(1, -1, 2) {
		t.Fatal("decrement failed")
	}
	if got := cart.Count(); got != 1 {
		t.Fatalf("count after decrement = %d, want 1", got)
	}
}
