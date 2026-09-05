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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"slop-cannon/internal/agents"
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

// StateFlushDelay is how long state.json may lag the run's own memory when
// the only thing that moved is the live agent list (P9-11). It is
// wall-clock rather than the run's clock on purpose: it bounds a
// background write, not the run's behaviour.
const StateFlushDelay = 50 * time.Millisecond

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
// implements all three halves (*Deliverer, P6-06): a pull request is
// re-opened through the same idempotent path that opened it, an
// interrupted delivery is run again, and a conflict round is offered to
// the scheduler as engineer work. The feedback loop then picks the round
// up through [Feedback.PlanFiring].
//
// [DeliveryResumeHandler] is the implementation every assembly uses; it
// exists because the deliverer is built after the run.
type ResumeHandler interface {
	// RedeliverPR re-runs the (idempotent) pull request delivery for a bead
	// whose PR already exists, so its gh:pr gate is recreated (D-20).
	RedeliverPR(ctx context.Context, e gitx.ResumeEntry) error
	// Redeliver re-runs the delivery of a bead the crash left in
	// delivering with no pull request and no conflict to point at (P9-03).
	Redeliver(ctx context.Context, e gitx.ResumeEntry) error
	// ConflictRound sends a bead whose worktree was left mid-rebase back to
	// its engineer as a conflict round (D-10).
	ConflictRound(ctx context.Context, e gitx.ResumeEntry) error
}

// ResumeDeliveryQueue is the queue a run routes its resume hand-offs
// through (P6-55). [*Conductor] implements it, and gives itself to the run
// when it is built, so an assembled run's resume-time deliveries are
// serialised against its ordinary ones and retried like them; a run built
// without one calls its [ResumeHandler] directly, which is what every
// resume did before P6-55.
type ResumeDeliveryQueue interface {
	// QueueResume runs one resume hand-off through the delivery queue.
	// bead is what the hand-off is about and what names it in the log,
	// and it returns when the hand-off's first attempt has finished — a
	// retryable failure is retried behind the caller rather than holding
	// [Run.Start] open for the whole backoff.
	QueueResume(ctx context.Context, bead domain.Bead, what string, fn func(context.Context) error) error
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

	// pubMu orders the publishes themselves. The counter under mu is not
	// enough on its own: two publishers can be counted in one order and
	// reach the bus in the other, which is how a conductor pass still in
	// flight put a graph_changed line after run_finished in the run log
	// even though the count agreed (P9-06 review). Held for the whole of
	// [Run.Publish], including the bus write; nothing a publisher reaches
	// takes it back, because a subscriber reads its own channel.
	pubMu sync.Mutex

	mu       sync.Mutex
	st       State
	started  bool
	finished bool
	// sealed is set by the [events.RunFinished] publish and makes every
	// later publish a no-op, so the run log's last line really is the
	// terminator and state.json's LastSeq really is the whole log (P9-07).
	sealed bool
	// afterSeal counts the publishes the seal refused, so a caller can
	// report that a helper beside the run was still going when it
	// finished. It is a diagnosis rather than an error.
	afterSeal  int
	stopping   bool
	stopReason string
	resumeCh   chan struct{}
	cancelLoop context.CancelFunc
	lock       *lockFile
	resumeRes  *gitx.ResumeResult
	resumeH    ResumeHandler
	resumeQ    ResumeDeliveryQueue
	beadSrc    BeadSource
	vitals     func() RunVitals
	budget     func() BudgetState
	agentBus   *events.Bus
	agentDone  chan struct{}
	// stateDirty and stateTimer coalesce the state.json writes the live
	// agent list asks for (P9-11): a dispatch that fires fifty agents
	// marks the state dirty fifty times and writes the file once.
	stateDirty bool
	stateTimer *time.Timer
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
		resumeH:   opts.OnResume,
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
			// P9-01: the spend comes with it, so `limits.max_tokens` and
			// `limits.max_cost_usd` cap the run rather than one invocation
			// of it. [RunBudgetOptions.Restore] is where it is read back.
			r.st.Budget = prev.Budget
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
//
// It writes only the run's own copy of [domain.Run] (State.Run, the one
// state.json is written from). Until P8-23 it also wrote the epic back
// through r.cfg — the very *config.RunConfig the command layer resolved and
// still holds — so every reader of rc.Run after [Run.Start] raced with the
// planner, and `sc run` had to snapshot domain.Run before starting to keep
// its renderers' headers off that race. Nothing needs the write: the epic
// is read back through [Run.Epic], [Run.State] and state.json, and
// [RestoreRunConfig] is what puts it into a *config.RunConfig, before New.
func (r *Run) SetEpic(id domain.BeadID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.st.Run.Epic == id {
		return nil
	}
	r.st.Run.Epic = id
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
//
// [events.RunFinished] is the terminator: publishing it seals the run, and
// every publish after it is dropped. The whole method is serialised, so the
// order the events are counted in is the order they reach the bus and the
// run log, and nothing can slip past the terminator on its way out.
func (r *Run) Publish(ev events.Event) {
	if ev == nil {
		return
	}
	r.pubMu.Lock()
	defer r.pubMu.Unlock()
	r.mu.Lock()
	// Nothing may follow the terminator. A pass that was still in flight
	// when the run finished — a conductor sync publishing
	// [events.GraphChanged], say — would otherwise land after
	// [events.RunFinished] and leave the run log one line longer than the
	// frozen state.json, which every reader of a finished run (`sc status`,
	// `sc logs`, resume) takes as the invariant it is not (P9-07).
	if r.sealed {
		r.afterSeal++
		r.mu.Unlock()
		return
	}
	if _, terminal := ev.(events.RunFinished); terminal {
		r.sealed = true
	}
	r.st.LastSeq++
	r.st.LastEventKind = ev.Kind()
	r.st.LastEventAt = ev.At()
	r.mu.Unlock()
	if r.bus != nil {
		r.bus.Publish(ev)
	}
}

// PublishedAfterFinish reports how many events were published after
// [events.RunFinished] sealed the stream and were therefore dropped. A
// non-zero count means something beside the run — a conductor pass, a gate
// poll — was still in flight when the run ended; the run log and state.json
// still agree, which is the point of the seal.
func (r *Run) PublishedAfterFinish() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.afterSeal
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
// The forwarding is asynchronous — a subscription is the only way to
// observe a bus — and P6-46 is what that used to cost: the one forwarder
// republished through [Run.Publish], which writes the run log, so a
// producer faster than the disk filled the subscription's
// [DefaultAgentBusBuffer] events and everything past that was dropped. It
// could never desynchronise the counter from the log (a dropped event
// reaches neither), but it lost agent output, which is the only record of
// what a model did.
//
// The relay is two goroutines rather than one, so the buffer is no longer
// the bound. [Run.relayAgentBus] does nothing but move events off the
// subscription into an unbounded queue — an append under a mutex, cheaper
// per event than the publish that produced it — and [Run.forwardAgentBus]
// drains that queue through [Run.Publish] at whatever rate the run log
// manages. A burst is absorbed by memory instead of being thrown away, and
// the queue is bounded in practice by the finish draining it
// ([Run.stopAgentBus]). The drop counter is still read and still warned
// about, because a relay that is descheduled for long enough could in
// principle miss one and a silent loss is the thing being fixed.
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
	q := newEventQueue()
	done := make(chan struct{})
	r.agentBus, r.agentDone = bus, done
	go relayAgentBus(sub, q)
	go r.forwardAgentBus(sub, q, done)
	return bus
}

// relayAgentBus moves everything the forwarding bus receives into q as fast
// as the subscription yields it, and closes q when the bus is closed. It
// does no work of its own on purpose: it is the goroutine that must keep up
// with the producer (P6-46).
func relayAgentBus(sub *events.Subscription, q *eventQueue) {
	defer q.close()
	for ev := range sub.Events() {
		q.push(ev)
	}
}

// forwardAgentBus republishes everything the relay queued, until the queue
// is closed and drained.
func (r *Run) forwardAgentBus(sub *events.Subscription, q *eventQueue, done chan struct{}) {
	defer close(done)
	for {
		ev, ok := q.pop()
		if !ok {
			break
		}
		r.Publish(ev)
	}
	if n := sub.Dropped(); n > 0 {
		r.Warn(SourceRun, fmt.Sprintf(
			"%d agent event(s) were dropped: the run log could not keep up with the agents' output", n), nil)
	}
}

// eventQueue is the unbounded hand-off between the two halves of the agent
// bus relay (P6-46): the relay pushes, the forwarder pops, and a closed
// queue still yields everything already in it before pop reports that it is
// finished.
type eventQueue struct {
	mu     sync.Mutex
	ready  *sync.Cond
	items  []events.Event
	closed bool
}

// newEventQueue returns an empty open queue.
func newEventQueue() *eventQueue {
	q := &eventQueue{}
	q.ready = sync.NewCond(&q.mu)
	return q
}

// push appends one event. A push after close is dropped, which cannot
// happen: only the relay pushes and only the relay closes.
func (q *eventQueue) push(ev events.Event) {
	q.mu.Lock()
	if !q.closed {
		q.items = append(q.items, ev)
		q.ready.Signal()
	}
	q.mu.Unlock()
}

// pop returns the oldest event, waiting for one. It reports false only when
// the queue is closed and empty.
func (q *eventQueue) pop() (events.Event, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.ready.Wait()
	}
	if len(q.items) == 0 {
		return nil, false
	}
	ev := q.items[0]
	q.items[0] = nil
	q.items = q.items[1:]
	return ev, true
}

// close ends the queue once everything in it has been popped.
func (q *eventQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.ready.Broadcast()
	q.mu.Unlock()
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
//
// The live bus goes behind a [StartupReplay] (P9-28), because the assembly
// this helper exists for starts the run and only then renders it: without
// the replay every event of the run's first milliseconds — the resume
// notices, and P9-01's "resumed over its budget" banner among them —
// reached the run log and never the screen. The log itself is not wrapped:
// it is a file, and a file is always listening.
func TeeRunLog(opts *Options, runDir string, bus Publisher) (*EventLog, error) {
	if opts == nil {
		return nil, errors.New("orchestrator: TeeRunLog: nil options")
	}
	log, err := OpenEventLog(runDir, opts.Logger)
	if err != nil {
		return nil, err
	}
	opts.Bus = Tee(log, NewStartupReplay(bus, 0))
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
//
// The write is coalesced rather than synchronous (P9-11). A cold tick fills
// every slot in one dispatch, and writing the whole state file once per
// agent — marshalling a list one agent longer each time — made the file
// two thirds of a fifty-agent dispatch and grew quadratically with the
// ammo. state.json is a crash-recovery record, not a transaction log, so
// the run marks itself dirty here and flushes once, at most
// [StateFlushDelay] later; every lifecycle transition (and
// [Run.FlushState]) still writes synchronously, so the file a resume reads
// is never more than that behind the last agent to be spawned. The error
// is therefore always nil, and a write that fails is logged by
// [Run.persistLocked] as before.
func (r *Run) AgentStarted(a domain.Agent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.st.Agents {
		if existing.ID == a.ID {
			r.st.Agents[i] = a
			r.markStateDirtyLocked()
			return nil
		}
	}
	r.st.Agents = append(r.st.Agents, a)
	r.markStateDirtyLocked()
	return nil
}

// AgentUpdated replaces a recorded agent (a state or pid change). An
// unknown agent is added, so a caller need not track which it is.
func (r *Run) AgentUpdated(a domain.Agent) error { return r.AgentStarted(a) }

// AgentStopped removes an agent from state.json. Unknown ids are ignored.
// Like [Run.AgentStarted] the write is coalesced (P9-11).
func (r *Run) AgentStopped(id domain.AgentID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, a := range r.st.Agents {
		if a.ID != id {
			continue
		}
		r.st.Agents = append(r.st.Agents[:i:i], r.st.Agents[i+1:]...)
		r.markStateDirtyLocked()
		return nil
	}
	return nil
}

// FlushState writes state.json now if anything the run coalesced is still
// only in memory (P9-11). The lifecycle flushes on every transition; this
// is for a caller — a test, or anything about to read the file behind the
// run's back — that wants the file to be current this instant.
func (r *Run) FlushState() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushStateLocked()
}

// markStateDirtyLocked records that state.json is behind r.st and arms the
// flush that will catch it up. The caller holds r.mu.
func (r *Run) markStateDirtyLocked() {
	r.stateDirty = true
	if r.stateTimer != nil || r.finished {
		return
	}
	r.stateTimer = time.AfterFunc(StateFlushDelay, func() { _ = r.FlushState() })
}

// flushStateLocked writes the coalesced state, if there is any. The caller
// holds r.mu.
func (r *Run) flushStateLocked() error {
	if !r.stateDirty {
		r.disarmFlushLocked()
		return nil
	}
	return r.persistLocked()
}

// disarmFlushLocked cancels a pending flush and marks the state clean. The
// caller holds r.mu.
func (r *Run) disarmFlushLocked() {
	r.stateDirty = false
	if r.stateTimer != nil {
		r.stateTimer.Stop()
		r.stateTimer = nil
	}
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

// ResumeHandler returns what the run acts on a resume with
// ([Options.OnResume], or whatever [Run.UseResumeHandler] gave it), or nil
// when it was given none — in which case [Run.reconcileResume] reconciles
// the worktrees and stops there, leaving a bead whose pull request is open
// without its gh:pr gate and a mid-rebase worktree without its engineer.
// Whoever assembles a run is meant to pass one, and this is how a test pins
// that they did (P9-03).
func (r *Run) ResumeHandler() ResumeHandler {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resumeH
}

// UseResumeHandler gives a run built without one the handler it acts on a
// resume with (P6-54). [Conductor.Attach] calls it with the delivery
// handoff it was just given, so a run assembled through the conductor
// reconciles a resumed pull request and a mid-rebase worktree whether or
// not the assembler remembered [Options.OnResume] — which for a whole
// phase nobody did, and nothing failed (P9-03).
//
// An explicit [Options.OnResume] wins: this only fills a gap, so an
// assembly that passes its own handler keeps it, and a caller can tell
// which it got from [Run.ResumeHandler]. Like [Run.UseBeads] it must be
// called before [Run.Start], because the resume is acted on inside Start; a
// started run returns [ErrAlreadyStarted], a finished one [ErrFinished].
func (r *Run) UseResumeHandler(h ResumeHandler) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.finished:
		return ErrFinished
	case r.started:
		return ErrAlreadyStarted
	case r.resumeH != nil:
		return nil
	}
	r.resumeH = h
	return nil
}

// UseResumeQueue names the queue the run's resume hand-offs run through
// (P6-55). Without one they are called on the delivery handoff directly,
// so two beads reconciled at resume time could rebase and fast-forward the
// same default branch at the same time as each other and as the
// conductor's own deliveries, and a retryable failure there was stranded.
// [NewConductor] passes the conductor, which is the queue P8-14 already
// serialises everything else through.
//
// Like [Run.UseResumeHandler] it must be called before [Run.Start].
func (r *Run) UseResumeQueue(q ResumeDeliveryQueue) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.finished:
		return ErrFinished
	case r.started:
		return ErrAlreadyStarted
	}
	r.resumeQ = q
	return nil
}

// ResumeQueue returns the queue the run's resume hand-offs run through, or
// nil when it has none.
func (r *Run) ResumeQueue() ResumeDeliveryQueue {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resumeQ
}

// resumeDelivery runs one resume hand-off, through the run's delivery queue
// when it has one (P6-55) and directly when it has not.
func (r *Run) resumeDelivery(ctx context.Context, b domain.Bead, what string, fn func(context.Context) error) error {
	if q := r.ResumeQueue(); q != nil {
		return q.QueueResume(ctx, b, what, fn)
	}
	return fn(ctx)
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
	handler := r.ResumeHandler()
	if handler == nil {
		return nil
	}
	for _, e := range res.Entries {
		switch {
		case e.Conflict:
			// A worktree left mid-rebase cannot be delivered; the engineer
			// resolves it first (D-10), even when a pull request exists.
			if err := r.resumeDelivery(ctx, e.Bead, "the conflict round", func(ctx context.Context) error {
				return handler.ConflictRound(ctx, e)
			}); err != nil {
				r.Warn(SourceRun, fmt.Sprintf("resume: %s: could not schedule the conflict round", e.Bead.ID), err)
			}
		case e.PRState != "":
			if err := r.resumeDelivery(ctx, e.Bead, "the pull request re-delivery", func(ctx context.Context) error {
				return handler.RedeliverPR(ctx, e)
			}); err != nil {
				r.Warn(SourceRun, fmt.Sprintf("resume: %s: could not re-run the pull request delivery", e.Bead.ID), err)
			}
		case e.State == domain.BeadStateDelivering && e.Err == nil:
			// P9-03: the crash landed inside a delivery, before there was
			// anything to point at — the delivering status is written
			// before any git work, so the window covers the rebase, the
			// repository's check command and the push (safety off) or the
			// `gh pr create` (safety on). gitx reconciles that to "the
			// delivery did not complete; deliver again", and nothing else
			// in the run would: the conductor ships only approved beads,
			// the gate poller watches only gh:pr gates, and delivering is
			// neither terminal nor settled, so the bead — and every
			// dependent of it (D-20) — would hang for as long as the run
			// lives. An entry gitx could not inspect is left alone: its
			// branch state is a guess, and delivering from a guess is how
			// work gets lost.
			if err := r.resumeDelivery(ctx, e.Bead, "the interrupted delivery", func(ctx context.Context) error {
				return handler.Redeliver(ctx, e)
			}); err != nil {
				r.Warn(SourceRun, fmt.Sprintf("resume: %s: could not finish the interrupted delivery", e.Bead.ID), err)
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

// UseBudget names the function that snapshots the run's spend for every
// state.json the run writes (P9-01), which is how a resumed run picks its
// budget up where the last invocation left it rather than counting from
// zero. Pass [RunBudget.Snapshot]; a nil function leaves State.Budget as
// whatever the run was resumed with.
//
// Like [Run.UseVitals] it is called on the run's own goroutine while the
// run holds its lock, so it must not block or call back into the run.
// [RunBudget.Snapshot] does neither.
func (r *Run) UseBudget(f func() BudgetState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.budget = f
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
	// P9-11: whatever the coalesced write still owes the file is owed now;
	// nothing will arm another flush.
	_ = r.flushStateLocked()
	r.releaseLockLocked()
	return nil
}

// persistLocked stamps and writes state.json. The caller holds r.mu. It is
// the synchronous write: every coalesced one (P9-11) ends here.
func (r *Run) persistLocked() error {
	r.disarmFlushLocked()
	r.st.Version = StateFileVersion
	r.st.UpdatedAt = r.now()
	if r.vitals != nil {
		v := r.vitals()
		r.st.Vitals = &v
	}
	if r.budget != nil {
		b := r.budget()
		r.st.Budget = &b
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

// processAlive reports whether a process with that id exists on this
// machine. It is P6-01's signal-0 probe, read carefully (P9-04): kill(2)
// answers EPERM for a process that *exists* but belongs to another user —
// `sudo sc /repo` in one terminal and `sc /repo` in another — and reading
// that as "the holder is gone" would reap a live run's lock, which is the
// one direction that must never happen. Only "no such process" counts as
// gone; an answer this probe cannot interpret is treated as alive, because
// a lock can always be taken with --force, while two runs firing at one
// repository cannot be undone.
//
// It still cannot say the process is the *same* one that took the lock —
// that is what [ProcessStart] adds.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return aliveFromSignal(p.Signal(syscall.Signal(0)))
}

// aliveFromSignal reads the answer kill(pid, 0) gave. It is separate from
// [processAlive] so the EPERM case can be tested without a second user.
func aliveFromSignal(err error) bool {
	switch {
	case err == nil:
		return true // the signal was accepted: the process is there.
	case errors.Is(err, syscall.ESRCH), errors.Is(err, os.ErrProcessDone):
		return false // no such process, or one this sc already waited for.
	case errors.Is(err, syscall.EPERM):
		return true // it exists; it simply belongs to somebody else.
	default:
		return true // an answer we cannot read is not proof it is gone.
	}
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
	// P9-04: the process start stamp goes into the record the lock is
	// published with, so pid and incarnation become visible together in the
	// one atomic step that takes the lock. [StampRunLock] then has nothing
	// to do: no reader ever meets a lock that names a pid without saying
	// which incarnation of it, and nothing has to read the lock back and
	// write it again — a read-modify-write a concurrent `--force` takeover
	// could have clobbered.
	opts := lockOptionsOf(policy)
	data, err := json.MarshalIndent(LockRecord{LockHolder: holder, Started: opts.startOf(holder.PID)}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("orchestrator: encode the run lock: %w", err)
	}
	data = append(data, '\n')

	for attempt := 0; attempt < 2; attempt++ {
		err := publishLock(path, data)
		if err == nil {
			return &lockFile{path: path, holder: holder}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("orchestrator: take the run lock: %w", err)
		}
		existing, readErr := readLock(path)
		if cause := lockUnusable(existing, readErr); cause != nil {
			// A lock this sc cannot read is not automatically rubbish: it
			// may be the half-written file of a run starting this very
			// instant. The policy decides, with a grace period (P9-04);
			// the read failure alone never does.
			if !unreadableLockStale(policy, path, cause) {
				return nil, &LockedError{Path: path, State: LockCorrupt, Age: lockFileAge(path, LockOptions{}), cause: cause}
			}
		} else if !policy.Stale(existing) {
			// The same *LockedError the preflight raises, so losing the
			// race here reads exactly like losing it there (P9-04).
			return nil, lockedBy(path, existing, policy)
		}
		// Stale, or debris the policy agreed to clear: take it over.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("orchestrator: drop the stale run lock: %w", err)
		}
	}
	return nil, fmt.Errorf("%w: %s keeps being retaken", ErrLocked, path)
}

// lockUnusable says why an existing lock cannot be reasoned about: it could
// not be read or parsed (readErr), or it names no process at all. A usable
// lock returns nil, and is then the [LockPolicy]'s business.
func lockUnusable(h LockHolder, readErr error) error {
	if readErr != nil {
		return readErr
	}
	if h.PID <= 0 {
		return fmt.Errorf("the run lock names no process (pid %d)", h.PID)
	}
	return nil
}

// publishLock writes the lock record so that it becomes visible *complete*:
// the bytes go to a temporary file in the same directory and that file is
// hard-linked into place, which fails when somebody already holds the lock
// (os.ErrExist) and otherwise publishes the whole record in one step.
//
// The plain O_EXCL create P6-01 used left a window of exactly the wrong
// kind (P9-04): between the create and the write the lock is a zero-byte
// file, and a second sc arriving in that window read it as debris, deleted
// it and started — two runs at one repository, each holding a lock the
// other had unlinked. A file system with no hard links (rare, and never
// where a state directory lives) falls back to the old create-then-write.
func publishLock(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, LockFileName+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, runFilePerm); err != nil {
		return err
	}
	if err := os.Link(name, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		return publishLockExclusive(path, data)
	}
	return nil
}

// publishLockExclusive is publishLock's fallback for a file system that
// cannot hard-link: O_EXCL, then the write.
func publishLockExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, runFilePerm)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(path)
		return fmt.Errorf("orchestrator: write the run lock: %w", errors.Join(werr, cerr))
	}
	return nil
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

// release removes the lock file if it is still ours. A lock that names
// another run, and one that cannot be read at all, is left where it is: an
// unreadable file cannot be proved to be ours, and it may be another sc's
// lock caught mid-write (P9-04). Real debris is cleared by the next run,
// once it is older than the grace period.
func (l *lockFile) release() error {
	existing, err := readLock(l.path)
	if err != nil {
		return nil
	}
	if existing.Run != l.holder.Run || existing.PID != l.holder.PID {
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
	// Filter is the run's secret filter (P9-05). It is the last thing
	// between the repository and a prompt: the change set
	// [Conductor.BeadContext] hands the feedback loop goes through
	// [agents.SecretFilter.GitDiff], so a change to ".env", a key or any
	// configured `exclude` path arrives as [agents.SecretPlaceholder]
	// rather than as its own lines. A nil filter is not "no filtering" —
	// it is the built-in patterns alone — so the seam can never be off by
	// omission; pass the run's filter to add the configured `exclude`
	// patterns to it.
	Filter *agents.SecretFilter

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
	// DeliveryAttempts bounds how many times one bead's delivery is tried
	// before it is handed to a person; zero means
	// [DefaultDeliveryAttempts]. DeliveryBackoff is the wait before the
	// second attempt, doubled before each one after it and capped at
	// [MaxDeliveryBackoff]; zero means [DefaultDeliveryBackoff]. Both are
	// wall-clock: they pace subprocesses, not the run's behaviour.
	DeliveryAttempts int
	DeliveryBackoff  time.Duration
	// Restore says whether the first pass rebuilds the feedback threads
	// from the beads' comments (P8-16). The zero value follows the run:
	// a resumed run restores, a fresh one does not.
	Restore *bool
	// States applies the D-07 transitions the glue itself owes, both of
	// which park a bead in needs_human because no retry can help:
	// [Conductor.giveUpOnDelivery]'s, and P9-06's — a bead whose replayed
	// thread the rules gate refused to conclude, where the thread the
	// failed replay left behind would otherwise make every later pass skip
	// the bead in silence. Nil leaves such a bead where bd holds it and
	// reports why, so a test that does not care may omit it.
	States *StateMachine
	// RulesGate is the run's rules compliance gate (P9-06). A resume replays
	// bd's comments without re-running any role check, so the gate is told
	// about a thread ([RulesRestorer.RestoreMessages]) before
	// [Feedback.Restore] applies it: without that the gate's memory is
	// empty on a resumed run and a violation an old comment settled the
	// wrong way comes back settled. Nil restores without a gate, which only
	// a test wants.
	RulesGate RulesRestorer
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
	// published (P6-32). Updates counts the events.BeadUpdated published
	// instead of a snapshot for a pass in which the graph's shape did not
	// move (P6-45).
	Announced int
	Graphs    int
	Updates   int
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

	// attempts is how many times one bead's delivery is tried before it is
	// given to a person, and backoff the first wait between two of them.
	attempts int
	backoff  time.Duration

	// deliveries counts the delivery worker; a delivery is not part of a
	// pass (P8-14 gap 5), so the pass does not wait for it.
	deliveries sync.WaitGroup
	// deliver serialises the deliveries themselves: whoever asks for one,
	// only one repository-mutating delivery runs at a time (P8-14 gap 3).
	deliver sync.Mutex

	mu       sync.Mutex
	idle     *sync.Cond
	feedback ConductorFeedback
	delivery ConductorDelivery
	gates    GateKicker
	inflight map[domain.BeadID]deliveryJob
	queue    []deliveryJob
	pending  int
	wake     chan struct{}
	worker   bool
	shipped  map[domain.BeadID]bool
	owed     map[domain.BeadID]bool
	frozen   bool
	seen     map[domain.BeadID]bool
	snapshot []beads.Issue
	// graph is the work beads as the run log last saw them, which is what
	// the next pass's snapshot is diffed against (P6-45).
	graph   map[domain.BeadID]domain.Bead
	warned  string
	stats   ConductorStats
	dctx    context.Context
	dcancel context.CancelFunc
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
		attempts: opts.DeliveryAttempts,
		backoff:  opts.DeliveryBackoff,
		inflight: map[domain.BeadID]deliveryJob{},
		wake:     make(chan struct{}, 1),
		shipped:  map[domain.BeadID]bool{},
		owed:     map[domain.BeadID]bool{},
		seen:     map[domain.BeadID]bool{},
		graph:    map[domain.BeadID]domain.Bead{},
	}
	c.idle = sync.NewCond(&c.mu)
	if c.interval <= 0 {
		c.interval = DefaultConductorInterval
	}
	if c.attempts <= 0 {
		c.attempts = DefaultDeliveryAttempts
	}
	if c.backoff <= 0 {
		c.backoff = DefaultDeliveryBackoff
	}
	if c.log == nil {
		c.log = opts.Run.Log()
	}
	if c.foreign == nil {
		c.foreign = NewForeignStatusReporter(ForeignStatusOptions{
			Host: opts.Run, Statuses: opts.Statuses, Logger: c.log,
		})
	}
	// P9-05: the tree summary a prompt renders is filtered once here, so an
	// assembler that forgets to filter its scan still cannot leak a path
	// the exclude patterns cover. The filter is idempotent, so the run's
	// own already-filtered scan passes through unchanged.
	c.opts.Repo = opts.Filter.RepoContext(opts.Repo)
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
	// P6-55: the run's resume hand-offs go through this conductor's
	// delivery queue, so they are serialised against its ordinary
	// deliveries and retried like them. A run that has already started is
	// past the only place a resume is acted on, so there is nothing to
	// wire.
	if err := c.run.UseResumeQueue(c); err != nil && !errors.Is(err, ErrAlreadyStarted) && !errors.Is(err, ErrFinished) {
		return nil, err
	}
	return c, nil
}

// *Conductor is the production queue a run's resume hand-offs run through.
var _ ResumeDeliveryQueue = (*Conductor)(nil)

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
	if p.Feedback != nil {
		c.feedback = p.Feedback
	}
	if p.Delivery != nil {
		c.delivery = p.Delivery
	}
	if p.Gates != nil {
		c.gates = p.Gates
	}
	delivery := c.delivery
	c.mu.Unlock()
	c.adoptResumeHandler(delivery)
}

// adoptResumeHandler gives the run a resume handler over the delivery
// handoff when nothing else has (P6-54).
//
// [Options.OnResume] was never filled in the shipped binary for a whole
// phase, so both P6-15 hand-offs were dead code and no test noticed:
// --resume reconciled the worktrees, announced the states it had written,
// and then left a bead whose pull request was open without its gh:pr gate
// (nothing polls a gate that does not exist, so its dependents never become
// ready, D-20) and a worktree mid-rebase without its engineer. P9-03 filled
// the call site in `sc run`; this makes it structural, because the
// conductor is handed both the run and the deliverer and is therefore the
// one place that can. An assembly that passes its own handler keeps it
// ([Run.UseResumeHandler] fills a gap rather than overwriting).
func (c *Conductor) adoptResumeHandler(delivery ConductorDelivery) {
	if delivery == nil || c.run.ResumeHandler() != nil {
		return
	}
	// *Deliverer has all three hand-offs, so the production delivery
	// handoff is already a ResumeHandler; a slice of it that is not says
	// this assembly has nothing to act on a resume with, which is the run's
	// own warning to raise, not a failure of Attach.
	h, ok := delivery.(ResumeHandler)
	if !ok {
		return
	}
	// The only errors are ErrAlreadyStarted and ErrFinished, and both mean
	// the resume is already behind this run, so there is nothing to wire.
	_ = c.run.UseResumeHandler(h)
}

// RulesWiring is P9-06's half of a conductor's options, as
// [Conductor.RulesWiring] reports it.
type RulesWiring struct {
	// Gate is [ConductorOptions.RulesGate]: what a resume tells about a
	// bead's comments before [Feedback.Restore] replays them.
	Gate RulesRestorer
	// States is [ConductorOptions.States]: what a bead whose replay the
	// gate refused is parked in needs_human with.
	States *StateMachine
}

// RulesWiring reports what the conductor was given for the rules
// compliance gate; either field is nil when the assembly wired none.
//
// It exists so an assembled run can be *asked* whether P9-06's resume half
// is in place. Both fields are otherwise read only on a resume, so dropping
// either from `sc run` broke nothing any test could see (P9-06 review,
// round 4). The options are set once, before the conductor runs, so no lock
// is needed.
func (c *Conductor) RulesWiring() RulesWiring {
	return RulesWiring{Gate: c.opts.RulesGate, States: c.opts.States}
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
// the agents' own calls. That pacing is about bd's latency: a delivery is
// not part of a pass at all ([Conductor.deliverApproved]), so a rebase, a
// check command and a `gh pr create` never hold the loop up.
//
// On the way out it cancels the deliveries still in flight and waits for
// them, bounded by [deliveryDrain], so nothing is left running behind a
// finished run.
func (c *Conductor) Run(ctx context.Context) error {
	defer c.stopDeliveries()
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
// of a resumed run first), and start the delivery of the approved ones.
// Every step is attempted even when an earlier one failed — a pass that
// cannot read the ready set can still ship an approved bead — and the
// failures come back joined.
//
// The deliveries run in the background ([Conductor.deliverApproved]), so
// the error a pass returns is the bookkeeping's, not theirs; a delivery
// that fails warns and is counted like a failed pass. [Conductor.Deliver]
// is the synchronous form, for a caller that wants the outcome.
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
		c.freezeRestorable(list)
		if err := c.restoreThreads(ctx, list); err != nil {
			errs = append(errs, err)
		}
	}
	if err := c.trackReady(ctx); err != nil {
		errs = append(errs, err)
	}
	c.deliverApproved(ctx, list)
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

// freezeRestorable takes the resume's restore set from the first pass's
// listing and never adds to it again (P8-14 gap 1). Restore is a *resume*
// step: it replays a bead's whole comment thread into a thread the loop is
// not holding yet. Anything that becomes restorable later became so because
// this run moved it — the scheduler claims an open bead the moment it is
// offered, which takes it to in_progress — and replaying an old thread over
// a live one abandons the engineer the run has just fired, double-counts
// the D-08 pushback exchanges and can settle the bead out from under the
// loop that is waiting on it.
//
// The set is the beads the run found already past open, whatever happens to
// them afterwards; a bead in it that turns out to have no thread to replay,
// or one the loop has already adopted, is dropped by [Conductor.restoreThreads].
func (c *Conductor) freezeRestorable(list []domain.Bead) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frozen {
		return
	}
	c.frozen = true
	for _, b := range list {
		if b.ID == "" || b.Type == domain.BeadTypeEpic || !conductorRestorable(b) {
			continue
		}
		c.owed[b.ID] = true
	}
}

// restoreOwed returns the beads of list whose threads the resume still owes,
// in the listing's order.
func (c *Conductor) restoreOwed(list []domain.Bead) []domain.Bead {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.owed) == 0 {
		return nil
	}
	out := make([]domain.Bead, 0, len(c.owed))
	for _, b := range list {
		if c.owed[b.ID] {
			out = append(out, b)
		}
	}
	return out
}

// restoreSettled marks a bead's restore finished — replayed, adopted
// instead, or already live — so no later pass touches its thread again.
func (c *Conductor) restoreSettled(id domain.BeadID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.owed, id)
}

// restoreThreads rebuilds the feedback threads of a resumed run from the
// beads' own comments (P8-16), once per bead. Without it a resumed run
// re-fires a changes_requested bead in implement mode and throws its items,
// responses, holds and rulings away.
//
// The candidates are frozen at the first pass ([Conductor.freezeRestorable])
// and only a bead that is past the open state and actually carries
// structured comments is restored: [Feedback.Restore] concludes the thread
// it replays, and a bead with nothing to replay would be concluded out of a
// state it never left. Everything else is adopted the ordinary way.
//
// A bead the loop already holds a thread for is left alone — the resume
// lost that race, and the live thread is the true one — and a bead whose
// comments or replay failed stays owed, so a transient bd or worktree
// failure is retried on the next pass instead of stranding the bead for the
// rest of the run (P8-14 gap 2).
func (c *Conductor) restoreThreads(ctx context.Context, list []domain.Bead) error {
	feedback, _, _ := c.parts()
	if feedback == nil {
		return nil
	}
	var errs []error
	for _, b := range c.restoreOwed(list) {
		if _, live := feedback.Thread(b.ID); live {
			c.restoreSettled(b.ID)
			continue
		}
		msgs, err := c.beads.Messages(ctx, b.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("orchestrator: conductor: reading %s's comments: %w", b.ID, err))
			continue
		}
		if len(msgs) == 0 {
			c.restoreSettled(b.ID)
			feedback.Track(b)
			continue
		}
		// P9-06: the gate reads the thread before the loop replays it, so a
		// violation these comments settled the wrong way is open again by
		// the time the replay tries to ship the bead through the guarded
		// state store.
		if c.opts.RulesGate != nil {
			c.opts.RulesGate.RestoreMessages(b.ID, msgs)
		}
		if err := feedback.Restore(ctx, b, msgs); err != nil {
			if errors.Is(err, ErrRulesBlocker) {
				errs = append(errs, c.rulesBlockedRestore(ctx, b, feedback, err))
				continue
			}
			errs = append(errs, fmt.Errorf("orchestrator: conductor: restoring %s's thread: %w", b.ID, err))
			continue
		}
		c.restoreSettled(b.ID)
		c.mu.Lock()
		c.stats.Restored++
		c.mu.Unlock()
		c.log.Info("feedback thread restored from the bead's comments",
			slog.String("bead", b.ID.String()), slog.String("state", string(b.State)),
			slog.Int("messages", len(msgs)))
		if err := c.reopenRestored(ctx, b, feedback); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// rulesBlockedRestore is what a resume does with a bead whose replayed
// thread the rules compliance gate refused to conclude (P9-06).
//
// It cannot be retried and it must not be dropped. The refusal is the gate
// reading the bead's own comments and finding a violation of a rules file
// that only code can settle, so every later pass would be refused
// identically; and [Feedback.Restore] registers the thread before it tries
// the transition, so the "a live thread is the true one" branch above would
// quietly stop retrying anyway and leave bd holding the bead in in_review
// with the loop believing it settled, no agent ever offered and nobody
// told (P9-06 review, round 4).
//
// So the bead is handed to a person instead: needs_human is a state
// [BeadEventBlocked] reaches from anywhere non-terminal, it is not a
// shipping state so the gate's own guarded store allows it, and the loop is
// told where the bead went so [Feedback.Outstanding] stops waiting on it.
// The refusal is still returned, so the pass reports it and `sc status`
// keeps the gate's own warning beside it.
func (c *Conductor) rulesBlockedRestore(ctx context.Context, b domain.Bead, feedback ConductorFeedback, cause error) error {
	// Nothing here is retried, so the bead is settled first: a later pass
	// must not walk into the live-thread branch and pretend all is well.
	c.restoreSettled(b.ID)
	c.log.Warn("the rules compliance gate refused a resumed thread; the bead needs a person",
		slog.String("bead", b.ID.String()), slog.String("state", string(b.State)),
		slog.String("error", cause.Error()))
	if c.opts.States == nil {
		return fmt.Errorf("orchestrator: conductor: restoring %s's thread: %w", b.ID, cause)
	}
	_, err := c.opts.States.Apply(ctx, StateRequest{
		Bead: b.ID, From: b.State, Event: BeadEventBlocked,
		Reason: "a resumed thread settled a rules-file violation the gate holds open",
	})
	if err != nil {
		c.rulesWarn(b.ID, "a bead the rules gate refused could not be parked in needs_human; it is where bd left it", err)
		return errors.Join(fmt.Errorf("orchestrator: conductor: restoring %s's thread: %w", b.ID, cause),
			fmt.Errorf("orchestrator: conductor: parking %s for a person: %w", b.ID, err))
	}
	feedback.Observe(b.ID, domain.BeadStateNeedsHuman)
	c.rulesWarn(b.ID, "a resumed bead was parked in needs_human: its thread settles a rules-file violation the gate holds open", cause)
	return fmt.Errorf("orchestrator: conductor: restoring %s's thread: %w", b.ID, cause)
}

// rulesWarn publishes one of [Conductor.rulesBlockedRestore]'s warnings. It
// is not [Run.Warn] because that one carries no bead, and the whole point
// of these two is to name the bead a person now owns.
func (c *Conductor) rulesWarn(bead domain.BeadID, message string, err error) {
	w := events.Warning{Meta: c.run.Meta(), Source: SourceRules, Message: message, Bead: bead}
	if err != nil {
		w.Err = err.Error()
	}
	c.run.Publish(w)
}

// conductorRestorable reports whether a resumed bead may have a thread
// worth rebuilding: every workflow state a run can be killed in.
//
// open is in the list, and it is the state most of them are in (P8-14 gap
// 2). A bead the killed run had claimed does not come back in_progress: a
// graceful stop takes the D-07 released edge (P6-03) and `--resume` runs
// [beads.Client.RecoverStale] before the run starts, which releases every
// stale in_progress bead — both of which leave it open with the previous
// run's whole thread still on it. Leaving open out meant the commonest
// resume of all threw away the items, the responses, the D-08 pushback
// counts and the rulings of every bead an engineer was working on, and
// started it again at round one.
//
// Nothing is restored on the strength of this alone: only the first pass's
// listing is a candidate ([Conductor.freezeRestorable]), only a bead that
// really carries structured comments is replayed, and a bead the loop has
// already adopted is left alone ([Conductor.restoreThreads]). The cost of
// the wider net is one `bd show <id> --comments` per open bead, once, on
// the first pass of a resumed run.
func conductorRestorable(b domain.Bead) bool {
	switch b.State {
	case domain.BeadStateOpen, domain.BeadStateInProgress, domain.BeadStateInReview,
		domain.BeadStateChangesRequested, domain.BeadStateEscalated,
		domain.BeadStateApproved, domain.BeadStateDelivering:
		return true
	default:
		return false
	}
}

// reopenRestoredReason is what the testers are told when a resumed bead's
// approvals are taken again.
const reopenRestoredReason = "this bead's previous run was killed while its engineer was still working, " +
	"so the approvals on its comments are for code that has since moved"

// reopenRestored keeps a restored open bead's thread and the state bd holds
// it in from contradicting each other (P8-14 gap 2).
//
// A bead the resume found open was in_progress when the run died, so the
// branch is whatever its engineer had reached. The replay, though, is of
// comments alone: with no change set to compare against, [Feedback.plan]
// decides who still owes a verdict from the round numbers, and a thread
// whose last round was fully approved comes back settled — over a bead bd
// holds open, which no demand will ever be made for and which
// [BeadStateSettled] calls unsettled, so the run would idle for ever.
//
// The loop's own phases say what bd should be holding: an engineer's phase
// needs nothing (the demand's claim moves the bead to in_progress and
// [Feedback.planEngineer] syncs the thread), a settled thread means the
// approvals are stale and the testers are sent back over the code that is
// really there, and every other phase means the bead is being judged, which
// is in_review — or escalated, while the product owner rules. Telling the
// loop that is what makes its next transition one the D-07 table allows;
// bd catches up with the write that transition performs.
func (c *Conductor) reopenRestored(ctx context.Context, b domain.Bead, feedback ConductorFeedback) error {
	if b.State != domain.BeadStateOpen {
		return nil
	}
	th, ok := feedback.Thread(b.ID)
	if !ok {
		return nil
	}
	switch th.Phase {
	case FeedbackImplementing, FeedbackAddressing:
		return nil
	case FeedbackSettled:
		c.log.Info("a resumed bead's approvals are for code its killed engineer was still changing; reviewing it again",
			slog.String("bead", b.ID.String()), slog.Int("round", th.Round))
		if err := feedback.ReviewAgain(ctx, b, th.Round, reopenRestoredReason); err != nil {
			return fmt.Errorf("orchestrator: conductor: reviewing %s again after its resume: %w", b.ID, err)
		}
	case FeedbackEscalating:
		feedback.Observe(b.ID, domain.BeadStateEscalated)
	default:
		feedback.Observe(b.ID, domain.BeadStateInReview)
	}
	return nil
}

// deliveryDrain bounds how long the conductor waits for the deliveries it
// cancelled on the way out. It is wall-clock rather than the run's clock on
// purpose: it bounds goroutines, not the run's behaviour.
const deliveryDrain = 30 * time.Second

// DefaultDeliveryAttempts is how many times one bead's delivery is tried
// before it is handed to a person. A retryable delivery failure is a `gh`
// that could not be reached, a remote that refused the push, a default
// branch that moved under the rebase, or a bd write that failed after the
// merge landed — all of which usually pass. Five attempts over
// [DefaultDeliveryBackoff]'s doubling waits is about a minute and a half of
// trying, which outlives the ordinary hiccup without hiding a real one.
const DefaultDeliveryAttempts = 5

// DefaultDeliveryBackoff is the wait before a delivery's second attempt. It
// doubles before each attempt after that, up to [MaxDeliveryBackoff].
const DefaultDeliveryBackoff = 5 * time.Second

// MaxDeliveryBackoff caps the doubling, so a long run's last retries are
// still minutes rather than hours apart.
const MaxDeliveryBackoff = 2 * time.Minute

// deliveryJob is one bead's delivery: the bead as the conductor last saw
// it, which attempt this is (1-based) and the earliest time it may run.
type deliveryJob struct {
	bead    domain.Bead
	attempt int
	at      time.Time
	// resume is set when the job is a resume hand-off routed through the
	// queue (P6-55) rather than an ordinary delivery of an approved bead.
	resume *resumeDelivery
}

// resumeDelivery is one resume hand-off waiting its turn in the delivery
// queue (P6-55): what it is, how to do it, and the answer the resume that
// asked for it is waiting on.
type resumeDelivery struct {
	what string
	fn   func(context.Context) error
	once sync.Once
	// first carries the first attempt's outcome. The resume waits on it
	// rather than on the whole job, so a failure that will be retried does
	// not hold Run.Start open for the backoff.
	first chan error
}

// answer hands the first attempt's outcome to whoever is waiting, once.
func (rd *resumeDelivery) answer(err error) {
	rd.once.Do(func() { rd.first <- err })
}

// deliverApproved queues the delivery of every bead the loop approved, at
// most one at a time per bead, and does not wait for it: a delivery is a
// rebase, the repository's check command, a push and a `gh pr create` —
// minutes in a real repository — and holding the pass open for it would
// stop `bd ready` reaching [Feedback.Track], so every engineer would idle
// until the delivery returned (P8-14 gap 5). The pass keeps offering work
// while the delivery runs; [Conductor.claim] is what stops the next pass
// shipping the same bead twice.
//
// The queue is drained by one worker, so two beads approved in the same
// pass are delivered one after the other rather than at once (P8-14 gap 3).
// They would otherwise race over the same repository: with safety off each
// delivery fetches, rebases, fast-forwards the default branch and pushes
// through the same [gitx.Manager], and the second of two concurrent
// deliveries finds the branch it rebased onto already moved and fails with
// [gitx.ErrNotFastForward] — every time, not sometimes.
//
// Seeing a bead anywhere but approved also clears the mark a finished
// delivery left ([Conductor.shipped]), so a bead that comes back to
// approved — a person resolves a needs_human hand-off, a gate escalation is
// undone — is delivered again rather than ignored for the rest of the run
// (P8-14 gap 6).
func (c *Conductor) deliverApproved(ctx context.Context, list []domain.Bead) {
	_, delivery, _ := c.parts()
	if delivery == nil {
		return
	}
	for _, b := range list {
		if b.ID == "" || b.Type == domain.BeadTypeEpic {
			continue
		}
		if b.State != domain.BeadStateApproved {
			c.leftApproved(b.ID)
			continue
		}
		if !c.claim(b) {
			continue
		}
		c.enqueue(ctx, deliveryJob{bead: b, attempt: 1, at: time.Now()})
	}
}

// enqueue puts one delivery in the queue and makes sure the worker is
// running. The bead is already claimed ([Conductor.claim]), so nothing else
// will queue it while it is there.
func (c *Conductor) enqueue(ctx context.Context, job deliveryJob) {
	dctx := c.deliveryContext(ctx)
	c.mu.Lock()
	c.queue = append(c.queue, job)
	c.pending++
	start := !c.worker
	c.worker = true
	c.mu.Unlock()
	if start {
		c.deliveries.Add(1)
		go func() {
			defer c.deliveries.Done()
			c.deliverQueued(dctx)
		}()
	}
	c.nudge()
}

// nudge wakes the worker without blocking: the channel holds one wake-up,
// which is all a worker that re-reads the whole queue needs.
func (c *Conductor) nudge() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// deliverQueued is the delivery worker: it runs the queued deliveries one
// at a time, oldest due first, until the context is cancelled. Serialising
// them is the point ([Conductor.deliverApproved]); the wait between a
// failed attempt and its retry is [Conductor.retryDelivery]'s.
//
// On the way out it drops whatever is still queued and wakes anything
// waiting in [Conductor.WaitDeliveries], so a cancelled run cannot leave a
// caller blocked on work that will never run.
func (c *Conductor) deliverQueued(ctx context.Context) {
	defer c.dropQueued()
	for {
		job, wait, ok := c.nextDelivery(time.Now())
		switch {
		case ok && wait <= 0:
			c.runDelivery(ctx, job)
			continue
		case ctx.Err() != nil:
			return
		}
		var (
			idle  <-chan time.Time
			timer *time.Timer
		)
		if ok {
			timer = time.NewTimer(wait)
			idle = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-c.wake:
		case <-idle:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// nextDelivery takes the job that is due first. It returns ok false when
// the queue is empty, and a positive wait when the head of the queue is a
// retry that is not due yet (the job is left in the queue then).
func (c *Conductor) nextDelivery(now time.Time) (deliveryJob, time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) == 0 {
		return deliveryJob{}, 0, false
	}
	first := 0
	for i, j := range c.queue {
		if j.at.Before(c.queue[first].at) {
			first = i
		}
	}
	job := c.queue[first]
	if wait := job.at.Sub(now); wait > 0 {
		return job, wait, true
	}
	c.queue = append(c.queue[:first], c.queue[first+1:]...)
	return job, 0, true
}

// runDelivery delivers one queued job and accounts for it. Whatever
// happened, the job is finished here: a retry is a new job the outcome
// queued ([Conductor.retryDelivery]) before this one was counted out, so
// the pending count never dips to zero between an attempt and its retry.
func (c *Conductor) runDelivery(ctx context.Context, job deliveryJob) {
	defer c.finishDelivery()
	if job.resume != nil {
		c.runResumeDelivery(ctx, job)
		return
	}
	if err := c.deliverJob(ctx, job); err != nil && ctx.Err() == nil {
		c.record(false, err)
		c.warn(err)
	}
}

// QueueResume runs one resume hand-off through the delivery queue (P6-55).
//
// [Deliverer.Redeliver] and [Deliverer.RedeliverPR] mutate the same
// repository an ordinary delivery does — a fetch, a rebase, a
// fast-forward and a push with safety off, a `gh pr create` with it on —
// so calling them straight off the deliverer, as the run's resume did until
// now, meant two beads reconciled at resume time could do that to the same
// default branch at once, and could do it while the conductor was already
// delivering a third. Routing them here puts them behind the same single
// worker and the same [Conductor.deliver] lock as everything else, and
// gives them the bounded retry a hand-off that failed on a `gh` that could
// not be reached has always needed.
//
// It returns when the first attempt has finished, with that attempt's
// error. A retryable failure is retried behind the caller — up to the
// conductor's attempt cap, on its backoff — because the resume is acted on
// inside [Run.Start] and holding a run's start open for a minute and a half
// of retries would be worse than the failure.
func (c *Conductor) QueueResume(ctx context.Context, bead domain.Bead, what string, fn func(context.Context) error) error {
	if fn == nil {
		return nil
	}
	if what == "" {
		what = "the resume hand-off"
	}
	rd := &resumeDelivery{what: what, fn: fn, first: make(chan error, 1)}
	c.enqueue(ctx, deliveryJob{bead: bead, attempt: 1, at: time.Now(), resume: rd})
	select {
	case err := <-rd.first:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runResumeDelivery runs one attempt of a queued resume hand-off under the
// delivery lock, answers the resume that is waiting for it, and queues the
// retry when there is one left to make.
func (c *Conductor) runResumeDelivery(ctx context.Context, job deliveryJob) {
	rd := job.resume
	c.deliver.Lock()
	err := rd.fn(ctx)
	c.deliver.Unlock()

	rd.answer(err)
	switch {
	case err == nil:
		if job.attempt > 1 {
			c.log.Info("a resume hand-off succeeded on a later attempt",
				slog.String("bead", job.bead.ID.String()),
				slog.String("what", rd.what), slog.Int("attempt", job.attempt))
		}
		return
	case ctx.Err() != nil:
		return
	case job.attempt >= c.attempts:
		c.record(false, fmt.Errorf("orchestrator: conductor: %s for %s: %w", rd.what, job.bead.ID, err))
		c.warn(fmt.Errorf("orchestrator: conductor: %s for %s: %w", rd.what, job.bead.ID, err))
		return
	}
	wait := deliveryBackoff(c.backoff, job.attempt)
	// Info, like [Conductor.retryDelivery]'s: the user hears about it when
	// the attempts run out, through the same warning every other delivery
	// failure raises.
	c.log.Info("the resume hand-off will be tried again",
		slog.String("bead", job.bead.ID.String()), slog.String("what", rd.what),
		slog.Int("attempt", job.attempt+1), slog.Int("of", c.attempts),
		slog.Duration("in", wait), slog.Any("err", err))
	// Queued before this attempt is counted out ([Conductor.runDelivery]
	// defers that), so the pending count never dips to zero between an
	// attempt and its retry.
	c.enqueue(ctx, deliveryJob{bead: job.bead, attempt: job.attempt + 1, at: time.Now().Add(wait), resume: rd})
}

// finishDelivery counts one delivery out and wakes [Conductor.WaitDeliveries]
// when nothing is left.
func (c *Conductor) finishDelivery() {
	c.mu.Lock()
	if c.pending > 0 {
		c.pending--
	}
	if c.pending == 0 {
		c.idle.Broadcast()
	}
	c.mu.Unlock()
}

// dropQueued abandons the queue when the worker stops, so a shutdown does
// not leave [Conductor.WaitDeliveries] waiting for work nothing will run.
func (c *Conductor) dropQueued() {
	c.mu.Lock()
	for _, job := range c.queue {
		delete(c.inflight, job.bead.ID)
		if job.resume != nil {
			// P6-55: a resume is waiting on this job's first attempt, and
			// the worker that would have made it is gone.
			job.resume.answer(fmt.Errorf(
				"orchestrator: conductor: the delivery queue stopped before %s for %s ran",
				job.resume.what, job.bead.ID))
		}
	}
	c.queue = nil
	c.pending = 0
	c.worker = false
	c.idle.Broadcast()
	c.mu.Unlock()
}

// deliveryContext is the context a background delivery runs under: the
// pass's values without its cancellation, cancelled when the run finishes,
// when [Conductor.Run] returns or by [Conductor.stopDeliveries]. A pass is
// over long before the delivery it started, so the pass's own cancellation
// must not reach it.
func (c *Conductor) deliveryContext(ctx context.Context) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dctx == nil {
		c.dctx, c.dcancel = context.WithCancel(context.WithoutCancel(ctx))
		dctx, cancel := c.dctx, c.dcancel
		go func() {
			select {
			case <-c.run.Done():
				cancel()
			case <-dctx.Done():
			}
		}()
	}
	return c.dctx
}

// WaitDeliveries blocks until the delivery queue is empty and nothing is in
// flight, retries included: a bead whose attempt failed is still owed one,
// and a caller that wants "the delivery is finished with this bead" wants
// to wait for that too. [Conductor.Run] waits on the way out, and a caller
// driving [Conductor.Sync] by hand — a test — waits when it wants the
// outcome of the pass it just made.
//
// A cancelled worker drops what it was holding ([Conductor.dropQueued]), so
// this cannot outlive the conductor.
func (c *Conductor) WaitDeliveries() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.pending > 0 {
		c.idle.Wait()
	}
}

// stopDeliveries cancels the deliveries in flight and waits for them,
// bounded by [deliveryDrain] so a delivery that ignores its context cannot
// hold the run's shutdown open for ever.
func (c *Conductor) stopDeliveries() {
	c.mu.Lock()
	cancel := c.dcancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	done := make(chan struct{})
	go func() { defer close(done); c.deliveries.Wait() }()
	timer := time.NewTimer(deliveryDrain)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		c.log.Warn("a delivery is still running after the conductor stopped",
			slog.Duration("waited", deliveryDrain))
	}
}

// Deliver ships one bead and routes the outcome back into the loops. The
// bead is held in flight until an outcome says it may be delivered again,
// so a pass that overlaps the previous one never ships it twice.
//
// It is the synchronous form, for a caller that wants the outcome; the
// conductor's own passes queue their deliveries instead
// ([Conductor.deliverApproved]). Either way the bead is delivered under the
// same lock, so two callers never rebase and fast-forward the same
// repository at once (P8-14 gap 3).
func (c *Conductor) Deliver(ctx context.Context, b domain.Bead) error {
	return c.deliverJob(ctx, deliveryJob{bead: b, attempt: c.attemptFor(b.ID)})
}

// deliverJob is [Conductor.Deliver] for one attempt of one job: it knows
// which attempt this is, so a retryable outcome can be given the next one
// ([Conductor.retryDelivery]).
func (c *Conductor) deliverJob(ctx context.Context, job deliveryJob) error {
	b := job.bead
	feedback, delivery, _ := c.parts()
	if delivery == nil {
		return nil
	}
	round := 1
	if b.Round > 0 {
		round = b.Round
	}
	// The pull request body is the only part of a run a human reviewer
	// reads, so it carries what the loop knows: the round it settled in,
	// the engineer's implementation report and the verdicts that approved
	// it (P8-30). Only the feedback loop holds those, so a run without one
	// still delivers, with the body's "not recorded" lines.
	var (
		report   *domain.ImplReport
		verdicts []domain.Verdict
	)
	if feedback != nil {
		if th, ok := feedback.Thread(b.ID); ok {
			if th.Round > 0 {
				round = th.Round
			}
			report, verdicts = th.Report, th.Verdicts
		}
	}
	c.mu.Lock()
	c.stats.Delivered++
	c.mu.Unlock()
	c.attempted(job)
	// One delivery at a time, whoever asked for it: everything below
	// mutates the same repository (D-10's rebase, fast-forward and push,
	// or a `gh pr create` against the same branch).
	c.deliver.Lock()
	out, err := delivery.Deliver(ctx, DeliveryRequest{Bead: b, Round: round, Report: report, Verdicts: verdicts})
	c.deliver.Unlock()
	if out == nil {
		// A refused request: the bead is not deliverable, or the run has no
		// path for its safety setting. Neither is retryable and neither
		// moved the bead, so the claim goes back and the next pass decides
		// again on what bd says then.
		c.release(b.ID)
		if err != nil {
			return fmt.Errorf("orchestrator: conductor: delivering %s: %w", b.ID, err)
		}
		return nil
	}
	if applyErr := c.ApplyOutcome(ctx, b.ID, out); applyErr != nil {
		err = errors.Join(err, applyErr)
	}
	if err != nil {
		return fmt.Errorf("orchestrator: conductor: delivering %s: %w", b.ID, err)
	}
	return nil
}

// ApplyOutcome is the boundary between the delivery handoff and the
// feedback loop: the deliverer has already written every status, so the
// loop is only told where the bead went (P6-06). A pull request that is now
// waiting for its gate kicks the gate poller so the merge is noticed
// without waiting for its next tick (D-20).
//
// It also ends the bead's claim on the conductor. An outcome that gives the
// bead back to the loop releases it outright; one that leaves it somewhere
// else — behind its gate, merged, or with a person — marks it shipped,
// which a later listing that finds it anywhere but approved clears
// ([Conductor.leftApproved]).
func (c *Conductor) ApplyOutcome(ctx context.Context, id domain.BeadID, out *DeliveryOutcome) error {
	feedback, _, gates := c.parts()
	if out == nil {
		return nil
	}
	c.log.Info("delivery outcome", slog.String("bead", id.String()),
		slog.String("kind", out.Kind.String()), slog.String("state", string(out.State)),
		slog.String("summary", out.Summary()))
	bead := domain.Bead{ID: id, State: out.State, Round: out.Round}
	// observed is where the loop is told the bead went, which is the
	// outcome's state everywhere but the give-up below: that one moves the
	// bead itself.
	observed := out.State
	var err error
	switch out.Kind {
	case DeliveryPending:
		// Safety on: the bead waits in delivering behind its gh:pr gate.
		c.ship(id)
		c.mu.Lock()
		c.stats.Shipped++
		c.mu.Unlock()
		if gates != nil {
			gates.Kick()
		}
	case DeliveryMerged:
		c.ship(id)
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
	case DeliveryConflict:
		// The engineer has the bead (D-10): the loop owns it again.
		c.release(id)
	case DeliveryRetryable:
		switch out.State {
		case domain.BeadStateDelivering:
			// The bead is still in delivering and nothing else will ever
			// pick it up: `bd ready` does not offer it, the feedback loop
			// was not given it back, and the next pass delivers approved
			// beads only. So the conductor owes it the retry the outcome
			// asks for (P8-14 gap 1), and after enough of them, a person.
			if !c.retryDelivery(ctx, id, out) {
				observed = c.giveUpOnDelivery(ctx, id, out)
			}
		default:
			// A retryable failure of something that is not a delivery —
			// [Deliverer.Resolved] could not write the transition a
			// finished conflict round owes — leaves the bead with the
			// loop, and re-delivering it is not what it is asking for.
			c.release(id)
		}
	case DeliveryHandedOff:
		// A person owns it now, so the conductor does not pick it back up
		// — until bd shows it out of approved and back in it again, which
		// is what resolving the hand-off looks like from here.
		c.ship(id)
	}
	if feedback != nil {
		feedback.Observe(id, observed)
	}
	if err != nil {
		return fmt.Errorf("orchestrator: conductor: acting on the delivery of %s: %w", id, err)
	}
	return nil
}

// retryDelivery queues the next attempt of a delivery that failed in a way
// the deliverer said may be tried again, and reports whether it did. The
// bead stays claimed — it is in delivering, and the conductor is the only
// thing that will ever come back to it — and the wait doubles with each
// attempt, so a remote that is down for a minute costs a minute rather than
// a delivery a second.
//
// It is false when the attempts are spent, which is
// [Conductor.giveUpOnDelivery]'s cue.
func (c *Conductor) retryDelivery(ctx context.Context, id domain.BeadID, out *DeliveryOutcome) bool {
	c.mu.Lock()
	job, ok := c.inflight[id]
	if !ok {
		// A delivery this conductor did not start (a resume handler's, a
		// caller's own): it still owes the bead its retries.
		job = deliveryJob{bead: domain.Bead{ID: id}}
	}
	if job.attempt >= c.attempts {
		c.mu.Unlock()
		return false
	}
	job.attempt++
	if job.bead.ID == "" {
		job.bead.ID = id
	}
	if out.State != "" {
		// delivering, which is where startDelivering left it: the retry is
		// a retry, not a second announcement.
		job.bead.State = out.State
	}
	if out.Round > 0 {
		job.bead.Round = out.Round
	}
	job.at = time.Now().Add(deliveryBackoff(c.backoff, job.attempt-1))
	c.inflight[id] = job
	wait := job.at
	c.mu.Unlock()
	c.log.Info("the delivery will be tried again",
		slog.String("bead", id.String()), slog.Int("attempt", job.attempt),
		slog.Int("of", c.attempts), slog.String("reason", out.Reason),
		slog.Time("at", wait))
	c.enqueue(ctx, job)
	return true
}

// deliveryBackoff is the wait before attempt n+1 (n retries already made):
// base, doubled each time, capped at [MaxDeliveryBackoff].
func deliveryBackoff(base time.Duration, retries int) time.Duration {
	wait := base
	for i := 0; i < retries && wait < MaxDeliveryBackoff; i++ {
		wait *= 2
	}
	if wait > MaxDeliveryBackoff {
		return MaxDeliveryBackoff
	}
	return wait
}

// giveUpOnDelivery hands a bead whose delivery has spent every attempt to a
// person, and returns the state the loop should be told about.
//
// Something is wrong that another attempt will not fix: `gh` cannot be
// reached, the remote keeps refusing the push, or bd would not take the
// close of a merge that has already landed. Leaving the bead in delivering
// would strand it — no work source offers a delivering bead and
// [BeadStateSettled] is false for it, so a brief run would never finish and
// D-20's "a bead is closed only when its code has landed" would be broken
// in the other direction: the code on the default branch with the bead
// never closed. needs_human is terminal, [BeadEventBlocked] reaches it from
// anywhere non-terminal, and the warning says what to look at.
func (c *Conductor) giveUpOnDelivery(ctx context.Context, id domain.BeadID, out *DeliveryOutcome) domain.BeadState {
	reason := strings.TrimSpace(out.Reason)
	if reason == "" {
		reason = "the delivery failed"
	}
	c.log.Warn("a delivery failed too many times; the bead needs a person",
		slog.String("bead", id.String()), slog.Int("attempts", c.attempts),
		slog.String("reason", reason))
	why := fmt.Errorf("%s (%d attempts): %s", id, c.attempts, reason)
	if c.opts.States == nil {
		// No machine to write with: the claim goes back so a person who
		// puts the bead back in approved still gets it delivered.
		c.release(id)
		c.run.Warn(SourceConductor, deliveryStrandedMessage, why)
		return out.State
	}
	from := out.State
	if from == "" {
		from = domain.BeadStateDelivering
	}
	if _, err := c.opts.States.Apply(ctx, StateRequest{
		Bead: id, From: from, Event: BeadEventBlocked, Round: out.Round,
		Reason: "the delivery failed " + strconv.Itoa(c.attempts) + " times: " + reason,
	}); err != nil {
		c.release(id)
		c.run.Warn(SourceConductor, deliveryStrandedMessage, errors.Join(why, err))
		return out.State
	}
	c.ship(id)
	c.run.Warn(SourceConductor, deliveryGaveUpMessage, why)
	return domain.BeadStateNeedsHuman
}

// deliveryGaveUpMessage is what a run says when it stops retrying a
// delivery, and deliveryStrandedMessage what it says when it could not move
// the bead afterwards. Both are on the troubleshooting page, and
// [TestGlueTheConductorsMessagesAreOnTheTroubleshootingPage] holds them
// there.
const (
	deliveryGaveUpMessage   = "a delivery failed too many times; the bead was handed to a person"
	deliveryStrandedMessage = "a delivery failed too many times and the bead could not be handed to a person; " +
		"it is where bd left it"
)

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
//
// This is the only production producer of the diff a tester's prompt
// renders, so it is where the P9-05 secret filter has to sit: the change
// set goes through [agents.SecretFilter.GitDiff] before it leaves, and a
// change to ".env", a key or any configured `exclude` path arrives as
// [agents.SecretPlaceholder] instead of as its own lines. The filter is
// applied even when [ConductorOptions.Filter] is nil, because a nil filter
// is the built-in patterns rather than none. The notes go through
// [agents.SecretFilter.Texts] for the same reason: they quote the
// repository.
//
// The D-09 change ratio is measured on this filtered diff, which the loop
// compares with the previous filtered one; the excluded paths are missing
// from both sides, so the comparison stays honest about everything a
// reviewer can actually see.
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
	// D-11's review bead is not this run's work: it belongs to somebody
	// else's pull request, and [PRWorktreeCheckout] leaves its worktree
	// detached at that pull request's head with the bead's own branch
	// deleted. Committing D-14's leftovers there would `git add -A` and
	// commit whatever a previous lens left in that directory — build
	// output, a test cache — onto a detached HEAD under an `sc(<bead>):
	// round N leftovers` message, and diffing it would describe the wrong
	// change set. The reviewer supplies the worktree, the branch and the
	// diff of the pull request itself (P8-14 gap 4).
	if issues[0].IsExternalPR() {
		return c.reviewBeadContext(bead, round), nil
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
	// P9-05: nothing below this line may carry an excluded path's
	// contents. The unfiltered copy goes on as
	// FeedbackBeadContext.Measure, which is only ever measured (the D-09
	// ratio has to count the lines a withheld file moved) and never
	// rendered.
	measure := diff
	diff = c.opts.Filter.GitDiff(diff)
	notes = c.opts.Filter.Texts(notes)

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
		Measure:      measure,
		Model:        c.opts.Model,
		MaxTurns:     c.opts.MaxTurns,
		Timeout:      c.opts.Timeout,
		Env:          c.opts.Env,
		InheritEnv:   c.opts.InheritEnv,
		Notes:        notes,
	}, nil
}

// reviewBeadContext is [Conductor.BeadContext] for D-11's external pull
// request review bead: everything the prompt needs about the repository and
// the run, and nothing about a worktree or a change set, both of which
// [PRReview.PlanFiring] fills in from the pull request itself. Nothing here
// touches git.
func (c *Conductor) reviewBeadContext(bead domain.Bead, round int) FeedbackBeadContext {
	return FeedbackBeadContext{
		Bead: prompt.BeadContext{
			Bead:  bead,
			Epic:  c.run.Epic(),
			Round: round,
		},
		Repo:         c.opts.Repo,
		Brief:        c.opts.Brief,
		Rules:        c.opts.Rules,
		Vibes:        c.opts.Vibes,
		Budget:       c.opts.Budget,
		CheckCommand: c.opts.CheckCommand,
		Model:        c.opts.Model,
		MaxTurns:     c.opts.MaxTurns,
		Timeout:      c.opts.Timeout,
		Env:          c.opts.Env,
		InheritEnv:   c.opts.InheritEnv,
	}
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
	c.stats.Announced += len(added)
	c.mu.Unlock()

	for _, b := range added {
		c.run.Publish(events.BeadCreated{Meta: c.run.Meta(), Bead: b})
	}
	c.publishGraph(list)
}

// publishGraph tells the run what moved in the bead graph since the last
// pass: a full [events.GraphChanged] snapshot when the graph's *shape*
// changed, and one [events.BeadUpdated] per bead when only their contents
// did.
//
// P6-45: every non-empty change used to publish the whole graph, which is
// what [beads.Watcher] does and what the TUI relays out of. But a change is
// any bd write at all — a status, an assignee, a renewed lease — while a
// snapshot is every work bead in full, description and acceptance criteria
// included. On a hundred-bead autopilot run that made graph_changed the
// biggest line in events.jsonl after the agents' own output, and the run
// wrote one for every claim, every verdict and every lease renewal. The
// information was never new: [events.GraphChanged]'s own contract is "the
// shape of the dependency graph changes: beads added or removed,
// dependencies added", and a bead whose state moved is exactly what
// [events.BeadUpdated] is for. Both are folded by [Summarise] and by the
// TUI, so the graph is rebuilt from the same events either way — the
// snapshot is just no longer repeated to say one field of one bead moved.
//
// A pass in which most of the graph changed publishes the snapshot anyway:
// per-bead events carry the same bead bodies plus a header each, so once
// half the graph has moved the snapshot is the smaller of the two.
func (c *Conductor) publishGraph(list []domain.Bead) {
	epic := c.run.Epic()
	work := make([]domain.Bead, 0, len(list))
	for _, b := range list {
		if b.ID == "" || b.Type == domain.BeadTypeEpic || b.ID == epic {
			continue
		}
		work = append(work, b)
	}

	c.mu.Lock()
	prev := c.graph
	next := make(map[domain.BeadID]domain.Bead, len(work))
	shape := false
	changed := make([]domain.Bead, 0, len(work))
	for _, b := range work {
		next[b.ID] = b
		before, known := prev[b.ID]
		switch {
		case !known:
			shape = true
		case beadShapeMoved(before, b):
			shape = true
			changed = append(changed, b)
		case len(beadFieldsChanged(before, b)) > 0:
			changed = append(changed, b)
		}
	}
	for id := range prev {
		if _, still := next[id]; !still {
			shape = true
		}
	}
	if !shape && len(changed) == 0 {
		c.mu.Unlock()
		return
	}
	full := shape || 2*len(changed) >= len(work)
	c.graph = next
	if full {
		c.stats.Graphs++
	} else {
		c.stats.Updates += len(changed)
	}
	fields := make([][]string, 0, len(changed))
	if !full {
		for _, b := range changed {
			fields = append(fields, beadFieldsChanged(prev[b.ID], b))
		}
	}
	c.mu.Unlock()

	if full {
		c.run.Publish(events.GraphChanged{Meta: c.run.Meta(), Epic: epic, Beads: work})
		return
	}
	for i, b := range changed {
		c.run.Publish(events.BeadUpdated{Meta: c.run.Meta(), Bead: b, Fields: fields[i]})
	}
}

// beadShapeMoved reports whether the graph's shape changed between two
// readings of one bead: only its dependencies are shape (P6-45).
func beadShapeMoved(before, after domain.Bead) bool {
	return !sameIDs(before.Deps, after.Deps)
}

// beadFieldsChanged names the JSON fields of [domain.Bead] that differ
// between two readings of it, in the struct's own order, for
// [events.BeadUpdated.Fields].
func beadFieldsChanged(before, after domain.Bead) []string {
	var out []string
	add := func(name string, differs bool) {
		if differs {
			out = append(out, name)
		}
	}
	add("title", before.Title != after.Title)
	add("description", before.Description != after.Description)
	add("acceptance", before.Acceptance != after.Acceptance)
	add("type", before.Type != after.Type)
	add("priority", before.Priority != after.Priority)
	add("size", before.Size != after.Size)
	add("deps", !sameIDs(before.Deps, after.Deps))
	add("state", before.State != after.State)
	add("assignee", before.Assignee != after.Assignee)
	add("labels", !sameStrings(before.Labels, after.Labels))
	add("round", before.Round != after.Round)
	add("pr_url", before.PRURL != after.PRURL)
	add("pr_number", before.PRNumber != after.PRNumber)
	return out
}

// sameIDs compares two bead id lists as sets, so a listing that returns the
// same dependencies in another order is not a change.
func sameIDs(a, b []domain.BeadID) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]domain.BeadID(nil), a...)
	y := append([]domain.BeadID(nil), b...)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// sameStrings is sameIDs for labels.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// claim reserves a bead for delivery so two passes never ship one twice: a
// delivery is queued or in flight, or the last one ended somewhere the bead
// has not come back from yet.
func (c *Conductor) claim(b domain.Bead) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, busy := c.inflight[b.ID]; busy || c.shipped[b.ID] {
		return false
	}
	c.inflight[b.ID] = deliveryJob{bead: b}
	return true
}

// attemptFor is which attempt a delivery of this bead would be: one more
// than whatever the conductor last recorded for it, and 1 for a bead it is
// not holding.
func (c *Conductor) attemptFor(id domain.BeadID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight[id].attempt + 1
}

// attempted records the attempt about to run, so a retryable outcome knows
// how many are left however it reaches [Conductor.ApplyOutcome] — through
// the queue, or through a caller's own [Conductor.Deliver].
func (c *Conductor) attempted(job deliveryJob) {
	if job.bead.ID == "" {
		return
	}
	if job.attempt < 1 {
		job.attempt = 1
	}
	c.mu.Lock()
	c.inflight[job.bead.ID] = job
	c.mu.Unlock()
}

// release lets a bead be delivered again: a check failure or a conflict,
// both of which come back through the feedback loop, or a request the
// deliverer refused outright.
func (c *Conductor) release(id domain.BeadID) {
	c.mu.Lock()
	delete(c.inflight, id)
	c.mu.Unlock()
}

// ship records a delivery that ended somewhere the conductor may not pick
// the bead back up from on its own: it is behind its gh:pr gate, it has
// merged, or a person owns it now. The mark is dropped the moment bd shows
// the bead anywhere but approved ([Conductor.leftApproved]), so a bead that
// really does come back to approved is delivered again.
func (c *Conductor) ship(id domain.BeadID) {
	c.mu.Lock()
	delete(c.inflight, id)
	c.shipped[id] = true
	c.mu.Unlock()
}

// leftApproved forgets a finished delivery once the bead has moved on from
// approved, which is what makes a later return to approved a new delivery
// rather than one the conductor refuses for ever.
func (c *Conductor) leftApproved(id domain.BeadID) {
	c.mu.Lock()
	delete(c.shipped, id)
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
