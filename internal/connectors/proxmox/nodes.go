package proxmox

import (
	"context"
)

// NodeAddresses implements connector.NodeAddresser.
//
// /cluster/status is the one call that names every node with the address the
// cluster itself uses to reach it, which is the address a sensor collector
// wants: it is on the management network, it is what the nodes already trust
// each other on, and it does not depend on DNS the portal may not share.
//
// A standalone node has no cluster endpoint. Its own name is still in the
// nodes list, so it falls back to the address the API is being reached at —
// which for a single-node install is the node.
func (c *Connector) NodeAddresses(ctx context.Context) (map[string]string, error) {
	var status []struct {
		Type   string `json:"type"`
		Name   string `json:"name"`
		IP     string `json:"ip"`
		Local  int    `json:"local"`
		Online int    `json:"online"`
	}
	out := map[string]string{}
	if err := c.client.get(ctx, "/cluster/status", &status); err == nil {
		for _, s := range status {
			if s.Type == "node" && s.Name != "" && s.IP != "" {
				out[s.Name] = s.IP
			}
		}
	}
	if len(out) > 0 {
		return out, nil
	}

	// Standalone: one node, reachable where the API is.
	var nodes []struct {
		Node string `json:"node"`
	}
	if err := c.client.get(ctx, "/nodes", &nodes); err != nil {
		return nil, err
	}
	host := c.client.base.Hostname()
	if host == "" {
		return out, nil
	}
	for _, n := range nodes {
		if n.Node != "" {
			out[n.Node] = host
		}
	}
	return out, nil
}
