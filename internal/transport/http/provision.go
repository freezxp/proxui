package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/app/provisioner"
	appsync "github.com/freezxp/proxui/internal/app/sync"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/provision"
)

// ProvisionDeps bundles what the create-and-destroy endpoints need (ADR 0010).
type ProvisionDeps struct {
	Provision *command.Provision
	Destroy   *command.Destroy
	Build     *command.BuildTemplate
	Requests  ports.ProvisionRepository
	Platforms ports.PlatformRepository
	Sync      *appsync.Service
}

type templateResponse struct {
	ExternalID   string `json:"external_id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	Node         string `json:"node"`
	DiskBytes    int64  `json:"disk_bytes"`
	HasCloudInit bool   `json:"has_cloud_init"`
}

// handleListTemplates lists what a platform can clone from.
//
// Read live rather than from inventory: templates are deliberately excluded
// from the synced VM tables, so there is nothing stored to read.
func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "platformID")
	if !ok {
		return
	}
	platform, err := s.provision.Platforms.Get(r.Context(), id)
	if err != nil {
		s.writePlatformError(w, r, err)
		return
	}
	conn, err := s.provision.Sync.Connect(r.Context(), platform)
	if err != nil {
		s.serverError(w, r, err, "Could not reach the platform.")
		return
	}
	defer conn.Close()

	p, supported := conn.(connector.Provisioner)
	if !supported {
		WriteJSON(w, http.StatusOK, map[string]any{"data": []templateResponse{}})
		return
	}
	templates, err := p.ListTemplates(r.Context())
	if err != nil {
		s.writeProvisionError(w, r, err)
		return
	}

	out := make([]templateResponse, 0, len(templates))
	for _, t := range templates {
		out = append(out, templateResponse{
			ExternalID: t.ExternalID, Name: t.Name, Type: t.Type, Node: t.HostID,
			DiskBytes: t.DiskBytes, HasCloudInit: t.HasCloudInit,
		})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

// provisionRequestBody is what an administrator submits.
//
// There is no password field, and there is not meant to be one: cloud-init
// takes a user and SSH keys, and nothing else the portal would have to carry
// (PROV-04, ADR 0005).
type provisionRequestBody struct {
	TemplateID string   `json:"template_id"`
	Name       string   `json:"name"`
	Node       string   `json:"node"`
	Storage    string   `json:"storage"`
	FullClone  bool     `json:"full_clone"`
	VMGroupID  string   `json:"vm_group_id"`
	CIUser     string   `json:"ci_user"`
	SSHKeys    []string `json:"ssh_keys"`
	IPConfig   string   `json:"ip_config"`
	Nameserver string   `json:"nameserver"`
	SearchDom  string   `json:"search_domain"`
	Cores      int      `json:"cores"`
	MemoryMB   int      `json:"memory_mb"`
	Bridge     string   `json:"bridge"`
	VLAN       int      `json:"vlan"`
	DiskName   string   `json:"disk_name"`
	DiskGrowGB int      `json:"disk_grow_gb"`
	StartAfter bool     `json:"start_after_create"`
	StartOnBt  bool     `json:"start_on_boot"`
	Upgrade    *bool    `json:"upgrade_packages"`
}

func (s *Server) handleProvision(w http.ResponseWriter, r *http.Request) {
	platformID, ok := s.pathUUID(w, r, "platformID")
	if !ok {
		return
	}
	p, _ := PrincipalFrom(r.Context())

	var body provisionRequestBody
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}

	var group *uuid.UUID
	if body.VMGroupID != "" {
		parsed, err := uuid.Parse(body.VMGroupID)
		if err != nil {
			WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
				"The VM group is not a valid identifier.",
				map[string]string{"vm_group_id": "must be a UUID"})
			return
		}
		group = &parsed
	}

	out, err := s.provision.Provision.Handle(r.Context(), command.ProvisionInput{
		Actor: s.actor(r), Role: p.Role, PlatformID: platformID,
		TemplateID: body.TemplateID, Name: body.Name, TargetNode: body.Node,
		VMGroupID: group,
		Spec: provision.Spec{
			TemplateNode: body.Node,
			FullClone:    body.FullClone,
			Storage:      body.Storage,
			CIUser:       body.CIUser,
			SSHKeys:      body.SSHKeys,
			IPConfig:     body.IPConfig,
			Nameserver:   body.Nameserver,
			SearchDomain: body.SearchDom,
			Cores:        body.Cores,
			MemoryMB:     body.MemoryMB,
			Bridge:       body.Bridge,
			VLAN:         body.VLAN,
			DiskName:     body.DiskName,
			// Submitted in gigabytes because that is the unit an operator
			// thinks in; stored in bytes because that is what the platform
			// takes and what nothing has to reinterpret later.
			DiskGrowBytes:    int64(body.DiskGrowGB) << 30,
			UpgradePackages:  body.Upgrade,
			StartOnBoot:      body.StartOnBt,
			StartAfterCreate: body.StartAfter,
		},
	})
	if err != nil {
		s.writeProvisionError(w, r, err)
		return
	}

	// 202: a request has been recorded and queued. The guest does not exist
	// yet, and how far along it is comes from polling the request.
	WriteJSON(w, http.StatusAccepted, map[string]any{
		"request_id": out.RequestID.String(), "state": out.State,
	})
}

type destroyRequestBody struct {
	// ConfirmName must match the guest's name. Checked on the server, because a
	// confirmation a client could skip is not a control.
	ConfirmName string `json:"confirm_name"`
}

func (s *Server) handleDestroyVM(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "vmID")
	if !ok {
		return
	}
	p, _ := PrincipalFrom(r.Context())

	var body destroyRequestBody
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}

	out, err := s.provision.Destroy.Handle(r.Context(), command.DestroyInput{
		Actor: s.actor(r), Role: p.Role, VMID: id, ConfirmName: body.ConfirmName,
	})
	if err != nil {
		s.writeProvisionError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{
		"request_id": out.RequestID.String(), "state": out.State,
	})
}

type provisionRequestResponse struct {
	ID          string `json:"id"`
	PlatformID  string `json:"platform_id"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	Step        string `json:"step,omitempty"`
	GuestName   string `json:"guest_name"`
	VMID        string `json:"vmid,omitempty"`
	TemplateID  string `json:"template_id,omitempty"`
	Node        string `json:"node,omitempty"`
	RequestedBy string `json:"requested_by,omitempty"`
	Error       string `json:"error,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func toProvisionResponse(r *provision.Request) provisionRequestResponse {
	return provisionRequestResponse{
		ID: r.ID.String(), PlatformID: r.PlatformID.String(),
		Kind: string(r.Kind), State: string(r.State), Step: r.Step,
		GuestName: r.GuestName, VMID: r.VMID, TemplateID: r.TemplateExternalID,
		Node: r.TargetNode, RequestedBy: r.RequestedByName, Error: r.Error,
		CreatedAt: r.Created.Format(timeFormat), UpdatedAt: r.Updated.Format(timeFormat),
	}
}

func (s *Server) handleListProvisionRequests(w http.ResponseWriter, r *http.Request) {
	var platformID uuid.UUID
	if raw := r.URL.Query().Get("platform_id"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
				"The platform filter is not a valid identifier.",
				map[string]string{"platform_id": "must be a UUID"})
			return
		}
		platformID = parsed
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	requests, err := s.provision.Requests.ListRequests(r.Context(), platformID, limit)
	if err != nil {
		s.serverError(w, r, err, "Could not list provisioning requests.")
		return
	}
	out := make([]provisionRequestResponse, 0, len(requests))
	for _, req := range requests {
		out = append(out, toProvisionResponse(req))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) handleGetProvisionRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "requestID")
	if !ok {
		return
	}
	req, err := s.provision.Requests.GetRequest(r.Context(), id)
	if errors.Is(err, ports.ErrNotFound) {
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
		return
	}
	if err != nil {
		s.serverError(w, r, err, "Could not read the provisioning request.")
		return
	}
	WriteJSON(w, http.StatusOK, toProvisionResponse(req))
}

func (s *Server) writeProvisionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, command.ErrProvisionNotPermitted):
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
	case errors.Is(err, command.ErrNameMismatch):
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
			"The confirmation does not match the guest's name.",
			map[string]string{"confirm_name": "must exactly match the name of the guest being destroyed"})
	case errors.Is(err, command.ErrTemplateProtected):
		WriteProblem(w, r, http.StatusConflict, "template_protected",
			"Templates cannot be destroyed through the portal. Every guest cloned from one depends on it.")
	case errors.Is(err, ports.ErrNotFound):
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
	case errors.Is(err, connector.ErrNotSupported):
		// Almost always a token that was never widened, which is a
		// configuration an administrator chose rather than a fault (PROV-01).
		WriteProblem(w, r, http.StatusConflict, "platform.not_capable",
			"This platform cannot create or destroy guests. Its API token needs the provisioning privileges.")
	case errors.Is(err, connector.ErrPermission):
		WriteProblem(w, r, http.StatusBadGateway, "platform.permission_denied",
			"The platform credential is not allowed to perform this action.")
	case errors.Is(err, connector.ErrInvalidConfig):
		WriteProblem(w, r, http.StatusUnprocessableEntity, "validation", err.Error())
	case errors.Is(err, connector.ErrRefused):
		WriteProblem(w, r, http.StatusConflict, "platform.refused", err.Error())
	case errors.Is(err, connector.ErrUnreachable):
		WriteProblem(w, r, http.StatusBadGateway, "platform.unreachable",
			"The platform could not be reached.")
	default:
		s.serverError(w, r, err, "The request could not be carried out.")
	}
}

// handleImageCatalogue lists the cloud images the portal knows where to find.
//
// The portal cannot look one up on an operator's behalf — its egress is
// allow-listed to the cluster (docs/15 §15.4) and it is the node that fetches —
// so this is the whole of the help it can offer: where the image lives, where
// its digest is published, and what the login user is called.
func (s *Server) handleImageCatalogue(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{"data": provisioner.Catalogue()})
}

type buildTemplateBody struct {
	Name         string `json:"name"`
	Node         string `json:"node"`
	ImageURL     string `json:"image_url"`
	ImageStorage string `json:"image_storage"`
	DiskStorage  string `json:"disk_storage"`
	Checksum     string `json:"checksum"`
	ChecksumAlgo string `json:"checksum_algo"`
	// SkipChecksum must be stated. A blank digest alone is not enough: the
	// difference between deciding not to verify and forgetting to is exactly
	// what this field records, and what the audit entry reports.
	SkipChecksum bool   `json:"skip_checksum"`
	Cores        int    `json:"cores"`
	MemoryMB     int    `json:"memory_mb"`
	Bridge       string `json:"bridge"`
}

func (s *Server) handleBuildTemplate(w http.ResponseWriter, r *http.Request) {
	platformID, ok := s.pathUUID(w, r, "platformID")
	if !ok {
		return
	}
	p, _ := PrincipalFrom(r.Context())

	var body buildTemplateBody
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}

	out, err := s.provision.Build.Handle(r.Context(), command.BuildTemplateInput{
		Actor: s.actor(r), Role: p.Role, PlatformID: platformID,
		Name: body.Name, Node: body.Node,
		ImageURL: body.ImageURL, ImageFile: provisioner.ImageFilename(body.ImageURL),
		ImageStorage: body.ImageStorage, DiskStorage: body.DiskStorage,
		Checksum: body.Checksum, ChecksumAlgo: body.ChecksumAlgo,
		SkipChecksum: body.SkipChecksum,
		Cores:        body.Cores, MemoryMB: body.MemoryMB, Bridge: body.Bridge,
	})
	if err != nil {
		s.writeProvisionError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{
		"request_id": out.RequestID.String(), "state": out.State,
	})
}
