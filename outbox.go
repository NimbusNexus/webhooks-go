package webhooks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// deadNextAttempt is the sentinel NextAttemptAt used to park a record whose retry budget is
// exhausted. It is far in the future, so ListPending (which returns only records due at or before
// now) never hands it out again — the record stays durably buffered, flagged dead. Encoding "dead"
// as a far-future next-attempt time (rather than a separate flag) keeps the Store contract to the
// six methods below and mirrors the TypeScript SDK's DEAD_NEXT_ATTEMPT_MS sentinel.
var deadNextAttempt = time.Date(9999, time.January, 1, 0, 0, 0, 0, time.UTC)

// Record is one buffered event. ID doubles as the webhookd Idempotency-Key (caller-supplied or a
// generated UUID v4), so re-saving the same ID (an idempotent enqueue) overwrites, and re-draining
// after a crash never double-publishes. An EMPTY ProjectID means "the workspace's default project": it
// is stored as SQL NULL and omitted from the publish body on Drain.
type Record struct {
	ID            string         `json:"id"`
	EventType     string         `json:"event_type"`
	Payload       map[string]any `json:"payload"`
	ProjectID     string         `json:"project_id,omitempty"`
	Source        *string        `json:"source"`
	CreatedAt     time.Time      `json:"created_at"`
	Attempts      int            `json:"attempts"`
	LastError     *string        `json:"last_error"`
	NextAttemptAt time.Time      `json:"next_attempt_at"`
}

// Store is a durable buffer of pending Records. Every method is safe to call from the background
// drainer goroutine and the caller goroutine concurrently. Built-ins MemoryStore and FileStore ship
// in this package (stdlib only); SqliteStore, RedisStore and PostgresStore live in subpackages under
// go/store so a consumer who never imports them never compiles their drivers.
type Store interface {
	// Save inserts-or-UPDATEs by record.ID — the same ID overwrites, so enqueue is idempotent.
	Save(ctx context.Context, record Record) error
	// ListPending returns records not yet sent (and not dead) whose NextAttemptAt <= now, oldest
	// first (by CreatedAt), capped at limit.
	ListPending(ctx context.Context, limit int) ([]Record, error)
	// MarkSent removes (or flags sent) a record after a 2xx.
	MarkSent(ctx context.Context, id string) error
	// MarkFailed persists the failure and schedules the next retry (or parks the record dead via a
	// far-future nextAttemptAt).
	MarkFailed(ctx context.Context, id string, lastError *string, attempts int, nextAttemptAt time.Time) error
	// Size reports the count of records still buffered (i.e. not yet sent); dead records included.
	Size(ctx context.Context) (int, error)
	// Close releases any resources (file handles, DB connections). The zero-work stores no-op.
	Close() error
}

// newUUIDv4 returns a random RFC 4122 v4 UUID string using crypto/rand (no third-party dep).
func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read never fails on supported platforms; fall back to a time-seeded value.
		return fmt.Sprintf("uuid-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// cloneRecord deep-copies a record (payload + pointer fields) so a store never hands out an object
// the caller can mutate in place.
func cloneRecord(r Record) Record {
	cp := r
	if r.Payload != nil {
		out := make(map[string]any, len(r.Payload))
		b, err := json.Marshal(r.Payload)
		if err == nil {
			_ = json.Unmarshal(b, &out)
		}
		cp.Payload = out
	}
	if r.Source != nil {
		s := *r.Source
		cp.Source = &s
	}
	if r.LastError != nil {
		s := *r.LastError
		cp.LastError = &s
	}
	return cp
}

// byCreatedAt orders records oldest-first, with ID as a stable tiebreak.
func byCreatedAt(a, b Record) bool {
	if a.CreatedAt.Equal(b.CreatedAt) {
		return a.ID < b.ID
	}
	return a.CreatedAt.Before(b.CreatedAt)
}

// -- MemoryStore --------------------------------------------------------------

// MemoryStore is an in-process, non-durable Store. The default for tests and single-process
// best-effort buffering.
type MemoryStore struct {
	mu   sync.Mutex
	rows map[string]Record
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make(map[string]Record)}
}

func (s *MemoryStore) Save(_ context.Context, record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[record.ID] = cloneRecord(record)
	return nil
}

func (s *MemoryStore) ListPending(_ context.Context, limit int) ([]Record, error) {
	now := time.Now()
	s.mu.Lock()
	due := make([]Record, 0, len(s.rows))
	for _, r := range s.rows {
		if !r.NextAttemptAt.After(now) {
			due = append(due, cloneRecord(r))
		}
	}
	s.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return byCreatedAt(due[i], due[j]) })
	if limit >= 0 && len(due) > limit {
		due = due[:limit]
	}
	return due, nil
}

func (s *MemoryStore) MarkSent(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, id)
	return nil
}

func (s *MemoryStore) MarkFailed(_ context.Context, id string, lastError *string, attempts int, nextAttemptAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rows[id]; ok {
		r.LastError = lastError
		r.Attempts = attempts
		r.NextAttemptAt = nextAttemptAt
		s.rows[id] = r
	}
	return nil
}

func (s *MemoryStore) Size(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows), nil
}

func (s *MemoryStore) Close() error { return nil }

// -- FileStore ----------------------------------------------------------------

// FileStore is a durable Store backed by a directory of per-record JSON files, written atomically
// (temp file + rename) so a crash mid-write never corrupts a record. It survives process restarts —
// a fresh FileStore over the same directory sees every un-sent record. Suitable for a single
// process; it does not coordinate concurrent drainers across processes.
type FileStore struct {
	mu  sync.Mutex
	dir string
}

// NewFileStore returns a durable file-backed store rooted at dir, creating it if necessary.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, &Error{Message: fmt.Sprintf("failed to create outbox directory: %v", err), Err: err}
	}
	return &FileStore{dir: dir}, nil
}

// pathFor maps an arbitrary id (ids may be caller-supplied idempotency keys) to a filesystem-safe
// filename by hex-encoding it.
func (s *FileStore) pathFor(id string) string {
	return filepath.Join(s.dir, hex.EncodeToString([]byte(id))+".json")
}

func (s *FileStore) write(record Record) error {
	dest := s.pathFor(record.ID)
	data, err := json.Marshal(record)
	if err != nil {
		return &Error{Message: fmt.Sprintf("failed to encode record: %v", err), Err: err}
	}
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	tmp := dest + ".tmp-" + hex.EncodeToString(suffix[:])
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return &Error{Message: fmt.Sprintf("failed to write record: %v", err), Err: err}
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return &Error{Message: fmt.Sprintf("failed to commit record: %v", err), Err: err}
	}
	return nil
}

func (s *FileStore) read(path string) (Record, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, false
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return Record{}, false
	}
	return r, true
}

func (s *FileStore) all() ([]Record, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, &Error{Message: fmt.Sprintf("failed to read outbox directory: %v", err), Err: err}
	}
	rows := make([]Record, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		if r, ok := s.read(filepath.Join(s.dir, name)); ok {
			rows = append(rows, r)
		}
	}
	return rows, nil
}

func (s *FileStore) Save(_ context.Context, record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(record)
}

func (s *FileStore) ListPending(_ context.Context, limit int) ([]Record, error) {
	now := time.Now()
	s.mu.Lock()
	all, err := s.all()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	due := make([]Record, 0, len(all))
	for _, r := range all {
		if !r.NextAttemptAt.After(now) {
			due = append(due, r)
		}
	}
	sort.Slice(due, func(i, j int) bool { return byCreatedAt(due[i], due[j]) })
	if limit >= 0 && len(due) > limit {
		due = due[:limit]
	}
	return due, nil
}

func (s *FileStore) MarkSent(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.pathFor(id)); err != nil && !os.IsNotExist(err) {
		return &Error{Message: fmt.Sprintf("failed to remove record: %v", err), Err: err}
	}
	return nil
}

func (s *FileStore) MarkFailed(_ context.Context, id string, lastError *string, attempts int, nextAttemptAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.read(s.pathFor(id))
	if !ok {
		return nil // gone — nothing to update
	}
	r.LastError = lastError
	r.Attempts = attempts
	r.NextAttemptAt = nextAttemptAt
	return s.write(r)
}

func (s *FileStore) Size(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.all()
	if err != nil {
		return 0, err
	}
	return len(all), nil
}

func (s *FileStore) Close() error { return nil }
