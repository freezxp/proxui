package httpapi

import (
	"net/http"
	"strings"
)

// contentSecurityPolicy is deliberately strict (docs/15-security-design.md
// §15.6).
//
// The build emits hashed assets and no inline script, so 'self' is enough for
// scripts. Two directives are looser than the rest and both are load-bearing:
//
//   - style-src allows 'unsafe-inline' because noVNC sets element styles
//     directly while sizing its canvas. Removing it breaks the console.
//   - connect-src includes ws: and wss: for the console bridge and the event
//     stream, which are same-origin but need the scheme named explicitly.
//
// frame-ancestors 'none' is the header-level version of X-Frame-Options and
// is what actually stops the console being framed by another site.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; " +
	"font-src 'self'; " +
	"connect-src 'self' ws: wss:; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// SecurityHeaders sets the response headers that make a browser enforce the
// portal's assumptions.
//
// These belong on the application rather than only on the reverse proxy: a
// self-hosted deployment may put anything in front of this, and a header that
// exists only in the recommended Caddy config is a header that is missing in
// half of real installations.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		// The portal needs none of these, and saying so stops an injected
		// script from asking on its behalf.
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")

		// HSTS only over TLS. Sending it on a plain-HTTP LAN deployment would
		// pin the browser to a scheme that deployment does not serve, locking
		// the operator out of their own portal.
		if isTLS(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		next.ServeHTTP(w, r)
	})
}

// isTLS reports whether the request reached the portal over TLS, including
// through a reverse proxy that terminated it.
func isTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
