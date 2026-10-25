// Package logs turns the raw docker log stream into a bounded, filtered
// answer for the HTTP log API: query validation, line filtering and the
// limits that keep one noisy container from flooding the caller. It knows
// nothing about docker itself — the command lines live in internal/compose.
package logs

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/local/dockops/internal/compose"
)

// Limits of the log API. Deliberately constants, not configuration: there is
// exactly one kind of caller (an operator or an agent reading logs), so a
// config surface would only add states nobody asked for.
const (
	// MaxTail bounds one snapshot's line count.
	MaxTail = 5000
	// MaxSince bounds how far back a request may look.
	MaxSince = 168 * time.Hour
	// MaxBytes bounds one snapshot response in bytes.
	MaxBytes = 2 << 20
	// MaxStreamBytes bounds one live stream in bytes.
	MaxStreamBytes = 10 << 20
	// MaxStreamDuration bounds one live stream in time.
	MaxStreamDuration = 15 * time.Minute
	// MaxConcurrentStreams bounds concurrent live streams per agent.
	MaxConcurrentStreams = 8
	// MaxGrepLen bounds the search term's length.
	MaxGrepLen = 256
	// MaxLineBytes bounds a single log line. A longer one is cut and its
	// remainder dropped: a crashing container can emit a multi-megabyte line
	// without a newline, and buffering it would defeat every other limit.
	MaxLineBytes = 64 << 10
)

// truncatedLineMarker is appended to a line the filter had to cut.
const truncatedLineMarker = " … [dockops: line truncated]"

// Kind selects what a request reads.
type Kind int

const (
	// KindContainer reads one container.
	KindContainer Kind = iota
	// KindProject reads every container of a compose project.
	KindProject
)

// Format selects the wire format of the answer.
type Format int

const (
	// FormatText writes the raw log lines.
	FormatText Format = iota
	// FormatJSON writes one JSON object per line (NDJSON).
	FormatJSON
)

// Options is a validated log request.
type Options struct {
	Kind   Kind
	Name   string
	Since  time.Time
	Tail   int
	Grep   string
	Regex  bool
	Format Format
	Follow bool
}

// nameRe is the allowlist for container and project names. The leading
// character requirement matters: a name starting with "-" would otherwise be
// taken as a flag by docker logs.
var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// daySuffix matches a leading "<n>d" so that "7d12h" works; time.ParseDuration
// has no day unit.
var daySuffix = regexp.MustCompile(`^(\d+)d(.*)$`)

// ParseQuery validates the query parameters of a snapshot request.
func ParseQuery(q url.Values, now time.Time) (Options, error) {
	return parse(q, now, false)
}

// ParseStreamQuery validates the query parameters of a live stream: Follow is
// set, Tail defaults to 0 (new lines only) and 0 stays allowed.
func ParseStreamQuery(q url.Values, now time.Time) (Options, error) {
	return parse(q, now, true)
}

func parse(q url.Values, now time.Time, stream bool) (Options, error) {
	var opts Options
	opts.Follow = stream

	container, project := q.Get("container"), q.Get("project")
	switch {
	case container == "" && project == "":
		return opts, errors.New("one of container or project is required")
	case container != "" && project != "":
		return opts, errors.New("container and project are mutually exclusive")
	case container != "":
		if !nameRe.MatchString(container) {
			return opts, fmt.Errorf("invalid container name %q", container)
		}
		opts.Kind, opts.Name = KindContainer, container
	default:
		if !nameRe.MatchString(project) {
			return opts, fmt.Errorf("invalid project name %q", project)
		}
		opts.Kind, opts.Name = KindProject, project
	}

	opts.Since = now.Add(-time.Hour)
	if raw := q.Get("since"); raw != "" {
		since, err := parseSince(raw, now)
		if err != nil {
			return opts, err
		}
		opts.Since = since
	}

	opts.Tail = 200
	if stream {
		opts.Tail = 0
	}
	if raw := q.Get("tail"); raw != "" {
		tail, err := strconv.Atoi(raw)
		if err != nil {
			return opts, fmt.Errorf("invalid tail %q: not a number", raw)
		}
		switch {
		case tail > MaxTail:
			return opts, fmt.Errorf("invalid tail %d: maximum is %d", tail, MaxTail)
		case tail < 0:
			return opts, fmt.Errorf("invalid tail %d: must not be negative", tail)
		case tail == 0 && !stream:
			return opts, errors.New("invalid tail 0: only a stream may start without tail")
		}
		opts.Tail = tail
	}

	opts.Grep = q.Get("grep")
	if len(opts.Grep) > MaxGrepLen {
		return opts, fmt.Errorf("grep too long: %d characters, maximum is %d", len(opts.Grep), MaxGrepLen)
	}
	switch q.Get("regex") {
	case "", "0":
	case "1":
		opts.Regex = true
		if opts.Grep == "" {
			return opts, errors.New("regex=1 requires grep")
		}
		if _, err := regexp.Compile(opts.Grep); err != nil {
			return opts, fmt.Errorf("invalid grep regex %q: %w", opts.Grep, err)
		}
	default:
		return opts, fmt.Errorf("invalid regex %q: want 0 or 1", q.Get("regex"))
	}

	switch q.Get("format") {
	case "", "text":
	case "json":
		opts.Format = FormatJSON
	default:
		return opts, fmt.Errorf("invalid format %q: want text or json", q.Get("format"))
	}
	return opts, nil
}

// parseSince accepts RFC3339 or a duration ("90s", "2h30m", "7d").
func parseSince(raw string, now time.Time) (time.Time, error) {
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		if ts.Before(now.Add(-MaxSince)) {
			return time.Time{}, fmt.Errorf("invalid since %q: older than the maximum of %s", raw, MaxSince)
		}
		return ts, nil
	}
	d, err := parseDuration(raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid since %q: want RFC3339 or a duration like 90m or 7d", raw)
	}
	if d <= 0 {
		return time.Time{}, fmt.Errorf("invalid since %q: must be positive", raw)
	}
	if d > MaxSince {
		return time.Time{}, fmt.Errorf("invalid since %q: maximum is %s", raw, MaxSince)
	}
	return now.Add(-d), nil
}

func parseDuration(raw string) (time.Duration, error) {
	if m := daySuffix.FindStringSubmatch(raw); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, err
		}
		rest := time.Duration(0)
		if m[2] != "" {
			parsed, err := time.ParseDuration(m[2])
			if err != nil {
				return 0, err
			}
			rest = parsed
		}
		return time.Duration(days)*24*time.Hour + rest, nil
	}
	return time.ParseDuration(raw)
}

// Compose renders the docker-facing request.
func (o Options) Compose() compose.LogsOptions {
	out := compose.LogsOptions{Since: o.Since, Tail: o.Tail, Follow: o.Follow}
	if o.Kind == KindProject {
		out.Project = o.Name
	} else {
		out.Container = o.Name
	}
	return out
}
