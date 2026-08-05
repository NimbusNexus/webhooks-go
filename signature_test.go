package webhooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// sigVector mirrors one entry in fixtures/signature-vectors.json — signatures produced by
// delivery-core (the SERVER's signer), so the SDK must reproduce AND verify each one.
type sigVector struct {
	Name      string `json:"name"`
	Secret    string `json:"secret"`
	Body      string `json:"body"`
	Timestamp int64  `json:"timestamp"`
	Signature string `json:"signature"`
}

func loadVectors(t *testing.T) []sigVector {
	t.Helper()
	path := filepath.Join("testdata", "signature-vectors.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var doc struct {
		Vectors []sigVector `json:"vectors"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	if len(doc.Vectors) == 0 {
		t.Fatal("no signature vectors loaded")
	}
	return doc.Vectors
}

// TestSharedSignatureVectors is the hard cross-language gate: every server-produced vector must be
// reproduced byte-for-byte by Sign and accepted by Verify.
func TestSharedSignatureVectors(t *testing.T) {
	for _, v := range loadVectors(t) {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			got := Sign(v.Secret, []byte(v.Body), v.Timestamp)
			if got != v.Signature {
				t.Fatalf("Sign mismatch:\n got  %s\n want %s", got, v.Signature)
			}
			ts := v.Timestamp
			if !Verify(v.Secret, []byte(v.Body), v.Signature, &VerifyOptions{Timestamp: &ts, Now: ts}) {
				t.Fatal("Verify rejected a valid server signature")
			}
			// the bare hex (no sha256= prefix) must also verify
			bare := strings.TrimPrefix(v.Signature, "sha256=")
			if !Verify(v.Secret, []byte(v.Body), bare, &VerifyOptions{Timestamp: &ts, Now: ts}) {
				t.Fatal("Verify rejected the bare-hex signature")
			}
			// a one-byte tamper must be rejected
			if Verify(v.Secret, []byte(v.Body+" "), v.Signature, &VerifyOptions{Timestamp: &ts, Now: ts}) {
				t.Fatal("Verify accepted a tampered body")
			}
		})
	}
}

const (
	testSecret = "whsec_test_value"
	testTS     = int64(1_700_000_000)
)

var testBody = []byte(`{"data":{"id":1},"id":"evt_1","type":"order.created","version":"1"}`)

func TestSignVerifyRoundtrip(t *testing.T) {
	sig := Sign(testSecret, testBody, testTS)
	if !strings.HasPrefix(sig, "sha256=") {
		t.Fatalf("signature missing sha256= prefix: %s", sig)
	}
	ts := testTS
	if !Verify(testSecret, testBody, sig, &VerifyOptions{Timestamp: &ts, Now: ts}) {
		t.Fatal("roundtrip signature failed to verify")
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	sig := Sign(testSecret, testBody, testTS)
	ts := testTS
	if Verify("whsec_other", testBody, sig, &VerifyOptions{Timestamp: &ts, Now: ts}) {
		t.Fatal("verified under the wrong secret")
	}
}

// TestVerifyReplayWindow enforces the tolerance window: a signature just inside it verifies, one
// just outside is rejected.
func TestVerifyReplayWindow(t *testing.T) {
	sig := Sign(testSecret, testBody, testTS)
	ts := testTS
	within := testTS + 299
	if !Verify(testSecret, testBody, sig, &VerifyOptions{Timestamp: &ts, Now: within}) {
		t.Fatal("expected a signature inside the 300s window to verify")
	}
	outside := testTS + 301
	if Verify(testSecret, testBody, sig, &VerifyOptions{Timestamp: &ts, Now: outside}) {
		t.Fatal("expected a stale (replayed) signature to be rejected")
	}
}

// TestVerifyDualCommaSeparated covers the rotation overlap: X-Webhook-Signature may carry two
// comma-separated tokens (current + previous). A subscriber holding EITHER secret must verify.
func TestVerifyDualCommaSeparated(t *testing.T) {
	current := "whsec_current"
	previous := "whsec_previous"
	body := []byte(`{"k":"v"}`)
	ts := testTS
	bogus := Sign(previous, body, ts) // a token that does NOT match the current secret
	real := Sign(current, body, ts)
	header := bogus + "," + real // one bogus + the real one
	if !Verify(current, body, header, &VerifyOptions{Timestamp: &ts, Now: ts}) {
		t.Fatal("expected the real token in a dual-signed header to verify")
	}
	// swap order and add surrounding whitespace — trimming + any-match still holds
	if !Verify(current, body, "  "+real+" , "+bogus+"  ", &VerifyOptions{Timestamp: &ts, Now: ts}) {
		t.Fatal("expected any matching token (with whitespace) to verify")
	}
	if Verify("whsec_neither", body, header, &VerifyOptions{Timestamp: &ts, Now: ts}) {
		t.Fatal("expected a header matching neither secret to be rejected")
	}
}

// TestVerifyMalformedTimestamp: a non-integer X-Webhook-Timestamp is malformed. Verify must reject
// (and never panic) rather than authenticate a timestamped delivery.
func TestVerifyMalformedTimestamp(t *testing.T) {
	sig := Sign(testSecret, testBody, testTS)
	// The X-Webhook-Timestamp header arrives as a string; a subscriber parses it before calling
	// Verify. A non-integer header cannot be parsed to a valid signed timestamp.
	if _, err := strconv.ParseInt("not-a-number", 10, 64); err == nil {
		t.Fatal("expected a non-integer timestamp header to fail parsing")
	}
	// A malformed timestamp must never authenticate a timestamped delivery (defence in depth: the
	// timestamped signature cannot be reconstructed body-only), and the call must not panic.
	if Verify(testSecret, testBody, sig, &VerifyOptions{Timestamp: nil, Now: testTS}) {
		t.Fatal("a signature signed with a timestamp must not verify body-only")
	}
}

// TestVerifyNoTimestampSignsBodyOnly: when no timestamp is supplied, the signature is computed over
// the body alone (no replay window).
func TestVerifyNoTimestampSignsBodyOnly(t *testing.T) {
	sig := signRaw(testSecret, testBody, nil)
	if !Verify(testSecret, testBody, sig, &VerifyOptions{Timestamp: nil}) {
		t.Fatal("expected a body-only signature to verify with no timestamp")
	}
	if !Verify(testSecret, testBody, sig, nil) {
		t.Fatal("expected a body-only signature to verify with nil options")
	}
}
