package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

func testConfig() Config {
	return Config{
		ClientID:     "client-id.apps.googleusercontent.com",
		ClientSecret: "client-secret",
		RedirectURL:  "https://portal.example/api/v1/auth/google/callback",
	}
}

// static is a configuration that never changes, for tests that do not care
// that the real one is re-read per call.
func static(cfg Config) ConfigSource {
	return func(context.Context) Config { return cfg }
}

func TestAuthorizeURLCarriesTheProtections(t *testing.T) {
	client := New(static(testConfig()), nil)
	attempt, err := NewAttempt("/vms")
	if err != nil {
		t.Fatal(err)
	}

	raw, err := client.AuthorizeURL(context.Background(), attempt)
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()

	if q.Get("state") != attempt.State || q.Get("nonce") != attempt.Nonce {
		t.Error("state and nonce must reach Google, or neither the callback nor the token can be tied to this attempt")
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	// The verifier itself must never leave the server; only its hash goes.
	want := sha256.Sum256([]byte(attempt.Verifier))
	if q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(want[:]) {
		t.Error("the code challenge is not the hash of the verifier")
	}
	if strings.Contains(raw, attempt.Verifier) {
		t.Error("the PKCE verifier was sent to Google; it must stay on the server")
	}
	if q.Get("redirect_uri") != testConfig().RedirectURL {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
}

func TestUnconfiguredClientRefuses(t *testing.T) {
	client := New(static(Config{}), nil)
	if client.Enabled(context.Background()) {
		t.Fatal("an empty configuration reported itself as enabled")
	}
	if _, err := client.AuthorizeURL(context.Background(), Attempt{}); err != ErrNotConfigured {
		t.Errorf("got %v, want ErrNotConfigured", err)
	}
}

// Each attempt must be unique, or two people signing in at once could collide
// and one would complete the other's flow.
func TestAttemptsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		a, err := NewAttempt("/")
		if err != nil {
			t.Fatal(err)
		}
		if seen[a.State] || seen[a.Nonce] || seen[a.Verifier] {
			t.Fatal("a random value repeated")
		}
		seen[a.State], seen[a.Nonce], seen[a.Verifier] = true, true, true
		if len(a.Verifier) < 43 {
			t.Errorf("verifier is %d characters; RFC 7636 wants at least 43", len(a.Verifier))
		}
	}
}

func TestRSAKeyRejectsNonsense(t *testing.T) {
	if _, err := rsaPublicKey("!!!not base64!!!", "AQAB"); err == nil {
		t.Error("a malformed modulus was accepted")
	}
	if _, err := rsaPublicKey("", ""); err == nil {
		t.Error("empty key material was accepted")
	}
	// A tiny exponent would make signature verification trivially forgeable.
	if _, err := rsaPublicKey(base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3}),
		base64.RawURLEncoding.EncodeToString([]byte{1})); err == nil {
		t.Error("an implausible exponent was accepted")
	}
	// The usual 65537, which is what Google publishes.
	if _, err := rsaPublicKey(base64.RawURLEncoding.EncodeToString(make([]byte, 256)),
		base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01})); err != nil {
		t.Errorf("a normal key was rejected: %v", err)
	}
}

// Configuration is re-read on every call, so a credential corrected in
// Settings takes effect on the next attempt rather than at the next restart.
func TestConfigurationIsReadPerCall(t *testing.T) {
	current := Config{}
	client := New(func(context.Context) Config { return current }, nil)

	if client.Enabled(context.Background()) {
		t.Fatal("reported enabled before anything was configured")
	}

	current = testConfig()
	if !client.Enabled(context.Background()) {
		t.Error("a configuration supplied after construction was not picked up")
	}

	raw, err := client.AuthorizeURL(context.Background(), mustAttempt(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, url.QueryEscape(testConfig().RedirectURL)) {
		t.Error("the authorize URL did not use the newly supplied redirect")
	}
}

func mustAttempt(t *testing.T) Attempt {
	t.Helper()
	a, err := NewAttempt("/")
	if err != nil {
		t.Fatal(err)
	}
	return a
}
