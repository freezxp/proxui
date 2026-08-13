package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/access"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// This is the security backbone of the test suite: it drives every route in
// the permission map with every role and asserts the declared outcome. A new
// endpoint automatically gains denial coverage the moment it is declared, and
// a route wired with the wrong role gate fails here rather than in production.

var allRoles = []identity.Role{
	identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor,
}

// roleTokenParser hands out claims for whichever role the test is exercising.
type roleTokenParser struct {
	role   identity.Role
	userID uuid.UUID
}

func (p roleTokenParser) Parse(string) (*crypto.Claims, error) {
	claims := &crypto.Claims{Role: string(p.role), SessionID: uuid.NewString()}
	claims.Subject = p.userID.String()
	return claims, nil
}

// stubAccess and stubUsers satisfy the repositories the admin routes need,
// returning empty results so the matrix measures authorization, not behaviour.
type stubAccess struct{}

func (stubAccess) CreateUserGroup(context.Context, *access.UserGroup) error    { return nil }
func (stubAccess) ListUserGroups(context.Context) ([]access.UserGroup, error)  { return nil, nil }
func (stubAccess) DeleteUserGroup(context.Context, uuid.UUID) error            { return nil }
func (stubAccess) SetUserGroups(context.Context, uuid.UUID, []uuid.UUID) error { return nil }
func (stubAccess) UserGroupNames(context.Context, uuid.UUID) ([]string, error) { return nil, nil }
func (stubAccess) CreateVMGroup(context.Context, *access.VMGroup) error        { return nil }
func (stubAccess) ListVMGroups(context.Context) ([]access.VMGroup, error)      { return nil, nil }
func (stubAccess) DeleteVMGroup(context.Context, uuid.UUID) error              { return nil }
func (stubAccess) CreateGrant(context.Context, *access.Grant) error            { return nil }
func (stubAccess) ListGrants(context.Context) ([]access.Grant, error)          { return nil, nil }
func (stubAccess) DeleteGrant(context.Context, uuid.UUID) error                { return nil }
func (stubAccess) VisibleVMGroupIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

type stubUsers struct{}

func (stubUsers) Create(context.Context, *identity.User) error { return nil }
func (stubUsers) GetByID(context.Context, uuid.UUID) (*identity.User, error) {
	return testUser(), nil
}
func (stubUsers) GetByUsername(context.Context, string) (*identity.User, error) {
	return testUser(), nil
}
func (stubUsers) Update(context.Context, *identity.User) error { return nil }
func (stubUsers) CountAll(context.Context) (int, error)        { return 1, nil }
func (stubUsers) List(context.Context, ports.UserFilter) ([]*identity.User, error) {
	return nil, nil
}

type stubSessions struct{}

func (stubSessions) Create(context.Context, *identity.Session) error { return nil }
func (stubSessions) GetByTokenHash(context.Context, []byte) (*identity.Session, error) {
	return nil, ports.ErrNotFound
}
func (stubSessions) Rotate(context.Context, *identity.Session, *identity.Session) error { return nil }
func (stubSessions) RevokeFamily(context.Context, uuid.UUID, time.Time) error           { return nil }
func (stubSessions) RevokeAllForUser(context.Context, uuid.UUID, time.Time) error       { return nil }
func (stubSessions) IsSessionActive(context.Context, uuid.UUID) (bool, error)           { return true, nil }

func matrixServer(role identity.Role) *Server {
	users, accessRepo, sessions := stubUsers{}, stubAccess{}, stubSessions{}
	audit := &noopAudit{}
	clock := ports.SystemClock{}

	return NewServer(ServerConfig{
		Log:     zerolog.New(io.Discard),
		Version: "test",
		Auth: AuthDeps{
			Login:    &fakeLogin{out: command.LoginOutput{User: testUser()}},
			Refresh:  &fakeRefresh{out: command.LoginOutput{User: testUser()}},
			Logout:   &fakeLogout{},
			Users:    &fakeUserLoader{user: testUser()},
			Tokens:   roleTokenParser{role: role, userID: uuid.New()},
			Sessions: &fakeSessionChecker{active: true},
		},
		Admin: AdminDeps{
			CreateUser:    &command.CreateUser{Users: users, Access: accessRepo, Hasher: noopHasher{}, Audit: audit, Clock: clock},
			UpdateUser:    &command.UpdateUser{Users: users, Sessions: sessions, Audit: audit, Clock: clock},
			ResetPassword: &command.ResetPassword{Users: users, Sessions: sessions, Hasher: noopHasher{}, Audit: audit, Clock: clock},
			SetUserGroups: &command.SetUserGroups{Users: users, Access: accessRepo, Audit: audit, Clock: clock},
			ManageAccess:  &command.ManageAccess{Access: accessRepo, Audit: audit, Clock: clock},
			Users:         users,
			Access:        accessRepo,
		},
	})
}

type noopAudit struct{}

func (noopAudit) Write(context.Context, ports.AuditEntry) error { return nil }

type noopHasher struct{}

func (noopHasher) Hash(p string) (string, error)    { return "hashed:" + p, nil }
func (noopHasher) Verify(p, h string) (bool, error) { return h == "hashed:"+p, nil }

// requestFor turns a permission-map key into a concrete request, substituting
// a real UUID for each path parameter and a minimal valid body for writes.
func requestFor(t *testing.T, key string) *http.Request {
	t.Helper()
	method, pattern, _ := strings.Cut(key, " ")

	path := pattern
	for _, param := range []string{"{userID}", "{groupID}", "{grantID}"} {
		path = strings.ReplaceAll(path, param, uuid.NewString())
	}

	var body io.Reader = strings.NewReader("{}")
	if method == http.MethodGet || method == http.MethodDelete {
		body = nil
	}
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestRBACMatrix(t *testing.T) {
	for _, key := range PermissionRoutes() {
		perm, _ := PermissionFor(strings.Split(key, " ")[0], strings.SplitN(key, " ", 2)[1])

		for _, role := range allRoles {
			t.Run(key+"/"+string(role), func(t *testing.T) {
				req := requestFor(t, key)
				req.Header.Set("Authorization", "Bearer token-for-"+string(role))
				rec := httptest.NewRecorder()
				matrixServer(role).Routes().ServeHTTP(rec, req)

				denied := rec.Code == http.StatusForbidden
				if perm.Allows(role) && denied {
					t.Errorf("%s: role %s was denied but the permission map allows it", key, role)
				}
				if !perm.Allows(role) && !denied {
					t.Errorf("%s: role %s got %d but the permission map forbids it (want 403)", key, role, rec.Code)
				}
			})
		}

		// Anonymous callers must hit the auth gate on every non-public route.
		// Public routes may still refuse a request for their own reasons (a
		// refresh with no cookie is 401), so the assertion is on the reason
		// code, not the bare status.
		t.Run(key+"/anonymous", func(t *testing.T) {
			rec := httptest.NewRecorder()
			matrixServer(identity.RoleAdmin).Routes().ServeHTTP(rec, requestFor(t, key))

			var p Problem
			_ = json.Unmarshal(rec.Body.Bytes(), &p)

			if perm.Access == AccessPublic {
				if p.Code == "auth.missing_token" {
					t.Errorf("%s: public route demanded an access token", key)
				}
				return
			}
			if rec.Code != http.StatusUnauthorized || p.Code != "auth.missing_token" {
				t.Errorf("%s: anonymous caller got %d/%q, want 401/auth.missing_token", key, rec.Code, p.Code)
			}
		})
	}
}

func TestEveryWiredRouteIsDeclared(t *testing.T) {
	// Deny by default: a route that ships without a permission-map entry must
	// fail the build, not quietly serve traffic.
	router, ok := matrixServer(identity.RoleAdmin).Routes().(chi.Routes)
	if !ok {
		t.Fatal("router does not expose chi.Routes")
	}
	if err := ValidatePermissions(router); err != nil {
		t.Fatalf("permission map out of sync with the router: %v", err)
	}
}

func TestPermissionAllows(t *testing.T) {
	adminOnly := roles(identity.RoleAdmin)
	if !adminOnly.Allows(identity.RoleAdmin) {
		t.Error("admin-only permission denied admin")
	}
	for _, r := range []identity.Role{identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor} {
		if adminOnly.Allows(r) {
			t.Errorf("admin-only permission allowed %s", r)
		}
	}
	authenticated := Permission{Access: AccessAuthenticated}
	for _, r := range allRoles {
		if !authenticated.Allows(r) {
			t.Errorf("authenticated permission denied %s", r)
		}
	}
}
