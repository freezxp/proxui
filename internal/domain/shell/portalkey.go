package shell

import (
	"crypto/subtle"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The portal's own SSH key (SSH-11..SSH-14, ADR 0006).
//
// This is the one SSH secret the portal keeps, and it is the portal's own
// rather than a guest account's: the private half never leaves the vault, the
// public half is installed into an operator's `authorized_keys` on guests they
// already had a shell on, and revoking it is removing that line. A guest
// password still arrives per session and is still never stored (SSH-03) - the
// key is what stops it having to arrive on *every* session.
var (
	// ErrNoPortalKey means no key pair has been generated yet.
	ErrNoPortalKey = errors.New("shell: the portal has no SSH key yet")
	// ErrBadAuthorizedKeys means the guest's authorized_keys could not be read
	// as text - almost always because the path is a directory or binary.
	ErrBadAuthorizedKeys = errors.New("shell: authorized_keys on the guest is not readable as text")
)

// KeyComment is the trailing comment written on the installed line. It is how
// an operator reading authorized_keys on a guest knows what put it there, and
// how removal finds its own line rather than someone else's.
const KeyComment = "proxui-portal"

// MaxAuthorizedKeysBytes bounds what the portal will read and rewrite. An
// authorized_keys file is a handful of lines; anything vastly larger is either
// not that file or not something to be rewriting blind.
const MaxAuthorizedKeysBytes = 512 * 1024

// PortalKey is the portal's key pair as everything above the vault sees it:
// the public half, and facts about it. The private half is never in this
// struct - it is opened from the vault at the moment of a dial and dropped.
type PortalKey struct {
	// PublicKey is a complete authorized_keys line: "ssh-ed25519 AAAA... proxui-portal".
	PublicKey   string
	Algorithm   string
	Fingerprint string
	CreatedAt   time.Time
	CreatedBy   uuid.UUID
}

// KeyInstall records that the portal key is in one account's authorized_keys
// on one guest.
//
// It is a record of what the portal did, not the authority for whether the key
// works: the guest's file is the truth, and an operator who edits it by hand
// leaves this row stale. It exists so the connect form can offer the key where
// it is likely to work, and so an administrator can see the estate-wide answer
// to "where is this key" before rotating it.
type KeyInstall struct {
	VMID        uuid.UUID `json:"vm_id"`
	VMName      string    `json:"vm_name,omitempty"`
	SSHUser     string    `json:"ssh_user"`
	Fingerprint string    `json:"fingerprint"`
	InstalledAt time.Time `json:"installed_at"`
	InstalledBy uuid.UUID `json:"installed_by"`
	// Stale marks an install whose fingerprint is not the current key's, which
	// is what a rotation leaves behind: the line on the guest is now a key
	// nobody holds, and it should be removed rather than trusted.
	Stale bool `json:"stale"`
}

// AuthorizedKeysPath is where the key goes, relative to the account's home.
const (
	SSHDir             = ".ssh"
	AuthorizedKeysFile = "authorized_keys"
	SSHDirMode         = 0o700
	AuthorizedKeysMode = 0o600
)

// keyBody reduces an authorized_keys line to the part that identifies the key:
// its type and base64 blob, without the comment and without any options
// prefix. Two lines match if their bodies match; a differing comment is the
// same key, and a differing comment is exactly what a hand-edited file has.
func keyBody(line string) string {
	fields := strings.Fields(strings.TrimSpace(line))
	for i, f := range fields {
		// The type is the first field that looks like a key type. Anything
		// before it is an options list ("no-pty,from=..."), which OpenSSH
		// allows and which must not stop a line matching.
		if strings.HasPrefix(f, "ssh-") || strings.HasPrefix(f, "ecdsa-") ||
			strings.HasPrefix(f, "sk-") {
			if i+1 < len(fields) {
				return f + " " + fields[i+1]
			}
			return f
		}
	}
	return ""
}

// SameKey reports whether two authorized_keys lines carry the same key.
func SameKey(a, b string) bool {
	ba, bb := keyBody(a), keyBody(b)
	if ba == "" || bb == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(ba), []byte(bb)) == 1
}

// HasAuthorizedKey reports whether content already authorizes this key.
func HasAuthorizedKey(content, publicKey string) bool {
	for _, line := range strings.Split(content, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if SameKey(line, publicKey) {
			return true
		}
	}
	return false
}

// AppendAuthorizedKey returns what to append to content so that it authorizes
// publicKey, and whether anything needs appending at all.
//
// Appending rather than rewriting is deliberate: this is the operation
// `ssh-copy-id` performs, and it cannot lose a line the portal did not put
// there. The returned string is empty when the key is already present, which
// makes installing twice a no-op rather than a duplicate line.
func AppendAuthorizedKey(content, publicKey string) (suffix string, needed bool) {
	if strings.TrimSpace(publicKey) == "" {
		return "", false
	}
	if HasAuthorizedKey(content, publicKey) {
		return "", false
	}
	var b strings.Builder
	// A file whose last line has no newline would otherwise gain a line that
	// reads "<their key>ssh-ed25519 AAAA...", authorizing nothing and breaking
	// the key that was there.
	if content != "" && !strings.HasSuffix(content, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(strings.TrimSpace(publicKey))
	b.WriteString("\n")
	return b.String(), true
}

// RemoveAuthorizedKey returns content without any line carrying publicKey, and
// whether it changed. Every other line survives byte for byte, including
// comments and blank lines: this rewrites a file the portal does not own.
func RemoveAuthorizedKey(content, publicKey string) (string, bool) {
	if strings.TrimSpace(publicKey) == "" {
		return content, false
	}
	lines := strings.Split(content, "\n")
	kept := make([]string, 0, len(lines))
	removed := false
	for _, line := range lines {
		if strings.TrimSpace(line) != "" && SameKey(line, publicKey) {
			removed = true
			continue
		}
		kept = append(kept, line)
	}
	if !removed {
		return content, false
	}
	return strings.Join(kept, "\n"), true
}
