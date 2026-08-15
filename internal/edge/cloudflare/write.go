package cloudflare

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/freezxp/proxui/internal/edge"
)

// ReplaceIngress writes a tunnel's whole routing table.
//
// Replace, not amend — Cloudflare's API has no way to add one rule, and the
// method is named for what it does so no caller mistakes it. Everything the
// caller does not include is deleted, which is why every call site has to have
// read the current table first (PUB-30) and had the result checked against the
// invariants in internal/domain/publish.
func (p *Provider) ReplaceIngress(ctx context.Context, tunnelID string, cfg edge.Config) error {
	if strings.TrimSpace(tunnelID) == "" {
		return edge.Errorf(edge.ErrInvalidConfig, "put_ingress", "a tunnel id is required")
	}
	if len(cfg.Rules) == 0 {
		// Cloudflare requires at least one rule, and an empty table would mean
		// the tunnel serves nothing at all. Refused here rather than sent, so
		// the error names the cause instead of echoing a 400.
		return edge.Errorf(edge.ErrInvalidConfig, "put_ingress",
			"a routing table needs at least a catch-all rule")
	}

	type ingressDTO struct {
		Hostname      string         `json:"hostname,omitempty"`
		Path          string         `json:"path,omitempty"`
		Service       string         `json:"service"`
		OriginRequest map[string]any `json:"originRequest,omitempty"`
	}
	body := struct {
		Config struct {
			Ingress       []ingressDTO   `json:"ingress"`
			OriginRequest map[string]any `json:"originRequest,omitempty"`
		} `json:"config"`
	}{}

	for _, r := range cfg.Rules {
		body.Config.Ingress = append(body.Config.Ingress, ingressDTO{
			Hostname: r.Hostname, Path: r.Path, Service: r.Service,
			// Settings the portal does not understand are written back exactly
			// as they were read. Dropping them would break working apps the
			// portal did not create, silently (PUB-11).
			OriginRequest: r.Origin,
		})
	}
	body.Config.OriginRequest = cfg.Origin

	path := "/accounts/" + p.account + "/cfd_tunnel/" + tunnelID + "/configurations"
	return p.do(ctx, http.MethodPut, path, body, nil)
}

// Zones lists the zones this credential may work with.
func (p *Provider) Zones(ctx context.Context) ([]edge.Zone, error) {
	var dtos []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	// per_page is at Cloudflare's maximum: the alternative is paging, and an
	// account with more than 50 zones would otherwise silently show a subset,
	// which is worse than a slow call.
	if err := p.get(ctx, "/zones?per_page=50", &dtos); err != nil {
		return nil, err
	}
	out := make([]edge.Zone, 0, len(dtos))
	for _, d := range dtos {
		out = append(out, edge.Zone{ID: d.ID, Name: d.Name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// FindDNSRecord looks for a record by exact name.
//
// Used before creating one and before deleting one. Before creating, because
// overwriting somebody's existing record is not this feature's business;
// before deleting, because the portal must only remove the record it made
// (PUB-23).
func (p *Provider) FindDNSRecord(ctx context.Context, zoneID, name string) (edge.DNSRecord, bool, error) {
	var dtos []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Type    string `json:"type"`
		Content string `json:"content"`
		Proxied bool   `json:"proxied"`
	}
	q := url.Values{}
	q.Set("name", name)
	if err := p.get(ctx, "/zones/"+zoneID+"/dns_records?"+q.Encode(), &dtos); err != nil {
		return edge.DNSRecord{}, false, err
	}
	if len(dtos) == 0 {
		return edge.DNSRecord{}, false, nil
	}
	d := dtos[0]
	return edge.DNSRecord{ID: d.ID, Name: d.Name, Type: d.Type, Content: d.Content, Proxied: d.Proxied}, true, nil
}

// TunnelTarget is what a published hostname's CNAME must point at.
func TunnelTarget(tunnelID string) string { return tunnelID + ".cfargotunnel.com" }

// CreateTunnelDNS points a hostname at a tunnel.
func (p *Provider) CreateTunnelDNS(ctx context.Context, zoneID, hostname, tunnelID string) (edge.DNSRecord, error) {
	body := map[string]any{
		"type":    "CNAME",
		"name":    hostname,
		"content": TunnelTarget(tunnelID),
		// Proxied is the point: an unproxied record would publish the
		// cfargotunnel address directly and the tunnel would never be used.
		"proxied": true,
		"comment": "Managed by ProxUI",
	}
	var created struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Type    string `json:"type"`
		Content string `json:"content"`
		Proxied bool   `json:"proxied"`
	}
	if err := p.do(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", body, &created); err != nil {
		return edge.DNSRecord{}, err
	}
	return edge.DNSRecord{
		ID: created.ID, Name: created.Name, Type: created.Type,
		Content: created.Content, Proxied: created.Proxied,
	}, nil
}

// DeleteDNSRecord removes a record by id.
func (p *Provider) DeleteDNSRecord(ctx context.Context, zoneID, recordID string) error {
	if strings.TrimSpace(zoneID) == "" || strings.TrimSpace(recordID) == "" {
		return edge.Errorf(edge.ErrInvalidConfig, "delete_dns", "a zone id and a record id are required")
	}
	return p.do(ctx, http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+recordID, nil, nil)
}
