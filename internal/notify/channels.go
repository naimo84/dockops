package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/local/dockops/internal/config"
)

const (
	// maxErrorBody caps the response snippet of a failing delivery that ends
	// up in the WARN log.
	maxErrorBody = 200
)

// telegramAPIBase is the root of the Bot API. A variable so tests can point
// the sender at an httptest server — the request URL embeds the bot token and
// is therefore not configurable through the destination itself.
var telegramAPIBase = "https://api.telegram.org"

// channel is one configured destination plus the redacted host used in logs
// (host only, never the URL: it may carry a token or a query secret).
type channel struct {
	cfg  config.NotificationDestination
	host string
}

// newChannel resolves the log host of a destination.
func newChannel(d config.NotificationDestination) *channel {
	raw := d.URL
	if raw == "" {
		raw = d.WebhookURL
	}
	host := ""
	if u, err := url.Parse(raw); err == nil {
		host = u.Host
	}
	if host == "" && d.Type == "telegram" {
		if u, err := url.Parse(telegramAPIBase); err == nil {
			host = u.Host
		}
	}
	return &channel{cfg: d, host: host}
}

// send delivers one event through this channel.
func (c *channel) send(ctx context.Context, client *http.Client, ev Event, text, host string) error {
	switch c.cfg.Type {
	case "webhook":
		return c.sendWebhook(ctx, client, ev, host)
	case "ntfy":
		return c.sendNtfy(ctx, client, ev, text)
	case "telegram":
		return c.sendTelegram(ctx, client, text)
	case "discord":
		return c.sendJSON(ctx, client, c.cfg.WebhookURL, map[string]string{"content": text})
	case "slack":
		return c.sendJSON(ctx, client, c.cfg.WebhookURL, map[string]string{"text": text})
	default:
		return fmt.Errorf("unknown destination type %q", c.cfg.Type)
	}
}

// webhookPayload is the JSON body of a webhook delivery. It mirrors Event and
// adds the sending host, so several hosts can share one channel.
type webhookPayload struct {
	Kind   string    `json:"kind"`
	Stack  string    `json:"stack"`
	Remote string    `json:"remote"`
	Stage  string    `json:"stage"`
	Error  string    `json:"error"`
	Hash   string    `json:"hash"`
	At     time.Time `json:"at"`
	Host   string    `json:"host"`
}

// sendWebhook posts the structured event as JSON plus the configured extra
// headers. It is the generic channel for own automation (e.g. n8n).
func (c *channel) sendWebhook(ctx context.Context, client *http.Client, ev Event, host string) error {
	body, err := json.Marshal(webhookPayload{
		Kind:   ev.Kind,
		Stack:  ev.Stack,
		Remote: ev.Remote,
		Stage:  ev.Stage,
		Error:  ev.Err,
		Hash:   ev.Hash,
		At:     ev.At,
		Host:   host,
	})
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	headers := map[string]string{"Content-Type": "application/json"}
	for name, value := range c.cfg.Headers {
		headers[name] = value
	}
	return post(ctx, client, c.cfg.URL, headers, body)
}

// sendNtfy posts the plain-text line to an ntfy topic. Title, Priority and
// Tags travel as headers, as ntfy expects them.
func (c *channel) sendNtfy(ctx context.Context, client *http.Client, ev Event, text string) error {
	headers := map[string]string{
		"Content-Type": "text/plain; charset=utf-8",
		"Title":        notificationTitle(ev),
	}
	if c.cfg.Priority != "" {
		headers["Priority"] = c.cfg.Priority
	}
	if len(c.cfg.Tags) > 0 {
		headers["Tags"] = strings.Join(c.cfg.Tags, ",")
	}
	return post(ctx, client, c.cfg.URL, headers, []byte(text))
}

// notificationTitle derives ntfy's Title header; the schema has no title
// field, so it is built from the event ("dockops: deploy_failed web").
func notificationTitle(ev Event) string {
	title := "dockops: " + ev.Kind
	if ev.Stack != "" {
		title += " " + ev.Stack
	}
	return title
}

// sendTelegram posts chat_id and text to the Bot API. parse_mode is
// deliberately not set: Markdown/HTML escaping is a support case the plain
// text form avoids.
func (c *channel) sendTelegram(ctx context.Context, client *http.Client, text string) error {
	endpoint := telegramAPIBase + "/bot" + c.cfg.Token + "/sendMessage"
	body, err := json.Marshal(map[string]string{
		"chat_id": c.cfg.ChatID,
		"text":    truncate(text, maxMessageLength),
	})
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	return post(ctx, client, endpoint, map[string]string{"Content-Type": "application/json"}, body)
}

// sendJSON posts a single-field JSON body (discord content, slack text).
func (c *channel) sendJSON(ctx context.Context, client *http.Client, rawURL string, payload map[string]string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	return post(ctx, client, rawURL, map[string]string{"Content-Type": "application/json"}, body)
}

// post performs one delivery attempt. Every HTTP status >= 400 is an error
// carrying the start of the response body (capped at 200 characters).
func post(ctx context.Context, client *http.Client, rawURL string, headers map[string]string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", sanitizeError(err))
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", sanitizeError(err))
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

// sanitizeError unwraps *url.Error: net/http embeds the full request URL
// there, and a telegram URL contains the bot token, so it must never reach a
// log line.
func sanitizeError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		return sanitizeError(uerr.Err)
	}
	return err
}
