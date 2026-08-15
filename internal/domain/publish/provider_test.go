package publish

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func validProvider() *Provider {
	return &Provider{
		ID: uuid.New(), Name: "home", Kind: KindCloudflareTunnel,
		AccountID: "acct", IsEnabled: true,
	}
}

func TestProviderValidation(t *testing.T) {
	if err := validProvider().Validate(); err != nil {
		t.Fatalf("rejected a valid provider: %v", err)
	}

	cases := map[string]func(*Provider){
		"no name":      func(p *Provider) { p.Name = "  " },
		"no account":   func(p *Provider) { p.AccountID = "" },
		"unknown kind": func(p *Provider) { p.Kind = "nginx" },
		"name too long": func(p *Provider) {
			p.Name = string(make([]byte, 101))
		},
	}
	for name, mangle := range cases {
		p := validProvider()
		mangle(p)
		if err := p.Validate(); !errors.Is(err, ErrInvalidProvider) {
			t.Errorf("%s: got %v, want ErrInvalidProvider", name, err)
		}
	}
}

// Registered and usable are different states. A credential that works but has
// no tunnel chosen yet is a legitimate place to be, and calling it ready would
// let a publish attempt reach the API with no tunnel to write to.
func TestReadyNeedsATunnel(t *testing.T) {
	p := validProvider()
	if p.Ready() {
		t.Error("a provider with no tunnel selected reported ready")
	}

	p.TunnelID = "t1"
	if !p.Ready() {
		t.Error("a provider with a tunnel should be ready")
	}

	p.IsEnabled = false
	if p.Ready() {
		t.Error("a disabled provider reported ready")
	}

	p.IsEnabled = true
	p.DeletedAt = time.Now()
	if p.Ready() {
		t.Error("a deleted provider reported ready")
	}
}

// PUB-04. DNS:Edit reaches a whole zone, so the allowed list is the real write
// boundary and must fail closed.
func TestZonesAreDeniedByDefault(t *testing.T) {
	p := validProvider()
	if p.AllowsZone("zone-1") {
		t.Error("a provider with no allowed zones permitted one")
	}

	p.AllowedZoneIDs = []string{"zone-1", "zone-2"}
	if !p.AllowsZone("zone-2") {
		t.Error("an allowed zone was refused")
	}
	if p.AllowsZone("zone-3") {
		t.Error("a zone outside the list was permitted")
	}
}

func TestBreakerOpen(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	p := validProvider()

	if p.BreakerOpen(now) {
		t.Error("a fresh provider had its breaker open")
	}
	p.BreakerOpenUntil = now.Add(time.Minute)
	if !p.BreakerOpen(now) {
		t.Error("the breaker should be open")
	}
	if p.BreakerOpen(now.Add(2 * time.Minute)) {
		t.Error("the breaker should have closed once the time passed")
	}
}

// MatchZone is the authoritative path: it picks from the zones the provider
// actually holds, rather than guessing from the name's shape.
func TestMatchZonePicksTheLongestSuffix(t *testing.T) {
	zones := []string{"cyberjaya.pro", "intranet.my", "my", "exstudios.com.my"}

	cases := map[string]string{
		"vm.cyberjaya.pro":     "cyberjaya.pro",
		"cyberjaya.pro":        "cyberjaya.pro",
		"a.b.vm.cyberjaya.pro": "cyberjaya.pro",
		"thing.intranet.my":    "intranet.my",
		"app.exstudios.com.my": "exstudios.com.my",
		"something.else.my":    "my",
		"nothing.example.com":  "",
		"notcyberjaya.pro":     "",
		"":                     "",
	}
	for hostname, want := range cases {
		if got := MatchZone(hostname, zones); got != want {
			t.Errorf("MatchZone(%q) = %q, want %q", hostname, got, want)
		}
	}
}

// A near-miss must not match: "notcyberjaya.pro" ends with "cyberjaya.pro" as
// a string but is a different domain, and treating it as a match would write a
// DNS record into the wrong zone.
func TestMatchZoneRequiresALabelBoundary(t *testing.T) {
	if got := MatchZone("evilcyberjaya.pro", []string{"cyberjaya.pro"}); got != "" {
		t.Errorf("MatchZone matched across a label boundary: %q", got)
	}
}

func TestZoneOfIsOnlyAHint(t *testing.T) {
	if got := ZoneOf("vm.cyberjaya.pro"); got != "cyberjaya.pro" {
		t.Errorf("ZoneOf = %q, want cyberjaya.pro", got)
	}
	// Documented wrong answer: a two-label public suffix. Asserted so the
	// limitation is visible rather than discovered.
	if got := ZoneOf("app.example.co.uk"); got != "co.uk" {
		t.Errorf("ZoneOf = %q; the naive rule is expected to return co.uk here", got)
	}
	if got := ZoneOf("localhost"); got != "" {
		t.Errorf("ZoneOf(%q) = %q, want empty", "localhost", got)
	}
}
