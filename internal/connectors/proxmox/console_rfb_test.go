package proxmox

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/freezxp/proxui/internal/connector"
)

// fakeConn is a MessageConn fed by a scripted list of inbound frames, keeping
// everything the code under test wrote.
type fakeConn struct {
	inbound [][]byte
	written [][]byte
	closed  bool
}

func (c *fakeConn) ReadMessage() (int, []byte, error) {
	if len(c.inbound) == 0 {
		return 0, nil, errors.New("no more frames")
	}
	next := c.inbound[0]
	c.inbound = c.inbound[1:]
	return binaryMessage, next, nil
}

func (c *fakeConn) WriteMessage(_ int, data []byte) error {
	if c.closed {
		return errors.New("closed")
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	c.written = append(c.written, cp)
	return nil
}

func (c *fakeConn) flat() []byte {
	var out []byte
	for _, w := range c.written {
		out = append(out, w...)
	}
	return out
}

const testPassword = "s3cr3tpw"

// upstreamScript is a well-behaved Proxmox side: version, one security type,
// a challenge, then success.
func upstreamScript(challenge []byte) [][]byte {
	return [][]byte{
		[]byte(rfbVersion),
		{1, rfbSecVNCAuth},
		challenge,
		{0, 0, 0, 0},
	}
}

func clientScript() [][]byte {
	return [][]byte{
		[]byte(rfbVersion),
		{rfbSecNone},
	}
}

func TestAuthenticateConsoleCompletesBothHandshakes(t *testing.T) {
	challenge := make([]byte, rfbChallengeSz)
	for i := range challenge {
		challenge[i] = byte(i * 7)
	}
	upstream := &fakeConn{inbound: upstreamScript(challenge)}
	client := &fakeConn{inbound: clientScript()}

	endpoint := &consoleEndpoint{password: testPassword}
	if err := endpoint.AuthenticateConsole(context.Background(), upstream, client); err != nil {
		t.Fatalf("handshake failed: %v", err)
	}

	// Upstream should have seen our version, the chosen type, and a response
	// that matches what the password produces.
	wantResponse, err := vncResponse(challenge, testPassword)
	if err != nil {
		t.Fatal(err)
	}
	gotUp := upstream.flat()
	wantUp := append([]byte(rfbVersion), rfbSecVNCAuth)
	wantUp = append(wantUp, wantResponse...)
	if string(gotUp) != string(wantUp) {
		t.Errorf("upstream saw %v, want %v", gotUp, wantUp)
	}

	// The browser must be offered "None" and nothing else: offering VNC Auth
	// would mean asking it for a password it must never hold.
	gotClient := client.flat()
	wantClient := append([]byte(rfbVersion), 1, rfbSecNone, 0, 0, 0, 0)
	if string(gotClient) != string(wantClient) {
		t.Errorf("client saw %v, want %v", gotClient, wantClient)
	}
}

// The bytes of an RFB field carry no relationship to WebSocket framing, so the
// reader must cope with a field split across frames and with several fields
// arriving in one.
func TestAuthenticateConsoleHandlesArbitraryFraming(t *testing.T) {
	challenge := make([]byte, rfbChallengeSz)
	joined := []byte(rfbVersion)
	joined = append(joined, 1, rfbSecVNCAuth)
	joined = append(joined, challenge...)
	joined = append(joined, 0, 0, 0, 0)

	// One byte per frame: the most hostile framing a server could produce.
	var split [][]byte
	for _, b := range joined {
		split = append(split, []byte{b})
	}

	upstream := &fakeConn{inbound: split}
	client := &fakeConn{inbound: clientScript()}
	endpoint := &consoleEndpoint{password: testPassword}
	if err := endpoint.AuthenticateConsole(context.Background(), upstream, client); err != nil {
		t.Fatalf("handshake failed on split framing: %v", err)
	}
}

func TestAuthenticateConsoleRejectsBadPassword(t *testing.T) {
	failure := []byte("Authentication failure")
	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(failure)))

	upstream := &fakeConn{inbound: [][]byte{
		[]byte(rfbVersion),
		{1, rfbSecVNCAuth},
		make([]byte, rfbChallengeSz),
		{0, 0, 0, 1}, // failed
		length,
		failure,
	}}
	client := &fakeConn{inbound: clientScript()}

	endpoint := &consoleEndpoint{password: testPassword}
	err := endpoint.AuthenticateConsole(context.Background(), upstream, client)
	if !errors.Is(err, connector.ErrAuth) {
		t.Fatalf("got %v, want an auth error", err)
	}
	// The reason the platform gave belongs in the message: "authentication
	// failed" alone leaves an operator nowhere to go.
	if got := err.Error(); !contains2(got, string(failure)) {
		t.Errorf("error %q does not carry the platform's reason", got)
	}
	// Nothing may be promised to the browser once upstream auth failed.
	if len(client.written) > 0 {
		t.Errorf("client was greeted despite upstream failure: %v", client.written)
	}
}

func TestAuthenticateConsoleRefusesWithoutPassword(t *testing.T) {
	endpoint := &consoleEndpoint{}
	err := endpoint.AuthenticateConsole(context.Background(),
		&fakeConn{inbound: upstreamScript(make([]byte, rfbChallengeSz))}, &fakeConn{})
	if err == nil {
		t.Fatal("expected a missing-password error")
	}
}

func TestAuthenticateConsoleRejectsUnsupportedSecurity(t *testing.T) {
	upstream := &fakeConn{inbound: [][]byte{
		[]byte(rfbVersion),
		{1, 19}, // VeNCrypt only
	}}
	err := (&consoleEndpoint{password: testPassword}).
		AuthenticateConsole(context.Background(), upstream, &fakeConn{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("got %v, want ErrNotSupported", err)
	}
}

// This value is not a published test vector: it was recorded from this
// implementation after the same code authenticated successfully against a real
// Proxmox node, which is what actually establishes correctness. It is kept as
// a regression lock, so a later edit to the key schedule or block order fails
// here rather than silently on someone's cluster.
func TestVNCResponseIsStable(t *testing.T) {
	challenge := []byte{
		0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
		0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	}
	got, err := vncResponse(challenge, "test1234")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x6c, 0xc4, 0xe1, 0x8b, 0x1f, 0xca, 0x17, 0x78,
		0x61, 0xbb, 0xa3, 0xc6, 0x8e, 0xbc, 0x91, 0x8f,
	}
	if string(got) != string(want) {
		t.Errorf("got % x, want % x", got, want)
	}
}

// The bit-reversed key schedule is the detail most likely to be "tidied away"
// by someone who reads it as a mistake, and a plain-DES response is exactly
// what a server rejects. Pin the difference explicitly.
func TestVNCResponseUsesBitReversedKey(t *testing.T) {
	challenge := make([]byte, rfbChallengeSz)
	for i := range challenge {
		challenge[i] = byte(i)
	}
	got, err := vncResponse(challenge, "test1234")
	if err != nil {
		t.Fatal(err)
	}
	plainDES := []byte{
		0xf3, 0x3d, 0x42, 0xca, 0x91, 0xf6, 0x5f, 0x59,
		0xf0, 0x2a, 0xcf, 0x49, 0x3d, 0x6a, 0x16, 0xde,
	}
	if string(got) == string(plainDES) {
		t.Error("response was computed with an unreversed key; RFB servers reject that")
	}
}

// A password longer than eight bytes is truncated by the protocol, so a change
// that started sending nine bytes to des.NewCipher would fail outright.
func TestVNCResponseTruncatesLongPassword(t *testing.T) {
	challenge := make([]byte, rfbChallengeSz)
	long, err := vncResponse(challenge, "abcdefghIGNORED")
	if err != nil {
		t.Fatal(err)
	}
	short, err := vncResponse(challenge, "abcdefgh")
	if err != nil {
		t.Fatal(err)
	}
	if string(long) != string(short) {
		t.Error("password was not truncated to the eight bytes VNC auth carries")
	}
}

func TestReverseBits(t *testing.T) {
	for _, tc := range []struct{ in, want byte }{
		{0x00, 0x00}, {0xff, 0xff}, {0x01, 0x80}, {0x80, 0x01}, {0x0f, 0xf0},
	} {
		if got := reverseBits(tc.in); got != tc.want {
			t.Errorf("reverseBits(%#x) = %#x, want %#x", tc.in, got, tc.want)
		}
	}
}

func contains2(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
