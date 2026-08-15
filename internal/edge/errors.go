package edge

import (
	"errors"
	"fmt"
)

// Error classes. Callers choose retry, refuse or surface-to-admin from these
// and never from error strings — the same discipline as internal/connector,
// and for the same reason: a message is for a human, a class is for code.
var (
	// ErrUnreachable is a transport failure: nothing answered. Retryable.
	ErrUnreachable = errors.New("edge: provider unreachable")
	// ErrAuth means the token was rejected. Retrying cannot help.
	ErrAuth = errors.New("edge: authentication failed")
	// ErrPermission means the token is valid but lacks a scope.
	ErrPermission = errors.New("edge: token lacks a required permission")
	// ErrRefused means the provider answered and declined — the same
	// distinction internal/connector needed after a Proxmox 500 spent a
	// release being reported as an unreachable cluster.
	ErrRefused = errors.New("edge: provider refused the request")
	// ErrThrottled means the provider asked us to slow down.
	ErrThrottled = errors.New("edge: throttled by provider")
	// ErrNotManageable means the target cannot be configured through the API
	// at all — a locally-managed tunnel. No amount of permission fixes it;
	// the tunnel itself has to be recreated differently.
	ErrNotManageable = errors.New("edge: target is not remotely managed")
	// ErrConflict means the configuration changed between our read and our
	// write. Never resolved by retrying the same write: the whole point is
	// that somebody else's change would be lost.
	ErrConflict = errors.New("edge: configuration changed since it was read")
	// ErrInvalidConfig marks configuration that cannot work.
	ErrInvalidConfig = errors.New("edge: invalid configuration")
)

// Error wraps a class with context an operator can act on.
type Error struct {
	Class   error
	Op      string // e.g. "list_tunnels", "put_ingress"
	Detail  string
	wrapped error
}

// Errorf builds a classified error.
func Errorf(class error, op, format string, args ...any) *Error {
	return &Error{Class: class, Op: op, Detail: fmt.Sprintf(format, args...)}
}

// Wrap classifies an underlying error.
func Wrap(class error, op string, err error) *Error {
	return &Error{Class: class, Op: op, Detail: err.Error(), wrapped: err}
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s (%s): %s", e.Class, e.Op, e.Detail)
}

// Is lets errors.Is match the class sentinel.
func (e *Error) Is(target error) bool { return errors.Is(e.Class, target) }

// Unwrap exposes the underlying error, if any.
func (e *Error) Unwrap() error { return e.wrapped }

// Retryable reports whether retrying the identical call could plausibly
// succeed. Everything else burns quota and delays the alert someone needs.
func Retryable(err error) bool {
	switch {
	case errors.Is(err, ErrUnreachable), errors.Is(err, ErrThrottled):
		return true
	default:
		return false
	}
}
