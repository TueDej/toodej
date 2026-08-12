package handlers

import "testing"

func TestValidIranianPhone(t *testing.T) {
	valid := []string{"09123456789", "09901234567", "09190001122"}
	for _, s := range valid {
		if !validIranianPhone(s) {
			t.Errorf("validIranianPhone(%q) = false, want true", s)
		}
	}

	invalid := []string{"", "0912345678", "091234567890", "19123456789", "0912345678a", "+989123456789", "0901234567", " 09123456789"}
	for _, s := range invalid {
		if validIranianPhone(s) {
			t.Errorf("validIranianPhone(%q) = true, want false", s)
		}
	}
}

func TestValidPostalCode(t *testing.T) {
	if !validPostalCode("1234567890") {
		t.Error("validPostalCode(1234567890) = false, want true")
	}
	for _, s := range []string{"", "123456789", "12345678901", "123456789a", "12345678 0"} {
		if validPostalCode(s) {
			t.Errorf("validPostalCode(%q) = true, want false", s)
		}
	}
}

func TestValidOrderID(t *testing.T) {
	if !validOrderID("TDJ-ABC123") {
		t.Error("validOrderID(TDJ-ABC123) = false, want true")
	}
	if !validOrderID("TDJ-999999") {
		t.Error("validOrderID(TDJ-999999) = false, want true")
	}
	for _, s := range []string{"", "TDJ-ABC12", "TDJ-ABC1234", "TDJ-", "abc-ABC123", "TDJ-ABC12\n", "TDJ-abc123", "TDJ-ABC123 "} {
		if validOrderID(s) {
			t.Errorf("validOrderID(%q) = true, want false", s)
		}
	}
}

func TestValidSessionID(t *testing.T) {
	if !validSessionID("abcdef0123456789abcdef0123456789") {
		t.Error("validSessionID(32-hex) = false, want true")
	}
	for _, s := range []string{"", "abc", "ABCDEF0123456789ABCDEF0123456789", "ghijkl0123456789abcdef0123456789", "abcdef0123456789abcdef01234567890"} {
		if validSessionID(s) {
			t.Errorf("validSessionID(%q) = true, want false", s)
		}
	}
}
