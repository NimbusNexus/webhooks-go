package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	webhooks "github.com/NimbusNexus/webhooks-go"
	"github.com/NimbusNexus/webhooks-go/store/sqlite"
	"github.com/NimbusNexus/webhooks-go/store/storetest"
)

// SQLite runs the full contract locally — the driver is pure Go and ":memory:" needs no server.
func TestSqliteStoreContract(t *testing.T) {
	storetest.RunContract(t, func(t *testing.T) webhooks.Store {
		s, err := sqlite.Open(":memory:")
		if err != nil {
			t.Fatalf("sqlite.Open: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

// The schema this SDK wrote before webhookd dropped project slugs: a NOT NULL `project` column
// holding a slug, with "default" as the magic "the workspace's default project" value.
const legacyDDL = `
CREATE TABLE webhookd_outbox (
	id              TEXT PRIMARY KEY,
	event_type      TEXT NOT NULL,
	payload         TEXT NOT NULL,
	project         TEXT NOT NULL,
	source          TEXT,
	created_at      INTEGER NOT NULL,
	attempts        INTEGER NOT NULL,
	last_error      TEXT,
	next_attempt_at INTEGER NOT NULL
)`

// Open must upgrade an outbox file written by the pre-project_id SDK instead of stranding the events
// buffered in it: the column is renamed and relaxed to nullable, the legacy "default" sentinel
// becomes NULL (unset = the workspace default), and any other slug survives for a human to fix.
func TestOpenMigratesLegacyProjectColumn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "outbox.db")

	db, err := sql.Open("sqlite", path) // driver registered by the store package's blank import
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(legacyDDL); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	past := time.Now().Add(-time.Minute).UnixMilli()
	seed := `INSERT INTO webhookd_outbox
		(id, event_type, payload, project, source, created_at, attempts, last_error, next_attempt_at)
		VALUES (?, 'order.created', '{"k":1}', ?, NULL, ?, 0, NULL, ?)`
	for _, row := range []struct{ id, project string }{
		{"was-default", "default"},
		{"was-slug", "payments"},
	} {
		if _, err := db.Exec(seed, row.id, row.project, past, past); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open on a legacy database: %v", err)
	}
	defer func() { _ = s.Close() }()

	rows, err := s.ListPending(ctx, 10)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (buffered events must survive the migration)", len(rows))
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.ID] = r.ProjectID
	}
	if got["was-default"] != "" {
		t.Errorf("project_id for the legacy \"default\" slug = %q, want \"\" (unset)", got["was-default"])
	}
	if got["was-slug"] != "payments" {
		t.Errorf("project_id = %q, want payments (an unresolvable slug is kept, not dropped)", got["was-slug"])
	}

	// The migrated column is nullable: an unset project must save without a NOT NULL violation.
	now := time.Now()
	fresh := webhooks.Record{ID: "fresh", EventType: "e", Payload: map[string]any{"k": 2}, CreatedAt: now, NextAttemptAt: now}
	if err := s.Save(ctx, fresh); err != nil {
		t.Fatalf("Save with an unset ProjectID after migration: %v", err)
	}

	// Re-opening an already-migrated database is a no-op, not a second migration.
	s2, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if sz, err := s2.Size(ctx); err != nil || sz != 3 {
		t.Fatalf("Size after re-open = %d, err = %v; want 3, nil", sz, err)
	}
}
