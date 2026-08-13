package identity

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

var now = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

func TestRegisterFailedLoginLocksAtThreshold(t *testing.T) {
	u := &User{IsActive: true}

	for i := 1; i < MaxFailedLogins; i++ {
		if locked := u.RegisterFailedLogin(now); locked {
			t.Fatalf("locked after %d failures, want lock only at %d", i, MaxFailedLogins)
		}
		if u.IsLocked(now) {
			t.Fatalf("IsLocked = true after %d failures", i)
		}
	}

	if locked := u.RegisterFailedLogin(now); !locked {
		t.Fatalf("failure %d did not lock the account", MaxFailedLogins)
	}
	if !u.IsLocked(now) {
		t.Error("IsLocked = false immediately after lockout")
	}
	if u.IsLocked(now.Add(LockoutDuration)) {
		t.Error("IsLocked = true once the lockout has elapsed")
	}
}

func TestFailuresOutsideWindowDoNotAccumulate(t *testing.T) {
	u := &User{IsActive: true}

	for i := 0; i < MaxFailedLogins-1; i++ {
		u.RegisterFailedLogin(now)
	}
	// A failure long after the window starts a fresh count rather than
	// combining with stale ones.
	later := now.Add(FailureWindow + time.Minute)
	if locked := u.RegisterFailedLogin(later); locked {
		t.Fatal("stale failures were counted toward the lockout threshold")
	}
	if u.FailedLoginCount != 1 {
		t.Errorf("FailedLoginCount = %d, want 1", u.FailedLoginCount)
	}
}

func TestSuccessfulLoginClearsFailureState(t *testing.T) {
	u := &User{IsActive: true}
	u.RegisterFailedLogin(now)
	u.RegisterSuccessfulLogin(now)

	if u.FailedLoginCount != 0 || !u.LockedUntil.IsZero() {
		t.Errorf("failure state survived a successful login: count=%d lockedUntil=%v", u.FailedLoginCount, u.LockedUntil)
	}
	if !u.LastLoginAt.Equal(now) {
		t.Errorf("LastLoginAt = %v, want %v", u.LastLoginAt, now)
	}
}

func TestCanAuthenticate(t *testing.T) {
	tests := []struct {
		name string
		user *User
		want error
	}{
		{"active user", &User{IsActive: true}, nil},
		{"inactive user", &User{IsActive: false}, ErrAccountInactive},
		{"locked user", &User{IsActive: true, LockedUntil: now.Add(time.Minute)}, ErrAccountLocked},
		{"expired lock", &User{IsActive: true, LockedUntil: now.Add(-time.Minute)}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.user.CanAuthenticate(now); !errors.Is(err, tt.want) {
				t.Errorf("CanAuthenticate() = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestRoleValid(t *testing.T) {
	for _, r := range []Role{RoleAdmin, RoleOperator, RoleReadOnly, RoleAuditor} {
		if !r.Valid() {
			t.Errorf("%q.Valid() = false", r)
		}
	}
	if Role("superuser").Valid() {
		t.Error(`Role("superuser").Valid() = true, want false`)
	}
}

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{"long enough", "correct horse battery staple", false},
		{"exactly minimum", "abcdefghijkl", false},
		{"too short", "short1234", true},
		{"contains username", "jsmith-password-1", true},
		{"contains email local part", "xx jane.doe xx yyyy", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePassword(tt.password, "jsmith", "jane.doe@example.com")
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidatePassword() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrWeakPassword) {
				t.Errorf("error does not unwrap to ErrWeakPassword: %v", err)
			}
		})
	}
}

func TestSessionRotationAndReuseDetection(t *testing.T) {
	s := NewSession(uuid.New(), []byte("hash-1"), "10.0.0.1", "curl", now)

	if err := s.Validate(now); err != nil {
		t.Fatalf("fresh session Validate() = %v", err)
	}

	next := s.Rotate([]byte("hash-2"), "10.0.0.1", "curl", now.Add(time.Minute))
	if next.FamilyID != s.FamilyID {
		t.Error("rotation started a new family; reuse detection needs a stable family")
	}
	if !next.ExpiresAt.Equal(s.ExpiresAt) {
		t.Error("rotation extended the session expiry")
	}

	// Presenting the original token again is theft, not a mistake.
	if err := s.Validate(now.Add(2 * time.Minute)); !errors.Is(err, ErrRefreshTokenReuse) {
		t.Errorf("Validate() on a rotated session = %v, want ErrRefreshTokenReuse", err)
	}
}

func TestSessionValidateRejectsExpiredAndRevoked(t *testing.T) {
	s := NewSession(uuid.New(), []byte("h"), "", "", now)
	if err := s.Validate(now.Add(RefreshTokenTTL)); !errors.Is(err, ErrSessionExpired) {
		t.Errorf("Validate() at expiry = %v, want ErrSessionExpired", err)
	}

	s2 := NewSession(uuid.New(), []byte("h"), "", "", now)
	s2.Revoke(now)
	if err := s2.Validate(now.Add(time.Minute)); !errors.Is(err, ErrSessionRevoked) {
		t.Errorf("Validate() on revoked session = %v, want ErrSessionRevoked", err)
	}
}
