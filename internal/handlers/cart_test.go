package handlers

import "testing"

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
