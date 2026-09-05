package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/orchestrator"
)

// P9-20: `sc run --headless | head` used to kill the run where it stood.
//
// A write to a closed stdout raises SIGPIPE, which the Go runtime leaves at
// its default disposition for file descriptors 1 and 2, so the process died
// without releasing the repository's run lock or cleaning its worktrees —
// and the mess stood until the next run's stale-lock notice removed it. The
// signal is now asked for on a channel (so the write returns EPIPE instead)
// and the write error stops the run, which makes the shutdown the same one
// SIGTERM gets.

// TestRunHeadlessStopsCleanlyWhenItsOutputCloses is the Done-when. It needs
// a real process: only file descriptor 1 turns EPIPE into a signal, and
// this test binary's is the test framework's own.
func TestRunHeadlessStopsCleanlyWhenItsOutputCloses(t *testing.T) {
	env := runFireSetup(t, true)

	r, w, err := os.Pipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	// Autopilot, because it never stops on its own (D-18): whatever ends
	// this run, it is the closed pipe.
	child := runChildCommand(t, env.Repo.Dir, "--headless", "--cartridge", "mock")
	child.Stdout = w
	child.Stderr = &stderr
	require.NoError(t, child.Start())
	require.NoError(t, w.Close()) // the child owns the write end now
	t.Cleanup(func() { _ = child.Process.Kill() })

	rec := runChildAwaitLock(t, env.Repo.Dir, child.Process.Pid)
	require.NotEmpty(t, rec.Run)
	// One line is all this reader wanted, exactly like `| head -1`.
	require.NotEmpty(t, runChildFirstLine(t, r))
	require.NoError(t, r.Close())

	waited := make(chan error, 1)
	go func() { waited <- child.Wait() }()
	select {
	case err := <-waited:
		signalled, how := runChildSignalled(child)
		require.Falsef(t, signalled, "the run was killed (%s) instead of stopping: %s", how, stderr.String())
		require.NoErrorf(t, err, "stderr:\n%s", stderr.String())
	case <-time.After(runFireTimeout):
		t.Fatalf("the run did not end after its output closed\nstderr:\n%s", stderr.String())
	}

	// The two things the signal used to skip.
	probe := orchestrator.InspectLock(config.StateDir(env.Repo.Dir), orchestrator.LockOptions{})
	require.Equal(t, orchestrator.LockFree, probe.State, "the run lock was left behind")
	require.Contains(t, stderr.String(), "worktree", "the P6-18 worktree cleanup never ran:\n"+stderr.String())

	// And it ended as a stop rather than as a failure, so the run log says
	// what happened to it.
	sum, err := orchestrator.SummariseLog(config.RunDir(env.Repo.Dir, rec.Run.String()), -1)
	require.NoError(t, err)
	require.Equal(t, orchestrator.PhaseFinished, sum.Phase)
}

// TestClosedOutputWriterStopsTheRunOnce is the unit half: the wrapper turns
// the first broken write into a stop and then swallows everything, so a
// renderer mid-stream neither fails the run nor calls the stop again.
func TestClosedOutputWriterStopsTheRunOnce(t *testing.T) {
	var stops int
	var mu sync.Mutex
	broken := &pipeErrWriter{err: syscall.EPIPE}
	w := &closedOutputWriter{w: broken, onClosed: func(error) {
		mu.Lock()
		defer mu.Unlock()
		stops++
	}}

	n, err := w.Write([]byte("first line\n"))
	require.NoError(t, err, "the renderer is not failed for something the user did on purpose")
	require.Equal(t, len("first line\n"), n)
	n, err = w.Write([]byte("second\n"))
	require.NoError(t, err)
	require.Equal(t, len("second\n"), n)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, stops, "the run is stopped once, however much the renderer still had to say")
	require.Equal(t, 1, broken.writes, "nothing is written to a reader that has gone")
}

// TestClosedOutputWriterPassesEverythingElseThrough: an ordinary write, and
// an error that is not a closed reader, are the writer's own business.
func TestClosedOutputWriterPassesEverythingElseThrough(t *testing.T) {
	var buf bytes.Buffer
	w := &closedOutputWriter{w: &buf, onClosed: func(error) { t.Fatal("nothing was closed") }}
	_, err := io.WriteString(w, "hello\n")
	require.NoError(t, err)
	require.Equal(t, "hello\n", buf.String())

	boom := errors.New("disk on fire")
	failing := &closedOutputWriter{w: &pipeErrWriter{err: boom}, onClosed: func(error) { t.Fatal("nothing was closed") }}
	_, err = io.WriteString(failing, "hello\n")
	require.ErrorIs(t, err, boom, "a real write failure is still the caller's problem")
}

// TestIsClosedPipe names the two shapes a reader going away takes.
func TestIsClosedPipe(t *testing.T) {
	require.True(t, isClosedPipe(syscall.EPIPE))
	require.True(t, isClosedPipe(&os.PathError{Op: "write", Path: "/dev/stdout", Err: syscall.EPIPE}))
	require.True(t, isClosedPipe(os.ErrClosed))
	require.False(t, isClosedPipe(errors.New("write /dev/stdout: no space left on device")))
	require.False(t, isClosedPipe(context.Canceled))
	require.False(t, isClosedPipe(nil))
}

// TestRunIgnoreBrokenPipeRestoresTheDefault: the guard is installed for the
// run and taken off afterwards, so a process that embeds `sc run` keeps its
// own SIGPIPE handling.
func TestRunIgnoreBrokenPipeRestoresTheDefault(t *testing.T) {
	restore := runIgnoreBrokenPipe()
	require.NotNil(t, restore)
	restore()
	restore = runIgnoreBrokenPipe()
	restore()
}

// pipeErrWriter fails every write with err and counts the attempts.
type pipeErrWriter struct {
	err    error
	writes int
}

// Write implements [io.Writer].
func (p *pipeErrWriter) Write(_ []byte) (int, error) {
	p.writes++
	return 0, p.err
}

// runPipeStderrTail is the last few lines of a child's stderr, for a
// failure message that does not run to pages.
func runPipeStderrTail(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
	}
	return strings.Join(lines, "\n")
}
