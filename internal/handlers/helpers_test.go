package handlers

import "testing"

func TestToPersianDigits(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"0123456789", "۰۱۲۳۴۵۶۷۸۹"},
		{"09123456789", "۰۹۱۲۳۴۵۶۷۸۹"},
		{"1,000، تومان", "۱,۰۰۰، تومان"},
		{"abc", "abc"},
	}
	for _, tt := range tests {
		if got := toPersianDigits(tt.in); got != tt.want {
			t.Errorf("toPersianDigits(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatToman(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		{0, "۰ تومان"},
		{5, "۵ تومان"},
		{1000, "۱,۰۰۰ تومان"},
		{1299000, "۱,۲۹۹,۰۰۰ تومان"},
		{1234567890, "۱,۲۳۴,۵۶۷,۸۹۰ تومان"},
	}
	for _, tt := range tests {
		if got := formatToman(tt.in); got != tt.want {
			t.Errorf("formatToman(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSanitizeReturnURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace", "   ", ""},
		{"root", "/", "/"},
		{"relative path", "/about", "/about"},
		{"relative path with query", "/orders?page=2", "/orders?page=2"},
		{"relative path with fragment", "/cart#top", "/cart#top"},
		{"absolute URL", "https://evil.com/login", "/"},
		{"protocol-relative", "//evil.com", "/"},
		{"javascript scheme", "javascript:alert(1)", "/"},
		{"backslash prefix", "/\\evil.com", "/"},
		{"backslash inside", "/foo\\bar", "/"},
		{"control char", "/foo\x00bar", "/"},
		{"host present", "//host/path", "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeReturnURL(tt.in); got != tt.want {
				t.Errorf("sanitizeReturnURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSecureEqual(t *testing.T) {
	if !secureEqual("admin", "admin") {
		t.Error("secureEqual(admin, admin) = false, want true")
	}
	if secureEqual("admin", "admin1") {
		t.Error("secureEqual(admin, admin1) = true, want false")
	}
	if secureEqual("", "x") {
		t.Error("secureEqual(empty, x) = true, want false")
	}
	if secureEqual("x", "") {
		t.Error("secureEqual(x, empty) = true, want false")
	}
}
