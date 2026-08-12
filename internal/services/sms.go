// Package services provides integrations with external providers such as Kavenegar SMS.
package services

import (
	"os"

	"farmstore/internal/logutil"

	"github.com/kavenegar/kavenegar-go"
)

// SendOTP sends a verification code via Kavenegar's Verify.Lookup API.
//
// When DEV_MODE=true or KAVENEGAR_API_KEY is empty, the code is logged to stdout
// instead of being sent as an actual SMS. This allows development without a real
// API key or phone number.
//
// Using Verify.Lookup (rather than the raw SMS send endpoint) delegates OTP template
// rendering and code storage to Kavenegar's own verify service, which simplifies the
// server-side implementation.
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
	_, err := api.Verify.Lookup(receptor, template, token, nil)
	if err != nil {
		logutil.Error("Kavenegar OTP send failed", "phone", receptor, "err", err)
		return err
	}
	logutil.Info("OTP sent successfully", "phone", receptor)
	return nil
}
