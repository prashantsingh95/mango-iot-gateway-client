package main

import (
	"bufio"
	"context"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	psCPU "github.com/shirou/gopsutil/v3/cpu"
	psDisk "github.com/shirou/gopsutil/v3/disk"
	psLoad "github.com/shirou/gopsutil/v3/load"
	psMem "github.com/shirou/gopsutil/v3/mem"
	psNet "github.com/shirou/gopsutil/v3/net"
)

type TelemetryData struct {
	DeviceID    string                 `json:"device_id"`
	Timestamp   string                 `json:"timestamp"`
	CPU         float64                `json:"cpu,omitempty"`
	Memory      float64                `json:"memory,omitempty"`
	Disk        float64                `json:"disk,omitempty"`
	Temperature float64                `json:"temperature,omitempty"`
	Signal      float64                `json:"signal,omitempty"`
	Voltage     float64                `json:"voltage,omitempty"`
	Battery     float64                `json:"battery,omitempty"`
	System      map[string]interface{} `json:"system,omitempty"`
	Modbus      []ModbusValue          `json:"modbus,omitempty"`
	GPIO        map[string]interface{} `json:"gpio,omitempty"`
}

type StatusData struct {
	DeviceID     string `json:"device_id"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	Uptime       int64  `json:"uptime"`
	Version      string `json:"version"`
	IP           string `json:"ip"`
	LastSeen     string `json:"last_seen"`
	FirmwareVer  string `json:"firmware_version"`
	SerialNumber string `json:"serial_number,omitempty"`
	Model        string `json:"model,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	MACAddress   string `json:"mac_address,omitempty"`
	HardwareVer  string `json:"hardware_version,omitempty"`
	OSVersion    string `json:"os_version,omitempty"`
}

// ---------- System Monitoring ----------

func collectSystemMetrics() map[string]interface{} {
	metrics := make(map[string]interface{})

	if cfg.Monitoring.CPU {
		if p, err := psCPU.Percent(0, false); err == nil && len(p) > 0 {
			metrics["cpu_percent"] = math.Round(p[0]*100) / 100
		}
		if l, err := psLoad.Avg(); err == nil {
			metrics["load_1"] = math.Round(l.Load1*100) / 100
			metrics["load_5"] = math.Round(l.Load5*100) / 100
			metrics["load_15"] = math.Round(l.Load15*100) / 100
		}
	}

	if cfg.Monitoring.Memory {
		if m, err := psMem.VirtualMemory(); err == nil {
			metrics["memory_total_mb"] = int(m.Total / 1024 / 1024)
			metrics["memory_used_mb"] = int(m.Used / 1024 / 1024)
			metrics["memory_percent"] = math.Round(m.UsedPercent*100) / 100
		}
		if s, err := psMem.SwapMemory(); err == nil {
			if s.Total > 0 {
				metrics["swap_total_mb"] = int(s.Total / 1024 / 1024)
				metrics["swap_used_mb"] = int(s.Used / 1024 / 1024)
			}
		}
	}

	if cfg.Monitoring.Disk {
		diskMetrics := make(map[string]interface{})
		partitions, _ := psDisk.Partitions(false)
		for _, p := range partitions {
			if usage, err := psDisk.Usage(p.Mountpoint); err == nil {
				diskMetrics[p.Mountpoint] = map[string]interface{}{
					"total_gb":  int(usage.Total / 1024 / 1024 / 1024),
					"used_gb":   int(usage.Used / 1024 / 1024 / 1024),
					"free_gb":   int(usage.Free / 1024 / 1024 / 1024),
					"used_pct":  math.Round(usage.UsedPercent*100) / 100,
				}
			}
		}
		metrics["disk"] = diskMetrics
	}

	if cfg.Monitoring.Temperature {
		temp := getCPUTemperature()
		if temp >= 0 {
			metrics["temperature_c"] = temp
		}
	}

	if cfg.Monitoring.Network {
		if io, err := psNet.IOCounters(false); err == nil && len(io) > 0 {
			metrics["network_rx_bytes"] = io[0].BytesRecv
			metrics["network_tx_bytes"] = io[0].BytesSent
		}
		metrics["uptime_seconds"] = int64(time.Since(startTime).Seconds())
		if signal := getWiFiSignal(); signal != 0 {
			metrics["signal_dbm"] = signal
		}
	}

	if cfg.Monitoring.CPU {
		if voltage := getCoreVoltage(); voltage > 0 {
			metrics["voltage_v"] = voltage
		}
	}

	return metrics
}

func getCPUTemperature() float64 {
	data, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return -1
	}
	tempStr := strings.TrimSpace(string(data))
	temp, err := strconv.ParseFloat(tempStr, 64)
	if err != nil {
		return -1
	}
	return temp / 1000.0
}

func getCoreVoltage() float64 {
	data, err := exec.Command("vcgencmd", "measure_volts", "core").Output()
	if err != nil {
		return 0
	}
	parts := strings.Split(strings.TrimSpace(string(data)), "=")
	if len(parts) != 2 {
		return 0
	}
	vStr := strings.TrimSuffix(strings.TrimSpace(parts[1]), "V")
	v, err := strconv.ParseFloat(vStr, 64)
	if err != nil {
		return 0
	}
	return v
}

func getWiFiSignal() float64 {
	data, err := os.ReadFile("/proc/net/wireless")
	if err != nil {
		return 0
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "wlan") {
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				signalStr := strings.TrimRight(fields[3], ".")
				signal, err := strconv.ParseFloat(signalStr, 64)
				if err == nil {
					return signal
				}
			}
		}
	}
	return 0
}

// ---------- Main Loop ----------

func runTelemetryLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(cfg.Monitoring.Interval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			telemetry := TelemetryData{
				DeviceID:  getDeviceID(),
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			}

			sys := collectSystemMetrics()
			telemetry.System = sys

			if v, ok := sys["cpu_percent"].(float64); ok {
				telemetry.CPU = v
			}
			if v, ok := sys["memory_percent"].(float64); ok {
				telemetry.Memory = v
			}
			if disk, ok := sys["disk"].(map[string]interface{}); ok {
				if rootDisk, ok := disk["/"].(map[string]interface{}); ok {
					if v, ok := rootDisk["used_pct"].(float64); ok {
						telemetry.Disk = v
					}
				}
			}
			if v, ok := sys["temperature_c"].(float64); ok {
				telemetry.Temperature = v
			}
			if v, ok := sys["signal_dbm"].(float64); ok {
				telemetry.Signal = v
			}
			if v, ok := sys["voltage_v"].(float64); ok {
				telemetry.Voltage = v
			}
			if v, ok := sys["battery_percent"].(float64); ok {
				telemetry.Battery = v
			}

			if telemetry.CPU > float64(cfg.Monitoring.CPUThresholdWarn) {
				logger.WithField("cpu", telemetry.CPU).Warn("CPU threshold exceeded")
			}
			if telemetry.Memory > float64(cfg.Monitoring.MemoryThresholdWarn) {
				logger.WithField("memory", telemetry.Memory).Warn("Memory threshold exceeded")
			}
			if telemetry.Temperature > float64(cfg.Monitoring.TempThresholdWarn) {
				logger.WithField("temperature", telemetry.Temperature).Warn("Temperature threshold exceeded")
			}

			if cfg.Modbus.Enabled && modbusCol != nil {
				telemetry.Modbus = modbusCol.getAll()
			}

			publishTelemetry(telemetry)
			state.mu.Lock()
			state.LastTelemetry = time.Now()
			state.mu.Unlock()
		}
	}
}

func sendStatus(status string, reason ...string) {
	r := ""
	if len(reason) > 0 {
		r = reason[0]
	}
	s := StatusData{
		DeviceID:     getDeviceID(),
		Status:       status,
		Reason:       r,
		Uptime:       int64(time.Since(startTime).Seconds()),
		Version:      version,
		IP:           getIPAddress(),
		LastSeen:     time.Now().UTC().Format(time.RFC3339),
		FirmwareVer:  version,
		SerialNumber: getSerialNumber(),
		Model:        getModel(),
		Manufacturer: getManufacturer(),
		MACAddress:   getMACAddress(),
		HardwareVer:  getHardwareVersion(),
		OSVersion:    getOSVersion(),
	}
	publishStatus(s)
}
