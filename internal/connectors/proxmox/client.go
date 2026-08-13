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
	"strings"
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

// client is a thin Proxmox API client: auth, TLS policy, rate limiting and
// error classification. Endpoint knowledge lives in the sibling files.
type client struct {
	base    *url.URL
	http    *http.Client
	limiter *rate.Limiter
	// authHeader is precomputed; it is never logged.
	authHeader  string
	concurrency int
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
	tlsCfg, err := tlsConfig(cfg.TLS, base.Hostname())
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

	return &client{
		base: base,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig:     tlsCfg,
				MaxIdleConnsPerHost: concurrency,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		limiter:     rate.NewLimiter(rate.Limit(rps), defaultBurst),
		authHeader:  auth,
		concurrency: concurrency,
	}, nil
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

func (c *client) do(ctx context.Context, method, path string, query, form url.Values, out any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return connector.Wrap(connector.ErrUnreachable, opFromPath(path), err)
	}

	target := c.base.JoinPath(apiPrefix + path)
	if len(query) > 0 {
		target.RawQuery = query.Encode()
	}
	endpoint := target.String()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return connector.Wrap(connector.ErrInvalidConfig, opFromPath(path), err)
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return classifyTransportError(path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}()

	if err := classifyStatus(path, resp); err != nil {
		return err
	}
	if out == nil {
		return nil
	}

	var env envelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&env); err != nil {
		return connector.Wrap(connector.ErrUnreachable, opFromPath(path), fmt.Errorf("decode response: %w", err))
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return connector.Wrap(connector.ErrUnreachable, opFromPath(path), fmt.Errorf("decode data: %w", err))
	}
	return nil
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
		return connector.Errorf(connector.ErrUnreachable, opFromPath(path), "platform returned HTTP %d", resp.StatusCode)
	default:
		return connector.Errorf(connector.ErrUnreachable, opFromPath(path), "unexpected HTTP %d", resp.StatusCode)
	}
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
