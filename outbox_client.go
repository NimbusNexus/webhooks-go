package webhookd

import (
	"context"
	"net/http"
	"time"
)

// WithStore configures the durable outbox store. When set, Enqueue / Drain / StartDrainer become
// usable; omit it to use only the live Publish.
func WithStore(store Store) Option {
	return func(c *Client) { c.store = store }
}

// WithMaxAttempts sets the delivery attempts a record gets before Drain parks it dead (default 10).
func WithMaxAttempts(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.maxAttempts = n
		}
	}
}

// WithDrainBatchLimit sets how many records a single Drain pulls from the store (default 100).
func WithDrainBatchLimit(n int) Option {
	return func(c *Client) {
		if n > 0 {
			c.drainBatchLimit = n
		}
	}
}

// WithOnDead registers a callback invoked once when Drain parks a record dead (retry budget
// exhausted). The record passed carries the final attempts count and last error.
func WithOnDead(fn func(Record)) Option {
	return func(c *Client) { c.onDead = fn }
}

// EnqueueOptions carries the optional arguments to Client.Enqueue. Environment defaults to "prod"
// and Application to "default" when empty. Source is stored only when non-nil. IdempotencyKey, when
// non-empty, becomes the record id (and the Idempotency-Key on every later Drain send); otherwise a
// fresh UUID v4 is generated.
type EnqueueOptions struct {
	Environment    string
	Application    string
	Source         *string
	IdempotencyKey string
}

// DrainOptions overrides the client defaults for a single Drain. A zero field falls back to the
// client's configured value.
type DrainOptions struct {
	BatchLimit  int
	MaxAttempts int
}

// DrainResult summarises one Drain call.
type DrainResult struct {
	// Sent is the number of records delivered (2xx) and marked sent this drain.
	Sent int
	// Failed is the number of records that failed this drain (rescheduled or newly parked dead).
	Failed int
	// Remaining is the number of records still buffered afterwards (Store.Size).
	Remaining int
}

// Enqueue durably buffers one event and returns its id WITHOUT any network call.
//
// The id is opts.IdempotencyKey (if non-empty) or a fresh UUID v4; it becomes the Idempotency-Key on
// every later Drain send, so re-draining after a crash never double-publishes. Requires a store
// (WithStore).
func (c *Client) Enqueue(ctx context.Context, eventType string, payload map[string]any, opts *EnqueueOptions) (string, error) {
	if c.store == nil {
		return "", &Error{Message: "enqueue requires a store — construct New(..., WithStore(...))"}
	}
	env, app := "prod", "default"
	var source *string
	id := ""
	if opts != nil {
		if opts.Environment != "" {
			env = opts.Environment
		}
		if opts.Application != "" {
			app = opts.Application
		}
		source = opts.Source
		id = opts.IdempotencyKey
	}
	if id == "" {
		id = newUUIDv4()
	}
	now := time.Now()
	record := Record{
		ID:            id,
		EventType:     eventType,
		Payload:       payload,
		Environment:   env,
		Application:   app,
		Source:        source,
		CreatedAt:     now,
		Attempts:      0,
		LastError:     nil,
		NextAttemptAt: now,
	}
	if err := c.store.Save(ctx, record); err != nil {
		return "", err
	}
	return id, nil
}

// Drain sends buffered records to webhookd. It pulls a due batch (oldest first) and POSTs each to
// /v1/events with header Idempotency-Key = record.ID (so a re-drain after a crash never
// double-publishes — webhookd dedupes). On 2xx the record is marked sent; on error attempts is
// bumped and the record is rescheduled with capped exponential backoff, or parked dead once attempts
// reaches maxAttempts (the WithOnDead hook fires). Requires a store (WithStore).
func (c *Client) Drain(ctx context.Context, opts *DrainOptions) (DrainResult, error) {
	if c.store == nil {
		return DrainResult{}, &Error{Message: "drain requires a store — construct New(..., WithStore(...))"}
	}
	batch, maxAttempts := c.drainBatchLimit, c.maxAttempts
	if opts != nil {
		if opts.BatchLimit > 0 {
			batch = opts.BatchLimit
		}
		if opts.MaxAttempts > 0 {
			maxAttempts = opts.MaxAttempts
		}
	}

	rows, err := c.store.ListPending(ctx, batch)
	if err != nil {
		return DrainResult{}, err
	}

	sent, failed := 0, 0
	for _, record := range rows {
		body := map[string]any{
			"event_type":  record.EventType,
			"payload":     record.Payload,
			"environment": record.Environment,
			"application": record.Application,
		}
		if record.Source != nil {
			body["source"] = *record.Source
		}

		resp, reqErr := c.do(ctx, http.MethodPost, "/v1/events", body, nil, map[string]string{"Idempotency-Key": record.ID})
		if reqErr != nil {
			attempts := record.Attempts + 1
			msg := reqErr.Error()
			if attempts >= maxAttempts {
				if e := c.store.MarkFailed(ctx, record.ID, &msg, attempts, deadNextAttempt); e != nil {
					return DrainResult{}, e
				}
				if c.onDead != nil {
					dead := record
					dead.Attempts = attempts
					dead.LastError = &msg
					dead.NextAttemptAt = deadNextAttempt
					c.onDead(dead)
				}
			} else {
				next := time.Now().Add(backoff(record.Attempts))
				if e := c.store.MarkFailed(ctx, record.ID, &msg, attempts, next); e != nil {
					return DrainResult{}, e
				}
			}
			failed++
			continue
		}
		drain(resp)
		if e := c.store.MarkSent(ctx, record.ID); e != nil {
			return DrainResult{}, e
		}
		sent++
	}

	remaining, err := c.store.Size(ctx)
	if err != nil {
		return DrainResult{}, err
	}
	return DrainResult{Sent: sent, Failed: failed, Remaining: remaining}, nil
}

// StartDrainer launches a background goroutine that calls Drain every interval until StopDrainer (or
// ctx cancellation). A per-drain error is swallowed so a transient outage never kills the loop.
// Idempotent — a second call while one is running is a no-op. Requires a store (WithStore).
func (c *Client) StartDrainer(ctx context.Context, interval time.Duration, opts *DrainOptions) {
	c.drainerMu.Lock()
	defer c.drainerMu.Unlock()
	if c.store == nil || c.drainerCancel != nil {
		return
	}
	dctx, cancel := context.WithCancel(ctx)
	c.drainerCancel = cancel
	c.drainerWG.Add(1)
	go func() {
		defer c.drainerWG.Done()
		for {
			_, _ = c.Drain(dctx, opts)
			select {
			case <-dctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()
}

// StopDrainer signals the background drainer to stop and waits for it to exit. Safe to call when
// none is running.
func (c *Client) StopDrainer() {
	c.drainerMu.Lock()
	cancel := c.drainerCancel
	c.drainerCancel = nil
	c.drainerMu.Unlock()
	if cancel != nil {
		cancel()
		c.drainerWG.Wait()
	}
}
