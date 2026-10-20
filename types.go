package webhooks

// Event is the published event, as returned by POST /v1/events (webhookd's EventOut). ProjectID is
// the opaque project id (prj_…) the event landed in — the workspace's default project when the request
// omitted one. SkippedUnverified counts the matching endpoints webhookd withheld because the
// verification gate is on and they have not passed the echo challenge.
type Event struct {
	Id                string  `json:"id"`
	EventUID          string  `json:"event_uid"`
	EventType         string  `json:"event_type"`
	ProjectID         string  `json:"project_id"`
	DeliveriesCreated int     `json:"deliveries_created"`
	SkippedUnverified int     `json:"skipped_unverified"`
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
// rotate-secret responses (returned exactly once). Status is enabled, disabled or auto_disabled;
// Health is healthy, degraded, disabled or gated. RetrySchedule is the delay in seconds before each
// retry (nil: webhookd's exponential backoff). Custom header values are write-only: webhookd returns
// their names, never the values.
type Endpoint struct {
	Id                   string         `json:"id"`
	URL                  string         `json:"url"`
	Status               string         `json:"status"`
	Health               string         `json:"health"`
	ProjectID            string         `json:"project_id"`
	MaxAttempts          *int           `json:"max_attempts,omitempty"`
	RetrySchedule        []float64      `json:"retry_schedule,omitempty"`
	Description          *string        `json:"description,omitempty"`
	CustomHeaderNames    []string       `json:"custom_header_names"`
	DeliveryTimeoutMs    *int           `json:"delivery_timeout_ms,omitempty"`
	Subscriptions        []Subscription `json:"subscriptions"`
	Verified             bool           `json:"verified"`
	GatedUnverified      bool           `json:"gated_unverified"`
	ConsecutiveFailures  int            `json:"consecutive_failures"`
	AutoDisableThreshold int            `json:"auto_disable_threshold"`
	DisabledAt           *string        `json:"disabled_at,omitempty"`
	DisabledReason       *string        `json:"disabled_reason,omitempty"`
	Secret               *string        `json:"secret,omitempty"`
}

// ApiKey is the shape POST /v1/api-keys returned (webhookd's ApiKeyOut). webhookd has deleted that
// route; the struct remains the return type of CreateAPIKey, which now fails with a 404. Key was
// present ONLY on the create response.
type ApiKey struct {
	Id        string  `json:"id"`
	Name      string  `json:"name"`
	Scope     string  `json:"scope"`
	Key       *string `json:"key,omitempty"`
	CreatedBy *string `json:"created_by,omitempty"`
	CreatedAt *string `json:"created_at,omitempty"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// Delivery is a delivery record, as returned by the deliveries endpoints (webhookd's DeliveryOut).
// ProjectID, EventType, EventSource and EventCreatedAt are read from the delivery's event, and are
// nil when that event is gone.
type Delivery struct {
	Id             string  `json:"id"`
	EventID        *string `json:"event_id,omitempty"`
	EndpointID     string  `json:"endpoint_id"`
	Status         string  `json:"status"`
	Attempts       *int    `json:"attempts,omitempty"`
	LastError      *string `json:"last_error,omitempty"`
	NextAttemptAt  *string `json:"next_attempt_at,omitempty"`
	SentAt         *string `json:"sent_at,omitempty"`
	ProjectID      *string `json:"project_id,omitempty"`
	EventType      *string `json:"event_type,omitempty"`
	EventSource    *string `json:"event_source,omitempty"`
	EventCreatedAt *string `json:"event_created_at,omitempty"`
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

// PublishOptions carries the optional arguments to Client.Publish. ProjectID is the opaque project
// id (prj_…); leave it EMPTY to target the workspace's default project — the field is then omitted from
// the request and the server resolves it. Source is sent only when non-nil. IdempotencyKey, when
// non-empty, is sent as the Idempotency-Key header.
type PublishOptions struct {
	ProjectID      string
	Source         *string
	IdempotencyKey string
}

// CreateEndpointOptions carries the optional arguments to Client.CreateEndpoint. ProjectID is the
// opaque project id (prj_…); leave it EMPTY to target the workspace's default project. Every field is
// sent only when set (non-empty string, non-nil pointer) so the server applies its own default.
// RetrySchedule is the delay in seconds before each retry.
type CreateEndpointOptions struct {
	ProjectID         string
	Subscriptions     []Subscription
	Secret            *string
	MaxAttempts       *int
	RetrySchedule     []float64
	Description       *string
	CustomHeaders     map[string]string
	DeliveryTimeoutMs *int
}

// ListEndpointsOptions carries the optional arguments to Client.ListEndpoints. ProjectID is the
// opaque project id (prj_…) and is sent only when non-empty — leave it empty to list the workspace's
// default project. Offset is always sent; Limit is sent only when non-nil.
type ListEndpointsOptions struct {
	ProjectID string
	Offset    int
	Limit     *int
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
