package deploy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/deployment"
)

// Actor is who asked, for the record and the audit entry.
type Actor struct {
	UserID   uuid.UUID
	Username string
}

// Input is a request to deploy one catalogue application.
//
// It names an application by identifier and a node by name, and carries six
// numbers. There is no field here that could become a command, and that is the
// design rather than an accident of this struct (ADR 0012).
type Input struct {
	Actor      Actor
	PlatformID uuid.UUID
	AppID      string
	Node       string
	Spec       deployment.Spec
}

// Start records a deployment and hands it to the queue.
//
// The identifier is resolved against the shipped catalogue before anything
// else. That lookup is the whole control: an application the binary does not
// know goes no further, and never becomes a path, a URL or a command.
func (d *Deployer) Start(ctx context.Context, in Input) (*deployment.Deployment, error) {
	app, ok := Find(strings.TrimSpace(in.AppID))
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownApp, in.AppID)
	}

	now := d.Clock.Now()
	rec := &deployment.Deployment{
		ID: uuid.New(), PlatformID: in.PlatformID,
		Node:  strings.TrimSpace(in.Node),
		AppID: app.ID, AppName: app.Name,
		State:           deployment.StatePending,
		RequestedByName: in.Actor.Username,
		Spec:            in.Spec,
		Created:         now, Updated: now,
	}
	if in.Actor.UserID != uuid.Nil {
		id := in.Actor.UserID
		rec.RequestedBy = &id
	}
	if err := rec.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %s", connector.ErrInvalidConfig, err)
	}

	// The node has to be one this platform reported. A name that is not in the
	// inventory is refused here rather than turning into a connection attempt.
	if _, err := d.host(ctx, rec.PlatformID, rec.Node); err != nil {
		return nil, err
	}

	if err := d.Deployments.Create(ctx, rec); err != nil {
		return nil, err
	}
	if d.Queue != nil {
		if err := d.Queue.EnqueueDeployStep(ctx, rec.ID, 0); err != nil {
			return nil, err
		}
	}
	d.Log.Info().Str("deployment", rec.ID.String()).Str("app", rec.AppID).
		Str("node", rec.Node).Str("actor", in.Actor.Username).
		Msg("container app deployment requested")
	return rec, nil
}

// Resume re-queues whatever was in flight when the portal stopped.
//
// The script itself is unaffected by a restart — it is detached on the node and
// keeps going — so this is only about the portal picking the log back up.
func (d *Deployer) Resume(ctx context.Context) error {
	open, err := d.Deployments.ListOpen(ctx)
	if err != nil {
		return err
	}
	for _, rec := range open {
		if d.Queue == nil {
			break
		}
		if err := d.Queue.EnqueueDeployStep(ctx, rec.ID, 0); err != nil {
			d.Log.Warn().Err(err).Str("deployment", rec.ID.String()).
				Msg("could not resume a deployment")
			continue
		}
		d.Log.Info().Str("deployment", rec.ID.String()).Msg("resumed a deployment")
	}
	return nil
}

// PollInterval is how often a running deploy is asked about. Slower than the
// provisioning driver's five seconds: an install is minutes long, and asking
// more often only costs handshakes.
const PollInterval = 15 * time.Second
