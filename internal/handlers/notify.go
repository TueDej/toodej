package handlers

import (
	"context"
	"strconv"
	"strings"
	"time"

	"farmstore/internal/database"
	"farmstore/internal/logutil"
	"farmstore/internal/services"
)

// orderStatusTemplate maps an order status to the Kavenegar Verify.Lookup
// template that notifies the customer about it. Each template is optional:
// an unset env var disables that notification. "pending" here means the
// payment was just confirmed (awaiting_payment → pending).
func orderStatusTemplate(status string) string {
	switch status {
	case "pending":
		return envDefault("KAVENEGAR_TEMPLATE_ORDER_CONFIRMED", "")
	case "dispatched":
		return envDefault("KAVENEGAR_TEMPLATE_ORDER_DISPATCHED", "")
	case "cancelled":
		return envDefault("KAVENEGAR_TEMPLATE_ORDER_CANCELLED", "")
	}
	return ""
}

// notifyOrderStatusAsync sends the customer an SMS about an order status
// change without blocking the HTTP response (or the payment reconciler);
// failures are logged and swallowed — a missing or failed SMS must never
// fail the admin operation or the payment flow it rides on.
//
// token2 carries the notification's second value (postal tracking code for
// dispatched, paid amount for confirmed); it may be empty.
func (h *Handler) notifyOrderStatusAsync(orderID, status, token2 string) {
	template := orderStatusTemplate(status)
	if template == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		o, err := database.GetOrder(ctx, h.db, orderID)
		if err != nil {
			logutil.Error("order status sms: load order", "order_id", orderID, "err", err)
			return
		}
		// Order IDs contain a hyphen (TDJ-XXXXXX); Kavenegar Lookup tokens
		// reject separator characters (HTTP 431), so the token omits it.
		token := strings.ReplaceAll(orderID, "-", "")
		if err := services.SendOrderStatusSMS(o.CustomerPhone, template, token, token2); err != nil {
			logutil.Error("order status sms", "order_id", orderID, "phone", o.CustomerPhone, "err", err)
		}
	}()
}

// notifyOrderConfirmedAsync fires the "order confirmed" SMS after a payment
// actually transitioned the order (idempotent callback replays skip it).
func (h *Handler) notifyOrderConfirmedAsync(orderID string, totalAmount int) {
	if orderStatusTemplate("pending") == "" {
		return
	}
	// Plain digits: Lookup tokens reject separator characters like commas.
	h.notifyOrderStatusAsync(orderID, "pending", strconv.Itoa(totalAmount))
}
