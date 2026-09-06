package deploy

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/deployment"
	"github.com/freezxp/proxui/internal/domain/inventory"
)

const sshPort = 22

// commandTimeout bounds one exchange with the node: a handshake and either the
// launch or a poll. The install itself is not bounded by this — it is detached
// and outlives the connection, which is the whole point.
const commandTimeout = 45 * time.Second

// deployWindow is how long a script is given before the portal stops waiting.
// A container build that fetches an application and its dependencies is minutes;
// the slowest of these are twenty. Past that the log is kept and the deployment
// is recorded as unfinished rather than waited on forever.
const deployWindow = 40 * time.Minute

// workRoot is where the script, its log and its exit status live on the node.
// Under /var/tmp rather than /tmp so a long install is not swept away underneath
// itself, and per-deployment so two of them cannot read each other's log.
const workRoot = "/var/tmp/proxui-deploy"

// ErrStillRunning reports that the script has not finished. The job layer turns
// it into another turn later.
var ErrStillRunning = errors.New("deploy: still running")

// Failures a caller has to tell apart.
var (
	ErrUnknownApp  = errors.New("deploy: no such application")
	ErrUnknownNode = errors.New("deploy: no such node on this platform")
	ErrNotPinned   = errors.New("deploy: the node has no pinned host key yet")
	ErrNoKey       = errors.New("deploy: the portal has no SSH key of its own")
)

// PlatformConnector opens a connector for a saved platform. An interface for
// the same reason the provisioning driver has one: a test needs a platform
// whose state survives between turns.
type PlatformConnector interface {
	Connect(ctx context.Context, p *inventory.Platform) (connector.Connector, error)
}

// PlatformLookup finds a saved platform. One method rather than the whole
// repository: the deployer reads a platform and never writes one, and a seam
// that admits more than it needs makes a test carry methods nothing calls.
type PlatformLookup interface {
	Get(ctx context.Context, id uuid.UUID) (*inventory.Platform, error)
}

// SyncEnqueuer asks the inventory to look again, so a container that now exists
// appears without waiting for the next scheduled pass.
type SyncEnqueuer interface {
	EnqueueInventorySync(ctx context.Context, platformID uuid.UUID, trigger string) error
}

// Deployer installs one catalogue application into a container on a node.
type Deployer struct {
	Deployments ports.DeploymentRepository
	Platforms   PlatformLookup
	Platform    PlatformConnector
	Hosts       ports.SensorHostLister
	SSH         ports.NodeSSHStore
	Key         ports.PortalKeyReader
	Runner      ports.NodeCommandRunner
	Queue       ports.DeployEnqueuer
	Sync        SyncEnqueuer
	Audit       ports.AuditWriter
	Clock       ports.Clock
	Log         zerolog.Logger
	// User is the account to connect as, root by default — the same account the
	// sensor collector uses, and the only one on every node.
	User string
}

func (d *Deployer) user() string {
	if d.User != "" {
		return d.User
	}
	return "root"
}

// Step advances one deployment as far as it can without waiting.
func (d *Deployer) Step(ctx context.Context, id uuid.UUID) error {
	rec, err := d.Deployments.Get(ctx, id)
	if err != nil {
		return err
	}
	if rec.State.Terminal() {
		return nil
	}

	target, cred, policy, err := d.reach(ctx, rec)
	if err != nil {
		return d.fail(ctx, rec, err)
	}

	switch rec.State {
	case deployment.StatePending:
		return d.launch(ctx, rec, target, cred, policy)
	case deployment.StateDeploying:
		return d.collect(ctx, rec, target, cred, policy)
	}
	return nil
}

// reach resolves where the node is and what to authenticate with.
//
// The host key must already be pinned. This writes to a machine and runs a
// program on it; a machine whose identity the portal is learning in the same
// breath is the wrong one to do that to (ADR 0007, ADR 0011, ADR 0012).
func (d *Deployer) reach(ctx context.Context, rec *deployment.Deployment) (
	ports.SSHTarget, ports.SSHCredential, ports.HostKeyPolicy, error) {

	var zeroT ports.SSHTarget
	var zeroC ports.SSHCredential

	platform, err := d.Platforms.Get(ctx, rec.PlatformID)
	if err != nil {
		return zeroT, zeroC, nil, err
	}
	conn, err := d.Platform.Connect(ctx, platform)
	if err != nil {
		return zeroT, zeroC, nil, err
	}
	defer conn.Close()

	addresser, ok := conn.(connector.NodeAddresser)
	if !ok {
		return zeroT, zeroC, nil, ErrUnknownNode
	}
	host, err := d.host(ctx, rec.PlatformID, rec.Node)
	if err != nil {
		return zeroT, zeroC, nil, err
	}
	addresses, err := addresser.NodeAddresses(ctx)
	if err != nil {
		return zeroT, zeroC, nil, err
	}
	address := addresses[host.ExternalID]
	if address == "" {
		return zeroT, zeroC, nil, ErrUnknownNode
	}

	key, err := d.Key.PrivateKey(ctx)
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		return zeroT, zeroC, nil, err
	}
	if key == "" {
		return zeroT, zeroC, nil, ErrNoKey
	}

	known, err := d.SSH.Get(ctx, host.ID)
	if errors.Is(err, ports.ErrNotFound) || (err == nil && known.Fingerprint == "") {
		return zeroT, zeroC, nil, ErrNotPinned
	}
	if err != nil {
		return zeroT, zeroC, nil, err
	}

	return ports.SSHTarget{Host: address, Port: sshPort},
		ports.SSHCredential{Username: d.user(), PrivateKey: key},
		pinnedOnly{known: known}, nil
}

// launch writes the vendored script and its runner onto the node and starts
// them detached.
//
// Detached because a deploy is minutes long and RunCommand buffers its output
// and throws the buffer away when its deadline fires — so a connection held for
// the whole install would lose exactly the transcript that explains a failure.
// Detaching also means the log survives a portal restart, because it was never
// in the portal.
func (d *Deployer) launch(ctx context.Context, rec *deployment.Deployment,
	target ports.SSHTarget, cred ports.SSHCredential, policy ports.HostKeyPolicy) error {

	script, err := Script(rec.AppID)
	if err != nil {
		return d.fail(ctx, rec, ErrUnknownApp)
	}

	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	out, err := d.Runner.RunCommand(ctx, target, cred, policy,
		launchCommand(rec, script))
	if err != nil {
		return d.fail(ctx, rec, fmt.Errorf("the script could not be started on %s: %w", rec.Node, err))
	}
	if !strings.Contains(string(out), launchedMarker) {
		return d.fail(ctx, rec, fmt.Errorf("the script could not be started on %s: %s",
			rec.Node, firstLine(string(out))))
	}

	d.Log.Info().Str("deployment", rec.ID.String()).Str("app", rec.AppID).
		Str("node", rec.Node).Msg("started a container app install")
	if err := rec.Advance(deployment.StateDeploying, d.Clock.Now()); err != nil {
		return err
	}
	if err := d.Deployments.Save(ctx, rec); err != nil {
		return err
	}
	return ErrStillRunning
}

// collect reads how far the script has got.
func (d *Deployer) collect(ctx context.Context, rec *deployment.Deployment,
	target ports.SSHTarget, cred ports.SSHCredential, policy ports.HostKeyPolicy) error {

	runCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	out, err := d.Runner.RunCommand(runCtx, target, cred, policy, pollCommand(rec.ID))
	if err != nil {
		// A node that stopped answering mid-install is worth another turn: the
		// script is still running there, and the log is still on disk.
		if d.Clock.Now().Sub(rec.Created) > deployWindow {
			return d.fail(ctx, rec, fmt.Errorf("the node stopped answering while %s was installing", rec.AppID))
		}
		return ErrStillRunning
	}

	status, ctid, log := parsePoll(string(out))
	rec.Log = log
	if ctid != "" {
		rec.CTID = ctid
	}

	if status == nil {
		if d.Clock.Now().Sub(rec.Created) > deployWindow {
			rec.Fail(fmt.Errorf("%s did not finish within %s; the log below is where it got to",
				rec.AppID, deployWindow), d.Clock.Now())
			return d.finish(ctx, rec)
		}
		if err := d.Deployments.Save(ctx, rec); err != nil {
			return err
		}
		return ErrStillRunning
	}

	rec.Finish(*status, d.Clock.Now())
	return d.finish(ctx, rec)
}

// finish records the outcome, tells the inventory to look, and audits.
func (d *Deployer) finish(ctx context.Context, rec *deployment.Deployment) error {
	if err := d.Deployments.Save(ctx, rec); err != nil {
		return err
	}
	outcome := ports.OutcomeSuccess
	if rec.State == deployment.StateFailed {
		outcome = ports.OutcomeFailure
	} else if d.Sync != nil {
		// The container exists now and nothing else will notice until the next
		// scheduled sync, which is up to a minute of an operator wondering
		// where it went.
		if err := d.Sync.EnqueueInventorySync(ctx, rec.PlatformID, "deploy"); err != nil {
			d.Log.Warn().Err(err).Msg("could not request a sync after a deployment")
		}
	}
	d.audit(ctx, rec, outcome)
	d.Log.Info().Str("deployment", rec.ID.String()).Str("app", rec.AppID).
		Str("node", rec.Node).Str("state", string(rec.State)).Str("ctid", rec.CTID).
		Msg("container app install finished")
	return nil
}

func (d *Deployer) fail(ctx context.Context, rec *deployment.Deployment, cause error) error {
	rec.Fail(cause, d.Clock.Now())
	d.Log.Warn().Err(cause).Str("deployment", rec.ID.String()).Msg("container app install failed")
	if err := d.Deployments.Save(ctx, rec); err != nil {
		return err
	}
	d.audit(ctx, rec, ports.OutcomeFailure)
	return nil
}

func (d *Deployer) audit(ctx context.Context, rec *deployment.Deployment, outcome string) {
	if d.Audit == nil {
		return
	}
	details := map[string]any{
		"app": rec.AppID, "node": rec.Node,
		"scripts_ref": ScriptsRef, "engine_ref": EngineRef,
	}
	if rec.CTID != "" {
		details["ctid"] = rec.CTID
	}
	if rec.ExitCode != nil {
		details["exit_code"] = *rec.ExitCode
	}
	if rec.Error != "" {
		details["error"] = rec.Error
	}
	_ = d.Audit.Write(ctx, ports.AuditEntry{
		Time: d.Clock.Now(), ActorUserID: rec.RequestedBy, ActorName: rec.RequestedByName,
		Category: ports.AuditCategorySecurity, Action: "container.deploy",
		TargetType: "node", TargetID: rec.Node, TargetName: rec.AppName,
		Outcome: outcome, Details: details,
	})
}

func (d *Deployer) host(ctx context.Context, platformID uuid.UUID, name string) (ports.SensorHost, error) {
	hosts, err := d.Hosts.OnlineHosts(ctx, platformID)
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

// pinnedOnly accepts exactly the key already recorded for a node and never
// learns one, as installing a package and preparing a disk do.
type pinnedOnly struct{ known ports.NodeSSH }

func (p pinnedOnly) Check(address, algorithm, fingerprint string, publicKey []byte) error {
	if p.known.Fingerprint == "" {
		return ErrNotPinned
	}
	if p.known.Algorithm != algorithm || p.known.Fingerprint != fingerprint {
		return fmt.Errorf("deploy: the node presented %s %s, not the pinned key",
			algorithm, fingerprint)
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

func atoi(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, err == nil
}
