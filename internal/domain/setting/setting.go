// Package setting is the Configuration bounded context: the handful of knobs
// an administrator can turn without a restart (ADM-02).
package setting

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrUnknownKey rejects a key the portal does not read, which would otherwise
// be stored forever and silently ignored.
var ErrUnknownKey = errors.New("setting: unknown key")

// ErrOutOfRange rejects a value that would break the thing it configures.
var ErrOutOfRange = errors.New("setting: value out of range")

// ErrInvalidValue rejects a value of the wrong shape for its setting.
var ErrInvalidValue = errors.New("setting: value is not valid for this setting")

// Kind is how a value should be entered and shown.
type Kind string

const (
	KindDuration Kind = "duration_s"
	KindCount    Kind = "count"
	KindDays     Kind = "days"
	KindText     Kind = "text"
	// KindImage is a picture the browser can render directly: a same-origin
	// path, or a data: URI produced by the settings page from a chosen file.
	// It is stored as text, so the portal still accepts no file uploads.
	KindImage Kind = "image"
	// KindSelect is a fixed set of choices, rendered as a dropdown.
	KindSelect Kind = "select"
	// KindSecret is write-only. It is stored encrypted and never returned by
	// the API — the UI shows whether one is set and offers to replace it,
	// which is the same treatment platform credentials get.
	KindSecret Kind = "secret"
)

// Numeric reports whether the setting holds a number rather than text.
func (k Kind) Numeric() bool {
	return k == KindDuration || k == KindCount || k == KindDays
}

// Secret reports whether the value must be encrypted at rest and withheld
// from every read.
func (k Kind) Secret() bool { return k == KindSecret }

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
	Default int    `json:"default,omitempty"`
	Min     int    `json:"min,omitempty"`
	Max     int    `json:"max,omitempty"`
	// DefaultText and MaxLength apply to text and image settings.
	DefaultText string `json:"default_text,omitempty"`
	MaxLength   int    `json:"max_length,omitempty"`
	// Options apply to select settings.
	Options []Option `json:"options,omitempty"`
	// Public marks a setting the login page needs before anyone has signed in.
	// Only branding qualifies: a portal that cannot show its own name until
	// after authentication is not branded.
	Public bool `json:"-"`
}

// ValidateNumber checks a proposed numeric value.
func (d Definition) ValidateNumber(value int) error {
	if !d.Kind.Numeric() {
		return fmt.Errorf("%w: %s takes text", ErrInvalidValue, d.Label)
	}
	if value < d.Min || value > d.Max {
		return fmt.Errorf("%w: %s must be between %d and %d", ErrOutOfRange, d.Label, d.Min, d.Max)
	}
	return nil
}

// Option is one choice in a select setting.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// ValidateText checks a proposed text value.
func (d Definition) ValidateText(value string) error {
	if d.Kind.Numeric() {
		return fmt.Errorf("%w: %s takes a number", ErrInvalidValue, d.Label)
	}
	if d.MaxLength > 0 && utf8.RuneCountInString(value) > d.MaxLength {
		return fmt.Errorf("%w: %s must be %d characters or fewer", ErrOutOfRange, d.Label, d.MaxLength)
	}
	if d.Kind == KindImage {
		return validateImage(value)
	}
	if d.Kind == KindSelect {
		for _, o := range d.Options {
			if o.Value == value {
				return nil
			}
		}
		return fmt.Errorf("%w: %s does not offer that choice", ErrInvalidValue, d.Label)
	}
	return nil
}

// validateImage accepts only what a browser can render without reaching off
// this origin: a same-origin path, or an inline image.
//
// An arbitrary external URL is refused rather than silently failing. The
// content security policy limits images to 'self' and data:, so a logo hosted
// elsewhere would be blocked by the browser and look like a broken feature
// (docs/25-security-checklist.md).
func validateImage(value string) error {
	if value == "" {
		return nil
	}
	if strings.HasPrefix(value, "/") {
		return nil
	}
	if !strings.HasPrefix(value, "data:image/") {
		return fmt.Errorf("%w: a logo must be an uploaded image or a path on this portal; "+
			"an address on another site would be blocked by the portal's content security policy",
			ErrInvalidValue)
	}
	// SVG can carry script. It is safe inside an <img>, where scripts do not
	// run, and that is the only way the portal renders a logo — but say so
	// here so the constraint is not lost if that ever changes.
	if !strings.Contains(value, ";base64,") {
		return fmt.Errorf("%w: the image must be base64 encoded", ErrInvalidValue)
	}
	return nil
}

// Choices for sync.live_reads.
const (
	LiveReadsOn  = "on"
	LiveReadsOff = "off"
)

// Catalogue is every setting the portal reads. Adding one here is what makes
// it settable; nothing else needs to change.
var Catalogue = []Definition{
	{
		Key: "branding.portal_name", Group: "Branding", Kind: KindText,
		Label: "Portal name", DefaultText: "", MaxLength: 40, Public: true,
		// Empty means "use the address this portal was reached at", resolved
		// in the browser rather than here: the server sees whatever Host a
		// proxy passed it, while the browser knows what the person actually
		// typed. For a self-hosted tool that address is usually the best name
		// it could have, which is why it is the default.
		Help: "Shown in the header, on the sign-in page and in the browser tab. " +
			"Leave empty to use the address people reach the portal at.",
	},
	{
		Key: "branding.logo", Group: "Branding", Kind: KindImage,
		Label: "Logo", MaxLength: 131072, Public: true,
		Help: "Shown beside the portal name. Wide marks work better than tall ones; it is drawn at 28 pixels high.",
	},
	{
		Key: "branding.login_banner", Group: "Branding", Kind: KindText,
		Label: "Sign-in notice", MaxLength: 280, Public: true,
		Help: "Optional. Shown on the sign-in page — an acceptable-use notice, or who to contact for access.",
	},
	{
		Key: "auth.google_client_id", Group: "Google sign-in", Kind: KindText,
		Label: "Client ID", MaxLength: 200,
		Help: "From the OAuth client you created in Google Cloud. Ends in .apps.googleusercontent.com.",
	},
	{
		Key: "auth.google_client_secret", Group: "Google sign-in", Kind: KindSecret,
		Label: "Client secret", MaxLength: 200,
		Help: "Stored encrypted with the same key as platform credentials, and never shown again.",
	},
	{
		Key: "auth.google_redirect_url", Group: "Google sign-in", Kind: KindText,
		Label: "Redirect URL", MaxLength: 400,
		// The single most common failure in this flow, and invisible from the
		// portal's side: Google compares it character for character.
		Help: "Must match a redirect URI registered with Google exactly, including scheme, host, port and path.",
	},
	{
		Key: "auth.self_registration", Group: "Access", Kind: KindSelect,
		Label: "Self-registration", DefaultText: RegistrationDisabled,
		Options: []Option{
			{Value: RegistrationDisabled, Label: "Disabled — an administrator creates every account"},
			{Value: RegistrationOpen, Label: "Open — anyone can create an account"},
		},
		// Off by default: a portal reachable on a network should not accept
		// new accounts until someone decides it should. A registered account
		// is read-only with no grants, so it can see nothing until granted,
		// but it does exist and it can sign in (docs/adr/0003).
		Help: "Whether people can create their own account. New accounts are read-only " +
			"and see nothing until an administrator grants them VMs.",
	},
	{
		Key: "sync.inventory_interval_s", Group: "Synchronization",
		Label: "Inventory interval", Kind: KindDuration, Default: 60, Min: 30, Max: 3600,
		Help: "How often each platform is asked what it has. Applies to platforms without their own override.",
	},
	{
		Key: "sync.live_reads", Group: "Synchronization", Kind: KindSelect,
		Label: "Live state on page load", DefaultText: LiveReadsOn,
		Options: []Option{
			{Value: LiveReadsOn, Label: "On — ask the platform when a page is opened"},
			{Value: LiveReadsOff, Label: "Off — show what the last sync found"},
		},
		// The switch exists because the failure mode is a slow portal rather
		// than a wrong one, and the hour you need to turn this off is the hour
		// you least want to be redeploying. A live read is one call per
		// platform, shared between concurrent viewers and reused for a few
		// seconds, and any failure falls back to the synced row.
		Help: "Whether opening the VM list or a VM asks the platform for current power state, " +
			"instead of showing what the last sync found. Falls back to synced data " +
			"if the platform is slow or unreachable.",
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

// Registration modes for auth.self_registration.
const (
	RegistrationDisabled = "disabled"
	RegistrationOpen     = "open"
)

// Lookup finds a definition by key.
func Lookup(key string) (Definition, bool) {
	for _, def := range Catalogue {
		if def.Key == key {
			return def, true
		}
	}
	return Definition{}, false
}

// Value is a setting as it currently stands. A secret's contents are never
// present: HasValue says only whether one is stored.
type Value struct {
	Definition
	Value    int    `json:"value,omitempty"`
	Text     string `json:"text,omitempty"`
	HasValue bool   `json:"has_value,omitempty"`
	Modified bool   `json:"modified"`
}

// Resolve overlays stored values on the catalogue, so the API always returns
// every setting with an effective value rather than only the changed ones.
//
// A stored value that no longer decodes — because a setting changed kind, say —
// is ignored in favour of the default rather than failing the whole page.
func Resolve(stored map[string]json.RawMessage) []Value {
	out := make([]Value, 0, len(Catalogue))
	for _, def := range Catalogue {
		v := Value{Definition: def, Value: def.Default, Text: def.DefaultText}
		if def.Kind.Secret() {
			// Presence only. The ciphertext never reaches this layer, so the
			// caller passes a marker rather than the value.
			_, present := stored[def.Key]
			v.HasValue, v.Modified = present, present
			out = append(out, v)
			continue
		}
		if raw, ok := stored[def.Key]; ok {
			if def.Kind.Numeric() {
				var n int
				if err := json.Unmarshal(raw, &n); err == nil {
					v.Value, v.Modified = n, n != def.Default
				}
			} else {
				var s string
				if err := json.Unmarshal(raw, &s); err == nil {
					v.Text, v.Modified = s, s != def.DefaultText
				}
			}
		}
		out = append(out, v)
	}
	return out
}

// PublicValues is the subset the sign-in page may read unauthenticated.
//
// Secrets are excluded structurally rather than by remembering to exclude
// them: a secret is never public, and the loop refuses to emit one even if a
// definition were mistakenly marked so.
func PublicValues(stored map[string]json.RawMessage) map[string]string {
	out := map[string]string{}
	for _, v := range Resolve(stored) {
		if v.Public && !v.Kind.Secret() {
			out[v.Key] = v.Text
		}
	}
	return out
}
