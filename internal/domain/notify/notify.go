// Package notify is the Notification bounded context: which events reach
// which channels, and what the resulting message says (NOTIF-01..03).
package notify

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Errors the transport maps onto status codes.
var (
	ErrUnknownKind     = errors.New("notify: unknown channel kind")
	ErrInvalidConfig   = errors.New("notify: invalid channel configuration")
	ErrUnknownCategory = errors.New("notify: unknown event category")
)

// Kind is a delivery mechanism.
type Kind string

const (
	KindEmail   Kind = "email"
	KindSlack   Kind = "slack"
	KindWebhook Kind = "webhook"
)

// Valid reports whether the kind is one this portal can deliver to.
func (k Kind) Valid() bool {
	switch k {
	case KindEmail, KindSlack, KindWebhook:
		return true
	}
	return false
}

// Severity orders how much a message matters. Rules express a minimum, so the
// order has to be total and explicit rather than alphabetical.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

var severityRank = map[Severity]int{SeverityInfo: 0, SeverityWarning: 1, SeverityCritical: 2}

// AtLeast reports whether s is as severe as minimum. An unrecognized severity
// is treated as info: a message with a strange severity should still be
// deliverable by a rule that asks for everything, and should not slip past a
// rule that asks only for critical ones.
func (s Severity) AtLeast(minimum Severity) bool {
	return severityRank[s] >= severityRank[minimum]
}

// Categories an event can belong to (NOTIF-02).
const (
	CategorySyncFailure      = "sync_failure"
	CategoryVMStateChange    = "vm_state_change"
	CategoryPerformanceAlert = "performance_alert"
	CategorySecurity         = "security"
)

// ValidCategory reports whether a routing rule may name this category.
func ValidCategory(category string) bool {
	switch category {
	case CategorySyncFailure, CategoryVMStateChange, CategoryPerformanceAlert, CategorySecurity:
		return true
	}
	return false
}

// Channel is a configured destination. The secret never leaves the
// repository layer in plaintext, so it is absent here by design.
type Channel struct {
	ID        uuid.UUID      `json:"id"`
	Name      string         `json:"name"`
	Kind      Kind           `json:"kind"`
	Config    map[string]any `json:"config"`
	IsEnabled bool           `json:"is_enabled"`
	HasSecret bool           `json:"has_secret"`
	CreatedAt time.Time      `json:"created_at"`
}

// Rule routes a category of event to a channel.
type Rule struct {
	ID          uuid.UUID  `json:"id"`
	Category    string     `json:"category"`
	MinSeverity Severity   `json:"min_severity"`
	PlatformID  *uuid.UUID `json:"platform_id,omitempty"`
	VMGroupID   *uuid.UUID `json:"vm_group_id,omitempty"`
	ChannelID   uuid.UUID  `json:"channel_id"`
	ChannelName string     `json:"channel_name"`
	IsEnabled   bool       `json:"is_enabled"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Event is what the notifier decides about: the outbox event, flattened to
// the fields routing actually looks at.
type Event struct {
	OutboxID   int64
	Category   string
	Type       string
	Severity   Severity
	OccurredAt time.Time
	PlatformID *uuid.UUID
	VMID       *uuid.UUID
	Payload    map[string]any
}

// Matches reports whether a rule should deliver this event.
//
// A nil platform or VM group on the rule means "any": scoping narrows, it
// never widens. An event carrying no platform cannot match a rule that names
// one, because there is no evidence it belongs to that platform.
func (r Rule) Matches(e Event) bool {
	if !r.IsEnabled || r.Category != e.Category {
		return false
	}
	if !e.Severity.AtLeast(r.MinSeverity) {
		return false
	}
	if r.PlatformID != nil && (e.PlatformID == nil || *r.PlatformID != *e.PlatformID) {
		return false
	}
	// VM-group scoping needs group membership, which the rule alone cannot
	// answer; the caller filters on it and passes only events that qualify.
	return true
}

// Message is the rendered notification, before a channel formats it.
type Message struct {
	Subject  string
	Body     string
	Severity Severity
	Category string
	Event    Event
}

// Render turns an event into human wording.
//
// The subject leads with what happened rather than with the portal's name:
// these arrive in a mail client or a Slack channel among everything else, and
// "ProxUI notification" in the subject line tells the reader nothing.
func Render(e Event) Message {
	subject, body := describe(e)
	return Message{
		Subject: subject, Body: body,
		Severity: e.Severity, Category: e.Category, Event: e,
	}
}

func describe(e Event) (subject, body string) {
	name := payloadString(e.Payload, "name", "vm_name")
	platform := payloadString(e.Payload, "platform_name", "platform")

	switch e.Type {
	case "sync.failed":
		subject = fmt.Sprintf("Synchronization failed: %s", orUnknown(platform))
		body = fmt.Sprintf("The portal could not synchronize %s.\n\n%s",
			orUnknown(platform), payloadString(e.Payload, "error", "detail"))
	case "sync.recovered":
		subject = fmt.Sprintf("Synchronization recovered: %s", orUnknown(platform))
		body = fmt.Sprintf("%s is synchronizing normally again.", orUnknown(platform))
	case "vm.state_changed":
		from := payloadString(e.Payload, "old_state", "from")
		to := payloadString(e.Payload, "new_state", "state", "to")
		subject = fmt.Sprintf("%s is now %s", orUnknown(name), orUnknown(to))
		body = fmt.Sprintf("%s changed from %s to %s.", orUnknown(name), orUnknown(from), orUnknown(to))
	case "vm.created":
		subject = fmt.Sprintf("New VM discovered: %s", orUnknown(name))
		body = fmt.Sprintf("%s appeared on %s.", orUnknown(name), orUnknown(platform))
	case "vm.deleted":
		subject = fmt.Sprintf("VM removed: %s", orUnknown(name))
		body = fmt.Sprintf("%s is no longer present on %s.", orUnknown(name), orUnknown(platform))
	default:
		subject = fmt.Sprintf("%s: %s", e.Category, e.Type)
		body = e.Type
	}

	if detail := formatPayload(e.Payload); detail != "" {
		body += "\n\n" + detail
	}
	return subject, body
}

func orUnknown(s string) string {
	if s == "" {
		return "an unnamed resource"
	}
	return s
}

func payloadString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := payload[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// formatPayload lists the event's fields in a stable order, so a reader can
// see what the portal actually knew and two identical events read identically.
func formatPayload(payload map[string]any) string {
	if len(payload) == 0 {
		return ""
	}
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %v\n", k, payload[k])
	}
	return strings.TrimRight(b.String(), "\n")
}
