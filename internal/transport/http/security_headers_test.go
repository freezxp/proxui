package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	httpapi "github.com/freezxp/proxui/internal/transport/http"
)

func serveWithHeaders(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	handler := httpapi.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Result()
}

func TestSecurityHeadersArePresent(t *testing.T) {
	resp := serveWithHeaders(t, httptest.NewRequest(http.MethodGet, "/", nil))
	defer resp.Body.Close()

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	csp := resp.Header.Get("Content-Security-Policy")
	for _, directive := range []string{
		"default-src 'self'",
		"object-src 'none'",
		"frame-ancestors 'none'", // what actually stops the console being framed
		"base-uri 'none'",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP is missing %q: %s", directive, csp)
		}
	}
	// An inline-script escape hatch would defeat the point of having a policy.
	if strings.Contains(csp, "script-src") && strings.Contains(csp, "'unsafe-inline' 'unsafe-eval'") {
		t.Errorf("CSP allows inline script: %s", csp)
	}
	// The console needs these; a policy that omits them breaks it.
	if !strings.Contains(csp, "ws:") || !strings.Contains(csp, "wss:") {
		t.Errorf("CSP would block the console WebSocket: %s", csp)
	}
}

// HSTS over plain HTTP would pin a browser to a scheme a LAN deployment does
// not serve, locking the operator out of their own portal.
func TestHSTSOnlyOverTLS(t *testing.T) {
	plain := serveWithHeaders(t, httptest.NewRequest(http.MethodGet, "/", nil))
	defer plain.Body.Close()
	if got := plain.Header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS sent over plain HTTP: %q", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	proxied := serveWithHeaders(t, req)
	defer proxied.Body.Close()
	if got := proxied.Header.Get("Strict-Transport-Security"); got == "" {
		t.Error("HSTS missing behind a TLS-terminating proxy")
	}
}
