package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateMeterMessageOK(t *testing.T) {
	payload := []byte(`{"meter_id":"MTR000001","timestamp":"2026-09-25T10:30:00Z","voltage":230.4,"current":5.2,"power":1198.1,"energy":12543.21}`)
	r, id, err := validateMeterMessage("meter/MTR000001/data", payload, 65536)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "MTR000001" || r.MeterID != "MTR000001" {
		t.Fatalf("wrong meter id: %q", id)
	}
}

func TestValidateMeterMessageMismatch(t *testing.T) {
	payload := []byte(`{"meter_id":"OTHER","timestamp":"2026-09-25T10:30:00Z"}`)
	if _, _, err := validateMeterMessage("meter/MTR000001/data", payload, 65536); err == nil {
		t.Fatal("expected meter_id mismatch error")
	}
}

func TestValidateMeterMessageBadTopic(t *testing.T) {
	for _, topic := range []string{"meter/data", "gateway/X/data", "meter/MTR1/unknown", "meter//data", "meter/MTR 1/data"} {
		if _, _, err := validateMeterMessage(topic, []byte(`{"a":1}`), 65536); err == nil {
			t.Fatalf("expected error for topic %q", topic)
		}
	}
}

func TestValidateMeterMessageBadJSON(t *testing.T) {
	if _, _, err := validateMeterMessage("meter/M1/data", []byte(`{broken`), 65536); err == nil {
		t.Fatal("expected JSON error")
	}
}

func TestValidateMeterMessageOversize(t *testing.T) {
	big := make([]byte, 70000)
	for i := range big {
		big[i] = 'x'
	}
	if _, _, err := validateMeterMessage("meter/M1/data", big, 65536); err == nil {
		t.Fatal("expected oversize error")
	}
}

func TestValidateMeterNumericsRange(t *testing.T) {
	payload := []byte(`{"meter_id":"M1","timestamp":"2026-09-25T10:30:00Z","voltage":99999}`)
	if _, _, err := validateMeterMessage("meter/M1/data", payload, 65536); err == nil {
		t.Fatal("expected voltage range error")
	}
	payload = []byte(`{"meter_id":"M1","timestamp":"2026-09-25T10:30:00Z","voltage":"high"}`)
	if _, _, err := validateMeterMessage("meter/M1/data", payload, 65536); err == nil {
		t.Fatal("expected non-numeric error")
	}
}

func TestValidTimestamp(t *testing.T) {
	for _, ts := range []string{"2026-09-25T10:30:00Z", "1727000000", " 1727000000 "} {
		if !validTimestamp(ts) {
			t.Fatalf("expected valid timestamp %q", ts)
		}
	}
	for _, ts := range []string{"yesterday", "123", ""} {
		if validTimestamp(ts) {
			t.Fatalf("expected invalid timestamp %q", ts)
		}
	}
}

func TestNormalizeBrokerMode(t *testing.T) {
	for _, m := range []string{"unsecured", "secure", "both", " Secure "} {
		if _, err := normalizeBrokerMode(m); err != nil {
			t.Fatalf("mode %q should be valid: %v", m, err)
		}
	}
	for _, m := range []string{"", "disabled", "tls-only", "1883"} {
		if _, err := normalizeBrokerMode(m); err == nil {
			t.Fatalf("mode %q should be invalid", m)
		}
	}
}

func TestValidBrokerUsername(t *testing.T) {
	for _, u := range []string{"meter001", "MTR-1", "gw.local", "a_b-c.d"} {
		if !validBrokerUsername(u) {
			t.Fatalf("username %q should be valid", u)
		}
	}
	for _, u := range []string{"meter 1", "m/t", "m@t", "münchen"} {
		if validBrokerUsername(u) {
			t.Fatalf("username %q should be invalid", u)
		}
	}
}

func TestGenerateBrokerConfModes(t *testing.T) {
	cfg.LocalBroker.Bind = "0.0.0.0"
	cfg.LocalBroker.PortPlain = 1883
	cfg.LocalBroker.PortTLS = 8883
	cfg.LocalBroker.MaxConnections = 100
	cfg.LocalBroker.MaxPayloadBytes = 65536
	cfg.LocalBroker.CertDir = "/tmp/x-certs"
	cfg.LocalBroker.DataDir = "/tmp/x-data"
	cfg.LocalClient.Username = "gateway-local"

	secure := generateBrokerConf("secure")
	if !strings.Contains(secure, "listener 8883") || !strings.Contains(secure, "tls_version tlsv1.2") {
		t.Fatal("secure conf must have TLS listener")
	}
	if strings.Contains(secure, "listener 1883") {
		t.Fatal("secure conf must not open 1883")
	}
	if !strings.Contains(secure, "require_certificate false") {
		t.Fatal("secure conf must not require client certs (v1)")
	}
	if !strings.Contains(secure, "allow_anonymous false") {
		t.Fatal("secure listener must forbid anonymous")
	}
	plain := generateBrokerConf("unsecured")
	if !strings.Contains(plain, "listener 1883") {
		t.Fatal("unsecured conf must have 1883 listener")
	}
	if strings.Contains(plain, "listener 8883") {
		t.Fatal("unsecured conf must not open 8883")
	}
	both := generateBrokerConf("both")
	if !strings.Contains(both, "listener 1883") || !strings.Contains(both, "listener 8883") {
		t.Fatal("both mode must open both listeners")
	}
	if !strings.Contains(secure, "passwd") || !strings.Contains(secure, "acl_file") {
		t.Fatal("conf must reference password and ACL files")
	}
}

func TestMeterReadingJSONShape(t *testing.T) {
	payload := []byte(`{"timestamp":"2026-09-25T10:30:00Z","voltage":230.4}`)
	r, _, err := validateMeterMessage("meter/M9/data", payload, 65536)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(r)
	var back map[string]interface{}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back["meter_id"] != "M9" {
		t.Fatal("meter_id must be stamped from topic")
	}
}
