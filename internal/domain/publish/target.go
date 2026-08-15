package publish

import (
	"strconv"
	"strings"
)

// Target is a routing rule's destination, taken apart.
//
// Parsed rather than kept as a string because the useful question — which VM
// is this pointing at? — needs the host on its own, and because a rule
// pointing at a machine that no longer has that address is exactly the drift
// this feature exists to notice.
type Target struct {
	Scheme string
	Host   string
	Port   int
	// Literal is set for a non-network target such as `http_status:404`,
	// where there is no host to match against anything.
	Literal string
}

// IsLiteral reports whether the target names no host.
func (t Target) IsLiteral() bool { return t.Literal != "" }

// ParseTarget takes a rule's service apart.
//
// Deliberately tolerant: it is used to describe rules the portal did not
// create, so anything it cannot parse must come back as an unmatched target
// rather than an error. A rule the portal does not understand is still a rule
// that has to be displayed and preserved.
func ParseTarget(service string) Target {
	s := strings.TrimSpace(service)
	if s == "" {
		return Target{}
	}
	if strings.HasPrefix(s, "http_status:") {
		return Target{Literal: s}
	}

	scheme := ""
	if i := strings.Index(s, "://"); i >= 0 {
		scheme, s = s[:i], s[i+3:]
	} else if i := strings.Index(s, ":"); i >= 0 && !strings.Contains(s[:i], ".") {
		// Something like "unix:/var/run/x.sock" — a scheme we do not handle,
		// and not a host:port pair.
		return Target{Literal: strings.TrimSpace(service)}
	}

	// Drop any path, query or fragment; only the authority matters here.
	for _, cut := range []string{"/", "?", "#"} {
		if i := strings.Index(s, cut); i >= 0 {
			s = s[:i]
		}
	}

	host, port := s, 0
	// A bracketed IPv6 literal keeps its colons, so the port is only whatever
	// follows the closing bracket.
	if strings.HasPrefix(s, "[") {
		if end := strings.Index(s, "]"); end >= 0 {
			host = s[1:end]
			if rest := s[end+1:]; strings.HasPrefix(rest, ":") {
				port, _ = strconv.Atoi(rest[1:])
			}
		}
	} else if i := strings.LastIndex(s, ":"); i >= 0 {
		if p, err := strconv.Atoi(s[i+1:]); err == nil {
			host, port = s[:i], p
		}
	}

	if port == 0 {
		// The scheme's default, so a rule written without a port still
		// matches a VM that is listening on the obvious one.
		switch scheme {
		case "https":
			port = 443
		case "http":
			port = 80
		}
	}
	return Target{Scheme: scheme, Host: strings.ToLower(strings.TrimSpace(host)), Port: port}
}

// String renders a target back to a service value.
func (t Target) String() string {
	if t.IsLiteral() {
		return t.Literal
	}
	if t.Host == "" {
		return ""
	}
	host := t.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // IPv6 needs its brackets back
	}
	scheme := t.Scheme
	if scheme == "" {
		scheme = "http"
	}
	if t.Port == 0 {
		return scheme + "://" + host
	}
	return scheme + "://" + host + ":" + strconv.Itoa(t.Port)
}

// MachineRef is enough of a VM to say which one a rule points at.
type MachineRef struct {
	ID          string
	Name        string
	Addresses   []string
	PlatformID  string
	State       string
	IsReachable bool
}

// MatchMachine finds the VM a target points at, by address.
//
// Address only, deliberately. Matching on name would be guesswork, and getting
// it wrong here means telling someone a rule points at a machine it does not —
// worse than admitting the rule points at something the portal does not know
// about, which is a perfectly ordinary thing for it to do.
func MatchMachine(t Target, machines []MachineRef) (MachineRef, bool) {
	if t.IsLiteral() || t.Host == "" {
		return MachineRef{}, false
	}
	for _, m := range machines {
		for _, addr := range m.Addresses {
			if strings.EqualFold(strings.TrimSpace(addr), t.Host) {
				return m, true
			}
		}
	}
	return MachineRef{}, false
}

// RuleOrigin says who put a rule in the routing table.
type RuleOrigin string

const (
	// OriginPortal marks a rule this portal created and therefore owns.
	OriginPortal RuleOrigin = "portal"
	// OriginExternal marks a rule that was already there — created in the
	// Cloudflare dashboard, by Terraform, or by hand. It is shown read-only
	// and preserved byte-for-byte on every write (PUB-11).
	OriginExternal RuleOrigin = "external"
	// OriginCatchAll marks the terminator, which belongs to neither.
	OriginCatchAll RuleOrigin = "catch_all"
)

// DescribeRule is one routing rule with everything known about it.
type DescribeRule struct {
	Index    int
	Hostname string
	Path     string
	Service  string
	Target   Target
	Origin   RuleOrigin

	// Machine is the VM this rule points at, when one has that address.
	Machine   *MachineRef
	IsPortal  bool // serves this portal, so it is protected (PUB-33)
	Unmatched bool // points at an address no known VM holds
}

// Describe annotates a routing table for display.
//
// selfHostname is the name this portal is served at; portalOwned lists the
// hostnames the portal created. Everything else is external and untouchable.
func Describe(rules []Rule, machines []MachineRef, selfHostname string, portalOwned map[string]bool) []DescribeRule {
	out := make([]DescribeRule, 0, len(rules))
	for i, r := range rules {
		d := DescribeRule{
			Index: i, Hostname: r.Hostname, Path: r.Path, Service: r.Service,
			Target: ParseTarget(r.Service),
		}

		switch {
		case r.IsCatchAll():
			d.Origin = OriginCatchAll
		case portalOwned[strings.ToLower(r.Hostname)+r.Path]:
			d.Origin = OriginPortal
		default:
			d.Origin = OriginExternal
		}

		d.IsPortal = selfHostname != "" && strings.EqualFold(r.Hostname, selfHostname)

		if m, ok := MatchMachine(d.Target, machines); ok {
			machine := m
			d.Machine = &machine
		} else if !d.Target.IsLiteral() && d.Target.Host != "" {
			d.Unmatched = true
		}
		out = append(out, d)
	}
	return out
}
