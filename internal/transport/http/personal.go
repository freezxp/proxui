package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/app/ports"
)

// PersonalDeps bundles the favourites-and-folders endpoints (INV-16…INV-19).
type PersonalDeps struct {
	Personal *command.Personal
}

// These routes are open to any authenticated role rather than gated on one.
// Arranging your own view is not a privilege: a read-only user has as much
// reason to star a machine they watch as an administrator does. What is
// enforced instead, in the command, is that you can only organise a VM you can
// already see.

func (s *Server) handleListFolders(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	folders, err := s.personal.Personal.ListFolders(r.Context(), p.UserID)
	if err != nil {
		s.serverError(w, r, err, "Could not list your folders.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": folders})
}

type folderRequest struct {
	Name     string `json:"name"`
	Position int    `json:"position"`
}

func (s *Server) handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	var req folderRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	folder, err := s.personal.Personal.CreateFolder(r.Context(), p.UserID, req.Name)
	if err != nil {
		s.writePersonalError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, folder)
}

func (s *Server) handleUpdateFolder(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "folderID")
	if !ok {
		return
	}
	p, _ := PrincipalFrom(r.Context())
	var req folderRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if err := s.personal.Personal.RenameFolder(r.Context(), p.UserID, id, req.Name, req.Position); err != nil {
		s.writePersonalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteFolder(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "folderID")
	if !ok {
		return
	}
	p, _ := PrincipalFrom(r.Context())
	if err := s.personal.Personal.DeleteFolder(r.Context(), p.UserID, id); err != nil {
		s.writePersonalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetFavourite(w http.ResponseWriter, r *http.Request) {
	s.setFavourite(w, r, true)
}

func (s *Server) handleClearFavourite(w http.ResponseWriter, r *http.Request) {
	s.setFavourite(w, r, false)
}

func (s *Server) setFavourite(w http.ResponseWriter, r *http.Request, on bool) {
	id, ok := s.pathUUID(w, r, "vmID")
	if !ok {
		return
	}
	p, _ := PrincipalFrom(r.Context())
	if err := s.personal.Personal.SetFavourite(r.Context(), p.UserID, id, p.Role, on); err != nil {
		s.writePersonalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type fileVMRequest struct {
	// FolderID is null to take the VM out of whatever folder it is in.
	FolderID *string `json:"folder_id"`
}

func (s *Server) handleFileVM(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "vmID")
	if !ok {
		return
	}
	p, _ := PrincipalFrom(r.Context())

	var req fileVMRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	folder, ok := parseOptionalUUID(w, r, req.FolderID, "folder_id")
	if !ok {
		return
	}
	if err := s.personal.Personal.FileVMs(r.Context(), p.UserID, p.Role, []uuid.UUID{id}, folder); err != nil {
		s.writePersonalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type fileManyRequest struct {
	VMIDs []string `json:"vm_ids"`
}

// handleFileVMs files several VMs into one folder — the case the feature exists
// for, and the reason it is one request rather than a loop in the browser: half
// of it succeeding is not an outcome anybody asked for.
func (s *Server) handleFileVMs(w http.ResponseWriter, r *http.Request) {
	folderID, ok := s.pathUUID(w, r, "folderID")
	if !ok {
		return
	}
	p, _ := PrincipalFrom(r.Context())

	var req fileManyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	ids := make([]uuid.UUID, 0, len(req.VMIDs))
	for _, raw := range req.VMIDs {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
				"One of the VM identifiers is not valid.",
				map[string]string{"vm_ids": "each must be a UUID"})
			return
		}
		ids = append(ids, parsed)
	}

	if err := s.personal.Personal.FileVMs(r.Context(), p.UserID, p.Role, ids, &folderID); err != nil {
		s.writePersonalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseOptionalUUID reads a nullable id from a request body.
func parseOptionalUUID(w http.ResponseWriter, r *http.Request, raw *string, field string) (*uuid.UUID, bool) {
	if raw == nil || *raw == "" {
		return nil, true
	}
	parsed, err := uuid.Parse(*raw)
	if err != nil {
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation",
			"The identifier is not valid.", map[string]string{field: "must be a UUID"})
		return nil, false
	}
	return &parsed, true
}

func (s *Server) writePersonalError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, command.ErrVMNotVisible), errors.Is(err, ports.ErrNotFound):
		// Indistinguishable on purpose: telling somebody a VM or a folder
		// exists but is not theirs is itself a disclosure (RBAC-05).
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
	case errors.Is(err, command.ErrFolderNameRequired):
		WriteProblemFields(w, r, http.StatusUnprocessableEntity, "validation", err.Error(),
			map[string]string{"name": "a folder needs a name"})
	case errors.Is(err, ports.ErrConflict):
		WriteProblem(w, r, http.StatusConflict, "conflict", err.Error())
	default:
		s.serverError(w, r, err, "Could not update your view of the inventory.")
	}
}
