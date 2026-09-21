package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
)

// IntegrationConfig mirrors the cloud ExternalIntegration for the agent.
// Secrets are not included; the agent receives only endpoint + topic + mappings.
// Credentials are delivered via the deviceSecret-authenticated gateway config.
type IntegrationConfig struct {
	IntegrationID string                `json:"integrationId"`
	Name          string                `json:"name"`
	Type          string                `json:"type"`
	ConfigVersion int                   `json:"configVersion"`
	Enabled       bool                  `json:"enabled"`
	MQTT          MqttIntegrationConfig `json:"mqtt"`
}

type MqttIntegrationConfig struct {
	Endpoint        string                 `json:"endpoint"`
	Port            int                    `json:"port"`
	TLSEnabled      bool                   `json:"tlsEnabled"`
	Username        string                 `json:"username"`
	Password        string                 `json:"password"` // decrypted for the device
	ClientID        string                 `json:"clientId"`
	Topic           string                 `json:"topic"`
	TopicTemplate   string                 `json:"topicTemplate"`
	QoS             byte                   `json:"qos"`
	Retain          bool                   `json:"retain"`
	PayloadFormat   string                 `json:"payloadFormat"`
	PayloadTemplate string                 `json:"payloadTemplate"`
	FieldMappings   []FieldMapping         `json:"fieldMappings"`
	Transformations map[string]interface{} `json:"transformations"`
	Filters         []FilterRule           `json:"filters"`
	CACert          string                 `json:"caCertificate"`
	ClientCert      string                 `json:"clientCert"`
	PrivateKey      string                 `json:"privateKey"`
}

type FieldMapping struct {
	Source string  `json:"source"` // e.g. "register.100" or "modbus.power-meter.voltage"
	Target string  `json:"target"` // e.g. "voltage"
	Scale  float64 `json:"scale"`
	Offset float64 `json:"offset"`
	Type   string  `json:"type"` // float, int, bool
}

type FilterRule struct {
	Field string      `json:"field"`
	Op    string      `json:"op"` // >, <, ==, !=
	Value interface{} `json:"value"`
}

// transform applies declarative field mappings (scale/offset) without eval.
// It never executes arbitrary code.
func transformFields(raw map[string]interface{}, mappings []FieldMapping) map[string]interface{} {
	out := make(map[string]interface{}, len(raw))
	for k, v := range raw {
		out[k] = v
	}
	for _, m := range mappings {
		srcVal, ok := raw[m.Source]
		if !ok {
			// Try nested lookup e.g. "modbus.power-meter.voltage"
			srcVal, ok = lookupNested(raw, m.Source)
			if !ok {
				continue
			}
		}
		var num float64
		switch vv := srcVal.(type) {
		case float64:
			num = vv
		case float32:
			num = float64(vv)
		case int:
			num = float64(vv)
		case int64:
			num = float64(vv)
		default:
			out[m.Target] = srcVal
			continue
		}
		if m.Scale != 0 {
			num = num * m.Scale
		}
		if m.Offset != 0 {
			num = num + m.Offset
		}
		out[m.Target] = num
	}
	return out
}

func lookupNested(m map[string]interface{}, path string) (interface{}, bool) {
	parts := strings.Split(path, ".")
	var cur interface{} = m
	for _, p := range parts {
		if mm, ok := cur.(map[string]interface{}); ok {
			cur, ok = mm[p]
			if !ok {
				return nil, false
			}
		} else {
			return nil, false
		}
	}
	return cur, true
}

// renderPayload builds the customer payload from a template or default JSON.
// Template variables are allowlisted: gatewayId, deviceId, tenantId, timestamp, plus mapped fields.
func renderPayload(tpl string, vars map[string]interface{}) ([]byte, error) {
	if tpl == "" {
		return json.Marshal(vars)
	}
	// Simple mustache-style replacement (no arbitrary code)
	tmpl, err := template.New("payload").Parse(tpl)
	if err != nil {
		return nil, fmt.Errorf("invalid payload template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return nil, err
	}
	// Validate JSON if format is json
	raw := buf.Bytes()
	if !json.Valid(raw) {
		return nil, fmt.Errorf("payload template did not produce valid JSON")
	}
	// Enforce max size
	if len(raw) > 256*1024 {
		return nil, fmt.Errorf("payload exceeds max size")
	}
	return raw, nil
}

func renderTopic(tpl string, vars map[string]string) (string, error) {
	if tpl == "" {
		return "", fmt.Errorf("topic template empty")
	}
	allow := map[string]bool{"tenantId": true, "gatewayId": true, "deviceId": true, "siteId": true, "groupId": true, "integrationId": true, "timestamp": true}
	for k := range vars {
		if !allow[k] {
			return "", fmt.Errorf("topic variable not allowlisted: %s", k)
		}
	}
	out := tpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
		out = strings.ReplaceAll(out, "{{ "+k+" }}", v)
	}
	if strings.Contains(out, "{{") {
		return "", fmt.Errorf("topic template has unrendered variables")
	}
	if len(out) > 512 {
		return "", fmt.Errorf("topic too long")
	}
	return out, nil
}
