// Package proxmox implements the connector contract for Proxmox VE 8.x.
//
// Authentication uses API tokens (PVEAPIToken), so there is no ticket
// lifecycle to manage for REST calls and no password is ever stored. Console
// sessions still mint short-lived per-session VNC tickets.
package proxmox

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/freezxp/proxui/internal/connector"
)

const (
	apiPrefix          = "/api2/json"
	defaultTimeout     = 30 * time.Second
	defaultRPS         = 10 // be a polite API citizen; Proxmox is not a CDN
	defaultBurst       = 5
	defaultConcurrency = 8
)

// target is one address the platform answers on, paired with the TLS trust
// that belongs to that address. Each carries its own http.Client because the
// members of a cluster present different certificates: under a pinned policy
// the trust differs per address, so the transport has to as well.
type target struct {
	base *url.URL
	http *http.Client
}

func (t *target) address() string { return t.base.Host }

// client is a thin Proxmox API client: auth, TLS policy, rate limiting and
// error classification. Endpoint knowledge lives in the sibling files.
//
// It holds every address the platform answers on rather than one, because
// Proxmox serves the same clustered API from every member and a single
// configured endpoint made one node's power switch an outage of the whole
// portal (ADR 0009). targets[0] is always the configured endpoint.
type client struct {
	targets []*target
	// pref indexes the target that answered last. Reads are lock-free because
	// the sync engine issues bounded-concurrency requests through one client,
	// and a stale read costs at most one extra failover hop.
	pref    atomic.Int64
	limiter *rate.Limiter
	// authHeader is precomputed; it is never logged.
	authHeader  string
	concurrency int
	// timeout is the per-request budget, kept so failover can tell whether
	// there is room for another attempt before the caller's deadline.
	timeout time.Duration
}

func newClient(cfg connector.Config, creds connector.Credentials, opts connector.Options) (*client, error) {
	if cfg.Endpoint == "" {
		return nil, connector.Errorf(connector.ErrInvalidConfig, "config", "endpoint is required")
	}
	raw := cfg.Endpoint
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, connector.Errorf(connector.ErrInvalidConfig, "config", "endpoint %q is not a URL", cfg.Endpoint)
	}
	if base.Scheme != "https" && base.Scheme != "http" {
		return nil, connector.Errorf(connector.ErrInvalidConfig, "config", "endpoint scheme %q is not http(s)", base.Scheme)
	}
	if base.Port() == "" && base.Scheme == "https" {
		base.Host = net.JoinHostPort(base.Hostname(), "8006")
	}

	auth, err := authHeader(creds)
	if err != nil {
		return nil, err
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	rps := opts.RequestsPerSec
	if rps <= 0 {
		rps = defaultRPS
	}
	concurrency := opts.MaxConcurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}

	primary, err := newTarget(base, cfg.TLS, timeout, concurrency)
	if err != nil {
		return nil, err
	}
	targets := []*target{primary}
	for _, ep := range cfg.Failover {
		t, err := failoverTarget(ep, cfg.TLS, timeout, concurrency)
		if err != nil || t == nil {
			// A candidate that cannot be trusted or parsed is dropped, not
			// fatal: the configured endpoint still works, and refusing to
			// build the connector would turn a stale discovered address into
			// an outage of its own.
			continue
		}
		if t.address() == primary.address() {
			continue
		}
		targets = append(targets, t)
	}

	return &client{
		targets:     targets,
		limiter:     rate.NewLimiter(rate.Limit(rps), defaultBurst),
		authHeader:  auth,
		concurrency: concurrency,
		timeout:     timeout,
	}, nil
}

// newTarget builds one address's transport under a TLS policy.
func newTarget(base *url.URL, policy connector.TLSPolicy, timeout time.Duration, concurrency int) (*target, error) {
	tlsCfg, err := tlsConfig(policy, base.Hostname())
	if err != nil {
		return nil, err
	}
	return &target{
		base: base,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig:     tlsCfg,
				MaxIdleConnsPerHost: concurrency,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}, nil
}

// failoverTarget builds a discovered cluster member's transport.
//
// Under a pinned policy the member's own fingerprint replaces the configured
// one, because each node presents its own certificate. A member whose
// fingerprint is unknown is dropped rather than trusted loosely: the point of
// pinning is that a self-signed cluster is trusted exactly, and an outage is
// not a reason to trust more (ADR 0009). Every other mode already covers the
// whole cluster through system roots or a cluster CA, so it passes through.
func failoverTarget(ep connector.Endpoint, policy connector.TLSPolicy, timeout time.Duration, concurrency int) (*target, error) {
	if ep.Address == "" {
		return nil, nil
	}
	raw := ep.Address
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if base.Scheme != "https" && base.Scheme != "http" {
		return nil, nil
	}
	if base.Port() == "" && base.Scheme == "https" {
		base.Host = net.JoinHostPort(base.Hostname(), "8006")
	}
	if policy.Mode == connector.TLSFingerprint {
		if ep.Fingerprint == "" {
			return nil, nil
		}
		policy.Fingerprint = ep.Fingerprint
	}
	return newTarget(base, policy, timeout, concurrency)
}

// current returns the target requests are being sent to. Callers that build a
// URL of their own — the console dialer, the standalone node fallback — must
// use this rather than a remembered base, or they will address a node the
// client has already failed away from.
func (c *client) current() *target { return c.targets[c.prefIndex()] }

func (c *client) prefIndex() int {
	i := int(c.pref.Load())
	if i < 0 || i >= len(c.targets) {
		return 0
	}
	return i
}

// order lists target indexes to try: the preferred one, then the rest in
// configured order. The configured endpoint is therefore tried first on a cold
// client and second-at-worst on a warm one.
func (c *client) order() []int {
	pref := c.prefIndex()
	out := make([]int, 0, len(c.targets))
	out = append(out, pref)
	for i := range c.targets {
		if i != pref {
			out = append(out, i)
		}
	}
	return out
}

// minAttemptBudget is the least time worth starting another attempt with.
// Below it the request would be cut off before a handshake could finish, so
// trying costs a wasted connection and tells us nothing.
const minAttemptBudget = 250 * time.Millisecond

// roomForAnother reports whether the caller's deadline leaves time to try
// another address.
//
// The bar is deliberately low, because the thing it once guarded against
// cannot happen: an attempt is bounded by the caller's context as well as by
// the client timeout, so a member that hangs is cut off by the deadline rather
// than allowed to overrun the cycle that asked.
//
// This originally demanded a full client timeout of headroom, which read as
// prudence and was in fact a silent disabling of failover in the place it was
// needed most. The health probe runs under a 30-second task deadline
// (jobs.NewSyncHealthTask) and the client timeout is also 30 seconds, so there
// was never room for a second address: with the configured endpoint dead, every
// inventory sync failed over and succeeded while every health probe reported
// the platform unreachable, and the platform flapped between healthy and
// unreachable once a minute. Verified against a live cluster, not a test.
func (c *client) roomForAnother(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Until(deadline) > minAttemptBudget
}

// authHeader builds the PVEAPIToken header. Token IDs look like
// user@realm!tokenname.
func authHeader(creds connector.Credentials) (string, error) {
	switch creds.Kind {
	case "", "api_token":
		if creds.TokenID == "" || creds.Secret == "" {
			return "", connector.Errorf(connector.ErrInvalidConfig, "config",
				"api token requires both a token id (user@realm!name) and a secret")
		}
		if !strings.Contains(creds.TokenID, "!") || !strings.Contains(creds.TokenID, "@") {
			return "", connector.Errorf(connector.ErrInvalidConfig, "config",
				"token id must look like user@realm!tokenname")
		}
		return "PVEAPIToken=" + creds.TokenID + "=" + creds.Secret, nil
	default:
		return "", connector.Errorf(connector.ErrInvalidConfig, "config",
			"credential kind %q is not supported; use an api token", creds.Kind)
	}
}

// tlsConfig turns the operator's TLS policy into a client configuration.
// Proxmox clusters are usually self-signed, so fingerprint pinning is a
// first-class option rather than an afterthought.
func tlsConfig(policy connector.TLSPolicy, host string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}

	switch policy.Mode {
	case "", connector.TLSVerify:
		return cfg, nil

	case connector.TLSCustomCA:
		if policy.CAPEM == "" {
			return nil, connector.Errorf(connector.ErrInvalidConfig, "config", "custom_ca mode requires a CA bundle")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(policy.CAPEM)) {
			return nil, connector.Errorf(connector.ErrInvalidConfig, "config", "CA bundle contains no usable certificate")
		}
		cfg.RootCAs = pool
		return cfg, nil

	case connector.TLSFingerprint:
		want := normalizeFingerprint(policy.Fingerprint)
		if len(want) != 64 {
			return nil, connector.Errorf(connector.ErrInvalidConfig, "config",
				"fingerprint must be a SHA-256 hex digest (64 hex characters)")
		}
		// Skip chain verification but pin the leaf certificate: this trusts
		// exactly one certificate rather than trusting nothing, which is what
		// makes self-signed clusters safe to talk to.
		cfg.InsecureSkipVerify = true
		cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return connector.Errorf(connector.ErrUnreachable, "tls", "server presented no certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			if got := hex.EncodeToString(sum[:]); got != want {
				return connector.Errorf(connector.ErrUnreachable, "tls",
					"certificate fingerprint mismatch: pinned %s but server presented %s", want, got)
			}
			return nil
		}
		return cfg, nil

	case connector.TLSInsecure:
		// Allowed, warned about in the UI and audited on every enable.
		cfg.InsecureSkipVerify = true
		return cfg, nil

	default:
		return nil, connector.Errorf(connector.ErrInvalidConfig, "config", "unknown TLS mode %q", policy.Mode)
	}
}

func normalizeFingerprint(fp string) string {
	return strings.ToLower(strings.NewReplacer(":", "", " ", "", "-", "").Replace(strings.TrimSpace(fp)))
}

// envelope is the standard Proxmox response wrapper.
type envelope struct {
	Data json.RawMessage `json:"data"`
}

// get performs a GET and decodes data into out.
func (c *client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, nil, out)
}

// getQuery performs a GET with query parameters. Query values must be passed
// separately rather than baked into path: path segments are escaped, so an
// embedded "?" would become "%3F" and the parameters would be lost.
func (c *client) getQuery(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, out)
}

// post performs a form POST and decodes data into out.
func (c *client) post(ctx context.Context, path string, form url.Values, out any) error {
	return c.do(ctx, http.MethodPost, path, nil, form, out)
}

// do performs one API call, failing over to another cluster member when — and
// only when — the address it used could not be reached.
//
// Every other failure class is returned from the first attempt untouched. The
// token is cluster-wide, so a 401 from one member is a 401 from all of them,
// and trying each in turn would multiply one clear, actionable failure into
// several identical ones while delaying the alert an administrator needs
// (ADR 0009). A malformed response is not a transport failure either: the
// platform answered, and asking a different one is unlikely to change what it
// says.
func (c *client) do(ctx context.Context, method, path string, query, form url.Values, out any) error {
	var firstErr error
	tried := make([]string, 0, len(c.targets))

	for attempt, idx := range c.order() {
		if attempt > 0 && !c.roomForAnother(ctx) {
			break
		}
		t := c.targets[idx]
		tried = append(tried, t.address())

		transport, err := c.attempt(ctx, t, method, path, query, form, out)
		if err == nil {
			// Preference is sticky: whatever answered keeps answering until it
			// stops, rather than drifting back to a configured endpoint that is
			// still down.
			c.pref.Store(int64(idx))
			return nil
		}
		if !transport {
			return err
		}
		if firstErr == nil {
			firstErr = err
		}
	}

	if firstErr == nil {
		// Unreachable with nothing attempted: the deadline was already spent.
		return connector.Errorf(connector.ErrUnreachable, opFromPath(path),
			"no time left to try any endpoint")
	}
	if len(tried) < 2 {
		return firstErr
	}
	return connector.Wrap(connector.ErrUnreachable, opFromPath(path),
		fmt.Errorf("no endpoint answered (tried %s): %w", strings.Join(tried, ", "), firstErr))
}

// attempt performs one request against one address.
//
// It reports separately whether the failure was at the transport layer, which
// is the only kind a different address could plausibly answer differently. The
// returned error keeps its existing classification either way, so the sync
// engine's retry and circuit-breaker decisions are unchanged.
func (c *client) attempt(ctx context.Context, t *target, method, path string, query, form url.Values, out any) (transport bool, err error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return false, connector.Wrap(connector.ErrUnreachable, opFromPath(path), err)
	}

	target := t.base.JoinPath(apiPrefix + path)
	if len(query) > 0 {
		target.RawQuery = query.Encode()
	}
	endpoint := target.String()
	// Rebuilt per attempt: a reader is consumed by the request that failed.
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return false, connector.Wrap(connector.ErrInvalidConfig, opFromPath(path), err)
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := t.http.Do(req)
	if err != nil {
		return true, classifyTransportError(path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}()

	if err := classifyStatus(path, resp); err != nil {
		return false, err
	}
	if out == nil {
		return false, nil
	}

	var env envelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&env); err != nil {
		return false, connector.Wrap(connector.ErrUnreachable, opFromPath(path), fmt.Errorf("decode response: %w", err))
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return false, nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return false, connector.Wrap(connector.ErrUnreachable, opFromPath(path), fmt.Errorf("decode data: %w", err))
	}
	return false, nil
}

// classifyStatus maps HTTP status codes onto connector error classes, which is
// what lets the sync engine tell "retry later" from "an admin must fix this".
func classifyStatus(path string, resp *http.Response) error {
	switch {
	case resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return connector.Errorf(connector.ErrAuth, opFromPath(path),
			"token rejected (HTTP 401); check the token id and secret, and that the token is not expired")
	case resp.StatusCode == http.StatusForbidden:
		return connector.Errorf(connector.ErrPermission, opFromPath(path),
			"token lacks privileges for %s (HTTP 403)", path)
	case resp.StatusCode == http.StatusTooManyRequests:
		return connector.Errorf(connector.ErrThrottled, opFromPath(path), "platform rate limited the request")
	case resp.StatusCode == http.StatusNotFound:
		return connector.Errorf(connector.ErrNotSupported, opFromPath(path),
			"endpoint %s not found (HTTP 404); the platform version may not support it", path)
	case resp.StatusCode >= 500:
		// Proxmox answers 500 for an operation it understood and would not
		// perform — "VM is already running", "config lock held", "storage
		// full" — as well as for genuine internal faults. It reached us either
		// way, so this is never ErrUnreachable, and its own words are far more
		// use than the status code.
		return connector.Errorf(connector.ErrRefused, opFromPath(path),
			"platform refused the request (HTTP %d)%s", resp.StatusCode, platformReason(resp))
	default:
		return connector.Errorf(connector.ErrRefused, opFromPath(path),
			"unexpected HTTP %d%s", resp.StatusCode, platformReason(resp))
	}
}

// platformReason extracts Proxmox's explanation of a failure.
//
// It arrives either as a plain `message`, or as an `errors` map keyed by the
// parameter at fault. Both are worth surfacing verbatim: the whole reason a
// power action's failure was previously unreadable is that this was thrown
// away and only the status code kept. Returns "" when there is nothing to add,
// so callers can append it unconditionally.
func platformReason(resp *http.Response) string {
	var body struct {
		Message string            `json:"message"`
		Errors  map[string]string `json:"errors"`
	}
	// Bounded: an error body is small, and a huge one is its own problem.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return ""
	}

	parts := make([]string, 0, len(body.Errors)+1)
	if msg := strings.TrimSpace(body.Message); msg != "" {
		parts = append(parts, msg)
	}
	// Map iteration is unordered, and an error that reads differently each
	// time it is logged is one nobody can search for.
	keys := make([]string, 0, len(body.Errors))
	for k := range body.Errors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %s", k, strings.TrimSpace(body.Errors[k])))
	}

	if len(parts) == 0 {
		return ""
	}
	return ": " + strings.Join(parts, "; ")
}

func classifyTransportError(path string, err error) error {
	// A pinned-certificate mismatch is already classified; keep its detail
	// rather than burying it under a generic "unreachable".
	var cerr *connector.Error
	if errors.As(err, &cerr) {
		return cerr
	}
	return connector.Wrap(connector.ErrUnreachable, opFromPath(path), err)
}

// opFromPath turns an API path into a short operation label for error messages
// and metrics, without leaking ids into label cardinality.
func opFromPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 {
		return "request"
	}
	if len(parts) > 2 {
		parts = parts[:2]
	}
	return strings.Join(parts, "_")
}
