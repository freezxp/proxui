package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// Deleting an account is the one admin action with no undo, so the refusals
// matter as much as the deletion: an administrator who deletes themselves, or
// the last administrator, locks the portal's own door from the outside.

// memUsers is the account table, in memory, for the handler tests.
type memUsers struct{ byID map[uuid.UUID]*identity.User }

func newMemUsers(users ...*identity.User) *memUsers {
	m := &memUsers{byID: map[uuid.UUID]*identity.User{}}
	for _, u := range users {
		m.byID[u.ID] = u
	}
	return m
}

func (m *memUsers) Create(context.Context, *identity.User) error { return nil }
func (m *memUsers) GetByID(_ context.Context, id uuid.UUID) (*identity.User, error) {
	if u, ok := m.byID[id]; ok {
		return u, nil
	}
	return nil, ports.ErrNotFound
}
func (m *memUsers) GetByUsername(context.Context, string) (*identity.User, error) {
	return nil, ports.ErrNotFound
}
func (m *memUsers) GetByEmail(context.Context, string) (*identity.User, error) {
	return nil, ports.ErrNotFound
}
func (m *memUsers) GetByExternalID(context.Context, identity.AuthProvider, string) (*identity.User, error) {
	return nil, ports.ErrNotFound
}
func (m *memUsers) Update(context.Context, *identity.User) error { return nil }
func (m *memUsers) Delete(_ context.Context, id uuid.UUID) error {
	if _, ok := m.byID[id]; !ok {
		return ports.ErrNotFound
	}
	delete(m.byID, id)
	return nil
}
func (m *memUsers) CountAll(context.Context) (int, error) { return len(m.byID), nil }
func (m *memUsers) List(_ context.Context, f ports.UserFilter) ([]*identity.User, error) {
	var out []*identity.User
	for _, u := range m.byID {
		if f.Role != "" && string(u.Role) != f.Role {
			continue
		}
		if f.Active != nil && u.IsActive != *f.Active {
			continue
		}
		out = append(out, u)
	}
	return out, nil
}

func admin(id uuid.UUID, name string, active bool) *identity.User {
	return &identity.User{ID: id, Username: name, Email: name + "@example.test",
		Role: identity.RoleAdmin, IsActive: active}
}

func deleteUser(t *testing.T, srv *Server, id uuid.UUID) (int, Problem) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+id.String(), nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	var p Problem
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return rec.Code, p
}

func TestDeleteUserEndpoint(t *testing.T) {
	actorID := uuid.New()
	other := admin(uuid.New(), "second-admin", true)
	leaver := &identity.User{ID: uuid.New(), Username: "leaver", Role: identity.RoleReadOnly, IsActive: true}
	repo := newMemUsers(admin(actorID, "me", true), other, leaver)
	srv := matrixServerAs(identity.RoleAdmin, actorID, repo)

	t.Run("deletes another account", func(t *testing.T) {
		if code, p := deleteUser(t, srv, leaver.ID); code != http.StatusNoContent {
			t.Fatalf("DELETE = %d/%q, want 204", code, p.Code)
		}
		if _, err := repo.GetByID(context.Background(), leaver.ID); err == nil {
			t.Errorf("the account is still there after a 204")
		}
	})

	t.Run("refuses the caller's own account", func(t *testing.T) {
		code, p := deleteUser(t, srv, actorID)
		if code != http.StatusConflict || p.Code != "user.self_delete" {
			t.Fatalf("DELETE self = %d/%q, want 409/user.self_delete", code, p.Code)
		}
		if _, err := repo.GetByID(context.Background(), actorID); err != nil {
			t.Errorf("the caller's own account was deleted anyway: %v", err)
		}
	})

	t.Run("refuses the last administrator", func(t *testing.T) {
		// The caller is the other administrator here, so deleting them is what
		// would empty the role.
		solo := matrixServerAs(identity.RoleAdmin, other.ID, newMemUsers(other))
		if code, p := deleteUser(t, solo, other.ID); code != http.StatusConflict {
			t.Fatalf("DELETE = %d/%q, want 409", code, p.Code)
		}

		// And with a second pair of hands present, the same account goes.
		spare := admin(uuid.New(), "spare", true)
		pair := matrixServerAs(identity.RoleAdmin, uuid.New(), newMemUsers(other, spare))
		if code, p := deleteUser(t, pair, other.ID); code != http.StatusNoContent {
			t.Fatalf("DELETE with a spare admin = %d/%q, want 204", code, p.Code)
		}
	})

	t.Run("unknown account is a 404", func(t *testing.T) {
		if code, _ := deleteUser(t, srv, uuid.New()); code != http.StatusNotFound {
			t.Fatalf("DELETE unknown = %d, want 404", code)
		}
	})
}
