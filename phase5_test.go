package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// ---------- Phase 5 gateway tests (first Go tests in this repo) ----------

func testSpool(t *testing.T) *spoolQueue {
	t.Helper()
	dir := t.TempDir()
	s, err := openSpool(filepath.Join(dir, "spool.db"), 100, 72, 10*1024*1024)
	if err != nil {
		t.Fatalf("openSpool: %v", err)
	}
	t.Cleanup(func() { s.close() })
	return s
}

func TestSpoolEnqueueFetchAckOrder(t *testing.T) {
	s := testSpool(t)
	if err := s.enqueue("log", []byte(`{"m":1}`), 1, "e-low"); err != nil {
		t.Fatal(err)
	}
	if err := s.enqueue("status", []byte(`{"m":2}`), 10, "e-high"); err != nil {
		t.Fatal(err)
	}
	batch, err := s.fetchBatch(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 2 {
		t.Fatalf("want 2 events, got %d", len(batch))
	}
	// Priority first, even though enqueued second.
	if batch[0].eventID != "e-high" || batch[1].eventID != "e-low" {
		t.Fatalf("priority order violated: %v %v", batch[0].eventID, batch[1].eventID)
	}
	var ids []int64
	for _, e := range batch {
		ids = append(ids, e.id)
	}
	if err := s.ack(ids); err != nil {
		t.Fatal(err)
	}
	if n := s.count(); n != 0 {
		t.Fatalf("spool not drained, depth %d", n)
	}
}

func TestSpoolDedupesEventIDs(t *testing.T) {
	s := testSpool(t)
	for i := 0; i < 3; i++ {
		if err := s.enqueue("telemetry", []byte(`{}`), 5, "same-id"); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.count(); n != 1 {
		t.Fatalf("eventID redelivery must collapse, depth %d", n)
	}
}

func TestSpoolEvictsOldestLowPriority(t *testing.T) {
	dir := t.TempDir()
	s, err := openSpool(filepath.Join(dir, "spool.db"), 3, 72, 10*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	for _, e := range []struct {
		id string
		pr int
	}{{"a", 1}, {"b", 1}, {"c", 1}, {"d", 10}} {
		if err := s.enqueue("log", []byte(e.id), e.pr, e.id); err != nil {
			t.Fatal(err)
		}
	}
	batch, err := s.fetchBatch(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 3 {
		t.Fatalf("row cap not enforced, got %d", len(batch))
	}
	// High-priority survivor + newest lows; oldest low ("a") evicted.
	seen := map[string]bool{}
	for _, e := range batch {
		seen[e.eventID] = true
	}
	if !seen["d"] || seen["a"] {
		t.Fatalf("eviction order wrong: %v", seen)
	}
}

func TestIdentityPinning(t *testing.T) {
	dir := t.TempDir()
	old := os.Getenv("GATEWAY_CONFIG")
	os.Setenv("GATEWAY_CONFIG", filepath.Join(dir, "config.yml"))
	defer os.Setenv("GATEWAY_CONFIG", old)

	first := resolveStableDeviceID("op-choice")
	if first != "op-choice" {
		t.Fatalf("configured ID must win, got %s", first)
	}
	// NIC/config change afterwards must NOT flip identity.
	second := resolveStableDeviceID("something-else")
	if second != first {
		t.Fatalf("pinned ID changed: %s -> %s", first, second)
	}
	if fi, err := os.Stat(filepath.Join(dir, "device.id")); err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("pin file must exist with 0600: %v %v", fi, err)
	}
}

func TestOtaSignatureVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	msg := []byte("fake-firmware-bytes")
	sig := ed25519.Sign(priv, msg)
	if err := verifyArtifactSignature(msg, hex.EncodeToString(sig), hex.EncodeToString(pub)); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	bad := make([]byte, len(sig))
	copy(bad, sig)
	bad[0] ^= 0xff
	if err := verifyArtifactSignature(msg, hex.EncodeToString(bad), hex.EncodeToString(pub)); err == nil {
		t.Fatal("forged signature accepted")
	}
	tampered := append(append([]byte{}, msg...), 0x00)
	if err := verifyArtifactSignature(tampered, hex.EncodeToString(sig), hex.EncodeToString(pub)); err == nil {
		t.Fatal("tampered binary accepted")
	}
}
