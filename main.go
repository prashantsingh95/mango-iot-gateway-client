package main

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/goburrow/modbus"
	psCPU "github.com/shirou/gopsutil/v3/cpu"
	psDisk "github.com/shirou/gopsutil/v3/disk"
	psHost "github.com/shirou/gopsutil/v3/host"
	psLoad "github.com/shirou/gopsutil/v3/load"
	psMem "github.com/shirou/gopsutil/v3/mem"
	psNet "github.com/shirou/gopsutil/v3/net"
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

// ---------- State ----------

type AgentState struct {
	mu            sync.RWMutex
	DeviceID      string            `json:"device_id"`
	Connected     bool              `json:"connected"`
	Uptime        int64             `json:"uptime"`
	FirmwareVersion string          `json:"firmware_version"`
	LastTelemetry time.Time         `json:"last_telemetry"`
	LastHeartbeat time.Time         `json:"last_heartbeat"`
}

type ModbusValue struct {
	Name  string      `json:"name"`
	Value interface{} `json:"value"`
	Unit  string      `json:"unit,omitempty"`
	Time  time.Time   `json:"time"`
}

type TelemetryData struct {
	DeviceID    string                 `json:"device_id"`
	Timestamp   string                 `json:"timestamp"`
	CPU         float64                `json:"cpu,omitempty"`
	Memory      float64                `json:"memory,omitempty"`
	Disk        float64                `json:"disk,omitempty"`
	Temperature float64                `json:"temperature,omitempty"`
	Signal      float64                `json:"signal,omitempty"`
	Voltage     float64                `json:"voltage,omitempty"`
	Battery     float64                `json:"battery,omitempty"`
	System      map[string]interface{} `json:"system,omitempty"`
	Modbus      []ModbusValue          `json:"modbus,omitempty"`
	GPIO        map[string]interface{} `json:"gpio,omitempty"`
}

type StatusData struct {
	DeviceID     string `json:"device_id"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	Uptime       int64  `json:"uptime"`
	Version      string `json:"version"`
	IP           string `json:"ip"`
	LastSeen     string `json:"last_seen"`
	FirmwareVer  string `json:"firmware_version"`
	SerialNumber string `json:"serial_number,omitempty"`
	Model        string `json:"model,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	MACAddress   string `json:"mac_address,omitempty"`
	HardwareVer  string `json:"hardware_version,omitempty"`
	OSVersion    string `json:"os_version,omitempty"`
}

type CommandRequest struct {
	ID        string          `json:"id"`
	CommandID string          `json:"commandId"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type CommandResponse struct {
	ID        string      `json:"id"`
	Status    string      `json:"status"`
	Success   bool        `json:"success"`
	Result    interface{} `json:"result,omitempty"`
	Error     string      `json:"error,omitempty"`
	Timestamp string      `json:"timestamp"`
}

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

// ---------- Telemetry Buffer (persistent, survives restarts) ----------

type telemetryBuffer struct {
	mu       sync.Mutex
	filePath string
	queue    [][]byte
	maxSize  int
}

func newTelemetryBuffer(filePath string, maxSize int) *telemetryBuffer {
	tb := &telemetryBuffer{
		filePath: filePath,
		maxSize:  maxSize,
	}
	// Load existing buffer from disk
	data, err := os.ReadFile(filePath)
	if err == nil && len(data) > 0 {
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		for _, line := range lines {
			if len(line) > 0 {
				tb.queue = append(tb.queue, []byte(line))
			}
		}
		logger.WithField("count", len(tb.queue)).Info("telemetry buffer loaded from disk")
	}
	return tb
}

func (tb *telemetryBuffer) push(payload []byte) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if len(tb.queue) >= tb.maxSize {
		tb.queue = tb.queue[1:] // drop oldest
	}
	tb.queue = append(tb.queue, payload)
	tb.persist()
}

func (tb *telemetryBuffer) pop() ([][]byte, bool) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if len(tb.queue) == 0 {
		return nil, false
	}
	// Return all queued items
	items := tb.queue
	tb.queue = nil
	tb.persist()
	return items, true
}

func (tb *telemetryBuffer) persist() {
	if tb.filePath == "" {
		return
	}
	var buf strings.Builder
	for _, item := range tb.queue {
		buf.Write(item)
		buf.WriteByte('\n')
	}
	os.WriteFile(tb.filePath, []byte(buf.String()), 0644)
}

func (tb *telemetryBuffer) len() int {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return len(tb.queue)
}

// ---------- Secrets Encryption ----------

type secretsManager struct {
	key     []byte
	keyPath string
	mu      sync.Mutex
}

func newSecretsManager(keyPath string) *secretsManager {
	sm := &secretsManager{keyPath: keyPath}
	data, err := os.ReadFile(keyPath)
	if err != nil {
		sm.key = make([]byte, 32)
		if _, err := rand.Read(sm.key); err != nil {
			logger.WithError(err).Warn("secrets: key generation failed, secrets will be stored in plaintext")
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
			logger.WithError(err).Warn("secrets: cannot create key dir")
			return nil
		}
		if err := os.WriteFile(keyPath, sm.key, 0600); err != nil {
			logger.WithError(err).Warn("secrets: cannot write key file")
			return nil
		}
		logger.Info("secrets: encryption key generated")
	} else {
		sm.key = data
	}
	return sm
}

func (sm *secretsManager) encrypt(plaintext string) (string, error) {
	if plaintext == "" || sm == nil {
		return plaintext, nil
	}
	block, err := aes.NewCipher(sm.key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return "enc:" + base64.StdEncoding.EncodeToString(sealed), nil
}

func (sm *secretsManager) decrypt(encrypted string) (string, error) {
	if !strings.HasPrefix(encrypted, "enc:") || sm == nil {
		return encrypted, nil
	}
	raw, err := base64.StdEncoding.DecodeString(encrypted[4:])
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(sm.key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := aead.NonceSize()
	if len(raw) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	plaintext, err := aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func (sm *secretsManager) processConfig(cfg *Config) error {
	if sm == nil {
		return nil
	}
	var needsRewrite bool

	if cfg.MQTT.Password != "" {
		if strings.HasPrefix(cfg.MQTT.Password, "enc:") {
			dec, err := sm.decrypt(cfg.MQTT.Password)
			if err != nil {
				return fmt.Errorf("decrypt mqtt password: %w", err)
			}
			cfg.MQTT.Password = dec
		} else {
			enc, err := sm.encrypt(cfg.MQTT.Password)
			if err != nil {
				return fmt.Errorf("encrypt mqtt password: %w", err)
			}
			cfg.MQTT.Password = enc
			needsRewrite = true
		}
	}

	if cfg.Gateway.ProvisionToken != "" {
		if strings.HasPrefix(cfg.Gateway.ProvisionToken, "enc:") {
			dec, err := sm.decrypt(cfg.Gateway.ProvisionToken)
			if err != nil {
				return fmt.Errorf("decrypt provision token: %w", err)
			}
			cfg.Gateway.ProvisionToken = dec
		} else {
			enc, err := sm.encrypt(cfg.Gateway.ProvisionToken)
			if err != nil {
				return fmt.Errorf("encrypt provision token: %w", err)
			}
			cfg.Gateway.ProvisionToken = enc
			needsRewrite = true
		}
	}

	if needsRewrite {
		data, err := yaml.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("marshal config: %w", err)
		}
		if err := os.WriteFile(configPath(), data, 0644); err != nil {
			return fmt.Errorf("rewrite config: %w", err)
		}
		logger.Info("secrets: encrypted plaintext secrets in config file")
	}
	return nil
}

// ---------- Modbus Collector (async, thread-safe) ----------

type modbusCollector struct {
	mu     sync.Mutex
	values map[string][]ModbusValue
}

func newModbusCollector() *modbusCollector {
	return &modbusCollector{values: make(map[string][]ModbusValue)}
}

func (mc *modbusCollector) set(deviceName string, vals []ModbusValue) {
	mc.mu.Lock()
	if len(vals) > 0 {
		mc.values[deviceName] = vals
	} else {
		delete(mc.values, deviceName)
	}
	mc.mu.Unlock()
}

func (mc *modbusCollector) getAll() []ModbusValue {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	var result []ModbusValue
	for _, vals := range mc.values {
		result = append(result, vals...)
	}
	return result
}

// ---------- HTTP Health Server ----------

type healthServer struct {
	server *http.Server
}

func newHealthServer(addr string) *healthServer {
	mux := http.NewServeMux()
	hs := &healthServer{
		server: &http.Server{Addr: addr, Handler: mux},
	}
	mux.HandleFunc("/health", hs.healthHandler)
	mux.HandleFunc("/ready", hs.readyHandler)
	return hs
}

func (hs *healthServer) start() {
	ln, err := net.Listen("tcp", hs.server.Addr)
	if err != nil {
		logger.WithError(err).Warn("health server: cannot listen, skipping")
		return
	}
	go hs.server.Serve(ln)
	logger.WithField("addr", hs.server.Addr).Info("health server started")
}

func (hs *healthServer) stop() {
	if hs.server != nil {
		hs.server.Close()
	}
}

func (hs *healthServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"uptime":  int64(time.Since(startTime).Seconds()),
		"version": version,
	})
}

func (hs *healthServer) readyHandler(w http.ResponseWriter, r *http.Request) {
	if isConnected() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "not ready"})
	}
}

// ---------- Modbus Handler ----------

type modbusHandler struct {
	name     string
	device   ModbusDevice
	handler  modbus.ClientHandler
	client   modbus.Client
}

func newModbusHandler(dev ModbusDevice) (*modbusHandler, error) {
	mh := &modbusHandler{name: dev.Name, device: dev}

	switch dev.Protocol {
	case "tcp":
		handler := modbus.NewTCPClientHandler(dev.Address)
		handler.Timeout = 10 * time.Second
		handler.SlaveId = dev.SlaveID
		if err := handler.Connect(); err != nil {
			return nil, fmt.Errorf("modbus tcp connect %s: %w", dev.Address, err)
		}
		mh.handler = handler
		mh.client = modbus.NewClient(handler)
	case "rtu":
		handler := modbus.NewRTUClientHandler(dev.Address)
		handler.BaudRate = dev.BaudRate
		handler.DataBits = dev.DataBits
		handler.StopBits = dev.StopBits
		handler.Parity = dev.Parity
		handler.Timeout = 5 * time.Second
		handler.SlaveId = dev.SlaveID
		if err := handler.Connect(); err != nil {
			return nil, fmt.Errorf("modbus rtu connect %s: %w", dev.Address, err)
		}
		mh.handler = handler
		mh.client = modbus.NewClient(handler)
	default:
		return nil, fmt.Errorf("unsupported modbus protocol: %s", dev.Protocol)
	}
	return mh, nil
}

func (mh *modbusHandler) readRegisters() []ModbusValue {
	var results []ModbusValue
	for _, reg := range mh.device.Registers {
		val, err := mh.readRegister(reg)
		if err != nil {
			logger.WithError(err).WithField("register", reg.Name).Warn("modbus read failed")
			continue
		}
		results = append(results, val)
	}
	return results
}

func (mh *modbusHandler) readRegister(reg ModbusRegister) (ModbusValue, error) {
	mv := ModbusValue{Name: reg.Name, Time: time.Now()}

	var raw []byte

	switch reg.Type {
	case "coil":
		raw = []byte{0}
		if reg.Quantity == 1 {
			v, err := mh.client.ReadCoils(reg.Address, 1)
			if err != nil {
				return mv, err
			}
			raw = v
		}
	case "discrete":
		v, err := mh.client.ReadDiscreteInputs(reg.Address, reg.Quantity)
		if err != nil {
			return mv, err
		}
		raw = v
	case "holding":
		v, err := mh.client.ReadHoldingRegisters(reg.Address, reg.Quantity)
		if err != nil {
			return mv, err
		}
		raw = v
	case "input":
		v, err := mh.client.ReadInputRegisters(reg.Address, reg.Quantity)
		if err != nil {
			return mv, err
		}
		raw = v
	default:
		// Default: read holding registers
		v, err := mh.client.ReadHoldingRegisters(reg.Address, reg.Quantity)
		if err != nil {
			return mv, err
		}
		raw = v
	}

	switch reg.Type {
	case "float32":
		if len(raw) >= 4 {
			mv.Value = math.Float32frombits(binary.BigEndian.Uint32(raw))
		}
	case "int16":
		if len(raw) >= 2 {
			v := int16(raw[0])<<8 | int16(raw[1])
			mv.Value = v
		}
	case "uint16":
		if len(raw) >= 2 {
			mv.Value = uint16(raw[0])<<8 | uint16(raw[1])
		}
	case "uint32":
		if len(raw) >= 4 {
			mv.Value = uint32(raw[0])<<24 | uint32(raw[1])<<16 | uint32(raw[2])<<8 | uint32(raw[3])
		}
	case "int32":
		if len(raw) >= 4 {
			mv.Value = int32(raw[0])<<24 | int32(raw[1])<<16 | int32(raw[2])<<8 | int32(raw[3])
		}
	case "bool":
		if len(raw) >= 1 {
			mv.Value = raw[0] != 0
		}
	default:
		mv.Value = raw
	}

	return mv, nil
}

func (mh *modbusHandler) close() {
	if closer, ok := mh.handler.(io.Closer); ok {
		closer.Close()
	}
}

// ---------- MQTT ----------

func mqttConnect() error {
	deviceID := getDeviceID()

	opts := MQTT.NewClientOptions()
	opts.AddBroker(cfg.MQTT.BrokerURL)
	opts.SetClientID(fmt.Sprintf("%s_%s_%d", cfg.MQTT.ClientIDPrefix, deviceID, time.Now().Unix()%10000))
	opts.SetCleanSession(cfg.MQTT.CleanSession)
	opts.SetKeepAlive(time.Duration(cfg.MQTT.KeepAlive) * time.Second)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(time.Duration(cfg.MQTT.ReconnectDelay) * time.Second)
	opts.SetMaxReconnectInterval(time.Duration(cfg.MQTT.MaxReconnectDelay) * time.Second)
	opts.SetConnectionLostHandler(func(c MQTT.Client, err error) {
		logger.WithError(err).Error("MQTT connection lost")
		setConnected(false)
	})
	opts.SetOnConnectHandler(func(c MQTT.Client) {
		logger.Info("MQTT connected")
		setConnected(true)
		subscribeCommands()
		sendStatus("ONLINE")
	})

	if cfg.MQTT.Username != "" {
		opts.SetUsername(cfg.MQTT.Username)
		opts.SetPassword(cfg.MQTT.Password)
	}

	if cfg.MQTT.SSL {
		tlsConfig, err := buildTLSConfig()
		if err != nil {
			return fmt.Errorf("tls config: %w", err)
		}
		opts.SetTLSConfig(tlsConfig)
	}

	client := MQTT.NewClient(opts)
	token := client.Connect()
	if !token.WaitTimeout(30 * time.Second) {
		return fmt.Errorf("mqtt connect: timeout after 30s")
	}
	if token.Error() != nil {
		return fmt.Errorf("mqtt connect: %w", token.Error())
	}

	mqttMu.Lock()
	mqttClient = client
	mqttMu.Unlock()
	return nil
}

func buildTLSConfig() (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}

	if cfg.MQTT.CACert != "" {
		caCert, err := os.ReadFile(cfg.MQTT.CACert)
		if err != nil {
			return nil, fmt.Errorf("ca cert: %w", err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}
		tc.RootCAs = caPool
	}

	if cfg.MQTT.ClientCert != "" && cfg.MQTT.ClientKey != "" {
		cert, err := tls.LoadX509KeyPair(cfg.MQTT.ClientCert, cfg.MQTT.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("client cert: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}

	return tc, nil
}

func subscribeCommands() {
	if !cfg.Commands.Enabled {
		return
	}
	mqttMu.Lock()
	c := mqttClient
	mqttMu.Unlock()
	if c == nil || !c.IsConnected() {
		return
	}
	cmdTopic := strings.ReplaceAll(cfg.MQTT.Topics.Command, "{device_id}", getDeviceID())
	if strings.Contains(cmdTopic, "{device_id}") {
		logger.Error("subscribe: command topic contains unresolved device_id placeholder")
		return
	}
	token := c.Subscribe(cmdTopic, cfg.MQTT.QoS, handleCommand)
	token.WaitTimeout(5 * time.Second)
	if token.Error() != nil {
		logger.WithError(token.Error()).Error("Failed to subscribe to commands")
	} else {
		logger.WithField("topic", cmdTopic).Info("Subscribed to commands")
	}
}

func mqttPublish(topic string, qos byte, retained bool, payload []byte) error {
	mqttMu.Lock()
	c := mqttClient
	mqttMu.Unlock()
	if c == nil || !c.IsConnected() {
		return fmt.Errorf("mqtt not connected")
	}
	if strings.Contains(topic, "{device_id}") || strings.Contains(topic, "{") {
		return fmt.Errorf("topic contains unresolved placeholder: %s", topic)
	}
	token := c.Publish(topic, qos, retained, payload)
	token.WaitTimeout(5 * time.Second)
	return token.Error()
}

func publishTelemetry(data interface{}) {
	topic := strings.ReplaceAll(cfg.MQTT.Topics.Telemetry, "{device_id}", getDeviceID())
	payload, _ := json.Marshal(data)
	for i := 0; i < 3; i++ {
		if err := mqttPublish(topic, cfg.MQTT.QoS, false, payload); err == nil {
			return
		} else if i < 2 {
			time.Sleep(time.Duration(i+1) * time.Second)
		}
	}
}

func publishStatus(status StatusData) {
	topic := strings.ReplaceAll(cfg.MQTT.Topics.Status, "{device_id}", getDeviceID())
	payload, _ := json.Marshal(status)
	for i := 0; i < 3; i++ {
		if err := mqttPublish(topic, cfg.MQTT.QoS, true, payload); err == nil {
			return
		} else if i < 2 {
			time.Sleep(time.Duration(i+1) * time.Second)
		}
	}
}

func publishLog(level, msg string, fields map[string]interface{}) {
	if !cfg.Logging.Remote {
		return
	}
	topic := strings.ReplaceAll(cfg.MQTT.Topics.Log, "{device_id}", getDeviceID())
	entry := map[string]interface{}{
		"level":     level,
		"message":   msg,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}
	for k, v := range fields {
		entry[k] = v
	}
	payload, _ := json.Marshal(entry)
	mqttPublish(topic, 0, false, payload)
}

// ---------- Command Handler ----------

func handleCommand(client MQTT.Client, msg MQTT.Message) {
	if len(msg.Payload()) > 1024 * 100 {
		logger.Warn("command payload exceeds 100KB, rejecting")
		return
	}
	var cmd CommandRequest
	if err := json.Unmarshal(msg.Payload(), &cmd); err != nil {
		logger.WithError(err).Warn("invalid command payload")
		return
	}

	if cmd.ID == "" && cmd.CommandID != "" {
		cmd.ID = cmd.CommandID
	}
	if cmd.ID == "" {
		logger.Warn("command missing ID, rejecting")
		return
	}
	if cmd.Type == "" {
		logger.Warn("command missing type, rejecting")
		return
	}

	logger.WithFields(logrus.Fields{"id": cmd.ID, "type": cmd.Type}).Info("received command")

	var resp CommandResponse
	resp.ID = cmd.ID
	resp.Timestamp = time.Now().UTC().Format(time.RFC3339)

	switch cmd.Type {
	case "reboot":
		resp = execReboot(cmd)
	case "restart_agent":
		resp = execRestartAgent(cmd)
	case "update_config":
		resp = execUpdateConfig(cmd)
	case "run_shell":
		resp = execShell(cmd)
	case "update_firmware":
		resp = execFirmwareUpdate(cmd)
	case "set_relay":
		resp = execSetRelay(cmd)
	case "read_register":
		resp = execReadRegister(cmd)
	default:
		resp.Status = "rejected"
		resp.Error = fmt.Sprintf("unknown command type: %s", cmd.Type)
	}

	sendCommandResponse(resp)
}

func execReboot(cmd CommandRequest) CommandResponse {
	logger.Warn("executing reboot command")
	go func() {
		time.Sleep(2 * time.Second)
		exec.Command("sudo", "reboot").Run()
	}()
	return CommandResponse{ID: cmd.ID, Status: "accepted", Result: "rebooting in 2s", Timestamp: time.Now().UTC().Format(time.RFC3339)}
}

func execRestartAgent(cmd CommandRequest) CommandResponse {
	logger.Warn("executing agent restart")
	go func() {
		time.Sleep(1 * time.Second)
		os.Exit(0) // systemd will restart
	}()
	return CommandResponse{ID: cmd.ID, Status: "accepted", Result: "restarting", Timestamp: time.Now().UTC().Format(time.RFC3339)}
}

func execUpdateConfig(cmd CommandRequest) CommandResponse {
	var newCfg Config
	if err := json.Unmarshal(cmd.Payload, &newCfg); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: fmt.Sprintf("invalid config: %s", err), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	// Encrypt secrets before writing to disk
	if err := secrets.processConfig(&newCfg); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: fmt.Sprintf("secrets: %s", err), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	data, _ := yaml.Marshal(&newCfg)
	if err := os.WriteFile(configPath(), data, 0644); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: err.Error(), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}
	return CommandResponse{ID: cmd.ID, Status: "completed", Result: "config updated, restart agent to apply", Timestamp: time.Now().UTC().Format(time.RFC3339)}
}

func execShell(cmd CommandRequest) CommandResponse {
	var payload struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil || payload.Command == "" {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: "invalid shell command payload", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	allowed := false

	// Check safe commands first (exact match only)
	safeCommands := []string{"ls", "ps", "df", "free", "uptime", "cat /sys/class/thermal/thermal_zone0/temp", "ifconfig", "ip a", "systemctl status gateway-agent"}
	for _, s := range safeCommands {
		if payload.Command == s {
			allowed = true
			break
		}
	}

	// Resolve path traversal: extract first token, clean it, then check allowed paths
	if !allowed {
		tokens := strings.Fields(payload.Command)
		if len(tokens) == 0 {
			return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "empty command", Timestamp: time.Now().UTC().Format(time.RFC3339)}
		}
		cmdToken := tokens[0]
		cleaned := filepath.Clean(cmdToken)
		for _, p := range cfg.Commands.Shell.AllowedPaths {
			if strings.HasPrefix(cleaned, p) {
				allowed = true
				break
			}
		}
	}

	if !allowed {
		return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "command not in allowed paths", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.Commands.Shell.Timeout)*time.Second)
	defer cancel()

	tokens := strings.Fields(payload.Command)
	if len(tokens) == 0 {
		return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "empty command", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	var cmdObj *exec.Cmd
	if len(tokens) == 1 {
		cmdObj = exec.CommandContext(ctx, tokens[0])
	} else {
		cmdObj = exec.CommandContext(ctx, tokens[0], tokens[1:]...)
	}
	out, err := cmdObj.CombinedOutput()
	if err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: err.Error(), Result: string(out), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}
	return CommandResponse{ID: cmd.ID, Status: "completed", Result: string(out), Timestamp: time.Now().UTC().Format(time.RFC3339)}
}

func execFirmwareUpdate(cmd CommandRequest) CommandResponse {
	var payload struct {
		URL         string `json:"url"`
		DownloadURL string `json:"downloadUrl"`
		Checksum    string `json:"checksum"`
		Version     string `json:"version"`
		FirmwareID  string `json:"firmwareId"`
		Filename    string `json:"filename"`
	}
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: "invalid payload", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	// Support both field naming conventions (platform uses downloadUrl/firmwareId, client uses url)
	url := payload.URL
	if url == "" {
		url = payload.DownloadURL
	}
	version := payload.Version
	if version == "" && payload.FirmwareID != "" {
		version = payload.FirmwareID
	}
	filename := payload.Filename
	if filename == "" && version != "" {
		filename = fmt.Sprintf("gateway-agent-%s", version)
	}

	if url == "" {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: "missing url/downloadUrl", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}
	if payload.Checksum == "" {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: "checksum required", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	logger.WithFields(logrus.Fields{"version": version, "url": url}).Info("starting firmware update")
	os.MkdirAll(cfg.OTA.FirmwareDir, 0755)
	os.MkdirAll(cfg.OTA.BackupDir, 0755)

	binPath := filepath.Join(cfg.OTA.FirmwareDir, filename)
	if err := downloadFile(binPath, url); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: fmt.Sprintf("download: %s", err), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	data, _ := os.ReadFile(binPath)
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	if hash != payload.Checksum {
		os.Remove(binPath)
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: "checksum mismatch", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	if err := os.Chmod(binPath, 0755); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: err.Error(), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	// Backup current binary
	selfPath, _ := os.Executable()
	backupPath := filepath.Join(cfg.OTA.BackupDir, "gateway-agent.bak")
	os.Remove(backupPath)
	if data, err := os.ReadFile(selfPath); err == nil {
		os.WriteFile(backupPath, data, 0755)
	}

	// Replace and restart
	if err := os.Rename(binPath, selfPath); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: err.Error(), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	go func() {
		time.Sleep(1 * time.Second)
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()

	return CommandResponse{ID: cmd.ID, Status: "completed", Result: fmt.Sprintf("updated to version %s, restarting", payload.Version), Timestamp: time.Now().UTC().Format(time.RFC3339)}
}

func execSetRelay(cmd CommandRequest) CommandResponse {
	var payload struct {
		Name  string `json:"name"`
		State bool   `json:"state"`
	}
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: "invalid payload", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	pin := -1
	for _, s := range cfg.GPIO.Sensors {
		if s.Name == payload.Name && s.Mode == "output" {
			pin = s.Pin
			break
		}
	}
	if pin < 0 {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: fmt.Sprintf("relay '%s' not found", payload.Name), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	val := "0"
	if payload.State {
		val = "1"
	}
	gpioPath := fmt.Sprintf("/sys/class/gpio/gpio%d/value", pin)
	if err := os.WriteFile(gpioPath, []byte(val), 0644); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: fmt.Sprintf("gpio write: %s", err), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}
	return CommandResponse{ID: cmd.ID, Status: "completed", Result: map[string]interface{}{"relay": payload.Name, "state": payload.State}, Timestamp: time.Now().UTC().Format(time.RFC3339)}
}

func execReadRegister(cmd CommandRequest) CommandResponse {
	var payload struct {
		Device string `yaml:"device"`
		Name   string `yaml:"name"`
	}
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: "invalid payload", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	modbusMu.RLock()
	handler, ok := modbusPools[payload.Device]
	modbusMu.RUnlock()
	if !ok {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: fmt.Sprintf("device '%s' not connected", payload.Device), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	for _, reg := range handler.device.Registers {
		if reg.Name == payload.Name {
			val, err := handler.readRegister(reg)
			if err != nil {
				return CommandResponse{ID: cmd.ID, Status: "failed", Error: err.Error(), Timestamp: time.Now().UTC().Format(time.RFC3339)}
			}
			return CommandResponse{ID: cmd.ID, Status: "completed", Result: val, Timestamp: time.Now().UTC().Format(time.RFC3339)}
		}
	}
	return CommandResponse{ID: cmd.ID, Status: "failed", Error: fmt.Sprintf("register '%s' not found in device '%s'", payload.Name, payload.Device), Timestamp: time.Now().UTC().Format(time.RFC3339)}
}

func sendCommandResponse(resp CommandResponse) {
	if resp.ID == "" {
		return
	}
	topic := strings.ReplaceAll(cfg.MQTT.Topics.Response, "{device_id}", getDeviceID())
	if strings.Contains(topic, "{device_id}") {
		return
	}
	resp.Success = resp.Status == "completed"
	payload, _ := json.Marshal(resp)
	mqttPublish(topic, cfg.MQTT.QoS, false, payload)
}

// ---------- System Monitoring ----------

func collectSystemMetrics() map[string]interface{} {
	metrics := make(map[string]interface{})

	if cfg.Monitoring.CPU {
		if p, err := psCPU.Percent(0, false); err == nil && len(p) > 0 {
			metrics["cpu_percent"] = math.Round(p[0]*100) / 100
		}
		if l, err := psLoad.Avg(); err == nil {
			metrics["load_1"] = math.Round(l.Load1*100) / 100
			metrics["load_5"] = math.Round(l.Load5*100) / 100
			metrics["load_15"] = math.Round(l.Load15*100) / 100
		}
	}

	if cfg.Monitoring.Memory {
		if m, err := psMem.VirtualMemory(); err == nil {
			metrics["memory_total_mb"] = int(m.Total / 1024 / 1024)
			metrics["memory_used_mb"] = int(m.Used / 1024 / 1024)
			metrics["memory_percent"] = math.Round(m.UsedPercent*100) / 100
		}
		if s, err := psMem.SwapMemory(); err == nil {
			if s.Total > 0 {
				metrics["swap_total_mb"] = int(s.Total / 1024 / 1024)
				metrics["swap_used_mb"] = int(s.Used / 1024 / 1024)
			}
		}
	}

	if cfg.Monitoring.Disk {
		diskMetrics := make(map[string]interface{})
		partitions, _ := psDisk.Partitions(false)
		for _, p := range partitions {
			if usage, err := psDisk.Usage(p.Mountpoint); err == nil {
				diskMetrics[p.Mountpoint] = map[string]interface{}{
					"total_gb":  int(usage.Total / 1024 / 1024 / 1024),
					"used_gb":   int(usage.Used / 1024 / 1024 / 1024),
					"free_gb":   int(usage.Free / 1024 / 1024 / 1024),
					"used_pct":  math.Round(usage.UsedPercent*100) / 100,
				}
			}
		}
		metrics["disk"] = diskMetrics
	}

	if cfg.Monitoring.Temperature {
		temp := getCPUTemperature()
		if temp >= 0 {
			metrics["temperature_c"] = temp
		}
	}

	if cfg.Monitoring.Network {
		if io, err := psNet.IOCounters(false); err == nil && len(io) > 0 {
			metrics["network_rx_bytes"] = io[0].BytesRecv
			metrics["network_tx_bytes"] = io[0].BytesSent
		}
		metrics["uptime_seconds"] = int64(time.Since(startTime).Seconds())
	}

	return metrics
}

func getCPUTemperature() float64 {
	data, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return -1
	}
	tempStr := strings.TrimSpace(string(data))
	temp, err := strconv.ParseFloat(tempStr, 64)
	if err != nil {
		return -1
	}
	return temp / 1000.0
}

// ---------- Watchdog ----------

func startWatchdog(ctx context.Context) {
	if !cfg.Watchdog.Enabled {
		return
	}

	startupGrace := time.Duration(cfg.Watchdog.Interval*cfg.Watchdog.MaxMissedPings) * time.Second

	ticker := time.NewTicker(time.Duration(cfg.Watchdog.Interval) * time.Second)
	defer ticker.Stop()

	missed := 0
	maxMissed := cfg.Watchdog.MaxMissedPings

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Since(startTime) < startupGrace {
				continue
			}
			if !isConnected() {
				missed++
				logger.WithField("missed", missed).Warn("watchdog: MQTT disconnected")
				if missed >= maxMissed {
					logger.Error("watchdog: max missed pings, taking action")
					switch cfg.Watchdog.Action {
					case "restart":
						os.Exit(0)
					case "reboot":
						exec.Command("sudo", "reboot").Run()
					default:
						if cfg.Watchdog.Action != "" {
							tokens := strings.Fields(cfg.Watchdog.Action)
							if len(tokens) > 0 {
								if len(tokens) == 1 {
									exec.Command(tokens[0]).Run()
								} else {
									exec.Command(tokens[0], tokens[1:]...).Run()
								}
							}
						}
					}
					return
				}
			} else {
				missed = 0
				sendStatus("ONLINE")
			}
		}
	}
}

// ---------- Utilities ----------

func getDeviceID() string {
	state.mu.RLock()
	id := state.DeviceID
	state.mu.RUnlock()
	return id
}

func setConnected(v bool) {
	state.mu.Lock()
	state.Connected = v
	state.mu.Unlock()
}

func isConnected() bool {
	state.mu.RLock()
	defer state.mu.RUnlock()
	if mqttClient != nil {
		return mqttClient.IsConnected()
	}
	return state.Connected
}

func getSerialNumber() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Serial") {
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

func getMACAddress() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp != 0 && iface.HardwareAddr != nil && iface.Name != "lo" {
			return iface.HardwareAddr.String()
		}
	}
	return ""
}

func getSerialNumber() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "Serial") {
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

func getModel() string {
	data, err := os.ReadFile("/proc/device-tree/model")
	if err != nil {
		return "Raspberry Pi"
	}
	return strings.TrimSpace(string(data))
}

func getManufacturer() string {
	return "Raspberry Pi Foundation"
}

func getHardwareVersion() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "Hardware") {
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

func getOSVersion() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), "\"")
		}
	}
	return "Linux"
}

func getMACAddress() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp != 0 && iface.HardwareAddr != nil && iface.Name != "lo" {
			return iface.HardwareAddr.String()
		}
	}
	return ""
}

func getIPAddress() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "0.0.0.0"
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}

func configPath() string {
	if p := os.Getenv("GATEWAY_CONFIG"); p != "" {
		return p
	}
	return "/opt/gateway/config.yml"
}

func downloadFile(path, url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.ContentLength > 100*1024*1024 {
		return fmt.Errorf("file too large: %d bytes", resp.ContentLength)
	}

	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, io.LimitReader(resp.Body, 100*1024*1024))
	return err
}

// ---------- Main Loop ----------

func runTelemetryLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(cfg.Monitoring.Interval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			telemetry := TelemetryData{
				DeviceID:  getDeviceID(),
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			}

sys := collectSystemMetrics()
		telemetry.System = sys

		// Extract flat fields for platform compatibility
		if v, ok := sys["cpu_percent"].(float64); ok {
			telemetry.CPU = v
		}
		if v, ok := sys["memory_percent"].(float64); ok {
			telemetry.Memory = v
		}
		// Extract root partition disk usage
		if disk, ok := sys["disk"].(map[string]interface{}); ok {
			if rootDisk, ok := disk["/"].(map[string]interface{}); ok {
				if v, ok := rootDisk["used_pct"].(float64); ok {
					telemetry.Disk = v
				}
			}
		}
		if v, ok := sys["temperature_c"].(float64); ok {
			telemetry.Temperature = v
		}
		// Add signal strength (from wifi if available)
		if v, ok := sys["signal_dbm"].(float64); ok {
			telemetry.Signal = v
		}
		// Add voltage and battery (placeholder for Pi - could be extended with HAT readings)
		if v, ok := sys["voltage_v"].(float64); ok {
			telemetry.Voltage = v
		}
		if v, ok := sys["battery_percent"].(float64); ok {
			telemetry.Battery = v
		}

			// Check thresholds
			if telemetry.CPU > float64(cfg.Monitoring.CPUThresholdWarn) {
				logger.WithField("cpu", telemetry.CPU).Warn("CPU threshold exceeded")
			}
			if telemetry.Memory > float64(cfg.Monitoring.MemoryThresholdWarn) {
				logger.WithField("memory", telemetry.Memory).Warn("Memory threshold exceeded")
			}
			if telemetry.Temperature > float64(cfg.Monitoring.TempThresholdWarn) {
				logger.WithField("temperature", telemetry.Temperature).Warn("Temperature threshold exceeded")
			}

			// Collect Modbus data from async collector
			if cfg.Modbus.Enabled && modbusCol != nil {
				telemetry.Modbus = modbusCol.getAll()
			}

			publishTelemetry(telemetry)
			state.mu.Lock()
			state.LastTelemetry = time.Now()
			state.mu.Unlock()
		}
	}
}

func runModbusLoop(ctx context.Context) {
	if !cfg.Modbus.Enabled {
		return
	}

	type poller struct {
		handler  *modbusHandler
		interval time.Duration
		ticker   *time.Ticker
	}

	var pollers []*poller
	for _, dev := range cfg.Modbus.Devices {
		mh, ok := modbusPools[dev.Name]
		if !ok {
			continue
		}
		interval := dev.Interval
		if interval <= 0 {
			interval = 10
		}
		p := &poller{
			handler:  mh,
			interval: time.Duration(interval) * time.Second,
			ticker:   time.NewTicker(time.Duration(interval) * time.Second),
		}
		pollers = append(pollers, p)
	}

	for {
		select {
		case <-ctx.Done():
			for _, p := range pollers {
				p.ticker.Stop()
			}
			return
		default:
			for _, p := range pollers {
				select {
				case <-p.ticker.C:
					vals := p.handler.readRegisters()
					if modbusCol != nil {
						modbusCol.set(p.handler.name, vals)
					}
					if len(vals) > 0 {
						logger.WithFields(logrus.Fields{
							"device": p.handler.name,
							"values": len(vals),
						}).Debug("modbus poll completed")
					}
				default:
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func sendStatus(status string, reason ...string) {
	r := ""
	if len(reason) > 0 {
		r = reason[0]
	}
	s := StatusData{
		DeviceID:     getDeviceID(),
		Status:       status,
		Reason:       r,
		Uptime:       int64(time.Since(startTime).Seconds()),
		Version:      version,
		IP:           getIPAddress(),
		LastSeen:     time.Now().UTC().Format(time.RFC3339),
		FirmwareVer:  version,
		SerialNumber: getSerialNumber(),
		Model:        getModel(),
		Manufacturer: getManufacturer(),
		MACAddress:   getMACAddress(),
		HardwareVer:  getHardwareVersion(),
		OSVersion:    getOSVersion(),
	}
	publishStatus(s)
}

// ---------- GPIO Initialization (sysfs) ----------

func initGPIO() {
	if !cfg.GPIO.Enabled {
		return
	}
	for _, s := range cfg.GPIO.Sensors {
		if s.Mode == "output" {
			pinStr := strconv.Itoa(s.Pin)
			os.WriteFile("/sys/class/gpio/export", []byte(pinStr), 0644)
			gpioDir := fmt.Sprintf("/sys/class/gpio/gpio%s", pinStr)
			for i := 0; i < 50; i++ {
				if _, err := os.Stat(gpioDir); err == nil {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			dirPath := gpioDir + "/direction"
			os.WriteFile(dirPath, []byte("out"), 0644)
			val := "0"
			if s.Default {
				val = "1"
			}
			valPath := gpioDir + "/value"
			os.WriteFile(valPath, []byte(val), 0644)
			logger.WithFields(logrus.Fields{"pin": s.Pin, "name": s.Name, "default": s.Default}).Info("GPIO output initialized")
		}
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
			// Decrypt secrets
			if secrets != nil {
				if err := secrets.processConfig(&newCfg); err != nil {
					logger.WithError(err).Error("config reload: secret processing failed")
					continue
				}
			}
			// Apply log level
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
			// Update monitoring interval
			if newCfg.Monitoring.Interval <= 0 {
				newCfg.Monitoring.Interval = 30
			}
			// Re-open log file if path changed
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

// ---------- Provisioning ----------

func provisionGateway() {
	if cfg.Gateway.ProvisionToken == "" || cfg.Gateway.PlatformURL == "" {
		return
	}
	body := map[string]interface{}{
		"token": cfg.Gateway.ProvisionToken,
		"gateway": map[string]interface{}{
			"deviceId":        getDeviceID(),
			"name":            cfg.Gateway.Name,
			"serialNumber":    getSerialNumber(),
			"tenantId":        cfg.Gateway.TenantID,
			"firmwareVersion": version,
			"model":           getModel(),
			"manufacturer":    getManufacturer(),
			"hardwareVersion": getHardwareVersion(),
			"osVersion":       getOSVersion(),
			"macAddress":      getMACAddress(),
		},
	}
	payload, _ := json.Marshal(body)
	url := strings.TrimRight(cfg.Gateway.PlatformURL, "/") + "/api/v1/provisioning/gateway"
	resp, err := http.Post(url, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		logger.WithError(err).Warn("provisioning: request failed")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		logger.Info("provisioning: gateway registered successfully")
	} else {
		body, _ := io.ReadAll(resp.Body)
		logger.WithFields(logrus.Fields{"status": resp.StatusCode, "response": string(body)}).Warn("provisioning: unexpected response")
	}
}

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
