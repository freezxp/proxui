package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// Principal is the authenticated caller derived from an access token.
type Principal struct {
	UserID    uuid.UUID
	Role      identity.Role
	SessionID uuid.UUID
}

type principalKey struct{}

// WithPrincipal returns a context carrying the authenticated caller.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom extracts the authenticated caller, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// TokenParser verifies access tokens.
type TokenParser interface {
	Parse(token string) (*crypto.Claims, error)
}

// SessionChecker reports whether the session behind a token is still usable.
// Checking on every request is what makes deactivation and logout take effect
// immediately rather than when the access token expires.
type SessionChecker interface {
	IsSessionActive(ctx context.Context, sessionID uuid.UUID) (bool, error)
}

// RequireAuth rejects requests without a valid access token whose session is
// still active.
func RequireAuth(tokens TokenParser, sessions SessionChecker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := bearerToken(r)
			if raw == "" {
				WriteProblem(w, r, http.StatusUnauthorized, "auth.missing_token", "Authentication is required.")
				return
			}
			claims, err := tokens.Parse(raw)
			if err != nil {
				WriteProblem(w, r, http.StatusUnauthorized, "auth.invalid_token", "The access token is invalid or expired.")
				return
			}
			userID, err := uuid.Parse(claims.Subject)
			if err != nil {
				WriteProblem(w, r, http.StatusUnauthorized, "auth.invalid_token", "The access token is invalid or expired.")
				return
			}
			sessionID, err := uuid.Parse(claims.SessionID)
			if err != nil {
				WriteProblem(w, r, http.StatusUnauthorized, "auth.invalid_token", "The access token is invalid or expired.")
				return
			}
			active, err := sessions.IsSessionActive(r.Context(), sessionID)
			if err != nil {
				WriteProblem(w, r, http.StatusInternalServerError, "internal", "Could not verify the session.")
				return
			}
			if !active {
				WriteProblem(w, r, http.StatusUnauthorized, "auth.session_revoked", "This session is no longer valid.")
				return
			}

			ctx := WithPrincipal(r.Context(), Principal{
				UserID:    userID,
				Role:      identity.Role(claims.Role),
				SessionID: sessionID,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole allows only the listed roles. Role checks are coarse capability
// gates; per-VM scoping is a separate query-level concern (sprint 3).
func RequireRole(roles ...identity.Role) func(http.Handler) http.Handler {
	allowed := make(map[identity.Role]bool, len(roles))
	for _, r := range roles {
		allowed[r] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				WriteProblem(w, r, http.StatusUnauthorized, "auth.missing_token", "Authentication is required.")
				return
			}
			if !allowed[p.Role] {
				WriteProblem(w, r, http.StatusForbidden, "rbac.role_denied",
					"Role "+string(p.Role)+" is not permitted to perform this action.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}
