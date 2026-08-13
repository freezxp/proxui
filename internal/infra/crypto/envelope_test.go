package crypto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newTestVault(t *testing.T) (*Vault, []byte) {
	t.Helper()
	key := make([]byte, MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	v, err := NewVault(key, 1)
	if err != nil {
		t.Fatalf("NewVault: %v", err)
	}
	return v, key
}

func TestSealOpenRoundTrip(t *testing.T) {
	v, _ := newTestVault(t)
	secret := "s3cr3t-proxmox-token-value"

	sealed, err := v.Seal(secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed.Ciphertext, []byte(secret)) {
		t.Fatal("the plaintext secret is visible in the ciphertext")
	}
	if len(sealed.Nonce) == 0 || len(sealed.DEKWrapped) == 0 || len(sealed.DEKNonce) == 0 {
		t.Fatalf("sealed secret is missing envelope material: %+v", sealed)
	}

	got, err := v.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != secret {
		t.Errorf("Open returned %q, want the original secret", got)
	}
}

// Each credential must get its own data key and nonce, or identical secrets
// would produce identical ciphertext and leak that they match.
func TestSealsAreUnique(t *testing.T) {
	v, _ := newTestVault(t)

	first, _ := v.Seal("same secret")
	second, _ := v.Seal("same secret")

	if bytes.Equal(first.Ciphertext, second.Ciphertext) {
		t.Error("identical secrets produced identical ciphertext")
	}
	if bytes.Equal(first.DEKWrapped, second.DEKWrapped) {
		t.Error("two credentials share a data key")
	}
	if bytes.Equal(first.Nonce, second.Nonce) {
		t.Error("nonce reuse: GCM security depends on unique nonces")
	}
}

func TestOpenRejectsWrongMasterKey(t *testing.T) {
	v, _ := newTestVault(t)
	other, _ := newTestVault(t)

	sealed, _ := v.Seal("token")
	if _, err := other.Open(sealed); !errors.Is(err, ErrDecrypt) {
		t.Errorf("error = %v, want ErrDecrypt when the master key differs", err)
	}
}

// GCM is authenticated: a tampered credential must fail to open rather than
// decrypt to something unpredictable.
func TestOpenDetectsTampering(t *testing.T) {
	v, _ := newTestVault(t)
	sealed, _ := v.Seal("token")

	tampered := sealed
	tampered.Ciphertext = append([]byte(nil), sealed.Ciphertext...)
	tampered.Ciphertext[0] ^= 0xff

	if _, err := v.Open(tampered); !errors.Is(err, ErrDecrypt) {
		t.Errorf("error = %v, want ErrDecrypt for a modified ciphertext", err)
	}

	tamperedKey := sealed
	tamperedKey.DEKWrapped = append([]byte(nil), sealed.DEKWrapped...)
	tamperedKey.DEKWrapped[0] ^= 0xff
	if _, err := v.Open(tamperedKey); !errors.Is(err, ErrDecrypt) {
		t.Errorf("error = %v, want ErrDecrypt for a modified data key", err)
	}
}

// Master-key rotation must not need the plaintext secret: that is what makes it
// a short transaction instead of a migration.
func TestRewrapRotatesMasterKeyWithoutPlaintext(t *testing.T) {
	oldVault, _ := newTestVault(t)
	newKey := make([]byte, MasterKeySize)
	if _, err := rand.Read(newKey); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	newVault, err := NewVault(newKey, 2)
	if err != nil {
		t.Fatalf("NewVault: %v", err)
	}

	sealed, _ := oldVault.Seal("rotate-me")
	rotated, err := newVault.Rewrap(sealed, oldVault)
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}

	if !bytes.Equal(rotated.Ciphertext, sealed.Ciphertext) {
		t.Error("rewrapping re-encrypted the secret; only the data key should change")
	}
	if rotated.KeyVersion != 2 {
		t.Errorf("key version = %d, want 2", rotated.KeyVersion)
	}

	got, err := newVault.Open(rotated)
	if err != nil {
		t.Fatalf("Open after rotation: %v", err)
	}
	if got != "rotate-me" {
		t.Errorf("Open returned %q after rotation", got)
	}
	// The old key must no longer open the rotated credential.
	if _, err := oldVault.Open(rotated); !errors.Is(err, ErrDecrypt) {
		t.Error("the previous master key still opens a rotated credential")
	}
}

func TestNewVaultRejectsShortKey(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33} {
		if _, err := NewVault(make([]byte, size), 1); !errors.Is(err, ErrMasterKey) {
			t.Errorf("NewVault with %d-byte key: error = %v, want ErrMasterKey", size, err)
		}
	}
}

func TestLoadMasterKeyGeneratesThenReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")

	first, err := LoadMasterKey(path)
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	if len(first) != MasterKeySize {
		t.Fatalf("generated key is %d bytes, want %d", len(first), MasterKeySize)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %o, want 600: it protects every stored credential", perm)
	}

	second, err := LoadMasterKey(path)
	if err != nil {
		t.Fatalf("LoadMasterKey (reload): %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("reloading produced a different key; every stored credential would become unreadable")
	}
}

func TestLoadMasterKeyRejectsMalformedFile(t *testing.T) {
	dir := t.TempDir()
	tests := []struct{ name, content string }{
		{"not base64", "!!!not-base64!!!"},
		{"wrong length", "c2hvcnQ="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name)
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, err := LoadMasterKey(path); !errors.Is(err, ErrMasterKey) {
				t.Errorf("error = %v, want ErrMasterKey", err)
			}
		})
	}
}
