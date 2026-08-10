// Package payment provides a thin client for the Zarinpal payment gateway.
package payment

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	sandboxRequestURL  = "https://sandbox.zarinpal.com/pg/v4/payment/request.json"
	sandboxVerifyURL   = "https://sandbox.zarinpal.com/pg/v4/payment/verify.json"
	sandboxGatewayBase = "https://sandbox.zarinpal.com/pg/StartPay/"

	productionRequestURL  = "https://api.zarinpal.com/pg/v4/payment/request.json"
	productionVerifyURL   = "https://api.zarinpal.com/pg/v4/payment/verify.json"
	productionGatewayBase = "https://zarinpal.com/pg/StartPay/"
)

// Zarinpal is a lightweight client for the Zarinpal payment gateway (v4 API).
type Zarinpal struct {
	merchantID   string
	sandbox      bool
	requestURL   string
	verifyURL    string
	gatewayBase  string
	httpClient   *http.Client
}

// New creates a Zarinpal client. If sandbox is true the sandbox endpoints are used.
func New(merchantID string, sandbox bool) *Zarinpal {
	requestURL := productionRequestURL
	verifyURL := productionVerifyURL
	gatewayBase := productionGatewayBase
	if sandbox {
		requestURL = sandboxRequestURL
		verifyURL = sandboxVerifyURL
		gatewayBase = sandboxGatewayBase
	}
	return &Zarinpal{
		merchantID:  merchantID,
		sandbox:     sandbox,
		requestURL:  requestURL,
		verifyURL:   verifyURL,
		gatewayBase: gatewayBase,
		httpClient:  &http.Client{Timeout: 15 * time.Second},
	}
}

// NewFromEnv creates a Zarinpal client from environment variables:
// ZARINPAL_MERCHANT_ID and ZARINPAL_SANDBOX (default "true").
func NewFromEnv() *Zarinpal {
	merchantID := os.Getenv("ZARINPAL_MERCHANT_ID")
	sandbox := os.Getenv("ZARINPAL_SANDBOX") != "false" // default sandbox
	return New(merchantID, sandbox)
}

// paymentRequest is the JSON body sent to Zarinpal's payment request endpoint.
type paymentRequest struct {
	MerchantID  string `json:"merchant_id"`
	Amount      int    `json:"amount"`
	CallbackURL string `json:"callback_url"`
	Description string `json:"description"`
}

// paymentResponse is the JSON response from Zarinpal's payment request endpoint.
type paymentResponse struct {
	Data struct {
		Authority string `json:"authority"`
		Code      int    `json:"code"`
		Message   string `json:"message"`
	} `json:"data"`
	Errors []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// RequestPayment initiates a payment request and returns the authority token
// needed to redirect the user to the payment gateway.
func (z *Zarinpal) RequestPayment(amount int, callbackURL, description string) (string, error) {
	body, err := json.Marshal(paymentRequest{
		MerchantID:  z.merchantID,
		Amount:      amount,
		CallbackURL: callbackURL,
		Description: description,
	})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	resp, err := z.httpClient.Post(z.requestURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("zarinpal request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	var pr paymentResponse
	if err := json.Unmarshal(respBody, &pr); err != nil {
		return "", fmt.Errorf("unmarshal response: %w", err)
	}

	if pr.Data.Code != 100 {
		return "", fmt.Errorf("zarinpal error code %d: %s", pr.Data.Code, pr.Data.Message)
	}

	return pr.Data.Authority, nil
}

// GatewayURL builds the full payment gateway URL from an authority token.
func (z *Zarinpal) GatewayURL(authority string) string {
	return z.gatewayBase + authority
}

// verifyRequest is the JSON body sent to Zarinpal's verify endpoint.
type verifyRequest struct {
	MerchantID string `json:"merchant_id"`
	Amount     int    `json:"amount"`
	Authority  string `json:"authority"`
}

// verifyResponse is the JSON response from Zarinpal's verify endpoint.
type verifyResponse struct {
	Data struct {
		Code      int    `json:"code"`
		Message   string `json:"message"`
		RefID     int64  `json:"ref_id"`
		CardPan   string `json:"card_pan"`
		FeeType   string `json:"fee_type"`
		Fee       int    `json:"fee"`
	} `json:"data"`
	Errors []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// VerifyResult holds the outcome of a payment verification.
type VerifyResult struct {
	RefID   int64
	Message string
	OK      bool // true when code is 100 (verified) or 101 (already verified)
}

// VerifyPayment verifies a completed transaction with Zarinpal.
// Amount must match the original request amount (in Rial).
func (z *Zarinpal) VerifyPayment(amount int, authority string) (*VerifyResult, error) {
	body, err := json.Marshal(verifyRequest{
		MerchantID: z.merchantID,
		Amount:     amount,
		Authority:  authority,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal verify: %w", err)
	}

	resp, err := z.httpClient.Post(z.verifyURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("zarinpal verify: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read verify response: %w", err)
	}

	var vr verifyResponse
	if err := json.Unmarshal(respBody, &vr); err != nil {
		return nil, fmt.Errorf("unmarshal verify response: %w", err)
	}

	return &VerifyResult{
		RefID:   vr.Data.RefID,
		Message: vr.Data.Message,
		OK:      vr.Data.Code == 100 || vr.Data.Code == 101,
	}, nil
}
