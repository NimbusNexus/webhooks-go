package webhooks

import "fmt"

// Error is the base error type for all webhookd SDK errors (network failures, etc.).
//
// It wraps an underlying cause when present so callers can use errors.Is/errors.As.
type Error struct {
	// Message is the human-readable description of the failure.
	Message string
	// Err is the underlying cause, if any (e.g. a transport error).
	Err error
}

func (e *Error) Error() string {
	return e.Message
}

// Unwrap exposes the underlying cause for errors.Is/errors.As.
func (e *Error) Unwrap() error {
	return e.Err
}

// APIError is a non-2xx response from the webhookd API.
//
// It carries the M5a error envelope: a stable machine Code (e.g. "rate_limited",
// "not_found", "validation_error") and a human Message, plus the HTTP StatusCode.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("[%d %s] %s", e.StatusCode, e.Code, e.Message)
}
