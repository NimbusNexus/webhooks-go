package webhookd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Version tracks the release tag; keep in lockstep with the Python/TypeScript SDKs.
const Version = "0.3.0"

var retryStatuses = map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true}

// Client publishes events to webhookd and manages endpoints, API keys and deliveries.
//
// Authenticate with a per-tenant API key (whsk_…) or a service token. Transient failures
// (connection errors, 429, 5xx) are retried with capped exponential backoff; a 429 honours its
// Retry-After header. Other non-2xx responses return an *APIError carrying the server's
// {error: {code, message}} envelope.
type Client struct {
	baseURL    string
	apiKey     string
	timeout    time.Duration
	maxRetries int
	httpClient *http.Client

	// Write-first async outbox (see outbox_client.go). Zero-valued unless configured via options.
	store           Store
	maxAttempts     int
	drainBatchLimit int
	onDead          func(Record)

	drainerMu     sync.Mutex
	drainerCancel context.CancelFunc
	drainerWG     sync.WaitGroup
}

// Option configures a Client in New.
type Option func(*Client)

// WithTimeout sets the per-request timeout (default 10s). Ignored when WithHTTPClient supplies a
// client with its own Timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// WithMaxRetries sets the number of extra attempts for retryable failures (default 2).
func WithMaxRetries(n int) Option {
	return func(c *Client) { c.maxRetries = n }
}

// WithHTTPClient injects a custom *http.Client (used for tests, proxies, custom transports).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClient = h }
}

// New creates a Client for the webhookd instance at baseURL authenticated with apiKey. Trailing
// slashes are trimmed from baseURL.
func New(baseURL, apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL:         strings.TrimRight(baseURL, "/"),
		apiKey:          apiKey,
		timeout:         10 * time.Second,
		maxRetries:      2,
		maxAttempts:     10,
		drainBatchLimit: 100,
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{Timeout: c.timeout}
	}
	return c
}

// Publish publishes one event (POST /v1/events). idempotency makes the publish safe to retry — a
// replay returns the original event without re-fanning-out.
func (c *Client) Publish(ctx context.Context, eventType string, payload map[string]any, opts *PublishOptions) (*Event, error) {
	env, app := "prod", "default"
	var source *string
	var idempotency string
	if opts != nil {
		if opts.Environment != "" {
			env = opts.Environment
		}
		if opts.Application != "" {
			app = opts.Application
		}
		source = opts.Source
		idempotency = opts.IdempotencyKey
	}
	body := map[string]any{
		"event_type":  eventType,
		"payload":     payload,
		"environment": env,
		"application": app,
	}
	if source != nil {
		body["source"] = *source
	}
	var headers map[string]string
	if idempotency != "" {
		headers = map[string]string{"Idempotency-Key": idempotency}
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/events", body, nil, headers)
	if err != nil {
		return nil, err
	}
	return decodeJSON[Event](resp)
}

// CreateEndpoint creates an endpoint (POST /v1/endpoints). The response includes the signing secret
// exactly once — persist it.
func (c *Client) CreateEndpoint(ctx context.Context, endpointURL string, opts *CreateEndpointOptions) (*Endpoint, error) {
	env, app := "prod", "default"
	if opts != nil {
		if opts.Environment != "" {
			env = opts.Environment
		}
		if opts.Application != "" {
			app = opts.Application
		}
	}
	body := map[string]any{
		"url":         endpointURL,
		"environment": env,
		"application": app,
	}
	if opts != nil {
		if opts.Subscriptions != nil {
			body["subscriptions"] = opts.Subscriptions
		}
		if opts.Secret != nil {
			body["secret"] = *opts.Secret
		}
		if opts.MaxAttempts != nil {
			body["max_attempts"] = *opts.MaxAttempts
		}
		if opts.RetrySchedule != nil {
			body["retry_schedule"] = opts.RetrySchedule
		}
		if opts.Description != nil {
			body["description"] = *opts.Description
		}
		if opts.CustomHeaders != nil {
			body["custom_headers"] = opts.CustomHeaders
		}
		if opts.DeliveryTimeoutMs != nil {
			body["delivery_timeout_ms"] = *opts.DeliveryTimeoutMs
		}
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/endpoints", body, nil, nil)
	if err != nil {
		return nil, err
	}
	return decodeJSON[Endpoint](resp)
}

// ListEndpoints lists endpoints for an environment (default "prod") (GET /v1/endpoints).
func (c *Client) ListEndpoints(ctx context.Context, opts *ListEndpointsOptions) (*Page[Endpoint], error) {
	env, offset := "prod", 0
	var limit *int
	if opts != nil {
		if opts.Environment != "" {
			env = opts.Environment
		}
		offset = opts.Offset
		limit = opts.Limit
	}
	query := url.Values{}
	query.Set("environment", env)
	query.Set("offset", strconv.Itoa(offset))
	if limit != nil {
		query.Set("limit", strconv.Itoa(*limit))
	}
	resp, err := c.do(ctx, http.MethodGet, "/v1/endpoints", nil, query, nil)
	if err != nil {
		return nil, err
	}
	return decodeJSON[Page[Endpoint]](resp)
}

// GetEndpoint fetches a single endpoint by id (GET /v1/endpoints/{id}).
func (c *Client) GetEndpoint(ctx context.Context, id string) (*Endpoint, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/endpoints/"+url.PathEscape(id), nil, nil, nil)
	if err != nil {
		return nil, err
	}
	return decodeJSON[Endpoint](resp)
}

// UpdateEndpoint partially updates an endpoint (PATCH /v1/endpoints/{id}). patch is sent verbatim:
// an omitted key is left unchanged, an explicit nil clears the field.
func (c *Client) UpdateEndpoint(ctx context.Context, id string, patch Patch) (*Endpoint, error) {
	resp, err := c.do(ctx, http.MethodPatch, "/v1/endpoints/"+url.PathEscape(id), patch, nil, nil)
	if err != nil {
		return nil, err
	}
	return decodeJSON[Endpoint](resp)
}

// DeleteEndpoint deletes an endpoint (DELETE /v1/endpoints/{id}, 204).
func (c *Client) DeleteEndpoint(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/v1/endpoints/"+url.PathEscape(id), nil, nil, nil)
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

// RotateEndpointSecret rotates an endpoint's signing secret (POST /v1/endpoints/{id}/rotate-secret).
// The response includes the new secret exactly once.
func (c *Client) RotateEndpointSecret(ctx context.Context, id string) (*Endpoint, error) {
	resp, err := c.do(ctx, http.MethodPost, "/v1/endpoints/"+url.PathEscape(id)+"/rotate-secret", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	return decodeJSON[Endpoint](resp)
}

// EnableEndpoint re-enables an endpoint webhookd auto-disabled after repeated failures
// (POST /v1/endpoints/{id}/enable).
func (c *Client) EnableEndpoint(ctx context.Context, id string) (*Endpoint, error) {
	resp, err := c.do(ctx, http.MethodPost, "/v1/endpoints/"+url.PathEscape(id)+"/enable", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	return decodeJSON[Endpoint](resp)
}

// CreateAPIKey creates an API key (POST /v1/api-keys). The response includes the raw key exactly
// once — persist it.
func (c *Client) CreateAPIKey(ctx context.Context, opts *CreateAPIKeyOptions) (*ApiKey, error) {
	name, scope := "", "admin"
	var expiresInDays *int
	if opts != nil {
		name = opts.Name
		if opts.Scope != "" {
			scope = opts.Scope
		}
		expiresInDays = opts.ExpiresInDays
	}
	body := map[string]any{"name": name, "scope": scope}
	if expiresInDays != nil {
		body["expires_in_days"] = *expiresInDays
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/api-keys", body, nil, nil)
	if err != nil {
		return nil, err
	}
	return decodeJSON[ApiKey](resp)
}

// RevokeAPIKey revokes an API key (DELETE /v1/api-keys/{id}, 204).
func (c *Client) RevokeAPIKey(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/v1/api-keys/"+url.PathEscape(id), nil, nil, nil)
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

// ListDeliveries lists deliveries (GET /v1/deliveries). Only the filters you provide are forwarded.
func (c *Client) ListDeliveries(ctx context.Context, opts *ListDeliveriesOptions) (*Page[Delivery], error) {
	query := url.Values{}
	if opts != nil {
		if opts.Status != "" {
			query.Set("status", opts.Status)
		}
		if opts.EndpointID != "" {
			query.Set("endpoint_id", opts.EndpointID)
		}
		if opts.EventType != "" {
			query.Set("event_type", opts.EventType)
		}
		if opts.Since != "" {
			query.Set("since", opts.Since)
		}
		if opts.Until != "" {
			query.Set("until", opts.Until)
		}
		if opts.Q != "" {
			query.Set("q", opts.Q)
		}
		if opts.Offset != nil {
			query.Set("offset", strconv.Itoa(*opts.Offset))
		}
		if opts.Limit != nil {
			query.Set("limit", strconv.Itoa(*opts.Limit))
		}
	}
	resp, err := c.do(ctx, http.MethodGet, "/v1/deliveries", nil, query, nil)
	if err != nil {
		return nil, err
	}
	return decodeJSON[Page[Delivery]](resp)
}

// Redeliver re-enqueues a delivery for another attempt (POST /v1/deliveries/{id}/redeliver).
func (c *Client) Redeliver(ctx context.Context, id string) (*Delivery, error) {
	resp, err := c.do(ctx, http.MethodPost, "/v1/deliveries/"+url.PathEscape(id)+"/redeliver", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	return decodeJSON[Delivery](resp)
}

// do issues one authenticated request with the shared retry policy and returns the raw response for
// the caller to parse (a 204 must not be parsed). body is JSON-encoded when non-nil. Connection
// errors, 429 (honouring Retry-After) and 5xx are retried with capped exponential backoff,
// respecting ctx; other non-2xx responses return an *APIError.
func (c *Client) do(ctx context.Context, method, path string, body any, query url.Values, extraHeaders map[string]string) (*http.Response, error) {
	fullURL := c.baseURL + path
	if len(query) > 0 {
		fullURL += "?" + query.Encode()
	}

	var bodyBytes []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, &Error{Message: fmt.Sprintf("failed to encode request body: %v", err), Err: err}
		}
		bodyBytes = encoded
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		var reader io.Reader
		if bodyBytes != nil {
			reader = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
		if err != nil {
			return nil, &Error{Message: fmt.Sprintf("failed to build request: %v", err), Err: err}
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range extraHeaders {
			req.Header.Set(k, v)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, &Error{Message: fmt.Sprintf("request failed: %v", ctx.Err()), Err: ctx.Err()}
			}
			lastErr = err
			if attempt < c.maxRetries {
				if serr := sleepCtx(ctx, backoff(attempt)); serr != nil {
					return nil, &Error{Message: fmt.Sprintf("request failed: %v", serr), Err: serr}
				}
				continue
			}
			return nil, &Error{Message: fmt.Sprintf("request failed: %v", err), Err: err}
		}

		if retryStatuses[resp.StatusCode] && attempt < c.maxRetries {
			wait := retryAfter(resp)
			if wait <= 0 {
				wait = backoff(attempt)
			}
			drain(resp)
			if serr := sleepCtx(ctx, wait); serr != nil {
				return nil, &Error{Message: fmt.Sprintf("request failed: %v", serr), Err: serr}
			}
			continue
		}
		if resp.StatusCode >= 400 {
			return nil, apiError(resp)
		}
		return resp, nil
	}
	return nil, &Error{Message: fmt.Sprintf("request failed after retries: %v", lastErr), Err: lastErr}
}

// decodeJSON decodes a successful response body into T and closes it. Never use for 204s.
func decodeJSON[T any](resp *http.Response) (*T, error) {
	defer resp.Body.Close()
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, &Error{Message: fmt.Sprintf("failed to decode response: %v", err), Err: err}
	}
	return &out, nil
}

// drain discards and closes a response body so the connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// backoff returns the capped exponential backoff for the given retry attempt: min(2s, 200ms·2^n).
func backoff(attempt int) time.Duration {
	ms := 200 * (1 << attempt)
	if ms > 2000 {
		ms = 2000
	}
	return time.Duration(ms) * time.Millisecond
}

// retryAfter returns the Retry-After delay when the header is a non-negative integer count of
// seconds, else 0.
func retryAfter(resp *http.Response) time.Duration {
	raw := resp.Header.Get("Retry-After")
	if raw == "" {
		return 0
	}
	if n, err := strconv.ParseUint(raw, 10, 63); err == nil {
		return time.Duration(n) * time.Second
	}
	return 0
}

// sleepCtx sleeps for d unless ctx is cancelled first, in which case it returns ctx.Err().
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// apiError reads a non-2xx response body and builds an *APIError, parsing the M5a
// {error: {code, message}} envelope when present and falling back to the raw text otherwise.
func apiError(resp *http.Response) *APIError {
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	code, message := "error", string(data)
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil {
		if envelope.Error.Code != "" {
			code = envelope.Error.Code
		}
		if envelope.Error.Message != "" {
			message = envelope.Error.Message
		}
	}
	return &APIError{StatusCode: resp.StatusCode, Code: code, Message: message}
}
