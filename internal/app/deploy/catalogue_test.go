package deploy

import (
	"regexp"
	"strings"
	"testing"
)

// The generated catalogue is what an operator picks from and what decides which
// vendored script runs as root on a hypervisor, so its shape is asserted rather
// than assumed to have survived a regeneration.
func TestCatalogueIsUsable(t *testing.T) {
	apps := Catalogue()
	if len(apps) < 100 {
		t.Fatalf("catalogue has %d entries; the generator produced 590 and this looks broken", len(apps))
	}

	id := regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	seen := map[string]bool{}
	for _, a := range apps {
		if !id.MatchString(a.ID) {
			t.Errorf("%q is not usable as an identifier — it is looked up and then names a file", a.ID)
		}
		if seen[a.ID] {
			t.Errorf("%q appears twice; the lookup would be ambiguous", a.ID)
		}
		seen[a.ID] = true
		if strings.TrimSpace(a.Name) == "" {
			t.Errorf("%s has no name, so a list would show a blank row", a.ID)
		}
	}
}

// Every entry must have the script it names. A catalogue offering something
// that was not vendored is a deploy that fails after the operator has chosen a
// node and confirmed a command.
func TestEveryEntryHasItsScript(t *testing.T) {
	for _, a := range Catalogue() {
		body, err := Script(a.ID)
		if err != nil {
			t.Errorf("%s: %v", a.ID, err)
			continue
		}
		if len(body) < 200 {
			t.Errorf("%s: vendored script is %d bytes, which is not a script", a.ID, len(body))
		}
		if !strings.Contains(string(body), "APP=") {
			t.Errorf("%s: vendored file does not look like a container script", a.ID)
		}
	}
}

// The whole control, asserted directly: an identifier the catalogue does not
// know never becomes a path. Without this check the id would be concatenated
// into an embedded-filesystem lookup, and traversal is the first thing anyone
// would try.
func TestUnknownIdentifiersAreRefused(t *testing.T) {
	for _, bad := range []string{
		"",
		"nginx",
		"../../../etc/passwd",
		"adguard/../../secrets",
		"adguard; reboot",
		"ADGUARD",
	} {
		if _, ok := Find(bad); ok {
			t.Errorf("Find(%q) resolved", bad)
		}
		if _, err := Script(bad); err == nil {
			t.Errorf("Script(%q) returned a script", bad)
		}
	}
}

// Both halves are pinned. An unpinned engine would mean the scripts pulled
// whatever was on a branch that morning, which is the thing vendoring exists to
// stop (ADR 0012).
func TestUpstreamIsPinnedToCommits(t *testing.T) {
	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	for name, ref := range map[string]string{"scripts": ScriptsRef, "engine": EngineRef} {
		if !sha.MatchString(ref) {
			t.Errorf("the %s pin is %q, which is not a commit — a branch would make a deploy "+
				"unreproducible and unreviewable", name, ref)
		}
	}
	if got := RawURL(ScriptsRepo, ScriptsRef); !strings.HasSuffix(got, ScriptsRef) {
		t.Errorf("RawURL = %q, want it to end at the pinned commit", got)
	}
}

func TestSearchNarrowsByNameAndTag(t *testing.T) {
	all := Catalogue()
	if got := Search("", ""); len(got) != len(all) {
		t.Errorf("an empty query returned %d of %d", len(got), len(all))
	}
	// Case-insensitive on the name, and exact on the identifier.
	if got := Search("ADGUARD", ""); len(got) == 0 {
		t.Error("searching for a known app by name found nothing")
	}
	tags := Tags()
	if len(tags) == 0 {
		t.Fatal("no tags, so the list cannot be filtered down from 590")
	}
	for _, a := range Search("", tags[0]) {
		if !hasTag(a, tags[0]) {
			t.Errorf("%s came back for tag %q it does not carry", a.ID, tags[0])
		}
	}
}

// The list is what decides what runs. A caller that sorted what it was handed
// would change what the next one is offered.
func TestCatalogueIsCopied(t *testing.T) {
	first := Catalogue()
	first[0] = App{ID: "tampered"}
	if Catalogue()[0].ID == "tampered" {
		t.Fatal("the catalogue is shared between callers")
	}
}
