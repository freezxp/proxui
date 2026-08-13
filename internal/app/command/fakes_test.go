package command

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// In-memory fakes let the command tests exercise real domain logic without a
// database. They implement the same ports the Postgres adapters do.

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

type fakeUsers struct {
	mu    sync.Mutex
	byID  map[uuid.UUID]*identity.User
	calls int
}

func newFakeUsers(users ...*identity.User) *fakeUsers {
	f := &fakeUsers{byID: map[uuid.UUID]*identity.User{}}
	for _, u := range users {
		f.byID[u.ID] = u
	}
	return f
}

func (f *fakeUsers) Create(_ context.Context, u *identity.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.byID {
		if strings.EqualFold(existing.Username, u.Username) {
			return ports.ErrConflict
		}
	}
	f.byID[u.ID] = u
	return nil
}

func (f *fakeUsers) GetByID(_ context.Context, id uuid.UUID) (*identity.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.byID[id]; ok {
		return u, nil
	}
	return nil, ports.ErrNotFound
}

func (f *fakeUsers) GetByUsername(_ context.Context, username string) (*identity.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.byID {
		if strings.EqualFold(u.Username, username) {
			return u, nil
		}
	}
	return nil, ports.ErrNotFound
}

func (f *fakeUsers) Update(_ context.Context, u *identity.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byID[u.ID]; !ok {
		return ports.ErrNotFound
	}
	f.byID[u.ID] = u
	f.calls++
	return nil
}

func (f *fakeUsers) CountAll(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byID), nil
}

type fakeSessions struct {
	mu        sync.Mutex
	byHash    map[string]*identity.Session
	revoked   map[uuid.UUID]bool // by family
	rotateErr error
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{byHash: map[string]*identity.Session{}, revoked: map[uuid.UUID]bool{}}
}

func (f *fakeSessions) Create(_ context.Context, s *identity.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byHash[string(s.TokenHash)] = s
	return nil
}

func (f *fakeSessions) GetByTokenHash(_ context.Context, hash []byte) (*identity.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.byHash[string(hash)]
	if !ok {
		return nil, ports.ErrNotFound
	}
	copied := *s
	if f.revoked[s.FamilyID] && copied.RevokedAt.IsZero() {
		copied.RevokedAt = time.Now()
	}
	return &copied, nil
}

func (f *fakeSessions) Rotate(_ context.Context, old, next *identity.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rotateErr != nil {
		return f.rotateErr
	}
	stored, ok := f.byHash[string(old.TokenHash)]
	if !ok {
		return ports.ErrNotFound
	}
	if stored.IsRotated() {
		return identity.ErrRefreshTokenReuse
	}
	stored.RotatedAt = old.RotatedAt
	f.byHash[string(next.TokenHash)] = next
	return nil
}

func (f *fakeSessions) RevokeFamily(_ context.Context, familyID uuid.UUID, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked[familyID] = true
	return nil
}

func (f *fakeSessions) RevokeAllForUser(_ context.Context, userID uuid.UUID, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.byHash {
		if s.UserID == userID {
			f.revoked[s.FamilyID] = true
		}
	}
	return nil
}

func (f *fakeSessions) IsSessionActive(_ context.Context, sessionID uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.byHash {
		if s.ID == sessionID {
			return !f.revoked[s.FamilyID] && !s.IsRevoked(), nil
		}
	}
	return false, nil
}

type fakeAudit struct {
	mu      sync.Mutex
	entries []ports.AuditEntry
}

func (f *fakeAudit) Write(_ context.Context, e ports.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeAudit) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.entries))
	for i, e := range f.entries {
		out[i] = e.Action
	}
	return out
}

func (f *fakeAudit) has(action string) bool {
	for _, a := range f.actions() {
		if a == action {
			return true
		}
	}
	return false
}

type fakeTokens struct{ issued int }

func (f *fakeTokens) Issue(userID uuid.UUID, role string, sessionID uuid.UUID, _ time.Time) (string, time.Duration, error) {
	f.issued++
	return "access-token-" + sessionID.String(), 15 * time.Minute, nil
}

// fastHasher keeps command tests quick: argon2id itself is covered by the
// crypto package's own tests.
type fastHasher struct{ failVerify error }

func (h fastHasher) Hash(password string) (string, error) { return "hashed:" + password, nil }

func (h fastHasher) Verify(password, encodedHash string) (bool, error) {
	if h.failVerify != nil {
		return false, h.failVerify
	}
	if !strings.HasPrefix(encodedHash, "hashed:") {
		return false, errors.New("malformed hash")
	}
	return strings.TrimPrefix(encodedHash, "hashed:") == password, nil
}

func mustUser(t interface{ Fatalf(string, ...any) }, username, password string, role identity.Role) *identity.User {
	hash, err := fastHasher{}.Hash(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return &identity.User{
		ID:           uuid.New(),
		Username:     username,
		Email:        username + "@example.test",
		DisplayName:  username,
		PasswordHash: hash,
		Role:         role,
		IsActive:     true,
	}
}

func newRefreshToken() (string, []byte) {
	token, hash, err := crypto.NewOpaqueToken()
	if err != nil {
		panic(err)
	}
	return token, hash
}
