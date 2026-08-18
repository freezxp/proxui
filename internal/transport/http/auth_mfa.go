package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/freezxp/proxui/internal/app/command"
)

// Second-factor endpoints (AUTH-04).
//
// The division of labour: /auth/mfa completes a sign-in and is reachable
// without a session, because by definition the caller has not got one yet.
// Everything under /auth/me/totp changes the caller's own enrolment and needs
// a session. Resetting somebody else's is an administrator's, and lives with
// the other user-management routes.

// MFAHandler is the command behind the second-factor endpoints. An interface so
// the transport tests can substitute a fake, like every other handler here.
type MFAHandler interface {
	Begin(ctx context.Context, actor command.Actor) (command.EnrollTOTPOutput, error)
	Confirm(ctx context.Context, actor command.Actor, code string) error
	Disable(ctx context.Context, actor command.Actor, password string) error
	Verify(ctx context.Context, in command.VerifyMFAInput) (command.LoginOutput, error)
}

type verifyMFARequest struct {
	MFAToken string `json:"mfa_token"`
	Code     string `json:"code"`
}

type totpCodeRequest struct {
	Code string `json:"code"`
}

type totpPasswordRequest struct {
	Password string `json:"password"`
}

// handleVerifyMFA completes a login that stopped at the password.
func (s *Server) handleVerifyMFA(w http.ResponseWriter, r *http.Request) {
	var req verifyMFARequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if req.MFAToken == "" || req.Code == "" {
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
			"A sign-in token and a code are required.",
			map[string]string{"mfa_token": "required", "code": "required"})
		return
	}

	out, err := s.auth.MFA.Verify(r.Context(), command.VerifyMFAInput{
		ChallengeID: req.MFAToken,
		Code:        req.Code,
		IP:          r.RemoteAddr,
		UserAgent:   r.UserAgent(),
		RequestID:   middleware.GetReqID(r.Context()),
	})
	if err != nil {
		s.writeAuthError(w, r, err)
		return
	}

	// Identical to a password-only success from here: same cookie, same body,
	// so the browser has one code path for "signed in".
	s.setRefreshCookie(w, r, out.RefreshToken, out.User.ID.String())
	WriteJSON(w, http.StatusOK, tokenResponse{
		AccessToken: out.AccessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int(out.ExpiresIn / time.Second),
	})
}

// handleBeginTOTP starts enrolment and returns what a QR code is drawn from.
func (s *Server) handleBeginTOTP(w http.ResponseWriter, r *http.Request) {
	out, err := s.auth.MFA.Begin(r.Context(), s.actor(r))
	if err != nil {
		s.writeAuthError(w, r, err)
		return
	}
	// The secret travels once, to the account it belongs to, over an
	// authenticated request. There is no way to get it into an authenticator
	// app otherwise, and no endpoint that will show it again.
	WriteJSON(w, http.StatusOK, map[string]any{
		"secret":      out.Secret,
		"otpauth_url": out.OTPAuthURL,
		"digits":      6,
		"period":      30,
	})
}

// handleConfirmTOTP turns the factor on once a code proves the device has it.
func (s *Server) handleConfirmTOTP(w http.ResponseWriter, r *http.Request) {
	var req totpCodeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if req.Code == "" {
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
			"A code from your authenticator app is required.",
			map[string]string{"code": "required"})
		return
	}
	if err := s.auth.MFA.Confirm(r.Context(), s.actor(r), req.Code); err != nil {
		s.writeAuthError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDisableTOTP turns the factor off, on production of the password.
func (s *Server) handleDisableTOTP(w http.ResponseWriter, r *http.Request) {
	var req totpPasswordRequest
	// A body is required but may be empty for a provider account, which has no
	// password to give; the command decides whether that is acceptable.
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if err := s.auth.MFA.Disable(r.Context(), s.actor(r), req.Password); err != nil {
		s.writeAuthError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleResetUserTOTP clears another account's factor: the lost-phone path.
func (s *Server) handleResetUserTOTP(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "userID")
	if !ok {
		return
	}
	if err := s.admin.MFA.Reset(r.Context(), s.actor(r), id); err != nil {
		s.writeAdminError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
