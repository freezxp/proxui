package proxmox

import "github.com/freezxp/proxui/internal/connector"

// nodePrerequisites is what a Proxmox node needs beyond the API, and how to
// check and install each one (ADR 0011).
//
// Two entries, and both exist for the same reason: Proxmox has no API for what
// they do. A temperature is not published anywhere in the API, and there is no
// endpoint for changing the contents of a disk. So the portal goes to the node,
// and the node has to have the tool.
//
// Neither is a hard requirement, which is exactly why they are worth reporting.
// Nothing fails without them — a chart has no line on it, or a guest arrives
// with no agent and therefore no address — so before this list existed each was
// discovered by somebody noticing something missing weeks later.
//
// Every string here is a constant. Nothing a request carries is interpolated
// into any of them; a request names an ID and the server looks it up.
var nodePrerequisites = []connector.NodePrerequisite{
	{
		ID:       "lm-sensors",
		Name:     "lm-sensors",
		Needed:   "reading node temperatures, which Proxmox publishes nowhere in its API",
		Probe:    "command -v sensors",
		Packages: []string{"lm-sensors"},
		// sensors-detect after the package, because installing lm-sensors
		// alone is usually not enough: the hwmon drivers for the board have to
		// be loaded, and until they are `sensors` runs and prints nothing —
		// which looks exactly like hardware with no sensors. docs/30 tells an
		// operator to run these two commands by hand for the same reason.
		//
		// The detection is best-effort and the installation is not: a node
		// whose chips nothing recognises has still had lm-sensors installed
		// correctly, and reporting that as a failure would be wrong. A refresh
		// of the package lists is best-effort too — a node with the enterprise
		// repository configured and no subscription fails `apt-get update`
		// every time, and has done so long before the portal turned up.
		Install: "DEBIAN_FRONTEND=noninteractive apt-get update >/dev/null 2>&1; " +
			"DEBIAN_FRONTEND=noninteractive apt-get install -y lm-sensors && " +
			"{ sensors-detect --auto >/dev/null 2>&1; /etc/init.d/kmod start >/dev/null 2>&1; true; }",
	},
	{
		ID:       "libguestfs-tools",
		Name:     "libguestfs-tools",
		Needed:   "installing a guest agent into a template's disk, without which every guest cloned from it reports no IP address",
		Probe:    "command -v virt-customize",
		Packages: []string{"libguestfs-tools"},
		Install: "DEBIAN_FRONTEND=noninteractive apt-get update >/dev/null 2>&1; " +
			"DEBIAN_FRONTEND=noninteractive apt-get install -y libguestfs-tools",
	},
}

// NodePrerequisites implements connector.NodePrerequisiteLister.
//
// The list is returned by value each time rather than shared, so a caller that
// modifies what it is given cannot change what the next caller is told.
func (c *Connector) NodePrerequisites() []connector.NodePrerequisite {
	out := make([]connector.NodePrerequisite, len(nodePrerequisites))
	copy(out, nodePrerequisites)
	return out
}
