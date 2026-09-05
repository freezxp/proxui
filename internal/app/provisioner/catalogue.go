package provisioner

import "strings"

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
	Notes     string `json:"notes,omitempty"`
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
	},
	{
		ID:           "rocky-10",
		Name:         "Rocky Linux 10",
		URL:          "https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base.latest.x86_64.qcow2",
		ChecksumURL:  "https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base.latest.x86_64.qcow2.CHECKSUM",
		ChecksumAlgo: "sha256",
		LoginUser:    "rocky",
	},
	{
		ID:           "alma-10",
		Name:         "AlmaLinux 10",
		URL:          "https://repo.almalinux.org/almalinux/10/cloud/x86_64/images/AlmaLinux-10-GenericCloud-latest.x86_64.qcow2",
		ChecksumURL:  "https://repo.almalinux.org/almalinux/10/cloud/x86_64/images/CHECKSUM",
		ChecksumAlgo: "sha256",
		LoginUser:    "almalinux",
	},
}

// Catalogue returns the shipped images.
func Catalogue() []Image {
	out := make([]Image, len(catalogue))
	copy(out, catalogue)
	return out
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
