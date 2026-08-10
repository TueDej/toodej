#!/usr/bin/env bash
set -euo pipefail

# --------------- helpers ---------------
GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
info()  { printf "${GREEN}==>${NC} %s\n" "$*"; }
warn()  { printf "${YELLOW}==>${NC} %s\n" "$*"; }
detail(){ printf "    ${CYAN}%s${NC}\n" "$*"; }

# --------------- defaults ---------------
# run.sh is the development entry point: default to DEV_MODE (OTP codes shown
# inline, default admin creds allowed) unless the caller sets it explicitly.
export DEV_MODE="${DEV_MODE:-true}"
export PORT="${PORT:-8080}"
export ADMIN_USER="${ADMIN_USER:-admin}"
export ADMIN_PASS="${ADMIN_PASS:-admin123}"
export DB_PATH="${DB_PATH:-farmstore.db}"
export APP_BASE_URL="${APP_BASE_URL:-http://localhost:${PORT}}"
export ZARINPAL_MERCHANT_ID="${ZARINPAL_MERCHANT_ID:-}"
export ZARINPAL_SANDBOX="${ZARINPAL_SANDBOX:-true}"

BINDIR="$(cd "$(dirname "$0")" && pwd)/bin"
BINARY="$BINDIR/server"

# --------------- steps ---------------
info "Verifying dependencies..."
go mod tidy

info "Building server binary..."
mkdir -p "$BINDIR"
go build -o "$BINARY" ./cmd/server
chmod +x "$BINARY"

printf "\n${GREEN}==>${NC} Starting Toodej server\n"
detail "Store:        http://localhost:${PORT}"
detail "Admin:        http://localhost:${PORT}/admin"
detail "Admin login:  ${ADMIN_USER} / ${ADMIN_PASS}"
detail "Database:     ${DB_PATH}"
if [ -n "$ZARINPAL_MERCHANT_ID" ]; then
  detail "Payment:      Zarinpal (${ZARINPAL_SANDBOX:-true})"
else
  warn "Payment:      ZARINPAL_MERCHANT_ID not set — payments will fail"
fi
echo ""

exec "$BINARY"
