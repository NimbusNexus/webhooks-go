package webhooks

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// writeProfiles writes a credentials file the way `nn-webhooks login` and `configure` do.
func writeProfiles(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("NN_WEBHOOKS_URL", "")
	t.Setenv("NN_WEBHOOKS_API_KEY", "")
	t.Setenv("NN_WEBHOOKS_PROFILE", "")
	if err := os.MkdirAll(filepath.Join(dir, "nn-webhooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nn-webhooks", "credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNewWithNoKeyUsesTheSignedInProfile(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"next_offset":null}`))
	}))
	defer srv.Close()
	writeProfiles(t, `{"profiles":{"default":{"url":"`+srv.URL+`/","kind":"login","api_key":"eyJ.saved.key"}}}`)

	c := New("", "")
	if _, err := c.ListEndpoints(bg(), nil); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer eyJ.saved.key" {
		t.Fatalf("Authorization = %q", auth)
	}
}

func TestAnExplicitKeyIsNeverReplaced(t *testing.T) {
	writeProfiles(t, `{"profiles":{"default":{"url":"https://saved","kind":"login","api_key":"saved"}}}`)
	c := New("https://given", "given")
	if c.baseURL != "https://given" || c.apiKey != "given" {
		t.Fatalf("got %s %s", c.baseURL, c.apiKey)
	}
}

func TestTheEnvironmentWinsOverTheProfile(t *testing.T) {
	writeProfiles(t, `{"profiles":{"default":{"url":"https://saved","kind":"login","api_key":"saved"}}}`)
	t.Setenv("NN_WEBHOOKS_API_KEY", "from-env")
	found, err := LoadCredentials("")
	if err != nil {
		t.Fatal(err)
	}
	if found.APIKey != "from-env" || found.Source != "$NN_WEBHOOKS_API_KEY" || found.URL != "https://saved" {
		t.Fatalf("got %+v", found)
	}
}

func TestWithProfilePicksTheProfile(t *testing.T) {
	writeProfiles(t, `{"profiles":{
		"default":{"url":"https://prod","kind":"login","api_key":"prod"},
		"staging":{"url":"https://staging","kind":"api_key","api_key":"stg"}}}`)
	c := New("", "", WithProfile("staging"))
	if c.baseURL != "https://staging" || c.apiKey != "stg" {
		t.Fatalf("got %s %s", c.baseURL, c.apiKey)
	}
	t.Setenv("NN_WEBHOOKS_PROFILE", "staging")
	found, _ := LoadCredentials("")
	if found.Profile != "staging" || found.Source != `profile "staging"` {
		t.Fatalf("got %+v", found)
	}
}

func TestACorruptFileIsAnErrorNotSilence(t *testing.T) {
	writeProfiles(t, `{not json`)
	if _, err := LoadCredentials(""); err == nil {
		t.Fatal("expected an error")
	}
}
