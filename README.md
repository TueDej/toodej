# Toodej — Farm Store E-Commerce

A Persian-language e-commerce web application for a small farm selling fresh figs, pomegranates, and derived products. Built with Go, HTMX, and Tailwind CSS — zero JavaScript framework, zero build step.

## Tech Stack

| Layer     | Technology |
| --------- | ---------- |
| Backend   | **Go 1.24** — `net/http` + `github.com/go-chi/chi/v5` |
| Database  | **SQLite** — `modernc.org/sqlite` (pure Go, CGO-free) |
| Frontend  | **Go `html/template`** + **HTMX 2.0** + **Tailwind CSS CDN** |
| SMS/OTP   | **Kavenegar Go SDK** (`github.com/kavenegar/kavenegar-go`) |
| Calendar  | **go-persian-calendar** (`github.com/yaa110/go-persian-calendar`) |

## Prerequisites

- Go 1.24.x (must remain compatible with Go 1.24)

## Setup & Running

```bash
# Clone the repository
git clone https://github.com/TueDej/toodej.git
cd toodej

# Run in development mode (OTP codes logged to stdout, no SMS needed)
export DEV_MODE=true
export PORT=8080
go run ./cmd/server
```

Or use the run script:

```bash
./run.sh
```

The server starts at `http://localhost:8080`. Admin panel at `http://localhost:8080/admin`.

## Environment Variables

| Variable | Default | Description |
| -------- | ------- | ----------- |
| `PORT` | `8080` | HTTP listen port |
| `DB_PATH` | `farmstore.db` | SQLite database file path |
| `ADMIN_USER` | `admin` | Admin panel login username |
| `ADMIN_PASS` | `admin123` | Admin panel login password |
| `KAVENEGAR_API_KEY` | _(empty)_ | Kavenegar SMS API key (leave empty for DEV_MODE) |
| `KAVENEGAR_TEMPLATE` | `verify-otp` | Kavenegar verification template name |
| `DEV_MODE` | _(not set)_ | Set to `true` to log OTPs to stdout instead of SMS |
| `ZARINPAL_MERCHANT_ID` | _(empty)_ | Zarinpal payment gateway merchant ID |
| `ZARINPAL_SANDBOX` | `true` | `true` for sandbox, `false` for production |
| `APP_BASE_URL` | `https://toodej.shop` | Base URL used for payment callbacks |
| `LOG_LEVEL` | `info` | Log level: `debug` \| `info` \| `warn` \| `error` |
| `LOG_FORMAT` | `json` | Log format: `json` \| `text` (defaults to `text` in DEV_MODE) |

> **Note:** In production (non-DEV_MODE), default admin credentials are rejected. You must set `ADMIN_USER` and `ADMIN_PASS` (at least 8 characters).

## Testing

```bash
go test ./...
```

## Deployment

A production-ready `deploy.sh` script handles the full deployment pipeline:

```bash
./deploy.sh
```

This script:

1. Reads previous config from the existing systemd service file (if any)
2. Prompts for port, admin credentials, and Kavenegar API key
3. Builds a stripped production binary (`go build -ldflags="-s -w"`)
4. Copies the binary to `/usr/local/bin/farmstore` and templates/assets to `/var/lib/farmstore/`
5. Creates/enables a systemd service (`farmstore.service`) with `DynamicUser=yes`
6. Optionally configures a Caddy reverse proxy block

### systemd service details

- **Binary:** `/usr/local/bin/farmstore`
- **Data directory:** `/var/lib/farmstore/`
- **Database:** `/var/lib/farmstore/farmstore.db`
- **Service name:** `farmstore.service`

## Project Structure

```
.
├── cmd/server/main.go          # Entry point — router setup, graceful shutdown
├── internal/
│   ├── database/db.go          # SQLite init, migrations, seeding, all queries
│   ├── models/models.go        # Domain structs: Product, Order, User, Category
│   ├── services/sms.go         # Kavenegar OTP sending
│   ├── payment/zarinpal.go     # Zarinpal payment gateway client
│   ├── utils/date.go           # Persian (Jalali) date formatting
│   └── handlers/               # HTTP handlers, middleware, session, CSRF, rate limiting
├── templates/                  # HTML templates (layout, storefront, cart, checkout, admin)
├── assets/                     # Images and static files
├── deploy.sh                   # Production deployment script
├── run.sh                      # Development run script
├── palette.json                # Design color palette reference
├── CHECKS.md                   # Manual verification checklist
├── DESIGN.md                   # Design system documentation
└── AGENTS.md                   # AI agent directives
```

## Features

- **OTP Authentication** via Kavenegar Verify.Lookup (5-digit code, 2-minute expiry)
- **Session management** with secure HTTP-only cookies, session fixation protection
- **In-memory shopping cart** per session with stock-limited quantities
- **Zarinpal payment gateway** integration with automatic payment reconciliation
- **Jalali (Persian) calendar** for all customer-facing dates
- **Admin panel** with order management (status state machine) and product/category CRUD
- **Security:** CSRF protection, rate limiting, security headers, input validation, OTP brute-force lockout
- **Responsive** RTL layout with Persian typography

## License

See [Koboyo icon license](https://koboyo.com/icons/license) for icon assets.
