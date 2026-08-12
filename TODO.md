By some random order, I don't even know...

[x] Automatically decrease stock once orders are sent
[x] Postal code validation
[x] Add a slideshow for each product type
[x] Add zarinpal gateway

[x] CSRF token cookie HttpOnly: false - accessible via JavaScript, defeats CSRF protection if XSS exists
[ ] Templates parsed once at startup - no hot reload in dev, syntax errors crash on boot
[ ] No payment reconciliation job - orders stuck in awaiting_payment only cleaned by janitor (15min TTL)
[ ] Product slug collision risk - strings.ToLower(strings.ReplaceAll(name, " ", "-")) with no uniqueness check
[ ] In-memory session store - sessions lost on restart, no revocation, not scalable, vulnerable to session fixation
[ ] OTP codes never cleaned up - otp_codes table grows indefinitely
[ ] No graceful shutdown - HTTP server doesn't handle SIGTERM, in-flight requests dropped
[ ] No structured logging - uses stdlib log, no levels, no JSON output for log aggregation

[ ] Add a guide for each product's benefits
[ ] Evaluate shipment costs and add to checkout
[ ] SMS/OTP service
[ ] Email for support
