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

### Remote Commands
| Command | Description |
|---------|-------------|
| `reboot` | Reboot the gateway (requires sudo) |
| `restart_agent` | Restart the gateway agent process |
| `run_shell` | Execute a shell command (path-restricted) |
| `update_firmware` | Download and apply OTA firmware update |
| `set_relay` | Control GPIO relay output |
| `read_register` | Read a Modbus register value |

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
│  └──────────────────┬───────────────────────┘   │
│                     │                            │
│                     ▼ MQTT (TLS)                 │
│              ┌──────────────┐                    │
│              │   Cloud      │                    │
│              │   Server     │                    │
│              │  (Platform)  │                    │
│              └──────────────┘                    │
└─────────────────────────────────────────────────┘
```

---

## Quick Start

### 1. Deploy Platform First (on your server)

```bash
git clone https://github.com/mango-iot/gateway-platform.git
cd mango-iot-gateway-platform
sudo bash setup-server.sh
```

After platform setup, login at `http://YOUR_SERVER_IP:3000` and:
1. Go to **Provisioning** page → Create Token → copy it
2. Get MQTT credentials from `/root/.iot-server-credentials`

### 2. Install Gateway Agent on Pi (One Command)

```bash
# From your machine:
scp -r gateway-client pi@YOUR_PI_IP:~/
ssh pi@YOUR_PI_IP
cd gateway-client

# Fully automated install with all params
sudo bash setup.sh \
  --server mqtt://YOUR_SERVER_IP:1883 \
  --mqtt-user iot \
  --mqtt-pass YOUR_MQTT_PASSWORD \
  --token YOUR_PROVISION_TOKEN \
  --device-id factory-gw-01 \
  --name "Factory Gateway #1"
```

### 3. Enable Terminal Access (SSH)
In platform UI: **Settings** → add:
- `sshUsername`: your Pi username (e.g., `pi`)
- `sshPassword`: your Pi SSH password
- `sshPort`: 22

Now you can open **Terminal** tab in gateway detail view!

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
    timeout: 30                     # Shell command timeout (s)
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
git clone https://github.com/mango-iot/gateway-client.git
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
├── mqtt.go              # MQTT client connect, publish, subscribe
├── commands.go          # Remote command handling (reboot, shell, firmware)
├── provisioning.go      # Token-based auto-registration
├── firmware.go          # OTA firmware download helper
├── modbus.go            # Modbus TCP/RTU collector
├── gpio.go              # GPIO sensor/relay handling
├── secrets.go           # AES-GCM secrets encryption
├── health.go            # HTTP health check server
├── state.go             # Agent state + telemetry buffer
├── watchdog.go          # MQTT health ping watchdog
├── helpers.go           # Device ID, serial, MAC, IP utilities
├── go.mod / go.sum      # Go module dependencies
├── config.yml           # Configuration template
├── setup.sh             # One-command Pi installer
├── configure.sh         # Interactive configuration helper
└── README.md            # This file
```

---

## License

MIT — see [LICENSE](LICENSE)
