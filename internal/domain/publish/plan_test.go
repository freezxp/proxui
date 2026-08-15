package publish

import (
	"errors"
	"testing"
)

func currentTable() Table {
	return Table{
		{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
		{Hostname: "app.example.com", Service: "http://10.0.0.9:80"},
		CatchAll(),
	}
}

func entryFor(p Plan, hostname string) (Entry, bool) {
	for _, e := range p.Entries {
		if e.Before != nil && e.Before.Hostname == hostname {
			return e, true
		}
		if e.After != nil && e.After.Hostname == hostname {
			return e, true
		}
	}
	return Entry{}, false
}

func TestBuildPlanSeesAnAddition(t *testing.T) {
	desired := Table{
		{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
		{Hostname: "app.example.com", Service: "http://10.0.0.9:80"},
		{Hostname: "new.example.com", Service: "http://10.0.0.20:3000"},
		CatchAll(),
	}
	plan := BuildPlan(currentTable(), desired, self)

	if !plan.Safe() {
		t.Fatalf("a valid addition was refused: %v", plan.Refusal)
	}
	if plan.Added != 1 || plan.Removed != 0 || plan.Modified != 0 {
		t.Errorf("added=%d removed=%d modified=%d, want 1/0/0", plan.Added, plan.Removed, plan.Modified)
	}
	if e, ok := entryFor(plan, "new.example.com"); !ok || e.Change != ChangeAdded {
		t.Errorf("entry = %+v, want an addition", e)
	}
}

// The mistake a full-table PUT makes easy: something that was in the table is
// simply not in the new one, and vanishes without anyone deciding to remove it.
func TestBuildPlanSeesADeletionNobodyAskedFor(t *testing.T) {
	desired := Table{
		{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
		CatchAll(),
	}
	plan := BuildPlan(currentTable(), desired, self)

	if plan.Removed != 1 {
		t.Fatalf("removed = %d, want 1", plan.Removed)
	}
	e, ok := entryFor(plan, "app.example.com")
	if !ok || e.Change != ChangeRemoved {
		t.Errorf("entry = %+v, want a removal", e)
	}
	if e.Before == nil || e.After != nil {
		t.Error("a removal must carry what was there and nothing that replaces it")
	}
}

func TestBuildPlanSeesAModification(t *testing.T) {
	desired := Table{
		{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
		{Hostname: "app.example.com", Service: "http://10.0.0.99:80"}, // moved host
		CatchAll(),
	}
	plan := BuildPlan(currentTable(), desired, self)

	if plan.Modified != 1 || plan.Added != 0 || plan.Removed != 0 {
		t.Fatalf("modified=%d added=%d removed=%d, want 1/0/0",
			plan.Modified, plan.Added, plan.Removed)
	}
	e, _ := entryFor(plan, "app.example.com")
	if e.Before.Service == e.After.Service {
		t.Error("a modification must carry both sides")
	}
}

// First match wins, so moving a rule changes behaviour even when nothing about
// the rule itself differs. Easy to do by accident and hard to see.
func TestBuildPlanSeesAReorder(t *testing.T) {
	desired := Table{
		{Hostname: "app.example.com", Service: "http://10.0.0.9:80"},
		{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
		CatchAll(),
	}
	plan := BuildPlan(currentTable(), desired, self)

	if plan.Moved != 2 {
		t.Fatalf("moved = %d, want both rules", plan.Moved)
	}
	if plan.Added+plan.Removed+plan.Modified != 0 {
		t.Error("a reorder is not an add, a remove or a change")
	}
	e, _ := entryFor(plan, "vm.example.com")
	if e.FromIndex != 0 || e.ToIndex != 1 {
		t.Errorf("indices = %d -> %d, want 0 -> 1", e.FromIndex, e.ToIndex)
	}
}

func TestBuildPlanNoticesNothingChanged(t *testing.T) {
	plan := BuildPlan(currentTable(), currentTable(), self)

	if plan.TouchesAnything() {
		t.Errorf("an identical table reported changes: %+v", plan)
	}
	if plan.Unchanged != 3 {
		t.Errorf("unchanged = %d, want 3", plan.Unchanged)
	}
}

// The refusal is carried rather than returned, so a caller can show what would
// have happened alongside why it will not.
func TestAPlanThatWouldRemoveThePortalIsRefusedButStillDescribed(t *testing.T) {
	desired := Table{
		{Hostname: "app.example.com", Service: "http://10.0.0.9:80"},
		CatchAll(),
	}
	plan := BuildPlan(currentTable(), desired, self)

	if plan.Safe() {
		t.Fatal("a plan removing the portal's own route was allowed")
	}
	if !errors.Is(plan.Refusal, ErrSelfRemoved) {
		t.Errorf("refusal = %v, want ErrSelfRemoved", plan.Refusal)
	}
	// The diff must still be there: seeing what you nearly did is the point.
	if plan.Removed != 1 {
		t.Errorf("removed = %d; the diff should be computed even when refused", plan.Removed)
	}
}

// A caller that forgot the terminator gets a correct plan rather than a
// refusal about something it never meant to say.
func TestBuildPlanNormalisesTheCatchAll(t *testing.T) {
	desired := Table{
		{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
		{Hostname: "app.example.com", Service: "http://10.0.0.9:80"},
	}
	plan := BuildPlan(currentTable(), desired, self)

	if !plan.Safe() {
		t.Fatalf("refused a plan that only omitted the catch-all: %v", plan.Refusal)
	}
	if plan.TouchesAnything() {
		t.Errorf("adding the implied catch-all counted as a change: %+v", plan)
	}
}

func TestCheckFreshUsesTheProvidersVersion(t *testing.T) {
	if err := CheckFresh(34, 34); err != nil {
		t.Errorf("same version reported stale: %v", err)
	}
	err := CheckFresh(34, 35)
	if !errors.Is(err, ErrStaleRead) {
		t.Fatalf("got %v, want ErrStaleRead", err)
	}
	// The message has to say what happened, since the fix is to re-read and
	// look rather than to try again.
	if err != nil && !contains(err.Error(), "version 34") {
		t.Errorf("error = %q, want both versions named", err)
	}

	// A provider that publishes no version cannot be checked this way, and
	// claiming freshness would be worse than admitting the check is unavailable.
	if err := CheckFresh(0, 12); err != nil {
		t.Errorf("a missing version should not report staleness: %v", err)
	}
	if err := CheckFresh(12, 0); err != nil {
		t.Errorf("a missing version should not report staleness: %v", err)
	}
}

func TestSameTableIsTheFallbackComparison(t *testing.T) {
	if !SameTable(currentTable(), currentTable()) {
		t.Error("identical tables compared unequal")
	}

	reordered := Table{
		{Hostname: "app.example.com", Service: "http://10.0.0.9:80"},
		{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
		CatchAll(),
	}
	// Order is part of the table's meaning, so a reorder is a difference.
	if SameTable(currentTable(), reordered) {
		t.Error("a reordered table compared equal")
	}

	shorter := Table{{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"}, CatchAll()}
	if SameTable(currentTable(), shorter) {
		t.Error("tables of different lengths compared equal")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// The catch-all sits last by construction, so its index shifts whenever
// anything is added before it. Calling that a reorder would put a spurious
// "moved" entry in every diff and teach people to ignore the one category that
// exists to catch real, dangerous reorderings.
func TestTheCatchAllIsNotReportedAsMovedWhenItIsStillLast(t *testing.T) {
	desired := Table{
		{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
		{Hostname: "app.example.com", Service: "http://10.0.0.9:80"},
		{Hostname: "new.example.com", Service: "http://10.0.0.20:3000"},
		CatchAll(),
	}
	plan := BuildPlan(currentTable(), desired, self)

	if plan.Added != 1 {
		t.Fatalf("added = %d, want 1", plan.Added)
	}
	if plan.Moved != 0 {
		t.Errorf("moved = %d, want 0 — only the catch-all shifted, and it is still last", plan.Moved)
	}
	for _, e := range plan.Entries {
		if e.Before != nil && e.Before.IsCatchAll() && e.Change == ChangeMoved {
			t.Error("the catch-all was reported as moved while remaining last")
		}
	}
}

// A genuine reorder of real rules must still be caught, or the fix above would
// have blunted the check it was meant to sharpen.
func TestARealReorderIsStillReported(t *testing.T) {
	desired := Table{
		{Hostname: "app.example.com", Service: "http://10.0.0.9:80"},
		{Hostname: "vm.example.com", Service: "http://10.0.0.5:8080"},
		CatchAll(),
	}
	if plan := BuildPlan(currentTable(), desired, self); plan.Moved != 2 {
		t.Errorf("moved = %d, want 2", plan.Moved)
	}
}
