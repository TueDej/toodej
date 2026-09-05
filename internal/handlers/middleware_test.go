package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// noopHandler returns 200 with a marker body so wrapping middleware can be
// observed.
func noopHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
}

func TestSecurityHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	SecurityHeaders(noopHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	expect := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
		"Permissions-Policy":     "camera=(), microphone=(), geolocation=()",
	}
	for k, v := range expect {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, frag := range []string{"default-src 'self'", "frame-ancestors 'none'", "object-src 'none'"} {
		if !strings.Contains(csp, frag) {
			t.Errorf("CSP missing %q: %q", frag, csp)
		}
	}
}

func TestSecurityHeadersStatusOK(t *testing.T) {
	rec := httptest.NewRecorder()
	SecurityHeaders(noopHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestSameOrigin(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		origin     string
		referer    string
		cookie     string
		wantStatus int
	}{
		{"mutating missing origin no cookies allowed", http.MethodPost, "", "", "", http.StatusOK},
		{"mutating same origin allowed", http.MethodPost, "http://shop.test", "", "", http.StatusOK},
		{"mutating same origin host different scheme allowed", http.MethodPost, "https://shop.test", "", "", http.StatusOK},
		{"mutating cross origin blocked", http.MethodPost, "http://evil.test", "", "", http.StatusForbidden},
		{"mutating malformed origin blocked", http.MethodPost, "not a url", "", "", http.StatusForbidden},
		{"get with cross origin allowed", http.MethodGet, "http://evil.test", "", "", http.StatusOK},
		{"put blocked cross origin", http.MethodPut, "http://evil.test", "", "", http.StatusForbidden},
		{"delete blocked cross origin", http.MethodDelete, "http://evil.test", "", "", http.StatusForbidden},
		// Cookie-carrying POSTs with neither Origin nor Referer are rejected:
		// a real browser always sends one of the two on mutating requests.
		{"mutating missing headers with session cookie blocked", http.MethodPost, "", "", "session=abc", http.StatusForbidden},
		{"mutating missing headers with admin cookie blocked", http.MethodPost, "", "", "admin_session=abc", http.StatusForbidden},
		// Referer fallback: same host passes, foreign host is blocked.
		{"mutating same-host referer allowed", http.MethodPost, "", "http://shop.test/checkout", "", http.StatusOK},
		{"mutating cross-host referer blocked", http.MethodPost, "", "http://evil.test/checkout", "", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/checkout", nil)
			req.Host = "shop.test"
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.referer != "" {
				req.Header.Set("Referer", tt.referer)
			}
			if tt.cookie != "" {
				key, val, _ := strings.Cut(tt.cookie, "=")
				req.AddCookie(&http.Cookie{Name: key, Value: val})
			}
			rec := httptest.NewRecorder()
			SameOrigin(noopHandler()).ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

func TestRequireAdmin(t *testing.T) {
	h := &Handler{adminSessions: make(map[string]time.Time)}

	// Unauthenticated browser navigation → 303 to the login page.
	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	rec := httptest.NewRecorder()
	h.RequireAdmin(noopHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/login" {
		t.Fatalf("no session = %d %q, want 303 /admin/login", rec.Code, rec.Header().Get("Location"))
	}

	// Unauthenticated HTMX request → bare 401 (never the login page).
	req = httptest.NewRequest(http.MethodGet, "/admin/orders/1/status-badge", nil)
	req.Header.Set("HX-Request", "true")
	rec = httptest.NewRecorder()
	h.RequireAdmin(noopHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("htmx no session = %d, want 401", rec.Code)
	}

	// Valid admin session cookie → pass through.
	sid, err := generateSessionID()
	if err != nil {
		t.Fatalf("generateSessionID: %v", err)
	}
	h.adminSessions[sid] = time.Now().Add(time.Hour)
	req = httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req.AddCookie(&http.Cookie{Name: adminCookieName, Value: sid})
	rec = httptest.NewRecorder()
	h.RequireAdmin(noopHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid session = %d, want 200", rec.Code)
	}

	// Expired admin session → redirect again.
	h.adminSessions[sid] = time.Now().Add(-time.Minute)
	req = httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req.AddCookie(&http.Cookie{Name: adminCookieName, Value: sid})
	rec = httptest.NewRecorder()
	h.RequireAdmin(noopHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expired session = %d, want 303", rec.Code)
	}
}

func TestCSRFMiddleware(t *testing.T) {
	post := func(extra func(*http.Request)) (int, http.Header) {
		req := httptest.NewRequest(http.MethodPost, "/checkout", nil)
		if extra != nil {
			extra(req)
		}
		rec := httptest.NewRecorder()
		CSRFMiddleware(noopHandler()).ServeHTTP(rec, req)
		return rec.Code, rec.Header()
	}
	withForm := func(v url.Values) func(*http.Request) {
		return func(r *http.Request) {
			r.Method = http.MethodPost
			r.Body = http.NoBody
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.ParseForm()
			for k, vs := range v {
				r.PostForm[k] = vs
			}
		}
	}
	withCookie := func(val string) func(*http.Request) {
		return func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: val})
		}
	}
	withHeader := func(val string) func(*http.Request) {
		return func(r *http.Request) {
			r.Header.Set(csrfHeaderName, val)
		}
	}

	cases := []struct {
		name string
		f    func(*http.Request)
		want int
	}{
		{"no token", nil, http.StatusForbidden},
		{"matching cookie + form", func(r *http.Request) {
			withCookie("abc")(r)
			withForm(url.Values{csrfFormField: {"abc"}})(r)
		}, http.StatusOK},
		{"mismatched cookie + form", func(r *http.Request) {
			withCookie("abc")(r)
			withForm(url.Values{csrfFormField: {"xyz"}})(r)
		}, http.StatusForbidden},
		{"cookie alone is rejected", withCookie("abc"), http.StatusForbidden},
		{"matching cookie + header", func(r *http.Request) {
			withCookie("abc")(r)
			withHeader("abc")(r)
		}, http.StatusOK},
		{"mismatched cookie + header", func(r *http.Request) {
			withCookie("abc")(r)
			withHeader("xyz")(r)
		}, http.StatusForbidden},
		{"empty form token is rejected", func(r *http.Request) {
			withCookie("abc")(r)
			withForm(url.Values{csrfFormField: {""}})(r)
		}, http.StatusForbidden}, // empty request token must not fall through to the cookie
		{"exempt verify path", func(r *http.Request) {
			r.URL.Path = "/checkout/verify"
		}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := post(tc.f)
			if got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCSRFMiddlewareNonMutatingPasses(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	CSRFMiddleware(noopHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET without token = %d, want 200", rec.Code)
	}
}

// TestCSRFTokenCookieAttributes pins down the security attributes of the CSRF
// cookie: it must be HttpOnly (not readable by JavaScript) and SameSite=Lax.
func TestCSRFTokenCookieAttributes(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ensureCSRFToken(rec, req)

	token := ""
	found := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == csrfCookieName {
			found = true
			token = c.Value
			if !c.HttpOnly {
				t.Error("csrf cookie must be HttpOnly (HttpOnly = false)")
			}
			if c.SameSite != http.SameSiteLaxMode {
				t.Errorf("csrf cookie SameSite = %v, want Lax", c.SameSite)
			}
			if c.Path != "/" {
				t.Errorf("csrf cookie Path = %q, want /", c.Path)
			}
		}
	}
	if !found {
		t.Fatal("csrf cookie not set")
	}
	if len(token) != 64 { // 32 random bytes hex-encoded
		t.Errorf("csrf token length = %d, want 64", len(token))
	}

	// Requesting again with the cookie present must reuse the same token.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})
	rec2 := httptest.NewRecorder()
	if got := ensureCSRFToken(rec2, req2); got != token {
		t.Errorf("second ensureCSRFToken = %q, want existing %q", got, token)
	}
}

// TestSessionCookieAttributes ensures the session cookie is HttpOnly and Lax.
func TestSessionCookieAttributes(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h := &Handler{}
	sid := h.getOrCreateSessionID(rec, req)
	if !validSessionID(sid) {
		t.Fatalf("session id %q invalid", sid)
	}
	found := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == "session" {
			found = true
			if !c.HttpOnly {
				t.Error("session cookie must be HttpOnly")
			}
			if c.SameSite != http.SameSiteLaxMode {
				t.Errorf("session cookie SameSite = %v, want Lax", c.SameSite)
			}
		}
	}
	if !found {
		t.Fatal("session cookie not set")
	}
}
