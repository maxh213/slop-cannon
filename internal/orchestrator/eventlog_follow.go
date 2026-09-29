package orchestrator

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"time"

	"slop-cannon/internal/events"
)

// DefaultFollowPoll is how often [FollowEventLog] looks for new lines once
// it has read everything the run has written so far.
const DefaultFollowPoll = 200 * time.Millisecond

// FollowOptions tune [FollowEventLog].
type FollowOptions struct {
	// Poll is how long the follower waits at the end of the file before it
	// looks again; zero means [DefaultFollowPoll].
	Poll time.Duration
	// Ended reports that nothing will write to the log again: the run's
	// process is gone. The follower asks it only at the end of the file, and
	// once it says true the follower drains what is left and closes the
	// stream. Nil means the run ends only with its [events.RunFinished].
	Ended func() bool
}

// FollowEventLog streams a run's log as it is written: the rotated segment
// (P6-33) and everything already in the live file first, then every event
// the run appends, the way `tail -F` follows a file. It is how `sc watch`
// puts a TUI on a run another process is firing.
//
// The stream closes when ctx is done, after the run's [events.RunFinished],
// or when opts.Ended reports the writer gone and the file is drained. A log
// that does not exist yet is waited for. A line that does not parse is
// skipped, and a line still being written is held until its newline lands.
// A rotation mid-follow is followed onto the new file, after the rest of the
// old one is read, so no event is lost to it.
func FollowEventLog(ctx context.Context, runDir string, opts FollowOptions) <-chan events.Event {
	f := &follower{
		dir:   filepath.Clean(runDir),
		poll:  opts.Poll,
		ended: opts.Ended,
		out:   make(chan events.Event, 256),
	}
	if f.poll <= 0 {
		f.poll = DefaultFollowPoll
	}
	if f.ended == nil {
		f.ended = func() bool { return false }
	}
	go func() {
		defer close(f.out)
		f.run(ctx)
	}()
	return f.out
}

// follower is the state of one [FollowEventLog] stream.
type follower struct {
	dir      string
	poll     time.Duration
	ended    func() bool
	out      chan events.Event
	finished bool
}

// run is the follower's goroutine body.
func (f *follower) run(ctx context.Context) {
	live := filepath.Join(f.dir, EventLogName)
	file, ok := f.waitForLog(ctx, live)
	if !ok {
		return
	}
	defer func() { _ = file.Close() }()
	r := bufio.NewReader(file)
	var partial []byte
	for {
		if !f.drain(ctx, r, &partial) || f.finished {
			return
		}
		if next, rotated := f.rotated(file, live); rotated {
			if !f.drain(ctx, r, &partial) || f.finished {
				_ = next.Close()
				return
			}
			_ = file.Close()
			file, r, partial = next, bufio.NewReader(next), nil
			continue
		}
		if f.ended() {
			f.drain(ctx, r, &partial)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(f.poll):
		}
	}
}

// waitForLog replays the rotated segment when there is one and opens the
// live file, waiting for it to appear. It gives up when ctx is done or the
// run ended without ever writing one.
func (f *follower) waitForLog(ctx context.Context, live string) (*os.File, bool) {
	for {
		file, err := os.Open(live) //nolint:gosec // the run directory sc owns (D-23).
		if err == nil {
			if !f.replayRotated(ctx) {
				_ = file.Close()
				return nil, false
			}
			return file, true
		}
		if f.ended() {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(f.poll):
		}
	}
}

// replayRotated emits the whole rotated segment, if there is one. It is read
// before the live file because it holds the older events.
func (f *follower) replayRotated(ctx context.Context) bool {
	file, err := os.Open(filepath.Join(f.dir, EventLogRotatedName)) //nolint:gosec // the run directory sc owns (D-23).
	if err != nil {
		return true
	}
	defer func() { _ = file.Close() }()
	var partial []byte
	return f.drain(ctx, bufio.NewReader(file), &partial)
}

// rotated reports whether the live path now names a different file from the
// one being read, and opens that new file when it does.
func (f *follower) rotated(file *os.File, live string) (*os.File, bool) {
	cur, err := file.Stat()
	if err != nil {
		return nil, false
	}
	now, err := os.Stat(live)
	if err != nil || os.SameFile(cur, now) {
		return nil, false
	}
	next, err := os.Open(live) //nolint:gosec // the run directory sc owns (D-23).
	if err != nil {
		return nil, false
	}
	return next, true
}

// drain emits every complete line r holds, keeping a trailing line that has
// no newline yet in partial. It returns false when ctx ended the stream.
func (f *follower) drain(ctx context.Context, r *bufio.Reader, partial *[]byte) bool {
	for {
		chunk, err := r.ReadBytes('\n')
		*partial = append(*partial, chunk...)
		if err != nil {
			// io.EOF is the end of what has been written so far; any other
			// read error is retried on the next poll the same way.
			return ctx.Err() == nil
		}
		line := bytes.TrimSpace(*partial)
		*partial = (*partial)[:0]
		if !f.emit(ctx, line) {
			return false
		}
	}
}

// emit publishes one line of the log, skipping a blank or unreadable one.
func (f *follower) emit(ctx context.Context, line []byte) bool {
	if len(line) == 0 {
		return true
	}
	ev, err := events.Unmarshal(line)
	if err != nil {
		return true
	}
	if _, ok := ev.(events.RunFinished); ok {
		f.finished = true
	}
	select {
	case f.out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}
