# CHECKS.md — Manual Verification Checklist

This document is the manual test plan for **Toodej** (the farm-store app). Use it after
a build/deploy to confirm both the **customer (user)** and **admin** sides work end to end.
Automated tests (`go test ./...`) cover a lot, but they cannot verify the rendered UI, the
real Zarinpal payment flow, real SMS delivery, or behaviour under a real browser — so do walk
through these steps.

> The storefront UI is Persian (RTL). The checks below describe *behaviour*; the on-screen
> text will be in Farsi.

---

## 0. Prerequisites & environment

Confirm the server is configured and started correctly before testing:

- [ ] Server builds and starts (`go build ./...` then run the binary / `./deploy.sh`).
- [ ] Logs show `server starting` and no fatal about admin credentials.
- [ ] **Production warning:** if `DEV_MODE` is not `true`, the process must refuse to start
      with default `admin`/`admin123` credentials — set `ADMIN_USER` and `ADMIN_PASS`
      (≥8 chars). In `DEV_MODE=true` the defaults `admin`/`admin123` are allowed (warning logged).
- [ ] Relevant env vars are set (see table). In dev mode, OTP codes are **logged to stdout**
      and also shown on the login page, so no real SMS is needed.
- [ ] `GET /health` returns `{"status":"ok"}`.

| Env var             | Purpose                                              | Dev default / note                 |
|---------------------|------------------------------------------------------|------------------------------------|
| `DEV_MODE`          | Enables text logging, default admin creds, OTP logs  | `true` for local testing           |
| `PORT`              | HTTP port                                            | `8080`                             |
| `DB_PATH`           | SQLite file path                                     | `farmstore.db`                     |
| `APP_BASE_URL`      | Base URL used for payment callback                   | `https://toodej.shop`              |
| `ADMIN_USER` / `ADMIN_PASS` | Admin panel login credentials                  | `admin`/`admin123` in dev only     |
| `ZARINPAL_MERCHANT_ID` | Zarinpal gateway merchant ID                     | required for real payments         |
| `ZARINPAL_SANDBOX`  | `true` → sandbox endpoints (default); `false` → live | set `true` for testing             |
| `KAVENEGAR_API_KEY` | Kavenegar SMS key (empty ⇒ OTP only logged)          | leave empty in dev                 |
| `KAVENEGAR_TEMPLATE`| Kavenegar OTP template name                          | `verify-otp`                       |
| `ADMIN_NOTIFY_PHONE` / `KAVENEGAR_TEMPLATE_ADMIN_ORDER` | Admin phone + template for order-submission SMS | empty ⇒ disabled; DEV_MODE logs instead |
| `LOG_LEVEL` / `LOG_FORMAT` | `debug|info|warn|error` / `json|text`          | `info` / `text` in dev             |

> **Cart caveat:** the cart is in-memory per server instance. Restarting the server clears
> all carts. Single-server only.

---

## 1. Customer (user) flow

### 1.1 Browsing & storefront
- [ ] **Home `/`** loads: hero, story strip, featured products row, seasonal banner, and the
      five category tiles (spring/summer/autumn/dried/processed).
- [ ] **Seasonal banner** points to the correct category for the current Gregorian season
      (spring→`/products/spring`, summer→`/products/summer`, autumn→`/products/autumn`,
      else→`/products/dried`).
- [ ] **Category listing `/products/{category}`** works for every slug:
      `spring`, `summer`, `autumn`, `dried`, `processed`, `all`.
- [ ] An **unknown category** (e.g. `/products/foo`) returns `404`.
- [ ] **About `/about`** loads.
- [ ] **Assets** load (CSS, images under `/assets/...`).
- [ ] **`/sitemap.xml`** and **`/robots.txt`** return content.

### 1.2 Cart
- [ ] **Add to cart** from a product (POST `/cart/add` with `product_id`) → success; cart badge
      count increases.
- [ ] **Cart count** (`/cart/count`) shows the correct Persian-numeral total.
- [ ] **View cart `/cart`** lists items with correct **product name, unit price, quantity,
      subtotal, and total**.
- [ ] **Update quantity** (POST `/cart/update` with `product_id` + `delta` of `+1`/`-1`):
  - [ ] `+1` increases; `-1` decreases; dropping to 0 removes the line.
  - [ ] Invalid delta (e.g. `5`, or `0`) is rejected with `400`.
  - [ ] Quantity **cannot exceed available stock** (capped at stock; extra add is rejected).
- [ ] **Remove item** (POST `/cart/remove` with `product_id`) deletes the line.
- [ ] **Inactive products** are not addable / are excluded from the cart display.
- [ ] Cart contents **persist across login** (add items, then log in — items remain).

### 1.3 Authentication (OTP login)
- [ ] Visiting a protected page while logged out (e.g. `/checkout`) redirects to
      `/login?next=<page>`.
- [ ] **Send OTP** (POST `/auth/send-otp` with `phone`):
  - [ ] Valid Iranian phone (`09` + 9 digits) → OTP generated; in dev mode the code appears
        on the page and in the logs.
  - [ ] Invalid phone → friendly error, **no user created**, no SMS attempt.
  - [ ] Per-phone rate limit eventually blocks repeated sends (HTTP error).
  - [ ] A number on login cooldown (see below) cannot get a new code — the countdown is shown
        instead, so the wrong-code budget cannot be reset by resending.
- [ ] **Verify OTP** (POST `/auth/verify-otp` with `phone` + `code`):
  - [ ] Correct code → logged in, session cookie set, **redirected to the `next` URL**
        (e.g. `/checkout`), not the homepage.
  - [ ] Wrong code → error appears **below the digit boxes**; the boxes are cleared and
        refocused but the form itself stays (the phone number does **not** have to be
        re-entered), and the message counts down the remaining attempts.
  - [ ] Typing the correct code after a wrong one still logs in.
  - [ ] Expired code → error and the resend button becomes usable immediately.
  - [ ] **5 wrong codes** for one number → inputs disabled and a `mm:ss` cooldown (5 min)
        ticks down; the correct code is refused while it runs. When it hits zero the inputs
        come back with a "you can request a new code" line, and login works again.
  - [ ] Resend timer / resend button works, and the resend button is **not** re-enabled by its
        90-second timer while a lockout countdown is still running.
- [ ] **Logout** (`/logout`) clears the session and CSRF cookies and redirects home; protected
      pages then require login again.

### 1.4 Checkout & payment
- [ ] **Checkout form `/checkout`** (logged in) loads with cart contents.
- [ ] **Preview** (POST `/checkout/preview`) validates shipping and shows totals; invalid
      shipping (bad phone, short address, bad postal code) is rejected with `400`.
- [ ] **Place order** (POST `/checkout`) with valid shipping:
  - [ ] Order created in `awaiting_payment`; **stock is reserved** (product stock decreases by
        the ordered quantity).
  - [ ] Redirects to the **Zarinpal gateway** (`StartPay` URL) with the correct amount
        (toman→rial conversion; `amount × 10`).
- [ ] **Cancel at gateway** (`/checkout/verify?Status=…` other than OK): order set to
      `cancelled` and **reserved stock is restored**.
- [ ] **Successful payment** (`/checkout/verify?Authority=…&Status=OK`):
  - [ ] Order moves to `pending`; payment reference id stored.
  - [ ] Redirects to **confirmation page `/checkout/confirmation/{id}`**.
  - [ ] **Confirmation page shows the correct product name(s)** for each line item (not a
        shifted/wrong name) and the correct totals.
  - [ ] **IDOR check:** opening another user's confirmation URL returns `404`; that order does
        **not** appear in the other user's order history.
- [ ] **Order history `/orders`** lists only the logged-in user's orders with correct status.
- [ ] **Resume payment** (the "ادامه پرداخت" button on `/orders` for an `awaiting_payment`
      order) re-issues a Zarinpal authority and redirects to the gateway. It is a
      CSRF-protected `POST /orders/{id}/pay`, so it must be triggered from the page's button
      (a bare `GET` of that URL now returns `405`).

### 1.5 Payment edge cases (verify stock integrity)
- [ ] Place an order, then **let the gateway callback be lost** (close browser after paying):
      the **payment reconciler** (runs every minute) should confirm the order to `pending`
      (check logs + order status). Stock is never needlessly restored.
- [ ] **Abandon an order** in `awaiting_payment` for >15 min: the **unpaid-order janitor**
      cancels it and restores stock (check logs + stock).

---

## 2. Admin flow

### 2.1 Access & dashboard
- [ ] Visiting `/admin` redirects to the **login page `/admin/login`**; wrong credentials
      rejected with an error, no session issued.
- [ ] Correct `ADMIN_USER`/`ADMIN_PASS` → **dashboard `/admin/`** loads, showing orders and
      products and counts; **خروج از پنل** link logs out and re-protects the panel.
- [ ] Admin rate limiter allows normal use but blocks abuse.
- [ ] HTMX interactions on the admin panel (status change, product edits) work without full
      page reloads.

### 2.2 Orders
- [ ] **Order detail `/admin/orders/{id}`** shows order fields, customer info, and **all line
      items with the correct product name, quantity, price, and subtotal**.
- [ ] **Update status** (`/admin/orders/{id}/status`):
  - [ ] Allowed transitions only (state machine enforced):
    - `awaiting_payment` → `pending` or `cancelled`
    - `pending` → `preparing` or `cancelled`
    - `preparing` → `dispatched` or `cancelled`
    - `dispatched` → `cancelled`
    - `cancelled` → (no further transitions)
    - same status → allowed
  - [ ] An **invalid transition** (e.g. `pending` → `dispatched` skipping `preparing`) is
        rejected (`400`).
  - [ ] The status `<select>` updates in place after change.
- [ ] **Status badge** (`/admin/orders/{id}/status-badge`) updates the badge element via HTMX.

### 2.3 Products
- [ ] **Create product** (`/admin/products`, POST): requires `name`, `category`, `price`;
      optional `stock_quantity`, `unit`, `description`.
  - [ ] Success prepends a new row; a **unique slug** is auto-generated (collisions handled).
  - [ ] Missing required fields → `400`.
- [ ] **Update product** (`/admin/products/{id}`, POST): editing `price` and/or
      `stock_quantity` (comma-separated thousands accepted) updates and re-renders the row;
      invalid values are ignored (kept as-is).
- [ ] **Toggle active/inactive** (`/admin/products/{id}/toggle`): flips `IsActive`;
      inactive products are hidden/excluded from the **storefront** but still in admin.
- [ ] Changes are immediately visible on the storefront category/listing pages.

---

## 3. Security & correctness (cross-cutting)

- [ ] **CSRF:** every mutating form/HTMX POST carries the token (header or hidden field) and
      succeeds; a POST **without** the token is rejected with `403`. (The cookie alone is not
      accepted.)
- [ ] **Session fixation:** logging in issues a **new session id**; the pre-login session id
      is discarded.
- [ ] **IDOR:** one user cannot view another user's order (confirmation/order-history).
- [ ] **Security headers** present on responses (e.g. `X-Content-Type-Options: nosniff`, etc.).
- [ ] **Same-origin** policy enforced for mutating requests.
- [ ] **Rate limiters** active on login / send-otp / verify-otp / admin.
- [ ] **OTP brute force:** 5 wrong codes lock that number out of login for 5 minutes (both
      verify and send); many wrong codes across different numbers from one address eventually
      lock that address too. A log line records the lockout with only the last 4 digits of
      the number.
- [ ] **Input validation:** phone, postal code, order id validated; bad values rejected
      gracefully (no 500s).
- [ ] **No broken pages / 500s** during the full walkthrough (watch the logs for errors).

---

## 4. Quick smoke order (suggested)

1. `/health` → ok.
2. Browse `/` and `/products/spring`.
3. Add an item → view `/cart` → update qty.
4. `/checkout` (redirects to login) → OTP login (use dev code) → redirected back to `/checkout`.
5. Place order → pay in Zarinpal sandbox → confirmation shows correct names → `/orders` lists it.
6. Admin: `/admin` (login page) → open the order → advance status through the allowed
   transitions → create/toggle a product → confirm it appears/ disappears on the storefront.
7. Re-run the security spot-checks (CSRF-without-token → 403; IDOR → 404).
