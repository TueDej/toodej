#!/usr/bin/env bash
set -euo pipefail

# Toodej — development entry point. Builds and runs the server locally.

# --------------- helpers ---------------
GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
STEP_NUM=0

info()  { printf "  ${GREEN}▸${NC}  %s\n" "$*"; }
warn()  { printf "  ${YELLOW}▸${NC}  %s\n" "$*"; }
ok()    { printf "  ${GREEN}✔${NC}  %s\n" "$*"; }

banner() {
  local w=58
  local pad=""
  for _ in $(seq 1 $w); do pad="${pad}═"; done
  printf "\n${BOLD}${GREEN}╔${pad}╗${NC}\n"
  for line in "$@"; do
    local char_count inner_w pad_right
    char_count=$(printf '%s' "$line" | wc -m)
    inner_w=$((w - 2))
    if [ "$char_count" -gt "$inner_w" ]; then
      line="${line:0:$inner_w}"
      char_count=$inner_w
    fi
    pad_right=$((inner_w - char_count))
    printf "${BOLD}${GREEN}║${NC}  ${BOLD}%s${NC}" "$line"
    printf "%${pad_right}s" ""
    printf "${BOLD}${GREEN}║${NC}\n"
  done
  printf "${BOLD}${GREEN}╚${pad}╝${NC}\n"
}

step() {
  STEP_NUM=$((STEP_NUM + 1))
  printf "\n${CYAN}── Step ${STEP_NUM}: %s ──${NC}\n" "$*"
}

kv() { printf "  %-26s %s\n" "$1" "$2"; }

divider() { printf "  ${CYAN}────────────────────────────────────────────────────────${NC}\n"; }

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

banner "Toodej — development server" \
  "mode:   DEV_MODE=${DEV_MODE}" \
  "port:   ${PORT}" \
  "db:     ${DB_PATH}"

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

divider
kv "Store:"    "http://localhost:${PORT}"
kv "Admin:"    "http://localhost:${PORT}/admin"
kv "Login:"    "${ADMIN_USER} / ${ADMIN_PASS}"
kv "Database:" "${DB_PATH}"
if [ -n "$ZARINPAL_MERCHANT_ID" ]; then
  kv "Payment:" "Zarinpal (${ZARINPAL_SANDBOX:-true})"
else
  warn "Payment: ZARINPAL_MERCHANT_ID not set — payments will fail"
fi
divider

printf "\n  ${CYAN}Press Ctrl+C to stop${NC}\n\n"

exec "$BINARY"
