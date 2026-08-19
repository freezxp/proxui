package sshclient

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/shell"
)

// maxCommandOutput bounds what one command may return. `sensors -j` on a
// large board is a few kilobytes; anything approaching this is a node
// answering with something other than what was asked for.
const maxCommandOutput = 1 << 20

// RunCommand connects, runs one command, and returns its standard output.
//
// This is deliberately not built on Dial: it never produces a ports.ShellConn,
// so there is no path from a node connection to a terminal, a file browser or
// a forwarded port. A node is reachable for exactly the command the caller
// names and nothing else, which is the boundary ADR 0007 relaxed SSH-02 to —
// and a boundary held by the shape of the code is worth more than one held by
// a comment asking callers not to.
//
// The command is a constant supplied by the collector. Nothing here quotes or
// escapes, because nothing here should ever be assembling a command out of
// anything a request carried.
func (d *Dialer) RunCommand(ctx context.Context, target ports.SSHTarget, cred ports.SSHCredential,
	policy ports.HostKeyPolicy, command string) ([]byte, error) {
	methods, err := authMethods(cred)
	if err != nil {
		return nil, err
	}

	cfg := &ssh.ClientConfig{
		User:              cred.Username,
		Auth:              methods,
		Timeout:           d.ConnectTimeout,
		HostKeyAlgorithms: hostKeyAlgorithms,
		HostKeyCallback: func(hostname string, _ net.Addr, key ssh.PublicKey) error {
			return policy.Check(hostname, key.Type(), ssh.FingerprintSHA256(key), key.Marshal())
		},
	}

	address := target.Address()
	dialer := &net.Dialer{Timeout: d.ConnectTimeout}
	tcp, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", shell.ErrUnreachable, cleanNetError(err))
	}
	defer tcp.Close()

	_ = tcp.SetDeadline(time.Now().Add(d.ConnectTimeout))
	conn, chans, reqs, err := ssh.NewClientConn(tcp, address, cfg)
	if err != nil {
		return nil, classifyHandshake(err)
	}
	client := ssh.NewClient(conn, chans, reqs)
	defer client.Close()

	// The command gets the whole remaining deadline of the caller's context.
	// A node that accepts the connection and then never answers is the shape
	// a hung collector takes, so the deadline is set rather than assumed.
	if deadline, ok := ctx.Deadline(); ok {
		_ = tcp.SetDeadline(deadline)
	} else {
		_ = tcp.SetDeadline(time.Now().Add(d.ConnectTimeout))
	}

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh: could not open a session: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &limitedWriter{w: &stdout, left: maxCommandOutput}
	// Kept and truncated: a node that answers "command not found" has said the
	// most useful thing anybody will say about this failure.
	session.Stderr = &limitedWriter{w: &stderr, left: 4096}

	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		return nil, ctx.Err()
	case err := <-done:
		if err != nil {
			if msg := firstLine(stderr.String()); msg != "" {
				return stdout.Bytes(), fmt.Errorf("ssh: %q failed: %s", command, msg)
			}
			return stdout.Bytes(), fmt.Errorf("ssh: %q failed: %w", command, err)
		}
	}
	return stdout.Bytes(), nil
}

// limitedWriter drops everything past a bound instead of failing, so a node
// that answers with a gigabyte costs a megabyte of memory and no more.
type limitedWriter struct {
	w    *bytes.Buffer
	left int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.left <= 0 {
		return len(p), nil
	}
	if len(p) > l.left {
		l.w.Write(p[:l.left])
		l.left = 0
		return len(p), nil
	}
	l.w.Write(p)
	l.left -= len(p)
	return len(p), nil
}
