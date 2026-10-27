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
// (P6-20), whose contained session is told in its prompt that the copy has
// no git in it ([CopiedWorktreeNote], P6-30).
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
//     which every firing on that cartridge waits out; a rate limit never
//     spends a crash retry, so a firing waits as long as the limit lasts
//     (D-18: a rate limit only pauses);
//   - cartridge.ErrCreditsExhausted retires that cartridge for the whole
//     run (D-18), re-routes the firing to any cartridge still alive
//     (ignoring the D-12 per-role override, which is a preference, not a
//     guarantee) and, when nothing is left, closes
//     [Supervisor.CreditsExhausted] so the planning loop can finish the run
//     with events.FinishCreditsExhausted;
//   - a failure of the firing's own post-session hook
//     ([AgentFiring.Result]) is the run's bookkeeping failing after a
//     session that worked, so it is terminal (retrying would fire the
//     whole session again and repeat its side effects, D-06) and is
//     reported as events.FailureInternal, never as a crash;
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

// CopiedWorktreeNote is the paragraph the supervisor appends to the prompt
// of every firing that runs under the P6-20 degrade
// ([agents.EnforceCopiedWorktree]).
//
// The copy deliberately has no `.git`: in a git worktree that entry is a
// file pointing back into the real repository's administrative directory,
// so copying it would let the very writes the degrade exists to contain
// reach the repository (see [CopyWorktree]). Every git command therefore
// fails in the copy, and an agent that does not know it will waste its
// session on `git diff`, `git log` and `git status` and then report that
// the repository is broken. No shipping adapter needs the degrade (D-19),
// so this note — rather than a sanitised standalone clone — is what the
// first adapter that does gets (P6-30).
const CopiedWorktreeNote = "Note on this working directory: it is a throwaway copy of the agent worktree, " +
	"made because the CLI running you cannot restrict its own tools. " +
	"It has no `.git`, so git is unavailable here: `git diff`, `git log`, `git status`, `git commit` and every " +
	"other git command will fail, and that is expected — it is not a broken repository. " +
	"Work from the code in this directory and from what this prompt tells you about the change. " +
	"Everything you write here is thrown away when the session ends and never reaches the repository, " +
	"so put your answer in your reply, not in a file."

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
	// ErrPostSession is matched by every error [AgentFiring.Result]
	// returned: the agent session succeeded and the run's own bookkeeping
	// after it did not. Such a firing is never retried.
	ErrPostSession = errors.New("orchestrator: the agent session succeeded but the run could not record its result")
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
	// RoleModels overrides the model per role (roles.<role>.model): a role
	// listed here is fired with that model whatever its builder chose.
	RoleModels map[domain.Role]string
	// ActiveCartridge is the run's own cartridge, the one whose model the
	// builders stamp on every firing. A firing that lands on another
	// cartridge (roles.<role>.cartridge, D-12/D-29) and has no role model
	// drops that model so its adapter uses its own configured one: Cursor
	// must not be asked for Claude's "sonnet". Empty disables the check.
	ActiveCartridge string
	// Research gives the planning product owner web search and fetch
	// (the `research` config key).
	Research bool
	// BeadTimeout, when set, returns the longer session limit the product
	// owner gave a bead (sc_timeout), or zero. Every agent fired on that
	// bead gets it, capped at MaxTimeout; it never shortens the default.
	BeadTimeout func(ctx context.Context, bead domain.BeadID) time.Duration
	// MaxTimeout caps a bead's own timeout (limits.max_agent_timeout).
	MaxTimeout time.Duration
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
		s.copier = WorktreeCopierFunc(s.copyWorktree)
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
//
// The timeout comes through [EffectiveLimits], not from the configuration
// directly, so a run built from a hand-made config with no agent_timeout
// gets the documented default rather than no timeout at all (P9-01).
func (s *Supervisor) Attach(h SupervisorHost) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.host = h
	if s.timeout > 0 || h == nil {
		return
	}
	if rc, ok := h.(interface{ RunConfig() *config.RunConfig }); ok {
		s.timeout = EffectiveLimits(rc.RunConfig()).AgentTimeout.Duration()
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

// timeoutFor is the session limit of an agent fired on bead: the run's own,
// raised to the bead's sc_timeout when the product owner gave it a longer
// one, and capped at MaxTimeout.
func (s *Supervisor) timeoutFor(ctx context.Context, bead domain.BeadID) time.Duration {
	base := s.Timeout()
	if bead == "" || s.opts.BeadTimeout == nil {
		return base
	}
	d := s.opts.BeadTimeout(ctx, bead)
	if limit := s.opts.MaxTimeout; limit > 0 && d > limit {
		d = limit
	}
	return max(base, d)
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
	// none). It runs after the session, so an error from it fails the
	// firing terminally: the firing reports events.FailureInternal and
	// matches [ErrPostSession], and the session is never fired again,
	// because the agent has already done the work and had its side effects
	// (D-06).
	Result func(payload any, tr agents.Transcript) error
	// Spent, when set, receives the transcript of an attempt that did not
	// deliver a usable reply: a crash, a timeout, a kill, a rate-limit
	// retry, a reroute to another cartridge. It is how a session that spent
	// real tokens and produced nothing is still charged against the run
	// budget (P9-13) — [AgentFiring.Result] runs only on a session that
	// finished and decoded, so an agent killed by `limits.agent_timeout`
	// after twenty minutes of work used to cost the budget nothing at all.
	//
	// It is called once per failed attempt, on the supervising goroutine,
	// with that attempt's own transcript (usage is per RunAgent call, so
	// nothing is counted twice); the successful attempt of a retried firing
	// is left to Result. It returns nothing: metering must never turn a
	// failure into a different failure. A firing abandoned before its
	// session started never calls it — there is no transcript and nothing
	// was spent.
	Spent func(tr agents.Transcript)
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
			// The session is over and its reply is decoded: anything the
			// hook reports is the run's own bookkeeping failing, and
			// supervise must not mistake it for a crashed session.
			if herr := af.Result(payload, tr); herr != nil {
				rerr = &supPostSessionError{err: herr}
			}
		}
		return tr, rerr
	})
	res.ExitCode = tr.ExitCode
	if err != nil {
		res.Produced, res.Failure = "", supFailureOf(err)
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

	timeout := s.timeoutFor(ctx, f.Agent.Bead)
	maxAttempts := 1 + s.crashRetries()
	attempt, reroutes := 0, 0
	var lastErr error
	// pending is the agent an attempt left recorded as running because it
	// announced its failure with WillRetry=true. Every spawn owes a
	// terminal agent event, so a firing abandoned before it can try again
	// (a stop, a dead context, a cartridge that can no longer be picked,
	// a worktree that can no longer be prepared) reports that failure as
	// terminal through abandon rather than leaving a ghost behind in
	// state.json.
	var pending *domain.Agent
	pendingAttempt := 0
	abandon := func(err error) error {
		if pending != nil {
			s.failed(h, *pending, f, tr, 0, supAbandonFailure(err), err, pendingAttempt, false)
			pending = nil
		}
		return err
	}
	for {
		attempt++
		if attempt > 1 && s.Stopped() {
			// A stop landed while this firing was waiting to try again.
			return tr, abandon(fmt.Errorf("%w: %s gave up after %w", ErrSupervisorStopped, f.Agent.ID, lastErr))
		}
		cart, err := s.pick(ctx, f.Agent.Role)
		if err != nil {
			return tr, abandon(err)
		}
		name := cart.Name()
		if err := s.awaitPause(ctx, name); err != nil {
			return tr, abandon(err)
		}

		prep, err := s.prepare(ctx, f, cart)
		if err != nil {
			return tr, abandon(err)
		}

		agent := f.Agent
		agent.Cartridge = name
		agent.State = domain.AgentStateRunning
		agent.StartedAt = s.now()

		run, err := f.Build(ctx, FiringContext{
			Run: h.ID(), Agent: agent, Task: f.Task, Round: f.Round,
			Cartridge: prep.cartridge, Workdir: prep.workdir, Env: mergeEnv(prep.env, nil),
			Timeout: timeout, Attempt: attempt, Enforcement: prep.enforcement, Shim: prep.shim,
		})
		if err != nil {
			prep.discard()
			return tr, abandon(fmt.Errorf("orchestrator: building the firing for %s: %w", agent.ID, err))
		}
		s.stamp(&run, f, agent, prep, h)
		agent.Model = run.Spec.Model

		// Tracking comes before anything else this attempt does, and is
		// refused once StopAgents has run: taking the stop flag and
		// registering the firing under one lock is what makes the
		// snapshot the stop path takes of s.live complete. Preparing an
		// attempt is slow — the rate-limit pause, the shim, the worktree
		// copy, the builder — and a stop landing in that window must not
		// leave a subprocess behind that nothing tracks or cancels.
		start := s.now()
		if run.Spec.Timeout < timeout {
			run.Spec.Timeout = timeout
		}
		fctx, cancel := context.WithTimeout(ctx, timeout+killSlack)
		entry, tracked := s.track(agent, cancel)
		if !tracked {
			cancel()
			prep.discard()
			return tr, abandon(fmt.Errorf("%w: %s was stopped before its session started",
				ErrSupervisorStopped, f.Agent.ID))
		}

		s.record(h, f, agent)
		// The agent is recorded and about to be announced again: whatever
		// the previous attempt left pending is now this attempt's story.
		pending = nil
		h.Publish(events.AgentSpawned{
			Meta: h.Meta(), Agent: agent, Task: f.Task,
			BeadTitle: f.BeadTitle, Round: f.Round, Attempt: attempt,
		})
		s.log(h).Info("agent fired",
			slog.String("agent", agent.ID.String()), slog.String("cartridge", name),
			slog.String("task", f.Task.String()), slog.Int("attempt", attempt),
			slog.String("workdir", prep.workdir))

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

		var hookErr *supPostSessionError
		if errors.As(err, &hookErr) {
			// The session did its job: the agent answered and the reply
			// was decoded. Only the run's bookkeeping afterwards failed —
			// a bead comment, a D-07 transition, a bd call — so firing the
			// cartridge again would repeat work that has already happened
			// (and its side effects, D-06), burn another session of
			// credits, and tell the human the cartridge crashed when it
			// did not.
			s.clearBackoff(name)
			s.failed(h, agent, f, tr, dur, events.FailureInternal, err, attempt, false)
			h.Publish(events.Error{
				Meta: h.Meta(), Source: SourceSupervisor, Agent: agent.ID, Bead: agent.Bead,
				Message: fmt.Sprintf(
					"the session for %s finished, but the run could not record its result; it is not fired again",
					agent.ID),
				Err: hookErr.err.Error(),
			})
			return tr, err
		}

		// Every path from here on is a session that ran and did not deliver:
		// what it spent is real, and only this hook can see it (P9-13). The
		// post-session hook above is deliberately above this line — its
		// session succeeded and was metered by Result.
		if f.Spent != nil {
			f.Spent(tr)
		}

		lastErr = err
		failure := agents.FailureOf(err)
		kind := cartridge.KindOf(err)
		retryable := supRetryable(err, failure, kind)
		if ctxErr := ctx.Err(); ctxErr != nil {
			// The caller (or the run) gave up: never a retry, and never the
			// cartridge's fault.
			s.failed(h, agent, f, tr, dur, events.FailureKilled, err, attempt, false)
			return tr, errors.Join(ctxErr, err)
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
				pending, pendingAttempt = supPending(agent), attempt+1
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
			// A rate limit only pauses (D-18): it says nothing about
			// whether this work can succeed, so it never spends one of the
			// crash retries. The firing waits out the pause for as long as
			// the limit lasts — a subscription quota window can be hours,
			// and dandelion reports "no account has quota" this way — and
			// only a stop or the caller giving up ends it.
			maxAttempts++
			pause := s.pauseCartridge(name, supRetryAfter(err))
			willRetry := attempt < maxAttempts
			h.Warn(SourceSupervisor,
				fmt.Sprintf("cartridge %q is rate limited; pausing it for %s", name, pause.Round(time.Second)), err)
			s.failed(h, agent, f, tr, dur, failure, err, attempt, willRetry)
			if !willRetry {
				return tr, err
			}
			pending, pendingAttempt = supPending(agent), attempt
			continue

		case kind == cartridge.KindAuth:
			s.invalidate(name)
			s.failed(h, agent, f, tr, dur, failure, err, attempt, false)
			return tr, err

		case retryable && attempt < maxAttempts:
			s.failed(h, agent, f, tr, dur, failure, err, attempt, true)
			pending, pendingAttempt = supPending(agent), attempt
			back := s.retryBackoff(attempt)
			s.log(h).Warn("agent failed; retrying",
				slog.String("agent", agent.ID.String()), slog.String("failure", string(failure)),
				slog.Int("attempt", attempt), slog.Duration("backoff", back), slog.Any("err", err))
			if serr := s.sleep(ctx, back); serr != nil {
				return tr, abandon(serr)
			}
			continue

		default:
			s.failed(h, agent, f, tr, dur, failure, err, attempt, false)
			return tr, err
		}
	}
}

// foreignCartridge reports whether c is not the run's active cartridge, so
// the model the builders took from the active one does not apply to it.
func (s *Supervisor) foreignCartridge(c cartridge.Cartridge) bool {
	return c != nil && s.opts.ActiveCartridge != "" && c.Name() != s.opts.ActiveCartridge
}

// stamp fills in the fields the supervisor owns on a built agent run, so a
// builder cannot fire in the wrong directory, without the shim, or without
// a timeout.
func (s *Supervisor) stamp(run *agents.AgentRun, f AgentFiring, agent domain.Agent, prep supPrep, h SupervisorHost) {
	// The per-role model and the planner's web access are stamped here,
	// where every firing passes, rather than in each role's builder: the
	// product owner is fired by the planner and by the feedback loop's
	// arbitration, and both must use the model configured for the role.
	if m := s.opts.RoleModels[agent.Role]; m != "" {
		run.Spec.Model = m
	} else if s.foreignCartridge(prep.cartridge) {
		run.Spec.Model = ""
	}
	if s.opts.Research && agent.Role == domain.RolePO && f.Task == events.TaskPlan {
		run.Spec.Web = true
	}
	run.Cartridge = prep.cartridge
	run.Agent = agent
	run.Task = f.Task
	run.Run = h.ID()
	if run.RunDir == "" {
		// P5-13: the firing path files a copy of the prompt it sent under
		// <run-dir>/prompts/<agent-id>.md. Stamping it here covers every
		// firing in one place, the way the workdir, the environment, the
		// shim and the timeout are stamped.
		run.RunDir = h.Dirs().Dir
	}
	if prep.workdir != "" {
		// A firing with no worktree of its own (the planning product
		// owner, P6-05; the arbitrating product owner, P6-04) keeps the
		// directory its builder chose — usually the repository itself.
		run.Spec.Workdir = prep.workdir
	}
	if prep.enforcement.Mode == agents.EnforceCopiedWorktree {
		// P6-30: the session is in a copy with no `.git`, so it is told so
		// rather than left to discover it by running git. The supervisor
		// does it here, not the role builders, because the degrade is
		// decided per attempt (by the cartridge the selector handed out)
		// and a builder cannot know which attempt it is building for.
		run.Spec.Prompt = noteCopiedWorktree(run.Spec.Prompt)
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
	if s.unrecord(h, f, done) {
		s.forget(agent.ID)
	}
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
		if s.unrecord(h, f, done) {
			s.forget(agent.ID)
		}
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
// state to the caller that owns the record. It reports whether the record
// is now clear: only then may the supervisor forget the agent, because an
// agent still listed as running is one [Supervisor.reap] must recognise as
// its own at the stop.
func (s *Supervisor) unrecord(h SupervisorHost, f AgentFiring, a domain.Agent) bool {
	if f.Update != nil {
		f.Update(a)
		return true
	}
	if err := h.AgentStopped(a.ID); err != nil {
		h.Warn(SourceSupervisor, fmt.Sprintf("could not clear agent %s", a.ID), err)
		return false
	}
	return true
}

// supRetryable reports whether firing the same job again can plausibly
// succeed. An *agents.AgentError knows; anything else is judged by its
// cartridge kind, with an unusable reply never retried (the schema retry
// inside RunAgent is already spent).
func supRetryable(err error, failure events.Failure, kind cartridge.Kind) bool {
	var pe *supPostSessionError
	if errors.As(err, &pe) {
		// The session already succeeded; only the run's bookkeeping after
		// it failed. Firing again would repeat the whole LLM session.
		return false
	}
	var ae *agents.AgentError
	if errors.As(err, &ae) {
		return ae.Retryable()
	}
	if failure == events.FailureBadOutput {
		return false
	}
	return kind.Retryable()
}

// supPostSessionError is what [AgentFiring.Result] returned: the agent
// session succeeded and its reply was decoded, and what failed afterwards
// is the run's own bookkeeping — a bead comment, a D-07 transition, a bd
// call. It is kept distinguishable from a session failure on purpose,
// because retrying it would fire the whole LLM session again and repeat
// the side effects the successful one already had (D-06).
type supPostSessionError struct{ err error }

// Error implements error.
func (e *supPostSessionError) Error() string { return ErrPostSession.Error() + ": " + e.err.Error() }

// Unwrap exposes the hook's own error, so a caller can still match what
// went wrong underneath (a beads error, say).
func (e *supPostSessionError) Unwrap() error { return e.err }

// Is makes errors.Is(err, [ErrPostSession]) true for every hook failure.
func (e *supPostSessionError) Is(target error) bool { return target == ErrPostSession }

// supFailureOf classifies a firing's error for events and [FireResult]. A
// post-session bookkeeping failure is the run's own: agents.FailureOf would
// call it a crash, because it is an ordinary error the cartridge never saw.
func supFailureOf(err error) events.Failure {
	var pe *supPostSessionError
	if errors.As(err, &pe) {
		return events.FailureInternal
	}
	return agents.FailureOf(err)
}

// supPending copies the agent a retry left recorded as running, so the
// terminal event a later abandonment publishes reports the agent as it was
// when it last ran.
func supPending(a domain.Agent) *domain.Agent {
	c := a
	return &c
}

// supAbandonFailure classifies the terminal failure of a firing given up
// between attempts: a stop or a dead context killed it, anything else keeps
// the failure the error itself carries.
func supAbandonFailure(err error) events.Failure {
	switch {
	case errors.Is(err, ErrSupervisorStopped),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return events.FailureKilled
	default:
		return supFailureOf(err)
	}
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
// the role (D-12), or — when that one has been retired for the run — any
// cartridge still alive, because a preference must not stop a run that
// still has credits somewhere (D-18).
//
// A selector error is the selector's verdict and is returned as it is: it
// carries P2-08's Detect diagnosis (not installed, not authenticated, the
// active cartridge broken too) and firing the very cartridge it refused
// would bypass that check and repeat the failure without ever naming its
// cause. The one exception is a run that has already retired a cartridge:
// D-18's re-route then looks for a live substitute anyway, and says with a
// Warning naming the selector's error that it is firing one the selector
// did not approve.
func (s *Supervisor) pick(ctx context.Context, role domain.Role) (cartridge.Cartridge, error) {
	if s.opts.Cartridges == nil {
		return nil, errors.New("orchestrator: the supervisor has no cartridge source")
	}
	names := s.opts.Cartridges.Names()
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: no cartridge is built for this run", ErrNoLiveCartridge)
	}
	chosen, selErr := s.opts.Cartridges.Select(ctx, role)
	switch {
	case selErr == nil && chosen != nil && !s.isDead(chosen.Name()):
		return chosen, nil
	case selErr != nil && !s.anyDead():
		// Nothing is retired, so there is nothing for D-18 to route
		// around: the selector's diagnosis is the honest answer.
		return nil, selErr
	}
	var reason string
	switch {
	case selErr != nil:
		reason = fmt.Sprintf("the cartridge selector refused %s (%v)", role, selErr)
	case chosen != nil:
		reason = fmt.Sprintf("cartridge %q is retired", chosen.Name())
	default:
		reason = fmt.Sprintf("the cartridge selector chose nothing for %s", role)
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
		if h := s.Host(); h != nil {
			h.Warn(SourceSupervisor, fmt.Sprintf("%s; %s fires %q instead", reason, role, n), selErr)
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

// anyDead reports whether any cartridge has been retired for this run.
func (s *Supervisor) anyDead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.dead) > 0
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

// track registers a live firing so the stop path can cancel it, and
// refuses once [Supervisor.StopAgents] has run: the stop flag and the
// registration are read and written under one lock, so a firing either
// appears in the snapshot the stop path takes of s.live — and is cancelled
// and waited for — or never starts its session at all. ok is false when it
// is refused.
//
// It is also where an agent joins s.started, the set [Supervisor.reap] uses
// to tell this supervisor's agents from another component's.
func (s *Supervisor) track(a domain.Agent, cancel context.CancelFunc) (e *supFiring, ok bool) {
	e = &supFiring{agent: a, cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, false
	}
	s.live[a.ID] = e
	s.started[a.ID] = true
	return e, true
}

// forget drops an agent from the set this supervisor has fired. A firing
// that ended terminally is already out of state.json, so [Supervisor.reap]
// has nothing left to recognise; keeping it would grow the map for the
// whole life of an autopilot run, which never ends by itself (D-18).
func (s *Supervisor) forget(id domain.AgentID) {
	s.mu.Lock()
	delete(s.started, id)
	s.mu.Unlock()
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
//
// "Nothing new" includes a firing that was still preparing when the stop
// landed — waiting out a rate-limit pause, installing a shim, copying a
// worktree: [Supervisor.track] refuses it before it starts a session, so no
// subprocess can appear behind a StopAgents that has already returned.
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
	// Registered before the copy starts, not after it succeeds: a copy
	// that fails half way has already made the directory, and only a
	// registered copy is reachable by dropCopy here and by the stop path's
	// removeCopies.
	s.mu.Lock()
	s.copyDirs[dst] = dst
	s.mu.Unlock()
	if err := s.copier.Copy(ctx, f.Worktree, dst); err != nil {
		s.dropCopy(dst)
		return p, fmt.Errorf("orchestrator: copying %s for %s: %w", f.Worktree, f.Agent.ID, err)
	}

	wrapped := &supCopiedCartridge{inner: c, root: dst, policy: policy}
	copied, err := wrapped.ToolPolicyEnforcement(policy)
	if err != nil {
		return p, err
	}
	p.workdir = dst
	p.cartridge = wrapped
	p.enforcement = copied
	p.discard = func() { s.dropCopy(dst) }
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
	// P9-22: the git guard shares that directory, so the one PATH entry the
	// firing gets carries both. An engineer runs read_write — Bash with no
	// deny list — in a linked worktree of the user's own repository, and
	// without the guard `git push origin HEAD:main`, `git checkout main` or
	// `git clean -fdx` from an agent would reach the user's branches. A
	// guard that cannot be installed fails the firing rather than letting an
	// unguarded session run.
	if _, err := agents.InstallGitGuardForWorktree(wt, sh.Dir); err != nil {
		return beads.Shim{}, fmt.Errorf("orchestrator: installing the git guard for %s: %w", wt, err)
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

// dropCopy deletes one throwaway worktree copy and forgets it. A copy that
// cannot be deleted stays registered, so the stop path's removeCopies tries
// again.
func (s *Supervisor) dropCopy(dst string) {
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
	switch name {
	// "..", like "." and the separator, names a directory that is not the
	// copy: filepath.Join(<run-dir>, "copies", "..") is the run directory
	// itself, which prepare would then delete along with state.json, the
	// event log and every worktree (D-23).
	case "", ".", "..", string(filepath.Separator):
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
		Detail: "no tool controls: the session runs in a throwaway copy of the worktree, discarded afterwards, " +
			"with no git in it (P6-20, P6-30)",
	}, nil
}

// noteCopiedWorktree appends [CopiedWorktreeNote] to a firing's prompt. It
// is idempotent: a retry builds the prompt again, and a builder that added
// the note itself (it can: [FiringContext.Enforcement] says the degrade is
// in force) is left alone.
func noteCopiedWorktree(prompt string) string {
	if strings.Contains(prompt, CopiedWorktreeNote) {
		return prompt
	}
	if strings.TrimSpace(prompt) == "" {
		// An empty prompt is refused by cartridge.RunSpec.Validate; the
		// note must not be what makes it pass.
		return prompt
	}
	return strings.TrimRight(prompt, "\n") + "\n\n" + CopiedWorktreeNote + "\n"
}

// CopyWorktree copies the tree at src to dst, which must not exist. It is
// the default [WorktreeCopier] of the P6-20 degrade: regular files (with
// their permission bits) and directories are copied and symlinks are
// recreated only when they stay inside the copy, so nothing outside dst can
// be written through the copy.
//
// The `.git` entry is deliberately not copied. In a git worktree it is a
// file pointing back into the real repository's administrative directory,
// so a copy that kept it would let the very writes the degrade exists to
// contain reach the real repository. The reviewed diff is in the agent's
// prompt, not in the copy's history.
//
// A symlink is the other way out of the copy, and the tree being reviewed
// is one an engineer agent wrote: a link with an absolute target, or a
// relative one that climbs out with "..", would point at the same place
// from inside the copy as it did from inside the real worktree. Such a link
// is dropped; an absolute link into src is rewritten to the matching path
// inside dst; a relative link that stays inside is recreated verbatim and
// therefore resolves inside the copy. [Supervisor] warns about every
// dropped link.
func CopyWorktree(ctx context.Context, src, dst string) error {
	_, err := copyWorktreeTree(ctx, src, dst)
	return err
}

// supDroppedLink is one symlink a copy refused to recreate, with the target
// that would have escaped.
type supDroppedLink struct {
	// Path is the link, relative to the copied tree's root.
	Path string
	// Target is what it pointed at, verbatim.
	Target string
}

// copyWorktreeTree is [CopyWorktree] and the links it dropped.
func copyWorktreeTree(ctx context.Context, src, dst string) ([]supDroppedLink, error) {
	src = filepath.Clean(src)
	var dropped []supDroppedLink
	info, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: copy worktree: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("orchestrator: copy worktree: %s is not a directory", src)
	}
	werr := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
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
			contained, ok := supContainedLink(src, dst, path, link)
			if !ok {
				dropped = append(dropped, supDroppedLink{Path: rel, Target: link})
				return nil
			}
			return os.Symlink(contained, target)
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
	return dropped, werr
}

// supContainedLink decides what a symlink at path (pointing at link) becomes
// in the copy of src at dst. A relative link that stays inside src is kept
// verbatim, because it then resolves inside the copy; an absolute link into
// src is rewritten to the matching path inside dst; anything else escapes
// the copy and is refused (ok is false), because a write through it would
// land in the tree the P6-20 degrade exists to protect.
func supContainedLink(src, dst, path, link string) (string, bool) {
	resolved := link
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(path), link)
	}
	rel, err := filepath.Rel(src, filepath.Clean(resolved))
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if !filepath.IsAbs(link) {
		return link, true
	}
	if rel == "." {
		return filepath.Clean(dst), true
	}
	return filepath.Join(dst, rel), true
}

// copyWorktree is the supervisor's default [WorktreeCopier]: [CopyWorktree]
// plus a Warning for every symlink the copy dropped for pointing out of the
// tree, so the run says why the copy differs from the worktree an agent is
// reviewing (P6-20).
func (s *Supervisor) copyWorktree(ctx context.Context, src, dst string) error {
	dropped, err := copyWorktreeTree(ctx, src, dst)
	if h := s.Host(); h != nil {
		for _, l := range dropped {
			h.Warn(SourceSupervisor, fmt.Sprintf(
				"the worktree copy left out the symlink %s -> %s: it points outside %s, and a contained session must not reach it (P6-20)",
				l.Path, l.Target, src), nil)
		}
	}
	return err
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
