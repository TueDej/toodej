package database

import (
	"context"
	"testing"
	"time"
)

// TestValidOrderStatusOptions pins the forward-only status ladder that the
// admin panel's <select> is built from: only adjacent forward steps (the
// database deliberately rejects skips like pending → dispatched), cancel
// allowed from any active status, cancelled terminal.
func TestValidOrderStatusOptions(t *testing.T) {
	cases := map[string][]string{
		"awaiting_payment": {"awaiting_payment", "pending", "cancelled"},
		"pending":          {"pending", "preparing", "cancelled"},
		"preparing":        {"preparing", "dispatched", "cancelled"},
		"dispatched":       {"dispatched", "cancelled"},
		"cancelled":        {"cancelled"},
		"bogus":            nil,
	}
	for current, want := range cases {
		got := ValidOrderStatusOptions(current)
		if len(got) != len(want) {
			t.Fatalf("ValidOrderStatusOptions(%q) = %v, want %v", current, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ValidOrderStatusOptions(%q)[%d] = %q, want %q", current, i, got[i], want[i])
			}
		}
	}
}

// TestValidOrderStatusOptionsNeverMoveBackward cross-checks every option the
// UI offers against the database's own transition validator: for every status
// and every offered option, validOrderTransition must accept it.
func TestValidOrderStatusOptionsNeverMoveBackward(t *testing.T) {
	all := []string{"awaiting_payment", "pending", "preparing", "dispatched", "cancelled"}
	for _, current := range all {
		for _, next := range ValidOrderStatusOptions(current) {
			if !validOrderTransition(current, next) {
				t.Fatalf("UI offers %s → %s but the DB state machine rejects it", current, next)
			}
		}
		// And the critical direction: a cancelled order must never be offered
		// any way out.
		if current == "cancelled" && len(ValidOrderStatusOptions(current)) != 1 {
			t.Fatal("cancelled order must offer only itself")
		}
	}
}

// TestCreateOTPStoresUTCExpiry guards the timezone bug: the expiry must be
// written as a UTC wall-clock string because VerifyOTP parses it as UTC and
// the purge compares it with datetime('now'). Storing local time shifted the
// effective OTP lifetime by the server's UTC offset (hours, not minutes).
func TestCreateOTPStoresUTCExpiry(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// Build the expiry in a deliberately non-UTC location (UTC+3:30) to prove
	// the stored value is normalized to UTC, not the argument's zone.
	tehran := time.FixedZone("test+0330", 3*60*60+30*60)
	expiresAt := time.Now().In(tehran).Add(2 * time.Minute)

	if err := CreateOTP(ctx, db, "09123456789", "12345", expiresAt); err != nil {
		t.Fatalf("CreateOTP: %v", err)
	}

	var stored string
	if err := db.QueryRow("SELECT expires_at FROM otp_codes WHERE phone_number = ?", "09123456789").Scan(&stored); err != nil {
		t.Fatalf("query otp: %v", err)
	}
	if want := expiresAt.UTC().Format("2006-01-02 15:04:05"); stored != want {
		t.Fatalf("stored expires_at = %q, want %q (UTC)", stored, want)
	}
}

func TestVerifyOTPSingleUseAndExpiry(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// A fresh code verifies once, then is consumed.
	if err := CreateOTP(ctx, db, "09123456789", "12345", time.Now().Add(2*time.Minute)); err != nil {
		t.Fatalf("CreateOTP: %v", err)
	}
	ok, err := VerifyOTP(ctx, db, "09123456789", "12345")
	if err != nil || !ok {
		t.Fatalf("VerifyOTP valid = %v, %v; want true, nil", ok, err)
	}
	if ok, _ := VerifyOTP(ctx, db, "09123456789", "12345"); ok {
		t.Fatal("VerifyOTP reused an already-consumed code")
	}

	// An expired code must not verify. The expiry is written 2 minutes in the
	// past, which on an offset host used to leak into the future.
	if err := CreateOTP(ctx, db, "09987654321", "54321", time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatalf("CreateOTP expired: %v", err)
	}
	if ok, err := VerifyOTP(ctx, db, "09987654321", "54321"); ok || err != nil {
		t.Fatalf("VerifyOTP expired = %v, %v; want false, nil", ok, err)
	}
}
