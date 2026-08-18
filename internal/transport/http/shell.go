package httpapi

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/command"
	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/app/shellreg"
	"github.com/freezxp/proxui/internal/domain/shell"
)

// ShellDeps bundles what the SSH endpoints need.
type ShellDeps struct {
	Open     *command.OpenShell
	Close    *command.CloseShell
	Files    *command.ShellFiles
	Keys     *command.PortalKey
	Sessions ports.ShellRepository
	HostKeys ports.HostKeyStore
	// Bridge serves the terminal WebSocket; an interface so the router can be
	// built in tests without a registry or an sshd.
	Bridge ConsoleBridge
}

// MaxUploadBytes caps a single upload. Large enough for the disk images and
// archives an operator actually moves, small enough that a runaway client
// cannot fill the guest's filesystem in one request.
const MaxUploadBytes = 2 << 30 // 2 GiB

// transferTimeout bounds a file transfer.
//
// The router puts a 30-second timeout on every request, which is right for an
// API call and wrong for moving a gigabyte. These two handlers therefore
// detach from the request context and impose their own limit; the response is
// not bound by the router's timeout because nothing here uses
// http.TimeoutHandler, so the write still lands.
const transferTimeout = 30 * time.Minute

type openShellRequest struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	PrivateKey string `json:"private_key"`
	Passphrase string `json:"passphrase"`
	// UsePortalKey asks for the portal's own key instead of a typed credential
	// (SSH-13). A boolean rather than a key, because the key never leaves the
	// server.
	UsePortalKey  bool   `json:"use_portal_key"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	AcceptHostKey string `json:"accept_host_key"`
}

func (s *Server) handleOpenShell(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "vmID")
	if !ok {
		return
	}
	p, _ := PrincipalFrom(r.Context())

	var req openShellRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	out, err := s.shell.Open.Handle(r.Context(), command.OpenShellInput{
		Actor: s.actor(r), Role: p.Role, VMID: id,
		Username: req.Username, Password: req.Password,
		PrivateKey: req.PrivateKey, Passphrase: req.Passphrase,
		UsePortalKey: req.UsePortalKey,
		Host:         req.Host, Port: req.Port, AcceptHostKey: req.AcceptHostKey,
	})
	if err != nil {
		s.writeShellError(w, r, err)
		return
	}

	// The response carries a ticket and facts about the connection. It never
	// echoes the credential back, not even the username's password field being
	// present (SSH-03).
	WriteJSON(w, http.StatusCreated, map[string]any{
		"session_id":      out.SessionID.String(),
		"ws_url":          "/ws/ssh/" + out.TicketID.String(),
		"expires_in":      int(out.ExpiresIn.Seconds()),
		"address":         out.Address,
		"ssh_user":        out.SSHUser,
		"host_key":        map[string]any{"algorithm": out.HostKey.Algorithm, "fingerprint": out.HostKey.Fingerprint},
		"home":            out.Home,
		"files_available": out.FilesAvailable,
		"files_detail":    out.FilesDetail,
	})
}

// handleShellWS upgrades to the terminal bridge. Authentication happened when
// the ticket was issued: a browser cannot set headers on a WebSocket, so the
// single-use ticket in the path is the credential.
func (s *Server) handleShellWS(w http.ResponseWriter, r *http.Request) {
	if s.shell.Bridge == nil {
		http.Error(w, "ssh terminals are not enabled", http.StatusServiceUnavailable)
		return
	}
	ticketID := chi.URLParam(r, "ticketID")
	if ticketID == "" {
		http.Error(w, "missing terminal ticket", http.StatusBadRequest)
		return
	}
	s.shell.Bridge.ServeHTTP(w, r, ticketID)
}

func (s *Server) handleCloseShell(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	actor := s.actor(r)
	p, _ := PrincipalFrom(r.Context())

	if err := s.shell.Close.Handle(r.Context(), actor, p.Role, id); err != nil {
		s.writeShellError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListShellSessions(w http.ResponseWriter, r *http.Request) {
	activeOnly := r.URL.Query().Get("active") == "true"
	sessions, err := s.shell.Sessions.List(r.Context(), activeOnly, 100)
	if err != nil {
		s.serverError(w, r, err, "Could not list SSH sessions.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": sessions})
}

// handleForgetHostKey drops a VM's pinned key.
//
// Without this a rebuilt guest is unreachable forever: its new key will never
// match, and refusing a changed key is deliberately not something an operator
// can click past (SSH-04). Un-pinning is therefore an administrator's decision,
// made away from the connect form and recorded.
func (s *Server) handleForgetHostKey(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "vmID")
	if !ok {
		return
	}
	previous, err := s.shell.HostKeys.Get(r.Context(), id)
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		s.serverError(w, r, err, "Could not read the pinned host key.")
		return
	}
	if err := s.shell.HostKeys.Forget(r.Context(), id); err != nil {
		s.serverError(w, r, err, "Could not forget the pinned host key.")
		return
	}
	s.auditShell(r, "ssh_host_key_forgotten", id,
		map[string]any{"fingerprint": previous.Fingerprint})
	w.WriteHeader(http.StatusNoContent)
}

// --- file browser (SSH-09) ----------------------------------------------

func (s *Server) handleListShellFiles(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	dir, entries, err := s.shell.Files.List(r.Context(), id, s.actor(r).UserID,
		r.URL.Query().Get("path"))
	if err != nil {
		s.writeShellError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"path":   dir,
		"parent": parentOf(dir),
		"data":   entries,
	})
}

func (s *Server) handleDownloadShellFile(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), transferTimeout)
	defer cancel()

	target := r.URL.Query().Get("path")
	body, size, err := s.shell.Files.Download(ctx, s.actor(r), id, target)
	if err != nil {
		s.writeShellError(w, r, err)
		return
	}
	defer body.Close()

	name := path.Base(target)
	// Content-Disposition with a quoted filename, and octet-stream regardless
	// of extension: a file fetched from a guest must never be rendered by the
	// browser, or an uploaded .html becomes a page served from the portal's
	// own origin.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := io.Copy(w, body); err != nil {
		s.log.Warn().Err(err).Str("path", target).Msg("download interrupted")
	}
}

func (s *Server) handleUploadShellFile(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	dir := r.URL.Query().Get("path")
	name := r.URL.Query().Get("name")
	if dir == "" || name == "" {
		WriteProblem(w, r, http.StatusBadRequest, "ssh.bad_request",
			"Both a directory and a file name are required.")
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), transferTimeout)
	defer cancel()

	// The body is streamed straight to the guest rather than buffered: a
	// gigabyte upload must not become a gigabyte of portal memory.
	body := http.MaxBytesReader(w, r.Body, MaxUploadBytes)
	defer body.Close()

	target, written, err := s.shell.Files.Upload(ctx, s.actor(r), id, dir, name, body)
	if err != nil {
		s.writeShellError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, map[string]any{"path": target, "bytes": written})
}

type shellPathRequest struct {
	Path string `json:"path"`
	Name string `json:"name"`
	To   string `json:"to"`
	Mode string `json:"mode"`
}

func (s *Server) handleShellMkdir(w http.ResponseWriter, r *http.Request) {
	id, req, ok := s.shellFileRequest(w, r)
	if !ok {
		return
	}
	target, err := s.shell.Files.Mkdir(r.Context(), s.actor(r), id, req.Path, req.Name)
	if err != nil {
		s.writeShellError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, map[string]any{"path": target})
}

func (s *Server) handleShellRename(w http.ResponseWriter, r *http.Request) {
	id, req, ok := s.shellFileRequest(w, r)
	if !ok {
		return
	}
	if err := s.shell.Files.Rename(r.Context(), s.actor(r), id, req.Path, req.To); err != nil {
		s.writeShellError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleShellChmod(w http.ResponseWriter, r *http.Request) {
	id, req, ok := s.shellFileRequest(w, r)
	if !ok {
		return
	}
	if err := s.shell.Files.Chmod(r.Context(), s.actor(r), id, req.Path, req.Mode); err != nil {
		s.writeShellError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteShellFile(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return
	}
	if err := s.shell.Files.Remove(r.Context(), s.actor(r), id, r.URL.Query().Get("path")); err != nil {
		s.writeShellError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) shellFileRequest(w http.ResponseWriter, r *http.Request) (uuid.UUID, shellPathRequest, bool) {
	id, ok := s.pathUUID(w, r, "sessionID")
	if !ok {
		return uuid.Nil, shellPathRequest{}, false
	}
	var req shellPathRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return uuid.Nil, shellPathRequest{}, false
	}
	return id, req, true
}

func parentOf(dir string) string {
	if dir == "/" || dir == "" {
		return ""
	}
	return path.Dir(dir)
}

// writeShellError maps the SSH failures onto the answers an operator can act
// on. The distinctions matter more here than anywhere else in the API: "wrong
// password", "unreachable" and "the host key changed" look identical from a
// browser and demand completely different responses.
func (s *Server) writeShellError(w http.ResponseWriter, r *http.Request, err error) {
	var prompt *command.HostKeyPrompt
	var changed *command.HostKeyChanged

	switch {
	case errors.As(err, &prompt):
		// 409 rather than 401: nothing is wrong with the request, it simply
		// cannot proceed until a human confirms the fingerprint.
		WriteProblemWithBody(w, r, http.StatusConflict, "ssh.host_key_unknown",
			"This VM's SSH host key has not been trusted yet. Check the fingerprint before accepting it.",
			map[string]any{
				"address":     prompt.Address,
				"algorithm":   prompt.Algorithm,
				"fingerprint": prompt.Fingerprint,
			})
	case errors.As(err, &changed):
		WriteProblemWithBody(w, r, http.StatusConflict, "ssh.host_key_mismatch",
			"This VM is presenting a different SSH host key than the one trusted earlier. "+
				"If the guest was rebuilt, an administrator can forget the old key; otherwise stop and investigate.",
			map[string]any{
				"address":       changed.Address,
				"expected":      changed.Expected,
				"got":           changed.Got,
				"first_seen_at": changed.FirstSeenAt,
			})
	case errors.Is(err, shell.ErrNotPermitted), errors.Is(err, ports.ErrNotFound):
		// Same response for "no such VM" and "not yours".
		WriteProblem(w, r, http.StatusNotFound, "not_found", "The requested resource does not exist.")
	case errors.Is(err, shell.ErrSessionNotFound):
		WriteProblem(w, r, http.StatusNotFound, "ssh.session_gone",
			"That SSH session is no longer open.")
	case errors.Is(err, shell.ErrNotRunning):
		WriteProblem(w, r, http.StatusConflict, "ssh.not_running",
			"This VM is not running, so nothing is listening for SSH.")
	case errors.Is(err, shell.ErrNoAddress):
		WriteProblem(w, r, http.StatusUnprocessableEntity, "ssh.no_address",
			"The portal knows no usable address for this VM. Install the guest agent, or wait for the next sync.")
	case errors.Is(err, shell.ErrCredentialMissing):
		WriteProblem(w, r, http.StatusBadRequest, "ssh.credential_required",
			"A username and either a password or a private key are required.")
	case errors.Is(err, shell.ErrNoPortalKey):
		WriteProblem(w, r, http.StatusConflict, "ssh.no_portal_key",
			"The portal has no SSH key of its own yet. An administrator can generate one in Settings.")
	case errors.Is(err, shell.ErrBadAuthorizedKeys):
		WriteProblem(w, r, http.StatusUnprocessableEntity, "ssh.bad_authorized_keys",
			"The authorized_keys file on the guest could not be read: "+err.Error())
	case errors.Is(err, shell.ErrBadKey):
		WriteProblem(w, r, http.StatusBadRequest, "ssh.bad_key", err.Error())
	case errors.Is(err, shell.ErrAuthFailed):
		WriteProblem(w, r, http.StatusUnauthorized, "ssh.auth_failed",
			"The guest rejected those credentials.")
	case errors.Is(err, shell.ErrUnreachable):
		WriteProblem(w, r, http.StatusBadGateway, "ssh.unreachable",
			"The guest could not be reached on its SSH port: "+err.Error())
	case errors.Is(err, shell.ErrFilesUnavailable):
		WriteProblem(w, r, http.StatusUnprocessableEntity, "ssh.no_sftp",
			"This guest does not offer SFTP, so files cannot be browsed. The terminal still works.")
	case errors.Is(err, shell.ErrBadPath):
		WriteProblem(w, r, http.StatusBadRequest, "ssh.bad_path", err.Error())
	case errors.Is(err, fs.ErrNotExist):
		WriteProblem(w, r, http.StatusNotFound, "ssh.no_such_file", "No such file or directory on the guest.")
	case errors.Is(err, fs.ErrPermission):
		WriteProblem(w, r, http.StatusForbidden, "ssh.permission_denied",
			"The account you signed in as on the guest may not do that.")
	case errors.Is(err, fs.ErrExist):
		WriteProblem(w, r, http.StatusConflict, "ssh.already_exists", "That path already exists on the guest.")
	case errors.Is(err, shellreg.ErrTooMany):
		WriteProblem(w, r, http.StatusTooManyRequests, "ssh.too_many_sessions",
			"You already have as many open SSH sessions as the portal allows. Close one and try again.")
	default:
		s.serverError(w, r, err, "Could not complete that SSH operation.")
	}
}

func (s *Server) auditShell(r *http.Request, action string, vmID uuid.UUID, details map[string]any) {
	if s.admin.Audit == nil {
		return
	}
	actor := s.actor(r)
	actorID := actor.UserID
	_ = s.admin.Audit.Write(r.Context(), ports.AuditEntry{
		Time: s.clock(), ActorUserID: &actorID, ActorName: actor.Username,
		Category: command.AuditCategoryShell, Action: action,
		TargetType: "vm", TargetID: vmID.String(),
		SourceIP: actor.IP, UserAgent: actor.UserAgent, RequestID: actor.RequestID,
		Outcome: ports.OutcomeSuccess, Details: details,
	})
}
