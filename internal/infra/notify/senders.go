// Package notify delivers rendered messages to configured channels. It is
// infrastructure: the decision of what to send lives in the domain, and the
// decision of whether to retry lives in the job runtime.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	"github.com/freezxp/proxui/internal/domain/notify"
)

// Sender delivers one message to one channel.
type Sender interface {
	Send(ctx context.Context, ch notify.Channel, secret string, msg notify.Message) error
}

// Registry picks a sender for a channel kind.
type Registry struct {
	Email   Sender
	Slack   Sender
	Webhook Sender
}

// For returns the sender for a kind.
func (r Registry) For(kind notify.Kind) (Sender, error) {
	switch kind {
	case notify.KindEmail:
		if r.Email != nil {
			return r.Email, nil
		}
	case notify.KindSlack:
		if r.Slack != nil {
			return r.Slack, nil
		}
	case notify.KindWebhook:
		if r.Webhook != nil {
			return r.Webhook, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", notify.ErrUnknownKind, kind)
}

// DefaultRegistry builds the senders the portal ships with.
func DefaultRegistry(client *http.Client) Registry {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return Registry{
		Email:   &EmailSender{},
		Slack:   &SlackSender{Client: client},
		Webhook: &WebhookSender{Client: client},
	}
}

// --- email ---------------------------------------------------------------

// EmailSender delivers over SMTP.
type EmailSender struct {
	// Dial is swapped in tests. Production uses net/smtp directly.
	Dial func(addr string) (*smtp.Client, error)
}

func (s *EmailSender) Send(ctx context.Context, ch notify.Channel, secret string, msg notify.Message) error {
	host := configString(ch.Config, "host")
	port := configString(ch.Config, "port")
	from := configString(ch.Config, "from")
	to := configStrings(ch.Config, "to")
	username := configString(ch.Config, "username")

	if host == "" || from == "" || len(to) == 0 {
		return fmt.Errorf("%w: email needs a host, a from address and at least one recipient",
			notify.ErrInvalidConfig)
	}
	if port == "" {
		port = "587"
	}

	body := buildMIME(from, to, msg)
	addr := net.JoinHostPort(host, port)

	var auth smtp.Auth
	if username != "" {
		auth = smtp.PlainAuth("", username, secret, host)
	}

	// net/smtp predates context, so the deadline is enforced by racing the
	// send against the caller's context rather than being pushed into it.
	done := make(chan error, 1)
	go func() { done <- smtp.SendMail(addr, auth, from, to, body) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		if err != nil {
			return fmt.Errorf("smtp send: %w", err)
		}
		return nil
	}
}

func buildMIME(from string, to []string, msg notify.Message) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", sanitizeHeader(msg.Subject))
	fmt.Fprintf(&b, "X-ProxUI-Category: %s\r\n", sanitizeHeader(msg.Category))
	fmt.Fprintf(&b, "X-ProxUI-Severity: %s\r\n", sanitizeHeader(string(msg.Severity)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(msg.Body, "\n", "\r\n"))
	return b.Bytes()
}

// sanitizeHeader strips CR and LF. A VM named with a newline would otherwise
// let the name inject extra headers into the message.
func sanitizeHeader(value string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
}

// --- slack ---------------------------------------------------------------

// SlackSender posts to an incoming webhook. The webhook URL is the secret:
// anyone holding it can post to the channel, so it is stored encrypted rather
// than in the config blob.
type SlackSender struct{ Client *http.Client }

func (s *SlackSender) Send(ctx context.Context, _ notify.Channel, secret string, msg notify.Message) error {
	if secret == "" {
		return fmt.Errorf("%w: slack needs its webhook URL", notify.ErrInvalidConfig)
	}

	payload := map[string]any{
		"text": fmt.Sprintf("%s %s", severityIcon(msg.Severity), msg.Subject),
		"blocks": []any{
			map[string]any{
				"type": "section",
				"text": map[string]any{
					"type": "mrkdwn",
					"text": fmt.Sprintf("*%s %s*\n%s", severityIcon(msg.Severity), msg.Subject, msg.Body),
				},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, secret, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("slack post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		// Slack answers with a plain-text reason ("invalid_token"), which is
		// the only useful thing to put in the delivery log.
		reason := make([]byte, 256)
		n, _ := resp.Body.Read(reason)
		return fmt.Errorf("slack refused the message: %s: %s", resp.Status, strings.TrimSpace(string(reason[:n])))
	}
	return nil
}

func severityIcon(s notify.Severity) string {
	switch s {
	case notify.SeverityCritical:
		return "🔴"
	case notify.SeverityWarning:
		return "🟠"
	default:
		return "🔵"
	}
}

// --- webhook -------------------------------------------------------------

// WebhookSender POSTs JSON, signed so the receiver can verify it came from
// this portal (NOTIF-01).
type WebhookSender struct{ Client *http.Client }

// SignatureHeader carries the hex-encoded HMAC-SHA256 of the request body.
const SignatureHeader = "X-ProxUI-Signature"

// TimestampHeader lets a receiver reject replayed deliveries.
const TimestampHeader = "X-ProxUI-Timestamp"

func (s *WebhookSender) Send(ctx context.Context, ch notify.Channel, secret string, msg notify.Message) error {
	url := configString(ch.Config, "url")
	if url == "" {
		return fmt.Errorf("%w: webhook needs a URL", notify.ErrInvalidConfig)
	}

	payload := map[string]any{
		"category":    msg.Category,
		"severity":    string(msg.Severity),
		"type":        msg.Event.Type,
		"subject":     msg.Subject,
		"body":        msg.Body,
		"occurred_at": msg.Event.OccurredAt.UTC().Format(time.RFC3339),
		"payload":     msg.Event.Payload,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ProxUI")

	if secret != "" {
		timestamp := fmt.Sprint(time.Now().UTC().Unix())
		req.Header.Set(TimestampHeader, timestamp)
		// The timestamp is inside the signed material, so an attacker cannot
		// replay an old body under a fresh timestamp.
		req.Header.Set(SignatureHeader, Sign(secret, timestamp, body))
	}

	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook receiver answered %s", resp.Status)
	}
	return nil
}

// Sign computes the delivery signature. Exported because a receiver
// implemented against this portal needs the exact construction, and because
// the test suite verifies it rather than reimplementing it.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// --- config helpers ------------------------------------------------------

func configString(config map[string]any, key string) string {
	if v, ok := config[key]; ok {
		switch typed := v.(type) {
		case string:
			return strings.TrimSpace(typed)
		case float64:
			return fmt.Sprintf("%g", typed)
		}
	}
	return ""
}

func configStrings(config map[string]any, key string) []string {
	v, ok := config[key]
	if !ok {
		return nil
	}
	switch typed := v.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case []string:
		return typed
	case string:
		// A single address, or a comma-separated list, both of which an
		// administrator will reasonably type into one box.
		var out []string
		for _, part := range strings.Split(typed, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	}
	return nil
}
