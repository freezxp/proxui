package proxmox

// Failover is transport behaviour, so it is tested against real servers rather
// than a fake: the questions worth answering — does a refused connection move
// to the next member, does a 401 stay put, does a pinned certificate still get
// checked — are all questions about what net/http and crypto/tls actually do
// (ADR 0009).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freezxp/proxui/internal/connector"
)

func testCreds() connector.Credentials {
	return connector.Credentials{Kind: "api_token", TokenID: "root@pam!test", Secret: "s3cret"}
}

// versionServer answers /version and counts what it was asked.
func versionServer(t *testing.T, hits *atomic.Int64, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const versionBody = `{"data":{"version":"9.2.10","release":"9.2"}}`

// The incident this exists for: the configured endpoint is the node that went
// down, and three other members were answering the whole time.
func TestUnreachableEndpointFailsOverToAnotherMember(t *testing.T) {
	var alive atomic.Int64
	up := versionServer(t, &alive, http.StatusOK, versionBody)

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close() // nothing is listening: connections are refused

	c, err := newClient(connector.Config{
		Endpoint: dead.URL,
		Failover: []connector.Endpoint{{Address: strings.TrimPrefix(up.URL, "http://")}},
	}, testCreds(), connector.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	// The failover address carries no scheme, so it defaults to https; point it
	// at the plain-HTTP test server explicitly instead.
	c.targets[1].base.Scheme = "http"

	var out struct {
		Version string `json:"version"`
	}
	if err := c.get(context.Background(), "/version", &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	if out.Version != "9.2.10" {
		t.Errorf("version = %q, want 9.2.10", out.Version)
	}
	if alive.Load() != 1 {
		t.Errorf("live member served %d requests, want 1", alive.Load())
	}
}

// A cluster-wide token that is rejected by one member is rejected by all of
// them. Trying each in turn multiplies one actionable failure into several and
// delays the alert an administrator needs.
func TestAuthFailureDoesNotTryOtherMembers(t *testing.T) {
	var firstHits, secondHits atomic.Int64
	first := versionServer(t, &firstHits, http.StatusUnauthorized, `{"data":null}`)
	second := versionServer(t, &secondHits, http.StatusOK, versionBody)

	c := clientOver(t, first, second)

	err := c.get(context.Background(), "/version", nil)
	if !errors.Is(err, connector.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if secondHits.Load() != 0 {
		t.Errorf("second member was asked %d times after a 401; want 0", secondHits.Load())
	}
}

// A platform that answered with a body we could not parse has still answered.
// Asking a different member is unlikely to change what it says.
func TestMalformedResponseDoesNotFailOver(t *testing.T) {
	var firstHits, secondHits atomic.Int64
	first := versionServer(t, &firstHits, http.StatusOK, `{"data":{`)
	second := versionServer(t, &secondHits, http.StatusOK, versionBody)

	c := clientOver(t, first, second)

	var out struct{ Version string }
	if err := c.get(context.Background(), "/version", &out); err == nil {
		t.Fatal("want an error for a malformed body")
	}
	if secondHits.Load() != 0 {
		t.Errorf("second member was asked %d times after a decode failure; want 0", secondHits.Load())
	}
}

// Preference is sticky: whatever answered keeps answering, rather than the
// client drifting back to a configured endpoint that is still down and paying
// a timeout for it on every call.
func TestPreferenceStaysWithTheMemberThatAnswered(t *testing.T) {
	var aliveHits atomic.Int64
	up := versionServer(t, &aliveHits, http.StatusOK, versionBody)

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()

	c, err := newClient(connector.Config{
		Endpoint: dead.URL,
		Failover: []connector.Endpoint{{Address: strings.TrimPrefix(up.URL, "http://")}},
	}, testCreds(), connector.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	c.targets[1].base.Scheme = "http"

	for i := 0; i < 3; i++ {
		if err := c.get(context.Background(), "/version", nil); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := c.prefIndex(); got != 1 {
		t.Errorf("preferred target = %d, want 1", got)
	}
	if aliveHits.Load() != 3 {
		t.Errorf("live member served %d requests, want 3", aliveHits.Load())
	}
}

// A cluster that is entirely down must fail inside the cycle that asked, not
// stack one timeout per member on top of it.
func TestFailoverStopsWhenTheDeadlineLeavesNoRoom(t *testing.T) {
	var hits atomic.Int64
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-r.Context().Done()
	}))
	t.Cleanup(slow.Close)

	c, err := newClient(connector.Config{
		Endpoint: slow.URL,
		Failover: []connector.Endpoint{
			{Address: strings.TrimPrefix(slow.URL, "http://")},
			{Address: "10.255.255.1:8006"},
		},
	}, testCreds(), connector.Options{Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := c.get(ctx, "/version", nil); !errors.Is(err, connector.ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	// One attempt fits; a second would not, so it is not made.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s: failover kept trying past the deadline", elapsed)
	}
}

// The point of fingerprint mode is that a self-signed cluster is trusted
// exactly. Each member presents its own certificate, so each needs its own pin
// — and a member whose pin is unknown is dropped rather than trusted loosely.
func TestPinnedFailoverUsesTheMembersOwnFingerprint(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(versionBody))
	}))
	t.Cleanup(up.Close)

	sum := sha256.Sum256(up.Certificate().Raw)
	pin := hex.EncodeToString(sum[:])

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()

	policy := connector.TLSPolicy{Mode: connector.TLSFingerprint, Fingerprint: strings.Repeat("ab", 32)}

	t.Run("with the member's pin it connects", func(t *testing.T) {
		c, err := newClient(connector.Config{
			Endpoint: dead.URL,
			TLS:      policy,
			Failover: []connector.Endpoint{{
				Address:     strings.TrimPrefix(up.URL, "https://"),
				Fingerprint: pin,
			}},
		}, testCreds(), connector.Options{Timeout: 2 * time.Second})
		if err != nil {
			t.Fatalf("newClient: %v", err)
		}
		if len(c.targets) != 2 {
			t.Fatalf("targets = %d, want 2", len(c.targets))
		}
		if err := c.get(context.Background(), "/version", nil); err != nil {
			t.Fatalf("get: %v", err)
		}
	})

	t.Run("the platform pin alone is rejected", func(t *testing.T) {
		c, err := newClient(connector.Config{
			Endpoint: dead.URL,
			TLS:      policy,
			// A pin that belongs to another member: correctly refused.
			Failover: []connector.Endpoint{{
				Address:     strings.TrimPrefix(up.URL, "https://"),
				Fingerprint: strings.Repeat("cd", 32),
			}},
		}, testCreds(), connector.Options{Timeout: 2 * time.Second})
		if err != nil {
			t.Fatalf("newClient: %v", err)
		}
		if err := c.get(context.Background(), "/version", nil); !errors.Is(err, connector.ErrUnreachable) {
			t.Fatalf("err = %v, want the mismatched pin to be refused", err)
		}
	})

	t.Run("a member with no pin is not a candidate", func(t *testing.T) {
		c, err := newClient(connector.Config{
			Endpoint: dead.URL,
			TLS:      policy,
			Failover: []connector.Endpoint{{Address: strings.TrimPrefix(up.URL, "https://")}},
		}, testCreds(), connector.Options{Timeout: 2 * time.Second})
		if err != nil {
			t.Fatalf("newClient: %v", err)
		}
		if len(c.targets) != 1 {
			t.Errorf("targets = %d, want 1: an unpinned member must not be trusted", len(c.targets))
		}
	})
}

// clientOver builds a client whose configured endpoint is the first server and
// whose single failover candidate is the second.
func clientOver(t *testing.T, first, second *httptest.Server) *client {
	t.Helper()
	c, err := newClient(connector.Config{
		Endpoint: first.URL,
		Failover: []connector.Endpoint{{Address: strings.TrimPrefix(second.URL, "http://")}},
	}, testCreds(), connector.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	c.targets[1].base.Scheme = "http"
	return c
}

// Discovery is what keeps the failover list correct as a cluster changes, and
// what makes the pins trustworthy: both the addresses and the certificates come
// from the cluster describing itself over a connection already verified.
func TestDiscoverEndpointsLearnsMembersAndTheirPins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/cluster/status"):
			_, _ = w.Write([]byte(`{"data":[
				{"type":"cluster","name":"home"},
				{"type":"node","name":"pve","ip":"10.0.30.111","local":1,"online":1},
				{"type":"node","name":"pve2","ip":"10.0.29.111","online":1},
				{"type":"node","name":"pve3","ip":"10.0.29.11","online":0}
			]}`))
		case strings.Contains(r.URL.Path, "/certificates/info"):
			// Proxmox prints fingerprints colon-separated and upper case, and
			// serves the operator's certificate in preference to its own.
			_, _ = w.Write([]byte(`{"data":[
				{"filename":"pve-root-ca.pem","fingerprint":"` + colonize(strings.Repeat("11", 32)) + `"},
				{"filename":"pve-ssl.pem","fingerprint":"` + colonize(strings.Repeat("22", 32)) + `"},
				{"filename":"pveproxy-ssl.pem","fingerprint":"` + colonize(strings.Repeat("33", 32)) + `"}
			]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := connector.Config{
		Endpoint: srv.URL,
		TLS:      connector.TLSPolicy{Mode: connector.TLSFingerprint, Fingerprint: strings.Repeat("ab", 32)},
	}
	c, err := newClient(cfg, testCreds(), connector.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	conn := &Connector{client: c, cfg: cfg}

	got, err := conn.DiscoverEndpoints(context.Background())
	if err != nil {
		t.Fatalf("DiscoverEndpoints: %v", err)
	}

	// pve3 is offline: a node that is deliberately down should not cost a
	// timeout on every failover, and it rejoins when the cluster says so.
	if len(got) != 2 {
		t.Fatalf("endpoints = %v, want the two online members", got)
	}
	for _, ep := range got {
		if ep.Address == "10.0.29.11" {
			t.Error("an offline member was offered as a failover candidate")
		}
		if ep.Fingerprint != strings.Repeat("33", 32) {
			t.Errorf("%s pinned %q, want the pveproxy certificate", ep.Address, ep.Fingerprint)
		}
	}
}

// Under a CA or system-root policy every member is already covered, so
// discovery must not invent per-member pins that nothing asked for.
func TestDiscoverEndpointsSkipsPinsUnderCAPolicies(t *testing.T) {
	var certCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/certificates/info") {
			certCalls.Add(1)
		}
		_, _ = w.Write([]byte(`{"data":[{"type":"node","name":"pve2","ip":"10.0.29.111","online":1}]}`))
	}))
	t.Cleanup(srv.Close)

	cfg := connector.Config{Endpoint: srv.URL, TLS: connector.TLSPolicy{Mode: connector.TLSVerify}}
	c, err := newClient(cfg, testCreds(), connector.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}

	got, err := (&Connector{client: c, cfg: cfg}).DiscoverEndpoints(context.Background())
	if err != nil {
		t.Fatalf("DiscoverEndpoints: %v", err)
	}
	if len(got) != 1 || got[0].Fingerprint != "" {
		t.Errorf("endpoints = %v, want one member with no pin", got)
	}
	if certCalls.Load() != 0 {
		t.Errorf("read %d certificates under a CA policy; want 0", certCalls.Load())
	}
}

func colonize(hexDigest string) string {
	parts := make([]string, 0, len(hexDigest)/2)
	for i := 0; i+2 <= len(hexDigest); i += 2 {
		parts = append(parts, strings.ToUpper(hexDigest[i:i+2]))
	}
	return strings.Join(parts, ":")
}

// A regression test for the bug that only a live cluster found: the health
// probe runs under a 30-second task deadline and the client timeout is also 30
// seconds. A guard that demanded a full timeout of headroom therefore refused
// every second attempt, so health probes reported a platform unreachable while
// inventory — on a longer deadline — failed over and reported it healthy.
func TestFailoverWorksUnderADeadlineNoLongerThanTheClientTimeout(t *testing.T) {
	var alive atomic.Int64
	up := versionServer(t, &alive, http.StatusOK, versionBody)

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()

	c, err := newClient(connector.Config{
		Endpoint: dead.URL,
		Failover: []connector.Endpoint{{Address: strings.TrimPrefix(up.URL, "http://")}},
	}, testCreds(), connector.Options{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	c.targets[1].base.Scheme = "http"

	// Exactly the health probe's shape: a deadline equal to the client timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := c.get(ctx, "/version", nil); err != nil {
		t.Fatalf("get: %v", err)
	}
	if alive.Load() != 1 {
		t.Errorf("live member served %d requests, want 1: failover was skipped", alive.Load())
	}
}
