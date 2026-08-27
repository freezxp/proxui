package shell_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/domain/shell"
)

func TestPickAddressPrefersOrdinaryIPv4(t *testing.T) {
	// The order here is the order a guest agent actually reports: loopback
	// first, then whatever the interfaces happen to be called.
	got, err := shell.PickAddress([]string{"127.0.0.1", "::1", "192.168.100.40", "fe80::1"})
	if err != nil {
		t.Fatalf("PickAddress: %v", err)
	}
	if got != "192.168.100.40" {
		t.Fatalf("picked %q, want the routable IPv4", got)
	}
}

func TestPickAddressSkipsContainerBridge(t *testing.T) {
	// A guest running Docker reports 172.17.0.1 before its own address on some
	// agent versions. Connecting there hangs rather than failing, which is the
	// worst kind of wrong.
	got, err := shell.PickAddress([]string{"172.17.0.1", "10.20.30.40"})
	if err != nil {
		t.Fatalf("PickAddress: %v", err)
	}
	if got != "10.20.30.40" {
		t.Fatalf("picked %q, want the guest's own address", got)
	}
}

func TestPickAddressFallsBackToBridgeRatherThanNothing(t *testing.T) {
	// If the bridge address is all there is, it is still better than refusing:
	// a static route may well make it reachable.
	got, err := shell.PickAddress([]string{"127.0.0.1", "172.17.0.5"})
	if err != nil {
		t.Fatalf("PickAddress: %v", err)
	}
	if got != "172.17.0.5" {
		t.Fatalf("picked %q", got)
	}
}

func TestPickAddressAcceptsIPv6WhenThatIsAll(t *testing.T) {
	got, err := shell.PickAddress([]string{"fe80::5054:ff:fe12:3456", "2001:db8::5"})
	if err != nil {
		t.Fatalf("PickAddress: %v", err)
	}
	if got != "2001:db8::5" {
		t.Fatalf("picked %q, want the global IPv6", got)
	}
}

func TestPickAddressStripsPrefixLength(t *testing.T) {
	got, err := shell.PickAddress([]string{"192.168.1.9/24"})
	if err != nil {
		t.Fatalf("PickAddress: %v", err)
	}
	if got != "192.168.1.9" {
		t.Fatalf("picked %q, want the address without its prefix", got)
	}
}

func TestPickAddressRefusesWhenNothingIsUsable(t *testing.T) {
	for _, addrs := range [][]string{
		nil,
		{},
		{"127.0.0.1", "::1"},
		{"169.254.3.4"},
		{"not-an-address"},
	} {
		if _, err := shell.PickAddress(addrs); !errors.Is(err, shell.ErrNoAddress) {
			t.Fatalf("PickAddress(%v) = %v, want ErrNoAddress", addrs, err)
		}
	}
}

func TestNormalizePort(t *testing.T) {
	if got, err := shell.NormalizePort(0); err != nil || got != 22 {
		t.Fatalf("NormalizePort(0) = %d, %v", got, err)
	}
	if got, err := shell.NormalizePort(2222); err != nil || got != 2222 {
		t.Fatalf("NormalizePort(2222) = %d, %v", got, err)
	}
	for _, bad := range []int{-1, 65536} {
		if _, err := shell.NormalizePort(bad); err == nil {
			t.Fatalf("NormalizePort(%d) accepted a nonsense port", bad)
		}
	}
}

func TestCleanPath(t *testing.T) {
	got, err := shell.CleanPath("/var/log/../lib/")
	if err != nil {
		t.Fatalf("CleanPath: %v", err)
	}
	if got != "/var/lib" {
		t.Fatalf("CleanPath = %q", got)
	}
	for _, bad := range []string{"", "etc/passwd", "/tmp/a\x00b"} {
		if _, err := shell.CleanPath(bad); !errors.Is(err, shell.ErrBadPath) {
			t.Fatalf("CleanPath(%q) = %v, want ErrBadPath", bad, err)
		}
	}
}

func TestJoinPathRefusesNamesThatAreNotNames(t *testing.T) {
	got, err := shell.JoinPath("/srv/app/", "config.yml")
	if err != nil {
		t.Fatalf("JoinPath: %v", err)
	}
	if got != "/srv/app/config.yml" {
		t.Fatalf("JoinPath = %q", got)
	}
	// An upload whose filename is "../../etc/cron.d/x" must not land there.
	for _, bad := range []string{"", ".", "..", "../etc/passwd", "a/b", "a\x00b"} {
		if _, err := shell.JoinPath("/srv", bad); !errors.Is(err, shell.ErrBadPath) {
			t.Fatalf("JoinPath(%q) = %v, want ErrBadPath", bad, err)
		}
	}
}

func TestTicketExpiry(t *testing.T) {
	now := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	ticket := shell.NewTicket(uuid.New(), uuid.New(), uuid.New(), now)

	if ticket.Expired(now.Add(shell.TicketTTL - time.Second)) {
		t.Fatal("ticket expired early")
	}
	if !ticket.Expired(now.Add(shell.TicketTTL)) {
		t.Fatal("ticket outlived its TTL")
	}
}

func TestExpiryCheck(t *testing.T) {
	start := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)

	if _, expired := shell.ExpiryCheck(start, start, start.Add(time.Minute), true); expired {
		t.Fatal("an active session was closed")
	}
	reason, expired := shell.ExpiryCheck(start, start, start.Add(shell.IdleTimeout), true)
	if !expired || reason != shell.ReasonIdle {
		t.Fatalf("idle check = %q, %v", reason, expired)
	}
	// The hard cap wins over idleness: a session that is both is reported as
	// the one the operator cannot avoid by typing.
	reason, expired = shell.ExpiryCheck(start, start, start.Add(shell.MaxDuration), true)
	if !expired || reason != shell.ReasonMaxDuration {
		t.Fatalf("max duration check = %q, %v", reason, expired)
	}
}

func TestExpiryCheckReclaimsAnAbandonedSession(t *testing.T) {
	start := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)

	// Detached but recently used - the file browser working on its own.
	if _, expired := shell.ExpiryCheck(start, start, start.Add(shell.DetachedGrace-time.Second), false); expired {
		t.Fatal("a detached session still being used was closed")
	}

	reason, expired := shell.ExpiryCheck(start, start, start.Add(shell.DetachedGrace), false)
	if !expired || reason != shell.ReasonAbandoned {
		t.Fatalf("detached check = %q, %v; want a session nobody can reach to be reclaimed", reason, expired)
	}

	// An attached terminal at the same moment keeps the long limit: someone is
	// sitting in front of it, however quiet they are being.
	if _, expired := shell.ExpiryCheck(start, start, start.Add(shell.DetachedGrace), true); expired {
		t.Fatal("an attached terminal was closed on the detached limit")
	}

	// Activity holds it open whether or not a terminal is attached, which is
	// what makes the file browser alone a legitimate way to use a session.
	later := start.Add(time.Hour)
	if _, expired := shell.ExpiryCheck(start, later, later.Add(time.Second), false); expired {
		t.Fatal("activity did not hold the detached limit off")
	}

	// The hard cap still wins over both.
	reason, expired = shell.ExpiryCheck(start, start, start.Add(shell.MaxDuration), false)
	if !expired || reason != shell.ReasonMaxDuration {
		t.Fatalf("max duration check = %q, %v", reason, expired)
	}
}

func TestSessionDuration(t *testing.T) {
	start := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	s := &shell.Session{StartedAt: start}

	if !s.Active() {
		t.Fatal("a session with no end time should be active")
	}
	if got := s.Duration(start.Add(90 * time.Second)); got != 90*time.Second {
		t.Fatalf("open duration = %v", got)
	}

	s.EndedAt = start.Add(time.Minute)
	if s.Active() {
		t.Fatal("a closed session should not be active")
	}
	if got := s.Duration(start.Add(time.Hour)); got != time.Minute {
		t.Fatalf("closed duration = %v, should not keep growing", got)
	}
}
