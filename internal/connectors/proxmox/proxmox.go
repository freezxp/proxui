package proxmox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/freezxp/proxui/internal/connector"
)

// Type is the registry key for this connector.
const Type = "proxmox"

// Version tracks this integration, not the platform.
const Version = "1.0.0"

func init() {
	connector.Register(connector.Info{
		Type:        Type,
		DisplayName: "Proxmox VE",
		Version:     Version,
		Schema:      schema(),
	}, New)
}

// Connector talks to one Proxmox VE cluster.
type Connector struct {
	client *client
	cfg    connector.Config
}

// New builds a Proxmox connector. It satisfies connector.Factory.
func New(cfg connector.Config, creds connector.Credentials, opts connector.Options) (connector.Connector, error) {
	c, err := newClient(cfg, creds, opts)
	if err != nil {
		return nil, err
	}
	return &Connector{client: c, cfg: cfg}, nil
}

// Info implements connector.Connector.
func (c *Connector) Info() connector.Info {
	return connector.Info{Type: Type, DisplayName: "Proxmox VE", Version: Version}
}

// ValidateConfig implements connector.Connector, checking what can be checked
// without touching the network.
func (c *Connector) ValidateConfig(cfg connector.Config) error {
	if cfg.Endpoint == "" {
		return connector.Errorf(connector.ErrInvalidConfig, "config", "endpoint is required")
	}
	if _, err := tlsConfig(cfg.TLS, "placeholder"); err != nil {
		return err
	}
	return nil
}

// Capabilities implements connector.Connector.
func (c *Connector) Capabilities() []connector.Capability {
	return []connector.Capability{
		connector.CapabilityVM,
		connector.CapabilityHost,
		connector.CapabilityStorage,
		connector.CapabilityNetwork,
		connector.CapabilityMetrics,
		connector.CapabilityMetricsBackfill,
		connector.CapabilityConsole,
		connector.CapabilityPower,
		connector.CapabilityNodeAddress,
		connector.CapabilityEndpointDiscovery,
		connector.CapabilityProvision,
		connector.CapabilityDestroy,
		connector.CapabilityTemplateBuild,
		connector.CapabilityNodePrerequisites,
	}
}

// includeTemplates reports whether the operator asked for templates to appear
// in the inventory alongside real guests.
//
// Extra arrives from a jsonb column, so a value an administrator set through
// the form is a bool and one written by hand may be a string. Both are read;
// anything else is the default.
func (c *Connector) includeTemplates() bool {
	switch v := c.cfg.Extra["include_templates"].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1" || v == "yes"
	default:
		return false
	}
}

// Close implements connector.Connector.
func (c *Connector) Close() error {
	for _, t := range c.client.targets {
		t.http.CloseIdleConnections()
	}
	return nil
}

type versionResponse struct {
	Version string `json:"version"`
	Release string `json:"release"`
}

type permissionMap map[string]map[string]int

// TestConnection proves reachability, authentication and privilege in one call
// chain, and reports precisely which privileges are missing. This is what an
// administrator sees when a registration fails, so it is deliberately specific
// rather than a bare "connection failed" (PLAT-02).
func (c *Connector) TestConnection(ctx context.Context) (connector.TestReport, error) {
	report := connector.TestReport{}

	var version versionResponse
	if err := c.client.get(ctx, "/version", &version); err != nil {
		// Auth errors prove we reached the cluster: the distinction matters
		// when an operator is debugging a firewall versus a bad token.
		if errors.Is(err, connector.ErrAuth) || errors.Is(err, connector.ErrPermission) {
			report.Reachable = true
		}
		return report, err
	}
	report.Reachable = true
	report.Authenticated = true
	report.Version = strings.TrimSpace(version.Version + " " + version.Release)

	var nodes []nodeStatus
	if err := c.client.get(ctx, "/nodes", &nodes); err != nil {
		if errors.Is(err, connector.ErrPermission) {
			report.MissingPermissions = append(report.MissingPermissions, "Sys.Audit on /nodes")
			return report, nil
		}
		return report, err
	}
	report.NodeCount = len(nodes)

	// Inspect the granted privileges so the report names what is missing
	// instead of failing later during a sync.
	var perms permissionMap
	if err := c.client.get(ctx, "/access/permissions", &perms); err != nil {
		report.Warnings = append(report.Warnings,
			"could not read token privileges; sync may still fail on individual endpoints")
		return report, nil
	}
	report.MissingPermissions = missingPrivileges(perms)
	if len(report.MissingPermissions) > 0 {
		report.Warnings = append(report.Warnings,
			"the portal will run with reduced functionality until these privileges are granted")
	}

	// Provisioning is reported as a capability rather than a shortfall: a
	// token without these privileges is a correctly configured read-and-console
	// platform, not a broken one (PROV-01, ADR 0010).
	report.MissingProvisioningPrivileges = missingProvisioningPrivileges(perms)
	report.ProvisioningAvailable = len(report.MissingProvisioningPrivileges) == 0

	report.MissingTemplatePrivileges = missingTemplatePrivileges(perms)
	report.TemplateBuildAvailable = len(report.MissingTemplatePrivileges) == 0
	if c.cfg.TLS.Mode == connector.TLSInsecure {
		report.Warnings = append(report.Warnings,
			"TLS verification is disabled for this platform; prefer pinning the certificate fingerprint")
	}
	return report, nil
}

// requiredPrivileges maps a privilege to what stops working without it, so the
// UI can explain the consequence rather than just the name.
var requiredPrivileges = []struct {
	Priv   string
	Needed string
}{
	{"VM.Audit", "listing virtual machines"},
	{"Sys.Audit", "listing nodes and cluster health"},
	{"Datastore.Audit", "listing storage pools"},
	{"VM.Console", "opening consoles"},
	{"VM.PowerMgmt", "power actions"},
	// PVE 9 gates guest-agent queries behind their own privilege (PVE 8 used
	// VM.Monitor). Without it the inventory simply has no IP addresses, which
	// looks like a broken agent unless the report says otherwise.
	{"VM.GuestAgent.Audit", "reading VM IP addresses from the guest agent"},
}

// provisioningPrivileges are what creating and destroying a guest costs
// (ADR 0010).
//
// Kept apart from requiredPrivileges on purpose: these are a capability, not a
// requirement. A token that has never been widened syncs, opens consoles and
// powers guests exactly as before, and TestConnection reports provisioning as
// unavailable rather than reporting the platform as broken. That also means
// this list can only take effect when a human edits the token on the cluster —
// nothing here widens anything.
var provisioningPrivileges = []struct {
	Priv   string
	Needed string
}{
	{"VM.Allocate", "creating and destroying guests"},
	{"VM.Clone", "cloning a template"},
	{"VM.Config.Disk", "sizing the disk"},
	{"VM.Config.CPU", "setting the core count"},
	{"VM.Config.Memory", "setting the memory size"},
	{"VM.Config.Network", "attaching the guest to a bridge"},
	{"VM.Config.Options", "setting boot and agent options"},
	{"VM.Config.Cloudinit", "writing the cloud-init user and SSH keys"},
	{"Datastore.AllocateSpace", "placing the new disk on a storage pool"},
	// Attaching a guest to a bridge. Proxmox states it plainly in create_vm's
	// own description — "if you use a bridge/vlan, you need SDN.Use on any used
	// bridge/vlan" — and enforces it on the config write too, so it is needed
	// by cloud-init configuration as much as by creation. A token without it
	// gets a 403 that names the node path and no privilege at all.
	{"SDN.Use", "attaching a guest to a network bridge"},
}

// templatePrivileges are what building a template costs on top of provisioning.
//
// Kept apart from provisioningPrivileges so the two capabilities are reported
// separately: a platform can perfectly well clone from templates somebody else
// built, and telling an administrator that provisioning is unavailable when
// only template building is would send them to grant more than they need.
//
// Sys.AccessNetwork rather than Sys.Modify: the API source says download-url
// accepts either, and Sys.Modify permits reconfiguring the node, where
// Sys.AccessNetwork is exactly the capability being used — asking the node to
// fetch a URL — and nothing else.
var templatePrivileges = []struct {
	Priv   string
	Needed string
}{
	{"Datastore.AllocateTemplate", "downloading a cloud image onto a storage"},
	{"Sys.AccessNetwork", "asking the node to fetch the image"},
	{"VM.Config.CDROM", "attaching the cloud-init drive"},
	// Required for scsihw, vga and — the one that actually bites — serial0.
	// PVE::API2::Qemu checks it per hardware type when a guest is created, so
	// a token without it gets a bare 403 on POST /nodes/{n}/qemu that names no
	// privilege. Found by building a template against a live cluster; nothing
	// short of that would have shown it.
	{"VM.Config.HWType", "giving the template a serial console and a SCSI controller"},
}

// missingPrivileges reports which required privileges are absent at the root
// path. Proxmox returns an effective privilege map per path.
func missingPrivileges(perms permissionMap) []string {
	return absent(grantedPrivileges(perms), requiredPrivileges)
}

// missingProvisioningPrivileges reports which of the provisioning privileges
// the token lacks. An empty result means the platform can provision.
func missingProvisioningPrivileges(perms permissionMap) []string {
	return absent(grantedPrivileges(perms), provisioningPrivileges)
}

// missingTemplatePrivileges reports which template-building privileges the
// token lacks. Empty means the platform can build templates.
//
// Building is a superset of provisioning rather than a separate set: it creates
// a guest, allocates a disk and attaches a bridge, exactly as provisioning
// does, and then needs four more things on top. Reporting only the four would
// tell an administrator they were one grant away when they were five.
func missingTemplatePrivileges(perms permissionMap) []string {
	granted := grantedPrivileges(perms)
	return append(absent(granted, provisioningPrivileges), absent(granted, templatePrivileges)...)
}

func grantedPrivileges(perms permissionMap) map[string]bool {
	granted := map[string]bool{}
	for path, privs := range perms {
		// Privileges at "/" apply everywhere; deeper paths still let the
		// portal work for the subtree it can see.
		if path != "/" && !strings.HasPrefix(path, "/vms") && !strings.HasPrefix(path, "/nodes") && !strings.HasPrefix(path, "/storage") {
			continue
		}
		for priv, enabled := range privs {
			if enabled == 1 {
				granted[priv] = true
			}
		}
	}
	return granted
}

func absent(granted map[string]bool, want []struct {
	Priv   string
	Needed string
}) []string {
	var missing []string
	for _, req := range want {
		if !granted[req.Priv] {
			missing = append(missing, fmt.Sprintf("%s (needed for %s)", req.Priv, req.Needed))
		}
	}
	return missing
}
