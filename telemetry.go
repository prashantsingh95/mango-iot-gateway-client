package main

import (
	"bufio"
	"context"
	"math"
	"net"
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
	Uptime      int64                  `json:"uptime,omitempty"`
	CPULoad1    float64                `json:"cpu_load_1,omitempty"`
	CPULoad5    float64                `json:"cpu_load_5,omitempty"`
	CPULoad15   float64                `json:"cpu_load_15,omitempty"`
	NICs        []InterfaceDetail      `json:"nics,omitempty"`
	IfaceStats  []InterfaceStat        `json:"interface_stats,omitempty"`
	Peers       []PeerSight            `json:"peers,omitempty"`
	Processes   []ProcessSight         `json:"processes,omitempty"`
	Business    map[string]interface{} `json:"business,omitempty"`
	System      map[string]interface{} `json:"system,omitempty"`
	Modbus      []ModbusValue          `json:"modbus,omitempty"`
	GPIO        map[string]interface{} `json:"gpio,omitempty"`
}

// InterfaceDetail is one NIC row for the platform inventory.
type InterfaceDetail struct {
	Name string `json:"name"`
	MAC  string `json:"mac,omitempty"`
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

// InterfaceStat carries monotonic byte counters; the platform derives
// speeds from deltas.
type InterfaceStat struct {
	Name string `json:"name"`
	Rx   uint64 `json:"rx_bytes"`
	Tx   uint64 `json:"tx_bytes"`
}

// PeerSight is one neighbor seen on a local link (ARP table).
type PeerSight struct {
	ID     string `json:"id"`
	Signal int    `json:"signal,omitempty"`
}

// ProcessSight is one supervised-process candidate (top CPU consumers).
type ProcessSight struct {
	Name  string `json:"name"`
	State string `json:"state,omitempty"`
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
	// Phase 5 / §22 — reported configuration: the platform diffs this
	// against desired state to detect drift (revision = unix apply time).
	ConfigRevision int64  `json:"config_revision"`
	ConfigHash     string `json:"config_hash"`
	// Device identity for the platform inventory + fleet map.
	BoardVendor string  `json:"board_vendor,omitempty"`
	BoardModel  string  `json:"board_model,omitempty"`
	ImageName   string  `json:"image_name,omitempty"`
	GeoLat      float64 `json:"geo_lat,omitempty"`
	GeoLng      float64 `json:"geo_lng,omitempty"`
}

// ---------- System Monitoring ----------

func collectSystemMetrics() map[string]interface{} {
	metrics := make(map[string]interface{})

	if cfg.Monitoring.CPU {
		if p, err := psCPU.Percent(0, false); err == nil && len(p) > 0 {
			// gopsutil returns percent (0-100); round to 2 decimals.
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
					"total_gb": int(usage.Total / 1024 / 1024 / 1024),
					"used_gb":  int(usage.Used / 1024 / 1024 / 1024),
					"free_gb":  int(usage.Free / 1024 / 1024 / 1024),
					"used_pct": math.Round(usage.UsedPercent*100) / 100,
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
		metrics["uptime_seconds"] = systemUptimeSeconds()
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

// systemUptimeSeconds returns OS uptime from /proc/uptime (falls back to
// process uptime when the host is not Linux or /proc is unavailable).
// The UI expects device uptime, not "seconds since agent start".
func systemUptimeSeconds() int64 {
	data, err := os.ReadFile("/proc/uptime")
	if err == nil {
		fields := strings.Fields(string(data))
		if len(fields) > 0 {
			if secs, err := strconv.ParseFloat(fields[0], 64); err == nil && secs >= 0 {
				return int64(secs)
			}
		}
	}
	return int64(time.Since(startTime).Seconds())
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
	// Bounded: a wedged vcgencmd must never stall the telemetry loop.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "vcgencmd", "measure_volts", "core").Output()
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

func getWiFiSignal() float64 {	data, err := os.ReadFile("/proc/net/wireless")
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

// toFloatNum coerces numeric readings (modbus/GPIO) to float64 for business
// telemetry. Non-numeric values are skipped (ok=false).
func toFloatNum(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

// ---------- Device observation (platform inventory) ----------

// collectInterfaceDetails lists NIC name/MAC/addresses via the stdlib.
// Loopback is skipped; failures yield an empty list, never an error.
func collectInterfaceDetails() []InterfaceDetail {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []InterfaceDetail
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		d := InterfaceDetail{Name: iface.Name, MAC: iface.HardwareAddr.String()}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		var v4, v6 []string
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if ip.To4() != nil {
				v4 = append(v4, a.String())
			} else {
				v6 = append(v6, a.String())
			}
		}
		if len(v4) > 0 {
			d.IPv4 = strings.Join(v4, ",")
		}
		if len(v6) > 0 {
			d.IPv6 = strings.Join(v6, ",")
		}
		out = append(out, d)
		if len(out) >= 16 {
			break
		}
	}
	return out
}

// collectInterfaceStats returns per-NIC monotonic byte counters.
func collectInterfaceStats() []InterfaceStat {
	io, err := psNet.IOCounters(true)
	if err != nil {
		return nil
	}
	var out []InterfaceStat
	for _, c := range io {
		if c.Name == "lo" {
			continue
		}
		out = append(out, InterfaceStat{Name: c.Name, Rx: c.BytesRecv, Tx: c.BytesSent})
		if len(out) >= 16 {
			break
		}
	}
	return out
}

// collectPeers reads /proc/net/arp: neighbors recently seen on local links.
// Entries with incomplete (zero) MACs are skipped.
func collectPeers() []PeerSight {
	data, err := os.ReadFile("/proc/net/arp")
	if err != nil {
		return nil
	}
	var out []PeerSight
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if i == 0 {
			continue // header
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		ip, mac := fields[0], fields[3]
		if mac == "00:00:00:00:00:00" || net.ParseIP(ip) == nil {
			continue
		}
		out = append(out, PeerSight{ID: ip + " (" + mac + ")"})
		if len(out) >= 64 {
			break
		}
	}
	return out
}

// collectProcesses lists distinct running process names from /proc (capped).
// The platform matches these against supervised-process watches.
func collectProcesses() []ProcessSight {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var out []ProcessSight
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		comm, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, ProcessSight{Name: name, State: "RUNNING"})
		if len(out) >= 25 {
			break
		}
	}
	return out
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
			// Storage monitoring §37: gateway data includes SD card health.
			// Must never block the publish path — a stuck SQLite query here
			// used to freeze the telemetry loop before enqueuePublish.
			if offlineStorage != nil {
				storageCh := make(chan map[string]interface{}, 1)
				go func() {
					defer func() {
						if r := recover(); r != nil {
							storageCh <- nil
						}
					}()
					storageCh <- offlineStorage.GetStorageInfo()
				}()
				select {
				case storage := <-storageCh:
					if storage != nil {
						sys["storage"] = storage
					}
				case <-time.After(2 * time.Second):
					// skip storage block; still publish CPU/RAM/etc
				}
			}
			telemetry.System = sys
			telemetry.Uptime = systemUptimeSeconds()

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
			if v, ok := sys["load_1"].(float64); ok {
				telemetry.CPULoad1 = v
			}
			if v, ok := sys["load_5"].(float64); ok {
				telemetry.CPULoad5 = v
			}
			if v, ok := sys["load_15"].(float64); ok {
				telemetry.CPULoad15 = v
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

			// Device observation for the platform inventory (bounded, best-effort).
			telemetry.NICs = collectInterfaceDetails()
			telemetry.IfaceStats = collectInterfaceStats()
			telemetry.Peers = collectPeers()
			telemetry.Processes = collectProcesses()

			// Business telemetry: numeric application readings (modbus +
			// numeric GPIO) mirrored under `business` for per-key charts.
			business := make(map[string]interface{})
			for _, v := range telemetry.Modbus {
				if f, ok := toFloatNum(v.Value); ok {
					business["modbus."+v.Name] = f
				}
			}
			for k, v := range telemetry.GPIO {
				if f, ok := toFloatNum(v); ok {
					business["gpio."+k] = f
				}
			}
			if len(business) > 0 {
				telemetry.Business = business
			}

			publishTelemetry(telemetry)
			// Customer data plane: route same modbus/system payload to external MQTT integrations via declarative pipeline
			customerRaw := map[string]interface{}{
				"deviceId":  telemetry.DeviceID,
				"gatewayId": getDeviceID(),
				"timestamp": telemetry.Timestamp,
				"system":    sys,
				"modbus":    telemetry.Modbus,
				"tenantId":  cfg.Gateway.TenantID,
			}
			// Flatten modbus values for fieldMappings like "modbus.power-meter.voltage"
			if len(telemetry.Modbus) > 0 {
				flat := make(map[string]interface{}, len(telemetry.Modbus))
				for _, v := range telemetry.Modbus {
					flat[v.Name] = v.Value
				}
				customerRaw["registers"] = flat
			}
			publishToCustomer(customerRaw, "telemetry")
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
		DeviceID:       getDeviceID(),
		Status:         status,
		Reason:         r,
		Uptime:         systemUptimeSeconds(),
		Version:        version,
		IP:             getIPAddress(),
		LastSeen:       time.Now().UTC().Format(time.RFC3339),
		FirmwareVer:    version,
		SerialNumber:   getSerialNumber(),
		Model:          getModel(),
		Manufacturer:   getManufacturer(),
		MACAddress:     getMACAddress(),
		HardwareVer:    getHardwareVersion(),
		OSVersion:      getOSVersion(),
		ConfigRevision: configRevision,
		ConfigHash:     configHash,
		BoardVendor:   getManufacturer(),
		BoardModel:    getModel(),
		ImageName:     cfg.Gateway.ImageName,
		GeoLat:        cfg.Gateway.GeoLat,
		GeoLng:        cfg.Gateway.GeoLng,
	}
	publishStatus(s)
}
