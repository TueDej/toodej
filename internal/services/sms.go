// Package services provides integrations with external providers such as Kavenegar SMS.
package services

import (
	"fmt"
	"os"

	"farmstore/internal/logutil"

	"github.com/kavenegar/kavenegar-go"
)

// SendOTP sends a verification code via Kavenegar's message send endpoint,
// matching the provider's documented usage:
//
//	api.Message.Send(sender, receptor, message, nil)
//
// The full SMS body is composed locally with the OTP token embedded, so no
// pre-registered Kavenegar template is required.
//
// When DEV_MODE=true or KAVENEGAR_API_KEY is empty, the code is logged to stdout
// instead of being sent as an actual SMS. This allows development without a real
// API key or phone number.
func SendOTP(receptor, token string) error {
	apiKey := os.Getenv("KAVENEGAR_API_KEY")
	sender := os.Getenv("KAVENEGAR_SENDER")
	messageTemplate := os.Getenv("KAVENEGAR_MESSAGE")
	if messageTemplate == "" {
		messageTemplate = "کد تایید شما در فروشگاه: %s"
	}

	if os.Getenv("DEV_MODE") == "true" || apiKey == "" {
		logutil.Info("dev mode: OTP not sent via SMS", "phone", receptor, "code", token)
		return nil
	}

	if sender == "" {
		err := fmt.Errorf("KAVENEGAR_SENDER is not configured")
		logutil.Error("Kavenegar OTP send failed", "phone", receptor, "err", err)
		return err
	}

	message := fmt.Sprintf(messageTemplate, token)

	logutil.Info("sending OTP via Kavenegar", "phone", receptor, "sender", sender)
	api := kavenegar.New(apiKey)
	if _, err := api.Message.Send(sender, []string{receptor}, message, nil); err != nil {
		logutil.Error("Kavenegar OTP send failed", "phone", receptor, "err", err)
		return err
	}
	logutil.Info("OTP sent successfully", "phone", receptor)
	return nil
}
