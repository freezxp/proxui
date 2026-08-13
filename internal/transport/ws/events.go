package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// EventChannel is the Redis pub/sub channel the outbox relay publishes to.
const EventChannel = "proxui:events"

// Heartbeat and read limits for the event socket.
const (
	pingInterval = 30 * time.Second
	pongWait     = 70 * time.Second
	writeWait    = 10 * time.Second
	// sendBuffer is how many events a slow browser may fall behind before it is
	// disconnected. Dropping a laggard protects every other subscriber from
	// being held up by one stalled connection.
	sendBuffer = 64
)

// VisibilityChecker decides whether a subscriber may see an event about a VM.
type VisibilityChecker interface {
	CanAccessVM(ctx context.Context, id uuid.UUID, role identity.Role, userID uuid.UUID) (bool, error)
}

// EventHub fans Redis pub/sub events out to subscribed browsers.
//
// Scoping happens per subscriber at delivery time: an operator watching the
// dashboard must not learn that a VM they cannot see just went down. That check
// is the reason this is not a plain broadcast (INV-04, RBAC-05).
type EventHub struct {
	Redis      *redis.Client
	Visibility VisibilityChecker
	Log        zerolog.Logger

	mu          sync.RWMutex
	subscribers map[*subscriber]struct{}
	started     bool
}

type subscriber struct {
	userID uuid.UUID
	role   identity.Role
	send   chan []byte
	done   chan struct{}
	once   sync.Once
}

func (s *subscriber) close() {
	s.once.Do(func() { close(s.done) })
}

// NewEventHub builds the hub.
func NewEventHub(rdb *redis.Client, visibility VisibilityChecker, log zerolog.Logger) *EventHub {
	return &EventHub{
		Redis: rdb, Visibility: visibility, Log: log,
		subscribers: map[*subscriber]struct{}{},
	}
}

// Start subscribes to the event channel and fans messages out until ctx ends.
func (h *EventHub) Start(ctx context.Context) {
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return
	}
	h.started = true
	h.mu.Unlock()

	pubsub := h.Redis.Subscribe(ctx, EventChannel)
	go func() {
		defer pubsub.Close()
		ch := pubsub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				h.dispatch(ctx, []byte(msg.Payload))
			}
		}
	}()
	h.Log.Info().Str("component", "events").Msg("event broadcaster started")
}

func (h *EventHub) dispatch(ctx context.Context, payload []byte) {
	var event ports.DomainEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		h.Log.Warn().Err(err).Msg("dropping unparseable event")
		return
	}

	vmID := vmIDFrom(event)

	h.mu.RLock()
	targets := make([]*subscriber, 0, len(h.subscribers))
	for sub := range h.subscribers {
		targets = append(targets, sub)
	}
	h.mu.RUnlock()

	for _, sub := range targets {
		if !h.visible(ctx, sub, vmID) {
			continue
		}
		select {
		case sub.send <- payload:
		default:
			// The subscriber is not keeping up. Disconnecting it is better
			// than blocking delivery to everyone else; the browser will
			// reconnect and resync.
			h.Log.Debug().Str("user_id", sub.userID.String()).Msg("dropping a slow event subscriber")
			sub.close()
		}
	}
}

// visible applies the same scoping rule the REST API uses.
func (h *EventHub) visible(ctx context.Context, sub *subscriber, vmID uuid.UUID) bool {
	if sub.role == identity.RoleAdmin || sub.role == identity.RoleAuditor {
		return true
	}
	if vmID == uuid.Nil {
		// Platform-level events (a sync failing, a platform recovering) carry
		// no VM. Only privileged roles receive them: an operator seeing "sync
		// failed" for a platform they have no VMs on is noise at best.
		return false
	}
	allowed, err := h.Visibility.CanAccessVM(ctx, vmID, sub.role, sub.userID)
	if err != nil {
		h.Log.Warn().Err(err).Msg("could not resolve event visibility; withholding the event")
		return false
	}
	return allowed
}

func vmIDFrom(event ports.DomainEvent) uuid.UUID {
	raw, ok := event.Payload["vm_id"].(string)
	if !ok {
		return uuid.Nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// ServeHTTP upgrades a subscriber and streams events for the connection's life.
func (h *EventHub) ServeHTTP(w http.ResponseWriter, r *http.Request, userID uuid.UUID, role identity.Role) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			return origin == "" || sameOrigin(origin, r)
		},
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	sub := &subscriber{
		userID: userID, role: role,
		send: make(chan []byte, sendBuffer), done: make(chan struct{}),
	}

	h.mu.Lock()
	h.subscribers[sub] = struct{}{}
	count := len(h.subscribers)
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.subscribers, sub)
		h.mu.Unlock()
		sub.close()
	}()
	h.Log.Debug().Int("subscribers", count).Msg("event subscriber connected")

	// A reader is required even though subscribers send nothing: it processes
	// pongs and notices the browser going away.
	go func() {
		defer sub.close()
		conn.SetReadLimit(1024)
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(pongWait))
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.done:
			return
		case payload := <-sub.send:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		case <-ping.C:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// Subscribers reports the current subscriber count, for metrics and tests.
func (h *EventHub) Subscribers() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subscribers)
}
