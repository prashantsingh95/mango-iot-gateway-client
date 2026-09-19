package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

// ---------- Persistent offline spool (Phase 5 / §20) ----------
//
// SQLite-backed durable queue (WAL mode) replacing the old in-memory
// telemetryBuffer (which was never wired). When MQTT/API is unreachable,
// telemetry, status and log events spool to disk; on reconnect they upload in
// priority+insertion order and acked rows are deleted. Bounded by row count,
// age (TTL) and disk usage — oldest low-priority rows evict first.

type queuedEvent struct {
	id       int64
	eventID  string
	etype    string
	payload  []byte
	priority int
	attempts int
}

type spoolQueue struct {
	mu       sync.Mutex
	db       *sql.DB
	maxRows  int
	ttlHours int
	maxBytes int64
}

func openSpool(path string, maxRows, ttlHours int, maxBytes int64) (*spoolQueue, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("spool dir: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?cache=shared")
	if err != nil {
		return nil, fmt.Errorf("spool open: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("spool pragma: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		event_id TEXT NOT NULL UNIQUE,
		etype TEXT NOT NULL,
		payload BLOB NOT NULL,
		priority INTEGER NOT NULL DEFAULT 0,
		attempts INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	)`);
		err != nil {
		db.Close()
		return nil, fmt.Errorf("spool schema: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_events_order ON events(priority DESC, id ASC)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("spool index: %w", err)
	}
	return &spoolQueue{db: db, maxRows: maxRows, ttlHours: ttlHours, maxBytes: maxBytes}, nil
}

func newEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("evt-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// enqueue stores an event; eventID callers should pass stable IDs so MQTT
// redelivery after a crash collapses instead of duplicating.
func (s *spoolQueue) enqueue(etype string, payload []byte, priority int, eventID string) error {
	if eventID == "" {
		eventID = newEventID()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT OR IGNORE INTO events(event_id, etype, payload, priority, attempts, created_at)
		VALUES(?, ?, ?, ?, 0, ?)`, eventID, etype, payload, priority, time.Now().Unix())
	if err != nil {
		return err
	}
	return s.enforceLimits()
}

func (s *spoolQueue) fetchBatch(limit int) ([]queuedEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT id, event_id, etype, payload, priority, attempts FROM events
		ORDER BY priority DESC, id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []queuedEvent
	for rows.Next() {
		var e queuedEvent
		if err := rows.Scan(&e.id, &e.eventID, &e.etype, &e.payload, &e.priority, &e.attempts); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *spoolQueue) ack(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM events WHERE id = ?`, id); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *spoolQueue) bumpAttempts(ids []int64) {
	if len(ids) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		_, _ = s.db.Exec(`UPDATE events SET attempts = attempts + 1 WHERE id = ?`, id)
	}
}

func (s *spoolQueue) count() int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
	return n
}

// enforceLimits evicts expired rows, then oldest low-priority rows past the
// row cap, then oldest rows past the byte cap. Critical telemetry (priority)
// always outlives debug logs.
func (s *spoolQueue) enforceLimits() error {
	if s.ttlHours > 0 {
		cutoff := time.Now().Add(-time.Duration(s.ttlHours) * time.Hour).Unix()
		if _, err := s.db.Exec(`DELETE FROM events WHERE created_at < ?`, cutoff); err != nil {
			return err
		}
	}
	if s.maxRows > 0 {
		if _, err := s.db.Exec(`DELETE FROM events WHERE id NOT IN
			(SELECT id FROM events ORDER BY priority DESC, id DESC LIMIT ?)`, s.maxRows); err != nil {
			return err
		}
	}
	if s.maxBytes > 0 {
		var total sql.NullInt64
		_ = s.db.QueryRow(`SELECT SUM(LENGTH(payload)) FROM events`).Scan(&total)
		for total.Valid && total.Int64 > s.maxBytes {
			if _, err := s.db.Exec(`DELETE FROM events WHERE id =
				(SELECT id FROM events ORDER BY priority ASC, id ASC LIMIT 1)`); err != nil {
				return err
			}
			_ = s.db.QueryRow(`SELECT SUM(LENGTH(payload)) FROM events`).Scan(&total)
		}
	}
	return nil
}

func (s *spoolQueue) close() error {
	return s.db.Close()
}

func spoolLogFields(s *spoolQueue) logrus.Fields {
	if s == nil {
		return logrus.Fields{"spool": "disabled"}
	}
	return logrus.Fields{"spool_depth": s.count()}
}
