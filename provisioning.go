package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"
)

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
