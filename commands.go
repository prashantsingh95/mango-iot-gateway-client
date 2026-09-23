package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

type CommandRequest struct {
	ID        string          `json:"id"`
	CommandID string          `json:"commandId"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type CommandResponse struct {
	ID        string      `json:"id"`
	CommandID string      `json:"commandId"`
	Status    string      `json:"status"`
	Success   bool        `json:"success"`
	Result    interface{} `json:"result,omitempty"`
	Error     string      `json:"error,omitempty"`
	Timestamp string      `json:"timestamp"`
}

// PHASE 1 — command idempotency (MASTER §9/§13). MQTT may redeliver; replaying
// reboot/firmware twice is unsafe. Keep a bounded in-memory set of recently
// seen command IDs (24h TTL, 1000 entries max) and ACK duplicates without
// re-executing.
var (
	seenCommands   = make(map[string]time.Time)
	seenCommandsMu = sync.Mutex{}
	commandSem     = make(chan struct{}, 4)
)

func isDuplicateCommand(id string) bool {
	seenCommandsMu.Lock()
	defer seenCommandsMu.Unlock()
	now := time.Now()
	// opportunistic expiry
	for k, t := range seenCommands {
		if now.Sub(t) > 24*time.Hour {
			delete(seenCommands, k)
		}
	}
	if _, ok := seenCommands[id]; ok {
		return true
	}
	seenCommands[id] = now
	// bound memory
	if len(seenCommands) > 1000 {
		oldest := ""
		var oldestT time.Time
		first := true
		for k, t := range seenCommands {
			if first || t.Before(oldestT) {
				oldest, oldestT, first = k, t, false
			}
		}
		delete(seenCommands, oldest)
	}
	return false
}

func isCommandAllowed(cmdType string) bool {
	if !cfg.Commands.Enabled {
		return false
	}
	if len(cfg.Commands.Allowed) == 0 {
		return true // backwards compat: empty allowlist = all enabled types
	}
	for _, a := range cfg.Commands.Allowed {
		if a == cmdType {
			return true
		}
	}
	return false
}

// ---------- Command Handler ----------

func handleCommand(client MQTT.Client, msg MQTT.Message) {
	if len(msg.Payload()) > 1024*100 {
		logger.Warn("command payload exceeds 100KB, rejecting")
		return
	}
	var cmd CommandRequest
	if err := json.Unmarshal(msg.Payload(), &cmd); err != nil {
		logger.WithError(err).Warn("invalid command payload")
		return
	}

	if cmd.ID == "" && cmd.CommandID != "" {
		cmd.ID = cmd.CommandID
	}
	if cmd.ID == "" {
		logger.Warn("command missing ID, rejecting")
		return
	}
	if cmd.Type == "" {
		logger.Warn("command missing type, rejecting")
		return
	}

	// PHASE 1 — dedup before execute (MASTER §13). Duplicate delivery => ACK
	// without re-executing.
	if isDuplicateCommand(cmd.ID) {
		logger.WithFields(logrus.Fields{"id": cmd.ID, "type": cmd.Type}).Info("duplicate command ignored (idempotent replay)")
		sendCommandResponse(CommandResponse{
			ID: cmd.ID, Status: "completed", Success: true,
			Result:    "duplicate ignored",
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		})
		return
	}

	// PHASE 1 — enforce commands.allowed (was configured but never checked).
	if !isCommandAllowed(cmd.Type) {
		logger.WithFields(logrus.Fields{"id": cmd.ID, "type": cmd.Type}).Warn("command type not allowed")
		sendCommandResponse(CommandResponse{
			ID: cmd.ID, Status: "rejected", Error: fmt.Sprintf("command type '%s' not allowed", cmd.Type),
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		})
		return
	}

	// Bounded async command dispatch: the paho callback must never block on
	// execution (run_shell up to 30s, firmware downloads). Validation above is
	// synchronous and fast; dedup marks the ID before dispatch so a redelivery
	// racing execution still ACKs as duplicate instead of double-running.
	// (commandSem is package-level for real concurrency control — see §13.)

	logger.WithFields(logrus.Fields{"id": cmd.ID, "type": cmd.Type}).Info("received command")

	go func(cmd CommandRequest) {
		select {
		case commandSem <- struct{}{}:
		default:
			sendCommandResponse(CommandResponse{
				ID: cmd.ID, Status: "rejected", Error: "too many concurrent commands, retry later",
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}
		defer func() { <-commandSem }()

		var resp CommandResponse
		resp.ID = cmd.ID
		resp.Timestamp = time.Now().UTC().Format(time.RFC3339)

		switch cmd.Type {
		case "reboot":
			resp = execReboot(cmd)
		case "restart_agent":
			resp = execRestartAgent(cmd)
		case "update_config":
			resp = execUpdateConfig(cmd)
		case "run_shell":
			resp = execShell(cmd)
		case "update_firmware":
			resp = execFirmwareUpdate(cmd)
		case "set_relay":
			resp = execSetRelay(cmd)
		case "read_register":
			resp = execReadRegister(cmd)
		case "wifi_ap.status", "wifi_ap.enable", "wifi_ap.disable", "wifi_ap.configure", "wifi_ap.clients", "wifi_ap.ping_client":
			resp = execWifiAP(cmd)
		default:
			resp.Status = "rejected"
			resp.Error = fmt.Sprintf("unknown command type: %s", cmd.Type)
		}

		sendCommandResponse(resp)
	}(cmd)
}

func execReboot(cmd CommandRequest) CommandResponse {
	logger.Warn("executing reboot command")
	go func() {
		time.Sleep(2 * time.Second)
		exec.Command("sudo", "reboot").Run()
	}()
	return CommandResponse{ID: cmd.ID, Status: "accepted", Result: "rebooting in 2s", Timestamp: time.Now().UTC().Format(time.RFC3339)}
}

func execRestartAgent(cmd CommandRequest) CommandResponse {
	logger.Warn("executing agent restart")
	go func() {
		time.Sleep(1 * time.Second)
		os.Exit(0)
	}()
	return CommandResponse{ID: cmd.ID, Status: "accepted", Result: "restarting", Timestamp: time.Now().UTC().Format(time.RFC3339)}
}

func execUpdateConfig(cmd CommandRequest) CommandResponse {
	var newCfg Config
	if err := json.Unmarshal(cmd.Payload, &newCfg); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: fmt.Sprintf("invalid config: %s", err), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	// Phase 5 / §22 — identity and tenant binding are platform-owned. A remote
	// config push that tries to move this gateway is rejected outright.
	if newCfg.Gateway.DeviceID != "" && newCfg.Gateway.DeviceID != cfg.Gateway.DeviceID && cfg.Gateway.DeviceID != "" {
		return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "device_id is immutable via remote config", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}
	if newCfg.Gateway.TenantID != "" && newCfg.Gateway.TenantID != cfg.Gateway.TenantID {
		return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "tenant_id is immutable via remote config", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}
	newCfg.Gateway.DeviceID = cfg.Gateway.DeviceID
	newCfg.Gateway.TenantID = cfg.Gateway.TenantID

	if err := secrets.processConfig(&newCfg); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: fmt.Sprintf("secrets: %s", err), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	data, _ := yaml.Marshal(&newCfg)
	if err := os.WriteFile(configPath(), data, 0600); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: err.Error(), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}
	return CommandResponse{ID: cmd.ID, Status: "completed", Result: "config updated, restart agent to apply", Timestamp: time.Now().UTC().Format(time.RFC3339)}
}

func execShell(cmd CommandRequest) CommandResponse {
	var payload struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil || payload.Command == "" {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: "invalid shell command payload", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	allowed := false

	safeCommands := []string{"ls", "ps", "df", "free", "uptime", "cat /sys/class/thermal/thermal_zone0/temp", "ifconfig", "ip a", "systemctl status gateway-agent"}
	for _, s := range safeCommands {
		if payload.Command == s {
			allowed = true
			break
		}
	}

	if !allowed {
		tokens := strings.Fields(payload.Command)
		if len(tokens) == 0 {
			return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "empty command", Timestamp: time.Now().UTC().Format(time.RFC3339)}
		}
		cmdToken := tokens[0]
		cleaned := filepath.Clean(cmdToken)
		for _, p := range cfg.Commands.Shell.AllowedPaths {
			if strings.HasPrefix(cleaned, p) {
				allowed = true
				break
			}
		}
	}

	if !allowed {
		return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "command not in allowed paths", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.Commands.Shell.Timeout)*time.Second)
	defer cancel()

	tokens := strings.Fields(payload.Command)
	if len(tokens) == 0 {
		return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "empty command", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	var cmdObj *exec.Cmd
	if len(tokens) == 1 {
		cmdObj = exec.CommandContext(ctx, tokens[0])
	} else {
		cmdObj = exec.CommandContext(ctx, tokens[0], tokens[1:]...)
	}
	out, err := cmdObj.CombinedOutput()
	if err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: err.Error(), Result: string(out), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}
	return CommandResponse{ID: cmd.ID, Status: "completed", Result: string(out), Timestamp: time.Now().UTC().Format(time.RFC3339)}
}

func execFirmwareUpdate(cmd CommandRequest) CommandResponse {
	var payload struct {
		URL         string `json:"url"`
		DownloadURL string `json:"downloadUrl"`
		Checksum    string `json:"checksum"`
		Signature   string `json:"signature"` // hex ed25519 signature over raw binary (required when ota.signing_key set)
		Version     string `json:"version"`
		FirmwareID  string `json:"firmwareId"`
		Filename    string `json:"filename"`
	}
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: "invalid payload", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	url := payload.URL
	if url == "" {
		url = payload.DownloadURL
	}
	version := payload.Version
	if version == "" && payload.FirmwareID != "" {
		version = payload.FirmwareID
	}
	filename := payload.Filename
	if filename == "" && version != "" {
		filename = fmt.Sprintf("gateway-agent-%s", version)
	}

	if url == "" {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: "missing url/downloadUrl", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}
	if payload.Checksum == "" {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: "checksum required", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}
	// P0 #3: Mandatory Ed25519 — production OTA must be signed. No unsigned fallback.
	if payload.Signature == "" {
		return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "signature required (OTA must be signed)", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}
	if cfg.OTA.SigningKey == "" {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: "ota.signing_key not configured — cannot verify signature", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	logger.WithFields(logrus.Fields{"version": version, "url": url}).Info("starting firmware update")
	os.MkdirAll(cfg.OTA.FirmwareDir, 0755)
	os.MkdirAll(cfg.OTA.BackupDir, 0755)

	binPath := filepath.Join(cfg.OTA.FirmwareDir, filename)
	if err := downloadFile(binPath, url); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: fmt.Sprintf("download: %s", err), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	data, _ := os.ReadFile(binPath)
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	if !strings.EqualFold(hash, payload.Checksum) {
		os.Remove(binPath)
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: "checksum mismatch", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}
	if cfg.OTA.SigningKey != "" {
		if err := verifyArtifactSignature(data, payload.Signature, cfg.OTA.SigningKey); err != nil {
			os.Remove(binPath)
			return CommandResponse{ID: cmd.ID, Status: "failed", Error: fmt.Sprintf("signature: %s", err), Timestamp: time.Now().UTC().Format(time.RFC3339)}
		}
	} else {
		logger.Warn("ota: no signing key configured — checksum-only verification (set ota.signing_key)")
	}

	if err := os.Chmod(binPath, 0755); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: err.Error(), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	selfPath, _ := os.Executable()
	backupPath := filepath.Join(cfg.OTA.BackupDir, "gateway-agent.bak")
	os.Remove(backupPath)
	if data, err := os.ReadFile(selfPath); err == nil {
		os.WriteFile(backupPath, data, 0755)
	}

	if err := os.Rename(binPath, selfPath); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: err.Error(), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	// Phase 5 / §19 — arm the boot health gate: the new binary must check in
	// with the cloud inside the rollback window or the backup is restored.
	armOtaPendingMarker(version)

	go func() {
		time.Sleep(1 * time.Second)
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()

	return CommandResponse{ID: cmd.ID, Status: "completed", Result: fmt.Sprintf("updated to version %s, restarting (health-gated)", payload.Version), Timestamp: time.Now().UTC().Format(time.RFC3339)}
}

func execSetRelay(cmd CommandRequest) CommandResponse {
	var payload struct {
		Name  string `json:"name"`
		State bool   `json:"state"`
	}
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: "invalid payload", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	pin := -1
	for _, s := range cfg.GPIO.Sensors {
		if s.Name == payload.Name && s.Mode == "output" {
			pin = s.Pin
			break
		}
	}
	if pin < 0 {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: fmt.Sprintf("relay '%s' not found", payload.Name), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	val := "0"
	if payload.State {
		val = "1"
	}
	gpioPath := fmt.Sprintf("/sys/class/gpio/gpio%d/value", pin)
	if err := os.WriteFile(gpioPath, []byte(val), 0644); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: fmt.Sprintf("gpio write: %s", err), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}
	return CommandResponse{ID: cmd.ID, Status: "completed", Result: map[string]interface{}{"relay": payload.Name, "state": payload.State}, Timestamp: time.Now().UTC().Format(time.RFC3339)}
}

func execReadRegister(cmd CommandRequest) CommandResponse {
	var payload struct {
		Device string `yaml:"device"`
		Name   string `yaml:"name"`
	}
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: "invalid payload", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	modbusMu.RLock()
	handler, ok := modbusPools[payload.Device]
	modbusMu.RUnlock()
	if !ok {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: fmt.Sprintf("device '%s' not connected", payload.Device), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	for _, reg := range handler.device.Registers {
		if reg.Name == payload.Name {
			val, err := handler.readRegister(reg)
			if err != nil {
				return CommandResponse{ID: cmd.ID, Status: "failed", Error: err.Error(), Timestamp: time.Now().UTC().Format(time.RFC3339)}
			}
			return CommandResponse{ID: cmd.ID, Status: "completed", Result: val, Timestamp: time.Now().UTC().Format(time.RFC3339)}
		}
	}
	return CommandResponse{ID: cmd.ID, Status: "failed", Error: fmt.Sprintf("register '%s' not found in device '%s'", payload.Name, payload.Device), Timestamp: time.Now().UTC().Format(time.RFC3339)}
}

func sendCommandResponse(resp CommandResponse) {
	if resp.ID == "" {
		return
	}
	resp.CommandID = resp.ID
	topic := strings.ReplaceAll(cfg.MQTT.Topics.Response, "{device_id}", getDeviceID())
	if strings.Contains(topic, "{device_id}") {
		return
	}
	resp.Success = resp.Status == "completed"
	payload, _ := json.Marshal(resp)
	// Non-blocking with durable fallback: responses survive outages too.
	enqueuePublish("command-response", topic, cfg.MQTT.QoS, false, payload, 10, "resp-"+resp.ID)
}
