#!/usr/bin/env bash
#=============================================================================
# Mango IoT Gateway Agent — Raspberry Pi One-Command Installer
#
#   Installs the Go gateway agent on a Raspberry Pi as a systemd service.
#   Connects to your cloud IoT Platform server via MQTT.
#
# Usage:
#   # Interactive (prompts for missing values)
#   sudo bash setup.sh
#
#   # Fully automated — telemetry + provisioning + remote terminal in ONE command
#   # (fresh Pi OS: apt-get install -y git && git clone <repo> && cd mango-iot-gateway-client && run:)
#   sudo bash setup.sh \
#     --server mqtts://your-broker.com:8883 \
#     --mqtt-user iot \
#     --mqtt-pass MySecret123 \
#     --token prov-token-abc123 \
#     --device-id factory-gw-01 \
#     --name "Factory Gateway #1" \
#     --platform-url http://YOUR_PLATFORM_HOST:3001 \
#     --agent-secret ONE_TIME_AGENT_SECRET \
#     --ws-url ws://YOUR_PLATFORM_HOST:3001 \
#     --signing-pepper YOUR_TERMINAL_SIGNING_PEPPER
#
# Arguments:
#   --server URL        MQTT broker URL (required). mqtts:// auto-enables TLS.
#   --mqtt-user USER    MQTT username   (required if server has auth)
#   --mqtt-pass PASS    MQTT password
#   --mqtt-ssl BOOL     Force MQTT TLS on/off (default: auto from --server scheme)
#   --token TOKEN       Provisioning token from cloud UI (Provisioning page)
#   --device-id ID      Unique device ID (default: auto from MAC)
#   --name NAME         Human-readable name
#   --platform-url URL  Platform API URL (required for provisioning)
#   --agent-secret SEC  One-time terminal agent secret (Platform: open the
#                       gateway → Terminal tab → Issue secret). Enables the
#                       remote-terminal block in one command.
#   --ws-url URL        Backend WebSocket URL for terminal (default: derived
#                       from --platform-url, http→ws / https→wss)
#   --signing-pepper P  Must equal backend TERMINAL_SIGNING_PEPPER
#   --terminal-shell S  Shell for remote terminal (default: /bin/bash)
#   --help
#
# Environment:
#   SKIP_BUILD=1     Use pre-built binary from GitHub releases
#=============================================================================

set -u
set -o pipefail

SCRIPT_VERSION="1.0.0"
LOGFILE="/var/log/iot-gateway-setup.log"
LOCKFILE="/var/run/iot-gateway-setup.lock"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ============================================================================
# Colors
# ============================================================================
setup_colors() {
  RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
}
log()  { echo -e "${GREEN}[$(date +%H:%M:%S)] ✓${NC} $1" | tee -a "$LOGFILE"; }
warn() { echo -e "${YELLOW}[$(date +%H:%M:%S)] !${NC} $1" | tee -a "$LOGFILE"; }
err()  { echo -e "${RED}[$(date +%H:%M:%S)] ✗${NC} $1" | tee -a "$LOGFILE"; exit 1; }
info() { echo -e "${CYAN}[$(date +%H:%M:%S)] i${NC} $1" | tee -a "$LOGFILE"; }

# ============================================================================
# Parse args
# ============================================================================
parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --server)     SERVER="$2"; shift 2 ;;
      --mqtt-user)  MQTT_USER="$2"; shift 2 ;;
      --mqtt-pass)  MQTT_PASS="$2"; shift 2 ;;
      --mqtt-ssl)   MQTT_SSL="$2"; shift 2 ;;
      --token)      TOKEN="$2"; shift 2 ;;
      --device-id)  DEVICE_ID="$2"; shift 2 ;;
      --name)       GW_NAME="$2"; shift 2 ;;
      --platform-url) PLATFORM_URL="$2"; shift 2 ;;
      --agent-secret) AGENT_SECRET="$2"; shift 2 ;;
      --ws-url) WS_URL="$2"; shift 2 ;;
      --signing-pepper) SIGNING_PEPPER="$2"; shift 2 ;;
      --terminal-shell) TERMINAL_SHELL="$2"; shift 2 ;;
      --help|-h)
        echo "Usage: sudo bash setup.sh [options]"
        echo ""
        echo "Options:"
        echo "  --server URL        MQTT broker URL (e.g. mqtts://broker:8883, mqtt://10.0.0.1:1883)"
        echo "  --mqtt-user USER    MQTT username"
        echo "  --mqtt-pass PASS    MQTT password"
        echo "  --mqtt-ssl BOOL     true/false (default: auto — on for mqtts:// or ssl://)"
        echo "  --token TOKEN       Provisioning token (from cloud UI > Provisioning)"
        echo "  --device-id ID      Unique device ID (auto from MAC if not set)"
        echo "  --name NAME         Human-readable name"
        echo "  --platform-url URL  Platform API URL (required for provisioning)"
        echo "  --agent-secret SEC  One-time terminal agent secret (enables remote terminal)"
        echo "  --ws-url URL        Backend WS URL (default: derived from --platform-url)"
        echo "  --signing-pepper P  Must match backend TERMINAL_SIGNING_PEPPER"
        echo "  --terminal-shell S  Shell for remote terminal (default: /bin/bash)"
        echo "  --help              Show this help"
        echo ""
        echo "One-command example (telemetry + provisioning + terminal):"
        echo "  sudo bash setup.sh \\"
        echo "    --server mqtts://broker.example.com:8883 \\"
        echo "    --mqtt-user iot --mqtt-pass MyPass123 \\"
        echo "    --token abc-123 --device-id factory-gw-01 \\"
        echo "    --name \"Factory Gateway #1\" \\"
        echo "    --platform-url http://10.0.0.1:3001 \\"
        echo "    --agent-secret ONE_TIME_SECRET \\"
        echo "    --signing-pepper PEPPER_FROM_BACKEND_ENV"
        exit 0 ;;
      *) err "Unknown: $1. See --help" ;;
    esac
  done
}

# ============================================================================
# Pre-flight
# ============================================================================
preflight() {
  [[ $EUID -eq 0 ]] || err "Run with sudo: sudo bash setup.sh"

  [[ -f "$LOCKFILE" ]] && kill -0 "$(cat "$LOCKFILE")" 2>/dev/null && \
    err "Setup already running (PID $(cat "$LOCKFILE"))"
  echo $$ > "$LOCKFILE"
  trap 'rm -f "$LOCKFILE"' EXIT

  ARCH=$(uname -m)
  case "$ARCH" in
    aarch64|armv8l) PI="Pi 4B/5 (64-bit)" ;;
    armv7l)         PI="Pi 3B/Zero 2W (32-bit)" ;;
    armv6l)         PI="Pi Zero/1" ;;
    x86_64)         PI="x86_64" ;;
    *)              PI="$ARCH" ;;
  esac

  MEM=$(free -m | awk '/^Mem:/{print $2}')
  DISK=$(df -m / | awk 'NR==2{print $4}')
  info "$PI | ${MEM}MB RAM | ${DISK}MB free"
  [[ $DISK -lt 200 ]] && err "Need 200MB+ free disk"
}

# ============================================================================
# Stage 1 — Dependencies
# ============================================================================
install_deps() {
  info "Installing system dependencies..."
  apt-get update -qq
  apt-get install -y -qq curl wget git ca-certificates haveged ntp logrotate jq make gcc

  apt-get install -y -qq gpio wiringpi i2c-tools 2>/dev/null || \
    warn "GPIO packages unavailable (non-Pi?)"

  raspi-config nonint do_i2c 0 2>/dev/null || true
  raspi-config nonint do_spi 0 2>/dev/null || true
}

# ============================================================================
# Stage 2 — Build binary
# ============================================================================
install_binary() {
  local BIN="/usr/local/bin/gateway-agent"

  if [[ -z "${SKIP_BUILD:-}" ]]; then
    # Install Go if missing
    if ! command -v go &>/dev/null; then
      local VER="1.22.5"
      case "$ARCH" in
        aarch64) GA="arm64" ;; armv7l|armv6l) GA="armv6l" ;; x86_64) GA="amd64" ;; *) GA="armv6l" ;;
      esac
      info "Installing Go ${VER} (${GA})..."
      curl -fsSL "https://go.dev/dl/go${VER}.linux-${GA}.tar.gz" | tar -C /usr/local -xz
      export PATH="/usr/local/go/bin:$PATH"
      echo 'export PATH=$PATH:/usr/local/go/bin' > /etc/profile.d/go.sh
    fi

    [[ -f "$SCRIPT_DIR/main.go" ]] || err "main.go not found in $SCRIPT_DIR"

    info "Compiling gateway agent..."
    pushd "$SCRIPT_DIR" >/dev/null
    go mod download >> "$LOGFILE" 2>&1
    CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${SCRIPT_VERSION}" -o "$BIN" . >> "$LOGFILE" 2>&1 || err "Build failed"
    popd >/dev/null
    log "Built: $BIN"
  else
    local SUFFIX; case "$ARCH" in aarch64) SUFFIX="arm64" ;; armv7l) SUFFIX="arm" ;; x86_64) SUFFIX="amd64" ;; *) SUFFIX="arm" ;; esac
    info "Downloading binary (${SUFFIX})..."
    curl -fsSL "https://github.com/your-org/iot-gateway-platform/releases/latest/download/gateway-agent-linux-${SUFFIX}" -o "$BIN" || err "Download failed"
    log "Downloaded: $BIN"
  fi

  chmod 755 "$BIN"
}

# ============================================================================
# Stage 3 — Configuration
# ============================================================================
configure() {
  # Interactive prompts if not provided
  if [[ -z "${SERVER:-}" ]]; then
    echo ""; echo "Enter MQTT broker URL from your cloud server:"
    read -r -p "  Server URL (mqtt://your-server.com:1883): " SERVER
    SERVER="${SERVER:-}"
    [[ -z "$SERVER" ]] && err "Server URL required"
  fi

  if [[ -z "${MQTT_USER:-}" ]]; then
    read -r -p "  MQTT username (optional): " MQTT_USER
  fi

  if [[ -z "${TOKEN:-}" ]]; then
    echo "  Provisioning token (optional — get from cloud UI > Provisioning page):"
    read -r -p "  Token: " TOKEN
  fi

  if [[ -z "${PLATFORM_URL:-}" ]]; then
    read -r -p "  Platform API URL (e.g. https://iot.example.com): " PLATFORM_URL
    [[ -z "$PLATFORM_URL" ]] && err "Platform API URL required for provisioning"
  fi

  # Auto device ID from MAC
  if [[ -z "${DEVICE_ID:-}" ]]; then
    local MAC
    MAC=$(cat /sys/class/net/eth0/address 2>/dev/null || cat /sys/class/net/wlan0/address 2>/dev/null || echo "unknown")
    DEVICE_ID="gw-$(echo "$MAC" | tr -d ':')"
  fi

  local IP; IP=$(hostname -I | awk '{print $1}')
  GW_NAME="${GW_NAME:-Pi Gateway ${IP}}"

  # MQTT TLS: explicit --mqtt-ssl wins, else auto from scheme (mqtts://, ssl://)
  if [[ -z "${MQTT_SSL:-}" ]]; then
    case "${SERVER}" in
      mqtts://*|ssl://*) MQTT_SSL="true" ;;
      *) MQTT_SSL="false" ;;
    esac
  fi

  # Terminal WS URL defaults to the platform URL with http→ws / https→wss
  if [[ -z "${WS_URL:-}" && -n "${PLATFORM_URL:-}" ]]; then
    WS_URL="${PLATFORM_URL/#http:/ws:}"
    WS_URL="${WS_URL/#https:/wss:}"
  fi
  TERMINAL_SHELL="${TERMINAL_SHELL:-/bin/bash}"

  mkdir -p /opt/gateway

  # Copy full config template if available, else generate complete config
  if [[ -f "$SCRIPT_DIR/config.yml" ]]; then
    # Use template as base and override with CLI-provided values.
    # NOTE: keys are anchored (^ + leading whitespace + exact key) so that
    # e.g. the name: pattern never clobbers username: or product_name:.
    sed \
      -e "s|^\([[:space:]]*\)broker_url:.*|\1broker_url: \"${SERVER}\"|" \
      -e "s|^\([[:space:]]*\)username:.*|\1username: \"${MQTT_USER:-}\"|" \
      -e "s|^\([[:space:]]*\)password:.*|\1password: \"${MQTT_PASS:-}\"|" \
      -e "s|^\([[:space:]]*\)ssl:.*|\1ssl: ${MQTT_SSL}|" \
      -e "s|^\([[:space:]]*\)device_id:.*|\1device_id: \"${DEVICE_ID}\"|" \
      -e "s|^\([[:space:]]*\)name:.*|\1name: \"${GW_NAME}\"|" \
      -e "s|^\([[:space:]]*\)provision_token:.*|\1provision_token: \"${TOKEN:-}\"|" \
      -e "s|^\([[:space:]]*\)platform_url:.*|\1platform_url: \"${PLATFORM_URL}\"|" \
      "$SCRIPT_DIR/config.yml" > /opt/gateway/config.yml
  else
    # Generate complete config from scratch
    cat > /opt/gateway/config.yml << YAML
gateway:
  device_id: "${DEVICE_ID}"
  name: "${GW_NAME}"
  tenant_id: "default"
  provision_token: "${TOKEN:-}"
  platform_url: ""

mqtt:
  broker_url: "${SERVER}"
  username: "${MQTT_USER:-}"
  password: "${MQTT_PASS:-}"
  client_id_prefix: "gw"
  ssl: ${MQTT_SSL:-false}
  qos: 1
  keep_alive: 60
  clean_session: false
  reconnect_delay: 5
  max_reconnect_delay: 60
  topics:
    telemetry: "gateway/{device_id}/telemetry"
    status: "gateway/{device_id}/status"
    log: "gateway/{device_id}/log"
    command: "gateway/{device_id}/command/set"
    response: "gateway/{device_id}/command/response"

modbus:
  enabled: true
  devices: []

gpio:
  enabled: false

monitoring:
  interval: 30
  cpu: true
  memory: true
  disk: true
  temperature: true
  network: true

logging:
  level: "info"
  file: "/var/log/gateway-agent.log"
  remote: true

ota:
  enabled: true
  firmware_dir: "/opt/gateway/firmware"
  backup_dir: "/opt/gateway/backup"
  auto_rollback: true
  rollback_timeout: 30

watchdog:
  enabled: true
  interval: 60
  max_missed_pings: 3
  action: "restart"

commands:
  enabled: true
  allowed:
    - "reboot"
    - "restart_agent"
    - "run_shell"
    - "update_firmware"
    - "set_relay"
    - "read_register"
  shell:
    allowed_paths:
      - "/opt/gateway/scripts/"
      - "/usr/local/bin/"
    timeout: 30
YAML
  fi

  chmod 644 /opt/gateway/config.yml
  log "Config: /opt/gateway/config.yml"

  # ── Remote terminal (one-command setup) ──────────────────────
  # Needs --agent-secret (one-time secret from platform gateway Terminal tab)
  # gateway_id stays empty: the agent auto-uses the platform UUID persisted
  # at provisioning time (terminal.go gatewayID()).
  if [[ -n "${AGENT_SECRET:-}" ]]; then
    [[ -z "${WS_URL:-}" ]] && err "--agent-secret needs --ws-url or --platform-url for backend_ws_url"
    [[ -z "${SIGNING_PEPPER:-}" ]] && err "--agent-secret needs --signing-pepper (must match backend TERMINAL_SIGNING_PEPPER)"
    if grep -q '^terminal:' /opt/gateway/config.yml; then
      # Drop the old terminal block (up to the next top-level key), then append fresh
      awk '/^terminal:/{skip=1; next} /^[A-Za-z_]+:/{skip=0} !skip' \
        /opt/gateway/config.yml > /opt/gateway/config.yml.new \
        && mv /opt/gateway/config.yml.new /opt/gateway/config.yml
    fi
    cat >> /opt/gateway/config.yml << YAML

# Remote terminal (reverse-connection, no inbound ports). Configured by setup.sh.
terminal:
  enabled: true
  gateway_id: ""                   # auto: platform UUID persisted at provisioning
  backend_ws_url: "${WS_URL}"
  agent_secret: "${AGENT_SECRET}"
  signing_pepper: "${SIGNING_PEPPER}"
  heartbeat_ms: 30000
  reconnect_base_ms: 1000
  reconnect_max_ms: 30000
  shell: "${TERMINAL_SHELL}"
  shell_allowlist:
    - "${TERMINAL_SHELL}"
  file_dir: "/tmp"
  max_file_bytes: 26214400
  idle_timeout_minutes: 30
  max_session_hours: 8
  max_sessions: 5
  insecure_skip_verify: false
YAML
    chmod 600 /opt/gateway/config.yml
    log "Remote terminal: ENABLED (ws: ${WS_URL})"
  else
    info "Remote terminal: disabled (pass --agent-secret to enable in one command)"
  fi

  # Example custom script
  mkdir -p /opt/gateway/scripts
  cat > /opt/gateway/scripts/example.sh << 'SH'
#!/bin/sh
echo "Gateway custom script ran at $(date)"
SH
  chmod +x /opt/gateway/scripts/example.sh
}

# ============================================================================
# Stage 4 — systemd service
# ============================================================================
install_service() {
  if ! id -u gateway &>/dev/null; then
    groupadd --system gateway 2>/dev/null; useradd --system --no-create-home -g gateway -s /usr/sbin/nologin gateway
  fi
  usermod -a -G gpio,i2c,dialout gateway 2>/dev/null

  mkdir -p /opt/gateway/firmware /opt/gateway/backup /opt/gateway/scripts /var/lib/gateway
  chown -R gateway:gateway /opt/gateway /var/lib/gateway 2>/dev/null

  cat > /etc/systemd/system/gateway-agent.service << UNIT
[Unit]
Description=Mango IoT Gateway Agent
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
User=gateway
Group=gateway
ExecStart=/usr/local/bin/gateway-agent --config /opt/gateway/config.yml
Restart=always
RestartSec=10
LimitNOFILE=65536
StandardOutput=append:/var/log/gateway-agent.log
StandardError=append:/var/log/gateway-agent.log

[Install]
WantedBy=multi-user.target
UNIT

  cat > /etc/logrotate.d/gateway-agent << LOG
/var/log/gateway-agent.log {
    daily; rotate 7; compress; delaycompress; missingok; notifempty; copytruncate; maxsize 10M
}
LOG

  systemctl daemon-reload
  systemctl enable gateway-agent
  log "Service installed"
}

# ============================================================================
# Stage 5 — Start
# ============================================================================
start_agent() {
  systemctl start gateway-agent >> "$LOGFILE" 2>&1 || {
    journalctl -u gateway-agent -n 20 --no-pager | tee -a "$LOGFILE"
    err "Start failed — journalctl -u gateway-agent -f"
  }

  sleep 3
  if systemctl is-active --quiet gateway-agent; then
    log "Gateway agent: RUNNING"
  else
    warn "Gateway agent: NOT RUNNING — check logs"
  fi

  echo ""
  echo -e "${GREEN}╔═════════════════════════════════════════════╗${NC}"
  echo -e "${GREEN}║      Gateway Agent — Installed             ║${NC}"
  echo -e "${GREEN}╚═════════════════════════════════════════════╝${NC}"
  echo ""
  echo -e "  ${CYAN}Server:${NC}    ${SERVER}"
  echo -e "  ${CYAN}MQTT TLS:${NC}  ${MQTT_SSL:-false}"
  echo -e "  ${CYAN}Device:${NC}    ${DEVICE_ID}"
  echo -e "  ${CYAN}Config:${NC}    /opt/gateway/config.yml"
  if [[ -n "${AGENT_SECRET:-}" ]]; then
  echo -e "  ${CYAN}Terminal:${NC}  enabled (${WS_URL:-})"
  else
  echo -e "  ${CYAN}Terminal:${NC}  disabled (re-run with --agent-secret to enable)"
  fi
  echo -e "  ${CYAN}Logs:${NC}      journalctl -u gateway-agent -f"
  echo -e "  ${CYAN}Status:${NC}    systemctl status gateway-agent"
  echo -e "  ${CYAN}Restart:${NC}   systemctl restart gateway-agent"
  echo ""
}

# ============================================================================
# Main
# ============================================================================
main() {
  setup_colors
  mkdir -p "$(dirname "$LOGFILE")"

  echo ""
  echo -e "${CYAN}╔═════════════════════════════════════════════╗${NC}"
  echo -e "${CYAN}║   Mango IoT Pi Gateway Agent Installer     ║${NC}"
  echo -e "${CYAN}╚═════════════════════════════════════════════╝${NC}"
  echo ""

  parse_args "$@"
  info "Log: $LOGFILE"

  preflight
  install_deps
  install_binary
  configure
  install_service
  start_agent
}

main "$@"
