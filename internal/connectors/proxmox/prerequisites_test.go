package proxmox

import (
	"regexp"
	"strings"
	"testing"

	"github.com/freezxp/proxui/internal/connector"
)

// These are the strings that actually reach a hypervisor's command line, so
// they are worth asserting on directly rather than only through a fake.
func TestNodePrerequisitesAreUsable(t *testing.T) {
	c := &Connector{}
	prereqs := c.NodePrerequisites()
	if len(prereqs) == 0 {
		t.Fatal("no prerequisites declared")
	}

	id := regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	seen := map[string]bool{}
	for _, p := range prereqs {
		if !id.MatchString(p.ID) {
			t.Errorf("%q is not usable as an identifier", p.ID)
		}
		if seen[p.ID] {
			t.Errorf("%q is declared twice; the lookup would be ambiguous", p.ID)
		}
		seen[p.ID] = true

		if p.Probe == "" || p.Needed == "" || p.Name == "" {
			t.Errorf("%s: every field is shown to an operator and none may be empty: %+v", p.ID, p)
		}
		// A probe must ask a question, not answer one: it is run for its exit
		// status on a node the portal does not own.
		if !strings.HasPrefix(p.Probe, "command -v ") {
			t.Errorf("%s: probe %q should test for a command", p.ID, p.Probe)
		}
		if p.Installable() && len(p.Packages) == 0 {
			t.Errorf("%s: installable but names no package, so the confirmation and the "+
				"audit entry would not say what is being put on the node", p.ID)
		}
		// The confirmation shows the command and the audit entry records it. A
		// package that appears in neither would be installed unannounced.
		for _, pkg := range p.Packages {
			if !strings.Contains(p.Install, pkg) {
				t.Errorf("%s: package %q is named but not installed by %q", p.ID, pkg, p.Install)
			}
		}
	}

	if !seen["lm-sensors"] || !seen["libguestfs-tools"] {
		t.Errorf("declared %v, want at least lm-sensors and libguestfs-tools — the two "+
			"things a node needs that Proxmox has no API for", seen)
	}
}

// The list is what the portal is willing to run as root. A caller naming an
// identifier gets one of these or nothing, so nothing else may creep in.
func TestNodePrerequisitesInstallOnlyThroughApt(t *testing.T) {
	for _, p := range (&Connector{}).NodePrerequisites() {
		if !p.Installable() {
			continue
		}
		if !strings.Contains(p.Install, "apt-get install -y ") {
			t.Errorf("%s: %q does not install a package with apt-get", p.ID, p.Install)
		}
		if strings.Contains(p.Install, "curl") || strings.Contains(p.Install, "wget") {
			t.Errorf("%s: %q fetches something from the network itself; a package "+
				"manager and its configured repositories are the whole point", p.ID, p.Install)
		}
	}
}

// The list must not be shared: a caller that modified what it was handed would
// change what the next one is told, and what the next one is told decides what
// runs on a node.
func TestNodePrerequisitesAreCopied(t *testing.T) {
	c := &Connector{}
	first := c.NodePrerequisites()
	first[0] = connector.NodePrerequisite{ID: "tampered", Install: "rm -rf /"}

	if second := c.NodePrerequisites(); second[0].ID == "tampered" {
		t.Fatal("the prerequisite list is shared between callers")
	}
}
