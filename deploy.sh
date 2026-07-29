#!/usr/bin/env bash
set -euo pipefail

# --------------- colors ---------------
GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; RED='\033[0;31m'; NC='\033[0m'
info()  { printf "${GREEN}==>${NC} %s\n" "$*"; }
warn()  { printf "${YELLOW}==>${NC} %s\n" "$*"; }
ok()    { printf "  ${GREEN}✓${NC} %s\n" "$*"; }
fail()  { printf "  ${RED}✗${NC} %s\n" "$*"; exit 1; }

# --------------- 1. read previous config ---------------
EXISTING_ENV_FILE="/etc/systemd/system/farmstore.service"
EXISTING_PORT=""
EXISTING_ADMIN_USER=""
EXISTING_ADMIN_PASS=""
EXISTING_KAVENEGAR_KEY=""
EXISTING_KAVENEGAR_TEMPLATE=""
if [ -f "$EXISTING_ENV_FILE" ]; then
  EXISTING_PORT=$(grep -oP 'Environment=PORT=\K.*' "$EXISTING_ENV_FILE" 2>/dev/null || echo "")
  EXISTING_ADMIN_USER=$(grep -oP 'Environment=ADMIN_USER=\K.*' "$EXISTING_ENV_FILE" 2>/dev/null || echo "")
  EXISTING_ADMIN_PASS=$(grep -oP 'Environment=ADMIN_PASS=\K.*' "$EXISTING_ENV_FILE" 2>/dev/null || echo "")
  EXISTING_KAVENEGAR_KEY=$(grep -oP 'Environment=KAVENEGAR_API_KEY=\K.*' "$EXISTING_ENV_FILE" 2>/dev/null || echo "")
  EXISTING_KAVENEGAR_TEMPLATE=$(grep -oP 'Environment=KAVENEGAR_TEMPLATE=\K.*' "$EXISTING_ENV_FILE" 2>/dev/null || echo "")
fi

if [ -n "$EXISTING_PORT" ] && [ -n "$EXISTING_ADMIN_USER" ] && [ -n "$EXISTING_ADMIN_PASS" ]; then
  APP_PORT="$EXISTING_PORT"
  ADMIN_USER="$EXISTING_ADMIN_USER"
  ADMIN_PASS="$EXISTING_ADMIN_PASS"
  info "Using previous config — port=$APP_PORT, user=$ADMIN_USER"
else
  read -rp "HTTP port for the app [${EXISTING_PORT:-8080}]: " APP_PORT
  APP_PORT="${APP_PORT:-${EXISTING_PORT:-8080}}"

  read -rp "Admin username [${EXISTING_ADMIN_USER:-admin}]: " ADMIN_USER
  ADMIN_USER="${ADMIN_USER:-${EXISTING_ADMIN_USER:-admin}}"

  read -rsp "Admin password [${EXISTING_ADMIN_PASS:-admin123}]: " ADMIN_PASS
  ADMIN_PASS="${ADMIN_PASS:-${EXISTING_ADMIN_PASS:-admin123}}"
  echo ""
fi

# --------------- Kavenegar SMS config ---------------
KAVENEGAR_API_KEY="${EXISTING_KAVENEGAR_KEY:-}"
KAVENEGAR_TEMPLATE="${EXISTING_KAVENEGAR_TEMPLATE:-verify-otp}"

if [ -z "$KAVENEGAR_API_KEY" ]; then
  read -rp "Kavenegar API key (leave blank for DEV_MODE): " KAVENEGAR_API_KEY
fi
if [ -n "$KAVENEGAR_API_KEY" ]; then
  read -rp "Kavenegar template name [${KAVENEGAR_TEMPLATE}]: " INPUT_TEMPLATE
  KAVENEGAR_TEMPLATE="${INPUT_TEMPLATE:-$KAVENEGAR_TEMPLATE}"
fi

# --------------- 2. local build ---------------
APP_DIR="$(cd "$(dirname "$0")" && pwd)"
info "Building production binary..."
cd "$APP_DIR"
go mod tidy
go build -ldflags="-s -w" -o ./bin/farmstore ./cmd/server
chmod +x ./bin/farmstore
ok "Binary built at ./bin/farmstore"

# --------------- 3. database reset prompt ---------------
DB_PATH="/var/lib/farmstore/farmstore.db"
if sudo test -f "$DB_PATH"; then
  warn "Existing database found at ${DB_PATH}"
  read -rp "Erase it and start fresh? [y/N]: " ERASE_DB
  if [[ "$ERASE_DB" =~ ^[Yy] ]]; then
    sudo rm -f "$DB_PATH"
    ok "Database erased"
  else
    info "Keeping existing database"
  fi
fi

# --------------- 4. stop running service ---------------
if systemctl is-active --quiet farmstore.service 2>/dev/null; then
  info "Stopping running service..."
  sudo systemctl stop farmstore.service
  ok "Previous service stopped"
fi

# --------------- 5. elevate and deploy ---------------
info "Elevating privileges for system deployment..."
sudo mkdir -p /var/lib/farmstore
sudo cp ./bin/farmstore /usr/local/bin/farmstore
sudo chmod +x /usr/local/bin/farmstore
sudo rm -rf /var/lib/farmstore/templates /var/lib/farmstore/assets
sudo cp -r templates /var/lib/farmstore/templates
sudo cp -r assets /var/lib/farmstore/assets
sudo chmod 755 /var/lib/farmstore
sudo chmod -R 755 /var/lib/farmstore/templates
sudo chmod -R 755 /var/lib/farmstore/assets
ok "Binary installed to /usr/local/bin/farmstore"
ok "Data directory /var/lib/farmstore ready (with templates & assets)"

info "Creating systemd service..."
sudo tee /etc/systemd/system/farmstore.service > /dev/null << UNIT
[Unit]
Description=Toodej — Farm Store E-Commerce
After=network.target

[Service]
Type=simple
DynamicUser=yes
Restart=on-failure
RestartSec=5
StateDirectory=farmstore
WorkingDirectory=/var/lib/farmstore
Environment=PORT=${APP_PORT}
Environment=ADMIN_USER=${ADMIN_USER}
Environment=ADMIN_PASS=${ADMIN_PASS}
Environment=DB_PATH=/var/lib/farmstore/farmstore.db
Environment=KAVENEGAR_API_KEY=${KAVENEGAR_API_KEY}
Environment=KAVENEGAR_TEMPLATE=${KAVENEGAR_TEMPLATE}
ExecStart=/usr/local/bin/farmstore

[Install]
WantedBy=multi-user.target
UNIT

sudo systemctl daemon-reload
sudo systemctl enable farmstore.service
sudo systemctl restart farmstore.service
ok "farmstore.service created, enabled, and started"

# --------------- 6. verify ---------------
sleep 1
if sudo systemctl is-active --quiet farmstore.service; then
  ok "Service is running"
else
  warn "Service failed to start — check: sudo journalctl -u farmstore.service --no-pager -n 30"
fi

printf "\n${GREEN}════════════════════════════════════════════${NC}\n"
printf "${GREEN}  Deployment complete!${NC}\n"
printf "${GREEN}════════════════════════════════════════════${NC}\n\n"

sudo systemctl status farmstore.service --no-pager 2>&1 | head -14

# --------------- 7. caddy prompt ---------------
printf "\n${CYAN}─── Caddy Reverse Proxy ─────────────────────${NC}\n"
read -rp "Do you want to configure a Caddy reverse proxy? [y/N]: " SETUP_CADDY
if [[ "$SETUP_CADDY" =~ ^[Yy] ]]; then
  read -rp "Enter your domain (e.g., store.example.com): " CADDY_DOMAIN
  CADDY_CONF="/etc/caddy/Caddyfile"

  printf "\n${YELLOW}Add this block to ${CADDY_CONF}${NC}:\n\n"
  printf "  ${GREEN}${CADDY_DOMAIN}${NC} {\n"
  printf "    reverse_proxy ${CYAN}127.0.0.1:${APP_PORT}${NC}\n"
  printf "  }\n\n"

  if [ -f "$CADDY_CONF" ]; then
    read -rp "Append this block to ${CADDY_CONF} now? [y/N]: " APPEND_NOW
    if [[ "$APPEND_NOW" =~ ^[Yy] ]]; then
      printf "\n  ${CADDY_DOMAIN} {\n    reverse_proxy 127.0.0.1:${APP_PORT}\n  }\n" | sudo tee -a "$CADDY_CONF" > /dev/null
      ok "Block appended to ${CADDY_CONF}"
      sudo systemctl restart caddy 2>/dev/null && ok "Caddy restarted" || warn "Run 'sudo systemctl restart caddy' manually"
    fi
  fi
fi

info "Done — Toodej is running on port ${APP_PORT}"
