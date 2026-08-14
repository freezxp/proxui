package command

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/domain/identity"
)

func newChangePassword(t *testing.T) (*ChangePassword, *fakeUsers, *fakeSessions, *fakeAudit, *identity.User) {
	t.Helper()
	user := mustUser(t, "someone", "original-password-1", identity.RoleOperator)
	// Every account starts flagged; this is the command that clears it.
	user.MustChangePassword = true

	users, sessions, audit := newFakeUsers(user), newFakeSessions(), &fakeAudit{}
	return &ChangePassword{
		Users: users, Sessions: sessions, Hasher: fastHasher{},
		Audit: audit, Clock: &fakeClock{testNow},
	}, users, sessions, audit, user
}

func TestChangePasswordClearsTheForcedFlag(t *testing.T) {
	h, users, _, audit, user := newChangePassword(t)

	err := h.Handle(context.Background(), ChangePasswordInput{
		Actor: Actor{UserID: user.ID}, UserID: user.ID,
		CurrentPassword: "original-password-1", NewPassword: "a-different-password-2",
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	stored, err := users.GetByID(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The flag is set at creation and nothing else clears it. If this
	// regresses, every account is permanently marked "password pending" —
	// which is exactly how it stood before this command existed.
	if stored.MustChangePassword {
		t.Error("MustChangePassword survived a successful change")
	}
	if ok, _ := (fastHasher{}).Verify("a-different-password-2", stored.PasswordHash); !ok {
		t.Error("the new password was not stored")
	}
	if !audit.has("password_changed") {
		t.Errorf("audit actions = %v, want password_changed", audit.actions())
	}
}

func TestChangePasswordRequiresTheCurrentOne(t *testing.T) {
	h, users, _, audit, user := newChangePassword(t)

	err := h.Handle(context.Background(), ChangePasswordInput{
		Actor: Actor{UserID: user.ID}, UserID: user.ID,
		CurrentPassword: "not-the-password", NewPassword: "a-different-password-2",
	})
	if !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("got %v, want invalid credentials", err)
	}

	stored, _ := users.GetByID(context.Background(), user.ID)
	if ok, _ := (fastHasher{}).Verify("original-password-1", stored.PasswordHash); !ok {
		t.Error("the password changed despite a wrong current password")
	}
	// Requiring the current password is what makes this safe for every role:
	// a stolen access token alone cannot lock its owner out. A failed attempt
	// is therefore a security event worth recording.
	if !audit.has("password_change_denied") {
		t.Errorf("audit actions = %v, want password_change_denied", audit.actions())
	}
}

// A forced change satisfied by re-entering the same password would defeat the
// point of forcing it.
func TestChangePasswordRejectsTheSamePassword(t *testing.T) {
	h, users, _, _, user := newChangePassword(t)

	err := h.Handle(context.Background(), ChangePasswordInput{
		Actor: Actor{UserID: user.ID}, UserID: user.ID,
		CurrentPassword: "original-password-1", NewPassword: "original-password-1",
	})
	if !errors.Is(err, identity.ErrPasswordUnchanged) {
		t.Fatalf("got %v, want ErrPasswordUnchanged", err)
	}

	stored, _ := users.GetByID(context.Background(), user.ID)
	if !stored.MustChangePassword {
		t.Error("the forced-change flag was cleared without a real change")
	}
}

func TestChangePasswordEnforcesPolicy(t *testing.T) {
	h, _, _, _, user := newChangePassword(t)

	err := h.Handle(context.Background(), ChangePasswordInput{
		Actor: Actor{UserID: user.ID}, UserID: user.ID,
		CurrentPassword: "original-password-1", NewPassword: "short",
	})
	if !errors.Is(err, identity.ErrWeakPassword) {
		t.Fatalf("got %v, want a policy error", err)
	}
}

// Changing a password because it may have been seen must end everyone else's
// access, not only prompt the owner to sign in again.
func TestChangePasswordRevokesSessions(t *testing.T) {
	h, _, sessions, _, user := newChangePassword(t)

	session := &identity.Session{
		ID: uuid.New(), UserID: user.ID, FamilyID: uuid.New(),
		TokenHash: []byte("a-live-session"),
	}
	if err := sessions.Create(context.Background(), session); err != nil {
		t.Fatal(err)
	}

	if err := h.Handle(context.Background(), ChangePasswordInput{
		Actor: Actor{UserID: user.ID}, UserID: user.ID,
		CurrentPassword: "original-password-1", NewPassword: "a-different-password-2",
	}); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	stored, err := sessions.GetByTokenHash(context.Background(), session.TokenHash)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RevokedAt.IsZero() {
		t.Error("an existing session outlived the password change")
	}
}
