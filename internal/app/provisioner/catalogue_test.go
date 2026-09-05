package provisioner

// The catalogue is a set of claims about other people's URLs, so what is worth
// testing is the part the portal is responsible for: that every entry can
// actually be stored, and that a name Proxmox would refuse is refused here
// first, with a message that says which extensions are the right ones.

import (
	"strings"
	"testing"
)

// Ubuntu publishes a qcow2 file called .img, which Proxmox refuses for an
// imported disk. Every shipped entry has to survive that check, or the
// catalogue is offering images that cannot be built.
func TestEveryCatalogueEntryCanBeStored(t *testing.T) {
	entries := Catalogue()
	if len(entries) == 0 {
		t.Fatal("the catalogue is empty")
	}
	for _, img := range entries {
		if img.Filename == "" {
			t.Errorf("%s: no filename to store under", img.ID)
		}
		if err := ValidateImageFilename(img.Filename); err != nil {
			t.Errorf("%s: %v", img.ID, err)
		}
		if img.URL == "" || img.ChecksumURL == "" {
			t.Errorf("%s: an entry with no image or no checksum to check it against", img.ID)
		}
		if img.LoginUser == "" {
			t.Errorf("%s: no login user, which is the thing people guess wrong", img.ID)
		}
	}
}

func TestUbuntuIsStoredUnderTheExtensionThatDescribesIt(t *testing.T) {
	for _, img := range Catalogue() {
		if img.ID != "ubuntu-24.04" {
			continue
		}
		if !strings.HasSuffix(img.URL, ".img") {
			t.Skip("upstream changed the published extension")
		}
		if !strings.HasSuffix(img.Filename, ".qcow2") {
			t.Errorf("stored as %q; the published .img is qcow2 and Proxmox will not take .img",
				img.Filename)
		}
	}
}

func TestValidateImageFilenameNamesWhatIsAccepted(t *testing.T) {
	for _, ok := range []string{
		"debian-13-generic-amd64.qcow2", "disk.raw", "appliance.ova", "image.VMDK",
	} {
		if err := ValidateImageFilename(ok); err != nil {
			t.Errorf("%q was refused: %v", ok, err)
		}
	}

	for _, bad := range []string{"", "   ", "noble-server-cloudimg-amd64.img", "image.iso", "no-extension"} {
		err := ValidateImageFilename(bad)
		if err == nil {
			t.Errorf("%q was accepted; the platform would refuse it", bad)
			continue
		}
		if bad != "" && strings.TrimSpace(bad) != "" && !strings.Contains(err.Error(), ".qcow2") {
			t.Errorf("%q: message does not say what is accepted: %v", bad, err)
		}
	}

	// A path would let a caller write outside the storage's own directory.
	if err := ValidateImageFilename("../../etc/passwd.qcow2"); err == nil {
		t.Error("a path was accepted as a filename")
	}
}

func TestImageFilenameHandlesAwkwardURLs(t *testing.T) {
	for url, want := range map[string]string{
		"https://x/y/debian-13.qcow2":      "debian-13.qcow2",
		"https://x/y/image.qcow2?sig=abc":  "image.qcow2",
		"https://x/y/image.qcow2#fragment": "image.qcow2",
		"https://x/y/":                     "y",
		"https://x/":                       "x",
	} {
		if got := ImageFilename(url); got != want {
			t.Errorf("ImageFilename(%q) = %q, want %q", url, got, want)
		}
	}
	if got := ImageFilename(""); got == "" {
		t.Error("an empty URL produced an empty filename the platform would reject")
	}
}
