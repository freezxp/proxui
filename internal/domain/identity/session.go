package identity

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Session lifetimes (AUTH-02). Access tokens are short so revocation is cheap;
// refresh tokens are long-lived but single-use.
const (
	AccessTokenTTL  = 15 * time.Minute
	RefreshTokenTTL = 7 * 24 * time.Hour
)

var (
	// ErrSessionExpired is returned for a refresh token past its expiry.
	ErrSessionExpired = errors.New("identity: session expired")
	// ErrSessionRevoked is returned for a refresh token belonging to a revoked
	// session — including one revoked by reuse detection.
	ErrSessionRevoked = errors.New("identity: session revoked")
	// ErrRefreshTokenReuse signals that an already-rotated token was presented,
	// which means the token leaked. The whole family must be revoked (AUTH-03).
	ErrRefreshTokenReuse = errors.New("identity: refresh token reuse detected")
)

// Session is one link in a refresh-token chain. Rotation creates a new session
// in the same family; presenting a rotated (already used) token is treated as
// theft rather than a mistake.
type Session struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	FamilyID  uuid.UUID
	TokenHash []byte
	IP        string
	UserAgent string
	ExpiresAt time.Time
	RotatedAt time.Time
	RevokedAt time.Time
	CreatedAt time.Time
}

// NewSession starts a new refresh-token family for a fresh login.
func NewSession(userID uuid.UUID, tokenHash []byte, ip, userAgent string, now time.Time) *Session {
	return &Session{
		ID:        uuid.New(),
		UserID:    userID,
		FamilyID:  uuid.New(),
		TokenHash: tokenHash,
		IP:        ip,
		UserAgent: userAgent,
		ExpiresAt: now.Add(RefreshTokenTTL),
		CreatedAt: now,
	}
}

// IsRevoked reports whether the session has been revoked.
func (s *Session) IsRevoked() bool { return !s.RevokedAt.IsZero() }

// IsRotated reports whether this session's token has already been exchanged.
func (s *Session) IsRotated() bool { return !s.RotatedAt.IsZero() }

// Validate checks whether this session may be exchanged for a new token pair.
// A rotated session means the presented token was already spent: the caller
// must revoke the entire family and raise a security event.
func (s *Session) Validate(now time.Time) error {
	switch {
	case s.IsRotated():
		return ErrRefreshTokenReuse
	case s.IsRevoked():
		return ErrSessionRevoked
	case !now.Before(s.ExpiresAt):
		return ErrSessionExpired
	}
	return nil
}

// Rotate marks this session spent and returns its successor in the same family.
// The successor inherits the original expiry so that rotation cannot extend a
// session indefinitely.
func (s *Session) Rotate(newTokenHash []byte, ip, userAgent string, now time.Time) *Session {
	s.RotatedAt = now
	return &Session{
		ID:        uuid.New(),
		UserID:    s.UserID,
		FamilyID:  s.FamilyID,
		TokenHash: newTokenHash,
		IP:        ip,
		UserAgent: userAgent,
		ExpiresAt: s.ExpiresAt,
		CreatedAt: now,
	}
}

// Revoke marks the session unusable.
func (s *Session) Revoke(now time.Time) {
	if s.RevokedAt.IsZero() {
		s.RevokedAt = now
	}
}
