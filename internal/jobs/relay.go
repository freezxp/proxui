package jobs

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	notifydomain "github.com/freezxp/proxui/internal/domain/notify"
)

// EventChannel is the Redis pub/sub channel domain events are published on.
// The notifier and the WebSocket broadcaster subscribe to it.
const EventChannel = "proxui:events"

// OutboxStore reads and acknowledges queued events.
type OutboxStore interface {
	UnpublishedEvents(ctx context.Context, limit int) ([]ports.DomainEvent, error)
	MarkEventsPublished(ctx context.Context, ids []int64, now time.Time) error
}

// EventNotifier delivers an event to whichever channels are routed to it. It
// is an interface so the relay stays independent of notification concerns and
// can run without one configured.
type EventNotifier interface {
	Dispatch(ctx context.Context, event notifydomain.Event) error
}

// Relay moves events from the transactional outbox onto the bus.
//
// This is the second half of the outbox pattern: writing the event with its
// state change guarantees it exists, and the relay guarantees it is delivered.
// Consumers are idempotent, so publishing an event twice after a crash between
// publish and acknowledge is harmless (docs/10-sync-engine.md §10.8).
type Relay struct {
	Store OutboxStore
	Redis *redis.Client
	Log   zerolog.Logger
	Clock ports.Clock
	// Notifier is optional: with none configured the portal still publishes
	// events to the live UI, it just sends nothing outward.
	Notifier EventNotifier
}

// Handle drains the outbox once.
func (r *Relay) Handle(ctx context.Context, _ *asynq.Task) error {
	events, err := r.Store.UnpublishedEvents(ctx, 200)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}

	published := make([]int64, 0, len(events))
	for _, e := range events {
		payload, err := json.Marshal(e)
		if err != nil {
			// A malformed event must not block the queue behind it; record it
			// as published so the relay moves on, and log loudly.
			r.Log.Error().Err(err).Int64("event_id", e.ID).Msg("dropping unencodable event")
			published = append(published, e.ID)
			continue
		}
		if err := r.Redis.Publish(ctx, EventChannel, payload).Err(); err != nil {
			// Stop at the first failure: the remaining events stay queued and
			// keep their order on the next tick.
			r.Log.Warn().Err(err).Msg("event publish failed; will retry")
			break
		}
		// Notification is deliberately after the publish and before the
		// acknowledgement: a crash in between re-delivers the event, and the
		// dispatcher's per-event claim makes that harmless.
		r.notify(ctx, e)
		published = append(published, e.ID)
	}

	if err := r.Store.MarkEventsPublished(ctx, published, r.Clock.Now()); err != nil {
		return err
	}
	r.Log.Debug().Int("events", len(published)).Msg("outbox drained")
	return nil
}

// notify hands one event to the dispatcher. A notification failure must not
// stop the outbox draining: the delivery log records what happened, and
// blocking the queue would take the live UI down with it.
func (r *Relay) notify(ctx context.Context, e ports.DomainEvent) {
	if r.Notifier == nil {
		return
	}
	event := notifydomain.Event{
		OutboxID:   e.ID,
		Category:   e.Category,
		Type:       e.Type,
		Severity:   notifydomain.Severity(e.Severity),
		OccurredAt: e.OccurredAt,
		Payload:    e.Payload,
	}
	event.PlatformID = payloadUUID(e.Payload, "platform_id")
	event.VMID = payloadUUID(e.Payload, "vm_id", "id")

	if err := r.Notifier.Dispatch(ctx, event); err != nil {
		r.Log.Error().Err(err).Int64("event_id", e.ID).Msg("could not dispatch notifications")
	}
}

// payloadUUID pulls an identifier out of an event payload. Events carry their
// subject as free-form JSON, so routing has to read it back out here.
func payloadUUID(payload map[string]any, keys ...string) *uuid.UUID {
	for _, key := range keys {
		raw, ok := payload[key]
		if !ok {
			continue
		}
		s, ok := raw.(string)
		if !ok {
			continue
		}
		if parsed, err := uuid.Parse(s); err == nil {
			return &parsed
		}
	}
	return nil
}

// Start runs the relay on a short interval. It polls rather than listening
// because the outbox is a database table: a two-second delay on a notification
// is imperceptible, and polling needs no extra moving parts.
func (r *Relay) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := r.Handle(ctx, nil); err != nil {
					r.Log.Error().Err(err).Msg("outbox relay failed")
				}
			}
		}
	}()
}
