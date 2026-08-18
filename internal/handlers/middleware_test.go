package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
		wantStatus int
	}{
		{"mutating missing origin allowed", http.MethodPost, "", http.StatusOK},
		{"mutating same origin allowed", http.MethodPost, "http://shop.test", http.StatusOK},
		{"mutating same origin host different scheme allowed", http.MethodPost, "https://shop.test", http.StatusOK},
		{"mutating cross origin blocked", http.MethodPost, "http://evil.test", http.StatusForbidden},
		{"mutating malformed origin blocked", http.MethodPost, "not a url", http.StatusForbidden},
		{"get with cross origin allowed", http.MethodGet, "http://evil.test", http.StatusOK},
		{"put blocked cross origin", http.MethodPut, "http://evil.test", http.StatusForbidden},
		{"delete blocked cross origin", http.MethodDelete, "http://evil.test", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/checkout", nil)
			req.Host = "shop.test"
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			rec := httptest.NewRecorder()
			SameOrigin(noopHandler()).ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

func TestBasicAuth(t *testing.T) {
	// Missing credentials → 401 with WWW-Authenticate.
	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	rec := httptest.NewRecorder()
	BasicAuth("admin", "secret")(noopHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no creds = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate header")
	}

	// Wrong credentials → 401.
	req.SetBasicAuth("admin", "wrong")
	rec = httptest.NewRecorder()
	BasicAuth("admin", "secret")(noopHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong creds = %d, want 401", rec.Code)
	}

	// Correct credentials → pass through.
	req.SetBasicAuth("admin", "secret")
	rec = httptest.NewRecorder()
	BasicAuth("admin", "secret")(noopHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct creds = %d, want 200", rec.Code)
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
				r.Form[k] = vs
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
