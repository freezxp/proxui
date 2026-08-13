package command

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freezxp/proxui/internal/domain/identity"
)

var testNow = time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)

func newLogin(users *fakeUsers, sessions *fakeSessions, audit *fakeAudit, clock *fakeClock) *Login {
	return &Login{
		Users: users, Sessions: sessions, Hasher: fastHasher{},
		Tokens: &fakeTokens{}, Audit: audit, Clock: clock,
	}
}

func TestLoginSuccessIssuesTokensAndAudits(t *testing.T) {
	user := mustUser(t, "jsmith", "correct horse battery staple", identity.RoleOperator)
	users, sessions, audit := newFakeUsers(user), newFakeSessions(), &fakeAudit{}
	h := newLogin(users, sessions, audit, &fakeClock{testNow})

	out, err := h.Handle(context.Background(), LoginInput{
		Username: "jsmith", Password: "correct horse battery staple", IP: "10.0.0.5",
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		t.Error("login did not return both tokens")
	}
	if out.ExpiresIn != 15*time.Minute {
		t.Errorf("ExpiresIn = %v, want 15m", out.ExpiresIn)
	}
	if !audit.has("login_success") {
		t.Errorf("audit actions = %v, want login_success", audit.actions())
	}
	if !user.LastLoginAt.Equal(testNow) {
		t.Error("successful login was not recorded on the user")
	}
}

func TestLoginRejectsWrongPasswordAndCountsFailure(t *testing.T) {
	user := mustUser(t, "jsmith", "correct horse battery staple", identity.RoleOperator)
	users, audit := newFakeUsers(user), &fakeAudit{}
	h := newLogin(users, newFakeSessions(), audit, &fakeClock{testNow})

	_, err := h.Handle(context.Background(), LoginInput{Username: "jsmith", Password: "wrong"})
	if !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("error = %v, want ErrInvalidCredentials", err)
	}
	if user.FailedLoginCount != 1 {
		t.Errorf("FailedLoginCount = %d, want 1", user.FailedLoginCount)
	}
	if !audit.has("login_failed") {
		t.Errorf("audit actions = %v, want login_failed", audit.actions())
	}
}

func TestLoginUnknownUserIsIndistinguishable(t *testing.T) {
	audit := &fakeAudit{}
	h := newLogin(newFakeUsers(), newFakeSessions(), audit, &fakeClock{testNow})

	_, err := h.Handle(context.Background(), LoginInput{Username: "ghost", Password: "whatever-long-pass"})
	if !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("error = %v, want ErrInvalidCredentials", err)
	}
	if !audit.has("login_failed") {
		t.Error("unknown-user attempt was not audited")
	}
}

func TestLoginLocksAccountAndRaisesSecurityEvent(t *testing.T) {
	user := mustUser(t, "jsmith", "correct horse battery staple", identity.RoleOperator)
	users, audit := newFakeUsers(user), &fakeAudit{}
	h := newLogin(users, newFakeSessions(), audit, &fakeClock{testNow})
	ctx := context.Background()

	for i := 0; i < identity.MaxFailedLogins; i++ {
		if _, err := h.Handle(ctx, LoginInput{Username: "jsmith", Password: "wrong"}); err == nil {
			t.Fatal("wrong password succeeded")
		}
	}
	if !audit.has("account_locked") {
		t.Errorf("audit actions = %v, want account_locked", audit.actions())
	}

	// Even the correct password must fail while the lock stands.
	_, err := h.Handle(ctx, LoginInput{Username: "jsmith", Password: "correct horse battery staple"})
	if !errors.Is(err, identity.ErrAccountLocked) {
		t.Errorf("error = %v, want ErrAccountLocked", err)
	}
}

func TestLoginRejectsInactiveAccount(t *testing.T) {
	user := mustUser(t, "jsmith", "correct horse battery staple", identity.RoleReadOnly)
	user.Deactivate()
	h := newLogin(newFakeUsers(user), newFakeSessions(), &fakeAudit{}, &fakeClock{testNow})

	_, err := h.Handle(context.Background(), LoginInput{Username: "jsmith", Password: "correct horse battery staple"})
	if !errors.Is(err, identity.ErrAccountInactive) {
		t.Errorf("error = %v, want ErrAccountInactive", err)
	}
}

func TestRefreshRotatesToken(t *testing.T) {
	user := mustUser(t, "jsmith", "correct horse battery staple", identity.RoleOperator)
	users, sessions, audit := newFakeUsers(user), newFakeSessions(), &fakeAudit{}
	clock := &fakeClock{testNow}
	login := newLogin(users, sessions, audit, clock)
	refresh := &Refresh{Users: users, Sessions: sessions, Tokens: &fakeTokens{}, Audit: audit, Clock: clock}
	ctx := context.Background()

	first, err := login.Handle(ctx, LoginInput{Username: "jsmith", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	clock.Advance(time.Minute)
	second, err := refresh.Handle(ctx, RefreshInput{RefreshToken: first.RefreshToken})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Error("refresh returned the same token; rotation did not happen")
	}
	if second.AccessToken == "" {
		t.Error("refresh did not issue a new access token")
	}
}

func TestRefreshReuseRevokesFamilyAndAlerts(t *testing.T) {
	user := mustUser(t, "jsmith", "correct horse battery staple", identity.RoleOperator)
	users, sessions, audit := newFakeUsers(user), newFakeSessions(), &fakeAudit{}
	clock := &fakeClock{testNow}
	login := newLogin(users, sessions, audit, clock)
	refresh := &Refresh{Users: users, Sessions: sessions, Tokens: &fakeTokens{}, Audit: audit, Clock: clock}
	ctx := context.Background()

	first, _ := login.Handle(ctx, LoginInput{Username: "jsmith", Password: "correct horse battery staple"})
	clock.Advance(time.Minute)
	second, err := refresh.Handle(ctx, RefreshInput{RefreshToken: first.RefreshToken})
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	// Replaying the spent token is treated as theft.
	clock.Advance(time.Minute)
	if _, err := refresh.Handle(ctx, RefreshInput{RefreshToken: first.RefreshToken}); !errors.Is(err, identity.ErrRefreshTokenReuse) {
		t.Fatalf("replay error = %v, want ErrRefreshTokenReuse", err)
	}
	if !audit.has("refresh_token_reuse") {
		t.Errorf("audit actions = %v, want refresh_token_reuse", audit.actions())
	}

	// The legitimate successor must die with the family.
	if _, err := refresh.Handle(ctx, RefreshInput{RefreshToken: second.RefreshToken}); err == nil {
		t.Error("successor token still worked after family revocation")
	}
}

func TestRefreshRejectsUnknownToken(t *testing.T) {
	refresh := &Refresh{
		Users: newFakeUsers(), Sessions: newFakeSessions(),
		Tokens: &fakeTokens{}, Audit: &fakeAudit{}, Clock: &fakeClock{testNow},
	}
	if _, err := refresh.Handle(context.Background(), RefreshInput{RefreshToken: "not-a-real-token"}); err == nil {
		t.Error("unknown refresh token was accepted")
	}
}

func TestLogoutRevokesFamily(t *testing.T) {
	user := mustUser(t, "jsmith", "correct horse battery staple", identity.RoleOperator)
	users, sessions, audit := newFakeUsers(user), newFakeSessions(), &fakeAudit{}
	clock := &fakeClock{testNow}
	login := newLogin(users, sessions, audit, clock)
	logout := &Logout{Sessions: sessions, Audit: audit, Clock: clock}
	refresh := &Refresh{Users: users, Sessions: sessions, Tokens: &fakeTokens{}, Audit: audit, Clock: clock}
	ctx := context.Background()

	out, _ := login.Handle(ctx, LoginInput{Username: "jsmith", Password: "correct horse battery staple"})
	if err := logout.Handle(ctx, LogoutInput{RefreshToken: out.RefreshToken, UserID: user.ID, Username: user.Username}); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if !audit.has("logout") {
		t.Errorf("audit actions = %v, want logout", audit.actions())
	}
	if _, err := refresh.Handle(ctx, RefreshInput{RefreshToken: out.RefreshToken}); err == nil {
		t.Error("refresh token still worked after logout")
	}
}

func TestLogoutUnknownTokenSucceeds(t *testing.T) {
	logout := &Logout{Sessions: newFakeSessions(), Audit: &fakeAudit{}, Clock: &fakeClock{testNow}}
	if err := logout.Handle(context.Background(), LogoutInput{RefreshToken: "stale"}); err != nil {
		t.Errorf("logout with unknown token = %v, want nil so clients can always clear state", err)
	}
}

func TestBootstrapAdminRunsOnceOnly(t *testing.T) {
	users, audit := newFakeUsers(), &fakeAudit{}
	h := &BootstrapAdmin{Users: users, Hasher: fastHasher{}, Audit: audit, Clock: &fakeClock{testNow}}
	in := BootstrapAdminInput{Username: "root", Email: "root@example.test", Password: "bootstrap-secret-value"}
	ctx := context.Background()

	created, err := h.Handle(ctx, in)
	if err != nil || !created {
		t.Fatalf("first bootstrap = %v, %v; want true, nil", created, err)
	}
	admin, err := users.GetByUsername(ctx, "root")
	if err != nil {
		t.Fatalf("admin not found: %v", err)
	}
	if admin.Role != identity.RoleAdmin || !admin.MustChangePassword {
		t.Errorf("admin = %+v; want admin role with forced password change", admin)
	}

	created, err = h.Handle(ctx, in)
	if err != nil || created {
		t.Errorf("second bootstrap = %v, %v; want false, nil", created, err)
	}
}

func TestBootstrapAdminEnforcesPasswordPolicy(t *testing.T) {
	h := &BootstrapAdmin{Users: newFakeUsers(), Hasher: fastHasher{}, Audit: &fakeAudit{}, Clock: &fakeClock{testNow}}
	_, err := h.Handle(context.Background(), BootstrapAdminInput{Username: "root", Email: "root@example.test", Password: "short"})
	if !errors.Is(err, identity.ErrWeakPassword) {
		t.Errorf("error = %v, want ErrWeakPassword", err)
	}
}
