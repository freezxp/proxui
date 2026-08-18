package crypto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP implements RFC 6238 time-based one-time passwords (AUTH-04).
//
// Written out rather than taken from a library, for one reason: RFC 6238
// publishes test vectors, so the implementation can be pinned to the standard
// itself instead of to a dependency's interpretation of it. It is forty lines
// of HMAC and a modulo; the risk in a dependency here is larger than the risk
// in the arithmetic.
//
// SHA-1 is the algorithm, which is not a mistake. It is what RFC 6238 defines,
// what every authenticator app implements, and its use inside HMAC is not
// affected by the collision attacks that retired SHA-1 for signatures.
const (
	// TOTPPeriod is the length of one time step.
	TOTPPeriod = 30 * time.Second
	// TOTPDigits is the code length an authenticator app will show.
	TOTPDigits = 6
	// TOTPSkew is how many steps either side of now are accepted. One step
	// covers the clock drift and human typing delay that make a correct code
	// arrive late; more would widen the replay window for no real gain.
	TOTPSkew = 1
	// TOTPSecretBytes is the seed length. RFC 4226 requires at least 128 bits
	// and recommends 160, which is also what fits an unpadded base32 string of
	// the length authenticator apps expect.
	TOTPSecretBytes = 20
)

// base32NoPad is the alphabet authenticator apps read, without padding: a
// trailing "=" survives a QR scan badly and is not part of what anyone types.
var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret generates a seed, base32-encoded for display and for the
// otpauth URL.
func NewTOTPSecret() (string, error) {
	buf := make([]byte, TOTPSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate totp secret: %w", err)
	}
	return base32NoPad.EncodeToString(buf), nil
}

// TOTPAt returns the code for a given time.
func TOTPAt(secret string, t time.Time) (string, error) {
	return totpCode(secret, step(t), TOTPDigits)
}

// step is the RFC 6238 counter: whole periods since the Unix epoch.
func step(t time.Time) int64 { return t.Unix() / int64(TOTPPeriod/time.Second) }

// ValidateTOTP checks a code against the steps around now and reports which
// step matched.
//
// The step is returned rather than a bare boolean so the caller can refuse a
// code that has already been used. A TOTP that is merely "correct" is correct
// for up to ninety seconds across the accepted window, and being correct twice
// is what makes a shoulder-surfed code worth anything.
func ValidateTOTP(secret, code string, now time.Time) (matched int64, ok bool) {
	code = strings.TrimSpace(code)
	if len(code) != TOTPDigits || strings.TrimSpace(secret) == "" {
		return 0, false
	}

	current := step(now)
	for offset := -TOTPSkew; offset <= TOTPSkew; offset++ {
		candidate := current + int64(offset)
		expected, err := totpCode(secret, candidate, TOTPDigits)
		if err != nil {
			return 0, false
		}
		// Constant time: a comparison that returns early leaks how much of a
		// guessed code was right, which turns 10^6 guesses into far fewer.
		if subtle.ConstantTimeCompare([]byte(expected), []byte(code)) == 1 {
			return candidate, true
		}
	}
	return 0, false
}

// totpCode is HOTP (RFC 4226) over a time step, with the dynamic truncation
// the RFC specifies. digits is a parameter so the implementation can be tested
// against RFC 6238's published 8-digit vectors.
func totpCode(secret string, counter int64, digits int) (string, error) {
	key, err := decodeSecret(secret)
	if err != nil {
		return "", err
	}

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(counter))

	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	// Dynamic truncation: the low nibble of the last byte picks where to read
	// four bytes from, and the top bit is masked off so the result is positive
	// on implementations without unsigned arithmetic.
	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])

	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, value%mod), nil
}

// decodeSecret accepts what a person might paste back: lower case, padded, or
// broken into the space-separated groups apps display it in.
func decodeSecret(secret string) ([]byte, error) {
	cleaned := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "=", "").Replace(secret))
	key, err := base32NoPad.DecodeString(cleaned)
	if err != nil {
		return nil, fmt.Errorf("decode totp secret: %w", err)
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("decode totp secret: empty")
	}
	return key, nil
}

// OTPAuthURL builds the otpauth:// URI an authenticator app scans.
//
// The issuer appears twice — as a label prefix and as a parameter — which is
// redundant and is what the Key URI format specifies: older apps read only the
// prefix, newer ones only the parameter, and an app that shows the wrong
// portal name is an app whose codes get typed into the wrong login form.
func OTPAuthURL(issuer, account, secret string) string {
	if issuer == "" {
		issuer = "ProxUI"
	}
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(TOTPDigits))
	q.Set("period", fmt.Sprint(int(TOTPPeriod/time.Second)))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// TOTPService adapts these functions to the port the command layer depends on.
type TOTPService struct {
	// Issuer names the portal in an authenticator app's list.
	Issuer string
}

// NewSecret generates a seed.
func (TOTPService) NewSecret() (string, error) { return NewTOTPSecret() }

// Validate checks a code and reports the step it matched.
func (TOTPService) Validate(secret, code string, now time.Time) (int64, bool) {
	return ValidateTOTP(secret, code, now)
}

// URL renders the enrolment URI for an account.
func (s TOTPService) URL(account, secret string) string {
	return OTPAuthURL(s.Issuer, account, secret)
}
