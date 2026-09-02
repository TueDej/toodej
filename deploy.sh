#!/usr/bin/env bash
set -euo pipefail

# Toodej — production deployment script.
# Runs initially without privileges; sudo is only used to write system files
# (moving the binary, writing /etc config, managing systemd service).

# --------------- config ---------------
APP_NAME="farmstore"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
UNIT_FILE="/etc/systemd/system/${APP_NAME}.service"
ENV_DIR="/etc/farmstore"
ENV_FILE="${ENV_DIR}/env"
DATA_DIR="/var/lib/farmstore"
DB_PATH="${DATA_DIR}/${APP_NAME}.db"
DEPLOYER_GROUP="$(id -gn)"
START_TIME=$SECONDS
DO_TIDY=0
ASSUME_YES=0

# Helpers: if the script is already running as root, skip sudo so it works both
# unprivileged (recommended) and via "sudo ./deploy.sh".
sudo_if_needed() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  else
    sudo "$@"
  fi
}

# --------------- colors & output ---------------
GREEN='\033[0;32m'; CYAN='\033[0;36m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; BOLD='\033[1m'; DIM='\033[2m'; NC='\033[0m'
STEP_NUM=0

# info  — normal progress message
info()  { printf "  ${DIM}→${NC}  %s\n" "$*"; }
# warn  — something non-fatal that the user should notice
warn()  { printf "  ${YELLOW}!${NC}  %s\n" "$*"; }
# ok    — task completed successfully
ok()    { printf "  ${GREEN}✓${NC}  %s\n" "$*"; }
# fail  — fatal error, prints to stderr and exits
fail()  { printf "  ${RED}✗${NC}  %s\n" "$*" >&2; exit 1; }

# step — numbered section header
step() {
  STEP_NUM=$((STEP_NUM + 1))
  printf "\n${CYAN}${BOLD}Step ${STEP_NUM}:${NC} %s\n" "$*"
}

# kv — key/value pair for summary tables (22-char label column)
kv() { printf "  %-22s %s\n" "$1" "$2"; }

usage() {
  cat <<'EOF'
Usage: ./deploy.sh [--tidy] [--yes]

Builds Toodej, installs it as a systemd service, and configures the runtime
environment. Run from the repository root.

Values already present in the env file (/etc/farmstore/env) are reused
silently — the script only asks for what is genuinely missing.

Options:
  --tidy      Run 'go mod tidy' before building (default: off).
  -y, --yes   Never prompt: take every saved value, otherwise the default.
              Note: on a fresh host the default admin password (admin123) is
              refused in production, so ADMIN_PASS must already be configured
              for --yes to succeed there.
  -h, --help  Show this help and exit.
EOF
}

for arg in "$@"; do
  case "$arg" in
    --tidy) DO_TIDY=1 ;;
    -y|--yes) ASSUME_YES=1 ;;
    -h|--help) usage; exit 0 ;;
    *) warn "Unknown argument '$arg' ignored (see --help)" ;;
  esac
done

printf "\n${BOLD}${GREEN}Toodej${NC} — production deployment\n"
printf "  ${DIM}repo:${NC} ${SCRIPT_DIR}   ${DIM}started:${NC} $(date '+%Y-%m-%d %H:%M:%S %Z')\n"

# --------------- 0. prerequisite check ---------------
missing=""
for cmd in go curl systemctl; do
  command -v "$cmd" >/dev/null 2>&1 || missing="$missing $cmd"
done
if [ "$(id -u)" -ne 0 ] && ! command -v sudo >/dev/null 2>&1; then
  missing="$missing sudo"
fi
[ -z "$missing" ] || fail "Required command(s) missing:$missing — install and re-run."

# --------------- 1. read previous config ---------------
# The current deploy keeps secrets in a mode-640 env file owned root:<deployer
# group> so re-deploys read it directly; older installs stored them in the unit
# file. Read both (env file wins) so a re-deploy never loses the existing
# configuration.
# Under 'set -euo pipefail' a failing side of a pipeline makes the whole
# pipeline fail, and a failed command substitution inside an assignment aborts
# the script. Guarding each sed/grep pipeline with '|| true' keeps a
# permission-denied (or missing-sudo) read from killing a re-deploy outright;
# the value simply comes back empty and read_existing falls through to next
# source. Env-file values may be double-quoted (systemd syntax) and '#' starts
# a comment, so values are unquoted/comment-stripped here, and %% is unescaped
# so a re-deploy never double-escapes '%'.
read_env_value() {
  [ -f "$ENV_FILE" ] || { echo ""; return; }
  local v=""
  # Try reading directly first (works when running as root or if the file's
  # group is readable). Fall back to sudo when permission is denied.
  if [ -r "$ENV_FILE" ]; then
    v=$(sed -n "s/^[[:space:]]*$1=[[:space:]]*//p" "$ENV_FILE" 2>/dev/null | tail -n1 || true)
  else
    v=$(sudo sed -n "s/^[[:space:]]*$1=[[:space:]]*//p" "$ENV_FILE" 2>/dev/null | tail -n1 || true)
  fi
  if [ "${#v}" -ge 2 ] && [ "${v:0:1}" = '"' ] && [ "${v: -1}" = '"' ]; then
    v="${v#\"}"; v="${v%\"}"
    v="${v//\\\"/\"}"
    v="${v//\\\\/\\}"
  else
    v="${v%%#*}"
    v="$(printf '%s' "$v" | sed -e 's/[[:space:]]*$//')"
  fi
  v="${v//%%/%}"
  printf '%s' "$v"
}
read_unit_value() {
  [ -f "$UNIT_FILE" ] || { echo ""; return; }
  # Matches both Environment=KEY=value and Environment="KEY=value" (older
  # installs stored secrets directly in the unit file). \K resets the match
  # start so only the value is printed.
  local v=""
  v=$(grep -oP "Environment=(?:\"?)$1=(?:\"?)\K[^\"]+" "$UNIT_FILE" 2>/dev/null | tail -n1 || true)
  v="${v//\\\\/\\}"
  v="${v//%%/%}"
  printf '%s' "$v"
}
read_existing() {
  local v
  v=$(read_env_value "$1")
  if [ -n "$v" ]; then
    echo "$v"
  else
    echo "$(read_unit_value "$1")"
  fi
}

# clean_input strips control characters (incl. newlines) so values can never
# break the environment file or the systemd unit. UTF-8 is preserved.
clean_input() {
  printf '%s' "$1" | tr -d '\000-\037\177'
}

valid_port() {
  [[ "$1" =~ ^[0-9]{1,5}$ ]] && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]
}

# The app (cmd/server/main.go) refuses to start without explicit, non-default,
# >=8-char admin credentials unless DEV_MODE=true. Enforce the same rules here
# so a misconfiguration fails fast at deploy time instead of the unit crashing
# on boot. Applies to freshly-entered AND previously-saved credentials.
validate_admin_creds() {
  local u="$1" p="$2"
  [ -n "$u" ] || fail "Admin username cannot be empty."
  [ -n "$p" ] || fail "Admin password cannot be empty."
  if [ "$u" = "admin" ] && [ "$p" = "admin123" ]; then
    fail "Production refuses the default admin/admin123 credentials (cmd/server/main.go). Choose an explicit, strong password."
  fi
  [ "${#p}" -ge 8 ] || fail "Admin password must be at least 8 characters (cmd/server/main.go refuses shorter ones in production)."
}

EXISTING_PORT=$(read_existing "PORT")
EXISTING_ADMIN_USER=$(read_existing "ADMIN_USER")
EXISTING_ADMIN_PASS=$(read_existing "ADMIN_PASS")
EXISTING_KAVENEGAR_KEY=$(read_existing "KAVENEGAR_API_KEY")
EXISTING_KAVENEGAR_TEMPLATE=$(read_existing "KAVENEGAR_TEMPLATE")
EXISTING_ADMIN_PHONE=$(read_existing "ADMIN_NOTIFY_PHONE")
EXISTING_KAVENEGAR_TEMPLATE_ADMIN_ORDER=$(read_existing "KAVENEGAR_TEMPLATE_ADMIN_ORDER")
EXISTING_ZARINPAL_MERCHANT_ID=$(read_existing "ZARINPAL_MERCHANT_ID")
EXISTING_ZARINPAL_SANDBOX=$(read_existing "ZARINPAL_SANDBOX")
EXISTING_APP_BASE_URL=$(read_existing "APP_BASE_URL")
EXISTING_DB_PATH=$(read_existing "DB_PATH")
[ -n "$EXISTING_DB_PATH" ] && DB_PATH="$EXISTING_DB_PATH"

# Each value already stored in the env file is reused without prompting; only
# genuinely missing values ask the user — and under --yes even those silently
# fall back to their defaults.
if [ -n "$EXISTING_PORT" ]; then
  APP_PORT="$EXISTING_PORT"
  info "Using saved port ${APP_PORT}"
elif [ "$ASSUME_YES" -eq 1 ]; then
  APP_PORT="8080"
else
  APP_PORT="8080"
  read -rp "HTTP port for the app [${APP_PORT}]: " APP_PORT
  APP_PORT="$(clean_input "${APP_PORT:-8080}")"
fi
while ! valid_port "$APP_PORT"; do
  [ "$ASSUME_YES" -eq 1 ] && fail "Invalid port '${APP_PORT}' (must be 1-65535)."
  warn "Invalid port '${APP_PORT}' — must be 1-65535."
  read -rp "HTTP port for the app [8080]: " APP_PORT
  APP_PORT="$(clean_input "${APP_PORT:-8080}")"
done

if [ -n "$EXISTING_ADMIN_USER" ]; then
  ADMIN_USER="$EXISTING_ADMIN_USER"
elif [ "$ASSUME_YES" -eq 1 ]; then
  ADMIN_USER="admin"
else
  read -rp "Admin username [${EXISTING_ADMIN_USER:-admin}]: " ADMIN_USER
  ADMIN_USER="$(clean_input "${ADMIN_USER:-${EXISTING_ADMIN_USER:-admin}}")"
fi
[ -n "$ADMIN_USER" ] || fail "Admin username cannot be empty."

# Never echo the saved password back into the terminal; -s only hides typed
# input. A saved password is reused silently, so it is never asked again.
if [ -n "$EXISTING_ADMIN_PASS" ]; then
  ADMIN_PASS="$EXISTING_ADMIN_PASS"
  info "Using saved admin credentials — user=${ADMIN_USER} (password from ${ENV_FILE})"
elif [ "$ASSUME_YES" -eq 1 ]; then
  ADMIN_PASS="admin123"
else
  read -rsp "Admin password [admin123 default]: " ADMIN_PASS
  echo ""
  ADMIN_PASS="$(clean_input "${ADMIN_PASS:-admin123}")"
fi

validate_admin_creds "$ADMIN_USER" "$ADMIN_PASS"

step "SMS, payment & URL configuration"

KAVENEGAR_API_KEY="$(clean_input "${EXISTING_KAVENEGAR_KEY:-}")"
KAVENEGAR_TEMPLATE="$(clean_input "${EXISTING_KAVENEGAR_TEMPLATE:-verify-otp}")"

if [ -z "$KAVENEGAR_API_KEY" ] && [ "$ASSUME_YES" -eq 0 ]; then
  read -rp "Kavenegar API key (leave blank to log OTPs instead of SMS): " KAVENEGAR_API_KEY
  KAVENEGAR_API_KEY="$(clean_input "$KAVENEGAR_API_KEY")"
fi
if [ -n "$KAVENEGAR_API_KEY" ]; then
  # Only ask for the template when it is not already configured.
  if [ -z "$EXISTING_KAVENEGAR_TEMPLATE" ] && [ "$ASSUME_YES" -eq 0 ]; then
    read -rp "Kavenegar template name [${KAVENEGAR_TEMPLATE}]: " INPUT_TEMPLATE
    KAVENEGAR_TEMPLATE="$(clean_input "${INPUT_TEMPLATE:-$KAVENEGAR_TEMPLATE}")"
  fi
else
  warn "No Kavenegar key — OTP codes will be printed to the service logs only."
fi

# Admin order-submission notification: SMS sent to ADMIN_NOTIFY_PHONE using the
# KAVENEGAR_TEMPLATE_ADMIN_ORDER Verify.Lookup template. Both are optional —
# leaving either blank disables the notification.
ADMIN_NOTIFY_PHONE="$(clean_input "${EXISTING_ADMIN_PHONE:-}")"
KAVENEGAR_TEMPLATE_ADMIN_ORDER="$(clean_input "${EXISTING_KAVENEGAR_TEMPLATE_ADMIN_ORDER:-}")"
if [ -n "$KAVENEGAR_API_KEY" ] && [ "$ASSUME_YES" -eq 0 ]; then
  if [ -z "$EXISTING_ADMIN_PHONE" ] || [ -z "$EXISTING_KAVENEGAR_TEMPLATE_ADMIN_ORDER" ]; then
    read -rp "Admin phone for order-submission SMS (blank to disable) [${ADMIN_NOTIFY_PHONE}]: " INPUT_ADMIN_PHONE
    ADMIN_NOTIFY_PHONE="$(clean_input "${INPUT_ADMIN_PHONE:-$ADMIN_NOTIFY_PHONE}")"
    read -rp "Kavenegar admin order template name (blank to disable) [${KAVENEGAR_TEMPLATE_ADMIN_ORDER}]: " INPUT_ADMIN_ORDER_TEMPLATE
    KAVENEGAR_TEMPLATE_ADMIN_ORDER="$(clean_input "${INPUT_ADMIN_ORDER_TEMPLATE:-$KAVENEGAR_TEMPLATE_ADMIN_ORDER}")"
  fi
  if [ -n "$ADMIN_NOTIFY_PHONE" ] && [ -z "$KAVENEGAR_TEMPLATE_ADMIN_ORDER" ]; then
    warn "Admin phone set but no KAVENEGAR_TEMPLATE_ADMIN_ORDER — order-submission SMS disabled."
  fi
fi

ZARINPAL_MERCHANT_ID="$(clean_input "${EXISTING_ZARINPAL_MERCHANT_ID:-}")"
ZARINPAL_SANDBOX="${EXISTING_ZARINPAL_SANDBOX:-false}"
APP_BASE_URL="$(clean_input "${EXISTING_APP_BASE_URL:-https://toodej.shop}")"

if [ -z "$ZARINPAL_MERCHANT_ID" ] && [ "$ASSUME_YES" -eq 0 ]; then
  read -rp "Zarinpal Merchant ID: " ZARINPAL_MERCHANT_ID
  ZARINPAL_MERCHANT_ID="$(clean_input "$ZARINPAL_MERCHANT_ID")"
fi
if [ -z "$ZARINPAL_MERCHANT_ID" ]; then
  warn "No Merchant ID — payments will not work"
fi

if [ -z "$EXISTING_ZARINPAL_SANDBOX" ] && [ "$ASSUME_YES" -eq 0 ]; then
  read -rp "Use Zarinpal sandbox? [y/N]: " USE_SANDBOX
  if [[ "$USE_SANDBOX" =~ ^[Yy] ]]; then
    ZARINPAL_SANDBOX="true"
  else
    ZARINPAL_SANDBOX="false"
  fi
fi

if [ -z "$EXISTING_APP_BASE_URL" ] && [ "$ASSUME_YES" -eq 0 ]; then
  read -rp "App base URL [${APP_BASE_URL}]: " INPUT_BASE_URL
  APP_BASE_URL="$(clean_input "${INPUT_BASE_URL:-$APP_BASE_URL}")"
fi

# Auto-prefix https:// if missing.
if [[ ! "$APP_BASE_URL" =~ ^https?:// ]]; then
  warn "URL missing protocol — prepending https://"
  APP_BASE_URL="https://${APP_BASE_URL}"
fi

# --------------- 2. local build ---------------
step "Building production binary"
cd "$SCRIPT_DIR"

if [ "$DO_TIDY" -eq 1 ]; then
  info "Running 'go mod tidy' (--tidy requested)..."
  GOTOOLCHAIN=local go mod tidy
  ok "go.mod/go.sum tidied"
else
  info "Skipping 'go mod tidy' (pass --tidy to run it)"
fi

info "Compiling all packages as a pre-flight check..."
# -trimpath must match the release build below: Go's build-cache keys include
# the flag set, so a plain 'go build ./...' here would leave the release build
# with a cold cache and it would recompile the whole tree (including
# modernc.org/sqlite) from scratch — minutes of silence on a small VPS.
GOTOOLCHAIN=local go build -trimpath ./...
ok "All packages compile"

info "Building release binary (first run can take a few minutes)..."
BUILD_START=$SECONDS
GOTOOLCHAIN=local go build -trimpath -ldflags="-s -w" -o "./bin/${APP_NAME}" ./cmd/server &
BUILD_PID=$!
SPIN='-\|/'
SPIN_I=0
while kill -0 "$BUILD_PID" 2>/dev/null; do
  SPIN_CHAR="${SPIN:SPIN_I%4:1}"
  SPIN_I=$((SPIN_I + 1))
  printf "\r  ${DIM}→${NC}  Building release binary... %c  %3ds" "$SPIN_CHAR" "$((SECONDS - BUILD_START))"
  sleep 1
done
if wait "$BUILD_PID"; then
  printf "\r  ${GREEN}✓${NC}  Release binary built in %ds                                \n" "$((SECONDS - BUILD_START))"
else
  printf "\n"
  fail "Release build failed"
fi
chmod +x "./bin/${APP_NAME}"
ok "Binary built at ./bin/${APP_NAME}"

# --------------- 3. database reset prompt ---------------
step "Database"
if sudo_if_needed test -f "$DB_PATH"; then
  warn "Existing database found at ${DB_PATH}"
  ERASE_DB="n"
  if [ "$ASSUME_YES" -eq 0 ]; then
    read -rp "Erase it and start fresh? [y/N]: " ERASE_DB
  else
    info "--yes: keeping existing database"
  fi
  if [[ "$ERASE_DB" =~ ^[Yy] ]]; then
    sudo_if_needed rm -f "$DB_PATH"
    ok "Database erased"
  else
    info "Keeping existing database"
  fi
fi

# --------------- 4. stop running service ---------------
step "Stopping running service"
if systemctl is-active --quiet "${APP_NAME}.service" 2>/dev/null; then
  info "Stopping running service..."
  sudo_if_needed systemctl stop "${APP_NAME}.service"
  ok "Previous service stopped"
else
  info "No running ${APP_NAME} service to stop"
fi

# --------------- 5. deploy files ---------------
step "Deploying to system"
info "Installing binary, templates, and assets (requires sudo)..."
sudo_if_needed mkdir -p "$DATA_DIR"
sudo_if_needed cp "./bin/${APP_NAME}" "/usr/local/bin/${APP_NAME}"
sudo_if_needed chmod +x "/usr/local/bin/${APP_NAME}"
sudo_if_needed rm -rf "${DATA_DIR}/templates" "${DATA_DIR}/assets"
sudo_if_needed cp -r templates "${DATA_DIR}/templates"
sudo_if_needed cp -r assets "${DATA_DIR}/assets"
sudo_if_needed chmod 755 "$DATA_DIR"
sudo_if_needed chmod -R 755 "${DATA_DIR}/templates"
sudo_if_needed chmod -R 755 "${DATA_DIR}/assets"
ok "Binary installed to /usr/local/bin/${APP_NAME}"
ok "Data directory ${DATA_DIR} ready (with templates & assets)"

# --------------- 6. write protected environment file ---------------
# Secrets (admin password, SMS key, gateway id) are kept in a root-owned,
# mode-640 env file (group = the deploying user's primary group) so the app's
# EnvironmentFile stays root-readable via systemd while the deployer can still
# read it back without a sudo prompt. It is never stored in the unit file.
# '%' must be escaped to '%%' because systemd expands specifiers.
sudo_if_needed mkdir -p "$ENV_DIR"
sudo_if_needed chown root:root "$ENV_DIR"
sudo_if_needed chmod 755 "$ENV_DIR"

# '%%' escapes '%' (systemd expands specifiers), and values are double-quoted
# so spaces and '#' survive the round-trip through EnvironmentFile.
env_line() {
  local val
  val="$(printf '%s' "$2" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/%/%%/g')"
  printf '%s="%s"\n' "$1" "$val"
}

{
  env_line "PORT" "$APP_PORT"
  env_line "ADMIN_USER" "$ADMIN_USER"
  env_line "ADMIN_PASS" "$ADMIN_PASS"
  env_line "DB_PATH" "$DB_PATH"
  env_line "KAVENEGAR_API_KEY" "$KAVENEGAR_API_KEY"
  env_line "KAVENEGAR_TEMPLATE" "$KAVENEGAR_TEMPLATE"
  env_line "ADMIN_NOTIFY_PHONE" "$ADMIN_NOTIFY_PHONE"
  env_line "KAVENEGAR_TEMPLATE_ADMIN_ORDER" "$KAVENEGAR_TEMPLATE_ADMIN_ORDER"
  env_line "ZARINPAL_MERCHANT_ID" "$ZARINPAL_MERCHANT_ID"
  env_line "ZARINPAL_SANDBOX" "$ZARINPAL_SANDBOX"
  env_line "APP_BASE_URL" "$APP_BASE_URL"
  env_line "DEV_MODE" "false"
} | sudo_if_needed tee "$ENV_FILE" > /dev/null
sudo_if_needed chown root:"${DEPLOYER_GROUP}" "$ENV_FILE"
sudo_if_needed chmod 640 "$ENV_FILE"
ok "Environment written to ${ENV_FILE} with 640 permissions (root:${DEPLOYER_GROUP})"

# --------------- 7. create systemd service ---------------
info "Creating systemd service..."
sudo_if_needed tee "$UNIT_FILE" > /dev/null << UNIT
[Unit]
Description=Toodej — Farm Store E-Commerce
After=network.target

[Service]
Type=simple
DynamicUser=yes
Restart=on-failure
RestartSec=5
StateDirectory=farmstore
WorkingDirectory=${DATA_DIR}
EnvironmentFile=${ENV_FILE}
ExecStart=/usr/local/bin/${APP_NAME}

[Install]
WantedBy=multi-user.target
UNIT

sudo_if_needed systemctl daemon-reload
sudo_if_needed systemctl enable "${APP_NAME}.service"
sudo_if_needed systemctl restart "${APP_NAME}.service"
ok "${APP_NAME}.service created, enabled, and started"

# --------------- 8. verify ---------------
step "Verifying deployment"
info "Waiting for HTTP health check on port ${APP_PORT}..."
HEALTHY=0
for _ in {1..30}; do
  if curl -sf --max-time 2 "http://127.0.0.1:${APP_PORT}/health" >/dev/null 2>&1; then
    HEALTHY=1
    break
  fi
  sleep 1
done

if [ "$HEALTHY" -eq 1 ]; then
  ok "Service is healthy"
  kv "health:" "http://127.0.0.1:${APP_PORT}/health → $(curl -s --max-time 2 "http://127.0.0.1:${APP_PORT}/health")"
else
  warn "No health response on :${APP_PORT} after 30s — the app may still be starting or failed."
fi

if systemctl is-active --quiet "${APP_NAME}.service" 2>/dev/null; then
  ok "systemd unit ${APP_NAME}.service is active"
else
  warn "Unit ${APP_NAME}.service is NOT active — check: sudo journalctl -u ${APP_NAME}.service --no-pager -n 30"
fi

ELAPSED=$((SECONDS - START_TIME))
printf "\n${BOLD}${GREEN}Deployment complete${NC}\n"
printf "  ${DIM}elapsed:${NC} $((ELAPSED / 3600))h $(((ELAPSED % 3600) / 60))m $((ELAPSED % 60))s\n"
printf "  ${DIM}status:${NC}  $([ "$HEALTHY" -eq 1 ] && echo "service is healthy" || echo "service may still be starting")\n"

step "Deployment summary"
kv "Service unit:" "${APP_NAME}.service"
kv "Port:" "$APP_PORT"
kv "Admin user:" "$ADMIN_USER"
kv "Admin password:" "[configured, masked]"
kv "Database:" "$DB_PATH"
kv "App base URL:" "$APP_BASE_URL"
kv "Zarinpal sandbox:" "${ZARINPAL_SANDBOX:-false}"
kv "Kavenegar configured:" "$([ -n "$KAVENEGAR_API_KEY" ] && echo "yes" || echo "no")"
kv "Admin order SMS:" "$([ -n "$ADMIN_NOTIFY_PHONE" ] && [ -n "$KAVENEGAR_TEMPLATE_ADMIN_ORDER" ] && echo "yes → ${ADMIN_NOTIFY_PHONE}" || echo "disabled")"
kv "Env file:" "${ENV_FILE} (640, root:${DEPLOYER_GROUP})"
printf "\n  ${DIM}Logs:${NC}     sudo journalctl -u %s -f\n" "${APP_NAME}.service"

if [ "$HEALTHY" -eq 1 ]; then
  printf "  ${DIM}Local:${NC}    http://127.0.0.1:%s\n\n" "$APP_PORT"
fi

# --------------- 9. caddy prompt ---------------
step "Caddy reverse proxy (optional)"
SETUP_CADDY="n"
if [ "$ASSUME_YES" -eq 0 ]; then
  read -rp "Do you want to configure a Caddy reverse proxy? [y/N]: " SETUP_CADDY
else
  info "--yes: skipping Caddy configuration"
fi
if [[ "$SETUP_CADDY" =~ ^[Yy] ]]; then
  read -rp "Enter your domain (e.g., store.example.com): " CADDY_DOMAIN
  CADDY_DOMAIN="$(clean_input "$CADDY_DOMAIN")"
  [ -n "$CADDY_DOMAIN" ] || fail "Domain cannot be empty."
  CADDY_CONF="/etc/caddy/Caddyfile"

  warn "Add this block to ${CADDY_CONF}:"
  printf "\n  ${GREEN}${CADDY_DOMAIN}${NC} {\n"
  printf "    reverse_proxy ${CYAN}127.0.0.1:${APP_PORT}${NC}\n"
  printf "  }\n"

  if [ -f "$CADDY_CONF" ]; then
    read -rp "Append this block to ${CADDY_CONF} now? [y/N]: " APPEND_NOW
    if [[ "$APPEND_NOW" =~ ^[Yy] ]]; then
      printf "\n  ${CADDY_DOMAIN} {\n    reverse_proxy 127.0.0.1:${APP_PORT}\n  }\n" \
        | sudo_if_needed tee -a "$CADDY_CONF" > /dev/null
      ok "Block appended to ${CADDY_CONF}"
      sudo_if_needed systemctl restart caddy 2>/dev/null && ok "Caddy restarted" \
        || warn "Run 'sudo systemctl restart caddy' manually"
    fi
  fi
fi

info "Done — Toodej is running on port ${APP_PORT}"
