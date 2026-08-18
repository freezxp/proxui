package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// The enrolment table against a real database. Two of these cover guards that
// live in the SQL rather than in Go, and would pass silently against a fake:
// enabling a factor whose seed has gone, and recording a step that has already
// been passed.

func totpVault(t *testing.T) *crypto.Vault {
	t.Helper()
	key := make([]byte, crypto.MasterKeySize)
	for i := range key {
		key[i] = byte(i * 3)
	}
	v, err := crypto.NewVault(key, 1)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	return v
}

func TestTOTPRoundTripsThroughTheVault(t *testing.T) {
	pool := testPool(t)
	repo := NewTOTPRepository(pool)
	vault := totpVault(t)
	ctx := context.Background()
	user := newTestUser(t, pool, identity.RoleOperator)
	now := time.Now().UTC()

	if _, err := repo.Secret(ctx, user.ID, vault); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("secret before enrolment = %v, want ErrNotFound", err)
	}

	secret, err := crypto.NewTOTPSecret()
	if err != nil {
		t.Fatalf("new secret: %v", err)
	}
	sealed, err := vault.Seal(secret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := repo.Enroll(ctx, user.ID, sealed, now); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	got, err := repo.Secret(ctx, user.ID, vault)
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	if got != secret {
		t.Fatalf("secret round-tripped as %q, want %q", got, secret)
	}

	// The seed is stored, but the factor is not live until a code proves it.
	loaded, err := NewUserRepository(pool).GetByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	if loaded.TOTPEnabled {
		t.Fatal("enrolment enabled the factor before anything confirmed it")
	}

	if err := repo.Enable(ctx, user.ID, 100, now); err != nil {
		t.Fatalf("enable: %v", err)
	}
	loaded, _ = NewUserRepository(pool).GetByID(ctx, user.ID)
	if !loaded.TOTPEnabled {
		t.Fatal("the factor is not enabled after confirmation")
	}
}

// Nothing readable must survive a disable: a seed left behind is a factor that
// can be silently switched back on.
func TestDisableClearsTheSeed(t *testing.T) {
	pool := testPool(t)
	repo := NewTOTPRepository(pool)
	vault := totpVault(t)
	ctx := context.Background()
	user := newTestUser(t, pool, identity.RoleOperator)
	now := time.Now().UTC()

	secret, _ := crypto.NewTOTPSecret()
	sealed, _ := vault.Seal(secret)
	if err := repo.Enroll(ctx, user.ID, sealed, now); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if err := repo.Enable(ctx, user.ID, 100, now); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := repo.Disable(ctx, user.ID, now); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if _, err := repo.Secret(ctx, user.ID, vault); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("secret after disable = %v, want ErrNotFound", err)
	}
	loaded, _ := NewUserRepository(pool).GetByID(ctx, user.ID)
	if loaded.TOTPEnabled {
		t.Fatal("the factor is still enabled after being disabled")
	}
	step, err := repo.LastStep(ctx, user.ID)
	if err != nil {
		t.Fatalf("last step: %v", err)
	}
	if step != nil {
		t.Fatalf("the spent-step marker survived a disable: %d", *step)
	}
}

// The guard in Enable: a confirm racing a disable must not switch the flag on
// for an account whose seed has just been cleared, leaving it demanding codes
// that nothing can produce.
func TestEnableRefusesAnAccountWithNoSeed(t *testing.T) {
	pool := testPool(t)
	repo := NewTOTPRepository(pool)
	ctx := context.Background()
	user := newTestUser(t, pool, identity.RoleOperator)

	err := repo.Enable(ctx, user.ID, 100, time.Now().UTC())
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("enable with no seed = %v, want ErrNotFound", err)
	}
	loaded, _ := NewUserRepository(pool).GetByID(ctx, user.ID)
	if loaded.TOTPEnabled {
		t.Fatal("an account with no seed was left demanding codes")
	}
}

// The guard in RecordStep: two requests arriving with the same code must not
// both be able to write it, and a late one must not move the marker backwards.
func TestRecordStepOnlyMovesForwards(t *testing.T) {
	pool := testPool(t)
	repo := NewTOTPRepository(pool)
	vault := totpVault(t)
	ctx := context.Background()
	user := newTestUser(t, pool, identity.RoleOperator)
	now := time.Now().UTC()

	secret, _ := crypto.NewTOTPSecret()
	sealed, _ := vault.Seal(secret)
	if err := repo.Enroll(ctx, user.ID, sealed, now); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	if step, err := repo.LastStep(ctx, user.ID); err != nil || step != nil {
		t.Fatalf("last step on a fresh enrolment = %v, %v; want nil", step, err)
	}
	if err := repo.RecordStep(ctx, user.ID, 500, now); err != nil {
		t.Fatalf("record: %v", err)
	}
	// An older step, as a replayed code would carry.
	if err := repo.RecordStep(ctx, user.ID, 499, now); err != nil {
		t.Fatalf("record older: %v", err)
	}

	step, err := repo.LastStep(ctx, user.ID)
	if err != nil {
		t.Fatalf("last step: %v", err)
	}
	if step == nil || *step != 500 {
		t.Fatalf("last step = %v, want 500: a replay moved the marker backwards", step)
	}
}

// Re-enrolling must not inherit the previous enrolment's spent step, or the
// first code from a newly scanned QR could be refused as a replay.
func TestReEnrolmentClearsTheSpentStep(t *testing.T) {
	pool := testPool(t)
	repo := NewTOTPRepository(pool)
	vault := totpVault(t)
	ctx := context.Background()
	user := newTestUser(t, pool, identity.RoleOperator)
	now := time.Now().UTC()

	secret, _ := crypto.NewTOTPSecret()
	sealed, _ := vault.Seal(secret)
	_ = repo.Enroll(ctx, user.ID, sealed, now)
	_ = repo.RecordStep(ctx, user.ID, 9_999_999, now)

	next, _ := crypto.NewTOTPSecret()
	sealedNext, _ := vault.Seal(next)
	if err := repo.Enroll(ctx, user.ID, sealedNext, now); err != nil {
		t.Fatalf("re-enroll: %v", err)
	}

	step, err := repo.LastStep(ctx, user.ID)
	if err != nil {
		t.Fatalf("last step: %v", err)
	}
	if step != nil {
		t.Fatalf("a new enrolment inherited step %d", *step)
	}
}
