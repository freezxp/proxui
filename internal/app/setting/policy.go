// Package setting reads runtime configuration on behalf of the layers that
// act on it, so those layers depend on a question rather than on a table.
package setting

import (
	"context"
	"encoding/json"

	"github.com/rs/zerolog"

	domain "github.com/freezxp/proxui/internal/domain/setting"
)

// Store reads the stored overrides.
type Store interface {
	All(ctx context.Context) (map[string]json.RawMessage, error)
}

// Policy answers configuration questions.
//
// Read per call rather than cached. An administrator switching registration
// off is usually doing it because something is happening now, and a cache
// would make that take effect at some unspecified later moment.
type Policy struct {
	Settings Store
	Log      zerolog.Logger
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
