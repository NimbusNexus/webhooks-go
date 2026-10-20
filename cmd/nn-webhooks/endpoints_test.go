package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recorded is one request the stand-in webhookd received.
type recorded struct {
	method, path, body string
}

// standInWebhookd records every request and answers with the body webhookd's own app sent for a
// subscription replace (fixtures/webhookd-responses.json).
func standInWebhookd(t *testing.T) (*[]recorded, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "webhookd-responses.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture struct {
		Responses map[string]string `json:"responses"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	reqs := &[]recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*reqs = append(*reqs, recorded{r.Method, r.URL.Path, string(body)})
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fixture.Responses["endpoint_subscriptions_replaced"])
	}))
	t.Cleanup(srv.Close)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("NN_WEBHOOKS_URL", "")
	t.Setenv("NN_WEBHOOKS_API_KEY", "")
	t.Setenv("NN_WEBHOOKS_PROFILE", "")
	return reqs, srv.URL
}

// runCaptured runs an endpoints subcommand against url and returns what it printed to stdout.
func runCaptured(t *testing.T, cmd func(context.Context, []string, string, string, string) error, url string, args ...string) (string, error) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("temp stdout: %v", err)
	}
	prev := os.Stdout
	os.Stdout = out
	runErr := cmd(context.Background(), args, url, "eyJ.test.key", "")
	os.Stdout = prev
	_ = out.Close()
	printed, _ := os.ReadFile(out.Name())
	return string(printed), runErr
}

func TestReplaceSubscriptionsSendsEverySubscribeFlag(t *testing.T) {
	reqs, url := standInWebhookd(t)
	printed, err := runCaptured(t, endpointsReplaceSubscriptions, url,
		"ep_1", "--subscribe", "exact:order.paid", "--subscribe", "prefix:invoice.")
	if err != nil {
		t.Fatalf("replace-subscriptions: %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("requests = %+v, want one", *reqs)
	}
	got := (*reqs)[0]
	if got.method != http.MethodPut || got.path != "/v1/endpoints/ep_1/subscriptions" {
		t.Errorf("%s %s, want PUT /v1/endpoints/ep_1/subscriptions", got.method, got.path)
	}
	want := `{"subscriptions":[{"match_kind":"exact","pattern":"order.paid"},{"match_kind":"prefix","pattern":"invoice."}]}`
	if got.body != want {
		t.Errorf("body = %s\nwant %s", got.body, want)
	}
	var ep map[string]any
	if err := json.Unmarshal([]byte(printed), &ep); err != nil {
		t.Fatalf("stdout is not the endpoint JSON: %v\n%s", err, printed)
	}
	if subs, _ := ep["subscriptions"].([]any); len(subs) != 2 {
		t.Errorf("printed subscriptions = %v", ep["subscriptions"])
	}
	if schedule, _ := ep["retry_schedule"].([]any); len(schedule) != 3 || schedule[1] != float64(30) {
		t.Errorf("printed retry_schedule = %v", ep["retry_schedule"])
	}
}

func TestReplaceSubscriptionsNoneSendsAnEmptyList(t *testing.T) {
	reqs, url := standInWebhookd(t)
	if _, err := runCaptured(t, endpointsReplaceSubscriptions, url, "ep_1", "--none"); err != nil {
		t.Fatalf("replace-subscriptions --none: %v", err)
	}
	if len(*reqs) != 1 || (*reqs)[0].body != `{"subscriptions":[]}` {
		t.Errorf("requests = %+v, want one with an empty subscription list", *reqs)
	}
}

func TestReplaceSubscriptionsWantsSubscribeOrNone(t *testing.T) {
	cases := map[string][]string{
		"neither": {"ep_1"},
		"both":    {"ep_1", "--none", "--subscribe", "all"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			reqs, url := standInWebhookd(t)
			_, err := runCaptured(t, endpointsReplaceSubscriptions, url, args...)
			if err == nil || !strings.Contains(err.Error(), "--none") {
				t.Errorf("error = %v, want one naming --none", err)
			}
			if len(*reqs) != 0 {
				t.Errorf("requests = %+v, want none", *reqs)
			}
		})
	}
}

func TestUpdateRefusesSubscriptions(t *testing.T) {
	reqs, url := standInWebhookd(t)
	_, err := runCaptured(t, endpointsUpdate, url, "ep_1", "--set", `subscriptions=[{"match_kind":"all"}]`)
	if err == nil || !strings.Contains(err.Error(), "replace-subscriptions") {
		t.Errorf("error = %v, want one pointing at replace-subscriptions", err)
	}
	if len(*reqs) != 0 {
		t.Errorf("requests = %+v, want none: webhookd would answer 200 and change nothing", *reqs)
	}
}
