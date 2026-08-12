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

func TestSendOTPNoKeyFallsBack(t *testing.T) {
	os.Setenv("KAVENEGAR_API_KEY", "")
	os.Setenv("DEV_MODE", "")
	if err := SendOTP("09121234567", "12345"); err != nil {
		t.Fatalf("SendOTP(no key) = %v, want nil", err)
	}
}
