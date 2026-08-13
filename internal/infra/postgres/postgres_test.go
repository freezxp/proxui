package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// Integration tests need a real Postgres (migrations, citext, partitions and
// constraint behaviour are exactly what we are testing). Point
// PROXUI_TEST_DATABASE_URL at a throwaway database — `make up` provides one
// locally, CI provides a service container.
func testPool(t *testing.T) *Pool {
	t.Helper()
	dsn := os.Getenv("PROXUI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PROXUI_TEST_DATABASE_URL not set; skipping database integration tests")
	}
	ctx := context.Background()
	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := Migrate(ctx, dsn, zerolog.Nop()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func newTestUser(t *testing.T, pool *Pool, role identity.Role) *identity.User {
	t.Helper()
	id := uuid.New()
	u := &identity.User{
		ID:           id,
		Username:     "user-" + id.String()[:8],
		Email:        id.String()[:8] + "@example.test",
		DisplayName:  "Test User",
		PasswordHash: "$argon2id$stub",
		Role:         role,
		IsActive:     true,
		CreatedAt:    time.Now().UTC().Truncate(time.Microsecond),
	}
	if err := NewUserRepository(pool).Create(context.Background(), u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func TestUserRepositoryRoundTrip(t *testing.T) {
	pool := testPool(t)
	repo := NewUserRepository(pool)
	ctx := context.Background()

	u := newTestUser(t, pool, identity.RoleOperator)

	got, err := repo.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Username != u.Username || got.Role != identity.RoleOperator || !got.IsActive {
		t.Errorf("round trip mismatch: %+v", got)
	}

	// citext: usernames must match case-insensitively.
	upper, err := repo.GetByUsername(ctx, upperFirst(u.Username))
	if err != nil {
		t.Fatalf("GetByUsername (mixed case): %v", err)
	}
	if upper.ID != u.ID {
		t.Error("username lookup is case sensitive; citext is not in effect")
	}

	if _, err := repo.GetByUsername(ctx, "definitely-missing"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("missing user error = %v, want ErrNotFound", err)
	}
}

func TestUserRepositoryRejectsDuplicateUsername(t *testing.T) {
	pool := testPool(t)
	repo := NewUserRepository(pool)
	u := newTestUser(t, pool, identity.RoleReadOnly)

	dup := *u
	dup.ID = uuid.New()
	dup.Email = "other-" + dup.ID.String()[:8] + "@example.test"

	if err := repo.Create(context.Background(), &dup); !errors.Is(err, ports.ErrConflict) {
		t.Errorf("duplicate username error = %v, want ErrConflict", err)
	}
}

func TestUserRepositoryPersistsLockoutState(t *testing.T) {
	pool := testPool(t)
	repo := NewUserRepository(pool)
	ctx := context.Background()
	u := newTestUser(t, pool, identity.RoleReadOnly)

	now := time.Now().UTC().Truncate(time.Microsecond)
	for i := 0; i < identity.MaxFailedLogins; i++ {
		u.RegisterFailedLogin(now)
	}
	if err := repo.Update(ctx, u); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := repo.GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !got.IsLocked(now) {
		t.Error("lockout did not survive the round trip")
	}
}

func TestSessionRotationAndReuseRevokesFamily(t *testing.T) {
	pool := testPool(t)
	sessions := NewSessionRepository(pool)
	ctx := context.Background()
	u := newTestUser(t, pool, identity.RoleOperator)
	now := time.Now().UTC().Truncate(time.Microsecond)

	first := identity.NewSession(u.ID, []byte("hash-a-"+u.ID.String()), "203.0.113.7:5555", "go-test", now)
	if err := sessions.Create(ctx, first); err != nil {
		t.Fatalf("Create: %v", err)
	}

	loaded, err := sessions.GetByTokenHash(ctx, first.TokenHash)
	if err != nil {
		t.Fatalf("GetByTokenHash: %v", err)
	}
	if loaded.IP != "203.0.113.7" {
		t.Errorf("IP = %q, want the host portion 203.0.113.7", loaded.IP)
	}

	next := loaded.Rotate([]byte("hash-b-"+u.ID.String()), "203.0.113.7:5556", "go-test", now.Add(time.Minute))
	if err := sessions.Rotate(ctx, loaded, next); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// Rotating the same session again is exactly the reuse case.
	replay := loaded.Rotate([]byte("hash-c-"+u.ID.String()), "", "", now.Add(2*time.Minute))
	if err := sessions.Rotate(ctx, loaded, replay); !errors.Is(err, identity.ErrRefreshTokenReuse) {
		t.Fatalf("replayed rotation error = %v, want ErrRefreshTokenReuse", err)
	}

	if err := sessions.RevokeFamily(ctx, first.FamilyID, now.Add(3*time.Minute)); err != nil {
		t.Fatalf("RevokeFamily: %v", err)
	}
	active, err := sessions.IsSessionActive(ctx, next.ID)
	if err != nil {
		t.Fatalf("IsSessionActive: %v", err)
	}
	if active {
		t.Error("descendant session still active after family revocation")
	}
}

func TestIsSessionActiveFollowsUserDeactivation(t *testing.T) {
	pool := testPool(t)
	users, sessions := NewUserRepository(pool), NewSessionRepository(pool)
	ctx := context.Background()
	u := newTestUser(t, pool, identity.RoleOperator)
	now := time.Now().UTC()

	s := identity.NewSession(u.ID, []byte("hash-active-"+u.ID.String()), "", "", now)
	if err := sessions.Create(ctx, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if active, _ := sessions.IsSessionActive(ctx, s.ID); !active {
		t.Fatal("fresh session reported inactive")
	}

	u.Deactivate()
	if err := users.Update(ctx, u); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if active, _ := sessions.IsSessionActive(ctx, s.ID); active {
		t.Error("session still active after the account was deactivated")
	}
}

func TestAuditWriteLandsInPartition(t *testing.T) {
	pool := testPool(t)
	audit := NewAuditRepository(pool)
	ctx := context.Background()
	u := newTestUser(t, pool, identity.RoleAdmin)
	reqID := uuid.NewString()

	err := audit.Write(ctx, ports.AuditEntry{
		Time:        time.Now().UTC(),
		ActorUserID: &u.ID,
		ActorName:   u.Username,
		Category:    ports.AuditCategoryAuth,
		Action:      "login_success",
		SourceIP:    "198.51.100.9",
		RequestID:   reqID,
		Details:     map[string]any{"method": "password"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	var action, outcome string
	var details []byte
	err = pool.QueryRow(ctx, `SELECT action, outcome, details FROM audit_logs WHERE request_id = $1`, reqID).
		Scan(&action, &outcome, &details)
	if err != nil {
		t.Fatalf("read back audit entry: %v", err)
	}
	if action != "login_success" || outcome != ports.OutcomeSuccess {
		t.Errorf("action=%q outcome=%q, want login_success/success", action, outcome)
	}
	if string(details) == "" {
		t.Error("details were not persisted")
	}
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-32) + s[1:]
}
