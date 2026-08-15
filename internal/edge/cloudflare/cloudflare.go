// Package cloudflare implements the edge port against Cloudflare Tunnel.
//
// Scope for now is read-only: verify a credential, list tunnels, read a
// tunnel's routing table. Writing comes later and behind the invariants in
// internal/domain/publish (docs/28 §28.11).
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/freezxp/proxui/internal/edge"
)

const (
	defaultBaseURL = "https://api.cloudflare.com/client/v4"
	defaultTimeout = 20 * time.Second
	// Cloudflare's global limit is 1200 requests per five minutes. Four per
	// second leaves plenty of headroom while staying far away from it.
	defaultRPS   = 4
	defaultBurst = 4
)

// Provider talks to the Cloudflare API.
type Provider struct {
	baseURL string
	token   string
	account string
	http    *http.Client
	limiter *rate.Limiter
}

// New builds a provider. The token is held in memory for the life of the
// value and is never logged.
func New(creds edge.Credentials, opts edge.Options) (*Provider, error) {
	if strings.TrimSpace(creds.Token) == "" {
		return nil, edge.Errorf(edge.ErrInvalidConfig, "config", "an API token is required")
	}
	if strings.TrimSpace(creds.AccountID) == "" {
		return nil, edge.Errorf(edge.ErrInvalidConfig, "config", "an account id is required")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	rps := opts.RequestsPerSec
	if rps <= 0 {
		rps = defaultRPS
	}

	return &Provider{
		baseURL: defaultBaseURL,
		token:   creds.Token,
		account: creds.AccountID,
		http:    &http.Client{Timeout: timeout},
		limiter: rate.NewLimiter(rate.Limit(rps), defaultBurst),
	}, nil
}

// Capabilities reports what this provider can do. DNS and Access are declared
// because the provider will implement them; the methods land with the sprints
// that need them.
func (p *Provider) Capabilities() []edge.Capability {
	return []edge.Capability{edge.CapabilityIngress, edge.CapabilityDNS}
}

// Close releases resources. Nothing to release yet; the method exists so the
// port does not have to change when something does.
func (p *Provider) Close() error { return nil }

// envelope is Cloudflare's standard response wrapper. Every endpoint returns
// it, including failures, and `success` is not always consistent with the HTTP
// status — so both are checked.
type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	// Chain carries the specific cause under a generic wrapper. A malformed
	// token arrives as 6003 "Invalid request headers" with 6111 "Invalid
	// format for Authorization header" nested inside, and only the inner one
	// says what is actually wrong.
	Chain []apiError `json:"error_chain"`
}

// authErrorCodes are Cloudflare error codes that mean "your credential is the
// problem", whatever HTTP status they arrive with.
//
// This exists because a malformed token comes back as **HTTP 400**, not 401.
// Classifying on status alone reports the single most common setup mistake —
// a truncated or mistyped token — as a generic refusal, sending someone to
// look at their request instead of their credential.
var authErrorCodes = map[int]bool{
	6003:  true, // invalid request headers, wrapping 6111
	6111:  true, // invalid format for Authorization header
	9109:  true, // invalid access token
	10000: true, // authentication error
}

// hasAuthCode reports whether any error, at any depth, blames the credential.
func hasAuthCode(errs []apiError) bool {
	for _, e := range errs {
		if authErrorCodes[e.Code] || hasAuthCode(e.Chain) {
			return true
		}
	}
	return false
}

// Verify checks the credential and reports what it can reach.
//
// Cloudflare does not let a token introspect its own permission list — the
// endpoint that would tell you needs credentials the portal does not have. So
// scopes are established **functionally**: call the endpoints the feature
// depends on and see which are refused. That is less elegant than reading a
// manifest and considerably more truthful, since it tests the access the
// feature will actually use rather than the access someone intended to grant.
func (p *Provider) Verify(ctx context.Context) (edge.Health, error) {
	var health edge.Health

	// 1. Is the token live at all?
	var verify struct {
		Status string `json:"status"`
	}
	if err := p.get(ctx, "/user/tokens/verify", &verify); err != nil {
		// Reaching Cloudflare and being rejected still proves reachability,
		// which separates a firewall problem from a bad token.
		if isClass(err, edge.ErrAuth, edge.ErrPermission) {
			health.Reachable = true
		}
		return health, err
	}
	health.Reachable = true
	health.Authenticated = true
	if verify.Status != "" && verify.Status != "active" {
		health.Warnings = append(health.Warnings,
			fmt.Sprintf("the token's status is %q rather than active", verify.Status))
	}

	// 2. Can it see tunnels? Without this nothing else matters.
	tunnels, err := p.Tunnels(ctx)
	switch {
	case err == nil:
		health.Tunnels = tunnels
	case isClass(err, edge.ErrPermission, edge.ErrAuth):
		health.MissingScopes = append(health.MissingScopes, edge.ScopeGap{
			Scope:  "Cloudflare Tunnel: Read/Edit (account)",
			Blocks: "listing tunnels and reading or changing their routing rules — nothing works without it",
		})
	default:
		return health, err
	}

	// 3. Can it write DNS? Publishing needs a CNAME as well as a rule, and
	// discovering that at publish time means a half-published app.
	if err := p.get(ctx, "/zones?per_page=1", nil); err != nil {
		if !isClass(err, edge.ErrPermission, edge.ErrAuth) {
			return health, err
		}
		health.MissingScopes = append(health.MissingScopes, edge.ScopeGap{
			Scope:  "DNS: Edit (zone)",
			Blocks: "creating the DNS record a published hostname needs; ingress rules alone leave the name unresolvable",
		})
	}

	// 4. Say something useful about what was found, since a token with every
	// scope and no manageable tunnel is still a dead end.
	manageable := 0
	for _, t := range health.Tunnels {
		if t.Manageable() {
			manageable++
		}
	}
	if len(health.Tunnels) > 0 && manageable == 0 {
		health.Warnings = append(health.Warnings,
			"every tunnel on this account is locally managed, so none of them can be configured through the API")
	}
	if len(health.MissingScopes) > 0 {
		health.Warnings = append(health.Warnings,
			"the portal will run with reduced functionality until these permissions are granted")
	}
	return health, nil
}

// tunnelDTO is Cloudflare's tunnel representation.
type tunnelDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ConfigSrc string `json:"config_src"`
	CreatedAt string `json:"created_at"`
	DeletedAt string `json:"deleted_at"`
	// Connections is empty for a tunnel with nothing running. Its length is
	// how many cloudflared instances are attached.
	Connections []struct {
		ID string `json:"id"`
	} `json:"connections"`
}

// Tunnels lists the account's tunnels.
func (p *Provider) Tunnels(ctx context.Context) ([]edge.Tunnel, error) {
	var dtos []tunnelDTO
	// is_deleted=false asks Cloudflare to omit tombstones; deleted_at is still
	// checked below, because relying on a query parameter to enforce an
	// invariant is how tombstones end up in a dropdown.
	if err := p.get(ctx, "/accounts/"+p.account+"/cfd_tunnel?is_deleted=false", &dtos); err != nil {
		return nil, err
	}

	out := make([]edge.Tunnel, 0, len(dtos))
	for _, d := range dtos {
		t := edge.Tunnel{
			ID:   d.ID,
			Name: d.Name,
			// "cloudflare" means the configuration lives in Cloudflare and
			// the API can write it. "local" means cloudflared reads a file
			// and ignores us entirely.
			RemotelyManaged: d.ConfigSrc == "cloudflare",
			Connections:     len(d.Connections),
		}
		if ts, err := time.Parse(time.RFC3339, d.CreatedAt); err == nil {
			t.CreatedAt = ts
		}
		if d.DeletedAt != "" {
			if ts, err := time.Parse(time.RFC3339, d.DeletedAt); err == nil {
				t.DeletedAt = &ts
			}
		}
		out = append(out, t)
	}
	// Stable order: the API's is not guaranteed, and a list that reshuffles
	// between refreshes is one nobody trusts.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// configDTO is the tunnel configuration response.
type configDTO struct {
	Version int `json:"version"`
	Config  struct {
		Ingress []struct {
			Hostname      string         `json:"hostname"`
			Path          string         `json:"path"`
			Service       string         `json:"service"`
			OriginRequest map[string]any `json:"originRequest"`
		} `json:"ingress"`
		OriginRequest map[string]any `json:"originRequest"`
	} `json:"config"`
}

// Ingress reads a tunnel's routing table.
func (p *Provider) Ingress(ctx context.Context, tunnelID string) (edge.Config, error) {
	if strings.TrimSpace(tunnelID) == "" {
		return edge.Config{}, edge.Errorf(edge.ErrInvalidConfig, "get_ingress", "a tunnel id is required")
	}

	var dto configDTO
	path := "/accounts/" + p.account + "/cfd_tunnel/" + tunnelID + "/configurations"
	if err := p.get(ctx, path, &dto); err != nil {
		return edge.Config{}, err
	}

	cfg := edge.Config{Version: dto.Version, Origin: dto.Config.OriginRequest}
	for _, r := range dto.Config.Ingress {
		cfg.Rules = append(cfg.Rules, edge.Rule{
			Hostname: r.Hostname,
			Path:     r.Path,
			Service:  r.Service,
			// Kept verbatim so a rule this portal did not create survives a
			// rewrite unchanged (PUB-11).
			Origin: r.OriginRequest,
		})
	}
	return cfg, nil
}

// --- transport ------------------------------------------------------------

func (p *Provider) get(ctx context.Context, path string, out any) error {
	return p.do(ctx, http.MethodGet, path, nil, out)
}

func (p *Provider) do(ctx context.Context, method, path string, body, out any) error {
	if err := p.limiter.Wait(ctx); err != nil {
		return edge.Wrap(edge.ErrUnreachable, opOf(method, path), err)
	}

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return edge.Wrap(edge.ErrInvalidConfig, opOf(method, path), err)
		}
		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, payload)
	if err != nil {
		return edge.Wrap(edge.ErrInvalidConfig, opOf(method, path), err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return edge.Wrap(edge.ErrUnreachable, opOf(method, path), err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}()

	var env envelope
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&env)

	if err := classify(method, path, resp.StatusCode, env); err != nil {
		return err
	}
	if decodeErr != nil {
		return edge.Wrap(edge.ErrRefused, opOf(method, path), fmt.Errorf("decode response: %w", decodeErr))
	}
	if out == nil || len(env.Result) == 0 || string(env.Result) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return edge.Wrap(edge.ErrRefused, opOf(method, path), fmt.Errorf("decode result: %w", err))
	}
	return nil
}

// classify maps a Cloudflare response onto an error class.
//
// Two things make this less obvious than it looks. Cloudflare can return HTTP
// 200 with `success: false`, so the status alone is not enough. And a 5xx
// means Cloudflare answered — the same distinction internal/connector had to
// learn after a Proxmox 500 spent a release being reported as an unreachable
// cluster.
func classify(method, path string, status int, env envelope) error {
	op := opOf(method, path)
	reason := describe(env.Errors)

	switch {
	case status == http.StatusUnauthorized:
		return edge.Errorf(edge.ErrAuth, op,
			"the API token was rejected (HTTP 401)%s", reason)
	// Checked before the generic status arms: Cloudflare answers 400 for a
	// malformed token, and "refused the request" would be true but useless.
	case status != http.StatusForbidden && hasAuthCode(env.Errors):
		return edge.Errorf(edge.ErrAuth, op, "the API token was rejected%s", reason)
	case status == http.StatusForbidden:
		return edge.Errorf(edge.ErrPermission, op,
			"the API token lacks the permission for %s (HTTP 403)%s", path, reason)
	case status == http.StatusTooManyRequests:
		return edge.Errorf(edge.ErrThrottled, op, "Cloudflare rate limited the request%s", reason)
	case status == http.StatusNotFound:
		return edge.Errorf(edge.ErrRefused, op, "%s was not found (HTTP 404)%s", path, reason)
	case status >= 500:
		return edge.Errorf(edge.ErrRefused, op, "Cloudflare returned HTTP %d%s", status, reason)
	case status >= 300:
		return edge.Errorf(edge.ErrRefused, op, "unexpected HTTP %d%s", status, reason)
	case !env.Success && len(env.Errors) > 0:
		// 200 with success:false. Cloudflare's own error code is the only
		// thing distinguishing these, so it is carried through.
		return edge.Errorf(edge.ErrRefused, op, "Cloudflare refused the request%s", reason)
	}
	return nil
}

// describe renders Cloudflare's error list, sorted so the same failure logs
// identically every time rather than in whatever order it arrived.
func describe(errs []apiError) string {
	if len(errs) == 0 {
		return ""
	}
	parts := collectMessages(errs)
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return ": " + strings.Join(parts, "; ")
}

// collectMessages flattens Cloudflare's error tree into readable parts.
func collectMessages(errs []apiError) []string {
	var parts []string
	for _, e := range errs {
		if msg := strings.TrimSpace(e.Message); msg != "" {
			if e.Code != 0 {
				parts = append(parts, fmt.Sprintf("%s (code %d)", msg, e.Code))
			} else {
				parts = append(parts, msg)
			}
		}
		parts = append(parts, collectMessages(e.Chain)...)
	}
	return parts
}

// opOf turns a method and path into a short label for errors and metrics,
// without leaking account or tunnel ids into label cardinality.
//
// The method is part of it because reading and writing a routing table use the
// same path, and an error labelled "get_ingress" for a failed PUT sends whoever
// reads it looking in the wrong direction.
func opOf(method, path string) string {
	switch {
	case strings.Contains(path, "/configurations"):
		if method == http.MethodPut {
			return "put_ingress"
		}
		return "get_ingress"
	case strings.Contains(path, "/cfd_tunnel"):
		return "list_tunnels"
	case strings.Contains(path, "/dns_records"):
		switch method {
		case http.MethodPost:
			return "create_dns"
		case http.MethodDelete:
			return "delete_dns"
		}
		return "find_dns"
	case strings.HasPrefix(path, "/zones"):
		return "list_zones"
	case strings.HasPrefix(path, "/user/tokens"):
		return "verify_token"
	default:
		return "request"
	}
}

// isClass reports whether err belongs to any of the given classes.
func isClass(err error, classes ...error) bool {
	for _, c := range classes {
		if errors.Is(err, c) {
			return true
		}
	}
	return false
}
