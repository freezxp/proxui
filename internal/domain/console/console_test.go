package console

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

var now = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

func TestTicketExpiry(t *testing.T) {
	ticket := NewTicket(uuid.New(), uuid.New(), uuid.New(), KindVNC, now)

	if ticket.Expired(now) {
		t.Error("a fresh ticket is already expired")
	}
	if ticket.Expired(now.Add(TicketTTL - time.Second)) {
		t.Error("ticket expired before its TTL elapsed")
	}
	if !ticket.Expired(now.Add(TicketTTL)) {
		t.Error("ticket outlived its TTL; a leaked ticket is a leaked console")
	}
	if TicketTTL > 5*time.Minute {
		t.Errorf("TicketTTL is %v; a console ticket only has to survive one WebSocket upgrade", TicketTTL)
	}
}

func TestTicketBindsUserAndVM(t *testing.T) {
	user, vm := uuid.New(), uuid.New()
	ticket := NewTicket(uuid.New(), user, vm, KindVNC, now)

	if ticket.UserID != user || ticket.VMID != vm {
		t.Error("ticket does not bind the user and VM it was issued for")
	}
	// Two tickets must never collide, or one user could redeem another's.
	other := NewTicket(uuid.New(), user, vm, KindVNC, now)
	if other.ID == ticket.ID {
		t.Error("two tickets share an id")
	}
}

func TestExpiryCheckEnforcesBothLimits(t *testing.T) {
	tests := []struct {
		name         string
		startedAt    time.Time
		lastActivity time.Time
		wantReason   string
		wantExpired  bool
	}{
		{"active session", now.Add(-time.Minute), now, "", false},
		{"idle just under the limit", now.Add(-time.Hour), now.Add(-IdleTimeout + time.Second), "", false},
		{"idle past the limit", now.Add(-time.Hour), now.Add(-IdleTimeout), ReasonIdle, true},
		{"busy but past the ceiling", now.Add(-MaxDuration), now, ReasonMaxDuration, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, expired := ExpiryCheck(tt.startedAt, tt.lastActivity, now)
			if expired != tt.wantExpired || reason != tt.wantReason {
				t.Errorf("ExpiryCheck() = %q, %v; want %q, %v", reason, expired, tt.wantReason, tt.wantExpired)
			}
		})
	}
}

// A session at the hard ceiling closes even if the user is actively typing:
// that is the point of a ceiling.
func TestMaxDurationBeatsActivity(t *testing.T) {
	reason, expired := ExpiryCheck(now.Add(-MaxDuration-time.Hour), now, now)
	if !expired || reason != ReasonMaxDuration {
		t.Errorf("ExpiryCheck() = %q, %v; an active session past the ceiling must still close", reason, expired)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := &Session{ID: uuid.New(), StartedAt: now}

	if !s.Active() {
		t.Error("a new session is not active")
	}
	if got := s.Duration(now.Add(5 * time.Minute)); got != 5*time.Minute {
		t.Errorf("Duration() = %v for an open session, want 5m", got)
	}

	s.EndedAt = now.Add(10 * time.Minute)
	s.CloseReason = ReasonUser
	if s.Active() {
		t.Error("a closed session still reports active")
	}
	if got := s.Duration(now.Add(time.Hour)); got != 10*time.Minute {
		t.Errorf("Duration() = %v after closing, want the recorded 10m", got)
	}
}

func TestKindValidation(t *testing.T) {
	if !KindVNC.Valid() || !KindSerial.Valid() {
		t.Error("a supported console kind was rejected")
	}
	if Kind("rdp").Valid() {
		t.Error("an unsupported console kind was accepted")
	}
}
