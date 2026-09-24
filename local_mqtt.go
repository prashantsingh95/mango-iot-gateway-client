package main

// ---------- Local MQTT Broker (Mosquitto) management ----------
//
// On-gateway broker for customer meters over gateway Wi-Fi/LAN:
//   TCP 1883 = unsecured MQTT (LAN-only, optional auth)
//   TCP 8883 = secure MQTT over TLS 1.2+ (username+password, no client certs)
//
// Fully independent from the remote HiveMQ client: disabling/stopping the
// local broker never affects gateway telemetry. The agent manages Mosquitto
// through a dedicated systemd unit (mango-local-broker, installed by
// setup.sh with scoped sudo) with a supervised direct-process fallback when
// systemd/sudo is unavailable.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
	"gopkg.in/yaml.v3"
)

// ---------- defaults & paths ----------

func applyLocalMQTTDefaults() {
	b := &cfg.LocalBroker
	if b.Mode == "" {
		b.Mode = "secure"
	}
	if b.Bind == "" {
		b.Bind = "0.0.0.0"
	}
	if b.PortPlain <= 0 {
		b.PortPlain = 1883
	}
	if b.PortTLS <= 0 {
		b.PortTLS = 8883
	}
	if b.MaxConnections <= 0 {
		b.MaxConnections = 100
	}
	if b.MaxPayloadBytes <= 0 {
		b.MaxPayloadBytes = 65536
	}
	base := localBrokerBaseDir()
	if b.CertDir == "" {
		b.CertDir = filepath.Join(base, "certs")
	}
	if b.ConfPath == "" {
		b.ConfPath = filepath.Join(base, "mosquitto.conf")
	}
	if b.DataDir == "" {
		b.DataDir = filepath.Join(base, "data")
	}
	if b.Service == "" {
		b.Service = "mango-local-broker"
	}
	c := &cfg.LocalClient
	if len(c.Topics) == 0 {
		c.Topics = []string{"meter/#"}
	}
	if c.Username == "" {
		c.Username = "gateway-local"
	}
	if c.MaxPayloadBytes <= 0 {
		c.MaxPayloadBytes = 65536
	}
}

func localBrokerBaseDir() string {
	return filepath.Join(filepath.Dir(configPath()), "mqtt")
}

func brokerPasswdPath() string { return filepath.Join(localBrokerBaseDir(), "passwd") }
func brokerACLPath() string    { return filepath.Join(localBrokerBaseDir(), "acl") }
func brokerLogPath() string    { return filepath.Join(localBrokerBaseDir(), "mosquitto.log") }
func brokerPrevConf() string   { return cfg.LocalBroker.ConfPath + ".prev" }

// ---------- small exec helper (never logs secrets; callers pass safe args) ----------

func runCmd(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.Bytes(), err
}

func lookPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// ---------- certificates (Go crypto, no openssl dependency) ----------

func ensureBrokerCerts() error {
	b := &cfg.LocalBroker
	if err := os.MkdirAll(b.CertDir, 0755); err != nil {
		return fmt.Errorf("cert dir: %w", err)
	}
	caCert := filepath.Join(b.CertDir, "ca.crt")
	caKey := filepath.Join(b.CertDir, "ca.key")
	srvCert := filepath.Join(b.CertDir, "server.crt")
	srvKey := filepath.Join(b.CertDir, "server.key")

	needCA := false
	if _, err := os.Stat(caCert); err != nil {
		needCA = true
	}
	if _, err := os.Stat(caKey); err != nil {
		needCA = true
	}
	if needCA {
		if err := generateCA(caCert, caKey); err != nil {
			return err
		}
		logger.Info("local broker: CA certificate generated")
	}
	regen := false
	if _, err := os.Stat(srvCert); err != nil {
		regen = true
	}
	if _, err := os.Stat(srvKey); err != nil {
		regen = true
	}
	if !regen && certExpiringSoon(srvCert, 30*24*time.Hour) {
		regen = true
		logger.Info("local broker: server certificate near expiry, regenerating")
	}
	if regen {
		if err := generateServerCert(caCert, caKey, srvCert, srvKey); err != nil {
			return err
		}
		logger.Info("local broker: server certificate generated")
	}
	return nil
}

func generateCA(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "IOT-2024G Local MQTT CA", Organization: []string{"Mango IoT"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	if err := writePEM(certPath, "CERTIFICATE", der, 0644); err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return writePEM(keyPath, "EC PRIVATE KEY", kb, 0600)
}

func generateServerCert(caCertPath, caKeyPath, certPath, keyPath string) error {
	caCertPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return err
	}
	caKeyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		return err
	}
	caBlock, _ := pem.Decode(caCertPEM)
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return err
	}
	keyBlock, _ := pem.Decode(caKeyPEM)
	caKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return err
	}
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "iot-2024g-local-broker", Organization: []string{"Mango IoT"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(825 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", getHostname()},
	}
	// Loopback first: the on-gateway meter client, health probes and local
	// tooling always dial 127.0.0.1 — without this SAN their TLS verify fails.
	tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	for _, ip := range localAdvertisedIPs() {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	if err := writePEM(certPath, "CERTIFICATE", der, 0644); err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		return err
	}
	return writePEM(keyPath, "EC PRIVATE KEY", kb, 0600)
}

func writePEM(path, typ string, der []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: typ, Bytes: der}); err != nil {
		return err
	}
	// Enforce permissions even when the file already existed.
	return os.Chmod(path, perm)
}

func certExpiringSoon(path string, within time.Duration) bool {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return true
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return true
	}
	return time.Until(cert.NotAfter) < within
}

func localAdvertisedIPs() []net.IP {
	var out []net.IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	seen := map[string]bool{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
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
			if v4 := ip.To4(); v4 != nil {
				if !seen[v4.String()] {
					seen[v4.String()] = true
					out = append(out, v4)
				}
			}
		}
	}
	return out
}

func getHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "iot-2024g"
	}
	return h
}

// ---------- passwd / ACL ----------

func brokerUsers() []LocalBrokerUser {
	users := append([]LocalBrokerUser{}, cfg.LocalBroker.Users...)
	// The on-gateway meter client always gets an account (created if missing).
	if cfg.LocalClient.Username != "" && cfg.LocalClient.Password != "" {
		found := false
		for _, u := range users {
			if u.Username == cfg.LocalClient.Username {
				found = true
				break
			}
		}
		if !found {
			users = append(users, LocalBrokerUser{Username: cfg.LocalClient.Username, Password: cfg.LocalClient.Password})
		}
	}
	return users
}

func writeBrokerPasswd(users []LocalBrokerUser) error {
	if !lookPath("mosquitto_passwd") {
		return fmt.Errorf("mosquitto_passwd not found — install mosquitto (setup.sh) first")
	}
	if err := os.MkdirAll(localBrokerBaseDir(), 0755); err != nil {
		return err
	}
	tmp := brokerPasswdPath() + ".tmp"
	os.Remove(tmp)
	for i, u := range users {
		if u.Username == "" || u.Password == "" {
			return fmt.Errorf("broker user %d has empty username/password", i)
		}
		args := []string{"-b", tmp, u.Username, u.Password}
		if i == 0 {
			args = []string{"-b", "-c", tmp, u.Username, u.Password}
		}
		if _, err := runCmd(15*time.Second, "mosquitto_passwd", args...); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("mosquitto_passwd: %w", err)
		}
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, brokerPasswdPath())
}

func writeBrokerACL(users []LocalBrokerUser) error {
	var sb strings.Builder
	sb.WriteString("# Generated by gateway-agent — do not edit by hand\n")
	for _, u := range users {
		sb.WriteString("user " + u.Username + "\n")
		if u.Username == cfg.LocalClient.Username {
			// Gateway's own service account: full read for ingest/monitoring
			// plus write for customer forwarding destinations. LAN-only
			// broker; credentials never leave the gateway.
			sb.WriteString("topic readwrite #\n")
		} else {
			sb.WriteString("topic readwrite meter/" + u.Username + "/#\n")
		}
	}
	return os.WriteFile(brokerACLPath(), []byte(sb.String()), 0600)
}

// ---------- mosquitto.conf generation + validation ----------

func generateBrokerConf(mode string) string {
	b := &cfg.LocalBroker
	var sb strings.Builder
	sb.WriteString("# Generated by gateway-agent — do not edit by hand\n")
	sb.WriteString("per_listener_settings true\n")
	sb.WriteString("persistence true\n")
	sb.WriteString("persistence_location " + b.DataDir + "/\n")
	sb.WriteString(fmt.Sprintf("message_size_limit %d\n", b.MaxPayloadBytes))
	// NB: with per_listener_settings, auth directives MUST live inside each
	// listener block. Global password_file/acl_file makes mosquitto 2.0 open
	// an extra default 1883 listener and leaves ours unauthenticated.
	sb.WriteString("log_dest file " + brokerLogPath() + "\n")
	sb.WriteString("log_type error\nlog_type warning\nlog_type notice\nlog_type information\n")
	sb.WriteString("connection_messages true\n")
	bind := b.Bind
	if bind == "" {
		bind = "0.0.0.0"
	}
	plain := mode == "unsecured" || mode == "both"
	secure := mode == "secure" || mode == "both"
	if plain {
		sb.WriteString(fmt.Sprintf("\nlistener %d %s\n", b.PortPlain, bind))
		sb.WriteString("protocol mqtt\n")
		if b.AllowAnonymousPlain {
			sb.WriteString("allow_anonymous true\n")
		} else {
			sb.WriteString("allow_anonymous false\n")
			sb.WriteString("password_file " + brokerPasswdPath() + "\n")
			sb.WriteString("acl_file " + brokerACLPath() + "\n")
		}
	}
	if secure {
		sb.WriteString(fmt.Sprintf("\nlistener %d %s\n", b.PortTLS, bind))
		sb.WriteString("protocol mqtt\n")
		sb.WriteString("allow_anonymous false\n")
		sb.WriteString("password_file " + brokerPasswdPath() + "\n")
		sb.WriteString("acl_file " + brokerACLPath() + "\n")
		sb.WriteString("cafile " + filepath.Join(b.CertDir, "ca.crt") + "\n")
		sb.WriteString("certfile " + filepath.Join(b.CertDir, "server.crt") + "\n")
		sb.WriteString("keyfile " + filepath.Join(b.CertDir, "server.key") + "\n")
		sb.WriteString("require_certificate false\n")
		sb.WriteString("tls_version tlsv1.2\n")
	}
	// NB (mosquitto 2.0.x quirk, verified on 2.0.11): a max_connections value
	// other than -1 placed BEFORE any listener block makes the broker open an
	// extra default 1883 listener. After the listener blocks it is clean.
	if b.MaxConnections != 0 {
		sb.WriteString(fmt.Sprintf("\nmax_connections %d\n", b.MaxConnections))
	}
	return sb.String()
}

func validateBrokerConf(confText string) error {
	if !lookPath("mosquitto") {
		return fmt.Errorf("mosquitto binary not found — install mosquitto (setup.sh) first")
	}
	tmp, err := os.CreateTemp("", "mosquitto-validate-*.conf")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(confText); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	out, err := runCmd(15*time.Second, "mosquitto", "-t", "-c", tmpPath)
	if err == nil {
		return nil
	}
	if strings.Contains(string(out), "Unknown option") {
		// Mosquitto < 2.1 has no `-t` test flag: fall back to structural
		// validation. Runtime safety still comes from the health check with
		// automatic rollback in applyLocalBrokerConfig.
		return structuralConfCheck(confText)
	}
	return fmt.Errorf("mosquitto config invalid: %v (%s)", err, strings.TrimSpace(string(out)))
}

// structuralConfCheck is the pre-2.1 validation fallback: required
// directives per enabled listener must be present and ports must differ.
func structuralConfCheck(confText string) error {
	if !strings.Contains(confText, "password_file ") {
		return fmt.Errorf("conf missing password_file")
	}
	if !strings.Contains(confText, "acl_file ") {
		return fmt.Errorf("conf missing acl_file")
	}
	if strings.Contains(confText, "listener ") {
		hasPlain, hasTLS := false, false
		for _, line := range strings.Split(confText, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "listener ") {
				if strings.Contains(line, " 8883") || strings.HasSuffix(strings.TrimSpace(line), "8883") {
					hasTLS = true
				} else {
					hasPlain = true
				}
			}
		}
		if !hasPlain && !hasTLS {
			return fmt.Errorf("conf has no usable listener")
		}
		if hasTLS && (!strings.Contains(confText, "certfile ") || !strings.Contains(confText, "keyfile ")) {
			return fmt.Errorf("TLS listener without certfile/keyfile")
		}
		return nil
	}
	return fmt.Errorf("conf has no listener")
}

// validateLiveFiles ensures files referenced by the generated conf exist.
func validateLiveFiles(mode string) error {
	if _, err := os.Stat(brokerPasswdPath()); err != nil {
		return fmt.Errorf("broker password file missing — configure users first")
	}
	if _, err := os.Stat(brokerACLPath()); err != nil {
		return fmt.Errorf("broker ACL file missing")
	}
	if mode == "secure" || mode == "both" {
		for _, f := range []string{"ca.crt", "server.crt", "server.key"} {
			if _, err := os.Stat(filepath.Join(cfg.LocalBroker.CertDir, f)); err != nil {
				return fmt.Errorf("TLS file %s missing — cannot start secure listener", f)
			}
		}
	}
	return nil
}

// ---------- service control (systemd w/ scoped sudo, else direct process) ----------

var (
	brokerProcMu sync.Mutex
	brokerProc   *os.Process
)

func brokerUnit() string {
	if cfg.LocalBroker.Service != "" {
		return cfg.LocalBroker.Service
	}
	return "mango-local-broker"
}

func hasSystemdUnit() bool {
	if !lookPath("systemctl") {
		return false
	}
	out, err := runCmd(10*time.Second, "systemctl", "cat", brokerUnit())
	return err == nil && len(out) > 0
}

func systemctl(args ...string) ([]byte, error) {
	full := append([]string{"systemctl"}, args...)
	// Prefer passwordless scoped sudo (setup.sh installs sudoers drop-in);
	// fall back to direct systemctl (works when agent runs as root).
	if out, err := runCmd(30*time.Second, "sudo", append([]string{"-n"}, full...)...); err == nil {
		return out, nil
	}
	return runCmd(30*time.Second, full[0], full[1:]...)
}

func brokerServiceActive() bool {
	if !hasSystemdUnit() {
		return directBrokerRunning()
	}
	out, err := runCmd(10*time.Second, "systemctl", "is-active", brokerUnit())
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

func directBrokerRunning() bool {
	brokerProcMu.Lock()
	defer brokerProcMu.Unlock()
	if brokerProc == nil {
		return false
	}
	// Signal 0 probes liveness without disturbing the process.
	if err := brokerProc.Signal(syscall.Signal(0)); err != nil {
		brokerProc = nil
		return false
	}
	return true
}

func brokerViaSystemd() bool { return hasSystemdUnit() }

func brokerStart() error {
	if brokerViaSystemd() {
		if _, err := systemctl("start", brokerUnit()); err != nil {
			return fmt.Errorf("systemctl start %s: %v (no sudo? falling back to direct mode is unavailable while a unit exists)", brokerUnit(), err)
		}
		return nil
	}
	return brokerStartDirect()
}

func brokerStop() error {
	if brokerViaSystemd() {
		out, err := systemctl("stop", brokerUnit())
		if err != nil {
			return fmt.Errorf("systemctl stop %s: %v (%s)", brokerUnit(), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return brokerStopDirect()
}

func brokerStartDirect() error {
	brokerProcMu.Lock()
	defer brokerProcMu.Unlock()
	if brokerProc != nil {
		return nil
	}
	if !lookPath("mosquitto") {
		return fmt.Errorf("mosquitto binary not found")
	}
	cmd := exec.Command("mosquitto", "-c", cfg.LocalBroker.ConfPath)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start mosquitto: %w", err)
	}
	brokerProc = cmd.Process
	// Reap on exit; supervisor loop restarts when enabled.
	go func(p *os.Process) {
		p.Wait()
		brokerProcMu.Lock()
		if brokerProc == p {
			brokerProc = nil
		}
		brokerProcMu.Unlock()
	}(cmd.Process)
	return nil
}

func brokerStopDirect() error {
	brokerProcMu.Lock()
	p := brokerProc
	brokerProc = nil
	brokerProcMu.Unlock()
	if p == nil {
		return nil
	}
	_ = p.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { p.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = p.Kill()
	}
	return nil
}

// ---------- apply pipeline: validate -> write -> restart -> health -> rollback ----------

// brokerApplyMu serializes broker state changes. The 30s supervisor must
// never stop a broker that an in-flight apply is health-checking (config
// still says disabled until apply succeeds) — without this the supervisor
// kills the broker mid-check and every enable fails.
var brokerApplyMu sync.Mutex

func normalizeBrokerMode(mode string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(mode))
	switch m {
	case "unsecured", "secure", "both":
		return m, nil
	case "", "disabled":
		return "", fmt.Errorf("mode disabled — broker must be stopped, not configured")
	default:
		return "", fmt.Errorf("invalid mode %q (want unsecured|secure|both)", mode)
	}
}

func ensureLocalBrokerPrereqs(mode string) error {
	b := &cfg.LocalBroker
	if err := os.MkdirAll(localBrokerBaseDir(), 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(b.DataDir, 0755); err != nil {
		return err
	}
	// Auto-provision the gateway-local client account on first enable.
	if cfg.LocalClient.Username != "" && cfg.LocalClient.Password == "" {
		pw, err := randomPassword(32)
		if err != nil {
			return err
		}
		cfg.LocalClient.Password = pw
		if secrets != nil {
			// Persist enc: immediately so restarts keep the same credential.
			if _, _, err := secrets.cryptField(&cfg.LocalClient.Password, "local client password"); err != nil {
				return err
			}
			if err := persistConfigFile(); err != nil {
				return err
			}
		}
	}
	users := brokerUsers()
	if len(users) == 0 && !b.AllowAnonymousPlain {
		return fmt.Errorf("no broker users configured (and anonymous 1883 is off) — add at least one user")
	}
	if err := ensureBrokerCerts(); err != nil {
		return err
	}
	if err := writeBrokerPasswd(users); err != nil {
		return err
	}
	if err := writeBrokerACL(users); err != nil {
		return err
	}
	return validateLiveFiles(mode)
}

// applyLocalBrokerConfig validates, writes and activates a new broker config
// with rollback to the previous working configuration on failure.
func applyLocalBrokerConfig(mode string) error {
	brokerApplyMu.Lock()
	defer brokerApplyMu.Unlock()
	return applyLocked(mode)
}

func applyLocked(mode string) error {
	if _, err := normalizeBrokerMode(mode); err != nil {
		return err
	}
	if err := ensureLocalBrokerPrereqs(mode); err != nil {
		return err
	}
	confText := generateBrokerConf(mode)
	if err := validateBrokerConf(confText); err != nil {
		return err
	}
	// Backup current working config for rollback.
	if cur, err := os.ReadFile(cfg.LocalBroker.ConfPath); err == nil {
		_ = os.WriteFile(brokerPrevConf(), cur, 0600)
	}
	if err := os.WriteFile(cfg.LocalBroker.ConfPath, []byte(confText), 0600); err != nil {
		return fmt.Errorf("write broker conf: %w", err)
	}
	wasActive := brokerServiceActive()
	if wasActive {
		if err := brokerStop(); err != nil {
			return err
		}
		time.Sleep(time.Second)
	}
	if err := brokerStart(); err != nil {
		return rollbackBrokerConfig(fmt.Errorf("start failed: %w", err))
	}
	if err := waitBrokerHealthy(mode, 20*time.Second); err != nil {
		_ = brokerStop()
		return rollbackBrokerConfig(fmt.Errorf("health check failed: %w", err))
	}
	cfg.LocalBroker.Mode = mode
	cfg.LocalBroker.Enabled = true
	// Bring the meter ingest client up immediately (don't wait for the
	// 30s supervisor tick — publications in between would be missed).
	ensureMeterClient()
	return nil
}

func rollbackBrokerConfig(cause error) error {
	prev, err := os.ReadFile(brokerPrevConf())
	if err != nil {
		return fmt.Errorf("%v (no rollback available: %v)", cause, err)
	}
	_ = os.WriteFile(cfg.LocalBroker.ConfPath, prev, 0600)
	_ = brokerStart()
	return fmt.Errorf("%v (rolled back to previous config)", cause)
}

func persistConfigFile() error {
	data, err := yaml.Marshal(&cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0600)
}

// ---------- health / status ----------

type brokerListenerStatus struct {
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
	TLS      bool   `json:"tls"`
	Status   string `json:"status"`
}

type brokerStatus struct {
	Enabled          bool                   `json:"enabled"`
	Running          bool                   `json:"running"`
	Mode             string                 `json:"mode"`
	Listeners        []brokerListenerStatus `json:"listeners"`
	ConnectedClients int                    `json:"connected_clients"`
	Version          string                 `json:"version,omitempty"`
	CertExpiry       string                 `json:"cert_expiry,omitempty"`
	ControlPlane     string                 `json:"control_plane"`
}

func dialTimeout(network, addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout(network, addr, timeout)
}

func probeListener(port int, tlsEnabled bool) bool {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if !tlsEnabled {
		c, err := dialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			logger.WithFields(map[string]interface{}{"addr": addr, "error": err.Error()}).Warn("local broker: plain probe failed")
			return false
		}
		c.Close()
		return true
	}
	// TLS handshake probe (self-signed CA — skip verification for the probe;
	// clients verify with the real CA cert).
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec
	if err != nil {
		logger.WithFields(map[string]interface{}{"addr": addr, "error": err.Error()}).Warn("local broker: TLS probe failed")
		return false
	}
	c.Close()
	return true
}

func brokerVersion() string {
	if !lookPath("mosquitto") {
		return ""
	}
	// NB: `mosquitto -h` exits nonzero — parse output regardless.
	out, _ := runCmd(10*time.Second, "mosquitto", "-h")
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "mosquitto version") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func brokerCertExpiry() string {
	p := filepath.Join(cfg.LocalBroker.CertDir, "server.crt")
	pemBytes, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	if block, _ := pem.Decode(pemBytes); block != nil {
		if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
			return cert.NotAfter.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

func getBrokerStatus() brokerStatus {
	b := &cfg.LocalBroker
	st := brokerStatus{
		Enabled: b.Enabled,
		Mode:    b.Mode,
	}
	if brokerViaSystemd() {
		st.ControlPlane = "systemd:" + brokerUnit()
	} else {
		st.ControlPlane = "direct-process"
	}
	// NB: always report ACTUAL state, even when disabled in config. The
	// enable health-check runs before Enabled is flipped — an early return
	// here made every enable fail with Running=false.
	st.Running = brokerServiceActive()
	st.Version = brokerVersion()
	st.CertExpiry = brokerCertExpiry()
	plain := b.Mode == "unsecured" || b.Mode == "both"
	secure := b.Mode == "secure" || b.Mode == "both"
	if plain {
		ok := st.Running && probeListener(b.PortPlain, false)
		st.Listeners = append(st.Listeners, brokerListenerStatus{Protocol: "mqtt", Port: b.PortPlain, TLS: false, Status: tern(ok, "listening", "down")})
	}
	if secure {
		ok := st.Running && probeListener(b.PortTLS, true)
		st.Listeners = append(st.Listeners, brokerListenerStatus{Protocol: "mqtts", Port: b.PortTLS, TLS: true, Status: tern(ok, "listening", "down")})
	}
	if st.Running {
		st.ConnectedClients = brokerConnectedCount()
	}
	return st
}

func waitBrokerHealthy(mode string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st := getBrokerStatus()
		if st.Running {
			allUp := true
			for _, l := range st.Listeners {
				if l.Status != "listening" {
					allUp = false
				}
			}
			if allUp && len(st.Listeners) > 0 {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("broker listeners not healthy within %s", timeout)
}

func tern(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// ---------- connected clients ----------

// brokerConnectedCount uses $SYS telemetry via a throwaway monitoring client.
func brokerConnectedCount() int {
	n, _, _ := brokerSysStats()
	return n
}

func brokerSysStats() (clients int, subs int, uptime string) {
	if cfg.LocalClient.Username == "" || cfg.LocalClient.Password == "" {
		return 0, 0, ""
	}
	addr := localClientBrokerURL()
	clientID := fmt.Sprintf("gw-%s-mon-%d", getDeviceID(), time.Now().UnixNano()%100000)
	opts := MQTT.NewClientOptions()
	opts.AddBroker(addr)
	opts.SetClientID(clientID)
	opts.SetUsername(cfg.LocalClient.Username)
	opts.SetPassword(cfg.LocalClient.Password)
	opts.SetCleanSession(true)
	opts.SetConnectTimeout(5 * time.Second)
	opts.SetAutoReconnect(false)
	if strings.HasPrefix(addr, "ssl") {
		caPath := filepath.Join(cfg.LocalBroker.CertDir, "ca.crt")
		pool := x509.NewCertPool()
		if pemBytes, err := os.ReadFile(caPath); err == nil {
			pool.AppendCertsFromPEM(pemBytes)
		}
		opts.SetTLSConfig(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	}
	cli := MQTT.NewClient(opts)
	if tok := cli.Connect(); tok.WaitTimeout(6*time.Second) || tok.Error() != nil {
		return 0, 0, ""
	}
	defer cli.Disconnect(500)
	got := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, t := range []string{"$SYS/broker/clients/connected", "$SYS/broker/subscriptions/count", "$SYS/broker/uptime"} {
		wg.Add(1)
		topic := t
		if tok := cli.Subscribe(topic, 0, func(_ MQTT.Client, m MQTT.Message) {
			mu.Lock()
			got[topic] = string(m.Payload())
			mu.Unlock()
			wg.Done()
		}); tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
			wg.Done()
		}
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
	}
	mu.Lock()
	defer mu.Unlock()
	clients, _ = strconv.Atoi(strings.TrimSpace(got["$SYS/broker/clients/connected"]))
	subs, _ = strconv.Atoi(strings.TrimSpace(got["$SYS/broker/subscriptions/count"]))
	uptime = strings.TrimSpace(got["$SYS/broker/uptime"])
	return clients, subs, uptime
}

type brokerClientInfo struct {
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	State     string `json:"state"`
	Transport string `json:"transport"`
}

// brokerConnectionTable parses /proc/net/tcp* (no `ss` dependency) for
// connections on the broker's meter ports.
func brokerConnectionTable() []brokerClientInfo {
	ports := map[int]string{}
	b := &cfg.LocalBroker
	if b.Mode == "unsecured" || b.Mode == "both" {
		ports[b.PortPlain] = "mqtt"
	}
	if b.Mode == "secure" || b.Mode == "both" {
		ports[b.PortTLS] = "mqtts"
	}
	var out []brokerClientInfo
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				continue
			}
			localIP, localPort := splitProcAddr(fields[1])
			remoteIP, remotePort := splitProcAddr(fields[2])
			transport, ok := ports[localPort]
			if !ok {
				continue
			}
			if localIP == remoteIP {
				continue // monitoring/local client loopback — skip noise
			}
			out = append(out, brokerClientInfo{
				IP:        remoteIP,
				Port:      remotePort,
				State:     procTCPState(fields[3]),
				Transport: transport,
			})
		}
	}
	return out
}

func splitProcAddr(s string) (string, int) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return s, 0
	}
	port64, _ := strconv.ParseInt(parts[1], 16, 32)
	ip := parts[0]
	if len(ip) == 8 {
		// IPv4 little-endian hex.
		b := make([]byte, 4)
		for i := 0; i < 4; i++ {
			v, _ := strconv.ParseUint(ip[i*2:i*2+2], 16, 8)
			b[3-i] = byte(v)
		}
		return net.IP(b).String(), int(port64)
	}
	if len(ip) == 32 {
		b := make([]byte, 16)
		for i := 0; i < 16; i += 4 {
			for j := 0; j < 4; j += 2 {
				v, _ := strconv.ParseUint(ip[i+j:i+j+2], 16, 8)
				// /proc tcp6 groups are little-endian per 32-bit word.
				b[i+(3-j)] = byte(v)
			}
		}
		return net.IP(b).String(), int(port64)
	}
	return ip, int(port64)
}

func procTCPState(hexState string) string {
	switch hexState {
	case "01":
		return "ESTABLISHED"
	case "0A":
		return "LISTEN"
	case "06":
		return "TIME_WAIT"
	case "08":
		return "ESTABLISHED"
	default:
		return "STATE_" + hexState
	}
}

// ---------- logs ----------

func brokerLogs(lines int) ([]string, error) {
	if lines <= 0 || lines > 500 {
		lines = 100
	}
	// Prefer journald when a unit exists; fall back to the log file.
	if hasSystemdUnit() {
		if out, err := runCmd(10*time.Second, "journalctl", "-u", brokerUnit(), "-n", strconv.Itoa(lines), "--no-pager"); err == nil {
			return nonEmptyLines(string(out)), nil
		}
	}
	data, err := os.ReadFile(brokerLogPath())
	if err != nil {
		return nil, fmt.Errorf("no broker logs available: %w", err)
	}
	all := nonEmptyLines(string(data))
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return all, nil
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimRight(l, "\r"); strings.TrimSpace(t) != "" {
			out = append(out, t)
		}
	}
	return out
}

// ---------- HTTP management API (localhost :8090) ----------

func registerLocalMQTTRoutes(mux *http.ServeMux, hs *healthServer) {
	mux.HandleFunc("/api/mqtt/local/status", hs.localStatusHandler)
	mux.HandleFunc("/api/mqtt/local/health", hs.localHealthHandler)
	mux.HandleFunc("/api/mqtt/local/enable", hs.localEnableHandler)
	mux.HandleFunc("/api/mqtt/local/disable", hs.localDisableHandler)
	mux.HandleFunc("/api/mqtt/local/restart", hs.localRestartHandler)
	mux.HandleFunc("/api/mqtt/local/config", hs.localConfigHandler)
	mux.HandleFunc("/api/mqtt/local/logs", hs.localLogsHandler)
	mux.HandleFunc("/api/mqtt/local/clients", hs.localClientsHandler)
	mux.HandleFunc("/api/mqtt/local/meters", hs.localMetersHandler)
	mux.HandleFunc("/api/mqtt/local/ca", hs.localCAHandler)
	mux.HandleFunc("/api/mqtt/local/test", hs.localTestHandler)
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (hs *healthServer) localStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}
	st := getBrokerStatus()
	clients, subs, uptime := 0, 0, ""
	if st.Running {
		clients, subs, uptime = brokerSysStats()
		st.ConnectedClients = clients
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": st.Enabled, "status": tern(st.Running, "running", tern(st.Enabled, "degraded", "stopped")),
		"mode": st.Mode, "secure": st.Mode == "secure" || st.Mode == "both",
		"listeners": st.Listeners, "connected_clients": clients,
		"subscriptions": subs, "broker_uptime": uptime,
		"version": st.Version, "cert_expiry": st.CertExpiry, "control_plane": st.ControlPlane,
		"local_client": localMeterClientStatus(),
	})
}

func (hs *healthServer) localHealthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}
	st := getBrokerStatus()
	healthy := !st.Enabled || st.Running
	for _, l := range st.Listeners {
		if l.Status != "listening" {
			healthy = false
		}
	}
	code := http.StatusOK
	if !healthy {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]interface{}{
		"broker": tern(st.Running, "running", "stopped"),
		"healthy": healthy, "mode": st.Mode, "listeners": st.Listeners,
		"cert_expiry": st.CertExpiry, "connected_clients": st.ConnectedClients,
	})
}

func (hs *healthServer) localEnableHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	mode := body.Mode
	if mode == "" {
		mode = cfg.LocalBroker.Mode
	}
	if err := enableLocalBroker(mode); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": true, "mode": cfg.LocalBroker.Mode, "status": getBrokerStatus()})
}

func (hs *healthServer) localDisableHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	if err := disableLocalBroker(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false, "status": getBrokerStatus()})
}

func (hs *healthServer) localRestartHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	if !cfg.LocalBroker.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "broker is disabled — enable it first"})
		return
	}
	if err := restartLocalBroker(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"restarted": true, "status": getBrokerStatus()})
}

func (hs *healthServer) localConfigHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, sanitizedBrokerConfig())
	case http.MethodPut:
		var body struct {
			Enabled                 *bool            `json:"enabled"`
			Mode                    *string          `json:"mode"`
			Bind                    *string          `json:"bind"`
			PortUnsecured           *int             `json:"port_unsecured"`
			PortSecure              *int             `json:"port_secure"`
			AllowAnonymousUnsecured *bool            `json:"allow_anonymous_unsecured"`
			Users                   *[]brokerUserIn `json:"users"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if err := updateBrokerConfig(body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"updated": true, "config": sanitizedBrokerConfig(), "status": getBrokerStatus()})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET or PUT only"})
	}
}

type brokerUserIn struct {
	Username string `json:"username"`
	Password string `json:"password,omitempty"` // empty = keep existing (update) / error (new user)
}

func (hs *healthServer) localLogsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}
	lines := 100
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			lines = n
		}
	}
	logs, err := brokerLogs(lines)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"logs": logs})
}

func (hs *healthServer) localClientsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"clients": brokerConnectionTable()})
}

func (hs *healthServer) localMetersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}
	writeJSON(w, http.StatusOK, meterStatsSnapshot())
}

func (hs *healthServer) localCAHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}
	pemBytes, err := os.ReadFile(filepath.Join(cfg.LocalBroker.CertDir, "ca.crt"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "CA not provisioned yet — enable the broker first"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ca_crt": string(pemBytes)})
}

func (hs *healthServer) localTestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	var body struct {
		Topic   string         `json:"topic"`
		Payload map[string]interface{} `json:"payload"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil || body.Topic == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "topic and payload required"})
		return
	}
	if err := publishLocalTest(body.Topic, body.Payload); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"published": body.Topic})
}

// ---------- enable / disable / restart / config update ----------

func enableLocalBroker(mode string) error {
	m, err := normalizeBrokerMode(mode)
	if err != nil {
		// Default to configured/secure when no usable mode given.
		m = cfg.LocalBroker.Mode
		if _, err2 := normalizeBrokerMode(m); err2 != nil {
			m = "secure"
		}
	}
	if err := applyLocalBrokerConfig(m); err != nil {
		return err
	}
	return persistLocalBrokerState()
}

func disableLocalBroker() error {
	brokerApplyMu.Lock()
	defer brokerApplyMu.Unlock()
	// Flag first so a concurrent supervisor tick can't restart it.
	cfg.LocalBroker.Enabled = false
	stopMeterClient()
	if err := brokerStop(); err != nil {
		return err
	}
	return persistLocalBrokerState()
}

func restartLocalBroker() error {
	if err := applyLocalBrokerConfig(cfg.LocalBroker.Mode); err != nil {
		return err
	}
	return persistLocalBrokerState()
}

func persistLocalBrokerState() error {
	if secrets != nil {
		// Encrypt any new plaintext passwords and persist the whole file.
		if _, _, err := secrets.cryptField(&cfg.LocalClient.Password, "local client password"); err != nil {
			return err
		}
		for i := range cfg.LocalBroker.Users {
			if _, _, err := secrets.cryptField(&cfg.LocalBroker.Users[i].Password, "local broker password"); err != nil {
				return err
			}
		}
	}
	return persistConfigFile()
}

func updateBrokerConfig(body struct {
	Enabled                 *bool            `json:"enabled"`
	Mode                    *string          `json:"mode"`
	Bind                    *string          `json:"bind"`
	PortUnsecured           *int             `json:"port_unsecured"`
	PortSecure              *int             `json:"port_secure"`
	AllowAnonymousUnsecured *bool            `json:"allow_anonymous_unsecured"`
	Users                   *[]brokerUserIn `json:"users"`
}) error {
	if body.Mode != nil {
		if _, err := normalizeBrokerMode(*body.Mode); err != nil {
			return err
		}
		cfg.LocalBroker.Mode = strings.ToLower(strings.TrimSpace(*body.Mode))
	}
	if body.Bind != nil {
		if ip := net.ParseIP(*body.Bind); ip == nil && *body.Bind != "localhost" {
			return fmt.Errorf("invalid bind address %q", *body.Bind)
		}
		cfg.LocalBroker.Bind = *body.Bind
	}
	if body.PortUnsecured != nil {
		if *body.PortUnsecured < 1 || *body.PortUnsecured > 65535 {
			return fmt.Errorf("invalid unsecured port")
		}
		cfg.LocalBroker.PortPlain = *body.PortUnsecured
	}
	if body.PortSecure != nil {
		if *body.PortSecure < 1 || *body.PortSecure > 65535 {
			return fmt.Errorf("invalid secure port")
		}
		cfg.LocalBroker.PortTLS = *body.PortSecure
	}
	if cfg.LocalBroker.PortPlain == cfg.LocalBroker.PortTLS {
		return fmt.Errorf("unsecured and secure ports must differ")
	}
	if body.AllowAnonymousUnsecured != nil {
		cfg.LocalBroker.AllowAnonymousPlain = *body.AllowAnonymousUnsecured
	}
	if body.Users != nil {
		users := make([]LocalBrokerUser, 0, len(*body.Users))
		seen := map[string]bool{}
		for i, u := range *body.Users {
			name := strings.TrimSpace(u.Username)
			if name == "" || len(name) > 64 || !validBrokerUsername(name) {
				return fmt.Errorf("user %d: invalid username", i)
			}
			if seen[name] {
				return fmt.Errorf("user %d: duplicate username %q", i, name)
			}
			seen[name] = true
			pw := u.Password
			if pw == "" {
				// Keep existing password on update.
				for _, e := range cfg.LocalBroker.Users {
					if e.Username == name {
						pw = e.Password
						break
					}
				}
				if pw == "" {
					return fmt.Errorf("user %q: password required for new users", name)
				}
			} else if len(pw) < 8 {
				return fmt.Errorf("user %q: password must be at least 8 characters", name)
			}
			users = append(users, LocalBrokerUser{Username: name, Password: pw})
		}
		cfg.LocalBroker.Users = users
	}
	wantEnabled := cfg.LocalBroker.Enabled
	if body.Enabled != nil {
		wantEnabled = *body.Enabled
	}
	if wantEnabled {
		return enableLocalBroker(cfg.LocalBroker.Mode)
	}
	return disableLocalBroker()
}

func validBrokerUsername(name string) bool {
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

func sanitizedBrokerConfig() map[string]interface{} {
	users := make([]map[string]string, 0, len(cfg.LocalBroker.Users))
	for _, u := range cfg.LocalBroker.Users {
		users = append(users, map[string]string{"username": u.Username, "password": "***"})
	}
	return map[string]interface{}{
		"enabled": cfg.LocalBroker.Enabled, "mode": cfg.LocalBroker.Mode,
		"bind": cfg.LocalBroker.Bind, "port_unsecured": cfg.LocalBroker.PortPlain,
		"port_secure": cfg.LocalBroker.PortTLS,
		"allow_anonymous_unsecured": cfg.LocalBroker.AllowAnonymousPlain,
		"users": users, "max_connections": cfg.LocalBroker.MaxConnections,
		"max_payload_bytes": cfg.LocalBroker.MaxPayloadBytes,
	}
}

// ---------- remote commands (platform -> gateway over existing channel) ----------

func execLocalMQTT(cmd CommandRequest) CommandResponse {
	mk := func(result interface{}, err error) CommandResponse {
		if err != nil {
			return CommandResponse{ID: cmd.ID, Status: "failed", Error: err.Error(), Timestamp: time.Now().UTC().Format(time.RFC3339)}
		}
		return CommandResponse{ID: cmd.ID, Status: "completed", Result: result, Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}
	switch cmd.Type {
	case "mqtt.local.status":
		st := getBrokerStatus()
		return mk(map[string]interface{}{
			"enabled": st.Enabled, "running": st.Running, "mode": st.Mode,
			"listeners": st.Listeners, "connected_clients": st.ConnectedClients,
			"version": st.Version, "cert_expiry": st.CertExpiry,
			"control_plane": st.ControlPlane, "local_client": localMeterClientStatus(),
		}, nil)
	case "mqtt.local.enable":
		var p struct {
			Mode string `json:"mode"`
		}
		_ = json.Unmarshal(cmd.Payload, &p)
		return mk(map[string]string{"mode": cfg.LocalBroker.Mode}, enableLocalBroker(p.Mode))
	case "mqtt.local.disable":
		return mk(map[string]bool{"enabled": false}, disableLocalBroker())
	case "mqtt.local.restart":
		if !cfg.LocalBroker.Enabled {
			return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "broker is disabled — enable it first", Timestamp: time.Now().UTC().Format(time.RFC3339)}
		}
		if err := restartLocalBroker(); err != nil {
			return mk(nil, err)
		}
		return mk(map[string]bool{"restarted": true}, nil)
	case "mqtt.local.config":
		var p struct {
			Enabled                 *bool          `json:"enabled"`
			Mode                    *string        `json:"mode"`
			Bind                    *string        `json:"bind"`
			PortUnsecured           *int           `json:"port_unsecured"`
			PortSecure              *int           `json:"port_secure"`
			AllowAnonymousUnsecured *bool          `json:"allow_anonymous_unsecured"`
			Users                   *[]brokerUserIn `json:"users"`
		}
		if len(cmd.Payload) == 0 {
			return mk(sanitizedBrokerConfig(), nil)
		}
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "invalid config payload", Timestamp: time.Now().UTC().Format(time.RFC3339)}
		}
		if err := updateBrokerConfig(p); err != nil {
			return mk(nil, err)
		}
		return mk(sanitizedBrokerConfig(), nil)
	case "mqtt.local.logs":
		var p struct {
			Lines int `json:"lines"`
		}
		_ = json.Unmarshal(cmd.Payload, &p)
		if p.Lines <= 0 {
			p.Lines = 100
		}
		logs, err := brokerLogs(p.Lines)
		return mk(map[string]interface{}{"logs": logs}, err)
	case "mqtt.local.clients":
		return mk(map[string]interface{}{"clients": brokerConnectionTable()}, nil)
	case "mqtt.local.meters":
		return mk(meterStatsSnapshot(), nil)
	case "mqtt.local.test":
		var p struct {
			Topic   string                 `json:"topic"`
			Payload map[string]interface{} `json:"payload"`
		}
		if err := json.Unmarshal(cmd.Payload, &p); err != nil || p.Topic == "" {
			return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "topic and payload required", Timestamp: time.Now().UTC().Format(time.RFC3339)}
		}
		return mk(map[string]string{"published": p.Topic}, publishLocalTest(p.Topic, p.Payload))
	default:
		return CommandResponse{ID: cmd.ID, Status: "rejected", Error: "unknown local mqtt command: " + cmd.Type, Timestamp: time.Now().UTC().Format(time.RFC3339)}
	}
}

// ---------- supervisor: reconcile desired vs actual every 30s ----------

func startLocalMQTTSupervisor(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				stopMeterClient()
				return
			case <-ticker.C:
				reconcileLocalMQTT()
			}
		}
	}()
	go func() {
		// Initial bring-up after provisioning so first-boot enable works.
		time.Sleep(5 * time.Second)
		reconcileLocalMQTT()
	}()
}

func reconcileLocalMQTT() {
	// Skip the tick when an apply is in flight (see brokerApplyMu).
	if !brokerApplyMu.TryLock() {
		return
	}
	defer brokerApplyMu.Unlock()
	b := &cfg.LocalBroker
	running := brokerServiceActive()
	if b.Enabled {
		if !running {
			logger.WithField("mode", b.Mode).Info("local broker: (re)starting to match desired state")
			if err := applyLocked(b.Mode); err != nil {
				logger.WithError(err).Warn("local broker: reconcile failed")
				return
			}
			_ = persistLocalBrokerState()
		}
		ensureMeterClient()
	} else {
		if running {
			_ = brokerStop()
		}
		stopMeterClient()
	}
}
