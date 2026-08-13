package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/inventory"
)

// redisOpt adapts an existing Redis client's options for Asynq, so the queue
// and the cache share one connection configuration.
func redisOpt(client *redis.Client) asynq.RedisConnOpt {
	o := client.Options()
	return asynq.RedisClientOpt{
		Addr:     o.Addr,
		Username: o.Username,
		Password: o.Password,
		DB:       o.DB,
	}
}

// Client enqueues tasks. The API role uses it for manually triggered syncs.
type Client struct {
	inner *asynq.Client
	log   zerolog.Logger
}

// NewClient builds a task client.
func NewClient(rdb *redis.Client, log zerolog.Logger) *Client {
	return &Client{inner: asynq.NewClient(redisOpt(rdb)), log: log}
}

// Close releases the client.
func (c *Client) Close() error { return c.inner.Close() }

// EnqueueInventorySync queues an immediate sync. A duplicate while one is
// already pending is not an error: the existing task will do the work.
func (c *Client) EnqueueInventorySync(ctx context.Context, platformID uuid.UUID, trigger string) error {
	task, err := NewSyncInventoryTask(platformID, trigger)
	if err != nil {
		return err
	}
	if _, err := c.inner.EnqueueContext(ctx, task); err != nil {
		if errors.Is(err, asynq.ErrTaskIDConflict) || errors.Is(err, asynq.ErrDuplicateTask) {
			c.log.Debug().Str("platform_id", platformID.String()).
				Msg("sync already queued; skipping duplicate")
			return nil
		}
		return fmt.Errorf("enqueue sync: %w", err)
	}
	return nil
}

// Worker consumes tasks.
type Worker struct {
	server *asynq.Server
	mux    *asynq.ServeMux
	log    zerolog.Logger
}

// NewWorker builds the task consumer.
func NewWorker(rdb *redis.Client, handler *SyncHandler, relay *Relay, log zerolog.Logger) *Worker {
	server := asynq.NewServer(redisOpt(rdb), asynq.Config{
		Concurrency: 8,
		// Health probes must not queue behind a slow inventory sync: they are
		// what closes the circuit breaker after an outage.
		Queues:          map[string]int{QueueDefault: 3, QueueSync: 7},
		StrictPriority:  false,
		ShutdownTimeout: 20 * time.Second,
		Logger:          asynqLogger{log},
		RetryDelayFunc: func(n int, err error, t *asynq.Task) time.Duration {
			return backoff(n)
		},
	})

	mux := asynq.NewServeMux()
	mux.HandleFunc(TaskSyncInventory, handler.HandleInventory)
	mux.HandleFunc(TaskSyncHealth, handler.HandleHealth)
	mux.HandleFunc(TaskOutboxRelay, relay.Handle)

	return &Worker{server: server, mux: mux, log: log}
}

// Start runs the worker in the background.
func (w *Worker) Start() error {
	if err := w.server.Start(w.mux); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}
	w.log.Info().Str("component", "worker").Msg("task handlers registered")
	return nil
}

// Shutdown drains in-flight tasks.
func (w *Worker) Shutdown() { w.server.Shutdown() }

// backoff is exponential with a ceiling: 10s, 20s, 40s, 80s, capped at 3m.
// Jitter comes from Asynq's own scheduling granularity.
func backoff(attempt int) time.Duration {
	d := time.Duration(10<<uint(attempt)) * time.Second
	if d > 3*time.Minute {
		return 3 * time.Minute
	}
	return d
}

// Scheduler enqueues periodic work. Exactly one scheduler may run at a time,
// which Asynq enforces through a Redis lock on the periodic task manager.
type Scheduler struct {
	client    *Client
	platforms ports.PlatformRepository
	log       zerolog.Logger
	interval  time.Duration
	stop      chan struct{}
}

// NewScheduler builds the periodic enqueuer.
func NewScheduler(client *Client, platforms ports.PlatformRepository, log zerolog.Logger) *Scheduler {
	return &Scheduler{
		client: client, platforms: platforms, log: log,
		interval: 15 * time.Second, stop: make(chan struct{}),
	}
}

// Start begins scheduling. It ticks faster than any platform's interval and
// decides per platform whether that platform is due, so per-platform cadences
// can differ without a task per platform per cadence.
func (s *Scheduler) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()

		lastInventory := map[uuid.UUID]time.Time{}
		lastHealth := map[uuid.UUID]time.Time{}

		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stop:
				return
			case now := <-ticker.C:
				s.tick(ctx, now.UTC(), lastInventory, lastHealth)
			}
		}
	}()
	s.log.Info().Str("component", "scheduler").Dur("tick", s.interval).Msg("periodic scheduling started")
}

// Stop ends scheduling.
func (s *Scheduler) Stop() { close(s.stop) }

func (s *Scheduler) tick(ctx context.Context, now time.Time, lastInventory, lastHealth map[uuid.UUID]time.Time) {
	platforms, err := s.platforms.List(ctx, false)
	if err != nil {
		s.log.Error().Err(err).Msg("scheduler could not list platforms")
		return
	}

	for _, p := range platforms {
		if !p.ShouldSync(now) {
			continue
		}

		intervals := p.SyncIntervals
		if intervals.Inventory <= 0 {
			intervals = inventory.DefaultSyncIntervals()
		}

		if due(lastInventory[p.ID], now, intervals.Inventory) {
			if err := s.client.EnqueueInventorySync(ctx, p.ID, "schedule"); err != nil {
				s.log.Error().Err(err).Str("platform", p.Name).Msg("could not enqueue inventory sync")
			} else {
				lastInventory[p.ID] = now
			}
		}

		if due(lastHealth[p.ID], now, intervals.Health) {
			task, err := NewSyncHealthTask(p.ID)
			if err == nil {
				if _, err := s.client.inner.EnqueueContext(ctx, task); err != nil && !isDuplicate(err) {
					s.log.Error().Err(err).Str("platform", p.Name).Msg("could not enqueue health probe")
				} else {
					lastHealth[p.ID] = now
				}
			}
		}
	}
}

func due(last, now time.Time, intervalSeconds int) bool {
	if intervalSeconds <= 0 {
		return false
	}
	return last.IsZero() || now.Sub(last) >= time.Duration(intervalSeconds)*time.Second
}

func isDuplicate(err error) bool {
	return errors.Is(err, asynq.ErrTaskIDConflict) || errors.Is(err, asynq.ErrDuplicateTask)
}

// asynqLogger routes Asynq's own output into the structured logger.
type asynqLogger struct{ log zerolog.Logger }

func (l asynqLogger) Debug(args ...any) { l.log.Debug().Msg(fmt.Sprint(args...)) }
func (l asynqLogger) Info(args ...any)  { l.log.Debug().Msg(fmt.Sprint(args...)) }
func (l asynqLogger) Warn(args ...any)  { l.log.Warn().Msg(fmt.Sprint(args...)) }
func (l asynqLogger) Error(args ...any) { l.log.Error().Msg(fmt.Sprint(args...)) }
func (l asynqLogger) Fatal(args ...any) { l.log.Error().Msg(fmt.Sprint(args...)) }
