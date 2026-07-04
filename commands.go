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
	Status    string      `json:"status"`
	Success   bool        `json:"success"`
	Result    interface{} `json:"result,omitempty"`
	Error     string      `json:"error,omitempty"`
	Timestamp string      `json:"timestamp"`
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

	logger.WithFields(logrus.Fields{"id": cmd.ID, "type": cmd.Type}).Info("received command")

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
	default:
		resp.Status = "rejected"
		resp.Error = fmt.Sprintf("unknown command type: %s", cmd.Type)
	}

	sendCommandResponse(resp)
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

	if err := secrets.processConfig(&newCfg); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: fmt.Sprintf("secrets: %s", err), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	data, _ := yaml.Marshal(&newCfg)
	if err := os.WriteFile(configPath(), data, 0644); err != nil {
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

	logger.WithFields(logrus.Fields{"version": version, "url": url}).Info("starting firmware update")
	os.MkdirAll(cfg.OTA.FirmwareDir, 0755)
	os.MkdirAll(cfg.OTA.BackupDir, 0755)

	binPath := filepath.Join(cfg.OTA.FirmwareDir, filename)
	if err := downloadFile(binPath, url); err != nil {
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: fmt.Sprintf("download: %s", err), Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}

	data, _ := os.ReadFile(binPath)
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	if hash != payload.Checksum {
		os.Remove(binPath)
		return CommandResponse{ID: cmd.ID, Status: "failed", Error: "checksum mismatch", Timestamp: time.Now().UTC().Format(time.RFC3339)}
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

	go func() {
		time.Sleep(1 * time.Second)
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()

	return CommandResponse{ID: cmd.ID, Status: "completed", Result: fmt.Sprintf("updated to version %s, restarting", payload.Version), Timestamp: time.Now().UTC().Format(time.RFC3339)}
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
	topic := strings.ReplaceAll(cfg.MQTT.Topics.Response, "{device_id}", getDeviceID())
	if strings.Contains(topic, "{device_id}") {
		return
	}
	resp.Success = resp.Status == "completed"
	payload, _ := json.Marshal(resp)
	mqttPublish(topic, cfg.MQTT.QoS, false, payload)
}
