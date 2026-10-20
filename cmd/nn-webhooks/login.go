package main

// `login` and `logout`: an OAuth device-authorization grant (RFC 8628) served by NimbusNexus
// Identity on the account console's host (RFC-0024). The browser approves; the CLI receives a
// named, 90-day API key and keeps it in the profile `configure` writes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/NimbusNexus/webhooks-go/internal/credentials"
)

const (
	clientID         = "nn-webhooks"
	defaultURL       = "https://webhooks.nimbusnexus.net"
	deviceCodeGrant  = "urn:ietf:params:oauth:grant-type:device_code"
	slowDownInterval = 5 * time.Second
)

// Swapped by tests.
var (
	httpClient  = &http.Client{Timeout: 30 * time.Second}
	openBrowser = defaultOpenBrowser
	sleep       = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	hostname = os.Hostname
)

type deviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type deviceToken struct {
	AccessToken   string  `json:"access_token"`
	Scope         string  `json:"scope"`
	KeyID         string  `json:"key_id"`
	KeyName       string  `json:"key_name"`
	Project       *string `json:"project"`
	WorkspaceID   string  `json:"workspace_id"`
	WorkspaceName string  `json:"workspace_name"`
	ExpiresAt     string  `json:"expires_at"`
}

// oauthError is an RFC 6749 §5.2 body; Identity's legacy `{"detail": {"error": ...}}` is read too.
type oauthError struct {
	Code        string
	Description string
}

func (e *oauthError) Error() string {
	if e.Description != "" {
		return e.Code + ": " + e.Description
	}
	return e.Code
}

func cmdLogin(ctx context.Context, args []string, gURL, gProfile string) error {
	fs := newFlagSet("nn-webhooks login", "Usage: nn-webhooks login [--url URL] [--account-url URL] [--scope admin|publish|read] [--name NAME] [--no-browser]\n\nSigns in through the browser and saves a named, revocable API key to the profile.")
	urlFlag := fs.String("url", "", "Webhooks base URL (default "+defaultURL+")")
	accountFlag := fs.String("account-url", "", "account console URL (default: derived from the Webhooks URL)")
	scope := fs.String("scope", "", "permissions to ask for: admin (default), publish or read")
	name := fs.String("name", "", "name for this device in the key's name (default: the hostname)")
	noBrowser := fs.Bool("no-browser", false, "print the sign-in URL instead of opening a browser")
	mustParse(fs, args)

	profileKey := profileName(gProfile)
	st, err := loadStore()
	if err != nil {
		return err
	}
	previous, hadPrevious := st.Profiles[profileKey]

	baseURL := firstNonEmpty(*urlFlag, gURL, os.Getenv(envURL), previous.URL, defaultURL)
	accountURL := strings.TrimRight(*accountFlag, "/")
	if accountURL == "" {
		if accountURL, err = deriveAccountURL(baseURL); err != nil {
			return err
		}
	}
	device := *name
	if device == "" {
		device, _ = hostname()
	}

	form := url.Values{"client_id": {clientID}, "device_name": {device}}
	if *scope != "" {
		form.Set("scope", *scope)
	}
	var auth deviceAuthorization
	if err := postForm(ctx, accountURL+"/api/v1/oauth/device_authorization", form, &auth); err != nil {
		return fmt.Errorf("could not start the sign-in at %s: %w", accountURL, err)
	}

	fmt.Fprintf(os.Stderr, "To sign in, open %s and enter the code %s\n", auth.VerificationURI, auth.UserCode)
	page := auth.VerificationURIComplete
	if !*noBrowser && (strings.HasPrefix(page, "https://") || strings.HasPrefix(page, "http://")) {
		if err := openBrowser(page); err == nil {
			fmt.Fprintln(os.Stderr, "Opening your browser… (pass --no-browser to skip)")
		}
	}
	fmt.Fprintln(os.Stderr, "Waiting for approval…")

	token, err := pollForToken(ctx, accountURL, auth)
	if err != nil {
		return err
	}

	if hadPrevious && previous.Kind == credentials.KindLogin && previous.APIKey != "" && previous.AccountURL != "" {
		// The profile's earlier sign-in is replaced, so its key is ended rather than orphaned.
		if _, err := revokeKey(ctx, previous.AccountURL, previous.APIKey); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not revoke the previous key %q: %v\n", previous.KeyName, err)
		}
	}

	stored := profile{
		URL:           baseURL,
		Kind:          credentials.KindLogin,
		APIKey:        token.AccessToken,
		AccountURL:    accountURL,
		KeyID:         token.KeyID,
		KeyName:       token.KeyName,
		WorkspaceID:   token.WorkspaceID,
		WorkspaceName: token.WorkspaceName,
		Scope:         token.Scope,
		ExpiresAt:     token.ExpiresAt,
	}
	if token.Project != nil {
		stored.Project = *token.Project
	}
	st.Profiles[profileKey] = stored
	path, err := saveStore(st)
	if err != nil {
		return err
	}
	if os.Getenv(envAPIKey) != "" {
		fmt.Fprintf(os.Stderr, "warning: $%s is set and takes precedence over this profile; unset it to use the new sign-in\n", envAPIKey)
	}
	return printJSON(map[string]string{
		"profile":     profileKey,
		"url":         baseURL,
		"workspace":   token.WorkspaceName,
		"key_name":    token.KeyName,
		"scope":       token.Scope,
		"expires_at":  token.ExpiresAt,
		"config_path": path,
	})
}

// pollForToken polls the token endpoint until the request is approved, denied or expired.
func pollForToken(ctx context.Context, accountURL string, auth deviceAuthorization) (deviceToken, error) {
	interval := time.Duration(auth.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	lifetime := time.Duration(auth.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = 15 * time.Minute
	}
	deadline := time.Now().Add(lifetime)
	form := url.Values{"grant_type": {deviceCodeGrant}, "device_code": {auth.DeviceCode}, "client_id": {clientID}}
	for {
		if err := sleep(ctx, interval); err != nil {
			return deviceToken{}, errors.New("sign-in cancelled")
		}
		var token deviceToken
		err := postForm(ctx, accountURL+"/api/v1/oauth/token", form, &token)
		if err == nil {
			return token, nil
		}
		var refusal *oauthError
		if !errors.As(err, &refusal) {
			if ctx.Err() != nil {
				return deviceToken{}, errors.New("sign-in cancelled")
			}
			if time.Now().After(deadline) {
				return deviceToken{}, fmt.Errorf("sign-in expired: %w", err)
			}
			// A dropped connection is retried at the same pace until the code expires.
			continue
		}
		switch refusal.Code {
		case "authorization_pending":
		case "slow_down":
			interval += slowDownInterval
		case "access_denied":
			return deviceToken{}, errors.New("the sign-in was denied in the browser")
		case "expired_token":
			return deviceToken{}, errors.New("the code expired before it was approved; run 'nn-webhooks login' again")
		default:
			return deviceToken{}, refusal
		}
	}
}

func cmdLogout(ctx context.Context, args []string, gProfile string) error {
	fs := newFlagSet("nn-webhooks logout", "Usage: nn-webhooks logout\n\nRevokes the API key 'nn-webhooks login' saved, then deletes the profile. A profile written by\n'configure' is deleted and its key left as it is.")
	mustParse(fs, args)

	name := profileName(gProfile)
	st, err := loadStore()
	if err != nil {
		return err
	}
	stored, ok := st.Profiles[name]
	if !ok {
		return fmt.Errorf("no profile %q to sign out of", name)
	}
	out := map[string]any{"profile": name, "revoked": false, "removed": true}
	if stored.Kind == credentials.KindLogin && stored.APIKey != "" && stored.AccountURL != "" {
		revoked, err := revokeKey(ctx, stored.AccountURL, stored.APIKey)
		if err != nil {
			return fmt.Errorf("could not revoke the key at %s, so the profile was kept: %w", stored.AccountURL, err)
		}
		out["revoked"] = revoked
	} else {
		out["note"] = "the key itself is still valid; revoke it in the account console under API keys"
	}
	delete(st.Profiles, name)
	if _, err := saveStore(st); err != nil {
		return err
	}
	return printJSON(out)
}

// revokeKey ends a key `login` minted (RFC 7009). It reports whether the key is revoked now.
func revokeKey(ctx context.Context, accountURL, apiKey string) (bool, error) {
	var answer struct {
		Revoked bool `json:"revoked"`
	}
	form := url.Values{"token": {apiKey}, "client_id": {clientID}}
	if err := postForm(ctx, accountURL+"/api/v1/oauth/revoke", form, &answer); err != nil {
		return false, err
	}
	return answer.Revoked, nil
}

// deriveAccountURL maps https://webhooks.<domain> to https://account.<domain>, port and scheme kept.
func deriveAccountURL(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid Webhooks URL %q", baseURL)
	}
	host, port := u.Hostname(), u.Port()
	rest, ok := strings.CutPrefix(host, "webhooks.")
	if !ok {
		return "", fmt.Errorf("cannot tell the account console from %s; pass --account-url", baseURL)
	}
	host = "account." + rest
	if port != "" {
		host += ":" + port
	}
	return u.Scheme + "://" + host, nil
}

// postForm POSTs a form and decodes a 2xx JSON answer into out. A 4xx/5xx with an OAuth error body
// returns *oauthError.
func postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return json.Unmarshal(body, out)
	}
	if refusal := parseOAuthError(body); refusal != nil {
		return refusal
	}
	return fmt.Errorf("HTTP %d from %s", resp.StatusCode, endpoint)
}

func parseOAuthError(body []byte) *oauthError {
	var flat struct {
		Error       any    `json:"error"`
		Description string `json:"error_description"`
		Detail      struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		} `json:"detail"`
	}
	if json.Unmarshal(body, &flat) != nil {
		return nil
	}
	if code, ok := flat.Error.(string); ok && code != "" {
		return &oauthError{Code: code, Description: flat.Description}
	}
	if flat.Detail.Error != "" {
		return &oauthError{Code: flat.Detail.Error, Description: flat.Detail.Description}
	}
	return nil
}

func defaultOpenBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
