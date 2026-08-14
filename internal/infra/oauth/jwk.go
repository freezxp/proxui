package oauth

import (
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"math/big"
)

// rsaPublicKey rebuilds an RSA key from a JWK's modulus and exponent.
//
// Written out rather than pulled from a JWK library: it is fifteen lines, and
// a dependency whose only job is base64-decoding two integers is a dependency
// with more attack surface than value.
func rsaPublicKey(modulusB64, exponentB64 string) (*rsa.PublicKey, error) {
	modulus, err := base64.RawURLEncoding.DecodeString(modulusB64)
	if err != nil {
		return nil, fmt.Errorf("decode modulus: %w", err)
	}
	exponent, err := base64.RawURLEncoding.DecodeString(exponentB64)
	if err != nil {
		return nil, fmt.Errorf("decode exponent: %w", err)
	}
	if len(modulus) == 0 || len(exponent) == 0 {
		return nil, fmt.Errorf("empty key material")
	}

	e := new(big.Int).SetBytes(exponent)
	if !e.IsInt64() || e.Int64() > 1<<31-1 || e.Int64() < 3 {
		return nil, fmt.Errorf("implausible public exponent")
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(modulus),
		E: int(e.Int64()),
	}, nil
}
