// Package ws holds the WebSocket transports: the console bridge and (from a
// later sprint) the live event stream.
package ws

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/console"
)

// Close codes the browser can act on (docs/08-api-specification.md §8.4).
const (
	CloseIdle        = 4000
	CloseMaxDuration = 4001
	CloseAdminForced = 4002
	CloseUpstream    = 4003
	CloseUnavailable = 4004
)

// EndpointResolver produces a live upstream console for a VM.
type EndpointResolver interface {
	Resolve(ctx context.Context, vmID uuid.UUID, kind console.Kind) (connector.ConsoleEndpoint, func(), error)
}

// ConsoleBridge relays a browser WebSocket to a platform console.
//
// It never interprets the bytes it carries. RFB, serial and anything else a
// platform speaks pass through untouched, which is what keeps this one piece of
// code correct for every connector (docs/06-sequence-diagrams.md §6.2).
type ConsoleBridge struct {
	Tickets  ports.TicketStore
	Sessions ports.ConsoleRepository
	Resolver EndpointResolver
	Audit    ports.AuditWriter
	Clock    ports.Clock
	Log      zerolog.Logger

	// AllowedOrigins restricts which pages may open a console. Empty means
	// same-origin only, which is what a browser enforces by default.
	AllowedOrigins []string
}

func (b *ConsoleBridge) upgrader() *websocket.Upgrader {
	return &websocket.Upgrader{
		ReadBufferSize:  32 * 1024,
		WriteBufferSize: 32 * 1024,
		// A console is a cross-origin target worth protecting: a page on
		// another site must not be able to open one using the visitor's
		// cookies (docs/15-security-design.md §15.6).
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			if origin == "" {
				return true // non-browser client, e.g. a test or CLI
			}
			for _, allowed := range b.AllowedOrigins {
				if origin == allowed {
					return true
				}
			}
			return sameOrigin(origin, r)
		},
	}
}

func sameOrigin(origin string, r *http.Request) bool {
	host := r.Host
	return origin == "http://"+host || origin == "https://"+host
}

// ServeHTTP upgrades the request and runs the bridge for its lifetime.
func (b *ConsoleBridge) ServeHTTP(w http.ResponseWriter, r *http.Request, ticketID string) {
	ctx := r.Context()

	// Redeem before upgrading: an invalid ticket should get a plain HTTP
	// error, not a WebSocket that immediately closes.
	ticket, err := b.Tickets.Redeem(ctx, ticketID)
	if err != nil {
		if errors.Is(err, console.ErrTicketNotFound) {
			http.Error(w, "console ticket is not valid", http.StatusUnauthorized)
			return
		}
		b.Log.Error().Err(err).Msg("could not redeem console ticket")
		http.Error(w, "console unavailable", http.StatusInternalServerError)
		return
	}
	if ticket.Expired(b.Clock.Now()) {
		http.Error(w, "console ticket has expired", http.StatusUnauthorized)
		return
	}

	client, err := b.upgrader().Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote a response.
		b.Log.Debug().Err(err).Msg("console upgrade failed")
		b.finish(ctx, ticket.SessionID, console.ReasonError, 0, 0)
		return
	}
	defer client.Close()

	upstream, endpoint, release, err := b.resolve(ctx, ticket)
	if err != nil {
		b.Log.Warn().Err(err).Str("vm_id", ticket.VMID.String()).Msg("could not reach the platform console")
		closeWith(client, CloseUnavailable, "platform console unavailable")
		b.finish(ctx, ticket.SessionID, console.ReasonUpstream, 0, 0)
		return
	}
	defer release()
	defer upstream.Close()

	// Some platforms demand an authentication exchange before the session
	// proper. The connector owns that exchange because it is platform
	// specific, and it runs before relaying starts so that the browser is
	// never asked to hold a platform secret (docs/adr/0002).
	if auth, ok := endpoint.(connector.ConsoleAuthenticator); ok {
		if err := auth.AuthenticateConsole(ctx, upstream, client); err != nil {
			b.Log.Warn().Err(err).Str("vm_id", ticket.VMID.String()).
				Msg("console handshake failed")
			closeWith(client, CloseUpstream, "console handshake failed")
			b.finish(context.WithoutCancel(ctx), ticket.SessionID, console.ReasonUpstream, 0, 0)
			return
		}
	}

	now := b.Clock.Now()
	if err := b.Sessions.MarkConnected(context.WithoutCancel(ctx), ticket.SessionID, now); err != nil {
		b.Log.Error().Err(err).Msg("could not record console connection")
	}
	b.Log.Info().Str("component", "console").
		Str("session_id", ticket.SessionID.String()).
		Str("vm_id", ticket.VMID.String()).
		Str("user_id", ticket.UserID.String()).
		Msg("console connected")

	reason, tx, rx := b.relay(client, upstream, now)

	b.finish(context.WithoutCancel(ctx), ticket.SessionID, reason, tx, rx)
	b.Log.Info().Str("component", "console").
		Str("session_id", ticket.SessionID.String()).
		Str("reason", reason).Int64("bytes_tx", tx).Int64("bytes_rx", rx).
		Dur("duration_ms", b.Clock.Now().Sub(now)).Msg("console closed")
}

// resolve opens the upstream console. Proxmox exposes it as a WebSocket, so the
// bridge dials one; a connector offering a raw stream is not supported here
// because the browser side is always a WebSocket.
func (b *ConsoleBridge) resolve(ctx context.Context, ticket console.Ticket) (*websocket.Conn, connector.ConsoleEndpoint, func(), error) {
	endpoint, release, err := b.Resolver.Resolve(ctx, ticket.VMID, ticket.Kind)
	if err != nil {
		return nil, nil, nil, err
	}

	wsEndpoint, ok := endpoint.(connector.WebsocketConsole)
	if !ok {
		release()
		return nil, nil, nil, console.ErrUnsupported
	}

	dialer := &websocket.Dialer{
		TLSClientConfig:  wsEndpoint.TLSClientConfig(),
		HandshakeTimeout: 15 * time.Second,
		// Proxmox's console speaks the binary subprotocol; asking for it keeps
		// the handshake explicit rather than relying on the default.
		Subprotocols: []string{"binary"},
	}
	conn, resp, err := dialer.DialContext(ctx, wsEndpoint.WebsocketURL(), wsEndpoint.RequestHeader())
	if err != nil {
		release()
		if resp != nil {
			return nil, nil, nil, errors.New("upstream console refused the connection: " + resp.Status)
		}
		return nil, nil, nil, err
	}
	return conn, endpoint, release, nil
}

// relay pumps frames in both directions until one side closes or a limit is
// reached, and reports why it stopped along with the traffic counters.
func (b *ConsoleBridge) relay(client, upstream *websocket.Conn, startedAt time.Time) (reason string, tx, rx int64) {
	var (
		bytesToUpstream atomic.Int64
		bytesToClient   atomic.Int64
		lastActivity    atomic.Int64
		once            sync.Once
		closeReason     string
		done            = make(chan struct{})
	)
	lastActivity.Store(startedAt.UnixNano())

	stop := func(r string) {
		once.Do(func() {
			closeReason = r
			close(done)
		})
	}

	// Browser to platform.
	go func() {
		for {
			kind, data, err := client.ReadMessage()
			if err != nil {
				stop(console.ReasonUser)
				return
			}
			lastActivity.Store(b.Clock.Now().UnixNano())
			if err := upstream.WriteMessage(kind, data); err != nil {
				stop(console.ReasonUpstream)
				return
			}
			bytesToUpstream.Add(int64(len(data)))
		}
	}()

	// Platform to browser.
	go func() {
		for {
			kind, data, err := upstream.ReadMessage()
			if err != nil {
				stop(console.ReasonUpstream)
				return
			}
			lastActivity.Store(b.Clock.Now().UnixNano())
			if err := client.WriteMessage(kind, data); err != nil {
				stop(console.ReasonUser)
				return
			}
			bytesToClient.Add(int64(len(data)))
		}
	}()

	// Limits are enforced here rather than trusted to the client: a browser
	// that stops sending heartbeats must not keep a console open forever.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return closeReason, bytesToUpstream.Load(), bytesToClient.Load()
		case <-ticker.C:
			now := b.Clock.Now()
			last := time.Unix(0, lastActivity.Load())
			if r, expired := console.ExpiryCheck(startedAt, last, now); expired {
				code := CloseIdle
				if r == console.ReasonMaxDuration {
					code = CloseMaxDuration
				}
				closeWith(client, code, r)
				_ = upstream.Close()
				return r, bytesToUpstream.Load(), bytesToClient.Load()
			}
		}
	}
}

func (b *ConsoleBridge) finish(ctx context.Context, sessionID uuid.UUID, reason string, tx, rx int64) {
	now := b.Clock.Now()
	if err := b.Sessions.Close(ctx, sessionID, reason, tx, rx, now); err != nil {
		b.Log.Error().Err(err).Str("session_id", sessionID.String()).Msg("could not close console session")
	}

	session, err := b.Sessions.Get(ctx, sessionID)
	if err != nil {
		return
	}
	actorID := session.UserID
	_ = b.Audit.Write(ctx, ports.AuditEntry{
		Time: now, ActorUserID: &actorID, ActorName: "",
		Category: "console", Action: "console_session_closed",
		TargetType: "vm", TargetID: session.VMID.String(),
		SourceIP: session.ClientIP, Outcome: ports.OutcomeSuccess,
		Details: map[string]any{
			"session_id": sessionID.String(), "reason": reason,
			"duration_seconds": int(session.Duration(now).Seconds()),
			"bytes_tx":         tx, "bytes_rx": rx,
		},
	})
}

func closeWith(conn *websocket.Conn, code int, reason string) {
	msg := websocket.FormatCloseMessage(code, reason)
	_ = conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(2*time.Second))
	_ = conn.Close()
}
