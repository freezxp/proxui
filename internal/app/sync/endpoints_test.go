package sync

// The sync engine's half of ADR 0009: it keeps the failover list current from
// what a healthy platform says about itself, and it declines to rewrite it from
// a platform that is not healthy.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/connectors/mock"
	"github.com/freezxp/proxui/internal/domain/inventory"
)

// endpointStore is livePlatforms with a memory for what was written to it.
type endpointStore struct {
	livePlatforms
	stored  []ports.PlatformEndpoint
	writes  int
	loadErr error
}

func (e *endpointStore) Endpoints(context.Context, uuid.UUID) ([]ports.PlatformEndpoint, error) {
	if e.loadErr != nil {
		return nil, e.loadErr
	}
	return e.stored, nil
}

func (e *endpointStore) ReplaceEndpoints(_ context.Context, _ uuid.UUID, eps []ports.PlatformEndpoint, _ time.Time) error {
	e.writes++
	e.stored = eps
	return nil
}

type fixedClock struct{ t time.Time }

func (f fixedClock) Now() time.Time { return f.t }

func testPlatform() *inventory.Platform {
	return &inventory.Platform{
		ID:          uuid.New(),
		Name:        "pve-home",
		Type:        mock.Type,
		EndpointURL: "https://10.0.30.111:8006",
	}
}

func newService(store ports.PlatformRepository, now time.Time) *Service {
	return &Service{Platforms: store, Clock: fixedClock{now}, Log: zerolog.Nop()}
}

func mockConn(t *testing.T, addrs ...string) connector.Connector {
	t.Helper()
	conn, err := mock.New(connector.Config{Endpoint: "mock://one"}, connector.Credentials{}, connector.Options{})
	if err != nil {
		t.Fatalf("mock.New: %v", err)
	}
	conn.(*mock.Connector).SetEndpoints(addrs...)
	return conn
}

// The address an administrator typed keeps its identity in the list, so an
// operator reading it can tell configuration from cluster fact.
func TestRefreshEndpointsMarksTheConfiguredMember(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := &endpointStore{}
	svc := newService(store, now)
	p := testPlatform()

	svc.refreshEndpoints(context.Background(), p, mockConn(t, "10.0.30.111", "10.0.29.111", "192.168.100.11"))

	if store.writes != 1 {
		t.Fatalf("writes = %d, want 1", store.writes)
	}
	if len(store.stored) != 3 {
		t.Fatalf("stored %d endpoints, want 3", len(store.stored))
	}
	var configured int
	for _, ep := range store.stored {
		if ep.Source == "configured" {
			configured++
			if ep.Address != "10.0.30.111" {
				t.Errorf("configured row is %q, want the platform's own endpoint", ep.Address)
			}
		}
		if !ep.RefreshedAt.Equal(now) {
			t.Errorf("%s refreshed at %s, want %s", ep.Address, ep.RefreshedAt, now)
		}
	}
	if configured != 1 {
		t.Errorf("%d rows marked configured, want exactly 1", configured)
	}
}

// Membership changes when an administrator adds a node, not every minute.
func TestRefreshEndpointsSkipsAFreshList(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := &endpointStore{stored: []ports.PlatformEndpoint{
		{Address: "10.0.29.111", Source: "discovered", RefreshedAt: now.Add(-time.Minute)},
	}}
	svc := newService(store, now)

	svc.refreshEndpoints(context.Background(), testPlatform(), mockConn(t, "10.0.29.111"))

	if store.writes != 0 {
		t.Errorf("rewrote a list %s old; want it left alone", EndpointRefreshInterval)
	}
}

func TestRefreshEndpointsRebuildsAStaleList(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := &endpointStore{stored: []ports.PlatformEndpoint{
		{Address: "10.0.29.111", Source: "discovered", RefreshedAt: now.Add(-EndpointRefreshInterval - time.Second)},
	}}
	svc := newService(store, now)

	svc.refreshEndpoints(context.Background(), testPlatform(), mockConn(t, "10.0.29.111", "10.0.29.11"))

	if store.writes != 1 {
		t.Fatalf("writes = %d, want the stale list rebuilt", store.writes)
	}
	if len(store.stored) != 2 {
		t.Errorf("stored %d endpoints, want 2", len(store.stored))
	}
}

// A discovery that comes back empty must not empty the list: the addresses
// that worked yesterday are a better guess than none.
func TestRefreshEndpointsKeepsTheListWhenDiscoveryFindsNothing(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	kept := []ports.PlatformEndpoint{
		{Address: "10.0.29.111", Source: "discovered", RefreshedAt: now.Add(-time.Hour)},
	}
	store := &endpointStore{stored: kept}
	svc := newService(store, now)

	svc.refreshEndpoints(context.Background(), testPlatform(), mockConn(t))

	if store.writes != 0 {
		t.Error("an empty discovery replaced the stored list")
	}
	if len(store.stored) != 1 {
		t.Errorf("stored = %v, want the previous list intact", store.stored)
	}
}

// The failover list is an optimization. Failing to read it must not stop a
// sync that the configured endpoint can serve perfectly well.
func TestFailoverEndpointsToleratesAnUnreadableList(t *testing.T) {
	store := &endpointStore{loadErr: errors.New("database is down")}
	svc := newService(store, time.Now())

	if got := svc.failoverEndpoints(context.Background(), testPlatform()); got != nil {
		t.Errorf("failoverEndpoints = %v, want nil rather than a failure", got)
	}
}

func TestEndpointHostNormalisesForms(t *testing.T) {
	for _, addr := range []string{"https://10.0.30.111:8006", "10.0.30.111:8006", "10.0.30.111"} {
		if got := endpointHost(addr); got != "10.0.30.111" {
			t.Errorf("endpointHost(%q) = %q, want 10.0.30.111", addr, got)
		}
	}
}
