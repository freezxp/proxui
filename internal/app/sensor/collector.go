// Package sensor collects hardware readings from the nodes themselves.
//
// Proxmox publishes no temperature anywhere in its API, so the number has to
// come from the node: one SSH connection, one fixed command, the portal's own
// key. ADR 0007 is the whole argument; this is the loop.
package sensor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/shell"
	"github.com/freezxp/proxui/internal/domain/telemetry"
)

// sshPort is where a node's sshd lives. Not configurable: the address comes
// from the platform's own cluster membership, and a node running sshd
// somewhere else is not something the portal can discover.
const sshPort = 22

// pollTimeout bounds one node. A handshake plus one command is a second on a
// healthy node; a node that has gone away must not hold the cycle open.
const pollTimeout = 25 * time.Second

// maxParallel caps how many nodes are polled at once. Estates here are tens of
// nodes, and an SSH handshake is cheap but not free.
const maxParallel = 4

// Collector polls every node of a platform for its sensors.
type Collector struct {
	Hosts  ports.SensorHostLister
	Store  ports.HostSensorStore
	SSH    ports.NodeSSHStore
	Source ports.NodeSensorReader
	Key    ports.PortalKeyReader
	Log    zerolog.Logger
	Clock  func() time.Time
	// User is the account to connect as. Root, because reading hwmon on a
	// Proxmox node needs no privilege but the account that exists on every one
	// of them does.
	User string
}

// Stats reports one collection pass.
type Stats struct {
	Nodes    int `json:"nodes"`
	Answered int `json:"answered"`
	Readings int `json:"readings"`
	// Silent counts nodes that could not be read. It is not an error count:
	// a node with no key installed is the normal starting state.
	Silent int `json:"silent"`
}

func (c *Collector) now() time.Time {
	if c.Clock != nil {
		return c.Clock()
	}
	return time.Now().UTC()
}

func (c *Collector) user() string {
	if c.User != "" {
		return c.User
	}
	return "root"
}

// Collect polls every online node of one platform.
//
// A platform whose connector cannot name its nodes' addresses is skipped
// silently: there is nothing to reach, and nothing has gone wrong.
func (c *Collector) Collect(ctx context.Context, platformID uuid.UUID, conn connector.Connector) (Stats, error) {
	var stats Stats

	addresser, ok := conn.(connector.NodeAddresser)
	if !ok {
		return stats, nil
	}
	hosts, err := c.Hosts.OnlineHosts(ctx, platformID)
	if err != nil {
		return stats, fmt.Errorf("sensor: list nodes: %w", err)
	}
	if len(hosts) == 0 {
		return stats, nil
	}

	addresses, err := addresser.NodeAddresses(ctx)
	if err != nil {
		return stats, fmt.Errorf("sensor: node addresses: %w", err)
	}

	// Read once for the whole pass rather than per node: it is one decryption
	// against the vault, and every node is offered the same key.
	privateKey, err := c.Key.PrivateKey(ctx)
	if err != nil || privateKey == "" {
		// No portal key means nothing to authenticate with. That is a portal
		// that has not been set up for this yet, not a failure of collection.
		c.Log.Debug().Msg("no portal SSH key; skipping sensor collection")
		return stats, nil
	}

	var mu sync.Mutex
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup

	for _, host := range hosts {
		address := addresses[host.ExternalID]
		if address == "" {
			continue
		}
		stats.Nodes++

		wg.Add(1)
		go func(host ports.SensorHost, address string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			readings, err := c.poll(ctx, host, address, privateKey)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				stats.Silent++
				return
			}
			stats.Answered++
			stats.Readings += len(readings)
		}(host, address)
	}
	wg.Wait()
	return stats, nil
}

// poll reads one node and stores what it said, recording either way.
func (c *Collector) poll(ctx context.Context, host ports.SensorHost, address, privateKey string) ([]telemetry.Reading, error) {
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()

	at := c.now()
	known, err := c.SSH.Get(ctx, host.ID)
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		return nil, err
	}
	policy := &pinPolicy{known: known}

	readings, err := c.Source.Read(ctx,
		ports.SSHTarget{Host: address, Port: sshPort},
		ports.SSHCredential{Username: c.user(), PrivateKey: privateKey},
		policy)
	if err != nil {
		c.record(ctx, host, at, describe(err))
		c.Log.Debug().Err(err).Str("node", host.Name).Msg("node sensors unavailable")
		return nil, err
	}

	// The key is pinned only once the node has also authenticated and
	// answered. Pinning a key that never got as far as proving it belongs to
	// something useful would record a stranger's key as this node's.
	if !policy.hadKnown && policy.seen.Fingerprint != "" {
		if err := c.SSH.Pin(ctx, ports.NodeSSH{
			HostID: host.ID, Address: address, SSHUser: c.user(),
			Algorithm: policy.seen.Algorithm, Fingerprint: policy.seen.Fingerprint,
			PublicKey: policy.seen.PublicKey, FirstSeenAt: at,
		}); err != nil {
			c.Log.Error().Err(err).Str("node", host.Name).Msg("could not pin the node host key")
		}
	}

	if _, err := c.Store.Write(ctx, ports.SensorReadings{
		HostID: host.ID, At: at, Readings: readings,
	}); err != nil {
		return nil, fmt.Errorf("sensor: store readings: %w", err)
	}
	c.record(ctx, host, at, "")
	return readings, nil
}

func (c *Collector) record(ctx context.Context, host ports.SensorHost, at time.Time, failure string) {
	// Deliberately not the poll's context: it may already be cancelled, and
	// the record of why is the one thing worth keeping from a failed poll.
	if err := c.SSH.RecordAttempt(context.WithoutCancel(ctx), host.ID, at, failure); err != nil {
		c.Log.Error().Err(err).Str("node", host.Name).Msg("could not record the sensor attempt")
	}
}

// describe turns a failure into the sentence an operator can act on.
//
// The three that matter are all indistinguishable at the protocol level from
// "something went wrong", and each has a different fix: install the key,
// install lm-sensors, or find out why the node's identity changed.
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
	case strings.Contains(err.Error(), "lm-sensors"):
		return "lm-sensors is not installed on the node"
	case strings.Contains(err.Error(), "command not found"):
		return "the node has no `sensors` command; install lm-sensors"
	}
	return firstSentence(err.Error())
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// pinPolicy trusts a node's key on first use and refuses a change after that.
//
// Nobody is present at the first connection to confirm a fingerprint — this
// runs on the scheduler — so this is trust-on-first-use, weaker than the
// operator-confirmed pinning a guest gets under SSH-04. What it still buys is
// the part that matters afterwards: a node cannot be swapped underneath a
// portal that has already met it. The fingerprint is shown on the host page so
// the comparison can be made by hand, late.
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
