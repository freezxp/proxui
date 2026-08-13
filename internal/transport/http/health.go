package httpapi

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Checker is a dependency whose reachability gates readiness.
type Checker interface {
	Ping(ctx context.Context) error
}

// CheckerFunc adapts a function to Checker.
type CheckerFunc func(ctx context.Context) error

// Ping implements Checker.
func (f CheckerFunc) Ping(ctx context.Context) error { return f(ctx) }

// Readiness aggregates the dependency checks behind /readyz.
type Readiness struct {
	Checkers map[string]Checker
	Timeout  time.Duration

	// MigrationsApplied is set once schema migrations have completed (or were
	// deliberately skipped). Until then the process must not accept traffic.
	MigrationsApplied atomic.Bool
}

type healthResponse struct {
	Status  string            `json:"status"`
	Version string            `json:"version,omitempty"`
	Checks  map[string]string `json:"checks,omitempty"`
}

// handleLive reports process liveness. It must stay dependency-free: a failing
// database is a readiness problem, not a reason to restart the container.
func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, healthResponse{Status: "ok", Version: s.version})
}

// handleReady reports whether this process can serve traffic right now.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	timeout := s.readiness.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	checks := make(map[string]string, len(s.readiness.Checkers)+1)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for name, c := range s.readiness.Checkers {
		wg.Add(1)
		go func(name string, c Checker) {
			defer wg.Done()
			result := "ok"
			if err := c.Ping(ctx); err != nil {
				result = "error: " + err.Error()
			}
			mu.Lock()
			checks[name] = result
			mu.Unlock()
		}(name, c)
	}
	wg.Wait()

	if s.readiness.MigrationsApplied.Load() {
		checks["migrations"] = "ok"
	} else {
		checks["migrations"] = "error: not applied"
	}

	status := http.StatusOK
	body := healthResponse{Status: "ready", Version: s.version, Checks: checks}
	for _, v := range checks {
		if v != "ok" {
			status = http.StatusServiceUnavailable
			body.Status = "not ready"
			break
		}
	}
	WriteJSON(w, status, body)
}
