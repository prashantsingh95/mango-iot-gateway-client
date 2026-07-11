package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ---------- Reverse-connection terminal protocol ----------
// Mirrors backend/src/terminal/protocol.ts and gateway-agent/src/protocol.ts.
// Every agent->backend message is HMAC-SHA256 signed with a per-gateway key
// derived from HMAC(SIGNING_PEPPER, SHA256(secret)). The backend signs every
// message it sends to the agent the same way; the agent verifies before acting.
//
// The canonical form is a deterministic, key-sorted JSON (HTML-escaping
// disabled) so signatures are byte-identical across TypeScript and Go.

const (
	msgAgentHello         = "AGENT_HELLO"
	msgHeartbeat          = "HEARTBEAT"
	msgHeartbeatAck       = "HEARTBEAT_ACK"
	msgSessionStart       = "SESSION_START"
	msgSessionReady       = "SESSION_READY"
	msgSessionResize      = "SESSION_RESIZE"
	msgSessionEnd         = "SESSION_END"
	msgSessionData        = "SESSION_DATA"
	msgSessionOutput      = "SESSION_OUTPUT"
	msgSessionStatus      = "SESSION_STATUS"
	msgFileTransferInit   = "FILE_TRANSFER_INIT"
	msgFileTransferData   = "FILE_TRANSFER_DATA"
	msgFileTransferEnd    = "FILE_TRANSFER_END"
	msgFileTransferStatus = "FILE_TRANSFER_STATUS"
	msgError              = "ERROR"
)

const terminalProtoVersion = 1

// TerminalMessage is the wire envelope. Payload is an arbitrary JSON object.
type TerminalMessage struct {
	Version        int                    `json:"version"`
	Type           string                 `json:"type"`
	TenantID       string                 `json:"tenantId"`
	GatewayID      string                 `json:"gatewayId"`
	SessionID      string                 `json:"sessionId"`
	UserID         string                 `json:"userId,omitempty"`
	Timestamp      int64                  `json:"timestamp"`
	SequenceNumber int64                  `json:"sequenceNumber"`
	Payload        map[string]interface{} `json:"payload,omitempty"`
	Signature      string                 `json:"signature,omitempty"`
}

func hashAgentSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

func deriveSigningKey(secretHash, pepper string) []byte {
	mac := hmac.New(sha256.New, []byte(pepper))
	mac.Write([]byte(secretHash))
	return mac.Sum(nil)
}

// marshalSortedNoEscape produces JSON with object keys sorted (like a map) and
// without HTML escaping, matching the backend's stableStringify exactly.
func marshalSortedNoEscape(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// canonical returns the deterministic JSON used for signing (signature excluded).
func canonical(m *TerminalMessage) ([]byte, error) {
	cp := map[string]interface{}{
		"version":        m.Version,
		"type":           m.Type,
		"tenantId":       m.TenantID,
		"gatewayId":      m.GatewayID,
		"sessionId":      m.SessionID,
		"timestamp":      m.Timestamp,
		"sequenceNumber": m.SequenceNumber,
	}
	if m.UserID != "" {
		cp["userId"] = m.UserID
	}
	if m.Payload != nil {
		cp["payload"] = m.Payload
	}
	return marshalSortedNoEscape(cp)
}

func signMessage(m *TerminalMessage, key []byte) string {
	b, _ := canonical(m)
	mac := hmac.New(sha256.New, key)
	mac.Write(b)
	return hex.EncodeToString(mac.Sum(nil))
}

func verifyMessage(m *TerminalMessage, key []byte) bool {
	if m.Signature == "" {
		return false
	}
	b, err := canonical(m)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(b)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(m.Signature))
}
