package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
)

// requestIDHeader is echoed on every response so users can quote it in reports
// and operators can grep logs and audit entries by it.
const requestIDHeader = "X-Request-ID"

// echoRequestID copies the request ID chi generated (or accepted from the
// client) into the response headers.
func echoRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := middleware.GetReqID(r.Context()); id != "" {
			w.Header().Set(requestIDHeader, id)
		}
		next.ServeHTTP(w, r)
	})
}

// requestLogger emits one structured line per request.
func requestLogger(log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			status := ww.Status()
			evt := log.Info()
			switch {
			case status >= 500:
				evt = log.Error()
			case status >= 400:
				evt = log.Warn()
			}
			evt.Str("component", "http").
				Str("request_id", middleware.GetReqID(r.Context())).
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Int("status", status).
				Int("bytes", ww.BytesWritten()).
				Dur("duration_ms", time.Since(start)).
				Str("remote_ip", r.RemoteAddr).
				Msg("request")
		})
	}
}

// recoverPanic turns a panic into a 500 problem response plus a logged stack,
// keeping one bad handler from taking down the process.
func recoverPanic(log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					log.Error().
						Str("component", "http").
						Str("request_id", middleware.GetReqID(r.Context())).
						Interface("panic", rec).
						Bytes("stack", stack()).
						Msg("panic recovered")
					WriteProblem(w, r, http.StatusInternalServerError, "internal", "An unexpected error occurred.")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
