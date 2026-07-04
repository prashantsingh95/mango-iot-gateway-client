package main

import (
	"io"
	"os"
	"os/signal"
	"syscall"

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

type GatewayConfig struct {
	DeviceID       string `yaml:"device_id"`
	Name           string `yaml:"name"`
	SerialNumber   string `yaml:"serial_number"`
	TenantID       string `yaml:"tenant_id"`
	ProvisionToken string `yaml:"provision_token"`
	PlatformURL    string `yaml:"platform_url"`
}

type Config struct {
	Gateway    GatewayConfig     `yaml:"gateway"`
	MQTT       MQTTConfig        `yaml:"mqtt"`
	Modbus     ModbusConfig      `yaml:"modbus"`
	GPIO       GPIOConfig        `yaml:"gpio"`
	Monitoring MonitorConfig     `yaml:"monitoring"`
	Logging    LogConfig         `yaml:"logging"`
	OTA        OTAConfig         `yaml:"ota"`
	Watchdog   WatchdogConfig    `yaml:"watchdog"`
	Commands   CommandsConfig    `yaml:"commands"`
}

func configPath() string {
	if p := os.Getenv("GATEWAY_CONFIG"); p != "" {
		return p
	}
	return "/opt/gateway/config.yml"
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
				f, err := os.OpenFile(newCfg.Logging.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
				if err == nil {
					logFile = f
					logger.SetOutput(io.MultiWriter(os.Stderr, f))
				}
			}
			cfg = newCfg
			logger.WithField("interval", cfg.Monitoring.Interval).Info("config reloaded")
		}
	}()
}
