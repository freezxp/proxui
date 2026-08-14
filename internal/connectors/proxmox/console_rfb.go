package proxmox

import (
	"context"
	"crypto/des" //nolint:gosec // RFB security type 2 specifies DES; not our choice.
	"encoding/binary"
	"fmt"

	"github.com/freezxp/proxui/internal/connector"
)

// RFB constants (RFC 6143 §7.1).
const (
	rfbVersion     = "RFB 003.008\n"
	rfbSecNone     = 1
	rfbSecVNCAuth  = 2
	rfbAuthOK      = 0
	rfbChallengeSz = 16
	binaryMessage  = 2 // websocket.BinaryMessage, without importing the library
)

// AuthenticateConsole implements connector.ConsoleAuthenticator for VNC.
//
// Proxmox offers exactly one security type — VNC Auth — so somebody has to
// answer its challenge. The portal answers it here, using the per-session
// password the node generated, and then presents the browser a handshake
// offering "None". The browser therefore holds no platform secret, and both
// streams come out of this aligned at ClientInit, where blind relaying begins.
func (e *consoleEndpoint) AuthenticateConsole(ctx context.Context, upstream, client connector.MessageConn) error {
	if e.password == "" {
		// A console we cannot authenticate must fail loudly rather than
		// silently handing the browser a handshake that will stall.
		return connector.Errorf(connector.ErrUnreachable, "console_auth",
			"platform did not return a console password")
	}
	if err := e.authenticateUpstream(upstream); err != nil {
		return err
	}
	return greetClient(client)
}

// authenticateUpstream plays the client half of RFB against the node.
func (e *consoleEndpoint) authenticateUpstream(upstream connector.MessageConn) error {
	r := &frameReader{conn: upstream}

	version, err := r.read(len(rfbVersion))
	if err != nil {
		return connector.Wrap(connector.ErrUnreachable, "console_auth", err)
	}
	// Proxmox speaks 3.8. Anything else means we are not talking to what we
	// think we are, and guessing at an older handshake would be worse.
	if string(version) != rfbVersion {
		return connector.Errorf(connector.ErrNotSupported, "console_auth",
			"unsupported console protocol version %q", string(version))
	}
	if err := upstream.WriteMessage(binaryMessage, []byte(rfbVersion)); err != nil {
		return connector.Wrap(connector.ErrUnreachable, "console_auth", err)
	}

	count, err := r.read(1)
	if err != nil {
		return connector.Wrap(connector.ErrUnreachable, "console_auth", err)
	}
	if count[0] == 0 {
		return connector.Errorf(connector.ErrUnreachable, "console_auth",
			"platform refused the console: %s", r.failureReason())
	}
	types, err := r.read(int(count[0]))
	if err != nil {
		return connector.Wrap(connector.ErrUnreachable, "console_auth", err)
	}
	if !contains(types, rfbSecVNCAuth) {
		return connector.Errorf(connector.ErrNotSupported, "console_auth",
			"platform offers no console security type this portal can answer (offered %v)", types)
	}
	if err := upstream.WriteMessage(binaryMessage, []byte{rfbSecVNCAuth}); err != nil {
		return connector.Wrap(connector.ErrUnreachable, "console_auth", err)
	}

	challenge, err := r.read(rfbChallengeSz)
	if err != nil {
		return connector.Wrap(connector.ErrUnreachable, "console_auth", err)
	}
	response, err := vncResponse(challenge, e.password)
	if err != nil {
		return connector.Wrap(connector.ErrInvalidConfig, "console_auth", err)
	}
	if err := upstream.WriteMessage(binaryMessage, response); err != nil {
		return connector.Wrap(connector.ErrUnreachable, "console_auth", err)
	}

	result, err := r.read(4)
	if err != nil {
		return connector.Wrap(connector.ErrUnreachable, "console_auth", err)
	}
	if binary.BigEndian.Uint32(result) != rfbAuthOK {
		return connector.Errorf(connector.ErrAuth, "console_auth",
			"platform rejected the console password: %s", r.failureReason())
	}
	return nil
}

// greetClient plays the server half of RFB towards the browser, offering the
// "None" security type because the portal has already authenticated upstream.
func greetClient(client connector.MessageConn) error {
	r := &frameReader{conn: client}

	if err := client.WriteMessage(binaryMessage, []byte(rfbVersion)); err != nil {
		return err
	}
	if _, err := r.read(len(rfbVersion)); err != nil {
		return err
	}
	if err := client.WriteMessage(binaryMessage, []byte{1, rfbSecNone}); err != nil {
		return err
	}
	chosen, err := r.read(1)
	if err != nil {
		return err
	}
	if chosen[0] != rfbSecNone {
		return fmt.Errorf("console client chose security type %d, which was not offered", chosen[0])
	}
	// SecurityResult: four zero bytes for OK.
	return client.WriteMessage(binaryMessage, []byte{0, 0, 0, 0})
}

// vncResponse answers an RFB challenge (RFC 6143 §7.2.2).
//
// VNC Auth is DES-ECB with a key made from the password, and the key bytes go
// in bit-reversed — an accident of the original implementation that every
// server since has had to reproduce. Passwords are truncated to eight bytes,
// which is why the node is asked to generate one that fits.
func vncResponse(challenge []byte, password string) ([]byte, error) {
	var key [8]byte
	copy(key[:], password)
	for i, b := range key {
		key[i] = reverseBits(b)
	}

	block, err := des.NewCipher(key[:]) //nolint:gosec // required by the protocol
	if err != nil {
		return nil, err
	}
	out := make([]byte, rfbChallengeSz)
	block.Encrypt(out[:8], challenge[:8])
	block.Encrypt(out[8:], challenge[8:])
	return out, nil
}

func reverseBits(b byte) byte {
	var out byte
	for i := 0; i < 8; i++ {
		out <<= 1
		out |= (b >> i) & 1
	}
	return out
}

func contains(haystack []byte, needle byte) bool {
	for _, b := range haystack {
		if b == needle {
			return true
		}
	}
	return false
}

// frameReader turns a stream of WebSocket messages into the byte-oriented
// reads RFB is written in terms of: message boundaries carry no meaning here,
// so a protocol field may arrive split across frames or share one.
type frameReader struct {
	conn connector.MessageConn
	buf  []byte
}

func (r *frameReader) read(n int) ([]byte, error) {
	for len(r.buf) < n {
		_, data, err := r.conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		r.buf = append(r.buf, data...)
	}
	out := make([]byte, n)
	copy(out, r.buf[:n])
	r.buf = r.buf[n:]
	return out, nil
}

// failureReason reads the string RFB 3.8 appends to a failure, for an error
// message that says what the platform actually objected to.
func (r *frameReader) failureReason() string {
	length, err := r.read(4)
	if err != nil {
		return "no reason given"
	}
	reason, err := r.read(int(binary.BigEndian.Uint32(length)))
	if err != nil {
		return "no reason given"
	}
	return string(reason)
}
