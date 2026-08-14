package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// refreshCookieName is scoped to the auth path so it never rides along on
// ordinary API calls (docs/15-security-design.md §15.6).
const refreshCookieName = "proxui_rt"
const refreshCookiePath = "/api/v1/auth"

// LoginHandler, RefreshHandler and LogoutHandler are the application commands
// this transport exposes. They are interfaces so tests can substitute fakes.
type LoginHandler interface {
	Handle(ctx context.Context, in command.LoginInput) (command.LoginOutput, error)
}

// RefreshHandler exchanges a refresh token for a new pair.
type RefreshHandler interface {
	Handle(ctx context.Context, in command.RefreshInput) (command.LoginOutput, error)
}

// LogoutHandler revokes sessions.
type LogoutHandler interface {
	Handle(ctx context.Context, in command.LogoutInput) error
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

type meResponse struct {
	ID                 string `json:"id"`
	Username           string `json:"username"`
	Email              string `json:"email"`
	DisplayName        string `json:"display_name"`
	Role               string `json:"role"`
	TOTPEnabled        bool   `json:"totp_enabled"`
	MustChangePassword bool   `json:"must_change_password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if req.Username == "" || req.Password == "" {
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", "Username and password are required.",
			map[string]string{"username": "required", "password": "required"})
		return
	}

	out, err := s.auth.Login.Handle(r.Context(), command.LoginInput{
		Username:  req.Username,
		Password:  req.Password,
		IP:        r.RemoteAddr,
		UserAgent: r.UserAgent(),
		RequestID: middleware.GetReqID(r.Context()),
	})
	if err != nil {
		s.writeAuthError(w, r, err)
		return
	}

	s.setRefreshCookie(w, r, out.RefreshToken, out.User.ID.String())
	WriteJSON(w, http.StatusOK, tokenResponse{
		AccessToken: out.AccessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int(out.ExpiresIn / time.Second),
	})
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(refreshCookieName)
	if err != nil || cookie.Value == "" {
		WriteProblem(w, r, http.StatusUnauthorized, "auth.missing_refresh_token", "No refresh token was presented.")
		return
	}

	out, err := s.auth.Refresh.Handle(r.Context(), command.RefreshInput{
		RefreshToken: cookie.Value,
		IP:           r.RemoteAddr,
		UserAgent:    r.UserAgent(),
		RequestID:    middleware.GetReqID(r.Context()),
	})
	if err != nil {
		// Any refresh failure clears the cookie: the client must re-login
		// rather than retry with a token we have already rejected.
		s.clearRefreshCookie(w, r)
		s.writeAuthError(w, r, err)
		return
	}

	s.setRefreshCookie(w, r, out.RefreshToken, out.User.ID.String())
	WriteJSON(w, http.StatusOK, tokenResponse{
		AccessToken: out.AccessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int(out.ExpiresIn / time.Second),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	in := command.LogoutInput{
		IP:        r.RemoteAddr,
		UserAgent: r.UserAgent(),
		RequestID: middleware.GetReqID(r.Context()),
	}
	if cookie, err := r.Cookie(refreshCookieName); err == nil {
		in.RefreshToken = cookie.Value
	}
	if p, ok := PrincipalFrom(r.Context()); ok {
		in.UserID = p.UserID
	}

	if err := s.auth.Logout.Handle(r.Context(), in); err != nil {
		s.log.Error().Err(err).Msg("logout failed")
		WriteProblem(w, r, http.StatusInternalServerError, "internal", "Could not complete logout.")
		return
	}
	s.clearRefreshCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogoutAll(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteProblem(w, r, http.StatusUnauthorized, "auth.missing_token", "Authentication is required.")
		return
	}
	err := s.auth.Logout.Handle(r.Context(), command.LogoutInput{
		UserID:      p.UserID,
		AllSessions: true,
		IP:          r.RemoteAddr,
		UserAgent:   r.UserAgent(),
		RequestID:   middleware.GetReqID(r.Context()),
	})
	if err != nil {
		s.log.Error().Err(err).Msg("logout-all failed")
		WriteProblem(w, r, http.StatusInternalServerError, "internal", "Could not complete logout.")
		return
	}
	s.clearRefreshCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword lets the signed-in user replace their own password.
// Every role may do this: it is the only way MustChangePassword is cleared,
// and an account that cannot change its own password is one an administrator
// must be involved in every time.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteProblem(w, r, http.StatusUnauthorized, "auth.missing_token", "Authentication is required.")
		return
	}

	var req changePasswordRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if req.CurrentPassword == "" || req.NewPassword == "" {
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
			"Both the current and the new password are required.",
			map[string]string{"current_password": "required", "new_password": "required"})
		return
	}

	err := s.auth.ChangePassword.Handle(r.Context(), command.ChangePasswordInput{
		Actor: s.actor(r), UserID: p.UserID,
		CurrentPassword: req.CurrentPassword, NewPassword: req.NewPassword,
	})
	if err != nil {
		s.writePasswordError(w, r, err)
		return
	}

	// Every session was revoked, including this one, so the refresh cookie is
	// cleared rather than left pointing at something dead.
	s.clearRefreshCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writePasswordError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, identity.ErrInvalidCredentials):
		WriteProblemFields(w, r, http.StatusUnauthorized, "auth.invalid_credentials",
			"That is not your current password.",
			map[string]string{"current_password": "incorrect"})
	case errors.Is(err, identity.ErrPasswordUnchanged):
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
			"The new password must be different from the current one.",
			map[string]string{"new_password": "unchanged"})
	case errors.Is(err, identity.ErrWeakPassword):
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", err.Error(),
			map[string]string{"new_password": "policy"})
	default:
		s.writeAuthError(w, r, err)
	}
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		WriteProblem(w, r, http.StatusUnauthorized, "auth.missing_token", "Authentication is required.")
		return
	}
	user, err := s.auth.Users.GetByID(r.Context(), p.UserID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			WriteProblem(w, r, http.StatusUnauthorized, "auth.unknown_user", "This account no longer exists.")
			return
		}
		WriteProblem(w, r, http.StatusInternalServerError, "internal", "Could not load the account.")
		return
	}
	WriteJSON(w, http.StatusOK, meResponse{
		ID:                 user.ID.String(),
		Username:           user.Username,
		Email:              user.Email,
		DisplayName:        user.DisplayName,
		Role:               string(user.Role),
		TOTPEnabled:        user.TOTPEnabled,
		MustChangePassword: user.MustChangePassword,
	})
}

// writeAuthError maps identity errors to responses. Credential problems are
// deliberately indistinguishable to the client; the audit log holds the detail.
func (s *Server) writeAuthError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, identity.ErrAccountLocked):
		WriteProblem(w, r, http.StatusLocked, "auth.account_locked",
			"Too many failed attempts. Try again later or contact an administrator.")
	case errors.Is(err, identity.ErrInvalidCredentials),
		errors.Is(err, identity.ErrAccountInactive),
		errors.Is(err, identity.ErrSessionExpired),
		errors.Is(err, identity.ErrSessionRevoked),
		errors.Is(err, identity.ErrRefreshTokenReuse):
		WriteProblem(w, r, http.StatusUnauthorized, "auth.invalid_credentials", "Invalid credentials.")
	default:
		s.log.Error().Err(err).Msg("authentication failed")
		WriteProblem(w, r, http.StatusInternalServerError, "internal", "Authentication could not be completed.")
	}
}

// cookieIsSecure decides whether the refresh cookie carries the Secure flag.
//
// The configured value is a floor, not the whole answer. A request that
// arrived over TLS always gets a Secure cookie, whatever the setting says:
// `PROXUI_SECURE_COOKIES=false` exists so a portal on a plain-HTTP LAN can be
// signed into at all — a Secure cookie is never sent back over HTTP, so
// nobody could — and that is a statement about the HTTP address, not a request
// to strip the flag from an HTTPS one. Deciding per request rather than at
// boot is what lets one deployment serve both, which is exactly the
// configuration where getting this wrong is invisible.
func (s *Server) cookieIsSecure(r *http.Request) bool {
	return s.secureCookies || isTLS(r)
}

func (s *Server) setRefreshCookie(w http.ResponseWriter, r *http.Request, token, _ string) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    token,
		Path:     refreshCookiePath,
		HttpOnly: true,
		Secure:   s.cookieIsSecure(r),
		SameSite: http.SameSiteStrictMode,
		Expires:  s.clock().Add(identity.RefreshTokenTTL),
		MaxAge:   int(identity.RefreshTokenTTL / time.Second),
	})
}

func (s *Server) clearRefreshCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     refreshCookiePath,
		HttpOnly: true,
		// The flag has to match the one it was set with, or the browser keeps
		// the cookie it was told to drop.
		Secure:   s.cookieIsSecure(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// decodeJSON reads a JSON body, rejecting unknown fields and oversized payloads.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "malformed_json", "The request body could not be parsed.")
		return err
	}
	return nil
}
