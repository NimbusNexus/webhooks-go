// Package sqlite provides a durable, transactional webhookd.Store backed by SQLite via the pure-Go
// modernc.org/sqlite driver (no cgo). Importing this package is what pulls the driver in — a
// consumer who only uses the core SDK never compiles it.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	webhookd "github.com/NimbusNexus/webhookd-go"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver
)

const ddl = `
CREATE TABLE IF NOT EXISTS webhookd_outbox (
	id              TEXT PRIMARY KEY,
	event_type      TEXT NOT NULL,
	payload         TEXT NOT NULL,
	environment     TEXT NOT NULL,
	application     TEXT NOT NULL,
	source          TEXT,
	created_at      INTEGER NOT NULL,
	attempts        INTEGER NOT NULL,
	last_error      TEXT,
	next_attempt_at INTEGER NOT NULL
)`

// Store is a SQLite-backed webhookd.Store. Sent records are deleted; a record whose retry budget is
// exhausted is parked with a far-future next_attempt_at, so ListPending never returns it again.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) a SQLite database at path and ensures the outbox table exists. Pass a file
// path to persist across restarts, or ":memory:" for an ephemeral store (tests). The connection pool
// is pinned to a single connection so an in-memory database is shared and writes never contend.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(ddl); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: create table: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Save(ctx context.Context, r webhookd.Record) error {
	payload, err := json.Marshal(r.Payload)
	if err != nil {
		return fmt.Errorf("sqlite: encode payload: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO webhookd_outbox
			(id, event_type, payload, environment, application, source, created_at, attempts, last_error, next_attempt_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			event_type      = excluded.event_type,
			payload         = excluded.payload,
			environment     = excluded.environment,
			application     = excluded.application,
			source          = excluded.source,
			created_at      = excluded.created_at,
			attempts        = excluded.attempts,
			last_error      = excluded.last_error,
			next_attempt_at = excluded.next_attempt_at`,
		r.ID, r.EventType, string(payload), r.Environment, r.Application, r.Source,
		r.CreatedAt.UnixMilli(), r.Attempts, r.LastError, r.NextAttemptAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("sqlite: save: %w", err)
	}
	return nil
}

func (s *Store) ListPending(ctx context.Context, limit int) ([]webhookd.Record, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, event_type, payload, environment, application, source, created_at, attempts, last_error, next_attempt_at
		FROM webhookd_outbox
		WHERE next_attempt_at <= ?
		ORDER BY created_at ASC, id ASC
		LIMIT ?`, time.Now().UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list_pending: %w", err)
	}
	defer rows.Close()

	var out []webhookd.Record
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) MarkSent(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM webhookd_outbox WHERE id = ?`, id); err != nil {
		return fmt.Errorf("sqlite: mark_sent: %w", err)
	}
	return nil
}

func (s *Store) MarkFailed(ctx context.Context, id string, lastError *string, attempts int, nextAttemptAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE webhookd_outbox SET last_error = ?, attempts = ?, next_attempt_at = ? WHERE id = ?`,
		lastError, attempts, nextAttemptAt.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("sqlite: mark_failed: %w", err)
	}
	return nil
}

func (s *Store) Size(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM webhookd_outbox`).Scan(&n); err != nil {
		return 0, fmt.Errorf("sqlite: size: %w", err)
	}
	return n, nil
}

func (s *Store) Close() error { return s.db.Close() }

func scanRow(rows *sql.Rows) (webhookd.Record, error) {
	var (
		r         webhookd.Record
		payload   string
		source    sql.NullString
		lastError sql.NullString
		createdMs int64
		nextMs    int64
	)
	if err := rows.Scan(&r.ID, &r.EventType, &payload, &r.Environment, &r.Application, &source, &createdMs, &r.Attempts, &lastError, &nextMs); err != nil {
		return webhookd.Record{}, fmt.Errorf("sqlite: scan: %w", err)
	}
	if err := json.Unmarshal([]byte(payload), &r.Payload); err != nil {
		return webhookd.Record{}, fmt.Errorf("sqlite: decode payload: %w", err)
	}
	if source.Valid {
		r.Source = &source.String
	}
	if lastError.Valid {
		r.LastError = &lastError.String
	}
	r.CreatedAt = time.UnixMilli(createdMs)
	r.NextAttemptAt = time.UnixMilli(nextMs)
	return r, nil
}
