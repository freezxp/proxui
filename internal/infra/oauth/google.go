// Package oauth implements sign-in through an external identity provider.
//
// Google only, for now, and deliberately hand-rolled against its OpenID
// Connect endpoints rather than pulled in as a framework: the flow is one
// redirect and one token exchange, and the parts worth getting right — state,
// PKCE, and verifying the identity token's signature — are the parts a
// framework would hide.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrNotConfigured is returned when Google sign-in has not been set up. The
// client credentials are deployment configuration, like the master key: they
// come from the environment rather than the settings table, because a settings
// row is stored in plain text and a client secret should not be.
var ErrNotConfigured = errors.New("oauth: google sign-in is not configured")

// ErrInvalidToken covers every way an identity token can fail to convince us.
var ErrInvalidToken = errors.New("oauth: the provider's identity token is not valid")

// Google's endpoints. Hardcoded rather than discovered at boot so the portal
// starts without reaching the internet; they have been stable for a decade and
// a change would break every client at once, loudly.
const (
	googleIssuer   = "https://accounts.google.com"
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	googleJWKSURL  = "https://www.googleapis.com/oauth2/v3/certs"
)

// Config is what a deployment supplies to enable Google sign-in.
type Config struct {
	ClientID     string
	ClientSecret string
	// RedirectURL must match one registered with Google exactly, including
	// scheme and path. Mismatches are the single most common failure here, so
	// the error says so in as many words.
	RedirectURL string
}

// Enabled reports whether enough was configured to attempt a sign-in.
func (c Config) Enabled() bool {
	return c.ClientID != "" && c.ClientSecret != "" && c.RedirectURL != ""
}

// Identity is what Google told us about the person who just signed in.
type Identity struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

// Client performs the authorization-code flow with PKCE.
type Client struct {
	Config Config
	HTTP   *http.Client

	mu       sync.RWMutex
	keys     map[string]any
	keysRead time.Time
}

// New builds a client.
func New(cfg Config, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{Config: cfg, HTTP: httpClient}
}

// Attempt is the per-sign-in state that has to survive the round trip to
// Google. It is held server-side and keyed by the state parameter, so a
// callback carrying a state nobody issued is rejected before anything else
// happens.
type Attempt struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Return   string `json:"return"`
}

// NewAttempt mints the random values one sign-in needs.
func NewAttempt(returnTo string) (Attempt, error) {
	state, err := randomString()
	if err != nil {
		return Attempt{}, err
	}
	nonce, err := randomString()
	if err != nil {
		return Attempt{}, err
	}
	verifier, err := randomString()
	if err != nil {
		return Attempt{}, err
	}
	return Attempt{State: state, Nonce: nonce, Verifier: verifier, Return: returnTo}, nil
}

// AuthorizeURL is where the browser is sent to sign in.
func (c *Client) AuthorizeURL(a Attempt) (string, error) {
	if !c.Config.Enabled() {
		return "", ErrNotConfigured
	}
	challenge := sha256.Sum256([]byte(a.Verifier))

	q := url.Values{}
	q.Set("client_id", c.Config.ClientID)
	q.Set("redirect_uri", c.Config.RedirectURL)
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	q.Set("state", a.State)
	q.Set("nonce", a.Nonce)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	q.Set("code_challenge_method", "S256")
	// Ask for an account chooser rather than silently reusing whichever Google
	// account the browser happens to be signed into.
	q.Set("prompt", "select_account")

	return googleAuthURL + "?" + q.Encode(), nil
}

// Exchange turns an authorization code into a verified identity.
func (c *Client) Exchange(ctx context.Context, code string, a Attempt) (Identity, error) {
	if !c.Config.Enabled() {
		return Identity{}, ErrNotConfigured
	}

	form := url.Values{}
	form.Set("client_id", c.Config.ClientID)
	form.Set("client_secret", c.Config.ClientSecret)
	form.Set("code", code)
	form.Set("code_verifier", a.Verifier)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", c.Config.RedirectURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("oauth: token exchange: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
		Detail  string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Identity{}, fmt.Errorf("oauth: token exchange returned %s", resp.Status)
	}
	if resp.StatusCode >= 300 || body.Error != "" {
		// redirect_uri_mismatch is the usual cause and is invisible from the
		// portal's side, so it is repeated verbatim rather than summarized.
		return Identity{}, fmt.Errorf("oauth: google refused the exchange: %s %s", body.Error, body.Detail)
	}
	if body.IDToken == "" {
		return Identity{}, fmt.Errorf("%w: no identity token in the response", ErrInvalidToken)
	}

	return c.verify(ctx, body.IDToken, a.Nonce)
}

// verify checks the identity token's signature and every claim that matters.
//
// The signature is the whole point: without it, anything that can reach the
// callback could present a token claiming to be anyone.
func (c *Client) verify(ctx context.Context, raw, nonce string) (Identity, error) {
	keys, err := c.signingKeys(ctx, false)
	if err != nil {
		return Identity{}, err
	}

	parse := func(keys map[string]any) (*jwt.Token, error) {
		return jwt.Parse(raw, func(token *jwt.Token) (any, error) {
			if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
				return nil, fmt.Errorf("%w: unexpected signing method %v", ErrInvalidToken, token.Header["alg"])
			}
			kid, _ := token.Header["kid"].(string)
			key, ok := keys[kid]
			if !ok {
				return nil, fmt.Errorf("%w: unknown signing key %q", ErrInvalidToken, kid)
			}
			return key, nil
		}, jwt.WithIssuer(googleIssuer), jwt.WithAudience(c.Config.ClientID),
			jwt.WithExpirationRequired())
	}

	token, err := parse(keys)
	if err != nil {
		// Google rotates its keys; an unknown kid means the cache is stale
		// rather than that the token is forged. Refetch once, then give up.
		if refreshed, rerr := c.signingKeys(ctx, true); rerr == nil {
			token, err = parse(refreshed)
		}
	}
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return Identity{}, ErrInvalidToken
	}
	// The nonce ties this token to the redirect this portal started. Without
	// it, a token obtained elsewhere for the same client could be replayed.
	if got, _ := claims["nonce"].(string); got != nonce {
		return Identity{}, fmt.Errorf("%w: the token belongs to a different sign-in", ErrInvalidToken)
	}

	subject, _ := claims["sub"].(string)
	email, _ := claims["email"].(string)
	name, _ := claims["name"].(string)
	verified, _ := claims["email_verified"].(bool)

	return Identity{
		Subject: subject, Email: email, EmailVerified: verified, Name: name,
	}, nil
}

// signingKeys returns Google's public keys, cached for an hour.
func (c *Client) signingKeys(ctx context.Context, force bool) (map[string]any, error) {
	c.mu.RLock()
	fresh := time.Since(c.keysRead) < time.Hour && len(c.keys) > 0
	cached := c.keys
	c.mu.RUnlock()
	if fresh && !force {
		return cached, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleJWKSURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth: fetch signing keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth: signing keys returned %s", resp.Status)
	}

	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("oauth: decode signing keys: %w", err)
	}

	keys := make(map[string]any, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		key, err := rsaPublicKey(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = key
	}
	if len(keys) == 0 {
		return nil, errors.New("oauth: the provider published no usable signing keys")
	}

	c.mu.Lock()
	c.keys, c.keysRead = keys, time.Now()
	c.mu.Unlock()
	return keys, nil
}

func randomString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oauth: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
