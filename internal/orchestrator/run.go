package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
)

// The run lifecycle (P6-01). One [Run] owns everything that is true of a
// firing as a whole: its directory under the state dir (D-23), its
// state.json, its lock, its live agents, and the transitions between
// running, paused, stopping and finished. The work itself — scheduling
// (P6-02), the feedback loops (P6-04), planning (P6-05) and delivery
// (P6-06) — is the [Loop] the run drives; everything the loop needs from
// the lifecycle (pause gating, agent bookkeeping, publishing) is a method
// on *Run.
//
// Resume folds in P6-11 (the frozen config snapshot, restored by
// [RestoreRunConfig] before New) and P6-15 (acting on gitx.ResumeResult).
// Finish folds in P6-18 (gitx cleanup and the kept worktree paths).

// SourceRun is the events.Warning.Source the run lifecycle stamps on its
// own notices.
const SourceRun = "run"

// DefaultStopGrace is how long a graceful stop gives an agent to exit after
// SIGTERM before it is killed.
const DefaultStopGrace = 10 * time.Second

// killSlack is the extra time a graceful stop allows the agent stopper on
// top of the grace period, so a stopper that honours the grace exactly is
// not cancelled at the finish line.
const killSlack = 5 * time.Second

// DefaultAgentBusBuffer is how many events [Run.AgentBus] holds while the
// run republishes them. One cartridge output line is one event (P5-08), so
// this is a few seconds of a chatty agent.
const DefaultAgentBusBuffer = 8192

// agentBusDrain bounds how long the finish waits for the forwarding bus to
// be drained. It is wall-clock rather than the run's clock on purpose: it
// bounds a goroutine, not the run's behaviour.
const agentBusDrain = 5 * time.Second

// Errors returned by the lifecycle.
var (
	// ErrNotStarted is returned by Wait and Pause before Start.
	ErrNotStarted = errors.New("orchestrator: run not started")
	// ErrAlreadyStarted is returned by a second Start.
	ErrAlreadyStarted = errors.New("orchestrator: run already started")
	// ErrFinished is returned by a transition attempted after the run ended.
	ErrFinished = errors.New("orchestrator: run has finished")
	// ErrLocked is returned by Start when another live run holds the
	// repository's run lock.
	ErrLocked = errors.New("orchestrator: another run holds the lock")
)

// Phase is where a run is in its lifecycle. It is recorded in state.json on
// every transition.
type Phase string

// The lifecycle phases.
const (
	// PhaseNew means the run exists but Start has not been called.
	PhaseNew Phase = "new"
	// PhaseRunning means the loop is firing agents.
	PhaseRunning Phase = "running"
	// PhasePaused means no new agents are fired; the running ones finish.
	PhasePaused Phase = "paused"
	// PhaseStopping means the run is shutting down: agents are being
	// stopped, leases released, worktrees cleaned.
	PhaseStopping Phase = "stopping"
	// PhaseFinished means the run is over and state.json is final.
	PhaseFinished Phase = "finished"
)

// String returns the phase name.
func (p Phase) String() string { return string(p) }

// Live reports whether the run is still going: running, paused or stopping.
func (p Phase) Live() bool {
	switch p {
	case PhaseRunning, PhasePaused, PhaseStopping:
		return true
	default:
		return false
	}
}

// Outcome is why a run ended, as reported by [Run.Wait] and published in
// [events.RunFinished].
type Outcome struct {
	// Reason is the finish reason (D-18).
	Reason events.FinishReason
	// Summary is a short human-readable account of what was achieved,
	// including the worktree cleanup report (P6-18).
	Summary string
	// Duration is how long the run was live.
	Duration time.Duration
}

// Loop is the body of a run: the scheduler tick loop and everything it
// drives (P6-02, P6-04, P6-05, P6-06). The lifecycle calls Run once, in its
// own goroutine, after the run has started and any resume reconciliation is
// done.
//
// The loop must return promptly when ctx is cancelled: that is how Stop
// asks it to wind down. Before firing an agent it must respect the pause
// gate ([Run.Paused], [Run.AwaitResume]) and register the agent with
// [Run.AgentStarted] so state.json and the ammo accounting stay honest.
type Loop interface {
	// Run drives the run until the work is done or ctx is cancelled.
	Run(ctx context.Context, r *Run) (Outcome, error)
}

// LoopFunc adapts a function to [Loop].
type LoopFunc func(ctx context.Context, r *Run) (Outcome, error)

// Run implements [Loop].
func (f LoopFunc) Run(ctx context.Context, r *Run) (Outcome, error) { return f(ctx, r) }

// AgentStopper stops the run's live agent subprocesses on shutdown: SIGTERM
// first, then SIGKILL for whatever is still alive after grace. The agent
// supervisor (P6-07) implements it; a nil one means the run has no
// subprocesses to stop.
type AgentStopper interface {
	// StopAgents returns when no agent of the run is alive any more. A
	// grace of zero means kill immediately (a second interrupt).
	StopAgents(ctx context.Context, grace time.Duration) error
}

// LeaseReleaser returns a bead to the ready set so a resumed run (or
// another cannon) can claim it. *beads.Client satisfies it; the issue it
// returns is ignored here.
type LeaseReleaser interface {
	Release(ctx context.Context, id domain.BeadID) (beads.Issue, error)
}

// WorktreeCleaner prunes the run's worktrees when it finishes (P4-08);
// *gitx.Manager satisfies it.
type WorktreeCleaner interface {
	Cleanup(ctx context.Context, beadList []domain.Bead, opts gitx.CleanupOptions) (*gitx.CleanupResult, error)
}

// GitResumer reattaches the run's worktrees and reconciles branch state
// into bead state on --resume (P4-08); *gitx.Resumer satisfies it.
type GitResumer interface {
	Resume(ctx context.Context, beadList []domain.Bead) (*gitx.ResumeResult, error)
}

// ResumeHandler acts on what a resume found (P6-15). The delivery handoff
// implements both halves (*Deliverer, P6-06): a pull request is re-opened
// through the same idempotent path that opened it, and a conflict round is
// offered to the scheduler as engineer work. The feedback loop then picks
// the round up through [Feedback.PlanFiring].
type ResumeHandler interface {
	// RedeliverPR re-runs the (idempotent) pull request delivery for a bead
	// whose PR already exists, so its gh:pr gate is recreated (D-20).
	RedeliverPR(ctx context.Context, e gitx.ResumeEntry) error
	// ConflictRound sends a bead whose worktree was left mid-rebase back to
	// its engineer as a conflict round (D-10).
	ConflictRound(ctx context.Context, e gitx.ResumeEntry) error
}

// BeadSource lists the run's beads with their current states. The run asks
// for them on resume (to reattach worktrees) and at finish (to decide which
// worktrees to prune). The beads client's run-scoped listing provides it.
type BeadSource func(ctx context.Context) ([]domain.Bead, error)

// Options configures [New]. The zero value is usable: the run then has no
// loop (it stays live until it is stopped), publishes nowhere, logs to
// slog.Default(), and does no agent, lease, worktree or resume work.
type Options struct {
	// Loop is the body of the run. Nil means the run does nothing but
	// honour the lifecycle, which is what the lifecycle tests and a TUI
	// harness want.
	Loop Loop
	// Bus receives every event the run publishes.
	Bus Publisher
	// Logger receives one line per transition; nil means slog.Default().
	Logger *slog.Logger
	// Now is the clock; nil means time.Now. Tests pass a fake one so
	// state.json and the events are deterministic.
	Now func() time.Time
	// Out is where the run prints its finish report, notably the worktrees
	// cleanup kept (P6-18). Nil means: os.Stdout in headless mode, and
	// nothing at all in the TUI, which owns the terminal — there the report
	// reaches the human through the run log and RunFinished.Summary.
	Out io.Writer
	// StopGrace is how long a graceful stop gives agents after SIGTERM;
	// zero means DefaultStopGrace.
	StopGrace time.Duration
	// Agents stops the live agent subprocesses on shutdown (P6-07).
	Agents AgentStopper
	// Leases releases the beads held by live agents on shutdown (P3-05).
	Leases LeaseReleaser
	// Cleaner prunes worktrees when the run finishes (P4-08, P6-18).
	Cleaner WorktreeCleaner
	// Beads lists the run's beads for the resume, lease-release and
	// worktree-cleanup passes. Wire it wherever Leases is wired (P8-17):
	// without it the shutdown release falls back to the in_progress the
	// claim set, which is right for an engineer and blind to every bead a
	// tester or the product owner was holding. [NewConductor] fills it in
	// for an assembled run ([Run.UseBeads]).
	Beads BeadSource
	// Resumer reconciles git state into bead state on --resume (P4-08).
	Resumer GitResumer
	// OnResume acts on what the Resumer found (P6-15).
	OnResume ResumeHandler
	// Signals delivers interrupts: the first asks for a graceful stop, the
	// second aborts immediately. Use [InterruptChannel] in production; a
	// test sends values itself.
	Signals <-chan os.Signal
	// LockPolicy decides whether an existing run lock may be taken over;
	// nil means [DefaultLockPolicy]. P9-04 hardens it.
	LockPolicy LockPolicy
	// SkipLock leaves the run lock alone, for a caller that only observes a
	// run (`sc status`) or a test that runs several in one directory.
	SkipLock bool
}

// Run is one firing of the cannon: its lifecycle, its durable state and the
// bookkeeping every other part of the orchestrator hangs off. Obtain one
// with [New]; all methods are safe for concurrent use.
type Run struct {
	cfg  *config.RunConfig
	dirs RunDirs
	opts Options

	now       func() time.Time
	log       *slog.Logger
	bus       Publisher
	stopGrace time.Duration

	interrupts atomic.Uint64
	abort      chan struct{}
	abortOnce  sync.Once
	done       chan struct{}

	mu         sync.Mutex
	st         State
	started    bool
	finished   bool
	stopping   bool
	stopReason string
	resumeCh   chan struct{}
	cancelLoop context.CancelFunc
	lock       *lockFile
	resumeRes  *gitx.ResumeResult
	beadSrc    BeadSource
	vitals     func() RunVitals
	agentBus   *events.Bus
	agentDone  chan struct{}
	outcome    Outcome
	err        error
}

// New prepares a run from the configuration the CLI resolved (P1-08). It
// touches no file: the run directory, the lock and state.json appear when
// [Run.Start] is called.
//
// When cfg.Resume is set the caller is expected to have called
// [RestoreRunConfig] first so cfg carries the run's frozen snapshot
// (P6-11); New reads the existing state file, if any, to carry the epic,
// the resume count and the event sequence forward.
func New(cfg *config.RunConfig, opts Options) (*Run, error) {
	if cfg == nil {
		return nil, errors.New("orchestrator: New: nil run config")
	}
	if cfg.Run.ID == "" {
		return nil, errors.New("orchestrator: New: run has no id")
	}
	if cfg.Run.StateDir == "" {
		return nil, errors.New("orchestrator: New: run has no state directory (D-23)")
	}
	if cfg.Run.RepoPath == "" {
		return nil, errors.New("orchestrator: New: run has no repository path")
	}
	r := &Run{
		cfg:       cfg,
		dirs:      DirsFor(cfg.Run.StateDir),
		opts:      opts,
		now:       opts.Now,
		log:       opts.Logger,
		bus:       opts.Bus,
		stopGrace: opts.StopGrace,
		beadSrc:   opts.Beads,
		abort:     make(chan struct{}),
		done:      make(chan struct{}),
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	if r.stopGrace <= 0 {
		r.stopGrace = DefaultStopGrace
	}
	r.log = r.log.With(slog.String("run", cfg.Run.ID.String()))

	r.st = State{
		Version: StateFileVersion,
		Run:     cfg.Run,
		Phase:   PhaseNew,
	}
	if cfg.Resume {
		prev, err := LoadState(cfg.Run.StateDir)
		switch {
		case err == nil:
			r.st.Resumes = prev.Resumes + 1
			r.st.LastSeq = prev.LastSeq
			if r.st.Run.Epic == "" {
				r.st.Run.Epic = prev.Run.Epic
			}
			if prev.Run.StartedAt.IsZero() {
				break
			}
			r.st.Run.StartedAt = prev.Run.StartedAt
		case errors.Is(err, ErrNoState):
			// A run directory with no state file: the previous attempt died
			// before its first transition. Resume from what we have.
		default:
			return nil, err
		}
	}
	return r, nil
}

// ID returns the run's id.
func (r *Run) ID() domain.RunID { return r.cfg.Run.ID }

// Dir returns the run directory, <state-dir>/runs/<run-id> (D-23).
func (r *Run) Dir() string { return r.dirs.Dir }

// Dirs returns the directories the run owns.
func (r *Run) Dirs() RunDirs { return r.dirs }

// RepoPath returns the target repository.
func (r *Run) RepoPath() string { return r.cfg.Run.RepoPath }

// RunConfig returns the configuration the run was created with. Callers
// must not mutate it.
func (r *Run) RunConfig() *config.RunConfig { return r.cfg }

// Snapshot returns the frozen configuration the run behaves by (D-23,
// P6-11): the values at start, not whatever the config files say now.
func (r *Run) Snapshot() domain.ConfigSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneSnapshot(r.st.Run.Config)
}

// Allocation returns the D-05 ammo split the run fires under.
func (r *Run) Allocation() config.Allocation { return r.cfg.Allocation }

// State returns a copy of the run's durable state as it stands.
func (r *Run) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st.clone()
}

// Phase returns the lifecycle phase.
func (r *Run) Phase() Phase {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st.Phase
}

// Now returns the run's clock reading.
func (r *Run) Now() time.Time { return r.now() }

// Meta stamps an event for this run at the current time.
func (r *Run) Meta() events.Meta { return events.MetaAt(r.ID(), r.now()) }

// Log returns the run's logger.
func (r *Run) Log() *slog.Logger { return r.log }

// SetEpic records the run's epic bead once the first plan lands (D-22) and
// persists it, so a resumed run finds its own beads again.
func (r *Run) SetEpic(id domain.BeadID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.st.Run.Epic == id {
		return nil
	}
	r.st.Run.Epic = id
	r.cfg.Run.Epic = id
	return r.persistLocked()
}

// Epic returns the run's epic bead, empty until the first plan lands.
func (r *Run) Epic() domain.BeadID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st.Run.Epic
}

// Publish counts an event and sends it to [Options.Bus]. Every part of the
// orchestrator publishes through here so the sequence number in state.json
// means something. The count is persisted at the next transition, not on
// every event: AgentOutput alone would otherwise rewrite the file thousands
// of times.
//
// The JSONL run log is one of the bus's publishers, not something this
// method writes: the wiring is Options.Bus = Tee(OpenEventLog(dir), bus),
// with the log first so a line is durable before any subscriber renders it
// (P6-08).
func (r *Run) Publish(ev events.Event) {
	if ev == nil {
		return
	}
	r.mu.Lock()
	r.st.LastSeq++
	r.st.LastEventKind = ev.Kind()
	r.st.LastEventAt = ev.At()
	r.mu.Unlock()
	if r.bus != nil {
		r.bus.Publish(ev)
	}
}

// Warn publishes an [events.Warning] for something recoverable a human
// should still see, and logs it.
func (r *Run) Warn(source, message string, err error) {
	w := events.Warning{Meta: r.Meta(), Source: source, Message: message}
	if err != nil {
		w.Err = err.Error()
	}
	r.log.Warn(message, slog.String("source", source), slog.Any("err", err))
	r.Publish(w)
}

// AgentBus returns the run's forwarding bus: an [events.Bus] every event of
// which is republished through [Run.Publish], and so reaches the run log,
// state.json's sequence number and the live bus like every other event.
//
// It exists because two producers can only publish to a concrete
// *events.Bus — [agents.AgentRun.Bus], which the supervisor fills from
// [SupervisorOptions.Bus], and [beads.WatcherOptions.Bus] — while
// everything else in the run publishes through a [Publisher]. Handing them
// the live bus directly is what P6-27 found: every AgentOutput line went
// straight past the run, so state.json's LastSeq undercounted the run log
// by however much the agents said. Hand them this one instead:
//
//	sup, err := NewSupervisor(SupervisorOptions{Bus: run.AgentBus(), …})
//
// The forwarding is asynchronous (a subscription is the only way to observe
// a bus), so an event can be dropped when the buffer is full — under a
// producer faster than the run log for [DefaultAgentBusBuffer] events. A
// dropped event reaches neither the counter nor the log, so the two still
// agree; the count is reported once as a warning when the run ends.
//
// Never pass the run's own [Options.Bus] to a producer through this: the
// forwarder would publish into a bus that feeds it.
func (r *Run) AgentBus() *events.Bus {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.agentBus != nil {
		return r.agentBus
	}
	bus := events.NewBus()
	sub := bus.Subscribe(context.Background(), DefaultAgentBusBuffer)
	done := make(chan struct{})
	r.agentBus, r.agentDone = bus, done
	go r.forwardAgentBus(sub, done)
	return bus
}

// forwardAgentBus republishes everything the forwarding bus receives, until
// the bus is closed and the buffered events have been drained.
func (r *Run) forwardAgentBus(sub *events.Subscription, done chan struct{}) {
	defer close(done)
	for ev := range sub.Events() {
		r.Publish(ev)
	}
	if n := sub.Dropped(); n > 0 {
		r.Warn(SourceRun, fmt.Sprintf(
			"%d agent event(s) were dropped: the run log could not keep up with the agents' output", n), nil)
	}
}

// stopAgentBus closes the forwarding bus and waits for the events already
// in it to be republished, so the sequence number the final state.json
// records is the whole run. A forwarder that has not drained within
// [agentBusDrain] is abandoned: a shutdown must end.
func (r *Run) stopAgentBus() {
	r.mu.Lock()
	bus, done := r.agentBus, r.agentDone
	r.agentBus = nil
	r.mu.Unlock()
	if bus == nil {
		return
	}
	bus.Close()
	select {
	case <-done:
	case <-time.After(agentBusDrain):
		r.log.Warn("the agent event forwarder did not drain in time")
	}
}

// removePlanShim deletes <run-dir>/bin/po-plan, the planning product
// owner's read-only bd shim (D-21, P6-05), when the run ends (P6-34). The
// per-bead shims are the supervisor's ([Supervisor.RemoveShim], P6-17);
// this one belongs to the run, lives for the whole of it, and had nothing
// removing it. A run that never planned has no such directory, and removing
// a missing one is not an error.
func (r *Run) removePlanShim() {
	dir := filepath.Join(r.dirs.Shims, PlanShimDirName)
	if err := os.RemoveAll(dir); err != nil {
		r.Warn(SourceRun, "could not remove the planning bd shim at "+dir, err)
		return
	}
	r.log.Debug("planning bd shim removed", slog.String("dir", dir))
}

// TeeRunLog opens the run log of runDir and puts it in front of bus in
// opts.Bus, which is the whole of the wiring P8-20 asks for:
//
//	log, err := orchestrator.TeeRunLog(&opts, cfg.Run.StateDir, bus)
//	defer log.Close()
//
// The log comes first so an event is durable before any subscriber renders
// it (P6-08). Without it a real run leaves no events.jsonl behind and
// `sc status` folds nothing. The caller closes the returned log when the
// run has finished; a nil opts is refused.
func TeeRunLog(opts *Options, runDir string, bus Publisher) (*EventLog, error) {
	if opts == nil {
		return nil, errors.New("orchestrator: TeeRunLog: nil options")
	}
	log, err := OpenEventLog(runDir, opts.Logger)
	if err != nil {
		return nil, err
	}
	opts.Bus = Tee(log, bus)
	return log, nil
}

// Start creates the run directory, takes the repository's run lock, writes
// the first state.json, publishes [events.RunStarted], performs the resume
// reconciliation when the run is being resumed (P6-15), and then drives the
// loop in its own goroutine. It returns as soon as the loop is under way;
// use [Run.Wait] for the outcome.
//
// ctx bounds the run: cancelling it stops the run as if [Run.Stop] had been
// called.
func (r *Run) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return ErrFinished
	}
	if r.started {
		r.mu.Unlock()
		return ErrAlreadyStarted
	}
	r.started = true
	r.mu.Unlock()

	dirs, err := EnsureRunDir(r.cfg.Run.StateDir)
	if err != nil {
		r.failStart(err)
		return err
	}
	r.dirs = dirs

	if !r.opts.SkipLock {
		lock, err := acquireLock(r.dirs.Dir, r.ID(), r.opts.LockPolicy, r.now())
		if err != nil {
			r.failStart(err)
			return err
		}
		r.mu.Lock()
		r.lock = lock
		r.mu.Unlock()
	}

	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.mu.Lock()
	r.cancelLoop = cancel
	r.st.Phase = PhaseRunning
	if err := r.persistLocked(); err != nil {
		r.mu.Unlock()
		cancel()
		r.failStart(err)
		return err
	}
	r.mu.Unlock()

	r.log.Info("run started",
		slog.String("dir", r.dirs.Dir),
		slog.String("mode", string(r.cfg.Run.Mode)),
		slog.Bool("resumed", r.cfg.Resume),
		slog.Int("ammo", r.cfg.Run.Config.Ammo),
		slog.Bool("safety", r.cfg.Run.Config.Safety))
	r.Publish(events.RunStarted{Meta: r.Meta(), Info: r.State().Run, Resumed: r.cfg.Resume})

	// A cancelled parent context is a stop request, not an abort.
	if ctx.Done() != nil {
		context.AfterFunc(ctx, func() { _ = r.Stop("context cancelled") })
	}
	if r.opts.Signals != nil {
		go r.watchSignals(r.opts.Signals)
	}

	if r.cfg.Resume {
		if err := r.reconcileResume(loopCtx); err != nil {
			// A resume that cannot even list the repository is fatal: the
			// loop would work from a wrong picture of the world.
			r.finish(Outcome{Reason: events.FinishFailed, Summary: "resume failed"}, err)
			return err
		}
	}

	go r.drive(loopCtx)
	return nil
}

// failStart unwinds a Start that could not get off the ground: the run is
// marked finished so Wait does not block forever.
func (r *Run) failStart(err error) {
	r.log.Error("run failed to start", slog.Any("err", err))
	r.mu.Lock()
	r.st.Phase = PhaseFinished
	r.st.Finish = events.FinishFailed
	r.st.FinishError = err.Error()
	r.st.FinishedAt = r.now()
	r.finished = true
	r.outcome = Outcome{Reason: events.FinishFailed, Summary: "run failed to start"}
	r.err = err
	r.releaseLockLocked()
	r.mu.Unlock()
	close(r.done)
}

// drive runs the loop and finishes the run with whatever it returned.
func (r *Run) drive(ctx context.Context) {
	var (
		out Outcome
		err error
	)
	if r.opts.Loop != nil {
		out, err = r.opts.Loop.Run(ctx, r)
	} else {
		// No loop: stay live until something stops us. This is the shape
		// the lifecycle tests and a TUI harness use.
		<-ctx.Done()
	}
	r.finish(out, err)
}

// Wait blocks until the run has finished and returns its outcome. It
// returns [ErrNotStarted] if the run was never started.
func (r *Run) Wait() (Outcome, error) {
	r.mu.Lock()
	started, finished := r.started, r.finished
	r.mu.Unlock()
	if !started && !finished {
		return Outcome{}, ErrNotStarted
	}
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outcome, r.err
}

// Done is closed when the run has finished.
func (r *Run) Done() <-chan struct{} { return r.done }

// Pause stops the scheduler firing new agents; the running ones finish
// (D-18 caps pause a run rather than ending it). It is idempotent: pausing
// an already paused run changes nothing and publishes nothing. reason says
// who or what paused it ("user", "cost_cap", ...).
func (r *Run) Pause(reason string) error {
	r.mu.Lock()
	switch {
	case r.finished:
		r.mu.Unlock()
		return ErrFinished
	case !r.started:
		r.mu.Unlock()
		return ErrNotStarted
	case r.stopping, r.st.Phase == PhasePaused:
		r.mu.Unlock()
		return nil
	}
	r.resumeCh = make(chan struct{})
	r.st.Phase = PhasePaused
	err := r.persistLocked()
	r.mu.Unlock()

	r.log.Info("run paused", slog.String("reason", reason))
	r.Publish(events.RunPaused{Meta: r.Meta(), Reason: reason})
	return err
}

// Resume lets a paused run fire agents again. It is idempotent, and is the
// counterpart of [Run.Pause] — resuming an *earlier* run from disk is
// `--resume`, which happens before [New] (see [RestoreRunConfig]).
func (r *Run) Resume(reason string) error {
	r.mu.Lock()
	switch {
	case r.finished:
		r.mu.Unlock()
		return ErrFinished
	case !r.started:
		r.mu.Unlock()
		return ErrNotStarted
	case r.stopping, r.st.Phase != PhasePaused:
		r.mu.Unlock()
		return nil
	}
	if r.resumeCh != nil {
		close(r.resumeCh)
		r.resumeCh = nil
	}
	r.st.Phase = PhaseRunning
	err := r.persistLocked()
	r.mu.Unlock()

	r.log.Info("run resumed", slog.String("reason", reason))
	r.Publish(events.RunResumed{Meta: r.Meta(), Reason: reason})
	return err
}

// Paused reports whether the run is paused. The scheduler checks it before
// firing an agent; [Run.AwaitResume] blocks until it clears.
func (r *Run) Paused() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st.Phase == PhasePaused
}

// AwaitResume blocks while the run is paused and returns as soon as it is
// resumed, stopped, or ctx is done (in which case it returns ctx.Err()).
func (r *Run) AwaitResume(ctx context.Context) error {
	for {
		r.mu.Lock()
		ch := r.resumeCh
		paused := r.st.Phase == PhasePaused
		r.mu.Unlock()
		if !paused || ch == nil {
			return nil
		}
		select {
		case <-ch:
		case <-r.abort:
			return nil
		case <-r.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Stop asks the run to shut down gracefully and returns at once: the loop's
// context is cancelled, its agents are sent SIGTERM and killed after the
// grace period, their leases are released and their worktrees are kept
// (only closed beads' worktrees are pruned, P6-18). Use [Run.Wait] for the
// outcome. Stopping twice, or stopping a finished run, is a no-op.
func (r *Run) Stop(reason string) error {
	r.mu.Lock()
	if r.finished || r.stopping {
		r.mu.Unlock()
		return nil
	}
	r.stopping = true
	r.stopReason = reason
	r.st.StopReason = reason
	started := r.started
	if started {
		r.st.Phase = PhaseStopping
	}
	// Let anything waiting on the pause gate through: it is over.
	if r.resumeCh != nil {
		close(r.resumeCh)
		r.resumeCh = nil
	}
	cancel := r.cancelLoop
	err := error(nil)
	if started {
		err = r.persistLocked()
	}
	r.mu.Unlock()

	r.log.Info("run stopping", slog.String("reason", reason))
	r.Publish(events.RunStopped{Meta: r.Meta(), Reason: reason})
	if cancel != nil {
		cancel()
	}
	if !started {
		// Nothing is driving the run, so nothing will finish it.
		r.finish(Outcome{Reason: events.FinishStopped, Summary: "stopped before it started"}, nil)
	}
	return err
}

// Kill stops the run immediately: agents are killed without a grace period
// and the tidy-up steps a graceful stop performs (releasing leases, pruning
// worktrees) are skipped. A resumed run picks the leases up through
// beads.RecoverStale and the worktrees through gitx.Resumer, so nothing is
// lost — it is only less tidy. This is what a second interrupt does.
func (r *Run) Kill(reason string) error {
	r.abortOnce.Do(func() { close(r.abort) })
	return r.Stop(reason)
}

// Interrupt records one interrupt from the user: the first asks for a
// graceful [Run.Stop], the second and any later one [Run.Kill] the run
// immediately. reason names the source ("SIGINT", "key").
func (r *Run) Interrupt(reason string) {
	switch n := r.interrupts.Add(1); n {
	case 1:
		_ = r.Stop(reason)
	case 2:
		r.Warn(SourceRun, "second "+reason+": stopping immediately, agents will be killed", nil)
		_ = r.Kill(reason + " (twice)")
	default:
		// Already aborting; further interrupts have nothing left to escalate.
	}
}

// aborted reports whether the run was killed rather than stopped.
func (r *Run) aborted() bool {
	select {
	case <-r.abort:
		return true
	default:
		return false
	}
}

// watchSignals turns each value on ch into an [Run.Interrupt]. It returns
// when the channel closes or the run finishes.
func (r *Run) watchSignals(ch <-chan os.Signal) {
	for {
		select {
		case sig, ok := <-ch:
			if !ok {
				return
			}
			name := "interrupt"
			if sig != nil {
				name = sig.String()
			}
			r.Interrupt(name)
		case <-r.done:
			return
		}
	}
}

// InterruptChannel installs a handler for SIGINT and SIGTERM and returns
// the channel to hand to [Options.Signals], plus a function that uninstalls
// it. The first signal stops the run gracefully, the second kills it
// (P6-01); the process's own default handler is restored by stop.
func InterruptChannel() (ch <-chan os.Signal, stop func()) {
	c := make(chan os.Signal, 4)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	return c, func() { signal.Stop(c) }
}

// AgentStarted records a live agent in state.json. The scheduler calls it
// when a subprocess has been spawned, so a resumed run knows what was in
// flight and so the stop path knows whose lease to release.
func (r *Run) AgentStarted(a domain.Agent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.st.Agents {
		if existing.ID == a.ID {
			r.st.Agents[i] = a
			return r.persistLocked()
		}
	}
	r.st.Agents = append(r.st.Agents, a)
	return r.persistLocked()
}

// AgentUpdated replaces a recorded agent (a state or pid change). An
// unknown agent is added, so a caller need not track which it is.
func (r *Run) AgentUpdated(a domain.Agent) error { return r.AgentStarted(a) }

// AgentStopped removes an agent from state.json. Unknown ids are ignored.
func (r *Run) AgentStopped(id domain.AgentID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, a := range r.st.Agents {
		if a.ID != id {
			continue
		}
		r.st.Agents = append(r.st.Agents[:i:i], r.st.Agents[i+1:]...)
		return r.persistLocked()
	}
	return nil
}

// Agents returns the run's live agents.
func (r *Run) Agents() []domain.Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.Agent(nil), r.st.Agents...)
}

// ResumeResult returns what the resume pass found, or nil for a fresh run
// (P6-15). The scheduler consults it to pick the reconciled beads up.
func (r *Run) ResumeResult() *gitx.ResumeResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resumeRes
}

// reconcileResume is the --resume half of P6-15: gitx reattaches the
// worktrees and reconciles branch state into bead state, then the entries
// that need an action get one. A bead left mid-rebase goes back to its
// engineer as a conflict round; a bead whose pull request already exists
// has its (idempotent) delivery re-run so the gh:pr gate is recreated. Only
// a failure to inspect the repository at all is returned as an error;
// per-bead failures become warnings.
func (r *Run) reconcileResume(ctx context.Context) error {
	if r.opts.Resumer == nil {
		return nil
	}
	list, err := r.listBeads(ctx)
	if err != nil {
		return fmt.Errorf("resume: list the run's beads: %w", err)
	}
	res, err := r.opts.Resumer.Resume(ctx, list)
	if err != nil {
		return fmt.Errorf("resume: reattach worktrees: %w", err)
	}
	r.mu.Lock()
	r.resumeRes = res
	r.mu.Unlock()

	for _, w := range res.Warnings {
		r.Warn(SourceRun, "resume: "+w, nil)
	}
	r.log.Info("resume reconciled", slog.Int("beads", len(res.Entries)),
		slog.Int("changed", len(res.Changed())), slog.Int("orphans", len(res.Orphans)),
		slog.String("summary", res.Summary()))
	for _, e := range res.Entries {
		if e.Err != nil {
			r.Warn(SourceRun, fmt.Sprintf("resume: %s could not be reattached", e.Bead.ID), e.Err)
		}
		// Only a transition gitx actually wrote to bd is announced. An entry
		// that reconciled to a new state but could not persist it (Applied
		// is false, and gitx.ResumeResult.Summary prints "(not applied)") is
		// a bd failure: publishing it would leave Summarise, state.json, the
		// run log, `sc status` and the TUI showing a state bd disagrees
		// with, for ever, since nothing re-reads. It is the same invariant
		// the state machine holds (P6-03): a bd write that failed is a
		// warning and no event.
		if e.Changed() && e.Applied {
			r.Publish(events.BeadStateChanged{
				Meta: r.Meta(), Bead: e.Bead.ID, From: e.Bead.State, To: e.State,
				Reason: e.Reason,
			})
		}
	}
	if r.opts.OnResume == nil {
		return nil
	}
	for _, e := range res.Entries {
		switch {
		case e.Conflict:
			// A worktree left mid-rebase cannot be delivered; the engineer
			// resolves it first (D-10), even when a pull request exists.
			if err := r.opts.OnResume.ConflictRound(ctx, e); err != nil {
				r.Warn(SourceRun, fmt.Sprintf("resume: %s: could not schedule the conflict round", e.Bead.ID), err)
			}
		case e.PRState != "":
			if err := r.opts.OnResume.RedeliverPR(ctx, e); err != nil {
				r.Warn(SourceRun, fmt.Sprintf("resume: %s: could not re-run the pull request delivery", e.Bead.ID), err)
			}
		}
	}
	return nil
}

// listBeads asks the bead source for the run's beads.
func (r *Run) listBeads(ctx context.Context) ([]domain.Bead, error) {
	src := r.BeadSource()
	if src == nil {
		return nil, nil
	}
	return src(ctx)
}

// BeadSource returns the run's bead source: [Options.Beads], or whatever
// [Run.UseBeads] replaced it with. Nil means the run cannot read bd, and
// both passes that need the listing — the resume reconciliation and the
// shutdown lease release — fall back to what they can infer without it.
func (r *Run) BeadSource() BeadSource {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.beadSrc
}

// UseBeads gives a run built without one the bead source it needs (P8-17).
// [Options.Leases] without [Options.Beads] leaves the shutdown release
// blind: it falls back to the in_progress the claim set, which is right for
// an engineer and wrong for every bead a tester or the product owner was
// holding, so whoever assembles a run passes both. [NewConductor] calls
// this when it is given a run that has no source and a bd client that can
// be one, which is why an assembled run always has one.
//
// It must be called before [Run.Start]: the resume reconciliation reads the
// listing, and changing the source underneath a live run would mean two
// passes of one run disagreed about what it owns. A started run returns
// [ErrAlreadyStarted], a finished one [ErrFinished].
func (r *Run) UseBeads(src BeadSource) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.finished:
		return ErrFinished
	case r.started:
		return ErrAlreadyStarted
	}
	r.beadSrc = src
	return nil
}

// UseVitals names the function that snapshots the run's vitals — what the
// planner is doing and what the gate poller has seen — for every state.json
// the run writes (P6-29). It may be set at any time, including after the
// run has started, because the parts it reads are assembled after the run
// itself; a nil function leaves State.Vitals absent.
//
// The function is called on the run's own goroutine while the run holds its
// lock, so it must not block or call back into the run. [Vitals] over a
// *Planner and a *GatePoller does neither.
func (r *Run) UseVitals(f func() RunVitals) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vitals = f
}

// finish performs the shutdown sequence exactly once and closes Done:
// stop the agents, release their leases, prune the worktrees of closed
// beads and report the ones kept (P6-18), write the final state.json,
// publish [events.RunFinished] and release the lock.
func (r *Run) finish(out Outcome, loopErr error) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	r.finished = true
	r.st.Phase = PhaseStopping
	stopping, stopReason := r.stopping, r.stopReason
	agents := append([]domain.Agent(nil), r.st.Agents...)
	startedAt := r.st.Run.StartedAt
	_ = r.persistLocked()
	r.mu.Unlock()

	// A loop that returns because we cancelled it is not a failure.
	if loopErr != nil && errors.Is(loopErr, context.Canceled) && stopping {
		loopErr = nil
	}
	switch {
	case loopErr != nil:
		out.Reason = events.FinishFailed
		if out.Summary == "" {
			out.Summary = "the run hit an unrecoverable error"
		}
	case stopping:
		out.Reason = events.FinishStopped
		if out.Summary == "" {
			out.Summary = "stopped: " + stopReason
		}
	case out.Reason == "":
		out.Reason = events.FinishCompleted
	}

	shutCtx, cancelShut := r.abortContext(context.Background())
	defer cancelShut()

	r.stopAgents(shutCtx, agents)
	// The agents are gone, so nothing else will publish through the
	// forwarding bus; draining it now is what makes the sequence number in
	// the final state.json the whole run (P6-27).
	r.stopAgentBus()
	r.removePlanShim()
	r.releaseLeases(shutCtx, agents)
	if report := r.cleanupWorktrees(shutCtx); report != "" {
		out.Summary = joinSummary(out.Summary, report)
	}

	now := r.now()
	if !startedAt.IsZero() {
		out.Duration = now.Sub(startedAt)
	}
	r.mu.Lock()
	r.st.Agents = nil
	r.st.Phase = PhaseFinished
	r.st.Finish = out.Reason
	r.st.FinishSummary = out.Summary
	r.st.FinishedAt = now
	if loopErr != nil {
		r.st.FinishError = loopErr.Error()
	}
	r.outcome = out
	r.err = loopErr
	_ = r.persistLocked()
	r.releaseLockLocked()
	r.mu.Unlock()

	if loopErr != nil {
		r.log.Error("run failed", slog.Any("err", loopErr))
		r.Publish(events.Error{Meta: r.Meta(), Source: SourceRun, Message: "the run ended with an error", Err: loopErr.Error()})
	}
	r.log.Info("run finished", slog.String("reason", out.Reason.String()),
		slog.Duration("duration", out.Duration), slog.String("summary", out.Summary))
	r.Publish(events.RunFinished{Meta: r.Meta(), Reason: out.Reason, Summary: out.Summary, Duration: out.Duration})

	// The sequence number of RunFinished itself belongs in the file.
	r.mu.Lock()
	_ = r.persistLocked()
	r.mu.Unlock()
	close(r.done)
}

// abortContext returns a context cancelled when the run is killed, so a
// second interrupt cuts the shutdown short. The returned cancel must be
// called to release the watcher.
func (r *Run) abortContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-r.abort:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// stopAgents sends the live agents SIGTERM, kills whatever survives the
// grace period, and reaps them. A killed run gives no grace at all.
func (r *Run) stopAgents(ctx context.Context, agents []domain.Agent) {
	if r.opts.Agents == nil {
		return
	}
	grace := r.stopGrace
	if r.aborted() {
		grace = 0
	}
	r.log.Info("stopping agents", slog.Int("live", len(agents)), slog.Duration("grace", grace))
	kctx, cancel := context.WithTimeout(ctx, grace+killSlack)
	err := r.opts.Agents.StopAgents(kctx, grace)
	cancel()
	if err == nil {
		return
	}
	if r.aborted() || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		// Whatever is left gets no more time.
		if err2 := r.opts.Agents.StopAgents(context.Background(), 0); err2 == nil {
			return
		}
	}
	r.Warn(SourceRun, "some agents could not be stopped cleanly", err)
}

// releaseLeases returns the beads held by the run's live agents to the
// ready set so a resumed run can claim them (D-06). A killed run skips
// this: beads.RecoverStale picks the leases up on the next resume.
//
// The release is a D-07 transition like any other, so it goes through the
// [StateMachine] (P6-03): only the agents whose role holds the claim are
// considered ([HeldBeads]), only the in_progress --released--> open edge is
// ever taken, and the beads that do go back to open publish an
// [events.BeadStateChanged] so the run log, `sc status`, state.json and the
// TUI agree with bd. A bead a tester was still reviewing or the product
// owner was ruling on keeps its state and its round.
func (r *Run) releaseLeases(ctx context.Context, agents []domain.Agent) {
	if r.opts.Leases == nil || r.aborted() {
		return
	}
	// Ask bd where the beads are only when there is something to release.
	held := HeldBeads(agents, nil)
	if len(held) == 0 {
		return
	}
	if states := r.beadStatesForRelease(ctx); len(states) > 0 {
		held = HeldBeads(agents, states)
	}
	states := NewStateMachine(ReleaseOnlyStore{Releaser: r.opts.Leases},
		WithStatePublisher(PublisherFunc(r.Publish)),
		WithStateRun(r.ID()),
		WithStateClock(r.now))
	for _, o := range states.ReleaseHeld(ctx, held, "the run stopped") {
		switch {
		case o.Err != nil:
			r.Warn(SourceRun, fmt.Sprintf("could not release the lease on %s", o.Bead), o.Err)
		case o.Released:
			r.log.Info("lease released", slog.String("bead", o.Bead.String()), slog.String("agent", o.Agent.String()))
		default:
			r.log.Info("lease kept", slog.String("bead", o.Bead.String()),
				slog.String("agent", o.Agent.String()), slog.String("state", string(o.From)),
				slog.String("why", o.Kept))
		}
	}
}

// beadStatesForRelease reads the run's beads so the lease release knows
// where each held bead actually is. No source, or a source that fails,
// leaves the states unknown and the release falls back to the in_progress
// the claim itself set.
func (r *Run) beadStatesForRelease(ctx context.Context) []domain.Bead {
	list, err := r.listBeads(ctx)
	if err != nil {
		r.Warn(SourceRun, "could not read the run's beads before releasing their leases", err)
		return nil
	}
	return list
}

// cleanupWorktrees is P6-18: at run finish the worktrees of closed beads
// are pruned and the ones kept for a human are named. The bead list is the
// run's own beads with their final states, so a bead that failed, was
// escalated or was still in flight keeps its worktree. The report goes to
// [Options.Out] — os.Stdout in headless mode, nothing in the TUI, which
// owns the terminal — and is returned so it also reaches the run log and
// [events.RunFinished.Summary]. A killed run skips the pass entirely.
func (r *Run) cleanupWorktrees(ctx context.Context) string {
	if r.opts.Cleaner == nil || r.aborted() {
		return ""
	}
	list, err := r.listBeads(ctx)
	if err != nil {
		r.Warn(SourceRun, "could not list the run's beads for the worktree cleanup", err)
		return ""
	}
	res, err := r.opts.Cleaner.Cleanup(ctx, list, gitx.CleanupOptions{Logger: r.log, Out: r.cleanupOut()})
	if err != nil {
		r.Warn(SourceRun, "the worktree cleanup did not run", err)
		return ""
	}
	if err := res.Err(); err != nil {
		r.Warn(SourceRun, "some worktrees could not be cleaned up", err)
	}
	summary := res.Summary()
	r.log.Info("worktree cleanup", slog.String("summary", summary), slog.Any("kept", res.KeptPaths()))
	return summary
}

// cleanupOut resolves where the finish report is printed: an explicit
// [Options.Out], else stdout when the run is headless (D-15), else nowhere
// because bubbletea owns the terminal.
func (r *Run) cleanupOut() io.Writer {
	if r.opts.Out != nil {
		return r.opts.Out
	}
	if r.cfg.Run.Config.Headless {
		return os.Stdout
	}
	return nil
}

// joinSummary appends the cleanup report to the outcome summary.
func joinSummary(summary, report string) string {
	if summary == "" {
		return report
	}
	if report == "" {
		return summary
	}
	return summary + "\n" + report
}

// Close releases the run lock and is safe to call more than once. A run
// that finished has already released it; Close is for a run that was
// created or started and then abandoned.
func (r *Run) Close() error {
	r.stopAgentBus()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releaseLockLocked()
	return nil
}

// persistLocked stamps and writes state.json. The caller holds r.mu.
func (r *Run) persistLocked() error {
	r.st.Version = StateFileVersion
	r.st.UpdatedAt = r.now()
	if r.vitals != nil {
		v := r.vitals()
		r.st.Vitals = &v
	}
	if err := WriteState(r.dirs.Dir, r.st); err != nil {
		r.log.Error("could not write the run state", slog.Any("err", err))
		return err
	}
	return nil
}

// releaseLockLocked drops the run lock if this run holds it. The caller
// holds r.mu.
func (r *Run) releaseLockLocked() {
	if r.lock == nil {
		return
	}
	if err := r.lock.release(); err != nil {
		r.log.Warn("could not release the run lock", slog.String("path", r.lock.path), slog.Any("err", err))
	}
	r.lock = nil
}

// LockHolder is what a run lock records about the process holding it.
type LockHolder struct {
	// Run is the run that took the lock.
	Run domain.RunID `json:"run"`
	// PID is the process id of the sc that took it.
	PID int `json:"pid"`
	// Host is the machine it runs on; a state directory can be on a shared
	// file system.
	Host string `json:"host,omitempty"`
	// TakenAt is when the lock was taken.
	TakenAt time.Time `json:"taken_at"`
}

// LockPolicy decides whether an existing run lock may be taken over. It is
// the hook P9-04 replaces with the hardened version (liveness probing with
// a grace period, an explicit --force, and a clear message naming the other
// run); until then [DefaultLockPolicy] applies the minimal rule below.
type LockPolicy interface {
	// Stale reports whether the lock described by h may be taken over.
	Stale(h LockHolder) bool
}

// DefaultLockPolicy takes over a lock held by this very process (the same
// run being resumed in-process) or by a process that no longer exists, and
// refuses every other. P9-04 replaces it.
type DefaultLockPolicy struct{}

// Stale implements [LockPolicy].
func (DefaultLockPolicy) Stale(h LockHolder) bool {
	if h.PID <= 0 || h.PID == os.Getpid() {
		return true
	}
	return !processAlive(h.PID)
}

// processAlive reports whether a process with that id exists. P9-04
// replaces this with something that also tells "not mine" from "not there".
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// LockPath returns the run lock of the repository a run directory belongs
// to: <state-dir>/run.lock, the grandparent of <state-dir>/runs/<run-id>
// (D-23). The lock deliberately does *not* live in the repository, so no
// `.slop-cannon/run.lock` ever appears in the user's working tree.
func LockPath(runDir string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(filepath.Clean(runDir))), LockFileName)
}

// lockFile is a held run lock.
type lockFile struct {
	path   string
	holder LockHolder
}

// acquireLock takes the repository's run lock for the run whose directory
// is runDir. An existing lock is taken over only when policy says it is
// stale; otherwise [ErrLocked] is returned, naming the other run.
func acquireLock(runDir string, run domain.RunID, policy LockPolicy, now time.Time) (*lockFile, error) {
	if policy == nil {
		policy = DefaultLockPolicy{}
	}
	path := LockPath(runDir)
	if err := os.MkdirAll(filepath.Dir(path), runDirPerm); err != nil {
		return nil, fmt.Errorf("orchestrator: create the state directory: %w", err)
	}
	host, _ := os.Hostname()
	holder := LockHolder{Run: run, PID: os.Getpid(), Host: host, TakenAt: now.UTC()}
	data, err := json.MarshalIndent(holder, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("orchestrator: encode the run lock: %w", err)
	}
	data = append(data, '\n')

	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, runFilePerm)
		if err == nil {
			_, werr := f.Write(data)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("orchestrator: write the run lock: %w", errors.Join(werr, cerr))
			}
			return &lockFile{path: path, holder: holder}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("orchestrator: take the run lock: %w", err)
		}
		existing, readErr := readLock(path)
		if readErr == nil && !policy.Stale(existing) {
			return nil, fmt.Errorf("%w: run %s (pid %d on %s, since %s) is using %s; stop it first, or resume it",
				ErrLocked, existing.Run, existing.PID, existing.Host,
				existing.TakenAt.Format(time.RFC3339), filepath.Dir(path))
		}
		// Stale, or unreadable and therefore useless: take it over.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("orchestrator: drop the stale run lock: %w", err)
		}
	}
	return nil, fmt.Errorf("%w: %s keeps being retaken", ErrLocked, path)
}

// readLock reads a lock file.
func readLock(path string) (LockHolder, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is sc's own state directory (D-23).
	if err != nil {
		return LockHolder{}, err
	}
	var h LockHolder
	if err := json.Unmarshal(data, &h); err != nil {
		return LockHolder{}, err
	}
	return h, nil
}

// release removes the lock file if it is still ours.
func (l *lockFile) release() error {
	existing, err := readLock(l.path)
	if err == nil && (existing.Run != l.holder.Run || existing.PID != l.holder.PID) {
		// Somebody took it over; leave theirs alone.
		return nil
	}
	if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// The conductor: the run-level glue above the scheduler (P8-14)
// ---------------------------------------------------------------------------

// The scheduler (P6-02) fires whatever the work sources offer, and the
// feedback loop (P6-04), the planner (P6-05) and the delivery handoff
// (P6-06) are those sources — but three things happen *between* bd and
// those parts that none of them owns, and without them an assembled run
// does nothing at all:
//
//   - a bead bd reports ready has to be offered to the feedback loop
//     ([Feedback.Track]), or no engineer is ever asked for it (D-20 leaves
//     the blocker filtering to `bd ready`);
//   - a bead the loop approved has to be handed to the delivery
//     ([Deliverer.Deliver]), and what the delivery decided has to be routed
//     back into the loop ([Feedback.CheckFailed], [Feedback.ReviewAgain],
//     [Feedback.Observe]) and to the gate poller ([GatePoller.Kick]);
//   - an engineer's uncommitted leftovers have to be committed before the
//     testers look at the code (D-14), through the [DeliveryGit] seam that
//     refuses to commit a worktree left mid-rebase (P6-16(b), P8-18).
//
// [Conductor] is those three, plus the two things only something above bd
// can do: rebuild the feedback threads of a resumed run from the bead's own
// comments ([Feedback.Restore], P8-16) and publish the bead inventory and
// the dependency graph the run log had neither of ([events.BeadCreated],
// [events.GraphChanged], P6-32).

// DefaultConductorInterval is how often the conductor syncs with bd when
// the caller sets no interval. Every pass costs a `bd list` and a `bd
// ready` (about 0.2 s each, Appendix A), so a second keeps the serialised
// client free for the agents' own calls.
const DefaultConductorInterval = time.Second

// SourceConductor is the events.Warning.Source the run-level glue stamps on
// its own notices.
const SourceConductor = "conductor"

// ConductorBeads is the bd the glue reads: the run's own beads, the
// claimable ones, one bead in full, and a bead's structured comments (for
// [Feedback.Restore] on resume). *beads.Client satisfies it.
type ConductorBeads interface {
	// ListRun lists the run's beads, closed ones included (`bd list -l
	// sc:run:<id>`, P3-14).
	ListRun(ctx context.Context, run domain.RunID, all bool) ([]beads.Issue, error)
	// Ready lists the claimable beads under the run's epic (`bd ready`),
	// which already excludes anything whose blockers are open (D-20).
	Ready(ctx context.Context, parent domain.BeadID) ([]beads.Issue, error)
	// Show reads one bead in full, for the prompt context.
	Show(ctx context.Context, ids ...domain.BeadID) ([]beads.Issue, error)
	// Messages reads a bead's structured comments (D-06, D-25).
	Messages(ctx context.Context, id domain.BeadID) ([]beads.Message, error)
}

// *beads.Client is the production bd of the glue.
var _ ConductorBeads = (*beads.Client)(nil)

// ConductorFeedback is the slice of [Feedback] the glue drives: what to
// adopt, what a resumed run must rebuild, and where a delivery outcome
// goes. *Feedback satisfies it.
type ConductorFeedback interface {
	// Track adopts beads into the loop (or refreshes them).
	Track(bs ...domain.Bead)
	// Restore rebuilds one bead's thread from its comments (P8-16).
	Restore(ctx context.Context, bead domain.Bead, msgs []beads.Message) error
	// Thread reads a thread back, for the round a delivery belongs to.
	Thread(bead domain.BeadID) (FeedbackThread, bool)
	// Observe tells the loop where a bead ended up.
	Observe(bead domain.BeadID, state domain.BeadState)
	// CheckFailed reopens a bead whose check command failed (P6-06).
	CheckFailed(ctx context.Context, bead domain.Bead, round int, items []domain.FeedbackItem) error
	// ReviewAgain sends the testers back to a resolved conflict that moved
	// more than the D-09 threshold.
	ReviewAgain(ctx context.Context, bead domain.Bead, round int, reason string) error
}

// *Feedback is the production feedback loop of the glue.
var _ ConductorFeedback = (*Feedback)(nil)

// ConductorDelivery is the slice of [Deliverer] the glue drives.
// *Deliverer satisfies it.
type ConductorDelivery interface {
	// Deliver ships one approved bead.
	Deliver(ctx context.Context, req DeliveryRequest) (*DeliveryOutcome, error)
	// Resolved is called when the engineer of a conflict round has
	// finished (D-10).
	Resolved(ctx context.Context, id domain.BeadID) (*DeliveryOutcome, error)
}

// *Deliverer is the production delivery handoff of the glue.
var _ ConductorDelivery = (*Deliverer)(nil)

// GateKicker asks the gate poller to look now rather than at its next tick,
// for a delivery that just opened a pull request. *GatePoller satisfies it.
type GateKicker interface {
	Kick()
}

// *GatePoller is the production gate poller.
var _ GateKicker = (*GatePoller)(nil)

// ConductorWorktrees is the worktree management the prompt context needs: a
// firing runs in the bead's worktree, so one is created on first use.
// *gitx.Manager satisfies it.
type ConductorWorktrees interface {
	// PathFor is where the bead's worktree lives (D-23).
	PathFor(id domain.BeadID) string
	// Create makes it if it is not there.
	Create(ctx context.Context, bead domain.Bead) (*gitx.Worktree, error)
}

// *gitx.Manager is the production worktree manager.
var _ ConductorWorktrees = (*gitx.Manager)(nil)

// ConductorOptions configures [NewConductor]. Run and Beads are required;
// the parts the glue drives may be attached afterwards ([Conductor.Attach]),
// because the feedback loop is built with [Conductor.BeadContext] and the
// delivery handoff with the run, so neither exists yet when the conductor
// is created.
type ConductorOptions struct {
	// Run is the run being conducted. Required.
	Run *Run
	// Beads is bd. Required.
	Beads ConductorBeads
	// Statuses maps bd's registered statuses to the D-07 states, so a
	// foreign status is never acted on (P6-12). Nil still resolves every
	// D-07 state.
	Statuses beads.StatusMap

	// Feedback, Delivery and Gates are the parts the glue drives. Any of
	// them may be nil here and attached later; a nil one at sync time means
	// that half of the glue does nothing.
	Feedback ConductorFeedback
	Delivery ConductorDelivery
	Gates    GateKicker

	// Worktrees creates the worktree a firing runs in; nil means the
	// context carries no worktree, which only a test wants.
	Worktrees ConductorWorktrees
	// Git is the delivery's git seam. The leftovers commit of D-14 goes
	// through it rather than through gitx.Manager directly, because it is
	// the one that refuses to commit a worktree left mid-rebase
	// (ErrRebaseInProgress, P6-16(b), P8-18). Nil commits nothing and
	// measures no diffs.
	Git DeliveryGit

	// Repo, Brief, Rules, Vibes, Budget and CheckCommand are the parts of
	// the prompt context that do not change from firing to firing.
	Repo         prompt.RepoContext
	Brief        string
	Rules        []prompt.Doc
	Vibes        []prompt.Doc
	Budget       prompt.Budget
	CheckCommand string
	// Model, MaxTurns and Timeout are passed to the cartridge; zero values
	// mean the adapter's (and the supervisor's) defaults.
	Model    string
	MaxTurns int
	Timeout  time.Duration
	// Env and InheritEnv are passed to the cartridge on top of the
	// read-only bd shim the supervisor installs (D-21, P6-17).
	Env        map[string]string
	InheritEnv []string

	// Foreign reports a bead bd holds in a status this run did not
	// register, so a repository whose beads all sit in someone else's
	// status says so once instead of looking like a run doing nothing
	// (P6-28). Nil builds one over the run, so the report is never lost;
	// pass the run's own reporter to share the once-per-bead bookkeeping
	// with the gate poller.
	Foreign *ForeignStatusReporter

	// Interval is how often [Conductor.Run] syncs; zero means
	// [DefaultConductorInterval].
	Interval time.Duration
	// Restore says whether the first pass rebuilds the feedback threads
	// from the beads' comments (P8-16). The zero value follows the run:
	// a resumed run restores, a fresh one does not.
	Restore *bool
	// Logger is where the glue logs; nil means the run's logger.
	Logger *slog.Logger
}

// ConductorStats is a point-in-time snapshot of what the glue has done.
type ConductorStats struct {
	// Passes counts completed syncs and Failures the ones that hit an
	// error (a pass can do most of its work and still fail).
	Passes   int
	Failures int
	// Tracked is how many beads have been offered to the feedback loop,
	// Restored how many threads were rebuilt from comments (P8-16).
	Tracked  int
	Restored int
	// Announced counts the beads first seen by the glue (one
	// events.BeadCreated each) and Graphs the events.GraphChanged
	// published (P6-32).
	Announced int
	Graphs    int
	// Delivered counts delivery attempts started, Shipped the ones that
	// merged or opened a pull request.
	Delivered int
	Shipped   int
	// LastError is the most recent pass failure, nil when the last pass
	// was clean.
	LastError error
}

// Conductor is the run-level glue: the loop above the scheduler that keeps
// bd, the feedback loop and the delivery handoff in step. Create one with
// [NewConductor], hand [Conductor.BeadContext], [Conductor.ChangeRatio] and
// [Conductor.ConflictResolved] to [NewFeedback], attach the parts once they
// exist, and run it beside the scheduler:
//
//	go func() { _ = poller.Run(ctx) }()
//	go func() { _ = conductor.Run(ctx) }()
//	err := run.Start(ctx)
//
// All methods are safe for concurrent use.
type Conductor struct {
	run      *Run
	beads    ConductorBeads
	statuses beads.StatusMap
	foreign  *ForeignStatusReporter
	opts     ConductorOptions
	interval time.Duration
	log      *slog.Logger
	restore  bool

	mu       sync.Mutex
	feedback ConductorFeedback
	delivery ConductorDelivery
	gates    GateKicker
	inflight map[domain.BeadID]bool
	restored map[domain.BeadID]bool
	seen     map[domain.BeadID]bool
	snapshot []beads.Issue
	warned   string
	stats    ConductorStats
}

// NewConductor builds the glue. It gives the run its bead source when it
// has none (P8-17), so an assembled run's shutdown release and worktree
// cleanup always see where bd holds the beads.
func NewConductor(opts ConductorOptions) (*Conductor, error) {
	if opts.Run == nil {
		return nil, errors.New("orchestrator: NewConductor: no run")
	}
	if opts.Beads == nil {
		return nil, errors.New("orchestrator: NewConductor: no beads client")
	}
	c := &Conductor{
		run:      opts.Run,
		beads:    opts.Beads,
		statuses: opts.Statuses,
		foreign:  opts.Foreign,
		opts:     opts,
		interval: opts.Interval,
		log:      opts.Logger,
		feedback: opts.Feedback,
		delivery: opts.Delivery,
		gates:    opts.Gates,
		inflight: map[domain.BeadID]bool{},
		restored: map[domain.BeadID]bool{},
		seen:     map[domain.BeadID]bool{},
	}
	if c.interval <= 0 {
		c.interval = DefaultConductorInterval
	}
	if c.log == nil {
		c.log = opts.Run.Log()
	}
	if c.foreign == nil {
		c.foreign = NewForeignStatusReporter(ForeignStatusOptions{
			Host: opts.Run, Statuses: opts.Statuses, Logger: c.log,
		})
	}
	c.restore = opts.Run.RunConfig().Resume
	if opts.Restore != nil {
		c.restore = *opts.Restore
	}
	if c.run.BeadSource() == nil {
		// P8-17: Options.Beads is wired wherever Options.Leases is, and
		// this is the assembly point that knows both.
		if err := c.run.UseBeads(c.BeadSource()); err != nil && !errors.Is(err, ErrAlreadyStarted) {
			return nil, err
		}
	}
	return c, nil
}

// ConductorParts are the components the glue drives, for [Conductor.Attach].
type ConductorParts struct {
	// Feedback is the feedback loop; nil leaves the one already attached.
	Feedback ConductorFeedback
	// Delivery is the delivery handoff; nil leaves the one already attached.
	Delivery ConductorDelivery
	// Gates is the gate poller; nil leaves the one already attached.
	Gates GateKicker
}

// Attach gives the conductor the parts that could not exist when it was
// created: the feedback loop takes [Conductor.BeadContext] as its context
// function, and the delivery handoff takes the run. A nil field leaves what
// is already attached alone, so Attach can be called more than once.
func (c *Conductor) Attach(p ConductorParts) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.Feedback != nil {
		c.feedback = p.Feedback
	}
	if p.Delivery != nil {
		c.delivery = p.Delivery
	}
	if p.Gates != nil {
		c.gates = p.Gates
	}
}

// parts reads the attached components under the lock.
func (c *Conductor) parts() (ConductorFeedback, ConductorDelivery, GateKicker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.feedback, c.delivery, c.gates
}

// Interval returns the sync period in force.
func (c *Conductor) Interval() time.Duration { return c.interval }

// Foreign is the reporter that names a bead bd holds in a status this run
// did not register (P6-28). It is never nil.
func (c *Conductor) Foreign() *ForeignStatusReporter { return c.foreign }

// Stats returns what the glue has done so far.
func (c *Conductor) Stats() ConductorStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// BeadSource returns the run's bead listing as a [BeadSource]: the run's own
// beads with the states bd holds, converted through [beads.SafeBeads] so a
// status the cannon never registered is reported as it is rather than acted
// on (P6-12). The planner's stop condition, the shutdown lease release and
// the worktree cleanup all read it.
func (c *Conductor) BeadSource() BeadSource {
	return NewBeadSource(c.beads, c.run.ID(), c.statuses)
}

// NewBeadSource returns the [BeadSource] of one run over a bd client.
func NewBeadSource(l BeadLister, run domain.RunID, statuses beads.StatusMap) BeadSource {
	if l == nil || run == "" {
		return nil
	}
	return func(ctx context.Context) ([]domain.Bead, error) {
		issues, err := l.ListRun(ctx, run, true)
		if err != nil {
			return nil, err
		}
		return beads.SafeBeads(issues, statuses), nil
	}
}

// BeadLister is the run-scoped listing [NewBeadSource] needs;
// *beads.Client satisfies it.
type BeadLister interface {
	ListRun(ctx context.Context, run domain.RunID, all bool) ([]beads.Issue, error)
}

// *beads.Client is the production bead lister.
var _ BeadLister = (*beads.Client)(nil)

// Run syncs until ctx is done or the run has finished, and returns
// ctx.Err() (nil when the run finished first). It paces itself: the wait
// between passes is the longer of the interval and the time the last pass
// took, so a slow bd — every pass spawns subprocesses through one
// serialised client — makes the glue run less often rather than starving
// the agents' own calls.
func (c *Conductor) Run(ctx context.Context) error {
	for {
		started := time.Now()
		if err := c.Sync(ctx); err != nil && ctx.Err() == nil {
			c.warn(err)
		}
		wait := c.interval
		if spent := time.Since(started); spent > wait {
			wait = spent
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-c.run.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// Sync is one pass: read the run's beads, announce the ones the log has not
// seen, adopt the ready ones into the feedback loop (rebuilding the threads
// of a resumed run first), and deliver the approved ones. Every step is
// attempted even when an earlier one failed — a pass that cannot read the
// ready set can still ship an approved bead — and the failures come back
// joined.
func (c *Conductor) Sync(ctx context.Context) error {
	issues, err := c.beads.ListRun(ctx, c.run.ID(), true)
	if err != nil {
		c.record(false, fmt.Errorf("listing the run's beads: %w", err))
		return fmt.Errorf("orchestrator: conductor: listing the run's beads: %w", err)
	}
	list := beads.SafeBeads(issues, c.statuses)
	c.announce(issues, list)

	var errs []error
	if c.restore {
		if err := c.restoreThreads(ctx, list); err != nil {
			errs = append(errs, err)
		}
	}
	if err := c.trackReady(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := c.deliverApproved(ctx, list); err != nil {
		errs = append(errs, err)
	}
	joined := errors.Join(errs...)
	c.record(true, joined)
	return joined
}

// trackReady offers the claimable beads to the feedback loop. `bd ready`
// already leaves out anything whose blockers are open (D-20), so this is
// the whole of the dependency policy.
func (c *Conductor) trackReady(ctx context.Context) error {
	feedback, _, _ := c.parts()
	epic := c.run.Epic()
	if feedback == nil || epic == "" {
		// Nothing has been planned yet, so there is nothing to be ready.
		return nil
	}
	issues, err := c.beads.Ready(ctx, epic)
	if err != nil {
		return fmt.Errorf("orchestrator: conductor: listing the ready beads: %w", err)
	}
	ready := beads.SafeBeads(issues, c.statuses)
	feedback.Track(ready...)
	c.mu.Lock()
	c.stats.Tracked += len(ready)
	c.mu.Unlock()
	return nil
}

// restoreThreads rebuilds the feedback threads of a resumed run from the
// beads' own comments (P8-16), once per bead. Without it a resumed run
// re-fires a changes_requested bead in implement mode and throws its items,
// responses, holds and rulings away.
//
// Only a bead that is past the open state and actually carries structured
// comments is restored: [Feedback.Restore] concludes the thread it replays,
// and a bead with nothing to replay would be concluded out of a state it
// never left. Everything else is adopted the ordinary way.
func (c *Conductor) restoreThreads(ctx context.Context, list []domain.Bead) error {
	feedback, _, _ := c.parts()
	if feedback == nil {
		return nil
	}
	var errs []error
	for _, b := range list {
		if b.ID == "" || b.Type == domain.BeadTypeEpic || !conductorRestorable(b) {
			continue
		}
		c.mu.Lock()
		done := c.restored[b.ID]
		c.restored[b.ID] = true
		c.mu.Unlock()
		if done {
			continue
		}
		msgs, err := c.beads.Messages(ctx, b.ID)
		if err != nil {
			c.mu.Lock()
			delete(c.restored, b.ID)
			c.mu.Unlock()
			errs = append(errs, fmt.Errorf("orchestrator: conductor: reading %s's comments: %w", b.ID, err))
			continue
		}
		if len(msgs) == 0 {
			feedback.Track(b)
			continue
		}
		if err := feedback.Restore(ctx, b, msgs); err != nil {
			errs = append(errs, fmt.Errorf("orchestrator: conductor: restoring %s's thread: %w", b.ID, err))
			continue
		}
		c.mu.Lock()
		c.stats.Restored++
		c.mu.Unlock()
		c.log.Info("feedback thread restored from the bead's comments",
			slog.String("bead", b.ID.String()), slog.String("state", string(b.State)),
			slog.Int("messages", len(msgs)))
	}
	return errors.Join(errs...)
}

// conductorRestorable reports whether a resumed bead has a thread worth
// rebuilding: it is in one of the states the loop reaches only after an
// engineer has submitted something, so its comments describe a conversation
// rather than nothing at all.
func conductorRestorable(b domain.Bead) bool {
	switch b.State {
	case domain.BeadStateInProgress, domain.BeadStateInReview,
		domain.BeadStateChangesRequested, domain.BeadStateEscalated,
		domain.BeadStateApproved, domain.BeadStateDelivering:
		return true
	default:
		return false
	}
}

// deliverApproved ships every bead the loop approved, at most once at a
// time per bead.
func (c *Conductor) deliverApproved(ctx context.Context, list []domain.Bead) error {
	_, delivery, _ := c.parts()
	if delivery == nil {
		return nil
	}
	var errs []error
	for _, b := range list {
		if b.State != domain.BeadStateApproved || b.Type == domain.BeadTypeEpic {
			continue
		}
		if !c.claim(b.ID) {
			continue
		}
		if err := c.Deliver(ctx, b); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Deliver ships one bead and routes the outcome back into the loops. The
// bead is held in flight until an outcome says it may be delivered again,
// so a pass that overlaps the previous one never ships it twice.
func (c *Conductor) Deliver(ctx context.Context, b domain.Bead) error {
	feedback, delivery, _ := c.parts()
	if delivery == nil {
		return nil
	}
	round := 1
	if b.Round > 0 {
		round = b.Round
	}
	if feedback != nil {
		if th, ok := feedback.Thread(b.ID); ok && th.Round > 0 {
			round = th.Round
		}
	}
	c.mu.Lock()
	c.stats.Delivered++
	c.mu.Unlock()
	out, err := delivery.Deliver(ctx, DeliveryRequest{Bead: b, Round: round})
	if err != nil {
		c.release(b.ID)
		return fmt.Errorf("orchestrator: conductor: delivering %s: %w", b.ID, err)
	}
	return c.ApplyOutcome(ctx, b.ID, out)
}

// ApplyOutcome is the boundary between the delivery handoff and the
// feedback loop: the deliverer has already written every status, so the
// loop is only told where the bead went (P6-06). A pull request that is now
// waiting for its gate kicks the gate poller so the merge is noticed
// without waiting for its next tick (D-20).
func (c *Conductor) ApplyOutcome(ctx context.Context, id domain.BeadID, out *DeliveryOutcome) error {
	feedback, _, gates := c.parts()
	if out == nil {
		return nil
	}
	c.log.Info("delivery outcome", slog.String("bead", id.String()),
		slog.String("kind", out.Kind.String()), slog.String("state", string(out.State)),
		slog.String("summary", out.Summary()))
	bead := domain.Bead{ID: id, State: out.State, Round: out.Round}
	var err error
	switch out.Kind {
	case DeliveryPending:
		// Safety on: the bead waits in delivering behind its gh:pr gate.
		c.mu.Lock()
		c.stats.Shipped++
		c.mu.Unlock()
		if gates != nil {
			gates.Kick()
		}
	case DeliveryMerged:
		c.mu.Lock()
		c.stats.Shipped++
		c.mu.Unlock()
	case DeliveryCheckFailed:
		c.release(id)
		if feedback != nil {
			err = feedback.CheckFailed(ctx, bead, out.Round, out.Items)
		}
	case DeliveryNeedsReview:
		c.release(id)
		if feedback != nil {
			err = feedback.ReviewAgain(ctx, bead, out.Round, out.Reason)
		}
	case DeliveryConflict, DeliveryRetryable:
		// The engineer has the bead (D-10), or the attempt may be retried.
		c.release(id)
	case DeliveryHandedOff:
		// A person owns it now; nothing here may pick it back up.
	}
	if feedback != nil {
		feedback.Observe(id, out.State)
	}
	if err != nil {
		return fmt.Errorf("orchestrator: conductor: acting on the delivery of %s: %w", id, err)
	}
	return nil
}

// ConflictResolved is [FeedbackOptions.ConflictResolved]: the engineer of a
// conflict round has finished, so the delivery handoff decides what happens
// to the bead next (P6-06) and its outcome is routed like any other.
func (c *Conductor) ConflictResolved(ctx context.Context, id domain.BeadID) error {
	_, delivery, _ := c.parts()
	if delivery == nil {
		return nil
	}
	out, err := delivery.Resolved(ctx, id)
	if err != nil {
		return fmt.Errorf("orchestrator: conductor: re-delivering %s after its conflict: %w", id, err)
	}
	return c.ApplyOutcome(ctx, id, out)
}

// ChangeRatio is [FeedbackOptions.Ratio]: the D-09 measurement, taken from
// the real repository through the delivery's git seam.
func (c *Conductor) ChangeRatio(ctx context.Context, id domain.BeadID, previous, current *gitx.Diff) (gitx.ChangeRatio, error) {
	if c.opts.Git == nil {
		return gitx.ChangeRatio{}, nil
	}
	return c.opts.Git.ChangeRatio(ctx, id, previous, current)
}

// BeadContext is [FeedbackOptions.Context]: everything outside the loop a
// prompt needs. It makes sure the bead has a worktree, commits whatever the
// last engineer left uncommitted so the testers always review committed
// code (D-14), and describes the change set.
//
// The leftovers commit goes through [DeliveryGit] rather than straight to
// gitx (P8-18): a conflict round's prompt is built while the worktree is
// mid-rebase by construction, and "git add -A" followed by a commit there
// would stage the conflict markers and destroy the rebase state (P6-16(b)).
// The adapter refuses that with [ErrRebaseInProgress], which is not a
// failure of the round — there is simply nothing to commit until the
// engineer has resolved the conflict, and nothing to diff either, since
// mid-rebase HEAD is the commit the branch is being rebased onto.
func (c *Conductor) BeadContext(ctx context.Context, id domain.BeadID) (FeedbackBeadContext, error) {
	feedback, _, _ := c.parts()
	issues, err := c.beads.Show(ctx, id)
	if err != nil {
		return FeedbackBeadContext{}, fmt.Errorf("orchestrator: conductor: reading %s: %w", id, err)
	}
	if len(issues) == 0 {
		return FeedbackBeadContext{}, fmt.Errorf("orchestrator: conductor: bd knows no bead %s", id)
	}
	bead, _ := issues[0].SafeBead(c.statuses)
	round := 1
	if feedback != nil {
		if th, ok := feedback.Thread(id); ok && th.Round > 0 {
			round = th.Round
		}
	}

	worktree := ""
	if c.opts.Worktrees != nil {
		worktree = c.opts.Worktrees.PathFor(id)
		if _, err := os.Stat(worktree); err != nil {
			if _, err := c.opts.Worktrees.Create(ctx, bead); err != nil {
				return FeedbackBeadContext{}, fmt.Errorf("orchestrator: conductor: worktree for %s: %w", id, err)
			}
		}
	}

	var (
		notes     []string
		midRebase bool
		diff      *gitx.Diff
	)
	if c.opts.Git != nil {
		switch _, err := c.opts.Git.CommitLeftovers(ctx, bead, round); {
		case err == nil:
		case errors.Is(err, ErrRebaseInProgress):
			midRebase = true
			notes = append(notes, "this worktree is in the middle of a rebase: "+
				"nothing was committed and the diff below is the branch as it stood before it")
		default:
			return FeedbackBeadContext{}, fmt.Errorf("orchestrator: conductor: committing %s's leftovers: %w", id, err)
		}
		if !midRebase {
			if diff, err = c.opts.Git.Diff(ctx, id); err != nil {
				return FeedbackBeadContext{}, fmt.Errorf("orchestrator: conductor: diffing %s: %w", id, err)
			}
		}
	}

	return FeedbackBeadContext{
		Bead: prompt.BeadContext{
			Bead:     bead,
			Epic:     c.run.Epic(),
			Worktree: worktree,
			Branch:   gitx.BranchName(bead),
			Round:    round,
			Notes:    notes,
		},
		Repo:         c.opts.Repo,
		Brief:        c.opts.Brief,
		Rules:        c.opts.Rules,
		Vibes:        c.opts.Vibes,
		Budget:       c.opts.Budget,
		CheckCommand: c.opts.CheckCommand,
		Diff:         diff,
		Model:        c.opts.Model,
		MaxTurns:     c.opts.MaxTurns,
		Timeout:      c.opts.Timeout,
		Env:          c.opts.Env,
		InheritEnv:   c.opts.InheritEnv,
		Notes:        notes,
	}, nil
}

// announce publishes the bead inventory and the dependency graph (P6-32).
// Both event kinds existed, round-trip through the codec and are folded by
// [Summarise], but nothing in a real run emitted either: the run log
// carried no bead the plan had not named — an external-PR review bead
// (D-11) never reached `sc status` — and the TUI graph relayed out of
// nothing. The conductor is already listing the run's beads every pass, so
// the diff costs nothing extra; [beads.DiffIssues] is the same comparison
// [beads.Watcher] makes, rather than a second implementation of it.
func (c *Conductor) announce(issues []beads.Issue, list []domain.Bead) {
	// P6-28: a bead in a status this run never registered is passed over
	// everywhere else in silence. The listing is already here, and the
	// reporter names each such bead once for the whole run.
	c.foreign.Observe(issues...)

	c.mu.Lock()
	change := beads.DiffIssues(c.snapshot, issues)
	c.snapshot = issues
	added := make([]domain.Bead, 0, len(change.Added))
	for _, id := range change.Added {
		if c.seen[id] {
			continue
		}
		c.seen[id] = true
		for _, b := range list {
			if b.ID == id && b.Type != domain.BeadTypeEpic {
				added = append(added, b)
				break
			}
		}
	}
	empty := change.Empty()
	c.stats.Announced += len(added)
	if !empty {
		c.stats.Graphs++
	}
	c.mu.Unlock()

	for _, b := range added {
		c.run.Publish(events.BeadCreated{Meta: c.run.Meta(), Bead: b})
	}
	if empty {
		return
	}
	work := make([]domain.Bead, 0, len(list))
	epic := c.run.Epic()
	for _, b := range list {
		if b.Type == domain.BeadTypeEpic || b.ID == epic {
			continue
		}
		work = append(work, b)
	}
	c.run.Publish(events.GraphChanged{Meta: c.run.Meta(), Epic: epic, Beads: work})
}

// claim reserves a bead for delivery so two passes never ship one twice.
func (c *Conductor) claim(id domain.BeadID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight[id] {
		return false
	}
	c.inflight[id] = true
	return true
}

// release lets a bead be delivered again: a retryable failure, a check
// failure or a conflict, all of which come back through the feedback loop.
func (c *Conductor) release(id domain.BeadID) {
	c.mu.Lock()
	delete(c.inflight, id)
	c.mu.Unlock()
}

// record counts a pass and remembers its error.
func (c *Conductor) record(completed bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if completed {
		c.stats.Passes++
	}
	if err != nil {
		c.stats.Failures++
	}
	c.stats.LastError = err
}

// warn publishes a pass failure, at most once per run of identical ones: bd
// being down means every pass fails, and one warning per second for an hour
// is not a report.
func (c *Conductor) warn(err error) {
	msg := err.Error()
	c.mu.Lock()
	repeat := c.warned == msg
	c.warned = msg
	c.mu.Unlock()
	if repeat {
		c.log.Debug("the run-level sync is still failing", slog.Any("err", err))
		return
	}
	c.run.Warn(SourceConductor, "a run-level sync with bd failed; the run carries on and retries", err)
}
