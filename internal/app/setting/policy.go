// Package setting reads runtime configuration on behalf of the layers that
// act on it, so those layers depend on a question rather than on a table.
package setting

import (
	"context"
	"encoding/json"

	"github.com/rs/zerolog"

	domain "github.com/freezxp/proxui/internal/domain/setting"
	"github.com/freezxp/proxui/internal/infra/crypto"
	"github.com/freezxp/proxui/internal/infra/oauth"
)

// Store reads the stored overrides.
type Store interface {
	All(ctx context.Context) (map[string]json.RawMessage, error)
	Secret(ctx context.Context, key string, vault *crypto.Vault) (string, error)
}

// Policy answers configuration questions.
//
// Read per call rather than cached. An administrator switching registration
// off is usually doing it because something is happening now, and a cache
// would make that take effect at some unspecified later moment.
type Policy struct {
	Settings Store
	Vault    *crypto.Vault
	Log      zerolog.Logger

	// Fallback is the configuration supplied by the environment. Settings win
	// where they are set, so a deployment can start configured and still be
	// changed from the UI later.
	Fallback oauth.Config
}

// GoogleConfig resolves the Google client configuration.
//
// Read per call, so a credential corrected in Settings takes effect on the
// next attempt rather than at the next restart — which matters because the
// usual mistake here, a mismatched redirect URL, is only discovered by trying.
func (p *Policy) GoogleConfig(ctx context.Context) oauth.Config {
	cfg := p.Fallback
	if p == nil || p.Settings == nil {
		return cfg
	}

	stored, err := p.Settings.All(ctx)
	if err != nil {
		p.Log.Error().Err(err).Msg("could not read the Google settings; using the environment")
		return cfg
	}
	for _, v := range domain.Resolve(stored) {
		switch v.Key {
		case "auth.google_client_id":
			if v.Text != "" {
				cfg.ClientID = v.Text
			}
		case "auth.google_redirect_url":
			if v.Text != "" {
				cfg.RedirectURL = v.Text
			}
		case "auth.google_client_secret":
			if !v.HasValue {
				continue
			}
			secret, err := p.Settings.Secret(ctx, v.Key, p.Vault)
			if err != nil {
				// A secret that cannot be decrypted usually means the master
				// key changed. Saying so beats "google sign-in is not
				// configured", which sends someone to re-enter a client ID
				// that was never the problem.
				p.Log.Error().Err(err).Msg("could not decrypt the Google client secret")
				continue
			}
			if secret != "" {
				cfg.ClientSecret = secret
			}
		}
	}
	return cfg
}

// SelfRegistrationEnabled reports whether people may create their own account.
//
// Fails closed: if the settings cannot be read, registration is treated as
// disabled. The alternative — a database hiccup opening the portal to new
// accounts — is the wrong way round to be wrong.
func (p *Policy) SelfRegistrationEnabled(ctx context.Context) bool {
	if p == nil || p.Settings == nil {
		return false
	}
	stored, err := p.Settings.All(ctx)
	if err != nil {
		p.Log.Error().Err(err).Msg("could not read the registration setting; treating it as disabled")
		return false
	}
	for _, v := range domain.Resolve(stored) {
		if v.Key == "auth.self_registration" {
			return v.Text == domain.RegistrationOpen
		}
	}
	return false
}
