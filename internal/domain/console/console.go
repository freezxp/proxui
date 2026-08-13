// Package console is the Console bounded context: the rules governing browser
// access to a VM's keyboard and screen.
//
// This is the portal's most sensitive capability, so the rules here are
// deliberately strict: a ticket is single-use and short-lived, a session is
// bound to one user and one VM, and idle sessions do not linger.
package console

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Kind selects a console protocol.
type Kind string

const (
	KindVNC    Kind = "vnc"
	KindSerial Kind = "serial"
)

// Valid reports whether k is a supported console kind.
func (k Kind) Valid() bool { return k == KindVNC || k == KindSerial }

// Lifetimes (CONS-03, CONS-05).
const (
	// TicketTTL is how long an unused console ticket stays valid. It only has
	// to survive the browser opening a WebSocket, so it is deliberately short:
	// a leaked ticket is a leaked console.
	TicketTTL = 60 * time.Second

	// IdleTimeout closes a console nobody is using. Someone who walks away
	// from an open root shell should not leave it open indefinitely.
	IdleTimeout = 30 * time.Minute

	// MaxDuration is the hard ceiling on any session, however active.
	MaxDuration = 8 * time.Hour
)

// Close reasons recorded in the audit trail.
const (
	ReasonUser        = "user"
	ReasonIdle        = "idle_timeout"
	ReasonMaxDuration = "max_duration"
	ReasonAdminForced = "admin_forced"
	ReasonUpstream    = "upstream_lost"
	ReasonError       = "error"
)

// Domain errors.
var (
	// ErrTicketNotFound covers an unknown, expired or already-used ticket.
	// The three are not distinguished: an attacker probing ticket ids learns
	// nothing from the difference.
	ErrTicketNotFound = errors.New("console: ticket is not valid")
	// ErrNotPermitted means the caller may not open consoles on this VM.
	ErrNotPermitted = errors.New("console: not permitted")
	// ErrUnsupported means the platform offers no console of this kind.
	ErrUnsupported = errors.New("console: not supported by this platform")
)

// Ticket is the one-time handle a browser exchanges for a live console.
//
// It exists so the VM id, the upstream address and the platform credential
// never reach the browser: the client gets an opaque id, and the portal keeps
// everything else server-side.
type Ticket struct {
	ID        uuid.UUID
	SessionID uuid.UUID
	UserID    uuid.UUID
	VMID      uuid.UUID
	Kind      Kind
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// NewTicket mints a ticket for a session.
func NewTicket(sessionID, userID, vmID uuid.UUID, kind Kind, now time.Time) Ticket {
	return Ticket{
		ID:        uuid.New(),
		SessionID: sessionID,
		UserID:    userID,
		VMID:      vmID,
		Kind:      kind,
		IssuedAt:  now,
		ExpiresAt: now.Add(TicketTTL),
	}
}

// Expired reports whether the ticket may still be exchanged.
func (t Ticket) Expired(now time.Time) bool { return !now.Before(t.ExpiresAt) }

// Session is a recorded console connection.
type Session struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	VMID        uuid.UUID
	Kind        Kind
	ClientIP    string
	UserAgent   string
	StartedAt   time.Time
	ConnectedAt time.Time
	EndedAt     time.Time
	CloseReason string
	BytesTx     int64
	BytesRx     int64
}

// Active reports whether the session is still open.
func (s *Session) Active() bool { return s.EndedAt.IsZero() }

// Duration reports how long the session lasted, or has lasted so far.
func (s *Session) Duration(now time.Time) time.Duration {
	if s.EndedAt.IsZero() {
		return now.Sub(s.StartedAt)
	}
	return s.EndedAt.Sub(s.StartedAt)
}

// ExpiryCheck reports whether an open session must be closed now, and why.
//
// Both limits are enforced on the server rather than trusted to the browser: a
// client that stops sending heartbeats must not thereby keep a console open.
func ExpiryCheck(startedAt, lastActivity time.Time, now time.Time) (reason string, expired bool) {
	switch {
	case now.Sub(startedAt) >= MaxDuration:
		return ReasonMaxDuration, true
	case now.Sub(lastActivity) >= IdleTimeout:
		return ReasonIdle, true
	default:
		return "", false
	}
}
