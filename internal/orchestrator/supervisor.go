package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// This file is the agent supervisor (P6-07) and the tasks folded into it:
// the read-only bd shim every agent worktree gets (P6-17) and the
// copy-worktree degrade for a cartridge that cannot restrict its own tools
// (P6-20).
//
// The loops above it never call [agents.RunAgent] directly: they describe
// an [AgentFiring] and hand it to [FireAgent], and the scheduler (P6-02)
// reaches the same path through [Supervisor.Fire], which implements its
// [Firer]. So every subprocess in a run gets the same treatment.
//
//   - a per-agent timeout (`agent_timeout`, default 30 min);
//   - crash and timeout retries, at most [DefaultCrashRetries], with an
//     exponential back-off between them;
//   - a per-cartridge pause on cartridge.ErrRateLimited, starting at
//     [DefaultRateLimitBackoff] and doubling to [MaxRateLimitBackoff],
//     which every firing on that cartridge waits out;
//   - cartridge.ErrCreditsExhausted retires that cartridge for the whole
//     run (D-18), re-routes the firing to any cartridge still alive
//     (ignoring the D-12 per-role override, which is a preference, not a
//     guarantee) and, when nothing is left, closes
//     [Supervisor.CreditsExhausted] so the planning loop can finish the run
//     with events.FinishCreditsExhausted;
//   - the bead's lease is renewed every [DefaultLeaseRenewInterval] while
//     the agent runs (P3-05), and a lost lease is reported;
//   - [Supervisor.StopAgents] cancels every live firing, waits out the
//     grace period and reaps whatever is left out of state.json, which is
//     what the run lifecycle calls on shutdown ([AgentStopper]).
//
// Everything it does reaches the human as events: events.AgentSpawned,
// events.AgentFinished, events.AgentFailed (with WillRetry), and
// events.Warning/events.Error from [SourceSupervisor].

// SourceSupervisor is the events.Warning/events.Error Source the supervisor
// stamps on its own notices.
const SourceSupervisor = "supervisor"

// Supervisor defaults. The agent timeout comes from the run's configuration
// (`limits.agent_timeout`, D-08) when the supervisor is attached to a run;
// the rest are the supervisor's own policy.
const (
	// DefaultAgentTimeout bounds one agent session when neither
	// [SupervisorOptions.Timeout] nor the run configuration says otherwise.
	DefaultAgentTimeout = config.DefaultAgentTimeout
	// DefaultCrashRetries is how many extra times a crashed, timed-out or
	// unclassified firing is tried (P6-07: max 2).
	DefaultCrashRetries = 2
	// DefaultRetryBackoff is the wait before the first crash retry; it
	// doubles per attempt up to [DefaultMaxRetryBackoff].
	DefaultRetryBackoff = 10 * time.Second
	// DefaultMaxRetryBackoff caps the crash-retry back-off.
	DefaultMaxRetryBackoff = 2 * time.Minute
	// DefaultRateLimitBackoff is the first per-cartridge pause after
	// cartridge.ErrRateLimited; it doubles per rate limit.
	DefaultRateLimitBackoff = 60 * time.Second
	// MaxRateLimitBackoff caps that pause.
	MaxRateLimitBackoff = 10 * time.Minute
	// DefaultLeaseRenewInterval is how often a running agent's bead lease is
	// renewed, well inside beads.DefaultLeaseMaxAge.
	DefaultLeaseRenewInterval = time.Minute
	// CopiesDirName is the run subdirectory holding the throwaway worktree
	// copies of the P6-20 degrade, one per agent, deleted after the firing.
	CopiesDirName = "copies"
)

// Supervisor errors, matched with errors.Is.
var (
	// ErrNoSupervisorHost means no run was attached; see [Supervisor.Attach].
	ErrNoSupervisorHost = errors.New("orchestrator: the supervisor has no run attached")
	// ErrSupervisorStopped means [Supervisor.StopAgents] has run: the
	// supervisor fires nothing else.
	ErrSupervisorStopped = errors.New("orchestrator: the supervisor has stopped; no new agents are fired")
	// ErrNoLiveCartridge means every configured cartridge is out of credits
	// (D-18) or none is usable. The planning loop turns it into
	// events.FinishCreditsExhausted; see [Supervisor.CreditsExhausted].
	ErrNoLiveCartridge = errors.New("orchestrator: no live cartridge is left for this run")
	// ErrInvalidFiring is wrapped by every rejected [AgentFiring].
	ErrInvalidFiring = errors.New("orchestrator: invalid firing")
)

// SupervisorHost is what the supervisor needs from the run it belongs to.
// *Run satisfies it; a test may pass its own.
type SupervisorHost interface {
	// EventHost is where the supervisor's events and log lines go, with the
	// run's own header on them.
	EventHost
	// ID is the run id stamped on firings and events.
	ID() domain.RunID
	// Now is the run's clock.
	Now() time.Time
	// Dirs are the run's directories (D-23): the shims live under Shims and
	// the worktree copies under Dir/CopiesDirName.
	Dirs() RunDirs
	// Warn publishes an events.Warning.
	Warn(source, message string, err error)
	// AgentStarted, AgentUpdated and AgentStopped keep state.json's live
	// agent list honest.
	AgentStarted(a domain.Agent) error
	AgentUpdated(a domain.Agent) error
	AgentStopped(id domain.AgentID) error
	// Agents lists the run's recorded agents, which the stop path reaps.
	Agents() []domain.Agent
}

// CartridgeSource hands out the cartridge a role should fire and lets the
// supervisor forget a cached Detect verdict. *cartridge.Selector satisfies
// it exactly (P2-08).
type CartridgeSource interface {
	// Select returns the cartridge the role should use (D-12).
	Select(ctx context.Context, role domain.Role) (cartridge.Cartridge, error)
	// Cartridge returns a built cartridge by registry name.
	Cartridge(name string) (cartridge.Cartridge, bool)
	// Names lists every built cartridge, sorted.
	Names() []string
	// Invalidate forgets a cached Detect verdict.
	Invalidate(name string)
}

var _ CartridgeSource = (*cartridge.Selector)(nil)

// LeaseRenewer refreshes the sc_lease of a bead an agent holds (P3-05).
// *beads.Client satisfies it. Renewal is deliberately not a state
// transition, so it does not go through the state machine (P6-03).
type LeaseRenewer interface {
	Renew(ctx context.Context, id domain.BeadID, agent domain.AgentID) (beads.Issue, error)
}

var _ LeaseRenewer = (*beads.Client)(nil)

// ShimInstaller writes the read-only bd shim for one agent worktree
// (P3-09). The package function beads.InstallForWorktree is the default;
// tests substitute their own.
type ShimInstaller interface {
	InstallForWorktree(ctx context.Context, wt string, opts beads.ShimOptions) (beads.Shim, error)
}

// ShimInstallerFunc adapts a function to [ShimInstaller].
type ShimInstallerFunc func(ctx context.Context, wt string, opts beads.ShimOptions) (beads.Shim, error)

// InstallForWorktree implements [ShimInstaller].
func (f ShimInstallerFunc) InstallForWorktree(ctx context.Context, wt string, opts beads.ShimOptions) (beads.Shim, error) {
	return f(ctx, wt, opts)
}

// WorktreeCopier makes the throwaway copy of a worktree the P6-20 degrade
// fires in. The default is [CopyWorktree].
type WorktreeCopier interface {
	Copy(ctx context.Context, src, dst string) error
}

// WorktreeCopierFunc adapts a function to [WorktreeCopier].
type WorktreeCopierFunc func(ctx context.Context, src, dst string) error

// Copy implements [WorktreeCopier].
func (f WorktreeCopierFunc) Copy(ctx context.Context, src, dst string) error { return f(ctx, src, dst) }

// FiringPlanner turns the scheduler's [Firing] — an agent and the [Demand]
// it was minted for — into the [AgentFiring] the supervisor runs: which
// worktree it works in and how its session is built. The role layers own
// that mapping (P6-04's feedback loop, P6-05's planning loop), so the
// supervisor takes it as a hook rather than importing them.
type FiringPlanner interface {
	PlanFiring(ctx context.Context, f Firing) (AgentFiring, error)
}

// FiringPlannerFunc adapts a function to [FiringPlanner].
type FiringPlannerFunc func(ctx context.Context, f Firing) (AgentFiring, error)

// PlanFiring implements [FiringPlanner].
func (fn FiringPlannerFunc) PlanFiring(ctx context.Context, f Firing) (AgentFiring, error) {
	return fn(ctx, f)
}

// SupervisorOptions configures [NewSupervisor]. Every field is optional:
// the zero value gives a supervisor with the documented defaults that has
// to be attached to a run and given a cartridge source before it can fire.
type SupervisorOptions struct {
	// Host is the run the supervisor belongs to. It may also be set later
	// with [Supervisor.Attach], which is what production wiring does: the
	// run needs the supervisor as its [AgentStopper] before it exists.
	Host SupervisorHost
	// Cartridges hands out the cartridge each role fires; required before
	// the first firing.
	Cartridges CartridgeSource
	// Planner turns a scheduler [Firing] into an [AgentFiring]; required
	// only by [Supervisor.Fire].
	Planner FiringPlanner
	// Leases renews the bead lease of a running agent; nil disables
	// renewal.
	Leases LeaseRenewer
	// Bus is where [agents.RunAgent] publishes the agents' output lines. A
	// firing that carries its own bus keeps it.
	Bus *events.Bus
	// Timeout bounds one agent session; zero means the run's
	// `limits.agent_timeout`, else [DefaultAgentTimeout].
	Timeout time.Duration
	// CrashRetries is how many extra attempts a retryable failure gets;
	// zero means [DefaultCrashRetries], negative means none.
	CrashRetries int
	// RetryBackoff and MaxRetryBackoff bound the crash-retry wait.
	RetryBackoff    time.Duration
	MaxRetryBackoff time.Duration
	// RateLimitBackoff and MaxRateLimitBackoff bound the per-cartridge
	// pause after a rate limit.
	RateLimitBackoff    time.Duration
	MaxRateLimitBackoff time.Duration
	// LeaseRenewInterval is how often a lease is renewed; negative disables
	// renewal.
	LeaseRenewInterval time.Duration
	// Shims installs the read-only bd shim (P6-17); nil means
	// beads.InstallForWorktree.
	Shims ShimInstaller
	// ShimOptions are passed to the installer (a pinned real bd binary, a
	// skipped self-check).
	ShimOptions beads.ShimOptions
	// NoShims fires agents without the read-only bd shim. Only for a
	// caller that has no bd at all; a run always installs it.
	NoShims bool
	// Copier makes the throwaway worktree copy of the P6-20 degrade; nil
	// means [CopyWorktree].
	Copier WorktreeCopier
	// Now is the clock; nil means the host's, else time.Now.
	Now func() time.Time
	// Sleep waits d or returns ctx.Err(); nil means a real timer. Tests
	// pass one that records the durations and returns at once.
	Sleep func(ctx context.Context, d time.Duration) error
	// Ticker drives lease renewal; nil means time.NewTicker. The returned
	// function stops it.
	Ticker func(d time.Duration) (<-chan time.Time, func())
	// Logger overrides the host's logger.
	Logger *slog.Logger
}

// Supervisor fires and watches every agent subprocess of one run. Create it
// with [NewSupervisor], hand it to [Options.Agents] as the run's
// [AgentStopper], then [Supervisor.Attach] the run to it. All methods are
// safe for concurrent use.
type Supervisor struct {
	opts SupervisorOptions

	shims    ShimInstaller
	copier   WorktreeCopier
	sleep    func(ctx context.Context, d time.Duration) error
	ticker   func(d time.Duration) (<-chan time.Time, func())
	baseCtx  context.Context
	baseStop context.CancelFunc

	renewals atomic.Int64

	mu        sync.Mutex
	host      SupervisorHost
	timeout   time.Duration
	stopped   bool
	dead      map[string]string
	backoff   map[string]time.Duration
	pauses    map[string]chan struct{}
	live      map[domain.AgentID]*supFiring
	started   map[domain.AgentID]bool
	shimDirs  map[string]beads.Shim
	copyDirs  map[string]string
	exhausted chan struct{}
	exhOnce   sync.Once
}

// supFiring is one firing in flight.
type supFiring struct {
	agent  domain.Agent
	cancel context.CancelFunc
	done   chan struct{}
}

// NewSupervisor builds a supervisor. It touches no file and starts no
// goroutine; the first firing does that.
func NewSupervisor(opts SupervisorOptions) (*Supervisor, error) {
	if opts.CrashRetries < 0 {
		opts.CrashRetries = -1 // "no retries", distinct from the zero default
	}
	s := &Supervisor{
		opts:      opts,
		shims:     opts.Shims,
		copier:    opts.Copier,
		sleep:     opts.Sleep,
		ticker:    opts.Ticker,
		host:      opts.Host,
		timeout:   opts.Timeout,
		dead:      map[string]string{},
		backoff:   map[string]time.Duration{},
		pauses:    map[string]chan struct{}{},
		live:      map[domain.AgentID]*supFiring{},
		started:   map[domain.AgentID]bool{},
		shimDirs:  map[string]beads.Shim{},
		copyDirs:  map[string]string{},
		exhausted: make(chan struct{}),
	}
	if s.shims == nil {
		s.shims = ShimInstallerFunc(beads.InstallForWorktree)
	}
	if s.copier == nil {
		s.copier = WorktreeCopierFunc(CopyWorktree)
	}
	if s.sleep == nil {
		s.sleep = supSleep
	}
	if s.ticker == nil {
		s.ticker = supTicker
	}
	s.baseCtx, s.baseStop = context.WithCancel(context.Background())
	if opts.Host != nil {
		s.Attach(opts.Host)
	}
	return s, nil
}

// Attach gives the supervisor its run. Production wiring builds the
// supervisor first (the run needs it as [Options.Agents]), then attaches
// the run [New] returned. Attaching also adopts the run's
// `limits.agent_timeout` unless [SupervisorOptions.Timeout] set one.
func (s *Supervisor) Attach(h SupervisorHost) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.host = h
	if s.timeout > 0 || h == nil {
		return
	}
	if rc, ok := h.(interface{ RunConfig() *config.RunConfig }); ok {
		if cfg := rc.RunConfig(); cfg != nil && cfg.Effective != nil {
			s.timeout = cfg.Effective.Limits.AgentTimeout.Duration()
		}
	}
}

// Host returns the attached run, or nil.
func (s *Supervisor) Host() SupervisorHost {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.host
}

// log is the supervisor's logger: its own when [SupervisorOptions.Logger]
// set one, else the run's, else the default.
func (s *Supervisor) log(h SupervisorHost) *slog.Logger {
	if s.opts.Logger != nil {
		return s.opts.Logger
	}
	if h != nil {
		return h.Log()
	}
	return slog.Default()
}

// now is the supervisor's clock: its own when [SupervisorOptions.Now] set
// one, else the run's, else the wall clock.
func (s *Supervisor) now() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	if h := s.Host(); h != nil {
		return h.Now()
	}
	return time.Now()
}

// Timeout returns the per-agent session timeout in force.
func (s *Supervisor) Timeout() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timeout > 0 {
		return s.timeout
	}
	return DefaultAgentTimeout
}

// Renewals counts the lease renewals the supervisor has performed.
func (s *Supervisor) Renewals() int { return int(s.renewals.Load()) }

// AgentFiring is one job for the supervisor: which agent, what it is
// doing, in which worktree, and how to build the [agents.AgentRun] once the
// supervisor has resolved the cartridge and the working directory.
//
// It is the supervisor's own job description. The scheduler's [Firing] is
// turned into one by a [FiringPlanner] (see [Supervisor.Fire]).
type AgentFiring struct {
	// Agent identifies the subprocess. ID is required; Role and Task decide
	// the tool policy (P5-06). Cartridge, Model, State and StartedAt are
	// filled in by the supervisor. PID stays zero: the firing path owns the
	// subprocess through the cartridge session, not a pid.
	Agent domain.Agent
	// Task is what the agent was fired to do.
	Task events.Task
	// Round is the review round, for events and labels.
	Round int
	// BeadTitle is Agent.Bead's title, for events.AgentSpawned.
	BeadTitle string
	// Worktree is the agent's git worktree (<run-dir>/wt/<bead-id>, D-23).
	// It gets the read-only bd shim (P6-17) and, under the P6-20 degrade,
	// is copied. Empty for a firing with no worktree of its own.
	Worktree string
	// NoLease skips lease renewal for a firing that does not hold its
	// bead's lease (a tester reviewing an engineer's work).
	NoLease bool
	// Update records a changed agent record for a caller that owns it: the
	// scheduler passes its own [Firing.Update]. When it is set the
	// supervisor leaves the run's live-agent list to that caller instead of
	// calling AgentStarted/AgentStopped itself.
	Update func(a domain.Agent)
	// Result, when set, receives the decoded payload and the transcript of
	// a successful firing made through [Supervisor.Fire], which has nowhere
	// else to hand the reply back (the scheduler's [FireResult] carries
	// none). An error from it fails the firing.
	Result func(payload any, tr agents.Transcript) error
	// Build returns the agent run for one attempt. It is called once per
	// attempt with the resolved cartridge, working directory and
	// environment; the supervisor then stamps the identity, the workdir,
	// the timeout and the shim on the result, so a builder cannot fire
	// somewhere else by accident.
	Build FiringBuilder
}

// FiringBuilder builds the agent run for one attempt (see
// [AgentFiring.Build]).
type FiringBuilder func(ctx context.Context, fc FiringContext) (agents.AgentRun, error)

// FiringContext is what the supervisor resolved for one attempt.
type FiringContext struct {
	// Run is the run id.
	Run domain.RunID
	// Agent is the agent with its cartridge and start time filled in.
	Agent domain.Agent
	// Task and Round repeat the firing's.
	Task  events.Task
	Round int
	// Cartridge is the adapter this attempt fires; under the P6-20 degrade
	// it is the wrapper that pins the firing to the copy.
	Cartridge cartridge.Cartridge
	// Workdir is where the session must run: the worktree, or its
	// throwaway copy under the degrade.
	Workdir string
	// Env holds the variables the supervisor requires, notably a PATH whose
	// first entry is the read-only bd shim (P6-17). Copy them into the run
	// spec, or ignore them: the supervisor applies the shim again to the
	// spec it fires.
	Env map[string]string
	// Timeout is the per-agent session timeout.
	Timeout time.Duration
	// Attempt is 1 for the first try and higher for retries.
	Attempt int
	// Enforcement is how the cartridge holds the firing's tool policy.
	Enforcement agents.Enforcement
	// Shim is the read-only bd shim installed for the worktree; the zero
	// value when there is none.
	Shim beads.Shim
}

// Validate reports the first problem that makes a firing impossible.
func (f AgentFiring) Validate() error {
	switch {
	case strings.TrimSpace(f.Agent.ID.String()) == "":
		return fmt.Errorf("%w: no agent id", ErrInvalidFiring)
	case f.Build == nil:
		return fmt.Errorf("%w: agent %s has no builder", ErrInvalidFiring, f.Agent.ID)
	case f.Agent.Role != "" && !f.Agent.Role.Valid():
		return fmt.Errorf("%w: agent role %q is not a role", ErrInvalidFiring, f.Agent.Role)
	case f.Worktree != "" && !filepath.IsAbs(f.Worktree):
		return fmt.Errorf("%w: worktree %q is not absolute", ErrInvalidFiring, f.Worktree)
	}
	return nil
}

// supExec runs one attempt; [Fire] and [Supervisor.FireMessage] differ only
// in how they decode the reply.
type supExec func(ctx context.Context, r agents.AgentRun) (agents.Transcript, error)

// FireAgent runs one firing under the supervisor and decodes the agent's reply
// into T (domain.Plan, domain.ImplReport, domain.Verdict,
// agents.ResponseSet, domain.Arbitration or domain.Escalation). It is the
// call every loop above the supervisor makes instead of
// [agents.RunAgent]: the retries, the back-offs, the shim, the lease and
// the events all come with it.
//
// The transcript of the last attempt is always returned, failure included.
func FireAgent[T any](ctx context.Context, s *Supervisor, f AgentFiring) (T, agents.Transcript, error) {
	var out T
	if s == nil {
		return out, agents.Transcript{}, errors.New("orchestrator: FireAgent: nil supervisor")
	}
	tr, err := s.supervise(ctx, f, func(ctx context.Context, r agents.AgentRun) (agents.Transcript, error) {
		v, tr, err := agents.RunAgent[T](ctx, r)
		if err == nil {
			out = v
		}
		return tr, err
	})
	if err != nil {
		var zero T
		return zero, tr, err
	}
	return out, tr, nil
}

// FireMessage is [FireAgent] for a caller that does not want to name the payload
// type: the reply is decoded into the payload of the run's message type
// (see [agents.RunMessage]).
func (s *Supervisor) FireMessage(ctx context.Context, f AgentFiring) (any, agents.Transcript, error) {
	var out any
	tr, err := s.supervise(ctx, f, func(ctx context.Context, r agents.AgentRun) (agents.Transcript, error) {
		v, tr, err := agents.RunMessage(ctx, r)
		if err == nil {
			out = v
		}
		return tr, err
	})
	if err != nil {
		return nil, tr, err
	}
	return out, tr, nil
}

var _ Firer = (*Supervisor)(nil)

// Fire implements the scheduler's [Firer] (P6-02): the scheduler has
// minted the agent, claimed its bead and taken an ammo slot; the supervisor
// runs the subprocess with the per-agent timeout, the crash retries, the
// rate-limit pause and the credits policy, and reports the outcome.
//
// It needs a [FiringPlanner] to know what the demand's session looks like.
// Because the scheduler already registered the agent and renews its lease
// (P6-13), the firing keeps the scheduler's record hook and skips the
// supervisor's own lease renewal. The scheduler should be built with
// SchedulerOptions.SuppressAgentEvents so the per-attempt
// events.AgentSpawned/AgentFailed the supervisor publishes are the only
// ones.
func (s *Supervisor) Fire(ctx context.Context, f Firing) (FireResult, error) {
	if s.opts.Planner == nil {
		return FireResult{}, fmt.Errorf("%w: the supervisor has no firing planner for %s", ErrInvalidFiring, f.Agent.ID)
	}
	if f.Run != nil && s.Host() == nil {
		s.Attach(f.Run)
	}
	af, err := s.opts.Planner.PlanFiring(ctx, f)
	if err != nil {
		return FireResult{}, err
	}
	if af.Agent.ID == "" {
		af.Agent = f.Agent
	}
	if af.Task == "" {
		af.Task = f.Demand.FiringTask()
	}
	if af.Round == 0 {
		af.Round = f.Demand.Round
	}
	if af.BeadTitle == "" {
		af.BeadTitle = f.Demand.BeadTitle
	}
	if af.Update == nil {
		af.Update = f.Update
	}
	// The scheduler owns the claim and its renewal (P6-13).
	af.NoLease = true

	var res FireResult
	tr, err := s.supervise(ctx, af, func(ctx context.Context, r agents.AgentRun) (agents.Transcript, error) {
		res.Produced = r.Message
		var (
			payload any
			tr      agents.Transcript
			rerr    error
		)
		if r.Message != "" {
			payload, tr, rerr = agents.RunMessage(ctx, r)
		} else {
			payload, tr, rerr = agents.RunAgent[any](ctx, r)
		}
		if rerr == nil && af.Result != nil {
			rerr = af.Result(payload, tr)
		}
		return tr, rerr
	})
	res.ExitCode = tr.ExitCode
	if err != nil {
		res.Produced, res.Failure = "", agents.FailureOf(err)
		return res, err
	}
	return res, nil
}

// supervise is the whole P6-07 policy: pick a live cartridge, wait out its
// rate-limit pause, prepare the worktree, fire, and decide what a failure
// means.
func (s *Supervisor) supervise(ctx context.Context, f AgentFiring, exec supExec) (agents.Transcript, error) {
	var tr agents.Transcript
	if ctx == nil {
		ctx = context.Background()
	}
	if err := f.Validate(); err != nil {
		return tr, err
	}
	h, err := s.hostOrErr()
	if err != nil {
		return tr, err
	}
	if s.Stopped() {
		return tr, fmt.Errorf("%w: %s", ErrSupervisorStopped, f.Agent.ID)
	}

	maxAttempts := 1 + s.crashRetries()
	attempt, reroutes := 0, 0
	var lastErr error
	for {
		attempt++
		if attempt > 1 && s.Stopped() {
			// A stop landed while this firing was waiting to try again.
			return tr, fmt.Errorf("%w: %s gave up after %w", ErrSupervisorStopped, f.Agent.ID, lastErr)
		}
		cart, err := s.pick(ctx, f.Agent.Role)
		if err != nil {
			return tr, err
		}
		name := cart.Name()
		if err := s.awaitPause(ctx, name); err != nil {
			return tr, err
		}

		prep, err := s.prepare(ctx, f, cart)
		if err != nil {
			return tr, err
		}

		agent := f.Agent
		agent.Cartridge = name
		agent.State = domain.AgentStateRunning
		agent.StartedAt = s.now()

		run, err := f.Build(ctx, FiringContext{
			Run: h.ID(), Agent: agent, Task: f.Task, Round: f.Round,
			Cartridge: prep.cartridge, Workdir: prep.workdir, Env: mergeEnv(prep.env, nil),
			Timeout: s.Timeout(), Attempt: attempt, Enforcement: prep.enforcement, Shim: prep.shim,
		})
		if err != nil {
			prep.discard()
			return tr, fmt.Errorf("orchestrator: building the firing for %s: %w", agent.ID, err)
		}
		s.stamp(&run, f, agent, prep, h)
		agent.Model = run.Spec.Model

		s.record(h, f, agent)
		s.mu.Lock()
		s.started[agent.ID] = true
		s.mu.Unlock()
		h.Publish(events.AgentSpawned{
			Meta: h.Meta(), Agent: agent, Task: f.Task,
			BeadTitle: f.BeadTitle, Round: f.Round, Attempt: attempt,
		})
		s.log(h).Info("agent fired",
			slog.String("agent", agent.ID.String()), slog.String("cartridge", name),
			slog.String("task", f.Task.String()), slog.Int("attempt", attempt),
			slog.String("workdir", prep.workdir))

		start := s.now()
		fctx, cancel := context.WithTimeout(ctx, s.Timeout()+killSlack)
		entry := s.track(agent, cancel)
		stopRenew := s.renewLease(fctx, f, agent)

		tr, err = exec(fctx, run)

		stopRenew()
		s.untrack(entry)
		cancel()
		prep.discard()
		dur := s.now().Sub(start)

		if err == nil {
			s.clearBackoff(name)
			s.finished(h, f, agent, run, tr, dur)
			return tr, nil
		}

		lastErr = err
		failure := agents.FailureOf(err)
		kind := cartridge.KindOf(err)
		retryable := supRetryable(err, failure, kind)
		if ctx.Err() != nil {
			// The caller (or the run) gave up: never a retry, and never the
			// cartridge's fault.
			s.failed(h, agent, f, tr, dur, events.FailureKilled, err, attempt, false)
			return tr, err
		}

		switch {
		case kind == cartridge.KindCreditsExhausted:
			live := s.markDead(name, "credits exhausted")
			s.invalidate(name)
			h.Warn(SourceSupervisor, fmt.Sprintf("cartridge %q is out of credits and is retired for this run (D-18)", name), err)
			if live > 0 && reroutes < len(s.cartridgeNames()) {
				reroutes++
				attempt--
				s.failed(h, agent, f, tr, dur, failure, err, attempt+1, true)
				continue
			}
			s.signalExhausted()
			h.Publish(events.Error{
				Meta: h.Meta(), Source: SourceSupervisor, Agent: agent.ID, Bead: agent.Bead,
				Message: "every cartridge is out of credits; the run can fire nothing more (D-18)",
				Err:     err.Error(),
			})
			s.failed(h, agent, f, tr, dur, failure, err, attempt, false)
			return tr, fmt.Errorf("%w: %w", ErrNoLiveCartridge, err)

		case kind == cartridge.KindRateLimited:
			pause := s.pauseCartridge(name, supRetryAfter(err))
			willRetry := attempt < maxAttempts
			h.Warn(SourceSupervisor,
				fmt.Sprintf("cartridge %q is rate limited; pausing it for %s", name, pause.Round(time.Second)), err)
			s.failed(h, agent, f, tr, dur, failure, err, attempt, willRetry)
			if !willRetry {
				return tr, err
			}
			continue

		case kind == cartridge.KindAuth:
			s.invalidate(name)
			s.failed(h, agent, f, tr, dur, failure, err, attempt, false)
			return tr, err

		case retryable && attempt < maxAttempts:
			s.failed(h, agent, f, tr, dur, failure, err, attempt, true)
			back := s.retryBackoff(attempt)
			s.log(h).Warn("agent failed; retrying",
				slog.String("agent", agent.ID.String()), slog.String("failure", string(failure)),
				slog.Int("attempt", attempt), slog.Duration("backoff", back), slog.Any("err", err))
			if serr := s.sleep(ctx, back); serr != nil {
				return tr, serr
			}
			continue

		default:
			s.failed(h, agent, f, tr, dur, failure, err, attempt, false)
			return tr, err
		}
	}
}

// stamp fills in the fields the supervisor owns on a built agent run, so a
// builder cannot fire in the wrong directory, without the shim, or without
// a timeout.
func (s *Supervisor) stamp(run *agents.AgentRun, f AgentFiring, agent domain.Agent, prep supPrep, h SupervisorHost) {
	run.Cartridge = prep.cartridge
	run.Agent = agent
	run.Task = f.Task
	run.Run = h.ID()
	if prep.workdir != "" {
		// A firing with no worktree of its own (the planning product
		// owner, P6-05; the arbitrating product owner, P6-04) keeps the
		// directory its builder chose — usually the repository itself.
		run.Spec.Workdir = prep.workdir
	}
	if run.Spec.Timeout <= 0 {
		run.Spec.Timeout = s.Timeout()
	}
	if run.Spec.Labels == nil {
		run.Spec.Labels = cartridge.LabelsFor(h.ID(), agent, f.Task, f.Round)
	}
	if run.Spec.Env == nil {
		run.Spec.Env = map[string]string{}
	}
	for k, v := range prep.env {
		if k == "PATH" {
			continue
		}
		if _, ok := run.Spec.Env[k]; !ok {
			run.Spec.Env[k] = v
		}
	}
	if prep.shim.Dir != "" {
		// Idempotent: beads.PrependPath drops a duplicate of the shim
		// directory, so a builder that already applied it is fine.
		prep.shim.Apply(run.Spec.Env)
	}
	if run.Bus == nil {
		run.Bus = s.opts.Bus
	}
	if run.Now == nil {
		run.Now = s.now
	}
}

// finished records and announces a successful firing.
func (s *Supervisor) finished(h SupervisorHost, f AgentFiring, agent domain.Agent, run agents.AgentRun, tr agents.Transcript, dur time.Duration) {
	done := agent
	done.State = domain.AgentStateFinished
	s.unrecord(h, f, done)
	h.Publish(events.AgentFinished{
		Meta: h.Meta(), Agent: agent.ID, Role: agent.Role, Bead: agent.Bead,
		Duration: dur, ExitCode: tr.ExitCode, Produced: run.Message,
	})
	s.log(h).Info("agent finished",
		slog.String("agent", agent.ID.String()), slog.String("cartridge", agent.Cartridge),
		slog.Duration("took", dur), slog.Int("attempts", len(tr.Attempts)))
}

// failed records and announces a failed attempt. A terminal failure also
// drops the agent from state.json; a retry keeps it, because the same agent
// id is fired again.
func (s *Supervisor) failed(h SupervisorHost, agent domain.Agent, f AgentFiring, tr agents.Transcript,
	dur time.Duration, failure events.Failure, err error, attempt int, willRetry bool,
) {
	if !willRetry {
		done := agent
		done.State = domain.AgentStateFailed
		if failure == events.FailureKilled {
			done.State = domain.AgentStateKilled
		}
		s.unrecord(h, f, done)
	}
	h.Publish(events.AgentFailed{
		Meta: h.Meta(), Agent: agent.ID, Role: agent.Role, Bead: agent.Bead,
		Failure: failure, Err: err.Error(), Duration: dur, ExitCode: tr.ExitCode,
		Attempt: attempt, WillRetry: willRetry,
	})
}

// record puts a running agent in state.json, or hands it to the caller that
// owns the record (the scheduler).
func (s *Supervisor) record(h SupervisorHost, f AgentFiring, a domain.Agent) {
	if f.Update != nil {
		f.Update(a)
		return
	}
	if err := h.AgentStarted(a); err != nil {
		h.Warn(SourceSupervisor, fmt.Sprintf("could not record agent %s", a.ID), err)
	}
}

// unrecord takes a finished agent out of state.json, or reports its final
// state to the caller that owns the record.
func (s *Supervisor) unrecord(h SupervisorHost, f AgentFiring, a domain.Agent) {
	if f.Update != nil {
		f.Update(a)
		return
	}
	if err := h.AgentStopped(a.ID); err != nil {
		h.Warn(SourceSupervisor, fmt.Sprintf("could not clear agent %s", a.ID), err)
	}
}

// supRetryable reports whether firing the same job again can plausibly
// succeed. An *agents.AgentError knows; anything else is judged by its
// cartridge kind, with an unusable reply never retried (the schema retry
// inside RunAgent is already spent).
func supRetryable(err error, failure events.Failure, kind cartridge.Kind) bool {
	var ae *agents.AgentError
	if errors.As(err, &ae) {
		return ae.Retryable()
	}
	if failure == events.FailureBadOutput {
		return false
	}
	return kind.Retryable()
}

// supRetryAfter returns the back-off a rate-limited cartridge asked for, or
// zero.
func supRetryAfter(err error) time.Duration {
	var ce *cartridge.Error
	if errors.As(err, &ce) {
		return ce.RetryAfter
	}
	var ae *agents.AgentError
	if errors.As(err, &ae) {
		return ae.RetryAfter
	}
	return 0
}

// crashRetries is how many extra attempts a retryable failure gets.
func (s *Supervisor) crashRetries() int {
	switch {
	case s.opts.CrashRetries < 0:
		return 0
	case s.opts.CrashRetries == 0:
		return DefaultCrashRetries
	default:
		return s.opts.CrashRetries
	}
}

// retryBackoff is the wait before attempt+1: the base doubled once per
// attempt already made, capped.
func (s *Supervisor) retryBackoff(attempt int) time.Duration {
	base := s.opts.RetryBackoff
	if base <= 0 {
		base = DefaultRetryBackoff
	}
	maxBack := s.opts.MaxRetryBackoff
	if maxBack <= 0 {
		maxBack = DefaultMaxRetryBackoff
	}
	d := base
	for i := 1; i < attempt && d < maxBack; i++ {
		d *= 2
	}
	return min(d, maxBack)
}

// hostOrErr returns the attached run.
func (s *Supervisor) hostOrErr() (SupervisorHost, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.host == nil {
		return nil, ErrNoSupervisorHost
	}
	return s.host, nil
}

// Stopped reports whether [Supervisor.StopAgents] has run.
func (s *Supervisor) Stopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// pick returns the cartridge this attempt fires: the one the selector gives
// the role (D-12), or — when that one is out of credits — any cartridge
// still alive, because a preference must not stop a run that still has
// credits somewhere (D-18).
func (s *Supervisor) pick(ctx context.Context, role domain.Role) (cartridge.Cartridge, error) {
	if s.opts.Cartridges == nil {
		return nil, errors.New("orchestrator: the supervisor has no cartridge source")
	}
	names := s.opts.Cartridges.Names()
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: no cartridge is built for this run", ErrNoLiveCartridge)
	}
	chosen, selErr := s.opts.Cartridges.Select(ctx, role)
	if selErr == nil && chosen != nil && !s.isDead(chosen.Name()) {
		return chosen, nil
	}
	for _, n := range names {
		if s.isDead(n) {
			continue
		}
		if selErr == nil && chosen != nil && n == chosen.Name() {
			continue
		}
		c, ok := s.opts.Cartridges.Cartridge(n)
		if !ok || c == nil {
			continue
		}
		if selErr == nil {
			if h := s.Host(); h != nil {
				h.Warn(SourceSupervisor,
					fmt.Sprintf("cartridge %q is retired; %s fires %q instead", chosen.Name(), role, n), nil)
			}
		}
		return c, nil
	}
	if s.allDead() {
		s.signalExhausted()
		return nil, fmt.Errorf("%w: every cartridge (%s) is out of credits", ErrNoLiveCartridge, strings.Join(names, ", "))
	}
	if selErr != nil {
		return nil, selErr
	}
	return nil, fmt.Errorf("%w: nothing usable among %s", ErrNoLiveCartridge, strings.Join(names, ", "))
}

func (s *Supervisor) cartridgeNames() []string {
	if s.opts.Cartridges == nil {
		return nil
	}
	return s.opts.Cartridges.Names()
}

func (s *Supervisor) invalidate(name string) {
	if s.opts.Cartridges != nil {
		s.opts.Cartridges.Invalidate(name)
	}
}

// markDead retires a cartridge for the rest of the run and returns how many
// are still alive.
func (s *Supervisor) markDead(name, reason string) int {
	names := s.cartridgeNames()
	s.mu.Lock()
	if _, done := s.dead[name]; !done {
		s.dead[name] = reason
	}
	live := 0
	for _, n := range names {
		if _, gone := s.dead[n]; !gone {
			live++
		}
	}
	s.mu.Unlock()
	return live
}

func (s *Supervisor) isDead(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, gone := s.dead[name]
	return gone
}

func (s *Supervisor) allDead() bool {
	names := s.cartridgeNames()
	if len(names) == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range names {
		if _, gone := s.dead[n]; !gone {
			return false
		}
	}
	return true
}

// DeadCartridges lists the cartridges retired for this run, sorted.
func (s *Supervisor) DeadCartridges() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.dead))
	for n := range s.dead {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// LiveCartridges lists the cartridges the run may still fire, sorted.
func (s *Supervisor) LiveCartridges() []string {
	names := s.cartridgeNames()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(names))
	for _, n := range names {
		if _, gone := s.dead[n]; !gone {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// CreditsExhausted is closed once every configured cartridge is out of
// credits. The planning loop (P6-05) selects on it and finishes the run
// with events.FinishCreditsExhausted, which is the only way an autopilot
// run ends by itself (D-18).
func (s *Supervisor) CreditsExhausted() <-chan struct{} { return s.exhausted }

// Exhausted reports whether every configured cartridge is out of credits.
func (s *Supervisor) Exhausted() bool {
	select {
	case <-s.exhausted:
		return true
	default:
		return false
	}
}

func (s *Supervisor) signalExhausted() { s.exhOnce.Do(func() { close(s.exhausted) }) }

// pauseCartridge starts (or keeps) the per-cartridge rate-limit pause and
// returns its length: the CLI's own hint when it gave one, else the
// cartridge's back-off, which starts at [DefaultRateLimitBackoff] and
// doubles up to [MaxRateLimitBackoff]. Every firing on that cartridge waits
// for the pause to end (see [Supervisor.awaitPause]).
func (s *Supervisor) pauseCartridge(name string, retryAfter time.Duration) time.Duration {
	base := s.opts.RateLimitBackoff
	if base <= 0 {
		base = DefaultRateLimitBackoff
	}
	maxBack := s.opts.MaxRateLimitBackoff
	if maxBack <= 0 {
		maxBack = MaxRateLimitBackoff
	}

	s.mu.Lock()
	next := s.backoff[name]
	switch {
	case next <= 0:
		next = base
	default:
		next = min(next*2, maxBack)
	}
	s.backoff[name] = next
	d := next
	if retryAfter > 0 {
		d = min(retryAfter, maxBack)
	}
	if _, paused := s.pauses[name]; paused {
		s.mu.Unlock()
		return d
	}
	ch := make(chan struct{})
	s.pauses[name] = ch
	s.mu.Unlock()

	go func() {
		_ = s.sleep(s.baseCtx, d)
		s.mu.Lock()
		delete(s.pauses, name)
		s.mu.Unlock()
		close(ch)
	}()
	return d
}

// awaitPause blocks while name is rate limited.
func (s *Supervisor) awaitPause(ctx context.Context, name string) error {
	s.mu.Lock()
	ch, paused := s.pauses[name]
	s.mu.Unlock()
	if !paused {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// clearBackoff forgets a cartridge's rate-limit history after a session it
// answered normally.
func (s *Supervisor) clearBackoff(name string) {
	s.mu.Lock()
	delete(s.backoff, name)
	s.mu.Unlock()
}

// Backoff returns the next rate-limit pause recorded for a cartridge; zero
// when it is not backed off.
func (s *Supervisor) Backoff(name string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backoff[name]
}

// track registers a live firing so the stop path can cancel it.
func (s *Supervisor) track(a domain.Agent, cancel context.CancelFunc) *supFiring {
	e := &supFiring{agent: a, cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	s.live[a.ID] = e
	s.mu.Unlock()
	return e
}

func (s *Supervisor) untrack(e *supFiring) {
	s.mu.Lock()
	if cur, ok := s.live[e.agent.ID]; ok && cur == e {
		delete(s.live, e.agent.ID)
	}
	s.mu.Unlock()
	close(e.done)
}

// Live returns the agents the supervisor currently has in flight.
func (s *Supervisor) Live() []domain.Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Agent, 0, len(s.live))
	for _, e := range s.live {
		out = append(out, e.agent)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// StopAgents implements [AgentStopper]: it stops firing anything new,
// cancels every live session (which sends the process group SIGTERM and
// then SIGKILL through internal/proc), waits out the grace period, reaps
// whatever is left out of state.json, and removes the run's bd shims and
// worktree copies. A grace of zero still allows the cancelled sessions the
// moment they need to die.
func (s *Supervisor) StopAgents(ctx context.Context, grace time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	s.stopped = true
	live := make([]*supFiring, 0, len(s.live))
	for _, e := range s.live {
		live = append(live, e)
	}
	s.mu.Unlock()

	for _, e := range live {
		e.cancel()
	}
	if grace < 0 {
		grace = 0
	}
	wctx, cancel := context.WithTimeout(ctx, grace+killSlack)
	defer cancel()

	var stuck []string
	for _, e := range live {
		select {
		case <-e.done:
		case <-wctx.Done():
			stuck = append(stuck, e.agent.ID.String())
		}
	}
	s.reap()
	s.removeShims()
	s.removeCopies()
	if len(stuck) > 0 {
		sort.Strings(stuck)
		return fmt.Errorf("orchestrator: %d agent(s) did not stop within %s: %s",
			len(stuck), grace, strings.Join(stuck, ", "))
	}
	return nil
}

// Close releases the supervisor's own resources (the rate-limit pauses)
// without stopping agents. [Supervisor.StopAgents] is what a run calls;
// Close is for a caller that built a supervisor and never fired.
func (s *Supervisor) Close() error {
	s.baseStop()
	return nil
}

// reap is the zombie reaping of P6-07: an agent this supervisor fired that
// is still recorded in state.json after the stop is dropped from it, so a
// resumed run does not think it is alive.
func (s *Supervisor) reap() {
	h := s.Host()
	if h == nil {
		return
	}
	s.mu.Lock()
	mine := make(map[domain.AgentID]bool, len(s.started))
	for id := range s.started {
		mine[id] = true
	}
	s.live = map[domain.AgentID]*supFiring{}
	s.mu.Unlock()

	for _, a := range h.Agents() {
		if !mine[a.ID] {
			continue
		}
		if err := h.AgentStopped(a.ID); err != nil {
			h.Warn(SourceSupervisor, fmt.Sprintf("could not reap agent %s", a.ID), err)
			continue
		}
		s.log(h).Info("agent reaped", slog.String("agent", a.ID.String()))
	}
	s.baseStop()
}

// supPrep is what one attempt runs against: the cartridge (possibly the
// copy-worktree wrapper), the working directory, the environment the
// supervisor requires and the throwaway copy's cleanup.
type supPrep struct {
	cartridge   cartridge.Cartridge
	workdir     string
	env         map[string]string
	shim        beads.Shim
	enforcement agents.Enforcement
	discard     func()
}

// prepare installs the read-only bd shim for the firing's worktree (P6-17)
// and, when the cartridge has no tool controls of its own, copies the
// worktree so the session cannot touch the real one (P6-20).
func (s *Supervisor) prepare(ctx context.Context, f AgentFiring, c cartridge.Cartridge) (supPrep, error) {
	p := supPrep{cartridge: c, workdir: f.Worktree, env: map[string]string{}, discard: func() {}}

	if f.Worktree != "" && !s.opts.NoShims {
		shim, err := s.installShim(ctx, f.Worktree)
		if err != nil {
			return p, fmt.Errorf("orchestrator: installing the read-only bd shim for %s: %w", f.Worktree, err)
		}
		p.shim = shim
		shim.Apply(p.env)
	}

	policy, perr := agents.Policy(f.Agent.Role, f.Task)
	if perr != nil {
		// Neither a role nor a task: nothing to derive a policy from, so
		// the firing's own spec decides and agents.RunAgent checks it.
		return p, nil
	}
	enf, err := agents.EnforcementFor(c, policy)
	if err != nil {
		return p, err
	}
	p.enforcement = enf
	if enf.Mode != agents.EnforceCopyWorktree {
		return p, nil
	}

	// P6-20: the adapter cannot restrict its own tools, so the session runs
	// in a throwaway copy of the worktree and the copy is deleted
	// afterwards. A firing with no worktree has nothing to copy and is
	// refused rather than run unrestricted.
	if f.Worktree == "" {
		return p, fmt.Errorf("%w: cartridge %q cannot hold %s and the firing has no worktree to copy",
			agents.ErrCopyWorktreeUnimplemented, c.Name(), policy)
	}
	dst, err := s.copyDir(f.Agent.ID)
	if err != nil {
		return p, err
	}
	if err := os.RemoveAll(dst); err != nil {
		return p, fmt.Errorf("orchestrator: clearing the worktree copy %s: %w", dst, err)
	}
	if err := s.copier.Copy(ctx, f.Worktree, dst); err != nil {
		return p, fmt.Errorf("orchestrator: copying %s for %s: %w", f.Worktree, f.Agent.ID, err)
	}
	s.mu.Lock()
	s.copyDirs[dst] = dst
	s.mu.Unlock()

	wrapped := &supCopiedCartridge{inner: c, root: dst, policy: policy}
	copied, err := wrapped.ToolPolicyEnforcement(policy)
	if err != nil {
		return p, err
	}
	p.workdir = dst
	p.cartridge = wrapped
	p.enforcement = copied
	p.discard = func() {
		if err := os.RemoveAll(dst); err != nil {
			if h := s.Host(); h != nil {
				h.Warn(SourceSupervisor, fmt.Sprintf("could not delete the worktree copy %s", dst), err)
			}
			return
		}
		s.mu.Lock()
		delete(s.copyDirs, dst)
		s.mu.Unlock()
	}
	if h := s.Host(); h != nil {
		s.log(h).Info("firing in a throwaway worktree copy (P6-20)",
			slog.String("agent", f.Agent.ID.String()), slog.String("cartridge", c.Name()),
			slog.String("policy", policy.String()), slog.String("copy", dst))
	}
	return p, nil
}

// installShim writes the read-only bd shim for a worktree, once per
// worktree per run (P3-09, P6-17). The shim lives beside the worktree at
// <run-dir>/bin/<bead-id>, never inside it, so the agent's tree stays
// clean.
func (s *Supervisor) installShim(ctx context.Context, wt string) (beads.Shim, error) {
	s.mu.Lock()
	if sh, ok := s.shimDirs[wt]; ok {
		s.mu.Unlock()
		return sh, nil
	}
	s.mu.Unlock()

	sh, err := s.shims.InstallForWorktree(ctx, wt, s.opts.ShimOptions)
	if err != nil {
		return beads.Shim{}, err
	}
	s.mu.Lock()
	s.shimDirs[wt] = sh
	s.mu.Unlock()
	return sh, nil
}

// Shim returns the read-only bd shim installed for a worktree, if any.
func (s *Supervisor) Shim(worktree string) (beads.Shim, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.shimDirs[worktree]
	return sh, ok
}

// RemoveShim deletes the shim directory of one bead, <run-dir>/bin/<bead-id>
// (P6-17's cleanup half). It is safe to call for a bead that never had one.
func (s *Supervisor) RemoveShim(bead domain.BeadID) error {
	h := s.Host()
	if h == nil || bead == "" {
		return nil
	}
	dir := filepath.Join(h.Dirs().Shims, bead.String())
	s.mu.Lock()
	for wt, sh := range s.shimDirs {
		if sh.Dir == dir {
			delete(s.shimDirs, wt)
		}
	}
	s.mu.Unlock()
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("orchestrator: removing the bd shim %s: %w", dir, err)
	}
	return nil
}

// removeShims deletes every shim directory this supervisor installed.
func (s *Supervisor) removeShims() {
	s.mu.Lock()
	dirs := make([]string, 0, len(s.shimDirs))
	for wt, sh := range s.shimDirs {
		dirs = append(dirs, sh.Dir)
		delete(s.shimDirs, wt)
	}
	s.mu.Unlock()
	for _, d := range dirs {
		if err := os.RemoveAll(d); err != nil {
			if h := s.Host(); h != nil {
				h.Warn(SourceSupervisor, fmt.Sprintf("could not remove the bd shim %s", d), err)
			}
		}
	}
}

// removeCopies deletes any throwaway worktree copy still on disk.
func (s *Supervisor) removeCopies() {
	s.mu.Lock()
	dirs := make([]string, 0, len(s.copyDirs))
	for d := range s.copyDirs {
		dirs = append(dirs, d)
		delete(s.copyDirs, d)
	}
	s.mu.Unlock()
	for _, d := range dirs {
		_ = os.RemoveAll(d)
	}
}

// copyDir is where the throwaway copy of an agent's worktree goes:
// <run-dir>/copies/<agent-id> (D-23: never inside the target repository).
func (s *Supervisor) copyDir(id domain.AgentID) (string, error) {
	h := s.Host()
	if h == nil {
		return "", ErrNoSupervisorHost
	}
	name := filepath.Base(filepath.Clean(id.String()))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "", fmt.Errorf("%w: agent id %q is not usable as a directory name", ErrInvalidFiring, id)
	}
	return filepath.Join(h.Dirs().Dir, CopiesDirName, name), nil
}

// renewLease keeps the bead's sc_lease fresh while the agent runs (P3-05).
// The returned function stops the renewer and waits for it.
func (s *Supervisor) renewLease(ctx context.Context, f AgentFiring, a domain.Agent) func() {
	interval := s.opts.LeaseRenewInterval
	if interval == 0 {
		interval = DefaultLeaseRenewInterval
	}
	if s.opts.Leases == nil || f.NoLease || a.Bead == "" || interval < 0 {
		return func() {}
	}
	h := s.Host()
	rctx, cancel := context.WithCancel(ctx)
	ticks, stopTicker := s.ticker(interval)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-rctx.Done():
				return
			case _, ok := <-ticks:
				if !ok {
					return
				}
				if _, err := s.opts.Leases.Renew(rctx, a.Bead, a.ID); err != nil {
					if rctx.Err() != nil {
						return
					}
					if h != nil {
						h.Warn(SourceSupervisor,
							fmt.Sprintf("could not renew the lease on %s for %s", a.Bead, a.ID), err)
					}
					if errors.Is(err, beads.ErrLeaseLost) {
						return
					}
					continue
				}
				s.renewals.Add(1)
				if h != nil {
					s.log(h).Debug("lease renewed",
						slog.String("bead", a.Bead.String()), slog.String("agent", a.ID.String()))
				}
			}
		}
	}()
	return func() {
		cancel()
		stopTicker()
		<-done
	}
}

// supCopiedCartridge is the P6-20 degrade in force: the wrapper answers for
// the tool policy itself ([agents.ToolPolicyEnforcer]) because the firing
// happens in a throwaway copy of the worktree, and it refuses to run
// anywhere else — so a builder that overrode the working directory cannot
// slip the session back into the real tree.
type supCopiedCartridge struct {
	inner  cartridge.Cartridge
	root   string
	policy cartridge.ToolPolicy
}

var (
	_ cartridge.Cartridge       = (*supCopiedCartridge)(nil)
	_ agents.ToolPolicyEnforcer = (*supCopiedCartridge)(nil)
)

// Name implements cartridge.Cartridge: the wrapper is invisible in events
// and logs, which name the real adapter.
func (c *supCopiedCartridge) Name() string { return c.inner.Name() }

// Detect implements cartridge.Cartridge.
func (c *supCopiedCartridge) Detect(ctx context.Context) (cartridge.Status, error) {
	return c.inner.Detect(ctx)
}

// Run implements cartridge.Cartridge, refusing any workdir but the copy.
func (c *supCopiedCartridge) Run(ctx context.Context, spec cartridge.RunSpec) (cartridge.Session, error) {
	if filepath.Clean(spec.Workdir) != filepath.Clean(c.root) {
		return nil, fmt.Errorf("%w: cartridge %q must fire %s in the worktree copy %s, not %s",
			agents.ErrCopyWorktreeUnimplemented, c.inner.Name(), c.policy, c.root, spec.Workdir)
	}
	return c.inner.Run(ctx, spec)
}

// ToolPolicyEnforcement implements [agents.ToolPolicyEnforcer]: the policy
// this wrapper was made for is held by the copy; any other policy is the
// inner adapter's business.
func (c *supCopiedCartridge) ToolPolicyEnforcement(p cartridge.ToolPolicy) (agents.Enforcement, error) {
	if p != c.policy {
		return agents.EnforcementFor(c.inner, p)
	}
	return agents.Enforcement{
		Cartridge: c.inner.Name(),
		Policy:    p,
		Mode:      agents.EnforceCopiedWorktree,
		Detail:    "no tool controls: the session runs in a throwaway copy of the worktree, discarded afterwards (P6-20)",
	}, nil
}

// CopyWorktree copies the tree at src to dst, which must not exist. It is
// the default [WorktreeCopier] of the P6-20 degrade: regular files (with
// their permission bits) and directories are copied and symlinks are
// recreated as symlinks, so nothing outside src is touched.
//
// The `.git` entry is deliberately not copied. In a git worktree it is a
// file pointing back into the real repository's administrative directory,
// so a copy that kept it would let the very writes the degrade exists to
// contain reach the real repository. The reviewed diff is in the agent's
// prompt, not in the copy's history.
func CopyWorktree(ctx context.Context, src, dst string) error {
	src = filepath.Clean(src)
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("orchestrator: copy worktree: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("orchestrator: copy worktree: %s is not a directory", src)
	}
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return os.MkdirAll(dst, runDirPerm)
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			fi, ferr := d.Info()
			if ferr != nil {
				return ferr
			}
			return os.MkdirAll(target, fi.Mode().Perm()|0o700)
		case d.Type()&os.ModeSymlink != 0:
			link, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			fi, ferr := d.Info()
			if ferr != nil {
				return ferr
			}
			return supCopyFile(path, target, fi.Mode().Perm())
		default:
			// Sockets, devices and pipes are not part of a source tree.
			return nil
		}
	})
}

// supCopyFile copies one regular file, creating it with mode.
func supCopyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// supSleep is the default back-off wait: a timer that a cancelled context
// cuts short.
func supSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// supTicker is the default lease-renewal ticker.
func supTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}
