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
