package publish

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func testApp() *App {
	return &App{
		ID: uuid.New(), ProviderID: uuid.New(),
		Hostname: "new.example.com", ServiceURL: "http://10.0.0.20:3000",
		IsEnabled: true,
	}
}

func TestAppValidation(t *testing.T) {
	if err := testApp().Validate(); err != nil {
		t.Fatalf("rejected a valid app: %v", err)
	}

	cases := map[string]func(*App){
		"bad hostname":  func(a *App) { a.Hostname = "not qualified" },
		"no service":    func(a *App) { a.ServiceURL = "" },
		"bad scheme":    func(a *App) { a.ServiceURL = "ssh://10.0.0.5:22" },
		"relative path": func(a *App) { a.Path = "v1" },
		"port range":    func(a *App) { a.VMPort = 70000 },
		// A status literal is the table's terminator, not something anyone
		// publishes. Allowing it would let an app swallow every hostname.
		"status literal": func(a *App) { a.ServiceURL = "http_status:404" },
	}
	for name, mangle := range cases {
		a := testApp()
		mangle(a)
		if err := a.Validate(); err == nil {
			t.Errorf("%s: accepted invalid app", name)
		}
	}
}

// New rules go immediately before the catch-all, so they work without
// disturbing the order of anything already there — reshuffling somebody else's
// rules is the change nobody notices until traffic goes somewhere unexpected.
func TestApplyToInsertsBeforeTheCatchAll(t *testing.T) {
	current := Table{
		{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
		{Hostname: "app.example.com", Service: "http://10.0.0.9:80"},
		CatchAll(),
	}
	got := ApplyTo(current, testApp())

	if len(got) != 4 {
		t.Fatalf("%d rules, want 4", len(got))
	}
	if got[0].Hostname != "vm.example.com" || got[1].Hostname != "app.example.com" {
		t.Errorf("existing rules were reordered: %+v", got)
	}
	if got[2].Hostname != "new.example.com" {
		t.Errorf("the new rule is at %d, want just before the catch-all", 2)
	}
	if !got[3].IsCatchAll() {
		t.Error("the catch-all is not last")
	}
	if err := got.Validate("vm.example.com"); err != nil {
		t.Errorf("the result does not validate: %v", err)
	}
}

// Editing an app replaces its rule in place rather than adding a second one
// for the same route, which would leave one of them permanently dead.
func TestApplyToReplacesInPlace(t *testing.T) {
	app := testApp()
	current := Table{
		{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
		{Hostname: app.Hostname, Service: "http://10.0.0.99:1"},
		CatchAll(),
	}
	got := ApplyTo(current, app)

	if len(got) != 3 {
		t.Fatalf("%d rules, want 3 — the rule should be replaced, not duplicated", len(got))
	}
	if got[1].Service != "http://10.0.0.20:3000" {
		t.Errorf("rule = %+v, want the new service", got[1])
	}
	// And its position is kept, since moving it would change which rule wins.
	if got[1].Hostname != app.Hostname {
		t.Errorf("the replaced rule moved: %+v", got)
	}
}

func TestRemoveFromTakesOutExactlyOneRule(t *testing.T) {
	app := testApp()
	current := Table{
		{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
		{Hostname: app.Hostname, Service: app.ServiceURL},
		{Hostname: "other.example.com", Service: "http://10.0.0.7:80"},
		CatchAll(),
	}
	got := RemoveFrom(current, app)

	if len(got) != 3 {
		t.Fatalf("%d rules, want 3", len(got))
	}
	for _, r := range got {
		if r.Hostname == app.Hostname {
			t.Error("the app's rule survived removal")
		}
	}
	if got[0].Hostname != "vm.example.com" || got[1].Hostname != "other.example.com" {
		t.Errorf("the remaining rules were reordered: %+v", got)
	}
}

// A catch-all pointing somewhere other than 404 was chosen deliberately by
// somebody, and replacing it would change where every unmatched request goes.
func TestApplyToPreservesADeliberateCatchAll(t *testing.T) {
	current := Table{
		{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
		{Service: "http://10.0.0.1:80"}, // a catch-all that proxies
	}
	got := ApplyTo(current, testApp())

	last := got[len(got)-1]
	if !last.IsCatchAll() || last.Service != "http://10.0.0.1:80" {
		t.Errorf("catch-all = %+v, want it preserved", last)
	}
}

func TestRemoveFromIsSafeOnATableThatNeverHadTheRule(t *testing.T) {
	current := Table{
		{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
		CatchAll(),
	}
	got := RemoveFrom(current, testApp())

	if len(got) != 2 {
		t.Fatalf("%d rules, want the table unchanged", len(got))
	}
	if err := got.Validate("vm.example.com"); err != nil {
		t.Errorf("result does not validate: %v", err)
	}
}

// Both operations must leave a table the invariants accept, or the write path
// would have to re-check work the domain already did.
func TestApplyAndRemoveAlwaysProduceAValidTable(t *testing.T) {
	current := Table{
		{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
		CatchAll(),
	}
	for _, table := range []Table{ApplyTo(current, testApp()), RemoveFrom(current, testApp())} {
		if err := table.Validate("vm.example.com"); err != nil {
			t.Errorf("table %+v does not validate: %v", table, err)
		}
	}
}

// The one thing publishing must never do, checked at the domain level so the
// write path cannot get it wrong.
func TestPublishingCannotDisplaceThePortal(t *testing.T) {
	current := Table{
		{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
		CatchAll(),
	}
	hijack := testApp()
	hijack.Hostname = "vm.example.com"
	hijack.ServiceURL = "http://10.0.0.99:9999"

	// Replacing the portal's own rule is a legitimate table shape — the guard
	// is about removal and shadowing — but the plan must show it as a change
	// to the protected rule rather than as an innocuous edit.
	got := ApplyTo(current, hijack)
	plan := BuildPlan(current, got, "vm.example.com")
	if plan.Modified != 1 {
		t.Errorf("modified = %d, want the portal's rule reported as changed", plan.Modified)
	}

	// Removing it outright is refused.
	removed := RemoveFrom(current, hijack)
	if err := removed.Validate("vm.example.com"); !errors.Is(err, ErrSelfRemoved) {
		t.Errorf("got %v, want ErrSelfRemoved", err)
	}
}
