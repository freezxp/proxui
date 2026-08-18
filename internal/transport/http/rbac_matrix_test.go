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
	"github.com/freezxp/proxui/internal/app/shellreg"
	"github.com/freezxp/proxui/internal/domain/access"
	"github.com/freezxp/proxui/internal/domain/console"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/domain/shell"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// This is the security backbone of the test suite: it drives every route in
// the permission map with every role and asserts the declared outcome. A new
// endpoint automatically gains denial coverage the moment it is declared, and
// a route wired with the wrong role gate fails here rather than in production.

var allRoles = []identity.Role{
	identity.RoleAdmin, identity.RoleOperator, identity.RoleReadOnly, identity.RoleAuditor,
	identity.RoleNewUser,
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

func (stubAccess) CreateUserGroup(context.Context, *access.UserGroup) error        { return nil }
func (stubAccess) ListUserGroups(context.Context) ([]access.UserGroup, error)      { return nil, nil }
func (stubAccess) DeleteUserGroup(context.Context, uuid.UUID) error                { return nil }
func (stubAccess) SetUserGroups(context.Context, uuid.UUID, []uuid.UUID) error     { return nil }
func (stubAccess) UserGroupNames(context.Context, uuid.UUID) ([]string, error)     { return nil, nil }
func (stubAccess) CreateVMGroup(context.Context, *access.VMGroup) error            { return nil }
func (stubAccess) ListVMGroups(context.Context) ([]access.VMGroup, error)          { return nil, nil }
func (stubAccess) DeleteVMGroup(context.Context, uuid.UUID) error                  { return nil }
func (stubAccess) SetVMGroupMembers(context.Context, uuid.UUID, []uuid.UUID) error { return nil }
func (stubAccess) VMGroupMemberIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}
func (stubAccess) CreateGrant(context.Context, *access.Grant) error   { return nil }
func (stubAccess) ListGrants(context.Context) ([]access.Grant, error) { return nil, nil }
func (stubAccess) DeleteGrant(context.Context, uuid.UUID) error       { return nil }
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
func (stubUsers) GetByEmail(context.Context, string) (*identity.User, error) {
	return testUser(), nil
}
func (stubUsers) GetByExternalID(context.Context, identity.AuthProvider, string) (*identity.User, error) {
	return nil, ports.ErrNotFound
}
func (stubUsers) Update(context.Context, *identity.User) error { return nil }
func (stubUsers) Delete(context.Context, uuid.UUID) error      { return nil }
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
	return matrixServerAs(role, uuid.New(), stubUsers{})
}

// matrixServerAs is the same server with the caller's own identity and user
// repository chosen, for the tests that care what the handler does rather than
// who is allowed to reach it.
func matrixServerAs(role identity.Role, actorID uuid.UUID, userRepo ports.UserRepository) *Server {
	users, accessRepo, sessions := userRepo, stubAccess{}, stubSessions{}
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
			Tokens:   roleTokenParser{role: role, userID: actorID},
			Sessions: &fakeSessionChecker{active: true},
			MFA:      stubMFA{},
		},
		Metrics: MetricsDeps{Metrics: stubMetrics{}},
		Console: ConsoleDeps{
			Open: &command.OpenConsole{
				Inventory: stubInventory{}, Sessions: stubConsole{},
				Tickets: stubTickets{}, Audit: audit, Clock: clock,
			},
			Sessions: stubConsole{},
		},
		Inventory: InventoryDeps{
			Inventory: stubInventory{}, Audit: stubAudit{}, Metrics: stubMetrics{},
		},
		// Wired with a dialer that reaches nothing: the matrix measures who is
		// allowed through the gate, and a handler that panicked on a nil
		// dependency would be indistinguishable from one that let them.
		Shell: ShellDeps{
			Open: &command.OpenShell{
				Inventory: stubInventory{}, Sessions: stubShell{}, HostKeys: stubHostKeys{},
				Tickets: shellreg.NewTicketStore(clock.Now), Registry: shellreg.NewRegistry(clock.Now),
				Dialer: refusingDialer{}, Audit: audit, Clock: clock,
			},
			Close: &command.CloseShell{Sessions: stubShell{}, Registry: shellreg.NewRegistry(clock.Now), Audit: audit, Clock: clock},
			Files: &command.ShellFiles{Registry: shellreg.NewRegistry(clock.Now), Audit: audit, Clock: clock},
			Keys: &command.PortalKey{
				Keys: stubPortalKey{}, KeyGen: stubKeyGen{},
				Registry: shellreg.NewRegistry(clock.Now), Inventory: stubInventory{},
				Audit: audit, Clock: clock,
			},
			Sessions: stubShell{},
			HostKeys: stubHostKeys{},
		},
		Admin: AdminDeps{
			CreateUser:    &command.CreateUser{Users: users, Access: accessRepo, Hasher: noopHasher{}, Audit: audit, Clock: clock},
			UpdateUser:    &command.UpdateUser{Users: users, Sessions: sessions, Audit: audit, Clock: clock},
			ResetPassword: &command.ResetPassword{Users: users, Sessions: sessions, Hasher: noopHasher{}, Audit: audit, Clock: clock},
			DeleteUser:    &command.DeleteUser{Users: users, Access: accessRepo, Audit: audit, Clock: clock},
			SetUserGroups: &command.SetUserGroups{Users: users, Access: accessRepo, Audit: audit, Clock: clock},
			ManageAccess:  &command.ManageAccess{Access: accessRepo, Audit: audit, Clock: clock},
			MFA: &command.MFA{
				Users: users, TOTP: stubTOTPStore{}, Codec: stubTOTPCodec{},
				Challenges: stubChallenges{}, Hasher: noopHasher{},
				Vault: matrixVault(), Audit: audit, Clock: clock,
			},
			Users:  users,
			Access: accessRepo,
		},
	})
}

// stubMetrics answers metrics reads with nothing, so the matrix measures
// authorization rather than data.
type stubMetrics struct{}

func (stubMetrics) WriteVMSamples(context.Context, []ports.VMSample) (int64, error) { return 0, nil }
func (stubMetrics) WriteHostSamples(context.Context, []ports.HostSample) (int64, error) {
	return 0, nil
}
func (stubMetrics) CounterState(context.Context, []uuid.UUID) (map[ports.CounterKey]ports.CounterValue, error) {
	return nil, nil
}
func (stubMetrics) SaveCounterState(context.Context, map[ports.CounterKey]ports.CounterValue) error {
	return nil
}
func (stubMetrics) VMSeries(context.Context, uuid.UUID, time.Time, time.Time, time.Time) (ports.MetricSeries, error) {
	return ports.MetricSeries{}, nil
}
func (stubMetrics) LatestVMMetrics(context.Context, time.Time) (map[uuid.UUID]ports.MetricPoint, error) {
	return nil, nil
}
func (stubMetrics) LastSampleTime(context.Context, uuid.UUID) (time.Time, error) {
	return time.Time{}, nil
}
func (stubMetrics) VMIDsByExternalID(context.Context, uuid.UUID) (map[string]uuid.UUID, error) {
	return nil, nil
}
func (stubMetrics) HostIDsByExternalID(context.Context, uuid.UUID) (map[string]uuid.UUID, error) {
	return nil, nil
}

// stubInventory and stubAudit answer read-model calls with nothing, so the
// matrix measures authorization rather than data.
type stubInventory struct{}

func (stubInventory) ListVMs(context.Context, ports.VMFilter) (ports.VMPage, error) {
	return ports.VMPage{}, nil
}
func (stubInventory) GetVM(context.Context, uuid.UUID, identity.Role, uuid.UUID) (ports.VMDetail, error) {
	return ports.VMDetail{}, nil
}
func (stubInventory) CanAccessVM(context.Context, uuid.UUID, identity.Role, uuid.UUID) (bool, error) {
	return true, nil
}
func (stubInventory) VMHistory(context.Context, uuid.UUID, int) ([]ports.HistoryEntry, error) {
	return nil, nil
}
func (stubInventory) SetPortalTags(context.Context, uuid.UUID, []string) error { return nil }
func (stubInventory) SetNotes(context.Context, uuid.UUID, string) error        { return nil }
func (stubInventory) Dashboard(context.Context, identity.Role, uuid.UUID) (ports.DashboardSummary, error) {
	return ports.DashboardSummary{}, nil
}

// stubPortalKey answers as a portal that has never generated a key: the
// authorization gate is what this file measures, and "no key yet" is the state
// every one of these routes has to survive without a panic.
type stubPortalKey struct{}

func (stubPortalKey) Get(context.Context) (shell.PortalKey, error) {
	return shell.PortalKey{}, ports.ErrNotFound
}
func (stubPortalKey) PrivateKey(context.Context) (string, error)             { return "", ports.ErrNotFound }
func (stubPortalKey) Replace(context.Context, shell.PortalKey, string) error { return nil }
func (stubPortalKey) Delete(context.Context) error                           { return nil }
func (stubPortalKey) Installs(context.Context, uuid.UUID) ([]shell.KeyInstall, error) {
	return nil, nil
}
func (stubPortalKey) RecordInstall(context.Context, shell.KeyInstall) error  { return nil }
func (stubPortalKey) ForgetInstall(context.Context, uuid.UUID, string) error { return nil }

// stubKeyGen hands back a fixed pair rather than burning entropy on a test
// that never dials anything.
type stubKeyGen struct{}

func (stubKeyGen) Generate(comment string) (string, shell.PortalKey, error) {
	return "-----BEGIN OPENSSH PRIVATE KEY-----\nnot-a-real-key\n-----END OPENSSH PRIVATE KEY-----\n",
		shell.PortalKey{
			PublicKey:   "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKeyForTheMatrix " + comment,
			Algorithm:   "ssh-ed25519",
			Fingerprint: "SHA256:matrix",
		}, nil
}

// The second factor, stubbed to the state every one of its routes has to
// survive: nothing enrolled, no challenge outstanding (AUTH-04).
type stubMFA struct{}

func (stubMFA) Begin(context.Context, command.Actor) (command.EnrollTOTPOutput, error) {
	return command.EnrollTOTPOutput{Secret: "JBSWY3DPEHPK3PXP", OTPAuthURL: "otpauth://totp/x"}, nil
}
func (stubMFA) Confirm(context.Context, command.Actor, string) error { return nil }
func (stubMFA) Disable(context.Context, command.Actor, string) error { return nil }
func (stubMFA) Verify(context.Context, command.VerifyMFAInput) (command.LoginOutput, error) {
	return command.LoginOutput{}, identity.ErrMFAChallengeNotFound
}

type stubTOTPStore struct{}

func (stubTOTPStore) Enroll(context.Context, uuid.UUID, crypto.SealedSecret, time.Time) error {
	return nil
}
func (stubTOTPStore) Secret(context.Context, uuid.UUID, *crypto.Vault) (string, error) {
	return "", ports.ErrNotFound
}
func (stubTOTPStore) Enable(context.Context, uuid.UUID, int64, time.Time) error { return nil }
func (stubTOTPStore) Disable(context.Context, uuid.UUID, time.Time) error       { return nil }
func (stubTOTPStore) LastStep(context.Context, uuid.UUID) (*int64, error)       { return nil, nil }
func (stubTOTPStore) RecordStep(context.Context, uuid.UUID, int64, time.Time) error {
	return nil
}

type stubTOTPCodec struct{}

func (stubTOTPCodec) NewSecret() (string, error)                       { return "JBSWY3DPEHPK3PXP", nil }
func (stubTOTPCodec) Validate(string, string, time.Time) (int64, bool) { return 0, false }
func (stubTOTPCodec) URL(string, string) string                        { return "otpauth://totp/x" }

type stubChallenges struct{}

func (stubChallenges) Issue(context.Context, identity.MFAChallenge) error { return nil }
func (stubChallenges) Get(context.Context, string) (identity.MFAChallenge, error) {
	return identity.MFAChallenge{}, ports.ErrNotFound
}
func (stubChallenges) Fail(context.Context, string) (identity.MFAChallenge, error) {
	return identity.MFAChallenge{}, ports.ErrNotFound
}
func (stubChallenges) Consume(context.Context, string) error { return nil }

func matrixVault() *crypto.Vault {
	key := make([]byte, crypto.MasterKeySize)
	for i := range key {
		key[i] = byte(i)
	}
	v, _ := crypto.NewVault(key, 1)
	return v
}

type stubAudit struct{}

func (stubAudit) Search(context.Context, ports.AuditFilter) (ports.AuditPage, error) {
	return ports.AuditPage{}, nil
}
func (stubAudit) Stream(context.Context, ports.AuditFilter, func(ports.AuditRecord) error) error {
	return nil
}
func (stubAudit) Categories(context.Context) (map[string][]string, error) { return nil, nil }

type stubConsole struct{}

func (stubConsole) Create(context.Context, *console.Session) error            { return nil }
func (stubConsole) MarkConnected(context.Context, uuid.UUID, time.Time) error { return nil }
func (stubConsole) Close(context.Context, uuid.UUID, string, int64, int64, time.Time) error {
	return nil
}
func (stubConsole) Get(context.Context, uuid.UUID) (*console.Session, error) {
	return &console.Session{}, nil
}
func (stubConsole) List(context.Context, bool, int) ([]ports.ConsoleSessionRecord, error) {
	return nil, nil
}

type stubTickets struct{}

func (stubTickets) Issue(context.Context, console.Ticket) error { return nil }
func (stubTickets) Redeem(context.Context, string) (console.Ticket, error) {
	return console.Ticket{}, console.ErrTicketNotFound
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
	for _, param := range []string{"{userID}", "{groupID}", "{grantID}", "{platformID}", "{vmID}", "{ticketID}"} {
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

// stubShell, stubHostKeys and refusingDialer keep the SSH routes answerable in
// the matrix without an sshd anywhere near it.
type stubShell struct{}

func (stubShell) Create(context.Context, *shell.Session) error              { return nil }
func (stubShell) MarkConnected(context.Context, uuid.UUID, time.Time) error { return nil }
func (stubShell) Close(context.Context, uuid.UUID, string, int64, int64, time.Time) error {
	return nil
}
func (stubShell) Get(context.Context, uuid.UUID) (*shell.Session, error) {
	return nil, ports.ErrNotFound
}
func (stubShell) List(context.Context, bool, int) ([]ports.ShellSessionRecord, error) {
	return nil, nil
}

type stubHostKeys struct{}

func (stubHostKeys) Get(context.Context, uuid.UUID) (shell.HostKey, error) {
	return shell.HostKey{}, ports.ErrNotFound
}
func (stubHostKeys) Trust(context.Context, shell.HostKey) error { return nil }
func (stubHostKeys) Forget(context.Context, uuid.UUID) error    { return nil }

type refusingDialer struct{}

func (refusingDialer) Dial(context.Context, ports.SSHTarget, ports.SSHCredential, ports.HostKeyPolicy) (ports.ShellConn, error) {
	return nil, shell.ErrUnreachable
}
