package webhookd_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	webhookd "github.com/NimbusNexus/webhookd-go"
	"github.com/NimbusNexus/webhookd-go/store/storetest"
)

// -- shared store contract, run fully for the stdlib-only stores ---------------

func TestMemoryStoreContract(t *testing.T) {
	storetest.RunContract(t, func(t *testing.T) webhookd.Store {
		s := webhookd.NewMemoryStore()
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestFileStoreContract(t *testing.T) {
	storetest.RunContract(t, func(t *testing.T) webhookd.Store {
		s, err := webhookd.NewFileStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewFileStore: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestFileStoreSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s1, err := webhookd.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	now := time.Now()
	if err := s1.Save(ctx, webhookd.Record{ID: "keep", EventType: "e", Payload: map[string]any{"k": 1}, Environment: "prod", Application: "default", CreatedAt: now, NextAttemptAt: now}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	_ = s1.Close()

	s2, err := webhookd.NewFileStore(dir) // a fresh store over the same directory
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	rows, err := s2.ListPending(ctx, 10)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "keep" {
		t.Fatalf("after reopen pending = %v, want [keep]", rows)
	}
}

// -- client enqueue -> drain flow against a fake transport ---------------------

// recorder is a concurrency-safe capture of what the fake webhookd server saw.
type recorder struct {
	mu     sync.Mutex
	count  int
	keys   []string
	failFn func(n int) bool // returns true to fail (500) the nth (1-based) request
}

func (rc *recorder) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rc.mu.Lock()
		rc.count++
		n := rc.count
		rc.keys = append(rc.keys, r.Header.Get("Idempotency-Key"))
		fail := rc.failFn != nil && rc.failFn(n)
		rc.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"evt_1","event_uid":"u1","event_type":"order.created","application":"default","environment":"prod","deliveries_created":1,"source":null}`))
	}
}

func (rc *recorder) snapshot() (int, []string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	keys := append([]string(nil), rc.keys...)
	return rc.count, keys
}

func newServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestEnqueueWritesNoNetworkAndDrainSendsEachWithIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	rc := &recorder{}
	srv := newServer(t, rc.handler(t))
	store := webhookd.NewMemoryStore()
	c := webhookd.New(srv.URL, "whsk_x", webhookd.WithStore(store), webhookd.WithMaxRetries(0))

	wantIDs := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		id, err := c.Enqueue(ctx, "order.created", map[string]any{"n": i}, nil)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if id == "" {
			t.Fatal("Enqueue returned empty id")
		}
		wantIDs = append(wantIDs, id)
	}

	// Enqueue must not touch the network, and all three must be durably buffered.
	if n, _ := rc.snapshot(); n != 0 {
		t.Fatalf("enqueue made %d requests, want 0 (write-first)", n)
	}
	if sz, _ := store.Size(ctx); sz != 3 {
		t.Fatalf("store size = %d, want 3", sz)
	}

	res, err := c.Drain(ctx, nil)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if res.Sent != 3 || res.Failed != 0 || res.Remaining != 0 {
		t.Fatalf("drain result = %+v, want {3 0 0}", res)
	}

	n, gotKeys := rc.snapshot()
	if n != 3 {
		t.Fatalf("drain made %d requests, want 3", n)
	}
	sort.Strings(gotKeys)
	sort.Strings(wantIDs)
	for i := range wantIDs {
		if gotKeys[i] != wantIDs[i] {
			t.Fatalf("Idempotency-Keys = %v, want %v (each record's id)", gotKeys, wantIDs)
		}
	}
}

func TestFailingTransportBumpsAttemptsAndLaterDrainRetriesSameID(t *testing.T) {
	ctx := context.Background()
	rc := &recorder{failFn: func(n int) bool { return n == 1 }} // only the first send fails
	srv := newServer(t, rc.handler(t))
	store := webhookd.NewMemoryStore()
	c := webhookd.New(srv.URL, "whsk_x", webhookd.WithStore(store), webhookd.WithMaxRetries(0))

	id, err := c.Enqueue(ctx, "order.created", map[string]any{"n": 1}, nil)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	res, err := c.Drain(ctx, nil)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if res.Sent != 0 || res.Failed != 1 || res.Remaining != 1 {
		t.Fatalf("first drain = %+v, want {0 1 1} (failed, not sent)", res)
	}

	// The record is rescheduled with backoff; wait for it to come due, then confirm attempts bumped.
	time.Sleep(300 * time.Millisecond)
	rows, err := store.ListPending(ctx, 10)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("pending after failure = %v, want [%s]", rows, id)
	}
	if rows[0].Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (bumped)", rows[0].Attempts)
	}
	if rows[0].LastError == nil {
		t.Fatal("last_error not recorded on failure")
	}

	res2, err := c.Drain(ctx, nil)
	if err != nil {
		t.Fatalf("second Drain: %v", err)
	}
	if res2.Sent != 1 || res2.Remaining != 0 {
		t.Fatalf("second drain = %+v, want {1 _ 0}", res2)
	}

	// The retry must carry the SAME idempotency key (record id) both times.
	_, keys := rc.snapshot()
	if len(keys) != 2 || keys[0] != id || keys[1] != id {
		t.Fatalf("idempotency keys = %v, want [%s %s]", keys, id, id)
	}
}

func TestMaxAttemptsDeadLettersAndFiresOnDead(t *testing.T) {
	ctx := context.Background()
	rc := &recorder{failFn: func(int) bool { return true }} // every send fails
	srv := newServer(t, rc.handler(t))
	store := webhookd.NewMemoryStore()

	var mu sync.Mutex
	var dead []webhookd.Record
	c := webhookd.New(srv.URL, "whsk_x",
		webhookd.WithStore(store),
		webhookd.WithMaxRetries(0),
		webhookd.WithMaxAttempts(2),
		webhookd.WithOnDead(func(r webhookd.Record) {
			mu.Lock()
			dead = append(dead, r)
			mu.Unlock()
		}),
	)

	id, err := c.Enqueue(ctx, "order.created", map[string]any{"n": 1}, nil)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Drain #1: attempts -> 1 (< 2), rescheduled, not dead yet.
	if _, err := c.Drain(ctx, nil); err != nil {
		t.Fatalf("drain 1: %v", err)
	}
	mu.Lock()
	deadCount := len(dead)
	mu.Unlock()
	if deadCount != 0 {
		t.Fatalf("onDead fired %d times after 1 attempt, want 0", deadCount)
	}

	// Drain #2: attempts -> 2 (== maxAttempts), dead-lettered + hook fires.
	time.Sleep(300 * time.Millisecond)
	res, err := c.Drain(ctx, nil)
	if err != nil {
		t.Fatalf("drain 2: %v", err)
	}
	if res.Remaining != 1 {
		t.Fatalf("remaining = %d, want 1 (dead record stays buffered)", res.Remaining)
	}
	mu.Lock()
	got := append([]webhookd.Record(nil), dead...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("onDead fired %d times, want 1", len(got))
	}
	if got[0].ID != id || got[0].Attempts != 2 {
		t.Fatalf("dead record = %+v, want id=%s attempts=2", got[0], id)
	}

	// A parked-dead record is never retried again.
	pending, err := store.ListPending(ctx, 10)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("dead record still pending: %v", pending)
	}
	res3, err := c.Drain(ctx, nil)
	if err != nil {
		t.Fatalf("drain 3: %v", err)
	}
	if res3.Sent != 0 || res3.Failed != 0 {
		t.Fatalf("drain 3 = %+v, want {0 0 _} (nothing due)", res3)
	}
}

func TestBackgroundDrainerShipsBufferedRecords(t *testing.T) {
	ctx := context.Background()
	rc := &recorder{}
	srv := newServer(t, rc.handler(t))
	store := webhookd.NewMemoryStore()
	c := webhookd.New(srv.URL, "whsk_x", webhookd.WithStore(store), webhookd.WithMaxRetries(0))

	if _, err := c.Enqueue(ctx, "order.created", map[string]any{"n": 1}, nil); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	c.StartDrainer(ctx, 20*time.Millisecond, nil)
	// A second StartDrainer while one runs is a no-op.
	c.StartDrainer(ctx, 20*time.Millisecond, nil)
	defer c.StopDrainer()

	deadline := time.Now().Add(3 * time.Second)
	for {
		sz, err := store.Size(ctx)
		if err != nil {
			t.Fatalf("Size: %v", err)
		}
		if sz == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background drainer did not empty the store within 3s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.StopDrainer()
	if n, _ := rc.snapshot(); n < 1 {
		t.Fatalf("drainer made %d requests, want >= 1", n)
	}
}
