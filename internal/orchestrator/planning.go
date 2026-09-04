package orchestrator

// Planning loop and stop conditions (P6-05), the policy for a plan the
// validator keeps rejecting (P6-19), and the config key that caps a plan
// (P1-16, `max_beads_per_plan`) as it reaches the product owner.
//
// The planner is not a second [Loop]. The scheduler (P6-02) is the run's
// loop, and this file plugs into its two hooks:
//
//   - [Planner.Demands] is a [WorkSource] that offers a [DemandPlan] when
//     the product owner should plan: once at the start of every run and,
//     in autopilot, whenever the ready queue is empty and nothing is
//     running (D-18).
//   - [Planner.Done] is the [StopCondition]. Brief mode finishes when every
//     bead of the run has settled; autopilot finishes only when the user
//     stops it or every configured cartridge is out of credits — there is
//     no bead cap and no runtime cap (D-18).
//
// [Planner.Fire] is the [Firer] for those plan demands: it fires the
// product owner through a [PlanRunner], turns the plan into beads with
// `bd create --graph` (D-22) and, when the validator rejects the plan a
// second time, drops it rather than letting a stubborn product owner spin
// (P6-19).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// SourcePlanning is the events.Warning/Error Source the planning loop uses.
const SourcePlanning = "planning"

// PlanShimDirName is the directory under <run-dir>/bin that holds the
// read-only bd shim of the planning product owner (D-21, D-23). The
// planning agent has no worktree of its own — it reads the repository —
// so it does not share the per-bead shim directories P6-17 installs.
const PlanShimDirName = "po-plan"

// Planning defaults.
const (
	// DefaultPlanRejectionLimit is how many planning firings in a row may
	// come back with a plan the validator rejected before the planner gives
	// up on that attempt (P6-19). agents.ParsePlanOutput already makes
	// every rule failure retryable inside one firing (P5-08 retries once),
	// so the first rejection that reaches the planner is already the second
	// plan the product owner got wrong.
	DefaultPlanRejectionLimit = 2
	// DefaultMaxPlanBackoff caps the wait an autopilot planner imposes on
	// itself after it gave up on a round of planning. Autopilot never stops
	// (D-18), so it must always come back — just not in a hot loop.
	DefaultMaxPlanBackoff = time.Hour
	// DefaultSettleInterval is the least time between the bead listings the
	// brief-mode stop condition makes. The scheduler ticks once a second;
	// asking bd that often for a listing nothing changed would be rude.
	DefaultSettleInterval = 2 * time.Second
)

// Planning errors.
var (
	// ErrNoPlanConfig means [NewPlanner] was given no run configuration, so
	// it cannot know the mode, the brief or the plan cap.
	ErrNoPlanConfig = errors.New("orchestrator: planning: no run configuration")
	// ErrNoPlanRunner means the planner has nothing to fire the product
	// owner with; see [SupervisorPlanRunner].
	ErrNoPlanRunner = errors.New("orchestrator: planning: no plan runner")
	// ErrNoPlanApplier means the planner has no way to turn a plan into
	// beads; *beads.Client is the production one (P3-10).
	ErrNoPlanApplier = errors.New("orchestrator: planning: no plan applier")
	// ErrNotAPlanDemand is returned by [Planner.Fire] for a demand of any
	// other kind: the planner fires product owners, nothing else.
	ErrNotAPlanDemand = errors.New("orchestrator: planning: not a plan demand")
	// ErrPlanningGaveUp ends a brief-mode run whose product owner could not
	// produce a usable plan (P6-19). Autopilot never fails this way: it
	// backs off and tries again (D-18).
	ErrPlanningGaveUp = errors.New("orchestrator: planning: the product owner could not produce a usable plan")
)

// CreditsWatcher reports whether every configured cartridge is out of
// credits, which is the one way besides a user stop that an autopilot run
// ends (D-18). *Supervisor satisfies it (P6-07).
type CreditsWatcher interface {
	// Exhausted is true once no cartridge can fire another agent.
	Exhausted() bool
}

// PlanRunner fires one planning product owner and returns the plan it
// produced. It is the seam between the planning loop and the agent
// supervisor: [SupervisorPlanRunner] is the production implementation and
// carries the per-agent timeout, the crash retries, the rate-limit
// back-off and the credits policy with it (P6-07); a test substitutes a
// function.
//
// The error of a rejected plan must keep wrapping [agents.ErrPlanRejected]
// so the planner can apply the P6-19 policy to it rather than treating it
// as a crash.
type PlanRunner interface {
	// RunPlan runs the firing's agent with req and decodes its plan.
	RunPlan(ctx context.Context, f Firing, req agents.PlanRequest) (domain.Plan, agents.Transcript, error)
}

// PlanRunnerFunc adapts a function to [PlanRunner].
type PlanRunnerFunc func(ctx context.Context, f Firing, req agents.PlanRequest) (domain.Plan, agents.Transcript, error)

// RunPlan implements [PlanRunner].
func (fn PlanRunnerFunc) RunPlan(ctx context.Context, f Firing, req agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
	return fn(ctx, f, req)
}

// SupervisorPlanRunner fires the planning product owner through the agent
// supervisor, so a planning firing gets the same timeout, crash retries,
// rate-limit back-off and credits handling as every other agent (P6-07).
//
// The firing has no worktree: the planning product owner runs read_only in
// the repository itself (P5-02), and the read-only bd shim it is promised
// comes from [PlannerOptions] rather than from the supervisor's per-worktree
// installer, whose layout rule (`<run-dir>/wt/<bead-id>`) the repository
// does not satisfy.
//
// A worktree-less firing keeps the workdir its builder chose (the
// repository, from [PlanAgentRun]): the supervisor only stamps a workdir
// when the firing names a worktree. Firing the plan through the supervisor
// is also what puts the planning product owner in the agent counts, so it
// shows up in `sc status` and on the TUI's Agents tab like every other
// agent.
func SupervisorPlanRunner(s *Supervisor) PlanRunner {
	return PlanRunnerFunc(func(ctx context.Context, f Firing, req agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
		if s == nil {
			return domain.Plan{}, agents.Transcript{}, ErrNoPlanRunner
		}
		return FireAgent[domain.Plan](ctx, s, AgentFiring{
			Agent:   f.Agent,
			Task:    events.TaskPlan,
			Update:  f.Update,
			NoLease: true,
			Build: func(_ context.Context, fc FiringContext) (agents.AgentRun, error) {
				return PlanAgentRun(fc, f.Run, req)
			},
		})
	})
}

// PlanAgentRun assembles the agent run of one planning attempt: the P5-02
// planning prompt, the read_only tool policy, and the plan contract with
// [agents.PlanCheck] so a plan that breaks a planning rule is retried
// inside the firing before it ever reaches the P6-19 policy.
//
// It is exported because the integrator may want the planning firing to go
// through the supervisor's own [FiringPlanner] table instead of
// [Planner.Fire]; note that only [Planner.Fire] observes the outcome, so a
// planner built that way never learns that a plan was rejected.
func PlanAgentRun(fc FiringContext, r *Run, req agents.PlanRequest) (agents.AgentRun, error) {
	req.AgentID = fc.Agent.ID
	if req.Timeout <= 0 {
		req.Timeout = fc.Timeout
	}
	req.Env = mergeEnv(req.Env, fc.Env)
	spec, p, err := agents.PlanRunSpec(req)
	if err != nil && !errors.Is(err, agents.ErrPromptOverBudget) {
		return agents.AgentRun{}, err
	}
	if err != nil && r != nil {
		r.Warn(SourcePlanning, "the planning prompt is over its token budget; firing it anyway", err)
	}
	// The planning product owner reads the repository; it has no worktree
	// of its own (see the note on SupervisorPlanRunner).
	spec.Workdir = req.Dir()
	if r != nil {
		if werr := p.Write(r.Dir()); werr != nil {
			r.Warn(SourcePlanning, "could not file the debug copy of the planning prompt", werr)
		}
	}
	return agents.AgentRun{
		Cartridge: fc.Cartridge,
		Spec:      spec,
		Message:   domain.MessagePlan,
		Check:     agents.PlanCheck(req),
		Agent:     fc.Agent,
		Task:      events.TaskPlan,
		Run:       fc.Run,
	}, nil
}

// PlanShimInstaller writes the read-only bd shim the planning product owner
// runs with and returns it (D-21). dir is the directory the shim goes in
// (never inside the target repository, D-23) and repo is the repository the
// self-check runs against.
type PlanShimInstaller func(ctx context.Context, dir, repo string, opts beads.ShimOptions) (beads.Shim, error)

// InstallPlanShim is the default [PlanShimInstaller]: it writes the shim and
// verifies it against the repository unless [beads.ShimOptions.SkipVerify]
// is set.
func InstallPlanShim(ctx context.Context, dir, repo string, opts beads.ShimOptions) (beads.Shim, error) {
	s, err := beads.WriteShim(dir, beads.ShimOptions{Real: opts.Real})
	if err != nil {
		return beads.Shim{}, err
	}
	if opts.SkipVerify {
		return s, nil
	}
	if err := s.Verify(ctx, repo); err != nil {
		return beads.Shim{}, err
	}
	return s, nil
}

// PlannerOptions configures [NewPlanner]. Config, Runner, Applier and Beads
// are what a real run needs; everything else has a default.
type PlannerOptions struct {
	// Config is the run's configuration: the mode, the brief, the
	// repository, the cartridge model, `idle_replan_interval` and
	// `max_beads_per_plan` all come from it. Required.
	Config *config.RunConfig
	// Runner fires the product owner; see [SupervisorPlanRunner].
	Runner PlanRunner
	// Applier turns a validated plan into beads (P3-10); *beads.Client.
	Applier agents.PlanApplier
	// Beads lists the run's beads: the open ones the plan must not
	// duplicate, and the ones brief mode waits to settle.
	Beads BeadSource
	// Credits reports every cartridge being out of credits (D-18); nil
	// means the run never ends that way.
	Credits CreditsWatcher
	// Status is the scheduler's live status, which is how autopilot knows
	// the ready queue is empty and no engineer is running. Pass
	// (*Scheduler).Status. Nil falls back to the snapshot the stop
	// condition last saw.
	Status func() SchedulerStatus
	// Repo is a pre-scanned repository context. When Root is empty the
	// planner scans the repository itself before every planning firing, so
	// the product owner sees the tree as it is now.
	Repo prompt.RepoContext
	// Scan tunes that scan (tree depth, README bytes).
	Scan prompt.ScanOptions
	// Rules and Vibes are the constraint and style documents. Nil means
	// "load them from the effective configuration", once, on first use.
	Rules []prompt.Doc
	Vibes []prompt.Doc
	// Shim installs the read-only bd shim; nil means [InstallPlanShim].
	Shim PlanShimInstaller
	// ShimOptions are passed to it (a pinned real bd, a skipped self-check).
	ShimOptions beads.ShimOptions
	// NoShim fires the product owner without a read-only bd shim. Only for
	// a caller that has no bd at all; a run always installs one.
	NoShim bool
	// Now is the clock; nil means time.Now. Tests pass a fake one and
	// nothing here reads the wall clock.
	Now func() time.Time
	// IdleReplanInterval overrides the configured `idle_replan_interval`.
	IdleReplanInterval time.Duration
	// RejectionLimit is the P6-19 cap; zero means
	// [DefaultPlanRejectionLimit], negative means "never give up".
	RejectionLimit int
	// MaxBackoff caps the autopilot cool-off; zero means
	// [DefaultMaxPlanBackoff].
	MaxBackoff time.Duration
	// SettleInterval is the least time between the bead listings the
	// brief-mode stop condition makes; zero means [DefaultSettleInterval].
	SettleInterval time.Duration
	// MaxBeads overrides `max_beads_per_plan` (P1-16).
	MaxBeads int
	// Logger receives one line per planning decision; nil means the run's,
	// else slog.Default().
	Logger *slog.Logger
}

// PlanningStatus is a snapshot of the planning loop, for `sc status` and
// the TUI.
type PlanningStatus struct {
	// Mode is the run mode the planner enforces.
	Mode domain.RunMode
	// Attempts is how many planning firings have been made.
	Attempts int
	// Plans is how many of them produced beads.
	Plans int
	// Beads is how many beads those plans created.
	Beads int
	// Empty counts the plans that proposed nothing ("nothing worth adding
	// right now", D-18).
	Empty int
	// Rejections is how many plans in a row the validator refused.
	Rejections int
	// GaveUp counts the times the planner dropped a round of planning under
	// the P6-19 policy.
	GaveUp int
	// Running is true while a product owner is planning.
	Running bool
	// Finished is true when the planner will not fire again.
	Finished bool
	// ResumedPlan is true when the plan counted above came back with a
	// resumed run rather than from a product owner this process fired
	// (P6-11): the run's epic was already in state.json.
	ResumedPlan bool
	// NextAt is the earliest time the planner will fire again; zero means
	// "as soon as the loop asks".
	NextAt time.Time
	// LastError is the last planning failure, if any.
	LastError string
}

// Planner is the run's planning loop: a [WorkSource] that asks for a
// product owner when one is wanted, a [Firer] that runs it, and the
// [StopCondition] that decides when the run is over. Obtain one with
// [NewPlanner]; all methods are safe for concurrent use.
type Planner struct {
	opts PlannerOptions
	cfg  *config.RunConfig
	log  *slog.Logger

	idle     time.Duration
	maxBeads int
	limit    int
	maxBack  time.Duration
	settle   time.Duration

	mu          sync.Mutex
	run         *Run
	attempts    int
	plans       int
	beads       int
	empty       int
	rejections  int
	gaveUp      int
	running     bool
	finished    bool
	resumedPlan bool
	failed      bool
	nextAt      time.Time
	lastErr     error
	lastStatus  SchedulerStatus
	lastSettle  time.Time
	settled     bool
	settleSum   string

	docsOnce sync.Once
	rules    []prompt.Doc
	vibes    []prompt.Doc
	docsErr  error

	shimOnce sync.Once
	shim     beads.Shim
	shimErr  error
}

// Compile-time proof that a planner is all three hooks the scheduler needs.
var (
	_ WorkSource    = (*Planner)(nil)
	_ StopCondition = (*Planner)(nil)
	_ Firer         = (*Planner)(nil)
)

// NewPlanner builds the planning loop. It reads nothing and fires nothing
// until the scheduler asks it to.
func NewPlanner(opts PlannerOptions) (*Planner, error) {
	if opts.Config == nil {
		return nil, ErrNoPlanConfig
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Shim == nil {
		opts.Shim = InstallPlanShim
	}
	p := &Planner{
		opts:     opts,
		cfg:      opts.Config,
		log:      opts.Logger,
		idle:     opts.IdleReplanInterval,
		maxBeads: opts.MaxBeads,
		limit:    opts.RejectionLimit,
		maxBack:  opts.MaxBackoff,
		settle:   opts.SettleInterval,
		rules:    opts.Rules,
		vibes:    opts.Vibes,
	}
	if p.idle <= 0 {
		p.idle = config.DefaultIdleReplanInterval
		if eff := p.cfg.Effective; eff != nil && eff.IdleReplanInterval.Duration() > 0 {
			p.idle = eff.IdleReplanInterval.Duration()
		}
	}
	if p.maxBeads <= 0 {
		p.maxBeads = config.DefaultMaxBeadsPerPlan
		if eff := p.cfg.Effective; eff != nil && eff.MaxBeadsPerPlan > 0 {
			p.maxBeads = eff.MaxBeadsPerPlan
		}
	}
	if p.limit == 0 {
		p.limit = DefaultPlanRejectionLimit
	}
	if p.maxBack <= 0 {
		p.maxBack = DefaultMaxPlanBackoff
	}
	if p.settle <= 0 {
		p.settle = DefaultSettleInterval
	}
	if opts.Rules != nil || opts.Vibes != nil {
		// The caller supplied them; never go to disk.
		p.docsOnce.Do(func() {})
	}
	return p, nil
}

// Attach gives the planner the run it belongs to, for events, warnings and
// the run directory. Production wiring builds the planner before the run
// (the scheduler needs it, and the run needs the scheduler), so this is
// called afterwards; [Planner.Fire] and [Planner.Done] attach the run they
// are handed if nothing else did.
//
// Attaching a resumed run whose plan already landed — its epic came back
// from state.json (D-22, P6-11) — counts that plan as made, so a brief run
// resumed halfway through does not plan its brief a second time and end up
// with two epics' worth of beads. An autopilot run still plans again when
// the ready queue empties, which is what D-18 asks for.
func (p *Planner) Attach(r *Run) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r == nil {
		return
	}
	p.run = r
	if p.cfg.Resume && p.attempts == 0 && r.Epic() != "" {
		p.attempts, p.plans, p.resumedPlan = 1, 1, true
	}
}

// Mode is the run mode the planner enforces.
func (p *Planner) Mode() domain.RunMode { return p.cfg.Run.Mode }

// MaxBeads is the effective `max_beads_per_plan` (P1-16).
func (p *Planner) MaxBeads() int { return p.maxBeads }

// IdleReplanInterval is the effective `idle_replan_interval` (D-18).
func (p *Planner) IdleReplanInterval() time.Duration { return p.idle }

// Status snapshots the planning loop.
func (p *Planner) Status() PlanningStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := PlanningStatus{
		Mode: p.cfg.Run.Mode, Attempts: p.attempts, Plans: p.plans, Beads: p.beads,
		Empty: p.empty, Rejections: p.rejections, GaveUp: p.gaveUp,
		Running: p.running, Finished: p.finished, NextAt: p.nextAt,
		ResumedPlan: p.resumedPlan,
	}
	if p.lastErr != nil {
		st.LastError = p.lastErr.Error()
	}
	return st
}

// Demands implements [WorkSource]: it offers the run's planning firing when
// one is wanted.
//
// Both modes plan once at the start. Brief mode never plans again (its
// scope is the human's -m text). Autopilot plans again whenever the ready
// queue is empty and nothing is running, and, after a plan that proposed
// nothing, not before `idle_replan_interval` has passed (D-18).
func (p *Planner) Demands(_ context.Context) ([]Demand, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return nil, nil
	}
	if p.running {
		// Still outstanding. Re-offering it is what a stateless source
		// does; the scheduler recognises the key and does not fire twice.
		return []Demand{planDemand()}, nil
	}
	now := p.opts.Now()
	if !p.nextAt.IsZero() && now.Before(p.nextAt) {
		return nil, nil
	}
	if p.attempts == 0 {
		return []Demand{planDemand()}, nil // plan at the start of every run
	}
	if p.cfg.Run.Mode == domain.RunModeBrief {
		if p.plans > 0 {
			return nil, nil // brief mode plans once
		}
		return []Demand{planDemand()}, nil // ... and retries until it has one
	}
	st := p.statusLocked()
	if st.Live > 0 || !st.Idle {
		return nil, nil // there is still work in flight
	}
	return []Demand{planDemand()}, nil
}

// planDemand is the run's planning demand. Its key is stable ("plan|||0"),
// so the scheduler never runs two product owners at once and mints the
// agent ids po-plan-1, po-plan-2, … itself.
func planDemand() Demand {
	return Demand{Kind: DemandPlan, Task: events.TaskPlan, Claim: ClaimNone}
}

// statusLocked returns the scheduler status, from the hook when there is
// one and from the last stop-condition call otherwise.
func (p *Planner) statusLocked() SchedulerStatus {
	if p.opts.Status != nil {
		return p.opts.Status()
	}
	return p.lastStatus
}

// Done implements [StopCondition].
//
// Every configured cartridge being out of credits ends any run (D-18).
// After that the modes part company: a brief run finishes once its beads
// have all settled — closed, needs_human, failed, or a bd status the cannon
// does not drive — and an autopilot run never finishes on its own. A stop,
// an interrupt or `sc stop` cancels the scheduler's context, which is how
// autopilot ends.
func (p *Planner) Done(ctx context.Context, r *Run, st SchedulerStatus) (Outcome, bool, error) {
	p.mu.Lock()
	p.lastStatus = st
	if r != nil && p.run == nil {
		p.run = r
	}
	failed, gaveUp, plans, attempts := p.failed, p.gaveUp, p.plans, p.attempts
	p.mu.Unlock()

	if p.opts.Credits != nil && p.opts.Credits.Exhausted() {
		return Outcome{
			Reason:  events.FinishCreditsExhausted,
			Summary: p.summary("every cartridge is out of credits"),
		}, true, nil
	}
	if failed {
		return Outcome{
			Reason:  events.FinishFailed,
			Summary: p.summary(fmt.Sprintf("the product owner could not produce a usable plan in %s", planCount(attempts, "attempt"))),
		}, true, fmt.Errorf("%w after %s (%s rejected)",
			ErrPlanningGaveUp, planCount(attempts, "attempt"), planCount(gaveUp, "round"))
	}
	if p.cfg.Run.Mode != domain.RunModeBrief {
		return Outcome{}, false, nil // autopilot never stops on its own (D-18)
	}
	if plans == 0 || st.Live > 0 || !st.Idle {
		return Outcome{}, false, nil
	}
	settled, summary, err := p.beadsSettled(ctx)
	if err != nil {
		p.warn("could not tell whether the run's beads have settled", err)
		return Outcome{}, false, nil
	}
	if !settled {
		return Outcome{}, false, nil
	}
	return Outcome{Reason: events.FinishCompleted, Summary: summary}, true, nil
}

// beadsSettled reports whether every bead of the run needs nothing more
// from the cannon, with a one-line account of them. The listing is
// debounced: the scheduler consults the stop condition on every tick.
func (p *Planner) beadsSettled(ctx context.Context) (bool, string, error) {
	if p.opts.Beads == nil {
		return false, "", nil
	}
	now := p.opts.Now()
	p.mu.Lock()
	if !p.lastSettle.IsZero() && now.Sub(p.lastSettle) < p.settle {
		settled, summary := p.settled, p.settleSum
		p.mu.Unlock()
		return settled, summary, nil
	}
	p.mu.Unlock()

	list, err := p.opts.Beads(ctx)
	if err != nil {
		return false, "", err
	}
	settled, total, summary := planSettlement(list)
	p.mu.Lock()
	created := p.beads
	p.mu.Unlock()
	if total == 0 && created > 0 {
		// The plan created beads and the listing has none: bd has not
		// caught up (or the listing is scoped wrong). Never finish a run on
		// that, or a brief run would end the moment it planned.
		settled = false
		summary = "waiting for the run's beads to appear in bd"
	}

	p.mu.Lock()
	p.lastSettle, p.settled, p.settleSum = now, settled, summary
	p.mu.Unlock()
	return settled, summary, nil
}

// planSettlement decides whether every work bead has settled, counts the
// work beads and renders the finish summary.
func planSettlement(list []domain.Bead) (settledAll bool, total int, summary string) {
	counts := map[domain.BeadState]int{}
	settled := 0
	for _, b := range list {
		if planInfrastructure(b) {
			continue
		}
		total++
		counts[b.State]++
		if BeadSettled(b) {
			settled++
		}
	}
	states := make([]string, 0, len(counts))
	for s := range counts {
		states = append(states, s.String())
	}
	sort.Strings(states)
	parts := make([]string, len(states))
	for i, s := range states {
		parts[i] = fmt.Sprintf("%d %s", counts[domain.BeadState(s)], s)
	}
	summary = planCount(total, "bead")
	if len(parts) > 0 {
		summary += " (" + strings.Join(parts, ", ") + ")"
	}
	if total == 0 {
		summary = "no beads"
	}
	return settled == total, total, summary
}

// BeadSettled reports whether the run needs nothing more from a bead: it is
// closed, parked for a human, failed, or in a bd status the cannon never
// drives (blocked, hooked, deferred, pinned — D-07). An escalated bead is
// not settled: the product owner still owes it a ruling (D-08).
func BeadSettled(b domain.Bead) bool {
	if planInfrastructure(b) {
		return true
	}
	return BeadStateSettled(b.State)
}

// planInfrastructure reports whether a bead is bookkeeping rather than work:
// the run epic (D-22) and the beads bd mints itself (D-20 gates, set-state
// events).
func planInfrastructure(b domain.Bead) bool {
	switch b.Type {
	case domain.BeadTypeEpic, domain.BeadTypeGate, domain.BeadTypeEvent:
		return true
	default:
		return false
	}
}

// summary renders the run summary the outcome carries.
func (p *Planner) summary(reason string) string {
	p.mu.Lock()
	beadCount, plans, sum := p.beads, p.plans, p.settleSum
	p.mu.Unlock()
	parts := []string{reason}
	if plans > 0 {
		parts = append(parts, fmt.Sprintf("%s from %s", planCount(beadCount, "bead"), planCount(plans, "plan")))
	}
	if sum != "" {
		parts = append(parts, sum)
	}
	return strings.Join(parts, "; ")
}

// planCount renders "1 bead" / "3 beads".
func planCount(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// Fire implements [Firer] for the planning demand: it assembles the
// planning request, runs the product owner through the [PlanRunner], turns
// the plan into beads and records the outcome so [Planner.Demands] knows
// whether to ask again.
//
// A demand of any other kind is refused with [ErrNotAPlanDemand]; see
// [PlanRouter] for the one-line split the integrator wires.
func (p *Planner) Fire(ctx context.Context, f Firing) (FireResult, error) {
	if f.Demand.Kind != DemandPlan {
		return FireResult{}, fmt.Errorf("%w: %s (agent %s)", ErrNotAPlanDemand, f.Demand.Kind, f.Agent.ID)
	}
	p.mu.Lock()
	p.attempts++
	p.running = true
	if f.Run != nil && p.run == nil {
		p.run = f.Run
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
	}()

	created, err := p.fire(ctx, f)
	p.record(f, created, err)

	res := FireResult{Produced: domain.MessagePlan}
	if errors.Is(err, agents.ErrEmptyPlan) {
		// "Nothing worth adding right now" is a real answer in autopilot
		// (D-18), not a failed agent: the run idles instead of publishing
		// an events.AgentFailed nobody should act on.
		return res, nil
	}
	if err != nil {
		res.Failure = agents.FailureOf(err)
		return res, err
	}
	return res, nil
}

// fire does the work of one planning firing and returns the ids it created.
func (p *Planner) fire(ctx context.Context, f Firing) (map[string]domain.BeadID, error) {
	if p.opts.Runner == nil {
		return nil, ErrNoPlanRunner
	}
	req, err := p.request(ctx, f)
	if err != nil {
		return nil, err
	}
	plan, _, err := p.opts.Runner.RunPlan(ctx, f, req)
	if err != nil {
		return nil, err
	}
	if p.opts.Applier == nil {
		return nil, ErrNoPlanApplier
	}
	res, err := agents.CreatePlanBeads(ctx, p.opts.Applier, req, plan)
	if errors.Is(err, agents.ErrEmptyPlan) {
		p.publishPlan(f, plan, nil, req.Epic)
		return nil, err
	}
	if err != nil {
		return res.IDs, err
	}
	if res.EpicCreated && res.Epic != "" && f.Run != nil {
		if serr := f.Run.SetEpic(res.Epic); serr != nil {
			p.warn("could not record the run epic "+string(res.Epic), serr)
		}
	}
	for _, w := range res.Warnings {
		p.warn("bd reported a warning while creating the plan: "+w, nil)
	}
	p.publishPlan(f, plan, res.IDs, res.Epic)
	return res.IDs, nil
}

// publishPlan announces a plan that was validated and applied (D-22).
func (p *Planner) publishPlan(f Firing, plan domain.Plan, created map[string]domain.BeadID, epic domain.BeadID) {
	r := p.host()
	if r == nil {
		return
	}
	if epic == "" {
		epic = r.Epic()
	}
	r.Publish(events.PlanProduced{
		Meta:    r.Meta(),
		Agent:   f.Agent.ID,
		Epic:    epic,
		Plan:    plan,
		Created: created,
	})
}

// record applies the outcome of one planning firing to the planner's state,
// including the P6-19 policy for a plan the validator rejected.
func (p *Planner) record(f Firing, created map[string]domain.BeadID, err error) {
	now := p.opts.Now()
	p.mu.Lock()
	p.lastErr = err
	switch {
	case err == nil:
		p.plans++
		p.beads += len(created)
		p.rejections = 0
		p.nextAt = time.Time{}
		if p.cfg.Run.Mode == domain.RunModeBrief {
			p.finished = true
		}
	case errors.Is(err, agents.ErrEmptyPlan):
		// Autopilot's "nothing worth adding right now" (D-18): a real
		// answer, so it counts as a plan and the run idles.
		p.plans++
		p.empty++
		p.rejections = 0
		p.nextAt = now.Add(p.idle)
		if p.cfg.Run.Mode == domain.RunModeBrief {
			p.finished = true
		}
	case errors.Is(err, agents.ErrPlanRejected):
		p.rejections++
	default:
		// A crash, a timeout, an unreadable rules file: not the plan's
		// fault, so it does not count towards the P6-19 cap, but the loop
		// still waits rather than firing again at once.
		p.nextAt = now.Add(p.idle)
	}
	rejections, giveUp := p.rejections, false
	if p.limit > 0 && p.rejections >= p.limit {
		giveUp = true
		p.rejections = 0
		p.gaveUp++
		if p.cfg.Run.Mode == domain.RunModeBrief {
			p.finished, p.failed = true, true
		} else {
			p.nextAt = now.Add(planBackoff(p.idle, p.gaveUp, p.maxBack))
		}
	}
	mode, attempts, nextAt := p.cfg.Run.Mode, p.attempts, p.nextAt
	p.mu.Unlock()

	switch {
	case err == nil:
		p.logger().Info("plan applied",
			slog.String("agent", f.Agent.ID.String()), slog.Int("beads", len(created)),
			slog.Int("attempt", attempts))
	case errors.Is(err, agents.ErrEmptyPlan):
		p.logger().Info("the product owner proposed nothing; idling",
			slog.String("agent", f.Agent.ID.String()), slog.Duration("replan_in", p.idle))
	case errors.Is(err, agents.ErrPlanRejected):
		p.warn(fmt.Sprintf("the product owner's plan broke a planning rule and was dropped (%s in a row)",
			planCount(rejections, "rejection")), err)
	default:
		p.warn("the planning product owner failed", err)
	}
	if !giveUp {
		return
	}
	// P6-19: the validator has now rejected this product owner's plan twice
	// in a row, once inside the firing (P5-08) and once here. Dropping the
	// plan is the whole policy: nothing was created, so nothing has to be
	// undone.
	if mode == domain.RunModeBrief {
		p.warn(fmt.Sprintf("giving up on planning after %s; the brief could not be turned into beads",
			planCount(attempts, "attempt")), err)
		return
	}
	p.warn(fmt.Sprintf("giving up on this round of planning; the next one is at %s",
		nextAt.UTC().Format(time.RFC3339)), err)
}

// planBackoff is the autopilot cool-off after the planner gave up on a
// round: the idle interval doubled once per consecutive give-up, capped.
// Autopilot never stops (D-18), so the wait always ends.
func planBackoff(idle time.Duration, gaveUp int, maxBackoff time.Duration) time.Duration {
	if idle <= 0 {
		idle = config.DefaultIdleReplanInterval
	}
	d := idle
	for i := 1; i < gaveUp && d < maxBackoff; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// request assembles the planning request for one firing: the repository as
// it is now, the rules and vibes, the beads the plan must not duplicate,
// the run epic, the plan cap (P1-16) and the read-only bd shim (D-21).
func (p *Planner) request(ctx context.Context, f Firing) (agents.PlanRequest, error) {
	repo, err := p.repoContext()
	if err != nil {
		return agents.PlanRequest{}, err
	}
	rules, vibes, err := p.docs()
	if err != nil {
		return agents.PlanRequest{}, err
	}
	open, err := p.openBeads(ctx)
	if err != nil {
		return agents.PlanRequest{}, err
	}
	req := agents.PlanRequest{
		Run:       p.cfg.Run.ID,
		AgentID:   f.Agent.ID,
		Mode:      p.cfg.Run.Mode,
		Brief:     p.cfg.Run.Brief,
		Repo:      repo,
		Rules:     rules,
		Vibes:     vibes,
		OpenBeads: open,
		MaxBeads:  p.maxBeads,
		Notes:     p.notes(open),
		Model:     p.cfg.Cartridge.Model,
	}
	if eff := p.cfg.Effective; eff != nil && eff.Limits.AgentTimeout.Duration() > 0 {
		req.Timeout = eff.Limits.AgentTimeout.Duration()
	}
	if r := p.host(); r != nil {
		req.Epic = r.Epic()
	}
	if !p.opts.NoShim {
		shim, err := p.planShim(ctx)
		if err != nil {
			return agents.PlanRequest{}, err
		}
		req.UseShim(shim)
	}
	if err := req.Validate(); err != nil {
		return agents.PlanRequest{}, err
	}
	return req, nil
}

// notes are the orchestrator's lines in the planning prompt.
func (p *Planner) notes(open []domain.Bead) []string {
	if p.cfg.Run.Mode == domain.RunModeBrief {
		return nil
	}
	p.mu.Lock()
	first := p.plans == 0
	p.mu.Unlock()
	notes := []string{
		"You may deprioritise open beads you proposed in an earlier plan if better work has appeared; " +
			"say so in the plan's brief. You may not delete them.",
	}
	if !first {
		notes = append(notes,
			"The ready queue is empty and no engineer is running, so this run has nothing left to do: "+
				"propose the follow-up work that is most worth doing next.")
	}
	if len(open) == 0 && !first {
		notes = append(notes, "Nothing this run planned earlier is still open.")
	}
	return notes
}

// repoContext returns the repository context for a planning prompt: the
// pre-scanned one when the caller supplied it, else a fresh scan, so the
// product owner sees the tree as it is now.
func (p *Planner) repoContext() (prompt.RepoContext, error) {
	if p.opts.Repo.Root != "" {
		return p.opts.Repo, nil
	}
	opts := p.opts.Scan
	if opts.CheckCommand == "" && p.cfg.Effective != nil {
		opts.CheckCommand = p.cfg.Effective.CheckCommand
	}
	rc, err := prompt.ScanRepo(p.cfg.Run.RepoPath, opts)
	if err != nil {
		return prompt.RepoContext{}, fmt.Errorf("orchestrator: planning: %w", err)
	}
	return rc, nil
}

// docs loads the rules and vibes once. A rules file that cannot be read is
// an error, not a warning: the rules are the user's hard constraints and a
// plan made without them is not the plan they asked for.
func (p *Planner) docs() ([]prompt.Doc, []prompt.Doc, error) {
	p.docsOnce.Do(func() {
		eff := p.cfg.Effective
		if eff == nil {
			return
		}
		rules, err := prompt.LoadDocs(eff.Rules)
		if err != nil {
			p.docsErr = fmt.Errorf("orchestrator: planning: rules: %w", err)
			return
		}
		vibes, err := prompt.LoadDocs(eff.Vibes)
		if err != nil {
			p.docsErr = fmt.Errorf("orchestrator: planning: vibes: %w", err)
			return
		}
		p.rules, p.vibes = rules, vibes
	})
	if p.docsErr != nil {
		return nil, nil, p.docsErr
	}
	return p.rules, p.vibes, nil
}

// openBeads lists the beads the plan must not duplicate: everything of this
// run that is not closed and is not bookkeeping.
func (p *Planner) openBeads(ctx context.Context) ([]domain.Bead, error) {
	if p.opts.Beads == nil {
		return nil, nil
	}
	list, err := p.opts.Beads(ctx)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: planning: listing the run's beads: %w", err)
	}
	out := make([]domain.Bead, 0, len(list))
	for _, b := range list {
		if planInfrastructure(b) || b.State == domain.BeadStateClosed {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// planShim installs the planning agent's read-only bd shim once, under
// <run-dir>/bin/po-plan (D-21, D-23).
func (p *Planner) planShim(ctx context.Context) (beads.Shim, error) {
	p.shimOnce.Do(func() {
		r := p.host()
		if r == nil {
			p.shimErr = errors.New("orchestrator: planning: no run directory for the read-only bd shim")
			return
		}
		dir := filepath.Join(r.Dirs().Shims, PlanShimDirName)
		p.shim, p.shimErr = p.opts.Shim(ctx, dir, p.cfg.Run.RepoPath, p.opts.ShimOptions)
		if p.shimErr != nil {
			p.shimErr = fmt.Errorf("orchestrator: planning: installing the read-only bd shim: %w", p.shimErr)
		}
	})
	return p.shim, p.shimErr
}

// host returns the run, if one has been attached.
func (p *Planner) host() *Run {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.run
}

// logger is the run's logger, the configured one, or the default.
func (p *Planner) logger() *slog.Logger {
	if p.opts.Logger != nil {
		return p.opts.Logger
	}
	if r := p.host(); r != nil {
		return r.Log()
	}
	return slog.Default()
}

// warn publishes a planning warning on the run, or logs it when the planner
// has no run yet.
func (p *Planner) warn(message string, err error) {
	if r := p.host(); r != nil {
		r.Warn(SourcePlanning, message, err)
		return
	}
	if err != nil {
		p.logger().Warn(message, slog.String("error", err.Error()), slog.String("source", SourcePlanning))
		return
	}
	p.logger().Warn(message, slog.String("source", SourcePlanning))
}

// PlanRouter returns a [Firer] that sends planning demands to the planner
// and everything else to rest (the agent supervisor). It is the whole
// wiring a run needs to give the planning loop its own firing path.
func PlanRouter(p *Planner, rest Firer) Firer {
	return FirerFunc(func(ctx context.Context, f Firing) (FireResult, error) {
		if f.Demand.Kind == DemandPlan && p != nil {
			return p.Fire(ctx, f)
		}
		if rest == nil {
			return FireResult{}, fmt.Errorf("%w: nothing to fire %s with", ErrNoFirer, f.Agent.ID)
		}
		return rest.Fire(ctx, f)
	})
}
