package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
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
			DeviceSecret string `json:"deviceSecret"`
			MQTTUsername string `json:"mqttUsername"`
			MQTTPassword string `json:"mqttPassword"`
			Gateway      struct {
				DeviceID string `json:"deviceId"`
				TenantID string `json:"tenantId"`
			} `json:"gateway"`
		}
		if err := json.Unmarshal(raw, &out); err == nil && out.DeviceSecret != "" {
			setCachedDeviceSecret(out.DeviceSecret)
			// Persist 0600 for restarts (never log secret)
			secretPath := deviceSecretPath()
			_ = os.MkdirAll(filepath.Dir(secretPath), 0755)
			_ = os.WriteFile(secretPath, []byte(out.DeviceSecret), 0600)
			logger.Info("provisioning: device secret cached for integration fetches")
		}
	} else {
		logger.WithFields(logrus.Fields{"status": resp.StatusCode, "response": string(raw)}).Warn("provisioning: unexpected response")
	}
}
