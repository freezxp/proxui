package sshclient

import (
	"io"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/shell"
)

// Conn is one authenticated SSH connection, carrying both the terminal and the
// file browser (SSH-09). One connection for both is not an optimization: a
// second connection would mean a second authentication, and the credential is
// gone by then.
type Conn struct {
	client  *ssh.Client
	hostKey shell.HostKey

	closeOnce sync.Once
	done      chan struct{}
}

// HostKey reports the key the guest presented during the handshake.
func (c *Conn) HostKey() shell.HostKey { return c.hostKey }

// Terminal opens an interactive shell with a PTY of the given size.
func (c *Conn) Terminal(size ports.TerminalSize) (ports.Terminal, error) {
	session, err := c.client.NewSession()
	if err != nil {
		return nil, err
	}

	cols, rows := size.Cols, size.Rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	// ECHO on and a sane baud rate: the shell on the far end draws the line
	// editor, and xterm.js in the browser is a dumb display of what it sends.
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 38400,
		ssh.TTY_OP_OSPEED: 38400,
	}
	// xterm-256color rather than xterm: it is what the browser side actually
	// renders, and claiming less makes vim and tmux pick worse colours.
	if err := session.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		_ = session.Close()
		return nil, err
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	// With a PTY the guest merges stderr into the same stream, but a server
	// that declines the PTY would otherwise drop error output silently.
	stderr, err := session.StderrPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}

	if err := session.Shell(); err != nil {
		_ = session.Close()
		return nil, err
	}

	t := &terminal{session: session, in: stdin, out: stdout, extra: stderr}
	t.pipe.r, t.pipe.w = io.Pipe()
	return t, nil
}

// Files opens the SFTP subsystem, or reports that the guest has none.
func (c *Conn) Files() (ports.RemoteFS, error) { return newRemoteFS(c.client) }

// Close tears down the connection and everything multiplexed on it.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.done)
		err = c.client.Close()
	})
	return err
}

// keepAlive prods the far end so a dropped flow surfaces as a closed terminal
// rather than as one that has quietly stopped listening.
func (c *Conn) keepAlive(every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if _, _, err := c.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				_ = c.Close()
				return
			}
		}
	}
}

// terminal adapts an ssh session to ports.Terminal.
type terminal struct {
	session *ssh.Session
	in      io.WriteCloser
	out     io.Reader
	extra   io.Reader

	// The merge pipe is built at construction, not on first Read.
	//
	// It used to be lazy, and that was a data race: Read wrote these fields
	// under one sync.Once while Close read them under another, and two Onces
	// guarding the same fields exclude nothing. A browser that closed its
	// WebSocket while the first read was still setting up could see a
	// half-written pipe writer - and then never close it, leaving the copy
	// goroutines blocked until the session sweep. Written once before any
	// goroutine exists, these are safe to read from both.
	pipe struct {
		r *io.PipeReader
		w *io.PipeWriter
	}

	once      sync.Once
	startOnce sync.Once
}

// Read merges stdout and stderr into the single stream the browser renders.
func (t *terminal) Read(p []byte) (int, error) {
	t.startOnce.Do(func() {
		w := t.pipe.w
		var wg sync.WaitGroup
		wg.Add(2)
		copyInto := func(src io.Reader) {
			defer wg.Done()
			_, _ = io.Copy(w, src)
		}
		go copyInto(t.out)
		go copyInto(t.extra)
		go func() {
			wg.Wait()
			// Both streams ended: the shell exited, so the reader should see
			// EOF rather than block forever.
			_ = w.Close()
		}()
	})
	return t.pipe.r.Read(p)
}

func (t *terminal) Write(p []byte) (int, error) { return t.in.Write(p) }

// Resize tells the far end the window changed, which is what makes `top` and
// an editor redraw at the browser's actual size.
func (t *terminal) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	return t.session.WindowChange(rows, cols)
}

func (t *terminal) Close() error {
	var err error
	t.once.Do(func() {
		_ = t.in.Close()
		if t.pipe.w != nil {
			_ = t.pipe.w.Close()
		}
		err = t.session.Close()
	})
	return err
}
