package connector

// FieldKind is how a configuration field should be entered and validated.
type FieldKind string

const (
	FieldText   FieldKind = "text"
	FieldNumber FieldKind = "number"
	FieldBool   FieldKind = "bool"
	FieldSelect FieldKind = "select"
	// FieldSecret is write-only: it is never returned by the API, and the UI
	// shows a "replace" affordance rather than a populated box when editing.
	FieldSecret FieldKind = "secret"
)

// FieldOption is one choice in a select field.
type FieldOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Field describes one configuration input a connector needs.
//
// This is deliberately not JSON Schema. What the platform form needs is a
// short, ordered list of typed inputs with labels and help text; JSON Schema
// would express all of that less directly, and none of its remaining power
// (composition, conditionals, references) has a use here. Adding a platform
// means declaring fields, not writing a schema document — see
// docs/09-connector-architecture.md.
type Field struct {
	Key      string        `json:"key"`
	Label    string        `json:"label"`
	Kind     FieldKind     `json:"kind"`
	Required bool          `json:"required"`
	Help     string        `json:"help,omitempty"`
	Default  any           `json:"default,omitempty"`
	Options  []FieldOption `json:"options,omitempty"`
	// Placeholder is an example value, not a default: it is never submitted.
	Placeholder string `json:"placeholder,omitempty"`
}

// CredentialForm describes one way of authenticating to a platform. A
// connector may offer several — an API token and a username/password, say —
// and the administrator picks one.
type CredentialForm struct {
	Kind   string  `json:"kind"`
	Label  string  `json:"label"`
	Help   string  `json:"help,omitempty"`
	Fields []Field `json:"fields"`
}

// ConfigSchema is everything the UI needs to render a platform form for a
// connector without knowing anything about that platform.
type ConfigSchema struct {
	// EndpointLabel and EndpointHelp let a connector name its address field in
	// its own terms ("API URL", "vCenter address") rather than a generic one.
	EndpointLabel string `json:"endpoint_label,omitempty"`
	EndpointHelp  string `json:"endpoint_help,omitempty"`
	// Fields are connector-specific settings, stored in Config.Extra.
	Fields []Field `json:"fields,omitempty"`
	// Credentials are the authentication methods on offer, best first.
	Credentials []CredentialForm `json:"credentials,omitempty"`
}
