// Package postgres provides a durable, transactional webhooks.Store backed by PostgreSQL via
// github.com/jackc/pgx/v5 (pgxpool). A single outbox table (configurable name) holds the records;
// upsert on id, and pending = WHERE NOT sent AND next_attempt_at <= now. Importing this package is
// what pulls the driver in — the core SDK never compiles it.
package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	webhooks "github.com/NimbusNexus/webhooks-go"
	"github.com/jackc/pgx/v5/pgxpool"
)

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Store is a Postgres-backed webhooks.Store. MarkSent flips a `sent` flag; a record whose retry
// budget is exhausted is parked with a far-future next_attempt_at, so ListPending excludes it.
//
// Every statement is templated with the SAME configured table name (computed once in Open), so a
// custom table name works end to end — there is no hardcoded table anywhere in the queries.
type Store struct {
	pool  *pgxpool.Pool
	table string

	saveSQL, listSQL, markSentSQL, markFailedSQL, sizeSQL string
}

// Options configures Open.
type Options struct {
	// ConnString is a Postgres connection string, e.g. postgres://user:pass@host:5432/db. Ignored
	// when Pool is supplied.
	ConnString string
	// Pool reuses an existing pgx pool instead of ConnString.
	Pool *pgxpool.Pool
	// Table is the outbox table name (a plain SQL identifier). Default "webhookd_outbox".
	Table string
}

// Open builds a Postgres-backed store and ensures the outbox table exists. Supply either
// opts.ConnString or opts.Pool.
func Open(ctx context.Context, opts Options) (*Store, error) {
	table := opts.Table
	if table == "" {
		table = "webhookd_outbox"
	}
	if !identifier.MatchString(table) {
		return nil, fmt.Errorf("postgres: invalid table name %q", table)
	}

	pool := opts.Pool
	if pool == nil {
		p, err := pgxpool.New(ctx, opts.ConnString)
		if err != nil {
			return nil, fmt.Errorf("postgres: connect: %w", err)
		}
		pool = p
	}

	s := &Store{pool: pool, table: table}
	s.saveSQL = fmt.Sprintf(`
		INSERT INTO %[1]s
			(id, event_type, payload, project_id, source, sent, attempts, last_error, next_attempt_at, created_at)
		VALUES ($1, $2, $3::jsonb, $4, $5, FALSE, $6, $7, $8, $9)
		ON CONFLICT (id) DO UPDATE SET
			event_type      = EXCLUDED.event_type,
			payload         = EXCLUDED.payload,
			project_id      = EXCLUDED.project_id,
			source          = EXCLUDED.source,
			sent            = EXCLUDED.sent,
			attempts        = EXCLUDED.attempts,
			last_error      = EXCLUDED.last_error,
			next_attempt_at = EXCLUDED.next_attempt_at,
			created_at      = EXCLUDED.created_at`, table)
	s.listSQL = fmt.Sprintf(`
		SELECT id, event_type, payload, project_id, source, attempts, last_error, next_attempt_at, created_at
		FROM %[1]s
		WHERE NOT sent AND next_attempt_at <= $1
		ORDER BY created_at ASC, id ASC
		LIMIT $2`, table)
	s.markSentSQL = fmt.Sprintf(`UPDATE %[1]s SET sent = TRUE WHERE id = $1`, table)
	s.markFailedSQL = fmt.Sprintf(`UPDATE %[1]s SET attempts = $2, last_error = $3, next_attempt_at = $4 WHERE id = $1`, table)
	s.sizeSQL = fmt.Sprintf(`SELECT COUNT(*) FROM %[1]s WHERE NOT sent`, table)

	// project_id is NULLABLE: NULL means "the workspace's default project", which only the server can
	// resolve (the id is opaque and per-workspace), so an unset project stores no value.
	ddl := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %[1]s (
			id              TEXT PRIMARY KEY,
			event_type      TEXT NOT NULL,
			payload         JSONB NOT NULL,
			project_id      TEXT,
			source          TEXT,
			sent            BOOLEAN NOT NULL DEFAULT FALSE,
			attempts        INTEGER NOT NULL,
			last_error      TEXT,
			next_attempt_at BIGINT NOT NULL,
			created_at      BIGINT NOT NULL
		)`, table)
	if _, err := pool.Exec(ctx, ddl); err != nil {
		if opts.Pool == nil {
			pool.Close()
		}
		return nil, fmt.Errorf("postgres: create table: %w", err)
	}
	if err := migrate(ctx, pool, table); err != nil {
		if opts.Pool == nil {
			pool.Close()
		}
		return nil, err
	}
	return s, nil
}

// migrate upgrades an outbox table written by an older SDK — whose `project` column held a project
// SLUG and was NOT NULL — to the current schema: `project_id`, nullable. Both statements are
// conditional/idempotent, so Open stays a no-op on a fresh or already-migrated table. table has
// already been validated as a plain SQL identifier by Open.
func migrate(ctx context.Context, pool *pgxpool.Pool, table string) error {
	// The columns are looked up through to_regclass, which resolves the table the SAME way the
	// store's own statements do (search_path, case folding) — an information_schema scan by bare
	// name could match a like-named table in another schema. Rename first, then relax NOT NULL, and
	// only then blank the legacy sentinels: the UPDATE writes NULLs the old constraint rejected.
	rename := fmt.Sprintf(`
		DO $$
		DECLARE tbl regclass := to_regclass('%[1]s');
		BEGIN
			IF tbl IS NOT NULL
			   AND EXISTS (
					SELECT 1 FROM pg_attribute
					WHERE attrelid = tbl AND attname = 'project' AND NOT attisdropped
			   ) AND NOT EXISTS (
					SELECT 1 FROM pg_attribute
					WHERE attrelid = tbl AND attname = 'project_id' AND NOT attisdropped
			   ) THEN
				EXECUTE 'ALTER TABLE ' || tbl::text || ' RENAME COLUMN project TO project_id';
				EXECUTE 'ALTER TABLE ' || tbl::text || ' ALTER COLUMN project_id DROP NOT NULL';
				-- The legacy magic slug "default" (and an empty slug) meant "the workspace's default
				-- project", which is now expressed as NULL. Any other buffered slug is kept
				-- verbatim: a slug cannot be resolved to an id client-side, so it is left for a
				-- human rather than silently dropped.
				EXECUTE 'UPDATE ' || tbl::text || ' SET project_id = NULL WHERE project_id IN ('''', ''default'')';
			END IF;
		END $$`, table)
	if _, err := pool.Exec(ctx, rename); err != nil {
		return fmt.Errorf("postgres: migrate project -> project_id: %w", err)
	}
	// Idempotent; also relaxes a project_id column left NOT NULL by an intermediate version.
	dropNotNull := fmt.Sprintf(`ALTER TABLE %[1]s ALTER COLUMN project_id DROP NOT NULL`, table)
	if _, err := pool.Exec(ctx, dropNotNull); err != nil {
		return fmt.Errorf("postgres: migrate project_id to nullable: %w", err)
	}
	return nil
}

// projectIDArg maps an empty ProjectID — "the workspace's default project" — to SQL NULL. The default
// project's id is opaque and per-workspace, so only the server can resolve it; the client stores
// nothing.
func projectIDArg(id string) *string {
	if id == "" {
		return nil
	}
	return &id
}

func (s *Store) Save(ctx context.Context, r webhooks.Record) error {
	payload, err := json.Marshal(r.Payload)
	if err != nil {
		return fmt.Errorf("postgres: encode payload: %w", err)
	}
	_, err = s.pool.Exec(ctx, s.saveSQL,
		r.ID, r.EventType, string(payload), projectIDArg(r.ProjectID), r.Source,
		r.Attempts, r.LastError, r.NextAttemptAt.UnixMilli(), r.CreatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("postgres: save: %w", err)
	}
	return nil
}

func (s *Store) ListPending(ctx context.Context, limit int) ([]webhooks.Record, error) {
	rows, err := s.pool.Query(ctx, s.listSQL, time.Now().UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list_pending: %w", err)
	}
	defer rows.Close()

	var out []webhooks.Record
	for rows.Next() {
		var (
			r         webhooks.Record
			payload   []byte
			projectID *string
			source    *string
			lastError *string
			nextMs    int64
			createdMs int64
		)
		if err := rows.Scan(&r.ID, &r.EventType, &payload, &projectID, &source, &r.Attempts, &lastError, &nextMs, &createdMs); err != nil {
			return nil, fmt.Errorf("postgres: scan: %w", err)
		}
		if err := json.Unmarshal(payload, &r.Payload); err != nil {
			return nil, fmt.Errorf("postgres: decode payload: %w", err)
		}
		if projectID != nil { // NULL -> "" -> the workspace's default project
			r.ProjectID = *projectID
		}
		r.Source = source
		r.LastError = lastError
		r.NextAttemptAt = time.UnixMilli(nextMs)
		r.CreatedAt = time.UnixMilli(createdMs)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) MarkSent(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx, s.markSentSQL, id); err != nil {
		return fmt.Errorf("postgres: mark_sent: %w", err)
	}
	return nil
}

func (s *Store) MarkFailed(ctx context.Context, id string, lastError *string, attempts int, nextAttemptAt time.Time) error {
	if _, err := s.pool.Exec(ctx, s.markFailedSQL, id, attempts, lastError, nextAttemptAt.UnixMilli()); err != nil {
		return fmt.Errorf("postgres: mark_failed: %w", err)
	}
	return nil
}

func (s *Store) Size(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, s.sizeSQL).Scan(&n); err != nil {
		return 0, fmt.Errorf("postgres: size: %w", err)
	}
	return n, nil
}

func (s *Store) Close() error {
	s.pool.Close()
	return nil
}
