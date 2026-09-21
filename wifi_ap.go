package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type wifiAPClient struct {
	IP        string `json:"ip"`
	MAC       string `json:"mac"`
	Hostname  string `json:"hostname,omitempty"`
	Reachable bool   `json:"reachable"`
}

type wifiAPStatus struct {
	Supported  bool           `json:"supported"`
	Backend    string         `json:"backend,omitempty"`
	Enabled    bool           `json:"enabled"`
	Interface  string         `json:"interface,omitempty"`
	Connection string         `json:"connection,omitempty"`
	SSID       string         `json:"ssid,omitempty"`
	Clients    []wifiAPClient `json:"clients,omitempty"`
}

// wifiAPCmdTimeout bounds every local subprocess call: a wedged nmcli/iw
// (D-Bus stall) must never occupy a command worker forever.
const wifiAPCmdTimeout = 15 * time.Second

func wifiAPRun(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), wifiAPCmdTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Run()
}

func wifiAPOutput(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wifiAPCmdTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

func wifiAPResponse(cmd CommandRequest, result interface{}, err error) CommandResponse {
	resp := CommandResponse{ID: cmd.ID, Timestamp: time.Now().UTC().Format(time.RFC3339)}
	if err != nil {
		resp.Status = "failed"
		resp.Error = err.Error()
		return resp
	}
	resp.Status = "completed"
	resp.Result = result
	return resp
}

func wifiAPBackend() string {
	if _, err := exec.LookPath("nmcli"); err == nil {
		return "networkmanager"
	}
	if _, err := exec.LookPath("hostapd"); err == nil {
		return "hostapd"
	}
	return ""
}

func wifiAPInterface() string {
	if cfg.WifiAP.Interface != "" {
		return cfg.WifiAP.Interface
	}
	out, err := wifiAPOutput("iw", "dev")
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i, field := range fields {
		if field == "Interface" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

func wifiAPStatusCommand() CommandResponse {
	cmd := CommandRequest{ID: "wifi-ap-status"}
	backend := wifiAPBackend()
	if backend == "" {
		return wifiAPResponse(cmd, nil, fmt.Errorf("neither NetworkManager (nmcli) nor hostapd is installed"))
	}
	status := wifiAPStatus{Supported: true, Backend: backend, Interface: wifiAPInterface()}
	if backend == "networkmanager" {
		name := cfg.WifiAP.Connection
		args := []string{"-t", "-f", "GENERAL.STATE,GENERAL.CONNECTION,802-11-wireless.ssid", "dev", "show"}
		out, err := wifiAPOutput("nmcli", args...)
		if err != nil {
			return wifiAPResponse(cmd, nil, fmt.Errorf("nmcli status: %w", err))
		}
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "GENERAL.STATE:") {
				status.Enabled = strings.Contains(strings.ToLower(line), "activated")
			} else if strings.HasPrefix(line, "GENERAL.CONNECTION:") && name == "" {
				name = strings.TrimSpace(strings.TrimPrefix(line, "GENERAL.CONNECTION:"))
			} else if strings.HasPrefix(line, "802-11-wireless.ssid:") {
				status.SSID = strings.TrimSpace(strings.TrimPrefix(line, "802-11-wireless.ssid:"))
			}
		}
		status.Connection = name
	} else {
		status.Enabled = systemdActive("hostapd")
		status.Connection = "hostapd"
	}
	return wifiAPResponse(cmd, status, nil)
}

func systemdActive(service string) bool {
	return wifiAPRun("systemctl", "is-active", "--quiet", service) == nil
}

func wifiAPClients() ([]wifiAPClient, error) {
	if _, err := exec.LookPath("iw"); err != nil {
		return nil, fmt.Errorf("iw is required to list connected AP clients")
	}
	iface := wifiAPInterface()
	if iface == "" {
		return nil, fmt.Errorf("AP interface is not configured or detectable")
	}
	out, err := wifiAPOutput("iw", "dev", iface, "station", "dump")
	if err != nil {
		return nil, fmt.Errorf("station list: %w", err)
	}
	var clients []wifiAPClient
	var current *wifiAPClient
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.EqualFold(fields[0], "Station") {
			if current != nil {
				clients = append(clients, *current)
			}
			current = &wifiAPClient{MAC: fields[1]}
		}
	}
	if current != nil {
		clients = append(clients, *current)
	}
	for i := range clients {
		clients[i].IP = lookupClientIP(clients[i].MAC)
		if clients[i].IP != "" {
			clients[i].Reachable = pingClient(clients[i].IP)
		}
	}
	if cfg.WifiAP.MaxClients > 0 && len(clients) > cfg.WifiAP.MaxClients {
		clients = clients[:cfg.WifiAP.MaxClients]
	}
	return clients, nil
}

func lookupClientIP(mac string) string {
	out, err := wifiAPOutput("ip", "neigh", "show", "lladdr", mac)
	if err != nil {
		return ""
	}
	for _, field := range strings.Fields(string(out)) {
		if net.ParseIP(field) != nil {
			return field
		}
	}
	return ""
}

func pingClient(ip string) bool {
	if net.ParseIP(ip) == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "ping", "-c", "1", "-W", "1", ip).Run() == nil
}

func execWifiAP(cmd CommandRequest) CommandResponse {
	if !cfg.WifiAP.Enabled {
		return wifiAPResponse(cmd, nil, fmt.Errorf("remote Wi-Fi AP management is disabled in gateway configuration"))
	}
	switch cmd.Type {
	case "wifi_ap.status":
		resp := wifiAPStatusCommand()
		resp.ID = cmd.ID
		return resp
	case "wifi_ap.clients":
		clients, err := wifiAPClients()
		return wifiAPResponse(cmd, clients, err)
	case "wifi_ap.ping_client":
		var payload struct {
			IP string `json:"ip"`
		}
		if err := json.Unmarshal(cmd.Payload, &payload); err != nil || net.ParseIP(payload.IP) == nil {
			return wifiAPResponse(cmd, nil, fmt.Errorf("a valid client IP is required"))
		}
		return wifiAPResponse(cmd, map[string]interface{}{"ip": payload.IP, "reachable": pingClient(payload.IP)}, nil)
	case "wifi_ap.enable", "wifi_ap.disable":
		backend := wifiAPBackend()
		if backend == "" {
			return wifiAPResponse(cmd, nil, fmt.Errorf("neither NetworkManager (nmcli) nor hostapd is installed"))
		}
		var err error
		if backend == "networkmanager" {
			connection := cfg.WifiAP.Connection
			if connection == "" {
				return wifiAPResponse(cmd, nil, fmt.Errorf("wifi_ap.connection is required for NetworkManager"))
			}
			action := "down"
			if cmd.Type == "wifi_ap.enable" {
				action = "up"
			}
			err = wifiAPRun("nmcli", "connection", action, connection)
		} else {
			action := "stop"
			if cmd.Type == "wifi_ap.enable" {
				action = "start"
			}
			err = wifiAPRun("systemctl", action, "hostapd")
		}
		if err != nil {
			return wifiAPResponse(cmd, nil, fmt.Errorf("%s: %w", cmd.Type, err))
		}
		return wifiAPStatusCommandWithID(cmd.ID)
	case "wifi_ap.configure":
		return wifiAPConfigure(cmd)
	default:
		return wifiAPResponse(cmd, nil, fmt.Errorf("unsupported Wi-Fi AP command: %s", cmd.Type))
	}
}

func wifiAPStatusCommandWithID(id string) CommandResponse {
	resp := wifiAPStatusCommand()
	resp.ID = id
	return resp
}

func wifiAPConfigure(cmd CommandRequest) CommandResponse {
	var payload struct {
		Connection string `json:"connection"`
		SSID       string `json:"ssid"`
		Password   string `json:"password"`
		Channel    int    `json:"channel"`
	}
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		return wifiAPResponse(cmd, nil, fmt.Errorf("invalid AP configuration"))
	}
	if len(payload.SSID) < 1 || len(payload.SSID) > 32 || len(payload.Password) < 8 || len(payload.Password) > 63 {
		return wifiAPResponse(cmd, nil, fmt.Errorf("SSID must be 1-32 characters and password must be 8-63 characters"))
	}
	if payload.Channel < 0 || payload.Channel > 196 {
		return wifiAPResponse(cmd, nil, fmt.Errorf("invalid Wi-Fi channel"))
	}
	if wifiAPBackend() != "networkmanager" {
		return wifiAPResponse(cmd, nil, fmt.Errorf("hostapd configuration requires a managed local profile; refusing direct file mutation"))
	}
	connection := payload.Connection
	if connection == "" {
		connection = cfg.WifiAP.Connection
	}
	if connection == "" {
		return wifiAPResponse(cmd, nil, fmt.Errorf("wifi_ap.connection is required"))
	}
	args := []string{"connection", "modify", connection, "802-11-wireless.ssid", payload.SSID, "802-11-wireless-security.key-mgmt", "wpa-psk", "802-11-wireless-security.psk", payload.Password}
	if payload.Channel > 0 {
		args = append(args, "802-11-wireless.channel", strconv.Itoa(payload.Channel))
	}
	if err := wifiAPRun("nmcli", args...); err != nil {
		return wifiAPResponse(cmd, nil, fmt.Errorf("configure AP: %w", err))
	}
	if err := wifiAPRun("nmcli", "connection", "up", connection); err != nil {
		return wifiAPResponse(cmd, nil, fmt.Errorf("activate AP: %w", err))
	}
	return wifiAPStatusCommandWithID(cmd.ID)
}
