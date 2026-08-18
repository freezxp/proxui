package sshclient_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/shell"
	"github.com/freezxp/proxui/internal/infra/sshclient"
	"github.com/freezxp/proxui/internal/infra/sshclient/sshtest"
)

// The generated pair has to satisfy two different readers: an sshd reading the
// public half out of authorized_keys, and this package's own dialer reading
// the private half. A test that only checks the strings look right would pass
// for a key that authenticates nothing, so this dials a real server with it.

func TestGeneratedKeyAuthenticatesAgainstARealServer(t *testing.T) {
	privatePEM, portalKey, err := sshclient.GenerateKeyPair(shell.KeyComment)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// Parse the public half exactly as sshd would parse the line the portal
	// writes into authorized_keys.
	authorized, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(portalKey.PublicKey))
	if err != nil {
		t.Fatalf("the public key is not a valid authorized_keys line: %v", err)
	}
	if comment != shell.KeyComment {
		t.Fatalf("comment is %q, want %q", comment, shell.KeyComment)
	}

	server := sshtest.Start(t, sshtest.Options{User: "tester", AuthorizedKey: authorized})
	conn, err := sshclient.NewDialer().Dial(context.Background(),
		ports.SSHTarget{Host: server.Host, Port: server.Port},
		ports.SSHCredential{Username: "tester", PrivateKey: privatePEM}, &acceptAny{})
	if err != nil {
		t.Fatalf("dial with the generated key: %v", err)
	}
	defer conn.Close()
}

func TestGeneratedKeyIsEd25519AndFingerprintsConsistently(t *testing.T) {
	privatePEM, portalKey, err := sshclient.GenerateKeyPair(shell.KeyComment)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if portalKey.Algorithm != ssh.KeyAlgoED25519 {
		t.Fatalf("algorithm is %q, want %q", portalKey.Algorithm, ssh.KeyAlgoED25519)
	}
	if !strings.HasPrefix(portalKey.Fingerprint, "SHA256:") {
		t.Fatalf("fingerprint %q is not the form an operator compares", portalKey.Fingerprint)
	}

	// The fingerprint recorded beside the public half must be the one derived
	// from the private half, or a restored database against a different master
	// key would present as an unexplained authentication failure.
	recovered, err := sshclient.PublicKeyFor(privatePEM)
	if err != nil {
		t.Fatalf("recover public key: %v", err)
	}
	if recovered.Fingerprint != portalKey.Fingerprint {
		t.Fatalf("fingerprints disagree: stored %q, derived %q",
			portalKey.Fingerprint, recovered.Fingerprint)
	}
	if !shell.SameKey(recovered.PublicKey, portalKey.PublicKey) {
		t.Fatal("the recovered public key is not the stored one")
	}
}

func TestTwoGeneratedKeysDiffer(t *testing.T) {
	_, first, err := sshclient.GenerateKeyPair(shell.KeyComment)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	_, second, err := sshclient.GenerateKeyPair(shell.KeyComment)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if first.Fingerprint == second.Fingerprint {
		t.Fatal("rotation must produce a different key")
	}
}

func TestPublicKeyForRejectsRubbish(t *testing.T) {
	if _, err := sshclient.PublicKeyFor("not a key"); err == nil {
		t.Fatal("want an error for a key that is not one")
	}
}

// TestAppendDoesNotOverwriteTheFile pins the behaviour the install path
// depends on.
//
// This is a regression test with a story: the first implementation opened the
// file with O_APPEND and trusted the server to honour it. SFTP writes carry
// an explicit offset, and a server that ignores the flag takes the client's -
// zero - and overwrites from the start. Installing a key silently erased
// every key already in authorized_keys.
func TestAppendDoesNotOverwriteTheFile(t *testing.T) {
	server := sshtest.Start(t, sshtest.Options{User: "tester", Password: "pw"})
	conn := dial(t, server)
	defer conn.Close()

	files, err := conn.Files()
	if err != nil {
		t.Fatalf("files: %v", err)
	}
	path := server.WriteFile(t, "authorized_keys", "first line\n")

	w, err := files.Append(context.Background(), path)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := io.WriteString(w, "second line\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(body) != "first line\nsecond line\n" {
		t.Fatalf("append clobbered the file: %q", body)
	}
}

func TestAppendCreatesAFileThatIsNotThere(t *testing.T) {
	server := sshtest.Start(t, sshtest.Options{User: "tester", Password: "pw"})
	conn := dial(t, server)
	defer conn.Close()

	files, err := conn.Files()
	if err != nil {
		t.Fatalf("files: %v", err)
	}
	path := filepath.Join(server.Root, "fresh_authorized_keys")

	w, err := files.Append(context.Background(), path)
	if err != nil {
		t.Fatalf("append to a missing file: %v", err)
	}
	if _, err := io.WriteString(w, "only line\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(body) != "only line\n" {
		t.Fatalf("unexpected content %q", body)
	}
}
