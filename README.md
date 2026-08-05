# webhookd (Go)

Official Go SDK for **NimbusNexus Webhooks** — publish events, manage your endpoints / keys /
deliveries, and verify the webhooks you receive. The core client is zero-dependency (standard library
only); Go ≥ 1.25 (the optional outbox store drivers raise the module's minimum — see below).

```sh
go get github.com/NimbusNexus/webhookd-go
```

```go
import webhooks "github.com/NimbusNexus/webhookd-go"
```

The module path's last element is `webhookd-go`, but the package is named `webhookd` — import it with
the explicit `webhookd` alias shown above.

## Verify an incoming webhook (subscribers)

When webhookd delivers a webhook it signs the body with your endpoint's signing secret. **Always
verify the signature** before trusting the payload — it proves the request really came from webhookd
and wasn't tampered with or replayed. Pass the **raw** request body bytes (do not re-serialize the
JSON, or the signature won't match).

```go
package main

import (
	"io"
	"net/http"
	"strconv"

	webhooks "github.com/NimbusNexus/webhookd-go"
)

const signingSecret = "whsec_…" // the endpoint's signing secret (shown once on create)

func handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body) // RAW bytes — do not decode then re-encode before verifying
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}

	var opts *webhookd.VerifyOptions
	if raw := r.Header.Get("X-Webhook-Timestamp"); raw != "" {
		ts, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil {
			http.Error(w, "invalid signature", http.StatusBadRequest) // malformed timestamp
			return
		}
		opts = &webhookd.VerifyOptions{Timestamp: &ts} // enforces the 300s replay window
	}

	if !webhookd.Verify(signingSecret, body, r.Header.Get("X-Webhook-Signature"), opts) {
		http.Error(w, "invalid signature", http.StatusBadRequest) // forged, tampered, or replayed
		return
	}

	// ... trust the payload — do your work idempotently, then ack with any 2xx ...
	w.WriteHeader(http.StatusOK)
}
```

`X-Webhook-Signature` may carry a single token, or several comma-separated tokens during a
signing-secret rotation — `Verify` accepts the request if **any** token matches, using a
constant-time comparison. See [`examples/receiver`](./examples/receiver) for a full runnable server.

`Sign(secret, body, timestamp)` produces the same `sha256=<hex>` signature the server sends; it's
mainly useful for tests — subscribers call `Verify`.

## Publish an event (producers)

```go
package main

import (
	"context"
	"errors"
	"fmt"

	webhooks "github.com/NimbusNexus/webhookd-go"
)

func main() {
	wh := webhookd.New("https://webhooks.example.com", "whsk_…")

	event, err := wh.Publish(context.Background(), "order.created",
		map[string]any{"order_id": "ord_123", "total": 4200},
		&webhookd.PublishOptions{IdempotencyKey: "order-123"}, // makes the publish safe to retry
	)
	if err != nil {
		var apiErr *webhookd.APIError
		if errors.As(err, &apiErr) {
			fmt.Println(apiErr.StatusCode, apiErr.Code, apiErr.Message) // the {error:{code,message}} envelope
		}
		return
	}
	fmt.Println(event.EventUID, event.DeliveriesCreated)
}
```

`New` accepts functional options: `WithTimeout(time.Duration)` (default 10s), `WithMaxRetries(int)`
(default 2), and `WithHTTPClient(*http.Client)`. Transient failures (connection errors, `429`, `5xx`)
are retried with capped exponential backoff (a `429` honours `Retry-After`); other non-2xx responses
return an `*APIError` carrying the stable `{error:{code,message}}` envelope. Every method takes a
`context.Context` first.

### Projects

A project is addressed by its **id** — an opaque, per-workspace string like `prj_3f9a…`. There are no
project slugs. `PublishOptions`, `EnqueueOptions`, `CreateEndpointOptions` and `ListEndpointsOptions`
all take an optional `ProjectID`; **leave it empty to target the workspace's default project** — the SDK
then omits the field entirely and the server resolves it. There is no client-side stand-in for "the
default project", so don't invent one.

```go
wh.Publish(ctx, "order.created", payload, nil)                                    // default project
wh.Publish(ctx, "order.created", payload, &webhookd.PublishOptions{ProjectID: "prj_3f9a"})
```

## Outbox / durable buffering (producers)

`Publish` calls webhookd synchronously — if webhookd is unreachable it returns an error and the event
is lost. The **write-first outbox** decouples the two: `Enqueue` durably persists the event to a
pluggable `Store` and returns IMMEDIATELY (no network); `Drain` (or a background drainer) ships the
buffered events later. Every send carries `Idempotency-Key = record.ID`, so a re-drain after a crash
or a lost response never double-publishes — webhookd dedupes. Delivery is **at-least-once**: nothing
is lost while webhookd is down. The live `Publish` above is unchanged.

```go
package main

import (
	"context"
	"log"
	"time"

	webhooks "github.com/NimbusNexus/webhookd-go"
	"github.com/NimbusNexus/webhookd-go/store/sqlite" // pulls in the SQLite driver
)

func main() {
	// 1. Configure a durable store (survives process restarts).
	store, err := sqlite.Open("outbox.db")
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	wh := webhookd.New("https://webhooks.example.com", "whsk_…",
		webhookd.WithStore(store),
		webhookd.WithMaxAttempts(10), // retry budget before a record is parked dead (default 10)
		webhookd.WithOnDead(func(r webhookd.Record) {
			log.Printf("dead-lettered %s: %v", r.ID, r.LastError)
		}),
	)
	ctx := context.Background()

	// 2. Enqueue instead of Publish — writes to the store and returns at once, NO network call.
	id, _ := wh.Enqueue(ctx, "order.created",
		map[string]any{"order_id": "ord_123", "total": 4200},
		&webhookd.EnqueueOptions{IdempotencyKey: "order-123"}) // id defaults to a fresh UUID v4
	_ = id

	// 3a. Drain on demand — returns DrainResult{Sent, Failed, Remaining}:
	wh.Drain(ctx, nil)

	// 3b. …or run a background drainer that calls Drain every 5s until StopDrainer.
	wh.StartDrainer(ctx, 5*time.Second, nil)
	// ... your app keeps enqueuing; the drainer ships in the background ...
	wh.StopDrainer() // signals the goroutine and waits for it to exit
}
```

**Idempotency guarantee.** The id returned by `Enqueue` is the `IdempotencyKey` you pass (or a
generated UUID v4) and becomes the `Idempotency-Key` header on every delivery attempt for that record.
If the process crashes after a send but before the response is recorded, the next `Drain` re-sends
with the *same* key and webhookd returns the original event without re-fanning-out. A record that
keeps failing is retried with capped exponential backoff up to `WithMaxAttempts` (default 10), then
flagged **dead** (never retried again, kept buffered) and handed to the optional `WithOnDead` hook.

**Built-in stores** — pass one to `webhookd.WithStore(...)`:

| Store | Durable? | Extra needed |
| --- | --- | --- |
| `webhookd.NewMemoryStore()` | No (in-process) | — (stdlib) |
| `webhookd.NewFileStore(dir)` | Yes (per-record JSON, atomic write+rename) | — (stdlib) |
| `sqlite.Open(path)` | Yes (transactional) | `store/sqlite` → `modernc.org/sqlite` (pure Go, no cgo) |
| `redis.Open(redis.Options{URL})` | Yes (sorted-set + hash under a key prefix) | `store/redis` → `github.com/redis/go-redis/v9` |
| `postgres.Open(ctx, postgres.Options{ConnString})` | Yes (`webhookd_outbox` table, name configurable) | `store/postgres` → `github.com/jackc/pgx/v5` |

A buffered `Record` carries an optional `ProjectID`; an empty one means the workspace's default project
and is persisted as SQL `NULL` (the `project_id` column is nullable), so `Drain` omits `project_id`
from the publish body and the server resolves the project. `sqlite.Open` / `postgres.Open` upgrade an
outbox written by an older SDK in place — the pre-existing `project` slug column is renamed to
`project_id` and relaxed to nullable, with the legacy `"default"` sentinel becoming `NULL` — so no
buffered event is stranded.

The core `webhookd` package stays stdlib-only. The three driver stores live in **subpackages** under
[`store/`](./store) (`store/sqlite`, `store/redis`, `store/postgres`); their third-party driver
dependencies are only compiled into your binary if you actually import the subpackage, so a consumer
who only uses the core SDK never pulls them in.

## Manage endpoints, keys & deliveries (operators)

The same `Client` wraps the control-plane API — register receivers, mint keys, and drain the
dead-letter queue from code (needs an **admin**-scoped key). Management methods return typed structs
(`*Endpoint`, `*ApiKey`, `*Delivery`); list methods return a `*Page[T]` (`Items` + `NextOffset`);
`DeleteEndpoint` / `RevokeAPIKey` return just an `error` (a `204`).

```go
ctx := context.Background()
wh := webhookd.New("https://webhooks.example.com", "whsk_admin_…")

// --- Endpoints ---------------------------------------------------------------
// Create a receiver — its signing secret is in the response exactly once, so persist it now.
ep, _ := wh.CreateEndpoint(ctx, "https://your-app.example/webhooks", &webhookd.CreateEndpointOptions{
	Subscriptions: []webhookd.Subscription{{MatchKind: "prefix", Pattern: "order."}},
	Description:   ptr("orders service"),
})
endpointID, signingSecret := ep.Id, *ep.Secret

wh.ListEndpoints(ctx, nil)                                                  // default project
wh.ListEndpoints(ctx, &webhookd.ListEndpointsOptions{ProjectID: "prj_3f9a"}) // *Page[Endpoint]
wh.GetEndpoint(ctx, endpointID)

// PATCH — send only the keys you want to change (an omitted key is unchanged, an explicit nil clears):
wh.UpdateEndpoint(ctx, endpointID, webhookd.Patch{"max_attempts": 10, "status": "disabled"})

wh.RotateEndpointSecret(ctx, endpointID) // returns the new secret, once
wh.EnableEndpoint(ctx, endpointID)       // recover an auto-disabled endpoint
wh.DeleteEndpoint(ctx, endpointID)       // -> error only (204)

// --- API keys ----------------------------------------------------------------
key, _ := wh.CreateAPIKey(ctx, &webhookd.CreateAPIKeyOptions{Name: "ci-publisher", Scope: "publish"})
fmt.Println(*key.Key)          // shown once
wh.RevokeAPIKey(ctx, key.Id)   // -> error only (204)

// --- Deliveries / dead-letter recovery ---------------------------------------
dead, _ := wh.ListDeliveries(ctx, &webhookd.ListDeliveriesOptions{Status: "dead"})
for _, d := range dead.Items {
	wh.Redeliver(ctx, d.Id)
}
```

Optional outbound scalar fields (e.g. `Description`, `MaxAttempts`) are pointers, so only the ones you
set are serialized. `ptr` above is a tiny helper — `func ptr[T any](v T) *T { return &v }`.

## CLI

The module ships a `webhookd` command that wraps this client and speaks the same v1 API.

```sh
go install github.com/NimbusNexus/webhookd-go/cmd/webhookd@latest
```

Configuration (base URL + API key) is resolved in order: the `--url` / `--api-key` flags, then the
`WEBHOOKD_URL` / `WEBHOOKD_API_KEY` environment variables, then `~/.webhookd/config.json` (written by
`webhookd configure`).

```sh
webhookd configure                                   # save base URL + API key to ~/.webhookd/config.json
webhookd publish order.created --data '{"id":123}' --idempotency-key order-123

webhookd endpoints create --url https://your-app.example/webhooks --subscribe prefix:order.
webhookd endpoints list                    # the workspace's default project
webhookd endpoints list --project-id prj_3f9a
webhookd endpoints get <id>
webhookd endpoints update <id> --set max_attempts=10 --set status=disabled  # values are JSON-coerced
webhookd endpoints rotate-secret <id>
webhookd endpoints enable <id>
webhookd endpoints delete <id>

webhookd keys create --name ci-publisher --scope publish --expires-in-days 90
webhookd keys revoke <id>

webhookd deliveries list --status dead
webhookd deliveries redeliver <id>

# Verify a webhook — the raw body is read from stdin; prints "ok"/"failed" and exits 0/1:
webhookd verify --secret whsec_… --signature "$SIG" --timestamp "$TS" < body.json

webhookd version
```

Successful results print as indented JSON to stdout; errors go to stderr and the process exits
non-zero (an API error renders as `code: message`).

## Examples

[`examples/receiver`](./examples/receiver) is a runnable `net/http` server that verifies incoming
webhooks; [`examples/publish`](./examples/publish) publishes one event. Both read their configuration
from environment variables:

```sh
WEBHOOKD_SIGNING_SECRET=whsec_… go run ./examples/receiver           # a verifying receiver on :8080
WEBHOOKD_URL=… WEBHOOKD_API_KEY=whsk_… go run ./examples/publish     # publish one event
```

## Develop

```sh
gofmt -l .        # must print nothing
go vet ./...
go build ./...
go test ./...     # includes the shared cross-language signature vectors in testdata/
```

The outbox stores share one contract test. Memory/File/SQLite run fully with no server. The Redis and
Postgres store contracts (`store/redis`, `store/postgres`) run the **same** contract but skip cleanly
unless you point them at a server: `WEBHOOKD_TEST_REDIS_URL` (e.g. `redis://localhost:6379`) and
`WEBHOOKD_TEST_PG_URL` (e.g. `postgres://postgres:postgres@localhost:5432/webhookd_test`).
