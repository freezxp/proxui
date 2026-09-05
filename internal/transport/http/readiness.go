package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/freezxp/proxui/internal/app/nodecheck"
)

// Platform readiness: what a platform's nodes need, and installing what the
// portal can (ADR 0011).
//
// Two questions in one place because an operator is asking one: is this
// platform going to be able to do what I expect of it? Half the answer is on
// the nodes — lm-sensors, libguestfs-tools, the portal's key — and half is in
// the credential's own privileges, and neither half is any use on its own.

type readinessResponse struct {
	// PortalKey is whether the portal has an SSH key at all; without one no
	// node can be reached and the reason is the same for all of them.
	PortalKey bool                   `json:"portal_key"`
	Nodes     []nodecheck.NodeReport `json:"nodes"`
	// Privileges is what the platform credential can and cannot do. Reported
	// here because until now it was built by POST /platforms/test and thrown
	// away, and that endpoint only works before a platform is saved — a
	// configured platform had nowhere to show it (ADR 0010, ADR 0011).
	Privileges privilegeReport `json:"privileges"`
}

type privilegeReport struct {
	Missing                []string `json:"missing"`
	ProvisioningAvailable  bool     `json:"provisioning_available"`
	MissingProvisioning    []string `json:"missing_provisioning"`
	TemplateBuildAvailable bool     `json:"template_build_available"`
	MissingTemplate        []string `json:"missing_template"`
	Warnings               []string `json:"warnings,omitempty"`
}

// handleReadiness reports what every node of a platform has and is missing.
//
// On demand, from a button, rather than on every page load: the answer needs an
// SSH handshake per node and changes about once a year. The failure it addresses
// is not that prerequisites change but that nobody knew about them until
// something quietly stopped working.
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "platformID")
	if !ok {
		return
	}
	platform, err := s.platforms.Platforms.Get(r.Context(), id)
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

	out := readinessResponse{Nodes: []nodecheck.NodeReport{}}

	// The privileges half first: it is one API call, and a platform whose
	// credential cannot even be used is worth saying before four SSH
	// handshakes.
	if report, err := conn.TestConnection(r.Context()); err == nil {
		out.Privileges = privilegeReport{
			Missing:                orEmpty(report.MissingPermissions),
			ProvisioningAvailable:  report.ProvisioningAvailable,
			MissingProvisioning:    orEmpty(report.MissingProvisioningPrivileges),
			TemplateBuildAvailable: report.TemplateBuildAvailable,
			MissingTemplate:        orEmpty(report.MissingTemplatePrivileges),
			Warnings:               report.Warnings,
		}
	} else {
		out.Privileges.Warnings = []string{"the platform's privileges could not be read: " + err.Error()}
	}

	if s.platforms.Nodes != nil {
		report, err := s.platforms.Nodes.Check(r.Context(), id, conn)
		if err != nil {
			s.serverError(w, r, err, "The nodes could not be checked.")
			return
		}
		out.PortalKey = report.PortalKey
		if report.Nodes != nil {
			out.Nodes = report.Nodes
		}
	}
	WriteJSON(w, http.StatusOK, out)
}

type installRequest struct {
	// Prerequisite is an identifier, never a package. The server maps it to a
	// command it compiled in and refuses one it does not recognise, which is
	// what keeps this a menu rather than a shell (ADR 0011).
	Prerequisite string `json:"prerequisite"`
}

// handleInstallPrerequisite installs one thing on one node.
//
// Answers as soon as the work has started: apt-get takes minutes and this API's
// request deadline is thirty seconds. There is nothing to resume and nothing to
// lose across a restart — checking again asks the node, which is the only
// authority on whether the tool is there. Who asked and what happened is in the
// audit trail under `node.install`.
func (s *Server) handleInstallPrerequisite(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "platformID")
	if !ok {
		return
	}
	node := chi.URLParam(r, "node")
	var body installRequest
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	if s.platforms.Nodes == nil {
		WriteProblem(w, r, http.StatusNotImplemented, "unsupported",
			"This portal cannot install anything on a node.")
		return
	}

	platform, err := s.platforms.Platforms.Get(r.Context(), id)
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

	a := s.actor(r)
	rec, err := s.platforms.Nodes.Install(r.Context(), id, conn, node, body.Prerequisite,
		nodecheck.Actor{
			UserID: a.UserID, Username: a.Username,
			IP: a.IP, UserAgent: a.UserAgent, RequestID: a.RequestID,
		})
	if err != nil {
		s.writeInstallError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusAccepted, rec)
}

func (s *Server) writeInstallError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, nodecheck.ErrUnknownPrerequisite):
		WriteProblem(w, r, http.StatusUnprocessableEntity, "validation",
			"That is not something this portal knows how to install.")
	case errors.Is(err, nodecheck.ErrNotInstallable):
		WriteProblem(w, r, http.StatusUnprocessableEntity, "validation",
			"This has to be done on the node by hand; the portal cannot install it.")
	case errors.Is(err, nodecheck.ErrUnknownNode):
		WriteProblem(w, r, http.StatusNotFound, "not_found",
			"That node is not part of this platform.")
	case errors.Is(err, nodecheck.ErrNotPinned):
		WriteProblem(w, r, http.StatusConflict, "node.not_pinned",
			"The portal has not met this node yet. Check readiness first, which pins its host key.")
	case errors.Is(err, nodecheck.ErrNoKey):
		WriteProblem(w, r, http.StatusConflict, "node.no_portal_key",
			"The portal has no SSH key of its own. Generate one in Settings → SSH key.")
	case errors.Is(err, nodecheck.ErrAlreadyRunning):
		WriteProblem(w, r, http.StatusConflict, "node.install_running",
			"That installation is already running on this node.")
	default:
		s.serverError(w, r, err, "The installation could not be started.")
	}
}

// orEmpty keeps an absent list out of the response as [] rather than null, so
// the UI does not have to distinguish "nothing missing" from "not checked".
func orEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
