package webhookd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func intPtr(n int) *int       { return &n }
func strPtr(s string) *string { return &s }
func bg() context.Context     { return context.Background() }

// newTestClient spins up an httptest server driven by handler and returns a Client pointed at it.
func newTestClient(t *testing.T, handler http.HandlerFunc, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", "whsk_x", opts...) // trailing slash exercises TrimRight
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return m
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

var okEvent = map[string]any{
	"id":                 "evt_db",
	"event_uid":          "u1",
	"event_type":         "order.created",
	"application":        "default",
	"environment":        "prod",
	"deliveries_created": 2,
	"source":             nil,
}

var okEndpoint = map[string]any{
	"id":            "ep_1",
	"url":           "https://sub.example.com/hook",
	"environment":   "prod",
	"application":   "default",
	"status":        "enabled",
	"secret":        "whsec_shown_once",
	"subscriptions": []any{map[string]any{"match_kind": "prefix", "pattern": "order."}},
}

// -- publish ------------------------------------------------------------------

func TestPublishSuccessAndRequestShape(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/events" {
			t.Errorf("path = %s, want /v1/events", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer whsk_x" {
			t.Errorf("Authorization = %q, want Bearer whsk_x", got)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		body := decodeBody(t, r)
		if body["event_type"] != "order.created" {
			t.Errorf("event_type = %v", body["event_type"])
		}
		if body["environment"] != "prod" || body["application"] != "default" {
			t.Errorf("defaults not applied: %v", body)
		}
		payload, _ := body["payload"].(map[string]any)
		if payload["id"] != float64(1) {
			t.Errorf("payload = %v", body["payload"])
		}
		writeJSON(t, w, http.StatusCreated, okEvent)
	})

	ev, err := c.Publish(bg(), "order.created", map[string]any{"id": 1}, nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if ev.EventUID != "u1" || ev.DeliveriesCreated != 2 {
		t.Errorf("event = %+v", ev)
	}
	if ev.Source != nil {
		t.Errorf("source = %v, want nil", *ev.Source)
	}
}

func TestPublishSetsIdempotencyKeyAndSource(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Idempotency-Key"); got != "order-123" {
			t.Errorf("Idempotency-Key = %q, want order-123", got)
		}
		body := decodeBody(t, r)
		if body["source"] != "billing" {
			t.Errorf("source = %v, want billing", body["source"])
		}
		if body["environment"] != "staging" {
			t.Errorf("environment = %v, want staging", body["environment"])
		}
		writeJSON(t, w, http.StatusCreated, okEvent)
	})

	_, err := c.Publish(bg(), "order.created", map[string]any{"id": 1}, &PublishOptions{
		Environment:    "staging",
		Source:         strPtr("billing"),
		IdempotencyKey: "order-123",
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

func TestPublishOmitsIdempotencyKeyWhenEmpty(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Idempotency-Key"]; ok {
			t.Errorf("Idempotency-Key header must be absent when unset")
		}
		body := decodeBody(t, r)
		if _, ok := body["source"]; ok {
			t.Errorf("source must be absent when unset")
		}
		writeJSON(t, w, http.StatusCreated, okEvent)
	})
	if _, err := c.Publish(bg(), "order.created", map[string]any{}, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

// -- errors + retries ---------------------------------------------------------

func TestAPIErrorEnvelope(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "validation_error", "message": "bad"},
		})
	})
	_, err := c.Publish(bg(), "order.created", map[string]any{}, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != 422 || apiErr.Code != "validation_error" || apiErr.Message != "bad" {
		t.Errorf("apiErr = %+v", apiErr)
	}
	if apiErr.Error() != "[422 validation_error] bad" {
		t.Errorf("Error() = %q", apiErr.Error())
	}
}

func TestAPIErrorNonEnvelopeBody(t *testing.T) {
	// A non-JSON (non-envelope) error body falls back to the raw text as the message.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "plain text failure")
	})
	_, err := c.GetEndpoint(bg(), "ep_1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != 400 || apiErr.Code != "error" || apiErr.Message != "plain text failure" {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

func TestRetriesOn503ThenSucceeds(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			writeJSON(t, w, http.StatusServiceUnavailable, map[string]any{
				"error": map[string]any{"code": "unavailable", "message": "x"},
			})
			return
		}
		writeJSON(t, w, http.StatusCreated, okEvent)
	}, WithMaxRetries(2))

	ev, err := c.Publish(bg(), "order.created", map[string]any{}, nil)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if calls != 2 || ev.EventUID != "u1" {
		t.Errorf("calls = %d, event = %+v", calls, ev)
	}
}

func TestRetriesOn429HonorsRetryAfter(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			writeJSON(t, w, http.StatusTooManyRequests, map[string]any{
				"error": map[string]any{"code": "rate_limited", "message": "slow down"},
			})
			return
		}
		writeJSON(t, w, http.StatusCreated, okEvent)
	}, WithMaxRetries(2))

	start := time.Now()
	if _, err := c.Publish(bg(), "order.created", map[string]any{}, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	elapsed := time.Since(start)
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	// Retry-After: 1 must dominate the 200ms default backoff.
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %v, expected >= ~1s (Retry-After honored)", elapsed)
	}
}

func TestDoesNotRetry4xx(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeJSON(t, w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"code": "not_found", "message": "no app"},
		})
	}, WithMaxRetries(2))

	_, err := c.Publish(bg(), "order.created", map[string]any{}, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
		t.Fatalf("error = %v, want 404 *APIError", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (a 404 is terminal)", calls)
	}
}

// -- endpoints ----------------------------------------------------------------

func TestCreateEndpointRequestShapeAndSecret(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/endpoints" {
			t.Errorf("%s %s, want POST /v1/endpoints", r.Method, r.URL.Path)
		}
		body := decodeBody(t, r)
		if body["url"] != "https://sub.example.com/hook" {
			t.Errorf("url = %v", body["url"])
		}
		if body["environment"] != "prod" || body["application"] != "default" {
			t.Errorf("defaults = %v", body)
		}
		if body["max_attempts"] != float64(5) {
			t.Errorf("max_attempts = %v", body["max_attempts"])
		}
		subs, _ := body["subscriptions"].([]any)
		if len(subs) != 1 {
			t.Errorf("subscriptions = %v", body["subscriptions"])
		}
		for _, k := range []string{"secret", "retry_schedule", "description", "custom_headers", "delivery_timeout_ms"} {
			if _, ok := body[k]; ok {
				t.Errorf("unset optional %q must be omitted", k)
			}
		}
		writeJSON(t, w, http.StatusCreated, okEndpoint)
	})

	ep, err := c.CreateEndpoint(bg(), "https://sub.example.com/hook", &CreateEndpointOptions{
		Subscriptions: []Subscription{{MatchKind: "prefix", Pattern: "order."}},
		MaxAttempts:   intPtr(5),
	})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if ep.Id != "ep_1" || ep.Secret == nil || *ep.Secret != "whsec_shown_once" {
		t.Errorf("endpoint = %+v", ep)
	}
}

func TestCreateEndpointOmitsSubscriptionsWhenAbsent(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeBody(t, r)
		if _, ok := body["subscriptions"]; ok {
			t.Errorf("subscriptions must be omitted when not provided")
		}
		writeJSON(t, w, http.StatusCreated, okEndpoint)
	})
	if _, err := c.CreateEndpoint(bg(), "https://sub.example.com/hook", nil); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
}

func TestListEndpointsQueryAndPage(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/endpoints" {
			t.Errorf("%s %s, want GET /v1/endpoints", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("environment") != "staging" || q.Get("offset") != "0" || q.Get("limit") != "50" {
			t.Errorf("query = %v", q.Encode())
		}
		writeJSON(t, w, http.StatusOK, map[string]any{"items": []any{okEndpoint}, "next_offset": 50})
	})

	page, err := c.ListEndpoints(bg(), &ListEndpointsOptions{Environment: "staging", Limit: intPtr(50)})
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Id != "ep_1" {
		t.Errorf("items = %+v", page.Items)
	}
	if page.NextOffset == nil || *page.NextOffset != 50 {
		t.Errorf("next_offset = %v", page.NextOffset)
	}
}

func TestListEndpointsDefaultsEnvironmentAndOffset(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("environment") != "prod" || q.Get("offset") != "0" {
			t.Errorf("query = %v", q.Encode())
		}
		if _, ok := q["limit"]; ok {
			t.Errorf("limit must be omitted when unset")
		}
		writeJSON(t, w, http.StatusOK, map[string]any{"items": []any{}, "next_offset": nil})
	})
	page, err := c.ListEndpoints(bg(), nil)
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if page.NextOffset != nil {
		t.Errorf("next_offset = %v, want nil", page.NextOffset)
	}
}

func TestGetEndpointPath(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/endpoints/ep_1" {
			t.Errorf("%s %s, want GET /v1/endpoints/ep_1", r.Method, r.URL.Path)
		}
		writeJSON(t, w, http.StatusOK, okEndpoint)
	})
	ep, err := c.GetEndpoint(bg(), "ep_1")
	if err != nil {
		t.Fatalf("GetEndpoint: %v", err)
	}
	if ep.Id != "ep_1" {
		t.Errorf("id = %s", ep.Id)
	}
}

func TestUpdateEndpointSendsPatchVerbatim(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/v1/endpoints/ep_1" {
			t.Errorf("%s %s, want PATCH /v1/endpoints/ep_1", r.Method, r.URL.Path)
		}
		body := decodeBody(t, r)
		// explicit null clears (present, nil); provided key present; omitted absent
		val, ok := body["description"]
		if !ok || val != nil {
			t.Errorf("description = %v (present=%v), want explicit null", val, ok)
		}
		if body["max_attempts"] != float64(10) {
			t.Errorf("max_attempts = %v", body["max_attempts"])
		}
		if _, ok := body["url"]; ok {
			t.Errorf("omitted key url must be absent")
		}
		writeJSON(t, w, http.StatusOK, okEndpoint)
	})

	_, err := c.UpdateEndpoint(bg(), "ep_1", Patch{"description": nil, "max_attempts": 10})
	if err != nil {
		t.Fatalf("UpdateEndpoint: %v", err)
	}
}

func TestDeleteEndpointHandles204(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/endpoints/ep_1" {
			t.Errorf("%s %s, want DELETE /v1/endpoints/ep_1", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent) // no body
	})
	if err := c.DeleteEndpoint(bg(), "ep_1"); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
}

func TestRotateEndpointSecret(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/endpoints/ep_1/rotate-secret" {
			t.Errorf("%s %s, want POST .../rotate-secret", r.Method, r.URL.Path)
		}
		data, _ := io.ReadAll(r.Body)
		if len(data) != 0 {
			t.Errorf("rotate-secret must send no body, got %q", data)
		}
		if _, ok := r.Header["Content-Type"]; ok {
			t.Errorf("no Content-Type expected for a bodyless POST")
		}
		out := map[string]any{}
		for k, v := range okEndpoint {
			out[k] = v
		}
		out["secret"] = "whsec_new"
		writeJSON(t, w, http.StatusOK, out)
	})
	ep, err := c.RotateEndpointSecret(bg(), "ep_1")
	if err != nil {
		t.Fatalf("RotateEndpointSecret: %v", err)
	}
	if ep.Secret == nil || *ep.Secret != "whsec_new" {
		t.Errorf("secret = %v", ep.Secret)
	}
}

func TestEnableEndpoint(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/endpoints/ep_1/enable" {
			t.Errorf("%s %s, want POST .../enable", r.Method, r.URL.Path)
		}
		writeJSON(t, w, http.StatusOK, okEndpoint)
	})
	ep, err := c.EnableEndpoint(bg(), "ep_1")
	if err != nil {
		t.Fatalf("EnableEndpoint: %v", err)
	}
	if ep.Status != "enabled" {
		t.Errorf("status = %s", ep.Status)
	}
}

// -- api keys -----------------------------------------------------------------

func TestCreateAPIKeyBodyAndReturn(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/api-keys" {
			t.Errorf("%s %s, want POST /v1/api-keys", r.Method, r.URL.Path)
		}
		body := decodeBody(t, r)
		if body["name"] != "ci" || body["scope"] != "publish" || body["expires_in_days"] != float64(30) {
			t.Errorf("body = %v", body)
		}
		writeJSON(t, w, http.StatusCreated, map[string]any{
			"id": "key_1", "name": "ci", "scope": "publish", "key": "whsk_secret_once",
		})
	})
	key, err := c.CreateAPIKey(bg(), &CreateAPIKeyOptions{Name: "ci", Scope: "publish", ExpiresInDays: intPtr(30)})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if key.Key == nil || *key.Key != "whsk_secret_once" || key.Scope != "publish" {
		t.Errorf("key = %+v", key)
	}
}

func TestCreateAPIKeyDefaultsAndOmitsExpiry(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeBody(t, r)
		if body["name"] != "" || body["scope"] != "admin" {
			t.Errorf("defaults = %v", body)
		}
		if _, ok := body["expires_in_days"]; ok {
			t.Errorf("expires_in_days must be omitted when unset")
		}
		writeJSON(t, w, http.StatusCreated, map[string]any{"id": "key_2", "name": "", "scope": "admin", "key": "whsk_x"})
	})
	if _, err := c.CreateAPIKey(bg(), nil); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
}

func TestRevokeAPIKeyHandles204(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/api-keys/key_1" {
			t.Errorf("%s %s, want DELETE /v1/api-keys/key_1", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.RevokeAPIKey(bg(), "key_1"); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
}

// -- deliveries ---------------------------------------------------------------

func TestListDeliveriesForwardsOnlyProvidedParams(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/deliveries" {
			t.Errorf("%s %s, want GET /v1/deliveries", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("status") != "failed" || q.Get("endpoint_id") != "ep_1" {
			t.Errorf("query = %v", q.Encode())
		}
		for _, k := range []string{"event_type", "since", "until", "q", "offset", "limit"} {
			if _, ok := q[k]; ok {
				t.Errorf("unprovided filter %q must not be forwarded", k)
			}
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"items":       []any{map[string]any{"id": "dlv_1", "endpoint_id": "ep_1", "status": "failed"}},
			"next_offset": nil,
		})
	})

	page, err := c.ListDeliveries(bg(), &ListDeliveriesOptions{Status: "failed", EndpointID: "ep_1"})
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Id != "dlv_1" {
		t.Errorf("items = %+v", page.Items)
	}
	if page.NextOffset != nil {
		t.Errorf("next_offset = %v, want nil", page.NextOffset)
	}
}

func TestListDeliveriesForwardsOffsetAndLimit(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("offset") != "20" || q.Get("limit") != "10" {
			t.Errorf("query = %v", q.Encode())
		}
		writeJSON(t, w, http.StatusOK, map[string]any{"items": []any{}, "next_offset": nil})
	})
	if _, err := c.ListDeliveries(bg(), &ListDeliveriesOptions{Offset: intPtr(20), Limit: intPtr(10)}); err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
}

func TestRedeliverPath(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/deliveries/dlv_1/redeliver" {
			t.Errorf("%s %s, want POST /v1/deliveries/dlv_1/redeliver", r.Method, r.URL.Path)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{"id": "dlv_1", "endpoint_id": "ep_1", "status": "queued"})
	})
	dlv, err := c.Redeliver(bg(), "dlv_1")
	if err != nil {
		t.Fatalf("Redeliver: %v", err)
	}
	if dlv.Status != "queued" {
		t.Errorf("status = %s", dlv.Status)
	}
}
