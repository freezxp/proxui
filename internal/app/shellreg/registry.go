// Package shellreg holds the live SSH connections a portal process is serving.
//
// It exists because an SSH session is stateful in a way the rest of the portal
// is not. A console session is a bridge the portal can rebuild from the
// database at any moment: it holds a platform credential and can re-dial. An
// SSH session cannot be rebuilt, because the credential that opened it was
// typed by an operator and deliberately never kept (SSH-03). The open
// connection *is* the session, so it lives in memory, and everything that
// wants to use it - the terminal socket, the file browser, an administrator
// closing it - comes through here.
//
// Two consequences worth stating plainly:
//
//   - A session belongs to the process that opened it. With the roles split
//     across machines, the terminal socket and the file requests must reach the
//     same api instance. Single-binary deployments (the documented default, see
//     docs/14-deployment.md) are unaffected; a load-balanced pair needs session
//     affinity, which is recorded in docs/29 rather than silently assumed.
//   - A restart ends every session. That is correct rather than unfortunate:
//     the alternative is persisting a credential.
package shellreg

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/shell"
)

// ErrTooMany means the caller already holds as many sessions as they may.
var ErrTooMany = errors.New("shellreg: too many open sessions")

// Limits bound how much a single portal process will hold open. An SSH
// connection is cheap but not free, and a user who opens terminals in tabs and
// never closes them should hit a wall long before the process does.
const (
	MaxPerUser = 8
	MaxTotal   = 64
)

// Live is one open, authenticated SSH connection and the bookkeeping around it.
type Live struct {
	SessionID uuid.UUID
	UserID    uuid.UUID
	VMID      uuid.UUID
	VMName    string
	SSHUser   string
	Address   string
	StartedAt time.Time

	conn ports.ShellConn

	// files is the SFTP subsystem, opened on first use. The file browser is
	// optional, and negotiating a subsystem nobody asked for would fail on
	// guests that do not run one.
	filesOnce sync.Once
	files     ports.RemoteFS
	filesErr  error

	lastActivity atomic.Int64 // UnixNano
	bytesTx      atomic.Int64
	bytesRx      atomic.Int64
	attached     atomic.Bool

	closeOnce sync.Once
	closed    atomic.Bool
}

// NewLive wraps an open connection.
func NewLive(sessionID, userID, vmID uuid.UUID, vmName, sshUser, address string,
	conn ports.ShellConn, now time.Time) *Live {
	l := &Live{
		SessionID: sessionID, UserID: userID, VMID: vmID, VMName: vmName,
		SSHUser: sshUser, Address: address, StartedAt: now, conn: conn,
	}
	l.lastActivity.Store(now.UnixNano())
	return l
}

// Conn exposes the underlying connection to the terminal bridge.
func (l *Live) Conn() ports.ShellConn { return l.conn }

// Files returns the SFTP subsystem, opening it on first use.
func (l *Live) Files() (ports.RemoteFS, error) {
	l.filesOnce.Do(func() { l.files, l.filesErr = l.conn.Files() })
	return l.files, l.filesErr
}

// Touch records activity, which is what holds the idle timer off. Both the
// terminal and the file browser call it: a session someone is uploading to is
// not idle, however long since the last keystroke.
func (l *Live) Touch(now time.Time) { l.lastActivity.Store(now.UnixNano()) }

// LastActivity reports when this session was last used.
func (l *Live) LastActivity() time.Time { return time.Unix(0, l.lastActivity.Load()) }

// CountTraffic accumulates the byte counters the session record reports.
func (l *Live) CountTraffic(tx, rx int64) {
	l.bytesTx.Add(tx)
	l.bytesRx.Add(rx)
}

// Traffic reports the counters so far.
func (l *Live) Traffic() (tx, rx int64) { return l.bytesTx.Load(), l.bytesRx.Load() }

// Attach claims the session for a terminal socket, reporting false if one
// already holds it. One terminal per session: a second socket on the same
// connection would interleave keystrokes from two tabs into one shell, which
// looks like a haunting rather than a feature.
func (l *Live) Attach() bool { return l.attached.CompareAndSwap(false, true) }

// Detach releases the terminal claim, so a reconnect can take it.
func (l *Live) Detach() { l.attached.Store(false) }

// Attached reports whether a terminal socket currently holds this session,
// which is what tells the sweep apart a session someone is sitting in front of
// from one whose browser tab has gone.
func (l *Live) Attached() bool { return l.attached.Load() }

// Closed reports whether the connection has been torn down.
func (l *Live) Closed() bool { return l.closed.Load() }

// Close tears the connection down exactly once.
func (l *Live) Close() {
	l.closeOnce.Do(func() {
		l.closed.Store(true)
		_ = l.conn.Close()
	})
}

// Registry holds the live sessions of one process.
type Registry struct {
	mu       sync.RWMutex
	sessions map[uuid.UUID]*Live

	// OnEvict is called after the registry closes a session on its own -
	// because it went idle or hit the ceiling - so the caller can write the
	// session record and the audit entry. It is not called for sessions closed
	// through Remove, where the caller is already doing that itself.
	OnEvict func(l *Live, reason string)

	clock func() time.Time
}

// NewRegistry builds an empty registry.
func NewRegistry(clock func() time.Time) *Registry {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Registry{sessions: map[uuid.UUID]*Live{}, clock: clock}
}

// Add registers a live session, enforcing the per-user and total ceilings.
func (r *Registry) Add(l *Live) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.sessions) >= MaxTotal {
		return ErrTooMany
	}
	mine := 0
	for _, existing := range r.sessions {
		if existing.UserID == l.UserID {
			mine++
		}
	}
	if mine >= MaxPerUser {
		return ErrTooMany
	}
	r.sessions[l.SessionID] = l
	return nil
}

// Get looks a session up and checks it belongs to the caller.
//
// Ownership is enforced here rather than at each call site, because "the file
// browser forgot to check" is exactly the mistake that turns one operator's
// session into everyone's (SSH-10). An empty userID means an administrator
// acting on someone else's session, which is the only case that skips the
// check and is audited where it is used.
func (r *Registry) Get(sessionID, userID uuid.UUID) (*Live, error) {
	r.mu.RLock()
	l, ok := r.sessions[sessionID]
	r.mu.RUnlock()

	if !ok || l.Closed() {
		return nil, shell.ErrSessionNotFound
	}
	if userID != uuid.Nil && l.UserID != userID {
		// Deliberately the same error as "no such session": the difference
		// would confirm that someone else's session exists.
		return nil, shell.ErrSessionNotFound
	}
	return l, nil
}

// Remove takes a session out of the registry and closes it, returning it so
// the caller can record the closure. A second call is a no-op.
func (r *Registry) Remove(sessionID uuid.UUID) (*Live, bool) {
	r.mu.Lock()
	l, ok := r.sessions[sessionID]
	delete(r.sessions, sessionID)
	r.mu.Unlock()

	if !ok {
		return nil, false
	}
	l.Close()
	return l, true
}

// Len reports how many sessions are open, for the readiness surface and tests.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sessions)
}

// Sweep closes sessions that have gone idle, been abandoned, or hit the hard
// ceiling, calling OnEvict for each. It is the server-side enforcement of
// SSH-07: a browser tab that was closed without a goodbye must not leave a root
// shell open.
//
// The browser is asked to say goodbye on its way out, and usually manages it.
// This is the backstop for when it does not - a killed tab, a lost network, a
// browser that dropped the unload request - which is why the limit it enforces
// has to be short enough to matter.
func (r *Registry) Sweep() {
	now := r.clock()

	r.mu.Lock()
	var expired []struct {
		live   *Live
		reason string
	}
	for id, l := range r.sessions {
		reason, done := shell.ExpiryCheck(l.StartedAt, l.LastActivity(), now, l.Attached())
		if !done {
			continue
		}
		delete(r.sessions, id)
		expired = append(expired, struct {
			live   *Live
			reason string
		}{l, reason})
	}
	r.mu.Unlock()

	// Closing and reporting happen outside the lock: OnEvict writes to the
	// database, and holding a registry lock across that would stall every
	// keystroke in every other session.
	for _, e := range expired {
		e.live.Close()
		if r.OnEvict != nil {
			r.OnEvict(e.live, e.reason)
		}
	}
}

// Run sweeps on a ticker until ctx is done, then closes everything left.
//
// The final pass matters: a shutdown that dropped the connections without
// writing their end times would leave sessions that look open forever in the
// administrator's list.
func (r *Registry) Run(done <-chan struct{}, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.Sweep()
		case <-done:
			r.CloseAll(shell.ReasonUpstream)
			return
		}
	}
}

// CloseUser ends every session belonging to one account and reports each
// through OnEvict, returning how many it closed. Revoking a session only stops
// the next request; an SSH connection that is already open answers no
// requests, so an account that has just been deleted would otherwise keep its
// shell until the idle sweep noticed. It closes here instead.
func (r *Registry) CloseUser(userID uuid.UUID, reason string) int {
	r.mu.Lock()
	var theirs []*Live
	for id, l := range r.sessions {
		if l.UserID == userID {
			theirs = append(theirs, l)
			delete(r.sessions, id)
		}
	}
	r.mu.Unlock()

	for _, l := range theirs {
		l.Close()
		if r.OnEvict != nil {
			r.OnEvict(l, reason)
		}
	}
	return len(theirs)
}

// CloseAll ends every session, reporting each through OnEvict.
func (r *Registry) CloseAll(reason string) {
	r.mu.Lock()
	all := make([]*Live, 0, len(r.sessions))
	for id, l := range r.sessions {
		all = append(all, l)
		delete(r.sessions, id)
	}
	r.mu.Unlock()

	for _, l := range all {
		l.Close()
		if r.OnEvict != nil {
			r.OnEvict(l, reason)
		}
	}
}
