package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NimbusNexus/webhooks-go/internal/credentials"
)

// identity stands in for Identity's device routes (RFC-0024).
type identity struct {
	t        *testing.T
	mu       sync.Mutex
	polls    []string // the answer given to each token poll, in order
	answers  []string // queued token answers: "pending", "slow", "denied", "expired", "ok"
	forms    map[string][]url.Values
	revokeOK bool
}

func newIdentity(t *testing.T, answers ...string) (*identity, *httptest.Server) {
	id := &identity{t: t, answers: answers, forms: map[string][]url.Values{}, revokeOK: true}
	srv := httptest.NewServer(http.HandlerFunc(id.serve))
	t.Cleanup(srv.Close)
	return id, srv
}

func (id *identity) serve(w http.ResponseWriter, r *http.Request) {
	id.mu.Lock()
	defer id.mu.Unlock()
	if err := r.ParseForm(); err != nil {
		id.t.Fatalf("form: %v", err)
	}
	id.forms[r.URL.Path] = append(id.forms[r.URL.Path], r.PostForm)
	w.Header().Set("Content-Type", "application/json")
	refuse := func(code string) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":%q,"error_description":"x"}`, code)
	}
	switch r.URL.Path {
	case "/api/v1/oauth/device_authorization":
		fmt.Fprint(w, `{"device_code":"dev-code","user_code":"BCDF-GHJK",
			"verification_uri":"https://account.example/device",
			"verification_uri_complete":"https://account.example/device?code=BCDF-GHJK",
			"expires_in":900,"interval":5}`)
	case "/api/v1/oauth/token":
		answer := "pending"
		if len(id.answers) > 0 {
			answer, id.answers = id.answers[0], id.answers[1:]
		}
		id.polls = append(id.polls, answer)
		switch answer {
		case "pending":
			refuse("authorization_pending")
		case "slow":
			refuse("slow_down")
		case "denied":
			refuse("access_denied")
		case "expired":
			refuse("expired_token")
		case "ok":
			fmt.Fprintf(w, `{"access_token":%q,"token_type":"Bearer","expires_in":7776000,
				"scope":"admin","key_id":"key_1","key_name":"nn-webhooks on box","product":"webhookd",
				"project":null,"workspace_id":"org_1","workspace_name":"Acme",
				"expires_at":"2027-01-17T10:00:00Z"}`, fakeKey("key_1"))
		}
	case "/api/v1/oauth/revoke":
		if !id.revokeOK {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"revoked":true}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func fakeKey(jti string) string {
	payload, _ := json.Marshal(map[string]any{
		"sub": "key:" + jti, "jti": jti, "workspace_id": "org_1", "scope": "admin",
		"product": "webhookd", "exp": 1800000000,
	})
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// isolate points the credentials file at a temp dir and stubs the browser, the clock and stdout.
func isolate(t *testing.T) (opened *[]string, slept *[]time.Duration) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(envAPIKey, "")
	t.Setenv(envURL, "")
	t.Setenv(credentials.EnvProfile, "")
	opened, slept = &[]string{}, &[]time.Duration{}
	prevOpen, prevSleep, prevHost := openBrowser, sleep, hostname
	openBrowser = func(u string) error { *opened = append(*opened, u); return nil }
	sleep = func(ctx context.Context, d time.Duration) error { *slept = append(*slept, d); return ctx.Err() }
	hostname = func() (string, error) { return "box", nil }
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	prevOut, prevErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devnull, devnull
	t.Cleanup(func() {
		openBrowser, sleep, hostname = prevOpen, prevSleep, prevHost
		os.Stdout, os.Stderr = prevOut, prevErr
		devnull.Close()
	})
	return opened, slept
}

func login(t *testing.T, srv *httptest.Server, extra ...string) error {
	t.Helper()
	args := append([]string{"--url", "https://webhooks.example", "--account-url", srv.URL}, extra...)
	return cmdLogin(context.Background(), args, "", "")
}

func TestLoginSavesTheKeyAfterApproval(t *testing.T) {
	opened, slept := isolate(t)
	id, srv := newIdentity(t, "pending", "slow", "ok")

	if err := login(t, srv); err != nil {
		t.Fatalf("login: %v", err)
	}

	st, err := loadStore()
	if err != nil {
		t.Fatal(err)
	}
	got := st.Profiles["default"]
	want := profile{
		URL: "https://webhooks.example", Kind: credentials.KindLogin, APIKey: fakeKey("key_1"),
		AccountURL: srv.URL, KeyID: "key_1", KeyName: "nn-webhooks on box", WorkspaceID: "org_1",
		WorkspaceName: "Acme", Scope: "admin", ExpiresAt: "2027-01-17T10:00:00Z",
	}
	if got != want {
		t.Fatalf("profile = %+v\nwant %+v", got, want)
	}
	if strings.Join(*opened, " ") != "https://account.example/device?code=BCDF-GHJK" {
		t.Fatalf("opened %v", *opened)
	}
	// 5 s, 5 s, then 10 s after slow_down.
	if fmt.Sprint(*slept) != "[5s 5s 10s]" {
		t.Fatalf("slept %v", *slept)
	}
	start := id.forms["/api/v1/oauth/device_authorization"][0]
	if start.Get("client_id") != "nn-webhooks" || start.Get("device_name") != "box" || start.Has("scope") {
		t.Fatalf("device_authorization form %v", start)
	}
	poll := id.forms["/api/v1/oauth/token"][0]
	if poll.Get("grant_type") != deviceCodeGrant || poll.Get("device_code") != "dev-code" {
		t.Fatalf("token form %v", poll)
	}
	info, err := os.Stat(os.Getenv("XDG_CONFIG_HOME") + "/nn-webhooks/credentials.json")
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file mode %v, %v", info.Mode().Perm(), err)
	}
}

func TestLoginAsksForTheScopeAndNameGiven(t *testing.T) {
	isolate(t)
	id, srv := newIdentity(t, "ok")
	if err := login(t, srv, "--scope", "read", "--name", "ci-box", "--no-browser"); err != nil {
		t.Fatal(err)
	}
	start := id.forms["/api/v1/oauth/device_authorization"][0]
	if start.Get("scope") != "read" || start.Get("device_name") != "ci-box" {
		t.Fatalf("form %v", start)
	}
}

func TestNoBrowserOpensNothing(t *testing.T) {
	opened, _ := isolate(t)
	_, srv := newIdentity(t, "ok")
	if err := login(t, srv, "--no-browser"); err != nil {
		t.Fatal(err)
	}
	if len(*opened) != 0 {
		t.Fatalf("opened %v", *opened)
	}
}

func TestADeniedSignInSavesNothing(t *testing.T) {
	isolate(t)
	_, srv := newIdentity(t, "pending", "denied")
	err := login(t, srv)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("err = %v", err)
	}
	st, _ := loadStore()
	if len(st.Profiles) != 0 {
		t.Fatalf("profiles %v", st.Profiles)
	}
}

func TestAnExpiredCodeSaysToRunLoginAgain(t *testing.T) {
	isolate(t)
	_, srv := newIdentity(t, "expired")
	err := login(t, srv)
	if err == nil || !strings.Contains(err.Error(), "run 'nn-webhooks login' again") {
		t.Fatalf("err = %v", err)
	}
}

func TestCancellingStopsTheWait(t *testing.T) {
	isolate(t)
	_, srv := newIdentity(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := cmdLogin(ctx, []string{"--url", "https://webhooks.example", "--account-url", srv.URL}, "", "")
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestLoggingInAgainRevokesTheProfilesPreviousKey(t *testing.T) {
	isolate(t)
	id, srv := newIdentity(t, "ok", "ok")
	if err := login(t, srv); err != nil {
		t.Fatal(err)
	}
	if err := login(t, srv); err != nil {
		t.Fatal(err)
	}
	revokes := id.forms["/api/v1/oauth/revoke"]
	if len(revokes) != 1 || revokes[0].Get("token") != fakeKey("key_1") || revokes[0].Get("client_id") != "nn-webhooks" {
		t.Fatalf("revokes %v", revokes)
	}
}

func TestLogoutRevokesThenDeletesTheProfile(t *testing.T) {
	isolate(t)
	id, srv := newIdentity(t, "ok")
	if err := login(t, srv); err != nil {
		t.Fatal(err)
	}
	if err := cmdLogout(context.Background(), nil, ""); err != nil {
		t.Fatal(err)
	}
	if len(id.forms["/api/v1/oauth/revoke"]) != 1 {
		t.Fatalf("revokes %v", id.forms["/api/v1/oauth/revoke"])
	}
	st, _ := loadStore()
	if _, ok := st.Profiles["default"]; ok {
		t.Fatal("profile kept")
	}
}

func TestLogoutKeepsTheProfileWhenTheKeyCannotBeRevoked(t *testing.T) {
	isolate(t)
	id, srv := newIdentity(t, "ok")
	if err := login(t, srv); err != nil {
		t.Fatal(err)
	}
	id.revokeOK = false
	if err := cmdLogout(context.Background(), nil, ""); err == nil {
		t.Fatal("expected an error")
	}
	st, _ := loadStore()
	if _, ok := st.Profiles["default"]; !ok {
		t.Fatal("profile deleted while its key is still live")
	}
}

func TestLogoutOfAConfiguredProfileLeavesItsKeyAlone(t *testing.T) {
	isolate(t)
	id, _ := newIdentity(t)
	st, _ := loadStore()
	st.Profiles["ci"] = profile{URL: "https://webhooks.example", Kind: credentials.KindAPIKey, APIKey: "eyJ.x.y"}
	if _, err := saveStore(st); err != nil {
		t.Fatal(err)
	}
	if err := cmdLogout(context.Background(), nil, "ci"); err != nil {
		t.Fatal(err)
	}
	if len(id.forms) != 0 {
		t.Fatalf("called Identity: %v", id.forms)
	}
	st, _ = loadStore()
	if len(st.Profiles) != 0 {
		t.Fatalf("profiles %v", st.Profiles)
	}
}

func TestWhoamiDescribesASignedInProfile(t *testing.T) {
	isolate(t)
	_, srv := newIdentity(t, "ok")
	if err := login(t, srv); err != nil {
		t.Fatal(err)
	}
	cred, err := resolveCredentials("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	got := describe(cred, time.Unix(1700000000, 0))
	for field, want := range map[string]any{
		"kind": "login", "source": `profile "default"`, "key_name": "nn-webhooks on box",
		"workspace": "Acme", "workspace_id": "org_1", "key_id": "key_1", "scope": "admin",
		"expires_at": "2027-01-15T08:00:00Z", "expired": false,
	} {
		if got[field] != want {
			t.Errorf("%s = %v, want %v", field, got[field], want)
		}
	}
	if strings.Contains(fmt.Sprint(got), fakeKey("key_1")) {
		t.Fatal("whoami printed the key")
	}
}

func TestTheAccountConsoleIsDerivedFromTheWebhooksHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://webhooks.nimbusnexus.net":      "https://account.nimbusnexus.net",
		"https://webhooks.dev.nimbusnexus.net/": "https://account.dev.nimbusnexus.net",
		"http://webhooks.localhost:8080/v1":     "http://account.localhost:8080",
		"https://hooks.example.com":             "",
		"not a url":                             "",
	} {
		got, err := deriveAccountURL(in)
		if want == "" {
			if err == nil {
				t.Errorf("%s: want an error, got %s", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestBothOAuthErrorShapesAreRead(t *testing.T) {
	if e := parseOAuthError([]byte(`{"error":"slow_down","error_description":"d"}`)); e == nil || e.Code != "slow_down" {
		t.Fatalf("standard: %v", e)
	}
	if e := parseOAuthError([]byte(`{"detail":{"error":"invalid_client"}}`)); e == nil || e.Code != "invalid_client" {
		t.Fatalf("legacy: %v", e)
	}
	if e := parseOAuthError([]byte(`{"error":{"code":"not_found"}}`)); e != nil {
		t.Fatalf("envelope read as OAuth: %v", e)
	}
}
