package sync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/inventory"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// Service owns a platform's synchronization lifecycle: build its connector,
// run the reconciler, and translate the outcome into health and circuit-breaker
// state. The job layer calls this; it knows nothing about queues.
type Service struct {
	Platforms  ports.PlatformRepository
	Runs       RunStore
	Reconciler *Reconciler
	Vault      *crypto.Vault
	Clock      ports.Clock
	Log        zerolog.Logger
}

// isManualTrigger reports whether a run was asked for by a person rather than
// by the scheduler.
func isManualTrigger(trigger string) bool {
	return strings.HasPrefix(trigger, "manual:") || trigger == "registration"
}

// ErrBreakerOpen reports that a platform is being skipped because it keeps
// failing. It is not an error condition in itself: skipping is the intended
// behaviour, and the job should end quietly rather than retry.
var ErrBreakerOpen = errors.New("sync: platform circuit breaker is open")

// Connect builds a connector for a platform, decrypting its credential for the
// duration of the call only.
func (s *Service) Connect(ctx context.Context, p *inventory.Platform) (connector.Connector, error) {
	cred, err := s.Platforms.Credential(ctx, p.ID, s.Vault)
	if err != nil {
		return nil, fmt.Errorf("load credential for %s: %w", p.Name, err)
	}

	cfg := connector.Config{
		Endpoint:   p.EndpointURL,
		Datacenter: p.Datacenter,
		TLS: connector.TLSPolicy{
			Mode:        connector.TLSMode(p.TLSMode),
			CAPEM:       p.TLSCAPEM,
			Fingerprint: p.TLSFingerprint,
		},
		Extra: p.Config,
	}
	creds := connector.Credentials{Kind: cred.Kind, TokenID: cred.TokenID, Secret: cred.Secret}

	conn, err := connector.New(p.Type, cfg, creds, connector.Options{Timeout: 30 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("build connector for %s: %w", p.Name, err)
	}
	return conn, nil
}

// SyncInventory runs one inventory synchronization, updating health and the
// circuit breaker from the result.
func (s *Service) SyncInventory(ctx context.Context, platformID uuid.UUID, trigger string) (Result, error) {
	now := s.Clock.Now()

	p, err := s.Platforms.Get(ctx, platformID)
	if err != nil {
		return Result{}, err
	}
	// A human asking for a sync overrides the automatic backoff: they may have
	// just fixed whatever was broken, and being told to wait five minutes for a
	// button they just pressed is indefensible. Scheduled runs still respect it.
	manual := isManualTrigger(trigger)

	if !p.IsEnabled || p.IsDeleted() {
		return Result{Status: "skipped"}, nil
	}
	if p.BreakerOpen(now) && !manual {
		s.Log.Debug().Str("platform", p.Name).Time("until", p.BreakerOpenUntil).
			Msg("skipping sync: circuit breaker open")
		return Result{Status: "skipped"}, ErrBreakerOpen
	}

	conn, err := s.Connect(ctx, p)
	if err != nil {
		s.recordFailure(ctx, p, err)
		return Result{Status: "failed"}, err
	}
	defer conn.Close()

	result, err := s.Reconciler.Reconcile(ctx, p, conn, trigger)
	if err != nil {
		s.recordFailure(ctx, p, err)
		return result, err
	}

	s.recordSuccess(ctx, p, conn)
	return result, nil
}

// CheckHealth probes a platform cheaply and updates its health. This is what
// closes the circuit breaker again after an outage.
func (s *Service) CheckHealth(ctx context.Context, platformID uuid.UUID) error {
	p, err := s.Platforms.Get(ctx, platformID)
	if err != nil {
		return err
	}
	if !p.IsEnabled || p.IsDeleted() {
		return nil
	}

	conn, err := s.Connect(ctx, p)
	if err != nil {
		s.recordFailure(ctx, p, err)
		return err
	}
	defer conn.Close()

	hc, ok := conn.(connector.HealthCollector)
	if !ok {
		return nil
	}
	report, err := hc.Health(ctx)
	if err != nil {
		s.recordFailure(ctx, p, err)
		return err
	}

	switch report.State {
	case connector.HealthHealthy:
		// A successful probe is what lets a broken platform recover on its own.
		s.applySuccess(ctx, p, report.Version)
	case connector.HealthDegraded:
		p.Health = inventory.HealthDegraded
		p.HealthDetail = report.Detail
		p.DetectedVersion = report.Version
		s.persistHealth(ctx, p)
	default:
		s.recordFailure(ctx, p, errors.New(report.Detail))
	}
	return nil
}

func (s *Service) recordFailure(ctx context.Context, p *inventory.Platform, cause error) {
	now := s.Clock.Now()
	opened := p.RecordSyncFailure(now, cause.Error())
	s.persistHealth(ctx, p)

	if !opened {
		return
	}
	// Opening the breaker is the moment worth telling someone about: it means
	// the portal has stopped trying, and data is going stale from here on.
	s.Log.Error().Str("platform", p.Name).Err(cause).
		Time("retry_after", p.BreakerOpenUntil).Msg("circuit breaker opened")

	severity := ports.SeverityWarning
	if errors.Is(cause, connector.ErrAuth) || errors.Is(cause, connector.ErrPermission) {
		// Credentials will not fix themselves; this needs a human.
		severity = ports.SeverityCritical
	}
	s.publish(ctx, ports.DomainEvent{
		OccurredAt: now,
		Category:   ports.EventCategorySyncFailure,
		Type:       ports.EventSyncFailed,
		Severity:   severity,
		Payload: map[string]any{
			"platform_id": p.ID.String(), "platform_name": p.Name,
			"error": cause.Error(), "retry_after": p.BreakerOpenUntil,
			"consecutive_failures": p.ConsecutiveFailures,
		},
	})
}

func (s *Service) recordSuccess(ctx context.Context, p *inventory.Platform, conn connector.Connector) {
	version := p.DetectedVersion
	if hc, ok := conn.(connector.HealthCollector); ok {
		if report, err := hc.Health(ctx); err == nil && report.Version != "" {
			version = report.Version
		}
	}
	s.applySuccess(ctx, p, version)
}

func (s *Service) applySuccess(ctx context.Context, p *inventory.Platform, version string) {
	now := s.Clock.Now()
	recovered := p.RecordSyncSuccess(now)
	if version != "" {
		p.DetectedVersion = version
	}
	s.persistHealth(ctx, p)

	if !recovered {
		return
	}
	s.Log.Info().Str("platform", p.Name).Msg("platform recovered")
	s.publish(ctx, ports.DomainEvent{
		OccurredAt: now,
		Category:   ports.EventCategorySyncFailure,
		Type:       ports.EventSyncRecovered,
		Severity:   ports.SeverityInfo,
		Payload:    map[string]any{"platform_id": p.ID.String(), "platform_name": p.Name},
	})
}

func (s *Service) persistHealth(ctx context.Context, p *inventory.Platform) {
	if err := s.Platforms.UpdateHealth(context.WithoutCancel(ctx), p); err != nil {
		s.Log.Error().Err(err).Str("platform", p.Name).Msg("could not persist platform health")
	}
}

// publish queues an event outside any reconciliation transaction, for events
// about the platform itself rather than about an asset.
func (s *Service) publish(ctx context.Context, e ports.DomainEvent) {
	detached := context.WithoutCancel(ctx)
	tx, err := s.Runs.Begin(detached)
	if err != nil {
		s.Log.Error().Err(err).Msg("could not queue event")
		return
	}
	defer func() { _ = tx.Rollback(detached) }()

	if err := s.Runs.PublishEvent(detached, tx, e); err != nil {
		s.Log.Error().Err(err).Str("event", e.Type).Msg("could not queue event")
		return
	}
	if err := tx.Commit(detached); err != nil {
		s.Log.Error().Err(err).Str("event", e.Type).Msg("could not commit event")
	}
}
