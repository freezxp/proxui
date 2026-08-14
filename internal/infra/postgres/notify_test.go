package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/freezxp/proxui/internal/app/ports"
	"github.com/freezxp/proxui/internal/domain/notify"
	"github.com/freezxp/proxui/internal/infra/crypto"
)

func newChannel(t *testing.T, repo *NotifyRepository, sealed *crypto.SealedSecret) *notify.Channel {
	t.Helper()
	ch := &notify.Channel{
		ID: uuid.New(), Name: "ch-" + uuid.NewString()[:8], Kind: notify.KindWebhook,
		Config:    map[string]any{"url": "https://example.invalid/hook"},
		IsEnabled: true, CreatedAt: time.Now().UTC(),
	}
	if err := repo.CreateChannel(context.Background(), ch, sealed); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return ch
}

func testVault(t *testing.T) *crypto.Vault {
	t.Helper()
	key := make([]byte, crypto.MasterKeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	vault, err := crypto.NewVault(key, 1)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	return vault
}

// The delivery claim is what stops a republished event producing a second
// message. Without it, a relay restart between publishing and acknowledging
// would notify everyone twice.
func TestDeliveryIsClaimedOncePerEventAndChannel(t *testing.T) {
	pool := testPool(t)
	repo := NewNotifyRepository(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	channel := newChannel(t, repo, nil)
	const outboxID = int64(987654321)

	id, claimed, err := repo.ClaimDelivery(ctx, outboxID, channel.ID, "first", now)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !claimed || id == 0 {
		t.Fatal("the first claim should have succeeded")
	}

	_, claimedAgain, err := repo.ClaimDelivery(ctx, outboxID, channel.ID, "second", now)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimedAgain {
		t.Error("the same event was claimed twice for one channel")
	}

	// A different channel is a different delivery: two channels routed to the
	// same event must each get their message.
	other := newChannel(t, repo, nil)
	_, claimedOther, err := repo.ClaimDelivery(ctx, outboxID, other.ID, "other", now)
	if err != nil {
		t.Fatalf("other channel claim: %v", err)
	}
	if !claimedOther {
		t.Error("a second channel was denied its own delivery of the same event")
	}
}

// Test sends have no originating event, so they must not collide with each
// other the way event deliveries deliberately do.
func TestUnclaimedDeliveriesDoNotCollide(t *testing.T) {
	pool := testPool(t)
	repo := NewNotifyRepository(pool)
	ctx := context.Background()
	channel := newChannel(t, repo, nil)

	first, err := repo.RecordDelivery(ctx, channel.ID, "test one", time.Now().UTC())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := repo.RecordDelivery(ctx, channel.ID, "test two", time.Now().UTC())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first == second {
		t.Error("two test sends shared a delivery row")
	}
}

func TestFinishDeliveryRecordsOutcomeAndAttempts(t *testing.T) {
	pool := testPool(t)
	repo := NewNotifyRepository(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	channel := newChannel(t, repo, nil)

	failed, err := repo.RecordDelivery(ctx, channel.ID, "will fail", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.FinishDelivery(ctx, failed, errors.New("receiver answered 500"), now); err != nil {
		t.Fatal(err)
	}
	if err := repo.FinishDelivery(ctx, failed, errors.New("receiver answered 500"), now); err != nil {
		t.Fatal(err)
	}

	sent, err := repo.RecordDelivery(ctx, channel.ID, "will succeed", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.FinishDelivery(ctx, sent, nil, now); err != nil {
		t.Fatal(err)
	}

	deliveries, err := repo.ListDeliveries(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]ports.DeliveryRecord{}
	for _, d := range deliveries {
		byID[d.ID] = d
	}

	if got := byID[failed]; got.Status != "failed" || got.Attempts != 2 {
		t.Errorf("failed delivery = status %q attempts %d, want failed/2", got.Status, got.Attempts)
	}
	if got := byID[failed]; got.LastError == "" {
		t.Error("a failed delivery carries no reason, so the log cannot explain itself")
	}
	if got := byID[sent]; got.Status != "sent" || got.SentAt == nil {
		t.Errorf("sent delivery = status %q sentAt %v, want sent with a timestamp", got.Status, got.SentAt)
	}
}

func TestChannelSecretRoundTripsAndIsNotListed(t *testing.T) {
	pool := testPool(t)
	repo := NewNotifyRepository(pool)
	ctx := context.Background()
	vault := testVault(t)

	const secret = "https://hooks.example.invalid/T000/B000/XXXX"
	sealed, err := vault.Seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	channel := newChannel(t, repo, &sealed)

	opened, err := repo.ChannelSecret(ctx, channel.ID, vault)
	if err != nil {
		t.Fatalf("open secret: %v", err)
	}
	if opened != secret {
		t.Errorf("secret round trip returned %q", opened)
	}

	// Listing exposes only whether a secret exists, never the value.
	channels, err := repo.ListChannels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range channels {
		if ch.ID != channel.ID {
			continue
		}
		if !ch.HasSecret {
			t.Error("a channel with a stored secret reported none")
		}
		for key, value := range ch.Config {
			if s, ok := value.(string); ok && s == secret {
				t.Errorf("the secret leaked into config[%q]", key)
			}
		}
	}
}

// Editing a channel without supplying a secret must keep the stored one, or
// every recipient-list change would silently break delivery.
func TestUpdateWithoutSecretKeepsTheStoredOne(t *testing.T) {
	pool := testPool(t)
	repo := NewNotifyRepository(pool)
	ctx := context.Background()
	vault := testVault(t)

	sealed, err := vault.Seal("original-secret")
	if err != nil {
		t.Fatal(err)
	}
	channel := newChannel(t, repo, &sealed)

	channel.Name = channel.Name + "-renamed"
	if err := repo.UpdateChannel(ctx, channel, nil); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := repo.ChannelSecret(ctx, channel.ID, vault)
	if err != nil {
		t.Fatal(err)
	}
	if got != "original-secret" {
		t.Errorf("secret after a no-secret update = %q, want it unchanged", got)
	}
}

func TestDeletedChannelFreesItsNameAndDropsItsRules(t *testing.T) {
	pool := testPool(t)
	repo := NewNotifyRepository(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	channel := newChannel(t, repo, nil)
	rule := &notify.Rule{
		ID: uuid.New(), Category: notify.CategorySyncFailure, MinSeverity: notify.SeverityInfo,
		ChannelID: channel.ID, IsEnabled: true, CreatedAt: now,
	}
	if err := repo.CreateRule(ctx, rule); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	if err := repo.DeleteChannel(ctx, channel.ID, now); err != nil {
		t.Fatalf("delete channel: %v", err)
	}

	rules, err := repo.ListRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if r.ID == rule.ID {
			t.Error("a rule outlived the channel it delivered to")
		}
	}

	// The name is free again: platforms learned this in migration 00007 and
	// channels were built with it from the start.
	reused := &notify.Channel{
		ID: uuid.New(), Name: channel.Name, Kind: notify.KindSlack,
		Config: map[string]any{}, IsEnabled: true, CreatedAt: now,
	}
	if err := repo.CreateChannel(ctx, reused, nil); err != nil {
		t.Errorf("could not reuse a deleted channel's name: %v", err)
	}
}
