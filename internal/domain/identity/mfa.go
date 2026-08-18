package identity

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Second-factor authentication (AUTH-04).
//
// The rules here exist because a second factor that can be brute-forced,
// replayed or waited out is decoration. Three of them do the work: a code is
// six digits over a ninety-second window, so the number of attempts must be
// small; a correct code is correct for that whole window, so a used one must
// not be accepted again; and the half-finished login between password and code
// must expire on its own rather than sit there indefinitely.
const (
	// MFAChallengeTTL bounds the gap between a correct password and a correct
	// code. Long enough to find a phone, short enough that an unattended
	// browser is not a standing invitation.
	MFAChallengeTTL = 5 * time.Minute

	// MaxMFAAttempts is how many codes may be tried against one challenge.
	// Six digits is a million possibilities; five guesses per challenge, with
	// the challenge dying afterwards, keeps guessing hopeless without
	// punishing a typo.
	MaxMFAAttempts = 5
)

var (
	// ErrMFARequired means the password was right and a code is still needed.
	// It is not an authentication failure and must not be reported as one: the
	// caller has to know to ask for the second factor.
	ErrMFARequired = errors.New("identity: a second factor is required")
	// ErrMFAChallengeNotFound covers an unknown, expired or already-spent
	// challenge. The three are not distinguished, for the same reason a login
	// failure does not say which half was wrong.
	ErrMFAChallengeNotFound = errors.New("identity: that sign-in attempt is no longer valid")
	// ErrInvalidTOTPCode is a code that did not match, or matched a step that
	// has already been used.
	ErrInvalidTOTPCode = errors.New("identity: that code is not valid")
	// ErrTOTPNotEnrolled is returned when confirming or disabling a factor
	// that was never set up.
	ErrTOTPNotEnrolled = errors.New("identity: this account has no authenticator enrolled")
	// ErrTOTPAlreadyEnabled refuses a second enrolment over a working one.
	// Replacing a factor means disabling the old one with a password first,
	// so that someone at a borrowed desk cannot quietly swap it for their own.
	ErrTOTPAlreadyEnabled = errors.New("identity: an authenticator is already enrolled")
)

// MFAChallenge is a login that has passed its password check and is waiting
// for a code.
//
// It deliberately carries no token of its own beyond the id: the id is the
// bearer, it is single-use, it lives in a store that expires it, and it is
// worth nothing without a code that only the enrolled device can produce.
type MFAChallenge struct {
	ID        uuid.UUID `json:"id"`
	UserID    uuid.UUID `json:"user_id"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// Attempts counts codes already tried. The store owns the increment; this
	// field is what a reader sees.
	Attempts int `json:"attempts"`
	// IP and UserAgent are carried from the password step so the session that
	// eventually gets minted, and the audit entry beside it, describe the
	// request that actually started the sign-in.
	IP        string `json:"ip,omitempty"`
	UserAgent string `json:"user_agent,omitempty"`
}

// NewMFAChallenge starts a challenge for a user.
func NewMFAChallenge(userID uuid.UUID, ip, userAgent string, now time.Time) MFAChallenge {
	return MFAChallenge{
		ID: uuid.New(), UserID: userID, IssuedAt: now,
		ExpiresAt: now.Add(MFAChallengeTTL), IP: ip, UserAgent: userAgent,
	}
}

// Expired reports whether the challenge is past its life. The store expires it
// too; this is the check for a reader holding one it fetched a moment ago.
func (c MFAChallenge) Expired(now time.Time) bool { return !now.Before(c.ExpiresAt) }

// Exhausted reports whether too many codes have been tried.
func (c MFAChallenge) Exhausted() bool { return c.Attempts >= MaxMFAAttempts }

// TOTPReplayed reports whether a code matching this step has already been
// accepted for this account.
//
// RFC 6238 codes are valid for a whole step, and the portal accepts one step
// either side, so without this check a code observed over a shoulder stays
// usable for up to ninety seconds. Refusing a step at or below the last one
// accepted makes each code good exactly once.
func TOTPReplayed(lastStep *int64, matched int64) bool {
	return lastStep != nil && matched <= *lastStep
}
