// Package storetest provides a shared Store contract test, parametrized over any webhooks.Store
// implementation. Each store package calls RunContract with a factory that builds a fresh, empty
// store; the durable stores that need a server (Redis, Postgres) skip cleanly when their env var is
// unset (that gating lives in each store's own _test.go, not here).
package storetest

import (
	"context"
	"testing"
	"time"

	webhooks "github.com/NimbusNexus/webhooks-go"
)

// Factory builds a fresh, empty Store for one subtest. Register a t.Cleanup to close/reset it.
type Factory func(t *testing.T) webhooks.Store

func strptr(s string) *string { return &s }

// testProjectID is an opaque project id (the wire shape since webhookd dropped project slugs).
const testProjectID = "prj_3f9a1c7b"

func rec(id string, createdAt, nextAttemptAt time.Time) webhooks.Record {
	return webhooks.Record{
		ID:            id,
		EventType:     "order.created",
		Payload:       map[string]any{"id": id},
		ProjectID:     testProjectID,
		CreatedAt:     createdAt,
		Attempts:      0,
		NextAttemptAt: nextAttemptAt,
	}
}

func mustSize(t *testing.T, ctx context.Context, s webhooks.Store, want int) {
	t.Helper()
	got, err := s.Size(ctx)
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if got != want {
		t.Fatalf("Size = %d, want %d", got, want)
	}
}

func mustList(t *testing.T, ctx context.Context, s webhooks.Store, limit int) []webhooks.Record {
	t.Helper()
	rows, err := s.ListPending(ctx, limit)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	return rows
}

// RunContract runs the full Store contract against stores built by newStore.
func RunContract(t *testing.T, newStore Factory) {
	t.Run("SaveAndUpsert", func(t *testing.T) {
		ctx := context.Background()
		s := newStore(t)
		now := time.Now()
		if err := s.Save(ctx, rec("a", now, now)); err != nil {
			t.Fatalf("Save: %v", err)
		}
		// Re-save the same id with new fields — an idempotent enqueue must overwrite, not duplicate.
		upd := rec("a", now, now)
		upd.Attempts = 5
		upd.EventType = "order.updated"
		upd.Payload = map[string]any{"changed": true}
		upd.LastError = strptr("prior boom")
		if err := s.Save(ctx, upd); err != nil {
			t.Fatalf("Save (upsert): %v", err)
		}
		mustSize(t, ctx, s, 1)
		rows := mustList(t, ctx, s, 10)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		got := rows[0]
		if got.Attempts != 5 || got.EventType != "order.updated" {
			t.Errorf("upsert not applied: %+v", got)
		}
		// Guards the column ORDER, not just the value. Renaming project -> project_id (like
		// environment -> project before it) kept it in slot 4 of the SQLite INSERT/SELECT and in the
		// Postgres params; project_id and source are both TEXT, so a transposition between the
		// SELECT list and the Scan targets would leave every other assertion here green. Postgres
		// and Redis SKIP without their URLs set, which makes SQLite the only store whose SQL
		// actually runs in CI.
		if got.ProjectID != testProjectID {
			t.Errorf("project_id = %q, want %q (column order may be transposed)", got.ProjectID, testProjectID)
		}
		if got.LastError == nil || *got.LastError != "prior boom" {
			t.Errorf("last_error = %v, want prior boom", got.LastError)
		}
		if v, ok := got.Payload["changed"].(bool); !ok || !v {
			t.Errorf("payload = %v, want {changed:true}", got.Payload)
		}
	})

	t.Run("EmptyProjectIDRoundTrips", func(t *testing.T) {
		// An unset ProjectID means "the workspace's default project" — the id is opaque and per-workspace,
		// so there is no client-side sentinel to store. The SQL stores persist it as NULL (the
		// column is nullable) and must hand it back as the empty string, NOT as some literal.
		ctx := context.Background()
		s := newStore(t)
		now := time.Now()
		r := rec("no-project", now, now)
		r.ProjectID = ""
		if err := s.Save(ctx, r); err != nil {
			t.Fatalf("Save with empty ProjectID: %v", err)
		}
		rows := mustList(t, ctx, s, 10)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		if rows[0].ProjectID != "" {
			t.Errorf("project_id = %q, want \"\" (default project stays unset)", rows[0].ProjectID)
		}
	})

	t.Run("ListOrderingDueFilterAndLimit", func(t *testing.T) {
		ctx := context.Background()
		s := newStore(t)
		now := time.Now()
		// Three due records, created oldest → newest, saved out of order.
		if err := s.Save(ctx, rec("mid", now.Add(-2*time.Second), now.Add(-time.Second))); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(ctx, rec("old", now.Add(-3*time.Second), now.Add(-time.Second))); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(ctx, rec("new", now.Add(-1*time.Second), now.Add(-time.Second))); err != nil {
			t.Fatal(err)
		}
		// One record not yet due — must never appear in ListPending.
		if err := s.Save(ctx, rec("future", now.Add(-4*time.Second), now.Add(time.Hour))); err != nil {
			t.Fatal(err)
		}

		rows := mustList(t, ctx, s, 10)
		if len(rows) != 3 {
			t.Fatalf("due rows = %d, want 3 (future excluded)", len(rows))
		}
		gotOrder := []string{rows[0].ID, rows[1].ID, rows[2].ID}
		wantOrder := []string{"old", "mid", "new"}
		for i := range wantOrder {
			if gotOrder[i] != wantOrder[i] {
				t.Fatalf("order = %v, want %v (oldest first)", gotOrder, wantOrder)
			}
		}
		// Limit caps the batch to the oldest N.
		capped := mustList(t, ctx, s, 2)
		if len(capped) != 2 || capped[0].ID != "old" || capped[1].ID != "mid" {
			t.Fatalf("limited = %v, want [old mid]", ids(capped))
		}
	})

	t.Run("MarkSentRemoves", func(t *testing.T) {
		ctx := context.Background()
		s := newStore(t)
		now := time.Now()
		if err := s.Save(ctx, rec("x", now.Add(-2*time.Second), now)); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(ctx, rec("y", now.Add(-1*time.Second), now)); err != nil {
			t.Fatal(err)
		}
		mustSize(t, ctx, s, 2)
		if err := s.MarkSent(ctx, "x"); err != nil {
			t.Fatalf("MarkSent: %v", err)
		}
		mustSize(t, ctx, s, 1)
		rows := mustList(t, ctx, s, 10)
		if len(rows) != 1 || rows[0].ID != "y" {
			t.Fatalf("after MarkSent(x), pending = %v, want [y]", ids(rows))
		}
		if err := s.MarkSent(ctx, "y"); err != nil {
			t.Fatalf("MarkSent: %v", err)
		}
		mustSize(t, ctx, s, 0)
		// MarkSent on a missing id is a no-op (not an error).
		if err := s.MarkSent(ctx, "gone"); err != nil {
			t.Errorf("MarkSent(missing) = %v, want nil", err)
		}
	})

	t.Run("MarkFailedReschedulesAndBumps", func(t *testing.T) {
		ctx := context.Background()
		s := newStore(t)
		now := time.Now()
		if err := s.Save(ctx, rec("r", now.Add(-2*time.Second), now)); err != nil {
			t.Fatal(err)
		}
		// Fail once, still due (retry time in the past): attempts bumped, error + reschedule persisted.
		if err := s.MarkFailed(ctx, "r", strptr("boom"), 1, now.Add(-time.Second)); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
		rows := mustList(t, ctx, s, 10)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		if rows[0].Attempts != 1 {
			t.Errorf("attempts = %d, want 1", rows[0].Attempts)
		}
		if rows[0].LastError == nil || *rows[0].LastError != "boom" {
			t.Errorf("last_error = %v, want boom", rows[0].LastError)
		}
		// Fail again, rescheduled into the future: excluded from the due batch but still buffered.
		if err := s.MarkFailed(ctx, "r", strptr("boom2"), 2, now.Add(time.Hour)); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
		if rows := mustList(t, ctx, s, 10); len(rows) != 0 {
			t.Fatalf("pending = %v, want [] (rescheduled to future)", ids(rows))
		}
		mustSize(t, ctx, s, 1)
		// MarkFailed on a missing id is a no-op (not an error).
		if err := s.MarkFailed(ctx, "gone", strptr("x"), 1, now); err != nil {
			t.Errorf("MarkFailed(missing) = %v, want nil", err)
		}
	})

	t.Run("Size", func(t *testing.T) {
		ctx := context.Background()
		s := newStore(t)
		mustSize(t, ctx, s, 0)
		now := time.Now()
		if err := s.Save(ctx, rec("a", now, now)); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(ctx, rec("b", now, now)); err != nil {
			t.Fatal(err)
		}
		mustSize(t, ctx, s, 2)
	})
}

func ids(rows []webhooks.Record) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}
