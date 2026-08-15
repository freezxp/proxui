package internal_test

// The layer rule, enforced rather than documented.
//
// docs/05-system-architecture.md and CLAUDE.md both state the import direction
// `domain <- app <- infra/transport/jobs`, and both described it as CI-enforced
// while nothing checked it. A rule nobody checks is a rule that decays: the
// cost of a violation is not the one import, it is that the next person
// reasonably assumes the boundary was never real.
//
// This walks the package graph with go/packages rather than adding a linter
// dependency, because the rule is project-specific enough that expressing it in
// Go is shorter than configuring a general tool to approximate it.

import (
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

const modulePath = "github.com/freezxp/proxui"

// layer classifies a package by its path prefix. Order matters: the first
// matching prefix wins, so more specific prefixes come first.
var layers = []struct {
	prefix string
	name   string
}{
	{"/internal/domain/", "domain"},
	{"/internal/app/", "app"},
	{"/internal/connector", "connector"},
	{"/internal/connectors/", "connectors"},
	{"/internal/edge/cloudflare", "edge-providers"},
	{"/internal/edge", "edge"},
	{"/internal/infra/", "infra"},
	{"/internal/transport/", "transport"},
	{"/internal/jobs", "jobs"},
}

// mayImport lists, per layer, the layers it is allowed to depend on. A layer
// may always import itself; anything absent is forbidden.
var mayImport = map[string][]string{
	// The centre. Domain depends on nothing of ours — that is what makes it
	// testable without a database and what stops business rules leaking
	// outward into whichever transport happened to need them first.
	"domain": {},

	// Application services orchestrate the domain behind ports they define.
	// They must not reach for a database driver or an HTTP router.
	"app": {"domain", "connector", "edge"},

	// The ports themselves.
	"connector": {"domain"},
	"edge":      {"domain"},

	// A platform integration implements the connector port and knows nothing
	// of the application that will call it. Crucially it may NOT import
	// another platform integration, or "just reuse that helper from proxmox"
	// quietly couples every platform to one of them.
	"connectors": {"connector", "domain"},

	// Same rule for edge providers, which is the reason this test grew an
	// entry the day internal/edge was created rather than a year later
	// (ADR 0004).
	"edge-providers": {"edge", "domain"},

	// The outside world. These are the only layers allowed to know about
	// Postgres, Redis, chi or the wire.
	"infra":     {"domain", "app", "connector", "edge"},
	"transport": {"domain", "app", "connector", "edge", "infra"},
	"jobs":      {"domain", "app", "connector", "edge", "infra"},
}

// knownViolations is debt, frozen on 2026-08-15 when this test was written and
// immediately found eleven breaches of a rule two documents claimed was
// enforced.
//
// It is a ratchet, not an amnesty. New violations fail; these eleven are
// allowed to persist until someone fixes them, and the test fails if one is
// *fixed* without being removed here, so the list can only shrink.
//
// All eleven are the same mistake in three flavours: treating infra packages
// as utilities because they read like them. `crypto` and `metrics` feel like
// helpers rather than adapters, so app code imports them directly instead of
// depending on a port the infra layer implements. `notify` and `oauth` are
// plainer breaches — app orchestration reaching for the concrete sender.
//
// Fixing them means defining the port in app (or domain) and moving the
// implementation behind it, which is a refactor of its own and not something
// to do halfway through a feature.
var knownViolations = map[string]bool{
	"app/ports -> infra/crypto":    true,
	"app/command -> infra/crypto":  true,
	"app/command -> infra/metrics": true,
	"app/sync -> infra/crypto":     true,
	"app/sync -> infra/metrics":    true,
	"app/notify -> infra/crypto":   true,
	"app/notify -> infra/metrics":  true,
	"app/notify -> infra/notify":   true,
	"app/setting -> infra/crypto":  true,
	"app/setting -> infra/oauth":   true,
	"app/alert -> infra/metrics":   true,
}

// short renders an edge the way knownViolations keys it.
func short(from, to string) string {
	trim := func(p string) string { return strings.TrimPrefix(p, modulePath+"/internal/") }
	return trim(from) + " -> " + trim(to)
}

func layerOf(pkgPath string) (string, bool) {
	suffix := strings.TrimPrefix(pkgPath, modulePath)
	if suffix == pkgPath {
		return "", false // not ours
	}
	for _, l := range layers {
		if strings.HasPrefix(suffix, l.prefix) || suffix == strings.TrimSuffix(l.prefix, "/") {
			return l.name, true
		}
	}
	return "", false
}

func TestLayersImportOnlyInwards(t *testing.T) {
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedImports}
	pkgs, err := packages.Load(cfg, modulePath+"/internal/...")
	if err != nil {
		t.Fatalf("load packages: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("no packages loaded; the layer rule is not being checked")
	}

	checked := 0
	seen := map[string]bool{}
	for _, pkg := range pkgs {
		from, ok := layerOf(pkg.PkgPath)
		if !ok {
			continue
		}
		allowed, known := mayImport[from]
		if !known {
			t.Errorf("package %s is in layer %q, which has no rule; add one", pkg.PkgPath, from)
			continue
		}
		checked++

		for imported := range pkg.Imports {
			to, ok := layerOf(imported)
			if !ok || to == from {
				continue
			}
			if contains(allowed, to) {
				continue
			}
			edge := short(pkg.PkgPath, imported)
			seen[edge] = true
			if knownViolations[edge] {
				continue // frozen debt, see knownViolations
			}
			t.Errorf("%s (%s) imports %s (%s)\n    %s may import %v — see docs/05-system-architecture.md\n"+
				"    If this is deliberate and permanent, change the rule. If it is debt, it still fails: "+
				"knownViolations is frozen and does not accept new entries.",
				pkg.PkgPath, from, imported, to, from, allowed)
		}
	}

	// The ratchet's other direction: an exception that no longer applies must
	// be deleted, or the list stops describing reality and quietly re-permits
	// a violation someone already paid to remove.
	for edge := range knownViolations {
		if !seen[edge] {
			t.Errorf("knownViolations lists %q, which no longer happens — delete the entry", edge)
		}
	}

	// Guards the guard: a typo in a prefix would silently check nothing.
	if checked < 10 {
		t.Errorf("only %d packages were classified; the layer prefixes are probably wrong", checked)
	}
}

// The domain is the rule worth stating twice. It may import the standard
// library and nothing of ours, which is what keeps it free of a database.
func TestDomainDependsOnNothingOfOurs(t *testing.T) {
	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedImports}
	pkgs, err := packages.Load(cfg, modulePath+"/internal/domain/...")
	if err != nil {
		t.Fatalf("load packages: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("no domain packages loaded")
	}

	for _, pkg := range pkgs {
		for imported := range pkg.Imports {
			if !strings.HasPrefix(imported, modulePath) {
				continue
			}
			// Sibling domain packages are fine; the estate is one model.
			if strings.HasPrefix(imported, modulePath+"/internal/domain/") {
				continue
			}
			t.Errorf("domain package %s imports %s; the domain must not depend on outer layers",
				pkg.PkgPath, imported)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
