package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

func startIntegrationPoller(ctx context.Context) {
	if cfg.Gateway.PlatformURL == "" {
		logger.Info("integrations: platform_url not set, skipping external integrations")
		return
	}
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	// Initial fetch
	fetchAndApplyIntegrations()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fetchAndApplyIntegrations()
		}
	}
}

func fetchAndApplyIntegrations() {
	deviceID := getDeviceID()
	// Device secret may be stored in spool or config; reuse provisioned deviceSecret if available via auth.
	// For MVP, try to read device secret from gateway state? We don't store it plaintext.
	// Instead, we use the deviceSecret that was returned at provisioning and persisted in config via secrets manager?
	// The config's deviceSecret is encrypted at rest but available in cfg after secrets.processConfig.
	// We can attempt to fetch with empty secret and rely on server's verifyDevice finding hash? No, server requires secret.
	// Workaround: if cfg.Gateway.ProvisionToken still present, we cannot fetch integrations; requires provisioned secret.
	// For now, we attempt to use the in-memory device secret if we cached it during provisionGateway response.
	secret := cachedDeviceSecret()
	if secret == "" {
		// Try to derive from stored hash? Not possible. Log and skip.
		logger.Debug("integrations: no device secret cached, skipping fetch")
		return
	}
	url := strings.TrimRight(cfg.Gateway.PlatformURL, "/") + fmt.Sprintf("/api/v1/integrations/gateway/config?deviceId=%s&deviceSecret=%s", deviceID, secret)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		logger.WithError(err).Warn("integrations: fetch failed")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		logger.WithFields(logrus.Fields{"status": resp.StatusCode, "body": string(body)}).Warn("integrations: unexpected status")
		return
	}
	var cfgs []IntegrationConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfgs); err != nil {
		logger.WithError(err).Warn("integrations: decode failed")
		return
	}
	logger.WithField("count", len(cfgs)).Info("integrations: fetched")
	// Hand off to customer MQTT manager
	customerMQTT.updateIntegrations(cfgs)
	// Also update reported config version for audit
	if len(cfgs) > 0 {
		// For MVP, just log; Phase: desired/reported version tracking would use configVersion
	}
}

// cachedDeviceSecret holds the plaintext device secret returned at provisioning.
// Guarded: written by provisionGateway (startup + background retry) and read
// by the poller goroutine.
var (
	cachedSecret   string
	cachedSecretMu sync.RWMutex
)

func cachedDeviceSecret() string {
	cachedSecretMu.RLock()
	if cachedSecret != "" {
		s := cachedSecret
		cachedSecretMu.RUnlock()
		return s
	}
	cachedSecretMu.RUnlock()
	if data, err := os.ReadFile(deviceSecretPath()); err == nil {
		if s := strings.TrimSpace(string(data)); s != "" {
			setCachedDeviceSecret(s)
			return s
		}
	}
	return ""
}

func setCachedDeviceSecret(s string) {
	cachedSecretMu.Lock()
	cachedSecret = s
	cachedSecretMu.Unlock()
}
