package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// ---------- Desired-state poller ----------
// Fetches the platform's effective desires (log taps, supervised processes,
// tamper intent, desired config revision) and tails tapped log files into
// the device log stream with source=<path> for the multi-file viewer.

type deviceDesires struct {
	ConfigRevision string   `json:"configRevision"`
	LogPaths       []string `json:"logPaths"`
	Processes      []string `json:"processes"`
	TamperWatch    bool     `json:"tamperWatch"`
}

var (
	desiresMu   sync.RWMutex
	desiredTaps []string
	tapOffsets  = make(map[string]int64)
	desiredAt   time.Time
)

func startDesiresPoller(ctx context.Context) {
	if cfg.Gateway.PlatformURL == "" {
		logger.Info("desires: platform_url not set, skipping")
		return
	}
	// Initial fetch, then every 5 minutes; tailing runs on its own ticker.
	fetchDesires()
	fetchTicker := time.NewTicker(5 * time.Minute)
	defer fetchTicker.Stop()
	tailTicker := time.NewTicker(15 * time.Second)
	defer tailTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-fetchTicker.C:
			fetchDesires()
		case <-tailTicker.C:
			tailTappedLogs()
		}
	}
}

func fetchDesires() {
	secret := cachedDeviceSecret()
	if secret == "" {
		logger.Debug("desires: no device secret cached, skipping fetch")
		return
	}
	deviceID := getDeviceID()
	base, _ := splitEdgeAuth(strings.TrimRight(cfg.Gateway.PlatformURL, "/"))
	url := fmt.Sprintf("%s/api/v1/provisioning/config?deviceId=%s&deviceSecret=%s", base, deviceID, secret)
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		logger.WithError(err).Warn("desires: fetch failed")
		return
	}
	applyEdgeAuth(req, cfg.Gateway.PlatformURL)
	resp, err := client.Do(req)
	if err != nil {
		logger.WithError(err).Warn("desires: fetch failed")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		logger.WithFields(logrus.Fields{"status": resp.StatusCode, "body": string(body)}).Warn("desires: unexpected status")
		return
	}
	var envelope struct {
		Desires deviceDesires `json:"desires"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		logger.WithError(err).Warn("desires: decode failed")
		return
	}
	desiresMu.Lock()
	desiredTaps = append([]string(nil), envelope.Desires.LogPaths...)
	desiredAt = time.Now()
	desiresMu.Unlock()
	logger.WithFields(logrus.Fields{
		"taps": len(envelope.Desires.LogPaths), "processes": len(envelope.Desires.Processes),
	}).Info("desires: updated")
}

// tailTappedLogs reads new lines from tapped files since the last pass.
// Bounded: 20 files, 200 lines/file/pass, 50MB files skipped, offsets kept
// in memory (a restart re-reads from the end, never the whole file).
func tailTappedLogs() {
	desiresMu.RLock()
	paths := append([]string(nil), desiredTaps...)
	desiresMu.RUnlock()
	if len(paths) == 0 {
		return
	}
	if len(paths) > 20 {
		paths = paths[:20]
	}
	for _, path := range paths {
		tailOneFile(path)
	}
}

func tailOneFile(path string) {
	if strings.Contains(path, "..") {
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	if st.Size() > 50<<20 {
		return
	}
	desiresMu.RLock()
	off := tapOffsets[path]
	desiresMu.RUnlock()
	if off == 0 {
		// First sighting: start at the end (history belongs to Download).
		desiresMu.Lock()
		tapOffsets[path] = st.Size()
		desiresMu.Unlock()
		return
	}
	if st.Size() < off {
		off = 0 // rotated/truncated
	}
	if st.Size() == off {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return
	}
	// Cap the read window: 200 lines max per pass.
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return
	}
	newOff := off + int64(len(data))
	text := string(data)
	lines := strings.Split(text, "\n")
	// Drop the trailing partial line: re-read it next pass.
	if !strings.HasSuffix(text, "\n") && len(lines) > 0 {
		last := lines[len(lines)-1]
		lines = lines[:len(lines)-1]
		newOff -= int64(len(last))
	}
	if len(lines) > 200 {
		lines = lines[len(lines)-200:]
	}
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if len(line) > 4000 {
			line = line[:4000] + "…"
		}
		publishLog("INFO", line, map[string]interface{}{"source": path})
	}
	desiresMu.Lock()
	tapOffsets[path] = newOff
	desiresMu.Unlock()
}
