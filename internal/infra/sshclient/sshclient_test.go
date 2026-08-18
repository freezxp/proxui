package sshclient_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/shell"
	"github.com/freezxp/proxui/internal/infra/sshclient"
	"github.com/freezxp/proxui/internal/infra/sshclient/sshtest"
)

// acceptAny trusts whatever the server presents, for the tests whose subject is
// something other than the host key.
type acceptAny struct{ seen string }

func (a *acceptAny) Check(_, _, fingerprint string, _ []byte) error {
	a.seen = fingerprint
	return nil
}

// pinned refuses anything but one specific key, which is what the portal's real
// policy does once a VM has been connected to before.
type pinned struct{ key []byte }

func (p pinned) Check(_, _, _ string, publicKey []byte) error {
	if string(p.key) == string(publicKey) {
		return nil
	}
	return shell.ErrHostKeyMismatch
}

func target(s *sshtest.Server) ports.SSHTarget {
	return ports.SSHTarget{Host: s.Host, Port: s.Port}
}

func TestDialWithPassword(t *testing.T) {
	server := sshtest.Start(t, sshtest.Options{User: "tester", Password: "correct horse"})
	policy := &acceptAny{}

	conn, err := sshclient.NewDialer().Dial(context.Background(), target(server),
		ports.SSHCredential{Username: "tester", Password: "correct horse"}, policy)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if policy.seen != server.Fingerprint {
		t.Fatalf("policy saw %q, server presents %q", policy.seen, server.Fingerprint)
	}
	if got := conn.HostKey().Fingerprint; got != server.Fingerprint {
		t.Fatalf("reported fingerprint %q, want %q", got, server.Fingerprint)
	}
	if got := conn.HostKey().Algorithm; got != ssh.KeyAlgoED25519 {
		t.Fatalf("negotiated %q; an operator comparing fingerprints expects ed25519", got)
	}
}

func TestDialWithPrivateKey(t *testing.T) {
	signer, pem := generateKey(t)
	server := sshtest.Start(t, sshtest.Options{User: "tester", AuthorizedKey: signer.PublicKey()})

	conn, err := sshclient.NewDialer().Dial(context.Background(), target(server),
		ports.SSHCredential{Username: "tester", PrivateKey: pem}, &acceptAny{})
	if err != nil {
		t.Fatalf("dial with key: %v", err)
	}
	conn.Close()
}

func TestDialRejectsWrongCredential(t *testing.T) {
	server := sshtest.Start(t, sshtest.Options{User: "tester", Password: "correct horse"})

	_, err := sshclient.NewDialer().Dial(context.Background(), target(server),
		ports.SSHCredential{Username: "tester", Password: "wrong"}, &acceptAny{})
	if !errors.Is(err, shell.ErrAuthFailed) {
		t.Fatalf("dial = %v, want ErrAuthFailed — a wrong password must not read as unreachable", err)
	}
}

func TestDialRefusesAChangedHostKey(t *testing.T) {
	server := sshtest.Start(t, sshtest.Options{User: "tester", Password: "pw"})

	// A key from a different machine entirely: what a replaced guest, or
	// something answering in its place, would present.
	other, _ := generateKey(t)

	_, err := sshclient.NewDialer().Dial(context.Background(), target(server),
		ports.SSHCredential{Username: "tester", Password: "pw"},
		pinned{key: other.PublicKey().Marshal()})
	if !errors.Is(err, shell.ErrHostKeyMismatch) {
		t.Fatalf("dial = %v, want ErrHostKeyMismatch", err)
	}
}

func TestDialAcceptsThePinnedHostKey(t *testing.T) {
	server := sshtest.Start(t, sshtest.Options{User: "tester", Password: "pw"})

	conn, err := sshclient.NewDialer().Dial(context.Background(), target(server),
		ports.SSHCredential{Username: "tester", Password: "pw"},
		pinned{key: server.HostKey.Marshal()})
	if err != nil {
		t.Fatalf("dial with the pinned key: %v", err)
	}
	conn.Close()
}

func TestDialReportsAnUnreachablePort(t *testing.T) {
	server := sshtest.Start(t, sshtest.Options{User: "tester", Password: "pw"})
	dead := target(server)
	server.Close()

	_, err := sshclient.NewDialer().Dial(context.Background(), dead,
		ports.SSHCredential{Username: "tester", Password: "pw"}, &acceptAny{})
	if !errors.Is(err, shell.ErrUnreachable) {
		t.Fatalf("dial = %v, want ErrUnreachable", err)
	}
}

func TestDialNeedsSomeCredential(t *testing.T) {
	server := sshtest.Start(t, sshtest.Options{User: "tester", Password: "pw"})

	_, err := sshclient.NewDialer().Dial(context.Background(), target(server),
		ports.SSHCredential{Username: "tester"}, &acceptAny{})
	if !errors.Is(err, shell.ErrCredentialMissing) {
		t.Fatalf("dial = %v, want ErrCredentialMissing", err)
	}
}

func TestDialWithAnEncryptedPrivateKey(t *testing.T) {
	signer, encrypted := generateEncryptedKey(t, "let me in")
	server := sshtest.Start(t, sshtest.Options{User: "tester", AuthorizedKey: signer.PublicKey()})

	// Without the passphrase the key cannot even be read, and the operator
	// needs to be told that rather than "authentication failed".
	_, err := sshclient.NewDialer().Dial(context.Background(), target(server),
		ports.SSHCredential{Username: "tester", PrivateKey: encrypted}, &acceptAny{})
	if !errors.Is(err, shell.ErrBadKey) {
		t.Fatalf("dial without the passphrase = %v, want ErrBadKey", err)
	}

	conn, err := sshclient.NewDialer().Dial(context.Background(), target(server),
		ports.SSHCredential{Username: "tester", PrivateKey: encrypted, Passphrase: "let me in"},
		&acceptAny{})
	if err != nil {
		t.Fatalf("dial with the passphrase: %v", err)
	}
	conn.Close()
}

func TestDialRejectsAnUnreadableKey(t *testing.T) {
	server := sshtest.Start(t, sshtest.Options{User: "tester", Password: "pw"})

	_, err := sshclient.NewDialer().Dial(context.Background(), target(server),
		ports.SSHCredential{Username: "tester", PrivateKey: "not a key at all"}, &acceptAny{})
	if !errors.Is(err, shell.ErrBadKey) {
		t.Fatalf("dial = %v, want ErrBadKey", err)
	}
}

func TestTerminalCarriesBytesBothWays(t *testing.T) {
	server := sshtest.Start(t, sshtest.Options{User: "tester", Password: "pw"})
	conn := dial(t, server)
	defer conn.Close()

	term, err := conn.Terminal(ports.TerminalSize{Cols: 100, Rows: 40})
	if err != nil {
		t.Fatalf("open terminal: %v", err)
	}
	defer term.Close()

	// The prompt arrives with no trailing newline, which is the case a
	// line-buffered relay would swallow.
	if got := readFor(t, term, "test$ "); !strings.Contains(got, "test$ ") {
		t.Fatalf("no prompt arrived; read %q", got)
	}

	if _, err := term.Write([]byte("echo hello\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readFor(t, term, "hello"); !strings.Contains(got, "hello") {
		t.Fatalf("command output missing; read %q", got)
	}
}

func TestTerminalResizeReachesTheServer(t *testing.T) {
	server := sshtest.Start(t, sshtest.Options{User: "tester", Password: "pw"})
	conn := dial(t, server)
	defer conn.Close()

	term, err := conn.Terminal(ports.TerminalSize{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("open terminal: %v", err)
	}
	defer term.Close()
	readFor(t, term, "test$ ")

	if err := term.Resize(132, 43); err != nil {
		t.Fatalf("resize: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, size := range server.WindowChanges() {
			if size.Cols == 132 && size.Rows == 43 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the server never saw the resize; saw %v", server.WindowChanges())
}

func TestTerminalDefaultsAnAbsurdSize(t *testing.T) {
	server := sshtest.Start(t, sshtest.Options{User: "tester", Password: "pw"})
	conn := dial(t, server)
	defer conn.Close()

	// A browser that has not laid out yet reports zero, and a PTY of zero
	// columns makes every guest program draw nothing.
	term, err := conn.Terminal(ports.TerminalSize{})
	if err != nil {
		t.Fatalf("open terminal: %v", err)
	}
	defer term.Close()
	if got := readFor(t, term, "test$ "); !strings.Contains(got, "test$ ") {
		t.Fatalf("terminal unusable at the default size; read %q", got)
	}
}

func TestFilesRoundTrip(t *testing.T) {
	server := sshtest.Start(t, sshtest.Options{User: "tester", Password: "pw"})
	server.WriteFile(t, "notes.txt", "first line\n")
	conn := dial(t, server)
	defer conn.Close()

	files, err := conn.Files()
	if err != nil {
		t.Fatalf("open sftp: %v", err)
	}
	ctx := context.Background()

	home, err := files.Home(ctx)
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	if home == "" {
		t.Fatal("home is empty; the browser would open nowhere")
	}

	entries, err := files.List(ctx, server.Root)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !hasEntry(entries, "notes.txt") {
		t.Fatalf("listing missed the file: %v", names(entries))
	}

	body, size, err := files.Open(ctx, server.Root+"/notes.txt")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	content, _ := io.ReadAll(body)
	body.Close()
	if string(content) != "first line\n" {
		t.Fatalf("read %q", content)
	}
	if size != int64(len("first line\n")) {
		t.Fatalf("size %d", size)
	}

	// Upload.
	w, err := files.Create(ctx, server.Root+"/uploaded.bin")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := w.Write([]byte("payload")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got, err := os.ReadFile(server.Root + "/uploaded.bin"); err != nil || string(got) != "payload" {
		t.Fatalf("uploaded file on disk = %q, %v", got, err)
	}

	// Directory, rename, mode, delete.
	if err := files.Mkdir(ctx, server.Root+"/sub"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := files.Rename(ctx, server.Root+"/uploaded.bin", server.Root+"/sub/moved.bin"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := files.Chmod(ctx, server.Root+"/sub/moved.bin", 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	info, err := os.Stat(server.Root + "/sub/moved.bin")
	if err != nil {
		t.Fatalf("stat after move: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode is %v after chmod", info.Mode().Perm())
	}
	if err := files.Remove(ctx, server.Root+"/sub/moved.bin"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(server.Root + "/sub/moved.bin"); !os.IsNotExist(err) {
		t.Fatal("the file survived the delete")
	}
	// An empty directory goes too; a full one is the guest's business.
	if err := files.Remove(ctx, server.Root+"/sub"); err != nil {
		t.Fatalf("remove directory: %v", err)
	}
}

func TestFilesReportMissingPathsAsNotExist(t *testing.T) {
	server := sshtest.Start(t, sshtest.Options{User: "tester", Password: "pw"})
	conn := dial(t, server)
	defer conn.Close()

	files, err := conn.Files()
	if err != nil {
		t.Fatalf("open sftp: %v", err)
	}
	_, _, err = files.Open(context.Background(), server.Root+"/nope")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("open missing = %v, want fs.ErrNotExist so the API can answer 404", err)
	}
}

func TestFilesRefuseToOpenADirectory(t *testing.T) {
	server := sshtest.Start(t, sshtest.Options{User: "tester", Password: "pw"})
	conn := dial(t, server)
	defer conn.Close()

	files, _ := conn.Files()
	if _, _, err := files.Open(context.Background(), server.Root); !errors.Is(err, shell.ErrBadPath) {
		t.Fatalf("open directory = %v, want ErrBadPath", err)
	}
}

func TestFilesListSortsDirectoriesFirst(t *testing.T) {
	server := sshtest.Start(t, sshtest.Options{User: "tester", Password: "pw"})
	server.WriteFile(t, "a-file", "x")
	if err := os.Mkdir(server.Root+"/z-dir", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	conn := dial(t, server)
	defer conn.Close()

	files, _ := conn.Files()
	entries, err := files.List(context.Background(), server.Root)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) < 2 || !entries[0].IsDir {
		t.Fatalf("directories should sort first, got %v", names(entries))
	}
}

func TestFilesUnavailableWithoutTheSubsystem(t *testing.T) {
	// A guest with `Subsystem sftp` commented out: the terminal still works,
	// and only the file panel is missing.
	server := sshtest.Start(t, sshtest.Options{User: "tester", Password: "pw", NoSFTP: true})
	conn := dial(t, server)
	defer conn.Close()

	if _, err := conn.Files(); !errors.Is(err, shell.ErrFilesUnavailable) {
		t.Fatalf("Files() = %v, want ErrFilesUnavailable", err)
	}
	term, err := conn.Terminal(ports.TerminalSize{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("the terminal should be unaffected: %v", err)
	}
	term.Close()
}

// --- helpers ------------------------------------------------------------

func dial(t *testing.T, server *sshtest.Server) ports.ShellConn {
	t.Helper()
	conn, err := sshclient.NewDialer().Dial(context.Background(), target(server),
		ports.SSHCredential{Username: "tester", Password: "pw"}, &acceptAny{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

// readFor reads until want appears or the deadline passes, returning
// everything it saw so a failure can show what did arrive.
func readFor(t *testing.T, r io.Reader, want string) string {
	t.Helper()
	var seen strings.Builder
	done := make(chan struct{})

	go func() {
		defer close(done)
		buf := make([]byte, 1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				seen.Write(buf[:n])
				if strings.Contains(seen.String(), want) {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	return seen.String()
}

// generateKey makes a fresh ed25519 key and returns it in the OpenSSH PEM form
// an operator would paste into the connect form. Generated rather than kept as
// a fixture: a private key checked into a repository is a private key, however
// loudly the comment says otherwise.
func generateKey(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "proxui test key")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return signer, string(pem.EncodeToMemory(block))
}

// generateEncryptedKey is the same thing behind a passphrase.
func generateEncryptedKey(t *testing.T, passphrase string) (ssh.Signer, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "proxui test key", []byte(passphrase))
	if err != nil {
		t.Fatalf("marshal encrypted key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return signer, string(pem.EncodeToMemory(block))
}

func hasEntry(entries []shell.FileEntry, name string) bool {
	for _, e := range entries {
		if e.Name == name {
			return true
		}
	}
	return false
}

func names(entries []shell.FileEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}
