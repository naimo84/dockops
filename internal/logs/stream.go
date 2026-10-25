package logs

import (
	"context"
	"encoding/json"
)

// Event is one server-sent event of a live stream.
type Event struct {
	// Name is "log" or "end"; the HTTP layer adds "ping" itself.
	Name string
	// Data is the payload: the raw line in text mode, the NDJSON object in
	// JSON mode, and {"reason":"…"} for the end event.
	Data []byte
}

// Stream follows the source and hands every surviving line to emit. emit
// blocks, so backpressure is the client's — there is no queue that could grow
// while a slow reader stalls. A non-nil source or emit error aborts the
// stream: the connection is gone or docker failed, and a clean "end" would be
// a lie. On a normal end the stream closes with an "end" event carrying the
// reason: "limit" when the byte limit stopped it, "docker_exit" when docker
// ended the follow.
//
// Time limits and ping keepalives are the HTTP layer's job — they need the
// ResponseWriter.
func Stream(ctx context.Context, src Source, opts Options, emit func(Event) error) error {
	opts.Follow = true
	f, err := newFilter(opts, &eventSink{emit: emit, format: opts.Format}, MaxStreamBytes)
	if err != nil {
		return err
	}
	srcErr := src.Logs(ctx, opts.Compose(), f)
	if srcErr == nil {
		srcErr = f.Flush()
	}
	if srcErr != nil {
		return srcErr
	}
	reason := "docker_exit"
	if f.truncated {
		reason = "limit"
	}
	end, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{reason})
	if err != nil {
		return err
	}
	return emit(Event{Name: "end", Data: end})
}

// eventSink turns filtered lines into log events.
type eventSink struct {
	emit   func(Event) error
	format Format
}

func (s *eventSink) Line(raw, ts, container, msg string) error {
	if s.format == FormatText {
		return s.emit(Event{Name: "log", Data: []byte(raw)})
	}
	record, err := json.Marshal(struct {
		T string `json:"t"`
		C string `json:"c"`
		M string `json:"m"`
	}{ts, container, msg})
	if err != nil {
		return err
	}
	return s.emit(Event{Name: "log", Data: record})
}

// Note is a no-op for streams: the byte limit turns into the end event's
// reason, and a marker line inside a live tail would be noise.
func (s *eventSink) Note(reason string) error { return nil }
