package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/signal"
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
	BrokerURL          string          `yaml:"broker_url"`
	Username           string          `yaml:"username"`
	Password           string          `yaml:"password"`
	ClientIDPrefix     string          `yaml:"client_id_prefix"`
	SSL                bool            `yaml:"ssl"`
	CACert             string          `yaml:"ca_cert"`
	ClientCert         string          `yaml:"client_cert"`
	ClientKey          string          `yaml:"client_key"`
	QoS                byte            `yaml:"qos"`
	KeepAlive          int             `yaml:"keep_alive"`
	CleanSession       bool            `yaml:"clean_session"`
	ReconnectDelay     int             `yaml:"reconnect_delay"`
	MaxReconnectDelay  int             `yaml:"max_reconnect_delay"`
	Topics             MQTTTopicConfig `yaml:"topics"`
}

type ModbusRegister struct {
	Name     string `yaml:"name"`
	Address  uint16 `yaml:"address"`
	Quantity uint16 `yaml:"quantity"`
	Type     string `yaml:"type"`
}

type ModbusDevice struct {
	Name      string            `yaml:"name"`
	Protocol  string            `yaml:"protocol"`
	Address   string            `yaml:"address"`
	SlaveID   byte              `yaml:"slave_id"`
	BaudRate  int               `yaml:"baud_rate"`
	DataBits  int               `yaml:"data_bits"`
	StopBits  int               `yaml:"stop_bits"`
	Parity    string            `yaml:"parity"`
	Interval  int               `yaml:"interval"`
	Registers []ModbusRegister  `yaml:"registers"`
}

type ModbusConfig struct {
	Enabled bool            `yaml:"enabled"`
	Devices []ModbusDevice  `yaml:"devices"`
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
	Interval          int    `yaml:"interval"`
	CPU               bool   `yaml:"cpu"`
	Memory            bool   `yaml:"memory"`
	Disk              bool   `yaml:"disk"`
	Temperature       bool   `yaml:"temperature"`
	Network           bool   `yaml:"network"`
	DiskThresholdWarn int    `yaml:"disk_threshold_warn"`
	MemoryThresholdWarn int  `yaml:"memory_threshold_warn"`
	CPUThresholdWarn    int   `yaml:"cpu_threshold_warn"`
	TempThresholdWarn   int   `yaml:"temp_threshold_warn"`
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
	Enabled        bool `yaml:"enabled"`
	Interval       int  `yaml:"interval"`
	MaxMissedPings int  `yaml:"max_missed_pings"`
	Action         string `yaml:"action"`
}

type ShellCommandConfig struct {
	AllowedPaths []string `yaml:"allowed_paths"`
	Timeout      int      `yaml:"timeout"`
}

type CommandsConfig struct {
	Enabled bool                `yaml:"enabled"`
	Allowed []string            `yaml:"allowed"`
	Shell   ShellCommandConfig  `yaml:"shell"`
}

// TerminalConfig enables the reverse-connection remote terminal agent.
// The agent dials OUT to the backend's Socket.IO /agent namespace (no inbound
// ports). gateway_id defaults to the device id if unset.
type TerminalConfig struct {
	Enabled            bool   `yaml:"enabled"`
	GatewayID          string `yaml:"gateway_id"`
	BackendWSURL       string `yaml:"backend_ws_url"` // ws://host:3001 or wss://...
	AgentSecret        string `yaml:"agent_secret"`   // issued by the platform
	SigningPepper      string `yaml:"signing_pepper"` // MUST match backend TERMINAL_SIGNING_PEPPER
	HeartbeatMs        int    `yaml:"heartbeat_ms"`
	ReconnectBaseMs    int    `yaml:"reconnect_base_ms"`
	ReconnectMaxMs     int    `yaml:"reconnect_max_ms"`
	Shell              string `yaml:"shell"`
	ShellAllowlist     []string `yaml:"shell_allowlist"` // exact shell binaries permitted (default: Shell only)
	FileDir            string `yaml:"file_dir"` // base dir for uploads AND downloads (jail, defaults /tmp)
	MaxFileBytes       int64  `yaml:"max_file_bytes"` // per-transfer cap (default 25MB)
	IdleTimeoutMinutes int    `yaml:"idle_timeout_minutes"` // kill idle PTY sessions (default 30, 0 disables)
	MaxSessionHours    int    `yaml:"max_session_hours"`    // absolute session lifetime (default 8, 0 disables)
	MaxSessions        int    `yaml:"max_sessions"`         // concurrent PTY cap (default 5)
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
}

// QueueConfig bounds the persistent offline spool (Phase 5 / §20).
// The spool is a core reliability path: always on unless max_events is 0.
type QueueConfig struct {
	Path     string `yaml:"path"`      // SQLite file (default <config-dir>/spool.db)
	MaxEvents int   `yaml:"max_events"` // row cap, oldest low-priority evicted first (0 disables spool)
	TTLHours int    `yaml:"ttl_hours"`  // event age cap
	MaxMB    int    `yaml:"max_mb"`     // payload byte cap
	FlushBatch int  `yaml:"flush_batch"` // events per reconnect flush cycle
}

// CloudflareTunnelConfig configures the Cloudflare Zero Trust Tunnel
// for secure SSH access (Phase 4 / Cloudflare Zero Trust).
type CloudflareTunnelConfig struct {
	Enabled            bool   `yaml:"enabled"`
	GatewayID          string `yaml:"gateway_id"`           // defaults to gateway.device_id
	BackendWSURL       string `yaml:"backend_ws_url"`       // wss://host:3001 or wss://...
	AgentSecret        string `yaml:"agent_secret"`         // issued by the platform
	SigningPepper      string `yaml:"signing_pepper"`       // MUST match backend TERMINAL_SIGNING_PEPPER
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
}

// BrandingConfig carries tenant/OEM identity (SaaS §22). Configuration-driven —
// the same binary serves Mango, ACME, or any OEM tenant. Delivered via
// provisioning/cloud config; the gateway must never change tenantId locally.
type BrandingConfig struct {
	ProductName  string `yaml:"product_name"`  // e.g. "ACME IoT Gateway" (default "Mango Gateway")
	Manufacturer string `yaml:"manufacturer"`  // e.g. "ACME Inc."
	SupportURL   string `yaml:"support_url"`
	SupportEmail string `yaml:"support_email"`
	DeviceLabel  string `yaml:"device_label"`
	DeviceModel  string `yaml:"device_model"`
}

type Config struct {
	Gateway    GatewayConfig         `yaml:"gateway"`
	Branding   BrandingConfig        `yaml:"branding"`
	Queue      QueueConfig           `yaml:"queue"`
	Cloudflare CloudflareTunnelConfig `yaml:"cloudflare_tunnel"`
	MQTT       MQTTConfig            `yaml:"mqtt"`
	Modbus     ModbusConfig          `yaml:"modbus"`
	GPIO       GPIOConfig            `yaml:"gpio"`
	Monitoring MonitorConfig         `yaml:"monitoring"`
	Logging    LogConfig             `yaml:"logging"`
	OTA        OTAConfig             `yaml:"ota"`
	Watchdog   WatchdogConfig        `yaml:"watchdog"`
	Commands   CommandsConfig        `yaml:"commands"`
	Terminal   TerminalConfig        `yaml:"terminal"`
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
