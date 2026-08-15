package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/freezxp/proxui/internal/edge"
)

// fakeCF is a stand-in for the Cloudflare API.
//
// It models the two behaviours that make the real client non-trivial: every
// response is wrapped in the success/errors/result envelope, and a failure can
// arrive either as an HTTP status or as HTTP 200 with success:false.
type fakeCF struct {
	t         *testing.T
	server    *httptest.Server
	handlers  map[string]func() (int, string)
	requested []string
	authSeen  string
}

func newFakeCF(t *testing.T) *fakeCF {
	t.Helper()
	f := &fakeCF{t: t, handlers: map[string]func() (int, string){}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requested = append(f.requested, r.URL.Path)
		f.authSeen = r.Header.Get("Authorization")

		// Longest prefix wins, and deterministically. "/cfd_tunnel" is a
		// prefix of "/cfd_tunnel/{id}/configurations", so iterating a map and
		// taking the first match would route by whichever order Go felt like
		// today — a fake that is flaky in a way the real API is not.
		best, bestLen := "", -1
		for prefix := range f.handlers {
			if strings.HasPrefix(r.URL.Path, prefix) && len(prefix) > bestLen {
				best, bestLen = prefix, len(prefix)
			}
		}
		if bestLen >= 0 {
			status, body := f.handlers[best]()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":7003,"message":"no route"}]}`))
	}))
	t.Cleanup(f.server.Close)

	// Sensible defaults; individual tests override what they care about.
	f.ok("/user/tokens/verify", `{"status":"active"}`)
	f.ok("/zones", `[]`)
	f.ok("/accounts/acct/cfd_tunnel", `[]`)
	return f
}

func (f *fakeCF) ok(prefix, result string) {
	f.handlers[prefix] = func() (int, string) {
		return http.StatusOK, `{"success":true,"errors":[],"result":` + result + `}`
	}
}

func (f *fakeCF) fail(prefix string, status int, code int, message string) {
	f.handlers[prefix] = func() (int, string) {
		body, _ := json.Marshal(map[string]any{
			"success": false,
			"errors":  []map[string]any{{"code": code, "message": message}},
		})
		return status, string(body)
	}
}

func (f *fakeCF) provider(t *testing.T) *Provider {
	t.Helper()
	p, err := New(edge.Credentials{Token: "token-value", AccountID: "acct"}, edge.Options{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	p.baseURL = f.server.URL
	return p
}

func TestNewRequiresBothHalvesOfTheCredential(t *testing.T) {
	for name, creds := range map[string]edge.Credentials{
		"no token":   {AccountID: "acct"},
		"no account": {Token: "t"},
		"neither":    {},
	} {
		if _, err := New(creds, edge.Options{}); !errors.Is(err, edge.ErrInvalidConfig) {
			t.Errorf("%s: got %v, want ErrInvalidConfig", name, err)
		}
	}
}

func TestVerifyReportsAHealthyCredential(t *testing.T) {
	f := newFakeCF(t)
	f.ok("/accounts/acct/cfd_tunnel", `[
		{"id":"t1","name":"home","config_src":"cloudflare","connections":[{"id":"c1"},{"id":"c2"}]}
	]`)

	health, err := f.provider(t).Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !health.Reachable || !health.Authenticated {
		t.Fatalf("reachable=%v authenticated=%v, want both true", health.Reachable, health.Authenticated)
	}
	if len(health.MissingScopes) != 0 {
		t.Errorf("missing scopes = %+v, want none", health.MissingScopes)
	}
	if len(health.Tunnels) != 1 || health.Tunnels[0].Connections != 2 {
		t.Fatalf("tunnels = %+v", health.Tunnels)
	}
	if !health.Tunnels[0].Manageable() || !health.Tunnels[0].Active() {
		t.Error("a remotely-managed tunnel with connections should be manageable and active")
	}
	// The token travels as a bearer credential and nowhere else.
	if f.authSeen != "Bearer token-value" {
		t.Errorf("Authorization = %q", f.authSeen)
	}
}

// Cloudflare will not tell a token what it may do, so the scopes are
// established by calling the endpoints and seeing which are refused.
func TestVerifyNamesTheScopeThatIsMissing(t *testing.T) {
	t.Run("no tunnel access", func(t *testing.T) {
		f := newFakeCF(t)
		f.fail("/accounts/acct/cfd_tunnel", http.StatusForbidden, 10000, "Authentication error")

		health, err := f.provider(t).Verify(context.Background())
		if err != nil {
			t.Fatalf("Verify() error = %v; a missing scope is a report, not a failure", err)
		}
		if len(health.MissingScopes) != 1 || !strings.Contains(health.MissingScopes[0].Scope, "Tunnel") {
			t.Fatalf("missing scopes = %+v, want the tunnel scope", health.MissingScopes)
		}
		if health.MissingScopes[0].Blocks == "" {
			t.Error("a missing scope must say what it blocks, not just its name")
		}
	})

	t.Run("no DNS access", func(t *testing.T) {
		f := newFakeCF(t)
		f.fail("/zones", http.StatusForbidden, 10000, "Authentication error")

		health, err := f.provider(t).Verify(context.Background())
		if err != nil {
			t.Fatalf("Verify() error = %v", err)
		}
		if len(health.MissingScopes) != 1 || !strings.Contains(health.MissingScopes[0].Scope, "DNS") {
			t.Fatalf("missing scopes = %+v, want the DNS scope", health.MissingScopes)
		}
	})
}

// A rejected token still proves we reached Cloudflare, which separates a
// firewall problem from a typo.
func TestVerifyDistinguishesRejectedFromUnreachable(t *testing.T) {
	f := newFakeCF(t)
	f.fail("/user/tokens/verify", http.StatusUnauthorized, 1000, "Invalid API Token")

	health, err := f.provider(t).Verify(context.Background())
	if !errors.Is(err, edge.ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}
	if !health.Reachable {
		t.Error("reachable = false, but Cloudflare answered")
	}
	if health.Authenticated {
		t.Error("authenticated = true for a rejected token")
	}
}

// The single most consequential field: a locally-managed tunnel ignores every
// write the API makes, so it must never be presented as configurable.
func TestLocallyManagedTunnelsAreNotManageable(t *testing.T) {
	f := newFakeCF(t)
	f.ok("/accounts/acct/cfd_tunnel", `[
		{"id":"t1","name":"local-one","config_src":"local","connections":[{"id":"c1"}]},
		{"id":"t2","name":"remote-one","config_src":"cloudflare","connections":[{"id":"c1"}]}
	]`)

	health, err := f.provider(t).Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	byName := map[string]edge.Tunnel{}
	for _, tn := range health.Tunnels {
		byName[tn.Name] = tn
	}
	if byName["local-one"].Manageable() {
		t.Error("a locally-managed tunnel was reported as manageable")
	}
	if !byName["remote-one"].Manageable() {
		t.Error("a remotely-managed tunnel was reported as unmanageable")
	}
}

func TestVerifyWarnsWhenNoTunnelCanBeManaged(t *testing.T) {
	f := newFakeCF(t)
	f.ok("/accounts/acct/cfd_tunnel", `[{"id":"t1","name":"only","config_src":"local","connections":[]}]`)

	health, err := f.provider(t).Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	// Every scope granted and nothing usable is still a dead end, and saying
	// so is the difference between a five-minute fix and an afternoon.
	if !containsSubstring(health.Warnings, "locally managed") {
		t.Errorf("warnings = %v, want one about every tunnel being locally managed", health.Warnings)
	}
}

// A tunnel with no cloudflared attached serves nothing, which is otherwise
// indistinguishable from a wrong rule.
func TestATunnelWithNoConnectionsIsNotActive(t *testing.T) {
	f := newFakeCF(t)
	f.ok("/accounts/acct/cfd_tunnel", `[{"id":"t1","name":"idle","config_src":"cloudflare","connections":[]}]`)

	tunnels, err := f.provider(t).Tunnels(context.Background())
	if err != nil {
		t.Fatalf("Tunnels() error = %v", err)
	}
	if tunnels[0].Active() {
		t.Error("a tunnel with no connections was reported active")
	}
	if !tunnels[0].Manageable() {
		t.Error("it is still configurable even with nothing attached")
	}
}

func TestIngressIsReadInOrderAndPreservesUnknownSettings(t *testing.T) {
	f := newFakeCF(t)
	f.ok("/accounts/acct/cfd_tunnel/t1/configurations", `{
		"version": 7,
		"config": {
			"ingress": [
				{"hostname":"vm.example.com","service":"http://10.0.0.5:8080",
				 "originRequest":{"noTLSVerify":true,"connectTimeout":30}},
				{"hostname":"app.example.com","path":"/v1","service":"http://10.0.0.9:80"},
				{"service":"http_status:404"}
			],
			"originRequest":{"connectTimeout":10}
		}
	}`)

	cfg, err := f.provider(t).Ingress(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Ingress() error = %v", err)
	}
	if cfg.Version != 7 {
		t.Errorf("version = %d, want 7", cfg.Version)
	}
	if len(cfg.Rules) != 3 {
		t.Fatalf("%d rules, want 3", len(cfg.Rules))
	}
	// Order is semantic — first match wins — so it must survive the round trip.
	if cfg.Rules[0].Hostname != "vm.example.com" || cfg.Rules[1].Path != "/v1" {
		t.Errorf("rules came back reordered: %+v", cfg.Rules)
	}
	if !cfg.Rules[2].IsCatchAll() {
		t.Error("the last rule should be the catch-all")
	}
	// PUB-11: settings the portal does not understand must survive a rewrite,
	// so they have to survive the read first.
	if cfg.Rules[0].Origin["noTLSVerify"] != true {
		t.Errorf("per-rule origin settings were dropped: %+v", cfg.Rules[0].Origin)
	}
	if cfg.Origin["connectTimeout"] == nil {
		t.Error("tunnel-wide origin settings were dropped")
	}
}

func TestIngressNeedsATunnelID(t *testing.T) {
	f := newFakeCF(t)
	if _, err := f.provider(t).Ingress(context.Background(), "  "); !errors.Is(err, edge.ErrInvalidConfig) {
		t.Errorf("got %v, want ErrInvalidConfig", err)
	}
}

// Cloudflare answers 200 with success:false for some refusals, so the status
// code alone is not enough to tell whether a call worked.
func TestASuccessFalseBodyIsAFailure(t *testing.T) {
	f := newFakeCF(t)
	f.handlers["/accounts/acct/cfd_tunnel"] = func() (int, string) {
		return http.StatusOK, `{"success":false,"errors":[{"code":1004,"message":"tunnel not found"}],"result":null}`
	}

	_, err := f.provider(t).Tunnels(context.Background())
	if !errors.Is(err, edge.ErrRefused) {
		t.Fatalf("got %v, want ErrRefused", err)
	}
	if !strings.Contains(err.Error(), "tunnel not found") {
		t.Errorf("error = %q, want Cloudflare's own message", err)
	}
}

// A 5xx means Cloudflare answered. Classifying it as unreachable sends someone
// to debug a network that is fine and marks the call retryable when it is not.
func TestServerErrorsAreRefusalsNotUnreachable(t *testing.T) {
	f := newFakeCF(t)
	f.fail("/accounts/acct/cfd_tunnel", http.StatusInternalServerError, 0, "internal error")

	_, err := f.provider(t).Tunnels(context.Background())
	if !errors.Is(err, edge.ErrRefused) {
		t.Fatalf("got %v, want ErrRefused", err)
	}
	if errors.Is(err, edge.ErrUnreachable) {
		t.Error("a provider that answered was classified unreachable")
	}
	if edge.Retryable(err) {
		t.Error("a refusal was marked retryable")
	}
}

func TestThrottlingIsRetryable(t *testing.T) {
	f := newFakeCF(t)
	f.fail("/accounts/acct/cfd_tunnel", http.StatusTooManyRequests, 971, "rate limited")

	_, err := f.provider(t).Tunnels(context.Background())
	if !errors.Is(err, edge.ErrThrottled) {
		t.Fatalf("got %v, want ErrThrottled", err)
	}
	if !edge.Retryable(err) {
		t.Error("throttling should be retryable")
	}
}

// Deleted tunnels are tombstones, not choices.
func TestDeletedTunnelsAreNotManageable(t *testing.T) {
	f := newFakeCF(t)
	f.ok("/accounts/acct/cfd_tunnel", `[
		{"id":"t1","name":"gone","config_src":"cloudflare","deleted_at":"2026-01-01T00:00:00Z","connections":[]}
	]`)

	tunnels, err := f.provider(t).Tunnels(context.Background())
	if err != nil {
		t.Fatalf("Tunnels() error = %v", err)
	}
	if tunnels[0].DeletedAt == nil {
		t.Fatal("deleted_at was not parsed")
	}
	if tunnels[0].Manageable() || tunnels[0].Active() {
		t.Error("a deleted tunnel was offered as usable")
	}
}

func TestCapabilities(t *testing.T) {
	f := newFakeCF(t)
	p := f.provider(t)
	if !edge.Has(p, edge.CapabilityIngress) || !edge.Has(p, edge.CapabilityDNS) {
		t.Errorf("capabilities = %v", p.Capabilities())
	}
	if edge.Has(p, edge.CapabilityAccess) {
		t.Error("Access is not implemented yet and must not be advertised")
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// Cloudflare answers a malformed token with HTTP 400, not 401, wrapping the
// real cause in an error_chain. Classifying on status alone reports the single
// most common setup mistake as a generic refusal, which sends someone to look
// at their request instead of their credential.
func TestAMalformedTokenIsAnAuthFailureNotARefusal(t *testing.T) {
	f := newFakeCF(t)
	f.handlers["/user/tokens/verify"] = func() (int, string) {
		return http.StatusBadRequest, `{"success":false,"result":null,"errors":[
			{"code":6003,"message":"Invalid request headers","error_chain":[
				{"code":6111,"message":"Invalid format for Authorization header"}]}]}`
	}

	health, err := f.provider(t).Verify(context.Background())

	if !errors.Is(err, edge.ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}
	if errors.Is(err, edge.ErrRefused) {
		t.Error("a rejected credential was classified as a refused request")
	}
	// Cloudflare answered, so this is not a network problem — and saying it is
	// sends someone to debug a firewall that is fine.
	if !health.Reachable {
		t.Error("reachable = false, but Cloudflare answered with a 400")
	}
	// The nested cause is the only part that says what is actually wrong.
	if !strings.Contains(err.Error(), "Invalid format for Authorization header") {
		t.Errorf("error = %q, want the chained cause", err)
	}
}

// A 403 stays a permission problem: that is a scope to grant, not a token to
// replace, and they are fixed in different places.
func TestForbiddenStaysAPermissionProblem(t *testing.T) {
	f := newFakeCF(t)
	f.fail("/accounts/acct/cfd_tunnel", http.StatusForbidden, 10000, "Authentication error")

	_, err := f.provider(t).Tunnels(context.Background())
	if !errors.Is(err, edge.ErrPermission) {
		t.Fatalf("got %v, want ErrPermission", err)
	}
}
