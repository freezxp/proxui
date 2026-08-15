package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

// --- write path ----------------------------------------------------------

// recordingCF captures what was sent, so a test can assert on the request body
// rather than only on the response handling.
type recordingCF struct {
	*fakeCF
	method string
	body   string
}

func newRecordingCF(t *testing.T, prefix string, status int, result string) *recordingCF {
	t.Helper()
	f := newFakeCF(t)
	rec := &recordingCF{fakeCF: f}
	f.handlers[prefix] = func() (int, string) {
		return status, `{"success":true,"errors":[],"result":` + result + `}`
	}
	// Wrap the server so the request body is captured too.
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.method = r.Method
		if b, err := io.ReadAll(r.Body); err == nil {
			rec.body = string(b)
		}
		if strings.HasPrefix(r.URL.Path, prefix) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"success":true,"errors":[],"result":` + result + `}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":7003,"message":"no route"}]}`))
	})
	return rec
}

// Settings the portal does not understand must be written back exactly as they
// were read, or a rewrite silently breaks apps the portal did not create.
func TestReplaceIngressPreservesUnknownSettings(t *testing.T) {
	rec := newRecordingCF(t, "/accounts/acct/cfd_tunnel/t1/configurations", http.StatusOK, `{}`)

	err := rec.provider(t).ReplaceIngress(context.Background(), "t1", edge.Config{
		Rules: []edge.Rule{
			{Hostname: "app.example.com", Service: "http://10.0.0.9:80",
				Origin: map[string]any{"noTLSVerify": true, "http2Origin": true}},
			{Service: "http_status:404"},
		},
		Origin: map[string]any{"connectTimeout": 30},
	})
	if err != nil {
		t.Fatalf("ReplaceIngress() error = %v", err)
	}
	if rec.method != http.MethodPut {
		t.Errorf("method = %s, want PUT", rec.method)
	}
	for _, want := range []string{"noTLSVerify", "http2Origin", "connectTimeout", "http_status:404"} {
		if !strings.Contains(rec.body, want) {
			t.Errorf("request body dropped %q: %s", want, rec.body)
		}
	}
	// The catch-all carries no hostname, and sending an empty one would make
	// Cloudflare treat it as a hostname rule that matches nothing.
	if strings.Contains(rec.body, `"hostname":""`) {
		t.Errorf("an empty hostname was sent: %s", rec.body)
	}
}

// An empty table would mean the tunnel serves nothing. Refused here so the
// error names the cause rather than echoing Cloudflare's 400.
func TestReplaceIngressRefusesAnEmptyTable(t *testing.T) {
	f := newFakeCF(t)
	err := f.provider(t).ReplaceIngress(context.Background(), "t1", edge.Config{})
	if !errors.Is(err, edge.ErrInvalidConfig) {
		t.Errorf("got %v, want ErrInvalidConfig", err)
	}
}

func TestReplaceIngressNeedsATunnelID(t *testing.T) {
	f := newFakeCF(t)
	err := f.provider(t).ReplaceIngress(context.Background(), "  ", edge.Config{
		Rules: []edge.Rule{{Service: "http_status:404"}},
	})
	if !errors.Is(err, edge.ErrInvalidConfig) {
		t.Errorf("got %v, want ErrInvalidConfig", err)
	}
}

func TestCreateTunnelDNSPointsAtTheTunnelAndIsProxied(t *testing.T) {
	rec := newRecordingCF(t, "/zones/z1/dns_records", http.StatusOK,
		`{"id":"rec1","name":"app.example.com","type":"CNAME","content":"t1.cfargotunnel.com","proxied":true}`)

	got, err := rec.provider(t).CreateTunnelDNS(context.Background(), "z1", "app.example.com", "t1")
	if err != nil {
		t.Fatalf("CreateTunnelDNS() error = %v", err)
	}
	if got.ID != "rec1" || got.Content != "t1.cfargotunnel.com" {
		t.Errorf("record = %+v", got)
	}
	if !strings.Contains(rec.body, `"content":"t1.cfargotunnel.com"`) {
		t.Errorf("body did not point at the tunnel: %s", rec.body)
	}
	// Unproxied would publish the cfargotunnel address directly and the tunnel
	// would never be used, which looks like a DNS problem and is not.
	if !strings.Contains(rec.body, `"proxied":true`) {
		t.Errorf("the record was not proxied: %s", rec.body)
	}
}

func TestFindDNSRecordReportsAbsence(t *testing.T) {
	f := newFakeCF(t)
	f.ok("/zones/z1/dns_records", `[]`)

	_, found, err := f.provider(t).FindDNSRecord(context.Background(), "z1", "app.example.com")
	if err != nil {
		t.Fatalf("FindDNSRecord() error = %v", err)
	}
	if found {
		t.Error("an empty result reported a record")
	}
}

func TestFindDNSRecordReturnsWhatIsThere(t *testing.T) {
	f := newFakeCF(t)
	f.ok("/zones/z1/dns_records",
		`[{"id":"rec9","name":"app.example.com","type":"CNAME","content":"elsewhere.example.com","proxied":false}]`)

	got, found, err := f.provider(t).FindDNSRecord(context.Background(), "z1", "app.example.com")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	// Enough detail to tell "somebody else's record" from "ours", which is
	// what stops the portal deleting a CNAME it did not create.
	if got.ID != "rec9" || got.Content != "elsewhere.example.com" || got.Proxied {
		t.Errorf("record = %+v", got)
	}
}

func TestDeleteDNSRecordValidatesItsArguments(t *testing.T) {
	f := newFakeCF(t)
	p := f.provider(t)
	for _, c := range [][2]string{{"", "rec1"}, {"z1", ""}, {" ", " "}} {
		if err := p.DeleteDNSRecord(context.Background(), c[0], c[1]); !errors.Is(err, edge.ErrInvalidConfig) {
			t.Errorf("DeleteDNSRecord(%q,%q) = %v, want ErrInvalidConfig", c[0], c[1], err)
		}
	}
}

func TestZonesAreSortedAndComplete(t *testing.T) {
	f := newFakeCF(t)
	f.ok("/zones", `[{"id":"z2","name":"b.example"},{"id":"z1","name":"a.example"}]`)

	got, err := f.provider(t).Zones(context.Background())
	if err != nil {
		t.Fatalf("Zones() error = %v", err)
	}
	if len(got) != 2 || got[0].Name != "a.example" {
		t.Errorf("zones = %+v, want sorted by name", got)
	}
}

// A write refused for want of a permission has to say so as a permission
// problem: the fix is a token scope, not a retry.
func TestAWriteRefusedForPermissionIsClassifiedAsSuch(t *testing.T) {
	f := newFakeCF(t)
	f.fail("/accounts/acct/cfd_tunnel/t1/configurations", http.StatusForbidden, 10000, "Authentication error")

	err := f.provider(t).ReplaceIngress(context.Background(), "t1", edge.Config{
		Rules: []edge.Rule{{Service: "http_status:404"}},
	})
	if !errors.Is(err, edge.ErrPermission) {
		t.Fatalf("got %v, want ErrPermission", err)
	}
	if edge.Retryable(err) {
		t.Error("a permission failure was marked retryable")
	}
}

// Reading and writing a routing table use the same path, so the label has to
// come from the method too — an error saying "get_ingress" for a failed PUT
// sends whoever reads it looking in the wrong direction.
func TestTheOperationLabelDistinguishesReadsFromWrites(t *testing.T) {
	cases := []struct {
		method, path, want string
	}{
		{http.MethodGet, "/accounts/a/cfd_tunnel/t1/configurations", "get_ingress"},
		{http.MethodPut, "/accounts/a/cfd_tunnel/t1/configurations", "put_ingress"},
		{http.MethodGet, "/zones/z1/dns_records?name=x", "find_dns"},
		{http.MethodPost, "/zones/z1/dns_records", "create_dns"},
		{http.MethodDelete, "/zones/z1/dns_records/rec1", "delete_dns"},
		{http.MethodGet, "/accounts/a/cfd_tunnel", "list_tunnels"},
		{http.MethodGet, "/user/tokens/verify", "verify_token"},
	}
	for _, c := range cases {
		if got := opOf(c.method, c.path); got != c.want {
			t.Errorf("opOf(%s %s) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

// The live account produced exactly this: a token that reads perfectly well,
// refused on a write with 401 "Not authorized" rather than 403.
func TestAWriteRefusedWith401IsStillAnAuthClass(t *testing.T) {
	f := newFakeCF(t)
	f.fail("/accounts/acct/cfd_tunnel/t1/configurations", http.StatusUnauthorized, 1001, "Not authorized")

	err := f.provider(t).ReplaceIngress(context.Background(), "t1", edge.Config{
		Rules: []edge.Rule{{Service: "http_status:404"}},
	})
	if !errors.Is(err, edge.ErrAuth) {
		t.Fatalf("got %v, want ErrAuth", err)
	}
	// The label must say it was a write, which is what makes the message
	// actionable at the transport layer.
	if !strings.Contains(err.Error(), "put_ingress") {
		t.Errorf("error = %q, want it labelled as a write", err)
	}
}
