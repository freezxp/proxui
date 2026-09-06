// Command gen vendors the Proxmox VE Helper-Scripts and writes the catalogue
// that ships with them (ADR 0012).
//
// Run through `make gen-apps`. It fetches the two upstream repositories at the
// commits named below, writes every container script into
// internal/app/deploy/scripts/ct, and generates catalogue_gen.go from their
// headers. Both outputs are committed: the scripts are what the portal runs, so
// bumping the pin has to be a diff somebody can read rather than an opaque blob
// that changed.
//
// Updating: change scriptsRef and engineRef, run `make gen-apps`, read the
// diff. That review is the whole compensating control — everything else in this
// feature assumes somebody did it.
package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The reviewed upstream commits. The engine lives in its own repository and is
// resolved by the scripts at run time, so it is pinned here even though nothing
// of it is vendored.
const (
	scriptsRepo = "community-scripts/ProxmoxVE"
	scriptsRef  = "08fdd8875172abcd3c167f13a00bdb65fcb0e61e"
	engineRepo  = "community-scripts/core"
	engineRef   = "b7ddecf9f0ddc88781657aff407b78867472ebd5"
)

const (
	outDir     = "internal/app/deploy"
	scriptsDir = outDir + "/scripts/ct"
	outFile    = outDir + "/catalogue_gen.go"
)

type app struct {
	ID, Name, Source, OS, Version string
	Tags                          []string
	Cores, MemoryMB, DiskGB       int
	Unprivileged                  bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run() error {
	scripts, err := fetchContainerScripts()
	if err != nil {
		return err
	}
	if len(scripts) == 0 {
		return fmt.Errorf("no container scripts at %s", scriptsRef)
	}

	// Written fresh rather than merged: a script that upstream deleted must
	// leave, and a stale one left behind would be offered by a catalogue that
	// no longer lists it.
	if err := os.RemoveAll(scriptsDir); err != nil {
		return err
	}
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		return err
	}

	apps := make([]app, 0, len(scripts))
	for _, name := range sortedKeys(scripts) {
		body := scripts[name]
		if err := os.WriteFile(filepath.Join(scriptsDir, name+".sh"), body, 0o644); err != nil {
			return err
		}
		apps = append(apps, parse(name, string(body)))
	}

	fmt.Printf("gen: %d container scripts from %s@%.7s\n", len(apps), scriptsRepo, scriptsRef)
	return writeCatalogue(apps)
}

// fetchContainerScripts pulls the repository once as a tarball rather than
// asking for 590 files, and keeps the ct/ directory.
func fetchContainerScripts() (map[string][]byte, error) {
	url := fmt.Sprintf("https://codeload.github.com/%s/tar.gz/%s", scriptsRepo, scriptsRef)
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %s", url, resp.Status)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	out := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		// Paths are "<repo>-<sha>/ct/<name>.sh"; the prefix carries the commit
		// and is not worth reproducing on disk.
		parts := strings.SplitN(header.Name, "/", 3)
		if header.Typeflag != tar.TypeReg || len(parts) != 3 || parts[1] != "ct" {
			continue
		}
		name := strings.TrimSuffix(parts[2], ".sh")
		if name == parts[2] || strings.ContainsAny(name, "/\\") {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return nil, err
		}
		out[name] = body
	}
	return out, nil
}

// The header every container script carries. `APP=` is present in all of them;
// the resource defaults in most. Where one is missing the script sets it itself,
// usually branching on the container OS — so an absent value here means "let the
// script decide", not "zero".
var (
	appName   = regexp.MustCompile(`(?m)^APP="([^"]+)"`)
	sourceURL = regexp.MustCompile(`(?m)^# Source:\s*(\S+)`)
)

func varPattern(key string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^` + key + `="?\$\{` + key + `:-([^}"]*)\}"?`)
}

func parse(id, body string) app {
	a := app{ID: id, Name: id, Unprivileged: true}
	if m := appName.FindStringSubmatch(body); m != nil {
		a.Name = m[1]
	}
	if m := sourceURL.FindStringSubmatch(body); m != nil {
		a.Source = m[1]
	}
	a.Cores = number(body, "var_cpu")
	a.MemoryMB = number(body, "var_ram")
	a.DiskGB = number(body, "var_disk")
	a.OS = text(body, "var_os")
	a.Version = text(body, "var_version")
	if v := text(body, "var_unprivileged"); v != "" {
		a.Unprivileged = v != "0"
	}
	if tags := text(body, "var_tags"); tags != "" {
		for _, tag := range strings.Split(tags, ";") {
			if tag = strings.TrimSpace(tag); tag != "" {
				a.Tags = append(a.Tags, tag)
			}
		}
	}
	return a
}

func text(body, key string) string {
	m := varPattern(key).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func number(body, key string) int {
	n, err := strconv.Atoi(text(body, key))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func writeCatalogue(apps []app) error {
	var b strings.Builder
	fmt.Fprintf(&b, `// Code generated by internal/app/deploy/gen. DO NOT EDIT.
//
// Regenerate with "make gen-apps" after changing the pinned commits in
// internal/app/deploy/gen/main.go, and read the diff: the scripts beside this
// file are what runs as root on a hypervisor (ADR 0012).

package deploy

// The upstream commits this catalogue and the vendored scripts came from. The
// engine is resolved by the scripts while they run, so it is pinned here even
// though none of it is vendored — an unpinned engine would mean a deploy ran
// whatever was pushed that morning.
const (
	ScriptsRepo = %q
	ScriptsRef  = %q
	EngineRepo  = %q
	EngineRef   = %q
)

var catalogue = []App{
`, scriptsRepo, scriptsRef, engineRepo, engineRef)

	for _, a := range apps {
		fmt.Fprintf(&b, "\t{ID: %q, Name: %q", a.ID, a.Name)
		if len(a.Tags) > 0 {
			fmt.Fprintf(&b, ", Tags: []string{")
			for i, t := range a.Tags {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "%q", t)
			}
			b.WriteString("}")
		}
		if a.Cores > 0 {
			fmt.Fprintf(&b, ", Cores: %d", a.Cores)
		}
		if a.MemoryMB > 0 {
			fmt.Fprintf(&b, ", MemoryMB: %d", a.MemoryMB)
		}
		if a.DiskGB > 0 {
			fmt.Fprintf(&b, ", DiskGB: %d", a.DiskGB)
		}
		if a.OS != "" {
			fmt.Fprintf(&b, ", OS: %q", a.OS)
		}
		if a.Version != "" {
			fmt.Fprintf(&b, ", Version: %q", a.Version)
		}
		if !a.Unprivileged {
			b.WriteString(", Privileged: true")
		}
		if a.Source != "" {
			fmt.Fprintf(&b, ", Source: %q", a.Source)
		}
		b.WriteString("},\n")
	}
	b.WriteString("}\n")
	return os.WriteFile(outFile, []byte(b.String()), 0o644)
}
