// Package sshclient dials guests over SSH and exposes the two things the
// portal needs from a connection: an interactive terminal and a file transfer
// subsystem.
//
// It is the only place in the codebase that imports golang.org/x/crypto/ssh.
// Everything above it works through ports.ShellConn, which is what lets the
// command layer be tested without a running sshd.
package sshclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/shell"
)

// Dialer opens authenticated SSH connections.
type Dialer struct {
	// ConnectTimeout bounds the TCP connect and the handshake together. A
	// guest whose agent reported a stale address does not answer at all, and
	// the operator should be told so in seconds rather than at TCP's leisure.
	ConnectTimeout time.Duration

	// KeepAlive is how often to prod an idle connection. Without it, a
	// firewall that drops the flow leaves a terminal that looks connected and
	// swallows every keystroke.
	KeepAlive time.Duration
}

// NewDialer builds a dialer with sensible bounds.
func NewDialer() *Dialer {
	return &Dialer{ConnectTimeout: 12 * time.Second, KeepAlive: 30 * time.Second}
}

// Dial connects, verifies the host key through the policy, and authenticates.
func (d *Dialer) Dial(ctx context.Context, target ports.SSHTarget, cred ports.SSHCredential,
	policy ports.HostKeyPolicy) (ports.ShellConn, error) {
	methods, err := authMethods(cred)
	if err != nil {
		return nil, err
	}

	var seen shell.HostKey
	cfg := &ssh.ClientConfig{
		User:    cred.Username,
		Auth:    methods,
		Timeout: d.ConnectTimeout,
		// Ask for the same host key OpenSSH would negotiate, in the same
		// order. The fingerprint the portal shows is meant to be compared
		// against what `ssh` or `ssh-keyscan` shows the operator, and a
		// portal that pinned the ECDSA key while their terminal showed the
		// Ed25519 one would make that comparison impossible to do.
		HostKeyAlgorithms: []string{
			ssh.KeyAlgoED25519,
			ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
			ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSA,
		},
		// The host key check is the policy's, and it runs inside the
		// handshake: refusing here aborts before any credential is offered,
		// which is the entire point of checking it at all (SSH-04).
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			seen = shell.HostKey{
				Address:     hostname,
				Algorithm:   key.Type(),
				Fingerprint: ssh.FingerprintSHA256(key),
				PublicKey:   key.Marshal(),
			}
			return policy.Check(hostname, key.Type(), ssh.FingerprintSHA256(key), key.Marshal())
		},
	}

	address := target.Address()
	dialer := &net.Dialer{Timeout: d.ConnectTimeout}
	tcp, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", shell.ErrUnreachable, cleanNetError(err))
	}

	// The handshake gets the same deadline as the connect. Set on the raw
	// connection because ssh.NewClientConn takes no context.
	_ = tcp.SetDeadline(time.Now().Add(d.ConnectTimeout))
	conn, chans, reqs, err := ssh.NewClientConn(tcp, address, cfg)
	if err != nil {
		_ = tcp.Close()
		return nil, classifyHandshake(err)
	}
	_ = tcp.SetDeadline(time.Time{})

	client := ssh.NewClient(conn, chans, reqs)
	c := &Conn{client: client, hostKey: seen, done: make(chan struct{})}
	if d.KeepAlive > 0 {
		go c.keepAlive(d.KeepAlive)
	}
	return c, nil
}

// authMethods turns what the operator typed into the methods to offer.
//
// Order matters: a key is tried before a password, so an account with both
// does not consume a password attempt against a server that counts them.
// Keyboard-interactive is offered alongside password because a stock sshd with
// `KbdInteractiveAuthentication yes` will refuse the plain password method and
// then accept the identical secret through the interactive one - the single
// most common reason a "wrong password" is not a wrong password.
func authMethods(cred ports.SSHCredential) ([]ssh.AuthMethod, error) {
	if strings.TrimSpace(cred.Username) == "" {
		return nil, shell.ErrCredentialMissing
	}

	var methods []ssh.AuthMethod
	if key := strings.TrimSpace(cred.PrivateKey); key != "" {
		signer, err := parseKey(key, cred.Passphrase)
		if err != nil {
			return nil, err
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if cred.Password != "" {
		password := cred.Password
		methods = append(methods,
			ssh.Password(password),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i, q := range questions {
					// Answer only what looks like a password prompt. A server
					// asking something else - a one-time code, say - gets an
					// empty answer and fails cleanly, rather than being handed
					// the password as the answer to an unknown question.
					if strings.Contains(strings.ToLower(q), "password") {
						answers[i] = password
					}
				}
				return answers, nil
			}))
	}
	if len(methods) == 0 {
		return nil, shell.ErrCredentialMissing
	}
	return methods, nil
}

func parseKey(key, passphrase string) (ssh.Signer, error) {
	if passphrase != "" {
		signer, err := ssh.ParsePrivateKeyWithPassphrase([]byte(key), []byte(passphrase))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", shell.ErrBadKey, err)
		}
		return signer, nil
	}
	signer, err := ssh.ParsePrivateKey([]byte(key))
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, fmt.Errorf("%w: this key is encrypted and needs its passphrase", shell.ErrBadKey)
		}
		return nil, fmt.Errorf("%w: %v", shell.ErrBadKey, err)
	}
	return signer, nil
}

// classifyHandshake separates the three failures that need different answers:
// the host key was refused, the credential was refused, or the connection died.
func classifyHandshake(err error) error {
	// A policy refusal is wrapped by x/crypto/ssh, so the sentinel survives
	// only through errors.Is - which is why the policy returns sentinels.
	if errors.Is(err, shell.ErrHostKeyMismatch) || errors.Is(err, shell.ErrHostKeyUnknown) {
		return err
	}
	text := err.Error()
	switch {
	case strings.Contains(text, "unable to authenticate"),
		strings.Contains(text, "no supported methods remain"),
		strings.Contains(text, "auth"):
		return fmt.Errorf("%w: %s", shell.ErrAuthFailed, firstLine(text))
	default:
		return fmt.Errorf("%w: %s", shell.ErrUnreachable, firstLine(text))
	}
}

// cleanNetError trims the address out of a dial error. The operator already
// knows which machine they asked for, and the raw string is mostly noise.
func cleanNetError(err error) string {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return opErr.Err.Error()
	}
	return firstLine(err.Error())
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
