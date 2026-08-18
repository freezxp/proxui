package command

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/access"
	"github.com/freezxp/proxui/internal/domain/identity"
)

func adminActor() Actor {
	return Actor{UserID: uuid.New(), Username: "admin", IP: "10.0.0.1", RequestID: "req-test"}
}

func TestCreateUserForcesPasswordChange(t *testing.T) {
	users, accessRepo, audit := newFakeUsers(), newFakeAccess(), &fakeAudit{}
	h := &CreateUser{Users: users, Access: accessRepo, Hasher: fastHasher{}, Audit: audit, Clock: &fakeClock{testNow}}

	user, err := h.Handle(context.Background(), CreateUserInput{
		Actor: adminActor(), Username: "newuser", Email: "new@example.test",
		Role: identity.RoleOperator, TempPassword: "temporary-password-1",
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if !user.MustChangePassword {
		t.Error("MustChangePassword = false; the admin who set the temp password would retain access")
	}
	if user.DisplayName != "newuser" {
		t.Errorf("DisplayName = %q, want the username as fallback", user.DisplayName)
	}
	if !audit.has("user_created") {
		t.Errorf("audit actions = %v, want user_created", audit.actions())
	}
}

func TestCreateUserRejectsBadRoleAndWeakPassword(t *testing.T) {
	h := &CreateUser{Users: newFakeUsers(), Access: newFakeAccess(), Hasher: fastHasher{}, Audit: &fakeAudit{}, Clock: &fakeClock{testNow}}
	ctx := context.Background()

	_, err := h.Handle(ctx, CreateUserInput{
		Actor: adminActor(), Username: "u1", Email: "u1@example.test",
		Role: identity.Role("superuser"), TempPassword: "temporary-password-1",
	})
	if !errors.Is(err, identity.ErrInvalidRole) {
		t.Errorf("invalid role error = %v, want ErrInvalidRole", err)
	}

	_, err = h.Handle(ctx, CreateUserInput{
		Actor: adminActor(), Username: "u2", Email: "u2@example.test",
		Role: identity.RoleReadOnly, TempPassword: "short",
	})
	if !errors.Is(err, identity.ErrWeakPassword) {
		t.Errorf("weak password error = %v, want ErrWeakPassword", err)
	}
}

func TestCreateUserRejectsDuplicateUsername(t *testing.T) {
	existing := mustUser(t, "taken", "correct horse battery staple", identity.RoleReadOnly)
	h := &CreateUser{Users: newFakeUsers(existing), Access: newFakeAccess(), Hasher: fastHasher{}, Audit: &fakeAudit{}, Clock: &fakeClock{testNow}}

	_, err := h.Handle(context.Background(), CreateUserInput{
		Actor: adminActor(), Username: "TAKEN", Email: "other@example.test",
		Role: identity.RoleReadOnly, TempPassword: "temporary-password-1",
	})
	if !errors.Is(err, ports.ErrConflict) {
		t.Errorf("duplicate username error = %v, want ErrConflict", err)
	}
}

func TestDeactivatingUserRevokesSessions(t *testing.T) {
	user := mustUser(t, "jsmith", "correct horse battery staple", identity.RoleOperator)
	users, sessions, audit := newFakeUsers(user), newFakeSessions(), &fakeAudit{}
	clock := &fakeClock{testNow}

	// Give the user a live session first.
	login := newLogin(users, sessions, audit, clock)
	out, err := login.Handle(context.Background(), LoginInput{Username: "jsmith", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	session, err := sessions.GetByTokenHash(context.Background(), hashOf(out.RefreshToken))
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if active, _ := sessions.IsSessionActive(context.Background(), session.ID); !active {
		t.Fatal("session inactive before deactivation")
	}

	inactive := false
	h := &UpdateUser{Users: users, Sessions: sessions, Audit: audit, Clock: clock}
	if _, err := h.Handle(context.Background(), UpdateUserInput{
		Actor: adminActor(), UserID: user.ID, IsActive: &inactive,
	}); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	if active, _ := sessions.IsSessionActive(context.Background(), session.ID); active {
		t.Error("session still active after deactivation; access outlives the account")
	}
	if !audit.has("user_updated") {
		t.Errorf("audit actions = %v, want user_updated", audit.actions())
	}
}

func TestUpdateUserRecordsRoleChange(t *testing.T) {
	user := mustUser(t, "jsmith", "correct horse battery staple", identity.RoleReadOnly)
	users, audit := newFakeUsers(user), &fakeAudit{}
	h := &UpdateUser{Users: users, Sessions: newFakeSessions(), Audit: audit, Clock: &fakeClock{testNow}}

	newRole := identity.RoleAdmin
	updated, err := h.Handle(context.Background(), UpdateUserInput{
		Actor: adminActor(), UserID: user.ID, Role: &newRole,
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if updated.Role != identity.RoleAdmin {
		t.Errorf("Role = %q, want admin", updated.Role)
	}

	// A privilege escalation must be reconstructible from the audit trail.
	var found bool
	for _, e := range audit.entries {
		if e.Action == "user_updated" {
			if change, ok := e.Details["role"]; ok {
				found = true
				m, _ := change.(map[string]any)
				if m["from"] != "readonly" || m["to"] != "admin" {
					t.Errorf("role change detail = %v, want readonly -> admin", change)
				}
			}
		}
	}
	if !found {
		t.Error("role change was not recorded in the audit details")
	}
}

func TestResetPasswordUnlocksAndRevokes(t *testing.T) {
	user := mustUser(t, "jsmith", "correct horse battery staple", identity.RoleOperator)
	for i := 0; i < identity.MaxFailedLogins; i++ {
		user.RegisterFailedLogin(testNow)
	}
	if !user.IsLocked(testNow) {
		t.Fatal("test setup: user should be locked")
	}

	users, sessions, audit := newFakeUsers(user), newFakeSessions(), &fakeAudit{}
	h := &ResetPassword{Users: users, Sessions: sessions, Hasher: fastHasher{}, Audit: audit, Clock: &fakeClock{testNow}}

	err := h.Handle(context.Background(), ResetPasswordInput{
		Actor: adminActor(), UserID: user.ID, TempPassword: "brand-new-password-2",
	})
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if user.IsLocked(testNow) {
		t.Error("account still locked after an admin password reset")
	}
	if !user.MustChangePassword {
		t.Error("MustChangePassword = false after reset")
	}
	if !user.LastLoginAt.IsZero() {
		t.Error("password reset recorded a login that never happened")
	}
	if !audit.has("user_password_reset") {
		t.Errorf("audit actions = %v, want user_password_reset", audit.actions())
	}
}

func TestGrantChangesAreAuditedAsSecurityEvents(t *testing.T) {
	accessRepo, audit := newFakeAccess(), &fakeAudit{}
	h := &ManageAccess{Access: accessRepo, Audit: audit, Clock: &fakeClock{testNow}}
	ctx := context.Background()

	ug, err := h.CreateUserGroup(ctx, GroupInput{Actor: adminActor(), Name: "ops"})
	if err != nil {
		t.Fatalf("CreateUserGroup: %v", err)
	}
	vg, err := h.CreateVMGroup(ctx, GroupInput{Actor: adminActor(), Name: "prod", AutoRule: map[string]any{"match": "pool", "value": "prod"}})
	if err != nil {
		t.Fatalf("CreateVMGroup: %v", err)
	}
	if len(vg.AutoRule) == 0 {
		t.Error("auto rule was not persisted on the VM group")
	}

	grant, err := h.CreateGrant(ctx, GrantInput{Actor: adminActor(), UserGroupID: ug.ID, VMGroupID: vg.ID})
	if err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}

	var category string
	for _, e := range audit.entries {
		if e.Action == "grant_created" {
			category = e.Category
		}
	}
	if category != ports.AuditCategorySecurity {
		t.Errorf("grant_created category = %q, want %q", category, ports.AuditCategorySecurity)
	}

	if err := h.DeleteGrant(ctx, adminActor(), grant.ID); err != nil {
		t.Fatalf("DeleteGrant: %v", err)
	}
	if !audit.has("grant_deleted") {
		t.Errorf("audit actions = %v, want grant_deleted", audit.actions())
	}
}

func TestGroupNameValidation(t *testing.T) {
	h := &ManageAccess{Access: newFakeAccess(), Audit: &fakeAudit{}, Clock: &fakeClock{testNow}}
	ctx := context.Background()

	for _, name := range []string{"", "   "} {
		if _, err := h.CreateUserGroup(ctx, GroupInput{Actor: adminActor(), Name: name}); !errors.Is(err, access.ErrInvalidName) {
			t.Errorf("CreateUserGroup(%q) error = %v, want ErrInvalidName", name, err)
		}
	}

	long := make([]byte, access.MaxNameLength+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := h.CreateVMGroup(ctx, GroupInput{Actor: adminActor(), Name: string(long)}); !errors.Is(err, access.ErrInvalidName) {
		t.Errorf("overlong group name error = %v, want ErrInvalidName", err)
	}
}

func TestSetUserGroupsRejectsUnknownGroup(t *testing.T) {
	user := mustUser(t, "jsmith", "correct horse battery staple", identity.RoleOperator)
	h := &SetUserGroups{Users: newFakeUsers(user), Access: newFakeAccess(), Audit: &fakeAudit{}, Clock: &fakeClock{testNow}}

	err := h.Handle(context.Background(), SetUserGroupsInput{
		Actor: adminActor(), UserID: user.ID, GroupIDs: []uuid.UUID{uuid.New()},
	})
	if !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("unknown group error = %v, want ErrNotFound", err)
	}
}

// fakeShells records which accounts had their live SSH sessions closed.
type fakeShells struct{ closed []uuid.UUID }

func (f *fakeShells) CloseUser(userID uuid.UUID, _ string) int {
	f.closed = append(f.closed, userID)
	return 1
}

func deletableUser(role identity.Role, active bool) *identity.User {
	return &identity.User{
		ID: uuid.New(), Username: "leaver", Email: "leaver@example.com",
		Role: role, IsActive: active,
	}
}

func TestDeleteUserRemovesAccountAndClosesShells(t *testing.T) {
	target := deletableUser(identity.RoleOperator, true)
	users, audit, shells := newFakeUsers(target), &fakeAudit{}, &fakeShells{}
	h := &DeleteUser{Users: users, Access: newFakeAccess(), Audit: audit, Clock: &fakeClock{testNow}, Shells: shells}

	if err := h.Handle(context.Background(), DeleteUserInput{Actor: adminActor(), UserID: target.ID}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if _, err := users.GetByID(context.Background(), target.ID); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("account still present after deletion: err = %v, want ErrNotFound", err)
	}
	if len(shells.closed) != 1 || shells.closed[0] != target.ID {
		t.Errorf("live SSH sessions closed for %v, want exactly [%v]", shells.closed, target.ID)
	}
	if !audit.has("user_deleted") {
		t.Errorf("audit actions = %v, want user_deleted", audit.actions())
	}
}

// The audit entry has to survive the account, and it only can if it carries
// what the deleted row held: the row itself is gone by the time anyone reads it.
func TestDeleteUserAuditKeepsWhatTheAccountWas(t *testing.T) {
	target := deletableUser(identity.RoleAuditor, true)
	audit := &fakeAudit{}
	h := &DeleteUser{Users: newFakeUsers(target), Access: newFakeAccess(), Audit: audit, Clock: &fakeClock{testNow}}

	if err := h.Handle(context.Background(), DeleteUserInput{Actor: adminActor(), UserID: target.ID}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	entry := audit.entries[len(audit.entries)-1]
	if entry.TargetName != target.Username || entry.TargetID != target.ID.String() {
		t.Errorf("audit target = %s/%s, want %s/%s", entry.TargetID, entry.TargetName, target.ID, target.Username)
	}
	if entry.Details["role"] != string(identity.RoleAuditor) {
		t.Errorf("audit details role = %v, want %s", entry.Details["role"], identity.RoleAuditor)
	}
	if entry.Details["email"] != target.Email {
		t.Errorf("audit details email = %v, want %s", entry.Details["email"], target.Email)
	}
}

func TestDeleteUserRefusesSelf(t *testing.T) {
	actor := adminActor()
	self := deletableUser(identity.RoleAdmin, true)
	self.ID = actor.UserID
	other := deletableUser(identity.RoleAdmin, true)
	users := newFakeUsers(self, other)
	h := &DeleteUser{Users: users, Access: newFakeAccess(), Audit: &fakeAudit{}, Clock: &fakeClock{testNow}}

	err := h.Handle(context.Background(), DeleteUserInput{Actor: actor, UserID: self.ID})
	if !errors.Is(err, identity.ErrCannotDeleteSelf) {
		t.Fatalf("Handle(self) error = %v, want ErrCannotDeleteSelf", err)
	}
	if _, err := users.GetByID(context.Background(), self.ID); err != nil {
		t.Errorf("own account was deleted anyway: %v", err)
	}
}

func TestDeleteUserRefusesTheLastAdministrator(t *testing.T) {
	last := deletableUser(identity.RoleAdmin, true)
	// A disabled admin and an active operator are both present, and neither
	// keeps the portal administrable: the check must not count them.
	disabledAdmin := deletableUser(identity.RoleAdmin, false)
	operator := deletableUser(identity.RoleOperator, true)
	users := newFakeUsers(last, disabledAdmin, operator)
	h := &DeleteUser{Users: users, Access: newFakeAccess(), Audit: &fakeAudit{}, Clock: &fakeClock{testNow}}

	err := h.Handle(context.Background(), DeleteUserInput{Actor: adminActor(), UserID: last.ID})
	if !errors.Is(err, identity.ErrLastAdmin) {
		t.Fatalf("Handle(last admin) error = %v, want ErrLastAdmin", err)
	}

	// The same account goes once a second active administrator exists, and the
	// disabled one may go at any time — it was never holding the door open.
	if err := h.Handle(context.Background(), DeleteUserInput{Actor: adminActor(), UserID: disabledAdmin.ID}); err != nil {
		t.Errorf("Handle(disabled admin) = %v, want nil", err)
	}
	second := deletableUser(identity.RoleAdmin, true)
	users.byID[second.ID] = second
	if err := h.Handle(context.Background(), DeleteUserInput{Actor: adminActor(), UserID: last.ID}); err != nil {
		t.Errorf("Handle(one of two admins) = %v, want nil", err)
	}
}

func TestDeleteUserUnknownAccount(t *testing.T) {
	h := &DeleteUser{Users: newFakeUsers(), Access: newFakeAccess(), Audit: &fakeAudit{}, Clock: &fakeClock{testNow}}

	err := h.Handle(context.Background(), DeleteUserInput{Actor: adminActor(), UserID: uuid.New()})
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Handle(unknown) error = %v, want ErrNotFound", err)
	}
}
