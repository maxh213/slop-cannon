//go:build unix

package proc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sh builds a Spec that runs script with /bin/sh.
func sh(script string) Spec {
	return Spec{Command: "sh", Args: []string{"-c", script}}
}

func TestRun_Success(t *testing.T) {
	t.Parallel()
	res, err := Run(context.Background(), sh("echo out; echo err >&2"))
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, "out\n", res.Stdout)
	assert.Equal(t, "err\n", res.Stderr)
	assert.Positive(t, res.PID)
	assert.Positive(t, res.Duration)
	assert.False(t, res.Started.IsZero())
	assert.False(t, res.TimedOut)
	assert.False(t, res.Cancelled)
}

func TestRun_NonZeroExit(t *testing.T) {
	t.Parallel()
	res, err := Run(context.Background(), sh("echo partial; echo boom >&2; exit 3"))
	require.Error(t, err)
	var ee *ExitError
	require.ErrorAs(t, err, &ee)
	assert.Equal(t, 3, res.ExitCode)
	assert.Equal(t, 3, ee.Result.ExitCode)
	assert.Equal(t, "partial\n", res.Stdout)
	assert.Equal(t, "boom\n", ee.Result.Stderr)
	assert.NoError(t, ee.Cause)
	assert.Equal(t, "proc: sh -c echo partial; echo boom >&2; exit 3: exit status 3: boom", err.Error())
	assert.False(t, res.TimedOut)
	assert.False(t, res.Cancelled)
}

func TestRun_Stdin(t *testing.T) {
	t.Parallel()
	res, err := Run(context.Background(), Spec{Command: "cat", Stdin: strings.NewReader("hello\nworld")})
	require.NoError(t, err)
	assert.Equal(t, "hello\nworld", res.Stdout)
}

func TestRun_NoStdinReadsEOF(t *testing.T) {
	t.Parallel()
	res, err := Run(context.Background(), Spec{Command: "cat", Timeout: 5 * time.Second})
	require.NoError(t, err)
	assert.Empty(t, res.Stdout)
}

func TestRun_Dir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	res, err := Run(context.Background(), Spec{Command: "pwd", Dir: dir})
	require.NoError(t, err)
	got, err := filepath.EvalSymlinks(strings.TrimSpace(res.Stdout))
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestRun_BadDir(t *testing.T) {
	t.Parallel()
	_, err := Run(context.Background(), Spec{Command: "pwd", Dir: filepath.Join(t.TempDir(), "missing")})
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist), "got %v", err)
}

func TestRun_EnvAllowlist(t *testing.T) {
	t.Setenv("SC_TEST_SECRET", "leak")
	t.Setenv("SC_TEST_KEEP", "kept")
	t.Setenv("SC_TEST_OVERRIDE", "parent")
	spec := Spec{
		Command:    "env",
		InheritEnv: []string{"SC_TEST_KEEP", "SC_TEST_OVERRIDE", "SC_TEST_UNSET"},
		Env:        map[string]string{"SC_TEST_EXPLICIT": "yes", "SC_TEST_OVERRIDE": "child"},
	}
	res, err := Run(context.Background(), spec)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	got := map[string]string{}
	for _, l := range lines {
		k, v, _ := strings.Cut(l, "=")
		got[k] = v
	}
	assert.Equal(t, "kept", got["SC_TEST_KEEP"])
	assert.Equal(t, "yes", got["SC_TEST_EXPLICIT"])
	assert.Equal(t, "child", got["SC_TEST_OVERRIDE"])
	assert.NotEmpty(t, got["PATH"])
	_, leaked := got["SC_TEST_SECRET"]
	assert.False(t, leaked, "unlisted variable reached the child: %v", lines)
	_, unset := got["SC_TEST_UNSET"]
	assert.False(t, unset, "unset variable should not be passed as empty")
	assert.Len(t, got, 4, "only PATH and the allowlisted variables should be present: %v", lines)
}

func TestRun_Timeout(t *testing.T) {
	t.Parallel()
	spec := sh("sleep 30")
	spec.Timeout = 200 * time.Millisecond
	begin := time.Now()
	res, err := Run(context.Background(), spec)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTimeout), "got %v", err)
	assert.False(t, errors.Is(err, context.Canceled))
	var ee *ExitError
	require.ErrorAs(t, err, &ee)
	assert.True(t, res.TimedOut)
	assert.False(t, res.Cancelled)
	assert.Equal(t, -1, res.ExitCode)
	assert.Less(t, time.Since(begin), 5*time.Second)
	assert.Less(t, res.Duration, 5*time.Second)
	assert.Contains(t, err.Error(), "timed out")
}

func TestRun_TimeoutWithGracePeriodLetsChildExitCleanly(t *testing.T) {
	t.Parallel()
	// The child handles SIGTERM and exits 42; with a generous grace period it
	// must not be SIGKILLed first.
	spec := sh(`trap 'exit 42' TERM; while :; do sleep 0.05; done`)
	spec.Timeout = 200 * time.Millisecond
	spec.GracePeriod = 5 * time.Second
	res, err := Run(context.Background(), spec)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTimeout), "got %v", err)
	assert.Equal(t, 42, res.ExitCode)
	assert.True(t, res.TimedOut)
	assert.Less(t, res.Duration, 3*time.Second)
}

func TestRun_GracePeriodExpiresIntoSIGKILL(t *testing.T) {
	t.Parallel()
	// SIGTERM is ignored (and inherited as ignored by sleep), so only the
	// SIGKILL that follows the grace period can end the group.
	spec := sh(`trap '' TERM; sleep 30`)
	spec.Timeout = 100 * time.Millisecond
	spec.GracePeriod = 200 * time.Millisecond
	begin := time.Now()
	res, err := Run(context.Background(), spec)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTimeout), "got %v", err)
	assert.Equal(t, -1, res.ExitCode)
	assert.Less(t, time.Since(begin), 5*time.Second)
}

func TestRun_CancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	res, err := Run(ctx, sh("sleep 30"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "got %v", err)
	assert.True(t, res.Cancelled)
	assert.False(t, res.TimedOut)
	assert.Equal(t, -1, res.ExitCode)
	assert.Less(t, res.Duration, 5*time.Second)
}

func TestRun_ContextCancelledBeforeStart(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, sh("echo never"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "got %v", err)
}

func TestStream_CancelKillsGrandchild(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// sh is the child; sleep is the grandchild. It inherits our stdout pipe,
	// so if it survived the kill Wait would stall on the open pipe.
	h, err := Stream(ctx, sh("sleep 30 & echo $!; wait"))
	require.NoError(t, err)

	var grandchild int
	select {
	case l := <-h.Lines():
		grandchild, err = strconv.Atoi(strings.TrimSpace(l.Text))
		require.NoError(t, err, "expected the grandchild pid, got %q", l.Text)
	case <-time.After(5 * time.Second):
		t.Fatal("never received the grandchild pid")
	}
	require.True(t, processAlive(grandchild), "grandchild %d should be running before cancel", grandchild)

	begin := time.Now()
	cancel()
	res, err := h.Wait()
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "got %v", err)
	assert.True(t, res.Cancelled)
	assert.Less(t, time.Since(begin), 3*time.Second, "Wait must not stall on the grandchild's pipe")

	deadline := time.Now().Add(3 * time.Second)
	for processAlive(grandchild) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	assert.False(t, processAlive(grandchild), "grandchild %d survived the group kill", grandchild)
	for range h.Lines() {
		// drain
	}
}

func TestRun_OrphanHoldingPipeDoesNotStallAndIsKilled(t *testing.T) {
	t.Parallel()
	// sh exits at once, but the backgrounded sleep inherits the stdout pipe.
	// Run must return after the exit (bounded by pipeWaitDelay), report the
	// clean exit, and kill the straggler.
	begin := time.Now()
	res, err := Run(context.Background(), sh("sleep 30 & echo $!"))
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Less(t, time.Since(begin), 5*time.Second)
	grandchild, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
	require.NoError(t, err, "expected the grandchild pid, got %q", res.Stdout)
	deadline := time.Now().Add(3 * time.Second)
	for processAlive(grandchild) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	assert.False(t, processAlive(grandchild), "orphaned grandchild %d survived", grandchild)
}

func TestStream_Interleaving(t *testing.T) {
	t.Parallel()
	script := "echo o1; sleep 0.1; echo e1 >&2; sleep 0.1; echo o2; sleep 0.1; echo e2 >&2"
	h, err := Stream(context.Background(), sh(script))
	require.NoError(t, err)
	var got []string
	var last time.Time
	for l := range h.Lines() {
		got = append(got, l.Source.String()+":"+l.Text)
		assert.False(t, l.At.Before(last), "timestamps must not go backwards")
		last = l.At
	}
	assert.Equal(t, []string{"stdout:o1", "stderr:e1", "stdout:o2", "stderr:e2"}, got)
	res, err := h.Wait()
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Empty(t, res.Stdout, "Stream does not capture output into Result")
	assert.Equal(t, h.PID(), res.PID)
}

func TestStream_PerSourceOrderUnderConcurrentWriters(t *testing.T) {
	t.Parallel()
	const n = 500
	script := fmt.Sprintf(
		"(i=1; while [ $i -le %d ]; do echo o$i; i=$((i+1)); done) & "+
			"i=1; while [ $i -le %d ]; do echo e$i >&2; i=$((i+1)); done; wait", n, n)
	h, err := Stream(context.Background(), sh(script))
	require.NoError(t, err)
	var outs, errs []string
	for l := range h.Lines() {
		switch l.Source {
		case Stdout:
			outs = append(outs, l.Text)
		case Stderr:
			errs = append(errs, l.Text)
		}
	}
	_, err = h.Wait()
	require.NoError(t, err)
	require.Len(t, outs, n)
	require.Len(t, errs, n)
	for i := 0; i < n; i++ {
		assert.Equal(t, fmt.Sprintf("o%d", i+1), outs[i])
		assert.Equal(t, fmt.Sprintf("e%d", i+1), errs[i])
	}
}

func TestStream_PartialLastLineAndCRLF(t *testing.T) {
	t.Parallel()
	h, err := Stream(context.Background(), Spec{Command: "printf", Args: []string{`a\r\nb\n\nc`}})
	require.NoError(t, err)
	var got []string
	for l := range h.Lines() {
		got = append(got, l.Text)
	}
	assert.Equal(t, []string{"a", "b", "", "c"}, got)
	_, err = h.Wait()
	require.NoError(t, err)
}

func TestStream_LongLine(t *testing.T) {
	t.Parallel()
	const size = 300_000 // well past bufio.Scanner's default 64 KiB limit
	h, err := Stream(context.Background(), sh(fmt.Sprintf("head -c %d /dev/zero | tr '\\0' x; echo; echo tail", size)))
	require.NoError(t, err)
	var got []string
	for l := range h.Lines() {
		got = append(got, l.Text)
	}
	require.Len(t, got, 2)
	assert.Len(t, got[0], size)
	assert.Equal(t, strings.Repeat("x", 10), got[0][:10])
	assert.Equal(t, "tail", got[1])
	_, err = h.Wait()
	require.NoError(t, err)
}

func TestStream_Kill(t *testing.T) {
	t.Parallel()
	h, err := Stream(context.Background(), sh("echo started; sleep 30"))
	require.NoError(t, err)
	select {
	case l := <-h.Lines():
		assert.Equal(t, "started", l.Text)
	case <-time.After(5 * time.Second):
		t.Fatal("no output")
	}
	h.Kill()
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done never closed after Kill")
	}
	res, err := h.Wait()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrKilled), "got %v", err)
	assert.True(t, res.Cancelled)
	assert.Equal(t, -1, res.ExitCode)
	// Wait is idempotent.
	res2, err2 := h.Wait()
	assert.Equal(t, res, res2)
	assert.Equal(t, err, err2)
	for range h.Lines() {
		// drain
	}
}

func TestStream_WaitBeforeDrainingDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	const n = 20_000
	h, err := Stream(context.Background(), sh(fmt.Sprintf("i=1; while [ $i -le %d ]; do echo line$i; i=$((i+1)); done", n)))
	require.NoError(t, err)
	res, err := h.Wait()
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	count := 0
	for l := range h.Lines() {
		count++
		assert.Equal(t, fmt.Sprintf("line%d", count), l.Text)
	}
	assert.Equal(t, n, count)
}

func TestStream_NonZeroExit(t *testing.T) {
	t.Parallel()
	h, err := Stream(context.Background(), sh("echo nope >&2; exit 7"))
	require.NoError(t, err)
	var lines []Line
	for l := range h.Lines() {
		lines = append(lines, l)
	}
	require.Len(t, lines, 1)
	assert.Equal(t, Stderr, lines[0].Source)
	res, err := h.Wait()
	var ee *ExitError
	require.ErrorAs(t, err, &ee)
	assert.Equal(t, 7, res.ExitCode)
	assert.Equal(t, "proc: sh -c echo nope >&2; exit 7: exit status 7", err.Error())
}

func TestRun_NotFound(t *testing.T) {
	t.Parallel()
	_, err := Run(context.Background(), Spec{Command: "sc-definitely-not-a-real-tool"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, exec.ErrNotFound), "got %v", err)
	assert.True(t, IsNotFound(err))
	_, err = Stream(context.Background(), Spec{Command: "sc-definitely-not-a-real-tool"})
	assert.True(t, IsNotFound(err), "got %v", err)
}

func TestRun_InvalidSpec(t *testing.T) {
	t.Parallel()
	_, err := Run(context.Background(), Spec{})
	assert.EqualError(t, err, "proc: empty command")
	_, err = Run(context.Background(), Spec{Command: "true", Timeout: -1})
	assert.ErrorContains(t, err, "negative timeout")
	_, err = Run(context.Background(), Spec{Command: "true", GracePeriod: -1})
	assert.ErrorContains(t, err, "negative grace period")
}

func TestExitError_Error(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  ExitError
		want string
	}{
		{"exit code", ExitError{Command: "git", Args: []string{"status"}, Result: Result{ExitCode: 128}}, "proc: git status: exit status 128"},
		{"stderr tail", ExitError{Command: "bd", Result: Result{ExitCode: 1, Stderr: "first\nfatal: nope\n\n"}}, "proc: bd: exit status 1: fatal: nope"},
		{"signal", ExitError{Command: "x", Result: Result{ExitCode: -1}}, "proc: x: killed by signal"},
		{"cause", ExitError{Command: "x", Result: Result{ExitCode: -1}, Cause: ErrTimeout}, "proc: x: proc: timed out"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.err.Error())
		})
	}
	assert.True(t, errors.Is(&ExitError{Cause: ErrKilled}, ErrKilled))
}

func TestSource_String(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "stdout", Stdout.String())
	assert.Equal(t, "stderr", Stderr.String())
}

// processAlive reports whether pid still exists as a running (non-zombie)
// process. Orphaned grandchildren may linger as zombies when nothing reaps
// them (containers without an init), so a zombie counts as gone.
func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	if runtime.GOOS == "linux" {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return false
		}
		// The state field follows the parenthesised command name.
		if i := strings.LastIndexByte(string(stat), ')'); i >= 0 && i+2 < len(stat) {
			return stat[i+2] != 'Z'
		}
	}
	return true
}
