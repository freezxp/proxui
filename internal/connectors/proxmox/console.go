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
	// Password is present only when generate-password is requested.
	Password string `json:"password"`
	UPID     string `json:"upid"`
}

// guestQEMU is how Proxmox names full virtual machines in its API paths.
// Containers ("lxc") take the same paths but not the same console options.
const guestQEMU = "qemu"

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
		guestType = guestQEMU
	}

	var (
		path string
		form = url.Values{}
	)
	switch kind {
	case connector.ConsoleVNC, "":
		path = fmt.Sprintf("/nodes/%s/%s/%s/vncproxy", vm.HostID, guestType, vm.ExternalID)
		form.Set("websocket", "1")
		// The node offers only RFB security type 2, so the console handshake
		// has to be answered with a password. For a QEMU guest, asking the
		// node to generate one gets an eight-character random string scoped
		// to this session — which is what VNC auth can actually carry, since
		// the API ticket is far longer and would be silently truncated to its
		// first eight bytes.
		//
		// A container's vncproxy does not accept generate-password: it is a
		// QEMU VNC server option, and passing it to an LXC guest is rejected
		// with HTTP 400 before a console is ever started. Containers fall back
		// to the ticket, which is what the node's own web UI uses, and which
		// both ends truncate identically.
		if guestType == guestQEMU {
			form.Set("generate-password", "1")
		}
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
	// Without a generated password the ticket is the credential, exactly as
	// the platform's own interface uses it.
	password := resp.Password
	if password == "" {
		password = resp.Ticket
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
		host:     target.Host,
		password: password,
		scheme:   target.Scheme,
		path:     wsPath,
		// The websocket upgrade is an authenticated API call in its own right:
		// Proxmox answers "401 No ticket" without the token header, even though
		// the URL already carries a VNC ticket. This is also precisely why the
		// browser cannot be allowed to connect directly - doing so would mean
		// handing it the platform credential.
		authHeader: c.client.authHeader,
		tlsConfig:  c.client.http.Transport.(*http.Transport).TLSClientConfig,
		expires:    time.Now().Add(consoleTicketTTL),
	}, nil
}

// consoleEndpoint dials the upstream console websocket. It deliberately does
// not speak RFB or the websocket framing itself: the portal's bridge pipes raw
// bytes, which keeps this independent of VNC and serial specifics.
type consoleEndpoint struct {
	host       string
	password   string
	scheme     string
	path       string
	authHeader string
	tlsConfig  *tls.Config
	expires    time.Time
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

// WebsocketURL is the upstream console URL, ticket included.
func (e *consoleEndpoint) WebsocketURL() string {
	scheme := "wss"
	if e.scheme == "http" {
		scheme = "ws"
	}
	return scheme + "://" + e.host + e.path
}

// TLSClientConfig carries the platform's trust policy onto the console path:
// a pinned fingerprint must be honoured here exactly as it is for the API.
func (e *consoleEndpoint) TLSClientConfig() *tls.Config { return e.tlsConfig }

// RequestHeader carries the API token onto the websocket handshake, which
// Proxmox requires in addition to the VNC ticket in the URL.
func (e *consoleEndpoint) RequestHeader() http.Header {
	h := http.Header{}
	if e.authHeader != "" {
		h.Set("Authorization", e.authHeader)
	}
	return h
}
