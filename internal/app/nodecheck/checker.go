// Package nodecheck asks a platform's nodes what they have, and installs what
// they are missing (ADR 0011).
//
// Three of the portal's features run on a node rather than against the API,
// because Proxmox has no API for what they do: reading a temperature, putting a
// guest agent into a template's disk, and — underneath both — being let in at
// all. None of them is a hard requirement, so a node missing one does not fail;
// it just quietly does less, and somebody finds out weeks later from a chart
// with no line on it.
//
// This is the loop that says so out loud, and the button that fixes the two
// halves the portal can reach. What it may install is a constant in the
// binary: a request names a prerequisite, the connector maps it to a command,
// and an identifier nothing recognises is refused. Nothing a caller sends is
// ever interpolated into what runs on a hypervisor.
package nodecheck

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/shell"
)

const sshPort = 22

// checkTimeout bounds one node's probe: a handshake and a single command.
const checkTimeout = 25 * time.Second

// installTimeout bounds one installation. `apt-get install libguestfs-tools`
// fetches a hundred packages, which is a minute or two on a warm mirror and
// longer on a cold one; a node that has stopped answering must not hold the
// slot forever.
const installTimeout = 10 * time.Minute

// maxParallel caps how many nodes are probed at once, as the sensor collector
// does: a handshake is cheap but not free.
const maxParallel = 4

// Failures a caller has to tell apart, because each is a different answer to
// give an administrator.
var (
	ErrUnknownPrerequisite = errors.New("nodecheck: no such prerequisite")
	ErrNotInstallable      = errors.New("nodecheck: the portal cannot install this")
	ErrUnknownNode         = errors.New("nodecheck: no such node on this platform")
	ErrNotPinned           = errors.New("nodecheck: the node has no pinned host key yet")
	ErrAlreadyRunning      = errors.New("nodecheck: an installation is already running on that node")
	ErrNoKey               = errors.New("nodecheck: the portal has no SSH key of its own")
)

// idPattern is what a prerequisite identifier may look like.
//
// The identifiers are constants inside the binary and are already the only
// thing a request can name, so this guards against a future connector rather
// than against a caller. It is cheap, and the string it protects is composed
// into a command that runs as root on a hypervisor.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Actor is who asked, for the audit entry. Held here rather than imported so
// that this package depends on nothing but ports and the connector contract.
type Actor struct {
	UserID    uuid.UUID
	Username  string
	IP        string
	UserAgent string
	RequestID string
}

// Install is the outcome of one installation.
//
// Deliberately in memory and not a table. A provisioning request needs durable
// state because nothing else can tell you afterwards whether the guest got
// made; an installation needs none, because checking again answers the question
// directly — the tool is on the node or it is not. What is durable is the audit
// entry, which records who asked and what happened.
type Install struct {
	Node         string     `json:"node"`
	Prerequisite string     `json:"prerequisite"`
	State        string     `json:"state"` // running | installed | failed
	Error        string     `json:"error,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

// Install states.
const (
	StateRunning   = "running"
	StateInstalled = "installed"
	StateFailed    = "failed"
)

// PrerequisiteState is one requirement as it stands on one node.
type PrerequisiteState struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Needed string `json:"needed"`
	// Present is what the probe said. Only meaningful on a node that answered;
	// a node that could not be reached reports no prerequisites at all rather
	// than a list of absences, because unknown is not the same as missing.
	Present     bool     `json:"present"`
	Installable bool     `json:"installable"`
	Packages    []string `json:"packages,omitempty"`
	// Command is exactly what installing would run. Shown rather than
	// summarised: the portal is asking for permission to run this as root on a
	// hypervisor, and the honest way to ask is to say what it is.
	Command string   `json:"command,omitempty"`
	Install *Install `json:"install,omitempty"`
}

// NodeReport is one node's answer.
type NodeReport struct {
	Node      string `json:"node"`
	Address   string `json:"address"`
	Reachable bool   `json:"reachable"`
	// Problem is the sentence an operator can act on when the node did not
	// answer: install the key, clear the pin, find out why port 22 is shut.
	Problem string `json:"problem,omitempty"`
	// Fingerprint is the host key the portal has pinned, so it can be compared
	// by hand against ssh-keyscan from somewhere else.
	Fingerprint   string              `json:"fingerprint,omitempty"`
	Prerequisites []PrerequisiteState `json:"prerequisites"`
}

// Report is a platform's nodes, as they stand.
type Report struct {
	// PortalKey is whether the portal has a key at all. Without one there is
	// nothing to authenticate with, and every node is unreachable for the same
	// single reason — which is worth saying once rather than per node.
	PortalKey bool         `json:"portal_key"`
	Nodes     []NodeReport `json:"nodes"`
}

// Checker probes and fixes the nodes of one platform at a time.
type Checker struct {
	Hosts  ports.SensorHostLister
	SSH    ports.NodeSSHStore
	Key    ports.PortalKeyReader
	Runner ports.NodeCommandRunner
	Audit  ports.AuditWriter
	Clock  func() time.Time
	Log    zerolog.Logger
	// User is the account to connect as, root by default — the same account
	// the sensor collector uses, and the only one on every node.
	User string

	mu       sync.Mutex
	installs map[string]*Install
}

func (c *Checker) now() time.Time {
	if c.Clock != nil {
		return c.Clock()
	}
	return time.Now().UTC()
}

func (c *Checker) user() string {
	if c.User != "" {
		return c.User
	}
	return "root"
}

// Check reports what every node of a platform has and is missing.
//
// A platform whose connector cannot name its prerequisites or its nodes'
// addresses reports nothing. That is the correct answer for a connector that
// has no node to reach, and better than inventing a Debian one.
func (c *Checker) Check(ctx context.Context, platformID uuid.UUID, conn connector.Connector) (Report, error) {
	var report Report

	lister, ok := conn.(connector.NodePrerequisiteLister)
	if !ok {
		return report, nil
	}
	addresser, ok := conn.(connector.NodeAddresser)
	if !ok {
		return report, nil
	}
	prereqs, err := validPrerequisites(lister)
	if err != nil {
		return report, err
	}
	if len(prereqs) == 0 {
		// Nothing to ask about, so nothing to dial. Without this the probe
		// would be an empty command run against every node for no answer.
		return report, nil
	}

	hosts, err := c.Hosts.OnlineHosts(ctx, platformID)
	if err != nil {
		return report, fmt.Errorf("nodecheck: list nodes: %w", err)
	}
	addresses, err := addresser.NodeAddresses(ctx)
	if err != nil {
		return report, fmt.Errorf("nodecheck: node addresses: %w", err)
	}

	key, err := c.Key.PrivateKey(ctx)
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		return report, err
	}
	report.PortalKey = key != ""

	results := make([]NodeReport, len(hosts))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup

	for i, host := range hosts {
		address := addresses[host.ExternalID]
		node := NodeReport{Node: host.Name, Address: address, Prerequisites: []PrerequisiteState{}}

		switch {
		case address == "":
			node.Problem = "the platform did not say where this node is"
			results[i] = node
			continue
		case !report.PortalKey:
			node.Problem = "the portal has no SSH key of its own; generate one in Settings → SSH key"
			results[i] = node
			continue
		}

		wg.Add(1)
		go func(i int, host ports.SensorHost, node NodeReport) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = c.probe(ctx, host, node, key, prereqs)
		}(i, host, node)
	}
	wg.Wait()

	sort.Slice(results, func(a, b int) bool { return results[a].Node < results[b].Node })
	report.Nodes = results
	return report, nil
}

// probe asks one node about every prerequisite in a single connection.
//
// One command rather than one per prerequisite: the handshake is the expensive
// part, and asking twice would double it for an answer that changes about once
// a year.
func (c *Checker) probe(ctx context.Context, host ports.SensorHost, node NodeReport,
	key string, prereqs []connector.NodePrerequisite) NodeReport {

	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	known, err := c.SSH.Get(ctx, host.ID)
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		node.Problem = err.Error()
		return node
	}
	node.Fingerprint = known.Fingerprint

	// Trust on first use, as the sensor collector does, and for a reason the
	// collector cannot serve: it pins only after a node has answered `sensors
	// -j`, so a node without lm-sensors is never pinned at all — and installing
	// lm-sensors is what this exists to do. Refusing to meet a node here would
	// make the one case that matters, a node the portal has never successfully
	// read, permanently unfixable. Checking only reads, an administrator is
	// present, and the fingerprint it pins is reported so it can be compared by
	// hand afterwards. Installing, which writes, requires the pin to exist
	// already.
	policy := &pinPolicy{known: known}

	out, err := c.Runner.RunCommand(ctx,
		ports.SSHTarget{Host: node.Address, Port: sshPort},
		ports.SSHCredential{Username: c.user(), PrivateKey: key},
		policy, probeCommand(prereqs))
	if err != nil {
		node.Problem = describe(err)
		return node
	}

	if !policy.hadKnown && policy.seen.Fingerprint != "" {
		if err := c.SSH.Pin(ctx, ports.NodeSSH{
			HostID: host.ID, Address: node.Address, SSHUser: c.user(),
			Algorithm: policy.seen.Algorithm, Fingerprint: policy.seen.Fingerprint,
			PublicKey: policy.seen.PublicKey, FirstSeenAt: c.now(),
		}); err != nil {
			c.Log.Error().Err(err).Str("node", host.Name).Msg("could not pin the node host key")
		}
		node.Fingerprint = policy.seen.Fingerprint
	}

	node.Reachable = true
	node.Prerequisites = c.states(prereqs, present(out), host.Name)
	return node
}

func (c *Checker) states(prereqs []connector.NodePrerequisite, found map[string]bool, node string) []PrerequisiteState {
	out := make([]PrerequisiteState, 0, len(prereqs))
	for _, p := range prereqs {
		out = append(out, PrerequisiteState{
			ID: p.ID, Name: p.Name, Needed: p.Needed,
			Present: found[p.ID], Installable: p.Installable(),
			Packages: p.Packages, Command: p.Install,
			Install: c.lastInstall(node, p.ID),
		})
	}
	return out
}

// Install puts one prerequisite on one node.
//
// It returns as soon as the work has started, because `apt-get` takes minutes
// and the API's request deadline is thirty seconds. There is nothing to resume
// and nothing to lose if the portal restarts halfway: checking again asks the
// node directly, which is the only authority on whether the tool is there.
func (c *Checker) Install(ctx context.Context, platformID uuid.UUID, conn connector.Connector,
	node, prerequisiteID string, actor Actor) (Install, error) {

	lister, ok := conn.(connector.NodePrerequisiteLister)
	if !ok {
		return Install{}, ErrUnknownPrerequisite
	}
	prereqs, err := validPrerequisites(lister)
	if err != nil {
		return Install{}, err
	}
	prereq, ok := find(prereqs, prerequisiteID)
	if !ok {
		// The whole control, in one line: a caller names an identifier, and an
		// identifier the binary does not know goes no further. Nothing the
		// caller sent has been near a command.
		return Install{}, ErrUnknownPrerequisite
	}
	if !prereq.Installable() {
		return Install{}, ErrNotInstallable
	}

	addresser, ok := conn.(connector.NodeAddresser)
	if !ok {
		return Install{}, ErrUnknownNode
	}
	host, err := c.host(ctx, platformID, node)
	if err != nil {
		return Install{}, err
	}
	addresses, err := addresser.NodeAddresses(ctx)
	if err != nil {
		return Install{}, fmt.Errorf("nodecheck: node addresses: %w", err)
	}
	address := addresses[host.ExternalID]
	if address == "" {
		return Install{}, ErrUnknownNode
	}

	key, err := c.Key.PrivateKey(ctx)
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		return Install{}, err
	}
	if key == "" {
		return Install{}, ErrNoKey
	}

	// The pin must already exist. Checking may meet a node for the first time
	// because it only reads; installing changes a hypervisor, and doing that to
	// a machine whose identity the portal is learning in the same breath is the
	// trust-on-first-use case at its weakest (ADR 0007, ADR 0011).
	known, err := c.SSH.Get(ctx, host.ID)
	if errors.Is(err, ports.ErrNotFound) || (err == nil && known.Fingerprint == "") {
		return Install{}, ErrNotPinned
	}
	if err != nil {
		return Install{}, err
	}

	rec, err := c.begin(host.Name, prereq.ID)
	if err != nil {
		return Install{}, err
	}

	c.Log.Info().Str("node", host.Name).Str("prerequisite", prereq.ID).
		Strs("packages", prereq.Packages).Str("actor", actor.Username).
		Msg("installing a prerequisite on a node")

	// Detached from the request: the caller has already been answered, and
	// cancelling a half-finished apt-get would leave dpkg needing a hand.
	go c.run(context.WithoutCancel(ctx), host.Name, address, key, known, prereq, actor)

	return *rec, nil
}

func (c *Checker) run(ctx context.Context, node, address, key string, known ports.NodeSSH,
	prereq connector.NodePrerequisite, actor Actor) {

	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()

	started := c.now()
	_, err := c.Runner.RunCommand(ctx,
		ports.SSHTarget{Host: address, Port: sshPort},
		ports.SSHCredential{Username: c.user(), PrivateKey: key},
		pinnedOnly{known: known}, prereq.Install)

	failure := ""
	if err != nil {
		failure = describe(err)
		c.Log.Warn().Err(err).Str("node", node).Str("prerequisite", prereq.ID).
			Msg("installing a prerequisite failed")
	} else {
		c.Log.Info().Str("node", node).Str("prerequisite", prereq.ID).
			Msg("installed a prerequisite on a node")
	}
	c.finish(node, prereq.ID, failure)
	c.audit(ctx, node, prereq, actor, started, failure)
}

// audit records what was put on a hypervisor and who asked for it.
//
// Written when the work finishes rather than when it starts, so the outcome is
// the real one. One entry, not two: the interesting question afterwards is what
// happened to the node, and an attempt that never reached the node changes
// nothing about it.
func (c *Checker) audit(ctx context.Context, node string, prereq connector.NodePrerequisite,
	actor Actor, started time.Time, failure string) {

	if c.Audit == nil {
		return
	}
	outcome := ports.OutcomeSuccess
	details := map[string]any{
		"prerequisite": prereq.ID,
		"packages":     prereq.Packages,
		"command":      prereq.Install,
		"seconds":      int(c.now().Sub(started).Seconds()),
	}
	if failure != "" {
		outcome = ports.OutcomeFailure
		details["error"] = failure
	}
	id := actor.UserID
	_ = c.Audit.Write(ctx, ports.AuditEntry{
		Time: c.now(), ActorUserID: &id, ActorName: actor.Username,
		Category: ports.AuditCategorySecurity, Action: "node.install",
		TargetType: "node", TargetID: node, TargetName: node,
		SourceIP: actor.IP, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
		Outcome: outcome, Details: details,
	})
}

func (c *Checker) host(ctx context.Context, platformID uuid.UUID, name string) (ports.SensorHost, error) {
	hosts, err := c.Hosts.OnlineHosts(ctx, platformID)
	if err != nil {
		return ports.SensorHost{}, err
	}
	for _, h := range hosts {
		if h.Name == name || h.ExternalID == name {
			return h, nil
		}
	}
	return ports.SensorHost{}, ErrUnknownNode
}

// begin claims the slot for one node and prerequisite, so two administrators
// pressing the same button do not run two apt-gets against one dpkg lock.
func (c *Checker) begin(node, id string) (*Install, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.installs == nil {
		c.installs = map[string]*Install{}
	}
	key := node + "\x00" + id
	if cur, ok := c.installs[key]; ok && cur.State == StateRunning {
		return nil, ErrAlreadyRunning
	}
	rec := &Install{Node: node, Prerequisite: id, State: StateRunning, StartedAt: c.now()}
	c.installs[key] = rec
	return rec, nil
}

func (c *Checker) finish(node, id, failure string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.installs[node+"\x00"+id]
	if !ok {
		return
	}
	at := c.now()
	rec.FinishedAt = &at
	rec.State = StateInstalled
	if failure != "" {
		rec.State = StateFailed
		rec.Error = failure
	}
}

func (c *Checker) lastInstall(node, id string) *Install {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.installs[node+"\x00"+id]
	if !ok {
		return nil
	}
	// Copied out: the caller marshals it while the goroutine may still be
	// writing to the original.
	out := *rec
	return &out
}

// probeCommand asks about every prerequisite at once and answers one line each.
//
// Built from the connector's own constants — identifiers checked against
// idPattern and probes written in this repository. Nothing a request carried is
// anywhere near it.
func probeCommand(prereqs []connector.NodePrerequisite) string {
	var b strings.Builder
	for _, p := range prereqs {
		b.WriteString("if " + p.Probe + " >/dev/null 2>&1; then echo '" + p.ID +
			" yes'; else echo '" + p.ID + " no'; fi; ")
	}
	return strings.TrimSpace(b.String())
}

// present reads what the probe said.
func present(out []byte) map[string]bool {
	found := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		id, answer, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		found[id] = answer == "yes"
	}
	return found
}

func find(prereqs []connector.NodePrerequisite, id string) (connector.NodePrerequisite, bool) {
	for _, p := range prereqs {
		if p.ID == id {
			return p, true
		}
	}
	return connector.NodePrerequisite{}, false
}

// validPrerequisites refuses a list this package would not be willing to put on
// a command line. A connector in this repository cannot fail it; one added
// later that would is stopped here rather than on a hypervisor.
func validPrerequisites(l connector.NodePrerequisiteLister) ([]connector.NodePrerequisite, error) {
	prereqs := l.NodePrerequisites()
	for _, p := range prereqs {
		if !idPattern.MatchString(p.ID) {
			return nil, fmt.Errorf("nodecheck: %q is not a prerequisite identifier", p.ID)
		}
		if strings.TrimSpace(p.Probe) == "" {
			return nil, fmt.Errorf("nodecheck: prerequisite %q has no probe", p.ID)
		}
	}
	return prereqs, nil
}

// describe turns a failure into the sentence an operator can act on. The three
// that matter are indistinguishable at the protocol level and have entirely
// different fixes.
func describe(err error) string {
	switch {
	case errors.Is(err, shell.ErrHostKeyMismatch):
		return "the node presented a different host key than the one pinned; " +
			"clear the pin if the node was rebuilt"
	case errors.Is(err, shell.ErrAuthFailed):
		return "the node refused the portal's key; install the portal's public key " +
			"in its authorized_keys"
	case errors.Is(err, shell.ErrUnreachable):
		return "the node could not be reached on port 22"
	case errors.Is(err, context.DeadlineExceeded):
		return "the node stopped answering before the command finished"
	}
	msg := strings.TrimSpace(err.Error())
	if len(msg) > 300 {
		return msg[:300]
	}
	return msg
}

// pinPolicy trusts a node's key on first use and refuses a change after that —
// the same policy the sensor collector uses, and the same weakness: nobody
// compares a fingerprint at the first connection. What it buys is the part that
// matters afterwards, that a node cannot be swapped underneath a portal that
// has already met it.
type pinPolicy struct {
	known    ports.NodeSSH
	hadKnown bool
	seen     shell.HostKey
}

func (p *pinPolicy) Check(address, algorithm, fingerprint string, publicKey []byte) error {
	p.seen = shell.HostKey{
		Address: address, Algorithm: algorithm,
		Fingerprint: fingerprint, PublicKey: publicKey,
	}
	if len(p.known.PublicKey) == 0 {
		return nil
	}
	p.hadKnown = true
	if string(p.known.PublicKey) == string(publicKey) {
		return nil
	}
	return shell.ErrHostKeyMismatch
}

// pinnedOnly accepts exactly the key already recorded and never learns one.
type pinnedOnly struct{ known ports.NodeSSH }

func (p pinnedOnly) Check(address, algorithm, fingerprint string, publicKey []byte) error {
	if p.known.Fingerprint == "" {
		return ErrNotPinned
	}
	if p.known.Algorithm != algorithm || p.known.Fingerprint != fingerprint {
		return fmt.Errorf("nodecheck: the node presented %s %s, not the pinned key",
			algorithm, fingerprint)
	}
	return nil
}
