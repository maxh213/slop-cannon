package events

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// JSONLWriter appends events to an io.Writer as newline-delimited JSON, one
// [Marshal] envelope per line, and never buffers: each Write reaches the
// underlying writer before it returns, so a run log on disk is complete up to
// the last event even if the process dies (D-15). It is safe for concurrent
// use; the first write error is sticky and returned by every later call.
type JSONLWriter struct {
	mu  sync.Mutex
	w   io.Writer
	err error
	n   uint64
}

// NewJSONLWriter wraps w. Pass an *os.File opened with O_APPEND for a run log.
func NewJSONLWriter(w io.Writer) *JSONLWriter {
	return &JSONLWriter{w: w}
}

// Write encodes one event and writes it followed by a newline. After the
// first failure every call returns that same error without writing.
func (j *JSONLWriter) Write(ev Event) error {
	line, err := Marshal(ev)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return j.err
	}
	if _, err := j.w.Write(line); err != nil {
		j.err = fmt.Errorf("events: jsonl write: %w", err)
		return j.err
	}
	j.n++
	return nil
}

// Err returns the sticky write error, if any.
func (j *JSONLWriter) Err() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.err
}

// Written returns how many events have been written successfully.
func (j *JSONLWriter) Written() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.n
}

// Serve subscribes to bus with the given buffer and writes every event it
// receives until ctx is done, the bus is closed, or a write fails. It returns
// nil when the bus closed, ctx.Err() when the context ended, and the write
// error otherwise. Events the subscription dropped are reported through the
// returned *Subscription's Dropped counter; Serve does not stop for them.
//
// Serve blocks, so call it in its own goroutine:
//
//	go func() { errc <- w.Serve(ctx, bus, 1024) }()
func (j *JSONLWriter) Serve(ctx context.Context, bus *Bus, buffer int) error {
	sub := bus.Subscribe(ctx, buffer)
	defer sub.Unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-sub.Events():
			if !ok {
				if err := ctx.Err(); err != nil {
					return err
				}
				return nil
			}
			if err := j.Write(ev); err != nil {
				return err
			}
		}
	}
}

// JSONLReader reads events back from a stream written by [JSONLWriter].
// Blank lines are skipped. A line that is not a valid envelope stops the
// reader with an error that names the line number; the caller may keep
// calling Next to skip past it.
type JSONLReader struct {
	sc   *bufio.Scanner
	line int
}

// NewJSONLReader wraps r. Lines up to 16 MiB are accepted, which covers a
// GraphChanged snapshot of any realistic run.
func NewJSONLReader(r io.Reader) *JSONLReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	return &JSONLReader{sc: sc}
}

// Next returns the next event, or io.EOF when the stream is exhausted.
func (r *JSONLReader) Next() (Event, error) {
	for r.sc.Scan() {
		r.line++
		b := bytes.TrimSpace(r.sc.Bytes())
		if len(b) == 0 {
			continue
		}
		ev, err := Unmarshal(b)
		if err != nil {
			return nil, fmt.Errorf("events: jsonl line %d: %w", r.line, err)
		}
		return ev, nil
	}
	if err := r.sc.Err(); err != nil {
		return nil, fmt.Errorf("events: jsonl read: %w", err)
	}
	return nil, io.EOF
}

// Line returns the number of the last line consumed, for diagnostics.
func (r *JSONLReader) Line() int { return r.line }

// ReadAll reads every event from r. It returns the events read so far along
// with the first error other than io.EOF.
func ReadAll(r io.Reader) ([]Event, error) {
	jr := NewJSONLReader(r)
	var out []Event
	for {
		ev, err := jr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
}
