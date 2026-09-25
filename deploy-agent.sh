#!/usr/bin/env bash
set -euo pipefail

# =============================================================================
# Mango IoT Gateway Agent — One-Command Deployment for Raspberry Pi 3/4
# Optional 4G module support (Telit LE910C4, Quectel EC25, SIM7600, etc.)
# Run on the Pi: curl -fsSL <url> | sudo bash -s -- --token <TOKEN> --url <PLATFORM_URL>
# =============================================================================

# ─────────────────────────────────────────────────────────────────────────────
# Config (override via flags/env)
# ─────────────────────────────────────────────────────────────────────────────
PLATFORM_URL="${PLATFORM_URL:-}"
PROVISION_TOKEN="${PROVISION_TOKEN:-}"
DEVICE_ID="${DEVICE_ID:-}"                    # Optional: set fixed device ID
SERIAL_NUMBER="${SERIAL_NUMBER:-}"            # Optional: override serial
INSTALL_DIR="${INSTALL_DIR:-/opt/gateway}"
CONFIG_DIR="${CONFIG_DIR:-/etc/gateway}"
DATA_DIR="${DATA_DIR:-/var/lib/gateway}"
LOG_DIR="${LOG_DIR:-/var/log/gateway}"
SERVICE_NAME="${SERVICE_NAME:-gateway-agent}"
ENABLE_4G="${ENABLE_4G:-auto}"                # auto | yes | no
APN="${APN:-}"                                # 4G APN (auto-detected if empty)
SKIP_DEPS="${SKIP_DEPS:-false}"               # Skip dependency install (for testing)
SKIP_BUILD="${SKIP_BUILD:-false}"             # Skip Go build (use prebuilt ./gateway-agent binary)
SKIP_4G_SETUP="${SKIP_4G_SETUP:-false}"       # Skip 4G setup entirely
FORCE_REINSTALL="${FORCE_REINSTALL:-false}"   # Force reinstall even if exists
BRANCH="${BRANCH:-main}"                      # Git branch to deploy
REPO_URL="${REPO_URL:-https://github.com/prashantsingh95/mango-iot-gateway-client}"

# Colors
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
log()   { echo -e "${GREEN}[INFO]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }
step()  { echo -e "${BLUE}[STEP]${NC} $*"; }

# ─────────────────────────────────────────────────────────────────────────────
# Usage
# ─────────────────────────────────────────────────────────────────────────────
usage() {
  cat <<EOF
Usage: $0 [OPTIONS]

One-command deployment for Mango IoT Gateway Agent on Raspberry Pi 3/4.

Required:
  --token TOKEN       Provisioning token from platform
  --url URL           Platform URL (e.g., https://gateway.example.com)

Optional:
  --device-id ID      Fixed device ID (default: auto-generated from CPU serial)
  --serial SERIAL     Override serial number
  --install-dir DIR   Installation directory (default: /opt/gateway)
  --config-dir DIR    Config directory (default: /etc/gateway)
  --data-dir DIR      Data directory (default: /var/lib/gateway)
  --service-name NAME Systemd service name (default: gateway-agent)
  --4g MODE           4G mode: auto|yes|no (default: auto)
  --apn APN           4G APN (default: auto-detect airtel/jio/vi/bsnl; aliases: airtel→airtelgprs.com, jio→jionet, vi→www, bsnl→bsnlnet; or raw APN)
  --branch BRANCH     Git branch (default: main)
  --repo-url URL      Git repo URL (default: GitHub)
  --skip-deps         Skip dependency installation
  --skip-build        Skip Go build (use prebuilt ./gateway-agent binary)
  --skip-4g           Skip 4G setup entirely
  --force             Force reinstall
  -h, --help          Show this help

Environment variables (alternative to flags):
  PLATFORM_URL, PROVISION_TOKEN, DEVICE_ID, SERIAL_NUMBER, INSTALL_DIR,
  CONFIG_DIR, DATA_DIR, LOG_DIR, SERVICE_NAME, ENABLE_4G, APN, SKIP_DEPS,
  SKIP_4G_SETUP, FORCE_REINSTALL, BRANCH, REPO_URL

Examples:
  # Interactive (prompts for token/url)
  curl -fsSL https://raw.githubusercontent.com/.../deploy-agent.sh | sudo bash

  # Fully automated
  curl -fsSL https://.../deploy-agent.sh | sudo bash -s -- \\
    --token abc123 --url https://gateway.example.com --4g yes --apn internet

  # With custom branch
  curl -fsSL https://.../deploy-agent.sh | sudo bash -s -- \\
    --token abc123 --url https://gateway.example.com --branch develop
EOF
  exit 1
}

# ─────────────────────────────────────────────────────────────────────────────
# Parse arguments
# ─────────────────────────────────────────────────────────────────────────────
parse_args() {
  while [[ $# -gt 0 ]]; do
    case $1 in
      --token) PROVISION_TOKEN="$2"; shift 2 ;;
      --url) PLATFORM_URL="$2"; shift 2 ;;
      --device-id) DEVICE_ID="$2"; shift 2 ;;
      --serial) SERIAL_NUMBER="$2"; shift 2 ;;
      --install-dir) INSTALL_DIR="$2"; shift 2 ;;
      --config-dir) CONFIG_DIR="$2"; shift 2 ;;
      --data-dir) DATA_DIR="$2"; shift 2 ;;
      --log-dir) LOG_DIR="$2"; shift 2 ;;
      --service-name) SERVICE_NAME="$2"; shift 2 ;;
      --4g) ENABLE_4G="$2"; shift 2 ;;
      --apn) APN="$2"; shift 2 ;;
      --branch) BRANCH="$2"; shift 2 ;;
      --repo-url) REPO_URL="$2"; shift 2 ;;
      --skip-deps) SKIP_DEPS="true"; shift ;;
      --skip-build) SKIP_BUILD="true"; shift ;;
      --skip-4g) SKIP_4G_SETUP="true"; shift ;;
      --force) FORCE_REINSTALL="true"; shift ;;
      -h|--help) usage ;;
      *) error "Unknown option: $1" ;;
    esac
  done
  
  # Prompt for missing required values
  if [[ -z "$PLATFORM_URL" ]]; then
    read -rp "Platform URL (e.g., https://gateway.example.com): " PLATFORM_URL
    [[ -z "$PLATFORM_URL" ]] && error "Platform URL is required"
  fi
  
  if [[ -z "$PROVISION_TOKEN" ]]; then
    read -rsp "Provisioning token: " PROVISION_TOKEN
    echo
    [[ -z "$PROVISION_TOKEN" ]] && error "Provisioning token is required"
  fi
  
  # Normalize URL
  PLATFORM_URL="${PLATFORM_URL%/}"
}

# ─────────────────────────────────────────────────────────────────────────────
# Hardware detection
# ─────────────────────────────────────────────────────────────────────────────
detect_hardware() {
  step "Detecting hardware..."
  
  # Pi model
  if [[ -f /proc/device-tree/model ]]; then
    PI_MODEL=$(tr -d '\0' < /proc/device-tree/model)
    log "Model: $PI_MODEL"
  else
    warn "Cannot detect Pi model (/proc/device-tree/model missing)"
    PI_MODEL="Unknown"
  fi
  
  # Architecture
  ARCH=$(uname -m)
  case "$ARCH" in
    aarch64|arm64) GOARCH="arm64" ;;
    armv7l|armhf) GOARCH="armv7" ;;
    *) error "Unsupported architecture: $ARCH (need arm64 or armv7)" ;;
  esac
  log "Architecture: $ARCH (Go: $GOARCH)"
  
  # CPU serial for device ID
  if [[ -z "$DEVICE_ID" ]]; then
    if [[ -f /proc/cpuinfo ]]; then
      DEVICE_ID=$(grep -m1 '^Serial' /proc/cpuinfo | awk '{print $3}' | tr -d '\0')
      [[ -z "$DEVICE_ID" ]] && DEVICE_ID=$(cat /proc/sys/kernel/random/uuid | tr -d '-')
    else
      DEVICE_ID=$(cat /proc/sys/kernel/random/uuid | tr -d '-')
    fi
    log "Device ID: $DEVICE_ID"
  fi
  
  # Serial number
  if [[ -z "$SERIAL_NUMBER" ]]; then
    SERIAL_NUMBER=$(grep -m1 '^Serial' /proc/cpuinfo 2>/dev/null | awk '{print $3}' || echo "PI-${DEVICE_ID:0:8}")
  fi
  log "Serial: $SERIAL_NUMBER"
  
  # 4G modem detection
  FOUR_G_DETECTED="false"
  FOUR_G_DEVICE=""
  if [[ "$ENABLE_4G" != "no" && "$SKIP_4G_SETUP" != "true" ]]; then
    detect_4g_modem
  fi
}

detect_4g_modem() {
  log "Scanning for 4G modems..."
  
  # Check USB devices
  if command -v lsusb &>/dev/null; then
    while IFS= read -r line; do
      if echo "$line" | grep -qiE "telit|quectel|simcom|sierra|huawei|fibocom|meig|longcheer|mobile.*broadband|wwan"; then
        FOUR_G_DETECTED="true"
        FOUR_G_DEVICE="$line"
        log "Found 4G modem: $line"
        break
      fi
    done < <(lsusb)
  fi
  
  # Check serial devices
  for dev in /dev/ttyUSB* /dev/ttyACM* /dev/ttyS*; do
    [[ -e "$dev" ]] || continue
    if [[ "$FOUR_G_DETECTED" == "false" ]]; then
      FOUR_G_DETECTED="true"
      FOUR_G_DEVICE="$dev"
      log "Found serial device: $dev"
    fi
  done
  
  # Check for ModemManager
  if systemctl is-active --quiet ModemManager 2>/dev/null; then
    log "ModemManager is running"
    if mmcli -L 2>/dev/null | grep -q "modem"; then
      FOUR_G_DETECTED="true"
      log "ModemManager reports modem present"
    fi
  fi
  
  if [[ "$FOUR_G_DETECTED" == "false" ]]; then
    if [[ "$ENABLE_4G" == "yes" ]]; then
      warn "4G requested but no modem detected — continuing anyway"
    else
      log "No 4G modem detected (auto mode: disabled)"
    fi
  fi
}

# ─────────────────────────────────────────────────────────────────────────────
# Install dependencies
# ─────────────────────────────────────────────────────────────────────────────
install_deps() {
  [[ "$SKIP_DEPS" == "true" ]] && { log "Skipping dependency install"; return; }
  
  step "Installing system dependencies..."
  
  apt-get update -qq
  apt-get install -y -qq \
    git curl wget ca-certificates gnupg lsb-release \
    build-essential pkg-config \
    mosquitto mosquitto-clients \
    sqlite3 \
    iw wireless-tools \
    network-manager \
    modemmanager \
    usb-modeswitch \
    jq \
    htop \
    || error "Failed to install system packages"
  
  # Go
  if ! command -v go &>/dev/null || [[ "$(go version | awk '{print $3}')" != "go1.22"* ]]; then
    log "Installing Go 1.22..."
    GO_TAR="go1.22.5.linux-${GOARCH}.tar.gz"
    wget -q "https://go.dev/dl/${GO_TAR}" -O "/tmp/${GO_TAR}"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "/tmp/${GO_TAR}"
    ln -sf /usr/local/go/bin/go /usr/local/bin/go
    ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
  fi
  export PATH=$PATH:/usr/local/go/bin
  log "Go: $(go version)"
  
  # Node.js (for any local tooling)
  if ! command -v node &>/dev/null; then
    log "Installing Node.js 20..."
    curl -fsSL https://deb.nodesource.com/setup_20.x | bash -
    apt-get install -y -qq nodejs
  fi
  
  # Docker (optional, for local containers)
  if ! command -v docker &>/dev/null; then
    log "Installing Docker..."
    curl -fsSL https://get.docker.com | sh
    usermod -aG docker "$SUDO_USER" 2>/dev/null || true
  fi
  
  log "Dependencies installed"
}

# ─────────────────────────────────────────────────────────────────────────────
# Clone & build agent
# ─────────────────────────────────────────────────────────────────────────────
build_agent() {
  if [[ "$SKIP_BUILD" == "true" ]]; then
    if [[ -x "$INSTALL_DIR/gateway-agent" ]]; then
      log "Skipping Go build (--skip-build), using prebuilt binary:"
      "$INSTALL_DIR/gateway-agent" --version 2>&1 | head -1 || true
      return
    fi
    error "SKIP_BUILD set but no executable $INSTALL_DIR/gateway-agent found — place prebuilt binary there first"
  fi
  step "Building gateway agent..."
  
  if [[ -d "$INSTALL_DIR/.git" && "$FORCE_REINSTALL" != "true" ]]; then
    log "Updating existing repository..."
    cd "$INSTALL_DIR"
    git fetch origin
    git checkout "$BRANCH"
    git pull origin "$BRANCH"
  else
    log "Cloning repository..."
    rm -rf "$INSTALL_DIR"
    git clone --branch "$BRANCH" --depth 1 "$REPO_URL" "$INSTALL_DIR"
    cd "$INSTALL_DIR"
  fi
  
  # Build
  log "Building Go binary..."
  CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -ldflags="-s -w -X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo 'dev')" -o gateway-agent .
  
  # Verify
  ./gateway-agent --version
  log "Build complete"
}

# ─────────────────────────────────────────────────────────────────────────────
# Generate config.yml
# ─────────────────────────────────────────────────────────────────────────────
generate_config() {
  step "Generating configuration..."
  
  mkdir -p "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR"
  
  # Derive MQTT credentials from platform
  MQTT_HOST=$(echo "$PLATFORM_URL" | sed 's|https\?://||' | sed 's|/.*||')
  MQTT_PORT="1883"
  MQTT_TLS_PORT="8883"
  
  cat > "$CONFIG_DIR/config.yml" <<EOF
# Mango IoT Gateway Agent Configuration
# Generated: $(date -u +"%Y-%m-%d %H:%M:%S UTC")
# Device: $DEVICE_ID ($PI_MODEL)

gateway:
  device_id: "$DEVICE_ID"
  serial_number: "$SERIAL_NUMBER"
  platform_url: "$PLATFORM_URL"
  provision_token: "$PROVISION_TOKEN"
  offline_path: "/data/offline"

mqtt:
  broker_url: "mqtt://$MQTT_HOST:$MQTT_PORT"
  broker_url_tls: "mqtts://$MQTT_HOST:$MQTT_TLS_PORT"
  client_id_prefix: "gw"
  qos: 1
  keepalive: 60
  reconnect_delay: 5
  max_reconnect_delay: 60
  topics:
    telemetry: "gateway/{device_id}/telemetry"
    status: "gateway/{device_id}/status"
    log: "gateway/{device_id}/log"
    command: "gateway/{device_id}/command/set"
    response: "gateway/{device_id}/command/response"

monitoring:
  interval: 30

logging:
  level: "info"
  file: "$LOG_DIR/agent.log"
  format: "json"

commands:
  enabled: true
  allowed: []
  shell:
    timeout: 30
    allowed_paths:
      - "/opt/gateway/scripts"
      - "/usr/local/bin"

modbus:
  enabled: false
  devices: []

gpio:
  enabled: true
  sensors: []

terminal:
  enabled: true
  backend_ws_url: "wss://$MQTT_HOST/ws"
  agent_secret: ""  # Filled after provisioning
  shell: "/bin/bash"
  file_dir: "/tmp"
  max_file_bytes: 26214400
  idle_timeout_minutes: 30
  max_session_hours: 8
  max_sessions: 5
  heartbeat_ms: 30000
  reconnect_base_ms: 1000
  reconnect_max_ms: 30000

wifi_ap:
  enabled: true
  interface: "wlan0"
  connection: "mango-ap"
  max_clients: 16

local_broker:
  enabled: true
  mode: "secure"
  bind: "0.0.0.0"
  port_unsecured: 1883
  port_secure: 8883
  allow_anonymous_unsecured: false
  users: []
  max_connections: 100
  max_payload_bytes: 65536
  cert_dir: "$CONFIG_DIR/mqtt/certs"
  conf_path: "$CONFIG_DIR/mqtt/mosquitto.conf"
  data_dir: "$DATA_DIR/mqtt"
  service: "mango-local-broker"

local_client:
  enabled: true
  username: "gateway-local"
  password: ""
  topics:
    - "meter/#"
  max_payload_bytes: 65536

forwarding:
  enabled: true
  batch_size: 100
  poll_interval_sec: 5
  telemetry_retention_days: 30
  sent_retention_days: 7
  max_db_mb: 512

ota:
  enabled: true
  firmware_dir: "$DATA_DIR/firmware"
  backup_dir: "$DATA_DIR/firmware/backup"
  signing_key: ""  # Set in platform for signed OTA
  rollback_window_min: 10
  health_check_interval_sec: 30

cloudflare:
  enabled: false
  agent_secret: ""
  tunnel_id: ""

queue:
  max_events: 20000
  ttl_hours: 72
  max_mb: 256
  flush_batch: 100
  path: "$DATA_DIR/spool.db"

health:
  enabled: true
  listen: "127.0.0.1:8090"
EOF

  # Set permissions
  chmod 600 "$CONFIG_DIR/config.yml"
  chown -R root:root "$CONFIG_DIR"
  
  log "Config written to $CONFIG_DIR/config.yml"
}

# ─────────────────────────────────────────────────────────────────────────────
# 4G Setup (optional, non-blocking)
# ─────────────────────────────────────────────────────────────────────────────
setup_4g() {
  [[ "$SKIP_4G_SETUP" == "true" ]] && { log "Skipping 4G setup (--skip-4g)"; return; }
  [[ "$ENABLE_4G" == "no" ]] && { log "4G disabled by config"; return; }
  [[ "$FOUR_G_DETECTED" != "true" && "$ENABLE_4G" == "auto" ]] && { log "No 4G modem detected (auto mode) — skipping"; return; }
  
  step "Setting up 4G connectivity..."
  
  # Install 4G auto-setup script if available
  if [[ -f "$INSTALL_DIR/setup-4g-auto.sh" ]]; then
    log "Running 4G auto-setup script..."
    apn_arg=()
    if [[ -n "${APN:-}" && "${APN}" != "auto" ]]; then
      # Alias or raw APN: airtel/jio/vi/bsnl or custom (e.g. airtelgprs.com/jionet/www)
      apn_arg=(--apn "$APN")
    elif [[ "$ENABLE_4G" != "no" && "$SKIP_4G_SETUP" != "true" ]]; then
      # No APN supplied → auto-detect (airtel→airtelgprs.com, jio→jionet, vi→www, bsnl→bsnlnet)
      apn_arg=(--apn auto)
    fi
    bash "$INSTALL_DIR/setup-4g-auto.sh" "${apn_arg[@]}" || warn "4G setup had issues (non-fatal)"
  else
    warn "4G setup script not found at $INSTALL_DIR/setup-4g-auto.sh — skipping"
  fi
  
  # Ensure ModemManager and fix-4g auto-start are enabled
  systemctl enable --now ModemManager 2>/dev/null || warn "ModemManager not available"
  systemctl enable --now NetworkManager 2>/dev/null || true
  systemctl is-enabled fix-4g.service 2>/dev/null | grep -q enabled || warn "fix-4g.service not enabled — will be on next setup-4g-auto run"
  
  log "4G setup attempted (check logs if issues)"
  log "Post-reboot check: systemctl is-enabled fix-4g ModemManager NetworkManager; nmcli c show airtel | grep autoconnect"
}

# ─────────────────────────────────────────────────────────────────────────────
# Systemd service
# ─────────────────────────────────────────────────────────────────────────────
install_service() {
  step "Installing systemd service..."
  
  cat > "/etc/systemd/system/$SERVICE_NAME.service" <<EOF
[Unit]
Description=Mango IoT Gateway Agent
Documentation=https://github.com/prashantsingh95/mango-iot-gateway-client
After=network-online.target mosquitto.service ModemManager.service
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
User=root
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/gateway-agent --config $CONFIG_DIR/config.yml
Restart=always
RestartSec=5
StartLimitBurst=3
StandardOutput=journal
StandardError=journal
SyslogIdentifier=$SERVICE_NAME
Environment=PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
Environment=CONFIG_DIR=$CONFIG_DIR
Environment=DATA_DIR=$DATA_DIR
Environment=LOG_DIR=$LOG_DIR
# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$CONFIG_DIR $DATA_DIR $LOG_DIR /data/offline /sys/class/gpio /dev/gpiomem
CapabilityBoundingSet=CAP_NET_ADMIN CAP_SYS_REBOOT CAP_DAC_OVERRIDE

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  systemctl enable "$SERVICE_NAME"
  
  log "Service installed and enabled"
}

# ─────────────────────────────────────────────────────────────────────────────
# Start service
# ─────────────────────────────────────────────────────────────────────────────
start_service() {
  step "Starting gateway agent..."
  
  systemctl restart "$SERVICE_NAME"
  
  # Wait for health endpoint
  local retries=30
  while [[ $retries -gt 0 ]]; do
    if curl -sf "http://127.0.0.1:8090/health" &>/dev/null; then
      log "Agent is healthy"
      break
    fi
    sleep 1
    ((retries--))
  done
  
  if [[ $retries -eq 0 ]]; then
    warn "Agent health check timed out — check logs: journalctl -u $SERVICE_NAME -f"
  else
    log "Agent started successfully"
  fi
}

# ─────────────────────────────────────────────────────────────────────────────
# Print summary
# ─────────────────────────────────────────────────────────────────────────────
print_summary() {
  echo
  echo -e "${BLUE}═══════════════════════════════════════════════════════════════════════${NC}"
  echo -e "${GREEN}  Mango IoT Gateway Agent — Deployment Complete${NC}"
  echo -e "${BLUE}═══════════════════════════════════════════════════════════════════════${NC}"
  echo
  echo -e "  ${YELLOW}Device ID:${NC}        $DEVICE_ID"
  echo -e "  ${YELLOW}Serial:${NC}           $SERIAL_NUMBER"
  echo -e "  ${YELLOW}Platform:${NC}         $PLATFORM_URL"
  echo -e "  ${YELLOW}Install Dir:${NC}      $INSTALL_DIR"
  echo -e "  ${YELLOW}Config Dir:${NC}       $CONFIG_DIR"
  echo -e "  ${YELLOW}Data Dir:${NC}         $DATA_DIR"
  echo -e "  ${YELLOW}Log Dir:${NC}          $LOG_DIR"
  echo -e "  ${YELLOW}Service:${NC}          $SERVICE_NAME"
  echo -e "  ${YELLOW}4G Modem:${NC}         $([[ "$FOUR_G_DETECTED" == "true" ]] && echo "Detected: $FOUR_G_DEVICE" || echo "Not detected / disabled")"
  echo
  echo -e "  ${YELLOW}Health Check:${NC}     curl http://127.0.0.1:8090/health"
  echo -e "  ${YELLOW}Logs:${NC}             journalctl -u $SERVICE_NAME -f"
  echo -e "  ${YELLOW}Config:${NC}           cat $CONFIG_DIR/config.yml"
  echo -e "  ${YELLOW}Local API:${NC}        curl http://127.0.0.1:8090/api/mqtt/local/status"
  echo
  echo -e "${BLUE}═══════════════════════════════════════════════════════════════════════${NC}"
  echo -e "${GREEN}  Agent will auto-start on boot. Provisioning happens automatically.${NC}"
  echo -e "${BLUE}═══════════════════════════════════════════════════════════════════════${NC}"
  echo
}

# ─────────────────────────────────────────────────────────────────────────────
# Main
# ─────────────────────────────────────────────────────────────────────────────
main() {
  echo -e "${BLUE}"
  echo "╔═══════════════════════════════════════════════════════════════════════╗"
  echo "║     Mango IoT Gateway Agent — One-Command Pi Deployment             ║"
  echo "║     Supports: Pi 3, Pi 4, Pi 400, CM4 — Optional 4G                 ║"
  echo "╚═══════════════════════════════════════════════════════════════════════╝"
  echo -e "${NC}"
  
  parse_args "$@"
  
  # Check root
  if [[ $EUID -ne 0 ]]; then
    error "Run as root (sudo)."
  fi
  
  # Check OS
  if ! grep -qi "raspberry\|debian\|ubuntu" /etc/os-release 2>/dev/null; then
    warn "Non-Raspberry Pi OS detected — continuing anyway"
  fi
  
  detect_hardware
  install_deps
  build_agent
  generate_config
  setup_4g
  install_service
  start_service
  print_summary
}

main "$@"