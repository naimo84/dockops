package logs

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"time"
)

// lineSink receives filtered log lines: one line, one call.
type lineSink interface {
	// Line gets the raw line (including docker's timestamp prefix), the
	// parsed timestamp and message, and the container name (empty in project
	// mode — docker compose prefixes those lines itself).
	Line(raw, ts, container, msg string) error
	// Note receives a marker about the stream itself, not about a container's
	// output (truncation).
	Note(reason string) error
}

// filter implements io.Writer: it splits the incoming byte stream into lines,
// applies the grep filter and the limits, and hands every surviving line to a
// sink. Splitting on bytes instead of using bufio.Scanner keeps an
// unterminated (and possibly huge) line from being buffered without bound.
type filter struct {
	sink      lineSink
	container string
	grep      string
	re        *regexp.Regexp
	maxBytes  int
	buf       []byte
	dropping  bool
	lines     int
	written   int
	truncated bool
}

func newFilter(opts Options, sink lineSink, maxBytes int) (*filter, error) {
	f := &filter{sink: sink, grep: opts.Grep, maxBytes: maxBytes}
	if opts.Kind == KindContainer {
		f.container = opts.Name
	}
	if opts.Regex && opts.Grep != "" {
		re, err := regexp.Compile(opts.Grep)
		if err != nil {
			return nil, err
		}
		f.re = re
	}
	return f, nil
}

func (f *filter) Write(p []byte) (int, error) {
	if f.truncated {
		return len(p), nil // keep draining; the answer is already cut
	}
	f.buf = append(f.buf, p...)
	for {
		if f.dropping {
			i := bytes.IndexByte(f.buf, '\n')
			if i < 0 {
				f.buf = f.buf[:0]
				return len(p), nil
			}
			f.buf = f.buf[i+1:]
			f.dropping = false
			continue
		}
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			if len(f.buf) > MaxLineBytes {
				// Unterminated line beyond the limit: emit the prefix once and
				// drop the rest as it arrives.
				cut := string(f.buf[:MaxLineBytes]) + truncatedLineMarker
				f.buf = f.buf[:0]
				f.dropping = true
				if err := f.line(cut); err != nil {
					return 0, err
				}
			}
			return len(p), nil
		}
		line := f.buf[:i]
		if len(line) > MaxLineBytes {
			line = []byte(string(line[:MaxLineBytes]) + truncatedLineMarker)
		}
		f.buf = f.buf[i+1:]
		if err := f.line(string(line)); err != nil {
			return 0, err
		}
		if f.truncated {
			return len(p), nil
		}
	}
}

// Flush drains whatever is still buffered: complete lines left over from a
// partial write (Write stops early when a sink rejects a line) followed by a
// trailing line that never got its newline.
func (f *filter) Flush() error {
	if _, err := f.Write(nil); err != nil {
		return err
	}
	if f.truncated || f.dropping || len(f.buf) == 0 {
		return nil
	}
	line := string(f.buf)
	f.buf = nil
	return f.line(line)
}

func (f *filter) line(raw string) error {
	if raw == "" {
		return nil
	}
	if !f.matches(raw) {
		return nil
	}
	if f.written+len(raw)+1 > f.maxBytes {
		f.truncated = true
		return f.sink.Note("truncated")
	}
	ts, msg := splitTimestamp(raw)
	if err := f.sink.Line(raw, ts, f.container, msg); err != nil {
		return err
	}
	f.lines++
	f.written += len(raw) + 1
	return nil
}

func (f *filter) matches(raw string) bool {
	if f.grep == "" {
		return true
	}
	if f.re != nil {
		return f.re.MatchString(raw)
	}
	return strings.Contains(raw, f.grep)
}

// splitTimestamp separates docker's --timestamps prefix from the message. A
// line without a recognizable prefix passes through untouched.
func splitTimestamp(raw string) (ts, msg string) {
	i := strings.IndexByte(raw, ' ')
	if i <= 0 {
		return "", raw
	}
	if _, err := time.Parse(time.RFC3339Nano, raw[:i]); err != nil {
		return "", raw
	}
	return raw[:i], raw[i+1:]
}

// writeSink writes lines to an io.Writer: raw in text mode, NDJSON in JSON
// mode. The truncation marker is only written in text mode — a JSON caller
// learns about truncation from the X-Dockops-Truncated header, and a plain
// marker line would break NDJSON parsing.
type writeSink struct {
	dst    io.Writer
	format Format
}

func (s *writeSink) Line(raw, ts, container, msg string) error {
	if s.format == FormatText {
		_, err := io.WriteString(s.dst, raw+"\n")
		return err
	}
	record, err := json.Marshal(struct {
		T string `json:"t"`
		C string `json:"c"`
		M string `json:"m"`
	}{ts, container, msg})
	if err != nil {
		return err
	}
	_, err = s.dst.Write(append(record, '\n'))
	return err
}

func (s *writeSink) Note(reason string) error {
	if s.format == FormatJSON {
		return nil
	}
	_, err := io.WriteString(s.dst, "… [dockops: "+reason+"]\n")
	return err
}
