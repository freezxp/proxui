// Package publish holds the rules that decide whether a proposed routing
// table is safe to send to an edge provider.
//
// These live in the domain, with no network and no database, because they are
// the requirements the feature exists to satisfy (docs/28 §28.3, PUB-3x) and
// they must be testable exhaustively and cheaply. Everything here answers one
// question: if we send this, does the portal stay reachable and does anyone
// else's route survive?
package publish

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrNoCatchAll means the table would end without a rule matching
	// everything left over.
	ErrNoCatchAll = errors.New("publish: the routing table must end with a catch-all rule")
	// ErrRuleAfterCatchAll means a hostname rule sits after the catch-all,
	// where first-match-wins guarantees it is dead.
	ErrRuleAfterCatchAll = errors.New("publish: no rule may follow the catch-all")
	// ErrSelfRemoved means the change would delete or disable the route that
	// serves this portal.
	ErrSelfRemoved = errors.New("publish: the change would remove the portal's own route")
	// ErrSelfShadowed means the portal's route survives but is unreachable,
	// because something earlier now matches its traffic first.
	ErrSelfShadowed = errors.New("publish: the change would shadow the portal's own route")
	// ErrDuplicateHostname means two rules claim the same hostname and path,
	// so one of them can never match.
	ErrDuplicateHostname = errors.New("publish: two rules claim the same hostname and path")
	// ErrInvalidHostname rejects a name that is not a hostname.
	ErrInvalidHostname = errors.New("publish: invalid hostname")
	// ErrInvalidService rejects a target that is not a usable service.
	ErrInvalidService = errors.New("publish: invalid service target")
)

// Rule mirrors edge.Rule. The domain declares its own type rather than
// importing the port, because the layer rule points the other way and because
// these invariants are about routing tables in general, not about Cloudflare.
type Rule struct {
	Hostname string
	Path     string
	Service  string
}

// IsCatchAll reports whether the rule matches everything left over.
func (r Rule) IsCatchAll() bool { return strings.TrimSpace(r.Hostname) == "" }

// Matches reports whether this rule would capture a request for the given
// hostname and path, which is how shadowing is detected.
func (r Rule) Matches(hostname, path string) bool {
	if r.IsCatchAll() {
		return true
	}
	if !strings.EqualFold(r.Hostname, hostname) {
		return false
	}
	// A rule with no path matches every path under that hostname. A rule with
	// a path only captures requests beneath it, so it cannot shadow a
	// pathless rule for the same name.
	if strings.TrimSpace(r.Path) == "" {
		return true
	}
	return strings.TrimSpace(path) != "" && strings.HasPrefix(path, r.Path)
}

// Table is a proposed routing table, in match order.
type Table []Rule

// Validate checks the invariants that must hold before a table is sent.
//
// selfHostname is the name this portal is served at, or "" when the portal is
// not published through this provider. When it is set, the portal's own route
// is protected: the tool you would use to fix a mistake must survive the
// mistake (PUB-33).
func (t Table) Validate(selfHostname string) error {
	if len(t) == 0 {
		return ErrNoCatchAll
	}

	for i, r := range t {
		if r.IsCatchAll() {
			// Everything after a catch-all is unreachable, which is a
			// mistake rather than a preference.
			if i != len(t)-1 {
				return fmt.Errorf("%w: rule %d (%s) is dead", ErrRuleAfterCatchAll, i+1, t[i+1].Hostname)
			}
			continue
		}
		if err := ValidateHostname(r.Hostname); err != nil {
			return err
		}
		if err := ValidateService(r.Service); err != nil {
			return err
		}
	}

	if !t[len(t)-1].IsCatchAll() {
		return ErrNoCatchAll
	}

	if err := t.checkDuplicates(); err != nil {
		return err
	}
	return t.checkSelf(selfHostname)
}

func (t Table) checkDuplicates() error {
	type key struct{ host, path string }
	seen := map[key]bool{}
	for _, r := range t {
		if r.IsCatchAll() {
			continue
		}
		k := key{strings.ToLower(r.Hostname), r.Path}
		if seen[k] {
			return fmt.Errorf("%w: %s%s", ErrDuplicateHostname, r.Hostname, r.Path)
		}
		seen[k] = true
	}
	return nil
}

// checkSelf enforces PUB-33: the portal's own route must still be present, and
// must still be the first rule that matches its traffic.
func (t Table) checkSelf(selfHostname string) error {
	if strings.TrimSpace(selfHostname) == "" {
		return nil
	}

	for _, r := range t {
		if r.IsCatchAll() {
			break // reached the end without finding it
		}
		if !r.Matches(selfHostname, "") {
			continue
		}
		// The first rule matching the portal's traffic must be the portal's
		// own. If some other hostname pattern got there first, the portal is
		// still listed but no longer served.
		if strings.EqualFold(r.Hostname, selfHostname) && strings.TrimSpace(r.Path) == "" {
			return nil
		}
		return fmt.Errorf("%w: %s%s matches %s first", ErrSelfShadowed, r.Hostname, r.Path, selfHostname)
	}
	return fmt.Errorf("%w: %s", ErrSelfRemoved, selfHostname)
}

// hostnameNotAllowed lists characters that have no business in a hostname.
// Checked explicitly rather than by regexp so the failure can say which.
const hostnameNotAllowed = ` /\?#@:"'<>[]{}|^~` + "`"

// ValidateHostname checks a public hostname.
func ValidateHostname(hostname string) error {
	h := strings.TrimSpace(hostname)
	switch {
	case h == "":
		return fmt.Errorf("%w: empty", ErrInvalidHostname)
	case len(h) > 253:
		return fmt.Errorf("%w: longer than 253 characters", ErrInvalidHostname)
	case strings.ContainsAny(h, hostnameNotAllowed):
		return fmt.Errorf("%w: %q contains a character that is not allowed in a hostname", ErrInvalidHostname, hostname)
	case !strings.Contains(h, "."):
		// A tunnel hostname is always a fully qualified name in a zone; a
		// bare label is a mistake that would otherwise fail much later, at
		// Cloudflare, with a worse message.
		return fmt.Errorf("%w: %q is not fully qualified", ErrInvalidHostname, hostname)
	case strings.HasPrefix(h, ".") || strings.HasSuffix(h, "."):
		return fmt.Errorf("%w: %q starts or ends with a dot", ErrInvalidHostname, hostname)
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" {
			return fmt.Errorf("%w: %q has an empty label", ErrInvalidHostname, hostname)
		}
		if len(label) > 63 {
			return fmt.Errorf("%w: label %q is longer than 63 characters", ErrInvalidHostname, label)
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("%w: label %q starts or ends with a hyphen", ErrInvalidHostname, label)
		}
	}
	return nil
}

// allowedSchemes are the service targets this portal will publish. Narrower
// than what cloudflared supports: the rest need a differently shaped UI, and
// accepting them here would let one be configured that nothing can display
// (docs/28 §28.7).
var allowedSchemes = []string{"http://", "https://"}

// ValidateService checks a routing target.
func ValidateService(service string) error {
	s := strings.TrimSpace(service)
	if s == "" {
		return fmt.Errorf("%w: empty", ErrInvalidService)
	}
	// The catch-all's literal, and the only status literal worth allowing.
	if strings.HasPrefix(s, "http_status:") {
		return nil
	}
	for _, scheme := range allowedSchemes {
		if !strings.HasPrefix(s, scheme) {
			continue
		}
		if strings.TrimSpace(strings.TrimPrefix(s, scheme)) == "" {
			return fmt.Errorf("%w: %q has no host", ErrInvalidService, service)
		}
		return nil
	}
	return fmt.Errorf("%w: %q must start with http:// or https://", ErrInvalidService, service)
}

// CatchAll is the conventional final rule: answer 404 for anything unmatched.
func CatchAll() Rule { return Rule{Service: "http_status:404"} }

// EnsureCatchAll returns the table with exactly one catch-all, last.
//
// Callers build tables by editing a list of hostname rules; making them
// remember the terminator is how it eventually gets forgotten. Any catch-all
// already present is preserved rather than replaced, since its service may
// have been chosen deliberately.
func EnsureCatchAll(rules Table) Table {
	out := make(Table, 0, len(rules)+1)
	existing := CatchAll()
	found := false
	for _, r := range rules {
		if r.IsCatchAll() {
			if !found {
				existing, found = r, true
			}
			continue
		}
		out = append(out, r)
	}
	return append(out, existing)
}
