package main

import (
	"os"
	"strings"
	"sync"
	"time"

)

// ---------- State ----------

type AgentState struct {
	mu              sync.RWMutex
	DeviceID        string    `json:"device_id"`
	Connected       bool      `json:"connected"`
	Uptime          int64     `json:"uptime"`
	FirmwareVersion string    `json:"firmware_version"`
	LastTelemetry   time.Time `json:"last_telemetry"`
	LastHeartbeat   time.Time `json:"last_heartbeat"`
}

type ModbusValue struct {
	Name  string      `json:"name"`
	Value interface{} `json:"value"`
	Unit  string      `json:"unit,omitempty"`
	Time  time.Time   `json:"time"`
}

// ---------- Telemetry Buffer (persistent, survives restarts) ----------

type telemetryBuffer struct {
	mu       sync.Mutex
	filePath string
	queue    [][]byte
	maxSize  int
}

func newTelemetryBuffer(filePath string, maxSize int) *telemetryBuffer {
	tb := &telemetryBuffer{
		filePath: filePath,
		maxSize:  maxSize,
	}
	data, err := os.ReadFile(filePath)
	if err == nil && len(data) > 0 {
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		for _, line := range lines {
			if len(line) > 0 {
				tb.queue = append(tb.queue, []byte(line))
			}
		}
		logger.WithField("count", len(tb.queue)).Info("telemetry buffer loaded from disk")
	}
	return tb
}

func (tb *telemetryBuffer) push(payload []byte) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if len(tb.queue) >= tb.maxSize {
		tb.queue = tb.queue[1:]
	}
	tb.queue = append(tb.queue, payload)
	tb.persist()
}

func (tb *telemetryBuffer) pop() ([][]byte, bool) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if len(tb.queue) == 0 {
		return nil, false
	}
	items := tb.queue
	tb.queue = nil
	tb.persist()
	return items, true
}

func (tb *telemetryBuffer) persist() {
	if tb.filePath == "" {
		return
	}
	var buf strings.Builder
	for _, item := range tb.queue {
		buf.Write(item)
		buf.WriteByte('\n')
	}
	os.WriteFile(tb.filePath, []byte(buf.String()), 0644)
}

func (tb *telemetryBuffer) len() int {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return len(tb.queue)
}
