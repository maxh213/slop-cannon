package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrTimeout is the cause reported when a process is killed because
// [Spec.Timeout] elapsed. Test with errors.Is.
var ErrTimeout = errors.New("proc: timed out")

// ErrKilled is the cause reported when a streamed process is stopped with
// [Handle.Kill]. Test with errors.Is.
var ErrKilled = errors.New("proc: killed")

// pipeWaitDelay bounds how long Wait blocks on stdout/stderr pipes that an
// orphaned grandchild keeps open after the direct child has exited (or after
// the grace period on cancellation). Once it expires the pipes are closed and
// the process group is killed.
const pipeWaitDelay = 500 * time.Millisecond

// Spec describes one subprocess invocation.
type Spec struct {
	// Command is the program to run. It is resolved on PATH the same way
	// exec.LookPath does; a path containing a separator is used as-is.
	Command string
	// Args are the arguments, not including the program name.
	Args []string
	// Dir is the working directory. Empty means the parent's cwd.
	Dir string
	// Env holds variables set explicitly on the child. These override
	// inherited values of the same name.
	Env map[string]string
	// InheritEnv names parent variables to pass through unchanged when they
	// are set. PATH is always inherited and need not be listed. Anything not
	// listed here or in Env is withheld from the child.
	InheritEnv []string
	// Stdin is connected to the child's standard input. nil means no input
	// (the child reads EOF immediately).
	Stdin io.Reader
	// Timeout kills the process group when it elapses. Zero means no limit.
	Timeout time.Duration
	// GracePeriod is the time between the polite stop signal (SIGTERM on
	// Unix) sent on timeout, cancellation, or Kill, and the SIGKILL that
	// follows if the group is still alive. Zero kills immediately.
	GracePeriod time.Duration
}

// Result records what a finished process did.
type Result struct {
	// ExitCode is the process's exit status, or -1 if it was killed by a
	// signal or never exited cleanly.
	ExitCode int
	// Stdout and Stderr hold the captured output. They are filled by [Run]
	// only; [Stream] delivers output through [Handle.Lines] instead.
	Stdout string
	Stderr string
	// PID is the child's process id (and process-group id).
	PID int
	// Started is when the process was started; Duration is how long it ran
	// until it was reaped.
	Started  time.Time
	Duration time.Duration
	// TimedOut is true when [Spec.Timeout] caused the kill.
	TimedOut bool
	// Cancelled is true when the process was stopped because the context was
	// done or [Handle.Kill] was called (but not by the timeout).
	Cancelled bool
}

// ExitError is returned by [Run] and [Handle.Wait] whenever the process did
// not exit with status 0, including when it was killed. Result is always
// populated. Cause, when non-nil, explains why the process was stopped:
// [ErrTimeout], [ErrKilled], or the context's cancellation cause.
type ExitError struct {
	Command string
	Args    []string
	Result  Result
	Cause   error
}

// Error renders a one-line description including the last stderr line when
// one was captured.
func (e *ExitError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "proc: %s", e.Command)
	if len(e.Args) > 0 {
		fmt.Fprintf(&b, " %s", strings.Join(e.Args, " "))
	}
	switch {
	case e.Cause != nil:
		fmt.Fprintf(&b, ": %v", e.Cause)
	case e.Result.ExitCode >= 0:
		fmt.Fprintf(&b, ": exit status %d", e.Result.ExitCode)
	default:
		b.WriteString(": killed by signal")
	}
	if tail := lastLine(e.Result.Stderr); tail != "" {
		fmt.Fprintf(&b, ": %s", tail)
	}
	return b.String()
}

// Unwrap exposes Cause so errors.Is(err, ErrTimeout) and
// errors.Is(err, context.Canceled) work through an ExitError.
func (e *ExitError) Unwrap() error { return e.Cause }

// Run starts the process described by spec and waits for it to finish,
// capturing stdout and stderr in full.
//
// A nil error means the process exited with status 0. Any other outcome
// after a successful start returns an *ExitError with the populated Result;
// errors.Is on that error reports [ErrTimeout] or the context's cancellation
// cause. A failure to start (unknown command, bad directory) returns the
// underlying error and a zero Result.
func Run(ctx context.Context, spec Spec) (Result, error) {
	var stdout, stderr bytes.Buffer
	p, err := start(ctx, spec, &stdout, &stderr)
	if err != nil {
		return Result{}, err
	}
	res, err := p.wait()
	res.Stdout = stdout.String()
	res.Stderr = stderr.String()
	if err != nil {
		var ee *ExitError
		if errors.As(err, &ee) {
			ee.Result = res
		}
	}
	return res, err
}

// Source identifies which output pipe a [Line] came from.
type Source int

// Source values.
const (
	Stdout Source = iota
	Stderr
)

// String returns "stdout" or "stderr".
func (s Source) String() string {
	if s == Stderr {
		return "stderr"
	}
	return "stdout"
}

// Line is one line of process output, without its trailing newline.
type Line struct {
	Source Source
	Text   string
	// At is when the line was read from the pipe.
	At time.Time
}

// Handle is a running process started with [Stream].
type Handle struct {
	p       *process
	q       *lineQueue
	writers [2]*lineWriter
	lines   chan Line

	done   chan struct{}
	result Result
	err    error
}

// Stream starts the process described by spec and returns immediately.
// Output is delivered line by line on [Handle.Lines]; the exit status comes
// from [Handle.Wait]. Callers should drain Lines, which is closed after the
// process exits and every line has been delivered. Lines that never see a
// consumer are buffered in memory, so Wait never deadlocks.
func Stream(ctx context.Context, spec Spec) (*Handle, error) {
	q := newLineQueue()
	out := &lineWriter{source: Stdout, q: q}
	errw := &lineWriter{source: Stderr, q: q}
	p, err := start(ctx, spec, out, errw)
	if err != nil {
		return nil, err
	}
	h := &Handle{
		p:       p,
		q:       q,
		writers: [2]*lineWriter{out, errw},
		lines:   make(chan Line, 64),
		done:    make(chan struct{}),
	}
	go h.pump()
	go h.reap()
	return h, nil
}

// PID returns the child's process id.
func (h *Handle) PID() int { return h.p.cmd.Process.Pid }

// Lines returns the channel of output lines. It is closed once the process
// has exited and all output has been delivered.
func (h *Handle) Lines() <-chan Line { return h.lines }

// Done is closed when the process has exited and [Handle.Wait] will return
// without blocking.
func (h *Handle) Done() <-chan struct{} { return h.done }

// Wait blocks until the process has exited and returns its Result. It may be
// called any number of times; every call returns the same values. The error
// semantics match [Run].
func (h *Handle) Wait() (Result, error) {
	<-h.done
	return h.result, h.err
}

// Kill stops the process group: the polite signal first, then SIGKILL after
// [Spec.GracePeriod]. It returns without waiting; use Wait for the outcome,
// which reports [ErrKilled] as the cause.
func (h *Handle) Kill() {
	h.p.cancel(ErrKilled)
}

func (h *Handle) reap() {
	h.result, h.err = h.p.wait()
	// cmd.Wait has returned, so exec's copy goroutines are finished and no
	// further Write calls can race with the flush of a trailing partial line.
	for _, w := range h.writers {
		w.closeFlush()
	}
	h.q.close()
	close(h.done)
}

func (h *Handle) pump() {
	defer close(h.lines)
	for {
		l, ok := h.q.pop()
		if !ok {
			return
		}
		h.lines <- l
	}
}

// process wraps an exec.Cmd started by this package.
type process struct {
	spec    Spec
	cmd     *exec.Cmd
	ctx     context.Context
	cancel  context.CancelCauseFunc
	started time.Time
	// stopTimeout releases the timer behind Spec.Timeout (a no-op when there
	// is none). The context's cause distinguishes a timeout from an outer
	// cancellation.
	stopTimeout context.CancelFunc
}

// start validates spec, wires the writers, and starts the process in its own
// process group.
func start(ctx context.Context, spec Spec, stdout, stderr io.Writer) (*process, error) {
	if spec.Command == "" {
		return nil, errors.New("proc: empty command")
	}
	if spec.Timeout < 0 {
		return nil, fmt.Errorf("proc: negative timeout %v", spec.Timeout)
	}
	if spec.GracePeriod < 0 {
		return nil, fmt.Errorf("proc: negative grace period %v", spec.GracePeriod)
	}
	path, err := exec.LookPath(spec.Command)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancelCause(ctx)
	stopTimeout := context.CancelFunc(func() {})
	if spec.Timeout > 0 {
		runCtx, stopTimeout = context.WithTimeoutCause(runCtx, spec.Timeout, ErrTimeout)
	}

	cmd := exec.CommandContext(runCtx, path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Environ()
	cmd.Stdin = spec.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = spec.GracePeriod + pipeWaitDelay
	setProcessGroup(cmd)
	cmd.Cancel = func() error {
		return stopGroup(cmd.Process, spec.GracePeriod)
	}

	p := &process{spec: spec, cmd: cmd, ctx: runCtx, cancel: cancel, stopTimeout: stopTimeout}
	p.started = time.Now()
	if err := cmd.Start(); err != nil {
		stopTimeout()
		cancel(nil)
		return nil, err
	}
	return p, nil
}

// wait reaps the process and translates the outcome into Result and error.
func (p *process) wait() (Result, error) {
	waitErr := p.cmd.Wait()
	finished := time.Now()
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		// The child exited but something in its group kept the pipes open.
		// Clean up the stragglers; the exit status itself was fine.
		killGroup(p.cmd.Process)
	}
	cause := context.Cause(p.ctx)
	p.stopTimeout()
	p.cancel(nil)

	res := Result{
		ExitCode: -1,
		PID:      p.cmd.Process.Pid,
		Started:  p.started,
		Duration: finished.Sub(p.started),
	}
	if ps := p.cmd.ProcessState; ps != nil {
		res.ExitCode = ps.ExitCode()
	}
	switch {
	case cause == nil:
		// Context never fired: plain exit.
	case errors.Is(cause, ErrTimeout):
		res.TimedOut = true
	default:
		res.Cancelled = true
	}

	if cause == nil && res.ExitCode == 0 {
		return res, nil
	}
	return res, &ExitError{Command: p.spec.Command, Args: p.spec.Args, Result: res, Cause: cause}
}

// lineQueue is an unbounded FIFO of lines shared by both output writers, so
// the exec copy goroutines never block on a slow consumer and the relative
// order in which stdout and stderr were written is preserved.
type lineQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []Line
	closed bool
}

func newLineQueue() *lineQueue {
	q := &lineQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *lineQueue) push(l Line) {
	q.mu.Lock()
	q.items = append(q.items, l)
	q.mu.Unlock()
	q.cond.Signal()
}

func (q *lineQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

// pop blocks until an item is available or the queue is closed and empty.
func (q *lineQueue) pop() (Line, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return Line{}, false
	}
	l := q.items[0]
	q.items = q.items[1:]
	return l, true
}

// lineWriter splits a byte stream into lines and pushes them onto a
// lineQueue. Lines may be arbitrarily long; a trailing partial line is
// flushed when the pipe closes.
type lineWriter struct {
	source Source
	q      *lineQueue
	buf    []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.buf = append(w.buf, p...)
			return n, nil
		}
		w.buf = append(w.buf, p[:i]...)
		w.flush()
		p = p[i+1:]
	}
}

func (w *lineWriter) flush() {
	text := string(bytes.TrimSuffix(w.buf, []byte{'\r'}))
	w.buf = w.buf[:0]
	w.q.push(Line{Source: w.source, Text: text, At: time.Now()})
}

// closeFlush emits any partial final line. It must only be called once no
// further Write can happen, i.e. after cmd.Wait has returned.
func (w *lineWriter) closeFlush() {
	if len(w.buf) > 0 {
		w.flush()
	}
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\r\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}
