package main

// Credential resolution. The file itself (named profiles, 0600) is go/internal/credentials, which
// the Go SDK reads too.
//
// SOURCE is carried through resolution so `whoami` can report WHICH input won. "Authenticated" on
// its own is unactionable when a stale environment variable is silently shadowing the profile
// someone just wrote, and that ordering makes it possible.

import (
	"fmt"
	"os"

	"github.com/NimbusNexus/webhooks-go/internal/credentials"
)

const (
	// The `NN_WEBHOOKS_` prefix, not `WEBHOOKD_`. webhookd is the daemon; Webhooks is the product,
	// and an environment variable a customer exports is as user-facing as a command name.
	envAPIKey = credentials.EnvAPIKey
	envURL    = credentials.EnvURL
)

type profile = credentials.Profile

type store = credentials.Store

func loadStore() (store, error) { return credentials.Load() }

func saveStore(s store) (string, error) { return credentials.Save(s) }

// profileName picks the profile: flag, then env, then "default".
func profileName(flagProfile string) string { return credentials.ProfileName(flagProfile) }

// resolved is the credential a command will use, and where it came from.
type resolved struct {
	URL     string
	Kind    string
	APIKey  string
	Source  string // "--api-key", "$NN_WEBHOOKS_API_KEY", or "profile \"name\""
	Profile string
	// Stored is the profile the key came from; zero unless Source is the profile.
	Stored profile
}

// resolveCredentials returns the effective credential. Flag > environment > stored profile, which
// is the order of how explicitly the caller asked for it — and an env var beating the file is what
// lets CI work with no config file at all.
//
// The error names every way to fix it rather than only the one this command happens to prefer,
// because which is appropriate depends on whether a human or a pipeline hit it.
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

	out := resolved{URL: url, Profile: name, Kind: credentials.KindAPIKey}
	switch {
	case flagKey != "":
		out.APIKey, out.Source = flagKey, "--api-key"
	case os.Getenv(envAPIKey) != "":
		out.APIKey, out.Source = os.Getenv(envAPIKey), "$"+envAPIKey
	case hasStored && stored.APIKey != "":
		out.APIKey, out.Source = stored.APIKey, fmt.Sprintf("profile %q", name)
		out.Stored = stored
		if stored.Kind != "" {
			out.Kind = stored.Kind
		}
	}

	if out.URL == "" {
		return resolved{}, fmt.Errorf(
			"no base URL configured; pass --url, set %s, or run 'nn-webhooks login'", envURL)
	}
	if out.APIKey == "" {
		return resolved{}, fmt.Errorf(
			"not signed in; run 'nn-webhooks login', pass --api-key, set %s, or run 'nn-webhooks configure'",
			envAPIKey)
	}
	return out, nil
}
