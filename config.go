package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// ---------- Configuration ----------

type MQTTTopicConfig struct {
	Telemetry string `yaml:"telemetry"`
	Status    string `yaml:"status"`
	Log       string `yaml:"log"`
	Command   string `yaml:"command"`
	Response  string `yaml:"response"`
}

type MQTTConfig struct {
	BrokerURL         string          `yaml:"broker_url"`
	Username          string          `yaml:"username"`
	Password          string          `yaml:"password"`
	ClientIDPrefix    string          `yaml:"client_id_prefix"`
	SSL               bool            `yaml:"ssl"`
	CACert            string          `yaml:"ca_cert"`
	ClientCert        string          `yaml:"client_cert"`
	ClientKey         string          `yaml:"client_key"`
	QoS               byte            `yaml:"qos"`
	KeepAlive         int             `yaml:"keep_alive"`
	CleanSession      bool            `yaml:"clean_session"`
	ReconnectDelay    int             `yaml:"reconnect_delay"`
	MaxReconnectDelay int             `yaml:"max_reconnect_delay"`
	Topics            MQTTTopicConfig `yaml:"topics"`
	AllowAnonymous    bool            `yaml:"allow_anonymous"` // explicit dev only; production NEVER allows true
}

type ModbusRegister struct {
	Name     string `yaml:"name"`
	Address  uint16 `yaml:"address"`
	Quantity uint16 `yaml:"quantity"`
	Type     string `yaml:"type"`
}

type ModbusDevice struct {
	Name      string           `yaml:"name"`
	Protocol  string           `yaml:"protocol"`
	Address   string           `yaml:"address"`
	SlaveID   byte             `yaml:"slave_id"`
	BaudRate  int              `yaml:"baud_rate"`
	DataBits  int              `yaml:"data_bits"`
	StopBits  int              `yaml:"stop_bits"`
	Parity    string           `yaml:"parity"`
	Interval  int              `yaml:"interval"`
	Registers []ModbusRegister `yaml:"registers"`
}

type ModbusConfig struct {
	Enabled bool           `yaml:"enabled"`
	Devices []ModbusDevice `yaml:"devices"`
}

type GPIOSensor struct {
	Name     string `yaml:"name"`
	Pin      int    `yaml:"pin"`
	Mode     string `yaml:"mode"`
	Pull     string `yaml:"pull"`
	Interval int    `yaml:"interval"`
	Default  bool   `yaml:"default"`
}

type GPIOConfig struct {
	Enabled bool         `yaml:"enabled"`
	Sensors []GPIOSensor `yaml:"sensors"`
}

type MonitorConfig struct {
	Interval            int  `yaml:"interval"`
	CPU                 bool `yaml:"cpu"`
	Memory              bool `yaml:"memory"`
	Disk                bool `yaml:"disk"`
	Temperature         bool `yaml:"temperature"`
	Network             bool `yaml:"network"`
	DiskThresholdWarn   int  `yaml:"disk_threshold_warn"`
	MemoryThresholdWarn int  `yaml:"memory_threshold_warn"`
	CPUThresholdWarn    int  `yaml:"cpu_threshold_warn"`
	TempThresholdWarn   int  `yaml:"temp_threshold_warn"`
}

type LogConfig struct {
	Level      string `yaml:"level"`
	File       string `yaml:"file"`
	MaxSize    int    `yaml:"max_size"`
	MaxBackups int    `yaml:"max_backups"`
	Remote     bool   `yaml:"remote"`
}

type OTAConfig struct {
	Enabled         bool   `yaml:"enabled"`
	FirmwareDir     string `yaml:"firmware_dir"`
	BackupDir       string `yaml:"backup_dir"`
	AutoRollback    bool   `yaml:"auto_rollback"`
	RollbackTimeout int    `yaml:"rollback_timeout"`
	// Hex-encoded ed25519 public key. When set, firmware artifacts MUST carry
	// a valid `signature` over the raw binary or the update is rejected
	// (Phase 5 / §19). Empty = checksum-only with a loud warning (legacy).
	SigningKey string `yaml:"signing_key"`
}

type WatchdogConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Interval       int    `yaml:"interval"`
	MaxMissedPings int    `yaml:"max_missed_pings"`
	Action         string `yaml:"action"`
}

type ShellCommandConfig struct {
	AllowedPaths []string `yaml:"allowed_paths"`
	Timeout      int      `yaml:"timeout"`
}

type CommandsConfig struct {
	Enabled bool               `yaml:"enabled"`
	Allowed []string           `yaml:"allowed"`
	Shell   ShellCommandConfig `yaml:"shell"`
}

// WifiAPConfig controls the host networking integration used by Wi-Fi AP
// commands. The client detects NetworkManager or hostapd at runtime.
type WifiAPConfig struct {
	Enabled       bool   `yaml:"enabled"`
	Interface     string `yaml:"interface"`
	Connection    string `yaml:"connection"`
	HostapdConfig string `yaml:"hostapd_config"`
	MaxClients    int    `yaml:"max_clients"`
}

// LocalBrokerUser is one meter/client credential for the on-gateway broker.
// Passwords are encrypted at rest by the secrets manager (enc:...).
type LocalBrokerUser struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// LocalBrokerConfig controls the on-gateway Mosquitto broker (meters over
// gateway Wi-Fi/LAN). Independent from the remote HiveMQ client: disabling
// the local broker never affects gateway telemetry.
type LocalBrokerConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Mode      string `yaml:"mode"` // unsecured|secure|both (disabled when Enabled=false)
	Bind      string `yaml:"bind"` // meter-facing listen address (default 0.0.0.0 = all LAN/Wi-Fi)
	PortPlain int    `yaml:"port_unsecured"`
	PortTLS   int    `yaml:"port_secure"`
	// AllowAnonymousPlain permits passwordless 1883 (LAN-only). Secure listener
	// NEVER allows anonymous.
	AllowAnonymousPlain bool              `yaml:"allow_anonymous_unsecured"`
	Users               []LocalBrokerUser `yaml:"users"`
	MaxConnections      int               `yaml:"max_connections"`
	MaxPayloadBytes     int               `yaml:"max_payload_bytes"`
	CertDir             string            `yaml:"cert_dir"`  // default <config-dir>/mqtt/certs
	ConfPath            string            `yaml:"conf_path"` // default <config-dir>/mqtt/mosquitto.conf
	DataDir             string            `yaml:"data_dir"`  // mosquitto persistence dir
	Service             string            `yaml:"service"`   // systemd unit (default mosquitto)
}

// LocalClientConfig controls the on-gateway MQTT client that ingests meter
// data from the local broker (127.0.0.1), validates it and queues it locally.
type LocalClientConfig struct {
	Enabled         bool     `yaml:"enabled"`
	Username        string   `yaml:"username"` // auto-provisioned into broker passwd
	Password        string   `yaml:"password"` // enc:... at rest
	Topics          []string `yaml:"topics"`   // default ["meter/#"]
	MaxPayloadBytes int      `yaml:"max_payload_bytes"`
}

// TerminalConfig enables the reverse-connection remote terminal agent.
// The agent dials OUT to the backend's Socket.IO /agent namespace (no inbound
// ports). gateway_id defaults to the device id if unset.
type TerminalConfig struct {
	Enabled            bool     `yaml:"enabled"`
	GatewayID          string   `yaml:"gateway_id"`
	BackendWSURL       string   `yaml:"backend_ws_url"` // ws://host:3001 or wss://...
	AgentSecret        string   `yaml:"agent_secret"`   // issued by the platform
	SigningPepper      string   `yaml:"signing_pepper"` // MUST match backend TERMINAL_SIGNING_PEPPER
	HeartbeatMs        int      `yaml:"heartbeat_ms"`
	ReconnectBaseMs    int      `yaml:"reconnect_base_ms"`
	ReconnectMaxMs     int      `yaml:"reconnect_max_ms"`
	Shell              string   `yaml:"shell"`
	ShellAllowlist     []string `yaml:"shell_allowlist"`      // exact shell binaries permitted (default: Shell only)
	FileDir            string   `yaml:"file_dir"`             // base dir for uploads AND downloads (jail, defaults /tmp)
	MaxFileBytes       int64    `yaml:"max_file_bytes"`       // per-transfer cap (default 25MB)
	IdleTimeoutMinutes int      `yaml:"idle_timeout_minutes"` // kill idle PTY sessions (default 30, 0 disables)
	MaxSessionHours    int      `yaml:"max_session_hours"`    // absolute session lifetime (default 8, 0 disables)
	MaxSessions        int      `yaml:"max_sessions"`         // concurrent PTY cap (default 5)
	InsecureSkipVerify bool     `yaml:"insecure_skip_verify"`
}

// QueueConfig bounds the persistent offline spool (Phase 5 / §20).
// The spool is a core reliability path: always on unless max_events is 0.
type QueueConfig struct {
	Path       string `yaml:"path"`        // SQLite file (default <config-dir>/spool.db)
	MaxEvents  int    `yaml:"max_events"`  // row cap, oldest low-priority evicted first (0 disables spool)
	TTLHours   int    `yaml:"ttl_hours"`   // event age cap
	MaxMB      int    `yaml:"max_mb"`      // payload byte cap
	FlushBatch int    `yaml:"flush_batch"` // events per reconnect flush cycle
}

// CloudflareTunnelConfig configures the Cloudflare Zero Trust Tunnel
// for secure SSH access (Phase 4 / Cloudflare Zero Trust).
type CloudflareTunnelConfig struct {
	Enabled            bool   `yaml:"enabled"`
	GatewayID          string `yaml:"gateway_id"`     // defaults to gateway.device_id
	BackendWSURL       string `yaml:"backend_ws_url"` // wss://host:3001 or wss://...
	AgentSecret        string `yaml:"agent_secret"`   // issued by the platform (tunnel token, 0600 file)
	TunnelID           string `yaml:"tunnel_id"`      // Cloudflare tunnel ID (for credentials-file)
	AccountTag         string `yaml:"account_tag"`    // Cloudflare account tag (for credentials-file)
	SigningPepper      string `yaml:"signing_pepper"` // MUST match backend TERMINAL_SIGNING_PEPPER
	HeartbeatMs        int    `yaml:"heartbeat_ms"`
	ReconnectBaseMs    int    `yaml:"reconnect_base_ms"`
	ReconnectMaxMs     int    `yaml:"reconnect_max_ms"`
	Shell              string `yaml:"shell"`
	FileDir            string `yaml:"file_dir"`             // base dir for uploads (jail, defaults /tmp)
	MaxFileBytes       int64  `yaml:"max_file_bytes"`       // per-transfer cap (default 25MB)
	IdleTimeoutMinutes int    `yaml:"idle_timeout_minutes"` // kill idle PTY sessions (default 30, 0 disables)
	MaxSessionHours    int    `yaml:"max_session_hours"`    // absolute session lifetime (default 8, 0 disables)
	MaxSessions        int    `yaml:"max_sessions"`         // concurrent PTY cap (default 5)
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"` // TEST ONLY. Never true in production.
}

type GatewayConfig struct {
	DeviceID       string `yaml:"device_id"`
	Name           string `yaml:"name"`
	SerialNumber   string `yaml:"serial_number"`
	TenantID       string `yaml:"tenant_id"`
	ProvisionToken string `yaml:"provision_token"`
	PlatformURL    string `yaml:"platform_url"`
	OfflinePath    string `yaml:"offline_path"` // override for offline storage base path (default /data/offline on Linux)
}

// BrandingConfig carries tenant/OEM identity (SaaS §22). Configuration-driven —
// the same binary serves Mango, ACME, or any OEM tenant. Delivered via
// provisioning/cloud config; the gateway must never change tenantId locally.
type BrandingConfig struct {
	ProductName  string `yaml:"product_name"` // e.g. "ACME IoT Gateway" (default "Mango Gateway")
	Manufacturer string `yaml:"manufacturer"` // e.g. "ACME Inc."
	SupportURL   string `yaml:"support_url"`
	SupportEmail string `yaml:"support_email"`
	DeviceLabel  string `yaml:"device_label"`
	DeviceModel  string `yaml:"device_model"`
}

type Config struct {
	Environment string                 `yaml:"environment"` // production | development | test (default: production)
	Gateway     GatewayConfig          `yaml:"gateway"`
	Branding    BrandingConfig         `yaml:"branding"`
	Queue       QueueConfig            `yaml:"queue"`
	Cloudflare  CloudflareTunnelConfig `yaml:"cloudflare_tunnel"`
	MQTT        MQTTConfig             `yaml:"mqtt"`
	Modbus      ModbusConfig           `yaml:"modbus"`
	GPIO        GPIOConfig             `yaml:"gpio"`
	Monitoring  MonitorConfig          `yaml:"monitoring"`
	Logging     LogConfig              `yaml:"logging"`
	OTA         OTAConfig              `yaml:"ota"`
	Watchdog    WatchdogConfig         `yaml:"watchdog"`
	Commands    CommandsConfig         `yaml:"commands"`
	WifiAP      WifiAPConfig           `yaml:"wifi_ap"`
	Terminal    TerminalConfig         `yaml:"terminal"`
	LocalBroker LocalBrokerConfig      `yaml:"local_broker"`
	LocalClient LocalClientConfig      `yaml:"local_client"`
	Forwarding  ForwardingConfig       `yaml:"forwarding"`
}

func configPath() string {
	if p := os.Getenv("GATEWAY_CONFIG"); p != "" {
		return p
	}
	return "/opt/gateway/config.yml"
}

// ---------- Reported configuration (Phase 5 / §22) ----------
// revision+hash describe the exact config the agent runs. Reported in every
// status payload so the platform detects drift against desired state.

var (
	configRevision int64
	configHash     string
)

func stampConfigRevision() {
	configRevision = time.Now().Unix()
	if data, err := yaml.Marshal(&cfg); err == nil {
		sum := sha256.Sum256(data)
		configHash = hex.EncodeToString(sum[:])
	}
}

// ---------- Config Hot-Reload (SIGHUP) ----------

func startConfigReloader() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP)
	go func() {
		for range sigCh {
			logger.Info("SIGHUP received, reloading config")
			data, err := os.ReadFile(configPath())
			if err != nil {
				logger.WithError(err).Error("config reload: read failed")
				continue
			}
			var newCfg Config
			if err := yaml.Unmarshal(data, &newCfg); err != nil {
				logger.WithError(err).Error("config reload: parse failed")
				continue
			}
			if secrets != nil {
				if err := secrets.processConfig(&newCfg); err != nil {
					logger.WithError(err).Error("config reload: secret processing failed")
					continue
				}
			}
			// Prevent auth downgrade on reload: validate new MQTT config before applying.
			// Use newCfg.Environment for env detection when GATEWAY_ENV is unset.
			reloadEnv := strings.ToLower(strings.TrimSpace(os.Getenv("GATEWAY_ENV")))
			if reloadEnv == "" {
				reloadEnv = strings.ToLower(strings.TrimSpace(newCfg.Environment))
				if reloadEnv == "" {
					reloadEnv = "production"
				}
			}
			if err := ValidateMQTTConfigWithEnv(newCfg.MQTT, reloadEnv); err != nil {
				logger.WithFields(logrus.Fields{"env": reloadEnv, "broker": newCfg.MQTT.BrokerURL}).Errorf("config reload: rejected invalid MQTT config: %v (keeping existing authenticated config, not downgrading)", err)
				continue
			}
			switch newCfg.Logging.Level {
			case "debug":
				logger.SetLevel(logrus.DebugLevel)
			case "warn":
				logger.SetLevel(logrus.WarnLevel)
			case "error":
				logger.SetLevel(logrus.ErrorLevel)
			default:
				logger.SetLevel(logrus.InfoLevel)
			}
			if newCfg.Monitoring.Interval <= 0 {
				newCfg.Monitoring.Interval = 30
			}
			if newCfg.Logging.File != "" && newCfg.Logging.File != cfg.Logging.File {
				if logFile != nil {
					logFile.Close()
					logFile = nil
				}
				f, err := os.OpenFile(newCfg.Logging.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
				if err == nil {
					logFile = f
					logger.SetOutput(io.MultiWriter(os.Stderr, f))
				}
			}
			cfg = newCfg
			stampConfigRevision()
			logger.WithField("interval", cfg.Monitoring.Interval).Info("config reloaded")
		}
	}()
}

// ---------- Environment & Production Validation ----------

func effectiveEnvironment() string {
	// GATEWAY_ENV wins over config file; default is production (fail-closed)
	if e := os.Getenv("GATEWAY_ENV"); e != "" {
		return strings.ToLower(strings.TrimSpace(e))
	}
	if e := strings.ToLower(strings.TrimSpace(cfg.Environment)); e != "" {
		return e
	}
	return "production"
}

func isProduction() bool { return effectiveEnvironment() == "production" }

// ValidateMQTTConfig enforces production-safe Mango MQTT credentials using the
// effective environment (GATEWAY_ENV > cfg.Environment > production).
// It never logs secrets.
func ValidateMQTTConfig(c MQTTConfig) error {
	return ValidateMQTTConfigWithEnv(c, effectiveEnvironment())
}

// ValidateMQTTConfigWithEnv enforces production-safe Mango MQTT credentials for an explicit env.
// It never logs secrets.
func ValidateMQTTConfigWithEnv(c MQTTConfig, env string) error {
	if c.BrokerURL == "" {
		return fmt.Errorf("mqtt.broker_url is required (env=%s)", env)
	}
	if c.ClientIDPrefix == "" {
		return fmt.Errorf("mqtt.client_id_prefix is required (env=%s)", env)
	}
	if c.KeepAlive <= 0 || c.KeepAlive > 3600 {
		return fmt.Errorf("mqtt.keep_alive must be 1..3600 (got %d)", c.KeepAlive)
	}
	if c.QoS > 2 {
		return fmt.Errorf("mqtt.qos must be 0..2 (got %d)", c.QoS)
	}
	if c.CleanSession != false {
		// Persistent session required for QoS1 inflight across reconnects.
		return fmt.Errorf("mqtt.clean_session must be false (persistent session required, env=%s)", env)
	}
	if c.SSL {
		// TLS: CA cert must exist if configured; if ca_cert path is set it must be readable
		// (actual TLS build will fail later if unreadable, but we validate early)
	}
	// Production: anonymous never allowed; Development: only when explicitly allow_anonymous true
	// NOTE: env must be pre-normalized to lowercase by caller.
	if env == "production" {
		if c.AllowAnonymous {
			return fmt.Errorf("mqtt.allow_anonymous=true is never allowed in production (env=%s)", env)
		}
		if strings.TrimSpace(c.Username) == "" || strings.TrimSpace(c.Password) == "" {
			return fmt.Errorf("production mqtt requires username and password (both non-empty, env=%s) — anonymous MQTT is disabled", env)
		}
	} else {
		// Development/test: anonymous allowed ONLY when explicitly allow_anonymous true
		if strings.TrimSpace(c.Username) == "" || strings.TrimSpace(c.Password) == "" {
			if !c.AllowAnonymous {
				return fmt.Errorf("development mqtt with empty credentials requires mqtt.allow_anonymous: true (explicit, env=%s)", env)
			}
			// explicitly allowed — warn loud
			logger.Warn("mqtt: anonymous connection explicitly allowed (development, allow_anonymous=true)")
		}
	}
	return nil
}
