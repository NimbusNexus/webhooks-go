package webhooks

import (
	"fmt"
	"os"

	"github.com/NimbusNexus/webhooks-go/internal/credentials"
)

// Credentials is a base URL and API key, found the way the nn-webhooks CLI finds them.
type Credentials struct {
	URL    string
	APIKey string
	// Profile is the credentials-file profile that was consulted.
	Profile string
	// Source is where the key came from: "$NN_WEBHOOKS_API_KEY" or `profile "<name>"`.
	Source string
}

// LoadCredentials resolves a base URL and API key from $NN_WEBHOOKS_URL and $NN_WEBHOOKS_API_KEY,
// then from a profile in the file `nn-webhooks configure` and `nn-webhooks login` write. profile
// names it; empty means $NN_WEBHOOKS_PROFILE, else "default". Either value may come back empty.
func LoadCredentials(profile string) (Credentials, error) {
	name := credentials.ProfileName(profile)
	out := Credentials{URL: os.Getenv(credentials.EnvURL), Profile: name}
	if key := os.Getenv(credentials.EnvAPIKey); key != "" {
		out.APIKey, out.Source = key, "$"+credentials.EnvAPIKey
	}
	if out.URL != "" && out.APIKey != "" {
		return out, nil
	}
	store, err := credentials.Load()
	if err != nil {
		return out, err
	}
	stored, ok := store.Profiles[name]
	if !ok {
		return out, nil
	}
	if out.URL == "" {
		out.URL = stored.URL
	}
	if out.APIKey == "" && stored.APIKey != "" {
		out.APIKey, out.Source = stored.APIKey, fmt.Sprintf("profile %q", name)
	}
	return out, nil
}
