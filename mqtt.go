package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
)

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
