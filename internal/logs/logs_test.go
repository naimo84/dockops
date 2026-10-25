package logs

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/local/dockops/internal/compose"
)

// fakeSource feeds a canned log stream and records the request it saw.
type fakeSource struct {
	output   string
	err      error
	got      compose.LogsOptions
	requests int
}

func (f *fakeSource) Logs(ctx context.Context, opts compose.LogsOptions, w io.Writer) error {
	f.got = opts
	f.requests++
	if f.err != nil {
		return f.err
	}
	_, err := io.WriteString(w, f.output)
	return err
}

func (f *fakeSource) PsContainers(ctx context.Context) ([]compose.ContainerInfo, error) {
	return nil, nil
}

func snapshotOpts() Options {
	return Options{Kind: KindContainer, Name: "n8n-1", Since: time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC), Tail: 200}
}

func TestCopyTextKeepsRawLines(t *testing.T) {
	src := &fakeSource{output: "2026-10-25T08:00:01.123456789Z hello\n2026-10-25T08:00:02Z world\n"}
	var buf strings.Builder
	lines, truncated, err := Copy(context.Background(), src, snapshotOpts(), &buf)
	if err != nil {
		t.Fatalf("Copy() error = %v", err)
	}
	if lines != 2 || truncated {
		t.Errorf("lines/truncated = %d/%v, want 2/false", lines, truncated)
	}
	if want := "2026-10-25T08:00:01.123456789Z hello\n2026-10-25T08:00:02Z world\n"; buf.String() != want {
		t.Errorf("body = %q, want %q", buf.String(), want)
	}
	if src.got.Container != "n8n-1" || src.got.Project != "" || src.got.Tail != 200 {
		t.Errorf("source saw %+v, want container n8n-1 tail 200", src.got)
	}
}

func TestCopyJSONSplitsTimestamp(t *testing.T) {
	src := &fakeSource{output: "2026-10-25T08:00:01.123456789Z Editor is now accessible\nno-timestamp-line\n"}
	var buf strings.Builder
	opts := snapshotOpts()
	opts.Format = FormatJSON
	if _, _, err := Copy(context.Background(), src, opts, &buf); err != nil {
		t.Fatalf("Copy() error = %v", err)
	}
	want := "{\"t\":\"2026-10-25T08:00:01.123456789Z\",\"c\":\"n8n-1\",\"m\":\"Editor is now accessible\"}\n" +
		"{\"t\":\"\",\"c\":\"n8n-1\",\"m\":\"no-timestamp-line\"}\n"
	if buf.String() != want {
		t.Errorf("body = %q, want %q", buf.String(), want)
	}
}

func TestCopyGrepLiteralAndRegex(t *testing.T) {
	output := "2026-10-25T08:00:01Z ERROR boom\n2026-10-25T08:00:02Z INFO fine\n2026-10-25T08:00:03Z error lower\n"
	cases := []struct {
		name string
		opts func(Options) Options
		want string
	}{
		{
			name: "literal case sensitive",
			opts: func(o Options) Options { o.Grep = "ERROR"; return o },
			want: "2026-10-25T08:00:01Z ERROR boom\n",
		},
		{
			name: "regex case insensitive",
			opts: func(o Options) Options { o.Grep = "(?i)error"; o.Regex = true; return o },
			want: "2026-10-25T08:00:01Z ERROR boom\n2026-10-25T08:00:03Z error lower\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{output: output}
			var buf strings.Builder
			if _, _, err := Copy(context.Background(), src, tc.opts(snapshotOpts()), &buf); err != nil {
				t.Fatalf("Copy() error = %v", err)
			}
			if buf.String() != tc.want {
				t.Errorf("body = %q, want %q", buf.String(), tc.want)
			}
		})
	}
}

func TestCopyTruncatesAtByteLimit(t *testing.T) {
	line := "2026-10-25T08:00:01Z " + strings.Repeat("x", 200) + "\n" // ~223 bytes
	const total = 12000                                               // ~2.7 MB, above MaxBytes
	src := &fakeSource{output: strings.Repeat(line, total)}
	var buf strings.Builder
	lines, truncated, err := Copy(context.Background(), src, snapshotOpts(), &buf)
	if err != nil {
		t.Fatalf("Copy() error = %v", err)
	}
	if !truncated {
		t.Fatal("truncated = false, want true at the byte limit")
	}
	if buf.Len() > MaxBytes+64 {
		t.Errorf("body = %d bytes, want <= %d plus marker", buf.Len(), MaxBytes)
	}
	if lines == 0 || lines >= total {
		t.Errorf("lines = %d, want 1..%d", lines, total-1)
	}
	if !strings.HasSuffix(buf.String(), "… [dockops: truncated]\n") {
		t.Errorf("body does not end with the truncation marker")
	}
}

// TestCopyCutsUnterminatedLongLine pins Review Focus #2: a container writing a
// single huge line without a newline must not buffer without bound.
func TestCopyCutsUnterminatedLongLine(t *testing.T) {
	src := &fakeSource{output: "2026-10-25T08:00:01Z " + strings.Repeat("y", 3*MaxLineBytes)}
	var buf strings.Builder
	lines, _, err := Copy(context.Background(), src, snapshotOpts(), &buf)
	if err != nil {
		t.Fatalf("Copy() error = %v", err)
	}
	if lines != 1 {
		t.Errorf("lines = %d, want exactly 1 (the cut prefix)", lines)
	}
	if buf.Len() > MaxLineBytes+128 {
		t.Errorf("body = %d bytes, want around MaxLineBytes", buf.Len())
	}
	if !strings.Contains(buf.String(), "line truncated") {
		t.Errorf("body lacks the line-truncation marker")
	}
}

func TestCopyPropagatesSourceError(t *testing.T) {
	src := &fakeSource{err: errors.New("No such container: nope")}
	var buf strings.Builder
	if _, _, err := Copy(context.Background(), src, snapshotOpts(), &buf); err == nil {
		t.Fatal("Copy() error = nil, want the source error")
	}
}

func TestStreamEmitsLogAndEndEvents(t *testing.T) {
	src := &fakeSource{output: "2026-10-25T08:00:01Z first\n2026-10-25T08:00:02Z second\n"}
	var got []Event
	err := Stream(context.Background(), src, snapshotOpts(), func(ev Event) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("events = %d, want 3 (two lines + end): %+v", len(got), got)
	}
	if got[0].Name != "log" || string(got[0].Data) != "2026-10-25T08:00:01Z first" {
		t.Errorf("event 0 = %+v, want log line", got[0])
	}
	if got[2].Name != "end" || string(got[2].Data) != `{"reason":"docker_exit"}` {
		t.Errorf("event 2 = %+v, want end{docker_exit}", got[2])
	}
	if !src.got.Follow {
		t.Error("Stream() did not request follow mode")
	}
}

func TestStreamJSONPayload(t *testing.T) {
	src := &fakeSource{output: "2026-10-25T08:00:01Z hello\n"}
	var got []Event
	opts := snapshotOpts()
	opts.Format = FormatJSON
	if err := Stream(context.Background(), src, opts, func(ev Event) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	want := `{"t":"2026-10-25T08:00:01Z","c":"n8n-1","m":"hello"}`
	if len(got) != 2 || string(got[0].Data) != want {
		t.Errorf("events = %+v, want one log event with data %s", got, want)
	}
}

func TestStreamStopsAtByteLimit(t *testing.T) {
	line := "2026-10-25T08:00:01Z " + strings.Repeat("z", 500) + "\n" // ~523 bytes
	const total = 25000                                               // ~13 MB, above MaxStreamBytes
	src := &fakeSource{output: strings.Repeat(line, total)}
	events := 0
	var last Event
	err := Stream(context.Background(), src, snapshotOpts(), func(ev Event) error {
		events++
		last = ev
		return nil
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if last.Name != "end" || !strings.Contains(string(last.Data), "limit") {
		t.Errorf("last event = %+v, want end{limit}", last)
	}
	if events >= total {
		t.Errorf("events = %d, want the byte limit to cut the stream", events)
	}
}

func TestStreamPropagatesEmitError(t *testing.T) {
	src := &fakeSource{output: "2026-10-25T08:00:01Z a\n2026-10-25T08:00:02Z b\n"}
	stop := errors.New("client gone")
	calls := 0
	err := Stream(context.Background(), src, snapshotOpts(), func(ev Event) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("Stream() error = %v, want %v", err, stop)
	}
	if calls != 1 {
		t.Errorf("emit calls = %d, want 1 (stop at the first error)", calls)
	}
}
