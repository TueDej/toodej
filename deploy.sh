#!/usr/bin/env bash
set -euo pipefail

# --------------- colors ---------------
GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; RED='\033[0;31m'; NC='\033[0m'
info()  { printf "${GREEN}==>${NC} %s\n" "$*"; }
warn()  { printf "${YELLOW}==>${NC} %s\n" "$*"; }
ok()    { printf "  ${GREEN}✓${NC} %s\n" "$*"; }
fail()  { printf "  ${RED}✗${NC} %s\n" "$*"; exit 1; }

# --------------- root check ---------------
[[ $EUID -eq 0 ]] || fail "This script must be run as root (sudo)."

# --------------- 1. interactive prompts ---------------
read -rp "HTTP port for the app [8080]: " APP_PORT
APP_PORT="${APP_PORT:-8080}"

read -rp "Admin username [admin]: " ADMIN_USER
ADMIN_USER="${ADMIN_USER:-admin}"

read -rsp "Admin password [admin123]: " ADMIN_PASS
ADMIN_PASS="${ADMIN_PASS:-admin123}"
echo ""

# --------------- 2. build ---------------
APP_DIR="$(cd "$(dirname "$0")" && pwd)"
info "Building production binary..."
cd "$APP_DIR"
go mod tidy
go build -ldflags="-s -w" -o /usr/local/bin/farmstore ./cmd/server
chmod +x /usr/local/bin/farmstore
ok "Binary installed to /usr/local/bin/farmstore"

# --------------- 3. data directory ---------------
info "Setting up data directory..."
mkdir -p /var/lib/farmstore
chown -R www-data:www-data /var/lib/farmstore 2>/dev/null || chown -R nobody:nogroup /var/lib/farmstore 2>/dev/null || true
chmod 755 /var/lib/farmstore
ok "/var/lib/farmstore ready"

# --------------- 4. systemd service ---------------
info "Creating systemd service..."
cat > /etc/systemd/system/farmstore.service << UNIT
[Unit]
Description=Toodej — Farm Store E-Commerce
After=network.target

[Service]
Type=simple
User=nobody
Group=nogroup
Restart=on-failure
RestartSec=5
WorkingDirectory=/var/lib/farmstore
Environment=PORT=${APP_PORT}
Environment=ADMIN_USER=${ADMIN_USER}
Environment=ADMIN_PASS=${ADMIN_PASS}
Environment=DB_PATH=/var/lib/farmstore/farmstore.db
ExecStart=/usr/local/bin/farmstore

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable farmstore.service
systemctl restart farmstore.service
ok "farmstore.service created, enabled, and started"

# --------------- 5. status + caddy instructions ---------------
printf "\n${GREEN}════════════════════════════════════════════${NC}\n"
printf "${GREEN}  Deployment complete!${NC}\n"
printf "${GREEN}════════════════════════════════════════════${NC}\n\n"

systemctl status farmstore.service --no-pager 2>&1 | head -12

printf "\n${CYAN}─── Caddy Reverse Proxy ─────────────────────${NC}\n"
printf "Add this block to ${YELLOW}/etc/caddy/Caddyfile${NC}:\n\n"
printf "  ${GREEN}your-domain.com${NC} {\n"
printf "    reverse_proxy ${CYAN}127.0.0.1:${APP_PORT}${NC}\n"
printf "  }\n\n"
printf "Then reload Caddy:\n"
printf "  ${YELLOW}sudo caddy reload${NC}\n\n"

info "Done — Toodej is running on port ${APP_PORT}"
