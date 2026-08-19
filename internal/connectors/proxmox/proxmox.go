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
	}
}

// Close implements connector.Connector.
func (c *Connector) Close() error {
	c.client.http.CloseIdleConnections()
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

// missingPrivileges reports which required privileges are absent at the root
// path. Proxmox returns an effective privilege map per path.
func missingPrivileges(perms permissionMap) []string {
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

	var missing []string
	for _, req := range requiredPrivileges {
		if !granted[req.Priv] {
			missing = append(missing, fmt.Sprintf("%s (needed for %s)", req.Priv, req.Needed))
		}
	}
	return missing
}
