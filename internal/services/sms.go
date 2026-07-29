// Package services provides integrations with external providers such as Kavenegar SMS.
package services

import (
	"log"
	"os"

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
		log.Printf("[DEV MODE] OTP for %s: %s", receptor, token)
		return nil
	}

	log.Printf("[SMS] sending OTP to %s via Kavenegar (template: %s)", receptor, template)
	api := kavenegar.New(apiKey)
	_, err := api.Verify.Lookup(receptor, template, token, nil)
	if err != nil {
		log.Printf("[SMS] Kavenegar error: %v", err)
		return err
	}
	log.Printf("[SMS] OTP sent successfully to %s", receptor)
	return nil
}
