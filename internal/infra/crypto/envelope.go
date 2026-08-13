package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Envelope encryption for platform credentials (docs/15-security-design.md
// §15.3). Each secret is sealed with its own data key; the data key is sealed
// with the master key, which lives outside the database. Rotating the master
// key rewraps data keys only — the secrets themselves are never re-encrypted,
// so rotation is one short transaction rather than a migration.
//
// AES-256-GCM throughout: authenticated, so a tampered ciphertext fails to open
// rather than decrypting to garbage.

// MasterKeySize is the required master key length in bytes.
const MasterKeySize = 32

// ErrMasterKey reports a missing or malformed master key.
var ErrMasterKey = errors.New("crypto: invalid master key")

// ErrDecrypt reports a credential that could not be opened, which usually means
// the wrong master key (a restored database with the old key backed up, say).
var ErrDecrypt = errors.New("crypto: cannot decrypt credential")

// SealedSecret is the stored form of an encrypted credential.
type SealedSecret struct {
	Ciphertext []byte
	Nonce      []byte
	DEKWrapped []byte
	DEKNonce   []byte
	KeyVersion int
}

// Vault seals and opens credentials with a master key.
type Vault struct {
	master     cipher.AEAD
	keyVersion int
}

// NewVault builds a vault from raw master key bytes.
func NewVault(masterKey []byte, keyVersion int) (*Vault, error) {
	if len(masterKey) != MasterKeySize {
		return nil, fmt.Errorf("%w: master key must be %d bytes, got %d", ErrMasterKey, MasterKeySize, len(masterKey))
	}
	aead, err := newAEAD(masterKey)
	if err != nil {
		return nil, err
	}
	if keyVersion <= 0 {
		keyVersion = 1
	}
	return &Vault{master: aead, keyVersion: keyVersion}, nil
}

// LoadMasterKey reads a base64-encoded master key from a file, generating one
// on first run so a fresh install needs no key ceremony. The file is written
// 0600: it protects every platform credential in the database.
func LoadMasterKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		key, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if decodeErr != nil {
			return nil, fmt.Errorf("%w: %s is not base64", ErrMasterKey, path)
		}
		if len(key) != MasterKeySize {
			return nil, fmt.Errorf("%w: %s holds %d bytes, want %d", ErrMasterKey, path, len(key), MasterKeySize)
		}
		return key, nil
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("crypto: read %s: %w", path, err)
	}

	key := make([]byte, MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("crypto: generate master key: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key) + "\n"
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		return nil, fmt.Errorf("crypto: write %s: %w", path, err)
	}
	return key, nil
}

// Seal encrypts a secret under a fresh data key.
func (v *Vault) Seal(secret string) (SealedSecret, error) {
	dek := make([]byte, MasterKeySize)
	if _, err := rand.Read(dek); err != nil {
		return SealedSecret{}, fmt.Errorf("crypto: generate data key: %w", err)
	}
	// The data key exists only for this call and this credential; it never
	// leaves memory unwrapped.
	defer zero(dek)

	dataAEAD, err := newAEAD(dek)
	if err != nil {
		return SealedSecret{}, err
	}
	nonce, err := newNonce(dataAEAD)
	if err != nil {
		return SealedSecret{}, err
	}
	ciphertext := dataAEAD.Seal(nil, nonce, []byte(secret), nil)

	dekNonce, err := newNonce(v.master)
	if err != nil {
		return SealedSecret{}, err
	}
	wrapped := v.master.Seal(nil, dekNonce, dek, nil)

	return SealedSecret{
		Ciphertext: ciphertext,
		Nonce:      nonce,
		DEKWrapped: wrapped,
		DEKNonce:   dekNonce,
		KeyVersion: v.keyVersion,
	}, nil
}

// Open decrypts a sealed secret.
func (v *Vault) Open(s SealedSecret) (string, error) {
	dek, err := v.master.Open(nil, s.DEKNonce, s.DEKWrapped, nil)
	if err != nil {
		return "", fmt.Errorf("%w: data key could not be unwrapped (wrong master key?)", ErrDecrypt)
	}
	defer zero(dek)

	dataAEAD, err := newAEAD(dek)
	if err != nil {
		return "", err
	}
	plaintext, err := dataAEAD.Open(nil, s.Nonce, s.Ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("%w: ciphertext failed authentication", ErrDecrypt)
	}
	return string(plaintext), nil
}

// Rewrap re-seals a credential's data key under this vault's master key,
// leaving the ciphertext untouched. This is master-key rotation: cheap, and it
// never needs the plaintext secret.
func (v *Vault) Rewrap(s SealedSecret, previous *Vault) (SealedSecret, error) {
	dek, err := previous.master.Open(nil, s.DEKNonce, s.DEKWrapped, nil)
	if err != nil {
		return SealedSecret{}, fmt.Errorf("%w: data key could not be unwrapped with the previous key", ErrDecrypt)
	}
	defer zero(dek)

	dekNonce, err := newNonce(v.master)
	if err != nil {
		return SealedSecret{}, err
	}
	s.DEKWrapped = v.master.Seal(nil, dekNonce, dek, nil)
	s.DEKNonce = dekNonce
	s.KeyVersion = v.keyVersion
	return s, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: build cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: build GCM: %w", err)
	}
	return aead, nil
}

func newNonce(aead cipher.AEAD) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: generate nonce: %w", err)
	}
	return nonce, nil
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
