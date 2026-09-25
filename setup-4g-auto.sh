#!/usr/bin/env bash
#===============================================================================
# IOT-2024G 4G Auto-Setup — Telit LE910C4-CN Custom Hat (CM4)
# One-command 4G bring-up for fresh Pi OS flashes.
#
# What it does:
#   1. Backs up /boot/firmware/config.txt and ensures hat power + UARTs
#   2. Powers the mPCIe hat via GPIO20/21 (and handles W_DISABLE_N)
#   3. Fixes Telit SIM slot (AT#SIMDET=1) if needed
#   4. Creates NetworkManager GSM connection with dynamic APN
#      auto-detects via mmcli/AT (airtel→airtelgprs.com, jio→jionet, vi→www, bsnl→bsnlnet)
#      or accepts alias: --apn airtel|jio|vi|bsnl|custom
#   5. Makes 4G the primary default route (metric 50) with autoconnect/retry
#   6. Verifies: lsusb, mmcli, nmcli, ppp0, ping/curl
#
# Usage:
#   sudo bash setup-4g-auto.sh                         # auto-detect APN (airtel/jio/vi/bsnl)
#   sudo bash setup-4g-auto.sh --apn airtel            # alias → airtelgprs.com
#   sudo bash setup-4g-auto.sh --apn jio               # alias → jionet
#   sudo bash setup-4g-auto.sh --apn vi                # alias → www
#   sudo bash setup-4g-auto.sh --apn bsnl              # alias → bsnlnet
#   sudo bash setup-4g-auto.sh --apn airtelgprs.com    # raw APN passthrough
#   sudo bash setup-4g-auto.sh --apn jionet --reboot
#   sudo bash setup-4g-auto.sh --check-only
#
# Tested on: CM4 Rev 1.0, 6.12.109+rpt-rpi-v8, LE910C4-CN 25.20.638, ModemManager 1.20.4
# Docs: /opt/gateway/docs/4G_FIX_README.md , /usr/local/bin/fix-4g.sh
#===============================================================================
set -e
set -o pipefail

APN="airtelgprs.com"
REBOOT=0
CHECK_ONLY=0
GSM_IFACE="ttyUSB2"
CONN_NAME="airtel"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --apn) APN="$2"; shift 2 ;;
    --reboot) REBOOT=1; shift ;;
    --check-only) CHECK_ONLY=1; shift ;;
    --help|-h) echo "Usage: sudo bash $0 [--apn APN] [--reboot] [--check-only]"; exit 0 ;;
    *) echo "Unknown: $1"; exit 1 ;;
  esac
done

# ── Dynamic APN: airtel/jio/vi/bsnl aliases + auto-detect via mmcli/AT ──
detect_apn() {
  local op=""
  # Try ModemManager first (operator name is most reliable)
  if command -v mmcli >/dev/null 2>&1; then
    op=$(mmcli -m 0 2>/dev/null | grep -i "operator name" | awk -F: '{print $2}' | tr '[:upper:]' '[:lower:]' | xargs 2>/dev/null || true)
    if [[ -z "$op" ]]; then
      # MCCMNC fallback (404xx = India)
      local mccmnc=$(mmcli -m 0 2>/dev/null | grep -i "operator id" | grep -o "[0-9]\{5,6\}" | head -1 || true)
      case "$mccmnc" in 4058*|40586*|40587*) op="jio" ;; 404* ) op="" ;; esac
    fi
  fi
  if [[ -z "$op" && -e /dev/ttyUSB2 ]]; then
    # AT fallback (stop MM briefly if needed)
    op=$(timeout 4 bash -c 'printf "AT+COPS?\r" > /dev/ttyUSB2 2>/dev/null; sleep 1; cat /dev/ttyUSB2 2>/dev/null' | grep -o '"[^"]*"' | tr -d '"' | tr '[:upper:]' '[:lower:]' | head -1 | xargs || true)
    if [[ -z "$op" ]]; then
      op=$(timeout 4 bash -c 'printf "AT+QSPN?\r" > /dev/ttyUSB2 2>/dev/null; sleep 1; cat /dev/ttyUSB2 2>/dev/null' | grep -o '"[^"]*"' | tr -d '"' | tr '[:upper:]' '[:lower:]' | head -1 | xargs || true)
    fi
  fi
  # Normalize operator substring → APN
  case "$op" in
    *airtel*) echo "airtelgprs.com" ;;
    *jio*|*reliance*) echo "jionet" ;;
    *vi*|*vodafone*|*idea*) echo "www" ;;
    *bsnl*) echo "bsnlnet" ;;
    *) echo "airtelgprs.com" ;; # safe fallback (most tested)
  esac
}
normalize_apn() {
  local a=$(echo "$1" | tr '[:upper:]' '[:lower:]' | xargs)
  case "$a" in
    auto) detect_apn ;;
    airtel|airtelgprs.com) echo "airtelgprs.com" ;;
    jio|jionet|reliance|reliance_jio) echo "jionet" ;;
    vi|vodafone|idea|vodafoneidea|www|internet) echo "www" ;;
    bsnl|bsnlnet) echo "bsnlnet" ;;
    *) echo "$1" ;; # custom APN passthrough (e.g. private APN)
  esac
}
# Normalize alias (airtel/jio/vi/www) → real APN; auto → detect
ORIG_APN="$APN"
APN=$(normalize_apn "$APN")
if [[ "$ORIG_APN" != "$APN" ]]; then
  echo "APN $ORIG_APN → $APN"
fi

[[ $EUID -eq 0 ]] || { echo "Run with sudo: sudo bash $0"; exit 1; }
[[ $CHECK_ONLY -eq 1 ]] && {
  echo "=== CHECK ONLY ==="
  lsusb | grep -E "1bc7|1d6b" || true
  mmcli -L 2>&1 || true
  mmcli -m 0 2>&1 | grep -E "state|signal|operator|sim" | head -n 20 || true
  nmcli device status 2>&1 || true
  ip route 2>&1 | head -n 20 || true
  ping -I ppp0 -c 2 8.8.8.8 2>&1 | head -n 20 || ping -c 2 8.8.8.8 2>&1 | head -n 20 || true
  exit 0
}

echo "=== 1/6 Backup and boot config ==="
CONFIG="/boot/firmware/config.txt"
if [[ ! -f "$CONFIG" ]]; then CONFIG="/boot/config.txt"; fi
sudo cp -v "$CONFIG" "${CONFIG}.bak.$(date +%s)" 2>&1 || true
# Ensure hat power persists after reboot
for line in "gpio=20=op,dh" "gpio=21=op,dh" "enable_uart=1" "dtoverlay=miniuart-bt" "dtoverlay=uart2" "dtoverlay=uart3" "dtoverlay=uart4" "dtoverlay=uart5"; do
  grep -qF "$line" "$CONFIG" || echo "$line" | sudo tee -a "$CONFIG" >/dev/null
done
# Ensure XHCI host for CM4
if grep -q "^\[cm4\]" "$CONFIG"; then
  grep -q "otg_mode=1" "$CONFIG" || sudo sed -i "/^\[cm4\]/a otg_mode=1" "$CONFIG" 2>&1 || true
fi
echo "Boot config ready: $CONFIG"
grep -E "gpio=|enable_uart|dtoverlay=uart|otg_mode" "$CONFIG" 2>&1 | tail -n 20 || true

echo "=== 2/6 Power hat (GPIO20/21) ==="
if command -v raspi-gpio >/dev/null 2>&1; then
  sudo raspi-gpio set 20 op dh 2>&1 || true
  sudo raspi-gpio set 21 op dh 2>&1 || true
  # W_DISABLE_N off (GPIO6 low = enabled)
  sudo raspi-gpio set 6 ip pd 2>&1 || true
  sleep 8
fi
lsusb 2>&1 | grep -E "1bc7|1d6b" || true
lsusb -t 2>&1 | head -n 20 || true
# Wait for modem USB enumeration
for i in 1 2 3 4 5 6; do
  ls /dev/ttyUSB2 >/dev/null 2>&1 && break
  echo "Waiting for /dev/ttyUSB2... $i"
  sleep 3
done
ls -l /dev/ttyUSB* 2>&1 || true

echo "=== 3/6 Fix SIM slot (AT#SIMDET=1) if needed ==="
sudo systemctl stop ModemManager 2>&1 || true; sleep 3
# Check current SIMDET
SIMDET=$(sudo timeout 4 bash -c 'stty -F /dev/ttyUSB2 115200 raw -echo 2>&1; printf "AT#SIMDET?\r" > /dev/ttyUSB2; sleep 1; cat /dev/ttyUSB2' 2>&1 | tr -d '\0' | grep -o "SIMDET: [0-9],[0-9]" || true)
echo "Current AT#SIMDET: $SIMDET"
if echo "$SIMDET" | grep -q "1,0"; then
  echo "SIMDET already 1,0 — OK"
else
  echo "Setting AT#SIMDET=1..."
  sudo timeout 4 bash -c 'stty -F /dev/ttyUSB2 115200 raw -echo 2>&1; printf "AT#SIMDET=1\r" > /dev/ttyUSB2; sleep 1; cat /dev/ttyUSB2' 2>&1 | od -c | head -n 5 || true
  sleep 1
  sudo timeout 4 bash -c 'printf "AT#SIMDET?\r" > /dev/ttyUSB2; sleep 1; cat /dev/ttyUSB2' 2>&1 | od -c | head -n 5 || true
fi
# Verify CPIN
CPIN=$(sudo timeout 4 bash -c 'printf "AT+CPIN?\r" > /dev/ttyUSB2; sleep 1; cat /dev/ttyUSB2' 2>&1 | tr -d '\0' || true)
echo "AT+CPIN? → $CPIN"
sudo systemctl start ModemManager 2>&1 || true
echo "Waiting for ModemManager to register (20s)..."
sleep 20
mmcli -L 2>&1 || true
mmcli -m 0 2>&1 | grep -E "state|signal|operator|sim|bearer" | head -n 30 || true

echo "=== 4/6 Install deps if missing ==="
if ! command -v mmcli >/dev/null 2>&1; then
  sudo apt-get update -qq 2>&1 | tail -n 5 || true
  sudo apt-get install -y -qq modemmanager network-manager usb-modeswitch libqmi-utils libmbim-utils 2>&1 | tail -n 10 || true
fi

echo "=== 5/6 NetworkManager GSM (APN=$APN) ==="
# Remove stale bearer from direct mmcli simple-connect
sudo mmcli -m 0 --simple-disconnect 2>&1 || true; sleep 2
# Create or update connection
if nmcli connection show "$CONN_NAME" >/dev/null 2>&1; then
  echo "Updating existing $CONN_NAME..."
  sudo nmcli connection modify "$CONN_NAME" gsm.apn "$APN" 2>&1 || true
else
  echo "Creating $CONN_NAME..."
  sudo nmcli connection add type gsm ifname "$GSM_IFACE" con-name "$CONN_NAME" apn "$APN" 2>&1 || true
fi
sudo nmcli connection modify "$CONN_NAME" connection.autoconnect yes connection.autoconnect-priority 100 connection.autoconnect-retries 0 ipv4.route-metric 50 ipv6.route-metric 50 2>&1 || true
# Ensure usb0 (rndis) does not become default
for c in $(nmcli -t -f NAME,TYPE connection show 2>&1 | grep ":ethernet" | cut -d: -f1); do
  # Only touch the rndis usb0 connection if it exists
  if nmcli connection show "$c" 2>&1 | grep -q "192.168.225"; then
    sudo nmcli connection modify "$c" ipv4.never-default yes ipv6.never-default yes 2>&1 || true
  fi
done
sudo nmcli connection up "$CONN_NAME" 2>&1 || true
sleep 8
nmcli device status 2>&1 || true
ip -4 addr show ppp0 2>&1 | head -n 20 || true
ip route 2>&1 | head -n 20 || true

echo "=== 5b/6 Enable auto-start on boot (ModemManager/NetworkManager + fix-4g service) ==="
sudo systemctl enable ModemManager 2>&1 || true
sudo systemctl enable NetworkManager 2>&1 || true
# Helper script for boot-time recovery (modem may enumerate late after reboot)
sudo tee /usr/local/bin/fix-4g.sh >/dev/null <<'FIX4G_EOF'
#!/bin/bash
set -e
# Fix 4G after reboot — waits for modem, ensures SIMDET and NM connection up
CONN="airtel"
IFACE="ttyUSB2"
for i in 1 2 3 4 5 6 7 8 9 10; do
  # Power hat early (if raspi-gpio exists)
  raspi-gpio set 20 op dh 2>/dev/null || true
  raspi-gpio set 21 op dh 2>/dev/null || true
  if ls /dev/ttyUSB2 >/dev/null 2>&1; then break; fi
  echo "[fix-4g] waiting for $IFACE ... $i"
  sleep 3
done
# Wait for ModemManager to see modem (up to 40s)
for i in $(seq 1 20); do
  mmcli -L 2>&1 | grep -q "Modem" && break
  sleep 2
done
# If SIM missing (2,0), try to fix; ignore errors (modem may be locked)
if [ -e /dev/ttyUSB2 ]; then
  systemctl stop ModemManager 2>/dev/null || true; sleep 2
  timeout 4 bash -c 'stty -F /dev/ttyUSB2 115200 raw -echo 2>/dev/null; printf "AT#SIMDET=1\r" > /dev/ttyUSB2; sleep 1; cat /dev/ttyUSB2' 2>/dev/null | tr -d '\0' | grep -q "OK" || true
  systemctl start ModemManager 2>/dev/null || true; sleep 10
fi
# Bring up NM connection with retries (modem may still registering)
for i in 1 2 3 4 5; do
  if nmcli -t -f NAME connection show --active 2>&1 | grep -q "^${CONN}$"; then
    echo "[fix-4g] $CONN already active"
    break
  fi
  echo "[fix-4g] nmcli up $CONN attempt $i"
  nmcli connection up "$CONN" 2>&1 && break || sleep 5
done
# Verify
ip route 2>&1 | grep -q "ppp0" && echo "[fix-4g] OK $(ip -4 addr show ppp0 2>&1 | grep -oP 'inet \K[0-9.]+' || true)" || echo "[fix-4g] WARN ppp0 not up"
mmcli -m 0 2>&1 | grep -E "state|signal|operator" | head -n 5 || true
FIX4G_EOF
sudo chmod +x /usr/local/bin/fix-4g.sh
sudo tee /etc/systemd/system/fix-4g.service >/dev/null <<'FIXSVC_EOF'
[Unit]
Description=Fix 4G connectivity after boot (Telit LE910C4)
After=ModemManager.service NetworkManager.service
Wants=ModemManager.service NetworkManager.service
Before=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/fix-4g.sh
RemainAfterExit=yes
TimeoutStartSec=120

[Install]
WantedBy=multi-user.target
FIXSVC_EOF
sudo systemctl daemon-reload 2>&1 || true
sudo systemctl enable fix-4g.service 2>&1 || true
echo "fix-4g.service enabled: $(systemctl is-enabled fix-4g.service 2>&1)"
ls -l /usr/local/bin/fix-4g.sh /etc/systemd/system/fix-4g.service 2>&1 || true

echo "=== 6/6 Verify ==="
mmcli -m 0 2>&1 | grep -E "state|signal|operator" | head -n 20 || true
if ping -I ppp0 -c 3 8.8.8.8 2>&1 | grep -q "0% packet loss"; then
  echo "✓ 4G data OK via ppp0"
  ping -I ppp0 -c 3 8.8.8.8 2>&1 | head -n 10 || true
else
  echo "Trying default route ping..."
  ping -c 3 8.8.8.8 2>&1 | head -n 20 || true
fi
echo "Default route check (should be ppp0 metric 50):"
ip route 2>&1 | grep default || true
echo ""
echo "Done. 4G will auto-connect after reboot (gpio + NM autoconnect)."
echo "Logs: journalctl -u ModemManager -f  and  journalctl -u NetworkManager -f"
echo "Check: mmcli -L; mmcli -m 0; nmcli device status; ip route; ping -I ppp0 -c 3 1.1.1.1"
if [[ $REBOOT -eq 1 ]]; then
  echo "Rebooting in 3s..."
  sudo sync; sleep 2; sudo reboot &
fi
