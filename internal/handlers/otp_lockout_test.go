package handlers

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// sendOTP starts the OTP exchange for a phone number and returns the code the
// server stored, so tests can answer correctly or deliberately wrongly.
func sendOTP(t *testing.T, c *testClient, h *Handler, phone string) string {
	t.Helper()
	if resp := c.post("/auth/send-otp", url.Values{"phone": {phone}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("send-otp %s = %d (body: %s)", phone, resp.StatusCode, c.body())
	}
	return otpCode(t, h.db, phone)
}

// wrongCode returns a 5-digit code that is guaranteed not to be the real one.
func wrongCode(real string) string {
	if real == "00000" {
		return "11111"
	}
	return "00000"
}

// assertRetargeted checks the response is an error fragment aimed at the login
// page's #login-error slot rather than a replacement for the whole form — this
// is what keeps the digit boxes on screen after a failed attempt.
func assertRetargeted(t *testing.T, resp *http.Response, body string) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", resp.StatusCode, body)
	}
	if got := resp.Header.Get("HX-Retarget"); got != "#login-error" {
		t.Fatalf("HX-Retarget = %q, want #login-error", got)
	}
	if got := resp.Header.Get("HX-Reswap"); got != "innerHTML" {
		t.Fatalf("HX-Reswap = %q, want innerHTML", got)
	}
	if strings.Contains(body, `id="login-form"`) {
		t.Fatalf("error fragment re-rendered the form, wiping the input: %s", body)
	}
	if resp.Header.Get("HX-Redirect") != "" {
		t.Fatal("error fragment set HX-Redirect")
	}
}

// A wrong code must cost one attempt and leave the form (and the typed phone
// number) alone, so the correct code still logs the user in afterwards.
func TestOTPWrongCodeKeepsFormAndCountsAttempt(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	if resp := c.get("/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap = %d", resp.StatusCode)
	}
	const phone = "09121110000"
	code := sendOTP(t, c, h, phone)

	resp := c.post("/auth/verify-otp", url.Values{"code": {wrongCode(code)}})
	assertRetargeted(t, resp, c.body())
	if !strings.Contains(c.body(), "otp-attempt-error") {
		t.Fatalf("wrong code did not tag the response as retryable: %s", c.body())
	}
	want := toPersianDigits(fmt.Sprint(maxOTPFailuresPerPhone - 1))
	if !strings.Contains(c.body(), want) {
		t.Fatalf("wrong code did not report %s remaining attempts: %s", want, c.body())
	}

	// The pending login survived, so the right code still works.
	resp = c.post("/auth/verify-otp", url.Values{"code": {code}})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("HX-Redirect") == "" {
		t.Fatalf("retry with correct code = %d, HX-Redirect %q (body: %s)",
			resp.StatusCode, resp.Header.Get("HX-Redirect"), c.body())
	}
	if lock := h.otpLockRemaining(phone, "127.0.0.1"); lock > 0 {
		t.Fatalf("successful login left the phone on cooldown (%s)", lock)
	}
}

// An incomplete code is a slip, not a guess: it must not burn an attempt.
func TestOTPEmptyCodeCostsNoAttempt(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	_ = c.get("/")
	const phone = "09121110001"
	code := sendOTP(t, c, h, phone)

	for i := 0; i < maxOTPFailuresPerPhone+2; i++ {
		resp := c.post("/auth/verify-otp", url.Values{"code": {""}})
		assertRetargeted(t, resp, c.body())
	}
	if resp := c.post("/auth/verify-otp", url.Values{"code": {code}}); resp.Header.Get("HX-Redirect") == "" {
		t.Fatalf("empty codes locked the number out (body: %s)", c.body())
	}
}

// Spending the attempt budget puts the number on cooldown: neither the correct
// code nor a fresh code request gets through until the cooldown elapses.
func TestOTPLockoutAfterRepeatedWrongCodes(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	_ = c.get("/")
	const phone = "09121110002"
	code := sendOTP(t, c, h, phone)
	bad := url.Values{"code": {wrongCode(code)}}

	for i := 1; i < maxOTPFailuresPerPhone; i++ {
		resp := c.post("/auth/verify-otp", bad)
		assertRetargeted(t, resp, c.body())
		if strings.Contains(c.body(), "otp-lock-timer") {
			t.Fatalf("locked out after %d of %d attempts", i, maxOTPFailuresPerPhone)
		}
	}

	resp := c.post("/auth/verify-otp", bad)
	assertRetargeted(t, resp, c.body())
	if !strings.Contains(c.body(), "otp-lock-timer") {
		t.Fatalf("attempt %d did not start the cooldown: %s", maxOTPFailuresPerPhone, c.body())
	}
	if lock := h.otpLockRemaining(phone, "127.0.0.1"); lock <= 0 || lock > otpLockoutDuration {
		t.Fatalf("cooldown = %s, want (0, %s]", lock, otpLockoutDuration)
	}

	// The correct code is refused while the cooldown runs.
	resp = c.post("/auth/verify-otp", url.Values{"code": {code}})
	assertRetargeted(t, resp, c.body())
	if !strings.Contains(c.body(), "otp-lock-timer") {
		t.Fatalf("correct code bypassed the cooldown: %s", c.body())
	}

	// ...and so is a new code request, so the lock cannot be reset by resending.
	resp = c.post("/auth/send-otp", url.Values{"phone": {phone}})
	assertRetargeted(t, resp, c.body())
	if !strings.Contains(c.body(), "otp-lock-timer") {
		t.Fatalf("send-otp bypassed the cooldown: %s", c.body())
	}

	// Once the cooldown expires the number logs in again with a full budget.
	h.otpAttempts.setNow(func() time.Time { return time.Now().Add(otpLockoutDuration + time.Second) })
	resp = c.post("/auth/verify-otp", url.Values{"code": {code}})
	if resp.Header.Get("HX-Redirect") == "" {
		t.Fatalf("login after cooldown failed: %d (body: %s)", resp.StatusCode, c.body())
	}
}

// A single host cycling through phone numbers trips the (much looser) per-IP
// budget, which locks even a number that has never failed.
func TestOTPLockoutPerIPAcrossPhones(t *testing.T) {
	r, h, _ := newTestRouter(t)
	c := newTestClient(t, r)
	_ = c.get("/")

	failures := 0
	for i := 0; failures < maxOTPFailuresPerIP; i++ {
		phone := fmt.Sprintf("091211100%02d", i+10)
		code := sendOTP(t, c, h, phone)
		bad := url.Values{"code": {wrongCode(code)}}
		// Each number is walked up to its own budget, then abandoned.
		for j := 0; j < maxOTPFailuresPerPhone && failures < maxOTPFailuresPerIP; j++ {
			resp := c.post("/auth/verify-otp", bad)
			assertRetargeted(t, resp, c.body())
			failures++
		}
	}
	if lock := h.otpAttempts.lockedFor("ip:127.0.0.1"); lock <= 0 {
		t.Fatalf("%d failures across phones did not lock the IP", maxOTPFailuresPerIP)
	}

	// An untouched number from the same address is turned away too.
	fresh := c.post("/auth/send-otp", url.Values{"phone": {"09121119999"}})
	assertRetargeted(t, fresh, c.body())
	if !strings.Contains(c.body(), "otp-lock-timer") {
		t.Fatalf("IP cooldown did not apply to a fresh number: %s", c.body())
	}
}

// Failures older than otpFailureWindow are forgotten, so occasional typos over
// a long session never add up to a lockout.
func TestAttemptTrackerForgetsStaleFailures(t *testing.T) {
	tr := newAttemptTracker()
	now := time.Now()
	tr.setNow(func() time.Time { return now })

	for i := 1; i < maxOTPFailuresPerPhone; i++ {
		if lock, remaining := tr.fail("phone:x", maxOTPFailuresPerPhone); lock != 0 || remaining != maxOTPFailuresPerPhone-i {
			t.Fatalf("fail #%d = (%s, %d)", i, lock, remaining)
		}
	}

	now = now.Add(otpFailureWindow + time.Second)
	if lock, remaining := tr.fail("phone:x", maxOTPFailuresPerPhone); lock != 0 || remaining != maxOTPFailuresPerPhone-1 {
		t.Fatalf("stale failures were not forgotten: (%s, %d)", lock, remaining)
	}
	if lock := tr.lockedFor("phone:x"); lock != 0 {
		t.Fatalf("lockedFor after forgetting = %s, want 0", lock)
	}
}
