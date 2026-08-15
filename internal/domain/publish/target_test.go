package publish

import "testing"

func TestParseTarget(t *testing.T) {
	cases := []struct {
		service string
		scheme  string
		host    string
		port    int
		literal string
	}{
		// The shapes actually in use on the live account.
		{"http://10.0.13.9:8080", "http", "10.0.13.9", 8080, ""},
		{"https://10.0.13.105:9442", "https", "10.0.13.105", 9442, ""},
		{"http://10.0.13.105", "http", "10.0.13.105", 80, ""},
		{"https://192.168.1.4:5001", "https", "192.168.1.4", 5001, ""},
		{"http://127.0.0.1:32400", "http", "127.0.0.1", 32400, ""},
		{"http_status:404", "", "", 0, "http_status:404"},

		// A scheme with no port takes the scheme's default, so a rule written
		// without one still matches a VM listening on the obvious port.
		{"https://app.internal", "https", "app.internal", 443, ""},

		// Paths and queries are not part of the authority.
		{"http://10.0.0.5:8080/health", "http", "10.0.0.5", 8080, ""},
		{"http://10.0.0.5:8080?x=1", "http", "10.0.0.5", 8080, ""},

		// IPv6 keeps its colons; only what follows the bracket is a port.
		{"http://[2001:db8::1]:8080", "http", "2001:db8::1", 8080, ""},
		{"http://[2001:db8::1]", "http", "2001:db8::1", 80, ""},

		// Case is not significant in a host.
		{"http://APP.Internal:80", "http", "app.internal", 80, ""},

		{"", "", "", 0, ""},
	}
	for _, c := range cases {
		got := ParseTarget(c.service)
		if got.Scheme != c.scheme || got.Host != c.host || got.Port != c.port || got.Literal != c.literal {
			t.Errorf("ParseTarget(%q) = %+v, want scheme=%q host=%q port=%d literal=%q",
				c.service, got, c.scheme, c.host, c.port, c.literal)
		}
	}
}

// The parser describes rules the portal did not write, so anything it cannot
// understand must come back as an unmatched target rather than an error. A
// rule the portal does not grasp is still one it has to display and preserve.
func TestParseTargetIsTolerantOfWhatItDoesNotKnow(t *testing.T) {
	for _, service := range []string{
		"ssh://10.0.0.5:22",
		"rdp://10.0.0.5:3389",
		"unix:/var/run/thing.sock",
		"tcp://10.0.0.5:9000",
	} {
		got := ParseTarget(service)
		if got.Host == "" && got.Literal == "" {
			t.Errorf("ParseTarget(%q) lost the target entirely", service)
		}
	}
}

func TestTargetRoundTrips(t *testing.T) {
	for _, service := range []string{
		"http://10.0.13.9:8080",
		"https://192.168.1.4:5001",
		"http_status:404",
		"http://[2001:db8::1]:8080",
	} {
		if got := ParseTarget(service).String(); got != service {
			t.Errorf("round trip of %q gave %q", service, got)
		}
	}
}

func machines() []MachineRef {
	return []MachineRef{
		{ID: "vm-1", Name: "amp", Addresses: []string{"10.0.13.10"}, State: "running"},
		{ID: "vm-2", Name: "db", Addresses: []string{"10.0.13.9", "172.17.0.1"}, State: "running"},
	}
}

func TestMatchMachineByAddress(t *testing.T) {
	if m, ok := MatchMachine(ParseTarget("http://10.0.13.9:8080"), machines()); !ok || m.Name != "db" {
		t.Errorf("got %+v ok=%v, want db", m, ok)
	}
	// A VM with several addresses matches on any of them.
	if m, ok := MatchMachine(ParseTarget("http://172.17.0.1:80"), machines()); !ok || m.Name != "db" {
		t.Errorf("got %+v ok=%v, want db via its second address", m, ok)
	}
	// An address nothing holds is not a match, and must not be guessed at.
	if _, ok := MatchMachine(ParseTarget("http://192.168.99.99:80"), machines()); ok {
		t.Error("an unknown address was matched to a VM")
	}
	// A literal has no host to match.
	if _, ok := MatchMachine(ParseTarget("http_status:404"), machines()); ok {
		t.Error("the catch-all was matched to a VM")
	}
}

func TestDescribeAnnotatesTheTable(t *testing.T) {
	rules := []Rule{
		{Hostname: "vm.example.com", Service: "http://10.0.13.10:8080"},
		{Hostname: "db.example.com", Service: "http://10.0.13.9:8080"},
		{Hostname: "old.example.com", Service: "http://192.168.99.99:80"},
		CatchAll(),
	}
	owned := map[string]bool{"db.example.com": true}

	got := Describe(rules, machines(), "vm.example.com", owned)
	if len(got) != 4 {
		t.Fatalf("%d described, want 4", len(got))
	}

	// The portal's own route is flagged, because it is the one that must
	// survive every change (PUB-33).
	if !got[0].IsPortal {
		t.Error("the portal's own rule was not flagged")
	}
	if got[0].Machine == nil || got[0].Machine.Name != "amp" {
		t.Errorf("rule 1 machine = %+v, want amp", got[0].Machine)
	}
	// Rules the portal created versus rules that were already there. The
	// second kind is read-only and preserved verbatim (PUB-11).
	if got[0].Origin != OriginExternal {
		t.Errorf("rule 1 origin = %q, want external", got[0].Origin)
	}
	if got[1].Origin != OriginPortal {
		t.Errorf("rule 2 origin = %q, want portal", got[1].Origin)
	}
	if got[3].Origin != OriginCatchAll {
		t.Errorf("the last rule origin = %q, want catch_all", got[3].Origin)
	}

	// A rule pointing at an address no VM holds is the drift worth surfacing:
	// it is what a VM that moved or was deleted leaves behind.
	if !got[2].Unmatched {
		t.Error("a rule pointing at an unknown address was not flagged")
	}
	if got[3].Unmatched {
		t.Error("the catch-all was flagged as unmatched; it has no host at all")
	}
}

// With no portal hostname configured the protection must stay quiet rather
// than flagging an arbitrary rule.
func TestDescribeFlagsNoPortalRuleWhenNotPublishedHere(t *testing.T) {
	rules := []Rule{{Hostname: "a.example.com", Service: "http://10.0.0.1:80"}, CatchAll()}
	for _, d := range Describe(rules, nil, "", nil) {
		if d.IsPortal {
			t.Error("a rule was flagged as the portal's own with no portal hostname set")
		}
	}
}
