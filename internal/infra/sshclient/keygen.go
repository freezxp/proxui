package sshclient

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/freezxp/proxui/internal/domain/shell"
)

// GenerateKeyPair makes the portal's SSH key (SSH-11).
//
// Ed25519 rather than RSA: every sshd the portal will meet has supported it
// for a decade, the public half is one short line an operator can eyeball
// against what is in authorized_keys, and there is no key size to get wrong.
//
// The private half comes back as an unencrypted OpenSSH PEM. It is unencrypted
// because the thing that protects it is the vault it is immediately sealed
// into, and a passphrase the portal would have to store beside the key
// protects nothing. It must not be written to disk or logged; the only caller
// seals it and drops it.
func GenerateKeyPair(comment string) (privatePEM string, pub shell.PortalKey, err error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", shell.PortalKey{}, fmt.Errorf("generate ssh key: %w", err)
	}

	block, err := ssh.MarshalPrivateKey(private, comment)
	if err != nil {
		return "", shell.PortalKey{}, fmt.Errorf("marshal ssh key: %w", err)
	}

	sshPub, err := ssh.NewPublicKey(public)
	if err != nil {
		return "", shell.PortalKey{}, fmt.Errorf("derive ssh public key: %w", err)
	}

	// MarshalAuthorizedKey emits "<type> <base64>\n" with no comment; the
	// comment is appended so the line on a guest says what installed it.
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	if comment != "" {
		line += " " + comment
	}

	return string(pem.EncodeToMemory(block)), shell.PortalKey{
		PublicKey:   line,
		Algorithm:   sshPub.Type(),
		Fingerprint: ssh.FingerprintSHA256(sshPub),
	}, nil
}

// PublicKeyFor recovers the public half of a stored private key.
//
// Used to verify that what came out of the vault is the key the public record
// describes. A mismatch means the two halves have drifted - a restored
// database against a different master key, say - and it is worth catching
// before it presents as an unexplained authentication failure on a guest.
func PublicKeyFor(privatePEM string) (shell.PortalKey, error) {
	signer, err := ssh.ParsePrivateKey([]byte(privatePEM))
	if err != nil {
		return shell.PortalKey{}, fmt.Errorf("%w: %v", shell.ErrBadKey, err)
	}
	pub := signer.PublicKey()
	return shell.PortalKey{
		Algorithm:   pub.Type(),
		Fingerprint: ssh.FingerprintSHA256(pub),
		PublicKey:   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))),
	}, nil
}

// KeyGen adapts GenerateKeyPair to ports.SSHKeyGenerator.
type KeyGen struct{}

// Generate makes a key pair.
func (KeyGen) Generate(comment string) (string, shell.PortalKey, error) {
	return GenerateKeyPair(comment)
}
