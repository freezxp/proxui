// Package sshtest runs a real SSH server in the test process.
//
// It exists because the interesting failures in an SSH client are protocol
// failures - a PTY request the server declines, a subsystem that is not
// configured, a host key that changes between connections - and none of them
// can be reproduced against a mock. Everything here speaks the actual protocol
// through golang.org/x/crypto/ssh and github.com/pkg/sftp, so a test that
// passes has exercised a handshake, a channel, a PTY and an SFTP session.
package sshtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Server is a listening SSH server.
type Server struct {
	// Addr is host:port to dial.
	Addr string
	// Host is the address half, Port the numeric half, for callers that need
	// them separately.
	Host string
	Port int
	// HostKey is what the server presents; its fingerprint is what a client
	// must pin.
	HostKey ssh.PublicKey
	// Fingerprint is the SHA256 form the portal shows an operator.
	Fingerprint string
	// Root is a temporary directory the SFTP subsystem serves.
	Root string

	// Resizes records every window-change the client sent, so a test can prove
	// a browser resize reached the far end.
	Resizes []WindowSize

	mu       sync.Mutex
	listener net.Listener
	conns    []net.Conn
	closed   bool
}

// WindowSize is one window-change request.
type WindowSize struct{ Cols, Rows uint32 }

// Options configures the server.
type Options struct {
	// User, Password and AuthorizedKey are the credentials that will be
	// accepted. An empty Password refuses every password attempt, which is how
	// a test reproduces a key-only server.
	User          string
	Password      string
	AuthorizedKey ssh.PublicKey

	// NoSFTP refuses the sftp subsystem, reproducing a guest with the
	// subsystem commented out - a working terminal and no file browser.
	NoSFTP bool
	// NoPTY declines the pty-req, reproducing a server that will not give an
	// interactive terminal.
	NoPTY bool
}

// Start brings up a server and registers its shutdown with the test.
func Start(t *testing.T, opts Options) *Server {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("host key signer: %v", err)
	}

	if opts.User == "" {
		opts.User = "tester"
	}

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if opts.Password != "" && conn.User() == opts.User && string(password) == opts.Password {
				return &ssh.Permissions{}, nil
			}
			return nil, errAuth
		},
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if opts.AuthorizedKey != nil && conn.User() == opts.User &&
				string(key.Marshal()) == string(opts.AuthorizedKey.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errAuth
		},
	}
	cfg.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().(*net.TCPAddr)

	s := &Server{
		Addr:        listener.Addr().String(),
		Host:        addr.IP.String(),
		Port:        addr.Port,
		HostKey:     signer.PublicKey(),
		Fingerprint: ssh.FingerprintSHA256(signer.PublicKey()),
		Root:        t.TempDir(),
		listener:    listener,
	}

	go s.accept(cfg, opts)
	t.Cleanup(s.Close)
	return s
}

// Close stops the server and drops every connection.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()

	_ = s.listener.Close()
	for _, c := range conns {
		_ = c.Close()
	}
}

// WindowChanges returns the resizes seen so far.
func (s *Server) WindowChanges() []WindowSize {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]WindowSize(nil), s.Resizes...)
}

func (s *Server) accept(cfg *ssh.ServerConfig, opts Options) {
	for {
		raw, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = raw.Close()
			return
		}
		s.conns = append(s.conns, raw)
		s.mu.Unlock()

		go s.serve(raw, cfg, opts)
	}
}

func (s *Server) serve(raw net.Conn, cfg *ssh.ServerConfig, opts Options) {
	defer raw.Close()

	_, chans, reqs, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		return // a failed handshake is a test asserting exactly that
	}
	// Keepalives and anything else global are answered "no" and ignored.
	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only sessions here")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			return
		}
		go s.session(channel, requests, opts)
	}
}

func (s *Server) session(channel ssh.Channel, requests <-chan *ssh.Request, opts Options) {
	for req := range requests {
		switch req.Type {
		case "pty-req":
			_ = req.Reply(!opts.NoPTY, nil)

		case "window-change":
			// The payload is four uint32s: columns, rows, width, height.
			if len(req.Payload) >= 8 {
				s.mu.Lock()
				s.Resizes = append(s.Resizes, WindowSize{
					Cols: binary.BigEndian.Uint32(req.Payload[0:4]),
					Rows: binary.BigEndian.Uint32(req.Payload[4:8]),
				})
				s.mu.Unlock()
			}
			_ = req.Reply(true, nil)

		case "shell":
			_ = req.Reply(true, nil)
			go s.shell(channel)

		case "subsystem":
			name := subsystemName(req.Payload)
			if name != "sftp" || opts.NoSFTP {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)
			go s.sftp(channel)

		default:
			_ = req.Reply(false, nil)
		}
	}
}

// shell is a deliberately tiny line-oriented shell: it prints a prompt, echoes
// what is typed the way a tty does, and answers `exit` by closing the channel.
// That is enough to prove the relay carries bytes both ways, that a prompt with
// no trailing newline still reaches the browser, and that the shell exiting
// ends the session.
func (s *Server) shell(channel ssh.Channel) {
	defer channel.Close()

	_, _ = channel.Write([]byte("test$ "))
	buf := make([]byte, 1)
	var line strings.Builder

	for {
		n, err := channel.Read(buf)
		if err != nil || n == 0 {
			return
		}
		c := buf[0]
		_, _ = channel.Write([]byte{c}) // the echo a tty would do

		if c != '\r' && c != '\n' {
			line.WriteByte(c)
			continue
		}

		command := strings.TrimSpace(line.String())
		line.Reset()
		_, _ = channel.Write([]byte("\r\n"))

		switch {
		case command == "exit":
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			return
		case strings.HasPrefix(command, "echo "):
			_, _ = channel.Write([]byte(strings.TrimPrefix(command, "echo ") + "\r\n"))
		case command != "":
			_, _ = channel.Write([]byte("sh: " + command + ": not found\r\n"))
		}
		_, _ = channel.Write([]byte("test$ "))
	}
}

func (s *Server) sftp(channel ssh.Channel) {
	defer channel.Close()

	// Served rooted at a temp directory, so a test can assert against real
	// files on disk without touching anything outside it.
	server, err := sftp.NewServer(channel, sftp.WithServerWorkingDirectory(s.Root))
	if err != nil {
		return
	}
	if err := server.Serve(); err != nil && err != io.EOF {
		return
	}
}

// subsystemName reads the length-prefixed string in a subsystem request.
func subsystemName(payload []byte) string {
	if len(payload) < 4 {
		return ""
	}
	length := binary.BigEndian.Uint32(payload[:4])
	if uint32(len(payload)) < 4+length {
		return ""
	}
	return string(payload[4 : 4+length])
}

var errAuth = &authError{}

type authError struct{}

func (*authError) Error() string { return "sshtest: rejected" }

// WriteFile puts a file into the served directory.
func (s *Server) WriteFile(t *testing.T, name, content string) string {
	t.Helper()
	path := s.Root + "/" + name
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
