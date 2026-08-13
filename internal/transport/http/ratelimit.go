package httpapi

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Limiter counts requests against a named bucket.
type Limiter interface {
	Allow(ctx context.Context, bucket string, limit int, window time.Duration) (allowed bool, retryAfter time.Duration, err error)
}

// Rate limits (docs/08-api-specification.md §8.10). Login is strictest because
// it is the one endpoint an attacker can use without an account.
const (
	loginLimit    = 5
	loginWindow   = time.Minute
	consoleLimit  = 10
	consoleWindow = time.Minute
	apiLimit      = 100
	apiWindow     = time.Minute
)

// rateLimit returns middleware enforcing a bucket per caller.
//
// Authenticated callers are limited per user, so one person cannot exhaust a
// shared IP's budget for their colleagues behind the same NAT. Anonymous
// callers are limited per IP, which is all we know about them.
func (s *Server) rateLimit(name string, limit int, window time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s.limiter == nil {
				next.ServeHTTP(w, r)
				return
			}

			bucket := name + ":ip:" + clientIP(r)
			if p, ok := PrincipalFrom(r.Context()); ok {
				bucket = name + ":user:" + p.UserID.String()
			}

			allowed, retryAfter, err := s.limiter.Allow(r.Context(), bucket, limit, window)
			if err != nil {
				// The limiter failing must not take the API with it.
				s.log.Warn().Err(err).Str("bucket", name).Msg("rate limiter unavailable; allowing the request")
				next.ServeHTTP(w, r)
				return
			}
			if !allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
				WriteProblem(w, r, http.StatusTooManyRequests, "rate_limited",
					"Too many requests. Try again shortly.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// loginRateLimit additionally buckets by username, so an attacker spraying one
// account from many addresses is still throttled.
func (s *Server) loginRateLimit() func(http.Handler) http.Handler {
	return s.rateLimit("login", loginLimit, loginWindow)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
