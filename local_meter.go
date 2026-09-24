package main

// ---------- Local MQTT meter client ----------
//
// Subscribes to meter topics on the on-gateway broker (127.0.0.1), validates
// payloads, queues accepted readings into the offline external-device store,
// and keeps per-meter stats. Never touches gateway telemetry.

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
)

type meterReading struct {
	MeterID   string                 `json:"meter_id"`
	Timestamp string                 `json:"timestamp"`
	Received  string                 `json:"received_at"`
	Data      map[string]interface{} `json:"data"`
}

type meterInfo struct {
	MeterID    string `json:"meter_id"`
	Count      int64  `json:"count"`
	LastSeen   string `json:"last_seen"`
	LastStatus string `json:"last_status,omitempty"`
}

type meterStats struct {
	mu         sync.Mutex
	connected  bool
	received   int64
	accepted   int64
	rejected   int64
	lastError  string
	meters     map[string]*meterInfo
	recent     []meterReading
}

var mStats = &meterStats{meters: map[string]*meterInfo{}}

var (
	meterClientMu sync.Mutex
	meterClient   MQTT.Client
	meterStopCh   chan struct{}
)

func localClientBrokerURL() string {
	b := &cfg.LocalBroker
	secure := b.Mode == "secure" || b.Mode == "both"
	host := "127.0.0.1"
	if secure {
		return fmt.Sprintf("ssl://%s:%d", host, b.PortTLS)
	}
	return fmt.Sprintf("tcp://%s:%d", host, b.PortPlain)
}

func localMeterClientStatus() map[string]interface{} {
	mStats.mu.Lock()
	defer mStats.mu.Unlock()
	return map[string]interface{}{
		"enabled": cfg.LocalClient.Enabled, "connected": mStats.connected,
		"received": mStats.received, "accepted": mStats.accepted, "rejected": mStats.rejected,
		"meters": len(mStats.meters), "last_error": mStats.lastError,
	}
}

func meterStatsSnapshot() map[string]interface{} {
	mStats.mu.Lock()
	defer mStats.mu.Unlock()
	meters := make([]meterInfo, 0, len(mStats.meters))
	for _, m := range mStats.meters {
		meters = append(meters, *m)
	}
	recent := append([]meterReading{}, mStats.recent...)
	return map[string]interface{}{
		"connected": mStats.connected, "received": mStats.received,
		"accepted": mStats.accepted, "rejected": mStats.rejected,
		"last_error": mStats.lastError, "meters": meters, "recent": recent,
	}
}

// ensureMeterClient starts the supervisor loop when the broker is enabled.
func ensureMeterClient() {
	if !cfg.LocalClient.Enabled || !cfg.LocalBroker.Enabled {
		return
	}
	meterClientMu.Lock()
	if meterStopCh != nil {
		meterClientMu.Unlock()
		return
	}
	meterStopCh = make(chan struct{})
	stopCh := meterStopCh
	meterClientMu.Unlock()
	go meterClientLoop(stopCh)
}

func stopMeterClient() {
	meterClientMu.Lock()
	if meterStopCh != nil {
		close(meterStopCh)
		meterStopCh = nil
	}
	cli := meterClient
	meterClient = nil
	meterClientMu.Unlock()
	if cli != nil && cli.IsConnected() {
		cli.Disconnect(1000)
	}
	mStats.mu.Lock()
	mStats.connected = false
	mStats.mu.Unlock()
}

func meterClientLoop(stopCh chan struct{}) {
	backoff := 2 * time.Second
	const maxBackoff = 5 * time.Minute
	for {
		select {
		case <-stopCh:
			return
		default:
		}
		if err := meterConnectOnce(stopCh); err != nil {
			mStats.mu.Lock()
			mStats.lastError = err.Error()
			mStats.mu.Unlock()
			logger.WithError(err).Warn("local meter client: connect failed, backing off")
			select {
			case <-stopCh:
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		backoff = 2 * time.Second
		// Block until disconnect or stop.
		tick := time.NewTicker(5 * time.Second)
	loop:
		for {
			select {
			case <-stopCh:
				tick.Stop()
				return
			case <-tick.C:
				meterClientMu.Lock()
				cli := meterClient
				meterClientMu.Unlock()
				if cli == nil || !cli.IsConnected() {
					tick.Stop()
					break loop
				}
			}
		}
	}
}

func meterConnectOnce(stopCh chan struct{}) error {
	if cfg.LocalClient.Username == "" || cfg.LocalClient.Password == "" {
		return fmt.Errorf("local client credentials not configured")
	}
	addr := localClientBrokerURL()
	opts := MQTT.NewClientOptions()
	opts.AddBroker(addr)
	opts.SetClientID(fmt.Sprintf("gw-%s-local", getDeviceID()))
	opts.SetUsername(cfg.LocalClient.Username)
	opts.SetPassword(cfg.LocalClient.Password)
	opts.SetCleanSession(false)
	opts.SetKeepAlive(30)
	opts.SetConnectTimeout(10 * time.Second)
	opts.SetAutoReconnect(false) // supervisor loop owns reconnect/backoff
	if strings.HasPrefix(addr, "ssl") {
		caPath := filepath.Join(cfg.LocalBroker.CertDir, "ca.crt")
		pool := x509.NewCertPool()
		if pemBytes, err := os.ReadFile(caPath); err == nil {
			pool.AppendCertsFromPEM(pemBytes)
		}
		opts.SetTLSConfig(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	}
	topics := cfg.LocalClient.Topics
	if len(topics) == 0 {
		topics = []string{"meter/#"}
	}
	cli := MQTT.NewClient(opts)
	tok := cli.Connect()
	if !tok.WaitTimeout(12*time.Second) {
		return fmt.Errorf("local broker connect timeout (%s)", addr)
	}
	if tok.Error() != nil {
		return tok.Error()
	}
	for _, t := range topics {
		stok := cli.Subscribe(t, 1, handleMeterMessage)
		if !stok.WaitTimeout(8*time.Second) || stok.Error() != nil {
			cli.Disconnect(500)
			return fmt.Errorf("subscribe %s: %v", t, stok.Error())
		}
	}
	meterClientMu.Lock()
	select {
	case <-stopCh:
		meterClientMu.Unlock()
		cli.Disconnect(500)
		return fmt.Errorf("stopped")
	default:
	}
	meterClient = cli
	meterClientMu.Unlock()
	mStats.mu.Lock()
	mStats.connected = true
	mStats.lastError = ""
	mStats.mu.Unlock()
	logger.WithField("broker", addr).Info("local meter client: connected")
	return nil
}

func handleMeterMessage(_ MQTT.Client, msg MQTT.Message) {
	topic := msg.Topic()
	payload := msg.Payload()
	mStats.mu.Lock()
	mStats.received++
	mStats.mu.Unlock()

	reading, meterID, err := validateMeterMessage(topic, payload, cfg.LocalClient.MaxPayloadBytes)
	if err != nil {
		mStats.mu.Lock()
		mStats.rejected++
		mStats.lastError = err.Error()
		mStats.mu.Unlock()
		logger.WithFields(map[string]interface{}{"topic": topic, "error": err.Error()}).Warn("local meter client: rejected message")
		return
	}

	// Queue into the offline external-device store (survives outages).
	if offlineStorage != nil {
		raw, _ := json.Marshal(reading)
		recordID := fmt.Sprintf("meter-%s-%d", meterID, time.Now().UnixNano())
		if qerr := offlineStorage.EnqueueExternal(recordID, getDeviceID(), meterID, "local-mqtt", 0, PriorityP3ExternalNormal, raw); qerr != nil {
			logger.WithError(qerr).Warn("local meter client: queue failed")
		}
	}

	// Customer forwarding pipeline: devices + telemetry + per-destination
	// queue rows in one SQLite transaction (independent from HiveMQ path).
	if fwdDB != nil && cfg.Forwarding.Enabled {
		raw, _ := json.Marshal(reading.Data)
		if ts, err := time.Parse(time.RFC3339, reading.Timestamp); err == nil {
			if _, qerr := forwardingIngest(meterID, topic, raw, ts); qerr != nil {
				logger.WithError(qerr).Warn("local meter client: forwarding ingest failed")
			}
		}
	}

	mStats.mu.Lock()
	mStats.accepted++
	mi, ok := mStats.meters[meterID]
	if !ok {
		if len(mStats.meters) >= 500 {
			// Cap cardinality: drop the oldest entry.
			for k, v := range mStats.meters {
				if mi == nil || v.LastSeen < mi.LastSeen {
					mi = v
					delete(mStats.meters, k)
					break
				}
			}
		}
		mi = &meterInfo{MeterID: meterID}
		mStats.meters[meterID] = mi
	}
	mi.Count++
	mi.LastSeen = reading.Received
	if s, ok := reading.Data["status"].(string); ok {
		mi.LastStatus = s
	}
	mStats.recent = append(mStats.recent, *reading)
	if len(mStats.recent) > 100 {
		mStats.recent = mStats.recent[len(mStats.recent)-100:]
	}
	mStats.mu.Unlock()
}

// validateMeterMessage enforces topic shape, size, JSON schema and ranges.
func validateMeterMessage(topic string, payload []byte, maxBytes int) (*meterReading, string, error) {
	if maxBytes <= 0 {
		maxBytes = 65536
	}
	if len(payload) == 0 {
		return nil, "", fmt.Errorf("empty payload")
	}
	if len(payload) > maxBytes {
		return nil, "", fmt.Errorf("payload %d bytes exceeds %d", len(payload), maxBytes)
	}
	parts := strings.Split(strings.Trim(topic, "/"), "/")
	if len(parts) != 3 || parts[0] != "meter" || parts[1] == "" {
		return nil, "", fmt.Errorf("topic must be meter/<meter_id>/{data|status|event}")
	}
	meterID := parts[1]
	kind := parts[2]
	if kind != "data" && kind != "status" && kind != "event" {
		return nil, "", fmt.Errorf("unknown meter topic kind %q", kind)
	}
	for _, r := range meterID {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return nil, "", fmt.Errorf("invalid meter id %q", meterID)
		}
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil, "", fmt.Errorf("invalid JSON: %v", err)
	}
	if id, _ := obj["meter_id"].(string); id != "" && id != meterID {
		return nil, "", fmt.Errorf("meter_id %q mismatches topic %q", id, meterID)
	}
	obj["meter_id"] = meterID
	ts, _ := obj["timestamp"].(string)
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
		obj["timestamp"] = ts
	} else if !validTimestamp(ts) {
		return nil, "", fmt.Errorf("invalid timestamp %q", ts)
	}
	if kind == "data" {
		if err := validateMeterNumerics(obj); err != nil {
			return nil, "", err
		}
	}
	return &meterReading{
		MeterID: meterID, Timestamp: ts,
		Received: time.Now().UTC().Format(time.RFC3339), Data: obj,
	}, meterID, nil
}

func validTimestamp(ts string) bool {
	if _, err := time.Parse(time.RFC3339, ts); err == nil {
		return true
	}
	// Allow unix seconds (number or numeric string).
	var f float64
	if _, err := fmt.Sscanf(strings.TrimSpace(ts), "%f", &f); err == nil && f > 1e9 && f < 5e9 {
		return true
	}
	return false
}

func validateMeterNumerics(obj map[string]interface{}) error {
	num := func(key string, min, max float64) error {
		v, ok := obj[key]
		if !ok || v == nil {
			return nil // optional
		}
		var f float64
		switch x := v.(type) {
		case float64:
			f = x
		case float32:
			f = float64(x)
		case int:
			f = float64(x)
		case int64:
			f = float64(x)
		case json.Number:
			var err error
			f, err = x.Float64()
			if err != nil {
				return fmt.Errorf("%s is not numeric", key)
			}
		default:
			return fmt.Errorf("%s is not numeric", key)
		}
		if f != f || f > 1e18 || f < -1e18 {
			return fmt.Errorf("%s out of range", key)
		}
		if f < min || f > max {
			return fmt.Errorf("%s %.2f outside [%.0f, %.0f]", key, f, min, max)
		}
		return nil
	}
	if err := num("voltage", 0, 500); err != nil {
		return err
	}
	if err := num("current", 0, 1000); err != nil {
		return err
	}
	if err := num("power", 0, 1000000); err != nil {
		return err
	}
	if err := num("energy", 0, 1e12); err != nil {
		return err
	}
	if err := num("temperature", -50, 150); err != nil {
		return err
	}
	return nil
}

// publishLocalTest publishes a test payload through the local client (loopback
// through the broker exercises the full ingest path).
func publishLocalTest(topic string, payload map[string]interface{}) error {
	meterClientMu.Lock()
	cli := meterClient
	meterClientMu.Unlock()
	if cli == nil || !cli.IsConnected() {
		return fmt.Errorf("local meter client not connected — is the broker enabled?")
	}
	raw, _ := json.Marshal(payload)
	tok := cli.Publish(topic, 1, false, raw)
	if !tok.WaitTimeout(8*time.Second) {
		return fmt.Errorf("publish timeout")
	}
	return tok.Error()
}

func randomPassword(n int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}
