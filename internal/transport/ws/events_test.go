package ws_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/transport/ws"
)

// The event stream is a second path to the same data the REST API guards, so
// it has to apply the same scoping. A live "VM stopped" notification for a
// machine the viewer cannot list would leak exactly what the grants exist to
// prevent.

// scopedVisibility allows only the VMs it was told about.
type scopedVisibility struct{ allowed map[uuid.UUID]bool }

func (v scopedVisibility) CanAccessVM(_ context.Context, id uuid.UUID, role identity.Role, _ uuid.UUID) (bool, error) {
	if role == identity.RoleAdmin || role == identity.RoleAuditor {
		return true, nil
	}
	return v.allowed[id], nil
}

func redisClient(t *testing.T) *redis.Client {
	t.Helper()
	url := "redis://127.0.0.1:6379/9"
	if v := envOr("PROXUI_TEST_REDIS_URL", ""); v != "" {
		url = v
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	client := redis.NewClient(opts)
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis not available: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func eventServer(t *testing.T, hub *ws.EventHub, userID uuid.UUID, role identity.Role) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.ServeHTTP(w, r, userID, role)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func dialEvents(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial events: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func publish(t *testing.T, client *redis.Client, event ports.DomainEvent) {
	t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	if err := client.Publish(context.Background(), ws.EventChannel, payload).Err(); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func TestEventsReachSubscribersWithAccess(t *testing.T) {
	client := redisClient(t)
	vmID := uuid.New()
	userID := uuid.New()

	hub := ws.NewEventHub(client, scopedVisibility{allowed: map[uuid.UUID]bool{vmID: true}}, zerolog.New(io.Discard))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub.Start(ctx)

	conn := dialEvents(t, eventServer(t, hub, userID, identity.RoleOperator))
	waitForSubscribers(t, hub, 1)

	publish(t, client, ports.DomainEvent{
		Type: ports.EventVMStateChanged, Category: ports.EventCategoryVMStateChange,
		Payload: map[string]any{"vm_id": vmID.String(), "vm_name": "web-01", "to": "stopped"},
	})

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read event: %v", err)
	}

	var got ports.DomainEvent
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if got.Type != ports.EventVMStateChanged {
		t.Errorf("event type = %q, want %q", got.Type, ports.EventVMStateChanged)
	}
}

// The test that matters: an operator must not be told about a VM they cannot see.
func TestEventsAreWithheldWithoutAccess(t *testing.T) {
	client := redisClient(t)
	visible, hidden := uuid.New(), uuid.New()

	hub := ws.NewEventHub(client, scopedVisibility{allowed: map[uuid.UUID]bool{visible: true}}, zerolog.New(io.Discard))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub.Start(ctx)

	conn := dialEvents(t, eventServer(t, hub, uuid.New(), identity.RoleOperator))
	waitForSubscribers(t, hub, 1)

	// Publish the hidden VM first, then the visible one. If scoping leaks, the
	// first message read will be the hidden one.
	publish(t, client, ports.DomainEvent{
		Type:    ports.EventVMStateChanged,
		Payload: map[string]any{"vm_id": hidden.String(), "vm_name": "secret-01"},
	})
	publish(t, client, ports.DomainEvent{
		Type:    ports.EventVMStateChanged,
		Payload: map[string]any{"vm_id": visible.String(), "vm_name": "web-01"},
	})

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	var got ports.DomainEvent
	_ = json.Unmarshal(payload, &got)

	if name, _ := got.Payload["vm_name"].(string); name != "web-01" {
		t.Fatalf("received an event for %q; an ungranted VM leaked through the event stream", name)
	}
}

// Platform-level events carry no VM, so they go only to roles that see the
// whole estate. An operator does not need "sync failed" for a platform whose
// VMs they cannot list.
func TestPlatformEventsGoToPrivilegedRolesOnly(t *testing.T) {
	client := redisClient(t)
	hub := ws.NewEventHub(client, scopedVisibility{allowed: map[uuid.UUID]bool{}}, zerolog.New(io.Discard))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub.Start(ctx)

	adminConn := dialEvents(t, eventServer(t, hub, uuid.New(), identity.RoleAdmin))
	operatorConn := dialEvents(t, eventServer(t, hub, uuid.New(), identity.RoleOperator))
	waitForSubscribers(t, hub, 2)

	publish(t, client, ports.DomainEvent{
		Type: ports.EventSyncFailed, Category: ports.EventCategorySyncFailure,
		Payload: map[string]any{"platform_name": "pve-dc1", "error": "unreachable"},
	})

	_ = adminConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := adminConn.ReadMessage(); err != nil {
		t.Fatalf("admin did not receive a platform event: %v", err)
	}

	_ = operatorConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := operatorConn.ReadMessage(); err == nil {
		t.Error("an operator received a platform-level event")
	}
}

func TestSubscribersAreTrackedAndReleased(t *testing.T) {
	client := redisClient(t)
	hub := ws.NewEventHub(client, scopedVisibility{}, zerolog.New(io.Discard))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub.Start(ctx)

	srv := eventServer(t, hub, uuid.New(), identity.RoleAdmin)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	waitForSubscribers(t, hub, 1)

	conn.Close()
	waitForSubscribers(t, hub, 0)
}

func waitForSubscribers(t *testing.T, hub *ws.EventHub, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hub.Subscribers() == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("subscriber count = %d, want %d", hub.Subscribers(), want)
}
