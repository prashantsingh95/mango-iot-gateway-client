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
	m.mu.Lock()
	defer m.mu.Unlock()
	desired := make(map[string]IntegrationConfig, len(cfgs))
	for _, c := range cfgs {
		if !c.Enabled {
			continue
		}
		desired[c.IntegrationID] = c
	}
	// Stop removed
	for id, cli := range m.clients {
		if _, ok := desired[id]; !ok {
			cli.Disconnect(500)
			delete(m.clients, id)
			delete(m.integrations, id)
			logger.WithField("integration", id).Info("customer mqtt: disconnected")
		}
	}
	// Start/update
	for id, cfg := range desired {
		prev, ok := m.integrations[id]
		if ok && prev.ConfigVersion == cfg.ConfigVersion {
			continue
		}
		if cli, ok := m.clients[id]; ok {
			cli.Disconnect(500)
			delete(m.clients, id)
		}
		cli, err := m.connectIntegration(cfg)
		if err != nil {
			logger.WithError(err).WithField("integration", id).Warn("customer mqtt: connect failed, will retry")
			// Keep config for retry on next update
			m.integrations[id] = cfg
			continue
		}
		m.clients[id] = cli
		m.integrations[id] = cfg
		logger.WithField("integration", id).Info("customer mqtt: connected")
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
func publishToCustomer(raw map[string]interface{}, source string) {
	customerMQTT.mu.Lock()
	defer customerMQTT.mu.Unlock()
	if len(customerMQTT.clients) == 0 && len(customerMQTT.integrations) == 0 {
		return
	}
	for id, cfg := range customerMQTT.integrations {
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
		cli, ok := customerMQTT.clients[id]
		if !ok || !cli.IsConnected() {
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

func spoolOrCustomerEnqueue(integrationId string, payload []byte) error {
	// Legacy shim: wrap payload as spooledMessage with dummy topic
	return spoolCustomerMessage(integrationId, "customer/"+integrationId, 1, false, payload, nil)
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
