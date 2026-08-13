package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func newTestServer(t *testing.T, checkers map[string]Checker, migrated bool) *Server {
	t.Helper()
	rd := &Readiness{Checkers: checkers}
	rd.MigrationsApplied.Store(migrated)
	return NewServer(zerolog.New(io.Discard), "test", rd)
}

func do(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestHealthzIsDependencyFree(t *testing.T) {
	// Liveness must stay green even when dependencies are down, otherwise the
	// container gets restarted for an outage it cannot fix.
	failing := map[string]Checker{"database": CheckerFunc(func(context.Context) error {
		return errors.New("connection refused")
	})}
	rec := do(t, newTestServer(t, failing, false).Routes(), http.MethodGet, "/healthz")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
}

func TestReadyzReportsPerCheckResults(t *testing.T) {
	ok := CheckerFunc(func(context.Context) error { return nil })
	tests := []struct {
		name     string
		checkers map[string]Checker
		migrated bool
		want     int
	}{
		{"all healthy", map[string]Checker{"database": ok, "redis": ok}, true, http.StatusOK},
		{"migrations pending", map[string]Checker{"database": ok}, false, http.StatusServiceUnavailable},
		{"dependency down", map[string]Checker{
			"database": CheckerFunc(func(context.Context) error { return errors.New("down") }),
		}, true, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, newTestServer(t, tt.checkers, tt.migrated).Routes(), http.MethodGet, "/readyz")
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.want, rec.Body)
			}
			var body healthResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if _, ok := body.Checks["migrations"]; !ok {
				t.Error("readiness response is missing the migrations check")
			}
		})
	}
}

func TestUnknownRouteReturnsProblemJSON(t *testing.T) {
	rec := do(t, newTestServer(t, nil, true).Routes(), http.MethodGet, "/api/v1/nope")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Code != "not_found" || p.Status != http.StatusNotFound {
		t.Errorf("problem = %+v, want code=not_found status=404", p)
	}
	if p.RequestID == "" {
		t.Error("problem.request_id is empty; operators need it to correlate logs")
	}
}

func TestResponsesEchoRequestID(t *testing.T) {
	rec := do(t, newTestServer(t, nil, true).Routes(), http.MethodGet, "/healthz")
	if rec.Header().Get(requestIDHeader) == "" {
		t.Errorf("%s header is empty", requestIDHeader)
	}
}

func TestPanicBecomesProblemResponse(t *testing.T) {
	s := newTestServer(t, nil, true)
	h := recoverPanic(s.log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Code != "internal" {
		t.Errorf("code = %q, want internal", p.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("panic detail leaked to the client")
	}
}
