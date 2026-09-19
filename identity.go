package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------- Stable device identity (Phase 5 / §21) ----------
//
// Pinning, not replacement: on first boot the ID resolves through the legacy
// chain (configured device_id → MAC → serial → fresh UUID) and is persisted
// to a 0600 pin file. Every later boot returns the pinned value, so identity
// survives NIC swaps, MAC randomization and reimaging of the OS layer while
// existing deployed gateways keep the exact ID they already use (§43).
// MAC alone is spoofable/unstable, so it is never re-resolved after pinning.

func devicePinPath() string {
	return filepath.Join(filepath.Dir(configPath()), "device.id")
}

func loadPinnedDeviceID() string {
	raw, err := os.ReadFile(devicePinPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func pinDeviceID(id string) {
	if id == "" {
		return
	}
	_ = os.WriteFile(devicePinPath(), []byte(id+"\n"), 0600)
}

func freshUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return hex.EncodeToString(b[:])
}

// resolveStableDeviceID implements the chain above. Pure except for the
// one-time pin write, so it is safe to call once at startup.
func resolveStableDeviceID(configured string) string {
	if pinned := loadPinnedDeviceID(); pinned != "" {
		return pinned
	}
	var id string
	switch {
	case configured != "":
		id = configured
	case getMACAddress() != "":
		id = strings.ReplaceAll(getMACAddress(), ":", "")
	case getSerialNumber() != "":
		id = getSerialNumber()
	default:
		if uuid := freshUUIDv4(); uuid != "" {
			id = "gw-" + uuid[:12]
		} else {
			id = fmt.Sprintf("pi-%d", time.Now().Unix())
		}
	}
	pinDeviceID(id)
	return id
}
