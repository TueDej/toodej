package handlers

import (
	"context"
	"os"
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

// adminOrderNotifyTarget returns the (receptor, template) pair used to inform
// the admins about a new order submission: ADMIN_NOTIFY_PHONE picks up the
// admin's phone number and KAVENEGAR_TEMPLATE_ADMIN_ORDER names the Verify
// Lookup template that must be pre-defined in the Kavenegar panel. Either
// being unset disables the notification.
func adminOrderNotifyTarget() (receptor, template string) {
	return strings.TrimSpace(os.Getenv("ADMIN_NOTIFY_PHONE")),
		envDefault("KAVENEGAR_TEMPLATE_ADMIN_ORDER", "")
}

// notifyAdminOrderAsync informs the admins that a new order was submitted
// (customer completed checkout, order created and awaiting payment). It never
// blocks the HTTP response; failures are logged and swallowed — a missing or
// failed admin SMS must never fail the checkout flow.
//
// %token carries the order ID without its hyphen (Lookup rejects separator
// characters); %token2 carries the customer's phone number so the admin can
// follow up without opening the panel.
func (h *Handler) notifyAdminOrderAsync(orderID, customerPhone string) {
	receptor, template := adminOrderNotifyTarget()
	if receptor == "" || template == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// Re-read the order so the notification reflects the persisted state.
		if _, err := database.GetOrder(ctx, h.db, orderID); err != nil {
			logutil.Error("admin order sms: load order", "order_id", orderID, "err", err)
			return
		}
		token := strings.ReplaceAll(orderID, "-", "")
		if err := services.SendOrderStatusSMS(receptor, template, token, customerPhone); err != nil {
			logutil.Error("admin order sms", "order_id", orderID, "phone", receptor, "err", err)
		}
	}()
}
