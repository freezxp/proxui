package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/app/shellreg"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/domain/shell"
)

// PortalKey manages the portal's own SSH key (SSH-11..SSH-14, ADR 0006).
//
// The point of the key is that a password stops being needed on every connect
// without any guest password being stored: an operator signs in with one once,
// installs the portal's public key into that account's authorized_keys through
// the session they already have, and connects with the key from then on.
//
// Every method here is audited under the shell category, including the reads
// that change nothing on a guest but change what the portal can do - because
// "when did this key appear" is the first question anyone asks of a key found
// in authorized_keys.
type PortalKey struct {
	Keys      ports.PortalKeyStore
	KeyGen    ports.SSHKeyGenerator
	Registry  *shellreg.Registry
	Inventory ports.InventoryReader
	Audit     ports.AuditWriter
	Clock     ports.Clock
}

// View returns the public record of the key, or ErrNoPortalKey when there is
// none yet.
func (h *PortalKey) View(ctx context.Context) (shell.PortalKey, error) {
	key, err := h.Keys.Get(ctx)
	if errors.Is(err, ports.ErrNotFound) {
		return shell.PortalKey{}, shell.ErrNoPortalKey
	}
	return key, err
}

// Installs lists every install in the estate. Administrators only - it is the
// map of where this key opens doors.
func (h *PortalKey) Installs(ctx context.Context) ([]shell.KeyInstall, error) {
	return h.Keys.Installs(ctx, uuid.Nil)
}

// InstallsForVM lists the installs on one VM, for the connect form.
//
// Scoped by the same grant as opening a shell: which accounts on a machine the
// portal can log into without a password is worth as much to someone probing
// as the machine's address, and an operator who may not touch the VM has no
// business reading it.
func (h *PortalKey) InstallsForVM(ctx context.Context, actor Actor, role identity.Role,
	vmID uuid.UUID) ([]shell.KeyInstall, error) {
	allowed, err := h.Inventory.CanAccessVM(ctx, vmID, role, actor.UserID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, shell.ErrNotPermitted
	}
	return h.Keys.Installs(ctx, vmID)
}

// Generate makes a new key pair, replacing any existing one.
//
// Generation and rotation are the same operation on purpose. A portal with two
// keys would have to decide which one to offer a guest, and an operator would
// have to know which of the two is in a given authorized_keys; one key means
// the only question is whether a guest has it. The cost is that rotating
// invalidates every install at once, which is why the previous fingerprint is
// audited and why the stale installs stay listed afterwards.
func (h *PortalKey) Generate(ctx context.Context, actor Actor) (shell.PortalKey, error) {
	previous, err := h.Keys.Get(ctx)
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		return shell.PortalKey{}, err
	}
	rotation := err == nil

	privatePEM, key, err := h.KeyGen.Generate(shell.KeyComment)
	if err != nil {
		return shell.PortalKey{}, err
	}
	key.CreatedAt = h.Clock.Now()
	key.CreatedBy = actor.UserID

	if err := h.Keys.Replace(ctx, key, privatePEM); err != nil {
		return shell.PortalKey{}, err
	}

	details := map[string]any{"fingerprint": key.Fingerprint, "algorithm": key.Algorithm}
	action := "ssh_portal_key_created"
	if rotation {
		action = "ssh_portal_key_rotated"
		details["previous_fingerprint"] = previous.Fingerprint
		// Rotation is the one operation here with an effect nobody asked for:
		// every line already on a guest stops working. Counting them into the
		// entry makes the consequence visible in the trail rather than only in
		// a UI somebody may not have been looking at.
		if installs, err := h.Keys.Installs(ctx, uuid.Nil); err == nil {
			details["installs_invalidated"] = len(installs)
		}
	}
	writeAudit(ctx, h.Audit, actor, h.Clock.Now(), AuditCategoryShell, action,
		"portal_key", key.Fingerprint, "", details)
	return key, nil
}

// Delete removes the key pair.
//
// The public halves already installed on guests are not removed - the portal
// has no way to reach a guest without a credential for it. They become lines
// authorizing a key nobody holds, which is inert but untidy, so the count is
// audited and the install records are kept for whoever has to go and clean up.
func (h *PortalKey) Delete(ctx context.Context, actor Actor) error {
	key, err := h.Keys.Get(ctx)
	if errors.Is(err, ports.ErrNotFound) {
		return shell.ErrNoPortalKey
	}
	if err != nil {
		return err
	}
	if err := h.Keys.Delete(ctx); err != nil {
		return err
	}

	details := map[string]any{"fingerprint": key.Fingerprint}
	if installs, err := h.Keys.Installs(ctx, uuid.Nil); err == nil {
		details["installs_left_on_guests"] = len(installs)
	}
	writeAudit(ctx, h.Audit, actor, h.Clock.Now(), AuditCategoryShell,
		"ssh_portal_key_deleted", "portal_key", key.Fingerprint, "", details)
	return nil
}

// Install writes the public key into authorized_keys for the account this
// session is signed in as (SSH-12).
//
// It runs over the operator's own live session, which is what makes it safe to
// offer: the authorization is that they already authenticated as that account
// on that guest, and the write happens with exactly that account's
// permissions. The portal is not reaching into a machine nobody let it into.
func (h *PortalKey) Install(ctx context.Context, actor Actor, sessionID uuid.UUID) (shell.KeyInstall, error) {
	key, err := h.View(ctx)
	if err != nil {
		return shell.KeyInstall{}, err
	}
	live, remote, err := h.session(sessionID, actor.UserID)
	if err != nil {
		return shell.KeyInstall{}, err
	}

	authPath, err := h.ensureSSHDir(ctx, remote)
	if err != nil {
		return shell.KeyInstall{}, err
	}
	existing, err := readAuthorizedKeys(ctx, remote, authPath)
	if err != nil {
		return shell.KeyInstall{}, err
	}

	suffix, needed := shell.AppendAuthorizedKey(existing, key.PublicKey)
	if needed {
		if err := writeKeysFile(ctx, remote.Append, remote, authPath, suffix); err != nil {
			return shell.KeyInstall{}, err
		}
	}

	install := shell.KeyInstall{
		VMID: live.VMID, VMName: live.VMName, SSHUser: live.SSHUser,
		Fingerprint: key.Fingerprint, InstalledAt: h.Clock.Now(), InstalledBy: actor.UserID,
	}
	if err := h.Keys.RecordInstall(ctx, install); err != nil {
		return shell.KeyInstall{}, err
	}

	writeAudit(ctx, h.Audit, actor, h.Clock.Now(), AuditCategoryShell,
		"ssh_portal_key_installed", "vm", live.VMID.String(), live.VMName,
		map[string]any{
			"session_id": live.SessionID.String(), "ssh_user": live.SSHUser,
			"fingerprint": key.Fingerprint, "path": authPath,
			// false means the line was already there: installing twice is a
			// no-op, and the trail should say which of the two happened.
			"changed": needed,
		})
	return install, nil
}

// Uninstall removes the portal's line from authorized_keys for the account
// this session is signed in as (SSH-14).
//
// This is the revocation an operator can perform themselves, and the reason
// storing a portal-owned key is a different proposition from storing a guest
// password: taking the key back is deleting one line from one file.
func (h *PortalKey) Uninstall(ctx context.Context, actor Actor, sessionID uuid.UUID) error {
	key, err := h.View(ctx)
	if err != nil {
		return err
	}
	live, remote, err := h.session(sessionID, actor.UserID)
	if err != nil {
		return err
	}

	home, err := remote.Home(ctx)
	if err != nil {
		return err
	}
	authPath := path.Join(home, shell.SSHDir, shell.AuthorizedKeysFile)
	existing, err := readAuthorizedKeys(ctx, remote, authPath)
	if err != nil {
		return err
	}

	updated, changed := shell.RemoveAuthorizedKey(existing, key.PublicKey)
	if changed {
		// Rewriting truncates, which is why the whole file was read first and
		// why removal is the only operation here that does it. Everything not
		// carrying the portal's key is written back exactly as it was found.
		if err := writeKeysFile(ctx, remote.Create, remote, authPath, updated); err != nil {
			return err
		}
	}

	// The record goes whether or not the file had the line: either way the
	// portal's belief afterwards is "not installed", and leaving a row behind
	// because the operator had already deleted the line by hand would make the
	// list permanently wrong.
	if err := h.Keys.ForgetInstall(ctx, live.VMID, live.SSHUser); err != nil {
		return err
	}

	writeAudit(ctx, h.Audit, actor, h.Clock.Now(), AuditCategoryShell,
		"ssh_portal_key_removed", "vm", live.VMID.String(), live.VMName,
		map[string]any{
			"session_id": live.SessionID.String(), "ssh_user": live.SSHUser,
			"fingerprint": key.Fingerprint, "path": authPath, "changed": changed,
		})
	return nil
}

// session resolves the caller's own live session and its file subsystem.
func (h *PortalKey) session(sessionID, owner uuid.UUID) (*shellreg.Live, ports.RemoteFS, error) {
	return liveFS(h.Registry, h.Clock, sessionID, owner)
}

// writeKeysFile writes authorized_keys through open and leaves it at a mode
// sshd will accept.
//
// sshd ignores an authorized_keys that anyone else can write, and fails the
// login without saying why. Fixing the mode on every write turns the most
// common "I installed it and it still asks for a password" into something that
// does not happen — which is why it belongs here rather than at the call sites.
func writeKeysFile(ctx context.Context, open func(context.Context, string) (io.WriteCloser, error), remote ports.RemoteFS, authPath, content string) error {
	w, err := open(ctx, authPath)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(w, content)
	// A close error matters only when the write itself succeeded: a failed
	// write is the more useful thing to report.
	if closeErr := w.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return writeErr
	}
	return remote.Chmod(ctx, authPath, shell.AuthorizedKeysMode)
}

// ensureSSHDir makes sure ~/.ssh exists with a mode sshd will accept, and
// returns the path of the authorized_keys inside it.
func (h *PortalKey) ensureSSHDir(ctx context.Context, remote ports.RemoteFS) (string, error) {
	home, err := remote.Home(ctx)
	if err != nil {
		return "", err
	}
	dir := path.Join(home, shell.SSHDir)

	switch _, err := remote.Stat(ctx, dir); {
	case err == nil:
		// Left as found. A directory that already exists belongs to the
		// account, and tightening a mode somebody chose is not this feature's
		// business.
	case errors.Is(err, fs.ErrNotExist):
		if err := remote.Mkdir(ctx, dir); err != nil {
			return "", err
		}
		if err := remote.Chmod(ctx, dir, shell.SSHDirMode); err != nil {
			return "", err
		}
	default:
		return "", err
	}
	return path.Join(dir, shell.AuthorizedKeysFile), nil
}

// readAuthorizedKeys reads the file, treating "not there" as empty: a fresh
// account has no authorized_keys and installing into it is the normal case.
func readAuthorizedKeys(ctx context.Context, remote ports.RemoteFS, authPath string) (string, error) {
	body, size, err := remote.Open(ctx, authPath)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer body.Close()

	if size > shell.MaxAuthorizedKeysBytes {
		return "", fmt.Errorf("%w: %s is %d bytes", shell.ErrBadAuthorizedKeys, authPath, size)
	}
	content, err := io.ReadAll(io.LimitReader(body, shell.MaxAuthorizedKeysBytes+1))
	if err != nil {
		return "", err
	}
	if len(content) > shell.MaxAuthorizedKeysBytes {
		return "", fmt.Errorf("%w: %s is larger than %d bytes",
			shell.ErrBadAuthorizedKeys, authPath, shell.MaxAuthorizedKeysBytes)
	}
	return string(content), nil
}
