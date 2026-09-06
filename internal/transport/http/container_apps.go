package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/deploy"
	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/deployment"
)

// Container apps: installing an application into an LXC container from the
// catalogue the portal ships (ADR 0012).
//
// Not to be confused with published apps, which are Cloudflare hostnames and
// have nothing to do with running software. These endpoints say "container"
// throughout for that reason.

// DeployDeps bundles what the container-app endpoints need. Nil on a portal
// built without the feature, which then reports an empty catalogue rather than
// failing.
type DeployDeps struct {
	Deployer    *deploy.Deployer
	Deployments ports.DeploymentRepository
}

// handleListContainerApps returns the catalogue.
//
// Shipped rather than fetched: the portal's egress is allow-listed to the
// cluster, so it could not ask upstream what is available even if it wanted to.
// What it can do is say which commit the list came from, which is the thing
// worth knowing about a list of programs that will run as root.
func (s *Server) handleListContainerApps(w http.ResponseWriter, r *http.Request) {
	apps := deploy.Search(r.URL.Query().Get("q"), r.URL.Query().Get("tag"))
	WriteJSON(w, http.StatusOK, map[string]any{
		"data": apps,
		"tags": deploy.Tags(),
		"upstream": map[string]string{
			"scripts_repo": deploy.ScriptsRepo, "scripts_ref": deploy.ScriptsRef,
			"engine_repo": deploy.EngineRepo, "engine_ref": deploy.EngineRef,
		},
	})
}

type deploymentResponse struct {
	ID          string          `json:"id"`
	PlatformID  string          `json:"platform_id"`
	Node        string          `json:"node"`
	AppID       string          `json:"app_id"`
	AppName     string          `json:"app_name"`
	CTID        string          `json:"ctid,omitempty"`
	State       string          `json:"state"`
	RequestedBy string          `json:"requested_by,omitempty"`
	Spec        deployment.Spec `json:"spec"`
	ExitCode    *int            `json:"exit_code,omitempty"`
	Error       string          `json:"error,omitempty"`
	// Log is the script's transcript. Only on the detail response: a list of
	// twenty deployments carrying a quarter of a megabyte each would be a
	// several-megabyte page nobody asked to read.
	Log       string `json:"log,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func toDeploymentResponse(d *deployment.Deployment, withLog bool) deploymentResponse {
	out := deploymentResponse{
		ID: d.ID.String(), PlatformID: d.PlatformID.String(), Node: d.Node,
		AppID: d.AppID, AppName: d.AppName, CTID: d.CTID,
		State: string(d.State), RequestedBy: d.RequestedByName,
		Spec: d.Spec, ExitCode: d.ExitCode, Error: d.Error,
		CreatedAt: d.Created.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt: d.Updated.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if withLog {
		out.Log = d.Log
	}
	return out
}

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	if s.deploy.Deployments == nil {
		WriteJSON(w, http.StatusOK, map[string]any{"data": []deploymentResponse{}})
		return
	}
	var platformID uuid.UUID
	if raw := r.URL.Query().Get("platform_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "validation", "That is not a platform id.")
			return
		}
		platformID = id
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	list, err := s.deploy.Deployments.List(r.Context(), platformID, limit)
	if err != nil {
		s.serverError(w, r, err, "The deployments could not be read.")
		return
	}
	out := make([]deploymentResponse, 0, len(list))
	for _, d := range list {
		out = append(out, toDeploymentResponse(d, false))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

// handleGetDeployment returns one deployment with its transcript.
//
// Polled while a deploy runs. It is the first thing in the portal to show the
// output of a non-interactive command on a node: everything before this
// reported a state and a sentence, which cannot explain a script that failed
// two thirds of the way through.
func (s *Server) handleGetDeployment(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "deploymentID")
	if !ok {
		return
	}
	if s.deploy.Deployments == nil {
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested deployment does not exist.")
		return
	}
	d, err := s.deploy.Deployments.Get(r.Context(), id)
	if errors.Is(err, ports.ErrNotFound) {
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested deployment does not exist.")
		return
	}
	if err != nil {
		s.serverError(w, r, err, "The deployment could not be read.")
		return
	}
	WriteJSON(w, http.StatusOK, toDeploymentResponse(d, true))
}

type deployRequestBody struct {
	// AppID is a catalogue identifier and nothing else. It is never a command,
	// a URL or a package, and one the binary does not know is refused before
	// anything is dialled (ADR 0012).
	AppID    string `json:"app_id"`
	Node     string `json:"node"`
	Hostname string `json:"hostname"`
	Cores    int    `json:"cores"`
	MemoryMB int    `json:"memory_mb"`
	DiskGB   int    `json:"disk_gb"`
	Storage  string `json:"storage"`
	Bridge   string `json:"bridge"`
	// Unprivileged is a pointer because not choosing and choosing privileged
	// are different answers, and most of these run unprivileged for a reason.
	Unprivileged *bool `json:"unprivileged"`
}

func (s *Server) handleDeployContainerApp(w http.ResponseWriter, r *http.Request) {
	platformID, ok := s.pathUUID(w, r, "platformID")
	if !ok {
		return
	}
	if s.deploy.Deployer == nil {
		WriteProblem(w, r, http.StatusNotImplemented, "unsupported",
			"This portal cannot deploy container applications.")
		return
	}
	var body deployRequestBody
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	p, _ := PrincipalFrom(r.Context())
	a := s.actor(r)

	rec, err := s.deploy.Deployer.Start(r.Context(), deploy.Input{
		Actor:      deploy.Actor{UserID: a.UserID, Username: a.Username},
		PlatformID: platformID,
		AppID:      body.AppID,
		Node:       body.Node,
		Spec: deployment.Spec{
			Hostname: body.Hostname, Cores: body.Cores, MemoryMB: body.MemoryMB,
			DiskGB: body.DiskGB, Storage: body.Storage, Bridge: body.Bridge,
			Unprivileged: body.Unprivileged,
		},
	})
	if err != nil {
		s.writeDeployError(w, r, err)
		return
	}
	_ = p
	WriteJSON(w, http.StatusAccepted, toDeploymentResponse(rec, false))
}

func (s *Server) writeDeployError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, deploy.ErrUnknownApp):
		WriteProblem(w, r, http.StatusUnprocessableEntity, "validation",
			"That is not an application this portal ships.")
	case errors.Is(err, deploy.ErrUnknownNode):
		WriteProblem(w, r, http.StatusNotFound, "not_found",
			"That node is not part of this platform.")
	case errors.Is(err, deploy.ErrNotPinned):
		WriteProblem(w, r, http.StatusConflict, "node.not_pinned",
			"The portal has not met this node yet. Check the platform's readiness first, which pins its host key.")
	case errors.Is(err, deploy.ErrNoKey):
		WriteProblem(w, r, http.StatusConflict, "node.no_portal_key",
			"The portal has no SSH key of its own. Generate one in Settings → SSH key.")
	case errors.Is(err, connector.ErrInvalidConfig):
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", err.Error(), nil)
	case errors.Is(err, ports.ErrNotFound):
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested platform does not exist.")
	default:
		s.serverError(w, r, err, "The deployment could not be started.")
	}
}
