package handlers

import (
	"testing"
	"time"
)

// TestAdminOrderNotifyTargetDisabled verifies the disabled-by-default path:
// with neither ADMIN_NOTIFY_PHONE nor KAVENEGAR_TEMPLATE_ADMIN_ORDER set, the
// notification target is empty and no SMS is attempted.
func TestAdminOrderNotifyTargetDisabled(t *testing.T) {
	t.Setenv("ADMIN_NOTIFY_PHONE", "")
	t.Setenv("KAVENEGAR_TEMPLATE_ADMIN_ORDER", "")

	receptor, template := adminOrderNotifyTarget()
	if receptor != "" || template != "" {
		t.Fatalf("adminOrderNotifyTarget() = (%q, %q), want empty pair", receptor, template)
	}

	h, _ := newTestHandler(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.notifyAdminOrderAsync("TDJ-000001", "09121234567")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notifyAdminOrderAsync did not return; disabled path should be synchronous")
	}
}

// TestAdminOrderNotifyTargetEnabled verifies the env-var plumbing: both vars
// set are passed through as the receptor/template pair (whitespace on the
// phone number trimmed).
func TestAdminOrderNotifyTargetEnabled(t *testing.T) {
	t.Setenv("ADMIN_NOTIFY_PHONE", " 09120000000 ")
	t.Setenv("KAVENEGAR_TEMPLATE_ADMIN_ORDER", "admin-order-submit")

	receptor, template := adminOrderNotifyTarget()
	if receptor != "09120000000" {
		t.Fatalf("receptor = %q, want %q", receptor, "09120000000")
	}
	if template != "admin-order-submit" {
		t.Fatalf("template = %q, want %q", template, "admin-order-submit")
	}
}

// TestAdminOrderNotifyPhoneOnlyDisabled verifies that configuring only one of
// the two env vars (phone without template) keeps the notification off.
func TestAdminOrderNotifyPhoneOnlyDisabled(t *testing.T) {
	t.Setenv("ADMIN_NOTIFY_PHONE", "09120000000")
	t.Setenv("KAVENEGAR_TEMPLATE_ADMIN_ORDER", "")

	receptor, template := adminOrderNotifyTarget()
	if receptor == "" {
		t.Fatal("receptor should be set")
	}
	if template != "" {
		t.Fatalf("template = %q, want empty (notification disabled)", template)
	}
}
