package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/infra/crypto"
	"github.com/freezxp/proxui/internal/infra/metrics"
)

// MFA owns second-factor enrolment and verification (AUTH-04).
//
// The shape to notice: enrolment is three steps, not one. A seed is generated
// and stored disabled, a code proves the device actually holds it, and only
// then does the factor start being demanded. Enabling on the first step would
// lock an account out of its own portal the moment somebody scanned a QR code
// badly, and the recovery for that is an administrator.
type MFA struct {
	Users      ports.UserRepository
	TOTP       ports.TOTPStore
	Codec      ports.TOTPService
	Challenges ports.MFAChallengeStore
	Hasher     ports.PasswordHasher
	Issue      *IssueSession
	Vault      *crypto.Vault
	Audit      ports.AuditWriter
	Clock      ports.Clock
}

// EnrollTOTPOutput is what the browser needs to show a QR code. The secret is
// returned in the clear on purpose — it is the one moment it has to be, since
// the whole point is getting it into an authenticator app — and it is returned
// only to the account it belongs to, over an authenticated request.
type EnrollTOTPOutput struct {
	Secret     string
	OTPAuthURL string
}

// Begin starts an enrolment, replacing any unconfirmed one.
func (h *MFA) Begin(ctx context.Context, actor Actor) (EnrollTOTPOutput, error) {
	user, err := h.Users.GetByID(ctx, actor.UserID)
	if err != nil {
		return EnrollTOTPOutput{}, err
	}
	// Swapping a working factor requires disabling the old one with a
	// password first, so that someone at a borrowed, still-signed-in desk
	// cannot quietly replace it with their own device.
	if user.TOTPEnabled {
		return EnrollTOTPOutput{}, identity.ErrTOTPAlreadyEnabled
	}

	secret, err := h.Codec.NewSecret()
	if err != nil {
		return EnrollTOTPOutput{}, err
	}
	sealed, err := h.Vault.Seal(secret)
	if err != nil {
		return EnrollTOTPOutput{}, fmt.Errorf("seal totp secret: %w", err)
	}
	now := h.Clock.Now()
	if err := h.TOTP.Enroll(ctx, user.ID, sealed, now); err != nil {
		return EnrollTOTPOutput{}, err
	}

	// Audited even though nothing is protected yet: "when did this account
	// start setting up a factor" is the question asked after an account is
	// found to have one nobody recognises.
	h.audit(ctx, actor, now, "mfa_enrollment_started", ports.OutcomeSuccess, nil)

	return EnrollTOTPOutput{
		Secret:     secret,
		OTPAuthURL: h.Codec.URL(user.Username, secret),
	}, nil
}

// Confirm turns on a factor whose code has been proved.
func (h *MFA) Confirm(ctx context.Context, actor Actor, code string) error {
	user, err := h.Users.GetByID(ctx, actor.UserID)
	if err != nil {
		return err
	}
	if user.TOTPEnabled {
		return identity.ErrTOTPAlreadyEnabled
	}

	secret, err := h.TOTP.Secret(ctx, user.ID, h.Vault)
	if errors.Is(err, ports.ErrNotFound) {
		return identity.ErrTOTPNotEnrolled
	}
	if err != nil {
		return err
	}

	now := h.Clock.Now()
	matched, ok := h.Codec.Validate(secret, code, now)
	if !ok {
		h.audit(ctx, actor, now, "mfa_enrollment_failed", ports.OutcomeFailure,
			map[string]any{"reason": "bad_code"})
		return identity.ErrInvalidTOTPCode
	}

	// The confirming code is spent by enabling: it must not double as the
	// first sign-in code a moment later.
	if err := h.TOTP.Enable(ctx, user.ID, matched, now); err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			return identity.ErrTOTPNotEnrolled
		}
		return err
	}
	h.audit(ctx, actor, now, "mfa_enabled", ports.OutcomeSuccess, nil)
	return nil
}

// Disable turns the factor off, and needs the account's password.
//
// The password is the point of the endpoint. Without it, an unlocked screen is
// enough to strip the second factor off an account, which would make the
// factor worth exactly as much as the session cookie it was meant to backstop.
func (h *MFA) Disable(ctx context.Context, actor Actor, password string) error {
	user, err := h.Users.GetByID(ctx, actor.UserID)
	if err != nil {
		return err
	}
	now := h.Clock.Now()

	if user.UsesPassword() {
		ok, err := h.Hasher.Verify(password, user.PasswordHash)
		if err != nil {
			return fmt.Errorf("disable totp: verify password: %w", err)
		}
		if !ok {
			h.audit(ctx, actor, now, "mfa_disable_failed", ports.OutcomeFailure,
				map[string]any{"reason": "bad_password"})
			return identity.ErrInvalidCredentials
		}
	}

	// Nothing enrolled is not an error worth failing on: an unconfirmed
	// enrolment is exactly what someone abandoning the flow wants cleared. It
	// is not audited as a disable, because nothing was ever on.
	if err := h.TOTP.Disable(ctx, user.ID, now); err != nil {
		return err
	}
	if user.TOTPEnabled {
		h.audit(ctx, actor, now, "mfa_disabled", ports.OutcomeSuccess, nil)
	}
	return nil
}

// Reset clears another account's factor. Administrators only: this is the
// recovery path for a lost phone, and it is the one operation here that
// weakens an account somebody else owns, so it is audited against both.
func (h *MFA) Reset(ctx context.Context, actor Actor, targetID uuid.UUID) error {
	target, err := h.Users.GetByID(ctx, targetID)
	if err != nil {
		return err
	}
	now := h.Clock.Now()
	if err := h.TOTP.Disable(ctx, target.ID, now); err != nil {
		return err
	}

	actorID := actor.UserID
	_ = h.Audit.Write(ctx, ports.AuditEntry{
		Time: now, ActorUserID: &actorID, ActorName: actor.Username,
		Category: ports.AuditCategoryUserMgmt, Action: "mfa_reset",
		TargetType: "user", TargetID: target.ID.String(), TargetName: target.Username,
		SourceIP: actor.IP, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
		Outcome: ports.OutcomeSuccess,
		Details: map[string]any{"was_enabled": target.TOTPEnabled},
	})
	return nil
}

// VerifyMFAInput completes a login that is waiting on a code.
type VerifyMFAInput struct {
	ChallengeID string
	Code        string
	IP          string
	UserAgent   string
	RequestID   string
}

// Verify checks the code and mints the session the password step withheld.
//
// Every failure returns the same two errors — the challenge is gone, or the
// code is wrong — because the alternative tells an attacker holding a stolen
// password whether they have the right account, the right challenge, or simply
// the wrong six digits.
func (h *MFA) Verify(ctx context.Context, in VerifyMFAInput) (LoginOutput, error) {
	now := h.Clock.Now()

	challenge, err := h.Challenges.Get(ctx, in.ChallengeID)
	if errors.Is(err, ports.ErrNotFound) {
		return LoginOutput{}, identity.ErrMFAChallengeNotFound
	}
	if err != nil {
		return LoginOutput{}, err
	}
	if challenge.Expired(now) || challenge.Exhausted() {
		_ = h.Challenges.Consume(ctx, in.ChallengeID)
		return LoginOutput{}, identity.ErrMFAChallengeNotFound
	}

	user, err := h.Users.GetByID(ctx, challenge.UserID)
	if err != nil {
		return LoginOutput{}, err
	}
	// Re-checked here, not only at the password step: an account disabled or
	// locked during the seconds someone spent reaching for their phone must
	// not still get in.
	if err := user.CanAuthenticate(now); err != nil {
		_ = h.Challenges.Consume(ctx, in.ChallengeID)
		return LoginOutput{}, err
	}

	secret, err := h.TOTP.Secret(ctx, user.ID, h.Vault)
	if errors.Is(err, ports.ErrNotFound) {
		return LoginOutput{}, identity.ErrTOTPNotEnrolled
	}
	if err != nil {
		return LoginOutput{}, err
	}

	matched, ok := h.Codec.Validate(secret, in.Code, now)
	if ok {
		lastStep, err := h.TOTP.LastStep(ctx, user.ID)
		if err != nil {
			return LoginOutput{}, err
		}
		if identity.TOTPReplayed(lastStep, matched) {
			// Correct arithmetic, already spent. Treated as a wrong code and
			// counted as one: a replay is either a bug in the client or
			// somebody reusing a code they watched being typed.
			ok = false
		}
	}
	if !ok {
		return LoginOutput{}, h.failVerify(ctx, in, user, now)
	}

	if err := h.TOTP.RecordStep(ctx, user.ID, matched, now); err != nil {
		return LoginOutput{}, err
	}
	if err := h.Challenges.Consume(ctx, in.ChallengeID); err != nil {
		return LoginOutput{}, err
	}

	// The session is minted by the same path every other non-password sign-in
	// uses; the password half was decided before the challenge was issued.
	out, err := h.Issue.Issue(ctx, user, in.IP, in.UserAgent, in.RequestID)
	if err != nil {
		return LoginOutput{}, err
	}

	actorID := user.ID
	_ = h.Audit.Write(ctx, ports.AuditEntry{
		Time: now, ActorUserID: &actorID, ActorName: user.Username,
		Category: ports.AuditCategoryAuth, Action: "mfa_verified",
		Outcome: ports.OutcomeSuccess, SourceIP: in.IP, UserAgent: in.UserAgent,
		RequestID: in.RequestID,
	})
	return out, nil
}

// failVerify records the wrong code and decides whether the challenge survives.
func (h *MFA) failVerify(ctx context.Context, in VerifyMFAInput, user *identity.User,
	now time.Time) error {
	metrics.LoginFailures.Inc()

	after, err := h.Challenges.Fail(ctx, in.ChallengeID)
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		return err
	}

	actorID := user.ID
	_ = h.Audit.Write(ctx, ports.AuditEntry{
		Time: now, ActorUserID: &actorID, ActorName: user.Username,
		Category: ports.AuditCategoryAuth, Action: "mfa_failed",
		Outcome: ports.OutcomeFailure, SourceIP: in.IP, UserAgent: in.UserAgent,
		RequestID: in.RequestID,
		Details:   map[string]any{"attempts": after.Attempts},
	})

	if after.Exhausted() {
		// The challenge is gone; saying so sends the browser back to the
		// password form rather than letting someone keep guessing at a prompt
		// that will never accept anything again.
		return identity.ErrMFAChallengeNotFound
	}
	return identity.ErrInvalidTOTPCode
}

func (h *MFA) audit(ctx context.Context, actor Actor, now time.Time, action, outcome string,
	details map[string]any) {
	actorID := actor.UserID
	_ = h.Audit.Write(ctx, ports.AuditEntry{
		Time: now, ActorUserID: &actorID, ActorName: actor.Username,
		Category: ports.AuditCategorySecurity, Action: action,
		TargetType: "user", TargetID: actor.UserID.String(), TargetName: actor.Username,
		SourceIP: actor.IP, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
		Outcome: outcome, Details: details,
	})
}
