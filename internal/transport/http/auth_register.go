package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/infra/oauth"
)

// AttemptStore holds one in-flight external sign-in, keyed by its state value.
// Redis rather than a cookie: the verifier must not travel through the browser
// at all, and a state nobody issued has to be rejectable.
type AttemptStore interface {
	Put(ctx context.Context, state string, attempt oauth.Attempt, ttl time.Duration) error
	Take(ctx context.Context, state string) (oauth.Attempt, error)
}

// RegisterHandler creates a local account for someone not signed in.
type RegisterHandler interface {
	Handle(ctx context.Context, in command.RegisterInput) (*identity.User, error)
}

// ExternalSignInHandler resolves a provider identity to a portal account.
type ExternalSignInHandler interface {
	Handle(ctx context.Context, in command.ExternalIdentity, actor command.Actor) (*identity.User, error)
}

// SessionIssuer mints a session for an account that has already proved who it
// is by some route other than a password.
type SessionIssuer interface {
	Issue(ctx context.Context, user *identity.User, ip, userAgent, requestID string) (command.LoginOutput, error)
}

// RegistrationDeps bundles registration and external sign-in.
type RegistrationDeps struct {
	Register RegisterHandler
	External ExternalSignInHandler
	Sessions SessionIssuer
	OAuth    *oauth.Client
	Attempts AttemptStore
	Policy   command.RegistrationPolicy
}

// attemptTTL bounds how long a half-finished sign-in stays valid. Long enough
// to pick an account and type a password, short enough that an abandoned one
// is not lying around.
const attemptTTL = 10 * time.Minute

type registerRequest struct {
	Username    string `json:"username"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
}

// handleAuthMethods tells the sign-in page which ways in exist, so it can offer
// them without guessing. Public, like branding: it describes the door, not
// what is behind it.
func (s *Server) handleAuthMethods(w http.ResponseWriter, r *http.Request) {
	registration := false
	if s.registration.Policy != nil {
		registration = s.registration.Policy.SelfRegistrationEnabled(r.Context())
	}
	google := s.registration.OAuth != nil && s.registration.OAuth.Enabled(r.Context())

	WriteJSON(w, http.StatusOK, map[string]any{
		"password":     true,
		"registration": registration,
		"google":       google,
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	user, err := s.registration.Register.Handle(r.Context(), command.RegisterInput{
		Actor:       s.actor(r),
		Username:    req.Username,
		Email:       req.Email,
		DisplayName: req.DisplayName,
		Password:    req.Password,
	})
	if err != nil {
		s.writeRegisterError(w, r, err)
		return
	}

	// Signed in immediately: an account that exists but cannot be used until
	// a second, identical form is filled in is a worse experience for no gain.
	out, err := s.registration.Sessions.Issue(r.Context(), user,
		r.RemoteAddr, r.UserAgent(), middleware.GetReqID(r.Context()))
	if err != nil {
		// The account was created; only the session was not. Say so rather
		// than implying the registration failed.
		s.log.Error().Err(err).Msg("could not sign in a newly registered account")
		WriteProblem(w, r, http.StatusCreated, "auth.sign_in_required",
			"Your account was created. Please sign in.")
		return
	}

	s.setRefreshCookie(w, out.RefreshToken, "")
	WriteJSON(w, http.StatusCreated, tokenResponse{
		AccessToken: out.AccessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int(out.ExpiresIn.Seconds()),
	})
}

func (s *Server) writeRegisterError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, command.ErrRegistrationClosed):
		WriteProblem(w, r, http.StatusForbidden, "auth.registration_closed",
			"This portal does not accept new registrations. Ask an administrator for an account.")
	case errors.Is(err, ports.ErrConflict):
		// One message for a taken username and a taken email: telling them
		// apart turns this form into an account enumeration oracle.
		WriteProblem(w, r, http.StatusConflict, "conflict",
			"That username or email address is already in use.")
	case errors.Is(err, identity.ErrInvalidUsername):
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", err.Error(),
			map[string]string{"username": "invalid"})
	case errors.Is(err, identity.ErrInvalidEmail):
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", err.Error(),
			map[string]string{"email": "invalid"})
	case errors.Is(err, identity.ErrWeakPassword):
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", err.Error(),
			map[string]string{"password": "policy"})
	default:
		s.serverError(w, r, err, "Could not create the account.")
	}
}

// handleGoogleStart begins an external sign-in.
func (s *Server) handleGoogleStart(w http.ResponseWriter, r *http.Request) {
	if s.registration.OAuth == nil || !s.registration.OAuth.Enabled(r.Context()) {
		WriteProblem(w, r, http.StatusNotFound, "auth.provider_unavailable",
			"Google sign-in is not configured on this portal.")
		return
	}

	attempt, err := oauth.NewAttempt(safeReturnPath(r.URL.Query().Get("return")))
	if err != nil {
		s.serverError(w, r, err, "Could not start sign-in.")
		return
	}
	if err := s.registration.Attempts.Put(r.Context(), attempt.State, attempt, attemptTTL); err != nil {
		s.serverError(w, r, err, "Could not start sign-in.")
		return
	}

	target, err := s.registration.OAuth.AuthorizeURL(r.Context(), attempt)
	if err != nil {
		s.serverError(w, r, err, "Could not start sign-in.")
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// handleGoogleCallback finishes an external sign-in.
//
// Errors redirect back to the sign-in page carrying a short reason rather than
// rendering a problem document: the browser arrived here by following a
// redirect, and a person who clicked "Sign in with Google" should land back
// where they started, told what went wrong.
func (s *Server) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	if s.registration.OAuth == nil || !s.registration.OAuth.Enabled(r.Context()) {
		s.failSignIn(w, r, "unavailable", nil)
		return
	}

	query := r.URL.Query()
	if reason := query.Get("error"); reason != "" {
		// The person declined at Google's screen. Not an error worth logging.
		s.failSignIn(w, r, "cancelled", nil)
		return
	}

	attempt, err := s.registration.Attempts.Take(r.Context(), query.Get("state"))
	if err != nil {
		// An unknown state is either a stale attempt or someone else's
		// callback. Both are refused the same way.
		s.failSignIn(w, r, "expired", nil)
		return
	}

	identityFromGoogle, err := s.registration.OAuth.Exchange(r.Context(), query.Get("code"), attempt)
	if err != nil {
		s.failSignIn(w, r, "rejected", err)
		return
	}

	user, err := s.registration.External.Handle(r.Context(), command.ExternalIdentity{
		Provider:      identity.ProviderGoogle,
		Subject:       identityFromGoogle.Subject,
		Email:         identityFromGoogle.Email,
		DisplayName:   identityFromGoogle.Name,
		EmailVerified: identityFromGoogle.EmailVerified,
	}, s.actor(r))
	if err != nil {
		switch {
		case errors.Is(err, command.ErrRegistrationClosed):
			s.failSignIn(w, r, "no_account", nil)
		case errors.Is(err, identity.ErrAccountInactive):
			s.failSignIn(w, r, "inactive", nil)
		default:
			s.failSignIn(w, r, "rejected", err)
		}
		return
	}

	out, err := s.registration.Sessions.Issue(r.Context(), user,
		r.RemoteAddr, r.UserAgent(), middleware.GetReqID(r.Context()))
	if err != nil {
		s.failSignIn(w, r, "rejected", err)
		return
	}

	s.setRefreshCookie(w, out.RefreshToken, "")
	// The access token is deliberately not put in the URL, where it would land
	// in history and any proxy log. The page it lands on refreshes from the
	// cookie it now holds, which is the same path a returning visitor takes.
	http.Redirect(w, r, attempt.Return, http.StatusFound)
}

func (s *Server) failSignIn(w http.ResponseWriter, r *http.Request, reason string, err error) {
	if err != nil {
		s.log.Warn().Err(err).Str("reason", reason).Msg("external sign-in failed")
	}
	http.Redirect(w, r, "/?sso="+url.QueryEscape(reason), http.StatusFound)
}

// safeReturnPath keeps a redirect on this portal. Without this the return
// parameter is an open redirect: a link that sends someone through a genuine
// Google sign-in and out to somewhere else entirely.
func safeReturnPath(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	if strings.Contains(raw, "\\") || strings.ContainsAny(raw, "\r\n") {
		return "/"
	}
	return raw
}
