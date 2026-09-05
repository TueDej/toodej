package services

import (
	"os"
	"testing"
)

// TestSendOTPDevMode verifies the no-send path: with no Kavenegar API key
// configured the function behaves as a no-op logger rather than making a
// network call, so tests and development flow work offline.
func TestSendOTPDevMode(t *testing.T) {
	old := os.Getenv("KAVENEGAR_API_KEY")
	os.Setenv("KAVENEGAR_API_KEY", "")
	os.Setenv("DEV_MODE", "true")
	defer func() {
		os.Setenv("KAVENEGAR_API_KEY", old)
		os.Setenv("DEV_MODE", "")
	}()

	if err := SendOTP("09121234567", "12345"); err != nil {
		t.Fatalf("SendOTP(no key, dev mode) = %v, want nil", err)
	}
}

// TestSendOTPNoKeyFailsClosed pins the fail-closed behavior: without DEV_MODE
// and without an API key the send must return an error instead of silently
// logging the OTP to stdout (which in production would publish login codes to
// whoever can read the logs).
func TestSendOTPNoKeyFailsClosed(t *testing.T) {
	os.Setenv("KAVENEGAR_API_KEY", "")
	os.Setenv("DEV_MODE", "")
	if err := SendOTP("09121234567", "12345"); err == nil {
		t.Fatal("SendOTP(no key, no dev mode) = nil, want error")
	}
	if err := SendOrderStatusSMS("09121234567", "order-confirmed", "TDJ000001", ""); err == nil {
		t.Fatal("SendOrderStatusSMS(no key, no dev mode) = nil, want error")
	}
}
