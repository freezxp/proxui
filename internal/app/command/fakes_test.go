package command

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/access"
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
		// Both columns are UNIQUE in the schema. The fake enforced only the
		// username, which hid the fact that a duplicate email is refused the
		// same way — the property registration relies on to avoid becoming an
		// account-enumeration oracle.
		if strings.EqualFold(existing.Username, u.Username) ||
			strings.EqualFold(existing.Email, u.Email) {
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

func (f *fakeUsers) GetByEmail(_ context.Context, email string) (*identity.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.byID {
		if strings.EqualFold(u.Email, email) {
			return u, nil
		}
	}
	return nil, ports.ErrNotFound
}

func (f *fakeUsers) GetByExternalID(_ context.Context, provider identity.AuthProvider, externalID string) (*identity.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.byID {
		if u.AuthProvider == provider && u.ExternalID != "" && u.ExternalID == externalID {
			return u, nil
		}
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

func (f *fakeUsers) List(_ context.Context, filter ports.UserFilter) ([]*identity.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*identity.User
	for _, u := range f.byID {
		if filter.Query != "" && !strings.Contains(strings.ToLower(u.Username), strings.ToLower(filter.Query)) {
			continue
		}
		if filter.Role != "" && string(u.Role) != filter.Role {
			continue
		}
		if filter.Active != nil && u.IsActive != *filter.Active {
			continue
		}
		out = append(out, u)
	}
	return out, nil
}

// fakeAccess implements ports.AccessRepository in memory.
type fakeAccess struct {
	mu         sync.Mutex
	userGroups map[uuid.UUID]*access.UserGroup
	vmGroups   map[uuid.UUID]*access.VMGroup
	grants     map[uuid.UUID]*access.Grant
	membership map[uuid.UUID][]uuid.UUID // user -> user groups
	// vmGroupMembers is VM group -> VMs, the link that makes a grant confer
	// access to anything at all.
	vmGroupMembers map[uuid.UUID][]uuid.UUID
}

func newFakeAccess() *fakeAccess {
	return &fakeAccess{
		userGroups: map[uuid.UUID]*access.UserGroup{},
		vmGroups:   map[uuid.UUID]*access.VMGroup{},
		grants:     map[uuid.UUID]*access.Grant{},
		membership: map[uuid.UUID][]uuid.UUID{},
	}
}

func (f *fakeAccess) CreateUserGroup(_ context.Context, g *access.UserGroup) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.userGroups {
		if strings.EqualFold(existing.Name, g.Name) {
			return ports.ErrConflict
		}
	}
	f.userGroups[g.ID] = g
	return nil
}

func (f *fakeAccess) ListUserGroups(context.Context) ([]access.UserGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]access.UserGroup, 0, len(f.userGroups))
	for _, g := range f.userGroups {
		out = append(out, *g)
	}
	return out, nil
}

func (f *fakeAccess) DeleteUserGroup(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.userGroups[id]; !ok {
		return ports.ErrNotFound
	}
	delete(f.userGroups, id)
	for gid, g := range f.grants {
		if g.UserGroupID == id {
			delete(f.grants, gid)
		}
	}
	return nil
}

func (f *fakeAccess) SetUserGroups(_ context.Context, userID uuid.UUID, groupIDs []uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, gid := range groupIDs {
		if _, ok := f.userGroups[gid]; !ok {
			return ports.ErrNotFound
		}
	}
	f.membership[userID] = groupIDs
	return nil
}

func (f *fakeAccess) UserGroupNames(_ context.Context, userID uuid.UUID) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, gid := range f.membership[userID] {
		if g, ok := f.userGroups[gid]; ok {
			out = append(out, g.Name)
		}
	}
	return out, nil
}

func (f *fakeAccess) CreateVMGroup(_ context.Context, g *access.VMGroup) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, existing := range f.vmGroups {
		if strings.EqualFold(existing.Name, g.Name) {
			return ports.ErrConflict
		}
	}
	f.vmGroups[g.ID] = g
	return nil
}

func (f *fakeAccess) ListVMGroups(context.Context) ([]access.VMGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]access.VMGroup, 0, len(f.vmGroups))
	for _, g := range f.vmGroups {
		out = append(out, *g)
	}
	return out, nil
}

func (f *fakeAccess) SetVMGroupMembers(_ context.Context, groupID uuid.UUID, vmIDs []uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.vmGroupMembers == nil {
		f.vmGroupMembers = map[uuid.UUID][]uuid.UUID{}
	}
	f.vmGroupMembers[groupID] = append([]uuid.UUID(nil), vmIDs...)
	return nil
}

func (f *fakeAccess) VMGroupMemberIDs(_ context.Context, groupID uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uuid.UUID(nil), f.vmGroupMembers[groupID]...), nil
}

func (f *fakeAccess) DeleteVMGroup(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.vmGroups[id]; !ok {
		return ports.ErrNotFound
	}
	delete(f.vmGroups, id)
	return nil
}

func (f *fakeAccess) CreateGrant(_ context.Context, g *access.Grant) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.userGroups[g.UserGroupID]; !ok {
		return ports.ErrNotFound
	}
	if _, ok := f.vmGroups[g.VMGroupID]; !ok {
		return ports.ErrNotFound
	}
	f.grants[g.ID] = g
	return nil
}

func (f *fakeAccess) ListGrants(context.Context) ([]access.Grant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]access.Grant, 0, len(f.grants))
	for _, g := range f.grants {
		out = append(out, *g)
	}
	return out, nil
}

func (f *fakeAccess) DeleteGrant(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.grants[id]; !ok {
		return ports.ErrNotFound
	}
	delete(f.grants, id)
	return nil
}

func (f *fakeAccess) VisibleVMGroupIDs(_ context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uuid.UUID
	for _, ugID := range f.membership[userID] {
		for _, g := range f.grants {
			if g.UserGroupID == ugID {
				out = append(out, g.VMGroupID)
			}
		}
	}
	return out, nil
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

func (f *fakeTokens) Issue(userID uuid.UUID, role, username string, sessionID uuid.UUID, _ time.Time) (string, time.Duration, error) {
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

// hashOf mirrors how the session repository keys refresh tokens.
func hashOf(token string) []byte { return crypto.HashToken(token) }
