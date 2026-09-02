package cartridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"slop-cannon/internal/events"
	"slop-cannon/internal/proc"
)

// Cartridge is one LLM CLI adapter. Implementations are safe for concurrent
// use: the orchestrator fires many sessions of one cartridge at once.
type Cartridge interface {
	// Name is the registry name the cartridge was built under (for example
	// "claude"); it is what config, events and logs call it.
	Name() string
	// Detect checks that the CLI is installed, reports its version, and runs
	// a cheap authentication probe. The returned error, when non-nil, is a
	// *Error whose Kind says why the cartridge cannot fire: ErrNotInstalled,
	// ErrAuth, ErrCreditsExhausted, ErrRateLimited, ErrTimeout or ErrCrash.
	// Status is filled in as far as the probe got even when err is non-nil.
	Detect(ctx context.Context) (Status, error)
	// Run starts one session for spec in spec.Workdir and returns as soon as
	// the process is up. A non-nil error means nothing was started (a
	// *Error, or a spec validation error). Cancelling ctx kills the session.
	Run(ctx context.Context, spec RunSpec) (Session, error)
}

// AuthState is what Detect learned about the CLI's authentication.
type AuthState string

// Authentication states.
const (
	// AuthUnknown means the adapter could not or did not probe (the zero
	// value); the cartridge is assumed usable.
	AuthUnknown AuthState = ""
	// AuthOK means the probe succeeded.
	AuthOK AuthState = "ok"
	// AuthMissing means the CLI is not logged in or its token was rejected.
	AuthMissing AuthState = "missing"
)

// String returns "unknown", "ok" or "missing".
func (a AuthState) String() string {
	if a == AuthUnknown {
		return "unknown"
	}
	return string(a)
}

// Status is what Detect learned about an installed CLI.
type Status struct {
	// Installed is true when the binary was found on PATH.
	Installed bool `json:"installed"`
	// Path is the resolved binary path.
	Path string `json:"path,omitempty"`
	// Version is the version string the CLI printed, trimmed.
	Version string `json:"version,omitempty"`
	// Auth is the authentication probe's verdict.
	Auth AuthState `json:"auth,omitempty"`
	// Detail is a short human note ("logged in as x", "probe skipped").
	Detail string `json:"detail,omitempty"`
}

// Ready reports whether the cartridge can fire: installed and not known to
// be unauthenticated.
func (s Status) Ready() bool { return s.Installed && s.Auth != AuthMissing }

// Session is one running firing. Callers must drain Events; the channel is
// closed after the terminal event, and Wait never blocks on an undrained
// channel (adapters buffer without bound, like internal/proc).
type Session interface {
	// Events streams what the model does, ending with exactly one EventDone
	// or EventError, after which the channel is closed.
	Events() <-chan SessionEvent
	// Wait blocks until the session has ended and returns its outcome. It
	// may be called any number of times. err is nil on success and a *Error
	// otherwise; Result.Usage may be partially filled on failure.
	Wait() (Result, error)
	// Kill stops the session (process group signal, then SIGKILL). It
	// returns immediately; Wait then reports ErrKilled unless the session
	// had already ended.
	Kill()
}

// Kind classifies a cartridge failure. See the package documentation for
// what the supervisor does with each.
type Kind string

// Failure kinds.
const (
	// KindUnknown is an error the adapter could not classify.
	KindUnknown Kind = "unknown"
	// KindRateLimited is a temporary rate limit: back off, keep the
	// cartridge.
	KindRateLimited Kind = "rate_limited"
	// KindCreditsExhausted is quota/billing exhaustion: the cartridge is
	// dead for this run (D-18).
	KindCreditsExhausted Kind = "credits_exhausted"
	// KindAuth is a missing or rejected login.
	KindAuth Kind = "auth"
	// KindCrash is a non-zero exit, a signal, or unparseable CLI output.
	KindCrash Kind = "crash"
	// KindTimeout is the session timeout elapsing.
	KindTimeout Kind = "timeout"
	// KindKilled is a deliberate stop (Session.Kill or context cancel).
	KindKilled Kind = "killed"
	// KindNotInstalled is the CLI binary missing from PATH (Detect only).
	KindNotInstalled Kind = "not_installed"
)

var kinds = []Kind{KindUnknown, KindRateLimited, KindCreditsExhausted, KindAuth, KindCrash, KindTimeout, KindKilled, KindNotInstalled}

// Kinds returns every failure kind.
func Kinds() []Kind { return append([]Kind(nil), kinds...) }

// String returns the canonical name; the empty kind reads as "unknown".
func (k Kind) String() string {
	if k == "" {
		return string(KindUnknown)
	}
	return string(k)
}

// Valid reports whether k is a declared kind.
func (k Kind) Valid() bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

// Retryable reports whether the supervisor may fire the same job again
// without human action: rate limits, crashes, timeouts and unknown errors.
// Credits, auth, killed and not-installed are not.
func (k Kind) Retryable() bool {
	switch k {
	case KindRateLimited, KindCrash, KindTimeout, KindUnknown, "":
		return true
	default:
		return false
	}
}

// Sentinel returns the errors.Is target for the kind (nil for KindUnknown).
func (k Kind) Sentinel() error {
	switch k {
	case KindRateLimited:
		return ErrRateLimited
	case KindCreditsExhausted:
		return ErrCreditsExhausted
	case KindAuth:
		return ErrAuth
	case KindCrash:
		return ErrCrash
	case KindTimeout:
		return ErrTimeout
	case KindKilled:
		return ErrKilled
	case KindNotInstalled:
		return ErrNotInstalled
	default:
		return nil
	}
}

// Failure maps the kind onto the events.Failure the orchestrator publishes
// in AgentFailed. Auth, not-installed and unknown errors report as crashes,
// which is what they look like from the outside.
func (k Kind) Failure() events.Failure {
	switch k {
	case KindRateLimited:
		return events.FailureRateLimited
	case KindCreditsExhausted:
		return events.FailureCreditsExhausted
	case KindTimeout:
		return events.FailureTimeout
	case KindKilled:
		return events.FailureKilled
	default:
		return events.FailureCrash
	}
}

// Sentinel errors, one per Kind. Test with errors.Is; every *Error matches
// the sentinel of its Kind.
var (
	// ErrRateLimited: the CLI reported a temporary rate limit. Back off and
	// retry; the cartridge stays alive.
	ErrRateLimited = errors.New("cartridge: rate limited")
	// ErrCreditsExhausted: quota, billing or credits are gone. The cartridge
	// is dead for the rest of the run; all dead ends autopilot (D-18).
	ErrCreditsExhausted = errors.New("cartridge: credits exhausted")
	// ErrAuth: the CLI is not logged in or its token was rejected.
	ErrAuth = errors.New("cartridge: not authenticated")
	// ErrCrash: the process exited non-zero, died by signal, or printed
	// output the adapter could not parse.
	ErrCrash = errors.New("cartridge: crashed")
	// ErrTimeout: RunSpec.Timeout elapsed and the session was killed.
	ErrTimeout = errors.New("cartridge: timed out")
	// ErrKilled: Session.Kill was called or the context was cancelled.
	ErrKilled = errors.New("cartridge: killed")
	// ErrNotInstalled: the CLI binary is not on PATH.
	ErrNotInstalled = errors.New("cartridge: not installed")
)

// Error is the typed error every cartridge reports. errors.Is(err, X) is true
// for the sentinel X of its Kind and for anything its Cause matches.
type Error struct {
	// Kind classifies the failure.
	Kind Kind
	// Cartridge is the adapter name, when known.
	Cartridge string
	// Msg is the CLI's own message or the adapter's summary, may be empty.
	Msg string
	// Cause is the underlying error (an *proc.ExitError, a parse error), may
	// be nil.
	Cause error
	// RetryAfter is the back-off the CLI asked for, when it said (rate
	// limits); zero otherwise.
	RetryAfter time.Duration
}

// NewError builds an Error. A nil cause is fine.
func NewError(kind Kind, cartridge, msg string, cause error) *Error {
	return &Error{Kind: kind, Cartridge: cartridge, Msg: msg, Cause: cause}
}

// Error renders "cartridge <name>: <kind>: <msg>: <cause>", skipping empty
// parts. The sentinel text stands in when the kind is unknown.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("cartridge")
	if e.Cartridge != "" {
		b.WriteString(" ")
		b.WriteString(e.Cartridge)
	}
	if s := e.Kind.Sentinel(); s != nil {
		b.WriteString(": ")
		b.WriteString(strings.TrimPrefix(s.Error(), "cartridge: "))
	} else {
		b.WriteString(": error")
	}
	if e.Msg != "" {
		b.WriteString(": ")
		b.WriteString(e.Msg)
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

// Unwrap exposes the cause.
func (e *Error) Unwrap() error { return e.Cause }

// Is matches the sentinel of e's Kind (the cause is handled by Unwrap).
func (e *Error) Is(target error) bool {
	s := e.Kind.Sentinel()
	return s != nil && target == s
}

// KindOf classifies any error: a *Error's Kind, a bare sentinel's kind, the
// internal/proc and context causes (timeout, killed, cancelled), and
// KindUnknown for everything else. A nil error is KindUnknown too.
func KindOf(err error) Kind {
	if err == nil {
		return KindUnknown
	}
	var ce *Error
	if errors.As(err, &ce) {
		if ce.Kind.Valid() && ce.Kind != KindUnknown {
			return ce.Kind
		}
		if ce.Cause != nil {
			return KindOf(ce.Cause)
		}
		return KindUnknown
	}
	for _, k := range kinds {
		if s := k.Sentinel(); s != nil && errors.Is(err, s) {
			return k
		}
	}
	switch {
	case errors.Is(err, proc.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return KindTimeout
	case errors.Is(err, proc.ErrKilled), errors.Is(err, context.Canceled):
		return KindKilled
	}
	var ee *proc.ExitError
	if errors.As(err, &ee) {
		return KindCrash
	}
	return KindUnknown
}

// AsError returns err as a *Error, wrapping it with the kind KindOf reports
// when it is not one already (unknown becomes KindCrash: something failed
// and nobody said why). A nil err yields a KindUnknown error so callers that
// need a terminal error always get one. cartridge names the adapter and is
// filled in only when the error does not already carry one.
func AsError(cartridge string, err error) *Error {
	if err == nil {
		return NewError(KindUnknown, cartridge, "session ended without a result", nil)
	}
	var ce *Error
	if errors.As(err, &ce) {
		if ce.Cartridge == "" && cartridge != "" {
			cp := *ce
			cp.Cartridge = cartridge
			return &cp
		}
		return ce
	}
	k := KindOf(err)
	if k == KindUnknown {
		k = KindCrash
	}
	if err == k.Sentinel() {
		// A bare sentinel carries no extra information; keeping it as the
		// cause would only repeat the kind's text.
		return NewError(k, cartridge, "", nil)
	}
	return NewError(k, cartridge, "", err)
}

// Pipe is a ready-made Session for adapters. The producer side calls Emit
// for every event and Finish once; the consumer side is the Session
// interface. Events are queued without bound so a consumer that only calls
// Wait never deadlocks the producer, and Finish appends the terminal event
// when the producer did not emit one, so the stream contract holds however
// the adapter ends.
type Pipe struct {
	name string
	kill func()

	mu       sync.Mutex
	cond     *sync.Cond
	queue    []SessionEvent
	closed   bool
	terminal bool

	events   chan SessionEvent
	done     chan struct{}
	result   Result
	err      error
	killOnce sync.Once
	killed   bool
	fin      sync.Once
}

// NewPipe returns an open Pipe. name is the cartridge name stamped on errors;
// kill is called at most once by Kill and may be nil.
func NewPipe(name string, kill func()) *Pipe {
	p := &Pipe{name: name, kill: kill, events: make(chan SessionEvent, 64), done: make(chan struct{})}
	p.cond = sync.NewCond(&p.mu)
	go p.pump()
	return p
}

// Emit queues ev for the consumer. It returns false, and drops ev, once the
// pipe is finished or a terminal event has already been emitted. A zero At
// is stamped now.
func (p *Pipe) Emit(ev SessionEvent) bool {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.terminal {
		return false
	}
	if ev.Kind.Terminal() {
		if ev.Kind == EventError {
			ev.Err = AsError(p.name, ev.Err)
		}
		p.terminal = true
	}
	p.queue = append(p.queue, ev)
	p.cond.Signal()
	return true
}

// Finish records the outcome, appends the terminal event when none was
// emitted (Done(res) on nil err, ErrorEvent otherwise), closes Events after
// the queue drains, and releases Wait. Later calls are no-ops. A non-*Error
// err is wrapped by AsError; a nil err after Kill is reported as ErrKilled.
func (p *Pipe) Finish(res Result, err error) {
	p.fin.Do(func() {
		p.mu.Lock()
		if err == nil && p.killed {
			err = NewError(KindKilled, p.name, "", nil)
		}
		if err != nil {
			err = AsError(p.name, err)
		}
		if !p.terminal {
			var ev SessionEvent
			if err == nil {
				ev = Done(res)
			} else {
				ev = ErrorEvent(err, res.Usage)
			}
			p.queue = append(p.queue, ev)
			p.terminal = true
		}
		p.result, p.err = res, err
		p.closed = true
		p.mu.Unlock()
		p.cond.Broadcast()
		close(p.done)
	})
}

// Events implements Session.
func (p *Pipe) Events() <-chan SessionEvent { return p.events }

// Wait implements Session.
func (p *Pipe) Wait() (Result, error) {
	<-p.done
	return p.result, p.err
}

// Done is closed when Finish has been called.
func (p *Pipe) Done() <-chan struct{} { return p.done }

// Kill implements Session: it runs the kill callback once and marks the
// pipe so that a Finish with a nil error still reports ErrKilled.
func (p *Pipe) Kill() {
	p.killOnce.Do(func() {
		p.mu.Lock()
		p.killed = true
		p.mu.Unlock()
		if p.kill != nil {
			p.kill()
		}
	})
}

// Killed reports whether Kill was called.
func (p *Pipe) Killed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}

func (p *Pipe) pump() {
	defer close(p.events)
	for {
		p.mu.Lock()
		for len(p.queue) == 0 && !p.closed {
			p.cond.Wait()
		}
		if len(p.queue) == 0 {
			p.mu.Unlock()
			return
		}
		ev := p.queue[0]
		p.queue = p.queue[1:]
		p.mu.Unlock()
		p.events <- ev
	}
}

// Collect drains a session, calling fn (if non-nil) for every event in
// order, then waits for the outcome. It is the simplest way to run a
// session to completion: tests, the conformance suite and the structured
// output retry loop use it.
func Collect(s Session, fn func(SessionEvent)) (Result, error) {
	for ev := range s.Events() {
		if fn != nil {
			fn(ev)
		}
	}
	return s.Wait()
}

// Transcript gathers the assistant text of a session's events in order,
// which is what Result.Output should equal for adapters that stream it.
func Transcript(evs []SessionEvent) string {
	var b strings.Builder
	for _, ev := range evs {
		if ev.Kind == EventTextDelta {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

// Describe renders an event on one line for logs and transcripts.
func Describe(ev SessionEvent) string {
	switch ev.Kind {
	case EventTextDelta:
		return ev.Text
	case EventToolUse:
		if ev.Input == "" {
			return fmt.Sprintf("[tool %s]", ev.Tool)
		}
		return fmt.Sprintf("[tool %s] %s", ev.Tool, ev.Input)
	case EventToolResult:
		status := "ok"
		if ev.IsError {
			status = "error"
		}
		return fmt.Sprintf("[result %s %s] %s", ev.Tool, status, ev.Output)
	case EventDone:
		if ev.Result == nil {
			return "[done]"
		}
		return fmt.Sprintf("[done] %d in / %d out tokens in %s", ev.Result.Usage.InputTokens, ev.Result.Usage.OutputTokens, ev.Result.Duration.Round(time.Millisecond))
	case EventError:
		if ev.Err == nil {
			return "[error]"
		}
		return "[error] " + ev.Err.Error()
	default:
		return fmt.Sprintf("[%s]", ev.Kind.String())
	}
}
