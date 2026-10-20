// Package credentials is the on-disk profile store nn-webhooks writes and the Go SDK reads:
// $XDG_CONFIG_HOME/nn-webhooks/credentials.json (else ~/.config/...), mode 0600, named profiles.
//
// The Python and TypeScript SDKs read the same file; keep the three in step.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	EnvAPIKey  = "NN_WEBHOOKS_API_KEY"
	EnvURL     = "NN_WEBHOOKS_URL"
	EnvProfile = "NN_WEBHOOKS_PROFILE"

	DefaultProfile = "default"

	// KindAPIKey is a key pasted into `nn-webhooks configure`.
	KindAPIKey = "api_key"
	// KindLogin is a key `nn-webhooks login` received; `logout` revokes it (RFC-0024).
	KindLogin = "login"
)

// Profile is one deployment plus how to authenticate to it. The fields after APIKey are set by
// `nn-webhooks login` only.
type Profile struct {
	URL    string `json:"url"`
	Kind   string `json:"kind"`
	APIKey string `json:"api_key"`

	AccountURL    string `json:"account_url,omitempty"`
	KeyID         string `json:"key_id,omitempty"`
	KeyName       string `json:"key_name,omitempty"`
	WorkspaceID   string `json:"workspace_id,omitempty"`
	WorkspaceName string `json:"workspace_name,omitempty"`
	Project       string `json:"project,omitempty"`
	Scope         string `json:"scope,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
}

// Store is the file's shape: named profiles, nothing else.
type Store struct {
	Profiles map[string]Profile `json:"profiles"`
}

// Path returns $XDG_CONFIG_HOME/nn-webhooks/credentials.json, else ~/.config/nn-webhooks/....
func Path() (string, error) {
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

// Load reads the file. A missing file is an empty store; a corrupt one is an error.
func Load() (Store, error) {
	s := Store{Profiles: map[string]Profile{}}
	path, err := Path()
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
		s.Profiles = map[string]Profile{}
	}
	return s, nil
}

// Save writes the store 0600, creating the directory 0700, and returns the path.
func Save(s Store) (string, error) {
	path, err := Path()
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
	// WriteFile keeps an existing file's mode; a file created looser than 0600 is tightened here.
	if err := os.Chmod(path, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// ProfileName picks the profile: the explicit name, then $NN_WEBHOOKS_PROFILE, then "default".
func ProfileName(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if p := os.Getenv(EnvProfile); p != "" {
		return p
	}
	return DefaultProfile
}
