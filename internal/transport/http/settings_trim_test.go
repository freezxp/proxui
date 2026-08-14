package httpapi

import "testing"

// A credential copied out of a web console picks up trailing whitespace more
// often than not, and the resulting failure points at the wrong thing: Google
// answers "the OAuth client was not found" for a client that is correct apart
// from one invisible character.
func TestSettingTextIsNormalizedBeforeStorage(t *testing.T) {
	const want = "1036600526065-glthi7rvjnjhih8pabdfqrc2m5av2kf3.apps.googleusercontent.com"

	cases := map[string]string{
		"trailing space":     want + " ",
		"leading space":      " " + want,
		"newline":            want + "\n",
		"tab":                "\t" + want + "\t",
		"non-breaking space": want + "\u00a0",
		"zero width space":   want + "\u200b",
		"byte order mark":    "\ufeff" + want,
		"already clean":      want,
	}
	for name, given := range cases {
		if got := normalizeSettingText(given); got != want {
			t.Errorf("%s: normalizeSettingText(%q) = %q", name, given, got)
		}
	}
}

// Whitespace inside a value is left alone: a sign-in notice is a sentence.
func TestNormalizationLeavesTheInsideAlone(t *testing.T) {
	const notice = "Authorised users only.  Access is logged."
	if got := normalizeSettingText("  " + notice + "  "); got != notice {
		t.Errorf("got %q, want the notice with its internal spacing intact", got)
	}
}
