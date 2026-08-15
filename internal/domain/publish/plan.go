package publish

import (
	"errors"
	"fmt"
	"strings"
)

// ErrStaleRead means the routing table changed between being read and a
// change being proposed against it (PUB-31).
//
// Never resolved by retrying the same write. The whole point is that somebody
// else's change is sitting in the table and would be silently deleted, so the
// only correct response is to re-read and let a human look at the difference.
var ErrStaleRead = errors.New("publish: the routing table changed since it was read")

// Change is what happens to one rule.
type Change string

const (
	ChangeAdded     Change = "added"
	ChangeRemoved   Change = "removed"
	ChangeModified  Change = "modified"
	ChangeMoved     Change = "moved"
	ChangeUnchanged Change = "unchanged"
)

// Entry is one rule's fate in a proposed change.
type Entry struct {
	Change Change
	// Key identifies the rule across both tables: hostname plus path, which
	// is what makes a rule the same rule.
	Key string
	// Before and After are nil for an addition and a removal respectively.
	Before *Rule
	After  *Rule
	// FromIndex and ToIndex are positions, -1 where the rule is absent. Order
	// is semantic here — first match wins — so a move is a real change even
	// when nothing about the rule itself differs.
	FromIndex int
	ToIndex   int
}

// Plan is a proposed change to a routing table, and whether it may be sent.
type Plan struct {
	Entries []Entry

	Added     int
	Removed   int
	Modified  int
	Moved     int
	Unchanged int

	// Refusal is why this plan must not be sent, or nil. Carried rather than
	// returned as an error so a caller can show the diff *and* the reason
	// together — seeing what you nearly did is most of the value.
	Refusal error
}

// Safe reports whether the plan may be sent.
func (p Plan) Safe() bool { return p.Refusal == nil }

// TouchesAnything reports whether the plan would change the table at all.
// A plan that changes nothing should not be sent: it would bump the
// provider's version for no reason and make a later conflict check lie.
func (p Plan) TouchesAnything() bool {
	return p.Added+p.Removed+p.Modified+p.Moved > 0
}

func ruleKey(r Rule) string {
	return strings.ToLower(strings.TrimSpace(r.Hostname)) + "\x00" + strings.TrimSpace(r.Path)
}

// BuildPlan works out what turning current into desired would do.
//
// desired is normalised first — exactly one catch-all, last — so a caller that
// forgot the terminator gets a correct plan rather than a refusal about
// something it never meant to say.
func BuildPlan(current, desired Table, selfHostname string) Plan {
	desired = EnsureCatchAll(desired)

	plan := Plan{Refusal: desired.Validate(selfHostname)}

	beforeAt := map[string]int{}
	for i, r := range current {
		beforeAt[ruleKey(r)] = i
	}
	seen := map[string]bool{}

	for i, want := range desired {
		key := ruleKey(want)
		seen[key] = true
		after := want

		j, existed := beforeAt[key]
		if !existed {
			plan.Entries = append(plan.Entries, Entry{
				Change: ChangeAdded, Key: key, After: &after, FromIndex: -1, ToIndex: i,
			})
			plan.Added++
			continue
		}

		before := current[j]
		switch {
		case before.Service != want.Service:
			plan.Entries = append(plan.Entries, Entry{
				Change: ChangeModified, Key: key, Before: &before, After: &after,
				FromIndex: j, ToIndex: i,
			})
			plan.Modified++
		// The catch-all is last by construction, so its index moves whenever
		// anything is added or removed before it. Reporting that as a reorder
		// would put a spurious "moved" entry in every single diff and teach
		// people to ignore the category that exists to catch real ones.
		case j != i && !(want.IsCatchAll() && j == len(current)-1 && i == len(desired)-1):
			// Same rule, different position. Worth its own category because
			// first-match-wins makes order meaningful, and a reorder is easy
			// to make by accident and hard to see.
			plan.Entries = append(plan.Entries, Entry{
				Change: ChangeMoved, Key: key, Before: &before, After: &after,
				FromIndex: j, ToIndex: i,
			})
			plan.Moved++
		default:
			plan.Entries = append(plan.Entries, Entry{
				Change: ChangeUnchanged, Key: key, Before: &before, After: &after,
				FromIndex: j, ToIndex: i,
			})
			plan.Unchanged++
		}
	}

	// Anything in the current table with no counterpart is being deleted —
	// including rules the portal did not create, which is exactly the mistake
	// a full-table PUT makes easy and this diff exists to expose.
	for i, have := range current {
		key := ruleKey(have)
		if seen[key] {
			continue
		}
		before := have
		plan.Entries = append(plan.Entries, Entry{
			Change: ChangeRemoved, Key: key, Before: &before, FromIndex: i, ToIndex: -1,
		})
		plan.Removed++
	}

	return plan
}

// CheckFresh reports whether a table read at readVersion is still current.
//
// Cloudflare increments a version on every configuration write, so this is
// exact rather than a heuristic — which matters, because the alternative is
// diffing two rule arrays and hoping that a change which happens to produce an
// identical array is genuinely not a change.
//
// A zero version on either side means the provider does not offer one, and the
// caller must fall back to comparing the rules themselves.
func CheckFresh(readVersion, currentVersion int) error {
	if readVersion == 0 || currentVersion == 0 {
		return nil
	}
	if readVersion != currentVersion {
		return fmt.Errorf("%w: it was version %d when read and is version %d now",
			ErrStaleRead, readVersion, currentVersion)
	}
	return nil
}

// SameTable reports whether two routing tables are identical, order included.
// The fallback for a provider that publishes no version.
func SameTable(a, b Table) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Hostname != b[i].Hostname || a[i].Path != b[i].Path || a[i].Service != b[i].Service {
			return false
		}
	}
	return true
}
