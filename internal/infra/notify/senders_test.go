package notify_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	domain "github.com/freezxp/proxui/internal/domain/notify"
	"github.com/freezxp/proxui/internal/infra/notify"
)

func testMessage() domain.Message {
	return domain.Message{
		Subject: "Synchronization failed: pve-home", Body: "connection refused",
		Severity: domain.SeverityCritical, Category: domain.CategorySyncFailure,
		Event: domain.Event{Type: "sync.failed", Payload: map[string]any{"platform_name": "pve-home"}},
	}
}

func TestWebhookSignsTheBody(t *testing.T) {
	const secret = "shared-hmac-key"
	var (
		gotBody      []byte
		gotSignature string
		gotTimestamp string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSignature = r.Header.Get(notify.SignatureHeader)
		gotTimestamp = r.Header.Get(notify.TimestampHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := &notify.WebhookSender{Client: server.Client()}
	channel := domain.Channel{Kind: domain.KindWebhook, Config: map[string]any{"url": server.URL}}
	if err := sender.Send(context.Background(), channel, secret, testMessage()); err != nil {
		t.Fatalf("send: %v", err)
	}

	if gotSignature == "" || gotTimestamp == "" {
		t.Fatal("the delivery carried no signature or timestamp")
	}
	// The receiver's check, performed exactly as a receiver would.
	if want := notify.Sign(secret, gotTimestamp, gotBody); want != gotSignature {
		t.Errorf("signature %q does not verify (want %q)", gotSignature, want)
	}
	// The timestamp is signed, so replaying an old body under a new timestamp
	// must not verify.
	if notify.Sign(secret, "1", gotBody) == gotSignature {
		t.Error("the signature does not cover the timestamp")
	}

	var payload map[string]any
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if payload["severity"] != "critical" || payload["type"] != "sync.failed" {
		t.Errorf("payload lost its identifying fields: %v", payload)
	}
}

func TestWebhookWithoutSecretSendsUnsigned(t *testing.T) {
	var signed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signed = r.Header.Get(notify.SignatureHeader) != ""
	}))
	defer server.Close()

	sender := &notify.WebhookSender{Client: server.Client()}
	channel := domain.Channel{Kind: domain.KindWebhook, Config: map[string]any{"url": server.URL}}
	if err := sender.Send(context.Background(), channel, "", testMessage()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if signed {
		t.Error("a channel with no secret produced a signature header")
	}
}

func TestWebhookReportsReceiverFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	sender := &notify.WebhookSender{Client: server.Client()}
	channel := domain.Channel{Kind: domain.KindWebhook, Config: map[string]any{"url": server.URL}}
	err := sender.Send(context.Background(), channel, "", testMessage())
	if err == nil {
		t.Fatal("a 500 from the receiver was reported as success")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not say what the receiver answered", err)
	}
}

func TestWebhookNeedsAURL(t *testing.T) {
	sender := &notify.WebhookSender{Client: http.DefaultClient}
	err := sender.Send(context.Background(), domain.Channel{Kind: domain.KindWebhook}, "", testMessage())
	if err == nil {
		t.Fatal("a webhook with no URL was accepted")
	}
}

func TestSlackPostsToTheWebhookURL(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sender := &notify.SlackSender{Client: server.Client()}
	// The webhook URL is the secret, so it arrives that way rather than in
	// the config blob.
	if err := sender.Send(context.Background(), domain.Channel{Kind: domain.KindSlack}, server.URL, testMessage()); err != nil {
		t.Fatalf("send: %v", err)
	}
	text, _ := body["text"].(string)
	if !strings.Contains(text, "pve-home") {
		t.Errorf("slack text %q does not carry the subject", text)
	}
}

func TestSlackRefusalCarriesTheReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("invalid_token"))
	}))
	defer server.Close()

	sender := &notify.SlackSender{Client: server.Client()}
	err := sender.Send(context.Background(), domain.Channel{Kind: domain.KindSlack}, server.URL, testMessage())
	if err == nil {
		t.Fatal("a rejected post was reported as success")
	}
	// "invalid_token" is the only actionable thing Slack says; losing it
	// leaves an administrator with nothing to go on.
	if !strings.Contains(err.Error(), "invalid_token") {
		t.Errorf("error %q dropped Slack's reason", err)
	}
}

func TestSlackNeedsItsURL(t *testing.T) {
	sender := &notify.SlackSender{Client: http.DefaultClient}
	if err := sender.Send(context.Background(), domain.Channel{Kind: domain.KindSlack}, "", testMessage()); err == nil {
		t.Fatal("a Slack channel with no webhook URL was accepted")
	}
}

func TestRegistryRejectsUnknownKinds(t *testing.T) {
	registry := notify.DefaultRegistry(nil)
	if _, err := registry.For(domain.KindSlack); err != nil {
		t.Errorf("slack should be available: %v", err)
	}
	if _, err := registry.For(domain.Kind("smoke-signal")); err == nil {
		t.Error("an unknown kind produced a sender")
	}
}
