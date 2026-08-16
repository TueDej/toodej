// Package services provides integrations with external providers such as Kavenegar SMS.
package services

import (
	"os"

	"farmstore/internal/logutil"

	"github.com/kavenegar/kavenegar-go"
)

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
// When DEV_MODE=true or KAVENEGAR_API_KEY is empty, the code is logged to stdout
// instead of being sent as an actual SMS. This allows development without a real
// API key or phone number.
func SendOTP(receptor, token string) error {
	apiKey := os.Getenv("KAVENEGAR_API_KEY")
	template := os.Getenv("KAVENEGAR_TEMPLATE")
	if template == "" {
		template = "verify-otp"
	}

	if os.Getenv("DEV_MODE") == "true" || apiKey == "" {
		logutil.Info("dev mode: OTP not sent via SMS", "phone", receptor, "code", token)
		return nil
	}

	logutil.Info("sending OTP via Kavenegar", "phone", receptor, "template", template)
	api := kavenegar.New(apiKey)
	if _, err := api.Verify.Lookup(receptor, template, token, nil); err != nil {
		logutil.Error("Kavenegar OTP send failed", "phone", receptor, "err", err)
		return err
	}
	logutil.Info("OTP sent successfully", "phone", receptor)
	return nil
}
