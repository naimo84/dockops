// Package notify delivers reconcile-failure events to the channels configured
// in the root stacks.yaml (webhook, ntfy, telegram, discord, slack).
//
// It is deliberately small: transport plus an in-memory dedupe per
// (kind, stack). Delivery errors never reach the caller — a broken channel is
// logged as a warning and must never fail a reconcile pass — and log output
// carries the channel type and host, never tokens or URL queries.
//
// Only the standard library is used (net/http, encoding/json, context,
// log/slog, sync); the package has no dependencies of its own.
package notify

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/local/dockops/internal/config"
)

const (
	// maxErrorLength caps Event.Err in every payload (characters, not
	// bytes: a multi-byte error text is cut on a rune boundary).
	maxErrorLength = 2000
	// maxMessageLength caps telegram's text (its documented limit is 4096;
	// 4000 leaves room for the prefix).
	maxMessageLength = 4000
	// defaultTimeout stands in for a zero timeout in hand-built configs;
	// the loader always fills 10s.
	defaultTimeout = 10 * time.Second
)

// Event is one reconcile failure to report.
type Event struct {
	// Kind is deploy_failed, prune_failed or repo_failed.
	Kind string
	// Stack is the affected stack; empty for repo_failed.
	Stack string
	// Remote is the affected remote; only set for repo_failed.
	Remote string
	// Stage names the failing phase: pre_deploy, compose_up, post_deploy,
	// secrets, config or repo.
	Stage string
	// Err is the failure text, truncated to 2000 characters.
	Err string
	// Hash is the stack hash at the time of the attempt, when known.
	Hash string
	// At is the event time; zero means "now" (the notifier's clock).
	At time.Time
}

// Notifier reports reconcile failures and clears their dedupe state.
type Notifier interface {
	// Notify delivers ev to every destination once per (kind, stack) within
	// the configured dedupe window. Kinds not listed in notifications.on are
	// dropped silently, as is everything while notifications.enabled is
	// false.
	Notify(ctx context.Context, ev Event)

	// Recovery clears the dedupe key of (kind, stack), so the next failure
	// alarms immediately instead of waiting for the window to expire.
	Recovery(kind, stack string)
}

// notifier is the stdlib-only Notifier implementation.
type notifier struct {
	cfg      config.Notifications
	logger   *slog.Logger
	clock    func() time.Time
	host     string
	events   map[string]bool
	client   *http.Client
	channels []*channel

	mu   sync.Mutex
	last map[string]time.Time
}

// Compile-time assertion that the implementation satisfies the interface.
var _ Notifier = (*notifier)(nil)

// New builds a Notifier for cfg. logger nil falls back to slog.Default() and
// clock nil to time.Now; the clock is injectable so the dedupe window is
// testable without sleeping.
func New(cfg config.Notifications, logger *slog.Logger, clock func() time.Time) Notifier {
	if logger == nil {
		logger = slog.Default()
	}
	if clock == nil {
		clock = time.Now
	}
	host, err := os.Hostname()
	if err != nil {
		// Several hosts share one channel; an unknown hostname only costs
		// the "host" field of the payload, so it must not break anything.
		logger.Debug("cannot determine hostname; notification payloads carry an empty host", "error", err)
		host = ""
	}
	n := &notifier{
		cfg:    cfg,
		logger: logger,
		clock:  clock,
		host:   host,
		events: make(map[string]bool, len(cfg.On)),
		client: &http.Client{},
		last:   make(map[string]time.Time),
	}
	for _, kind := range cfg.On {
		n.events[kind] = true
	}
	for _, d := range cfg.Destinations {
		n.channels = append(n.channels, newChannel(d))
	}
	return n
}

// Notify implements Notifier.
func (n *notifier) Notify(ctx context.Context, ev Event) {
	if !n.cfg.Enabled {
		n.logger.Debug("notifications disabled; event dropped", "kind", ev.Kind, "stack", ev.Stack)
		return
	}
	if !n.events[ev.Kind] {
		// Not requested by notifications.on: no send, no log noise per event.
		return
	}
	if ev.At.IsZero() {
		ev.At = n.clock()
	}
	ev.Err = truncate(ev.Err, maxErrorLength)
	if !n.allow(ev.Kind, ev.Stack) {
		n.logger.Debug("notification within dedupe window; suppressed", "kind", ev.Kind, "stack", ev.Stack)
		return
	}
	text := renderText(ev)
	for _, ch := range n.channels {
		n.deliver(ctx, ch, ev, text)
	}
}

// Recovery implements Notifier.
func (n *notifier) Recovery(kind, stack string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.last, dedupeKey(kind, stack))
}

// allow reports whether the (kind, stack) pair may alarm now and records the
// attempt when it may. The window is consumed by the attempt, not by a
// successful delivery: a channel that is down would otherwise turn every
// pass into another flood. The dedupe state is in memory only: a restart
// forgets it and may repeat one alarm — accepted, because the alternative (a
// state file for notification bookkeeping) is not worth the failure modes.
func (n *notifier) allow(kind, stack string) bool {
	window := time.Duration(n.cfg.RepeatAfter)
	now := n.clock()
	key := dedupeKey(kind, stack)
	n.mu.Lock()
	defer n.mu.Unlock()
	if prev, ok := n.last[key]; ok && window > 0 && now.Before(prev.Add(window)) {
		return false
	}
	n.last[key] = now
	return true
}

// dedupeKey names the dedupe bucket of an event. repo_failed events have no
// stack; the reconciler reports at most one repo failure per pass, so the
// empty stack name is a pass-wide bucket there by design.
func dedupeKey(kind, stack string) string {
	return kind + "\x00" + stack
}

// deliver sends one event to one destination, bounded by its own timeout, so
// a hanging channel cannot delay the following ones or the pass.
func (n *notifier) deliver(ctx context.Context, ch *channel, ev Event, text string) {
	timeout := time.Duration(n.cfg.Timeout)
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	sendCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ch.send(sendCtx, n.client, ev, text, n.host); err != nil {
		n.logger.Warn("notification delivery failed", "type", ch.cfg.Type, "host", ch.host, "kind", ev.Kind, "stack", ev.Stack, "error", err)
		return
	}
	n.logger.Debug("notification delivered", "type", ch.cfg.Type, "host", ch.host, "kind", ev.Kind, "stack", ev.Stack)
}

// renderText is the plain-text form shared by ntfy, telegram, discord and
// slack: "[<kind>] <stack>: <err>", without the stack part for repo_failed.
func renderText(ev Event) string {
	if ev.Stack == "" {
		return "[" + ev.Kind + "] " + ev.Err
	}
	return "[" + ev.Kind + "] " + ev.Stack + ": " + ev.Err
}

// truncate cuts s to at most limit characters (runes, so a multi-byte
// character never gets split) and marks the cut with an ellipsis.
func truncate(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit-1]) + "…"
}
