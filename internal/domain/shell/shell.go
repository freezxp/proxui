// Package shell is the SSH bounded context: the rules governing a browser
// terminal and file transfer session on a guest, over SSH.
//
// It is deliberately separate from the console context next door. A console is
// keyboard-and-screen access to a machine through its hypervisor, and the
// portal holds the credential for it. An SSH session is access to an operating
// system through its own network stack, using a credential the operator types
// and the portal never keeps (SSH-03). The two share a shape - a one-time
// ticket, a bridged WebSocket, an audited session record - and almost nothing
// else, so the rules live apart rather than growing a `kind` column full of
// exceptions.
package shell

import (
	"errors"
	"net"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Lifetimes (SSH-05, SSH-07). These match the console's on purpose: an idle
// root shell is the same risk whichever door it was opened through, and an
// operator should not have to learn two sets of rules.
const (
	// TicketTTL is how long an unused terminal ticket stays valid. It has only
	// to survive the browser opening a WebSocket.
	TicketTTL = 60 * time.Second

	// IdleTimeout closes a terminal nobody is typing at.
	IdleTimeout = 30 * time.Minute

	// DetachedGrace closes a session no terminal is attached to and nothing is
	// using. It is far shorter than IdleTimeout because the two describe
	// different situations: an attached terminal has someone in front of it who
	// may yet come back to it, while a detached session usually means the tab
	// is gone - and the id of a session lives only in that tab, so nobody can
	// ever reach it again. Waiting the full half hour to reclaim something
	// provably unreachable holds a guest login open for nothing and spends one
	// of the operator's eight slots while it does.
	//
	// A detached session is still a legitimate state - the file browser alone
	// is a reasonable way to use one - which is why this is measured from the
	// last activity rather than from the moment of detaching. Anything using
	// the session, terminal or not, holds it open.
	DetachedGrace = 2 * time.Minute

	// MaxDuration is the hard ceiling on any session, however active.
	MaxDuration = 8 * time.Hour

	// DefaultPort is the SSH port assumed when the caller names none.
	DefaultPort = 22
)

// Close reasons recorded in the audit trail.
const (
	ReasonUser        = "user"
	ReasonIdle        = "idle_timeout"
	ReasonAbandoned   = "abandoned"
	ReasonMaxDuration = "max_duration"
	ReasonAdminForced = "admin_forced"
	ReasonUpstream    = "upstream_lost"
	ReasonError       = "error"
)

// Domain errors.
var (
	// ErrTicketNotFound covers an unknown, expired or already-used ticket. The
	// three are not distinguished: an attacker probing ticket ids learns
	// nothing from the difference.
	ErrTicketNotFound = errors.New("shell: ticket is not valid")
	// ErrNotPermitted means the caller may not open a shell on this VM.
	ErrNotPermitted = errors.New("shell: not permitted")
	// ErrNotRunning means the guest is not running, so nothing is listening.
	ErrNotRunning = errors.New("shell: the VM is not running")
	// ErrNoAddress means the portal knows no address to connect to. A guest
	// without the agent reports no IP, and guessing one is worse than saying so.
	ErrNoAddress = errors.New("shell: no usable address is known for this VM")
	// ErrSessionNotFound covers a session that has ended, was never opened, or
	// belongs to someone else - again not distinguished, for the same reason.
	ErrSessionNotFound = errors.New("shell: no such session")
	// ErrBadPath rejects a path that is not absolute or is otherwise unusable.
	ErrBadPath = errors.New("shell: path must be absolute")
	// ErrCredentialMissing means neither a password nor a key was supplied.
	ErrCredentialMissing = errors.New("shell: a password or a private key is required")
	// ErrAuthFailed means the guest rejected the credential. It is reported as
	// itself rather than as a generic failure, because "wrong password" and
	// "the machine is unreachable" send an operator down entirely different
	// paths.
	ErrAuthFailed = errors.New("shell: the guest rejected these credentials")
	// ErrUnreachable means nothing answered on the SSH port.
	ErrUnreachable = errors.New("shell: the guest could not be reached")
	// ErrBadKey means the supplied private key could not be parsed, usually
	// because it needs a passphrase or was pasted incompletely.
	ErrBadKey = errors.New("shell: the private key could not be read")
	// ErrFilesUnavailable means the guest has no SFTP subsystem, so the file
	// browser cannot run even though the terminal works.
	ErrFilesUnavailable = errors.New("shell: this guest does not offer SFTP")
)

// Host key trust (SSH-04).
var (
	// ErrHostKeyUnknown means this VM has no pinned key yet. The caller is
	// expected to show the fingerprint and connect again with an explicit
	// acceptance, which is the only moment a fingerprint can be checked by a
	// human who knows what it should be.
	ErrHostKeyUnknown = errors.New("shell: the host key for this VM is not yet trusted")
	// ErrHostKeyMismatch means the guest presented a different key than the one
	// pinned. This is refused outright rather than offered as a prompt: the
	// benign explanation (the guest was rebuilt) and the alarming one (someone
	// is between you and it) look identical from here, and only one of them is
	// survivable by clicking through.
	ErrHostKeyMismatch = errors.New("shell: the host key for this VM has changed")
)

// HostKey is a guest's SSH public key as the portal pinned it.
type HostKey struct {
	VMID        uuid.UUID
	Address     string // host:port the key was seen at
	Algorithm   string // e.g. ssh-ed25519
	Fingerprint string // SHA256:… as OpenSSH prints it
	PublicKey   []byte // the wire-format key, so a comparison is exact
	FirstSeenAt time.Time
	TrustedBy   uuid.UUID
}

// Ticket is the one-time handle a browser exchanges for a live terminal.
//
// Unlike the console's, it carries no upstream address and no credential: by
// the time it exists the SSH connection is already open and authenticated, and
// the ticket only says which one this browser may attach to.
type Ticket struct {
	ID        uuid.UUID
	SessionID uuid.UUID
	UserID    uuid.UUID
	VMID      uuid.UUID
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// NewTicket mints a ticket for a session.
func NewTicket(sessionID, userID, vmID uuid.UUID, now time.Time) Ticket {
	return Ticket{
		ID:        uuid.New(),
		SessionID: sessionID,
		UserID:    userID,
		VMID:      vmID,
		IssuedAt:  now,
		ExpiresAt: now.Add(TicketTTL),
	}
}

// Expired reports whether the ticket may still be exchanged.
func (t Ticket) Expired(now time.Time) bool { return !now.Before(t.ExpiresAt) }

// Session is a recorded SSH connection.
//
// SSHUser is recorded because it is the only part of the credential that is
// not a secret, and it is the part an audit needs: "who logged into that box
// as root" is the question the trail has to answer. The password or key that
// went with it is never here (SSH-03).
type Session struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	VMID        uuid.UUID
	SSHUser     string
	Address     string
	ClientIP    string
	UserAgent   string
	StartedAt   time.Time
	ConnectedAt time.Time
	EndedAt     time.Time
	CloseReason string
	BytesTx     int64
	BytesRx     int64
}

// Active reports whether the session is still open.
func (s *Session) Active() bool { return s.EndedAt.IsZero() }

// Duration reports how long the session lasted, or has lasted so far.
func (s *Session) Duration(now time.Time) time.Duration {
	if s.EndedAt.IsZero() {
		return now.Sub(s.StartedAt)
	}
	return s.EndedAt.Sub(s.StartedAt)
}

// ExpiryCheck reports whether an open session must be closed now, and why.
//
// Enforced on the server rather than trusted to the browser: a client that
// stops sending heartbeats must not thereby keep a shell open.
//
// attached says whether a terminal socket currently holds the session, which
// decides which idle limit applies: a browser that went away without saying so
// is reclaimed on the short one (SSH-07).
func ExpiryCheck(startedAt, lastActivity, now time.Time, attached bool) (reason string, expired bool) {
	idle := now.Sub(lastActivity)
	switch {
	case now.Sub(startedAt) >= MaxDuration:
		return ReasonMaxDuration, true
	case !attached && idle >= DetachedGrace:
		return ReasonAbandoned, true
	case idle >= IdleTimeout:
		return ReasonIdle, true
	default:
		return "", false
	}
}

// PickAddress chooses which of a VM's synced addresses to connect to.
//
// The guest agent reports every address on every interface, in no useful
// order, and the list routinely contains addresses that will never accept a
// connection from here: loopback, link-local, and the host side of whatever
// container bridge the guest happens to run. Picking the first entry is how a
// terminal ends up trying to SSH to 172.17.0.1 and timing out with no
// explanation, so the choice is made deliberately and is testable on its own.
//
// The ranking, best first:
//
//  1. an ordinary IPv4 address - the overwhelmingly common answer
//  2. a container-bridge IPv4 (172.17/16, Docker's default) - plausible but
//     usually the guest's own bridge rather than a way in, so it goes last
//     among IPv4
//  3. a routable IPv6 address, for an estate that has moved on
//
// Loopback, link-local and unspecified addresses are never candidates.
func PickAddress(addresses []string) (string, error) {
	var v4, bridge, v6 []string
	for _, raw := range addresses {
		// The agent sometimes reports a prefix length; the address is what
		// matters here.
		text := strings.TrimSpace(raw)
		if i := strings.IndexByte(text, '/'); i >= 0 {
			text = text[:i]
		}
		ip := net.ParseIP(text)
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			continue
		}
		switch {
		case ip.To4() != nil && dockerBridge.Contains(ip):
			bridge = append(bridge, text)
		case ip.To4() != nil:
			v4 = append(v4, text)
		default:
			v6 = append(v6, text)
		}
	}
	for _, group := range [][]string{v4, v6, bridge} {
		if len(group) > 0 {
			return group[0], nil
		}
	}
	return "", ErrNoAddress
}

// dockerBridge is Docker's default bridge subnet. A guest running containers
// reports it, and it is the address of the guest's own bridge rather than a
// route to the guest.
var dockerBridge = &net.IPNet{IP: net.IPv4(172, 17, 0, 0), Mask: net.CIDRMask(16, 32)}

// NormalizePort applies the default when none was asked for, and rejects
// nonsense before it reaches a dialer.
func NormalizePort(port int) (int, error) {
	if port == 0 {
		return DefaultPort, nil
	}
	if port < 1 || port > 65535 {
		return 0, errors.New("shell: port must be between 1 and 65535")
	}
	return port, nil
}

// CleanPath normalizes a remote path for the file browser.
//
// Everything the panel does is expressed as an absolute path on the guest, and
// the guest's own permissions decide what may be read - the portal is not
// trying to jail an operator inside a directory they could reach from the
// terminal anyway. What it does refuse is a path that is not absolute (which
// SFTP would resolve against a working directory the browser cannot see) and
// one containing a NUL byte (which is how a filename smuggles a second
// filename past a C library).
func CleanPath(p string) (string, error) {
	if p == "" || !strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return "", ErrBadPath
	}
	return path.Clean(p), nil
}

// JoinPath resolves name inside dir, refusing anything that is not a plain
// entry name. Upload and mkdir take a directory from the browser and a name
// from a file picker; a name containing a separator would silently write
// somewhere the operator did not choose.
func JoinPath(dir, name string) (string, error) {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, "/\x00") {
		return "", ErrBadPath
	}
	base, err := CleanPath(dir)
	if err != nil {
		return "", err
	}
	return path.Join(base, name), nil
}

// FileEntry is one row of a remote directory listing.
type FileEntry struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Mode     string    `json:"mode"`
	ModeBits uint32    `json:"mode_bits"`
	IsDir    bool      `json:"is_dir"`
	IsLink   bool      `json:"is_link"`
	Target   string    `json:"target,omitempty"`
	Owner    string    `json:"owner,omitempty"`
	Group    string    `json:"group,omitempty"`
	ModTime  time.Time `json:"mod_time"`
}
