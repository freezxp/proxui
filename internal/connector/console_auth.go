package connector

import "context"

// MessageConn is the slice of a WebSocket connection a console handshake
// needs. It matches *websocket.Conn structurally, which keeps the connector
// layer free of a dependency on any particular WebSocket library.
type MessageConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
}

// ConsoleAuthenticator is implemented by console endpoints whose protocol
// demands an authentication exchange before the session proper begins.
//
// The portal performs that exchange itself, on both sides, and only then hands
// the two streams to the bridge to relay blindly. The alternative — forwarding
// the platform's console password to the browser and letting it authenticate —
// would put a platform secret somewhere docs/15-security-design.md §15.4
// deliberately keeps it out of, and would mean the browser could reach the
// node directly if it ever learned the address.
//
// An endpoint that does not implement this is relayed from its first byte, as
// before; the bridge stays protocol-neutral for every platform that needs
// nothing here.
type ConsoleAuthenticator interface {
	// AuthenticateConsole completes the handshake against the upstream and the
	// matching handshake with the browser, leaving both streams at the same
	// protocol position, ready for raw relaying.
	AuthenticateConsole(ctx context.Context, upstream, client MessageConn) error
}
