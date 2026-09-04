package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// The scheduler (P6-02) is the run's [Loop]: a tick loop that wakes on
// events and on an interval, asks a [WorkSource] what wants doing, and
// fills the free ammo slots in the D-05 priority order
//
//	arbitrations and escalations   (the product owner)
//	reviews owed on in_review beads (testers, one per lens)
//	implementation of ready beads   (engineers)
//	planning                        (the product owner)
//
// under the D-05 allocation, which splits ammo into role pools:
//
//	ammo | PO | engineers | testers | testers per bead
//	-----+----+-----------+---------+-----------------
//	   3 |  1 |         1 |       1 |                1
//	   4 |  1 |         1 |       2 |                2
//	  10 |  1 |         2 |       7 |                3
//	  12 |  1 |         2 |       9 |                3
//	  13 |  1 |         3 |       9 |                3
//
// (PO + engineers + testers always add up to exactly ammo, so honouring the
// per-role caps is what keeps live agents ≤ ammo; the total is checked as
// well, in priority order, in case a caller hands in an allocation whose
// parts do not add up.) The table is [config.Allocate]'s, mirrored here
// because the scheduler is what makes it true at runtime; the "testers per
// bead" column is [SlotCaps.TestersPerBead], the D-05 rule that a review
// gets one tester per lens, fewer when ammo cannot afford three, never
// zero.
//
// Slot accounting is exact: a slot is taken in [Scheduler.dispatch], which
// is the only place that starts an agent and only ever runs on the loop
// goroutine, and given back when the agent's goroutine returns — whether it
// finished, failed, was cancelled or panicked. So a crashing agent frees
// its slot, and the counts never drift.
//
// Everything the loop waits on goes through [beads.Clock], so a test with a
// fake clock is deterministic: nothing here reads the wall clock.
//
// P6-13 lives here too: a demand that hands a bead to an agent claims it
// through the beads client before the agent is fired — [beads.Client.ClaimFor]
// for a fresh claim, [beads.Client.Reclaim] for a bead coming back from
// review or a conflict round, and [beads.Client.Release] first when the bead
// is still assigned to a different agent (bd 1.2.2 refuses a claim
// otherwise). The lease is renewed every [beads.DefaultRenewInterval] while
// the agent runs.

// SchedulerWarningSource is the [events.Warning] Source the scheduler
// stamps on anything it had to skip.
const SchedulerWarningSource = "scheduler"

// DefaultTickInterval is how often the loop wakes up when nothing kicks it.
// The scheduler is event-driven — every finished agent kicks it — so the
// tick is only a backstop for work that appears in beads without an event.
const DefaultTickInterval = time.Second

// DefaultDrainGrace bounds how long [Scheduler.Run] waits for its agent
// goroutines after its context is cancelled. Subprocesses that outlive it
// are the supervisor's problem ([AgentStopper]).
const DefaultDrainGrace = DefaultStopGrace

// Scheduler errors, matched with errors.Is.
var (
	// ErrNoFirer means the scheduler was given work to do but no [Firer]
	// to do it with.
	ErrNoFirer = errors.New("orchestrator: scheduler has a work source but no firer")
	// ErrAgentPanic wraps a panic recovered from a [Firer]. One agent must
	// not take the run down with it.
	ErrAgentPanic = errors.New("orchestrator: agent panicked")
)

// DemandKind is a job the scheduler can fire an agent for. The kinds are
// declared in the D-05 priority order and [DemandKind.Priority] returns it.
type DemandKind string

// The demand kinds, highest priority first.
const (
	// DemandArbitrate is the product owner ruling on held feedback items or
	// deciding a bead that hit the round cap (D-08).
	DemandArbitrate DemandKind = "arbitrate"
	// DemandReview is a tester reviewing a bead through one lens.
	DemandReview DemandKind = "review"
	// DemandImplement is an engineer implementing a bead, answering a round
	// of feedback, or resolving a merge conflict.
	DemandImplement DemandKind = "implement"
	// DemandPlan is the product owner producing a plan: at the start of a
	// run and, in autopilot, whenever the ready queue empties (D-18).
	DemandPlan DemandKind = "plan"
)

var demandKinds = []DemandKind{DemandArbitrate, DemandReview, DemandImplement, DemandPlan}

// DemandKinds returns every kind in priority order.
func DemandKinds() []DemandKind { return append([]DemandKind(nil), demandKinds...) }

// String returns the kind's name.
func (k DemandKind) String() string { return string(k) }

// Valid reports whether k is one of the declared kinds.
func (k DemandKind) Valid() bool {
	for _, c := range demandKinds {
		if c == k {
			return true
		}
	}
	return false
}

// Priority is the D-05 rank, 0 being the first to be given a slot.
// An unknown kind sorts last.
func (k DemandKind) Priority() int {
	for i, c := range demandKinds {
		if c == k {
			return i
		}
	}
	return len(demandKinds)
}

// Role is the role that serves this kind of demand.
func (k DemandKind) Role() domain.Role {
	switch k {
	case DemandArbitrate, DemandPlan:
		return domain.RolePO
	case DemandReview:
		return domain.RoleTester
	case DemandImplement:
		return domain.RoleEngineer
	default:
		return ""
	}
}

// Task is the default [events.Task] for the kind, used when a [Demand]
// does not name a more specific one.
func (k DemandKind) Task() events.Task {
	switch k {
	case DemandArbitrate:
		return events.TaskArbitrate
	case DemandReview:
		return events.TaskReview
	case DemandImplement:
		return events.TaskImplement
	case DemandPlan:
		return events.TaskPlan
	default:
		return events.Task(k)
	}
}

// ClaimMode says what the scheduler must do in beads before an agent may
// touch the bead (P6-13, D-07).
type ClaimMode string

// The claim modes.
const (
	// ClaimNone takes no lease: testers and the product owner read a bead,
	// they do not own it.
	ClaimNone ClaimMode = ""
	// ClaimNew is the atomic `bd update --claim` for a bead in the ready
	// set: open, and not assigned to anybody else.
	ClaimNew ClaimMode = "claim"
	// ClaimRework is the plain update bd's atomic claim cannot do: a bead
	// in changes_requested (whose assignee is still the previous round's
	// engineer) or a delivery conflict round handed to an engineer.
	ClaimRework ClaimMode = "reclaim"
)

// Demand is one unit of work that wants an agent. A [WorkSource] returns
// the demands that are outstanding right now; the scheduler orders them,
// drops the ones already in flight, and fires as many as the ammo
// allocation allows. A demand is not a queue entry: it is re-offered on
// every tick until the work behind it is done, so a source stays
// stateless and the scheduler never holds stale work.
type Demand struct {
	// Kind is what wants doing; it decides the priority and the role.
	Kind DemandKind
	// Bead is the bead the work is about; empty for planning.
	Bead domain.BeadID
	// BeadTitle is that bead's title, for the events the scheduler
	// publishes; optional.
	BeadTitle string
	// Lens is the tester's perspective; set for DemandReview only.
	Lens domain.Lens
	// Round is the review round the work belongs to (D-08). It is part of
	// the demand's identity, so a new round is new work.
	Round int
	// Task overrides the kind's default [events.Task] — an engineer
	// answering feedback (events.TaskAddressFeedback) or resolving a
	// conflict (events.TaskResolveConflict) is still a DemandImplement.
	Task events.Task
	// Claim says whether the bead must be claimed before firing, and how
	// (P6-13).
	Claim ClaimMode
	// From is the state the bead is in before the claim, when the source
	// knows it. The claim itself is a D-07 transition to in_progress that
	// bd performs atomically rather than through the state machine, so the
	// scheduler publishes it from here; leave it empty (or already
	// in_progress) and no BeadStateChanged is published for the claim.
	From domain.BeadState
	// HeldBy is the agent beads records as the bead's current holder, when
	// the source knows it. A different holder makes a ClaimNew a
	// hand-over: the bead is released first, because bd refuses to claim a
	// bead assigned to somebody else.
	HeldBy domain.AgentID
	// Agent overrides the minted agent id; leave empty for the default
	// naming ("eng-<bead>-r<round>", "tester-<lens>-<bead>-r<round>", …).
	Agent domain.AgentID
	// Order breaks ties within a kind, lower first; use it for bead age or
	// priority. Demands with equal Order fall back to a stable order:
	// conflict rounds before feedback rounds before fresh implementation,
	// then bead id, lens and round.
	Order int
	// Payload is whatever the source wants handed back to the [Firer] —
	// the round's feedback items, the plan request, the verdicts to
	// arbitrate. The scheduler never looks at it.
	Payload any
}

// Role is the role that serves the demand.
func (d Demand) Role() domain.Role { return d.Kind.Role() }

// FiringTask is the [events.Task] the agent is fired for.
func (d Demand) FiringTask() events.Task {
	if d.Task != "" {
		return d.Task
	}
	return d.Kind.Task()
}

// Key identifies the work, not the attempt: two demands with the same key
// are the same job, so the scheduler never has two agents on it at once.
func (d Demand) Key() string {
	return string(d.Kind) + "|" + string(d.Bead) + "|" + string(d.Lens) + "|" + strconv.Itoa(d.Round)
}

// WorkSource reports everything that wants an agent right now. It is
// consulted once per tick on the loop goroutine, so it need not be safe for
// concurrent use, but it must not block for long: nothing else is scheduled
// while it thinks.
//
// The feedback engine (P6-04), the planning loop (P6-05) and the delivery
// handoff (P6-06) each contribute demands; a run composes them.
type WorkSource interface {
	// Demands returns the outstanding work in any order. An error is a
	// warning, not a run failure: the loop keeps its current agents and
	// tries again on the next tick.
	Demands(ctx context.Context) ([]Demand, error)
}

// WorkSourceFunc adapts a function to [WorkSource].
type WorkSourceFunc func(ctx context.Context) ([]Demand, error)

// Demands implements [WorkSource].
func (f WorkSourceFunc) Demands(ctx context.Context) ([]Demand, error) { return f(ctx) }

// WorkSources concatenates several sources into one, in the order given.
// An error from any of them is returned with whatever the others produced,
// so one broken source does not silence the rest.
func WorkSources(sources ...WorkSource) WorkSource {
	return WorkSourceFunc(func(ctx context.Context) ([]Demand, error) {
		var (
			all  []Demand
			errs []error
		)
		for _, s := range sources {
			if s == nil {
				continue
			}
			ds, err := s.Demands(ctx)
			all = append(all, ds...)
			if err != nil {
				errs = append(errs, err)
			}
		}
		return all, errors.Join(errs...)
	})
}

// Firing is everything a [Firer] needs to run one agent.
type Firing struct {
	// Agent is the agent record the scheduler minted and registered. Its
	// State is running and its StartedAt is set.
	Agent domain.Agent
	// Demand is the work it was fired for, Payload included.
	Demand Demand
	// Run is the run, for publishing events and reading configuration.
	Run *Run
	// Update records a change to the agent record — most usefully the pid
	// once the subprocess exists — in state.json and in the scheduler's
	// own view. Never nil.
	Update func(domain.Agent)
}

// FireResult is what an agent produced. Everything in it is optional; the
// scheduler only puts it in the [events.AgentFinished] and
// [events.AgentFailed] it publishes.
type FireResult struct {
	// Produced is the structured message the agent returned.
	Produced domain.MessageType
	// ExitCode is the subprocess exit status.
	ExitCode int
	// Failure classifies a non-nil error; empty means [events.FailureCrash].
	Failure events.Failure
}

// Firer runs one agent to completion; a [Firing] is one firing of the
// cannon. The scheduler calls it on its own
// goroutine, one per agent, and frees the slot when it returns.
//
// The context is cancelled when the run stops, so Fire must return promptly
// on cancellation. Fire owns the subprocess: the agent supervisor (P6-07)
// implements it, applying the per-agent timeout, the crash retries and the
// rate-limit backoff, and it is the supervisor's [AgentStopper] that kills
// whatever is left at shutdown.
type Firer interface {
	// Fire runs the shot's agent and returns when it is done.
	Fire(ctx context.Context, f Firing) (FireResult, error)
}

// FirerFunc adapts a function to [Firer].
type FirerFunc func(ctx context.Context, f Firing) (FireResult, error)

// Fire implements [Firer].
func (fn FirerFunc) Fire(ctx context.Context, f Firing) (FireResult, error) { return fn(ctx, f) }

// ClaimStore is the slice of *beads.Client the scheduler needs to hand
// beads to agents (P3-05, P6-13). Every method matches the client's
// signature, so *beads.Client satisfies it; tests use a fake.
//
// It is deliberately separate from [StateStore]: the state machine (P6-03)
// moves beads between statuses, the scheduler decides who holds them.
type ClaimStore interface {
	// ClaimFor atomically assigns an open, unassigned bead and sets
	// in_progress.
	ClaimFor(ctx context.Context, id domain.BeadID, agent domain.AgentID) (beads.Issue, error)
	// Reclaim moves a bead that came back from review to in_progress for a
	// possibly different agent.
	Reclaim(ctx context.Context, id domain.BeadID, agent domain.AgentID) (beads.Issue, error)
	// Release returns a bead to the ready set and drops the lease.
	Release(ctx context.Context, id domain.BeadID) (beads.Issue, error)
	// Renew refreshes the lease of a bead the agent still holds.
	Renew(ctx context.Context, id domain.BeadID, agent domain.AgentID) (beads.Issue, error)
}

// *beads.Client is the production ClaimStore.
var _ ClaimStore = (*beads.Client)(nil)

// SlotCaps is the D-05 allocation as the scheduler enforces it: one pool
// per role plus the overall ammo cap.
type SlotCaps struct {
	// Ammo is the total number of agents that may be live at once.
	Ammo int
	// PO is how many product owners may run at once (D-05: one).
	PO int
	// Engineers is max_engineers_in_flight.
	Engineers int
	// Testers is every remaining slot.
	Testers int
}

// CapsFor turns an allocation into the caps the scheduler enforces. An
// allocation with no ammo (the zero value) falls back to the D-05 minimum,
// so a misconfigured scheduler still fires one agent of each role rather
// than nothing at all.
func CapsFor(a config.Allocation) SlotCaps {
	if a.Ammo < config.MinAmmo {
		a, _ = config.Allocate(config.MinAmmo)
	}
	return SlotCaps{Ammo: a.Ammo, PO: max(a.PO, 1), Engineers: max(a.MaxEngineers, 1), Testers: max(a.MaxTesters, 1)}
}

// For returns the cap of one role's pool; an unknown role has none.
func (c SlotCaps) For(role domain.Role) int {
	switch role {
	case domain.RolePO:
		return c.PO
	case domain.RoleEngineer:
		return c.Engineers
	case domain.RoleTester:
		return c.Testers
	default:
		return 0
	}
}

// TestersPerBead is the D-05 review width: one tester per lens, fewer when
// ammo cannot afford three, never zero.
func (c SlotCaps) TestersPerBead() int {
	return max(1, min(len(domain.Lenses()), c.Testers))
}

// String renders the caps the way the CLI renders an allocation.
func (c SlotCaps) String() string {
	return fmt.Sprintf("ammo %d → %d PO, up to %d engineers, up to %d testers, %d testers per bead",
		c.Ammo, c.PO, c.Engineers, c.Testers, c.TestersPerBead())
}

// SchedulerStatus is a snapshot of the ammo in use, for `sc status`, the
// TUI header and the stop conditions.
type SchedulerStatus struct {
	// Caps is the allocation being enforced.
	Caps SlotCaps
	// Live is how many agents are running right now.
	Live int
	// ByRole counts the live agents of each role.
	ByRole map[domain.Role]int
	// Agents are the live agent records, in the order they were fired.
	Agents []domain.Agent
	// Ticks is how many times the loop has dispatched.
	Ticks uint64
	// Fired is how many agents the scheduler has started in total.
	Fired uint64
	// Idle is true when nothing is running and the last dispatch found
	// nothing to fire: the state a stop condition looks for.
	Idle bool
}

// Free is how many slots of the role are still available.
func (s SchedulerStatus) Free(role domain.Role) int {
	return max(0, min(s.Caps.For(role)-s.ByRole[role], s.Caps.Ammo-s.Live))
}

// StopCondition decides when the run is over. The planning loop (P6-05)
// implements it: brief mode finishes when every bead has settled, autopilot
// only on a stop or when every cartridge is out of credits (D-18). With no
// stop condition the scheduler runs until its context is cancelled.
type StopCondition interface {
	// Done is consulted after every dispatch. It returns the outcome to
	// finish with and true to stop; an error ends the run as a failure.
	Done(ctx context.Context, r *Run, st SchedulerStatus) (Outcome, bool, error)
}

// StopConditionFunc adapts a function to [StopCondition].
type StopConditionFunc func(ctx context.Context, r *Run, st SchedulerStatus) (Outcome, bool, error)

// Done implements [StopCondition].
func (f StopConditionFunc) Done(ctx context.Context, r *Run, st SchedulerStatus) (Outcome, bool, error) {
	return f(ctx, r, st)
}

// SchedulerOptions configures a [Scheduler]. The zero value is a scheduler
// that fires nothing and never finishes, which is what a lifecycle test
// wants; everything else is opt-in.
type SchedulerOptions struct {
	// Allocation is the D-05 split to enforce. The zero value means "the
	// run's own", read from [Run.Allocation] when the loop starts.
	Allocation config.Allocation
	// Work reports what wants doing. Nil means nothing ever does.
	Work WorkSource
	// Fire runs the agents. Required whenever Work is set.
	Fire Firer
	// Claims claims, reclaims, releases and renews beads (P6-13). Nil
	// skips the bd writes, which is what a test with no beads wants.
	Claims ClaimStore
	// Stop decides when the run finishes (P6-05). Nil means "never".
	Stop StopCondition
	// Clock is the time source; nil means [beads.SystemClock]. Tests pass a
	// fake one and nothing in the loop reads the wall clock.
	Clock beads.Clock
	// Tick is the backstop interval between dispatches; zero means
	// [DefaultTickInterval].
	Tick time.Duration
	// RenewInterval is how often a running agent's bead lease is renewed;
	// zero means [beads.DefaultRenewInterval]. Negative disables renewal.
	RenewInterval time.Duration
	// DrainGrace bounds the wait for agent goroutines after the context is
	// cancelled; zero means [DefaultDrainGrace].
	DrainGrace time.Duration
	// Wake is an extra kick channel: send (or close) to make the loop
	// dispatch at once. A bead watcher's changes go here.
	Wake <-chan struct{}
	// SuppressAgentEvents stops the scheduler publishing AgentSpawned,
	// AgentFinished and AgentFailed, for a [Firer] that publishes richer
	// ones itself (it knows the pid, the exit code and the retries).
	SuppressAgentEvents bool
}

// liveAgent is one slot in use.
type liveAgent struct {
	agent   domain.Agent
	demand  Demand
	seq     uint64
	attempt int
	cancel  context.CancelFunc
}

// Scheduler is the run's [Loop]: it keeps the ammo full, in D-05 priority
// order, and hands every bead to its agent through the beads claim
// protocol. Obtain one with [NewScheduler]; all methods are safe for
// concurrent use.
type Scheduler struct {
	opts  SchedulerOptions
	clock beads.Clock
	tick  time.Duration
	renew time.Duration
	drain time.Duration

	kickCh chan struct{}
	wg     sync.WaitGroup

	mu       sync.Mutex
	caps     SlotCaps
	live     map[domain.AgentID]*liveAgent
	byKey    map[string]domain.AgentID
	attempts map[string]int
	byRole   map[domain.Role]int
	ticks    uint64
	fired    uint64
	idle     bool
}

// NewScheduler returns a scheduler configured by opts.
func NewScheduler(opts SchedulerOptions) *Scheduler {
	s := &Scheduler{
		opts:     opts,
		clock:    opts.Clock,
		tick:     opts.Tick,
		renew:    opts.RenewInterval,
		drain:    opts.DrainGrace,
		kickCh:   make(chan struct{}, 1),
		caps:     CapsFor(opts.Allocation),
		live:     map[domain.AgentID]*liveAgent{},
		byKey:    map[string]domain.AgentID{},
		attempts: map[string]int{},
		byRole:   map[domain.Role]int{},
	}
	if s.clock == nil {
		s.clock = beads.SystemClock()
	}
	if s.tick <= 0 {
		s.tick = DefaultTickInterval
	}
	if s.renew == 0 {
		s.renew = beads.DefaultRenewInterval
	}
	if s.drain <= 0 {
		s.drain = DefaultDrainGrace
	}
	return s
}

// Kick wakes the loop so it dispatches at once instead of waiting for the
// next tick. It never blocks: a kick that arrives while one is pending is
// dropped, because a dispatch that has not happened yet will see the same
// world.
func (s *Scheduler) Kick() {
	select {
	case s.kickCh <- struct{}{}:
	default:
	}
}

// Caps returns the allocation the scheduler enforces.
func (s *Scheduler) Caps() SlotCaps {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.caps
}

// Live is how many agents are running right now.
func (s *Scheduler) Live() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live)
}

// Status snapshots the ammo in use.
func (s *Scheduler) Status() SchedulerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

func (s *Scheduler) statusLocked() SchedulerStatus {
	st := SchedulerStatus{
		Caps:   s.caps,
		Live:   len(s.live),
		ByRole: make(map[domain.Role]int, len(s.byRole)),
		Ticks:  s.ticks,
		Fired:  s.fired,
		Idle:   s.idle && len(s.live) == 0,
	}
	for role, n := range s.byRole {
		if n > 0 {
			st.ByRole[role] = n
		}
	}
	ordered := make([]*liveAgent, 0, len(s.live))
	for _, l := range s.live {
		ordered = append(ordered, l)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].seq < ordered[j].seq })
	for _, l := range ordered {
		// A reservation whose agent record does not exist yet (the claim is
		// still in flight) holds a slot but has nothing to show.
		if l.agent.ID == "" {
			continue
		}
		st.Agents = append(st.Agents, l.agent)
	}
	return st
}

// Run drives the run until its work is done or ctx is cancelled. It
// implements [Loop], so it is what [Options.Loop] is given.
//
// One dispatch happens immediately, then on every kick, on every agent that
// ends, and at least every tick. While the run is paused no agent is fired
// and the loop waits on [Run.AwaitResume]; the agents already running
// finish. When ctx is cancelled the agents' own contexts are cancelled too
// and Run waits up to the drain grace for their goroutines.
func (s *Scheduler) Run(ctx context.Context, r *Run) (Outcome, error) {
	if r == nil {
		return Outcome{}, errors.New("orchestrator: scheduler: nil run")
	}
	if s.opts.Work != nil && s.opts.Fire == nil {
		return Outcome{}, ErrNoFirer
	}
	if s.opts.Allocation.Ammo < config.MinAmmo {
		s.mu.Lock()
		s.caps = CapsFor(r.Allocation())
		s.mu.Unlock()
	}
	r.Log().Info("scheduler started", slog.String("caps", s.Caps().String()))
	defer s.drainAgents(r)

	for {
		if err := ctx.Err(); err != nil {
			return Outcome{Reason: events.FinishStopped, Summary: "the scheduler was stopped"}, nil
		}
		if r.Paused() {
			if err := r.AwaitResume(ctx); err != nil {
				return Outcome{Reason: events.FinishStopped, Summary: "the scheduler was stopped"}, nil
			}
		}
		if !r.Paused() {
			s.dispatch(ctx, r)
		}
		if out, done, err := s.stopped(ctx, r); err != nil {
			return out, err
		} else if done {
			return out, nil
		}
		select {
		case <-ctx.Done():
		case <-s.kickCh:
		case <-s.wake():
		case <-s.clock.After(s.tick):
		}
	}
}

// wake returns the caller's extra kick channel, or nil (which blocks
// forever in a select) when there is none.
func (s *Scheduler) wake() <-chan struct{} { return s.opts.Wake }

// stopped consults the stop condition.
func (s *Scheduler) stopped(ctx context.Context, r *Run) (Outcome, bool, error) {
	if s.opts.Stop == nil {
		return Outcome{}, false, nil
	}
	out, done, err := s.opts.Stop.Done(ctx, r, s.Status())
	if err != nil {
		return out, false, err
	}
	return out, done, nil
}

// dispatch fills every free slot it can, in the D-05 priority order. It
// runs only on the loop goroutine, which is what makes the slot accounting
// exact: nothing else ever takes a slot.
func (s *Scheduler) dispatch(ctx context.Context, r *Run) {
	s.mu.Lock()
	s.ticks++
	s.mu.Unlock()

	if s.opts.Work == nil {
		s.setIdle(true)
		return
	}
	demands, err := s.opts.Work.Demands(ctx)
	if err != nil {
		r.Warn(SchedulerWarningSource, "could not work out what to schedule", err)
	}
	sortDemands(demands)

	fired := 0
	for _, d := range demands {
		if ctx.Err() != nil {
			return
		}
		if !d.Kind.Valid() {
			r.Warn(SchedulerWarningSource, fmt.Sprintf("ignored a demand of unknown kind %q", d.Kind), nil)
			continue
		}
		if !s.reserve(d) {
			continue
		}
		agentID := s.plannedAgentID(d)
		if err := s.claim(ctx, d, agentID); err != nil {
			s.unreserve(d)
			r.Warn(SchedulerWarningSource, fmt.Sprintf("could not claim %s for %s", d.Bead, d.Kind), err)
			continue
		}
		s.announceClaim(r, d, agentID)
		s.start(ctx, r, d)
		fired++
	}
	s.setIdle(fired == 0)
}

func (s *Scheduler) setIdle(v bool) {
	s.mu.Lock()
	s.idle = v
	s.mu.Unlock()
}

// sortDemands puts the demands in the order the slots are handed out:
// the D-05 kind priority first, then the source's own ordering, then
// unfinished work before fresh work, then a stable tie-break.
func sortDemands(ds []Demand) {
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i], ds[j]
		if a.Kind != b.Kind {
			return a.Kind.Priority() < b.Kind.Priority()
		}
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		if ra, rb := taskRank(a.FiringTask()), taskRank(b.FiringTask()); ra != rb {
			return ra < rb
		}
		if a.Bead != b.Bead {
			return a.Bead < b.Bead
		}
		if a.Lens != b.Lens {
			return lensRank(a.Lens) < lensRank(b.Lens)
		}
		return a.Round < b.Round
	})
}

// taskRank prefers finishing a bead over starting one: a conflict blocks a
// delivery, feedback blocks a review round, fresh implementation blocks
// nothing yet.
func taskRank(t events.Task) int {
	switch t {
	case events.TaskResolveConflict:
		return 0
	case events.TaskAddressFeedback:
		return 1
	case events.TaskImplement:
		return 3
	default:
		return 2
	}
}

// lensRank orders lenses the way domain.Lenses does.
func lensRank(l domain.Lens) int {
	for i, c := range domain.Lenses() {
		if c == l {
			return i
		}
	}
	return len(domain.Lenses())
}

// reserve takes a slot for the demand if the allocation allows one and the
// work is not already in flight. It is the whole of the ammo accounting.
func (s *Scheduler) reserve(d Demand) bool {
	role := d.Role()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.byKey[d.Key()]; busy {
		return false
	}
	if len(s.live) >= s.caps.Ammo {
		return false
	}
	if s.byRole[role] >= s.caps.For(role) {
		return false
	}
	if d.Kind == DemandReview && s.testersOn(d.Bead) >= s.caps.TestersPerBead() {
		return false
	}
	// A placeholder holds the slot until start() replaces it, so two
	// demands of one dispatch cannot both take the last one.
	s.byKey[d.Key()] = ""
	s.byRole[role]++
	s.live[s.pendingID(d)] = &liveAgent{demand: d}
	return true
}

// pendingID is the placeholder key a reservation holds in the live map
// until the real agent record exists.
func (s *Scheduler) pendingID(d Demand) domain.AgentID {
	return domain.AgentID("\x00pending|" + d.Key())
}

// unreserve gives a reservation back when the claim failed.
func (s *Scheduler) unreserve(d Demand) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byKey, d.Key())
	delete(s.live, s.pendingID(d))
	s.byRole[d.Role()]--
}

// testersOn counts the live testers reviewing one bead; the caller holds
// the lock.
func (s *Scheduler) testersOn(bead domain.BeadID) int {
	n := 0
	for _, l := range s.live {
		if l.demand.Kind == DemandReview && l.demand.Bead == bead {
			n++
		}
	}
	return n
}

// plannedAgentID is the id the demand's agent will get, needed before the
// claim because the claim records the holder.
func (s *Scheduler) plannedAgentID(d Demand) domain.AgentID {
	if d.Agent != "" {
		return d.Agent
	}
	s.mu.Lock()
	attempt := s.attempts[d.Key()] + 1
	s.mu.Unlock()
	return mintAgentID(d, attempt)
}

// mintAgentID names an agent after its job: "eng-sc-68s-r2",
// "tester-qa-sc-68s-r2", "po-sc-68s-r3", "po-plan-1". A retry of the same
// job gets an "-a<attempt>" suffix so no two agents of a run share a name.
func mintAgentID(d Demand, attempt int) domain.AgentID {
	var id string
	switch d.Kind {
	case DemandPlan:
		id = "po-plan-" + strconv.Itoa(attempt)
		return domain.AgentID(id)
	case DemandArbitrate:
		id = "po-" + string(d.Bead)
	case DemandReview:
		id = "tester-" + string(d.Lens) + "-" + string(d.Bead)
	case DemandImplement:
		id = "eng-" + string(d.Bead)
	default:
		id = string(d.Kind) + "-" + string(d.Bead)
	}
	if d.Round > 0 {
		id += "-r" + strconv.Itoa(d.Round)
	}
	if attempt > 1 {
		id += "-a" + strconv.Itoa(attempt)
	}
	return domain.AgentID(id)
}

// claim gives the bead to the agent before it is fired (P6-13).
//
// Verified against bd 1.2.2 (Appendix A): `bd update --claim` succeeds only
// on an open, unassigned bead or on the same actor's in_progress bead, and
// a changes_requested bead keeps the previous round's engineer as its
// assignee. So a fresh claim uses ClaimFor, a bead coming back from review
// or a conflict round uses Reclaim, and a bead still assigned to somebody
// else is released first — which is the only way a second engineer ever
// gets it without ErrAlreadyClaimed.
func (s *Scheduler) claim(ctx context.Context, d Demand, agent domain.AgentID) error {
	if s.opts.Claims == nil || d.Bead == "" || d.Claim == ClaimNone {
		return nil
	}
	switch d.Claim {
	case ClaimRework:
		_, err := s.opts.Claims.Reclaim(ctx, d.Bead, agent)
		return err
	case ClaimNew:
		if d.HeldBy != "" && d.HeldBy != agent {
			if _, err := s.opts.Claims.Release(ctx, d.Bead); err != nil {
				return fmt.Errorf("release %s before handing it to %s: %w", d.Bead, agent, err)
			}
		}
		_, err := s.opts.Claims.ClaimFor(ctx, d.Bead, agent)
		if err != nil && errors.Is(err, beads.ErrAlreadyClaimed) && d.HeldBy == "" {
			// The bead carries an assignee the source did not know about;
			// hand it over the same way and try once more.
			if _, rerr := s.opts.Claims.Release(ctx, d.Bead); rerr != nil {
				return fmt.Errorf("release %s after a lost claim: %w", d.Bead, errors.Join(err, rerr))
			}
			_, err = s.opts.Claims.ClaimFor(ctx, d.Bead, agent)
		}
		return err
	default:
		return fmt.Errorf("orchestrator: scheduler: unknown claim mode %q", d.Claim)
	}
}

// announceClaim publishes the D-07 transition the claim just performed.
//
// bd's atomic claim sets in_progress itself, so the write does not go
// through the state machine (P6-03) and nothing else would publish it: the
// run log, `sc status` and the TUI graph would show a bead jumping from
// open straight to in_review. The demand has to say where the bead was
// ([Demand.From]); a bead already in_progress — the conflict round of a
// bead the deliverer already moved (D-10) — announces nothing, because it
// did not move.
func (s *Scheduler) announceClaim(r *Run, d Demand, agent domain.AgentID) {
	if d.Bead == "" || d.Claim == ClaimNone || d.From == "" ||
		d.From == domain.BeadStateInProgress || !d.From.Workflow() {
		return
	}
	r.Publish(events.BeadStateChanged{
		Meta:   r.Meta(),
		Bead:   d.Bead,
		From:   d.From,
		To:     domain.BeadStateInProgress,
		Round:  d.Round,
		Reason: "claimed by " + string(agent),
	})
}

// start turns a reservation into a running agent.
func (s *Scheduler) start(ctx context.Context, r *Run, d Demand) {
	actx, cancel := context.WithCancel(ctx)
	now := s.clock.Now()

	s.mu.Lock()
	s.attempts[d.Key()]++
	attempt := s.attempts[d.Key()]
	s.fired++
	seq := s.fired
	id := d.Agent
	if id == "" {
		id = mintAgentID(d, attempt)
	}
	agent := domain.Agent{
		ID:        id,
		Role:      d.Role(),
		Lens:      d.Lens,
		Cartridge: r.RunConfig().CartridgeName,
		Model:     r.RunConfig().Cartridge.Model,
		Bead:      d.Bead,
		State:     domain.AgentStateRunning,
		StartedAt: now,
	}
	l := &liveAgent{agent: agent, demand: d, seq: seq, attempt: attempt, cancel: cancel}
	delete(s.live, s.pendingID(d))
	s.live[id] = l
	s.byKey[d.Key()] = id
	s.mu.Unlock()

	if err := r.AgentStarted(agent); err != nil {
		r.Warn(SchedulerWarningSource, "could not record agent "+string(id), err)
	}
	if !s.opts.SuppressAgentEvents {
		r.Publish(events.AgentSpawned{
			Meta:      r.Meta(),
			Agent:     agent,
			Task:      d.FiringTask(),
			BeadTitle: d.BeadTitle,
			Round:     d.Round,
			Attempt:   attempt,
		})
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		s.fire(actx, r, l, now)
	}()
}

// fire runs one agent and gives its slot back, whatever happens to it.
func (s *Scheduler) fire(ctx context.Context, r *Run, l *liveAgent, startedAt time.Time) {
	defer func() {
		s.finished(l)
		s.Kick()
	}()

	s.mu.Lock()
	spawned := l.agent
	s.mu.Unlock()

	if s.opts.Claims != nil && l.demand.Claim != ClaimNone && l.demand.Bead != "" && s.renew > 0 {
		stop := make(chan struct{})
		defer close(stop)
		go s.renewLease(ctx, r, l.demand.Bead, spawned.ID, stop)
	}

	res, err := s.call(ctx, Firing{
		Agent:  spawned,
		Demand: l.demand,
		Run:    r,
		Update: func(a domain.Agent) { s.update(l, r, a) },
	})
	dur := s.clock.Now().Sub(startedAt)

	s.mu.Lock()
	agent := l.agent
	s.mu.Unlock()

	if stopErr := r.AgentStopped(agent.ID); stopErr != nil {
		r.Warn(SchedulerWarningSource, "could not clear agent "+string(agent.ID), stopErr)
	}
	if s.opts.SuppressAgentEvents {
		return
	}
	if err != nil {
		failure := res.Failure
		if failure == "" {
			failure = events.FailureCrash
		}
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			failure = events.FailureKilled
		}
		r.Publish(events.AgentFailed{
			Meta:     r.Meta(),
			Agent:    agent.ID,
			Role:     agent.Role,
			Bead:     agent.Bead,
			Failure:  failure,
			Err:      err.Error(),
			Duration: dur,
			ExitCode: res.ExitCode,
			Attempt:  l.attempt,
		})
		return
	}
	r.Publish(events.AgentFinished{
		Meta:     r.Meta(),
		Agent:    agent.ID,
		Role:     agent.Role,
		Bead:     agent.Bead,
		Duration: dur,
		ExitCode: res.ExitCode,
		Produced: res.Produced,
	})
}

// call invokes the firer, turning a panic into an error so one bad agent
// cannot take the run down.
func (s *Scheduler) call(ctx context.Context, f Firing) (res FireResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%w: %v", ErrAgentPanic, p)
			res.Failure = events.FailureCrash
		}
	}()
	if s.opts.Fire == nil {
		return FireResult{}, ErrNoFirer
	}
	return s.opts.Fire.Fire(ctx, f)
}

// update records a change to a live agent (its pid, most usefully).
func (s *Scheduler) update(l *liveAgent, r *Run, a domain.Agent) {
	s.mu.Lock()
	a.ID = l.agent.ID
	l.agent = a
	s.mu.Unlock()
	if err := r.AgentUpdated(a); err != nil {
		r.Warn(SchedulerWarningSource, "could not record agent "+string(a.ID), err)
	}
}

// finished frees the agent's slot.
func (s *Scheduler) finished(l *liveAgent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.live[l.agent.ID]; !ok {
		return
	}
	delete(s.live, l.agent.ID)
	if s.byKey[l.demand.Key()] == l.agent.ID {
		delete(s.byKey, l.demand.Key())
	}
	if n := s.byRole[l.demand.Role()]; n > 0 {
		s.byRole[l.demand.Role()] = n - 1
	}
}

// renewLease keeps the agent's claim alive while it works (P6-13). A lease
// that is gone is a warning, not a kill: the bead has moved on, and what to
// do about that is the supervisor's and the feedback engine's business.
func (s *Scheduler) renewLease(ctx context.Context, r *Run, bead domain.BeadID, agent domain.AgentID, stop <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-s.clock.After(s.renew):
		}
		if _, err := s.opts.Claims.Renew(ctx, bead, agent); err != nil {
			if ctx.Err() != nil {
				return
			}
			r.Warn(SchedulerWarningSource, fmt.Sprintf("stopped renewing the lease %s holds on %s", agent, bead), err)
			return
		}
	}
}

// drainAgents cancels every live agent and waits for its goroutine, so the
// run's shutdown does not race the scheduler's own bookkeeping. Agents that
// outstay the drain grace are left to the supervisor's [AgentStopper].
func (s *Scheduler) drainAgents(r *Run) {
	s.mu.Lock()
	for _, l := range s.live {
		if l.cancel != nil {
			l.cancel()
		}
	}
	remaining := len(s.live)
	s.mu.Unlock()
	if remaining == 0 {
		return
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-s.clock.After(s.drain):
		r.Warn(SchedulerWarningSource,
			fmt.Sprintf("%d agents were still running after %s; leaving them to the supervisor", s.Live(), s.drain), nil)
	}
}
