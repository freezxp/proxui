package command

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/app/shellreg"
	"github.com/freezxp/proxui/internal/domain/identity"
	"github.com/freezxp/proxui/internal/domain/shell"
)

// AuditCategoryShell labels SSH access in the audit trail. It is separate from
// the console's category because the two answer different questions: a console
// entry says someone had the keyboard, an SSH entry says someone logged in as a
// named account and may have moved files.
const AuditCategoryShell = "ssh"

// OpenShellInput asks for an SSH session on a VM.
//
// The credential fields are the only place in the portal where a secret
// arrives, is used, and is never stored (SSH-03). They are not logged, not
// echoed back, and not copied into the session record.
type OpenShellInput struct {
	Actor      Actor
	Role       identity.Role
	VMID       uuid.UUID
	Username   string
	Password   string
	PrivateKey string
	Passphrase string

	// UsePortalKey authenticates with the portal's own key instead of anything
	// the operator typed (SSH-13). It is the whole point of storing that key:
	// the secret is fetched from the vault here and never travels to a browser,
	// which is why this is a flag rather than the key arriving in the request.
	UsePortalKey bool

	// Host optionally names which of the VM's addresses to use. It is checked
	// against the synced list rather than trusted: allowing an arbitrary host
	// would turn the portal into an SSH proxy to anywhere its network reaches,
	// which is a different and much larger power than "reach the VMs I was
	// granted".
	Host string
	Port int

	// AcceptHostKey is the fingerprint the operator confirmed after being
	// shown an unknown key. Empty on the first attempt (SSH-04).
	AcceptHostKey string
}

// OpenShellOutput is the handle the browser uses to connect.
type OpenShellOutput struct {
	SessionID      uuid.UUID
	TicketID       uuid.UUID
	ExpiresIn      time.Duration
	Address        string
	SSHUser        string
	HostKey        shell.HostKey
	Home           string
	FilesAvailable bool
	FilesDetail    string
}

// HostKeyPrompt is returned when a VM's key is not yet pinned. It carries the
// fingerprint to show, and is an error type so it cannot be mistaken for a
// successful connection by a caller that forgot to check.
type HostKeyPrompt struct {
	Address     string
	Algorithm   string
	Fingerprint string
}

func (p *HostKeyPrompt) Error() string {
	return fmt.Sprintf("%v: %s (%s)", shell.ErrHostKeyUnknown, p.Fingerprint, p.Algorithm)
}

// Unwrap lets callers match the sentinel while still reading the fingerprint.
func (p *HostKeyPrompt) Unwrap() error { return shell.ErrHostKeyUnknown }

// HostKeyChanged is returned when the pinned key no longer matches.
type HostKeyChanged struct {
	Address     string
	Expected    string
	Got         string
	FirstSeenAt time.Time
}

func (c *HostKeyChanged) Error() string {
	return fmt.Sprintf("%v: expected %s, got %s", shell.ErrHostKeyMismatch, c.Expected, c.Got)
}

func (c *HostKeyChanged) Unwrap() error { return shell.ErrHostKeyMismatch }

// OpenShell authorizes, connects, and registers a live SSH session.
//
// Unlike OpenConsole it does reach the far end before returning. It has to:
// the credential exists only for the duration of this call, so authentication
// cannot be deferred to the moment the WebSocket opens. The happy consequence
// is that a wrong password is a plain 401 on a JSON request rather than a
// WebSocket that closes for reasons the browser has to guess at.
type OpenShell struct {
	Inventory ports.InventoryReader
	Sessions  ports.ShellRepository
	HostKeys  ports.HostKeyStore
	Tickets   ports.ShellTicketStore
	Registry  *shellreg.Registry
	Dialer    ports.SSHDialer
	Audit     ports.AuditWriter
	Clock     ports.Clock

	// Keys opens the portal's own key for a UsePortalKey connect. Optional: a
	// deployment that never generated one, or a test that does not care, leaves
	// it nil and every connect is a typed credential as before.
	Keys ports.PortalKeyStore
}

// Handle opens the session.
func (h *OpenShell) Handle(ctx context.Context, in OpenShellInput) (OpenShellOutput, error) {
	if strings.TrimSpace(in.Username) == "" {
		return OpenShellOutput{}, shell.ErrCredentialMissing
	}
	if !in.UsePortalKey && in.Password == "" && strings.TrimSpace(in.PrivateKey) == "" {
		return OpenShellOutput{}, shell.ErrCredentialMissing
	}

	// Scoping, not just the role gate: an operator may open a shell on the VMs
	// they were granted, not on every VM in the estate (SSH-01).
	allowed, err := h.Inventory.CanAccessVM(ctx, in.VMID, in.Role, in.Actor.UserID)
	if err != nil {
		return OpenShellOutput{}, fmt.Errorf("open shell: %w", err)
	}
	if !allowed {
		h.auditDenied(ctx, in, "not_permitted")
		return OpenShellOutput{}, shell.ErrNotPermitted
	}

	vm, err := h.Inventory.GetVM(ctx, in.VMID, identity.RoleAdmin, uuid.Nil)
	if err != nil {
		return OpenShellOutput{}, err
	}
	if vm.State != "running" {
		return OpenShellOutput{}, shell.ErrNotRunning
	}

	host, err := chooseHost(in.Host, vm.IPAddresses)
	if err != nil {
		return OpenShellOutput{}, err
	}
	port, err := shell.NormalizePort(in.Port)
	if err != nil {
		return OpenShellOutput{}, err
	}
	target := ports.SSHTarget{Host: host, Port: port}

	policy, err := h.policyFor(ctx, in)
	if err != nil {
		return OpenShellOutput{}, err
	}

	// The vault is opened only once the caller has been shown to be allowed on
	// this VM, so a request that was never going to connect cannot make the
	// portal decrypt its key.
	cred, err := h.credential(ctx, in)
	if err != nil {
		return OpenShellOutput{}, err
	}

	conn, dialErr := h.Dialer.Dial(ctx, target, cred, policy)

	// Pin on acceptance rather than on a successful login, which is what
	// OpenSSH does and what an operator therefore expects: the human confirmed
	// the fingerprint, and whether they then mistyped their password says
	// nothing about whose key it is. Pinning only on success would make every
	// fat-fingered password cost a second trust decision, and a trust prompt
	// people see repeatedly is one they stop reading.
	if policy.accepted {
		key := policy.seen
		key.VMID = in.VMID
		key.FirstSeenAt = h.Clock.Now()
		key.TrustedBy = in.Actor.UserID
		if err := h.HostKeys.Trust(ctx, key); err != nil {
			if conn != nil {
				conn.Close()
			}
			return OpenShellOutput{}, err
		}
		writeAudit(ctx, h.Audit, in.Actor, h.Clock.Now(), AuditCategoryShell, "ssh_host_key_trusted",
			"vm", in.VMID.String(), vm.Name, map[string]any{
				"address": target.Address(), "fingerprint": key.Fingerprint,
				"algorithm": key.Algorithm,
			})
	}

	if dialErr != nil {
		return OpenShellOutput{}, h.explainDialFailure(ctx, in, target, policy, dialErr)
	}

	now := h.Clock.Now()

	session := &shell.Session{
		ID: uuid.New(), UserID: in.Actor.UserID, VMID: in.VMID,
		SSHUser: in.Username, Address: target.Address(),
		ClientIP: in.Actor.IP, UserAgent: in.Actor.UserAgent, StartedAt: now,
	}
	if err := h.Sessions.Create(ctx, session); err != nil {
		conn.Close()
		return OpenShellOutput{}, err
	}

	live := shellreg.NewLive(session.ID, in.Actor.UserID, in.VMID, vm.Name,
		in.Username, target.Address(), conn, now)
	if err := h.Registry.Add(live); err != nil {
		conn.Close()
		_ = h.Sessions.Close(ctx, session.ID, shell.ReasonError, 0, 0, now)
		return OpenShellOutput{}, err
	}

	ticket := shell.NewTicket(session.ID, in.Actor.UserID, in.VMID, now)
	if err := h.Tickets.Issue(ctx, ticket); err != nil {
		h.Registry.Remove(session.ID)
		_ = h.Sessions.Close(ctx, session.ID, shell.ReasonError, 0, 0, now)
		return OpenShellOutput{}, err
	}

	writeAudit(ctx, h.Audit, in.Actor, now, AuditCategoryShell, "ssh_session_opened",
		"vm", in.VMID.String(), vm.Name, map[string]any{
			"session_id": session.ID.String(), "ssh_user": in.Username,
			"address": target.Address(), "auth_method": authMethod(in),
		})

	out := OpenShellOutput{
		SessionID: session.ID, TicketID: ticket.ID, ExpiresIn: shell.TicketTTL,
		Address: target.Address(), SSHUser: in.Username, HostKey: policy.seen,
		Home: "/",
	}

	// Probe the file subsystem now so the browser knows whether to draw the
	// panel at all, and where to open it. A guest without SFTP is a working
	// terminal, not a failure (SSH-09).
	if fs, err := live.Files(); err == nil {
		out.FilesAvailable = true
		if home, err := fs.Home(ctx); err == nil && home != "" {
			out.Home = home
		}
	} else {
		out.FilesDetail = err.Error()
	}
	return out, nil
}

// credential resolves what to authenticate with.
//
// The portal key path never lets the secret out of this call: it is read from
// the vault, handed to the dialer, and goes out of scope. Nothing about it
// reaches the session record, the response, or the audit entry beyond the fact
// that it was used (SSH-13).
func (h *OpenShell) credential(ctx context.Context, in OpenShellInput) (ports.SSHCredential, error) {
	if !in.UsePortalKey {
		return ports.SSHCredential{
			Username:   in.Username,
			Password:   in.Password,
			PrivateKey: in.PrivateKey,
			Passphrase: in.Passphrase,
		}, nil
	}
	if h.Keys == nil {
		return ports.SSHCredential{}, shell.ErrNoPortalKey
	}
	privateKey, err := h.Keys.PrivateKey(ctx)
	if errors.Is(err, ports.ErrNotFound) {
		return ports.SSHCredential{}, shell.ErrNoPortalKey
	}
	if err != nil {
		return ports.SSHCredential{}, err
	}
	return ports.SSHCredential{Username: in.Username, PrivateKey: privateKey}, nil
}

// authMethod names how a connect authenticated, for the audit trail. It says
// which door was used, never what went through it.
func authMethod(in OpenShellInput) string {
	switch {
	case in.UsePortalKey:
		return "portal_key"
	case strings.TrimSpace(in.PrivateKey) != "":
		return "private_key"
	default:
		return "password"
	}
}

// chooseHost decides which address to connect to, refusing any host that is
// not one the platform reported for this VM.
func chooseHost(requested string, known []string) (string, error) {
	if requested == "" {
		return shell.PickAddress(known)
	}
	for _, addr := range known {
		text := addr
		if i := strings.IndexByte(text, '/'); i >= 0 {
			text = text[:i]
		}
		if strings.EqualFold(strings.TrimSpace(text), strings.TrimSpace(requested)) {
			return text, nil
		}
	}
	return "", fmt.Errorf("%w: %s is not an address this VM reported",
		shell.ErrNoAddress, requested)
}

func (h *OpenShell) policyFor(ctx context.Context, in OpenShellInput) (*hostKeyPolicy, error) {
	known, err := h.HostKeys.Get(ctx, in.VMID)
	switch {
	case err == nil:
		return &hostKeyPolicy{known: known, hasKnown: true}, nil
	case errors.Is(err, ports.ErrNotFound):
		return &hostKeyPolicy{accept: in.AcceptHostKey}, nil
	default:
		return nil, err
	}
}

// explainDialFailure turns a dial error into the answer the operator needs, and
// records the attempt. Authentication failures are audited as denials: a burst
// of them against one VM is exactly what an audit trail exists to show.
func (h *OpenShell) explainDialFailure(ctx context.Context, in OpenShellInput,
	target ports.SSHTarget, policy *hostKeyPolicy, err error) error {
	switch {
	case errors.Is(err, shell.ErrHostKeyUnknown):
		return &HostKeyPrompt{
			Address:     target.Address(),
			Algorithm:   policy.seen.Algorithm,
			Fingerprint: policy.seen.Fingerprint,
		}
	case errors.Is(err, shell.ErrHostKeyMismatch):
		h.auditDenied(ctx, in, "host_key_mismatch")
		return &HostKeyChanged{
			Address:     target.Address(),
			Expected:    policy.known.Fingerprint,
			Got:         policy.seen.Fingerprint,
			FirstSeenAt: policy.known.FirstSeenAt,
		}
	case errors.Is(err, shell.ErrAuthFailed):
		h.auditDenied(ctx, in, "auth_failed")
		return err
	default:
		return err
	}
}

func (h *OpenShell) auditDenied(ctx context.Context, in OpenShellInput, reason string) {
	actorID := in.Actor.UserID
	_ = h.Audit.Write(ctx, ports.AuditEntry{
		Time: h.Clock.Now(), ActorUserID: &actorID, ActorName: in.Actor.Username,
		Category: AuditCategoryShell, Action: "ssh_denied",
		TargetType: "vm", TargetID: in.VMID.String(),
		SourceIP: in.Actor.IP, UserAgent: in.Actor.UserAgent, RequestID: in.Actor.RequestID,
		Outcome: ports.OutcomeDenied,
		// The SSH username is recorded; the secret that failed is not.
		Details: map[string]any{
			"reason": reason, "ssh_user": in.Username, "auth_method": authMethod(in),
		},
	})
}

// hostKeyPolicy implements ports.HostKeyPolicy against a pinned key.
type hostKeyPolicy struct {
	known    shell.HostKey
	hasKnown bool
	accept   string

	// seen is what the guest actually presented, captured so the caller can
	// show a fingerprint the operator has to confirm.
	seen shell.HostKey
	// accepted records that this handshake approved a previously unknown key
	// because the operator confirmed its fingerprint, which is the moment it
	// becomes worth pinning.
	accepted bool
}

// Check runs inside the handshake, before any credential is offered.
func (p *hostKeyPolicy) Check(address, algorithm, fingerprint string, publicKey []byte) error {
	p.seen = shell.HostKey{
		Address: address, Algorithm: algorithm,
		Fingerprint: fingerprint, PublicKey: publicKey,
	}

	if p.hasKnown {
		// Compare the key itself, not its fingerprint: the fingerprint is a
		// display of the key, and comparing the thing rather than its
		// rendering removes a whole class of "close enough" bug.
		if subtle.ConstantTimeCompare(p.known.PublicKey, publicKey) == 1 {
			return nil
		}
		return shell.ErrHostKeyMismatch
	}
	if p.accept != "" && subtle.ConstantTimeCompare([]byte(p.accept), []byte(fingerprint)) == 1 {
		p.accepted = true
		return nil
	}
	return shell.ErrHostKeyUnknown
}

// CloseShell ends a session and records why.
//
// Used by the operator's disconnect button, by an administrator closing
// someone else's session, and by the registry's own sweep - one path, so a
// session cannot end in a way the audit trail has never seen.
type CloseShell struct {
	Sessions ports.ShellRepository
	Registry *shellreg.Registry
	Audit    ports.AuditWriter
	Clock    ports.Clock
}

// Handle closes the session. actor is who asked; owner is the user whose
// session it must be, or uuid.Nil for an administrator closing any.
// An administrator may close anyone's session; everyone else may close only
// their own. Both halves of that decision — the ownership scope and the reason
// the trail records — are derived here rather than by the caller, because they
// have to agree: a handler that computed the scope and forgot the reason filed
// an administrator's forced disconnect as the operator's own.
func (h *CloseShell) Handle(ctx context.Context, actor Actor, role identity.Role, sessionID uuid.UUID) error {
	owner := actor.UserID
	reason := shell.ReasonUser
	if role == identity.RoleAdmin {
		// uuid.Nil is what the registry reads as "skip the ownership check".
		owner = uuid.Nil
	}

	live, err := h.Registry.Get(sessionID, owner)
	if err != nil {
		return err
	}
	// Closing your own session from another tab is still an ordinary close; it
	// is only somebody else's session that is a forced one.
	if live.UserID != actor.UserID {
		reason = shell.ReasonAdminForced
	}

	h.Registry.Remove(sessionID)
	h.Record(ctx, live, reason)
	return nil
}

// Record finalizes a session's row and writes its audit entry. It is exported
// because the registry's sweep and the process shutdown both end sessions
// nobody asked to end, and those endings are as much a part of the trail as a
// deliberate disconnect (SSH-06).
func (h *CloseShell) Record(ctx context.Context, live *shellreg.Live, reason string) {
	now := h.Clock.Now()
	tx, rx := live.Traffic()
	_ = h.Sessions.Close(ctx, live.SessionID, reason, tx, rx, now)

	actorID := live.UserID
	_ = h.Audit.Write(ctx, ports.AuditEntry{
		Time: now, ActorUserID: &actorID,
		Category: AuditCategoryShell, Action: "ssh_session_closed",
		TargetType: "vm", TargetID: live.VMID.String(), TargetName: live.VMName,
		Outcome: ports.OutcomeSuccess,
		Details: map[string]any{
			"session_id": live.SessionID.String(), "reason": reason,
			"ssh_user":         live.SSHUser,
			"duration_seconds": int(now.Sub(live.StartedAt).Seconds()),
			"bytes_tx":         tx, "bytes_rx": rx,
		},
	})
}
