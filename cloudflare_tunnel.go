package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Cloudflare Tunnel manager (Phase: Cloudflare Zero Trust)
// Runs cloudflared as outbound tunnel for SSH (no inbound 22).
// Credentials are tunnel-token (0600 file), never logged.

type cfTunnelManager struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	cfg  CloudflareTunnelConfig
	path string
}

func cfTunnelConfigPath() string {
	return filepath.Join(filepath.Dir(configPath()), "cloudflare-tunnel.json")
}

func cfTunnelCredPath() string {
	return filepath.Join(filepath.Dir(configPath()), "cloudflare-tunnel-token")
}

type cfTunnelFile struct {
	TunnelID   string `json:"tunnelId"`
	TunnelName string `json:"tunnelName"`
	Hostname   string `json:"hostname"`
	Token      string `json:"token"`
}

func startCloudflareTunnel(ctx context.Context) {
	mgr := &cfTunnelManager{cfg: cfg.Cloudflare, path: cfTunnelConfigPath()}
	// Ensure creds file is 0600 if present; otherwise wait for provisioning to populate it via config update
	go mgr.runLoop(ctx)
	logger.WithField("path", mgr.path).Info("cloudflare tunnel: manager started")
}

func (m *cfTunnelManager) runLoop(ctx context.Context) {
	backoff := time.Duration(m.cfg.ReconnectBaseMs) * time.Millisecond
	if backoff <= 0 {
		backoff = 5 * time.Second
	}
	maxBackoff := time.Duration(m.cfg.ReconnectMaxMs) * time.Millisecond
	if maxBackoff <= 0 {
		maxBackoff = 60 * time.Second
	}
	for {
		select {
		case <-ctx.Done():
			m.stopLocked()
			return
		default:
		}
		if err := m.ensureConfig(); err != nil {
			logger.WithError(err).Warn("cloudflare tunnel: config not ready, retrying")
		} else if err := m.startOnce(ctx); err != nil {
			logger.WithError(err).Warn("cloudflare tunnel: exited, retrying")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (m *cfTunnelManager) ensureConfig() error {
	// Token may arrive via provisioned config (cloudflare_tunnel.token) or via separate credential endpoint.
	// We persist minimal file for cloudflared; cloudflared itself is started with `cloudflared tunnel run --token <token>`
	// where supported, otherwise via config file.
	if m.cfg.AgentSecret == "" {
		return fmt.Errorf("tunnel token not configured")
	}
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	// Write token file 0600 (never log contents)
	tokenPath := cfTunnelCredPath()
	if err := os.WriteFile(tokenPath, []byte(m.cfg.AgentSecret), 0600); err != nil {
		return err
	}
	// Write json descriptor for health checks
	desc := cfTunnelFile{Token: m.cfg.AgentSecret, Hostname: m.cfg.BackendWSURL}
	raw, _ := json.Marshal(desc)
	_ = os.WriteFile(m.path, raw, 0600)
	return nil
}

func (m *cfTunnelManager) startOnce(ctx context.Context) error {
	tokenPath := cfTunnelCredPath()
	if _, err := os.Stat(tokenPath); err != nil {
		return err
	}
	// Prefer cloudflared binary if available
	bin, err := exec.LookPath("cloudflared")
	if err != nil {
		logger.Warn("cloudflare tunnel: cloudflared not found, tunnel disabled (install cloudflared for SSH)")
		// Block until ctx cancels or token changes
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(60 * time.Second):
			return fmt.Errorf("cloudflared missing")
		}
	}
	token, _ := os.ReadFile(tokenPath)
	m.mu.Lock()
	m.cmd = exec.CommandContext(ctx, bin, "tunnel", "run", "--token", string(token))
	m.cmd.Stdout = os.Stdout
	m.cmd.Stderr = os.Stderr
	err = m.cmd.Start()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	logger.WithField("hostname", m.cfg.BackendWSURL).Info("cloudflare tunnel: cloudflared started")
	done := make(chan error, 1)
	go func() { done <- m.cmd.Wait() }()
	select {
	case <-ctx.Done():
		m.stopLocked()
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func (m *cfTunnelManager) stopLocked() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Signal(os.Interrupt)
		time.AfterFunc(5*time.Second, func() {
			if m.cmd.Process != nil {
				_ = m.cmd.Process.Kill()
			}
		})
	}
	m.cmd = nil
}

func cfTunnelHealth() (string, error) {
	p := cfTunnelConfigPath()
	if _, err := os.Stat(p); err != nil {
		return "DISABLED", nil
	}
	if _, err := os.Stat(cfTunnelCredPath()); err != nil {
		return "DEGRADED", fmt.Errorf("token missing")
	}
	// If cloudflared process check needed, use pid file or pgrep
	return "CONNECTED", nil
}

// Ensure logrus import used
var _ = logrus.InfoLevel
