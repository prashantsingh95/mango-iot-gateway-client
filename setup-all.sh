#!/usr/bin/env bash
#===============================================================================
# setup-all.sh — ONE command after git clone: agent + config + 4G + services
#
#   git clone -b <branch> https://github.com/prashantsingh95/mango-iot-gateway-client.git
#   cd mango-iot-gateway-client
#   sudo bash setup-all.sh \
#     --server mqtts://BROKER:8883 --mqtt-user iot --mqtt-pass SECRET \
#     --token PROV_TOKEN --platform-url http://HOST:3001 \
#     --agent-secret ONE_TIME_SECRET --signing-pepper PEPPER
#
# What it does (fully automatic, handles the 4G reboot for you):
#   Phase 1: deps → build → config → systemd service → 4G boot config → REBOOT
#   Phase 2 (runs by itself after reboot):
#           wait for modem → SIM slot fix → 4G data up → agent health → report
#
# Flags (setup.sh-compatible):
#   --server URL          MQTT broker URL (mqtts://host:8883)
#   --mqtt-user USER      MQTT username
#   --mqtt-pass PASS      MQTT password
#   --token TOKEN         Provisioning token (Platform > Provisioning)
#   --platform-url URL    Platform API URL (http://HOST:3001)
#   --device-id ID        Fixed device ID (default: auto from CPU serial)
#   --name NAME           Human-readable gateway name
#   --agent-secret SEC    One-time terminal secret (enables remote terminal)
#   --ws-url URL          Backend WS URL (default: derived from --platform-url)
#   --signing-pepper P    Must match backend TERMINAL_SIGNING_PEPPER
#   --local-mqtt MODE     disabled|unsecured|secure|both (default: disabled)
#   --apn APN             4G APN: auto|airtel|jio|vi|bsnl|<raw> (default: auto)
#   --4g MODE             auto|no  (auto = set up 4G + reboot for it)
#   --no-reboot           Do not reboot; you reboot manually later
#   --force-config        Regenerate config.yml even if it already exists
#   --skip-build          Use existing ./gateway-agent (must be Linux ELF)
#   --phase2              INTERNAL: post-reboot step (used by resume service)
#   --help                This help
#
# Re-runs are safe (idempotent): existing config/service/4G are kept.
#===============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STATE_DIR="/var/lib/mango-bootstrap"
ARGS_FILE="$STATE_DIR/args.env"
RESUME_UNIT="mango-bootstrap-resume.service"
BIN="/opt/gateway/gateway-agent"
CFG="/opt/gateway/config.yml"
LOGFILE="/var/log/mango-setup-all.log"
# Not root yet (e.g. --help or arg errors): log somewhere writable
[[ $EUID -ne 0 || ! -w /var/log ]] && LOGFILE="/tmp/mango-setup-all.log"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; CYAN='\033[0;36m'; NC='\033[0m'
log()  { echo -e "${GREEN}[$(date +%H:%M:%S)] ✓${NC} $*" | tee -a "$LOGFILE"; }
info() { echo -e "${CYAN}[$(date +%H:%M:%S)] i${NC} $*" | tee -a "$LOGFILE"; }
warn() { echo -e "${YELLOW}[$(date +%H:%M:%S)] !${NC} $*" | tee -a "$LOGFILE"; }
err()  { echo -e "${RED}[$(date +%H:%M:%S)] ✗${NC} $*" | tee -a "$LOGFILE"; exit 1; }
step() { echo -e "${BLUE}[$(date +%H:%M:%S)] ▸${NC} $*" | tee -a "$LOGFILE"; }

usage() { sed -n '2,38p' "$0" | sed 's/^# \{0,1\}//'; exit 0; }

# ── Args ─────────────────────────────────────────────────────────────────────
SERVER=""; MQTT_USER=""; MQTT_PASS=""; MQTT_SSL=""
TOKEN=""; PLATFORM_URL=""; DEVICE_ID=""; GW_NAME=""
AGENT_SECRET=""; WS_URL=""; SIGNING_PEPPER=""; TERMINAL_SHELL="/bin/bash"
LOCAL_MQTT_MODE="disabled"; NO_FIREWALL=""
APN="auto"; FOUR_G="auto"; NO_REBOOT=0; FORCE_CONFIG=0; SKIP_BUILD=""
PHASE2=0
NEED_REBOOT=0

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --server)         SERVER="$2"; shift 2 ;;
      --mqtt-user)      MQTT_USER="$2"; shift 2 ;;
      --mqtt-pass)      MQTT_PASS="$2"; shift 2 ;;
      --mqtt-ssl)       MQTT_SSL="$2"; shift 2 ;;
      --token)          TOKEN="$2"; shift 2 ;;
      --platform-url)   PLATFORM_URL="$2"; shift 2 ;;
      --device-id)      DEVICE_ID="$2"; shift 2 ;;
      --name)           GW_NAME="$2"; shift 2 ;;
      --agent-secret)   AGENT_SECRET="$2"; shift 2 ;;
      --ws-url)         WS_URL="$2"; shift 2 ;;
      --signing-pepper) SIGNING_PEPPER="$2"; shift 2 ;;
      --terminal-shell) TERMINAL_SHELL="$2"; shift 2 ;;
      --local-mqtt)     LOCAL_MQTT_MODE="$2"; shift 2 ;;
      --no-firewall)    NO_FIREWALL=1; shift ;;
      --apn)            APN="$2"; shift 2 ;;
      --4g)             FOUR_G="$2"; shift 2 ;;
      --no-reboot)      NO_REBOOT=1; shift ;;
      --force-config)   FORCE_CONFIG=1; shift ;;
      --skip-build)     SKIP_BUILD=1; shift ;;
      --phase2)         PHASE2=1; shift ;;
      --help|-h)        usage ;;
      *) err "Unknown: $1 (see --help)" ;;
    esac
  done
  case "$LOCAL_MQTT_MODE" in
    disabled|unsecured|secure|both) ;;
    *) err "--local-mqtt must be disabled|unsecured|secure|both" ;;
  esac
  case "$FOUR_G" in auto|no) ;; *) err "--4g must be auto|no" ;; esac
}

# Persist args so the post-reboot phase can re-use them
save_state() {
  mkdir -p "$STATE_DIR"
  cat > "$ARGS_FILE" << EOF
SCRIPT_DIR='$SCRIPT_DIR'
APN='$APN'
FOUR_G='$FOUR_G'
PLATFORM_URL='$PLATFORM_URL'
EOF
  chmod 600 "$ARGS_FILE"
}

load_state() {
  [[ -f "$ARGS_FILE" ]] && source "$ARGS_FILE"
}

# ── Phase 1 ──────────────────────────────────────────────────────────────────
preflight() {
  [[ $EUID -eq 0 ]] || err "Run with sudo: sudo bash setup-all.sh"
  mkdir -p "$(dirname "$LOGFILE")"
  ARCH=$(uname -m)
  case "$ARCH" in
    aarch64) GOARCH="arm64" ;;
    armv7l|armv6l) GOARCH="arm" ;;
    x86_64) GOARCH="amd64" ;;
    *) err "Unsupported arch: $ARCH" ;;
  esac
  local disk; disk=$(df -m / | awk 'NR==2{print $4}')
  [[ "$disk" -lt 200 ]] && err "Need 200MB+ free disk (have ${disk}MB)"
  info "Arch: $ARCH (go: $GOARCH) | disk free: ${disk}MB"
}

install_deps() {
  step "Installing dependencies..."
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq >>"$LOGFILE" 2>&1 || warn "apt-get update had issues (continuing)"
  apt-get install -y -qq \
    curl wget git ca-certificates jq make gcc \
    network-manager modemmanager usb-modeswitch libqmi-utils libmbim-utils \
    mosquitto mosquitto-clients iw wireless-tools \
    >>"$LOGFILE" 2>&1 || warn "Some packages failed to install (see $LOGFILE)"

  # Go toolchain (only needed for the build)
  if ! command -v go >/dev/null 2>&1; then
    info "Installing Go 1.22.5 ($GOARCH)..."
    local tarball="go1.22.5.linux-${GOARCH}.tar.gz"
    curl -fsSL "https://go.dev/dl/${tarball}" -o "/tmp/${tarball}" \
      && rm -rf /usr/local/go && tar -C /usr/local -xzf "/tmp/${tarball}" \
      && ln -sf /usr/local/go/bin/go /usr/local/bin/go \
      || warn "Go install failed — --skip-build will be required"
    export PATH="/usr/local/go/bin:$PATH"
  fi

  # Wi-Fi power save off (SSH stability)
  if command -v nmcli >/dev/null 2>&1; then
    mkdir -p /etc/NetworkManager/conf.d
    printf '[connection]\nwifi.powersave = 2\n' > /etc/NetworkManager/conf.d/powersave.conf
  fi
  log "Dependencies installed"
}

is_linux_elf() {
  [[ -f "$1" ]] || return 1
  [[ "$(head -c 4 "$1" 2>/dev/null | od -An -tx1 | tr -d ' \n')" == "7f454c46" ]]
}

build_agent() {
  step "Building gateway agent..."
  mkdir -p /opt/gateway /var/lib/gateway /var/log/gateway
  export PATH="/usr/local/go/bin:$PATH"

  if [[ -n "$SKIP_BUILD" ]]; then
    is_linux_elf "$SCRIPT_DIR/gateway-agent" || is_linux_elf "$BIN" \
      || err "--skip-build: no Linux ELF gateway-agent found (repo copy is not a Linux binary)"
    if is_linux_elf "$SCRIPT_DIR/gateway-agent"; then
      install -m 755 "$SCRIPT_DIR/gateway-agent" "$BIN"
    fi
    log "Using existing binary: $BIN"
    return
  fi

  command -v go >/dev/null 2>&1 || err "Go not available — re-run with --skip-build after placing a Linux binary at ./gateway-agent"
  [[ -f "$SCRIPT_DIR/main.go" ]] || err "main.go not found in $SCRIPT_DIR"

  pushd "$SCRIPT_DIR" >/dev/null
  go mod download >>"$LOGFILE" 2>&1 || warn "go mod download had issues (continuing)"
  if CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" \
      go build -ldflags="-s -w -X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" \
      -o "$BIN" . >>"$LOGFILE" 2>&1; then
    popd >/dev/null 2>&1 || true
    log "Built: $BIN"
    return
  fi

  # Known failure mode on Pi: corrupted module cache → clean once and retry
  warn "Build failed — cleaning module cache and retrying once..."
  go clean -modcache >>"$LOGFILE" 2>&1 || true
  go mod download >>"$LOGFILE" 2>&1 || true
  if CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" \
      go build -ldflags="-s -w" -o "$BIN" . >>"$LOGFILE" 2>&1; then
    popd >/dev/null 2>&1 || true
    log "Built after modcache clean: $BIN"
    return
  fi
  popd >/dev/null 2>&1 || true

  # Last resort: a prebuilt Linux binary in the repo
  if is_linux_elf "$SCRIPT_DIR/gateway-agent"; then
    install -m 755 "$SCRIPT_DIR/gateway-agent" "$BIN"
    warn "Build failed twice — using prebuilt ./gateway-agent"
    return
  fi
  err "Build failed (see $LOGFILE). Fix Go build or place a Linux ARM binary at ./gateway-agent and re-run with --skip-build"
}

generate_config() {
  step "Generating config..."
  if [[ -f "$CFG" && "$FORCE_CONFIG" -eq 0 ]]; then
    log "Config exists: $CFG (keeping — use --force-config to regenerate)"
    return
  fi

  [[ -z "$SERVER" || -z "$PLATFORM_URL" ]] && [[ -t 0 ]] && {
    echo ""
    [[ -z "$SERVER" ]] && read -r -p "  MQTT broker URL (mqtts://host:8883): " SERVER
    [[ -z "$PLATFORM_URL" ]] && read -r -p "  Platform API URL (http://host:3001): " PLATFORM_URL
    [[ -z "$TOKEN" ]] && read -r -p "  Provisioning token (optional): " TOKEN
  }
  [[ -z "$SERVER" ]] && warn "--server missing — agent will use localhost broker (won't reach cloud)"
  [[ -z "$PLATFORM_URL" ]] && warn "--platform-url missing — provisioning/terminal disabled"

  # MQTT TLS auto from scheme
  if [[ -z "$MQTT_SSL" ]]; then
    case "$SERVER" in mqtts://*|ssl://*) MQTT_SSL="true" ;; *) MQTT_SSL="false" ;; esac
  fi
  # Terminal WS URL: derive from platform URL
  if [[ -z "$WS_URL" && -n "$PLATFORM_URL" ]]; then
    WS_URL="${PLATFORM_URL/#http:/ws:}"; WS_URL="${WS_URL/#https:/wss:}"
  fi

  local DEVICE_ID_EXPLICIT=0
  [[ -n "$DEVICE_ID" ]] && DEVICE_ID_EXPLICIT=1
  [[ -z "$DEVICE_ID" ]] && DEVICE_ID=$(grep -m1 '^Serial' /proc/cpuinfo 2>/dev/null | awk '{print $3}' | tr -d '\0')
  [[ -z "$DEVICE_ID" ]] && DEVICE_ID=$(tr -d '-' </proc/sys/kernel/random/uuid)
  [[ -z "$GW_NAME" ]] && GW_NAME="Gateway $(hostname -I 2>/dev/null | awk '{print $1}')"

  [[ -f "$SCRIPT_DIR/config.yml" ]] || err "config.yml template missing in $SCRIPT_DIR"
  # Temp file first: if this script runs from /opt/gateway, the template IS
  # $CFG — a direct > $CFG redirect would truncate it before sed reads it.
  local TMPCFG; TMPCFG="$(mktemp /opt/gateway/.config.yml.XXXXXX)"
  sed \
    -e "s|^\([[:space:]]*\)broker_url:.*|\1broker_url: \"${SERVER}\"|" \
    -e "s|^\([[:space:]]*\)username:.*|\1username: \"${MQTT_USER}\"|" \
    -e "s|^\([[:space:]]*\)password:.*|\1password: \"${MQTT_PASS}\"|" \
    -e "s|^\([[:space:]]*\)ssl:.*|\1ssl: ${MQTT_SSL}|" \
    -e "s|^\([[:space:]]*\)device_id:.*|\1device_id: \"${DEVICE_ID}\"|" \
    -e "s|^\([[:space:]]*\)name:.*|\1name: \"${GW_NAME}\"|" \
    -e "s|^\([[:space:]]*\)provision_token:.*|\1provision_token: \"${TOKEN}\"|" \
    -e "s|^\([[:space:]]*\)platform_url:.*|\1platform_url: \"${PLATFORM_URL}\"|" \
    "$SCRIPT_DIR/config.yml" > "$TMPCFG"
  [[ -s "$TMPCFG" ]] || err "config render produced empty file (template: $SCRIPT_DIR/config.yml)"
  mv "$TMPCFG" "$CFG"

  # Explicit --device-id must beat the identity pin the agent wrote on first
  # boot (identity.go always returns the pinned value) — re-pin to agree.
  if [[ "$DEVICE_ID_EXPLICIT" -eq 1 && -f /opt/gateway/device.id ]]; then
    local PINNED; PINNED="$(tr -d '[:space:]' < /opt/gateway/device.id)"
    if [[ -n "$PINNED" && "$PINNED" != "$DEVICE_ID" ]]; then
      printf '%s\n' "$DEVICE_ID" > /opt/gateway/device.id
      warn "device.id pin updated: ${PINNED} -> ${DEVICE_ID}"
    fi
  fi

  # Remote terminal block (only when a one-time secret was issued)
  if [[ -n "$AGENT_SECRET" ]]; then
    [[ -z "$WS_URL" ]] && err "--agent-secret needs --ws-url or --platform-url"
    [[ -z "$SIGNING_PEPPER" ]] && err "--agent-secret needs --signing-pepper"
    awk '/^terminal:/{skip=1; next} /^[A-Za-z_]+:/{skip=0} !skip' "$CFG" > "$CFG.new" && mv "$CFG.new" "$CFG"
    cat >> "$CFG" << YAML

terminal:
  enabled: true
  gateway_id: ""
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
    log "Remote terminal: ENABLED (${WS_URL})"
  fi

  # Local meter broker block
  local lb_enabled=false lb_mode="secure"
  [[ "$LOCAL_MQTT_MODE" != "disabled" ]] && { lb_enabled=true; lb_mode="$LOCAL_MQTT_MODE"; }
  awk '/^local_broker:/{skip=1; next} /^local_client:/{skip=1; next} /^[A-Za-z_]+:/{skip=0} !skip' \
    "$CFG" > "$CFG.new" && mv "$CFG.new" "$CFG"
  cat >> "$CFG" << YAML

local_broker:
  enabled: ${lb_enabled}
  mode: "${lb_mode}"
  bind: "0.0.0.0"
  port_unsecured: 1883
  port_secure: 8883
  allow_anonymous_unsecured: false
  users: []
  max_connections: 100
  max_payload_bytes: 65536

local_client:
  enabled: true
  username: "gateway-local"
  password: ""
  topics:
    - "meter/#"
  max_payload_bytes: 65536
YAML

  chmod 600 "$CFG"
  mkdir -p /opt/gateway/scripts /opt/gateway/firmware /opt/gateway/backup
  log "Config written: $CFG"
}

install_service() {
  step "Installing gateway-agent service (root)..."
  cat > /etc/systemd/system/gateway-agent.service << 'UNIT'
[Unit]
Description=Mango IoT Gateway Agent
After=network-online.target fix-4g.service
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
User=root
WorkingDirectory=/opt/gateway
ExecStart=/opt/gateway/gateway-agent --config /opt/gateway/config.yml
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal
SyslogIdentifier=gateway-agent

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable gateway-agent >/dev/null 2>&1
  log "gateway-agent.service enabled (root — required for WiFi AP / NM / terminal)"
}

# 4G boot config + fix-4g service. Returns NEED_REBOOT=1 when the boot
# config changed (GPIO hat power lines) or the modem is not visible yet.
setup_4g_phase1() {
  NEED_REBOOT=0
  if [[ "$FOUR_G" == "no" ]]; then
    info "4G disabled (--4g no)"
    return
  fi

  step "Setting up 4G (boot config + NetworkManager)..."
  local cfgfile="/boot/firmware/config.txt"
  [[ -f "$cfgfile" ]] || cfgfile="/boot/config.txt"
  local had_gpio=0
  grep -qF "gpio=20=op,dh" "$cfgfile" 2>/dev/null && had_gpio=1

  # ModemManager/NM must be present and enabled for boot bring-up
  systemctl enable --now ModemManager >/dev/null 2>&1 || warn "ModemManager enable failed"
  systemctl enable --now NetworkManager >/dev/null 2>&1 || warn "NetworkManager enable failed"

  local apn_arg=(--apn "$APN")
  if [[ -f "$SCRIPT_DIR/setup-4g-auto.sh" ]]; then
    bash "$SCRIPT_DIR/setup-4g-auto.sh" "${apn_arg[@]}" >>"$LOGFILE" 2>&1 \
      || warn "setup-4g-auto.sh reported issues (non-fatal — retried after reboot)"
  else
    warn "setup-4g-auto.sh not found — only boot GPIO config applied"
    [[ "$had_gpio" -eq 0 ]] && {
      grep -qF "gpio=20=op,dh" "$cfgfile" || echo "gpio=20=op,dh" | tee -a "$cfgfile" >/dev/null
      grep -qF "gpio=21=op,dh" "$cfgfile" || echo "gpio=21=op,dh" | tee -a "$cfgfile" >/dev/null
    }
  fi

  # Reboot needed when boot GPIO lines were just added, or modem invisible
  grep -qF "gpio=20=op,dh" "$cfgfile" 2>/dev/null || NEED_REBOOT=1
  [[ "$had_gpio" -eq 0 ]] && NEED_REBOOT=1
  if ! lsusb 2>/dev/null | grep -qiE '1bc7|2c7c|1e0e|2dee|05c6|2bbc|19d2'; then
    if ls /dev/ttyUSB* >/dev/null 2>&1; then
      :
    else
      info "No modem visible yet — reboot will apply hat power from boot config"
      NEED_REBOOT=1
    fi
  fi
}

install_resume_unit() {
  step "Installing post-reboot auto-resume..."
  mkdir -p "$STATE_DIR"
  # Copy ourselves to a stable path (clone dir may move/delete)
  install -m 755 "${BASH_SOURCE[0]}" /usr/local/sbin/mango-bootstrap.sh
  # Keep the 4G script reachable even if the clone is removed
  if [[ -f "$SCRIPT_DIR/setup-4g-auto.sh" ]]; then
    install -m 755 "$SCRIPT_DIR/setup-4g-auto.sh" /usr/local/sbin/setup-4g-auto.sh
    SCRIPT_DIR_SAVED="$SCRIPT_DIR"
    SCRIPT_DIR="/usr/local/sbin"
    save_state
    SCRIPT_DIR="$SCRIPT_DIR_SAVED"
  fi
  cat > "/etc/systemd/system/$RESUME_UNIT" << EOF
[Unit]
Description=Mango gateway bootstrap phase 2 (4G bring-up + verify, runs once after reboot)
After=multi-user.target network-online.target fix-4g.service ModemManager.service NetworkManager.service
Wants=network-online.target ModemManager.service NetworkManager.service
ConditionPathExists=$ARGS_FILE

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/mango-bootstrap.sh --phase2
TimeoutStartSec=600
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable "$RESUME_UNIT" >/dev/null 2>&1
  log "Resume service enabled: $RESUME_UNIT"
}

# ── Phase 2 (after reboot) ───────────────────────────────────────────────────
modem_present() {
  lsusb 2>/dev/null | grep -qiE '1bc7|2c7c|1e0e|2dee|05c6|2bbc|19d2' && return 0
  ls /dev/ttyUSB* >/dev/null 2>&1 && return 0
  return 1
}

wait_for_modem() {
  step "Waiting for 4G modem to enumerate (up to ~5 min)..."
  local i
  for i in $(seq 1 24); do
    if modem_present; then
      log "Modem detected: $(lsusb | grep -iE '1bc7|2c7c|1e0e|2dee' | head -1)"
      return 0
    fi
    # Power-cycle the mPCIe hat every other attempt (GPIO20/21 = hat power)
    if (( i % 4 == 0 )) && command -v raspi-gpio >/dev/null 2>&1; then
      info "Power-cycling hat (GPIO20/21 low→high)..."
      raspi-gpio set 20 dl 2>/dev/null; raspi-gpio set 21 dl 2>/dev/null
      sleep 5
      raspi-gpio set 20 op dh 2>/dev/null; raspi-gpio set 21 op dh 2>/dev/null
    fi
    sleep 10
  done
  warn "Modem not detected after waiting"
  return 1
}

phase2() {
  load_state
  [[ -f "$CFG" && -x "$BIN" ]] || err "Phase 2 prerequisites missing (config/binary) — run phase 1 first"
  echo ""
  echo -e "${BLUE}═══ Phase 2: post-reboot 4G bring-up + verification ═══${NC}"

  if [[ "$FOUR_G" != "no" ]]; then
    if wait_for_modem; then
      step "Configuring SIM slot + data connection (APN=$APN)..."
      systemctl stop ModemManager >/dev/null 2>&1; sleep 2
      if [[ -f "$SCRIPT_DIR/setup-4g-auto.sh" ]]; then
        bash "$SCRIPT_DIR/setup-4g-auto.sh" --apn "$APN" >>"$LOGFILE" 2>&1 \
          || warn "4G setup reported issues"
      fi
      systemctl enable --now ModemManager >/dev/null 2>&1 || true
      systemctl enable --now NetworkManager >/dev/null 2>&1 || true
      # Wait for the PPP data path to come up (NM activation can take ~30s)
      step "Waiting for 4G data path (ppp0)..."
      for i in $(seq 1 24); do
        ip route 2>/dev/null | grep -q "^default dev ppp0" && { log "ppp0 default route up"; break; }
        sleep 5
      done
      ip route 2>/dev/null | grep -q "^default dev ppp0" \
        || warn "ppp0 route not up yet (fix-4g.service retries on next boot)"
    else
      warn "4G modem not present — skipping data setup (check hat/SIM, fix-4g.service retries each boot)"
    fi
  fi

  step "Starting gateway agent..."
  systemctl daemon-reload
  systemctl enable gateway-agent >/dev/null 2>&1 || true
  systemctl restart gateway-agent
  local i
  for i in $(seq 1 30); do
    curl -sf http://127.0.0.1:8090/health >/dev/null 2>&1 && break
    sleep 1
  done

  verify
  # One-shot resume service disables itself after success
  systemctl disable "$RESUME_UNIT" >/dev/null 2>&1 || true
  rm -f "/etc/systemd/system/$RESUME_UNIT"
  systemctl daemon-reload
  log "Resume service removed (fix-4g.service keeps 4G alive on later boots)"
}

verify() {
  echo ""
  echo -e "${BLUE}══════════════ Verification ══════════════${NC}"
  local pass=0 fail=0 ok
  chk() { # chk "label" cmd...
    local label="$1"; shift
    if "$@" >/dev/null 2>&1; then
      echo -e "  ${GREEN}✓${NC} $label"; pass=$((pass+1))
    else
      echo -e "  ${RED}✗${NC} $label"; fail=$((fail+1))
    fi
  }

  chk "gateway-agent service active" systemctl is-active --quiet gateway-agent
  chk "agent health :8090" curl -sf http://127.0.0.1:8090/health
  chk "gateway-agent enabled at boot" systemctl is-enabled --quiet gateway-agent
  chk "fix-4g enabled at boot" systemctl is-enabled --quiet fix-4g
  chk "ModemManager enabled at boot" systemctl is-enabled --quiet ModemManager

  if [[ "$FOUR_G" != "no" ]]; then
    chk "modem enumerated (USB)" modem_present
    chk "SIM registered (mmcli)" bash -c 'mmcli -m 0 2>/dev/null | grep -q "state.*connected\|state.*registered"'
    chk "ppp0 default route (metric 50)" bash -c 'ip route | grep -q "^default dev ppp0"'
    chk "4G data ping 8.8.8.8" bash -c 'ping -c 3 -W 3 8.8.8.8 2>&1 | grep -q "0% packet loss"'
  fi

  echo ""
  echo -e "${BLUE}═══════════════════════════════════════════════${NC}"
  if [[ $fail -eq 0 ]]; then
    echo -e "${GREEN}  ALL CHECKS PASSED ($pass/$pass) — gateway is ONLINE${NC}"
  else
    echo -e "${YELLOW}  $pass passed, $fail failed — see $LOGFILE / journalctl -u gateway-agent${NC}"
  fi
  echo -e "${BLUE}═══════════════════════════════════════════════${NC}"
  echo ""
  echo "  Logs:     journalctl -u gateway-agent -f"
  echo "  4G:       mmcli -m 0 ; ip route ; ping -I ppp0 1.1.1.1"
  echo "  Config:   $CFG"
  echo "  Health:   curl http://127.0.0.1:8090/health"
  echo ""
}

# ── Main ─────────────────────────────────────────────────────────────────────
main() {
  parse_args "$@"

  if [[ "$PHASE2" -eq 1 ]]; then
    phase2
    return
  fi

  echo -e "${BLUE}"
  echo "╔═══════════════════════════════════════════════════════╗"
  echo "║  Mango IoT Gateway — Full Auto Setup (clone → run)    ║"
  echo "╚═══════════════════════════════════════════════════════╝"
  echo -e "${NC}"

  preflight
  install_deps
  build_agent
  generate_config
  install_service
  setup_4g_phase1
  save_state

  # Local broker unit (agent-managed) if mosquitto exists
  if command -v mosquitto >/dev/null 2>&1; then
    local mosq; mosq="$(command -v mosquitto)"
    mkdir -p /opt/gateway/mqtt
    cat > /etc/systemd/system/mango-local-broker.service << EOF
[Unit]
Description=Mango Local MQTT Broker (agent-managed)
After=network-online.target gateway-agent.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=${mosq} -c /opt/gateway/mqtt/mosquitto.conf
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    log "Local broker unit installed (mango-local-broker)"
  fi

  echo ""
  if [[ "$NEED_REBOOT" -eq 1 && "$NO_REBOOT" -eq 0 ]]; then
    install_resume_unit
    step "Rebooting in 5s to apply 4G hat power config — phase 2 auto-continues after boot..."
    sync; sleep 5
    systemctl reboot
  elif [[ "$NEED_REBOOT" -eq 1 && "$NO_REBOOT" -eq 1 ]]; then
    install_resume_unit
    warn "Reboot required for 4G (hat power config). Run: sudo reboot"
    warn "Phase 2 (4G bring-up + verification) runs automatically after the reboot."
  else
    phase2
  fi
}

main "$@"
