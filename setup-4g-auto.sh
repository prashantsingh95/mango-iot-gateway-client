#!/usr/bin/env bash
#===============================================================================
# IOT-2024G 4G Auto-Setup — Telit LE910C4-CN Custom Hat (CM4)
# One-command 4G bring-up for fresh Pi OS flashes.
#
# What it does:
#   1. Backs up /boot/firmware/config.txt and ensures hat power + UARTs
#   2. Powers the mPCIe hat via GPIO20/21 (and handles W_DISABLE_N)
#   3. Fixes Telit SIM slot (AT#SIMDET=1) if needed
#   4. Creates NetworkManager GSM connection with APN (default: airtelgprs.com)
#   5. Makes 4G the primary default route (metric 50) with autoconnect/retry
#   6. Verifies: lsusb, mmcli, nmcli, ppp0, ping/curl
#
# Usage:
#   sudo bash setup-4g-auto.sh
#   sudo bash setup-4g-auto.sh --apn airtelgprs.com
#   sudo bash setup-4g-auto.sh --apn jionet --reboot
#   sudo bash setup-4g-auto.sh --apn airtelgprs.com --check-only
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
