// Package deploy installs an application into an LXC container on a node, from
// a catalogue the portal ships (ADR 0012).
//
// The applications are the Proxmox VE Helper-Scripts: one script per app that
// creates a container, installs the app into it and configures it. They are
// vendored beside this file at a reviewed commit, so the bytes the portal runs
// are the bytes in this repository rather than whatever a URL serves today, and
// bumping the pin is a diff somebody reads.
//
// This is the largest thing the portal does to a node and the boundary is worth
// restating: a request names an identifier from this catalogue, never a command,
// a URL or a package. Everything else — cores, memory, hostname — is validated
// and passed as an environment assignment. Nothing a caller sends is placed in
// what runs.
package deploy

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

// scripts holds every vendored container script. Plain files rather than an
// archive: the point of vendoring is that a pin bump can be reviewed, and a
// tarball's diff says only that some bytes changed.
//
//go:embed scripts/ct/*.sh
var scripts embed.FS

// App is one entry an operator can deploy.
//
// The resource fields are the script's own defaults, for showing and for
// prefilling a form. A zero means the script decides — usually because it
// branches on the container OS — and the portal then sends nothing rather than
// overriding with a guess.
type App struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Tags []string `json:"tags,omitempty"`
	// Source is the application's own project page, not the script's.
	Source   string `json:"source,omitempty"`
	Cores    int    `json:"cores,omitempty"`
	MemoryMB int    `json:"memory_mb,omitempty"`
	DiskGB   int    `json:"disk_gb,omitempty"`
	OS       string `json:"os,omitempty"`
	Version  string `json:"version,omitempty"`
	// Privileged is the exception and is why the field is named for it: all but
	// a handful of these run unprivileged, and the ones that cannot are worth
	// noticing in a list.
	Privileged bool `json:"privileged,omitempty"`
}

// Catalogue returns every application, by identifier.
//
// Copied rather than shared: a caller that sorted or filtered what it was given
// would change what the next one is offered, and what the next one is offered
// decides what runs on a node.
func Catalogue() []App {
	out := make([]App, len(catalogue))
	copy(out, catalogue)
	return out
}

// Find returns one application by identifier.
//
// This is the whole control in one function. Everything a request can ask for
// passes through here, and an identifier that does not resolve goes no further
// — it is never turned into a filename, a URL or a command.
func Find(id string) (App, bool) {
	for _, a := range catalogue {
		if a.ID == id {
			return a, true
		}
	}
	return App{}, false
}

// Script returns the vendored bytes for one application.
//
// The path is built from an identifier this package already matched against the
// catalogue, so it names a file that was embedded at build time; a caller's
// string never reaches the filesystem.
func Script(id string) ([]byte, error) {
	if _, ok := Find(id); !ok {
		return nil, fmt.Errorf("deploy: no application called %q", id)
	}
	body, err := scripts.ReadFile("scripts/ct/" + id + ".sh")
	if err != nil {
		return nil, fmt.Errorf("deploy: %s is in the catalogue but was not vendored: %w", id, err)
	}
	return body, nil
}

// Tags lists every tag in the catalogue, for filtering a list of 590 things
// down to something a person can look at.
func Tags() []string {
	seen := map[string]bool{}
	for _, a := range catalogue {
		for _, t := range a.Tags {
			seen[t] = true
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// RawURL is where the scripts resolve the rest of themselves from while they
// run — pinned to the reviewed commit rather than left to default to a branch.
func RawURL(repo, ref string) string {
	return "https://raw.githubusercontent.com/" + repo + "/" + ref
}

// Search narrows the catalogue by a substring of the name or identifier and by
// tag. Both are optional; an empty query is the whole catalogue.
func Search(query, tag string) []App {
	query = strings.ToLower(strings.TrimSpace(query))
	out := []App{}
	for _, a := range catalogue {
		if tag != "" && !hasTag(a, tag) {
			continue
		}
		if query != "" &&
			!strings.Contains(strings.ToLower(a.Name), query) &&
			!strings.Contains(a.ID, query) {
			continue
		}
		out = append(out, a)
	}
	return out
}

func hasTag(a App, tag string) bool {
	for _, t := range a.Tags {
		if t == tag {
			return true
		}
	}
	return false
}
