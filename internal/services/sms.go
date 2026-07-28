package services

import (
	"log"
	"os"

	"github.com/kavenegar/kavenegar-go"
)

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
