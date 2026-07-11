package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
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
}

type terminalAgent struct {
	cfg  TerminalConfig
	key  []byte
	seq  int64
	conn *websocket.Conn

	mu         sync.Mutex
	connected  bool
	lastSeq    int64
	sessions   map[string]*ptySession
	uploads    map[string]*os.File
	pingStart  int64
}

func startTerminalAgent(ctx context.Context) {
	agent := &terminalAgent{
		cfg:     cfg.Terminal,
		key:     deriveSigningKey(hashAgentSecret(cfg.Terminal.AgentSecret), cfg.Terminal.SigningPepper),
		sessions: make(map[string]*ptySession),
		uploads:  make(map[string]*os.File),
	}

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

	// read loop until closed
	readErr := a.readLoop(ctx)

	a.mu.Lock()
	a.connected = false
	a.conn = nil
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
	case "error":
		logger.WithField("detail", string(arr[1])).Warn("terminal agent: backend error")
	}
}

func (a *terminalAgent) onConnected() {
	a.mu.Lock()
	a.connected = true
	a.lastSeq = 0
	a.mu.Unlock()

	// AGENT_HELLO
	a.sendMessage(msgAgentHello, map[string]interface{}{
		"agentVersion": version,
		"capabilities": []string{"terminal", "file-transfer"},
		"os":           runtimeGOOS(),
		"arch":         runtimeGOARCH(),
		"hostname":     hostname(),
	}, "")

	// heartbeat
	go a.heartbeatLoop()
	logger.Info("terminal agent: session established with backend")
}

func (a *terminalAgent) heartbeatLoop() {
	ticker := time.NewTicker(time.Duration(cfg.Terminal.HeartbeatMs) * time.Millisecond)
	defer ticker.Stop()
	for {
		a.mu.Lock()
		connected := a.connected
		conn := a.conn
		a.mu.Unlock()
		if !connected || conn == nil {
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
	if s, ok := p["shell"].(string); ok && s != "" {
		shell = s
	}
	cols, _ := toUint16(p["cols"], 80)
	rows, _ := toUint16(p["rows"], 24)

	cmd := exec.Command(shell)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		a.sendMessage(msgError, map[string]interface{}{"message": err.Error()}, sessionID)
		return
	}

	a.mu.Lock()
	a.sessions[sessionID] = &ptySession{file: f, cmd: cmd}
	a.mu.Unlock()

	// pump PTY output -> backend
	go func() {
		buf := make([]byte, 4096)
		for {
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
	}()

	go func() {
		_ = cmd.Wait()
	}()

	a.sendMessage(msgSessionReady, map[string]interface{}{"shell": shell}, sessionID)
	logger.WithField("session", sessionID).Info("terminal agent: spawned PTY")
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
	a.mu.Unlock()
	if sess != nil {
		_, _ = sess.file.Write(raw)
	}
}

func (a *terminalAgent) handleSessionResize(msg *TerminalMessage) {
	p := msg.Payload
	cols, _ := toUint16(p["cols"], 80)
	rows, _ := toUint16(p["rows"], 24)
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

func (a *terminalAgent) handleFileInit(msg *TerminalMessage) {
	p := msg.Payload
	direction, _ := p["direction"].(string)
	remotePath, _ := p["path"].(string)
	sessionID := msg.SessionID

	if direction == "upload" {
		if strings.Contains(remotePath, "..") {
			a.sendMessage(msgError, map[string]interface{}{"message": "invalid path"}, sessionID)
			return
		}
		dir := a.cfg.FileDir
		if dir == "" {
			dir = "/tmp"
		}
		target := filepath.Join(dir, filepath.Base(remotePath))
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			a.sendMessage(msgError, map[string]interface{}{"message": err.Error()}, sessionID)
			return
		}
		a.mu.Lock()
		a.uploads[sessionID] = f
		a.mu.Unlock()
		a.sendMessage(msgFileTransferStatus, map[string]interface{}{"status": "ready", "path": target}, sessionID)
	} else {
		// download: stream the file back to the browser
		if strings.Contains(remotePath, "..") || !pathExists(remotePath) {
			a.sendMessage(msgError, map[string]interface{}{"message": "file not found: " + remotePath}, sessionID)
			return
		}
		info, _ := os.Stat(remotePath)
		a.sendMessage(msgFileTransferInit, map[string]interface{}{
			"direction": "download",
			"path":      remotePath,
			"size":      info.Size(),
			"mode":      int(info.Mode().Perm()),
		}, sessionID)

		go func() {
			f, err := os.Open(remotePath)
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
	a.mu.Lock()
	f := a.uploads[msg.SessionID]
	a.mu.Unlock()
	if f != nil {
		_, _ = f.Write(raw)
	}
}

func (a *terminalAgent) handleFileEnd(msg *TerminalMessage) {
	a.mu.Lock()
	f := a.uploads[msg.SessionID]
	delete(a.uploads, msg.SessionID)
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
	if !a.connected || a.conn == nil {
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
	id := cfg.Terminal.GatewayID
	if id == "" {
		id = cfg.Gateway.DeviceID
	}
	return id
}

func (a *terminalAgent) writeFrame(frame string) {
	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()
	if conn == nil {
		return
	}
	_ = conn.WriteMessage(websocket.TextMessage, []byte(frame))
}

func (a *terminalAgent) closeAll() {
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

func runtimeGOOS() string  { return runtime.GOOS }
func runtimeGOARCH() string { return runtime.GOARCH }

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
