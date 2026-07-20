// Command receiver is a runnable net/http server that verifies incoming webhookd webhooks.
//
//	WEBHOOKD_SIGNING_SECRET=whsec_... go run ./examples/receiver
//	# then register an endpoint pointing at http://<this host>:8080/webhooks and publish an event.
//
// The one rule: verify against the RAW request body bytes (not a re-serialized JSON object).
// This mirrors examples/python/receiver.py and examples/typescript/receiver.ts.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"

	webhookd "github.com/NimbusNexus/webhookd-go"
)

func main() {
	secret := os.Getenv("WEBHOOKD_SIGNING_SECRET") // the endpoint's signing secret (shown once on create)
	if secret == "" {
		log.Fatal("set WEBHOOKD_SIGNING_SECRET to the endpoint's signing secret")
	}

	http.HandleFunc("/webhooks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body) // RAW bytes — do NOT decode then re-encode before verifying
		if err != nil {
			http.Error(w, `{"error":"failed to read body"}`, http.StatusBadRequest)
			return
		}

		// X-Webhook-Timestamp arrives as a string; parse it so Verify can enforce the replay window.
		// A missing or malformed timestamp is rejected rather than silently verified body-only.
		var opts *webhookd.VerifyOptions
		if raw := r.Header.Get("X-Webhook-Timestamp"); raw != "" {
			ts, perr := strconv.ParseInt(raw, 10, 64)
			if perr != nil {
				reject(w) // malformed timestamp — never authenticate it
				return
			}
			opts = &webhookd.VerifyOptions{Timestamp: &ts}
		}

		if !webhookd.Verify(secret, body, r.Header.Get("X-Webhook-Signature"), opts) {
			reject(w) // forged, tampered, or replayed
			return
		}

		var event struct {
			ID   string          `json:"id"`
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
			return
		}
		log.Printf("received %s (%s): %s", event.Type, event.ID, event.Data)
		// ... do your work — idempotently; a retry can re-deliver, so dedupe on event.ID ...

		w.WriteHeader(http.StatusOK) // any 2xx acknowledges; anything else (or a timeout) triggers a retry
	})

	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}
	fmt.Printf("listening on %s/webhooks\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// reject responds with 400 + a JSON error for a signature that did not verify.
func reject(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"error":"invalid signature"}`))
}
