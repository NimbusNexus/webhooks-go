// Package webhookd is the official Go SDK for NimbusNexus Webhooks (webhookd).
//
//   - Verify — verify an incoming webhook's HMAC signature (for subscribers).
//   - Client — publish events to webhookd and manage endpoints/deliveries (for producers).
//
// webhookd signs every delivery as HMAC_SHA256(secret, "<timestamp>." + rawBody) (its default
// timestamped mode) and sends X-Webhook-Signature: sha256=<hex> plus X-Webhook-Timestamp
// (unix seconds). A subscriber MUST verify the signature to prove the request genuinely came from
// webhookd and was not tampered with. This mirrors delivery_core.webhook_outbox.{sign,verify}
// byte-for-byte.
package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

const (
	signaturePrefix = "sha256="
	// DefaultToleranceSeconds is webhookd's default replay window for signature verification.
	DefaultToleranceSeconds = 300
)

// signedBytes rebuilds the exact bytes webhookd signs: when a timestamp is given,
// ASCII(itoa(timestamp) + ".") concatenated with the raw body; otherwise just the raw body.
func signedBytes(body []byte, timestamp *int64) []byte {
	if timestamp == nil {
		return body
	}
	prefix := strconv.FormatInt(*timestamp, 10) + "."
	out := make([]byte, 0, len(prefix)+len(body))
	out = append(out, prefix...)
	out = append(out, body...)
	return out
}

// signRaw computes the "sha256=<hex>" signature webhookd would send for body (+ optional timestamp).
func signRaw(secret string, body []byte, timestamp *int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(signedBytes(body, timestamp))
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// Sign returns the "sha256=<hex>" signature webhookd would send for body, signing over
// "<timestamp>." + body. Mainly useful for tests; subscribers call Verify.
func Sign(secret string, body []byte, timestamp int64) string {
	return signRaw(secret, body, &timestamp)
}

// VerifyOptions configures Verify.
type VerifyOptions struct {
	// Timestamp is the X-Webhook-Timestamp header value (unix seconds). When non-nil the replay
	// window is enforced against it — always pass it. When nil, the signature is computed over the
	// body alone.
	Timestamp *int64
	// ToleranceSeconds is the replay window; 0 means the default (300).
	ToleranceSeconds int
	// Now overrides the current unix time (for tests); 0 means time.Now().Unix().
	Now int64
}

// Verify reports whether signature is a valid webhookd signature for body.
//
// Pass the EXACT bytes you received as body — do not re-serialize the JSON, or the signature won't
// match. When opts.Timestamp is set, the signature is rejected if it is older/newer than the
// tolerance window (replay protection). X-Webhook-Signature carries one token normally, or several
// comma-separated tokens during a signing-secret rotation (dual-sign overlap); Verify accepts the
// request if ANY token matches, using a constant-time comparison. A malformed X-Webhook-Timestamp
// returns false rather than panicking.
func Verify(secret string, body []byte, signature string, opts *VerifyOptions) bool {
	var ts *int64
	tolerance := DefaultToleranceSeconds
	var now int64

	if opts != nil {
		ts = opts.Timestamp
		if opts.ToleranceSeconds != 0 {
			tolerance = opts.ToleranceSeconds
		}
		now = opts.Now
	}

	if ts != nil {
		current := now
		if current == 0 {
			current = time.Now().Unix()
		}
		diff := current - *ts
		if diff < 0 {
			diff = -diff
		}
		if diff > int64(tolerance) {
			return false
		}
	}

	expected := signRaw(secret, body, ts)
	// Accept if ANY comma-separated token verifies, so a subscriber configured with EITHER the
	// current or the previous secret keeps working during a rotation overlap.
	for _, raw := range strings.Split(signature, ",") {
		token := strings.TrimSpace(raw)
		candidate := token
		if !strings.HasPrefix(candidate, signaturePrefix) {
			candidate = signaturePrefix + candidate
		}
		if hmac.Equal([]byte(expected), []byte(candidate)) {
			return true
		}
	}
	return false
}
