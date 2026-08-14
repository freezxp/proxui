package identity

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MinPasswordLength is the only length rule: length beats composition theater
// (docs/15-security-design.md).
const MinPasswordLength = 12

// ErrWeakPassword is returned when a password fails policy.
var ErrWeakPassword = errors.New("identity: password does not meet policy")

// ErrPasswordUnchanged rejects a change that changes nothing. A forced change
// satisfied by re-entering the same password would defeat the point of forcing
// it (AUTH-08).
var ErrPasswordUnchanged = errors.New("identity: the new password matches the current one")

// PasswordPolicyError describes why a password was rejected, so the API can
// return a useful field error instead of a generic refusal.
type PasswordPolicyError struct{ Reason string }

func (e *PasswordPolicyError) Error() string { return "identity: " + e.Reason }
func (e *PasswordPolicyError) Unwrap() error { return ErrWeakPassword }

// ValidatePassword enforces the password policy (AUTH-08). The breached
// password list check is layered on top of this in the application service,
// where the list is available.
func ValidatePassword(password, username, email string) error {
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return &PasswordPolicyError{Reason: fmt.Sprintf("password must be at least %d characters", MinPasswordLength)}
	}
	lower := strings.ToLower(password)
	if username != "" && strings.Contains(lower, strings.ToLower(username)) {
		return &PasswordPolicyError{Reason: "password must not contain the username"}
	}
	if local, _, ok := strings.Cut(email, "@"); ok && local != "" && strings.Contains(lower, strings.ToLower(local)) {
		return &PasswordPolicyError{Reason: "password must not contain the email address"}
	}
	return nil
}

// ErrInvalidUsername and ErrInvalidEmail reject account details that could not
// be used to sign in, or that would be confusing to see in an audit trail.
var (
	ErrInvalidUsername = errors.New("identity: invalid username")
	ErrInvalidEmail    = errors.New("identity: invalid email address")
)

// MaxUsernameLength keeps a username readable in tables and audit entries.
const MaxUsernameLength = 32

// ValidateUsername accepts a modest, unambiguous set of characters.
//
// Deliberately narrow: a username appears in the audit trail, and one
// containing spaces, control characters or lookalike Unicode makes that record
// harder to read and easier to spoof.
func ValidateUsername(username string) error {
	if len(username) < 3 || len(username) > MaxUsernameLength {
		return fmt.Errorf("%w: between 3 and %d characters", ErrInvalidUsername, MaxUsernameLength)
	}
	for _, r := range username {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return fmt.Errorf("%w: use lowercase letters, digits, dot, dash or underscore", ErrInvalidUsername)
		}
	}
	return nil
}

// ValidateEmail checks the shape of an address rather than its deliverability,
// which only sending to it can establish.
func ValidateEmail(email string) error {
	if len(email) < 3 || len(email) > 254 {
		return fmt.Errorf("%w: that is not an email address", ErrInvalidEmail)
	}
	local, domain, found := strings.Cut(email, "@")
	if !found || local == "" || domain == "" {
		return fmt.Errorf("%w: that is not an email address", ErrInvalidEmail)
	}
	if !strings.Contains(domain, ".") || strings.ContainsAny(email, " \t\r\n") {
		return fmt.Errorf("%w: that is not an email address", ErrInvalidEmail)
	}
	return nil
}
