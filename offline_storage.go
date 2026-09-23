package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

// StorageState represents the current storage pressure level
type StorageState string

const (
	StateNormal    StorageState = "NORMAL"
	StateHigh      StorageState = "HIGH"
	StateCritical  StorageState = "CRITICAL"
	StateEmergency StorageState = "EMERGENCY"
)

// Priority classes for data
const (
	PriorityP0GatewayCritical   = 100 // critical Gateway Data
	PriorityP1GatewayImportant  = 80  // important Gateway Data
	PriorityP2ExternalImportant = 60  // important External Device Data
	PriorityP3ExternalNormal    = 40  // normal External Device Data
	PriorityP4ExternalBulk      = 20  // bulk/raw External Device Data
)

// StorageManager enforces SD card quotas and thresholds per spec §18-19
type StorageManager struct {
	mu sync.Mutex

	basePath string // /data/offline
	// Quotas (configurable, not hard-coded)
	offlineQuotaBytes          int64
	warningThresholdPercent    int
	criticalThresholdPercent   int
	emergencyThresholdPercent  int
	minimumFilesystemFreeBytes int64
	chunkMaxBytes              int64

	// Runtime state
	state StorageState
}

func NewStorageManager(basePath string, offlineQuotaBytes int64, chunkMaxBytes int64) *StorageManager {
	if basePath == "" {
		basePath = "/data/offline"
	}
	if offlineQuotaBytes <= 0 {
		offlineQuotaBytes = 2 * 1024 * 1024 * 1024 // 2GB default, well below 64GB
	}
	if chunkMaxBytes <= 0 {
		chunkMaxBytes = 256 * 1024 * 1024 // 256MB per chunk
	}
	return &StorageManager{
		basePath:                   basePath,
		offlineQuotaBytes:          offlineQuotaBytes,
		warningThresholdPercent:    70,
		criticalThresholdPercent:   85,
		emergencyThresholdPercent:  95,
		minimumFilesystemFreeBytes: 2 * 1024 * 1024 * 1024, // 2GB safety reserve
		chunkMaxBytes:              chunkMaxBytes,
		state:                      StateNormal,
	}
}

func (sm *StorageManager) GetState() StorageState {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.state
}

func (sm *StorageManager) UpdateState(usedBytes int64) StorageState {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	percent := 0
	if sm.offlineQuotaBytes > 0 {
		percent = int((usedBytes * 100) / sm.offlineQuotaBytes)
	}
	switch {
	case percent >= sm.emergencyThresholdPercent:
		sm.state = StateEmergency
	case percent >= sm.criticalThresholdPercent:
		sm.state = StateCritical
	case percent >= sm.warningThresholdPercent:
		sm.state = StateHigh
	default:
		sm.state = StateNormal
	}
	return sm.state
}

func (sm *StorageManager) ShouldDrop(priority int) bool {
	state := sm.GetState()
	switch state {
	case StateEmergency:
		// Only P0 survives
		return priority < PriorityP0GatewayCritical
	case StateCritical:
		// P0, P1 survive; P2 preferred; P3/P4 dropped
		return priority <= PriorityP3ExternalNormal
	case StateHigh:
		// Log warning, prefer old data sync
		return false
	default:
		return false
	}
}

func (sm *StorageManager) GetInfo() map[string]interface{} {
	// Filesystem stats - simplified for portability
	var total, free int64
	// Try to get filesystem stats via os.Stat on basePath
	if fi, err := os.Stat(sm.basePath); err == nil && fi.IsDir() {
		// For MVP, report quota-based stats
		total = 64 * 1024 * 1024 * 1024
		free = total - sm.offlineQuotaBytes
	}
	return map[string]interface{}{
		"storageTotal":            total,
		"storageFree":             free,
		"offlineQuota":            sm.offlineQuotaBytes,
		"offlineUsed":             0, // filled by caller
		"storageState":            string(sm.GetState()),
		"chunkMaxBytes":           sm.chunkMaxBytes,
		"gatewayQueuePath":        filepath.Join(sm.basePath, "gateway"),
		"externalDeviceQueuePath": filepath.Join(sm.basePath, "external-devices"),
	}
}

// ChunkedQueue implements bounded chunk storage per spec §21-22
// Each queue stores data in chunk files: chunk-000001.db, chunk-000002.db, etc.
// Active chunk is `active.db`, sealed when full, then new active.
// Sync deletes oldest complete chunk after ACK.

type ChunkedQueue struct {
	mu            sync.Mutex
	baseDir       string
	prefix        string // "gateway" or "external-devices"
	activePath    string
	maxChunkBytes int64
	maxQueueBytes int64
	storageMgr    *StorageManager
	priority      int // default priority for this queue

	// Active DB
	db *sql.DB
	// Chunk tracking
	chunkIndex int
}

func NewChunkedQueue(baseDir, prefix string, maxChunkBytes, maxQueueBytes int64, storageMgr *StorageManager, defaultPriority int) (*ChunkedQueue, error) {
	dir := filepath.Join(baseDir, prefix)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("queue dir: %w", err)
	}
	cq := &ChunkedQueue{
		baseDir:       dir,
		prefix:        prefix,
		activePath:    filepath.Join(dir, "active.db"),
		maxChunkBytes: maxChunkBytes,
		maxQueueBytes: maxQueueBytes,
		storageMgr:    storageMgr,
		priority:      defaultPriority,
	}
	if err := cq.openActive(); err != nil {
		return nil, err
	}
	// Recover chunk index from existing files
	files, _ := filepath.Glob(filepath.Join(dir, "chunk-*.db"))
	cq.chunkIndex = len(files) + 1
	return cq, nil
}

func (cq *ChunkedQueue) openActive() error {
	db, err := sql.Open("sqlite", cq.activePath+"?cache=shared")
	if err != nil {
		return fmt.Errorf("queue open: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return fmt.Errorf("queue pragma: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		record_id TEXT NOT NULL UNIQUE,
		gateway_id TEXT NOT NULL,
		external_device_id TEXT,
		integration_id TEXT,
		sequence INTEGER,
		timestamp INTEGER NOT NULL,
		priority INTEGER NOT NULL DEFAULT 0,
		state TEXT NOT NULL DEFAULT 'PENDING',
		payload BLOB NOT NULL,
		created_at INTEGER NOT NULL
	)`); err != nil {
		db.Close()
		return fmt.Errorf("queue schema: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_events_fifo ON events(priority DESC, created_at ASC, id ASC)`); err != nil {
		db.Close()
		return fmt.Errorf("queue index: %w", err)
	}
	cq.db = db
	return nil
}

func (cq *ChunkedQueue) Enqueue(recordID, gatewayID, externalDeviceID, integrationID string, sequence int64, priority int, payload []byte) error {
	cq.mu.Lock()
	defer cq.mu.Unlock()

	// Check storage state before write
	if cq.storageMgr != nil && cq.storageMgr.ShouldDrop(priority) {
		// Drop per priority policy, increment metric
		logger.WithFields(logrus.Fields{
			"queue": cq.prefix, "priority": priority, "state": cq.storageMgr.GetState(),
		}).Warn("offline queue: dropping low-priority record due to storage pressure")
		return fmt.Errorf("dropped: storage %s", cq.storageMgr.GetState())
	}

	// Check chunk rollover
	if cq.shouldRollover() {
		if err := cq.rollover(); err != nil {
			return err
		}
	}

	_, err := cq.db.Exec(`INSERT OR IGNORE INTO events(record_id, gateway_id, external_device_id, integration_id, sequence, timestamp, priority, state, payload, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, 'PENDING', ?, ?)`,
		recordID, gatewayID, externalDeviceID, integrationID, sequence, time.Now().Unix(), priority, payload, time.Now().Unix())
	if err != nil {
		return err
	}
	return cq.enforceQuota()
}

func (cq *ChunkedQueue) shouldRollover() bool {
	if cq.maxChunkBytes <= 0 {
		return false
	}
	fi, err := os.Stat(cq.activePath)
	if err != nil {
		return false
	}
	return fi.Size() >= cq.maxChunkBytes
}

func (cq *ChunkedQueue) rollover() error {
	if err := cq.db.Close(); err != nil {
		return err
	}
	chunkPath := filepath.Join(cq.baseDir, fmt.Sprintf("chunk-%06d.db", cq.chunkIndex))
	if err := os.Rename(cq.activePath, chunkPath); err != nil {
		// Active may not exist yet
		if !os.IsNotExist(err) {
			return err
		}
	}
	cq.chunkIndex++
	// WAL file also needs handling
	walPath := cq.activePath + "-wal"
	_ = os.Remove(walPath)
	shmPath := cq.activePath + "-shm"
	_ = os.Remove(shmPath)
	return cq.openActive()
}

func (cq *ChunkedQueue) enforceQuota() error {
	if cq.maxQueueBytes <= 0 {
		return nil
	}
	// Get total size of all chunks + active
	var totalSize int64
	files, _ := filepath.Glob(filepath.Join(cq.baseDir, "*.db"))
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil {
			totalSize += fi.Size()
		}
	}
	// Also account for WAL/shm
	files, _ = filepath.Glob(filepath.Join(cq.baseDir, "*.db-*"))
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil {
			totalSize += fi.Size()
		}
	}
	if totalSize <= cq.maxQueueBytes {
		return nil
	}
	// Evict oldest sealed chunks first (FIFO), then low-priority rows
	// For MVP, delete oldest chunk file entirely if it's sealed and empty-ish
	// More precise: delete low-priority rows
	for totalSize > cq.maxQueueBytes {
		// Try to delete oldest chunk file
		oldest := filepath.Join(cq.baseDir, fmt.Sprintf("chunk-%06d.db", 1))
		// Find smallest existing chunk
		var oldestFile string
		var oldestIdx int = 999999
		for i := 1; i < cq.chunkIndex; i++ {
			p := filepath.Join(cq.baseDir, fmt.Sprintf("chunk-%06d.db", i))
			if _, err := os.Stat(p); err == nil {
				if i < oldestIdx {
					oldestIdx = i
					oldestFile = p
				}
			}
		}
		_ = oldest
		if oldestFile != "" {
			if err := os.Remove(oldestFile); err == nil {
				_ = os.Remove(oldestFile + "-wal")
				_ = os.Remove(oldestFile + "-shm")
				break
			}
		}
		// Fallback: delete low-priority rows
		_, _ = cq.db.Exec(`DELETE FROM events WHERE id IN (SELECT id FROM events ORDER BY priority ASC, created_at ASC LIMIT 10)`)
		break
	}
	return nil
}

func (cq *ChunkedQueue) FetchBatch(limit int) ([]QueuedRecord, error) {
	cq.mu.Lock()
	defer cq.mu.Unlock()
	// FIFO: oldest first, but priority-aware (higher priority first within FIFO).
	// Critical: scan+close BEFORE any Exec. The pool is MaxOpenConns(1); issuing
	// UPDATE while rows are still open deadlocks the single connection and
	// wedges the whole agent (flush holds mu; Count/Enqueue wait forever).
	rows, err := cq.db.Query(`SELECT id, record_id, gateway_id, external_device_id, integration_id, sequence, priority, payload FROM events WHERE state='PENDING' ORDER BY priority DESC, created_at ASC, id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var out []QueuedRecord
	for rows.Next() {
		var r QueuedRecord
		if err := rows.Scan(&r.ID, &r.RecordID, &r.GatewayID, &r.ExternalDeviceID, &r.IntegrationID, &r.Sequence, &r.Priority, &r.Payload); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for _, r := range out {
		// Mark as SENDING for crash recovery (separate pass; connection free).
		_, _ = cq.db.Exec(`UPDATE events SET state='SENDING' WHERE id=?`, r.ID)
	}
	return out, nil
}

type QueuedRecord struct {
	ID               int64
	RecordID         string
	GatewayID        string
	ExternalDeviceID sql.NullString
	IntegrationID    sql.NullString
	Sequence         sql.NullInt64
	Priority         int
	Payload          []byte
}

func (cq *ChunkedQueue) Ack(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	cq.mu.Lock()
	defer cq.mu.Unlock()
	tx, err := cq.db.Begin()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM events WHERE id=?`, id); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (cq *ChunkedQueue) RecoverSending() error {
	cq.mu.Lock()
	defer cq.mu.Unlock()
	_, err := cq.db.Exec(`UPDATE events SET state='PENDING' WHERE state='SENDING'`)
	return err
}

func (cq *ChunkedQueue) Count() int {
	cq.mu.Lock()
	defer cq.mu.Unlock()
	return cq.countLocked()
}

func (cq *ChunkedQueue) countLocked() int {
	if cq.db == nil {
		return 0
	}
	var n int
	_ = cq.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
	// Also count sealed chunks
	files, _ := filepath.Glob(filepath.Join(cq.baseDir, "chunk-*.db"))
	for _, f := range files {
		db, err := sql.Open("sqlite", f+"?cache=shared")
		if err != nil {
			continue
		}
		var c int
		_ = db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&c)
		n += c
		db.Close()
	}
	return n
}

func (cq *ChunkedQueue) Close() error {
	cq.mu.Lock()
	defer cq.mu.Unlock()
	if cq.db != nil {
		return cq.db.Close()
	}
	return nil
}

// OfflineStorage manages two independent queues
type OfflineStorage struct {
	mu            sync.Mutex
	storageMgr    *StorageManager
	gatewayQueue  *ChunkedQueue
	externalQueue *ChunkedQueue
	basePath      string
}

func NewOfflineStorage(basePath string, offlineQuotaBytes, chunkMaxBytes int64) (*OfflineStorage, error) {
	sm := NewStorageManager(basePath, offlineQuotaBytes, chunkMaxBytes)
	gwQueue, err := NewChunkedQueue(basePath, "gateway", chunkMaxBytes, offlineQuotaBytes/2, sm, PriorityP1GatewayImportant)
	if err != nil {
		return nil, err
	}
	extQueue, err := NewChunkedQueue(basePath, "external-devices", chunkMaxBytes, offlineQuotaBytes/2, sm, PriorityP3ExternalNormal)
	if err != nil {
		gwQueue.Close()
		return nil, err
	}
	// Crash recovery: reset SENDING to PENDING
	_ = gwQueue.RecoverSending()
	_ = extQueue.RecoverSending()
	return &OfflineStorage{
		storageMgr:    sm,
		gatewayQueue:  gwQueue,
		externalQueue: extQueue,
		basePath:      basePath,
	}, nil
}

func (s *OfflineStorage) EnqueueGateway(recordID, gatewayID string, priority int, payload []byte) error {
	return s.gatewayQueue.Enqueue(recordID, gatewayID, "", "", 0, priority, payload)
}

func (s *OfflineStorage) EnqueueExternal(recordID, gatewayID, externalDeviceID, integrationID string, sequence int64, priority int, payload []byte) error {
	return s.externalQueue.Enqueue(recordID, gatewayID, externalDeviceID, integrationID, sequence, priority, payload)
}

func (s *OfflineStorage) GetStorageInfo() map[string]interface{} {
	info := s.storageMgr.GetInfo()
	info["gatewayQueueSize"] = s.gatewayQueue.Count()
	info["externalDeviceQueueSize"] = s.externalQueue.Count()
	info["oldestGatewayTimestamp"] = s.getOldestTimestamp(s.gatewayQueue)
	info["oldestExternalTimestamp"] = s.getOldestTimestamp(s.externalQueue)
	info["gatewayQueuePath"] = filepath.Join(s.basePath, "gateway")
	info["externalDeviceQueuePath"] = filepath.Join(s.basePath, "external-devices")
	// Calculate used bytes via WalkDir (Glob does not support **)
	used := int64(0)
	_ = filepath.WalkDir(s.basePath, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			ext := filepath.Ext(path)
			if ext == ".db" || ext == ".db-wal" || ext == ".db-shm" || ext == ".wal" || ext == ".shm" {
				if fi, err := d.Info(); err == nil {
					used += fi.Size()
				}
			}
		}
		return nil
	})
	info["offlineUsed"] = used
	s.storageMgr.UpdateState(used)
	info["storageState"] = string(s.storageMgr.GetState())
	return info
}

func (s *OfflineStorage) getOldestTimestamp(cq *ChunkedQueue) interface{} {
	cq.mu.Lock()
	defer cq.mu.Unlock()
	if cq.db == nil {
		return nil
	}
	var ts sql.NullInt64
	_ = cq.db.QueryRow(`SELECT MIN(created_at) FROM events`).Scan(&ts)
	if ts.Valid {
		return time.Unix(ts.Int64, 0).UTC().Format(time.RFC3339)
	}
	return nil
}

func (s *OfflineStorage) Close() error {
	var err1, err2 error
	if s.gatewayQueue != nil {
		err1 = s.gatewayQueue.Close()
	}
	if s.externalQueue != nil {
		err2 = s.externalQueue.Close()
	}
	if err1 != nil {
		return err1
	}
	return err2
}
