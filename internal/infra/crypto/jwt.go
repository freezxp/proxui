package crypto

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// ErrInvalidToken covers every reason an access token is unusable; callers must
// not distinguish them for clients (that would leak validation detail).
var ErrInvalidToken = errors.New("crypto: invalid access token")

// Claims is the access-token payload. Role is embedded so authorization needs
// no database round trip, while SessionID lets us revoke immediately by
// checking a small Redis set.
type Claims struct {
	Role     string `json:"role"`
	Username string `json:"preferred_username"`
	// SessionID lets a token be revoked immediately by checking a small set,
	// rather than waiting for it to expire.
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

// TokenIssuer signs and verifies RS256 access tokens.
type TokenIssuer struct {
	private *rsa.PrivateKey
	public  *rsa.PublicKey
	issuer  string
	ttl     time.Duration
}

// NewTokenIssuer builds an issuer from an RSA private key.
func NewTokenIssuer(key *rsa.PrivateKey, issuer string, ttl time.Duration) *TokenIssuer {
	return &TokenIssuer{private: key, public: &key.PublicKey, issuer: issuer, ttl: ttl}
}

// LoadOrCreateRSAKey reads a PEM private key from path, generating and
// persisting one on first run so a fresh install needs no key ceremony.
func LoadOrCreateRSAKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("crypto: %s does not contain a PEM block", path)
		}
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("crypto: parse %s: %w", path, err)
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("crypto: %s is not an RSA private key", path)
		}
		return rsaKey, nil
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("crypto: read %s: %w", path, err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("crypto: generate signing key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: marshal signing key: %w", err)
	}
	// 0600: the signing key is as sensitive as every session it protects.
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, fmt.Errorf("crypto: write %s: %w", path, err)
	}
	return key, nil
}

// Issue signs an access token for a user session.
func (t *TokenIssuer) Issue(userID uuid.UUID, role, username string, sessionID uuid.UUID, now time.Time) (string, time.Duration, error) {
	claims := Claims{
		Role:      role,
		Username:  username,
		SessionID: sessionID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    t.issuer,
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(t.ttl)),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(t.private)
	if err != nil {
		return "", 0, fmt.Errorf("crypto: sign access token: %w", err)
	}
	return signed, t.ttl, nil
}

// Parse verifies a token's signature, algorithm, issuer and expiry.
func (t *TokenIssuer) Parse(token string) (*Claims, error) {
	parsed, err := jwt.ParseWithClaims(token, &Claims{},
		func(tok *jwt.Token) (any, error) {
			// Pin the algorithm: accepting alg from the token is the classic
			// JWT confusion vulnerability.
			if _, ok := tok.Method.(*jwt.SigningMethodRSA); !ok {
				return nil, fmt.Errorf("unexpected signing method %v", tok.Header["alg"])
			}
			return t.public, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithIssuer(t.issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidToken, err)
	}
	claims, ok := parsed.Claims.(*Claims)
	if !ok || !parsed.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}
