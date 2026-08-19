package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/infra/oauth"
)

// The return parameter is attacker-supplied and ends up in a redirect. Without
// this it is an open redirect: a link that sends someone through a genuine
// Google sign-in and then out to somewhere else entirely, with the portal's
// name on the journey.
func TestReturnPathCannotLeaveThePortal(t *testing.T) {
	for _, hostile := range []string{
		"https://evil.example/steal",
		"//evil.example/steal",
		"http://evil.example",
		"/\\evil.example",
		"/vms\r\nSet-Cookie: x=1",
		"",
		"vms",
	} {
		if got := safeReturnPath(hostile); got != "/" {
			t.Errorf("safeReturnPath(%q) = %q, want /", hostile, got)
		}
	}
	for _, ok := range []string{"/", "/vms", "/vms/123?tab=performance"} {
		if got := safeReturnPath(ok); got != ok {
			t.Errorf("safeReturnPath(%q) = %q, want it unchanged", ok, got)
		}
	}
}

// memoryAttempts holds sign-in attempts for a test.
type memoryAttempts struct{ saved map[string]oauth.Attempt }

func (m *memoryAttempts) Put(_ context.Context, state string, a oauth.Attempt, _ time.Duration) error {
	if m.saved == nil {
		m.saved = map[string]oauth.Attempt{}
	}
	m.saved[state] = a
	return nil
}

func (m *memoryAttempts) Take(_ context.Context, state string) (oauth.Attempt, error) {
	a, ok := m.saved[state]
	if !ok {
		return oauth.Attempt{}, errors.New("no such attempt")
	}
	delete(m.saved, state)
	return a, nil
}

func googleServer(t *testing.T, cfg oauth.Config, attempts *memoryAttempts) http.Handler {
	t.Helper()
	return NewServer(ServerConfig{
		Log:       zerolog.New(io.Discard),
		Version:   "test",
		Readiness: &Readiness{},
		Registration: RegistrationDeps{
			OAuth:    oauth.New(func(context.Context) oauth.Config { return cfg }, nil),
			Attempts: attempts,
		},
	}).Routes()
}

// startGoogle follows /auth/google/start and reports the redirect_uri it sent
// Google, plus the one recorded against the attempt.
func startGoogle(t *testing.T, h http.Handler, req *http.Request, attempts *memoryAttempts) (string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body %s)", rec.Code, rec.Body)
	}
	target, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	sent := target.Query().Get("redirect_uri")
	stored, ok := attempts.saved[target.Query().Get("state")]
	if !ok {
		t.Fatal("the attempt was not stored under the state sent to Google")
	}
	return sent, stored.Redirect
}

// A portal answers to more than one name — a LAN name, a public one, a tunnel
// hostname — and Google returns the browser to the redirect URI verbatim.
// Signing in on one name has to come back to that name, or the session lands
// under a host the person is not looking at.
func TestGoogleRedirectFollowsTheHostTheBrowserUsed(t *testing.T) {
	unpinned := oauth.Config{ClientID: "id.apps.googleusercontent.com", ClientSecret: "secret"}

	tests := []struct {
		name    string
		prepare func(*http.Request)
		want    string
	}{
		{
			name:    "plain HTTP on the LAN",
			prepare: func(r *http.Request) { r.Host = "vm.intranet.my" },
			want:    "http://vm.intranet.my/api/v1/auth/google/callback",
		},
		{
			name: "TLS terminated by a reverse proxy",
			prepare: func(r *http.Request) {
				r.Host = "vm.intranet.my"
				r.Header.Set("X-Forwarded-Proto", "https")
			},
			want: "https://vm.intranet.my/api/v1/auth/google/callback",
		},
		{
			name: "a proxy that rewrote Host to its own address",
			prepare: func(r *http.Request) {
				r.Host = "proxui:8080"
				r.Header.Set("X-Forwarded-Proto", "https")
				r.Header.Set("X-Forwarded-Host", "vm.cyberjaya.pro")
			},
			want: "https://vm.cyberjaya.pro/api/v1/auth/google/callback",
		},
		{
			name: "a chain of proxies names the browser's host first",
			prepare: func(r *http.Request) {
				r.Header.Set("X-Forwarded-Proto", "https")
				r.Header.Set("X-Forwarded-Host", "vm.cyberjaya.pro, inner.proxy")
			},
			want: "https://vm.cyberjaya.pro/api/v1/auth/google/callback",
		},
		{
			name:    "a port is part of the address and is kept",
			prepare: func(r *http.Request) { r.Host = "vm.intranet.my:8080" },
			want:    "http://vm.intranet.my:8080/api/v1/auth/google/callback",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempts := &memoryAttempts{}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/google/start", nil)
			tt.prepare(req)

			sent, stored := startGoogle(t, googleServer(t, unpinned, attempts), req, attempts)
			if sent != tt.want {
				t.Errorf("redirect_uri = %q, want %q", sent, tt.want)
			}
			// The exchange has to send Google the same value, so it is
			// recorded rather than resolved a second time on the way back.
			if stored != tt.want {
				t.Errorf("the attempt recorded %q, want %q", stored, tt.want)
			}
		})
	}
}

// A deployment that pinned the redirect URL did so to say something the
// request cannot — it sits behind something whose address it never sees — so
// the pin wins over the host.
func TestPinnedGoogleRedirectIgnoresTheHost(t *testing.T) {
	pinned := "https://portal.example/api/v1/auth/google/callback"
	attempts := &memoryAttempts{}
	h := googleServer(t, oauth.Config{
		ClientID:     "id.apps.googleusercontent.com",
		ClientSecret: "secret",
		RedirectURL:  pinned,
	}, attempts)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/google/start", nil)
	req.Host = "somewhere.else"

	sent, stored := startGoogle(t, h, req, attempts)
	if sent != pinned || stored != pinned {
		t.Errorf("redirect_uri = %q, attempt recorded %q, want %q for both", sent, stored, pinned)
	}
}

// Google sign-in used to need a redirect URL before it would offer itself at
// all. Now that one is derived from the request, a client ID and secret are
// the whole configuration.
func TestGoogleIsOfferedWithoutAPinnedRedirect(t *testing.T) {
	attempts := &memoryAttempts{}
	h := googleServer(t, oauth.Config{
		ClientID: "id.apps.googleusercontent.com", ClientSecret: "secret",
	}, attempts)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/methods", nil))

	var body struct {
		Google bool `json:"google"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	if !body.Google {
		t.Error("Google sign-in was not offered by a portal that has a client ID and secret")
	}
}
