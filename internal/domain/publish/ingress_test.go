package publish

import (
	"errors"
	"testing"
)

const self = "vm.example.com"

// portalRule is the rule that keeps the portal reachable. Most tables need it,
// because most tests are about not breaking it.
func portalRule() Rule {
	return Rule{Hostname: self, Service: "http://10.0.0.5:8080"}
}

func TestATableMustEndWithACatchAll(t *testing.T) {
	cases := map[string]Table{
		"empty":        {},
		"no catch-all": {portalRule()},
		"catch-all first": {
			CatchAll(),
			portalRule(),
		},
	}
	for name, table := range cases {
		t.Run(name, func(t *testing.T) {
			if err := table.Validate(self); err == nil {
				t.Fatal("accepted a table with no usable catch-all")
			}
		})
	}
}

// Everything after the catch-all is dead: first match wins and the catch-all
// matches everything. Accepting it would publish a hostname that silently
// never works.
func TestNothingMayFollowTheCatchAll(t *testing.T) {
	table := Table{
		portalRule(),
		CatchAll(),
		{Hostname: "app.example.com", Service: "http://10.0.0.9:80"},
	}
	err := table.Validate(self)
	if !errors.Is(err, ErrRuleAfterCatchAll) {
		t.Fatalf("got %v, want ErrRuleAfterCatchAll", err)
	}
}

func TestAValidTableIsAccepted(t *testing.T) {
	table := Table{
		portalRule(),
		{Hostname: "app.example.com", Service: "http://10.0.0.9:80"},
		{Hostname: "api.example.com", Path: "/v1", Service: "https://10.0.0.9:8443"},
		CatchAll(),
	}
	if err := table.Validate(self); err != nil {
		t.Fatalf("rejected a valid table: %v", err)
	}
}

// PUB-33. The portal is published through the tunnel it is editing, so the
// tool you would use to fix a mistake must survive the mistake.
func TestThePortalsOwnRouteCannotBeRemoved(t *testing.T) {
	table := Table{
		{Hostname: "app.example.com", Service: "http://10.0.0.9:80"},
		CatchAll(),
	}
	err := table.Validate(self)
	if !errors.Is(err, ErrSelfRemoved) {
		t.Fatalf("got %v, want ErrSelfRemoved", err)
	}

	// And with no portal hostname configured — the portal is reached another
	// way — the same table is fine. The protection must not fire for people
	// it does not apply to.
	if err := table.Validate(""); err != nil {
		t.Errorf("protection fired when the portal is not published here: %v", err)
	}
}

// Subtler than deletion and likelier in practice: the rule is still listed,
// but something earlier now captures its traffic, so the portal is present and
// unreachable.
func TestThePortalsOwnRouteCannotBeShadowed(t *testing.T) {
	t.Run("by an earlier catch-all", func(t *testing.T) {
		table := Table{
			{Service: "http://10.0.0.9:80"}, // a catch-all sitting first
			portalRule(),
			CatchAll(),
		}
		// Caught as a rule following a catch-all, which is the same mistake
		// seen from the other end.
		if err := table.Validate(self); err == nil {
			t.Fatal("accepted a table whose first rule swallows everything")
		}
	})

	t.Run("by an identical hostname earlier", func(t *testing.T) {
		table := Table{
			{Hostname: self, Service: "http://10.0.0.99:9999"}, // hijacked
			portalRule(),
			CatchAll(),
		}
		// Two rules for the same hostname and path: one can never match.
		if err := table.Validate(self); !errors.Is(err, ErrDuplicateHostname) {
			t.Fatalf("got %v, want ErrDuplicateHostname", err)
		}
	})
}

// A path rule for the portal's hostname is allowed to sit before the pathless
// one — that is how you carve out a subpath — and must not trip the guard.
func TestAPathRuleOnThePortalsHostnameIsAllowed(t *testing.T) {
	table := Table{
		{Hostname: self, Path: "/metrics", Service: "http://10.0.0.7:9100"},
		portalRule(),
		CatchAll(),
	}
	if err := table.Validate(self); err != nil {
		t.Fatalf("rejected a legitimate subpath carve-out: %v", err)
	}
}

func TestDuplicateHostnamesAreRejected(t *testing.T) {
	table := Table{
		portalRule(),
		{Hostname: "app.example.com", Service: "http://10.0.0.9:80"},
		{Hostname: "APP.example.com", Service: "http://10.0.0.10:80"}, // case differs only
		CatchAll(),
	}
	if err := table.Validate(self); !errors.Is(err, ErrDuplicateHostname) {
		t.Fatalf("got %v, want ErrDuplicateHostname", err)
	}
}

func TestHostnameValidation(t *testing.T) {
	valid := []string{
		"app.example.com",
		"a.b.c.d.example.com",
		"xn--bcher-kva.example.com",
		"app-1.example.com",
	}
	for _, h := range valid {
		if err := ValidateHostname(h); err != nil {
			t.Errorf("ValidateHostname(%q) = %v, want nil", h, err)
		}
	}

	invalid := map[string]string{
		"empty":           "",
		"not qualified":   "app",
		"leading dot":     ".app.example.com",
		"trailing dot":    "app.example.com.",
		"empty label":     "app..example.com",
		"has a scheme":    "https://app.example.com",
		"has a path":      "app.example.com/thing",
		"has a space":     "app example.com",
		"leading hyphen":  "-app.example.com",
		"trailing hyphen": "app-.example.com",
		"label too long":  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.example.com",
		"has a port":      "app.example.com:8080",
		"has an @":        "user@app.example.com",
		"square brackets": "[::1].example.com",
	}
	for name, h := range invalid {
		if err := ValidateHostname(h); !errors.Is(err, ErrInvalidHostname) {
			t.Errorf("%s: ValidateHostname(%q) = %v, want ErrInvalidHostname", name, h, err)
		}
	}
}

func TestServiceValidation(t *testing.T) {
	valid := []string{
		"http://10.0.0.5:8080",
		"https://10.0.0.5:8443",
		"http://localhost:3000",
		"http_status:404",
	}
	for _, s := range valid {
		if err := ValidateService(s); err != nil {
			t.Errorf("ValidateService(%q) = %v, want nil", s, err)
		}
	}

	invalid := map[string]string{
		"empty":       "",
		"no scheme":   "10.0.0.5:8080",
		"scheme only": "http://",
		"ssh":         "ssh://10.0.0.5:22",
		"rdp":         "rdp://10.0.0.5:3389",
		"unix socket": "unix:/var/run/thing.sock",
		"a bare word": "myservice",
	}
	for name, s := range invalid {
		if err := ValidateService(s); !errors.Is(err, ErrInvalidService) {
			t.Errorf("%s: ValidateService(%q) = %v, want ErrInvalidService", name, s, err)
		}
	}
}

func TestEnsureCatchAllPutsExactlyOneAtTheEnd(t *testing.T) {
	t.Run("adds one when missing", func(t *testing.T) {
		got := EnsureCatchAll(Table{portalRule()})
		if len(got) != 2 || !got[1].IsCatchAll() {
			t.Fatalf("got %+v, want the rule then a catch-all", got)
		}
	})

	t.Run("moves one that is in the wrong place", func(t *testing.T) {
		got := EnsureCatchAll(Table{CatchAll(), portalRule()})
		if len(got) != 2 || !got[1].IsCatchAll() || got[0].Hostname != self {
			t.Fatalf("got %+v, want the rule then a catch-all", got)
		}
	})

	t.Run("collapses several into one", func(t *testing.T) {
		got := EnsureCatchAll(Table{portalRule(), CatchAll(), CatchAll()})
		if len(got) != 2 {
			t.Fatalf("got %d rules, want 2", len(got))
		}
	})

	// A deliberately chosen catch-all service must survive: someone who set it
	// to proxy elsewhere rather than 404 meant it.
	t.Run("keeps a deliberate catch-all service", func(t *testing.T) {
		custom := Rule{Service: "http://10.0.0.1:80"}
		got := EnsureCatchAll(Table{custom, portalRule()})
		if got[len(got)-1].Service != "http://10.0.0.1:80" {
			t.Errorf("catch-all service = %q, want it preserved", got[len(got)-1].Service)
		}
	})

	// The output of EnsureCatchAll must always be something Validate accepts,
	// which is the property that makes it worth having.
	t.Run("output always validates", func(t *testing.T) {
		got := EnsureCatchAll(Table{CatchAll(), portalRule(), CatchAll()})
		if err := got.Validate(self); err != nil {
			t.Errorf("EnsureCatchAll produced a table Validate rejects: %v", err)
		}
	})
}

func TestMatches(t *testing.T) {
	cases := []struct {
		rule     Rule
		hostname string
		path     string
		want     bool
	}{
		{CatchAll(), "anything.example.com", "", true},
		{Rule{Hostname: "app.example.com"}, "app.example.com", "", true},
		{Rule{Hostname: "app.example.com"}, "APP.example.com", "", true},
		{Rule{Hostname: "app.example.com"}, "other.example.com", "", false},
		{Rule{Hostname: "app.example.com"}, "app.example.com", "/v1", true},
		// A rule with a path cannot capture a request that has none, which is
		// what makes a subpath carve-out safe to put first.
		{Rule{Hostname: "app.example.com", Path: "/v1"}, "app.example.com", "", false},
		{Rule{Hostname: "app.example.com", Path: "/v1"}, "app.example.com", "/v1/users", true},
		{Rule{Hostname: "app.example.com", Path: "/v1"}, "app.example.com", "/v2", false},
	}
	for _, c := range cases {
		if got := c.rule.Matches(c.hostname, c.path); got != c.want {
			t.Errorf("Rule{%q,%q}.Matches(%q,%q) = %v, want %v",
				c.rule.Hostname, c.rule.Path, c.hostname, c.path, got, c.want)
		}
	}
}
