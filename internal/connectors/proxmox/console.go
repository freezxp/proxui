package proxmox

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/freezxp/proxui/internal/connector"
)

// vncProxyResponse is the per-session ticket Proxmox mints for a console.
type vncProxyResponse struct {
	Ticket string `json:"ticket"`
	Port   any    `json:"port"` // string in some releases, number in others
	User   string `json:"user"`
	Cert   string `json:"cert"`
	UPID   string `json:"upid"`
}

// consoleTicketTTL is how long a Proxmox VNC ticket stays usable. The portal's
// own one-time session token is shorter still (CONS-03).
const consoleTicketTTL = 30 * time.Second

// CreateConsoleSession implements connector.ConsoleProvider.
//
// The flow is: ask the node for a single-use VNC ticket, then hand back an
// endpoint that dials the node's websocket. The browser never learns the node
// address, the ticket or the certificate — the portal proxies the bytes
// (docs/06-sequence-diagrams.md §6.2).
func (c *Connector) CreateConsoleSession(ctx context.Context, vm connector.VMRef, kind connector.ConsoleKind) (connector.ConsoleEndpoint, error) {
	if vm.HostID == "" || vm.ExternalID == "" {
		return nil, connector.Errorf(connector.ErrInvalidConfig, "console",
			"console needs both the node and the VMID")
	}
	guestType := vm.Type
	if guestType == "" {
		guestType = "qemu"
	}

	var (
		path string
		form = url.Values{}
	)
	switch kind {
	case connector.ConsoleVNC, "":
		path = fmt.Sprintf("/nodes/%s/%s/%s/vncproxy", vm.HostID, guestType, vm.ExternalID)
		form.Set("websocket", "1")
		// generate-password is deliberately not requested: the ticket is the
		// credential, and asking for a password would add a second secret to
		// carry around.
	case connector.ConsoleSerial:
		path = fmt.Sprintf("/nodes/%s/%s/%s/termproxy", vm.HostID, guestType, vm.ExternalID)
	default:
		return nil, connector.Errorf(connector.ErrNotSupported, "console", "console kind %q is not supported", kind)
	}

	var resp vncProxyResponse
	if err := c.client.post(ctx, path, form, &resp); err != nil {
		return nil, err
	}
	if resp.Ticket == "" {
		return nil, connector.Errorf(connector.ErrUnreachable, "console", "platform returned no console ticket")
	}
	port := fmt.Sprintf("%v", resp.Port)
	if port == "" || port == "<nil>" {
		return nil, connector.Errorf(connector.ErrUnreachable, "console", "platform returned no console port")
	}

	wsPath := fmt.Sprintf("%s/nodes/%s/%s/%s/vncwebsocket?port=%s&vncticket=%s",
		apiPrefix, vm.HostID, guestType, vm.ExternalID, url.QueryEscape(port), url.QueryEscape(resp.Ticket))
	if kind == connector.ConsoleSerial {
		wsPath = fmt.Sprintf("%s/nodes/%s/%s/%s/vncwebsocket?port=%s&vncticket=%s",
			apiPrefix, vm.HostID, guestType, vm.ExternalID, url.QueryEscape(port), url.QueryEscape(resp.Ticket))
	}

	target := *c.client.base
	target.Path = ""
	return &consoleEndpoint{
		host:      target.Host,
		scheme:    target.Scheme,
		path:      wsPath,
		tlsConfig: c.client.http.Transport.(*http.Transport).TLSClientConfig,
		expires:   time.Now().Add(consoleTicketTTL),
	}, nil
}

// consoleEndpoint dials the upstream console websocket. It deliberately does
// not speak RFB or the websocket framing itself: the portal's bridge pipes raw
// bytes, which keeps this independent of VNC and serial specifics.
type consoleEndpoint struct {
	host      string
	scheme    string
	path      string
	tlsConfig *tls.Config
	expires   time.Time
}

// DialContext opens a TCP/TLS connection to the node hosting the console.
func (e *consoleEndpoint) DialContext(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if e.scheme == "http" {
		conn, err := dialer.DialContext(ctx, "tcp", e.host)
		if err != nil {
			return nil, connector.Wrap(connector.ErrUnreachable, "console_dial", err)
		}
		return conn, nil
	}
	conn, err := tls.DialWithDialer(dialer, "tcp", e.host, e.tlsConfig)
	if err != nil {
		return nil, connector.Wrap(connector.ErrUnreachable, "console_dial", err)
	}
	return conn, nil
}

// ExpiresAt is when the upstream ticket stops being accepted.
func (e *consoleEndpoint) ExpiresAt() time.Time { return e.expires }

// WebsocketPath is the upstream request path the bridge must use when it
// performs the websocket handshake.
func (e *consoleEndpoint) WebsocketPath() string { return e.path }

// Host is the upstream authority for the websocket handshake.
func (e *consoleEndpoint) Host() string { return e.host }
