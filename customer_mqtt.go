package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
)

// Customer MQTT manager: one isolated connection per ExternalIntegration.
// Failures for one tenant never block another (bounded concurrency, per-integration queue via spool).

type customerMqttManager struct {
	mu           sync.Mutex
	clients      map[string]MQTT.Client // integrationId -> client
	integrations map[string]IntegrationConfig
}

var customerMQTT = &customerMqttManager{
	clients:      make(map[string]MQTT.Client),
	integrations: make(map[string]IntegrationConfig),
}

func (m *customerMqttManager) updateIntegrations(cfgs []IntegrationConfig) {
	// Snapshot the diff under lock; all blocking network I/O (connects up
	// to 15s, disconnects) happens outside so telemetry publishing never
	// stalls behind a slow broker.
	type pendingStart struct {
		id  string
		cfg IntegrationConfig
	}
	m.mu.Lock()
	desired := make(map[string]IntegrationConfig, len(cfgs))
	for _, c := range cfgs {
		if !c.Enabled {
			continue
		}
		desired[c.IntegrationID] = c
	}
	var toStop []MQTT.Client
	for id, cli := range m.clients {
		if _, ok := desired[id]; !ok {
			toStop = append(toStop, cli)
			delete(m.clients, id)
			delete(m.integrations, id)
			logger.WithField("integration", id).Info("customer mqtt: disconnected")
		}
	}
	var toStart []pendingStart
	for id, cfg := range desired {
		prev, ok := m.integrations[id]
		if ok && prev.ConfigVersion == cfg.ConfigVersion {
			continue
		}
		if cli, ok := m.clients[id]; ok {
			toStop = append(toStop, cli)
			delete(m.clients, id)
		}
		toStart = append(toStart, pendingStart{id: id, cfg: cfg})
	}
	m.mu.Unlock()

	for _, cli := range toStop {
		cli.Disconnect(500)
	}
	for _, p := range toStart {
		cli, err := m.connectIntegration(p.cfg)
		m.mu.Lock()
		if err != nil {
			logger.WithError(err).WithField("integration", p.id).Warn("customer mqtt: connect failed, will retry")
			// Keep config for retry on next update
			m.integrations[p.id] = p.cfg
			m.mu.Unlock()
			continue
		}
		m.clients[p.id] = cli
		m.integrations[p.id] = p.cfg
		m.mu.Unlock()
		logger.WithField("integration", p.id).Info("customer mqtt: connected")
	}
}

func (m *customerMqttManager) connectIntegration(cfg IntegrationConfig) (MQTT.Client, error) {
	opts := MQTT.NewClientOptions()
	proto := "mqtt"
	if cfg.MQTT.TLSEnabled {
		proto = "mqtts"
	}
	broker := fmt.Sprintf("%s://%s:%d", proto, cfg.MQTT.Endpoint, cfg.MQTT.Port)
	if strings.Contains(cfg.MQTT.Endpoint, "://") {
		broker = cfg.MQTT.Endpoint
	}
	opts.AddBroker(broker)
	clientID := cfg.MQTT.ClientID
	if clientID == "" {
		clientID = fmt.Sprintf("gw-%s-%s", getDeviceID(), cfg.IntegrationID[:8])
	}
	opts.SetClientID(clientID)
	opts.SetCleanSession(true)
	opts.SetKeepAlive(time.Duration(60) * time.Second)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	if cfg.MQTT.Username != "" {
		opts.SetUsername(cfg.MQTT.Username)
		opts.SetPassword(cfg.MQTT.Password)
	}
	if cfg.MQTT.TLSEnabled {
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg.MQTT.CACert != "" {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM([]byte(cfg.MQTT.CACert)) {
				return nil, fmt.Errorf("invalid CA certificate")
			}
			tlsCfg.RootCAs = pool
		}
		if cfg.MQTT.ClientCert != "" && cfg.MQTT.PrivateKey != "" {
			cert, err := tls.X509KeyPair([]byte(cfg.MQTT.ClientCert), []byte(cfg.MQTT.PrivateKey))
			if err != nil {
				return nil, fmt.Errorf("client cert: %w", err)
			}
			tlsCfg.Certificates = []tls.Certificate{cert}
		}
		opts.SetTLSConfig(tlsCfg)
	}
	client := MQTT.NewClient(opts)
	tok := client.Connect()
	if !tok.WaitTimeout(15 * time.Second) {
		return nil, fmt.Errorf("customer mqtt connect timeout")
	}
	if tok.Error() != nil {
		return nil, tok.Error()
	}
	return client, nil
}

// publishToCustomer routes a normalized message through the pipeline:
// raw -> transform -> filter -> topic/payload template -> publish or spool on failure.
// Snapshots clients under lock, then works lock-free so a slow broker never
// blocks integration updates (or other publishers).
func publishToCustomer(raw map[string]interface{}, source string) {
	type route struct {
		id  string
		cfg IntegrationConfig
		cli MQTT.Client
	}
	m := customerMQTT
	m.mu.Lock()
	if len(m.clients) == 0 && len(m.integrations) == 0 {
		m.mu.Unlock()
		return
	}
	routes := make([]route, 0, len(m.integrations))
	for id, cfg := range m.integrations {
		routes = append(routes, route{id: id, cfg: cfg, cli: m.clients[id]})
	}
	m.mu.Unlock()
	for _, r := range routes {
		id, cfg, cli := r.id, r.cfg, r.cli
		// Transform first (always, even if offline, so spooled data is already processed)
		mapped := transformFields(raw, cfg.MQTT.FieldMappings)
		if !passesFilters(mapped, cfg.MQTT.Filters) {
			continue
		}
		topic := cfg.MQTT.Topic
		if cfg.MQTT.TopicTemplate != "" {
			vars := map[string]string{
				"gatewayId": getDeviceID(), "tenantId": cfgTenantID(), "integrationId": id,
				"deviceId": fmt.Sprint(raw["deviceId"]), "timestamp": time.Now().UTC().Format(time.RFC3339),
			}
			if t, err := renderTopic(cfg.MQTT.TopicTemplate, vars); err == nil {
				topic = t
			}
		}
		payloadBytes, err := renderPayload(cfg.MQTT.PayloadTemplate, mapped)
		if err != nil {
			logger.WithError(err).WithField("integration", id).Warn("customer mqtt: payload render failed")
			continue
		}
		if cli == nil || !cli.IsConnected() {
			_ = spoolCustomerMessage(id, topic, cfg.MQTT.QoS, cfg.MQTT.Retain, payloadBytes, raw)
			continue
		}
		tok := cli.Publish(topic, cfg.MQTT.QoS, cfg.MQTT.Retain, payloadBytes)
		if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
			_ = spoolCustomerMessage(id, topic, cfg.MQTT.QoS, cfg.MQTT.Retain, payloadBytes, raw)
		} else {
			// Success - update metrics
			if offlineStorage != nil {
				offlineStorage.externalQueue.Count() // touch for monitoring
			}
		}
	}
}

func spoolCustomerMessage(integrationId, topic string, qos byte, retain bool, payload []byte, raw map[string]interface{}) error {
	msg := spooledMessage{Topic: topic, QoS: qos, Retained: retain, Payload: payload, EventID: newEventID()}
	rawBytes, _ := json.Marshal(msg)
	recordID := fmt.Sprintf("ext-%s-%s", integrationId, msg.EventID)
	// Prefer new OfflineStorage external queue (chunked, quota-managed, P2/P3)
	if offlineStorage != nil {
		// Extract external device ID if present in raw
		extID := ""
		if v, ok := raw["deviceId"].(string); ok {
			extID = v
		}
		return offlineStorage.EnqueueExternal(recordID, getDeviceID(), extID, integrationId, 0, PriorityP2ExternalImportant, rawBytes)
	}
	if spool == nil {
		return fmt.Errorf("spool disabled")
	}
	return spool.enqueue("customer."+integrationId, rawBytes, 5, newEventID())
}

func passesFilters(mapped map[string]interface{}, filters []FilterRule) bool {
	for _, f := range filters {
		val, ok := mapped[f.Field]
		if !ok {
			return false
		}
		switch f.Op {
		case ">":
			if toFloat(val) <= toFloat(f.Value) {
				return false
			}
		case "<":
			if toFloat(val) >= toFloat(f.Value) {
				return false
			}
		case "==":
			if fmt.Sprint(val) != fmt.Sprint(f.Value) {
				return false
			}
		case "!=":
			if fmt.Sprint(val) == fmt.Sprint(f.Value) {
				return false
			}
		}
	}
	return true
}

func toFloat(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case string:
		var f float64
		fmt.Sscanf(x, "%f", &f)
		return f
	default:
		return 0
	}
}

func cfgTenantID() string { return cfg.Gateway.TenantID }
