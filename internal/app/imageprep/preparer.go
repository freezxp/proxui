// Package imageprep prepares a freshly imported disk before it becomes a
// template (PROV-14).
//
// A published cloud image is not ready to be cloned as it stands. It carries no
// qemu-guest-agent, so every guest made from it reports no IP address — which
// is what leaves the portal unable to offer an SSH session, since it has
// nowhere to connect. And it carries a machine-id and host keys that every
// clone would then share.
//
// Proxmox has no API for changing the contents of a disk, so this is done on
// the node with virt-customize, over the SSH channel the portal already opens
// for sensors: one fixed command, no shell, the portal's own key (ADR 0007).
// A node without the tool is not a failure — the template is built without
// preparation and says so.
package imageprep

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
)

// prepareTimeout bounds one preparation. Installing a package into an image
// takes seconds on a warm node; a node that has gone away must not hold a
// provisioning request open.
const prepareTimeout = 4 * time.Minute

const sshPort = 22

// ErrToolMissing reports that the node has no virt-customize. It is not a
// failure of the build: the template is usable, its guests simply have to have
// the agent installed some other way.
var ErrToolMissing = errors.New("imageprep: the node has no virt-customize; install libguestfs-tools")

// volumeIDPattern is what a Proxmox volume identifier looks like. Nothing that
// reaches the node is taken on trust: the identifier is built by the portal
// from a VMID it allocated, but it travels through a database row, and a value
// that ended up carrying shell metacharacters would be running them as root on
// a hypervisor.
var volumeIDPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+:[a-zA-Z0-9._/-]+$`)

// Preparer runs the preparation on a node.
type Preparer struct {
	Hosts  ports.SensorHostLister
	SSH    ports.NodeSSHStore
	Key    ports.PortalKeyReader
	Runner ports.NodeCommandRunner
	Log    zerolog.Logger
	// User is the account to connect as, root by default — the same account
	// the sensor collector uses, and the only one present on every node.
	User string
}

func (p *Preparer) user() string {
	if p.User != "" {
		return p.User
	}
	return "root"
}

// Prepare installs the guest agent into a guest's disk and clears the identity
// a clone must not inherit.
//
// It is given the node's address rather than looking it up, because the caller
// already has the connector that knows it and a second discovery call would be
// asking the platform a question it has just been asked.
func (p *Preparer) Prepare(ctx context.Context, platformID uuid.UUID, nodeName, address, volumeID string) error {
	if !volumeIDPattern.MatchString(volumeID) {
		return fmt.Errorf("imageprep: %q is not a volume identifier", volumeID)
	}

	ctx, cancel := context.WithTimeout(ctx, prepareTimeout)
	defer cancel()

	host, err := p.findHost(ctx, platformID, nodeName)
	if err != nil {
		return err
	}
	key, err := p.Key.PrivateKey(ctx)
	if err != nil {
		return fmt.Errorf("imageprep: no portal key: %w", err)
	}

	known, err := p.SSH.Get(ctx, host.ID)
	if errors.Is(err, ports.ErrNotFound) || (err == nil && known.Fingerprint == "") {
		// The key must already be pinned. Preparation is not the moment to meet
		// a node for the first time: the sensor collector pins on a cadence
		// with nobody waiting, and accepting a new key here would mean trusting
		// one while a request is held open (ADR 0007).
		return fmt.Errorf("imageprep: node %s has no pinned host key yet", nodeName)
	}
	if err != nil {
		return err
	}

	target := ports.SSHTarget{Host: address, Port: sshPort}
	cred := ports.SSHCredential{Username: p.user(), PrivateKey: key}
	policy := pinnedOnly{known: known}

	path, err := p.diskPath(ctx, target, cred, policy, volumeID)
	if err != nil {
		return err
	}
	out, err := p.Runner.RunCommand(ctx, target, cred, policy, customizeCommand(path))
	if err != nil {
		if isMissingTool(err, out) {
			return ErrToolMissing
		}
		return fmt.Errorf("imageprep: %w", err)
	}
	p.Log.Info().Str("node", nodeName).Str("volume", volumeID).
		Msg("prepared a template disk: guest agent installed, identity cleared")
	return nil
}

// diskPath asks the node where a volume actually lives. A volume identifier is
// storage-relative; virt-customize needs a path, and only the node can map one
// to the other.
func (p *Preparer) diskPath(ctx context.Context, target ports.SSHTarget, cred ports.SSHCredential,
	policy ports.HostKeyPolicy, volumeID string) (string, error) {

	out, err := p.Runner.RunCommand(ctx, target, cred, policy, "pvesm path "+volumeID)
	if err != nil {
		if isMissingTool(err, out) {
			return "", ErrToolMissing
		}
		return "", fmt.Errorf("imageprep: resolve %s: %w", volumeID, err)
	}
	path := strings.TrimSpace(string(out))
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("imageprep: %q is not a path to %s", path, volumeID)
	}
	return path, nil
}

// customizeCommand is the one command that changes the image.
//
// What it does and why:
//   - installs qemu-guest-agent, without which a guest reports no address and
//     the portal cannot offer it an SSH session;
//   - truncates /etc/machine-id, which every clone would otherwise share — DHCP
//     servers that key on it hand the same lease to all of them;
//   - deletes the SSH host keys, so each clone generates its own rather than
//     presenting an identity its siblings also have;
//   - clears cloud-init's record of having run, so the settings the portal
//     writes are applied on the clone's first boot rather than skipped.
//
// It deliberately does not enable the service. On Debian and its derivatives,
// and on the RHEL family too, the package ships a udev rule that starts the
// agent when the virtio port appears, which the template has because
// provisioning sets agent=1; a `systemctl enable` reports success, creates no
// symlink, and would only mislead whoever reads this next.
//
// Nothing here relabels for SELinux, and it deliberately does not try. A file
// written into a RHEL-family image from outside arrives unlabelled, and
// libguestfs handles that by touching /.autorelabel so the guest relabels
// itself on its first boot — which it does, and the agent comes up. Passing
// --selinux-relabel was tried against a live AlmaLinux 10 template and made no
// difference: relabelling needs the guest's policy loaded, which the appliance
// on a Debian host cannot do, so it falls back to the same /.autorelabel.
func customizeCommand(path string) string {
	return "virt-customize -a " + path +
		" --install qemu-guest-agent" +
		" --truncate /etc/machine-id" +
		" --delete '/etc/ssh/ssh_host_*'" +
		" --delete /var/lib/cloud/instances" +
		" --delete /var/lib/cloud/instance"
}

func (p *Preparer) findHost(ctx context.Context, platformID uuid.UUID, name string) (ports.SensorHost, error) {
	hosts, err := p.Hosts.OnlineHosts(ctx, platformID)
	if err != nil {
		return ports.SensorHost{}, err
	}
	for _, h := range hosts {
		if h.Name == name || h.ExternalID == name {
			return h, nil
		}
	}
	return ports.SensorHost{}, fmt.Errorf("imageprep: node %q is not a known host of this platform", name)
}

// isMissingTool recognises a node that simply does not have the tooling, which
// is a configuration to report rather than a fault to retry.
func isMissingTool(err error, out []byte) bool {
	text := strings.ToLower(err.Error() + " " + string(out))
	return strings.Contains(text, "command not found") ||
		strings.Contains(text, "not found") && strings.Contains(text, "virt-customize")
}

// pinnedOnly accepts exactly the key already recorded for a node and nothing
// else. Unlike the sensor collector's policy it never learns one: preparation
// runs with a request waiting on it, which is the wrong moment to decide a
// stranger is trustworthy.
type pinnedOnly struct{ known ports.NodeSSH }

func (p pinnedOnly) Check(address, algorithm, fingerprint string, publicKey []byte) error {
	if p.known.Fingerprint == "" {
		return errors.New("imageprep: no pinned key for this node")
	}
	if p.known.Algorithm != algorithm || p.known.Fingerprint != fingerprint {
		return fmt.Errorf("imageprep: the node presented %s %s, not the pinned key",
			algorithm, fingerprint)
	}
	return nil
}
