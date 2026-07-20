// Package postgres provides a durable, transactional webhookd.Store backed by PostgreSQL via
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

	webhookd "github.com/NimbusNexus/webhookd-go"
	"github.com/jackc/pgx/v5/pgxpool"
)

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Store is a Postgres-backed webhookd.Store. MarkSent flips a `sent` flag; a record whose retry
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
			(id, event_type, payload, environment, application, source, sent, attempts, last_error, next_attempt_at, created_at)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6, FALSE, $7, $8, $9, $10)
		ON CONFLICT (id) DO UPDATE SET
			event_type      = EXCLUDED.event_type,
			payload         = EXCLUDED.payload,
			environment     = EXCLUDED.environment,
			application     = EXCLUDED.application,
			source          = EXCLUDED.source,
			sent            = EXCLUDED.sent,
			attempts        = EXCLUDED.attempts,
			last_error      = EXCLUDED.last_error,
			next_attempt_at = EXCLUDED.next_attempt_at,
			created_at      = EXCLUDED.created_at`, table)
	s.listSQL = fmt.Sprintf(`
		SELECT id, event_type, payload, environment, application, source, attempts, last_error, next_attempt_at, created_at
		FROM %[1]s
		WHERE NOT sent AND next_attempt_at <= $1
		ORDER BY created_at ASC, id ASC
		LIMIT $2`, table)
	s.markSentSQL = fmt.Sprintf(`UPDATE %[1]s SET sent = TRUE WHERE id = $1`, table)
	s.markFailedSQL = fmt.Sprintf(`UPDATE %[1]s SET attempts = $2, last_error = $3, next_attempt_at = $4 WHERE id = $1`, table)
	s.sizeSQL = fmt.Sprintf(`SELECT COUNT(*) FROM %[1]s WHERE NOT sent`, table)

	ddl := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %[1]s (
			id              TEXT PRIMARY KEY,
			event_type      TEXT NOT NULL,
			payload         JSONB NOT NULL,
			environment     TEXT NOT NULL,
			application     TEXT NOT NULL,
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
	return s, nil
}

func (s *Store) Save(ctx context.Context, r webhookd.Record) error {
	payload, err := json.Marshal(r.Payload)
	if err != nil {
		return fmt.Errorf("postgres: encode payload: %w", err)
	}
	_, err = s.pool.Exec(ctx, s.saveSQL,
		r.ID, r.EventType, string(payload), r.Environment, r.Application, r.Source,
		r.Attempts, r.LastError, r.NextAttemptAt.UnixMilli(), r.CreatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("postgres: save: %w", err)
	}
	return nil
}

func (s *Store) ListPending(ctx context.Context, limit int) ([]webhookd.Record, error) {
	rows, err := s.pool.Query(ctx, s.listSQL, time.Now().UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list_pending: %w", err)
	}
	defer rows.Close()

	var out []webhookd.Record
	for rows.Next() {
		var (
			r         webhookd.Record
			payload   []byte
			source    *string
			lastError *string
			nextMs    int64
			createdMs int64
		)
		if err := rows.Scan(&r.ID, &r.EventType, &payload, &r.Environment, &r.Application, &source, &r.Attempts, &lastError, &nextMs, &createdMs); err != nil {
			return nil, fmt.Errorf("postgres: scan: %w", err)
		}
		if err := json.Unmarshal(payload, &r.Payload); err != nil {
			return nil, fmt.Errorf("postgres: decode payload: %w", err)
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
