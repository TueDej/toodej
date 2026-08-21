#!/usr/bin/env bash
set -euo pipefail

# Toodej — development entry point. Builds and runs the server locally.

# --------------- helpers ---------------
GREEN='\033[0;32m'; CYAN='\033[0;36m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; BOLD='\033[1m'; DIM='\033[2m'; NC='\033[0m'
STEP_NUM=0

step() {
  STEP_NUM=$((STEP_NUM + 1))
  printf "\n${CYAN}${BOLD}Step ${STEP_NUM}:${NC} %s\n" "$*"
}

info()  { printf "  ${DIM}→${NC}  %s\n" "$*"; }
warn()  { printf "  ${YELLOW}!${NC}  %s\n" "$*"; }
ok()    { printf "  ${GREEN}✓${NC}  %s\n" "$*"; }
kv()    { printf "  %-22s %s\n" "$1" "$2"; }

# --------------- defaults ---------------
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

MODE_LABEL="dev"
[ "$DEV_MODE" != "true" ] && MODE_LABEL="production"

printf "\n${BOLD}${GREEN}Toodej${NC} — development server\n"
printf "  ${DIM}mode:${NC} ${MODE_LABEL}   ${DIM}port:${NC} ${PORT}   ${DIM}db:${NC} ${DB_PATH}\n"

# --------------- build ---------------
step "Build"

info "Verifying dependencies..."
go mod tidy
ok "Dependencies verified"

info "Compiling server binary..."
mkdir -p "$BINDIR"
go build -o "$BINARY" ./cmd/server
chmod +x "$BINARY"
ok "Binary built at ${BINARY}"

# --------------- server ---------------
step "Server"

kv "Store:"    "http://localhost:${PORT}"
kv "Admin:"    "http://localhost:${PORT}/admin"
kv "Login:"    "${ADMIN_USER} / ${ADMIN_PASS}"
kv "Database:" "${DB_PATH}"
if [ -n "$ZARINPAL_MERCHANT_ID" ]; then
  kv "Payment:" "Zarinpal (${ZARINPAL_SANDBOX:-true})"
else
  warn "Payment: ZARINPAL_MERCHANT_ID not set — payments will fail"
fi

printf "\n  ${DIM}Press Ctrl+C to stop${NC}\n\n"

exec "$BINARY"
