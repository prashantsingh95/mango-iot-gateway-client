package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

func deviceSecretPath() string { return filepath.Join(filepath.Dir(configPath()), "device.secret") }

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
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		logger.Info("provisioning: gateway registered successfully")
		// Cache returned deviceSecret/mqtt credentials for later authenticated fetches (integrations, config)
		var out struct {
			DeviceSecret  string `json:"deviceSecret"`
			MQTTUsername  string `json:"mqttUsername"`
			MQTTPassword  string `json:"mqttPassword"`
			MqttBrokerURL string `json:"mqttBrokerUrl"`
			Gateway       struct {
				DeviceID string `json:"deviceId"`
				TenantID string `json:"tenantId"`
			} `json:"gateway"`
		}
		if err := json.Unmarshal(raw, &out); err == nil {
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
						// Encrypt password via secretsManager and rewrite config 0600
						if secrets != nil {
							if err := secrets.processConfig(&cfg); err != nil {
								logger.WithError(err).Error("provisioning: failed to encrypt MQTT password")
							} else {
								// processConfig already rewrote config with ENC, but we also need to write non-password fields
								// Ensure config file 0600 with updated broker/username
								if data, err := yaml.Marshal(&cfg); err == nil {
									_ = os.WriteFile(configPath(), data, 0600)
								}
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
		if resp.StatusCode == 404 {
			logger.WithFields(fields).Error("provisioning: token invalid or spent (create a fresh token to re-provision); continuing with stored credentials")
		} else {
			logger.WithFields(fields).Warn("provisioning: unexpected response")
		}
	}
}
