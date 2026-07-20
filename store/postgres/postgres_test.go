package postgres_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	webhookd "github.com/NimbusNexus/webhookd-go"
	"github.com/NimbusNexus/webhookd-go/store/postgres"
	"github.com/NimbusNexus/webhookd-go/store/storetest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The Postgres contract is the SAME as every other store's; it runs only when WEBHOOKD_TEST_PG_URL
// points at a reachable server, and skips cleanly otherwise. Each store uses a unique table name (a
// valid identifier) so runs never collide, and the table is dropped on cleanup.
func TestPostgresStoreContract(t *testing.T) {
	dsn := os.Getenv("WEBHOOKD_TEST_PG_URL")
	if dsn == "" {
		t.Skip("set WEBHOOKD_TEST_PG_URL to run the Postgres store contract")
	}
	ctx := context.Background()
	var n int
	storetest.RunContract(t, func(t *testing.T) webhookd.Store {
		n++
		table := fmt.Sprintf("webhookd_outbox_test_%d_%d", time.Now().UnixNano(), n)
		s, err := postgres.Open(ctx, postgres.Options{ConnString: dsn, Table: table})
		if err != nil {
			t.Fatalf("postgres.Open: %v", err)
		}
		t.Cleanup(func() {
			if pool, perr := pgxpool.New(ctx, dsn); perr == nil {
				_, _ = pool.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", table))
				pool.Close()
			}
			_ = s.Close()
		})
		return s
	})
}

// A custom table name must be honoured by EVERY query (regression guard for the templating bug the
// Python PostgresStore once shipped). Only runs with a live server.
func TestPostgresCustomTableUsedEverywhere(t *testing.T) {
	dsn := os.Getenv("WEBHOOKD_TEST_PG_URL")
	if dsn == "" {
		t.Skip("set WEBHOOKD_TEST_PG_URL to run the Postgres store contract")
	}
	ctx := context.Background()
	table := fmt.Sprintf("custom_outbox_%d", time.Now().UnixNano())
	s, err := postgres.Open(ctx, postgres.Options{ConnString: dsn, Table: table})
	if err != nil {
		t.Fatalf("postgres.Open: %v", err)
	}
	defer func() {
		if pool, perr := pgxpool.New(ctx, dsn); perr == nil {
			_, _ = pool.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", table))
			pool.Close()
		}
		_ = s.Close()
	}()

	now := time.Now()
	rec := webhookd.Record{ID: "c1", EventType: "e", Payload: map[string]any{"x": 1}, Environment: "prod", Application: "default", CreatedAt: now, NextAttemptAt: now}
	if err := s.Save(ctx, rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if sz, err := s.Size(ctx); err != nil || sz != 1 {
		t.Fatalf("Size = %d, err = %v; want 1, nil", sz, err)
	}
	rows, err := s.ListPending(ctx, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListPending = %v, err = %v; want 1 row", rows, err)
	}
	if err := s.MarkSent(ctx, "c1"); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	if sz, _ := s.Size(ctx); sz != 0 {
		t.Fatalf("Size after MarkSent = %d, want 0", sz)
	}
}
