package main

// Credential resolution and the on-disk store.
//
// Ported from the Python CLI this command replaced, which had two things worth keeping when the
// two were consolidated: NAMED PROFILES and a REPORTABLE SOURCE. Everything else (flag > env >
// file precedence, 0600) this command already did.
//
// PROFILES exist because one credential is the unusual case. Anyone with a staging deployment
// needs two, and without profiles the choice is re-running `configure` between environments —
// which is how a production key ends up used against staging, or worse.
//
// SOURCE is carried through resolution so `status` can report WHICH input won. "Authenticated" on
// its own is unactionable when a stale environment variable is silently shadowing the profile
// someone just wrote, and that ordering makes it possible.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// The `NN_WEBHOOKS_` prefix, not `WEBHOOKD_`. webhookd is the daemon; Webhooks is the product,
	// and an environment variable a customer exports is as user-facing as a command name.
	envAPIKey  = "NN_WEBHOOKS_API_KEY"
	envURL     = "NN_WEBHOOKS_URL"
	envProfile = "NN_WEBHOOKS_PROFILE"

	defaultProfile = "default"
)

// profile is one deployment plus how to authenticate to it.
type profile struct {
	URL string `json:"url"`
	// "api_key" today. Human sign-in would add "token": Identity authenticates people with a
	// browser cookie and mints nothing a CLI can hold, so there is no second kind to store yet.
	// Recorded anyway — a store that can only describe one kind needs migrating the moment the
	// second exists, and every profile written before then is ambiguous.
	Kind   string `json:"kind"`
	APIKey string `json:"api_key"`
}

// store is the on-disk shape: named profiles, nothing else.
type store struct {
	Profiles map[string]profile `json:"profiles"`
}

// configPath returns $XDG_CONFIG_HOME/nn-webhooks/credentials.json, falling back to
// ~/.config/... when XDG_CONFIG_HOME is unset.
//
// XDG rather than the old ~/.webhookd/: it sits with other tool config, is trivial to exclude from
// a dotfile repo, and honours a machine that has moved its config root. This file holds live
// credentials, so where it lands is not cosmetic.
func configPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "nn-webhooks", "credentials.json"), nil
}

// loadStore reads the credentials file. A MISSING file is an empty store, not an error — running
// a command before configuring anything should say "not configured", not fail on a path that was
// never expected to exist. A CORRUPT file DOES error: treating unreadable credentials as absent
// sends someone chasing an auth problem that is really a broken file.
func loadStore() (store, error) {
	s := store{Profiles: map[string]profile{}}
	path, err := configPath()
	if err != nil {
		return s, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	if s.Profiles == nil {
		s.Profiles = map[string]profile{}
	}
	return s, nil
}

// saveStore writes the store 0600, creating the directory 0700.
//
// The mode is set explicitly rather than left to umask, which varies by machine: a credentials
// file that lands world-readable is the sort of thing nobody notices until it matters.
func saveStore(s store) (string, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// resolved is the credential a command will use, and where it came from.
type resolved struct {
	URL     string
	Kind    string
	APIKey  string
	Source  string // "--api-key", "$NN_WEBHOOKS_API_KEY", or "profile \"name\""
	Profile string
}

// profileName picks the profile: flag, then env, then "default".
func profileName(flagProfile string) string {
	if flagProfile != "" {
		return flagProfile
	}
	if p := os.Getenv(envProfile); p != "" {
		return p
	}
	return defaultProfile
}

// resolveCredentials returns the effective credential. Flag > environment > stored profile, which
// is the order of how explicitly the caller asked for it — and an env var beating the file is what
// lets CI work with no config file at all.
//
// The error names all three ways to fix it rather than only the one this command happens to
// prefer, because which is appropriate depends on whether a human or a pipeline hit it.
func resolveCredentials(flagURL, flagKey, flagProfile string) (resolved, error) {
	name := profileName(flagProfile)
	s, err := loadStore()
	if err != nil {
		return resolved{}, err
	}
	stored, hasStored := s.Profiles[name]

	url := firstNonEmpty(flagURL, os.Getenv(envURL))
	if url == "" && hasStored {
		url = stored.URL
	}

	out := resolved{URL: url, Profile: name, Kind: "api_key"}
	switch {
	case flagKey != "":
		out.APIKey, out.Source = flagKey, "--api-key"
	case os.Getenv(envAPIKey) != "":
		out.APIKey, out.Source = os.Getenv(envAPIKey), "$"+envAPIKey
	case hasStored && stored.APIKey != "":
		out.APIKey, out.Source = stored.APIKey, fmt.Sprintf("profile %q", name)
		if stored.Kind != "" {
			out.Kind = stored.Kind
		}
	}

	if out.URL == "" {
		return resolved{}, fmt.Errorf(
			"no base URL configured; pass --url, set %s, or run 'nn-webhooks configure'", envURL)
	}
	if out.APIKey == "" {
		return resolved{}, fmt.Errorf(
			"no API key configured; pass --api-key, set %s, or run 'nn-webhooks configure'", envAPIKey)
	}
	return out, nil
}
