package command

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// The second factor is only worth having if the failure modes are closed:
// a code cannot be replayed, guesses are bounded, the password step alone
// mints nothing, and turning the factor off costs a password. Each of those is
// a test here.

// --- fakes ---------------------------------------------------------------

type fakeTOTPStore struct {
	mu       sync.Mutex
	secrets  map[uuid.UUID]crypto.SealedSecret
	lastStep map[uuid.UUID]int64
	enabled  map[uuid.UUID]bool
}

func newFakeTOTPStore() *fakeTOTPStore {
	return &fakeTOTPStore{
		secrets:  map[uuid.UUID]crypto.SealedSecret{},
		lastStep: map[uuid.UUID]int64{},
		enabled:  map[uuid.UUID]bool{},
	}
}

func (f *fakeTOTPStore) Enroll(_ context.Context, id uuid.UUID, sealed crypto.SealedSecret, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secrets[id] = sealed
	f.enabled[id] = false
	delete(f.lastStep, id)
	return nil
}

func (f *fakeTOTPStore) Secret(_ context.Context, id uuid.UUID, vault *crypto.Vault) (string, error) {
	f.mu.Lock()
	sealed, ok := f.secrets[id]
	f.mu.Unlock()
	if !ok {
		return "", ports.ErrNotFound
	}
	return vault.Open(sealed)
}

func (f *fakeTOTPStore) Enable(_ context.Context, id uuid.UUID, step int64, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.secrets[id]; !ok {
		return ports.ErrNotFound
	}
	f.enabled[id] = true
	f.lastStep[id] = step
	return nil
}

func (f *fakeTOTPStore) Disable(_ context.Context, id uuid.UUID, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.secrets, id)
	delete(f.lastStep, id)
	f.enabled[id] = false
	return nil
}

func (f *fakeTOTPStore) LastStep(_ context.Context, id uuid.UUID) (*int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	step, ok := f.lastStep[id]
	if !ok {
		return nil, nil
	}
	return &step, nil
}

func (f *fakeTOTPStore) RecordStep(_ context.Context, id uuid.UUID, step int64, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if current, ok := f.lastStep[id]; !ok || step > current {
		f.lastStep[id] = step
	}
	return nil
}

// fakeChallenges is the Redis store's behaviour in a map, including the part
// that matters: attempts accumulate and the challenge dies when they run out.
type fakeChallenges struct {
	mu   sync.Mutex
	live map[string]identity.MFAChallenge
}

func newFakeChallenges() *fakeChallenges {
	return &fakeChallenges{live: map[string]identity.MFAChallenge{}}
}

func (f *fakeChallenges) Issue(_ context.Context, c identity.MFAChallenge) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live[c.ID.String()] = c
	return nil
}

func (f *fakeChallenges) Get(_ context.Context, id string) (identity.MFAChallenge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.live[id]
	if !ok {
		return identity.MFAChallenge{}, ports.ErrNotFound
	}
	return c, nil
}

func (f *fakeChallenges) Fail(_ context.Context, id string) (identity.MFAChallenge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.live[id]
	if !ok {
		return identity.MFAChallenge{}, ports.ErrNotFound
	}
	c.Attempts++
	if c.Exhausted() {
		delete(f.live, id)
		return c, nil
	}
	f.live[id] = c
	return c, nil
}

func (f *fakeChallenges) Consume(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, id)
	return nil
}

func (f *fakeChallenges) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.live)
}

// --- harness -------------------------------------------------------------

type mfaHarness struct {
	mfa        *MFA
	login      *Login
	users      *fakeUsers
	totp       *fakeTOTPStore
	challenges *fakeChallenges
	audit      *fakeAudit
	clock      *fakeClock
	user       *identity.User
	actor      Actor
	vault      *crypto.Vault
}

func newMFAHarness(t *testing.T) *mfaHarness {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)}
	hasher := crypto.NewPasswordHasher()
	hash, err := hasher.Hash("correct horse")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	user := &identity.User{
		ID: uuid.New(), Username: "ada", Email: "ada@example.com",
		DisplayName: "Ada", Role: identity.RoleOperator, IsActive: true,
		PasswordHash: hash, CreatedAt: clock.Now(),
	}

	key := make([]byte, crypto.MasterKeySize)
	for i := range key {
		key[i] = byte(i + 7)
	}
	vault, err := crypto.NewVault(key, 1)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}

	users := newFakeUsers(user)
	totp := newFakeTOTPStore()
	challenges := newFakeChallenges()
	audit := &fakeAudit{}
	sessions := newFakeSessions()

	return &mfaHarness{
		mfa: &MFA{
			Users: users, TOTP: totp, Codec: crypto.TOTPService{Issuer: "ProxUI"},
			Challenges: challenges, Hasher: hasher,
			Issue: &IssueSession{
				Users: users, Sessions: sessions, Tokens: &fakeTokens{}, Audit: audit, Clock: clock,
			},
			Vault: vault, Audit: audit, Clock: clock,
		},
		login: &Login{
			Users: users, Sessions: sessions, Hasher: hasher, Tokens: &fakeTokens{},
			Audit: audit, Clock: clock, Challenges: challenges,
		},
		users: users, totp: totp, challenges: challenges, audit: audit, clock: clock,
		user:  user,
		actor: Actor{UserID: user.ID, Username: user.Username, IP: "10.0.0.1"},
		vault: vault,
	}
}

// enroll runs the real two-step enrolment and returns the secret.
func (h *mfaHarness) enroll(t *testing.T) string {
	t.Helper()
	out, err := h.mfa.Begin(context.Background(), h.actor)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	code, err := crypto.TOTPAt(out.Secret, h.clock.Now())
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	if err := h.mfa.Confirm(context.Background(), h.actor, code); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	// The confirm wrote through the fake store; the user aggregate the login
	// path reads has to agree.
	h.user.TOTPEnabled = true
	return out.Secret
}

func (h *mfaHarness) codeAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := crypto.TOTPAt(secret, at)
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	return code
}

// --- enrolment -----------------------------------------------------------

func TestEnrolmentIsNotLiveUntilConfirmed(t *testing.T) {
	// The failure this prevents: a badly scanned QR turning into an account
	// nobody can sign into, recoverable only by an administrator.
	h := newMFAHarness(t)
	out, err := h.mfa.Begin(context.Background(), h.actor)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if !strings.HasPrefix(out.OTPAuthURL, "otpauth://totp/") {
		t.Errorf("otpauth url is %q", out.OTPAuthURL)
	}
	if h.totp.enabled[h.user.ID] {
		t.Fatal("the factor is live before any code proved the device has it")
	}

	if err := h.mfa.Confirm(context.Background(), h.actor, "000000"); !errors.Is(err, identity.ErrInvalidTOTPCode) {
		t.Fatalf("confirm with a wrong code = %v, want ErrInvalidTOTPCode", err)
	}
	if h.totp.enabled[h.user.ID] {
		t.Fatal("a wrong code enabled the factor")
	}

	code := h.codeAt(t, out.Secret, h.clock.Now())
	if err := h.mfa.Confirm(context.Background(), h.actor, code); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !h.totp.enabled[h.user.ID] {
		t.Fatal("a correct code did not enable the factor")
	}
}

func TestEnrolmentRefusesToOverwriteAWorkingFactor(t *testing.T) {
	// Otherwise a borrowed, still-signed-in desk is enough to swap somebody
	// else's second factor for your own.
	h := newMFAHarness(t)
	h.enroll(t)

	if _, err := h.mfa.Begin(context.Background(), h.actor); !errors.Is(err, identity.ErrTOTPAlreadyEnabled) {
		t.Fatalf("begin over a live factor = %v, want ErrTOTPAlreadyEnabled", err)
	}
}

func TestTheConfirmingCodeCannotBeReusedToSignIn(t *testing.T) {
	h := newMFAHarness(t)
	secret := h.enroll(t)
	code := h.codeAt(t, secret, h.clock.Now())

	out, err := h.login.Handle(context.Background(), LoginInput{Username: "ada", Password: "correct horse"})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	_, err = h.mfa.Verify(context.Background(), VerifyMFAInput{ChallengeID: out.MFAToken, Code: code})
	if !errors.Is(err, identity.ErrInvalidTOTPCode) {
		t.Fatalf("replaying the enrolment code = %v, want it refused", err)
	}
}

func TestDisableNeedsThePassword(t *testing.T) {
	h := newMFAHarness(t)
	h.enroll(t)

	if err := h.mfa.Disable(context.Background(), h.actor, "wrong"); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("disable with a wrong password = %v, want ErrInvalidCredentials", err)
	}
	if _, ok := h.totp.secrets[h.user.ID]; !ok {
		t.Fatal("a wrong password removed the factor")
	}

	if err := h.mfa.Disable(context.Background(), h.actor, "correct horse"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, ok := h.totp.secrets[h.user.ID]; ok {
		t.Fatal("the seed survived the factor being disabled")
	}
	if h.totp.enabled[h.user.ID] {
		t.Fatal("the factor is still enabled")
	}
}

func TestAdminResetClearsTheFactorAndSaysWhose(t *testing.T) {
	h := newMFAHarness(t)
	h.enroll(t)
	admin := Actor{UserID: uuid.New(), Username: "root"}

	if err := h.mfa.Reset(context.Background(), admin, h.user.ID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, ok := h.totp.secrets[h.user.ID]; ok {
		t.Fatal("the seed survived an admin reset")
	}

	var found bool
	for _, e := range h.audit.entries {
		if e.Action == "mfa_reset" {
			found = true
			if e.TargetName != "ada" || e.ActorName != "root" {
				t.Errorf("reset entry names actor %q target %q", e.ActorName, e.TargetName)
			}
		}
	}
	if !found {
		t.Fatalf("an admin reset must be audited, got %v", h.audit.actions())
	}
}

// --- signing in ----------------------------------------------------------

func TestPasswordAloneMintsNothingWhenAFactorIsEnrolled(t *testing.T) {
	h := newMFAHarness(t)
	h.enroll(t)

	out, err := h.login.Handle(context.Background(), LoginInput{Username: "ada", Password: "correct horse"})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if !out.MFARequired || out.MFAToken == "" {
		t.Fatal("login should have asked for a second factor")
	}
	// The important half: no tokens, at all.
	if out.AccessToken != "" || out.RefreshToken != "" {
		t.Fatal("the password step handed out a session before the code")
	}
	// And nothing that reads like a completed sign-in was recorded.
	for _, action := range h.audit.actions() {
		if action == "login_success" {
			t.Fatal("the password step recorded a successful login")
		}
	}
}

func TestVerifyCompletesTheSignIn(t *testing.T) {
	h := newMFAHarness(t)
	secret := h.enroll(t)

	out, err := h.login.Handle(context.Background(), LoginInput{Username: "ada", Password: "correct horse"})
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	// A step later, so the enrolment code is not the sign-in code.
	h.clock.Advance(crypto.TOTPPeriod)
	code := h.codeAt(t, secret, h.clock.Now())

	session, err := h.mfa.Verify(context.Background(), VerifyMFAInput{ChallengeID: out.MFAToken, Code: code})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if session.AccessToken == "" || session.RefreshToken == "" {
		t.Fatal("a verified sign-in produced no session")
	}
	if h.challenges.count() != 0 {
		t.Error("the challenge outlived the code that answered it")
	}
}

func TestACodeCannotBeUsedTwice(t *testing.T) {
	// A correct code stays arithmetically correct for up to ninety seconds
	// across the accepted window. Without the spent-step check, one glimpse
	// over a shoulder is a working sign-in.
	h := newMFAHarness(t)
	secret := h.enroll(t)
	h.clock.Advance(crypto.TOTPPeriod)
	code := h.codeAt(t, secret, h.clock.Now())

	first, _ := h.login.Handle(context.Background(), LoginInput{Username: "ada", Password: "correct horse"})
	if _, err := h.mfa.Verify(context.Background(), VerifyMFAInput{ChallengeID: first.MFAToken, Code: code}); err != nil {
		t.Fatalf("first use: %v", err)
	}

	second, _ := h.login.Handle(context.Background(), LoginInput{Username: "ada", Password: "correct horse"})
	_, err := h.mfa.Verify(context.Background(), VerifyMFAInput{ChallengeID: second.MFAToken, Code: code})
	if !errors.Is(err, identity.ErrInvalidTOTPCode) {
		t.Fatalf("replaying a code = %v, want it refused", err)
	}
}

func TestGuessingIsBounded(t *testing.T) {
	h := newMFAHarness(t)
	h.enroll(t)
	out, _ := h.login.Handle(context.Background(), LoginInput{Username: "ada", Password: "correct horse"})

	for i := 0; i < identity.MaxMFAAttempts-1; i++ {
		if _, err := h.mfa.Verify(context.Background(),
			VerifyMFAInput{ChallengeID: out.MFAToken, Code: "000000"}); !errors.Is(err, identity.ErrInvalidTOTPCode) {
			t.Fatalf("guess %d = %v, want ErrInvalidTOTPCode", i+1, err)
		}
	}

	// The last one takes the challenge with it, so the browser has to go back
	// to the password rather than keep guessing at a live prompt.
	_, err := h.mfa.Verify(context.Background(), VerifyMFAInput{ChallengeID: out.MFAToken, Code: "000000"})
	if !errors.Is(err, identity.ErrMFAChallengeNotFound) {
		t.Fatalf("the exhausting guess = %v, want ErrMFAChallengeNotFound", err)
	}
	if h.challenges.count() != 0 {
		t.Error("an exhausted challenge is still live")
	}
}

func TestAnExpiredChallengeIsRefused(t *testing.T) {
	h := newMFAHarness(t)
	secret := h.enroll(t)
	out, _ := h.login.Handle(context.Background(), LoginInput{Username: "ada", Password: "correct horse"})

	h.clock.Advance(identity.MFAChallengeTTL + time.Second)
	code := h.codeAt(t, secret, h.clock.Now())

	_, err := h.mfa.Verify(context.Background(), VerifyMFAInput{ChallengeID: out.MFAToken, Code: code})
	if !errors.Is(err, identity.ErrMFAChallengeNotFound) {
		t.Fatalf("verify on an expired challenge = %v, want ErrMFAChallengeNotFound", err)
	}
}

func TestAnUnknownChallengeIsRefused(t *testing.T) {
	h := newMFAHarness(t)
	h.enroll(t)
	_, err := h.mfa.Verify(context.Background(),
		VerifyMFAInput{ChallengeID: uuid.NewString(), Code: "123456"})
	if !errors.Is(err, identity.ErrMFAChallengeNotFound) {
		t.Fatalf("verify on an invented challenge = %v, want ErrMFAChallengeNotFound", err)
	}
}

func TestAnAccountDisabledMidSignInDoesNotGetIn(t *testing.T) {
	// The gap this closes: the seconds between a correct password and a
	// correct code, during which an administrator may have disabled the
	// account precisely because it is being attacked.
	h := newMFAHarness(t)
	secret := h.enroll(t)
	out, _ := h.login.Handle(context.Background(), LoginInput{Username: "ada", Password: "correct horse"})

	h.user.IsActive = false
	h.clock.Advance(crypto.TOTPPeriod)
	code := h.codeAt(t, secret, h.clock.Now())

	if _, err := h.mfa.Verify(context.Background(),
		VerifyMFAInput{ChallengeID: out.MFAToken, Code: code}); !errors.Is(err, identity.ErrAccountInactive) {
		t.Fatalf("verify for a disabled account = %v, want ErrAccountInactive", err)
	}
}

func TestLoginIsUnchangedForAccountsWithoutAFactor(t *testing.T) {
	h := newMFAHarness(t)
	out, err := h.login.Handle(context.Background(), LoginInput{Username: "ada", Password: "correct horse"})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if out.MFARequired {
		t.Fatal("an account with no factor was challenged")
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		t.Fatal("a plain login produced no session")
	}
}

func TestNoAuditEntryCarriesTheSeedOrACode(t *testing.T) {
	h := newMFAHarness(t)
	secret := h.enroll(t)
	out, _ := h.login.Handle(context.Background(), LoginInput{Username: "ada", Password: "correct horse"})
	h.clock.Advance(crypto.TOTPPeriod)
	code := h.codeAt(t, secret, h.clock.Now())
	_, _ = h.mfa.Verify(context.Background(), VerifyMFAInput{ChallengeID: out.MFAToken, Code: code})

	for _, e := range h.audit.entries {
		for k, v := range e.Details {
			text, ok := v.(string)
			if !ok {
				continue
			}
			if strings.Contains(text, secret) {
				t.Fatalf("audit detail %q leaked the TOTP seed", k)
			}
			if code != "" && strings.Contains(text, code) {
				t.Fatalf("audit detail %q leaked a code", k)
			}
		}
	}
}
