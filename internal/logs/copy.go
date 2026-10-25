package logs

import (
	"context"
	"io"

	"github.com/local/dockops/internal/compose"
)

// Source is everything the log API needs from the docker side. The real
// implementation is *compose.Executor; tests inject a fake.
type Source interface {
	Logs(ctx context.Context, opts compose.LogsOptions, w io.Writer) error
	PsContainers(ctx context.Context) ([]compose.ContainerInfo, error)
}

// Copy reads one snapshot from src, applies the grep filter and the byte
// limit, and writes the result to dst. The read is streamed through the
// filter rather than buffered: the byte limit bounds the answer, not the
// container's history.
func Copy(ctx context.Context, src Source, opts Options, dst io.Writer) (lines int, truncated bool, err error) {
	f, err := newFilter(opts, &writeSink{dst: dst, format: opts.Format}, MaxBytes)
	if err != nil {
		return 0, false, err
	}
	err = src.Logs(ctx, opts.Compose(), f)
	if err == nil {
		// Only a completed read gets flushed: an aborted one already stopped
		// mid-answer, and flushing would append a truncated tail (or repeat a
		// line) to a response the caller has already given up on.
		err = f.Flush()
	}
	return f.lines, f.truncated, err
}
