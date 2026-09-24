package main

// ---------- Customer forwarding: SQLite store ----------
//
// Meter readings flow: local meter client -> devices/telemetry tables ->
// one forwarding_queue row per enabled destination -> forwarding engine.
// Single SQLite transactions (BEGIN/COMMIT) so committed data survives
// power loss; pending rows are never auto-deleted by age.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ForwardingConfig lives in config.yml under `forwarding:`.
type ForwardingConfig struct {
	Enabled               bool `yaml:"enabled"`
	BatchSize             int  `yaml:"batch_size"`
	PollIntervalSec       int  `yaml:"poll_interval_sec"`
	TelemetryRetentionDay int  `yaml:"telemetry_retention_days"`
	SentRetentionDays     int  `yaml:"sent_retention_days"`
	MaxDBMB               int  `yaml:"max_db_mb"`
}

func applyForwardingDefaults() {
	f := &cfg.Forwarding
	// Engine runs by default once destinations exist; harmless otherwise.
	// Config files pre-dating the forwarding section get enabled=true;
	// an explicit `enabled: false` in the file is respected.
	if !forwardingSectionInFile() {
		f.Enabled = true
	}
	if f.BatchSize <= 0 {
		f.BatchSize = 100
	}
	if f.PollIntervalSec <= 0 {
		f.PollIntervalSec = 5
	}
	if f.TelemetryRetentionDay <= 0 {
		f.TelemetryRetentionDay = 30
	}
	if f.SentRetentionDays <= 0 {
		f.SentRetentionDays = 7
	}
	if f.MaxDBMB <= 0 {
		f.MaxDBMB = 512
	}
}

// Destination describes one customer remote (MQTT or TCP).
type Destination struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Protocol  string `json:"protocol"` // mqtt | tcp
	Host      string `json:"host"`
	Port      int    `json:"port"`
	TLS       bool   `json:"tls_enabled"`
	Username  string `json:"username,omitempty"`
	Password  string `json:"-"` // never serialized
	Topic     string `json:"topic,omitempty"`
	QoS       byte   `json:"qos,omitempty"`
	CACert    string `json:"-"`
	Framing   string `json:"framing,omitempty"` // json_lines | length_prefix | raw (tcp)
	AckMode   string `json:"ack_mode,omitempty"` // ack | flush (tcp)
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type QueueStats struct {
	Pending int64 `json:"pending"`
	Sending int64 `json:"sending"`
	Sent    int64 `json:"sent_24h"`
	Failed  int64 `json:"failed"`
	Retry   int64 `json:"retry"`
}

type ForwarderStats struct {
	Destinations []DestinationStatus `json:"destinations"`
	Queue        QueueStats          `json:"queue"`
	LastRun      string              `json:"last_run,omitempty"`
	LastError    string              `json:"last_error,omitempty"`
}

type DestinationStatus struct {
	Destination
	Connected   bool   `json:"connected"`
	Pending     int64  `json:"pending"`
	Sent        int64  `json:"sent_total"`
	Failed      int64  `json:"failed_total"`
	LastSend    string `json:"last_send,omitempty"`
	LastFailure string `json:"last_failure,omitempty"`
}

var (
	fwdDB   *sql.DB
	fwdDBMu sync.Mutex
)

func forwardingSectionInFile() bool {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "forwarding:") {
			return true
		}
	}
	return false
}

func forwardingDBPath() string {
	return filepath.Join(filepath.Dir(configPath()), "forwarding", "forwarding.db")
}

func openForwardingDB() error {
	fwdDBMu.Lock()
	defer fwdDBMu.Unlock()
	if fwdDB != nil {
		return nil
	}
	path := forwardingDBPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("forwarding dir: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?cache=shared")
	if err != nil {
		return fmt.Errorf("forwarding open: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return fmt.Errorf("forwarding pragma: %w", err)
		}
	}
	// Best-effort migration for DBs created before ca_cert existed.
	if _, err := db.Exec(`ALTER TABLE forwarding_destinations ADD COLUMN ca_cert TEXT`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			logger.WithError(err).Warn("forwarding: ca_cert migration skipped")
		}
	}
	schema := []string{
		`CREATE TABLE IF NOT EXISTS devices (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			device_id TEXT UNIQUE NOT NULL,
			device_type TEXT DEFAULT 'meter',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS device_config (
			device_id TEXT NOT NULL,
			key TEXT NOT NULL,
			value TEXT,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (device_id, key)
		)`,
		`CREATE TABLE IF NOT EXISTS telemetry (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id TEXT UNIQUE NOT NULL,
			device_id TEXT NOT NULL,
			topic TEXT,
			payload BLOB NOT NULL,
			timestamp INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS forwarding_destinations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			protocol TEXT NOT NULL,
			host TEXT NOT NULL,
			port INTEGER NOT NULL,
			tls_enabled INTEGER DEFAULT 0,
			username TEXT,
			password_enc TEXT,
			topic TEXT,
			qos INTEGER DEFAULT 1,
			framing TEXT DEFAULT 'json_lines',
			ack_mode TEXT DEFAULT 'ack',
			enabled INTEGER DEFAULT 1,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS forwarding_queue (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id TEXT NOT NULL,
			destination_id INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'PENDING',
			retry_count INTEGER NOT NULL DEFAULT 0,
			next_retry_at INTEGER,
			last_error TEXT,
			created_at INTEGER NOT NULL,
			sent_at INTEGER,
			FOREIGN KEY(destination_id) REFERENCES forwarding_destinations(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS transmission_attempts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			queue_id INTEGER NOT NULL,
			at INTEGER NOT NULL,
			result TEXT NOT NULL,
			latency_ms INTEGER,
			error TEXT,
			FOREIGN KEY(queue_id) REFERENCES forwarding_queue(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			device_id TEXT,
			kind TEXT NOT NULL,
			detail TEXT,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_queue_status ON forwarding_queue(status, next_retry_at)`,
		`CREATE INDEX IF NOT EXISTS idx_queue_dest ON forwarding_queue(destination_id, status)`,
		`CREATE INDEX IF NOT EXISTS idx_telemetry_device_time ON telemetry(device_id, timestamp)`,
		`CREATE INDEX IF NOT EXISTS idx_telemetry_msg ON telemetry(message_id)`,
		`CREATE INDEX IF NOT EXISTS idx_events_time ON events(created_at)`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return fmt.Errorf("forwarding schema: %w", err)
		}
	}
	fwdDB = db
	logger.WithField("path", path).Info("forwarding store open (SQLite WAL)")
	return nil
}

func fwdDecryptPassword(enc string) string {
	if enc == "" || secrets == nil {
		return enc
	}
	if dec, err := secrets.decrypt(enc); err == nil {
		return dec
	}
	return enc
}

func fwdEncryptPassword(plain string) string {
	if plain == "" || secrets == nil {
		return plain
	}
	if enc, err := secrets.encrypt(plain); err == nil {
		return enc
	}
	return plain
}

// ---------- destinations CRUD ----------

func fwdListDestinations() ([]Destination, error) {
	rows, err := fwdDB.Query(`SELECT id,name,protocol,host,port,tls_enabled,username,topic,qos,framing,ack_mode,enabled,created_at,updated_at FROM forwarding_destinations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Destination
	for rows.Next() {
		var d Destination
		var tls, enabled int
		var username, topic, framing, ackMode sql.NullString
		if err := rows.Scan(&d.ID, &d.Name, &d.Protocol, &d.Host, &d.Port, &tls, &username, &topic, &d.QoS, &framing, &ackMode, &enabled, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		d.TLS = tls == 1
		d.Enabled = enabled == 1
		d.Username = username.String
		d.Topic = topic.String
		d.Framing = framing.String
		d.AckMode = ackMode.String
		out = append(out, d)
	}
	return out, rows.Err()
}

func fwdValidateDestination(d *Destination) error {
	d.Protocol = strings.ToLower(strings.TrimSpace(d.Protocol))
	if d.Protocol != "mqtt" && d.Protocol != "tcp" {
		return fmt.Errorf("protocol must be mqtt or tcp")
	}
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(d.Host) == "" {
		return fmt.Errorf("host is required")
	}
	if d.Port < 1 || d.Port > 65535 {
		return fmt.Errorf("port must be 1..65535")
	}
	if d.Protocol == "mqtt" {
		if d.QoS > 2 {
			return fmt.Errorf("qos must be 0..2")
		}
		if d.Topic == "" {
			d.Topic = "customer/meter/data"
		}
	} else {
		if d.Framing == "" {
			d.Framing = "json_lines"
		}
		if d.Framing != "json_lines" && d.Framing != "length_prefix" && d.Framing != "raw" {
			return fmt.Errorf("framing must be json_lines|length_prefix|raw")
		}
		if d.AckMode == "" {
			d.AckMode = "ack"
			if d.Framing == "raw" {
				d.AckMode = "flush"
			}
		}
	}
	return nil
}

func fwdCreateDestination(d *Destination, password string) (int64, error) {
	if err := fwdValidateDestination(d); err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	tls := 0
	if d.TLS {
		tls = 1
	}
	enabled := 1
	if !d.Enabled {
		enabled = 0
	}
	res, err := fwdDB.Exec(`INSERT INTO forwarding_destinations
		(name,protocol,host,port,tls_enabled,username,password_enc,topic,qos,framing,ack_mode,enabled,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.Name, d.Protocol, d.Host, d.Port, tls, d.Username, fwdEncryptPassword(password),
		d.Topic, d.QoS, d.Framing, d.AckMode, enabled, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func fwdUpdateDestination(id int64, d *Destination, password *string) error {
	if err := fwdValidateDestination(d); err != nil {
		return err
	}
	tls := 0
	if d.TLS {
		tls = 1
	}
	enabled := 1
	if !d.Enabled {
		enabled = 0
	}
	if password != nil {
		_, err := fwdDB.Exec(`UPDATE forwarding_destinations SET name=?,protocol=?,host=?,port=?,tls_enabled=?,username=?,password_enc=?,topic=?,qos=?,framing=?,ack_mode=?,enabled=?,updated_at=? WHERE id=?`,
			d.Name, d.Protocol, d.Host, d.Port, tls, d.Username, fwdEncryptPassword(*password), d.Topic, d.QoS, d.Framing, d.AckMode, enabled, time.Now().Unix(), id)
		return err
	}
	_, err := fwdDB.Exec(`UPDATE forwarding_destinations SET name=?,protocol=?,host=?,port=?,tls_enabled=?,username=?,topic=?,qos=?,framing=?,ack_mode=?,enabled=?,updated_at=? WHERE id=?`,
		d.Name, d.Protocol, d.Host, d.Port, tls, d.Username, d.Topic, d.QoS, d.Framing, d.AckMode, enabled, time.Now().Unix(), id)
	return err
}

func fwdDeleteDestination(id int64) error {
	if _, err := fwdDB.Exec(`DELETE FROM forwarding_queue WHERE destination_id=?`, id); err != nil {
		return err
	}
	_, err := fwdDB.Exec(`DELETE FROM forwarding_destinations WHERE id=?`, id)
	return err
}

func fwdGetDestinationPassword(id int64) string {
	var enc sql.NullString
	if err := fwdDB.QueryRow(`SELECT password_enc FROM forwarding_destinations WHERE id=?`, id).Scan(&enc); err != nil {
		return ""
	}
	return fwdDecryptPassword(enc.String)
}

// ---------- ingest: devices + telemetry + per-destination queue rows, one txn ----------

func forwardingIngest(meterID, topic string, payload []byte, ts time.Time) (string, error) {
	if fwdDB == nil {
		return "", fmt.Errorf("forwarding store not open")
	}
	msgID := fmt.Sprintf("GW%s-%s-%d", sanitizeID(getDeviceID()), sanitizeID(meterID), time.Now().UnixNano())
	now := time.Now().Unix()
	tx, err := fwdDB.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO devices(device_id,device_type,enabled,created_at,updated_at)
		VALUES (?, 'meter', 1, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET updated_at=excluded.updated_at`,
		meterID, now, now); err != nil {
		return "", fmt.Errorf("devices upsert: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO telemetry(message_id,device_id,topic,payload,timestamp,created_at)
		VALUES (?,?,?,?,?,?)`, msgID, meterID, topic, payload, ts.Unix(), now); err != nil {
		return "", fmt.Errorf("telemetry insert: %w", err)
	}
	rows, err := tx.Query(`SELECT id FROM forwarding_destinations WHERE enabled=1`)
	if err != nil {
		return "", err
	}
	var destIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", err
		}
		destIDs = append(destIDs, id)
	}
	rows.Close()
	for _, destID := range destIDs {
		if _, err := tx.Exec(`INSERT INTO forwarding_queue(message_id,destination_id,status,retry_count,next_retry_at,created_at)
			VALUES (?,?,'PENDING',0,?,?)`, msgID, destID, now, now); err != nil {
			return "", fmt.Errorf("queue insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	fwdRecordEvent(meterID, "meter.ingested", msgID)
	return msgID, nil
}

func sanitizeID(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			sb.WriteRune(r)
		}
	}
	out := sb.String()
	if out == "" {
		return "unknown"
	}
	return out
}

func fwdRecordEvent(deviceID, kind, detail string) {
	if fwdDB == nil {
		return
	}
	_, _ = fwdDB.Exec(`INSERT INTO events(device_id,kind,detail,created_at) VALUES (?,?,?,?)`,
		deviceID, kind, detail, time.Now().Unix())
}

// ---------- queue reads / stats ----------

type queueRow struct {
	ID        int64
	MessageID string
	DestID    int64
	Retry     int
	Payload   []byte
	Topic     string
}

func fwdFetchDue(destID int64, limit int) ([]queueRow, error) {
	now := time.Now().Unix()
	rows, err := fwdDB.Query(`SELECT q.id,q.message_id,q.destination_id,q.retry_count,t.payload,t.topic
		FROM forwarding_queue q JOIN telemetry t ON t.message_id=q.message_id
		WHERE q.destination_id=? AND q.status IN ('PENDING','RETRY') AND (q.next_retry_at IS NULL OR q.next_retry_at<=?)
		ORDER BY q.id LIMIT ?`, destID, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []queueRow
	for rows.Next() {
		var r queueRow
		if err := rows.Scan(&r.ID, &r.MessageID, &r.DestID, &r.Retry, &r.Payload, &r.Topic); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func fwdMarkSending(ids []int64) {
	if len(ids) == 0 {
		return
	}
	q := `UPDATE forwarding_queue SET status='SENDING' WHERE id IN (` + placeholders(len(ids)) + `)`
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	_, _ = fwdDB.Exec(q, args...)
}

func fwdMarkSent(queueID int64) {
	now := time.Now().Unix()
	_, _ = fwdDB.Exec(`UPDATE forwarding_queue SET status='SENT',sent_at=?,last_error=NULL WHERE id=?`, now, queueID)
	_, _ = fwdDB.Exec(`INSERT INTO transmission_attempts(queue_id,at,result) VALUES (?,?,'sent')`, queueID, now)
}

func fwdMarkRetry(queueID int64, retry int, errMsg string, authFailure bool) {
	if authFailure {
		// Configuration error: park as FAILED, never hot-retry.
		_, _ = fwdDB.Exec(`UPDATE forwarding_queue SET status='FAILED',last_error=? WHERE id=?`, "auth failed: "+errMsg, queueID)
		_, _ = fwdDB.Exec(`INSERT INTO transmission_attempts(queue_id,at,result,error) VALUES (?,?,'auth-failed',?)`, queueID, time.Now().Unix(), errMsg)
		return
	}
	backoff := retryBackoffSec(retry)
	next := time.Now().Unix() + backoff
	_, _ = fwdDB.Exec(`UPDATE forwarding_queue SET status='RETRY',retry_count=?,next_retry_at=?,last_error=? WHERE id=?`, retry+1, next, errMsg, queueID)
	_, _ = fwdDB.Exec(`INSERT INTO transmission_attempts(queue_id,at,result,error) VALUES (?,?,'retry',?)`, queueID, time.Now().Unix(), errMsg)
}

func fwdMarkFailed(queueID int64, errMsg string) {
	_, _ = fwdDB.Exec(`UPDATE forwarding_queue SET status='FAILED',last_error=? WHERE id=?`, errMsg, queueID)
	_, _ = fwdDB.Exec(`INSERT INTO transmission_attempts(queue_id,at,result,error) VALUES (?,?,'failed',?)`, queueID, time.Now().Unix(), errMsg)
}

// Retry 1:5s 2:15s 3:30s 4:60s 5+:5min.
func retryBackoffSec(retry int) int64 {
	switch {
	case retry <= 0:
		return 5
	case retry == 1:
		return 15
	case retry == 2:
		return 30
	case retry == 3:
		return 60
	default:
		return 300
	}
}

func placeholders(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ",")
}

func fwdQueueStats() QueueStats {
	var s QueueStats
	day := time.Now().Unix() - 24*3600
	_ = fwdDB.QueryRow(`SELECT COUNT(*) FROM forwarding_queue WHERE status='PENDING'`).Scan(&s.Pending)
	_ = fwdDB.QueryRow(`SELECT COUNT(*) FROM forwarding_queue WHERE status='SENDING'`).Scan(&s.Sending)
	_ = fwdDB.QueryRow(`SELECT COUNT(*) FROM forwarding_queue WHERE status='SENT' AND sent_at>=?`, day).Scan(&s.Sent)
	_ = fwdDB.QueryRow(`SELECT COUNT(*) FROM forwarding_queue WHERE status='FAILED'`).Scan(&s.Failed)
	_ = fwdDB.QueryRow(`SELECT COUNT(*) FROM forwarding_queue WHERE status='RETRY'`).Scan(&s.Retry)
	return s
}

// ---------- retention + disk guard ----------

func fwdRetention() {
	if fwdDB == nil {
		return
	}
	now := time.Now().Unix()
	if days := cfg.Forwarding.TelemetryRetentionDay; days > 0 {
		cut := now - int64(days)*86400
		if res, err := fwdDB.Exec(`DELETE FROM telemetry WHERE created_at<? AND message_id NOT IN (SELECT message_id FROM forwarding_queue WHERE status IN ('PENDING','RETRY','SENDING'))`, cut); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				logger.WithField("rows", n).Info("forwarding: telemetry retention cleanup")
			}
		}
		if _, err := fwdDB.Exec(`DELETE FROM events WHERE created_at<?`, cut); err == nil {
		}
	}
	if days := cfg.Forwarding.SentRetentionDays; days > 0 {
		cut := now - int64(days)*86400
		if res, err := fwdDB.Exec(`DELETE FROM forwarding_queue WHERE status='SENT' AND sent_at<?`, cut); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				logger.WithField("rows", n).Info("forwarding: sent-queue cleanup")
			}
		}
	}
	fwdDiskGuard()
}

// Never let the DB eat the SD card: 70% warn, 80% critical log, 90% evict
// oldest already-forwarded telemetry first (pending is never auto-deleted).
func fwdDiskGuard() {
	maxBytes := int64(cfg.Forwarding.MaxDBMB) * 1024 * 1024
	if maxBytes <= 0 {
		return
	}
	st, err := os.Stat(forwardingDBPath())
	if err != nil {
		return
	}
	size := st.Size()
	for _, suffix := range []string{"-wal", "-journal", "-shm"} {
		if s2, err := os.Stat(forwardingDBPath() + suffix); err == nil {
			size += s2.Size()
		}
	}
	pct := float64(size) / float64(maxBytes) * 100
	switch {
	case pct >= 90:
		logger.WithField("pct", int(pct)).Error("forwarding: DB over 90% of cap — evicting oldest forwarded telemetry")
		_, _ = fwdDB.Exec(`DELETE FROM telemetry WHERE message_id NOT IN (SELECT message_id FROM forwarding_queue WHERE status IN ('PENDING','RETRY','SENDING')) AND created_at < (SELECT COALESCE(MIN(created_at),0) FROM telemetry) + 86400`)
	case pct >= 80:
		logger.WithField("pct", int(pct)).Error("forwarding: DB over 80% of cap")
	case pct >= 70:
		logger.WithField("pct", int(pct)).Warn("forwarding: DB over 70% of cap")
	}
}

func fwdQueueList(status string, limit int) []map[string]interface{} {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	where := ""
	if status != "" && status != "ALL" {
		where = "WHERE q.status='" + strings.ToUpper(status) + "'"
	}
	rows, err := fwdDB.Query(fmt.Sprintf(`SELECT q.id,q.message_id,q.destination_id,q.status,q.retry_count,q.next_retry_at,q.last_error,q.created_at,q.sent_at,d.name
		FROM forwarding_queue q LEFT JOIN forwarding_destinations d ON d.id=q.destination_id %s ORDER BY q.id DESC LIMIT %d`, where, limit))
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var id, destID, retry int64
		var msgID, st, name sql.NullString
		var next, created, sent sql.NullInt64
		var lastErr sql.NullString
		if err := rows.Scan(&id, &msgID, &destID, &st, &retry, &next, &lastErr, &created, &sent, &name); err != nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"id": id, "message_id": msgID.String, "destination": name.String,
			"status": st.String, "retry_count": retry,
			"next_retry_at": next.Int64, "last_error": lastErr.String,
			"created_at": created.Int64, "sent_at": sent.Int64,
		})
	}
	return out
}
