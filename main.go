package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
	psHost "github.com/shirou/gopsutil/v3/host"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// ---------- Globals ----------

var (
	cfg         Config
	state       AgentState
	mqttClient  MQTT.Client
	mqttMu      sync.Mutex
	modbusPools = make(map[string]*modbusHandler)
	modbusMu    sync.RWMutex
	logger      = logrus.New()
	logFile     *os.File
	startTime   = time.Now()
	version     = "1.0.0"
	secrets     *secretsManager
	modbusCol   *modbusCollector
	healthSrv   *healthServer
	telemetryBuf *telemetryBuffer
)

// ---------- Main ----------

func main() {
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
		ForceColors:   runtime.GOOS != "linux",
	})

	// Parse flags
	configFile := configPath()
	for i, arg := range os.Args {
		if arg == "--config" && i+1 < len(os.Args) {
			configFile = os.Args[i+1]
		}
		if arg == "--version" {
			fmt.Printf("Mango IoT Gateway Agent v%s\n", version)
			os.Exit(0)
		}
	}

	// Load config
	data, err := os.ReadFile(configFile)
	if err != nil {
		logger.Fatalf("Cannot read config %s: %v", configFile, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		logger.Fatalf("Cannot parse config: %v", err)
	}

	if cfg.MQTT.Topics.Telemetry == "" {
		cfg.MQTT.Topics.Telemetry = "gateway/{device_id}/telemetry"
	}
	if cfg.MQTT.Topics.Status == "" {
		cfg.MQTT.Topics.Status = "gateway/{device_id}/status"
	}
	if cfg.MQTT.Topics.Log == "" {
		cfg.MQTT.Topics.Log = "gateway/{device_id}/log"
	}
	if cfg.MQTT.Topics.Command == "" {
		cfg.MQTT.Topics.Command = "gateway/{device_id}/command/set"
	}
	if cfg.MQTT.Topics.Response == "" {
		cfg.MQTT.Topics.Response = "gateway/{device_id}/command/response"
	}

	if cfg.Monitoring.Interval <= 0 {
		cfg.Monitoring.Interval = 30
	}
	if cfg.MQTT.ClientIDPrefix == "" {
		cfg.MQTT.ClientIDPrefix = "gw"
	}
	if cfg.MQTT.QoS == 0 {
		cfg.MQTT.QoS = 1
	}
	if cfg.MQTT.KeepAlive <= 0 {
		cfg.MQTT.KeepAlive = 60
	}
	if cfg.MQTT.ReconnectDelay <= 0 {
		cfg.MQTT.ReconnectDelay = 5
	}
	if cfg.MQTT.MaxReconnectDelay <= 0 {
		cfg.MQTT.MaxReconnectDelay = 60
	}

	// Terminal (reverse-connection agent) defaults
	if cfg.Terminal.HeartbeatMs <= 0 {
		cfg.Terminal.HeartbeatMs = 30000
	}
	if cfg.Terminal.ReconnectBaseMs <= 0 {
		cfg.Terminal.ReconnectBaseMs = 1000
	}
	if cfg.Terminal.ReconnectMaxMs <= 0 {
		cfg.Terminal.ReconnectMaxMs = 30000
	}
	if cfg.Terminal.Shell == "" {
		cfg.Terminal.Shell = "/bin/bash"
	}
	if cfg.Terminal.FileDir == "" {
		cfg.Terminal.FileDir = "/tmp"
	}

	// Setup logging
	switch cfg.Logging.Level {
	case "debug":
		logger.SetLevel(logrus.DebugLevel)
	case "warn":
		logger.SetLevel(logrus.WarnLevel)
	case "error":
		logger.SetLevel(logrus.ErrorLevel)
	default:
		logger.SetLevel(logrus.InfoLevel)
	}

	if cfg.Logging.File != "" {
		f, err := os.OpenFile(cfg.Logging.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err == nil {
			logFile = f
			logger.SetOutput(io.MultiWriter(os.Stderr, f))
		}
	}

	// Initialize secrets encryption
	secrets = newSecretsManager(filepath.Join(filepath.Dir(configFile), ".encryption_key"))
	if err := secrets.processConfig(&cfg); err != nil {
		logger.WithError(err).Warn("secrets: processing failed, continuing with plaintext")
	}

	// Initialize device ID
	deviceID := cfg.Gateway.DeviceID
	if deviceID == "" {
		deviceID = strings.ReplaceAll(getMACAddress(), ":", "")
		if deviceID == "" {
			deviceID = fmt.Sprintf("pi-%d", time.Now().Unix())
		}
	}

	serial := cfg.Gateway.SerialNumber
	if serial == "" {
		serial = getSerialNumber()
	}

	state.mu.Lock()
	state.DeviceID = deviceID
	state.FirmwareVersion = version
	state.mu.Unlock()

	info, _ := psHost.Info()
	logger.WithFields(logrus.Fields{
		"device_id":  deviceID,
		"serial":     serial,
		"hostname":   info.Hostname,
		"os":         info.OS,
		"platform":   info.Platform,
		"kernel":     info.KernelVersion,
		"version":    version,
	}).Info("Mango IoT Gateway Agent starting")

	// Provision with platform (if token and URL configured)
	provisionGateway()

	// Connect MQTT
	if err := mqttConnect(); err != nil {
		logger.WithError(err).Fatal("MQTT connection failed")
	}

	if cfg.Modbus.Enabled {
		for _, dev := range cfg.Modbus.Devices {
			mh, err := newModbusHandler(dev)
			if err != nil {
				logger.WithError(err).WithField("device", dev.Name).Error("modbus init failed")
				continue
			}
			modbusMu.Lock()
			modbusPools[dev.Name] = mh
			modbusMu.Unlock()
			logger.WithField("device", dev.Name).Info("modbus device connected")
		}
	}

	// Initialize async Modbus collector
	modbusCol = newModbusCollector()

	// Initialize GPIO via sysfs
	initGPIO()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start loops
	go runTelemetryLoop(ctx)
	go runModbusLoop(ctx)
	go startWatchdog(ctx)

	// Reverse-connection terminal agent (optional)
	if cfg.Terminal.Enabled && cfg.Terminal.BackendWSURL != "" && cfg.Terminal.AgentSecret != "" {
		go startTerminalAgent(ctx)
		logger.Info("terminal agent: enabled (reverse-connection)")
	}

	// Start HTTP health endpoint on localhost:8090
	healthSrv = newHealthServer("127.0.0.1:8090")
	healthSrv.start()

	// Start config hot-reload via SIGHUP
	startConfigReloader()

	// Print hardware info
	logger.WithFields(logrus.Fields{
		"cpu_count": runtime.NumCPU(),
		"go_arch":   runtime.GOARCH,
		"go_os":     runtime.GOOS,
	}).Info("Hardware info")

	// Send initial status
	time.Sleep(2 * time.Second) // wait for MQTT to settle
	sendStatus("ONLINE")

	// Wait for signal (SIGINT/SIGTERM only; SIGHUP is handled separately)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh

	logger.WithField("signal", sig.String()).Info("shutting down")

	// Cancel all goroutine contexts first
	cancel()

	// Send final status
	sendStatus("OFFLINE", "shutdown")
	time.Sleep(500 * time.Millisecond)

	// Graceful cleanup in order
	mqttMu.Lock()
	if mqttClient != nil && mqttClient.IsConnected() {
		mqttClient.Disconnect(500)
	}
	mqttClient = nil
	mqttMu.Unlock()

	modbusMu.Lock()
	for _, mh := range modbusPools {
		mh.close()
	}
	modbusMu.Unlock()

	if healthSrv != nil {
		healthSrv.stop()
	}
	if logFile != nil {
		logFile.Close()
	}
}
