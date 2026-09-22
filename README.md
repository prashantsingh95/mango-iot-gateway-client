# Mango IoT Gateway Client

**Developed by Prashant Kumar** — Director & Founder, Tech Burst Solutions LLP

**Business Contact:**
- Email: business@techburstsolutions.in, iot.techburst@gmail.com
- Phone/WhatsApp: +91 9310720730
- Web: www.techburstsolutions.in
- Office: New Delhi - 41, India

**Standalone Go agent for Raspberry Pi 3B/4B/5.** Connects to Mango IoT Gateway Platform (or any MQTT broker) for cloud-based management, monitoring, and control of industrial IoT gateways.

Designed for production deployments — static binary, minimal dependencies, systemd-managed lifecycle.

---

## Features

### MQTT Connectivity
- TLS/SSL with mutual authentication
- Auto-reconnect with exponential backoff
- Configurable QoS (0/1/2), keep-alive, clean session
- Multiple broker URL fallback support
- **Production MQTT authentication (fail-closed):** production gateways REQUIRE
  per-gateway `mqtt.username` + `mqtt.password` (provisioned via `provisioning.go`,
  encrypted at rest `0600` via `secrets.go`). Empty production credentials are
  rejected by `ValidateMQTTConfig` — the gateway never connects anonymously in
  production, stays alive queuing offline, and retries provisioning. Anonymous
  MQTT is allowed ONLY in `development` with explicit `mqtt.allow_anonymous: true`
  (local Mosquitto). Production broker must enforce `allow_anonymous false`;
  `clean_session` must be `false` (persistent QoS1). Set `environment: production`
  (default) or `GATEWAY_ENV=production`. Each IOT-2024G gateway receives its own
  credentials — never hardcode or share them.

### Remote Commands
| Command | Description |
|---------|-------------|
| `reboot` | Reboot the gateway (requires sudo) |
| `restart_agent` | Restart the gateway agent process |
| `run_shell` | Execute a shell command (path-restricted) |
| `update_firmware` | Download and apply OTA firmware update |
| `set_relay` | Control GPIO relay output |
| `read_register` | Read a Modbus register value |

### Remote Terminal (Reverse-Connection)
- The agent opens an **outbound TLS WebSocket** to the platform `/agent` namespace — no inbound ports, works behind NAT/CGNAT/firewall.
- Authenticated with gateway id + secret; all traffic HMAC-signed (replay-protected).
- 30s heartbeat with exponential-backoff reconnect, offline detection.
- PTY shell sessions (xterm.js in the browser); SCP-like file upload/download over the same channel.
- Enable via the `terminal:` config block (see Configuration Reference).

### Industrial Protocol Support
- **Modbus TCP** — Connect to Modbus devices over TCP/IP
- **Modbus RTU** — Connect via serial (RS-232/RS-485)
- **Multiple devices** — Concurrent polling of many Modbus slaves
- **Register types** — float32, int16, uint16, uint32, int32, bool, holding
- **GPIO** — Input monitoring and relay output (wiringPi)

### System Monitoring
- CPU usage, load average
- RAM & swap utilization
- Disk usage per partition
- CPU temperature
- Network I/O counters
- Configurable thresholds with warnings

### OTA Firmware Updates
- Download binary via HTTP/HTTPS
- MD5 checksum verification
- Automatic backup of current binary
- Rollback on failure
- Systemd service restart after update

### Watchdog
- MQTT health ping monitoring
- Configurable missed-ping threshold
- Auto-restart agent or reboot gateway on failure

### Logging
- Local file logging with logrotate
- Remote logging to cloud via MQTT
- Structured JSON log entries
- Configurable levels (debug, info, warn, error)

### Provisioning
- Token-based auto-registration with cloud platform via REST API
- Automatic device ID from MAC address
- Serial number detection from `/proc/cpuinfo`
- `platform_url` config field enables auto-registration on startup

### Uptime Tracking
- 15-minute slot-based uptime monitoring
- Platform scheduler aggregates slots from heartbeat data
- Digital signal graph in gateway detail view (green/red timeline)
- Uptime percentage per gateway (24h window)

---

## Requirements

| Component | Requirement |
|-----------|-------------|
| **Hardware** | Raspberry Pi 3B, 3B+, 4B, 5, or Zero 2W |
| **RAM** | 256 MB minimum (512 MB recommended) |
| **Storage** | 200 MB free disk |
| **OS** | Raspberry Pi OS (Debian Bookworm/Bullseye), Ubuntu Server |
| **Network** | Internet access to your cloud MQTT broker (port 1883) |
| **Optional** | I2C enabled for Modbus RTU, SPI for some peripherals |

---

## Architecture

```
┌─────────────────────────────────────────────────┐
│                 Raspberry Pi                     │
│                                                  │
│  ┌──────────────────────────────────────────┐   │
│  │         Gateway Agent (Go)               │   │
│  │                                          │   │
│  │  ┌─────────┐  ┌──────────┐  ┌────────┐  │   │
│  │  │ MQTT    │  │ Modbus   │  │ GPIO   │  │   │
│  │  │ Client  │  │ TCP/RTU  │  │ Reader │  │   │
│  │  └────┬────┘  └────┬─────┘  └───┬────┘  │   │
│  │       │             │            │       │   │
│  │  ┌────┴────┐  ┌────┴─────┐  ┌───┴────┐  │   │
│  │  │ System  │  │ Modbus   │  │ Relay  │  │   │
│  │  │ Monitor │  │ Devices  │  │ Control│  │   │
│  │  └─────────┘  └──────────┘  └────────┘  │   │
│  │  ┌──────────────────────────────────┐   │   │
│  │  │ Terminal Agent (PTY + file xfer) │   │   │
│  │  │ outbound TLS WS → /agent         │   │   │
│  │  └──────────────────┬───────────────┘   │   │
│  └──────────┬───────────┴───────────────────┘   │
│             │  ┌────────────┐                   │
│             │  │  MQTT (TLS) │                   │
│             └─▶└──────┬─────┘                   │
│                ┌─────┴────────┐                  │
│                │   Cloud      │  ◀── Terminal WS │
│                │   Server     │      (/agent)    │
│                │  (Platform)  │                  │
│                └──────────────┘                  │
 └─────────────────────────────────────────────────┘
```

### Runtime (async, non-blocking)

Collection never waits on the network:

- **Async publisher** (`mqtt.go`): telemetry, status, logs and command
  responses enqueue into a buffered FIFO channel (512) and return instantly.
  One sender owns all broker I/O; failures and overflows spill to the durable
  offline spool. Shutdown drains the queue before exit.
- **Bounded command dispatch** (`commands.go`): each command runs in its own
  goroutine (max 4 concurrent) so a 30s shell or firmware download never
  stalls other commands. Dedup marks IDs synchronously, preserving idempotency.
- **Lock-free customer pipeline** (`customer_mqtt.go`): integration configs
  are snapshotted under lock; connects, transforms and publishes happen
  outside it.
- **Per-device Modbus pollers** (`modbus.go`): one goroutine + ticker per
  device instead of a shared spin loop.
- **Bounded subprocesses**: `vcgencmd` (3s), all `iw`/`nmcli`/`systemctl`/`ip`
  calls (15s), firmware downloads (10min + status check), watchdog custom
  actions (30s).
- **Offline queues**: chunked dual-queue storage is primary; the legacy
  SQLite spool opens only as fallback. A 60s flush loop drains both when
  connected, plus a flush on every (re)connect.

---

## Quick Start

### 1. Deploy Platform First (on your server)

```bash
git clone https://github.com/prashantsingh95/mango-iot-gateway-platform.git
cd mango-iot-gateway-platform
sudo bash setup-server.sh
```

After platform setup, login at `http://YOUR_SERVER_IP:3000` and:
1. Go to **Provisioning** page → Create Token → copy it
2. Get MQTT credentials from `/root/.iot-server-credentials`

### 2. Install Gateway Agent on Pi (One Command)

On a fresh Pi OS, only git is needed — clone and run **one command** with all
details (telemetry + provisioning + remote terminal):

```bash
# On the Pi:
sudo apt-get install -y git
git clone https://github.com/prashantsingh95/mango-iot-gateway-client.git
cd mango-iot-gateway-client

# ONE command — everything included:
sudo bash setup.sh \
  --server mqtts://YOUR_BROKER_HOST:8883 \
  --mqtt-user iot \
  --mqtt-pass YOUR_MQTT_PASSWORD \
  --token YOUR_PROVISION_TOKEN \
  --platform-url http://YOUR_SERVER_IP:3001 \
  --device-id factory-gw-01 \
  --name "Factory Gateway #1" \
  --agent-secret ONE_TIME_AGENT_SECRET \
  --signing-pepper YOUR_TERMINAL_SIGNING_PEPPER
# --ws-url defaults to --platform-url with http→ws; --mqtt-ssl auto-detects mqtts://
```

Get the values from the platform **Provisioning page**: it generates this exact
command with your token prefilled (Gateway Client Setup card), plus the
terminal secret issuer (Remote Terminal Setup card).

`--platform-url` must be reachable from the Pi. Do not use `localhost` unless
the platform backend is running on the same Pi. The token is generated from
the platform's **Add Gateway** flow and is valid for the device number entered
there.

To configure an existing installation:

```bash
sudo nano /opt/gateway/config.yml
# Set gateway.platform_url and gateway.provision_token
sudo systemctl restart gateway-agent
sudo journalctl -u gateway-agent -f
```

### 3. Enable Remote Terminal (Reverse-Connection)
The browser terminal does **not** use SSH. This agent opens an **outbound TLS
WebSocket** to the platform's `/agent` Socket.IO namespace — no inbound ports,
works behind NAT/CGNAT/firewall.

**Easiest: pass it in the one-command install** (`--agent-secret` +
`--signing-pepper` in step 2) — `setup.sh` writes the whole `terminal:` block
for you. Manual alternative:

**a) Issue an agent secret** (Admin) from the platform API or UI:
```bash
curl -X POST "$PLATFORM_URL/api/v1/gateways/<GATEWAY_ID>/agent-secret" \
  -H "Authorization: Bearer $TOKEN"
# => { "gatewayId": "...", "secret": "<ONE-TIME SECRET>", "backendUrl": "wss://..." }
```

**b) Configure and enable the terminal agent** in `/opt/gateway/config.yml`:
```yaml
terminal:
  enabled: true
  backend_ws_url: "wss://your-platform.example.com"   # from backendUrl above
  agent_secret: "<ONE-TIME SECRET>"
  signing_pepper: "<MUST MATCH backend TERMINAL_SIGNING_PEPPER>"
  shell: "/bin/bash"
  file_dir: "/tmp"
sudo systemctl restart gateway-agent
```
Once the agent shows connected in the platform gateway view, open the
**Terminal** tab for a multi-tab, resizable shell with file upload/download.

**Identity:** the agent authenticates with the platform **UUID** (`gateway.id`),
not the deviceId. The UUID is captured automatically from the provisioning
response and persisted to `<config-dir>/gateway.id` (0600); explicit
`terminal.gateway_id` wins if set, deviceId is only a fallback. The backend
accepts either form, but the UUID is canonical for relay keys and dashboard
sessions — keep it stable, do not hand-edit the file.

**Secrets:** `agent_secret` is one-time — issuing a new secret from the
platform **invalidates the previous one immediately**. If the agent logs
`Invalid gateway credentials` right after working, someone re-issued the
secret: copy the new value into `terminal.agent_secret` and restart.

**URLs:** `backend_ws_url` (and `platform_url`, MQTT `broker_url`) must be
reachable **from the Pi**. `ws://localhost:3001` on the Pi means the Pi
itself — use the server's LAN/host address, e.g. `ws://10.138.113.194:3001`.
`signing_pepper` must equal the backend's `TERMINAL_SIGNING_PEPPER` exactly
(no trailing spaces).

> The platform also ships a standalone Node reference agent in
> `gateway-agent/` if you prefer not to enable the terminal module here.

---

### Manual Install (Interactive)

```bash
sudo bash setup.sh
```

This will prompt for:
- MQTT broker URL (from your cloud server)
- MQTT username/password
- Provisioning token (from the cloud platform's Provisioning page)

### What the installer does:

1. Installs system dependencies (curl, git, gcc, GPIO libs)
2. Installs Go 1.22, downloads module dependencies
3. Compiles the gateway agent binary (`/usr/local/bin/gateway-agent`)
4. Generates configuration at `/opt/gateway/config.yml`
5. Creates `gateway` system user with GPIO/I2C permissions
6. Installs systemd service (`gateway-agent.service`)
7. Configures log rotation
8. Starts the agent
9. Saves connection info to `/root/.iot-client-credentials`

---

## Post-Install

```bash
# Check agent status
sudo systemctl status gateway-agent

# View live logs
sudo journalctl -u gateway-agent -f

# Or tail the log file
sudo tail -f /var/log/gateway-agent.log

# Edit configuration
sudo nano /opt/gateway/config.yml
sudo systemctl restart gateway-agent

# Check MQTT connection
sudo journalctl -u gateway-agent --since "5 min ago" | grep -i mqtt
```

---

## Configuration Reference

All configuration is in `/opt/gateway/config.yml` (or set `GATEWAY_CONFIG` env var).

```yaml
gateway:
  device_id: "gw-aabbccddee"        # Unique ID (auto from MAC)
  name: "Factory Gateway"           # Human-readable name
  tenant_id: "default"              # Multi-tenant partition
  provision_token: ""               # Token for auto-registration
  platform_url: ""                  # REST API base URL for provisioning

mqtt:
  broker_url: "mqtt://10.0.0.1:1883"
  username: "iot"                   # MQTT username (empty = anonymous)
  password: "secret"                # MQTT password
  client_id_prefix: "gw"            # Client ID prefix
  ssl: false                        # Enable TLS
  ca_cert: ""                       # CA certificate path
  client_cert: ""                   # Client certificate path
  client_key: ""                    # Client key path
  qos: 1                            # MQTT QoS (0, 1, 2)
  keep_alive: 60                    # Keep-alive interval (seconds)
  reconnect_delay: 5                # Initial reconnect delay
  max_reconnect_delay: 60           # Maximum reconnect delay
  topics:
    telemetry: "gateway/{device_id}/telemetry"
    status: "gateway/{device_id}/status"
    log: "gateway/{device_id}/log"
    command: "gateway/{device_id}/command/set"
    response: "gateway/{device_id}/command/response"

modbus:
  enabled: false
  devices:
    - name: "power-meter"
      protocol: "tcp"              # tcp or rtu
      address: "192.168.1.100:502"
      slave_id: 1
      interval: 10                  # Poll interval (seconds)
      registers:
        - name: "voltage"
          address: 0
          quantity: 2
          type: "float32"

gpio:
  enabled: false
  sensors:
    - name: "relay-1"
      pin: 17
      mode: "output"                # input or output
      default: false

monitoring:
  interval: 30                      # Telemetry interval (seconds)
  cpu: true                         # Collect CPU metrics
  memory: true                      # Collect memory metrics
  disk: true                        # Collect disk metrics
  temperature: true                 # Collect CPU temperature
  network: true                     # Collect network I/O

logging:
  level: "info"                     # debug, info, warn, error
  file: "/var/log/gateway-agent.log"
  remote: true                      # Send logs to cloud

ota:
  enabled: true
  firmware_dir: "/opt/gateway/firmware"
  backup_dir: "/opt/gateway/backup"
  auto_rollback: true               # Rollback on failure
  rollback_timeout: 30              # Seconds before rollback

watchdog:
  enabled: true
  interval: 60                      # Check interval (seconds)
  max_missed_pings: 3               # Missed pings before action
  action: "restart"                 # restart, reboot, or custom cmd

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
    allowed_paths:                  # Restricted shell paths
      - "/opt/gateway/scripts/"
      - "/usr/local/bin/"
    timeout: 30                      # Shell command timeout (s)

terminal:                            # Reverse-connection remote terminal agent
  enabled: false
  gateway_id: ""                     # Defaults to gateway.device_id
  backend_ws_url: "ws://localhost:3001"   # wss://... in production
  agent_secret: ""                   # One-time secret from platform /agent-secret
  signing_pepper: ""                 # MUST match backend TERMINAL_SIGNING_PEPPER
  heartbeat_ms: 30000
  reconnect_base_ms: 1000
  reconnect_max_ms: 30000
  shell: "/bin/bash"
  file_dir: "/tmp"                   # Base dir for uploaded files
  insecure_skip_verify: false        # true only for self-signed test backends
```

---

## Remote Commands Reference

Commands are sent by the cloud platform to `gateway/{device_id}/command/set`.

### Reboot
```json
{
  "id": "cmd-001",
  "type": "reboot",
  "payload": {}
}
```

### Restart Agent
```json
{
  "id": "cmd-002",
  "type": "restart_agent",
  "payload": {}
}
```

### Run Shell
```json
{
  "id": "cmd-003",
  "type": "run_shell",
  "payload": {
    "command": "/opt/gateway/scripts/example.sh",
    "args": ["--flag", "value"]
  }
}
```

### Update Firmware
```json
{
  "id": "cmd-004",
  "type": "update_firmware",
  "payload": {
    "url": "https://storage.example.com/firmware/v2.1.0/gateway-agent",
    "checksum": "d41d8cd98f00b204e9800998ecf8427e",
    "version": "2.1.0"
  }
}
```

### Set Relay
```json
{
  "id": "cmd-005",
  "type": "set_relay",
  "payload": {
    "pin": 17,
    "state": true
  }
}
```

### Read Register
```json
{
  "id": "cmd-006",
  "type": "read_register",
  "payload": {
    "device": "power-meter",
    "register": "voltage"
  }
}
```

---

## MQTT Topics

| Topic | Direction | QoS | Retain | Description |
|-------|-----------|-----|--------|-------------|
| `gateway/{id}/telemetry` | → Cloud | 1 | No | Periodic system metrics |
| `gateway/{id}/status` | → Cloud | 1 | Yes | Online/offline + uptime |
| `gateway/{id}/log` | → Cloud | 0 | No | Log entries |
| `gateway/{id}/command/response` | → Cloud | 1 | No | Command execution results (with `success` bool) |
| `gateway/{id}/command/set` | Cloud → | 1 | No | Incoming commands (with `commandId` field) |

---

## Development

### Prerequisites
- Go 1.21+
- Access to a MQTT broker for testing

### Build locally

```bash
git clone https://github.com/prashantsingh95/mango-iot-gateway-client.git
cd gateway-client
go mod download
CGO_ENABLED=0 go build -ldflags="-s -w" -o gateway-agent .
```

### Run with custom config

```bash
GATEWAY_CONFIG=/path/to/config.yml ./gateway-agent
```

### Cross-compile for Pi

```bash
# For Pi 3B (32-bit ARM):
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o gateway-agent-armv7 .

# For Pi 4B/5 (64-bit ARM):
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o gateway-agent-arm64 .
```

---

## Troubleshooting

| Symptom | Check |
|---------|-------|
| Agent won't start | `journalctl -u gateway-agent -n 50` |
| MQTT connection refused | Verify broker URL, username, password, firewall (port 1883) |
| Modbus timeout | Check device IP, port, protocol (TCP vs RTU) |
| GPIO permission denied | `sudo usermod -a -G gpio gateway` then restart |
| Firmware update fails | Check URL reachability, checksum match, disk space |
| Provisioning fails | Verify token is active in cloud platform |
| High CPU usage | Reduce `monitoring.interval` or disable unused modules |
| Terminal shows "Agent offline" | `terminal.enabled: true`, `backend_ws_url` & `agent_secret` set, platform reachable over WS; check agent logs |
| Terminal "Invalid gateway credentials" | `agent_secret` mismatch or not issued via platform `/agent-secret`. **Re-issuing invalidates the old secret** — update the agent right away. Verify the hash matches: `sha256(secret)` must equal the gateway's `agentSecretHash`. Backend accepts deviceId or UUID since v1 (canonical UUID preferred). |
| Terminal "Message signature invalid" | `signing_pepper` on agent ≠ backend `TERMINAL_SIGNING_PEPPER` (must match, no trailing whitespace) |
| Terminal UUID / gateway.id | Auto-written at provisioning next to the config; delete it only to force re-capture on next successful provision |

---

## Uninstall

```bash
sudo systemctl stop gateway-agent
sudo systemctl disable gateway-agent
sudo rm /usr/local/bin/gateway-agent
sudo rm -r /opt/gateway
sudo rm /etc/systemd/system/gateway-agent.service
sudo rm /etc/logrotate.d/gateway-agent
sudo systemctl daemon-reload
```

---

## Project Structure

```
mango-iot-gateway-client/
├── main.go              # Entry point, globals, signal handling
├── config.go            # Configuration structs + loader
├── telemetry.go         # Telemetry/status data, system metrics collection
├── mqtt.go              # MQTT connect, async publisher, spool flush
├── commands.go          # Remote command handling (reboot, shell, firmware)
├── provisioning.go      # Token auto-registration, UUID + secret persistence
├── firmware.go          # OTA firmware download helper (bounded)
├── modbus.go            # Modbus TCP/RTU per-device pollers
├── gpio.go              # GPIO sensor/relay handling
├── secrets.go           # AES-GCM secrets encryption
├── health.go            # HTTP health check server
├── state.go             # Agent state
├── watchdog.go          # MQTT health ping watchdog
├── helpers.go           # Device ID, serial, MAC, IP utilities
├── identity.go          # Stable device identity pinning
├── protocol.go          # Terminal HMAC signing (must match backend)
├── terminal.go          # Reverse-connection PTY agent (/agent namespace)
├── cloudflare_tunnel.go # Cloudflare Zero-Trust tunnel (alternative shell)
├── customer_mqtt.go     # External MQTT integration fan-out
├── integration.go       # Field mapping / templates for integrations
├── integration_poller.go# Polls platform for integration configs
├── offline_storage.go   # Chunked dual-queue offline storage
├── queue.go             # Legacy SQLite spool (fallback only)
├── ota.go               # OTA apply, signature verify, rollback gate
├── wifi_ap.go           # Remote Wi-Fi AP management (bounded subprocess)
├── go.mod / go.sum      # Go module dependencies
├── config.yml           # Configuration template
├── setup.sh             # One-command Pi installer
├── configure.sh         # Interactive configuration helper
└── README.md            # This file
```

---

## License

MIT — see [LICENSE](LICENSE)
