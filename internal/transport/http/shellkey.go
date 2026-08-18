package httpapi

import (
	"errors"
	"net/http"

	"github.com/freezxp/proxui/internal/domain/shell"
)

// The portal's SSH key endpoints (SSH-11..SSH-14, ADR 0006).
//
// Two rules shape every response here. The private half never appears in one -
// it exists in the vault and in the argument to a dial, nowhere else. And the
// public half is not treated as a secret at all: an operator has to be able to
// read it to paste it into cloud-init, and a public key that cannot be copied
// is a public key that gets installed by a worse route.

// keyResponse renders the key for a settings page. "Not generated yet" is a
// normal state rather than an error, so it comes back as a 200 saying so - a
// page that has to distinguish a 404 meaning "no key" from a 404 meaning "no
// such endpoint" is a page that will get it wrong.
func keyResponse(key shell.PortalKey, exists bool) map[string]any {
	if !exists {
		return map[string]any{"exists": false}
	}
	return map[string]any{
		"exists":      true,
		"public_key":  key.PublicKey,
		"algorithm":   key.Algorithm,
		"fingerprint": key.Fingerprint,
		"created_at":  key.CreatedAt,
	}
}

func (s *Server) handleGetPortalKey(w http.ResponseWriter, r *http.Request) {
	key, err := s.shell.Keys.View(r.Context())
	if err != nil {
		if errors.Is(err, shell.ErrNoPortalKey) {
			WriteJSON(w, http.StatusOK, keyResponse(shell.PortalKey{}, false))
			return
		}
		s.serverError(w, r, err, "Could not read the portal's SSH key.")
		return
	}
	WriteJSON(w, http.StatusOK, keyResponse(key, true))
}

// handleGeneratePortalKey creates the key, or rotates it if one exists.
//
// One endpoint for both because they are one operation: there is a single
// portal key, and asking for it to be generated when one is already there can
// only mean replacing it. The response says which happened by way of the
// fingerprint, and the audit trail names it explicitly.
func (s *Server) handleGeneratePortalKey(w http.ResponseWriter, r *http.Request) {
	key, err := s.shell.Keys.Generate(r.Context(), s.actor(r))
	if err != nil {
		s.serverError(w, r, err, "Could not generate an SSH key for the portal.")
		return
	}
	WriteJSON(w, http.StatusCreated, keyResponse(key, true))
}

func (s *Server) handleDeletePortalKey(w http.ResponseWriter, r *http.Request) {
	if err := s.shell.Keys.Delete(r.Context(), s.actor(r)); err != nil {
		s.writeShellError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListKeyInstalls answers "where does this key open a door", which is
// the question to ask before rotating or deleting it.
func (s *Server) handleListKeyInstalls(w http.ResponseWriter, r *http.Request) {
	installs, err := s.shell.Keys.Installs(r.Context())
	if err != nil {
		s.serverError(w, r, err, "Could not list where the portal's SSH key is installed.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": installs})
}

// handleVMKeyInstalls tells the connect form whether the key will work here,
// and for which accounts.
func (s *Server) handleVMKeyInstalls(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "vmID")
	if !ok {
		return
	}
	p, _ := PrincipalFrom(r.Context())

	installs, err := s.shell.Keys.InstallsForVM(r.Context(), s.actor(r), p.Role, id)
	if err != nil {
		s.writeShellError(w, r, err)
		return
	}

	// The fingerprint of the current key rides along so the form can say "the
	// key on this box is not the one the portal now holds" without a second
	// request.
	body := map[string]any{"data": installs, "key_exists": false}
	if key, err := s.shell.Keys.View(r.Context()); err == nil {
		body["key_exists"] = true
		body["fingerprint"] = key.Fingerprint
	}
	WriteJSON(w, http.StatusOK, body)
}

// handleInstallPortalKey adds the key to authorized_keys over an open session.
func (s *Server) handleInstallPortalKey(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	install, err := s.shell.Keys.Install(r.Context(), s.actor(r), id)
	if err != nil {
		s.writeShellError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, install)
}

// handleRemovePortalKey takes the key back out of authorized_keys.
func (s *Server) handleRemovePortalKey(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	if err := s.shell.Keys.Uninstall(r.Context(), s.actor(r), id); err != nil {
		s.writeShellError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
