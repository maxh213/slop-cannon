package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/events"
)

// followPoll keeps the follower tests quick without busy-looping.
const followPoll = 5 * time.Millisecond

// followWait bounds every wait for the follower in these tests.
const followWait = 5 * time.Second

// followLine is ev as one line of the run log.
func followLine(t *testing.T, ev events.Event) []byte {
	t.Helper()
	b, err := events.Marshal(ev)
	require.NoError(t, err)
	return append(b, '\n')
}

// followAppend appends raw bytes to the file at path.
func followAppend(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // a test temp dir
	require.NoError(t, err)
	_, err = f.Write(b)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// followWrite appends evs to the file at path.
func followWrite(t *testing.T, path string, evs ...events.Event) {
	t.Helper()
	for _, ev := range evs {
		followAppend(t, path, followLine(t, ev))
	}
}

// followNext waits for the next event on ch.
func followNext(t *testing.T, ch <-chan events.Event) events.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		require.True(t, ok, "the stream closed early")
		return ev
	case <-time.After(followWait):
		t.Fatal("no event arrived")
		return nil
	}
}

// followTake waits for n events on ch.
func followTake(t *testing.T, ch <-chan events.Event, n int) []events.Event {
	t.Helper()
	out := make([]events.Event, 0, n)
	for range n {
		out = append(out, followNext(t, ch))
	}
	return out
}

// followClosed waits for ch to close with nothing more on it.
func followClosed(t *testing.T, ch <-chan events.Event) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		require.False(t, ok, "expected the stream to close, got %T", ev)
	case <-time.After(followWait):
		t.Fatal("the stream did not close")
	}
}

// followNothing asserts nothing arrives on ch for a few polls.
func followNothing(t *testing.T, ch <-chan events.Event) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		t.Fatalf("expected nothing, got %T (open %v)", ev, ok)
	case <-time.After(20 * followPoll):
	}
}

func TestFollowEventLogReadsTheBacklogThenFollows(t *testing.T) {
	dir := t.TempDir()
	path := EventLogPath(dir)
	run := elogRun()
	followWrite(t, path, run[:5]...)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := FollowEventLog(ctx, dir, FollowOptions{Poll: followPoll})

	require.Equal(t, run[:5], followTake(t, ch, 5))
	followNothing(t, ch)

	followWrite(t, path, run[5:]...)
	require.Equal(t, run[5:], followTake(t, ch, len(run)-5))
}

func TestFollowEventLogHoldsALineUntilItsNewlineLands(t *testing.T) {
	dir := t.TempDir()
	path := EventLogPath(dir)
	ev := elogRun()[0]
	line := followLine(t, ev)
	followAppend(t, path, line[:10])

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := FollowEventLog(ctx, dir, FollowOptions{Poll: followPoll})
	followNothing(t, ch)

	followAppend(t, path, line[10:])
	require.Equal(t, ev, followNext(t, ch))
}

func TestFollowEventLogClosesAfterTheRunFinishes(t *testing.T) {
	dir := t.TempDir()
	path := EventLogPath(dir)
	done := events.RunFinished{Meta: elogAt(99), Reason: events.FinishCompleted}
	followWrite(t, path, elogRun()[0], done)

	ch := FollowEventLog(context.Background(), dir, FollowOptions{Poll: followPoll})
	require.Equal(t, done, followTake(t, ch, 2)[1])
	followClosed(t, ch)
}

func TestFollowEventLogFollowsAcrossAResumeWithoutReplay(t *testing.T) {
	dir := t.TempDir()
	path := EventLogPath(dir)
	first, ok := elogRun()[0].(events.RunStarted)
	require.True(t, ok)
	finished := events.RunFinished{Meta: elogAt(99), Reason: events.FinishStopped}
	resumed := first
	resumed.Meta = elogAt(100)
	resumed.Resumed = true
	followWrite(t, path, first, finished, resumed)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := FollowEventLog(ctx, dir, FollowOptions{Poll: followPoll, FollowResumed: true})
	require.Equal(t, []events.Event{first, finished, resumed}, followTake(t, ch, 3))
	followNothing(t, ch)

	later := events.RunResumed{Meta: elogAt(101), Reason: "live append"}
	followWrite(t, path, later)
	require.Equal(t, later, followNext(t, ch))
	followNothing(t, ch)
}

func TestFollowEventLogClosesWhenTheWriterIsGone(t *testing.T) {
	dir := t.TempDir()
	path := EventLogPath(dir)
	run := elogRun()
	followWrite(t, path, run[0])
	var gone atomic.Bool

	ch := FollowEventLog(context.Background(), dir, FollowOptions{Poll: followPoll, Ended: gone.Load})
	require.Equal(t, run[0], followNext(t, ch))
	followNothing(t, ch)

	followWrite(t, path, run[1])
	gone.Store(true)
	require.Equal(t, run[1], followNext(t, ch))
	followClosed(t, ch)
}

func TestFollowEventLogReadsTheRotatedSegmentFirst(t *testing.T) {
	dir := t.TempDir()
	run := elogRun()
	followWrite(t, filepath.Join(dir, EventLogRotatedName), run[:3]...)
	followWrite(t, EventLogPath(dir), run[3:6]...)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := FollowEventLog(ctx, dir, FollowOptions{Poll: followPoll})
	require.Equal(t, run[:6], followTake(t, ch, 6))
}

func TestFollowEventLogFollowsARotation(t *testing.T) {
	dir := t.TempDir()
	path := EventLogPath(dir)
	run := elogRun()
	followWrite(t, path, run[:2]...)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := FollowEventLog(ctx, dir, FollowOptions{Poll: followPoll})
	require.Equal(t, run[:2], followTake(t, ch, 2))

	// The writer's last lines before the roll land in the old file, then it
	// renames that file aside and starts a fresh one.
	followWrite(t, path, run[2:4]...)
	require.NoError(t, os.Rename(path, filepath.Join(dir, EventLogRotatedName)))
	followWrite(t, path, run[4:6]...)

	require.Equal(t, run[2:6], followTake(t, ch, 4))
}

func TestFollowEventLogWaitsForTheLogToAppear(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := FollowEventLog(ctx, dir, FollowOptions{Poll: followPoll})
	followNothing(t, ch)

	ev := elogRun()[0]
	followWrite(t, EventLogPath(dir), ev)
	require.Equal(t, ev, followNext(t, ch))
}

func TestFollowEventLogGivesUpOnARunThatNeverWroteOne(t *testing.T) {
	ch := FollowEventLog(context.Background(), t.TempDir(), FollowOptions{
		Poll:  followPoll,
		Ended: func() bool { return true },
	})
	followClosed(t, ch)
}

func TestFollowEventLogSkipsLinesItCannotRead(t *testing.T) {
	dir := t.TempDir()
	path := EventLogPath(dir)
	run := elogRun()
	followWrite(t, path, run[0])
	followAppend(t, path, []byte("not json\n\n{\"kind\":\"from_the_future\"}\n"))
	followWrite(t, path, run[1])

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := FollowEventLog(ctx, dir, FollowOptions{Poll: followPoll})
	require.Equal(t, run[:2], followTake(t, ch, 2))
}

func TestFollowEventLogStopsWithItsContext(t *testing.T) {
	dir := t.TempDir()
	followWrite(t, EventLogPath(dir), elogRun()[0])
	ctx, cancel := context.WithCancel(context.Background())
	ch := FollowEventLog(ctx, dir, FollowOptions{Poll: followPoll})
	followNext(t, ch)
	cancel()
	followClosed(t, ch)
}
