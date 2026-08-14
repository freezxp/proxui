// Package notify decides which events reach which channels and drives
// delivery (NOTIF-02, NOTIF-03).
package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/notify"
	"github.com/freezxp/proxui/internal/infra/crypto"
	infranotify "github.com/freezxp/proxui/internal/infra/notify"
)

// MaxAttempts is how many times a delivery is tried before it is left failed
// (NOTIF-03). Three covers a restarted mail server or a brief network fault;
// beyond that the configuration is wrong and retrying only hides it.
const MaxAttempts = 3

// VMGroupLookup answers whether a VM belongs to a group, for rules scoped to
// one. It is a separate port because scoping is an Access concern.
type VMGroupLookup interface {
	VMGroupMemberIDs(ctx context.Context, groupID uuid.UUID) ([]uuid.UUID, error)
}

// Dispatcher routes one event to every channel whose rule matches.
type Dispatcher struct {
	Repo     ports.NotifyRepository
	Groups   VMGroupLookup
	Senders  infranotify.Registry
	Vault    *crypto.Vault
	Clock    ports.Clock
	Log      zerolog.Logger
	Audit    ports.AuditWriter
	Attempts int // 0 means MaxAttempts
}

// Dispatch delivers one event. It is safe to call twice for the same event:
// the delivery claim is unique per event and channel, so a relay that
// republishes after a crash produces no second message.
func (d *Dispatcher) Dispatch(ctx context.Context, event notify.Event) error {
	rules, err := d.Repo.ListRules(ctx)
	if err != nil {
		return fmt.Errorf("dispatch: %w", err)
	}
	if len(rules) == 0 {
		return nil
	}

	message := notify.Render(event)

	// A channel reached by two matching rules gets one message, not two: the
	// rules describe why it should hear about this, not how many times.
	seen := map[uuid.UUID]bool{}
	for _, rule := range rules {
		if !rule.Matches(event) || seen[rule.ChannelID] {
			continue
		}
		inScope, err := d.inVMGroup(ctx, rule, event)
		if err != nil {
			d.Log.Error().Err(err).Str("rule_id", rule.ID.String()).
				Msg("could not evaluate rule scope; skipping the rule")
			continue
		}
		if !inScope {
			continue
		}
		seen[rule.ChannelID] = true

		deliveryID, claimed, err := d.Repo.ClaimDelivery(ctx, event.OutboxID, rule.ChannelID, message.Subject, d.Clock.Now())
		if err != nil {
			return err
		}
		if !claimed {
			continue // an earlier pass already sent, or is sending, this one
		}
		d.deliver(ctx, deliveryID, rule.ChannelID, message)
	}
	return nil
}

// inVMGroup applies the VM-group half of a rule's scope. A rule naming a group
// only fires for events about a VM in it; an event with no VM cannot qualify,
// because there is no evidence it concerns anything in the group.
func (d *Dispatcher) inVMGroup(ctx context.Context, rule notify.Rule, event notify.Event) (bool, error) {
	if rule.VMGroupID == nil {
		return true, nil
	}
	if event.VMID == nil {
		return false, nil
	}
	members, err := d.Groups.VMGroupMemberIDs(ctx, *rule.VMGroupID)
	if err != nil {
		return false, err
	}
	for _, id := range members {
		if id == *event.VMID {
			return true, nil
		}
	}
	return false, nil
}

// deliver sends one message, retrying transient failures.
func (d *Dispatcher) deliver(ctx context.Context, deliveryID int64, channelID uuid.UUID, message notify.Message) {
	attempts := d.Attempts
	if attempts <= 0 {
		attempts = MaxAttempts
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		lastErr = d.sendOnce(ctx, channelID, message)
		if lastErr == nil {
			break
		}
		// A misconfigured channel fails identically every time; retrying it
		// three times only delays the delivery log telling the truth.
		if errors.Is(lastErr, notify.ErrInvalidConfig) || errors.Is(lastErr, notify.ErrUnknownKind) {
			break
		}
		if attempt < attempts {
			select {
			case <-ctx.Done():
				lastErr = ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
	}

	if err := d.Repo.FinishDelivery(context.WithoutCancel(ctx), deliveryID, lastErr, d.Clock.Now()); err != nil {
		d.Log.Error().Err(err).Int64("delivery_id", deliveryID).Msg("could not record delivery outcome")
	}

	if lastErr != nil {
		d.Log.Warn().Err(lastErr).Str("channel_id", channelID.String()).
			Str("subject", message.Subject).Msg("notification delivery failed")
		// A notification that never arrived is itself worth auditing: the
		// absence of an alert is otherwise indistinguishable from calm
		// (NOTIF-03).
		if d.Audit != nil {
			_ = d.Audit.Write(context.WithoutCancel(ctx), ports.AuditEntry{
				Time: d.Clock.Now(), Category: "notification", Action: "notification_failed",
				TargetType: "notification_channel", TargetID: channelID.String(),
				Outcome: ports.OutcomeFailure,
				Details: map[string]any{"subject": message.Subject, "error": lastErr.Error()},
			})
		}
	}
}

func (d *Dispatcher) sendOnce(ctx context.Context, channelID uuid.UUID, message notify.Message) error {
	channel, err := d.Repo.GetChannel(ctx, channelID)
	if err != nil {
		return err
	}
	if !channel.IsEnabled {
		return fmt.Errorf("%w: the channel is disabled", notify.ErrInvalidConfig)
	}
	sender, err := d.Senders.For(channel.Kind)
	if err != nil {
		return err
	}
	secret, err := d.Repo.ChannelSecret(ctx, channelID, d.Vault)
	if err != nil {
		return fmt.Errorf("read channel secret: %w", err)
	}
	return sender.Send(ctx, channel, secret, message)
}

// SendTest delivers a sample message, so an administrator can prove a channel
// works before an incident depends on it (NOTIF-01).
func (d *Dispatcher) SendTest(ctx context.Context, channelID uuid.UUID) error {
	channel, err := d.Repo.GetChannel(ctx, channelID)
	if err != nil {
		return err
	}
	message := notify.Message{
		Subject:  "ProxUI test notification",
		Body:     fmt.Sprintf("This is a test message from ProxUI, sent to the channel %q.\n\nIf you are reading it, the channel works.", channel.Name),
		Severity: notify.SeverityInfo,
		Category: notify.CategorySecurity,
		Event:    notify.Event{Type: "notification.test", OccurredAt: d.Clock.Now()},
	}

	deliveryID, err := d.Repo.RecordDelivery(ctx, channelID, message.Subject, d.Clock.Now())
	if err != nil {
		return err
	}
	// A test reports its failure to the caller rather than only to the log:
	// the whole point is to find out now.
	sendErr := d.sendOnce(ctx, channelID, message)
	if err := d.Repo.FinishDelivery(ctx, deliveryID, sendErr, d.Clock.Now()); err != nil {
		d.Log.Error().Err(err).Msg("could not record test delivery")
	}
	return sendErr
}
