package crypto

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238 Appendix B publishes vectors for the SHA-1 variant with the ASCII
// seed "12345678901234567890" and 8 digits. Matching them is the difference
// between an implementation that looks right and one that is right: every
// authenticator app in the world agrees with this table.
func TestRFC6238Vectors(t *testing.T) {
	// The RFC's seed, base32-encoded as the otpauth format carries it.
	secret := base32NoPad.EncodeToString([]byte("12345678901234567890"))

	cases := []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, c := range cases {
		got, err := totpCode(secret, step(time.Unix(c.unix, 0)), 8)
		if err != nil {
			t.Fatalf("t=%d: %v", c.unix, err)
		}
		if got != c.want {
			t.Errorf("t=%d: code = %s, want %s", c.unix, got, c.want)
		}
	}
}

func TestValidateAcceptsTheCurrentCode(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)

	code, err := TOTPAt(secret, now)
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	if len(code) != TOTPDigits {
		t.Fatalf("code %q is %d digits, want %d", code, len(code), TOTPDigits)
	}
	matched, ok := ValidateTOTP(secret, code, now)
	if !ok {
		t.Fatal("the current code was rejected")
	}
	if matched != step(now) {
		t.Errorf("matched step %d, want %d", matched, step(now))
	}
}

// A code arriving a little late must still work — the operator read it off a
// phone and typed it — and the step it matches must be the one it was minted
// for, so replay protection can tell the two apart.
func TestValidateAcceptsOneStepEitherSide(t *testing.T) {
	secret, _ := NewTOTPSecret()
	now := time.Unix(1_700_000_000, 0)

	previous, _ := TOTPAt(secret, now.Add(-TOTPPeriod))
	next, _ := TOTPAt(secret, now.Add(TOTPPeriod))

	if matched, ok := ValidateTOTP(secret, previous, now); !ok || matched != step(now)-1 {
		t.Errorf("previous step: matched=%d ok=%v", matched, ok)
	}
	if matched, ok := ValidateTOTP(secret, next, now); !ok || matched != step(now)+1 {
		t.Errorf("next step: matched=%d ok=%v", matched, ok)
	}
}

func TestValidateRejectsCodesOutsideTheWindow(t *testing.T) {
	secret, _ := NewTOTPSecret()
	now := time.Unix(1_700_000_000, 0)

	for _, offset := range []time.Duration{-2 * TOTPPeriod, 2 * TOTPPeriod, time.Hour} {
		code, _ := TOTPAt(secret, now.Add(offset))
		if _, ok := ValidateTOTP(secret, code, now); ok {
			t.Errorf("a code %v away was accepted", offset)
		}
	}
}

func TestValidateRejectsRubbish(t *testing.T) {
	secret, _ := NewTOTPSecret()
	now := time.Now()
	for _, code := range []string{"", "12345", "1234567", "abcdef", "      "} {
		if _, ok := ValidateTOTP(secret, code, now); ok {
			t.Errorf("%q was accepted as a code", code)
		}
	}
	// A wrong secret must not validate a right-shaped code.
	other, _ := NewTOTPSecret()
	code, _ := TOTPAt(other, now)
	if _, ok := ValidateTOTP(secret, code, now); ok {
		t.Error("a code for a different secret was accepted")
	}
}

// People paste the secret back with the spacing an app displayed it in.
func TestSecretDecodingToleratesHumanFormatting(t *testing.T) {
	secret, _ := NewTOTPSecret()
	now := time.Unix(1_700_000_000, 0)
	code, _ := TOTPAt(secret, now)

	messy := strings.ToLower(secret[:4] + " " + secret[4:8] + "-" + secret[8:] + "==")
	if _, ok := ValidateTOTP(messy, code, now); !ok {
		t.Errorf("a secret formatted as %q was rejected", messy)
	}
}

func TestNewSecretIsLongAndUnique(t *testing.T) {
	first, err := NewTOTPSecret()
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	second, _ := NewTOTPSecret()
	if first == second {
		t.Fatal("two generated secrets were identical")
	}
	decoded, err := decodeSecret(first)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != TOTPSecretBytes {
		t.Errorf("secret is %d bytes, want %d", len(decoded), TOTPSecretBytes)
	}
}

func TestOTPAuthURLCarriesWhatAnAppNeeds(t *testing.T) {
	got := OTPAuthURL("ProxUI", "ada@example.com", "JBSWY3DPEHPK3PXP")

	for _, want := range []string{
		"otpauth://totp/",
		"secret=JBSWY3DPEHPK3PXP",
		"issuer=ProxUI",
		"digits=6",
		"period=30",
		"algorithm=SHA1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("url %q is missing %q", got, want)
		}
	}
	// The label carries the issuer too, for apps that read only that half.
	if !strings.Contains(got, "ProxUI:ada@example.com") {
		t.Errorf("url %q has no issuer-prefixed label", got)
	}
}
