package crypto

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	h := NewPasswordHasher()
	hash, err := h.Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("hash = %q, want argon2id PHC format", hash)
	}

	ok, err := h.Verify("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Errorf("Verify(correct) = %v, %v; want true, nil", ok, err)
	}

	ok, err = h.Verify("wrong password entirely", hash)
	if err != nil || ok {
		t.Errorf("Verify(wrong) = %v, %v; want false, nil", ok, err)
	}
}

func TestPasswordHashesAreSalted(t *testing.T) {
	h := NewPasswordHasher()
	a, _ := h.Hash("same password twice")
	b, _ := h.Hash("same password twice")
	if a == b {
		t.Error("identical passwords produced identical hashes; salt is missing")
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	h := NewPasswordHasher()
	for _, bad := range []string{"", "plaintext", "$argon2i$v=19$m=1,t=1,p=1$AAAA$BBBB", "$argon2id$bad"} {
		if _, err := h.Verify("x", bad); !errors.Is(err, ErrInvalidHash) {
			t.Errorf("Verify(_, %q) error = %v, want ErrInvalidHash", bad, err)
		}
	}
}

func TestOpaqueTokenHashing(t *testing.T) {
	token, hash, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	if len(token) < 40 {
		t.Errorf("token length = %d, want >= 40 chars of entropy", len(token))
	}
	if string(HashToken(token)) != string(hash) {
		t.Error("HashToken() disagrees with the hash returned at creation")
	}

	other, _, _ := NewOpaqueToken()
	if other == token {
		t.Error("two generated tokens collided")
	}
}

func newTestIssuer(t *testing.T) *TokenIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return NewTokenIssuer(key, "proxui-test", 15*time.Minute)
}

func TestAccessTokenRoundTrip(t *testing.T) {
	issuer := newTestIssuer(t)
	userID, sessionID := uuid.New(), uuid.New()

	token, ttl, err := issuer.Issue(userID, "operator", sessionID, time.Now())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if ttl != 15*time.Minute {
		t.Errorf("ttl = %v, want 15m", ttl)
	}

	claims, err := issuer.Parse(token)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if claims.Subject != userID.String() {
		t.Errorf("sub = %q, want %q", claims.Subject, userID)
	}
	if claims.Role != "operator" {
		t.Errorf("role = %q, want operator", claims.Role)
	}
	if claims.SessionID != sessionID.String() {
		t.Errorf("sid = %q, want %q", claims.SessionID, sessionID)
	}
}

func TestParseRejectsExpiredToken(t *testing.T) {
	issuer := newTestIssuer(t)
	token, _, err := issuer.Issue(uuid.New(), "admin", uuid.New(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if _, err := issuer.Parse(token); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Parse(expired) error = %v, want ErrInvalidToken", err)
	}
}

func TestParseRejectsForeignSignature(t *testing.T) {
	attacker := newTestIssuer(t)
	victim := newTestIssuer(t)

	token, _, _ := attacker.Issue(uuid.New(), "admin", uuid.New(), time.Now())
	if _, err := victim.Parse(token); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Parse(foreign signature) error = %v, want ErrInvalidToken", err)
	}
}

func TestParseRejectsAlgNone(t *testing.T) {
	// The classic JWT confusion attack: an unsigned token claiming admin.
	claims := Claims{Role: "admin", RegisteredClaims: jwt.RegisteredClaims{
		Subject:   uuid.NewString(),
		Issuer:    "proxui-test",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}}
	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("build unsigned token: %v", err)
	}
	if _, err := newTestIssuer(t).Parse(unsigned); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Parse(alg=none) error = %v, want ErrInvalidToken", err)
	}
}

func TestLoadOrCreateRSAKeyPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jwt.pem")

	first, err := LoadOrCreateRSAKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateRSAKey() error = %v", err)
	}
	second, err := LoadOrCreateRSAKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateRSAKey() reload error = %v", err)
	}
	if !first.Equal(second) {
		t.Error("reloading the key file produced a different key; sessions would break on restart")
	}
}
