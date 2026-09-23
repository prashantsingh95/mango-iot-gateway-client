package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/sirupsen/logrus"
)

// ---------- MQTT ----------

func mqttConnect() error {
	// Production-safe validation: fail-closed, never anonymous in production (§3, §5)
	if err := ValidateMQTTConfig(cfg.MQTT); err != nil {
		// Safe log: never expose password/secret (see §7)
		logger.WithFields(logrus.Fields{"env": effectiveEnvironment(), "broker": cfg.MQTT.BrokerURL, "clientIdPrefix": cfg.MQTT.ClientIDPrefix}).Errorf("mqtt: production validation failed: %v (not connecting anonymously, retry provisioning)", err)
		setConnected(false)
		return fmt.Errorf("mqtt validation: %w", err)
	}

	deviceID := getDeviceID()

	opts := MQTT.NewClientOptions()
	opts.AddBroker(cfg.MQTT.BrokerURL)
	// PHASE 1 — stable client ID (MASTER §20). Random suffixes cause broker
	// session flapping and lose durable subscriptions. One stable ID per
	// gateway lets the broker hold QoS1 offline messages across reconnects.
	opts.SetClientID(fmt.Sprintf("%s-%s", cfg.MQTT.ClientIDPrefix, deviceID))
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
	// PHASE 1 — Last Will (MASTER §20). Retained OFFLINE status ensures the
	// platform marks the gateway OFFLINE on abrupt power loss, instead of
	// leaving a stale retained ONLINE.
	willTopic := strings.ReplaceAll(cfg.MQTT.Topics.Status, "{device_id}", deviceID)
	willPayload, _ := json.Marshal(map[string]interface{}{
		"status":    "OFFLINE",
		"reason":    "connection_lost",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
	opts.SetWill(willTopic, string(willPayload), cfg.MQTT.QoS, true)
	opts.SetOnConnectHandler(func(c MQTT.Client) {
		logger.WithFields(spoolLogFields(spool)).Info("MQTT connected")
		setConnected(true)
		subscribeCommands()
		sendStatus("ONLINE")
		// New binary proved it runs and reaches the cloud: OTA health gate passes.
		clearOtaPendingMarker()
		// Phase 5 / §20 — drain the offline spool in order after every
		// (re)connect, then report state sync.
		go flushSpool()
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
	enqueuePublish("telemetry", topic, cfg.MQTT.QoS, false, payload, 5, newEventID())
}

func publishStatus(status StatusData) {
	topic := strings.ReplaceAll(cfg.MQTT.Topics.Status, "{device_id}", getDeviceID())
	payload, _ := json.Marshal(status)
	enqueuePublish("status", topic, cfg.MQTT.QoS, true, payload, 10, newEventID())
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
	enqueuePublish("log", topic, 0, false, payload, 1, newEventID())
}

// ---------- Async publisher (real-time, non-blocking) ----------

// publishCh decouples metric/command collection from network I/O: callers
// never block on the broker (mqttPublish waits up to 5s per message).
// One FIFO sender preserves global publish order; failures fall back to
// the durable spool. A full channel also spills to spool — never blocks.
const publishQueueSize = 512

type asyncPub struct {
	etype    string
	topic    string
	qos      byte
	retained bool
	payload  []byte
	priority int
	eventID  string
}

var publishCh = make(chan asyncPub, publishQueueSize)

func startPublisher(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				// Best-effort: persist leftovers durably instead of dropping.
				for {
					select {
					case m := <-publishCh:
						spoolOrDrop(m.etype, m.topic, m.qos, m.retained, m.payload, m.priority, m.eventID)
					default:
						return
					}
				}
			case m := <-publishCh:
				if err := mqttPublish(m.topic, m.qos, m.retained, m.payload); err != nil {
					spoolOrDrop(m.etype, m.topic, m.qos, m.retained, m.payload, m.priority, m.eventID)
				}
			}
		}
	}()
}

// enqueuePublish never blocks: overflow spills straight to the durable spool.
func enqueuePublish(etype, topic string, qos byte, retained bool, payload []byte, priority int, eventID string) {
	if eventID == "" {
		eventID = newEventID()
	}
	m := asyncPub{etype: etype, topic: topic, qos: qos, retained: retained, payload: payload, priority: priority, eventID: eventID}
	select {
	case publishCh <- m:
	default:
		spoolOrDrop(etype, topic, qos, retained, payload, priority, eventID)
	}
}

// waitPublisherEmpty lets queued messages reach the broker before shutdown.
func waitPublisherEmpty(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for len(publishCh) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
}

// runSpoolFlushLoop periodically retries the spool (covers flaky links where
// the client reports connected but publishes fail, plus process-lifetime drift).
func runSpoolFlushLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !isConnected() {
				continue
			}
			depth := 0
			if spool != nil {
				depth += spool.count()
			}
			if offlineStorage != nil {
				depth += offlineStorage.gatewayQueue.Count() + offlineStorage.externalQueue.Count()
			}
			if depth > 0 {
				flushSpool()
			}
		}
	}
}

type spooledMessage struct {
	Topic    string `json:"topic"`
	QoS      byte   `json:"qos"`
	Retained bool   `json:"retained"`
	Payload  []byte `json:"payload"`
	EventID  string `json:"event_id"`
}

// spoolOrDrop persists an event when the broker is unreachable. eventID should
// be the message's stable ID when the caller has one (server dedupes on it).
// Uses OfflineStorage gateway queue (P0/P1) if available, falling back to legacy spool.
func spoolOrDrop(etype, topic string, qos byte, retained bool, payload []byte, priority int, eventID string) {
	msg := spooledMessage{Topic: topic, QoS: qos, Retained: retained, Payload: payload, EventID: eventID}
	raw, _ := json.Marshal(msg)
	if eventID == "" {
		eventID = newEventID()
	}
	// Prefer new OfflineStorage gateway queue (chunked, quota-managed)
	if offlineStorage != nil {
		gwPriority := PriorityP1GatewayImportant
		if etype == "status" || etype == "critical" {
			gwPriority = PriorityP0GatewayCritical
		}
		if priority > 0 {
			gwPriority = priority // allow caller override
		}
		if err := offlineStorage.EnqueueGateway("gw-"+eventID, getDeviceID(), gwPriority, raw); err != nil {
			logger.WithError(err).Warn("gateway queue enqueue failed: event dropped")
			return
		}
		logger.WithFields(logrus.Fields{"queue": "gateway", "state": offlineStorage.storageMgr.GetState()}).Debug("gateway data spooled while offline")
		return
	}
	if spool == nil {
		logger.WithField("type", etype).Warn("mqtt offline and spool disabled: event dropped")
		return
	}
	if err := spool.enqueue(etype, raw, priority, "spool-"+eventID); err != nil {
		logger.WithError(err).Warn("spool enqueue failed: event dropped")
		return
	}
	logger.WithFields(spoolLogFields(spool)).Debug("event spooled while offline")
}

// flushSpool uploads queued events in priority+insertion order, acking only
// what the broker accepted. Runs after every connect and on an interval.
// Handles both legacy spool and new OfflineStorage gateway queue.
func flushSpool() {
	// Prefer new OfflineStorage gateway queue
	if offlineStorage != nil {
		flushGatewayQueue()
		flushExternalQueue()
		return
	}
	if spool == nil {
		return
	}
	batch := cfg.Queue.FlushBatch
	if batch <= 0 {
		batch = 100
	}
	for {
		events, err := spool.fetchBatch(batch)
		if err != nil {
			logger.WithError(err).Warn("spool fetch failed")
			return
		}
		if len(events) == 0 {
			return
		}
		var acked []int64
		var failed []int64
		for _, e := range events {
			var msg spooledMessage
			if err := json.Unmarshal(e.payload, &msg); err != nil {
				acked = append(acked, e.id) // poison row: drop, don't loop forever
				continue
			}
			if err := mqttPublish(msg.Topic, msg.QoS, msg.Retained, msg.Payload); err != nil {
				failed = append(failed, e.id)
				break // stop at first failure to preserve ordering
			}
			acked = append(acked, e.id)
		}
		if err := spool.ack(acked); err != nil {
			logger.WithError(err).Warn("spool ack failed")
			return
		}
		spool.bumpAttempts(failed)
		if len(failed) > 0 {
			return // broker went away mid-flush; next connect resumes
		}
	}
}

func flushGatewayQueue() {
	if offlineStorage == nil || offlineStorage.gatewayQueue == nil {
		return
	}
	// Reset any rows stuck SENDING from a prior crash/failed flush.
	_ = offlineStorage.gatewayQueue.RecoverSending()
	batch := cfg.Queue.FlushBatch
	if batch <= 0 {
		batch = 100
	}
	for {
		records, err := offlineStorage.gatewayQueue.FetchBatch(batch)
		if err != nil {
			logger.WithError(err).Warn("gateway queue fetch failed")
			return
		}
		if len(records) == 0 {
			return
		}
		var acked []int64
		var failed []int64
		for _, r := range records {
			var msg spooledMessage
			if err := json.Unmarshal(r.Payload, &msg); err != nil {
				acked = append(acked, r.ID)
				continue
			}
			if err := mqttPublish(msg.Topic, msg.QoS, msg.Retained, msg.Payload); err != nil {
				failed = append(failed, r.ID)
				break // stop at first failure to preserve ordering
			}
			acked = append(acked, r.ID)
		}
		_ = offlineStorage.gatewayQueue.Ack(acked)
		if len(failed) > 0 {
			// Return failed rows to PENDING so the next flush retries them —
			// leaving them SENDING permanently stalls the queue.
			_ = offlineStorage.gatewayQueue.RecoverSending()
			return
		}
	}
}

func flushExternalQueue() {
	if offlineStorage == nil || offlineStorage.externalQueue == nil {
		return
	}
	batch := cfg.Queue.FlushBatch
	if batch <= 0 {
		batch = 100
	}
	for {
		records, err := offlineStorage.externalQueue.FetchBatch(batch)
		if err != nil {
			logger.WithError(err).Warn("external queue fetch failed")
			return
		}
		if len(records) == 0 {
			return
		}
		var acked []int64
		var failed []int64
		for _, r := range records {
			// External device data: payload is already customer MQTT spooledMessage
			var msg spooledMessage
			if err := json.Unmarshal(r.Payload, &msg); err != nil {
				acked = append(acked, r.ID)
				continue
			}
			// Publish via customer MQTT manager - need to find integration for this payload
			// For MVP, try Mango MQTT as fallback, then customer MQTT if topic indicates customer
			var err error
			if strings.HasPrefix(msg.Topic, "factory/") || strings.Contains(msg.Topic, "customer") {
				// External device data - publish via customer MQTT
				// Extract integration ID from record
				integrationID := ""
				if r.IntegrationID.Valid {
					integrationID = r.IntegrationID.String
				}
				if integrationID != "" {
					customerMQTT.mu.Lock()
					cli, ok := customerMQTT.clients[integrationID]
					customerMQTT.mu.Unlock()
					if ok && cli.IsConnected() {
						tok := cli.Publish(msg.Topic, msg.QoS, msg.Retained, msg.Payload)
						tok.WaitTimeout(5 * time.Second)
						err = tok.Error()
					} else {
						err = fmt.Errorf("customer mqtt not connected")
					}
				} else {
					err = mqttPublish(msg.Topic, msg.QoS, msg.Retained, msg.Payload)
				}
			} else {
				err = mqttPublish(msg.Topic, msg.QoS, msg.Retained, msg.Payload)
			}
			if err != nil {
				failed = append(failed, r.ID)
				break
			}
			acked = append(acked, r.ID)
		}
		_ = offlineStorage.externalQueue.Ack(acked)
		if len(failed) > 0 {
			return
		}
	}
}
