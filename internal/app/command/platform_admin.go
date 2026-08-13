package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/connector"
	"github.com/freezxp/proxui/internal/domain/inventory"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

// PlatformInput describes a platform to register or update. The secret is
// write-only: it is sealed on arrival and no API ever returns it (PLAT-03).
type PlatformInput struct {
	Actor          Actor
	Name           string
	Type           string
	EndpointURL    string
	Datacenter     string
	TLSMode        string
	TLSCAPEM       string
	TLSFingerprint string
	Config         map[string]any
	CredentialKind string
	TokenID        string
	Secret         string
	Intervals      *inventory.SyncIntervals
	IsEnabled      *bool
}

// ManagePlatforms owns platform registration and configuration.
type ManagePlatforms struct {
	Platforms ports.PlatformRepository
	Vault     *crypto.Vault
	Audit     ports.AuditWriter
	Clock     ports.Clock
}

// AuditCategoryPlatform labels platform configuration changes.
const AuditCategoryPlatform = "platform"

// TestConnection probes a prospective platform without persisting anything, so
// an administrator learns about a bad token or a missing privilege before the
// registration exists (PLAT-02).
func (h *ManagePlatforms) TestConnection(ctx context.Context, in PlatformInput) (connector.TestReport, error) {
	conn, err := h.build(in)
	if err != nil {
		return connector.TestReport{}, err
	}
	defer conn.Close()

	report, err := conn.TestConnection(ctx)
	if err != nil {
		return report, err
	}
	return report, nil
}

// Create registers a platform and seals its credential.
func (h *ManagePlatforms) Create(ctx context.Context, in PlatformInput) (*inventory.Platform, error) {
	now := h.Clock.Now()
	p := &inventory.Platform{
		ID:             uuid.New(),
		Name:           in.Name,
		Type:           in.Type,
		EndpointURL:    in.EndpointURL,
		Datacenter:     defaultString(in.Datacenter, "default"),
		IsEnabled:      true,
		TLSMode:        defaultString(in.TLSMode, "verify"),
		TLSCAPEM:       in.TLSCAPEM,
		TLSFingerprint: in.TLSFingerprint,
		Config:         in.Config,
		SyncIntervals:  inventory.DefaultSyncIntervals(),
		Health:         inventory.HealthUnknown,
		CreatedAt:      now,
	}
	if in.Intervals != nil {
		p.SyncIntervals = *in.Intervals
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if !connector.IsRegistered(p.Type) {
		return nil, fmt.Errorf("%w: no connector is registered for type %q", inventory.ErrInvalidPlatform, p.Type)
	}

	cred, err := h.seal(in)
	if err != nil {
		return nil, err
	}
	if err := h.Platforms.Create(ctx, p, cred); err != nil {
		return nil, err
	}

	writeAudit(ctx, h.Audit, in.Actor, now, AuditCategoryPlatform, "platform_created",
		"platform", p.ID.String(), p.Name, map[string]any{
			"type": p.Type, "endpoint": p.EndpointURL, "tls_mode": p.TLSMode,
		})
	// Disabling certificate verification is a security-relevant decision, so it
	// is recorded separately rather than buried in the creation details.
	if p.TLSMode == string(connector.TLSInsecure) {
		writeAudit(ctx, h.Audit, in.Actor, now, ports.AuditCategorySecurity, "tls_verification_disabled",
			"platform", p.ID.String(), p.Name, nil)
	}
	return p, nil
}

// Update changes platform configuration and optionally rotates the credential.
func (h *ManagePlatforms) Update(ctx context.Context, id uuid.UUID, in PlatformInput) (*inventory.Platform, error) {
	p, err := h.Platforms.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	changes := map[string]any{}
	if in.Name != "" && in.Name != p.Name {
		changes["name"] = in.Name
		p.Name = in.Name
	}
	if in.EndpointURL != "" && in.EndpointURL != p.EndpointURL {
		changes["endpoint"] = in.EndpointURL
		p.EndpointURL = in.EndpointURL
	}
	if in.Datacenter != "" {
		p.Datacenter = in.Datacenter
	}
	if in.TLSMode != "" && in.TLSMode != p.TLSMode {
		changes["tls_mode"] = in.TLSMode
		p.TLSMode = in.TLSMode
		p.TLSCAPEM = in.TLSCAPEM
		p.TLSFingerprint = in.TLSFingerprint
	}
	if in.Intervals != nil {
		changes["sync_intervals"] = *in.Intervals
		p.SyncIntervals = *in.Intervals
	}
	if in.IsEnabled != nil && *in.IsEnabled != p.IsEnabled {
		changes["is_enabled"] = *in.IsEnabled
		p.IsEnabled = *in.IsEnabled
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := h.Platforms.Update(ctx, p); err != nil {
		return nil, err
	}

	if in.Secret != "" {
		cred, err := h.seal(in)
		if err != nil {
			return nil, err
		}
		if err := h.Platforms.ReplaceCredential(ctx, p.ID, cred); err != nil {
			return nil, err
		}
		changes["credential"] = "rotated"
	}

	now := h.Clock.Now()
	writeAudit(ctx, h.Audit, in.Actor, now, AuditCategoryPlatform, "platform_updated",
		"platform", p.ID.String(), p.Name, changes)
	return p, nil
}

// Delete soft-deletes a platform, keeping its assets resolvable for history.
func (h *ManagePlatforms) Delete(ctx context.Context, actor Actor, id uuid.UUID) error {
	p, err := h.Platforms.Get(ctx, id)
	if err != nil {
		return err
	}
	now := h.Clock.Now()
	if err := h.Platforms.SoftDelete(ctx, id, now); err != nil {
		return err
	}
	writeAudit(ctx, h.Audit, actor, now, AuditCategoryPlatform, "platform_deleted",
		"platform", id.String(), p.Name, nil)
	return nil
}

// build constructs a connector from unsaved input, for TestConnection.
func (h *ManagePlatforms) build(in PlatformInput) (connector.Connector, error) {
	if !connector.IsRegistered(in.Type) {
		return nil, fmt.Errorf("%w: no connector is registered for type %q", inventory.ErrInvalidPlatform, in.Type)
	}
	cfg := connector.Config{
		Endpoint:   in.EndpointURL,
		Datacenter: in.Datacenter,
		TLS: connector.TLSPolicy{
			Mode:        connector.TLSMode(defaultString(in.TLSMode, "verify")),
			CAPEM:       in.TLSCAPEM,
			Fingerprint: in.TLSFingerprint,
		},
		Extra: in.Config,
	}
	creds := connector.Credentials{
		Kind:    defaultString(in.CredentialKind, "api_token"),
		TokenID: in.TokenID,
		Secret:  in.Secret,
	}
	return connector.New(in.Type, cfg, creds, connector.Options{Timeout: 20 * time.Second})
}

func (h *ManagePlatforms) seal(in PlatformInput) (ports.SealedCredential, error) {
	if in.Secret == "" {
		return ports.SealedCredential{}, errors.New("command: a credential secret is required")
	}
	sealed, err := h.Vault.Seal(in.Secret)
	if err != nil {
		return ports.SealedCredential{}, err
	}
	return ports.SealedCredential{
		Kind:    defaultString(in.CredentialKind, "api_token"),
		TokenID: in.TokenID,
		Sealed:  sealed,
	}, nil
}

func defaultString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
