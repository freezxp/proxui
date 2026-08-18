package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/identity"
)

// What a deletion takes with it, and what it must leave behind, is decided by
// the schema rather than by any Go code — so it is only really tested here.
func TestDeleteUserCascadesSessionsAndSparesTheAuditTrail(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	users := NewUserRepository(pool)
	sessions := NewSessionRepository(pool)
	audit := NewAuditRepository(pool)

	user := newTestUser(t, pool, identity.RoleOperator)
	now := time.Now().UTC()

	session := identity.NewSession(user.ID, []byte("hash-"+user.ID.String()), "10.0.0.9", "test", now)
	if err := sessions.Create(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := audit.Write(ctx, ports.AuditEntry{
		Time: now, ActorUserID: &user.ID, ActorName: user.Username,
		Category: ports.AuditCategoryAuth, Action: "login_succeeded",
		Outcome: ports.OutcomeSuccess,
	}); err != nil {
		t.Fatalf("write audit: %v", err)
	}

	if err := users.Delete(ctx, user.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := users.GetByID(ctx, user.ID); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("GetByID after delete = %v, want ErrNotFound", err)
	}
	// Deleting twice is not an error worth inventing a second code for; it is
	// the same "no such account" the first caller would have seen.
	if err := users.Delete(ctx, user.ID); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}

	// The session went with the account, which is what makes an access token
	// issued a minute ago stop working at its next request.
	var live int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, user.ID).Scan(&live); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if live != 0 {
		t.Errorf("%d sessions survived the account", live)
	}

	// The audit entry did not. It keeps the id and the name it was written
	// with, because the record of what somebody did outlives their account.
	var name string
	if err := pool.QueryRow(ctx,
		`SELECT actor_name FROM audit_logs WHERE actor_user_id = $1`, user.ID).Scan(&name); err != nil {
		t.Fatalf("read audit entry after deletion: %v", err)
	}
	if name != user.Username {
		t.Errorf("audit actor_name = %q, want %q", name, user.Username)
	}
}
