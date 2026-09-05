// Package services provides integrations with external providers such as Kavenegar SMS.
package services

import (
	"fmt"
	"os"

	"farmstore/internal/logutil"

	"github.com/kavenegar/kavenegar-go"
)

// phoneSuffix returns the last four digits of a phone number, so log lines can
// correlate an SMS with a flow without recording the full number.
func phoneSuffix(phone string) string {
	if len(phone) <= 4 {
		return phone
	}
	return phone[len(phone)-4:]
}

// SendOTP sends a verification code via Kavenegar's Verify.Lookup API.
//
// Verify.Lookup is Kavenegar's dedicated authentication endpoint: it does not
// require a sender number (the system picks the best line), its messages get
// highest-priority delivery and are never filtered as promotional, and it can
// deliver internationally. The message body is rendered from a template that
// must be pre-defined in the Kavenegar panel; the OTP token is interpolated
// into the template's %token placeholder.
//
// The token must contain no whitespace and at most 100 characters — our OTP is
// a 5-digit numeric code, so it always satisfies this constraint.
//
// When DEV_MODE=true the code is logged to stdout instead of being sent as an
// actual SMS (development without a real API key or phone number). Without
// DEV_MODE an empty KAVENEGAR_API_KEY is a hard error, never a silent
// fallback: silently logging the OTP to stdout in production would publish
// every login code to whoever can read the logs.
func SendOTP(receptor, token string) error {
	if os.Getenv("DEV_MODE") == "true" {
		logutil.Info("dev mode: OTP not sent via SMS", "phone_suffix", phoneSuffix(receptor), "code", token)
		return nil
	}

	apiKey := os.Getenv("KAVENEGAR_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("KAVENEGAR_API_KEY is not set (set DEV_MODE=true for local development)")
	}
	template := os.Getenv("KAVENEGAR_TEMPLATE")
	if template == "" {
		template = "verify-otp"
	}

	logutil.Info("sending OTP via Kavenegar", "phone_suffix", phoneSuffix(receptor), "template", template)
	api := kavenegar.New(apiKey)
	if _, err := api.Verify.Lookup(receptor, template, token, nil); err != nil {
		logutil.Error("Kavenegar OTP send failed", "phone_suffix", phoneSuffix(receptor), "err", err)
		return err
	}
	logutil.Info("OTP sent successfully", "phone_suffix", phoneSuffix(receptor))
	return nil
}

// SendOrderStatusSMS notifies a customer about an order status change via
// Kavenegar's Verify.Lookup, using one of the order-notification templates
// defined in the Kavenegar panel (order-confirmed / order-dispatched /
// order-cancelled, resolved by the caller from the KAVENEGAR_TEMPLATE_ORDER_*
// env vars). An empty template disables that notification.
//
// token is interpolated into the template's %token placeholder (the order ID
// without its hyphen — Lookup rejects separator characters), token2 into
// %token2 (the postal tracking code or paid amount); an empty token2 is
// simply omitted from the request.
//
// Like SendOTP: DEV_MODE logs the payload instead of sending; without
// DEV_MODE an empty API key is a hard error.
func SendOrderStatusSMS(receptor, template, token, token2 string) error {
	if template == "" {
		return nil
	}
	if os.Getenv("DEV_MODE") == "true" {
		logutil.Info("dev mode: order status SMS not sent", "phone_suffix", phoneSuffix(receptor), "template", template, "token", token, "token2", token2)
		return nil
	}

	apiKey := os.Getenv("KAVENEGAR_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("KAVENEGAR_API_KEY is not set (set DEV_MODE=true for local development)")
	}
	logutil.Info("sending order status SMS via Kavenegar", "phone_suffix", phoneSuffix(receptor), "template", template)
	api := kavenegar.New(apiKey)
	param := &kavenegar.VerifyLookupParam{}
	if token2 != "" {
		param.Token2 = token2
	}
	if _, err := api.Verify.Lookup(receptor, template, token, param); err != nil {
		logutil.Error("Kavenegar order status SMS failed", "phone_suffix", phoneSuffix(receptor), "template", template, "err", err)
		return err
	}
	logutil.Info("order status SMS sent", "phone_suffix", phoneSuffix(receptor), "template", template)
	return nil
}
