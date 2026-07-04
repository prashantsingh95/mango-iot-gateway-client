package main

import (
	"bufio"
	"net"
	"os"
	"strings"
)

// ---------- Utilities ----------

func getDeviceID() string {
	state.mu.RLock()
	id := state.DeviceID
	state.mu.RUnlock()
	return id
}

func setConnected(v bool) {
	state.mu.Lock()
	state.Connected = v
	state.mu.Unlock()
}

func isConnected() bool {
	state.mu.RLock()
	defer state.mu.RUnlock()
	if mqttClient != nil {
		return mqttClient.IsConnected()
	}
	return state.Connected
}

func getSerialNumber() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Serial") {
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				return strings.TrimSpace(strings.ReplaceAll(parts[1], "\x00", ""))
			}
		}
	}
	return ""
}

func getMACAddress() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp != 0 && iface.HardwareAddr != nil && iface.Name != "lo" {
			return iface.HardwareAddr.String()
		}
	}
	return ""
}

func getModel() string {
	data, err := os.ReadFile("/proc/device-tree/model")
	if err != nil {
		return "Raspberry Pi"
	}
	return strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", ""))
}

func getManufacturer() string {
	return "Raspberry Pi Foundation"
}

func getHardwareVersion() string {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "Hardware") {
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				return strings.TrimSpace(strings.ReplaceAll(parts[1], "\x00", ""))
			}
		}
	}
	return ""
}

func getOSVersion() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.TrimSpace(strings.ReplaceAll(strings.TrimPrefix(line, "PRETTY_NAME="), "\x00", ""))
		}
	}
	return "Linux"
}

func getIPAddress() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "0.0.0.0"
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}
