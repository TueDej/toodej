package payment

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestTomanToRial(t *testing.T) {
	got, err := TomanToRial(129900)
	if err != nil {
		t.Fatalf("TomanToRial returned error: %v", err)
	}
	if got != 1299000 {
		t.Fatalf("TomanToRial = %d, want 1299000", got)
	}

	if _, err := TomanToRial(-1); err == nil {
		t.Fatal("TomanToRial(-1) returned nil error")
	}
	if _, err := TomanToRial(1 << 62); err == nil {
		t.Fatal("TomanToRial(huge) returned no overflow error")
	}
}

// fakeRESTGateway is a tiny fake Zarinpal v4 gateway for exercising the client.
type fakeRESTGateway struct {
	mu      sync.Mutex
	url     string
	reqCode int      // code the /request endpoint returns (default 100)
	verCode int      // code the /verify endpoint returns (default 100)
	seen    []string // captured request bodies, for assertions
}

func newFakeREST(t *testing.T, configure func(*fakeRESTGateway)) *fakeRESTGateway {
	t.Helper()
	g := &fakeRESTGateway{reqCode: 100, verCode: 100}
	if configure != nil {
		configure(g)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/request", func(w http.ResponseWriter, r *http.Request) {
		b := readAllBody(t, r)
		g.mu.Lock()
		g.seen = append(g.seen, b)
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if g.reqCode == 100 {
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"authority": "AUTH-XYZ123", "code": 100, "message": "ok"},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"code": g.reqCode, "message": "bad request"},
		})
	})
	mux.HandleFunc("/verify", func(w http.ResponseWriter, r *http.Request) {
		b := readAllBody(t, r)
		g.mu.Lock()
		g.seen = append(g.seen, b)
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		data := map[string]any{"code": g.verCode, "message": "status"}
		if g.verCode == 100 || g.verCode == 101 {
			data["ref_id"] = 987654321
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	g.url = ts.URL
	return g
}

func (g *fakeRESTGateway) client() *Zarinpal {
	return NewTestClient("merchant-test", g.url+"/request", g.url+"/verify", g.url+"/pg/StartPay/", http.DefaultClient)
}

func readAllBody(t *testing.T, r *http.Request) string {
	t.Helper()
	var sb strings.Builder
	defer r.Body.Close()
	buf := make([]byte, 512)
	for {
		n, err := r.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

func (g *fakeRESTGateway) seenBodies() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.seen))
	copy(out, g.seen)
	return out
}

func (g *fakeRESTGateway) setRequestCode(c int) {
	g.mu.Lock()
	g.reqCode = c
	g.mu.Unlock()
}

func (g *fakeRESTGateway) setVerifyCode(c int) {
	g.mu.Lock()
	g.verCode = c
	g.mu.Unlock()
}

// url is assigned in newFakeREST after the httptest server starts.

func TestRequestPaymentSuccess(t *testing.T) {
	g := newFakeREST(t, nil)
	auth, err := g.client().RequestPayment(1299000, "https://shop/callback", "order TDJ-000001")
	if err != nil {
		t.Fatalf("RequestPayment: %v", err)
	}
	if auth != "AUTH-XYZ123" {
		t.Fatalf("authority = %q", auth)
	}

	bodies := g.seenBodies()
	if len(bodies) != 1 {
		t.Fatalf("expected 1 request, got %d", len(bodies))
	}
	for _, frag := range []string{`"merchant_id":"merchant-test"`, `"amount":1299000`, `"callback_url":"https://shop/callback"`, `"description":"order TDJ-000001"`} {
		if !strings.Contains(bodies[0], frag) {
			t.Errorf("request body missing %s: %s", frag, bodies[0])
		}
	}
}

func TestRequestPaymentGatewayError(t *testing.T) {
	g := newFakeREST(t, nil)
	g.setRequestCode(101)
	if _, err := g.client().RequestPayment(1000, "cb", "d"); err == nil {
		t.Fatal("expected error for non-100 code")
	}
}

func TestRequestPaymentNetworkError(t *testing.T) {
	z := NewTestClient("m", "http://127.0.0.1:1/request", "http://127.0.0.1:1/verify", "http://x/", &http.Client{Transport: &http.Transport{}})
	if _, err := z.RequestPayment(1000, "cb", "d"); err == nil {
		t.Fatal("expected network error")
	}
}

func TestGatewayURL(t *testing.T) {
	g := newFakeREST(t, nil)
	if got := g.client().GatewayURL("AUTH1"); got != g.url+"/pg/StartPay/AUTH1" {
		t.Fatalf("GatewayURL = %q", got)
	}
}

func TestVerifyPaymentSuccess(t *testing.T) {
	g := newFakeREST(t, nil)
	res, err := g.client().VerifyPayment(12990000, "AUTH1")
	if err != nil {
		t.Fatalf("VerifyPayment: %v", err)
	}
	if !res.OK || res.RefID != 987654321 {
		t.Fatalf("result = %+v", res)
	}
}

func TestVerifyPaymentAlreadyVerified(t *testing.T) {
	g := newFakeREST(t, nil)
	g.setVerifyCode(101)
	res, err := g.client().VerifyPayment(1, "AUTH1")
	if err != nil {
		t.Fatalf("VerifyPayment: %v", err)
	}
	if !res.OK {
		t.Fatal("code 101 should be treated as OK")
	}
}

func TestVerifyPaymentFailure(t *testing.T) {
	g := newFakeREST(t, nil)
	g.setVerifyCode(102)
	res, err := g.client().VerifyPayment(1, "AUTH1")
	if err != nil {
		t.Fatalf("VerifyPayment: %v", err)
	}
	if res.OK {
		t.Fatal("code 102 should be reported as failed")
	}
}

func TestVerifyPaymentNetworkError(t *testing.T) {
	z := NewTestClient("m", "http://127.0.0.1:1/request", "http://127.0.0.1:1/verify", "http://x/", &http.Client{Transport: &http.Transport{}})
	if _, err := z.VerifyPayment(1, "A"); err == nil {
		t.Fatal("expected network error")
	}
}
