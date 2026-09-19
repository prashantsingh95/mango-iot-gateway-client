package main

import (
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

// NOTE: the old in-memory telemetryBuffer lived here. It was never wired into
// any publish path, so Phase 5 deleted it outright and replaced it with the
// SQLite-backed spool in queue.go (durable, bounded, priority-ordered).
