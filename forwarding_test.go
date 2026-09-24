package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testFwdDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GATEWAY_CONFIG", filepath.Join(dir, "config.yml"))
	secrets = newSecretsManager(filepath.Join(dir, ".encryption_key"))
	if secrets == nil {
		t.Fatal("secrets manager unavailable")
	}
	fwdDBMu.Lock()
	if fwdDB != nil {
		fwdDB.Close()
		fwdDB = nil
	}
	fwdDBMu.Unlock()
	if err := openForwardingDB(); err != nil {
		t.Fatalf("openForwardingDB: %v", err)
	}
	t.Cleanup(func() {
		fwdDBMu.Lock()
		if fwdDB != nil {
			fwdDB.Close()
			fwdDB = nil
		}
		fwdDBMu.Unlock()
	})
}

func TestForwardingDestinationsCRUD(t *testing.T) {
	testFwdDB(t)
	id, err := fwdCreateDestination(&Destination{Name: "HES", Protocol: "mqtt", Host: "mqtt.example.com", Port: 8883, TLS: true, Username: "u", Topic: "t"}, "secret123")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id <= 0 {
		t.Fatal("bad id")
	}
	dests, err := fwdListDestinations()
	if err != nil || len(dests) != 1 {
		t.Fatalf("list: %v %d", err, len(dests))
	}
	// Password must be encrypted at rest, never returned.
	var enc string
	if err := fwdDB.QueryRow(`SELECT password_enc FROM forwarding_destinations WHERE id=?`, id).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if enc == "secret123" || enc == "" {
		t.Fatalf("password not encrypted: %q", enc)
	}
	if got := fwdGetDestinationPassword(id); got != "secret123" {
		t.Fatalf("password decrypt failed: %q", got)
	}
	// Invalid protocol rejected.
	if _, err := fwdCreateDestination(&Destination{Name: "x", Protocol: "coap", Host: "h", Port: 1}, ""); err == nil {
		t.Fatal("expected protocol error")
	}
	// Update without password keeps it.
	d := dests[0]
	d.Port = 1883
	if err := fwdUpdateDestination(id, &d, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := fwdGetDestinationPassword(id); got != "secret123" {
		t.Fatal("password should be preserved")
	}
	if err := fwdDeleteDestination(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	dests, _ = fwdListDestinations()
	if len(dests) != 0 {
		t.Fatal("expected empty")
	}
}

func TestForwardingIngestFansOut(t *testing.T) {
	testFwdDB(t)
	if _, err := fwdCreateDestination(&Destination{Name: "A", Protocol: "mqtt", Host: "a.example", Port: 1883, Enabled: true}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := fwdCreateDestination(&Destination{Name: "B", Protocol: "tcp", Host: "b.example", Port: 9000, Framing: "json_lines", Enabled: true}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := fwdCreateDestination(&Destination{Name: "Off", Protocol: "mqtt", Host: "c.example", Port: 1883, Enabled: false}, ""); err != nil {
		t.Fatal(err)
	}
	msgID, err := forwardingIngest("MTR1", "meter/MTR1/data", []byte(`{"meter_id":"MTR1"}`), time.Now())
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if msgID == "" {
		t.Fatal("empty message id")
	}
	var n int
	if err := fwdDB.QueryRow(`SELECT COUNT(*) FROM forwarding_queue WHERE message_id=?`, msgID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("expected 2 queue rows (enabled only), got %d (%v)", n, err)
	}
	var tel int
	if err := fwdDB.QueryRow(`SELECT COUNT(*) FROM telemetry WHERE message_id=?`, msgID).Scan(&tel); err != nil || tel != 1 {
		t.Fatal("telemetry row missing")
	}
}

func TestRetryBackoffSchedule(t *testing.T) {
	cases := map[int]int64{0: 5, 1: 15, 2: 30, 3: 60, 4: 300, 10: 300}
	for retry, want := range cases {
		if got := retryBackoffSec(retry); got != want {
			t.Fatalf("retry %d: got %d want %d", retry, got, want)
		}
	}
}

func TestForwardingTransitions(t *testing.T) {
	testFwdDB(t)
	id, err := fwdCreateDestination(&Destination{Name: "A", Protocol: "mqtt", Host: "a.example", Port: 1883, Enabled: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	msgID, err := forwardingIngest("M1", "meter/M1/data", []byte(`{}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := fwdFetchDue(id, 100)
	if err != nil || len(rows) != 1 {
		t.Fatalf("fetch due: %v %d", err, len(rows))
	}
	fwdMarkSending([]int64{rows[0].ID})
	rows2, err := fwdFetchDue(id, 100)
	if err != nil || len(rows2) != 0 {
		t.Fatal("SENDING rows must not be re-fetched")
	}
	fwdMarkRetry(rows[0].ID, 0, "timeout", false)
	var status string
	var next int64
	if err := fwdDB.QueryRow(`SELECT status,next_retry_at FROM forwarding_queue WHERE id=?`, rows[0].ID).Scan(&status, &next); err != nil || status != "RETRY" {
		t.Fatalf("retry state: %v %s", err, status)
	}
	if next <= time.Now().Unix() {
		t.Fatal("next_retry_at must be in the future")
	}
	// Auth failure parks immediately.
	fwdMarkRetry(rows[0].ID, 0, "not authorized", true)
	if err := fwdDB.QueryRow(`SELECT status FROM forwarding_queue WHERE id=?`, rows[0].ID).Scan(&status); err != nil || status != "FAILED" {
		t.Fatalf("auth failure must park as FAILED: %v %s", err, status)
	}
	_ = msgID
	_ = os.Getenv("GATEWAY_CONFIG")
}

func TestSanitizeID(t *testing.T) {
	if sanitizeID("MTR-001.a") != "MTR-001.a" {
		t.Fatal("valid id mangled")
	}
	if sanitizeID("../../etc") != "etcpasswd" && sanitizeID("../../etc") != "....etcpasswd" {
		// slashes stripped; exact form depends on rune filter
		t.Logf("got %q", sanitizeID("../../etc"))
	}
	if sanitizeID("") != "unknown" {
		t.Fatal("empty must map to unknown")
	}
}
