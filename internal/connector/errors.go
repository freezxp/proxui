package connector

import (
	"errors"
	"fmt"
)

// Error classes. The sync engine chooses retry, circuit-break or surface-to-
// admin behaviour from these, never from error strings (docs/17 §17.2).
var (
	// ErrUnreachable is a network-level failure: retry with backoff.
	ErrUnreachable = errors.New("connector: platform unreachable")
	// ErrAuth means the credentials were rejected. Retrying cannot help, so
	// the engine breaks the circuit and notifies an administrator.
	ErrAuth = errors.New("connector: authentication failed")
	// ErrPermission means the credentials are valid but lack a privilege.
	ErrPermission = errors.New("connector: insufficient permissions")
	// ErrThrottled means the platform asked us to slow down.
	ErrThrottled = errors.New("connector: throttled by platform")
	// ErrRefused means the platform was reached, understood the request, and
	// refused it — a VM already in the state being asked for, a lock held by
	// another task, a full storage pool. Distinct from ErrUnreachable because
	// retrying cannot help and because telling an operator the platform could
	// not be reached, when it answered, sends them to debug the network.
	ErrRefused = errors.New("connector: platform refused the operation")
	// ErrNotSupported means the platform cannot do this at all.
	ErrNotSupported = errors.New("connector: capability not supported")
	// ErrInvalidConfig marks configuration that cannot work.
	ErrInvalidConfig = errors.New("connector: invalid configuration")
)

// Error wraps a class with context an operator can act on.
type Error struct {
	Class   error
	Op      string // e.g. "list_vms", "vncproxy"
	Detail  string
	wrapped error
}

// Errorf builds a classified connector error.
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

// Is lets errors.Is match against the class sentinel.
func (e *Error) Is(target error) bool { return errors.Is(e.Class, target) }

// Unwrap exposes the underlying error, if any.
func (e *Error) Unwrap() error { return e.wrapped }

// Retryable reports whether retrying the same call could plausibly succeed.
// Auth and permission failures are excluded on purpose: retrying them just
// burns quota and delays the alert an administrator needs to see.
func Retryable(err error) bool {
	switch {
	case errors.Is(err, ErrUnreachable), errors.Is(err, ErrThrottled):
		return true
	default:
		return false
	}
}
