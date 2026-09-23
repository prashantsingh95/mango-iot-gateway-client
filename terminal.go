package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

// ---------- Reverse-connection terminal agent ----------
//
// Connects OUT to the Mango backend's Socket.IO `/agent` namespace over a
// persistent TLS WebSocket — no inbound ports, works behind NAT/CGNAT/firewall.
// Authenticates with the gateway id + secret, keeps a 30s heartbeat with
// exponential-backoff reconnect, spawns PTYs for terminal sessions, and
// supports SCP-like file transfer. All messages are HMAC-signed.

type ptySession struct {
	file *os.File
	cmd  *exec.Cmd
	// Phase 5 — session lifetime enforcement.
	startedAt  time.Time
	lastActive time.Time
}

type terminalAgent struct {
	cfg  TerminalConfig
	key  []byte
	seq  int64
	conn *websocket.Conn
	ctx  context.Context

	mu          sync.Mutex
	writeMu     sync.Mutex
	connected   bool
	ready       bool
	lastSeq     int64
	sessions    map[string]*ptySession
	uploads     map[string]*os.File
	uploadBytes map[string]int64
	pingStart   int64
	connGen     int64
}

func startTerminalAgent(ctx context.Context) {
	agent := &terminalAgent{
		cfg:         cfg.Terminal,
		key:         deriveSigningKey(hashAgentSecret(cfg.Terminal.AgentSecret), cfg.Terminal.SigningPepper),
		sessions:    make(map[string]*ptySession),
		uploads:     make(map[string]*os.File),
		uploadBytes: make(map[string]int64),
		ctx:         ctx,
	}

	// Unblock readLoop/WriteMessage as soon as the process is shutting down.
	// Without this, cancel() alone leaves the agent stuck in a blocking
	// ReadMessage and SIGTERM never completes within TimeoutStopSec.
	go func() {
		<-ctx.Done()
		agent.closeAll()
	}()

	backoff := time.Duration(cfg.Terminal.ReconnectBaseMs) * time.Millisecond
	if backoff <= 0 {
		backoff = time.Second
	}
	maxBackoff := time.Duration(cfg.Terminal.ReconnectMaxMs) * time.Millisecond
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}

	for {
		select {
		case <-ctx.Done():
			agent.closeAll()
			return
		default:
		}

		err := agent.connect(ctx)
		if err != nil {
			logger.WithError(err).Warn("terminal agent: connection failed, retrying")
		}
		agent.closeAll()

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

func (a *terminalAgent) wsURL() string {
	u := strings.TrimRight(a.cfg.BackendWSURL, "/")
	if !strings.HasPrefix(u, "ws://") && !strings.HasPrefix(u, "wss://") {
		u = "ws://" + u
	}
	return u + "/socket.io/?EIO=4&transport=websocket"
}

func (a *terminalAgent) connect(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	if a.cfg.InsecureSkipVerify {
		// Test-only escape hatch: certificate verification OFF. Never enable
		// in production — the agent cannot tell the platform from an imposter.
		logger.Error("terminal agent: TLS verification DISABLED (insecure_skip_verify) — test use only")
		dialer.TLSClientConfig = tlsConfigInsecure()
	}
	conn, _, err := dialer.Dial(a.wsURL(), nil)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()

	logger.WithField("url", a.wsURL()).Info("terminal agent: connected to backend")

	a.mu.Lock()
	a.connGen++
	gen := a.connGen
	a.mu.Unlock()

	// read loop until closed
	readErr := a.readLoop(ctx)

	a.mu.Lock()
	if a.connGen == gen {
		a.connected = false
		a.ready = false
		if a.conn != nil {
			_ = a.conn.Close()
			a.conn = nil
		}
	}
	a.mu.Unlock()
	return readErr
}

func (a *terminalAgent) readLoop(ctx context.Context) error {
	conn := a.conn
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		frame := string(data)
		if len(frame) == 0 {
			continue
		}
		etype := frame[0:1]
		body := frame[1:]
		switch etype {
		case "0": // engine.io OPEN
			a.sendConnect()
		case "2": // engine.io PING
			a.writeFrame("3") // PONG
		case "4": // engine.io MESSAGE (socket.io packet)
			a.handleSocketIOPacket(body)
		}
	}
}

func (a *terminalAgent) handleSocketIOPacket(body string) {
	// body: <socket.io type><namespace[,data]>
	if len(body) < 1 {
		return
	}
	ptype := body[0:1]
	rest := body[1:]
	// strip namespace prefix if present
	if strings.HasPrefix(rest, "/") {
		if idx := strings.Index(rest, ","); idx >= 0 {
			rest = rest[idx+1:]
		} else {
			rest = ""
		}
	}

	switch ptype {
	case "0": // connect ack
		a.onConnected()
	case "4": // connect_error
		logger.WithField("detail", rest).Error("terminal agent: connect error from backend")
	case "2": // event
		a.handleEvent(rest)
	}
}

func (a *terminalAgent) handleEvent(data string) {
	// data: JSON array [eventName, payload]
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(data), &arr); err != nil || len(arr) < 2 {
		return
	}
	var event string
	if err := json.Unmarshal(arr[0], &event); err != nil {
		return
	}
	switch event {
	case "message":
		var msg TerminalMessage
		if err := json.Unmarshal(arr[1], &msg); err != nil {
			return
		}
		a.onBackendMessage(&msg)
	case "ready":
		logger.Info("terminal agent: backend accepted connection")
		a.onReady()
	case "error":
		logger.WithField("detail", string(arr[1])).Warn("terminal agent: backend error")
	}
}

// onReady runs after NestJS finished async agent auth (handleConnection).
// AGENT_HELLO/heartbeats sent earlier were dropped or raced the auth path.
func (a *terminalAgent) onReady() {
	a.mu.Lock()
	a.ready = true
	gen := a.connGen
	a.mu.Unlock()

	a.sendMessage(msgAgentHello, map[string]interface{}{
		"agentVersion": version,
		"capabilities": []string{"terminal", "file-transfer"},
		"os":           runtimeGOOS(),
		"arch":         runtimeGOARCH(),
		"hostname":     hostname(),
	}, "")
	go a.heartbeatLoop(gen)
	logger.Info("terminal agent: session established with backend")
}

func (a *terminalAgent) onConnected() {
	a.mu.Lock()
	a.connected = true
	a.ready = false
	a.lastSeq = 0
	a.mu.Unlock()
	// Wait for backend 'ready' (auth complete) before AGENT_HELLO/heartbeat.
	logger.Info("terminal agent: namespace connected, waiting for backend ready")
}

func (a *terminalAgent) heartbeatLoop(gen int64) {
	ticker := time.NewTicker(time.Duration(cfg.Terminal.HeartbeatMs) * time.Millisecond)
	defer ticker.Stop()
	for {
		a.mu.Lock()
		alive := a.connected && a.conn != nil && a.connGen == gen
		a.mu.Unlock()
		if !alive {
			return
		}
		a.pingStart = time.Now().UnixMilli()
		a.sendMessage(msgHeartbeat, map[string]interface{}{"ts": a.pingStart}, "")
		<-ticker.C
	}
}

func (a *terminalAgent) onBackendMessage(msg *TerminalMessage) {
	if !verifyMessage(msg, a.key) {
		logger.WithField("type", msg.Type).Warn("terminal agent: dropping unsigned/forged message")
		return
	}
	a.mu.Lock()
	if msg.SequenceNumber <= a.lastSeq {
		a.mu.Unlock()
		logger.WithField("seq", msg.SequenceNumber).Warn("terminal agent: dropping replayed message")
		return
	}
	a.lastSeq = msg.SequenceNumber
	a.mu.Unlock()

	switch msg.Type {
	case msgHeartbeatAck:
		if echo, ok := msg.Payload["echo"].(float64); ok {
			latency := time.Now().UnixMilli() - int64(echo)
			logger.WithField("latency_ms", latency).Debug("terminal agent: heartbeat ack")
		}
	case msgSessionStart:
		a.handleSessionStart(msg)
	case msgSessionData:
		a.handleSessionData(msg)
	case msgSessionResize:
		a.handleSessionResize(msg)
	case msgSessionEnd:
		a.handleSessionEnd(msg)
	case msgFileTransferInit:
		a.handleFileInit(msg)
	case msgFileTransferData:
		a.handleFileData(msg)
	case msgFileTransferEnd:
		a.handleFileEnd(msg)
	default:
		logger.WithField("type", msg.Type).Debug("terminal agent: ignoring message type")
	}
}

// ---------- terminal sessions (PTY) ----------

func (a *terminalAgent) handleSessionStart(msg *TerminalMessage) {
	p := msg.Payload
	sessionID := msg.SessionID

	a.mu.Lock()
	if _, ok := a.sessions[sessionID]; ok {
		a.mu.Unlock()
		a.sendMessage(msgSessionReady, map[string]interface{}{"resumed": true}, sessionID)
		return
	}
	a.mu.Unlock()

	shell := a.cfg.Shell
	if s, ok := p["shell"].(string); ok && s != "" && s != shell {
		// Phase 5 / §27 — the backend may SUGGEST a shell, never impose one.
		// Only shells on the local allowlist are honored; anything else keeps
		// the pinned default (and is logged for audit).
		allowed := false
		for _, a := range a.cfg.ShellAllowlist {
			if s == a {
				allowed = true
				break
			}
		}
		if allowed {
			shell = s
		} else {
			logger.WithFields(map[string]interface{}{"requested": s, "session": sessionID}).Warn("terminal agent: rejecting non-allowlisted shell")
		}
	}
	cols, _ := toUint16(p["cols"], 80)
	rows, _ := toUint16(p["rows"], 24)

	// Prefer an absolute cwd from the backend; otherwise start in /home (or ~)
	// so the prompt is not the agent's systemd WorkingDirectory (often /).
	startDir := ""
	if s, ok := p["cwd"].(string); ok {
		startDir = s
	}
	if startDir == "" || !filepath.IsAbs(startDir) {
		if st, err := os.Stat("/home"); err == nil && st.IsDir() {
			startDir = "/home"
		} else if home, err := os.UserHomeDir(); err == nil && home != "" {
			startDir = home
		} else {
			startDir = "/"
		}
	}
	if st, err := os.Stat(startDir); err != nil || !st.IsDir() {
		startDir = "/"
	}

	// Phase 5 — concurrent session cap (fail closed, audited).
	a.mu.Lock()
	if len(a.sessions) >= a.maxSessions() {
		a.mu.Unlock()
		a.sendMessage(msgError, map[string]interface{}{"message": "too many terminal sessions"}, sessionID)
		return
	}
	a.mu.Unlock()

	cmd := exec.Command(shell)
	cmd.Dir = startDir
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		a.sendMessage(msgError, map[string]interface{}{"message": err.Error()}, sessionID)
		return
	}

	now := time.Now()
	a.mu.Lock()
	a.sessions[sessionID] = &ptySession{file: f, cmd: cmd, startedAt: now, lastActive: now}
	a.mu.Unlock()

	// Phase 5 — idle + absolute lifetime reaper for THIS session.
	go a.reapSession(sessionID)

	// pump PTY output -> backend
	go func(ctx context.Context) {
		buf := make([]byte, 4096)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			n, err := f.Read(buf)
			if n > 0 {
				a.sendMessage(msgSessionOutput, map[string]interface{}{
					"data": base64.StdEncoding.EncodeToString(buf[:n]),
				}, sessionID)
			}
			if err != nil {
				break
			}
		}
		a.sendMessage(msgSessionEnd, map[string]interface{}{"reason": "process exited"}, sessionID)
		a.mu.Lock()
		delete(a.sessions, sessionID)
		a.mu.Unlock()
	}(a.ctx)

	go func() {
		_ = cmd.Wait()
	}()

	a.sendMessage(msgSessionReady, map[string]interface{}{"shell": shell}, sessionID)
	logger.WithField("session", sessionID).Info("terminal agent: spawned PTY")
}

func (a *terminalAgent) maxSessions() int {
	if a.cfg.MaxSessions > 0 {
		return a.cfg.MaxSessions
	}
	return 5
}

// reapSession kills a PTY when it idles past IdleTimeoutMinutes or lives past
// MaxSessionHours. Ticks at 1/6th of the idle window (min 1m) to bound drift.
func (a *terminalAgent) reapSession(sessionID string) {
	idle := time.Duration(a.cfg.IdleTimeoutMinutes) * time.Minute
	if idle <= 0 {
		idle = 30 * time.Minute
	}
	maxLife := time.Duration(a.cfg.MaxSessionHours) * time.Hour
	if maxLife <= 0 {
		maxLife = 8 * time.Hour
	}
	tick := idle / 6
	if tick < time.Minute {
		tick = time.Minute
	}
	if tick > maxLife {
		tick = maxLife
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for range t.C {
		a.mu.Lock()
		sess, ok := a.sessions[sessionID]
		var idleFor, age time.Duration
		if ok {
			idleFor = time.Since(sess.lastActive)
			age = time.Since(sess.startedAt)
		}
		a.mu.Unlock()
		if !ok {
			return // session already closed
		}
		if idleFor >= idle || age >= maxLife {
			reason := "idle timeout"
			if age >= maxLife {
				reason = "max session lifetime"
			}
			logger.WithFields(map[string]interface{}{"session": sessionID, "reason": reason}).Warn("terminal agent: reaping session")
			a.sendMessage(msgSessionEnd, map[string]interface{}{"reason": reason}, sessionID)
			a.mu.Lock()
			if s, ok := a.sessions[sessionID]; ok {
				_ = s.file.Close()
				if s.cmd.Process != nil {
					_ = s.cmd.Process.Kill()
				}
				delete(a.sessions, sessionID)
			}
			a.mu.Unlock()
			return
		}
	}
}

func (a *terminalAgent) touchSession(sessionID string) {
	a.mu.Lock()
	if s, ok := a.sessions[sessionID]; ok {
		s.lastActive = time.Now()
	}
	a.mu.Unlock()
}

func (a *terminalAgent) handleSessionData(msg *TerminalMessage) {
	data, ok := msg.Payload["data"].(string)
	if !ok {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return
	}
	a.mu.Lock()
	sess := a.sessions[msg.SessionID]
	if sess != nil {
		sess.lastActive = time.Now()
	}
	a.mu.Unlock()
	if sess != nil {
		_, _ = sess.file.Write(raw)
	}
}

func (a *terminalAgent) handleSessionResize(msg *TerminalMessage) {
	p := msg.Payload
	cols, _ := toUint16(p["cols"], 80)
	rows, _ := toUint16(p["rows"], 24)
	a.touchSession(msg.SessionID)
	a.mu.Lock()
	sess := a.sessions[msg.SessionID]
	a.mu.Unlock()
	if sess != nil {
		_ = pty.Setsize(sess.file, &pty.Winsize{Rows: rows, Cols: cols})
	}
}

func (a *terminalAgent) handleSessionEnd(msg *TerminalMessage) {
	a.mu.Lock()
	sess := a.sessions[msg.SessionID]
	delete(a.sessions, msg.SessionID)
	a.mu.Unlock()
	if sess != nil {
		_ = sess.file.Close()
		_ = sess.cmd.Process.Kill()
	}
}

// ---------- file transfer ----------

// jailPath resolves p inside the configured file dir and rejects escapes
// (absolute paths outside the jail, ".." traversal, symlink breakouts).
func (a *terminalAgent) jailPath(p string) (string, error) {
	dir := a.cfg.FileDir
	if dir == "" {
		dir = "/tmp"
	}
	jailAbs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	candidate := p
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(jailAbs, candidate)
	}
	cleaned := filepath.Clean(candidate)
	rel, err := filepath.Rel(jailAbs, cleaned)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes file directory")
	}
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		// Missing file (upload target): enforce the lexical jail only.
		if os.IsNotExist(err) {
			return cleaned, nil
		}
		return "", err
	}
	if rel2, err := filepath.Rel(jailAbs, resolved); err != nil || rel2 == ".." || strings.HasPrefix(rel2, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("symlink escapes file directory")
	}
	return resolved, nil
}

func (a *terminalAgent) handleFileInit(msg *TerminalMessage) {
	p := msg.Payload
	direction, _ := p["direction"].(string)
	remotePath, _ := p["path"].(string)
	sessionID := msg.SessionID
	maxBytes := a.cfg.MaxFileBytes
	if maxBytes <= 0 {
		maxBytes = 25 * 1024 * 1024
	}

	if direction == "upload" {
		// Uploads land inside the jail under the client basename (never
		// attacker-controlled directories).
		target, err := a.jailPath(filepath.Base(remotePath))
		if err != nil {
			a.sendMessage(msgError, map[string]interface{}{"message": "invalid path"}, sessionID)
			return
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			a.sendMessage(msgError, map[string]interface{}{"message": err.Error()}, sessionID)
			return
		}
		a.mu.Lock()
		a.uploads[sessionID] = f
		a.uploadBytes[sessionID] = 0
		a.mu.Unlock()
		a.sendMessage(msgFileTransferStatus, map[string]interface{}{"status": "ready", "path": target}, sessionID)
	} else {
		// Phase 5 / §27 — downloads are jailed to FileDir (previously ANY
		// absolute path was readable, e.g. /etc/shadow) and size-capped.
		resolved, err := a.jailPath(remotePath)
		if err != nil || !pathExists(resolved) {
			a.sendMessage(msgError, map[string]interface{}{"message": "file not found"}, sessionID)
			return
		}
		info, err := os.Stat(resolved)
		if err != nil || info.IsDir() || info.Size() > maxBytes {
			a.sendMessage(msgError, map[string]interface{}{"message": "file not available"}, sessionID)
			return
		}
		a.sendMessage(msgFileTransferInit, map[string]interface{}{
			"direction": "download",
			"path":      resolved,
			"size":      info.Size(),
			"mode":      int(info.Mode().Perm()),
		}, sessionID)

		go func() {
			f, err := os.Open(resolved)
			if err != nil {
				a.sendMessage(msgError, map[string]interface{}{"message": err.Error()}, sessionID)
				return
			}
			defer f.Close()
			buf := make([]byte, 32*1024)
			for {
				n, err := f.Read(buf)
				if n > 0 {
					a.sendMessage(msgFileTransferData, map[string]interface{}{
						"data": base64.StdEncoding.EncodeToString(buf[:n]),
					}, sessionID)
				}
				if err != nil {
					break
				}
			}
			a.sendMessage(msgFileTransferEnd, map[string]interface{}{"status": "done"}, sessionID)
		}()
	}
}

func (a *terminalAgent) handleFileData(msg *TerminalMessage) {
	data, ok := msg.Payload["data"].(string)
	if !ok {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return
	}
	maxBytes := a.cfg.MaxFileBytes
	if maxBytes <= 0 {
		maxBytes = 25 * 1024 * 1024
	}
	a.mu.Lock()
	f := a.uploads[msg.SessionID]
	written := a.uploadBytes[msg.SessionID]
	var over bool
	if f != nil {
		if int64(len(raw))+written > maxBytes {
			over = true
		} else {
			if _, err := f.Write(raw); err == nil {
				a.uploadBytes[msg.SessionID] = written + int64(len(raw))
			}
		}
	}
	a.mu.Unlock()
	if over {
		a.sendMessage(msgError, map[string]interface{}{"message": "upload exceeds size limit"}, msg.SessionID)
	}
}

func (a *terminalAgent) handleFileEnd(msg *TerminalMessage) {
	a.mu.Lock()
	f := a.uploads[msg.SessionID]
	delete(a.uploads, msg.SessionID)
	delete(a.uploadBytes, msg.SessionID)
	a.mu.Unlock()
	if f != nil {
		_ = f.Close()
		a.sendMessage(msgFileTransferStatus, map[string]interface{}{"status": "done"}, msg.SessionID)
		logger.WithField("session", msg.SessionID).Info("terminal agent: upload complete")
	}
}

// ---------- outbound ----------

func (a *terminalAgent) sendConnect() {
	auth := map[string]interface{}{
		"gatewayId": a.gatewayID(),
		"secret":    a.cfg.AgentSecret,
	}
	b, _ := json.Marshal(auth)
	a.writeFrame("40/agent," + string(b))
}

func (a *terminalAgent) sendMessage(msgType string, payload map[string]interface{}, sessionID string) {
	a.mu.Lock()
	if !a.connected || !a.ready || a.conn == nil {
		a.mu.Unlock()
		return
	}
	a.seq++
	seq := a.seq
	a.mu.Unlock()

	m := &TerminalMessage{
		Version:        terminalProtoVersion,
		Type:           msgType,
		TenantID:       "",
		GatewayID:      a.gatewayID(),
		SessionID:      sessionID,
		Timestamp:      time.Now().UnixMilli(),
		SequenceNumber: seq,
		Payload:        payload,
	}
	m.Signature = signMessage(m, a.key)

	b, err := json.Marshal([]interface{}{"message", m})
	if err != nil {
		return
	}
	a.writeFrame("42/agent," + string(b))
}

func (a *terminalAgent) gatewayID() string {
	// Prefer the platform UUID: explicit config first, then the UUID file
	// written at provisioning time. The deviceId is only a fallback for
	// agents that never provisioned (backend also accepts it, but the UUID
	// is canonical for relay keys and dashboard sessions).
	if id := strings.TrimSpace(cfg.Terminal.GatewayID); id != "" {
		return id
	}
	if id := loadPersistedGatewayID(); id != "" {
		return id
	}
	return getDeviceID()
}

func (a *terminalAgent) writeFrame(frame string) {
	// Hold the write lock for the whole WriteMessage so engine.io pings,
	// heartbeats and session output never interleave on the socket
	// (concurrent gorilla/websocket writes can corrupt frames → close 1005).
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()
	if conn == nil {
		return
	}
	// Bounded write: a half-open TCP peer must never pin writeMu forever
	// (that blocked SIGTERM shutdown past systemd's TimeoutStopSec).
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = conn.WriteMessage(websocket.TextMessage, []byte(frame))
}

func (a *terminalAgent) closeAll() {
	// Same lock order as writeFrame (writeMu → mu) so shutdown can interrupt
	// a blocked WriteMessage without deadlocking against the write path.
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	a.mu.Lock()
	defer a.mu.Unlock()
	for id, s := range a.sessions {
		_ = s.file.Close()
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		delete(a.sessions, id)
	}
	for id, f := range a.uploads {
		_ = f.Close()
		delete(a.uploads, id)
	}
	if a.conn != nil {
		_ = a.conn.Close()
		a.conn = nil
	}
	a.connected = false
	a.ready = false
}

// ---------- helpers ----------

func toUint16(v interface{}, def uint16) (uint16, bool) {
	switch n := v.(type) {
	case float64:
		return uint16(n), true
	case int:
		return uint16(n), true
	case json.Number:
		i, _ := n.Int64()
		return uint16(i), true
	}
	return def, false
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func tlsConfigInsecure() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true}
}

func runtimeGOOS() string   { return runtime.GOOS }
func runtimeGOARCH() string { return runtime.GOARCH }

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
