package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/publish"
	"github.com/freezxp/proxui/internal/edge"
)

// PublishDeps bundles the published-app handlers.
type PublishDeps struct {
	Publish   *command.PublishApp
	Unpublish *command.UnpublishApp
	Apps      ports.PublishedAppRepository
}

type publishedAppResponse struct {
	ID         string  `json:"id"`
	ProviderID string  `json:"provider_id"`
	Hostname   string  `json:"hostname"`
	Path       string  `json:"path,omitempty"`
	ServiceURL string  `json:"service_url"`
	VMID       *string `json:"vm_id,omitempty"`
	VMPort     int     `json:"vm_port,omitempty"`
	IsEnabled  bool    `json:"is_enabled"`
	// ManagesDNS says whether unpublishing will remove the DNS record too. It
	// is false for a record the portal adopted rather than created, and the UI
	// needs to say so rather than implying a clean removal.
	ManagesDNS    bool    `json:"manages_dns"`
	URL           string  `json:"url"`
	LastAppliedAt *string `json:"last_applied_at,omitempty"`
	LastError     string  `json:"last_error,omitempty"`
	CreatedAt     string  `json:"created_at"`
}

func toPublishedAppResponse(a *publish.App) publishedAppResponse {
	out := publishedAppResponse{
		ID: a.ID.String(), ProviderID: a.ProviderID.String(),
		Hostname: a.Hostname, Path: a.Path, ServiceURL: a.ServiceURL,
		VMPort: a.VMPort, IsEnabled: a.IsEnabled,
		ManagesDNS: a.DNSRecordID != "",
		URL:        "https://" + a.Hostname + a.Path,
		LastError:  a.LastError,
		CreatedAt:  a.CreatedAt.Format(time.RFC3339),
	}
	if a.VMID != nil {
		id := a.VMID.String()
		out.VMID = &id
	}
	if !a.LastAppliedAt.IsZero() {
		applied := a.LastAppliedAt.Format(time.RFC3339)
		out.LastAppliedAt = &applied
	}
	return out
}

func (s *Server) handleListPublishedApps(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "providerID")
	if !ok {
		return
	}
	apps, err := s.publishing.Apps.ListByProvider(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err, "Could not list published apps.")
		return
	}
	out := make([]publishedAppResponse, 0, len(apps))
	for _, a := range apps {
		out = append(out, toPublishedAppResponse(a))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

type publishAppRequest struct {
	Hostname string `json:"hostname"`
	Path     string `json:"path"`

	// Either a resolved target...
	ServiceURL string `json:"service_url"`
	// ...or an address and port, which is what picking a VM produces.
	Scheme  string `json:"scheme"`
	Address string `json:"address"`
	Port    int    `json:"port"`
	VMID    string `json:"vm_id"`

	OriginRequest map[string]any `json:"origin_request"`

	AcknowledgeExposure bool `json:"acknowledge_exposure"`
	ReadVersion         int  `json:"read_version"`
}

func (s *Server) handlePublishApp(w http.ResponseWriter, r *http.Request) {
	providerID, ok := s.pathUUID(w, r, "providerID")
	if !ok {
		return
	}
	var req publishAppRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	in := command.PublishAppInput{
		Actor: s.actor(r), ProviderID: providerID,
		Hostname: req.Hostname, Path: req.Path,
		ServiceURL: req.ServiceURL, Scheme: req.Scheme, Address: req.Address,
		VMPort: req.Port, OriginRequest: req.OriginRequest,
		AcknowledgeExposure: req.AcknowledgeExposure, ReadVersion: req.ReadVersion,
	}
	if req.VMID != "" {
		vmID, err := uuid.Parse(req.VMID)
		if err != nil {
			WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
				"vm_id is not a valid identifier.", map[string]string{"vm_id": "invalid"})
			return
		}
		in.VMID = &vmID
	}

	app, err := s.publishing.Publish.Handle(r.Context(), in)
	if err != nil {
		s.writePublishError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, toPublishedAppResponse(app))
}

func (s *Server) handleUnpublishApp(w http.ResponseWriter, r *http.Request) {
	appID, ok := s.pathUUID(w, r, "appID")
	if !ok {
		return
	}
	readVersion := 0
	if v := r.URL.Query().Get("read_version"); v != "" {
		// Best-effort: a malformed value means no concurrency check rather
		// than a refusal, and zero already means "not supplied".
		if n, err := strconv.Atoi(v); err == nil {
			readVersion = n
		}
	}

	if err := s.publishing.Unpublish.Handle(r.Context(), appID, s.actor(r), readVersion); err != nil {
		s.writePublishError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// missingWritePermission names the permission the failed call needed.
//
// Publishing writes to two systems with two different permissions, granted on
// two different tabs of Cloudflare's token editor. Telling someone to add both
// when one is already there is the kind of advice that gets ignored, so the
// operation that actually failed decides the wording.
func missingWritePermission(err error) string {
	const nothingChanged = " Nothing was changed."

	var cerr *edge.Error
	if !errors.As(err, &cerr) {
		return "Cloudflare refused the change. The API token appears to lack write permission." + nothingChanged
	}
	switch cerr.Op {
	case "put_ingress":
		return "Cloudflare refused to change the tunnel's routing table. The API token can read " +
			"this account but not configure the tunnel: add **Account → Cloudflare Tunnel : Edit** " +
			"to the token." + nothingChanged
	case "create_dns", "delete_dns", "find_dns":
		return "Cloudflare refused the DNS change. The API token can reach the tunnel but not the " +
			"zone: add **Zone → DNS : Edit** for this zone to the token." + nothingChanged
	default:
		return "Cloudflare refused the change. The API token can read this account but appears to " +
			"lack write permission: publishing needs Account → Cloudflare Tunnel : Edit and " +
			"Zone → DNS : Edit." + nothingChanged
	}
}

// writePublishError maps a publishing failure onto a problem response.
func (s *Server) writePublishError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, publish.ErrExposureNotAcknowledged):
		// Its own code because the UI has to turn it into a confirmation
		// rather than an error message.
		WriteProblem(w, r, http.StatusUnprocessableEntity, "publish.exposure_not_acknowledged",
			"Publishing this hostname makes the service reachable by anyone on the internet. "+
				"Confirm that is intended.")
	case errors.Is(err, publish.ErrStaleRead):
		WriteProblem(w, r, http.StatusConflict, "publish.stale", err.Error())
	case errors.Is(err, publish.ErrSelfRemoved), errors.Is(err, publish.ErrSelfShadowed):
		WriteProblem(w, r, http.StatusUnprocessableEntity, "publish.would_break_portal", err.Error())
	case errors.Is(err, publish.ErrZoneNotAllowed):
		WriteProblem(w, r, http.StatusUnprocessableEntity, "publish.zone_not_allowed", err.Error())
	case errors.Is(err, publish.ErrNoTunnel):
		WriteProblem(w, r, http.StatusUnprocessableEntity, "edge.no_tunnel",
			"This provider has no tunnel selected yet.")
	case errors.Is(err, publish.ErrInvalidHostname), errors.Is(err, publish.ErrInvalidService),
		errors.Is(err, publish.ErrNoCatchAll), errors.Is(err, publish.ErrRuleAfterCatchAll),
		errors.Is(err, publish.ErrDuplicateHostname):
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", err.Error(), nil)
	case errors.Is(err, ports.ErrConflict):
		WriteProblem(w, r, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, ports.ErrNotFound):
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
	case errors.Is(err, edge.ErrAuth), errors.Is(err, edge.ErrPermission):
		// Cloudflare answers 401 "Not authorized" when a token lacks a *write*
		// scope, not only when the token is wrong. Reported as-is, that sends
		// someone to replace a credential that reads perfectly well. Since the
		// portal only got this far by reading successfully, a missing scope is
		// far and away the likelier cause, and it is the one worth naming —
		// specifically, because the two halves of publishing need different
		// permissions and are granted in different places.
		WriteProblem(w, r, http.StatusBadGateway, "publish.write_not_permitted", missingWritePermission(err))
	default:
		s.writeEdgeError(w, r, err, healthResponseBody{})
	}
}
