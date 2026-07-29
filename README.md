# تودج (Toodej) — Farm Store E-Commerce

A Persian-language e-commerce web application for a small farm selling fresh figs, pomegranates, and derived products (jams, juice, concentrate). Built with Go, HTMX, and Tailwind CSS — zero JavaScript framework, zero build step.

---

## Tech Stack

| Layer          | Technology                                                    |
| -------------- | ------------------------------------------------------------- |
| Backend        | **Go 1.24** — `net/http` + `github.com/go-chi/chi/v5`         |
| Database       | **SQLite** — `modernc.org/sqlite` (pure Go, CGO-free)         |
| Frontend       | **Go `html/template`** + **HTMX 2.0** + **Tailwind CSS CDN**  |
| SMS/OTP        | **Kavenegar Go SDK** (`github.com/kavenegar/kavenegar-go`)     |
| Calendar       | **go-persian-calendar** (`github.com/yaa110/go-persian-calendar`) |

---

## Project Architecture

```
.
├── cmd/server/main.go          # Entry point — router setup, env vars
├── internal/
│   ├── database/db.go          # SQLite init, migrations, seeding, queries
│   ├── models/models.go        # Domain structs: Product, Order, User, OTPCode
│   ├── services/sms.go         # Kavenegar OTP sending (falls back to stdout in DEV_MODE)
│   ├── utils/date.go           # Persian (Jalali) date formatting + Persian digit conversion
│   └── handlers/
│       ├── handlers.go         # Handler struct, template engine, Home, Cart, Checkout, Confirmation, UserOrders
│       ├── auth.go             # OTP-based login flow (SendOTP, VerifyOTP, Logout)
│       ├── admin.go            # Admin dashboard — order status, product CRUD
│       ├── cart.go             # In-memory Cart/CartStore with per-session carts
│       └── middleware.go       # HTTP Basic Auth middleware for /admin
├── templates/
│   ├── layout.html             # Base layout — nav, footer, toast container, HTMX/CDN includes
│   ├── index.html              # Home page — hero banner, product grid, category filter
│   ├── cart.html               # Shopping cart with +/-/remove, desktop + mobile layouts
│   ├── checkout.html           # Checkout form (name, phone, address)
│   ├── confirmation.html       # Order confirmation with summary + delivery info
│   ├── login.html              # OTP login — phone input → code input
│   ├── orders.html             # Authenticated user's order history
│   └── admin.html              # Admin panel — orders table + products CRUD with tabs
├── assets/
│   ├── hero-bg-1.png           # Hero section background
│   └── hero-bg.jpg             # Hero section fallback background
├── deploy.sh                   # Production deployment script (systemd + Caddy)
├── run.sh                      # Development build & run script
├── palette.json                # Design color palette reference
├── AGENTS.md                   # AI agent directives for this repo
├── go.mod / go.sum             # Go module dependencies
└── README.md                   # This file
```

---

## Core Functionality & Flow

### 1. OTP Authentication (via Kavenegar)

Uses the **Kavenegar `Verify.Lookup`** method rather than the lower-level SMS send API. This delegates OTP code storage and template rendering to Kavenegar's built-in verify service.

- **`POST /auth/send-otp`** — receives a phone number, creates/retrieves the user, generates a random 5-digit code, stores it in `otp_codes` with a 2-minute expiry.
- **`POST /auth/verify-otp`** — verifies the code against `otp_codes`, checks expiry + `is_used` flag, then sets a server-side session cookie (in-memory `map[string]int64`).
- **DEV_MODE fallback:** When `DEV_MODE=true` or `KAVENEGAR_API_KEY` is empty, OTP codes are logged to stdout instead of being sent via SMS.

### 2. Session Management

- Random 32-hex-char session IDs (via `crypto/rand`) stored in HTTP-only cookies (7-day expiry).
- Sessions map to user IDs in an in-memory `sync.RWMutex`-guarded map.
- Pending logins (phone → session binding before OTP verification) use a separate map.

### 3. Jalali (Persian) Calendar Integration

- All dates/times displayed to users are converted via `github.com/yaa110/go-persian-calendar`.
- Template functions `persianDate` and `persianDateTime` render dates in Persian format.
- Western digits are converted to Persian/Arabic Unicode digits (`۰۱۲۳۴۵۶۷۸۹`).

### 4. Random Order IDs

- Orders receive IDs in the format `TDJ-XXXXXX` where `X` is a random alphanumeric character (`A-Z0-9`).
- Generated via `crypto/rand` for unpredictability (no sequential IDs).

### 5. HTMX Shopping Cart

- **In-memory per-session cart** (`CartStore`). Not persisted — cart is lost on server restart.
- Cart interactions use `hx-post` with `hx-target="#cart-content"` / `hx-target="#cart-count"`.
- `HX-Trigger` events (`cartUpdated`, `cartEvent`) update the cart badge in the navbar and display toast notifications (slide-in/slide-out) without page reload.
- Category filtering on the home page uses `hx-get` with `hx-target="#product-section"` and `hx-push-url` for bookmarkable URLs.

### 6. Checkout Flow

1. User must be logged in (redirected to `/login` otherwise).
2. Cart must be non-empty (redirected to `/cart` otherwise).
3. `POST /checkout` validates name/phone/address, creates an order with `TDJ-XXXXXX` ID, links it to the user, and redirects to `/checkout/confirmation/{id}`.
4. The order confirmation page displays order summary and delivery info.

### 7. Admin Panel

- Protected by HTTP Basic Auth (`ADMIN_USER` / `ADMIN_PASS` env vars).
- Tabbed dashboard: Orders table with inline status select (HTMX `hx-post` on change) and Products table with inline price/stock editing + toggle for active/inactive.

---

## Environment & Configuration

All configuration is via environment variables:

| Variable             | Default          | Description                                               |
| -------------------- | ---------------- | --------------------------------------------------------- |
| `PORT`               | `8080`           | HTTP listen port                                          |
| `DB_PATH`            | `farmstore.db`   | SQLite database file path                                 |
| `ADMIN_USER`         | `admin`          | Admin panel Basic Auth username                           |
| `ADMIN_PASS`         | `admin123`       | Admin panel Basic Auth password                           |
| `KAVENEGAR_API_KEY`  | _(empty)_        | Kavenegar SMS API key (leave empty for DEV_MODE)          |
| `KAVENEGAR_TEMPLATE` | `verify-otp`     | Kavenegar verification template name                      |
| `DEV_MODE`           | _(not set)_      | Set to `true` to log OTPs to stdout instead of SMS        |

### Running locally

```bash
# Quick start (builds and runs):
export DEV_MODE=true
export PORT=8080
go run ./cmd/server

# Or using the run script:
./run.sh
```

The server listens on `http://localhost:8080`. Admin panel at `http://localhost:8080/admin`. In DEV_MODE, OTP codes appear in the server logs / inline in the login form.

---

## Deployment

A production-ready `deploy.sh` script handles the full deployment pipeline:

1. **Reads previous config** from the existing systemd service file (if any).
2. **Prompts** for port, admin credentials, and Kavenegar API key.
3. **Builds** a stripped production binary (`go build -ldflags="-s -w"`).
4. **Copies** the binary to `/usr/local/bin/farmstore` and templates/assets to `/var/lib/farmstore/`.
5. **Creates/enables** a systemd service (`farmstore.service`) with `DynamicUser=yes` and `Restart=on-failure`.
6. **Optionally configures** a Caddy reverse proxy block.

```bash
# On the VPS:
./deploy.sh
```

### systemd service details

- **Binary:** `/usr/local/bin/farmstore`
- **Data directory:** `/var/lib/farmstore/`
- **Database:** `/var/lib/farmstore/farmstore.db`
- **Service name:** `farmstore.service`
- **User:** Dynamic system user (created by `DynamicUser=yes`)

---

## Database Schema

```sql
products  (id, name, slug, category, description, price, stock_quantity, unit, image_url, is_active, created_at)
users     (id, phone_number, created_at)
orders    (id TEXT PK, customer_name, customer_phone, customer_address, total_amount, status, user_id FK, created_at)
order_items (id, order_id FK, product_id FK, quantity, price_per_unit)
otp_codes (id, phone_number, code, expires_at, is_used)
```

- `orders.id` is a `TEXT PRIMARY KEY` — stores `TDJ-XXXXXX` format order IDs.
- `orders.status` is constrained via `CHECK` to `pending | processing | completed | cancelled`.
- `order_items` has a `UNIQUE(order_id, product_id)` constraint.
- Schema migration is automatic on startup (`CREATE TABLE IF NOT EXISTS` + a best-effort `ALTER TABLE` for the `user_id` column).

---

## Design & Branding

- **Primary color:** Garnet (`#8B263E`)
- **Secondary:** Forest (`#2D4A3E`)
- **Background:** Cream (`#FBF9F5`)
- **Cards:** Parchment (`#F4EFE6`)
- **Text:** Charcoal (`#2C302E`)
- **Fonts:** Estedad (sans) + Alyamama (serif) via Google Fonts

See `palette.json` for the full color reference.
