package handlers

import (
	"net/http"
	"net/url"
	"testing"
)

// TestLoginRedirectsToNext reproduces the reported bug: visiting
// /login?next=/checkout and completing OTP login must redirect back to
// /checkout, not to the home page. The post-login destination is stored on the
// pre-auth session and migrated to the regenerated session ID on login, so the
// handler must read it from the NEW session id.
func TestLoginRedirectsToNext(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)

	resp := c.get("/login?next=/checkout")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /login?next=/checkout = %d", resp.StatusCode)
	}

	phone := "09121234567"
	resp = c.post("/auth/send-otp", url.Values{"phone": {phone}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send-otp = %d", resp.StatusCode)
	}

	code := otpCode(t, h.db, phone)
	resp = c.post("/auth/verify-otp", url.Values{"phone": {phone}, "code": {code}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("verify-otp = %d", resp.StatusCode)
	}

	loc := resp.Header.Get("HX-Redirect")
	if loc != "/checkout" {
		t.Fatalf("HX-Redirect = %q, want /checkout", loc)
	}
}
