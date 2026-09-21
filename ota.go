package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
)

// ---------- OTA authenticity + rollback (Phase 5 / §19) ----------

type otaPending struct {
	Version  string `json:"version"`
	Deadline int64  `json:"deadline_unix"`
	Attempts int    `json:"attempts"`
}

func otaPendingPath() string {
	dir := cfg.OTA.FirmwareDir
	if dir == "" {
		dir = "/opt/gateway/firmware"
	}
	return filepath.Join(dir, ".ota_pending")
}

// verifyArtifactSignature checks a hex ed25519 signature over the raw binary.
// The public key is operator-pinned in ota.signing_key (provisioned securely,
// never via the same channel as the artifact).
// Signature may be `hex` or `keyId:hex` (server stores keyId:hex).
func verifyArtifactSignature(data []byte, sigHex, pubHex string) error {
	// Handle keyId prefix: "primary:abc123..." → "abc123..."
	if idx := lastIndex(sigHex, ":"); idx >= 0 {
		sigHex = sigHex[idx+1:]
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("malformed signature")
	}
	pub, err := hex.DecodeString(pubHex)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("malformed ota.signing_key")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), data, sig) {
		return fmt.Errorf("signature verification failed")
	}
	return nil
}

func lastIndex(s, substr string) int {
	for i := len(s) - len(substr); i >= 0; i-- {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func armOtaPendingMarker(version string) {
	if !cfg.OTA.AutoRollback {
		return
	}
	window := cfg.OTA.RollbackTimeout
	if window <= 0 {
		window = 300
	}
	marker := otaPending{Version: version, Deadline: time.Now().Unix() + int64(window)}
	raw, _ := json.Marshal(marker)
	if err := os.WriteFile(otaPendingPath(), raw, 0600); err != nil {
		logger.WithError(err).Warn("ota: could not arm rollback marker")
		return
	}
	logger.WithField("deadline_s", window).Info("ota: rollback marker armed")
}

func clearOtaPendingMarker() {
	_ = os.Remove(otaPendingPath())
}

// otaBootGate runs at startup when a previous boot installed new firmware.
// Crash-looping binaries (attempts>3) roll back immediately; otherwise a timer
// restores the backup if the new binary never checks in before its deadline.
func otaBootGate() {
	raw, err := os.ReadFile(otaPendingPath())
	if err != nil {
		return // no pending update
	}
	var marker otaPending
	if err := json.Unmarshal(raw, &marker); err != nil {
		_ = os.Remove(otaPendingPath())
		return
	}
	marker.Attempts++
	if marker.Attempts > 3 {
		logger.Warn("ota: new binary crash-looping — rolling back immediately")
		restoreOtaBackup(marker.Version)
		return
	}
	raw, _ = json.Marshal(marker)
	_ = os.WriteFile(otaPendingPath(), raw, 0600)

	delay := time.Until(time.Unix(marker.Deadline, 0))
	if delay <= 0 {
		delay = time.Second
	}
	logger.WithFields(logrus.Fields{"version": marker.Version, "deadline_in": delay.String()}).Info("ota: health gate armed")
	go func(version string, d time.Duration) {
		time.Sleep(d)
		if _, err := os.Stat(otaPendingPath()); err == nil {
			logger.Warn("ota: new binary failed health gate — rolling back")
			restoreOtaBackup(version)
		}
	}(marker.Version, delay)
}

func restoreOtaBackup(version string) {
	backupPath := filepath.Join(cfg.OTA.BackupDir, "gateway-agent.bak")
	selfPath, err := os.Executable()
	if err != nil {
		logger.WithError(err).Error("ota rollback: cannot locate running binary")
		return
	}
	data, err := os.ReadFile(backupPath)
	if err != nil {
		logger.WithError(err).Error("ota rollback: no backup available")
		_ = os.Remove(otaPendingPath())
		return
	}
	if err := os.WriteFile(selfPath, data, 0755); err != nil {
		logger.WithError(err).Error("ota rollback: restore failed")
		return
	}
	_ = os.Remove(otaPendingPath())
	logger.WithField("version", version).Warn("ota: rolled back to previous binary, restarting")
	time.Sleep(time.Second)
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
}
