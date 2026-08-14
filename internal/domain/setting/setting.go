// Package setting is the Configuration bounded context: the handful of knobs
// an administrator can turn without a restart (ADM-02).
package setting

import (
	"errors"
	"fmt"
)

// ErrUnknownKey rejects a key the portal does not read, which would otherwise
// be stored forever and silently ignored.
var ErrUnknownKey = errors.New("setting: unknown key")

// ErrOutOfRange rejects a value that would break the thing it configures.
var ErrOutOfRange = errors.New("setting: value out of range")

// Kind is how a value should be entered and shown.
type Kind string

const (
	KindDuration Kind = "duration_s"
	KindCount    Kind = "count"
	KindDays     Kind = "days"
)

// Definition describes one setting: what it means, and what it may be.
//
// The catalogue is declared in code rather than seeded into the table, so a
// setting cannot exist without something reading it, and the UI can render an
// explanation beside each field.
type Definition struct {
	Key     string `json:"key"`
	Group   string `json:"group"`
	Label   string `json:"label"`
	Help    string `json:"help"`
	Kind    Kind   `json:"kind"`
	Default int    `json:"default"`
	Min     int    `json:"min"`
	Max     int    `json:"max"`
}

// Validate checks a proposed value.
func (d Definition) Validate(value int) error {
	if value < d.Min || value > d.Max {
		return fmt.Errorf("%w: %s must be between %d and %d", ErrOutOfRange, d.Label, d.Min, d.Max)
	}
	return nil
}

// Catalogue is every setting the portal reads. Adding one here is what makes
// it settable; nothing else needs to change.
var Catalogue = []Definition{
	{
		Key: "sync.inventory_interval_s", Group: "Synchronization",
		Label: "Inventory interval", Kind: KindDuration, Default: 60, Min: 30, Max: 3600,
		Help: "How often each platform is asked what it has. Applies to platforms without their own override.",
	},
	{
		Key: "sync.metrics_interval_s", Group: "Synchronization",
		Label: "Metrics interval", Kind: KindDuration, Default: 60, Min: 30, Max: 3600,
		Help: "How often performance samples are collected. Shorter intervals mean more storage.",
	},
	{
		Key: "session.access_token_ttl_s", Group: "Sessions & security",
		Label: "Access token lifetime", Kind: KindDuration, Default: 900, Min: 300, Max: 3600,
		Help: "How long a signed-in browser goes before silently refreshing.",
	},
	{
		Key: "session.refresh_token_ttl_s", Group: "Sessions & security",
		Label: "Session lifetime", Kind: KindDuration, Default: 604800, Min: 3600, Max: 2592000,
		Help: "How long someone stays signed in without using the portal.",
	},
	{
		Key: "session.lockout_threshold", Group: "Sessions & security",
		Label: "Failed sign-ins before lockout", Kind: KindCount, Default: 5, Min: 3, Max: 20,
		Help: "Consecutive failures before an account is temporarily locked.",
	},
	{
		Key: "console.idle_timeout_s", Group: "Sessions & security",
		Label: "Console idle timeout", Kind: KindDuration, Default: 1800, Min: 300, Max: 7200,
		Help: "A console with no keyboard or screen activity for this long is closed.",
	},
	{
		Key: "console.max_duration_s", Group: "Sessions & security",
		Label: "Console maximum length", Kind: KindDuration, Default: 14400, Min: 900, Max: 86400,
		Help: "A console is closed after this long regardless of activity.",
	},
	{
		Key: "retention.metrics_days", Group: "Retention",
		Label: "Raw metrics", Kind: KindDays, Default: 7, Min: 1, Max: 90,
		Help: "How long per-minute samples are kept. Rollups last far longer and are what the year view reads.",
	},
	{
		Key: "retention.audit_days", Group: "Retention",
		Label: "Audit log", Kind: KindDays, Default: 365, Min: 30, Max: 3650,
		Help: "How long audit entries are kept. Check this against whatever you have to comply with.",
	},
	{
		Key: "retention.history_days", Group: "Retention",
		Label: "Change history", Kind: KindDays, Default: 90, Min: 7, Max: 730,
		Help: "How long per-VM field changes are kept.",
	},
}

// Lookup finds a definition by key.
func Lookup(key string) (Definition, bool) {
	for _, def := range Catalogue {
		if def.Key == key {
			return def, true
		}
	}
	return Definition{}, false
}

// Value is a setting as it currently stands.
type Value struct {
	Definition
	Value    int  `json:"value"`
	Modified bool `json:"modified"` // differs from the default
}

// Resolve overlays stored values on the catalogue, so the API always returns
// every setting with an effective value rather than only the changed ones.
func Resolve(stored map[string]int) []Value {
	out := make([]Value, 0, len(Catalogue))
	for _, def := range Catalogue {
		value := def.Default
		modified := false
		if v, ok := stored[def.Key]; ok {
			value, modified = v, v != def.Default
		}
		out = append(out, Value{Definition: def, Value: value, Modified: modified})
	}
	return out
}
