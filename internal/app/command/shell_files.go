package command

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/app/shellreg"
	"github.com/freezxp/proxui/internal/domain/shell"
)

// ShellFiles serves the file browser over an already-open SSH session (SSH-09).
//
// Every method starts by resolving the session for this caller, which is where
// ownership is enforced: a session id is a bearer of nothing, and the fact that
// one exists must not let another signed-in user read files through it
// (SSH-10).
//
// Listing and stat are not audited. A browser panel lists a directory on every
// click, and an audit trail that fills with "looked at /var" buries the entries
// that matter. What changed and what left the machine is audited: uploads,
// downloads, deletions, renames and mode changes.
type ShellFiles struct {
	Registry *shellreg.Registry
	Audit    ports.AuditWriter
	Clock    ports.Clock
}

// liveFS resolves a caller's own live session and its file subsystem.
//
// The ownership check and the idle-timer touch belong together: every path that
// reaches a guest's filesystem goes through here, so there is one place for
// SSH-10 to be enforced rather than one per command.
func liveFS(reg *shellreg.Registry, clock ports.Clock, sessionID, owner uuid.UUID) (*shellreg.Live, ports.RemoteFS, error) {
	live, err := reg.Get(sessionID, owner)
	if err != nil {
		return nil, nil, err
	}
	fs, err := live.Files()
	if err != nil {
		return nil, nil, err
	}
	live.Touch(clock.Now())
	return live, fs, nil
}

func (h *ShellFiles) open(sessionID, owner uuid.UUID) (*shellreg.Live, ports.RemoteFS, error) {
	return liveFS(h.Registry, h.Clock, sessionID, owner)
}

// List reads a directory.
func (h *ShellFiles) List(ctx context.Context, sessionID, owner uuid.UUID, dir string) (string, []shell.FileEntry, error) {
	_, fs, err := h.open(sessionID, owner)
	if err != nil {
		return "", nil, err
	}
	if dir == "" {
		if home, err := fs.Home(ctx); err == nil && home != "" {
			dir = home
		} else {
			dir = "/"
		}
	}
	clean, err := shell.CleanPath(dir)
	if err != nil {
		return "", nil, err
	}
	entries, err := fs.List(ctx, clean)
	if err != nil {
		return "", nil, err
	}
	return clean, entries, nil
}

// Download opens a file for reading and records that it left the machine.
func (h *ShellFiles) Download(ctx context.Context, actor Actor, sessionID uuid.UUID,
	path string) (io.ReadCloser, int64, error) {
	live, fs, err := h.open(sessionID, actor.UserID)
	if err != nil {
		return nil, 0, err
	}
	clean, err := shell.CleanPath(path)
	if err != nil {
		return nil, 0, err
	}
	body, size, err := fs.Open(ctx, clean)
	if err != nil {
		return nil, 0, err
	}

	h.audit(ctx, actor, live, "ssh_file_downloaded", map[string]any{
		"path": clean, "bytes": size,
	})
	live.CountTraffic(0, size)
	return body, size, nil
}

// Upload writes a file into dir under name, returning how many bytes landed.
//
// The name is taken from the browser's file picker and joined rather than
// concatenated, so an upload called "../../etc/cron.d/x" cannot escape the
// directory the operator was looking at.
func (h *ShellFiles) Upload(ctx context.Context, actor Actor, sessionID uuid.UUID,
	dir, name string, src io.Reader) (string, int64, error) {
	live, fs, err := h.open(sessionID, actor.UserID)
	if err != nil {
		return "", 0, err
	}
	target, err := shell.JoinPath(dir, name)
	if err != nil {
		return "", 0, err
	}

	dst, err := fs.Create(ctx, target)
	if err != nil {
		return "", 0, err
	}
	written, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil {
		// The partial file is left in place deliberately: deleting it would be
		// a second write on a path the operator can see, and a truncated file
		// they can inspect beats one that vanished.
		h.audit(ctx, actor, live, "ssh_file_upload_failed", map[string]any{
			"path": target, "bytes": written, "error": copyErr.Error(),
		})
		return "", written, copyErr
	}
	if closeErr != nil {
		return "", written, closeErr
	}

	live.CountTraffic(written, 0)
	h.audit(ctx, actor, live, "ssh_file_uploaded", map[string]any{
		"path": target, "bytes": written,
	})
	return target, written, nil
}

// Mkdir creates a directory inside dir.
func (h *ShellFiles) Mkdir(ctx context.Context, actor Actor, sessionID uuid.UUID, dir, name string) (string, error) {
	live, fs, err := h.open(sessionID, actor.UserID)
	if err != nil {
		return "", err
	}
	target, err := shell.JoinPath(dir, name)
	if err != nil {
		return "", err
	}
	if err := fs.Mkdir(ctx, target); err != nil {
		return "", err
	}
	h.audit(ctx, actor, live, "ssh_directory_created", map[string]any{"path": target})
	return target, nil
}

// Remove deletes a file or an empty directory.
func (h *ShellFiles) Remove(ctx context.Context, actor Actor, sessionID uuid.UUID, path string) error {
	live, fs, err := h.open(sessionID, actor.UserID)
	if err != nil {
		return err
	}
	clean, err := shell.CleanPath(path)
	if err != nil {
		return err
	}
	if clean == "/" {
		return fmt.Errorf("%w: refusing to delete the filesystem root", shell.ErrBadPath)
	}
	if err := fs.Remove(ctx, clean); err != nil {
		return err
	}
	h.audit(ctx, actor, live, "ssh_file_deleted", map[string]any{"path": clean})
	return nil
}

// Rename moves an entry. Both ends are absolute paths, so a rename can also be
// a move - which is what a file browser's drag is.
func (h *ShellFiles) Rename(ctx context.Context, actor Actor, sessionID uuid.UUID, from, to string) error {
	live, fs, err := h.open(sessionID, actor.UserID)
	if err != nil {
		return err
	}
	src, err := shell.CleanPath(from)
	if err != nil {
		return err
	}
	dst, err := shell.CleanPath(to)
	if err != nil {
		return err
	}
	if err := fs.Rename(ctx, src, dst); err != nil {
		return err
	}
	h.audit(ctx, actor, live, "ssh_file_renamed", map[string]any{"from": src, "to": dst})
	return nil
}

// Chmod changes a mode. The mode arrives as the octal an operator types.
func (h *ShellFiles) Chmod(ctx context.Context, actor Actor, sessionID uuid.UUID, path, mode string) error {
	live, fs, err := h.open(sessionID, actor.UserID)
	if err != nil {
		return err
	}
	clean, err := shell.CleanPath(path)
	if err != nil {
		return err
	}
	bits, err := strconv.ParseUint(mode, 8, 32)
	if err != nil || bits > 0o7777 {
		return fmt.Errorf("%w: mode must be octal, like 644", shell.ErrBadPath)
	}
	if err := fs.Chmod(ctx, clean, uint32(bits)); err != nil {
		return err
	}
	h.audit(ctx, actor, live, "ssh_file_mode_changed", map[string]any{
		"path": clean, "mode": mode,
	})
	return nil
}

func (h *ShellFiles) audit(ctx context.Context, actor Actor, live *shellreg.Live,
	action string, details map[string]any) {
	details["session_id"] = live.SessionID.String()
	details["ssh_user"] = live.SSHUser
	writeAudit(ctx, h.Audit, actor, h.Clock.Now(), AuditCategoryShell, action,
		"vm", live.VMID.String(), live.VMName, details)
}
