package provisioner

import (
	"errors"
	"fmt"
	"strings"
)

// The image catalogue (ADR 0010).
//
// A short list of images an operator would otherwise have to go and find. The
// portal cannot look them up on their behalf — its egress is allow-listed to
// the cluster (docs/15 §15.4), and it is the node that fetches — so what the
// catalogue offers is knowing where to look and what the login user is called,
// which is the part people get wrong.
//
// It deliberately carries **no checksums**. A point release moves and a
// hardcoded digest is wrong within weeks; a digest that is wrong gets skipped,
// and an operator who has skipped one once skips the next. What it carries
// instead is the URL of the distribution's own checksum file, which is always
// right, so the digest that gets pasted is one somebody actually read.

// Image is one entry an operator can start from.
type Image struct {
	ID string `json:"id"`
	// Name is what the image is, as a person would say it.
	Name string `json:"name"`
	URL  string `json:"url"`
	// ChecksumURL is where the distribution publishes the digest for this file.
	ChecksumURL string `json:"checksum_url"`
	// ChecksumAlgo is what that file contains, so the form can preselect it.
	ChecksumAlgo string `json:"checksum_algo"`
	// LoginUser is the account cloud-init configures by default. It differs per
	// distribution and is the first thing that goes wrong when guessed.
	LoginUser string `json:"login_user"`
	// Filename is the name the image is stored under, which is not always the
	// name it is published under. Proxmox accepts only .ova, .qcow2, .raw and
	// .vmdk for imported disks, and Ubuntu publishes a file called .img that
	// is in fact qcow2 — so it is stored under the extension that describes
	// what it is. Empty means the published name is already fine.
	Filename string `json:"filename"`
	// CPU is the processor model the guest has to be given, when the default
	// will not boot it. RHEL 10 and everything rebuilt from it — AlmaLinux 10,
	// Rocky 10 — are compiled for x86-64-v3, and their glibc aborts before
	// init runs on anything less. Empty means the default is fine.
	CPU   string `json:"cpu,omitempty"`
	Notes string `json:"notes,omitempty"`
}

// catalogue is the shipped list. Kept small on purpose: every entry is a claim
// that a URL is still right, and a long list is a long list of things to be
// wrong about.
var catalogue = []Image{
	{
		ID:           "debian-13",
		Name:         "Debian 13 (trixie)",
		URL:          "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-generic-amd64.qcow2",
		ChecksumURL:  "https://cloud.debian.org/images/cloud/trixie/latest/SHA512SUMS",
		ChecksumAlgo: "sha512",
		LoginUser:    "debian",
	},
	{
		ID:           "debian-12",
		Name:         "Debian 12 (bookworm)",
		URL:          "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-generic-amd64.qcow2",
		ChecksumURL:  "https://cloud.debian.org/images/cloud/bookworm/latest/SHA512SUMS",
		ChecksumAlgo: "sha512",
		LoginUser:    "debian",
	},
	{
		ID:           "ubuntu-24.04",
		Name:         "Ubuntu 24.04 LTS (noble)",
		URL:          "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img",
		ChecksumURL:  "https://cloud-images.ubuntu.com/noble/current/SHA256SUMS",
		ChecksumAlgo: "sha256",
		LoginUser:    "ubuntu",
		// Published as .img, which Proxmox refuses for an imported disk. The
		// file is qcow2 — `qemu-img info` on the URL says so — so this is the
		// honest name for it rather than a rename to get past a check.
		Filename: "noble-server-cloudimg-amd64.qcow2",
	},
	{
		ID:           "rocky-10",
		Name:         "Rocky Linux 10",
		URL:          "https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base.latest.x86_64.qcow2",
		ChecksumURL:  "https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base.latest.x86_64.qcow2.CHECKSUM",
		ChecksumAlgo: "sha256",
		LoginUser:    "rocky",
		CPU:          "x86-64-v3",
		Notes:        "needs an x86-64-v3 host: Haswell, Zen, or newer",
	},
	{
		ID:           "alma-10",
		Name:         "AlmaLinux 10",
		URL:          "https://repo.almalinux.org/almalinux/10/cloud/x86_64/images/AlmaLinux-10-GenericCloud-latest.x86_64.qcow2",
		ChecksumURL:  "https://repo.almalinux.org/almalinux/10/cloud/x86_64/images/CHECKSUM",
		ChecksumAlgo: "sha256",
		LoginUser:    "almalinux",
		// Found the hard way: with Proxmox's default CPU the guest prints
		// "Fatal glibc error: CPU does not support x86-64-v3" and panics
		// before init, which from outside is indistinguishable from a guest
		// whose agent will not start.
		CPU:   "x86-64-v3",
		Notes: "needs an x86-64-v3 host: Haswell, Zen, or newer",
	},
}

// Catalogue returns the shipped images, each with the name it would be stored
// under filled in, so a caller never has to work out which entries override it.
func Catalogue() []Image {
	out := make([]Image, len(catalogue))
	copy(out, catalogue)
	for i := range out {
		if out[i].Filename == "" {
			out[i].Filename = ImageFilename(out[i].URL)
		}
	}
	return out
}

// importExtensions are what Proxmox accepts for a disk image it will import.
// The list is PVE::Storage's UPLOAD_IMPORT_EXT_RE_1, and a name outside it is
// refused before anything is downloaded.
var importExtensions = []string{".qcow2", ".raw", ".vmdk", ".ova"}

// ValidateImageFilename reports why a name would be refused, naming what is
// accepted — Proxmox's own message is "invalid filename or wrong extension",
// which does not say which extensions are the right ones.
func ValidateImageFilename(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("the image needs a filename to be stored under")
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("%q is a path, not a filename", name)
	}
	lower := strings.ToLower(name)
	for _, ext := range importExtensions {
		if strings.HasSuffix(lower, ext) {
			return nil
		}
	}
	return fmt.Errorf("%q must end in one of %s — a cloud image published as .img is usually qcow2, "+
		"and is stored under the extension that describes it",
		name, strings.Join(importExtensions, ", "))
}

// ImageFilename derives the name a downloaded image is stored under.
//
// The last path segment, which is what every one of these publishers uses and
// what an operator would recognise in a storage listing. A URL that ends in a
// slash or carries a query string gets a safe fallback rather than an empty
// filename the platform would reject.
func ImageFilename(rawURL string) string {
	trimmed := rawURL
	if i := strings.IndexAny(trimmed, "?#"); i >= 0 {
		trimmed = trimmed[:i]
	}
	trimmed = strings.TrimSuffix(trimmed, "/")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		trimmed = trimmed[i+1:]
	}
	if trimmed == "" {
		return "cloud-image.img"
	}
	return trimmed
}
