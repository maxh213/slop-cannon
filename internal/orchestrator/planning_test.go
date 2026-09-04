package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// Tests for the planning loop and the stop conditions (P6-05), the policy
// for a plan the validator rejects twice (P6-19) and the
// `max_beads_per_plan` config key reaching the product owner (P1-16).
// Every helper here is prefixed "planning" so it cannot collide with
// another Phase 6 task's file in this package.

// planningEpoch is the fixed start time every planning test runs from.
var planningEpoch = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

// planningClock is a fake [beads.Clock]: time moves only when a test says
// so, and every pending After is delivered when its deadline passes.
type planningClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []planningWaiter
}

type planningWaiter struct {
	deadline time.Time
	ch       chan time.Time
}

func newPlanningClock() *planningClock { return &planningClock{now: planningEpoch} }

func (c *planningClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *planningClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, planningWaiter{deadline: c.now.Add(d), ch: ch})
	return ch
}

func (c *planningClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.deadline.After(c.now) {
			w.ch <- c.now
			continue
		}
		kept = append(kept, w)
	}
	c.waiters = kept
}

// planningRunner is a fake [PlanRunner]: it hands back whatever the test
// queued and records every request it was given.
type planningRunner struct {
	mu    sync.Mutex
	reqs  []agents.PlanRequest
	plan  domain.Plan
	err   error
	calls int
	// next, when set, overrides plan/err per call.
	next func(n int) (domain.Plan, error)
}

func (r *planningRunner) RunPlan(_ context.Context, _ Firing, req agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
	r.mu.Lock()
	r.calls++
	n := r.calls
	r.reqs = append(r.reqs, req)
	plan, err, next := r.plan, r.err, r.next
	r.mu.Unlock()
	if next != nil {
		plan, err = next(n)
	}
	return plan, agents.Transcript{}, err
}

// setPlan and setErr change what the next firing returns. They are
// mutex-guarded because a run fires the product owner on its own
// goroutine.
func (r *planningRunner) setPlan(p domain.Plan) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plan, r.err = p, nil
}

func (r *planningRunner) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plan, r.err = domain.Plan{}, err
}

func (r *planningRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *planningRunner) last() (agents.PlanRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reqs) == 0 {
		return agents.PlanRequest{}, false
	}
	return r.reqs[len(r.reqs)-1], true
}

// planningApplier is a fake [agents.PlanApplier]: it mints an id per plan
// key and records the requests.
type planningApplier struct {
	mu    sync.Mutex
	calls []beads.PlanRequest
	err   error
}

func (a *planningApplier) ApplyPlan(_ context.Context, req beads.PlanRequest, _ bool) (beads.PlanResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, req)
	if a.err != nil {
		return beads.PlanResult{}, a.err
	}
	res := beads.PlanResult{Run: req.Run, Epic: req.Epic, IDs: map[string]domain.BeadID{}}
	if res.Epic == "" {
		res.Epic, res.EpicCreated, res.EpicKey = "sc-epic", true, "epic"
	}
	for _, b := range req.Plan.Beads {
		id := domain.BeadID("sc-" + b.Key)
		res.IDs[b.Key] = id
		res.Beads = append(res.Beads, id)
	}
	return res, nil
}

func (a *planningApplier) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

// planningBeadStore is the mutable bead listing behind a [BeadSource].
type planningBeadStore struct {
	mu   sync.Mutex
	list []domain.Bead
	err  error
}

func (s *planningBeadStore) source() BeadSource {
	return func(context.Context) ([]domain.Bead, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.err != nil {
			return nil, s.err
		}
		return append([]domain.Bead(nil), s.list...), nil
	}
}

func (s *planningBeadStore) set(list ...domain.Bead) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = list
}

// planningCredits is a fake [CreditsWatcher] a test can flip.
type planningCredits struct {
	mu  sync.Mutex
	out bool
}

func (c *planningCredits) Exhausted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out
}

func (c *planningCredits) exhaust() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.out = true
}

// planningPlan builds a valid two-bead plan.
func planningPlan(keys ...string) domain.Plan {
	p := domain.Plan{Brief: "the work worth doing"}
	for _, k := range keys {
		p.Beads = append(p.Beads, domain.PlanBead{
			Key:        k,
			Title:      "Do " + k,
			Acceptance: "it is done",
			Priority:   domain.PriorityDefault,
		})
	}
	return p
}

// planningFixture is one hermetic run driven by a scheduler whose work
// source, firer and stop condition are all the planner.
type planningFixture struct {
	t       *testing.T
	Repo    string
	Bus     *events.Bus
	Sub     *events.Subscription
	Clock   *planningClock
	Run     *Run
	Sched   *Scheduler
	Planner *Planner
	Runner  *planningRunner
	Applier *planningApplier
	Beads   *planningBeadStore
	Credits *planningCredits
	Eff     *config.Effective
	started bool
}

// planningSetup points XDG at temp directories (D-23) and builds a run
// whose loop is a scheduler driven entirely by the planner. brief is the -m
// text; empty means autopilot. tune may adjust the planner options before
// the planner is built. The run is not started: a test arranges what the
// product owner will answer first and then calls start.
func planningSetup(t *testing.T, brief string, tune func(*PlannerOptions)) *planningFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	repo := filepath.Join(home, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("# fixture\n"), 0o600))

	cfg := config.Defaults()
	cfg.Ammo = 6
	cfg.ActiveCartridge = "claude"
	cfg.Cartridges = map[string]config.Cartridge{"claude": {Model: "opus"}}
	eff := &config.Effective{Config: cfg, Sources: map[string]config.Source{}}
	rc, err := config.NewRunConfig(repo, eff, config.RunSettings{
		ID: "20260903-120000-abcd", Brief: brief, StartedAt: planningEpoch,
	})
	require.NoError(t, err)

	clock := newPlanningClock()
	bus := events.NewBus()
	sub := bus.Subscribe(context.Background(), 4096)
	t.Cleanup(func() { sub.Unsubscribe(); bus.Close() })

	f := &planningFixture{
		t: t, Repo: repo, Bus: bus, Sub: sub, Clock: clock, Eff: eff,
		Runner: &planningRunner{}, Applier: &planningApplier{},
		Beads: &planningBeadStore{}, Credits: &planningCredits{},
	}
	opts := PlannerOptions{
		Config: rc, Runner: f.Runner, Applier: f.Applier, Beads: f.Beads.source(),
		Credits: f.Credits, Now: clock.Now, NoShim: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if tune != nil {
		tune(&opts)
	}
	planner, err := NewPlanner(opts)
	require.NoError(t, err)
	f.Planner = planner

	sched := NewScheduler(SchedulerOptions{
		Work: planner, Fire: PlanRouter(planner, nil), Stop: planner, Clock: clock,
	})
	planner.opts.Status = sched.Status
	f.Sched = sched

	run, err := New(rc, Options{
		Loop:   sched,
		Bus:    bus,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    clock.Now,
	})
	require.NoError(t, err)
	planner.Attach(run)
	f.Run = run
	t.Cleanup(func() {
		if !f.started {
			return
		}
		_ = run.Stop("test cleanup")
		select {
		case <-run.Done():
		case <-time.After(5 * time.Second):
			t.Error("the run did not finish")
		}
	})
	return f
}

// start starts the run once the test has arranged everything the first
// planning firing will see.
func (f *planningFixture) start() {
	f.t.Helper()
	f.started = true
	require.NoError(f.t, f.Run.Start(context.Background()))
}

// wait blocks until the run has finished and returns its outcome.
func (f *planningFixture) wait() (Outcome, error) {
	f.t.Helper()
	select {
	case <-f.Run.Done():
	case <-time.After(5 * time.Second):
		f.t.Fatal("the run did not finish")
	}
	return f.Run.Wait()
}

// events drains the bus.
func (f *planningFixture) events() []events.Event {
	var out []events.Event
	for {
		select {
		case ev := <-f.Sub.Events():
			out = append(out, ev)
		default:
			return out
		}
	}
}

// planningEventually polls cond until it holds, so no test sleeps for a
// fixed time.
func planningEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// planningApply records the outcome of one planning firing the way
// [Planner.Fire] does, without running an agent.
func planningApply(p *Planner, created map[string]domain.BeadID, err error) {
	p.mu.Lock()
	p.attempts++
	p.mu.Unlock()
	p.record(Firing{Agent: domain.Agent{ID: "po-plan-1"}}, created, err)
}

// planningBead is a work bead in a given state.
func planningBead(id domain.BeadID, state domain.BeadState) domain.Bead {
	return domain.Bead{ID: id, Title: string(id), Type: domain.BeadTypeTask, State: state}
}

// --- brief mode -----------------------------------------------------------

func TestPlannerBriefModePlansOnceAndFinishesWhenEveryBeadHasSettled(t *testing.T) {
	f := planningSetup(t, "make it work", nil)
	f.Runner.setPlan(planningPlan("alpha", "beta"))
	// The beads the plan will create, already listed: a brief run must not
	// finish just because bd has not caught up yet.
	f.Beads.set(planningBead("sc-alpha", domain.BeadStateOpen), planningBead("sc-beta", domain.BeadStateOpen))
	f.start()

	planningEventually(t, "the brief plan to be applied", func() bool {
		f.Clock.Advance(time.Second)
		return f.Applier.count() == 1
	})
	// The beads exist and are still open, so the run must not finish.
	f.Beads.set(planningBead("sc-alpha", domain.BeadStateOpen), planningBead("sc-beta", domain.BeadStateInReview))
	for i := 0; i < 5; i++ {
		f.Clock.Advance(3 * time.Second)
		time.Sleep(2 * time.Millisecond)
	}
	require.Equal(t, PhaseRunning, f.Run.Phase(), "an unsettled bead must keep the run live")

	// Every bead settled: closed, failed and parked for a human all count.
	f.Beads.set(
		planningBead("sc-alpha", domain.BeadStateClosed),
		planningBead("sc-beta", domain.BeadStateNeedsHuman),
		domain.Bead{ID: "sc-epic", Type: domain.BeadTypeEpic, State: domain.BeadStateOpen},
	)
	planningEventually(t, "the brief run to finish", func() bool {
		f.Clock.Advance(3 * time.Second)
		select {
		case <-f.Run.Done():
			return true
		default:
			return false
		}
	})
	out, err := f.wait()
	require.NoError(t, err)
	assert.Equal(t, events.FinishCompleted, out.Reason)
	assert.Contains(t, out.Summary, "2 beads")
	assert.Contains(t, out.Summary, "1 closed")
	assert.Contains(t, out.Summary, "1 needs_human")

	assert.Equal(t, 1, f.Runner.count(), "brief mode plans exactly once")
	st := f.Planner.Status()
	assert.Equal(t, 1, st.Plans)
	assert.Equal(t, 2, st.Beads)
	assert.True(t, st.Finished)

	var produced *events.PlanProduced
	for _, ev := range f.events() {
		if p, ok := ev.(events.PlanProduced); ok {
			produced = &p
		}
	}
	require.NotNil(t, produced, "the plan must be announced")
	assert.Equal(t, domain.AgentID("po-plan-1"), produced.Agent)
	assert.Equal(t, domain.BeadID("sc-epic"), produced.Epic)
	assert.Equal(t, map[string]domain.BeadID{"alpha": "sc-alpha", "beta": "sc-beta"}, produced.Created)
	assert.Equal(t, domain.BeadID("sc-epic"), f.Run.Epic(), "the created epic is recorded on the run")
}

func TestPlannerBriefModeWaitsForBdToListWhatThePlanCreated(t *testing.T) {
	f := planningSetup(t, "make it work", nil)
	f.Runner.setPlan(planningPlan("solo"))
	f.start()

	planningEventually(t, "the brief plan to be applied", func() bool {
		f.Clock.Advance(time.Second)
		return f.Applier.count() == 1
	})
	// bd has not listed the new bead yet, so the run stays live rather
	// than finishing on an empty listing.
	for i := 0; i < 5; i++ {
		f.Clock.Advance(3 * time.Second)
		time.Sleep(2 * time.Millisecond)
	}
	require.Equal(t, PhaseRunning, f.Run.Phase())

	f.Beads.set(planningBead("sc-solo", domain.BeadStateClosed))
	planningEventually(t, "the brief run to finish", func() bool {
		f.Clock.Advance(3 * time.Second)
		select {
		case <-f.Run.Done():
			return true
		default:
			return false
		}
	})
	out, err := f.wait()
	require.NoError(t, err)
	assert.Equal(t, events.FinishCompleted, out.Reason)
}

// --- autopilot ------------------------------------------------------------

func TestPlannerAutopilotKeepsReplanningThroughTwentyFourHours(t *testing.T) {
	f := planningSetup(t, "", nil)
	f.Runner.setPlan(domain.Plan{Brief: "nothing worth adding right now"}) // empty plan (D-18)
	f.start()

	planningEventually(t, "the first plan", func() bool {
		f.Clock.Advance(time.Second)
		return f.Runner.count() == 1
	})
	assert.Equal(t, 0, f.Applier.count(), "an empty plan creates nothing")

	const day = 24 * time.Hour
	replans := int(day / config.DefaultIdleReplanInterval)
	for i := 1; i <= replans; i++ {
		want := i + 1
		planningEventually(t, fmt.Sprintf("re-plan %d", i), func() bool {
			f.Clock.Advance(config.DefaultIdleReplanInterval)
			return f.Runner.count() >= want
		})
	}
	assert.GreaterOrEqual(t, f.Runner.count(), replans, "autopilot re-plans every idle_replan_interval")
	assert.Equal(t, PhaseRunning, f.Run.Phase(), "autopilot never stops on its own (D-18)")
	st := f.Planner.Status()
	assert.Equal(t, st.Attempts, st.Empty, "every plan was empty")
	assert.Zero(t, st.Beads)

	require.NoError(t, f.Run.Stop("the user pressed s"))
	out, err := f.wait()
	require.NoError(t, err)
	assert.Equal(t, events.FinishStopped, out.Reason)
}

func TestPlannerAutopilotEndsWhenEveryCartridgeIsOutOfCredits(t *testing.T) {
	f := planningSetup(t, "", nil)
	f.Runner.setPlan(domain.Plan{Brief: "nothing for now"})
	f.start()

	planningEventually(t, "the first plan", func() bool {
		f.Clock.Advance(time.Second)
		return f.Runner.count() == 1
	})
	assert.Equal(t, PhaseRunning, f.Run.Phase())

	f.Credits.exhaust()
	planningEventually(t, "the run to finish", func() bool {
		f.Clock.Advance(time.Second)
		select {
		case <-f.Run.Done():
			return true
		default:
			return false
		}
	})
	out, err := f.wait()
	require.NoError(t, err)
	assert.Equal(t, events.FinishCreditsExhausted, out.Reason)
	assert.Contains(t, out.Summary, "out of credits")
}

func TestPlannerAutopilotEndsOnStop(t *testing.T) {
	f := planningSetup(t, "", nil)
	f.Runner.setPlan(planningPlan("alpha"))
	f.Beads.set(planningBead("sc-alpha", domain.BeadStateOpen))
	f.start()

	planningEventually(t, "the first plan", func() bool {
		f.Clock.Advance(time.Second)
		return f.Applier.count() == 1
	})
	// Beads exist and are open: brief mode would finish here, autopilot
	// must not.
	f.Beads.set(planningBead("sc-alpha", domain.BeadStateClosed))
	for i := 0; i < 5; i++ {
		f.Clock.Advance(3 * time.Second)
		time.Sleep(2 * time.Millisecond)
	}
	require.Equal(t, PhaseRunning, f.Run.Phase(), "autopilot has no completion condition (D-18)")

	require.NoError(t, f.Run.Stop("sc stop"))
	out, err := f.wait()
	require.NoError(t, err)
	assert.Equal(t, events.FinishStopped, out.Reason)
}

func TestPlannerAutopilotReplansOnlyWhenNothingIsInFlight(t *testing.T) {
	rc := planningRunConfig(t, "")
	runner := &planningRunner{plan: planningPlan("alpha")}
	clock := newPlanningClock()
	status := SchedulerStatus{Live: 1}
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: runner, Applier: &planningApplier{}, Now: clock.Now, NoShim: true,
		Status: func() SchedulerStatus { return status },
	})
	require.NoError(t, err)

	ds, err := p.Demands(context.Background())
	require.NoError(t, err)
	require.Len(t, ds, 1, "every run plans at the start")
	assert.Equal(t, DemandPlan, ds[0].Kind)
	assert.Equal(t, events.TaskPlan, ds[0].FiringTask())
	assert.Equal(t, ClaimNone, ds[0].Claim)

	// Pretend the first plan landed.
	planningApply(p, map[string]domain.BeadID{"alpha": "sc-alpha"}, nil)

	ds, err = p.Demands(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ds, "an engineer is running, so there is nothing to re-plan for")

	status = SchedulerStatus{Live: 0, Idle: false}
	ds, err = p.Demands(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ds, "the last dispatch still found work to fire")

	status = SchedulerStatus{Live: 0, Idle: true}
	ds, err = p.Demands(context.Background())
	require.NoError(t, err)
	assert.Len(t, ds, 1, "the ready queue is empty and nothing is running: re-plan (D-18)")
	assert.Contains(t, ds, planDemand())
}

func TestPlannerBriefModeAsksForOnlyOnePlan(t *testing.T) {
	rc := planningRunConfig(t, "ship the thing")
	clock := newPlanningClock()
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: &planningRunner{}, Applier: &planningApplier{}, Now: clock.Now, NoShim: true,
		Status: func() SchedulerStatus { return SchedulerStatus{Idle: true} },
	})
	require.NoError(t, err)

	ds, _ := p.Demands(context.Background())
	require.Len(t, ds, 1)
	planningApply(p, map[string]domain.BeadID{"alpha": "sc-alpha"}, nil)
	ds, _ = p.Demands(context.Background())
	assert.Empty(t, ds, "brief mode plans once")
	assert.True(t, p.Status().Finished)
}

// --- P6-19: a plan the validator rejects twice ----------------------------

func TestPlannerDropsAPlanTheValidatorRejectsTwiceAndBacksOff(t *testing.T) {
	f := planningSetup(t, "", nil)
	rejected := fmt.Errorf("plan: %w: bead \"a\" duplicates open bead sc-1", agents.ErrPlanRejected)
	f.Runner.setErr(rejected)
	f.start()

	planningEventually(t, "two rejected plans", func() bool {
		f.Clock.Advance(time.Second)
		return f.Runner.count() >= 2
	})
	planningEventually(t, "the planner to give up on the round", func() bool {
		f.Clock.Advance(time.Second)
		return f.Planner.Status().GaveUp >= 1
	})

	st := f.Planner.Status()
	assert.Equal(t, 0, st.Rejections, "the counter resets when the round is dropped")
	assert.Zero(t, f.Applier.count(), "a rejected plan creates nothing")
	assert.False(t, st.Finished, "autopilot never stops on its own (D-18)")
	assert.False(t, st.NextAt.IsZero(), "the next round is scheduled, not immediate")
	assert.True(t, st.NextAt.After(f.Clock.Now()), "the planner waits before trying again")

	var warned bool
	for _, ev := range f.events() {
		if w, ok := ev.(events.Warning); ok && w.Source == SourcePlanning {
			warned = true
		}
	}
	assert.True(t, warned, "a dropped plan is a warning, never a silent no-op")

	// It really does come back: autopilot has no give-up state.
	before := f.Runner.count()
	planningEventually(t, "the planner to try again after the backoff", func() bool {
		f.Clock.Advance(time.Minute)
		return f.Runner.count() > before
	})
}

func TestPlannerBriefModeFailsWhenThePlanIsRejectedTwice(t *testing.T) {
	f := planningSetup(t, "ship the thing", nil)
	f.Runner.setErr(fmt.Errorf("%w: the acceptance criteria are empty", agents.ErrPlanRejected))
	f.start()

	planningEventually(t, "the brief run to fail", func() bool {
		f.Clock.Advance(time.Second)
		select {
		case <-f.Run.Done():
			return true
		default:
			return false
		}
	})
	out, err := f.wait()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPlanningGaveUp)
	assert.Equal(t, events.FinishFailed, out.Reason)
	assert.Contains(t, out.Summary, "could not produce a usable plan")
	assert.Equal(t, 2, f.Runner.count(), "the brief is retried once, then dropped")
	assert.Zero(t, f.Applier.count())
}

func TestPlannerRejectionLimitIsConfigurable(t *testing.T) {
	rc := planningRunConfig(t, "")
	clock := newPlanningClock()
	runner := &planningRunner{err: fmt.Errorf("%w: nope", agents.ErrPlanRejected)}
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: runner, Applier: &planningApplier{}, Now: clock.Now, NoShim: true,
		RejectionLimit: -1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		planningApply(p, nil, runner.err)
	}
	st := p.Status()
	assert.Equal(t, 0, st.GaveUp, "a negative limit never gives up")
	assert.Equal(t, 5, st.Rejections)
}

func TestPlanBackoffDoublesAndIsCapped(t *testing.T) {
	idle := 10 * time.Minute
	assert.Equal(t, idle, planBackoff(idle, 1, time.Hour))
	assert.Equal(t, 20*time.Minute, planBackoff(idle, 2, time.Hour))
	assert.Equal(t, 40*time.Minute, planBackoff(idle, 3, time.Hour))
	assert.Equal(t, time.Hour, planBackoff(idle, 4, time.Hour))
	assert.Equal(t, time.Hour, planBackoff(idle, 40, time.Hour))
	assert.Equal(t, config.DefaultIdleReplanInterval, planBackoff(0, 1, time.Hour))
}

// --- P1-16: the plan cap --------------------------------------------------

func TestPlannerPassesTheConfiguredPlanCapToTheProductOwner(t *testing.T) {
	f := planningSetup(t, "", func(o *PlannerOptions) {
		o.Config.Effective.MaxBeadsPerPlan = 3
	})
	f.Runner.setPlan(domain.Plan{})
	f.start()
	planningEventually(t, "the first plan", func() bool {
		f.Clock.Advance(time.Second)
		return f.Runner.count() == 1
	})
	req, ok := f.Runner.last()
	require.True(t, ok)
	assert.Equal(t, 3, req.MaxBeads, "max_beads_per_plan reaches the planning request (P1-16)")
	assert.Equal(t, 3, req.BeadLimit())
	assert.Equal(t, 3, f.Planner.MaxBeads())
}

func TestPlannerFallsBackToTheDefaultPlanCap(t *testing.T) {
	rc := planningRunConfig(t, "")
	rc.Effective = nil
	p, err := NewPlanner(PlannerOptions{Config: rc, Runner: &planningRunner{}, Applier: &planningApplier{}})
	require.NoError(t, err)
	assert.Equal(t, config.DefaultMaxBeadsPerPlan, p.MaxBeads())
	assert.Equal(t, agents.DefaultMaxBeadsPerPlan, p.MaxBeads())
	assert.Equal(t, config.DefaultIdleReplanInterval, p.IdleReplanInterval())
}

// --- the planning request -------------------------------------------------

func TestPlannerRequestCarriesTheRunTheModeAndTheOpenBeads(t *testing.T) {
	f := planningSetup(t, "", nil)
	f.Runner.setPlan(domain.Plan{})
	f.Beads.set(
		planningBead("sc-open", domain.BeadStateOpen),
		planningBead("sc-done", domain.BeadStateClosed),
		domain.Bead{ID: "sc-epic", Type: domain.BeadTypeEpic, State: domain.BeadStateOpen},
	)
	f.start()
	planningEventually(t, "the first plan", func() bool {
		f.Clock.Advance(time.Second)
		return f.Runner.count() == 1
	})
	req, ok := f.Runner.last()
	require.True(t, ok)
	assert.Equal(t, domain.RunID("20260903-120000-abcd"), req.Run)
	assert.Equal(t, domain.RunModeAutopilot, req.Mode)
	assert.Empty(t, req.Brief)
	assert.Equal(t, domain.AgentID("po-plan-1"), req.AgentID)
	assert.Equal(t, f.Repo, req.Repo.Root)
	assert.Equal(t, "opus", req.Model)
	require.Len(t, req.OpenBeads, 1, "closed beads and the epic are not duplicates to avoid")
	assert.Equal(t, domain.BeadID("sc-open"), req.OpenBeads[0].ID)
	assert.NoError(t, req.Validate())

	joined := ""
	for _, n := range req.Notes {
		joined += n + "\n"
	}
	assert.Contains(t, joined, "deprioritise", "autopilot may deprioritise its own beads (Q-04)")
}

func TestPlannerBriefRequestCarriesTheBriefAndNoNotes(t *testing.T) {
	f := planningSetup(t, "make the tests pass", nil)
	f.Runner.setPlan(planningPlan("alpha"))
	f.start()
	planningEventually(t, "the first plan", func() bool {
		f.Clock.Advance(time.Second)
		return f.Runner.count() == 1
	})
	req, ok := f.Runner.last()
	require.True(t, ok)
	assert.Equal(t, domain.RunModeBrief, req.Mode)
	assert.Equal(t, "make the tests pass", req.Brief)
	assert.Empty(t, req.Notes)
	assert.NoError(t, req.Validate())
}

func TestPlannerReportsAFailureToListBeads(t *testing.T) {
	rc := planningRunConfig(t, "")
	store := &planningBeadStore{err: errors.New("bd is not answering")}
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: &planningRunner{}, Applier: &planningApplier{},
		Beads: store.source(), NoShim: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	_, err = p.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bd is not answering")
	assert.Equal(t, 1, p.Status().Attempts)
	assert.False(t, p.Status().Running)
}

// --- plumbing -------------------------------------------------------------

func TestNewPlannerNeedsARunConfiguration(t *testing.T) {
	_, err := NewPlanner(PlannerOptions{})
	assert.ErrorIs(t, err, ErrNoPlanConfig)
}

func TestPlannerFireRefusesADemandThatIsNotAPlan(t *testing.T) {
	rc := planningRunConfig(t, "")
	p, err := NewPlanner(PlannerOptions{Config: rc, Runner: &planningRunner{}, Applier: &planningApplier{}, NoShim: true})
	require.NoError(t, err)
	_, err = p.Fire(context.Background(), Firing{
		Demand: Demand{Kind: DemandImplement, Bead: "sc-1"},
		Agent:  domain.Agent{ID: "eng-sc-1"},
	})
	assert.ErrorIs(t, err, ErrNotAPlanDemand)
	assert.Zero(t, p.Status().Attempts)
}

func TestPlannerFireNeedsARunner(t *testing.T) {
	rc := planningRunConfig(t, "")
	p, err := NewPlanner(PlannerOptions{Config: rc, Applier: &planningApplier{}, NoShim: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	require.NoError(t, err)
	_, err = p.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
	assert.ErrorIs(t, err, ErrNoPlanRunner)
}

func TestPlanRouterSendsPlansToThePlannerAndTheRestOnwards(t *testing.T) {
	rc := planningRunConfig(t, "")
	runner := &planningRunner{plan: domain.Plan{}}
	p, err := NewPlanner(PlannerOptions{Config: rc, Runner: runner, Applier: &planningApplier{}, NoShim: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	require.NoError(t, err)

	var others int
	router := PlanRouter(p, FirerFunc(func(context.Context, Firing) (FireResult, error) {
		others++
		return FireResult{}, nil
	}))
	_, err = router.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
	require.NoError(t, err, "an empty autopilot plan is an answer, not a failed agent (D-18)")
	assert.Equal(t, 1, runner.count())
	assert.Equal(t, 1, p.Status().Empty)
	assert.Zero(t, p.Status().Beads)
	assert.Zero(t, others)

	_, err = router.Fire(context.Background(), Firing{Demand: Demand{Kind: DemandReview, Bead: "sc-1"}})
	require.NoError(t, err)
	assert.Equal(t, 1, others)

	_, err = PlanRouter(p, nil).Fire(context.Background(), Firing{Demand: Demand{Kind: DemandReview}})
	assert.ErrorIs(t, err, ErrNoFirer)
}

func TestPlanAgentRunBuildsAValidReadOnlyPlanningSession(t *testing.T) {
	rc := planningRunConfig(t, "ship it")
	cart, err := mock.NewAdapter("claude", mock.NewScenario("plan"))
	require.NoError(t, err)

	repo, err := prompt.ScanRepo(rc.Run.RepoPath, prompt.ScanOptions{})
	require.NoError(t, err)
	req := agents.PlanRequest{
		Run: rc.Run.ID, Mode: domain.RunModeBrief, Brief: rc.Run.Brief, Repo: repo,
		MaxBeads: 5, Env: map[string]string{"PATH": "/shim:/usr/bin"},
	}
	run, err := PlanAgentRun(FiringContext{
		Run:       rc.Run.ID,
		Agent:     domain.Agent{ID: "po-plan-1", Role: domain.RolePO},
		Task:      events.TaskPlan,
		Cartridge: cart,
		Timeout:   17 * time.Minute,
		Env:       map[string]string{"HOME": "/tmp/nowhere"},
		Attempt:   1,
	}, nil, req)
	require.NoError(t, err)
	require.NoError(t, run.Validate())
	assert.Equal(t, domain.MessagePlan, run.Message)
	assert.Equal(t, events.TaskPlan, run.Task)
	assert.Equal(t, rc.Run.RepoPath, run.Spec.Workdir, "the planning product owner reads the repository")
	assert.Equal(t, cartridge.ToolReadOnly, run.Spec.ToolPolicy)
	assert.Equal(t, 17*time.Minute, run.Spec.Timeout)
	assert.Equal(t, "/shim:/usr/bin", run.Spec.Env["PATH"], "the read-only bd shim survives (D-21)")
	assert.Equal(t, "/tmp/nowhere", run.Spec.Env["HOME"])
	assert.Contains(t, run.Spec.Prompt, "ship it")
	require.NotNil(t, run.Check)

	// The rule check is the planning validator: a plan over the cap is
	// refused inside the firing, before the P6-19 policy ever sees it.
	assert.Error(t, run.Check(structured.Parsed{}, `{"type":"plan","brief":"x","beads":[]}`),
		"brief mode must propose at least one bead")
}

func TestSupervisorPlanRunnerNeedsASupervisor(t *testing.T) {
	_, _, err := SupervisorPlanRunner(nil).RunPlan(context.Background(), Firing{}, agents.PlanRequest{})
	assert.ErrorIs(t, err, ErrNoPlanRunner)
}

func TestBeadSettled(t *testing.T) {
	tests := []struct {
		bead domain.Bead
		want bool
	}{
		{planningBead("a", domain.BeadStateClosed), true},
		{planningBead("a", domain.BeadStateNeedsHuman), true},
		{planningBead("a", domain.BeadStateFailed), true},
		{planningBead("a", domain.BeadStateBlocked), true},
		{planningBead("a", domain.BeadStateDeferred), true},
		{planningBead("a", domain.BeadStateOpen), false},
		{planningBead("a", domain.BeadStateInProgress), false},
		{planningBead("a", domain.BeadStateInReview), false},
		{planningBead("a", domain.BeadStateChangesRequested), false},
		{planningBead("a", domain.BeadStateApproved), false},
		{planningBead("a", domain.BeadStateDelivering), false},
		{planningBead("a", domain.BeadStateEscalated), false},
		{domain.Bead{ID: "epic", Type: domain.BeadTypeEpic, State: domain.BeadStateOpen}, true},
		{domain.Bead{ID: "gate", Type: domain.BeadTypeGate, State: domain.BeadStateOpen}, true},
	}
	for _, tc := range tests {
		t.Run(string(tc.bead.ID)+"/"+tc.bead.State.String(), func(t *testing.T) {
			assert.Equal(t, tc.want, BeadSettled(tc.bead))
		})
	}
}

func TestPlanSettlementSummarisesTheRun(t *testing.T) {
	settled, total, summary := planSettlement(nil)
	assert.True(t, settled)
	assert.Zero(t, total)
	assert.Equal(t, "no beads", summary)

	settled, total, summary = planSettlement([]domain.Bead{
		planningBead("a", domain.BeadStateClosed),
		planningBead("b", domain.BeadStateClosed),
		planningBead("c", domain.BeadStateFailed),
		{ID: "epic", Type: domain.BeadTypeEpic, State: domain.BeadStateOpen},
	})
	assert.True(t, settled)
	assert.Equal(t, 3, total)
	assert.Equal(t, "3 beads (2 closed, 1 failed)", summary)

	settled, total, summary = planSettlement([]domain.Bead{
		planningBead("a", domain.BeadStateClosed),
		planningBead("b", domain.BeadStateInReview),
	})
	assert.False(t, settled)
	assert.Equal(t, 2, total)
	assert.Equal(t, "2 beads (1 closed, 1 in_review)", summary)
}

func TestMergeEnvKeepsTheShimPath(t *testing.T) {
	out := mergeEnv(map[string]string{"PATH": "/shim:/usr/bin"}, map[string]string{"PATH": "/usr/bin", "X": "1"})
	assert.Equal(t, map[string]string{"PATH": "/shim:/usr/bin", "X": "1"}, out)
}

func TestPlanningStopConditionIsQuietWhenTheBeadListingFails(t *testing.T) {
	rc := planningRunConfig(t, "ship it")
	store := &planningBeadStore{err: errors.New("bd is busy")}
	clock := newPlanningClock()
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: &planningRunner{}, Applier: &planningApplier{}, Beads: store.source(),
		Now: clock.Now, NoShim: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	planningApply(p, map[string]domain.BeadID{"a": "sc-a"}, nil)

	_, done, err := p.Done(context.Background(), nil, SchedulerStatus{Idle: true})
	require.NoError(t, err, "a listing failure must not fail the run")
	assert.False(t, done)
}

func TestPlanningStopConditionWaitsForTheFirstPlan(t *testing.T) {
	rc := planningRunConfig(t, "ship it")
	store := &planningBeadStore{}
	clock := newPlanningClock()
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: &planningRunner{}, Applier: &planningApplier{}, Beads: store.source(),
		Now: clock.Now, NoShim: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)

	_, done, err := p.Done(context.Background(), nil, SchedulerStatus{Idle: true})
	require.NoError(t, err)
	assert.False(t, done, "a brief run cannot finish before it has planned")

	planningApply(p, map[string]domain.BeadID{"a": "sc-a"}, nil)
	clock.Advance(time.Minute)
	_, done, err = p.Done(context.Background(), nil, SchedulerStatus{Idle: true})
	require.NoError(t, err)
	assert.False(t, done, "bd has not listed the new bead yet")

	store.set(planningBead("sc-a", domain.BeadStateClosed))
	clock.Advance(time.Minute)
	out, done, err := p.Done(context.Background(), nil, SchedulerStatus{Idle: true})
	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, events.FinishCompleted, out.Reason)

	// A live agent keeps the run going whatever the beads say.
	clock.Advance(time.Minute)
	_, done, err = p.Done(context.Background(), nil, SchedulerStatus{Live: 1})
	require.NoError(t, err)
	assert.False(t, done)
}

// planningRunConfig builds a run configuration for a unit test that needs no
// run of its own. brief empty means autopilot.
func planningRunConfig(t *testing.T, brief string) *config.RunConfig {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	repo := filepath.Join(home, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	cfg := config.Defaults()
	cfg.ActiveCartridge = "claude"
	cfg.Cartridges = map[string]config.Cartridge{"claude": {Model: "opus"}}
	eff := &config.Effective{Config: cfg, Sources: map[string]config.Source{}}
	rc, err := config.NewRunConfig(repo, eff, config.RunSettings{
		ID: "20260903-120000-abcd", Brief: brief, StartedAt: planningEpoch,
	})
	require.NoError(t, err)
	return rc
}
