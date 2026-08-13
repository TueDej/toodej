package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"farmstore/internal/database"
)

func TestPurgeExpiredSessions(t *testing.T) {
	h := &Handler{
		userSessions:  map[string]session{"s1": {userID: 1, expiresAt: time.Now().Add(-time.Hour)}, "s2": {userID: 2, expiresAt: time.Now().Add(time.Hour)}},
		pendingLogins: map[string]pendingLogin{"p1": {phone: "09121234567", expiresAt: time.Now().Add(-time.Hour)}, "p2": {phone: "09139998877", expiresAt: time.Now().Add(time.Hour)}},
		pendingNext:   map[string]pendingReturn{"n1": {url: "/checkout", expiresAt: time.Now().Add(-time.Hour)}, "n2": {url: "/orders", expiresAt: time.Now().Add(time.Hour)}},
	}

	h.purgeExpiredSessions(time.Now())

	if _, ok := h.userSessions["s1"]; ok {
		t.Error("expired user session not purged")
	}
	if _, ok := h.userSessions["s2"]; !ok {
		t.Error("valid user session purged")
	}
	if _, ok := h.pendingLogins["p1"]; ok {
		t.Error("expired pending login not purged")
	}
	if _, ok := h.pendingLogins["p2"]; !ok {
		t.Error("valid pending login purged")
	}
	if _, ok := h.pendingNext["n1"]; ok {
		t.Error("expired pending return not purged")
	}
	if _, ok := h.pendingNext["n2"]; !ok {
		t.Error("valid pending return purged")
	}
}

func TestGenerateOTP5(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		code := generateOTP5()
		if len(code) != 5 {
			t.Fatalf("generateOTP5() = %q, want 5 digits", code)
		}
		for _, r := range code {
			if r < '0' || r > '9' {
				t.Fatalf("generateOTP5() = %q contains non-digit", code)
			}
		}
		seen[code] = true
	}
	if len(seen) < 900 {
		t.Fatalf("generateOTP5() distribution too narrow: %d unique in 1000", len(seen))
	}
}

func TestServeSitemap(t *testing.T) {
	h, _ := newTestHandler(t)
	r, _ := http.NewRequest(http.MethodGet, "/sitemap.xml", nil)
	rec := httptest.NewRecorder()
	h.ServeSitemap(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("sitemap = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Fatalf("sitemap content-type = %q", ct)
	}
	body := rec.Body.String()
	for _, frag := range []string{"<urlset", "<loc>https://toodej.shop/</loc>", "<loc>https://toodej.shop/product/"} {
		if !strings.Contains(body, frag) {
			t.Errorf("sitemap missing %q", frag)
		}
	}
}

func TestServeRobotsTXT(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "/robots.txt", nil)
	rec := httptest.NewRecorder()
	ServeRobotsTXT(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("robots = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Disallow: /admin/") ||
		!strings.Contains(body, "Sitemap: https://toodej.shop/sitemap.xml") {
		t.Fatalf("robots content wrong: %q", body)
	}
}

func TestAuthFlowRejectsBadOTP(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	if r2 := c.get("/"); r2.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap = %d", r2.StatusCode)
	}

	resp := c.post("/auth/send-otp", url.Values{"phone": {"09121234567"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send-otp = %d", resp.StatusCode)
	}

	// Wrong code must not authenticate: protected routes still bounce to login.
	bad := c.post("/auth/verify-otp", url.Values{"phone": {"09121234567"}, "code": {"00000"}})
	if bad.StatusCode != http.StatusOK {
		t.Fatalf("bad-code verify = %d", bad.StatusCode)
	}
	if resp := c.get("/orders"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("invalid OTP left user authenticated (orders = %d)", resp.StatusCode)
	}

	// Expired pending-login binding must be rejected.
	c2 := newTestClient(t, r)
	_ = c2.get("/")
	c2.post("/auth/send-otp", url.Values{"phone": {"09139998877"}})
	sid := c2.cookies["session"].Value
	h.sessionMu.Lock()
	h.pendingLogins[sid] = pendingLogin{phone: "09139998877", expiresAt: time.Now().Add(-time.Minute)}
	h.sessionMu.Unlock()
	if err := database.CreateOTP(context.Background(), h.db, "09139998877", "11111", time.Now().Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	resp2 := c2.post("/auth/verify-otp", url.Values{"phone": {"09139998877"}, "code": {"11111"}})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expired verify = %d", resp2.StatusCode)
	}
	if !strings.Contains(c2.body(), "منقضی") {
		t.Fatalf("expired binding response did not mention expiry: %q", c2.body())
	}
	if _, ok := h.pendingLogins[sid]; ok {
		t.Fatal("expired pending login not cleared")
	}
}
