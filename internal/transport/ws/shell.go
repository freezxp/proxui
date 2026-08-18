package ws

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/app/shellreg"
	"github.com/freezxp/proxui/internal/domain/shell"
)

// CloseSessionGone tells the browser the session it holds a ticket for is no
// longer there - closed by an administrator, swept as idle, or lost with the
// process. It is distinct from the console's codes because the browser's
// response differs: reconnecting needs a new password, not a retry.
const CloseSessionGone = 4005

// ShellBridge relays a browser WebSocket to an open SSH session's terminal.
//
// It carries raw terminal bytes as binary frames and control messages as text
// frames. That split is the whole protocol: a terminal stream is bytes, not
// text - a half-received UTF-8 sequence or a stray 0x1b is data, and wrapping
// every keystroke in JSON would cost an allocation per character for nothing.
// Text frames therefore never collide with terminal traffic and can carry the
// one thing the stream cannot express, which is the window changing size.
type ShellBridge struct {
	Tickets  ports.ShellTicketStore
	Sessions ports.ShellRepository
	Registry *shellreg.Registry
	Closer   *command.CloseShell
	Clock    ports.Clock
	Log      zerolog.Logger

	// AllowedOrigins restricts which pages may open a terminal. Empty means
	// same-origin only, which is what a browser enforces by default.
	AllowedOrigins []string
}

type shellControl struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func (b *ShellBridge) upgrader() *websocket.Upgrader {
	return &websocket.Upgrader{
		ReadBufferSize:  32 * 1024,
		WriteBufferSize: 32 * 1024,
		CheckOrigin:     originAllowed(b.AllowedOrigins),
	}
}

// ServeHTTP upgrades the request and runs the terminal for its lifetime.
func (b *ShellBridge) ServeHTTP(w http.ResponseWriter, r *http.Request, ticketID string) {
	ctx := r.Context()

	// Redeem before upgrading: an invalid ticket should get a plain HTTP
	// error, not a WebSocket that immediately closes.
	ticket, err := b.Tickets.Redeem(ctx, ticketID)
	if err != nil {
		if errors.Is(err, shell.ErrTicketNotFound) {
			http.Error(w, "terminal ticket is not valid", http.StatusUnauthorized)
			return
		}
		b.Log.Error().Err(err).Msg("could not redeem terminal ticket")
		http.Error(w, "terminal unavailable", http.StatusInternalServerError)
		return
	}
	if ticket.Expired(b.Clock.Now()) {
		http.Error(w, "terminal ticket has expired", http.StatusUnauthorized)
		return
	}

	live, err := b.Registry.Get(ticket.SessionID, ticket.UserID)
	if err != nil {
		// The connection was closed between opening it and attaching - most
		// likely an administrator, or a sweep of a tab left open too long.
		http.Error(w, "that session is no longer open", http.StatusGone)
		return
	}
	// One terminal per session. A second tab attaching to the same connection
	// would interleave two people's keystrokes into one shell.
	if !live.Attach() {
		http.Error(w, "that session already has a terminal attached", http.StatusConflict)
		return
	}
	defer live.Detach()

	client, err := b.upgrader().Upgrade(w, r, nil)
	if err != nil {
		b.Log.Debug().Err(err).Msg("terminal upgrade failed")
		return
	}
	defer client.Close()

	term, err := live.Conn().Terminal(ports.TerminalSize{
		Cols: intParam(r, "cols", 80),
		Rows: intParam(r, "rows", 24),
	})
	if err != nil {
		b.Log.Warn().Err(err).Str("session_id", live.SessionID.String()).
			Msg("could not open a shell on the guest")
		closeWith(client, CloseUpstream, "could not open a shell")
		return
	}
	defer term.Close()

	now := b.Clock.Now()
	if err := b.Sessions.MarkConnected(context.WithoutCancel(ctx), live.SessionID, now); err != nil {
		b.Log.Error().Err(err).Msg("could not record terminal connection")
	}
	b.Log.Info().Str("component", "ssh").
		Str("session_id", live.SessionID.String()).
		Str("vm_id", live.VMID.String()).
		Str("user_id", live.UserID.String()).
		Str("ssh_user", live.SSHUser).
		Msg("terminal attached")

	reason, tx, rx := b.relay(client, term, live)
	live.CountTraffic(tx, rx)

	b.Log.Info().Str("component", "ssh").
		Str("session_id", live.SessionID.String()).Str("reason", reason).
		Int64("bytes_tx", tx).Int64("bytes_rx", rx).Msg("terminal detached")

	// The shell exiting ends the session: the operator typed `exit`, and
	// leaving an authenticated connection open with no way to reach it would
	// hold a guest login for nothing. A browser that merely closed the socket
	// leaves the session alive, so a reconnect does not need the password
	// again until the idle sweep takes it.
	if reason == shell.ReasonUpstream {
		if _, ok := b.Registry.Remove(live.SessionID); ok && b.Closer != nil {
			b.Closer.Record(context.WithoutCancel(ctx), live, shell.ReasonUser)
		}
	}
}

// relay pumps bytes both ways until one side stops, and reports why.
func (b *ShellBridge) relay(client *websocket.Conn, term ports.Terminal, live *shellreg.Live) (reason string, tx, rx int64) {
	var (
		toGuest   atomic.Int64
		toBrowser atomic.Int64
		once      sync.Once
		stopped   string
		done      = make(chan struct{})
	)
	stop := func(r string) {
		once.Do(func() {
			stopped = r
			close(done)
		})
	}

	// Browser to guest. Binary frames are keystrokes; text frames are control.
	go func() {
		for {
			kind, data, err := client.ReadMessage()
			if err != nil {
				stop(shell.ReasonUser)
				return
			}
			live.Touch(b.Clock.Now())

			if kind == websocket.TextMessage {
				var msg shellControl
				if err := json.Unmarshal(data, &msg); err == nil && msg.Type == "resize" {
					if err := term.Resize(msg.Cols, msg.Rows); err != nil {
						b.Log.Debug().Err(err).Msg("terminal resize refused")
					}
				}
				continue
			}
			if _, err := term.Write(data); err != nil {
				stop(shell.ReasonUpstream)
				return
			}
			toGuest.Add(int64(len(data)))
		}
	}()

	// Guest to browser. Reads are chunked rather than line-buffered: a shell
	// prompt has no newline, and waiting for one would leave the operator
	// looking at a blank terminal.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := term.Read(buf)
			if n > 0 {
				live.Touch(b.Clock.Now())
				if err := client.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
					stop(shell.ReasonUser)
					return
				}
				toBrowser.Add(int64(n))
			}
			if err != nil {
				// io.EOF here is the shell exiting, which is a normal end.
				if !errors.Is(err, io.EOF) {
					b.Log.Debug().Err(err).Msg("terminal read ended")
				}
				stop(shell.ReasonUpstream)
				return
			}
		}
	}()

	// The idle and duration limits belong to the registry's sweep, which owns
	// sessions with no terminal attached as well. This loop only has to notice
	// that the sweep closed the connection underneath it and tell the browser
	// something specific enough to act on.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			if stopped == shell.ReasonUpstream && live.Closed() {
				closeWith(client, CloseSessionGone, "session closed")
			}
			return stopped, toGuest.Load(), toBrowser.Load()
		case <-ticker.C:
			if live.Closed() {
				closeWith(client, CloseSessionGone, "session closed")
				_ = term.Close()
				return shell.ReasonUpstream, toGuest.Load(), toBrowser.Load()
			}
		}
	}
}

func intParam(r *http.Request, name string, fallback int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || v <= 0 || v > 1000 {
		return fallback
	}
	return v
}
