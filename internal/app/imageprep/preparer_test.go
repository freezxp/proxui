package imageprep

import (
	"strings"
	"testing"
)

// The one command that changes a template's disk, asserted directly: it runs as
// root on a hypervisor against an image every future guest is cloned from, and
// there is no cheaper place to notice that a flag has gone missing.
func TestCustomizeCommand(t *testing.T) {
	cmd := customizeCommand("/dev/pve/base-149-disk-0")

	for _, want := range []struct{ flag, why string }{
		{"--install qemu-guest-agent",
			"without the agent a guest reports no address, and an address is what the portal needs before it can offer SSH"},
		{"--truncate /etc/machine-id",
			"every clone would otherwise share one, and a DHCP server that keys on it hands them all the same lease"},
		{"--delete '/etc/ssh/ssh_host_*'",
			"each clone must generate its own rather than present an identity its siblings also have"},
		{"--delete /var/lib/cloud/instance",
			"cloud-init must run on the clone's first boot rather than believe it already has"},
	} {
		if !strings.Contains(cmd, want.flag) {
			t.Errorf("customizeCommand is missing %s — %s\ngot: %s", want.flag, want.why, cmd)
		}
	}

	// The disk is named once, and nothing else is: this command is built from
	// constants and one path the caller already validated.
	if strings.Count(cmd, "/dev/pve/base-149-disk-0") != 1 {
		t.Errorf("the disk should appear exactly once: %s", cmd)
	}
}

// The volume identifier is the only thing here that travels through a database
// row, so it is the only thing that could carry a shell metacharacter onto a
// hypervisor.
func TestVolumeIDPattern(t *testing.T) {
	for _, ok := range []string{
		"local-lvm:vm-149-disk-0",
		"Datastore-SSD1:vm-148-disk-0",
		"local:149/vm-149-disk-0.qcow2",
	} {
		if !volumeIDPattern.MatchString(ok) {
			t.Errorf("%q should be a valid volume identifier", ok)
		}
	}
	for _, bad := range []string{
		"local-lvm:vm-1; reboot",
		"local-lvm:$(reboot)",
		"local-lvm:vm-1 && rm -rf /",
		"local-lvm:vm-1`id`",
		"no-colon",
		"",
	} {
		if volumeIDPattern.MatchString(bad) {
			t.Errorf("%q must not be accepted as a volume identifier", bad)
		}
	}
}
