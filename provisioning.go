package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

func deviceSecretPath() string { return filepath.Join(filepath.Dir(configPath()), "device.secret") }

// gatewayIDPath persists the platform UUID assigned at provisioning. The
// terminal agent authenticates with this UUID (see gatewayID()); the
// human-readable deviceId is only a fallback for never-provisioned agents.
func gatewayIDPath() string { return filepath.Join(filepath.Dir(configPath()), "gateway.id") }

func loadPersistedGatewayID() string {
	data, err := os.ReadFile(gatewayIDPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func persistGatewayID(id string) {
	if strings.TrimSpace(id) == "" {
		return
	}
	secretPath := gatewayIDPath()
	_ = os.MkdirAll(filepath.Dir(secretPath), 0755)
	if err := os.WriteFile(secretPath, []byte(strings.TrimSpace(id)), 0600); err != nil {
		logger.WithError(err).Warn("provisioning: failed to persist gateway UUID")
		return
	}
	logger.Info("provisioning: gateway UUID persisted for terminal auth")
}

// ---------- Provisioning ----------

// provisionHTTPClient bounds the registration call. The default http client
// has no timeout, so a blackholed route (flaky WiFi/4G handoff) would hang
// startup forever instead of falling through to the retry loop.
var provisionHTTPClient = &http.Client{Timeout: 20 * time.Second}

// provisionState tracks registration against the platform. MQTT connecting is
// NOT a proxy for this: the agent can hold static broker credentials and
// publish fine while the gateway row is still PROVISIONING and the
// provisioning token sits unused. Retries must continue until one of these
// terminal outcomes is reached.
//
//	0 = pending (retry)
//	1 = registered (2xx)
//	2 = refused (4xx — bad/spent token; retrying cannot help)
var provisionState atomic.Int32

// provisioningSatisfied reports whether registration should stop being retried.
func provisioningSatisfied() bool { return provisionState.Load() != 0 }

func provisionGateway() {
	if cfg.Gateway.ProvisionToken == "" || cfg.Gateway.PlatformURL == "" {
		provisionState.Store(1)
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
	resp, err := provisionHTTPClient.Post(url, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		logger.WithError(err).Warn("provisioning: request failed")
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		provisionState.Store(1)
		logger.Info("provisioning: gateway registered successfully")
		// Cache returned deviceSecret/mqtt credentials for later authenticated fetches (integrations, config)
		var out struct {
			DeviceSecret  string `json:"deviceSecret"`
			MQTTUsername  string `json:"mqttUsername"`
			MQTTPassword  string `json:"mqttPassword"`
			MqttBrokerURL string `json:"mqttBrokerUrl"`
			Gateway       struct {
				ID       string `json:"id"`
				DeviceID string `json:"deviceId"`
				TenantID string `json:"tenantId"`
			} `json:"gateway"`
		}
		if err := json.Unmarshal(raw, &out); err == nil {
			if out.Gateway.ID != "" {
				// First connect: capture the platform UUID now; the terminal
				// agent uses it for all later sessions (see gatewayID()).
				persistGatewayID(out.Gateway.ID)
				if strings.TrimSpace(cfg.Terminal.GatewayID) == "" {
					cfg.Terminal.GatewayID = strings.TrimSpace(out.Gateway.ID)
				}
			}
			if out.DeviceSecret != "" {
				setCachedDeviceSecret(out.DeviceSecret)
				// Persist 0600 for restarts (never log secret)
				secretPath := deviceSecretPath()
				_ = os.MkdirAll(filepath.Dir(secretPath), 0755)
				_ = os.WriteFile(secretPath, []byte(out.DeviceSecret), 0600)
				logger.Info("provisioning: device secret cached for integration fetches")
			}
			// Securely persist per-gateway MQTT credentials (production, fail-closed)
			// Do NOT overwrite valid existing credentials with empty values (prevent downgrade)
			// Do NOT log passwords/secrets (see §7)
			if out.MQTTUsername != "" || out.MQTTPassword != "" || out.MqttBrokerURL != "" {
				// Production: reject empty production MQTT credentials
				if isProduction() && (strings.TrimSpace(out.MQTTUsername) == "" || strings.TrimSpace(out.MQTTPassword) == "") {
					logger.Error("provisioning: rejected empty MQTT credentials in production (fail-closed, not downgrading)")
				} else if out.MQTTUsername == "" && out.MQTTPassword == "" && out.MqttBrokerURL == "" {
					logger.Warn("provisioning: no MQTT credentials in response, keeping existing")
				} else {
					// Only overwrite if new values are non-empty and not downgrading
					updated := false
					if strings.TrimSpace(out.MQTTUsername) != "" {
						if cfg.MQTT.Username != "" && out.MQTTUsername != cfg.MQTT.Username {
							logger.WithFields(logrus.Fields{"gatewayId": getDeviceID()}).Info("provisioning: MQTT username updated")
						}
						cfg.MQTT.Username = out.MQTTUsername
						updated = true
					}
					if strings.TrimSpace(out.MQTTPassword) != "" {
						cfg.MQTT.Password = out.MQTTPassword
						updated = true
					}
					if strings.TrimSpace(out.MqttBrokerURL) != "" {
						cfg.MQTT.BrokerURL = out.MqttBrokerURL
						updated = true
					}
				if updated {
					if secrets != nil {
						if err := secrets.processConfig(&cfg); err != nil {
							logger.WithError(err).Error("provisioning: failed to encrypt MQTT password")
						}
					} else {
						if data, err := yaml.Marshal(&cfg); err == nil {
							_ = os.WriteFile(configPath(), data, 0600)
						}
					}
					stampConfigRevision()
					logger.Info("provisioning: MQTT credentials persisted (0600, encrypted)")
				}
				}
			}
		}
	} else {
		fields := logrus.Fields{"status": resp.StatusCode, "response": string(raw)}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// Definitive rejection (bad/spent token, wrong tenant): stop retrying.
			provisionState.Store(2)
		}
		if resp.StatusCode == 404 {
			logger.WithFields(fields).Error("provisioning: token invalid or spent (create a fresh token to re-provision); continuing with stored credentials")
		} else {
			logger.WithFields(fields).Warn("provisioning: unexpected response")
		}
	}
}
