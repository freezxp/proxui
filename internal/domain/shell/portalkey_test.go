package shell_test

import (
	"strings"
	"testing"

	"github.com/freezxp/proxui/internal/domain/shell"
)

const (
	portalLine = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPortalKeyBodyForTests proxui-portal"
	otherLine  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAISomebodyElsesKeyEntirely alice@laptop"
)

func TestSameKeyIgnoresTheComment(t *testing.T) {
	// The comment is what a hand-edited file changes, and changing it must not
	// make removal miss the line it put there.
	renamed := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPortalKeyBodyForTests do-not-delete"
	if !shell.SameKey(portalLine, renamed) {
		t.Fatal("same key body with a different comment should match")
	}
	if shell.SameKey(portalLine, otherLine) {
		t.Fatal("different key bodies must not match")
	}
}

func TestSameKeyLooksPastAnOptionsPrefix(t *testing.T) {
	restricted := `no-pty,from="10.0.0.0/8" ` + portalLine
	if !shell.SameKey(restricted, portalLine) {
		t.Fatal("an options prefix should not hide the key")
	}
}

func TestSameKeyRejectsRubbish(t *testing.T) {
	for _, line := range []string{"", "   ", "# just a comment", "not-a-key at all"} {
		if shell.SameKey(line, portalLine) {
			t.Fatalf("%q should not match a key", line)
		}
	}
}

func TestAppendAuthorizedKeyIsIdempotent(t *testing.T) {
	content := otherLine + "\n" + portalLine + "\n"
	if suffix, needed := shell.AppendAuthorizedKey(content, portalLine); needed || suffix != "" {
		t.Fatalf("installing twice should append nothing, got %q", suffix)
	}
}

func TestAppendAuthorizedKeyFixesAMissingNewline(t *testing.T) {
	// The failure this prevents: appending onto a file whose last line has no
	// terminator concatenates two keys into one unusable line.
	content := otherLine // no trailing newline
	suffix, needed := shell.AppendAuthorizedKey(content, portalLine)
	if !needed {
		t.Fatal("a key that is absent should be appended")
	}
	if !strings.HasPrefix(suffix, "\n") {
		t.Fatalf("suffix must open a new line, got %q", suffix)
	}

	result := content + suffix
	if !shell.HasAuthorizedKey(result, portalLine) {
		t.Fatal("the appended key should be found")
	}
	if !shell.HasAuthorizedKey(result, otherLine) {
		t.Fatal("the key that was already there should survive")
	}
	if !strings.HasSuffix(result, "\n") {
		t.Fatal("the file should end with a newline")
	}
}

func TestAppendAuthorizedKeyOnAnEmptyFile(t *testing.T) {
	suffix, needed := shell.AppendAuthorizedKey("", portalLine)
	if !needed {
		t.Fatal("an empty file needs the key")
	}
	if suffix != portalLine+"\n" {
		t.Fatalf("unexpected suffix %q", suffix)
	}
}

func TestRemoveAuthorizedKeyLeavesEverythingElseAlone(t *testing.T) {
	content := "# my keys\n" + otherLine + "\n" + portalLine + "\n\n"
	out, removed := shell.RemoveAuthorizedKey(content, portalLine)
	if !removed {
		t.Fatal("the portal key should have been removed")
	}
	if shell.HasAuthorizedKey(out, portalLine) {
		t.Fatal("the portal key is still authorized")
	}
	if !shell.HasAuthorizedKey(out, otherLine) {
		t.Fatal("removal took someone else's key with it")
	}
	if !strings.Contains(out, "# my keys") {
		t.Fatal("removal dropped a comment line")
	}
}

func TestRemoveAuthorizedKeyWhenAbsentChangesNothing(t *testing.T) {
	content := otherLine + "\n"
	out, removed := shell.RemoveAuthorizedKey(content, portalLine)
	if removed {
		t.Fatal("nothing should have been removed")
	}
	if out != content {
		t.Fatal("content must be returned byte for byte when nothing matched")
	}
}

func TestHasAuthorizedKeyIgnoresCommentedOutLines(t *testing.T) {
	// A key someone commented out is not authorized, and reporting it as
	// installed would leave an operator with a login that silently fails.
	content := "#" + portalLine + "\n"
	if shell.HasAuthorizedKey(content, portalLine) {
		t.Fatal("a commented-out key must not count as installed")
	}
}
