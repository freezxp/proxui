package command

import (
	"context"
	"errors"
	"testing"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

type policy bool

func (p policy) SelfRegistrationEnabled(context.Context) bool { return bool(p) }

func newRegister(open bool, seed ...*identity.User) (*Register, *fakeUsers, *fakeAudit) {
	users, audit := newFakeUsers(seed...), &fakeAudit{}
	return &Register{
		Users: users, Policy: policy(open), Hasher: fastHasher{},
		Audit: audit, Clock: &fakeClock{testNow},
	}, users, audit
}

func TestRegisterCreatesAReadOnlyAccountWithNoAccess(t *testing.T) {
	h, users, audit := newRegister(true)

	user, err := h.Handle(context.Background(), RegisterInput{
		Username: "newcomer", Email: "newcomer@example.test",
		DisplayName: "New Comer", Password: "a-perfectly-fine-password",
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	// The whole containment story for open registration: an account that can
	// see nothing until an administrator grants it something.
	if user.Role != identity.RoleReadOnly {
		t.Errorf("role = %q, want readonly", user.Role)
	}
	if user.AuthProvider != identity.ProviderLocal {
		t.Errorf("provider = %q, want local", user.AuthProvider)
	}
	// They chose the password a moment ago; forcing an immediate change would
	// be theatre.
	if user.MustChangePassword {
		t.Error("a self-chosen password was marked as needing a change")
	}
	if _, err := users.GetByUsername(context.Background(), "newcomer"); err != nil {
		t.Errorf("the account was not stored: %v", err)
	}
	if !audit.has("user_registered") {
		t.Errorf("audit actions = %v, want user_registered", audit.actions())
	}
}

// Off is how the portal ships, and the switch has to actually stop it.
func TestRegisterRefusedWhenDisabled(t *testing.T) {
	h, users, _ := newRegister(false)

	_, err := h.Handle(context.Background(), RegisterInput{
		Username: "newcomer", Email: "newcomer@example.test", Password: "a-perfectly-fine-password",
	})
	if !errors.Is(err, ErrRegistrationClosed) {
		t.Fatalf("got %v, want ErrRegistrationClosed", err)
	}
	if _, err := users.GetByUsername(context.Background(), "newcomer"); !errors.Is(err, ports.ErrNotFound) {
		t.Error("an account was created while registration was closed")
	}
}

func TestRegisterValidatesItsInput(t *testing.T) {
	cases := []struct {
		name  string
		in    RegisterInput
		wants error
	}{
		{"username too short", RegisterInput{Username: "ab", Email: "a@b.test", Password: "a-perfectly-fine-password"}, identity.ErrInvalidUsername},
		{"username with spaces", RegisterInput{Username: "two words", Email: "a@b.test", Password: "a-perfectly-fine-password"}, identity.ErrInvalidUsername},
		{"not an email", RegisterInput{Username: "someone", Email: "not-an-email", Password: "a-perfectly-fine-password"}, identity.ErrInvalidEmail},
		{"weak password", RegisterInput{Username: "someone", Email: "a@b.test", Password: "short"}, identity.ErrWeakPassword},
	}
	for _, tc := range cases {
		h, _, _ := newRegister(true)
		if _, err := h.Handle(context.Background(), tc.in); !errors.Is(err, tc.wants) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.wants)
		}
	}
}

// A taken username and a taken email must be the same answer, or the form
// becomes a way to ask which accounts exist.
func TestRegisterDoesNotRevealWhichFieldWasTaken(t *testing.T) {
	existing := mustUser(t, "taken", "any-password-at-all", identity.RoleOperator)
	h, _, _ := newRegister(true, existing)

	_, byName := h.Handle(context.Background(), RegisterInput{
		Username: "taken", Email: "fresh@example.test", Password: "a-perfectly-fine-password",
	})
	_, byEmail := h.Handle(context.Background(), RegisterInput{
		Username: "fresh", Email: "taken@example.test", Password: "a-perfectly-fine-password",
	})
	if !errors.Is(byName, ports.ErrConflict) || !errors.Is(byEmail, ports.ErrConflict) {
		t.Fatalf("username conflict = %v, email conflict = %v; both should be a plain conflict", byName, byEmail)
	}
}

// --- external sign-in ----------------------------------------------------

func newExternal(open bool, seed ...*identity.User) (*SignInExternal, *fakeUsers, *fakeAudit) {
	users, audit := newFakeUsers(seed...), &fakeAudit{}
	return &SignInExternal{
		Users: users, Policy: policy(open), Audit: audit, Clock: &fakeClock{testNow},
	}, users, audit
}

func googleIdentity() ExternalIdentity {
	return ExternalIdentity{
		Provider: identity.ProviderGoogle, Subject: "google-subject-1",
		Email: "person@example.test", DisplayName: "A Person", EmailVerified: true,
	}
}

func TestExternalSignInProvisionsAnAccount(t *testing.T) {
	h, users, audit := newExternal(true)

	user, err := h.Handle(context.Background(), googleIdentity(), Actor{})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if user.AuthProvider != identity.ProviderGoogle || user.ExternalID != "google-subject-1" {
		t.Errorf("provider = %q external = %q", user.AuthProvider, user.ExternalID)
	}
	if user.Role != identity.RoleReadOnly {
		t.Errorf("role = %q, want readonly", user.Role)
	}
	// No password at all, so the password path has nothing to compare against.
	if user.PasswordHash != "" {
		t.Error("a provider account was given a password")
	}
	if !audit.has("user_registered") {
		t.Errorf("audit actions = %v, want user_registered", audit.actions())
	}

	// Signing in again finds the same account rather than making another.
	again, err := h.Handle(context.Background(), googleIdentity(), Actor{})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != user.ID {
		t.Error("a second sign-in created a second account")
	}
	all, _ := users.List(context.Background(), ports.UserFilter{})
	if len(all) != 1 {
		t.Errorf("%d accounts exist, want 1", len(all))
	}
}

// An address the provider has not verified could belong to someone else, and
// the address is what links a provider identity to a portal account.
func TestExternalSignInRefusesUnverifiedEmail(t *testing.T) {
	h, users, _ := newExternal(true)

	in := googleIdentity()
	in.EmailVerified = false
	if _, err := h.Handle(context.Background(), in, Actor{}); err == nil {
		t.Fatal("an unverified address was accepted")
	}
	all, _ := users.List(context.Background(), ports.UserFilter{})
	if len(all) != 0 {
		t.Error("an account was created for an unverified address")
	}
}

// An account an administrator already made is linked rather than duplicated,
// so the grants attached to it survive the switch to Google.
func TestExternalSignInLinksAnExistingAccount(t *testing.T) {
	// mustUser derives the address from the username, so this account is
	// person@example.test — the same address Google will report.
	existing := mustUser(t, "person", "any-password-at-all", identity.RoleOperator)
	h, users, audit := newExternal(false, existing) // registration off: linking is not creating

	user, err := h.Handle(context.Background(), googleIdentity(), Actor{})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if user.ID != existing.ID {
		t.Fatal("linking created a new account instead")
	}
	if user.AuthProvider != identity.ProviderGoogle || user.ExternalID != "google-subject-1" {
		t.Errorf("the account was not linked: provider=%q external=%q", user.AuthProvider, user.ExternalID)
	}
	// The role it already had is kept: linking must not quietly demote someone.
	if user.Role != identity.RoleOperator {
		t.Errorf("role = %q, want the operator role it already had", user.Role)
	}
	if !audit.has("user_linked_provider") {
		t.Errorf("audit actions = %v, want user_linked_provider", audit.actions())
	}
	all, _ := users.List(context.Background(), ports.UserFilter{})
	if len(all) != 1 {
		t.Errorf("%d accounts exist, want 1", len(all))
	}
}

func TestExternalSignInRefusedWhenRegistrationClosed(t *testing.T) {
	h, users, _ := newExternal(false)

	if _, err := h.Handle(context.Background(), googleIdentity(), Actor{}); !errors.Is(err, ErrRegistrationClosed) {
		t.Fatalf("got %v, want ErrRegistrationClosed", err)
	}
	all, _ := users.List(context.Background(), ports.UserFilter{})
	if len(all) != 0 {
		t.Error("an unknown identity created an account while registration was closed")
	}
}

// A disabled account must not get in through a different door.
func TestExternalSignInRespectsAccountState(t *testing.T) {
	existing := mustUser(t, "person", "any-password-at-all", identity.RoleOperator)
	existing.AuthProvider = identity.ProviderGoogle
	existing.ExternalID = "google-subject-1"
	existing.IsActive = false

	h, _, _ := newExternal(true, existing)
	if _, err := h.Handle(context.Background(), googleIdentity(), Actor{}); err == nil {
		t.Fatal("a disabled account signed in through the provider")
	}
}

func TestUsernameIsDerivedFromTheAddress(t *testing.T) {
	for in, want := range map[string]string{
		"Person.Name@example.test": "person.name",
		"a+tag@example.test":       "atag",
		"UPPER@example.test":       "upper",
	} {
		if got := usernameFromEmail(in); got != want {
			t.Errorf("usernameFromEmail(%q) = %q, want %q", in, got, want)
		}
	}
}
