package webhookd

// Event is the published event, as returned by POST /v1/events (webhookd's EventOut).
type Event struct {
	Id                string  `json:"id"`
	EventUID          string  `json:"event_uid"`
	EventType         string  `json:"event_type"`
	Application       string  `json:"application"`
	Environment       string  `json:"environment"`
	DeliveriesCreated int     `json:"deliveries_created"`
	Source            *string `json:"source"`
}

// Subscription is a subscription filter attached to an endpoint. MatchKind is one of
// exact|prefix|suffix|all.
type Subscription struct {
	MatchKind string `json:"match_kind"`
	Pattern   string `json:"pattern"`
}

// Endpoint is an endpoint, as returned by the management endpoints (webhookd's EndpointOut).
// Fields mirror the server's snake_case wire shape verbatim. Secret is present ONLY on the create +
// rotate-secret responses (returned exactly once).
type Endpoint struct {
	Id                string            `json:"id"`
	URL               string            `json:"url"`
	Environment       string            `json:"environment"`
	Application       string            `json:"application"`
	Status            string            `json:"status"`
	Subscriptions     []Subscription    `json:"subscriptions"`
	Secret            *string           `json:"secret,omitempty"`
	MaxAttempts       *int              `json:"max_attempts,omitempty"`
	RetrySchedule     []int             `json:"retry_schedule,omitempty"`
	Description       *string           `json:"description,omitempty"`
	CustomHeaders     map[string]string `json:"custom_headers,omitempty"`
	DeliveryTimeoutMs *int              `json:"delivery_timeout_ms,omitempty"`
	CreatedAt         *string           `json:"created_at,omitempty"`
	UpdatedAt         *string           `json:"updated_at,omitempty"`
}

// ApiKey is an API key, as returned by POST /v1/api-keys (webhookd's ApiKeyOut). Key is present
// ONLY on the create response (returned exactly once).
type ApiKey struct {
	Id        string  `json:"id"`
	Name      string  `json:"name"`
	Scope     string  `json:"scope"`
	Key       *string `json:"key,omitempty"`
	CreatedBy *string `json:"created_by,omitempty"`
	CreatedAt *string `json:"created_at,omitempty"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// Delivery is a delivery attempt record, as returned by the deliveries endpoints (webhookd's
// DeliveryOut).
type Delivery struct {
	Id         string  `json:"id"`
	EndpointID string  `json:"endpoint_id"`
	EventID    *string `json:"event_id,omitempty"`
	EventType  *string `json:"event_type,omitempty"`
	Status     string  `json:"status"`
	Attempts   *int    `json:"attempts,omitempty"`
	CreatedAt  *string `json:"created_at,omitempty"`
	UpdatedAt  *string `json:"updated_at,omitempty"`
}

// Page is a single page of a list endpoint — items plus the cursor for the next page (nil at the
// end).
type Page[T any] struct {
	Items      []T  `json:"items"`
	NextOffset *int `json:"next_offset"`
}

// Patch is a raw PATCH mapping for UpdateEndpoint, keyed with the server's snake_case names. PATCH
// semantics: an OMITTED key is left unchanged; an explicit nil CLEARS the field (JSON null). The map
// is sent verbatim.
type Patch = map[string]any

// PublishOptions carries the optional arguments to Client.Publish. Environment defaults to "prod"
// and Application to "default" when empty. Source is sent only when non-nil. IdempotencyKey, when
// non-empty, is sent as the Idempotency-Key header.
type PublishOptions struct {
	Environment    string
	Application    string
	Source         *string
	IdempotencyKey string
}

// CreateEndpointOptions carries the optional arguments to Client.CreateEndpoint. Environment
// defaults to "prod" and Application to "default" when empty. Every other field is sent only when
// non-nil so the server applies its own default.
type CreateEndpointOptions struct {
	Environment       string
	Application       string
	Subscriptions     []Subscription
	Secret            *string
	MaxAttempts       *int
	RetrySchedule     []int
	Description       *string
	CustomHeaders     map[string]string
	DeliveryTimeoutMs *int
}

// ListEndpointsOptions carries the optional arguments to Client.ListEndpoints. Environment defaults
// to "prod" when empty; Offset is always sent; Limit is sent only when non-nil.
type ListEndpointsOptions struct {
	Environment string
	Offset      int
	Limit       *int
}

// CreateAPIKeyOptions carries the optional arguments to Client.CreateAPIKey. Scope defaults to
// "admin" when empty; ExpiresInDays is sent only when non-nil.
type CreateAPIKeyOptions struct {
	Name          string
	Scope         string
	ExpiresInDays *int
}

// ListDeliveriesOptions carries the optional filters for Client.ListDeliveries. Every filter is
// forwarded only when provided (non-empty string, or non-nil pointer).
type ListDeliveriesOptions struct {
	Status     string
	EndpointID string
	EventType  string
	Since      string
	Until      string
	Q          string
	Offset     *int
	Limit      *int
}
