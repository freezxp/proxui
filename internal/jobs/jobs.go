// Package jobs is the background-work layer: Asynq task definitions, their
// handlers, and the periodic scheduler that enqueues them.
//
// Handlers stay thin. They unmarshal a payload, call an application service and
// map the outcome onto retry semantics; the work itself lives in internal/app.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/provisioner"
	appsync "github.com/freezxp/proxui/internal/app/sync"
)

// Task types.
const (
	TaskSyncInventory = "sync:inventory"
	TaskSyncHealth    = "sync:health"
	TaskSyncMetrics   = "sync:metrics"
	TaskSyncBackfill  = "sync:backfill"
	TaskOutboxRelay   = "outbox:relay"
	TaskSyncSensors   = "sync:sensors"
	TaskProvisionStep = "provision:step"
)

// Queues. Separating them means a slow inventory sync cannot delay the health
// probes that drive the circuit breaker.
const (
	QueueDefault = "default"
	QueueSync    = "sync"
)

// PlatformPayload identifies which platform a task acts on.
type PlatformPayload struct {
	PlatformID uuid.UUID `json:"platform_id"`
	Trigger    string    `json:"trigger"`
}

// SyncUniqueWindow is how long a scheduled sync holds its uniqueness lock. It
// is slightly longer than the default cadence so a slow run causes a skipped
// cycle rather than a pile-up (docs/10-sync-engine.md §10.2).
const SyncUniqueWindow = 90 * time.Second

// NewSyncInventoryTask builds an inventory sync task.
//
// Scheduled runs take a uniqueness lock that Asynq releases when the task
// finishes. A fixed task ID would be wrong here: Asynq keeps completed and
// archived tasks under their ID, so a failed run would block every later sync
// for the whole retention period — which is exactly the opposite of what a
// failing platform needs.
//
// Manual runs deliberately skip the lock. Someone pressing "sync now" has
// usually just changed something and expects it to happen.
func NewSyncInventoryTask(platformID uuid.UUID, trigger string) (*asynq.Task, error) {
	payload, err := json.Marshal(PlatformPayload{PlatformID: platformID, Trigger: trigger})
	if err != nil {
		return nil, fmt.Errorf("encode sync payload: %w", err)
	}

	opts := []asynq.Option{
		asynq.Queue(QueueSync),
		asynq.MaxRetry(5),
		asynq.Timeout(5 * time.Minute),
	}
	if !IsManualTrigger(trigger) {
		opts = append(opts, asynq.Unique(SyncUniqueWindow))
	}
	return asynq.NewTask(TaskSyncInventory, payload, opts...), nil
}

// IsManualTrigger reports whether a run was requested by a person.
func IsManualTrigger(trigger string) bool {
	return strings.HasPrefix(trigger, "manual:") || trigger == "registration"
}

// NewSyncHealthTask builds a health probe task.
func NewSyncHealthTask(platformID uuid.UUID) (*asynq.Task, error) {
	payload, err := json.Marshal(PlatformPayload{PlatformID: platformID, Trigger: "schedule"})
	if err != nil {
		return nil, fmt.Errorf("encode health payload: %w", err)
	}
	return asynq.NewTask(TaskSyncHealth, payload,
		asynq.Queue(QueueDefault),
		asynq.Unique(20*time.Second),
		asynq.MaxRetry(2),
		asynq.Timeout(30*time.Second),
	), nil
}

// NewSyncMetricsTask builds a metrics collection task.
func NewSyncMetricsTask(platformID uuid.UUID) (*asynq.Task, error) {
	payload, err := json.Marshal(PlatformPayload{PlatformID: platformID, Trigger: "schedule"})
	if err != nil {
		return nil, fmt.Errorf("encode metrics payload: %w", err)
	}
	return asynq.NewTask(TaskSyncMetrics, payload,
		asynq.Queue(QueueSync),
		asynq.Unique(SyncUniqueWindow),
		asynq.MaxRetry(2),
		asynq.Timeout(2*time.Minute),
	), nil
}

// NewBackfillTask builds a history import task. It is slow and runs once per
// registration, so it gets a generous timeout and no retries: a failed
// backfill costs history, not correctness.
func NewBackfillTask(platformID uuid.UUID) (*asynq.Task, error) {
	payload, err := json.Marshal(PlatformPayload{PlatformID: platformID, Trigger: "registration"})
	if err != nil {
		return nil, fmt.Errorf("encode backfill payload: %w", err)
	}
	return asynq.NewTask(TaskSyncBackfill, payload,
		asynq.Queue(QueueDefault),
		asynq.MaxRetry(1),
		asynq.Timeout(15*time.Minute),
	), nil
}

// ProvisionPayload names the request a step acts on.
type ProvisionPayload struct {
	RequestID uuid.UUID `json:"request_id"`
}

// provisionPollInterval is how often a step that is waiting on the platform
// asks again. Cloning a disk takes minutes and polling is one cheap call, so
// this is chosen to keep the portal's picture fresh rather than to spare the
// platform.
const provisionPollInterval = 5 * time.Second

// uniqueAtEntry reports whether a task should carry an idempotency lock.
//
// Only the entry points do: a submit, and the resume sweep at boot, which can
// race each other over the same request. The poll continuation must not,
// because asynq holds a unique lock until the task *completes* and the
// continuation is enqueued from inside the handler that still holds it — so it
// would be refused as a duplicate of the very task enqueuing it. The refusal is
// not an error either, which is how a request stalls in whatever state it had
// reached with nothing left watching it. Found against a live cluster; no unit
// test that drives the driver directly can see it, because the job layer is the
// part that breaks.
//
// The continuation needs no lock of its own: exactly one handler issues it, the
// one that just finished the step before.
func uniqueAtEntry(delay time.Duration) bool { return delay == 0 }

// NewProvisionTask builds one turn of the provisioning driver.
//
// The request id is the idempotency key at the entry points, so a retried
// submit or an overlapping resume sweep cannot start a second clone of the
// same guest (PROV-05).
func NewProvisionTask(requestID uuid.UUID, delay time.Duration) (*asynq.Task, error) {
	payload, err := json.Marshal(ProvisionPayload{RequestID: requestID})
	if err != nil {
		return nil, fmt.Errorf("encode provision payload: %w", err)
	}
	opts := []asynq.Option{
		asynq.Queue(QueueDefault),
		// Retries here are for a portal-side stumble — a database blip, a
		// momentarily unreachable platform. A failure the platform reported is
		// recorded on the request and does not come back as an error.
		asynq.MaxRetry(5),
		asynq.Timeout(2 * time.Minute),
	}
	if delay > 0 {
		opts = append(opts, asynq.ProcessIn(delay))
	} else if uniqueAtEntry(delay) {
		opts = append(opts, asynq.Unique(provisionPollInterval))
	}
	return asynq.NewTask(TaskProvisionStep, payload, opts...), nil
}

// SyncHandler runs synchronization tasks.
type SyncHandler struct {
	Service *appsync.Service
	Log     zerolog.Logger
}

// HandleInventory processes an inventory sync task.
func (h *SyncHandler) HandleInventory(ctx context.Context, t *asynq.Task) error {
	var payload PlatformPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		// A malformed payload will never succeed, so do not retry it.
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}

	start := time.Now()
	result, err := h.Service.SyncInventory(ctx, payload.PlatformID, payload.Trigger)
	if err != nil {
		if errors.Is(err, appsync.ErrBreakerOpen) {
			// Skipping is the intended outcome, not a failure to retry.
			return nil
		}
		h.Log.Error().Err(err).Str("platform_id", payload.PlatformID.String()).
			Msg("inventory sync failed")
		return err
	}

	h.Log.Info().
		Str("component", "sync").
		Str("platform_id", payload.PlatformID.String()).
		Str("status", result.Status).
		Int("vms", result.Stats.VMs).
		Int("added", result.Stats.Added).
		Int("changed", result.Stats.Changed).
		Int("missing", result.Stats.Missing).
		Int("deleted", result.Stats.Deleted).
		Dur("duration_ms", time.Since(start)).
		Msg("inventory sync complete")
	return nil
}

// HandleMetrics processes a metrics collection task.
func (h *SyncHandler) HandleMetrics(ctx context.Context, t *asynq.Task) error {
	var payload PlatformPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}
	stats, err := h.Service.SyncMetrics(ctx, payload.PlatformID)
	if err != nil {
		h.Log.Warn().Err(err).Str("platform_id", payload.PlatformID.String()).Msg("metrics collection failed")
		return err
	}
	h.Log.Debug().Str("component", "metrics").
		Int("vm_samples", stats.VMSamples).Int("host_samples", stats.HostSamples).
		Int("dropped", stats.Dropped).Msg("metrics collected")
	return nil
}

// NewSyncSensorsTask builds a node sensor collection task.
//
// Unique over a long window: a poll is an SSH handshake per node, and a
// backlog of them would queue up behind a node that has gone away rather than
// being dropped as the stale work it is.
func NewSyncSensorsTask(platformID uuid.UUID) (*asynq.Task, error) {
	payload, err := json.Marshal(PlatformPayload{PlatformID: platformID})
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(TaskSyncSensors, payload,
		asynq.Queue(QueueDefault), asynq.MaxRetry(1),
		asynq.Unique(4*time.Minute), asynq.Timeout(2*time.Minute)), nil
}

// HandleSensors polls a platform's nodes for their hardware sensors.
func (h *SyncHandler) HandleSensors(ctx context.Context, t *asynq.Task) error {
	var payload PlatformPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}
	stats, err := h.Service.SyncSensors(ctx, payload.PlatformID)
	if err != nil {
		h.Log.Warn().Err(err).Str("platform_id", payload.PlatformID.String()).
			Msg("node sensor collection failed")
		return err
	}
	if stats.Nodes == 0 {
		return nil
	}
	h.Log.Debug().Str("component", "sensors").
		Int("nodes", stats.Nodes).Int("answered", stats.Answered).
		Int("readings", stats.Readings).Int("silent", stats.Silent).
		Msg("node sensors collected")
	return nil
}

// HandleBackfill imports historical metrics for a newly registered platform.
func (h *SyncHandler) HandleBackfill(ctx context.Context, t *asynq.Task) error {
	var payload PlatformPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}
	from := time.Now().Add(-BackfillWindow)
	written, err := h.Service.BackfillMetrics(ctx, payload.PlatformID, from)
	if err != nil {
		h.Log.Warn().Err(err).Msg("metrics backfill failed")
		return err
	}
	h.Log.Info().Int("samples", written).Str("platform_id", payload.PlatformID.String()).
		Msg("historical metrics imported")
	return nil
}

// BackfillWindow is how much history to import on registration. A month gives
// immediately useful charts without a long first import; the platform's own
// retention decides how much actually comes back.
const BackfillWindow = 30 * 24 * time.Hour

// HandleHealth processes a health probe task.
func (h *SyncHandler) HandleHealth(ctx context.Context, t *asynq.Task) error {
	var payload PlatformPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}
	// A failed probe already updated health and the breaker; returning the
	// error would retry a platform we have just decided to back off from.
	if err := h.Service.CheckHealth(ctx, payload.PlatformID); err != nil {
		h.Log.Debug().Err(err).Str("platform_id", payload.PlatformID.String()).Msg("health probe failed")
	}
	return nil
}

// ProvisionHandler turns provisioning requests one step at a time (ADR 0010).
type ProvisionHandler struct {
	Driver *provisioner.Driver
	Client *Client
	Log    zerolog.Logger
}

// HandleStep advances one request.
//
// A step that is waiting on the platform is not a failure, so it does not come
// back as an error: retrying through asynq's backoff would exhaust MaxRetry
// while a large disk was still copying, and the request would be abandoned
// mid-clone. Instead the handler schedules the next look itself, which also
// keeps the interval a property of provisioning rather than of the retry curve.
func (h *ProvisionHandler) HandleStep(ctx context.Context, t *asynq.Task) error {
	var payload ProvisionPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}

	err := h.Driver.Step(ctx, payload.RequestID)
	switch {
	case err == nil:
		// Either finished or moved on; a request that is still open asks for
		// its next turn immediately rather than waiting out a poll interval.
		return h.rescheduleIfOpen(ctx, payload.RequestID)
	case errors.Is(err, provisioner.ErrStillRunning):
		return h.Client.EnqueueProvisionStep(ctx, payload.RequestID, provisionPollInterval)
	default:
		return err
	}
}

// rescheduleIfOpen queues the next step for a request that has not finished.
func (h *ProvisionHandler) rescheduleIfOpen(ctx context.Context, id uuid.UUID) error {
	req, err := h.Driver.Requests.GetRequest(ctx, id)
	if err != nil {
		return err
	}
	if req.State.Terminal() {
		h.Log.Info().Str("request", id.String()).Str("state", string(req.State)).
			Msg("provisioning request finished")
		return nil
	}
	return h.Client.EnqueueProvisionStep(ctx, id, provisionPollInterval)
}

// ResumeOpenRequests re-enqueues work that was in flight when the portal
// stopped.
//
// This is the other half of storing requests in a table: without it a clone
// interrupted by a restart would sit at `cloning` forever, with a guest on the
// platform that nothing was waiting for.
func (h *ProvisionHandler) ResumeOpenRequests(ctx context.Context) error {
	open, err := h.Driver.Requests.ListOpenRequests(ctx)
	if err != nil {
		return err
	}
	for _, req := range open {
		if err := h.Client.EnqueueProvisionStep(ctx, req.ID, 0); err != nil {
			h.Log.Warn().Err(err).Str("request", req.ID.String()).
				Msg("could not resume a provisioning request")
			continue
		}
		h.Log.Info().Str("request", req.ID.String()).Str("state", string(req.State)).
			Msg("resumed a provisioning request")
	}
	return nil
}
