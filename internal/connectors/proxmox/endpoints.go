package proxmox

import (
	"context"
	"sort"

	"github.com/freezxp/proxui/internal/connector"
)

// certificate filenames pveproxy serves, in the order it prefers them: an
// operator-supplied certificate wins over the cluster-signed one.
var apiCertFiles = []string{"pveproxy-ssl.pem", "pve-ssl.pem"}

// DiscoverEndpoints implements connector.EndpointDiscoverer.
//
// Every member of a Proxmox cluster serves the same API — /cluster/resources
// answers for the whole cluster from any of them, and /nodes/{n}/... is
// proxied to the node that owns the resource. Which means the portal has three
// other ways to ask a question when the address it was configured with stops
// answering, and until ADR 0009 it used none of them.
//
// The addresses come from /cluster/status, which names every member with the
// address the cluster itself uses to reach it. Members the cluster reports as
// offline are left out: a node that is deliberately down should not cost a
// timeout on every failover, and it returns to the list on its own when the
// cluster says it is back.
func (c *Connector) DiscoverEndpoints(ctx context.Context) ([]connector.Endpoint, error) {
	var status []struct {
		Type   string `json:"type"`
		Name   string `json:"name"`
		IP     string `json:"ip"`
		Local  int    `json:"local"`
		Online int    `json:"online"`
	}
	if err := c.client.get(ctx, "/cluster/status", &status); err != nil {
		return nil, err
	}

	pinned := c.cfg.TLS.Mode == connector.TLSFingerprint
	out := make([]connector.Endpoint, 0, len(status))
	for _, s := range status {
		if s.Type != "node" || s.Name == "" || s.IP == "" || s.Online == 0 {
			continue
		}
		ep := connector.Endpoint{Address: s.IP}
		if pinned {
			// Asked through the connection already in use, whose certificate
			// has already been verified — the cluster describing itself over a
			// channel we trust. Reading the certificate from s.IP directly
			// would be trust-on-first-use at the moment the network is least
			// trustworthy, which is the one thing ADR 0009 will not do.
			fp, err := c.nodeCertFingerprint(ctx, s.Name)
			if err != nil || fp == "" {
				// No pin, no candidate. Dropping a member costs a failover
				// option; adding one we cannot verify costs the guarantee that
				// makes a self-signed cluster safe to talk to at all.
				continue
			}
			ep.Fingerprint = fp
		}
		out = append(out, ep)
	}

	// Stable order so a discovery that changes nothing writes nothing.
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out, nil
}

// nodeCertFingerprint reads the certificate a member's API presents, proxied
// through whichever member is currently answering.
func (c *Connector) nodeCertFingerprint(ctx context.Context, node string) (string, error) {
	var certs []struct {
		Filename    string `json:"filename"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := c.client.get(ctx, "/nodes/"+node+"/certificates/info", &certs); err != nil {
		return "", err
	}
	for _, want := range apiCertFiles {
		for _, cert := range certs {
			if cert.Filename != want {
				continue
			}
			fp := normalizeFingerprint(cert.Fingerprint)
			if len(fp) == 64 {
				return fp, nil
			}
		}
	}
	return "", nil
}
