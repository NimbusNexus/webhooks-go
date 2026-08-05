// Package sqlite provides a durable, transactional webhooks.Store backed by SQLite via the pure-Go
// modernc.org/sqlite driver (no cgo). Importing this package is what pulls the driver in — a
// consumer who only uses the core SDK never compiles it.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	webhooks "github.com/NimbusNexus/webhooks-go"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver
)

// project_id is NULLABLE: NULL means "the workspace's default project", which the server resolves —
// there is no client-side sentinel for it, so an unset project is stored as NULL, not as a literal.
const ddl = `
CREATE TABLE IF NOT EXISTS webhookd_outbox (
	id              TEXT PRIMARY KEY,
	event_type      TEXT NOT NULL,
	payload         TEXT NOT NULL,
	project_id      TEXT,
	source          TEXT,
	created_at      INTEGER NOT NULL,
	attempts        INTEGER NOT NULL,
	last_error      TEXT,
	next_attempt_at INTEGER NOT NULL
)`

// Store is a SQLite-backed webhooks.Store. Sent records are deleted; a record whose retry budget is
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
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrate upgrades an outbox database written by an older SDK — whose `project` column held a
// project SLUG and was NOT NULL — to the current schema: `project_id`, nullable. SQLite cannot drop
// a NOT NULL constraint in place, so the table is rebuilt inside a transaction and the rows are
// copied across (an empty slug becoming NULL, i.e. the workspace default). It is a no-op on a fresh
// database and on one that has already been migrated, so Open stays idempotent.
func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(webhookd_outbox)`)
	if err != nil {
		return fmt.Errorf("sqlite: inspect table: %w", err)
	}
	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, colType    string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("sqlite: inspect table: %w", err)
		}
		cols[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite: inspect table: %w", err)
	}
	if !cols["project"] || cols["project_id"] {
		return nil // fresh or already migrated
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("sqlite: migrate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`ALTER TABLE webhookd_outbox RENAME TO webhookd_outbox_pre_project_id`,
		ddl,
		// The legacy magic slug "default" (and an empty slug) meant "the workspace's default project",
		// which is now expressed as NULL. Any other buffered slug is copied verbatim — a slug cannot
		// be resolved to an id client-side, so it is left for a human rather than silently dropped.
		`INSERT INTO webhookd_outbox
			(id, event_type, payload, project_id, source, created_at, attempts, last_error, next_attempt_at)
		 SELECT id, event_type, payload, NULLIF(NULLIF(project, ''), 'default'), source, created_at,
		        attempts, last_error, next_attempt_at
		 FROM webhookd_outbox_pre_project_id`,
		`DROP TABLE webhookd_outbox_pre_project_id`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("sqlite: migrate project -> project_id: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: migrate project -> project_id: %w", err)
	}
	return nil
}

// projectIDArg maps an empty ProjectID — "the workspace's default project" — to SQL NULL. The default
// project's id is opaque and per-workspace, so only the server can resolve it; the client stores
// nothing.
func projectIDArg(id string) any {
	if id == "" {
		return nil
	}
	return id
}

func (s *Store) Save(ctx context.Context, r webhooks.Record) error {
	payload, err := json.Marshal(r.Payload)
	if err != nil {
		return fmt.Errorf("sqlite: encode payload: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO webhookd_outbox
			(id, event_type, payload, project_id, source, created_at, attempts, last_error, next_attempt_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			event_type      = excluded.event_type,
			payload         = excluded.payload,
			project_id      = excluded.project_id,
			source          = excluded.source,
			created_at      = excluded.created_at,
			attempts        = excluded.attempts,
			last_error      = excluded.last_error,
			next_attempt_at = excluded.next_attempt_at`,
		r.ID, r.EventType, string(payload), projectIDArg(r.ProjectID), r.Source,
		r.CreatedAt.UnixMilli(), r.Attempts, r.LastError, r.NextAttemptAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("sqlite: save: %w", err)
	}
	return nil
}

func (s *Store) ListPending(ctx context.Context, limit int) ([]webhooks.Record, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, event_type, payload, project_id, source, created_at, attempts, last_error, next_attempt_at
		FROM webhookd_outbox
		WHERE next_attempt_at <= ?
		ORDER BY created_at ASC, id ASC
		LIMIT ?`, time.Now().UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list_pending: %w", err)
	}
	defer rows.Close()

	var out []webhooks.Record
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

func scanRow(rows *sql.Rows) (webhooks.Record, error) {
	var (
		r         webhooks.Record
		payload   string
		projectID sql.NullString
		source    sql.NullString
		lastError sql.NullString
		createdMs int64
		nextMs    int64
	)
	if err := rows.Scan(&r.ID, &r.EventType, &payload, &projectID, &source, &createdMs, &r.Attempts, &lastError, &nextMs); err != nil {
		return webhooks.Record{}, fmt.Errorf("sqlite: scan: %w", err)
	}
	if err := json.Unmarshal([]byte(payload), &r.Payload); err != nil {
		return webhooks.Record{}, fmt.Errorf("sqlite: decode payload: %w", err)
	}
	r.ProjectID = projectID.String // NULL -> "" -> the workspace's default project
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
