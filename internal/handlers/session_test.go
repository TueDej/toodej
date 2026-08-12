package handlers

import (
	"testing"
	"time"
)

func TestGenerateSessionID(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id := generateSessionID()
		if !validSessionID(id) {
			t.Fatalf("generateSessionID() = %q, does not match session ID pattern", id)
		}
		if seen[id] {
			t.Fatalf("generateSessionID() returned duplicate %q", id)
		}
		seen[id] = true
	}
}

func TestRateLimiterAllow(t *testing.T) {
	rl := NewRateLimiter(3, 1000000) // effectively non-expiring window
	for i := 0; i < 3; i++ {
		if !rl.Allow("ip-1") {
			t.Fatalf("request %d within limit rejected", i+1)
		}
	}
	if rl.Allow("ip-1") {
		t.Fatal("request over limit accepted")
	}
	// A different key is unaffected.
	if !rl.Allow("ip-2") {
		t.Fatal("distinct key rejected")
	}
}

func TestRateLimiterWindowReset(t *testing.T) {
	rl := NewRateLimiter(2, time.Minute)
	if !rl.Allow("k") {
		t.Fatal("first request rejected")
	}
	if !rl.Allow("k") {
		t.Fatal("second request in window rejected")
	}
	if rl.Allow("k") {
		t.Fatal("third request in window accepted")
	}
}

func TestRateLimiterDefaults(t *testing.T) {
	rl := NewRateLimiter(0, 0) // invalid args should fall back to sane defaults
	if rl.limit != 1 {
		t.Errorf("limit = %d, want 1", rl.limit)
	}
	if rl.window != time.Minute {
		t.Errorf("window = %v, want 1m", rl.window)
	}
}
