package main

// ---------- Customer forwarding engine ----------
//
// Reads PENDING/RETRY rows in batches, delivers to customer MQTT or TCP
// destinations, marks SENT only on acknowledgement. One failed destination
// never blocks another. Independent from the gateway HiveMQ client.

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
)

var (
	fwdMu       sync.Mutex
	fwdRunning  bool
	fwdStopCh   chan struct{}
	fwdLastRun  string
	fwdLastErr  string
	mqttFwdMu   sync.Mutex
	mqttFwdCli  = map[int64]MQTT.Client{}
	destConnMu  sync.Mutex
	destConnOK  = map[int64]bool{}
	destLastOK  = map[int64]string{}
	destLastErr = map[int64]string{}
	destSent    = map[int64]int64{}
	destFailed  = map[int64]int64{}
)

func startForwardingEngine(ctx context.Context) {
	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		// Open store + initial retention shortly after boot.
		time.Sleep(5 * time.Second)
		if err := openForwardingDB(); err != nil {
			logger.WithError(err).Error("forwarding: store unavailable, engine disabled")
			return
		}
		fwdRetention()
		for {
			select {
			case <-ctx.Done():
				stopForwardingEngine()
				return
			case <-tick.C:
				interval := cfg.Forwarding.PollIntervalSec
				if interval <= 0 {
					interval = 5
				}
				tick.Reset(time.Duration(interval) * time.Second)
				forwardingTick()
				// Retention sweep hourly (cheap guard: run when minute==0).
				if time.Now().Minute() == 0 {
					fwdRetention()
				}
			}
		}
	}()
}

func stopForwardingEngine() {
	fwdMu.Lock()
	if fwdStopCh != nil {
		close(fwdStopCh)
		fwdStopCh = nil
	}
	fwdMu.Unlock()
	mqttFwdMu.Lock()
	for id, cli := range mqttFwdCli {
		if cli.IsConnected() {
			cli.Disconnect(500)
		}
		delete(mqttFwdCli, id)
	}
	mqttFwdMu.Unlock()
}

func forwardingTick() {
	if fwdDB == nil || !cfg.Forwarding.Enabled {
		return
	}
	dests, err := fwdListDestinations()
	if err != nil {
		fwdLastErr = err.Error()
		return
	}
	batch := cfg.Forwarding.BatchSize
	if batch <= 0 {
		batch = 100
	}
	for _, d := range dests {
		if !d.Enabled {
			continue
		}
		rows, err := fwdFetchDue(d.ID, batch)
		if err != nil || len(rows) == 0 {
			continue
		}
		ids := make([]int64, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		fwdMarkSending(ids)
		for _, r := range rows {
			deliverRow(d, r)
		}
	}
	fwdLastRun = time.Now().UTC().Format(time.RFC3339)
}

func deliverRow(d Destination, r queueRow) {
	start := time.Now()
	var err error
	authFail := false
	switch d.Protocol {
	case "mqtt":
		err, authFail = forwardMQTT(d, r)
	case "tcp":
		err, authFail = forwardTCP(d, r)
	default:
		err = fmt.Errorf("unknown protocol %q", d.Protocol)
	}
	latency := time.Since(start).Milliseconds()
	if err == nil {
		fwdMarkSent(r.ID)
		destConnMu.Lock()
		destConnOK[d.ID] = true
		destSent[d.ID]++
		destLastOK[d.ID] = time.Now().UTC().Format(time.RFC3339)
		destConnMu.Unlock()
		fwdRecordEvent("", "forwarding.sent", r.MessageID)
	} else {
		if isInvalidPayload(err) {
			fwdMarkFailed(r.ID, err.Error())
		} else {
			fwdMarkRetry(r.ID, r.Retry, err.Error(), authFail)
		}
		destConnMu.Lock()
		destConnOK[d.ID] = false
		destFailed[d.ID]++
		destLastErr[d.ID] = fmt.Sprintf("%s (%dms)", err.Error(), latency)
		destConnMu.Unlock()
		_, _ = fwdDB.Exec(`INSERT INTO transmission_attempts(queue_id,at,result,latency_ms,error) VALUES (?,?,'attempt',?,?)`, r.ID, time.Now().Unix(), latency, err.Error())
	}
}

func isInvalidPayload(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "invalid payload") || strings.Contains(msg, "not numeric") || strings.Contains(msg, "outside")
}

// ---------- MQTT sender (independent connection per destination) ----------

func forwardMQTT(d Destination, r queueRow) (error, bool) {
	cli, err := mqttFwdClient(d)
	if err != nil {
		return err, isAuthError(err)
	}
	topic := renderFwdTopic(d.Topic, r)
	payload := renderFwdPayload(r)
	tok := cli.Publish(topic, d.QoS, false, payload)
	if !tok.WaitTimeout(15 * time.Second) {
		dropMqttFwdClient(d.ID)
		return fmt.Errorf("mqtt publish timeout"), false
	}
	if tok.Error() != nil {
		dropMqttFwdClient(d.ID)
		return tok.Error(), isAuthError(tok.Error())
	}
	return nil, false
}

func mqttFwdClient(d Destination) (MQTT.Client, error) {
	mqttFwdMu.Lock()
	if cli, ok := mqttFwdCli[d.ID]; ok && cli.IsConnected() {
		mqttFwdMu.Unlock()
		return cli, nil
	}
	mqttFwdMu.Unlock()

	password := fwdGetDestinationPassword(d.ID)
	scheme := "tcp"
	if d.TLS {
		scheme = "ssl"
	}
	addr := fmt.Sprintf("%s://%s:%d", scheme, d.Host, d.Port)
	opts := MQTT.NewClientOptions()
	opts.AddBroker(addr)
	opts.SetClientID(fmt.Sprintf("gw-%s-fwd-%d", sanitizeID(getDeviceID()), d.ID))
	if d.Username != "" {
		opts.SetUsername(d.Username)
		opts.SetPassword(password)
	}
	opts.SetCleanSession(true)
	opts.SetKeepAlive(60)
	opts.SetConnectTimeout(10 * time.Second)
	opts.SetAutoReconnect(false)
	if d.TLS {
		opts.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	cli := MQTT.NewClient(opts)
	tok := cli.Connect()
	if !tok.WaitTimeout(12*time.Second) {
		return nil, fmt.Errorf("customer mqtt connect timeout (%s:%d)", d.Host, d.Port)
	}
	if tok.Error() != nil {
		return nil, tok.Error()
	}
	mqttFwdMu.Lock()
	mqttFwdCli[d.ID] = cli
	mqttFwdMu.Unlock()
	return cli, nil
}

func dropMqttFwdClient(id int64) {
	mqttFwdMu.Lock()
	if cli, ok := mqttFwdCli[id]; ok {
		if cli.IsConnected() {
			cli.Disconnect(250)
		}
		delete(mqttFwdCli, id)
	}
	mqttFwdMu.Unlock()
}

func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not authorized") || strings.Contains(msg, "not authorised") ||
		strings.Contains(msg, "bad user") || strings.Contains(msg, "auth") && strings.Contains(msg, "fail")
}

func renderFwdTopic(tmpl string, r queueRow) string {
	if tmpl == "" {
		return "customer/meter/data"
	}
	var obj map[string]interface{}
	meterID := ""
	if err := json.Unmarshal(r.Payload, &obj); err == nil {
		meterID, _ = obj["meter_id"].(string)
	}
	out := strings.ReplaceAll(tmpl, "{meter_id}", meterID)
	out = strings.ReplaceAll(out, "{message_id}", r.MessageID)
	return out
}

func renderFwdPayload(r queueRow) []byte {
	// Preserve the original payload; stamp the gateway message id for
	// customer-side dedup.
	var obj map[string]interface{}
	if err := json.Unmarshal(r.Payload, &obj); err != nil {
		return r.Payload
	}
	obj["_message_id"] = r.MessageID
	raw, err := json.Marshal(obj)
	if err != nil {
		return r.Payload
	}
	return raw
}

// ---------- TCP sender ----------
//
// Framing (per destination):
//   json_lines    JSON + "\n"; ACK = a JSON line containing {"ack":<message_id>}
//   length_prefix 4-byte big-endian length + JSON; ACK = "ACK <message_id>\n"
//   raw           bytes as-is; NO protocol ACK exists -> SENT on kernel flush
//                 (documented limitation: delivery to the socket, not the app).
func forwardTCP(d Destination, r queueRow) (error, bool) {
	if d.Framing == "" {
		d.Framing = "json_lines"
	}
	addr := net.JoinHostPort(d.Host, fmt.Sprint(d.Port))
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if d.TLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("tcp dial: %w", err), false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))

	payload := renderFwdPayload(r)
	switch d.Framing {
	case "length_prefix":
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
		if _, err := conn.Write(append(hdr[:], payload...)); err != nil {
			return fmt.Errorf("tcp write: %w", err), false
		}
		return awaitLineACK(conn, r.MessageID)
	case "raw":
		if _, err := conn.Write(payload); err != nil {
			return fmt.Errorf("tcp write: %w", err), false
		}
		// No application ACK exists for raw framing: SENT means flushed to
		// the socket (documented limitation — prefer json_lines).
		return nil, false
	default: // json_lines
		if _, err := conn.Write(append(payload, '\n')); err != nil {
			return fmt.Errorf("tcp write: %w", err), false
		}
		return awaitJSONACK(conn, r.MessageID)
	}
}

func awaitJSONACK(conn net.Conn, msgID string) (error, bool) {
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("tcp ack read: %w", err), false
	}
	var ack map[string]interface{}
	if err := json.Unmarshal([]byte(line), &ack); err != nil {
		return fmt.Errorf("tcp ack not JSON: %s", strings.TrimSpace(line)), false
	}
	for _, k := range []string{"ack", "message_id", "id"} {
		if v, ok := ack[k].(string); ok && v == msgID {
			return nil, false
		}
	}
	return fmt.Errorf("tcp ack mismatch for %s", msgID), false
}

func awaitLineACK(conn net.Conn, msgID string) (error, bool) {
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("tcp ack read: %w", err), false
	}
	line = strings.TrimSpace(line)
	if line == "ACK "+msgID || line == "OK "+msgID || line == msgID {
		return nil, false
	}
	return fmt.Errorf("tcp ack mismatch for %s (got %q)", msgID, line), false
}

// ---------- test-connection (staged: DNS -> TCP -> TLS -> auth -> publish) ----------

func testDestination(d Destination) map[string]interface{} {
	steps := []map[string]interface{}{}
	fail := func(stage string, err error) map[string]interface{} {
		steps = append(steps, map[string]interface{}{"stage": stage, "ok": false, "error": err.Error()})
		return map[string]interface{}{"ok": false, "steps": steps}
	}
	ips, err := net.LookupHost(d.Host)
	if err != nil || len(ips) == 0 {
		return fail("dns", fmt.Errorf("DNS failed: %v", err))
	}
	steps = append(steps, map[string]interface{}{"stage": "dns", "ok": true, "detail": ips[0]})
	addr := net.JoinHostPort(d.Host, fmt.Sprint(d.Port))
	dialer := &net.Dialer{Timeout: 8 * time.Second}
	if d.Protocol == "tcp" {
		var conn net.Conn
		if d.TLS {
			conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{MinVersion: tls.VersionTLS12})
		} else {
			conn, err = dialer.Dial("tcp", addr)
		}
		if err != nil {
			stage := "tcp"
			if d.TLS {
				stage = "tcp+tls"
			}
			return fail(stage, err)
		}
		conn.Close()
		stage := "tcp"
		if d.TLS {
			stage = "tcp+tls"
		}
		steps = append(steps, map[string]interface{}{"stage": stage, "ok": true})
		steps = append(steps, map[string]interface{}{"stage": "note", "ok": true, "detail": "TCP reachable; send/ACK semantics depend on destination framing (" + d.Framing + ")"})
		return map[string]interface{}{"ok": true, "steps": steps}
	}
	// MQTT: full CONNECT + test publish.
	password := fwdGetDestinationPassword(d.ID)
	scheme := "tcp"
	if d.TLS {
		scheme = "ssl"
	}
	opts := MQTT.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("%s://%s", scheme, addr))
	opts.SetClientID(fmt.Sprintf("gw-%s-test-%d", sanitizeID(getDeviceID()), time.Now().Unix()%10000))
	if d.Username != "" {
		opts.SetUsername(d.Username)
		opts.SetPassword(password)
	}
	opts.SetCleanSession(true)
	opts.SetConnectTimeout(8 * time.Second)
	opts.SetAutoReconnect(false)
	if d.TLS {
		opts.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	steps = append(steps, map[string]interface{}{"stage": "tcp", "ok": true})
	cli := MQTT.NewClient(opts)
	tok := cli.Connect()
	if !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		err := tok.Error()
		if err == nil {
			err = fmt.Errorf("connect timeout")
		}
		return fail("mqtt-auth", err)
	}
	defer cli.Disconnect(500)
	steps = append(steps, map[string]interface{}{"stage": "mqtt-auth", "ok": true})
	ptok := cli.Publish(renderFwdTopic(d.Topic, queueRow{MessageID: "test"}), d.QoS, false, []byte(`{"test":true}`))
	if !ptok.WaitTimeout(8*time.Second) || ptok.Error() != nil {
		err := ptok.Error()
		if err == nil {
			err = fmt.Errorf("publish timeout")
		}
		return fail("publish-test", err)
	}
	steps = append(steps, map[string]interface{}{"stage": "publish-test", "ok": true})
	return map[string]interface{}{"ok": true, "steps": steps}
}

// ---------- status snapshot ----------

func forwardingStatus() map[string]interface{} {
	dests, _ := fwdListDestinations()
	out := make([]DestinationStatus, 0, len(dests))
	destConnMu.Lock()
	defer destConnMu.Unlock()
	for _, d := range dests {
		var pending, sent, failed int64
		_ = fwdDB.QueryRow(`SELECT COUNT(*) FROM forwarding_queue WHERE destination_id=? AND status IN ('PENDING','RETRY')`, d.ID).Scan(&pending)
		_ = fwdDB.QueryRow(`SELECT COUNT(*) FROM forwarding_queue WHERE destination_id=? AND status='SENT'`, d.ID).Scan(&sent)
		_ = fwdDB.QueryRow(`SELECT COUNT(*) FROM forwarding_queue WHERE destination_id=? AND status IN ('FAILED')`, d.ID).Scan(&failed)
		out = append(out, DestinationStatus{
			Destination: d, Connected: destConnOK[d.ID], Pending: pending,
			Sent: destSent[d.ID] + sent, Failed: destFailed[d.ID] + failed,
			LastSend: destLastOK[d.ID], LastFailure: destLastErr[d.ID],
		})
	}
	return map[string]interface{}{
		"enabled": cfg.Forwarding.Enabled, "destinations": out,
		"queue": fwdQueueStats(), "last_run": fwdLastRun, "last_error": fwdLastErr,
	}
}

