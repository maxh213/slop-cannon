package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// setNext scripts the product owner per firing: n is the firing's number,
// counting from 1, and the function may read [planningRunner.last] to see
// the request it is answering.
func (r *planningRunner) setNext(fn func(n int) (domain.Plan, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next = fn
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

// titles are the bead titles of the nth plan the applier was given
// (counting from 1), in plan order.
func (a *planningApplier) titles(n int) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n < 1 || n > len(a.calls) {
		return nil
	}
	out := make([]string, 0, len(a.calls[n-1].Plan.Beads))
	for _, b := range a.calls[n-1].Plan.Beads {
		out = append(out, b.Title)
	}
	return out
}

// planningBeadStore is the mutable bead listing behind a [BeadSource].
type planningBeadStore struct {
	mu    sync.Mutex
	list  []domain.Bead
	err   error
	calls int
}

func (s *planningBeadStore) source() BeadSource {
	return func(context.Context) ([]domain.Bead, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls++
		if s.err != nil {
			return nil, s.err
		}
		return append([]domain.Bead(nil), s.list...), nil
	}
}

func (s *planningBeadStore) set(list ...domain.Bead) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list, s.err = list, nil
}

// fail makes every later listing return err; nil heals the store.
func (s *planningBeadStore) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// count is how many listings the store has been asked for.
func (s *planningBeadStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
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
		budget := schedWait(schedPollBudget)
		select {
		case <-run.Done():
		case <-time.After(budget):
			t.Errorf("the run did not finish %s", schedContention(budget))
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
	budget := schedWait(schedPollBudget)
	select {
	case <-f.Run.Done():
	case <-time.After(budget):
		f.t.Fatalf("the run did not finish %s", schedContention(budget))
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
// planningEventually polls cond, which is expected to drive the fake clock
// itself. A condition must be written as "at least", never as an exact
// count: the clock jumps a second per poll, so on a loaded machine an
// autopilot planner can get several idle re-plans between two polls and an
// equality would never be observed.
// The deadline is load-scaled the way every wait in these files is
// (P6-56, [schedWait]): a planning round that has not landed yet on a
// machine running four race-instrumented copies of this package is not the
// same thing as a planner that is stuck.
func planningEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	budget := schedWait(schedPollBudget)
	deadline := time.Now().Add(budget)
	for {
		if cond() {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("timed out waiting for %s %s", what, schedContention(budget))
		}
		time.Sleep(time.Millisecond)
	}
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
		return f.Applier.count() >= 1
	})
	// The beads exist and are still open, so the run must not finish.
	f.Beads.set(planningBead("sc-alpha", domain.BeadStateOpen), planningBead("sc-beta", domain.BeadStateInReview))
	for i := 0; i < 5; i++ {
		f.Clock.Advance(3 * time.Second)
		schedPause(2 * time.Millisecond)
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
		return f.Applier.count() >= 1
	})
	// bd has not listed the new bead yet, so the run stays live rather
	// than finishing on an empty listing.
	for i := 0; i < 5; i++ {
		f.Clock.Advance(3 * time.Second)
		schedPause(2 * time.Millisecond)
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
		return f.Runner.count() >= 1
	})
	assert.Equal(t, 0, f.Applier.count(), "an empty plan creates nothing")

	const day = 24 * time.Hour
	replans := int(day / config.DefaultIdleReplanInterval)
	for i := 1; i <= replans; i++ {
		want := i + 1
		// Wait on the planner's own record, not on the runner's call
		// counter: the counter is incremented inside the runner, so a
		// firing that has been made but not yet recorded would let the
		// loop — and the Attempts/Empty assertion below — run against a
		// planning round that is still in flight.
		planningEventually(t, fmt.Sprintf("re-plan %d", i), func() bool {
			f.Clock.Advance(config.DefaultIdleReplanInterval)
			return f.Planner.Status().Empty >= want
		})
	}
	assert.GreaterOrEqual(t, f.Runner.count(), replans, "autopilot re-plans every idle_replan_interval")
	assert.Equal(t, PhaseRunning, f.Run.Phase(), "autopilot never stops on its own (D-18)")
	st := f.Planner.Status()
	// Plans counts finished planning rounds and Empty the ones that
	// returned nothing, so comparing those two says "every plan that came
	// back was empty" without racing the round that is still in flight —
	// Attempts is incremented when a firing starts, not when it lands.
	assert.Equal(t, st.Plans, st.Empty, "every plan was empty")
	assert.GreaterOrEqual(t, st.Attempts, st.Empty)
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
		return f.Runner.count() >= 1
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
		return f.Applier.count() >= 1
	})
	// Beads exist and are open: brief mode would finish here, autopilot
	// must not.
	f.Beads.set(planningBead("sc-alpha", domain.BeadStateClosed))
	for i := 0; i < 5; i++ {
		f.Clock.Advance(3 * time.Second)
		schedPause(2 * time.Millisecond)
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

	status = SchedulerStatus{Live: 1, Quiet: true, ByRole: map[domain.Role]int{domain.RoleEngineer: 1}}
	ds, err = p.Demands(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ds, "an engineer is still carrying out the last plan")

	status = SchedulerStatus{Live: 0, Idle: false, Quiet: false}
	ds, err = p.Demands(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ds, "the last dispatch still found work to fire")

	// Fired moves on: the scheduler has started an agent for the plan, so
	// the P8-28 watermark is behind us and the queue really is empty.
	status = SchedulerStatus{Live: 0, Idle: true, Quiet: true, Fired: 1}
	ds, err = p.Demands(context.Background())
	require.NoError(t, err)
	assert.Len(t, ds, 1, "the ready queue is empty and nothing is running: re-plan (D-18)")
	assert.Contains(t, ds, planDemand())
}

// TestPlannerAutopilotReplansWhileANonEngineerIsStillRunning pins P6-05's
// own words — "whenever the ready queue is empty and no engineer is
// running" — rather than the stricter "the run is completely quiescent" the
// first implementation used, which left every free slot idle behind a
// single tester.
func TestPlannerAutopilotReplansWhileANonEngineerIsStillRunning(t *testing.T) {
	rc := planningRunConfig(t, "")
	clock := newPlanningClock()
	status := SchedulerStatus{}
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: &planningRunner{plan: planningPlan("alpha")},
		Applier: &planningApplier{}, Now: clock.Now, NoShim: true,
		Status: func() SchedulerStatus { return status },
	})
	require.NoError(t, err)
	planningApply(p, map[string]domain.BeadID{"alpha": "sc-alpha"}, nil)

	for _, tc := range []struct {
		name string
		role domain.Role
		want int
	}{
		{name: "a tester reviewing one bead does not hold planning up", role: domain.RoleTester, want: 1},
		{name: "nor does the run's own product owner", role: domain.RolePO, want: 1},
		{name: "an engineer does", role: domain.RoleEngineer, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Quiet without Idle is the state the gap describes: one agent
			// is live, the ready queue behind it is empty, and eight of the
			// ten slots are free.
			status = SchedulerStatus{
				Live: 1, Idle: false, Quiet: true, Fired: 1,
				Caps:   SlotCaps{Ammo: 10, PO: 1},
				ByRole: map[domain.Role]int{tc.role: 1},
			}
			ds, err := p.Demands(context.Background())
			require.NoError(t, err)
			assert.Len(t, ds, tc.want)
		})
	}
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
		return f.Runner.count() >= 1
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
		return f.Runner.count() >= 1
	})
	req, ok := f.Runner.last()
	require.True(t, ok)
	assert.Equal(t, domain.RunID("20260903-120000-abcd"), req.Run)
	assert.Equal(t, domain.RunModeAutopilot, req.Mode)
	assert.Empty(t, req.Brief)
	assert.Equal(t, domain.AgentID("po-plan-1"), req.AgentID)
	assert.Equal(t, f.Repo, req.Repo.Root)
	assert.Equal(t, "opus", req.Model)
	require.Len(t, req.OpenBeads, 2, "the epic is bookkeeping, not work to avoid")
	assert.Equal(t, domain.BeadID("sc-open"), req.OpenBeads[0].ID, "open beads come first")
	assert.Equal(t, domain.BeadID("sc-done"), req.OpenBeads[1].ID,
		"and the run's finished work comes after it, so the product owner remembers it (P8-29)")
	assert.Equal(t, domain.BeadStateClosed, req.OpenBeads[1].State, "marked closed")
	assert.NoError(t, req.Validate())

	assert.Equal(t, []string{planDoneNote}, req.Notes,
		"the only note on a first autopilot plan is the one about the finished work (P8-29): "+
			"there is nothing to reprioritise yet (Q-04)")
}

func TestPlannerBriefRequestCarriesTheBriefAndNoNotes(t *testing.T) {
	f := planningSetup(t, "make the tests pass", nil)
	f.Runner.setPlan(planningPlan("alpha"))
	f.start()
	planningEventually(t, "the first plan", func() bool {
		f.Clock.Advance(time.Second)
		return f.Runner.count() >= 1
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
		// A state that is not a bd status at all is never settled
		// (P6-36): a malformed listing must not finish a brief run.
		{planningBead("a", ""), false},
		{planningBead("a", domain.BeadState("wat")), false},
		{planningBead("a", BeadStateUnrecorded), false},
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

// --- D-21: the planning product owner's read-only bd shim -----------------

// planningFakeBD writes a stand-in for the real bd binary, so the shim can
// be generated and exercised without a bd on this machine.
func planningFakeBD(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "fakebin")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "bd")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\necho \"real bd: $*\"\n"), 0o700)) //nolint:gosec // test fixture
	return path
}

// planningFailingShim is a [PlanShimInstaller] that fails until a test lets
// it succeed, so the retry policy can be observed.
type planningFailingShim struct {
	mu    sync.Mutex
	calls int
	err   error
	real  string
}

func (s *planningFailingShim) install(_ context.Context, dir, _ string, _ beads.ShimOptions) (beads.Shim, error) {
	s.mu.Lock()
	s.calls++
	err := s.err
	real := s.real
	s.mu.Unlock()
	if err != nil {
		return beads.Shim{}, err
	}
	return beads.WriteShim(dir, beads.ShimOptions{Real: real})
}

func (s *planningFailingShim) heal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = nil
}

func (s *planningFailingShim) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestPlannerFiresTheProductOwnerBehindTheReadOnlyBDShim(t *testing.T) {
	fake := planningFakeBD(t)
	f := planningSetup(t, "ship the thing", func(o *PlannerOptions) {
		o.NoShim = false
		o.ShimOptions = beads.ShimOptions{Real: fake, SkipVerify: true}
	})
	f.Runner.setPlan(planningPlan("alpha"))
	f.Beads.set(planningBead("sc-alpha", domain.BeadStateOpen))
	f.start()

	planningEventually(t, "the first plan", func() bool {
		f.Clock.Advance(time.Second)
		return f.Runner.count() >= 1
	})
	req, ok := f.Runner.last()
	require.True(t, ok)

	// The planning agent is the one agent with no throw-away worktree: it
	// runs in the user's own repository (PlanAgentRun sets the workdir to
	// it), so the shim is the only thing between the product owner and the
	// user's beads database.
	assert.Equal(t, f.Repo, req.Dir(), "the planning product owner reads the repository itself")

	shimDir := filepath.Join(f.Run.Dirs().Shims, PlanShimDirName)
	path := req.Env["PATH"]
	require.NotEmpty(t, path, "the firing carries a PATH")
	assert.Equal(t, shimDir, strings.SplitN(path, string(os.PathListSeparator), 2)[0],
		"the product owner's PATH starts with the shim directory")
	assert.False(t, strings.HasPrefix(shimDir, f.Repo+string(os.PathSeparator)),
		"the shim lives beside the run, never inside the repository (D-23)")

	shimPath := filepath.Join(shimDir, "bd")
	// A read reaches the real binary, always with --readonly in front...
	out, err := exec.Command(shimPath, "ready").CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "real bd: --readonly ready")

	// ...and every write is refused before bd ever runs (P3-09, D-21).
	for _, args := range [][]string{
		{"create", "sneaky bead"},
		{"update", "sc-alpha", "-s", "closed"},
		{"--readonly=false", "create", "sneakier bead"},
	} {
		out, err = exec.Command(shimPath, args...).CombinedOutput() //nolint:gosec // fixed test arguments
		require.Error(t, err, "a write through the shim must fail: %s", out)
		assert.NotContains(t, string(out), "real bd:", "bd never saw `%s`", strings.Join(args, " "))
		assert.Contains(t, string(out), "is not allowed in read-only mode")
	}
}

func TestInstallPlanShimWritesTheShimAndVerifiesIt(t *testing.T) {
	fake := planningFakeBD(t)
	dir := filepath.Join(t.TempDir(), "po-plan")
	repo := t.TempDir()

	shim, err := InstallPlanShim(context.Background(), dir, repo, beads.ShimOptions{Real: fake, SkipVerify: true})
	require.NoError(t, err)
	assert.Equal(t, dir, shim.Dir)
	assert.Equal(t, filepath.Join(dir, "bd"), shim.Path)
	assert.True(t, beads.IsShim(shim.Path), "the file it wrote is a slop-cannon shim")

	// The self-check runs when it is not skipped, and a bd that answers
	// nothing useful fails it rather than being handed to an agent.
	_, err = InstallPlanShim(context.Background(), filepath.Join(t.TempDir(), "po-plan"), repo,
		beads.ShimOptions{Real: fake})
	require.Error(t, err, "a bd whose `version --json` prints no version cannot be verified")
	assert.Contains(t, err.Error(), "shim")
}

func TestPlannerRetriesAShimInstallThatFailed(t *testing.T) {
	installer := &planningFailingShim{err: errors.New("no space left on device"), real: planningFakeBD(t)}
	f := planningSetup(t, "", func(o *PlannerOptions) {
		o.NoShim = false
		o.Shim = installer.install
	})
	f.Runner.setPlan(domain.Plan{Brief: "nothing for now"})
	f.start()

	planningEventually(t, "two failed shim installs", func() bool {
		f.Clock.Advance(config.DefaultIdleReplanInterval)
		return installer.count() >= 2
	})
	assert.Zero(t, f.Runner.count(), "no product owner is fired without the shim it was promised")
	st := f.Planner.Status()
	assert.Contains(t, st.LastError, "no space left on device")
	assert.Contains(t, st.LastError, "in a row", "a repeated failure says how many times it has happened")
	assert.False(t, st.Finished, "autopilot never stops on its own (D-18)")

	// The condition clears, and the very next round installs the shim and
	// fires: a failure is never cached for the life of the run.
	installer.heal()
	planningEventually(t, "the planner to recover", func() bool {
		f.Clock.Advance(config.DefaultIdleReplanInterval)
		return f.Runner.count() >= 1
	})
	req, ok := f.Runner.last()
	require.True(t, ok)
	assert.Contains(t, req.Env["PATH"], PlanShimDirName)
}

// --- P6-05: the rules and vibes documents ---------------------------------

// planningDoc writes a rules or vibes file and returns its path.
func planningDoc(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestPlannerLoadsTheDocumentTheCallerDidNotSupply(t *testing.T) {
	rulesPath := planningDoc(t, "rules.md", "- Never delete a test.\n")
	vibesPath := planningDoc(t, "vibes.md", "- Short functions.\n")
	supplied := []prompt.Doc{{Name: "given.md", Content: "- Supplied by the caller.\n"}}

	tests := []struct {
		name               string
		rules, vibes       []prompt.Doc
		wantRule, wantVibe string
	}{
		{name: "neither supplied", wantRule: "rules.md", wantVibe: "vibes.md"},
		{name: "rules supplied", rules: supplied, wantRule: "given.md", wantVibe: "vibes.md"},
		{name: "vibes supplied", vibes: supplied, wantRule: "rules.md", wantVibe: "given.md"},
		{name: "both supplied", rules: supplied, vibes: supplied, wantRule: "given.md", wantVibe: "given.md"},
		{name: "empty means none", rules: []prompt.Doc{}, vibes: []prompt.Doc{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc := planningRunConfig(t, "")
			rc.Effective.Rules = []string{rulesPath}
			rc.Effective.Vibes = []string{vibesPath}
			p, err := NewPlanner(PlannerOptions{
				Config: rc, Runner: &planningRunner{}, Applier: &planningApplier{},
				Rules: tc.rules, Vibes: tc.vibes, NoShim: true,
			})
			require.NoError(t, err)
			rules, vibes, err := p.docs()
			require.NoError(t, err)
			if tc.wantRule == "" {
				assert.Empty(t, rules, "a non-nil empty slice means the caller says there are none")
				assert.Empty(t, vibes)
				return
			}
			require.Len(t, rules, 1)
			require.Len(t, vibes, 1)
			assert.Equal(t, tc.wantRule, rules[0].Name)
			assert.Equal(t, tc.wantVibe, vibes[0].Name,
				"supplying one document must never lose the other")
		})
	}
}

func TestPlannerRetriesAnUnreadableRulesFile(t *testing.T) {
	rc := planningRunConfig(t, "")
	path := filepath.Join(t.TempDir(), "rules.md")
	rc.Effective.Rules = []string{path}
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: &planningRunner{}, Applier: &planningApplier{}, NoShim: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)

	_, _, err = p.docs()
	require.Error(t, err, "the rules are hard constraints: a plan made without them is not the plan the user asked for")
	assert.Contains(t, err.Error(), "rules")

	// The failure is not cached: the file appears and the next round works.
	require.NoError(t, os.WriteFile(path, []byte("- Never delete a test.\n"), 0o600))
	rules, _, err := p.docs()
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, "rules.md", rules[0].Name)
}

// --- Q-04: the product owner deprioritises its own open beads -------------

// planningPriorityCall is one bd priority update the planner made.
type planningPriorityCall struct {
	Bead     domain.BeadID
	Priority domain.Priority
}

// planningPrioritiser is a fake [PlanPrioritiser].
type planningPrioritiser struct {
	mu    sync.Mutex
	calls []planningPriorityCall
	err   error
}

func (p *planningPrioritiser) Update(_ context.Context, id domain.BeadID, patch beads.Patch) (beads.Issue, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return beads.Issue{}, p.err
	}
	if patch.Priority == nil {
		return beads.Issue{}, fmt.Errorf("the planner must only ever set a priority, got %+v", patch)
	}
	p.calls = append(p.calls, planningPriorityCall{Bead: id, Priority: *patch.Priority})
	// bd echoes the updated issue back, and the planner publishes it.
	return beads.Issue{ID: id, Status: string(domain.BeadStateOpen), Priority: *patch.Priority}, nil
}

func (p *planningPrioritiser) applied() []planningPriorityCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]planningPriorityCall(nil), p.calls...)
}

// planningDeprioritiseFixture is a planner whose product owner returns plan
// over an open bead the run proposed earlier ("Do alpha", priority 1).
func planningDeprioritiseFixture(t *testing.T, brief string, plan domain.Plan, withPrioritiser bool) (*Planner, *planningApplier, *planningPrioritiser) {
	t.Helper()
	rc := planningRunConfig(t, brief)
	store := &planningBeadStore{}
	store.set(domain.Bead{
		ID: "sc-old", Title: "Do alpha", Type: domain.BeadTypeTask,
		State: domain.BeadStateOpen, Priority: domain.Priority(1),
	})
	applier := &planningApplier{}
	pr := &planningPrioritiser{}
	opts := PlannerOptions{
		Config: rc, Runner: &planningRunner{plan: plan}, Applier: applier, Beads: store.source(),
		NoShim: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if withPrioritiser {
		opts.Prioritiser = pr
	}
	p, err := NewPlanner(opts)
	require.NoError(t, err)
	return p, applier, pr
}

// planningRestate returns a plan that repeats the open bead "Do alpha" at
// priority want, alongside the genuinely new beads named by extra.
func planningRestate(want domain.Priority, extra ...string) domain.Plan {
	plan := planningPlan(append([]string{"alpha"}, extra...)...)
	plan.Beads[0].Priority = want
	return plan
}

func TestPlannerDeprioritisesAnOpenBeadTheProductOwnerRestates(t *testing.T) {
	p, applier, pr := planningDeprioritiseFixture(t, "", planningRestate(domain.PriorityLowest, "beta"), true)
	_, err := p.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
	require.NoError(t, err)

	// The restated bead was not created a second time...
	require.Equal(t, 1, applier.count())
	applier.mu.Lock()
	created := applier.calls[0].Plan.Beads
	applier.mu.Unlock()
	require.Len(t, created, 1, "only the genuinely new bead is created")
	assert.Equal(t, "beta", created[0].Key)

	// ...its priority was lowered instead (Q-04).
	assert.Equal(t, []planningPriorityCall{{Bead: "sc-old", Priority: domain.PriorityLowest}}, pr.applied())
	assert.Equal(t, 1, p.Status().Deprioritised)
	assert.Equal(t, 1, p.Status().Beads, "the deprioritised bead is not a new bead")
}

func TestPlannerAppliesADeprioritisationEvenWhenThePlanProposesNothingElse(t *testing.T) {
	p, applier, pr := planningDeprioritiseFixture(t, "", planningRestate(domain.Priority(3)), true)
	_, err := p.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
	require.NoError(t, err, "an empty autopilot plan is an answer, not a failed agent (D-18)")
	assert.Zero(t, applier.count(), "nothing is created")
	assert.Equal(t, []planningPriorityCall{{Bead: "sc-old", Priority: domain.Priority(3)}}, pr.applied())
	assert.Equal(t, 1, p.Status().Deprioritised)
	assert.Equal(t, 1, p.Status().Empty)
}

func TestPlannerRefusesARestatementThatIsNotADeprioritisation(t *testing.T) {
	tests := []struct {
		name            string
		plan            domain.Plan
		brief           string
		withPrioritiser bool
	}{
		{name: "same priority is a plain duplicate", plan: planningRestate(domain.Priority(1)), withPrioritiser: true},
		{name: "more urgent is a plain duplicate", plan: planningRestate(domain.PriorityHighest), withPrioritiser: true},
		{name: "brief mode never deprioritises", plan: planningRestate(domain.PriorityLowest), brief: "ship it", withPrioritiser: true},
		{name: "no prioritiser, no deprioritisation", plan: planningRestate(domain.PriorityLowest)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, applier, pr := planningDeprioritiseFixture(t, tc.brief, tc.plan, tc.withPrioritiser)
			_, err := p.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
			require.Error(t, err)
			assert.ErrorIs(t, err, agents.ErrPlanRejected, "a repeat that is not a demotion is still a duplicate")
			assert.Contains(t, err.Error(), "duplicates open bead sc-old")
			assert.Zero(t, applier.count())
			assert.Empty(t, pr.applied())
			assert.Zero(t, p.Status().Deprioritised)
		})
	}
}

func TestPlannerKeepsARestatedBeadThatTheRestOfThePlanDependsOn(t *testing.T) {
	plan := planningRestate(domain.PriorityLowest, "beta")
	plan.Beads[1].Deps = []string{"alpha"}
	p, applier, pr := planningDeprioritiseFixture(t, "", plan, true)
	_, err := p.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, agents.ErrPlanRejected,
		"dropping a bead another one depends on would dangle the dependency; the validator rules on it instead")
	assert.Zero(t, applier.count())
	assert.Empty(t, pr.applied())
}

func TestPlannerWarnsWhenBdRefusesADeprioritisation(t *testing.T) {
	p, applier, pr := planningDeprioritiseFixture(t, "", planningRestate(domain.PriorityLowest, "beta"), true)
	pr.mu.Lock()
	pr.err = errors.New("bd is locked")
	pr.mu.Unlock()

	_, err := p.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
	require.NoError(t, err, "a refused priority update never undoes the beads that were created")
	assert.Equal(t, 1, applier.count())
	assert.Zero(t, p.Status().Deprioritised)
}

func TestPlannerTellsTheProductOwnerHowToDeprioritise(t *testing.T) {
	plan := domain.Plan{Brief: "nothing for now"}
	p, _, _ := planningDeprioritiseFixture(t, "", plan, true)
	_, err := p.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
	require.NoError(t, err)
	runner, ok := p.opts.Runner.(*planningRunner)
	require.True(t, ok)
	req, ok := runner.last()
	require.True(t, ok)
	joined := strings.Join(req.Notes, "\n")
	assert.Contains(t, joined, "deprioritise", "autopilot may deprioritise its own beads (Q-04)")
	assert.Contains(t, joined, "larger `priority` number", "the note describes the convention the planner implements")
	assert.Contains(t, joined, "`sc-old` 1", "the product owner is told the priority each open bead has now")

	// A planner that cannot reprioritise never offers to.
	q, _, _ := planningDeprioritiseFixture(t, "", plan, false)
	_, err = q.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
	require.NoError(t, err)
	quiet, ok := q.opts.Runner.(*planningRunner)
	require.True(t, ok)
	req, ok = quiet.last()
	require.True(t, ok)
	assert.NotContains(t, strings.Join(req.Notes, "\n"), "deprioritise",
		"an instruction the orchestrator would drop on the floor is never given")
}

// --- P6-19: a plan the validator really rejects ---------------------------

// planningCyclicPlan is schema-valid and rule-invalid: a depends on b and b
// on a, which is the rejection domain.Plan.Validate finds and no JSON schema
// can express.
func planningCyclicPlan() domain.Plan {
	plan := planningPlan("a", "b")
	plan.Beads[0].Deps = []string{"b"}
	plan.Beads[1].Deps = []string{"a"}
	return plan
}

func TestPlannerDropsACyclicPlanItsOwnValidatorRejects(t *testing.T) {
	f := planningSetup(t, "ship the thing", nil)
	// The product owner answers with a cyclic plan every time: nothing is
	// injected by hand, so the planner's own second-line validator
	// (agents.CreatePlanBeads -> ValidatePlan) is what rejects it.
	f.Runner.setPlan(planningCyclicPlan())
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
	assert.Equal(t, 2, f.Runner.count(), "the brief is retried once, then dropped")
	assert.Zero(t, f.Applier.count(), "a cyclic plan creates nothing")
	assert.Contains(t, f.Planner.Status().LastError, "dependency cycle: a -> b -> a")
	assert.Equal(t, 1, f.Planner.Status().GaveUp)
}

// planningSupervisor builds a real agent supervisor over the mock cartridge
// and attaches it to the run, so a planning firing goes through the
// production path: FireAgent -> agents.RunAgent -> PlanCheck ->
// ParsePlanOutput. The adapter is registered as "mock" because that is the
// name agents.EnforcementFor knows holds read_only in-process; an adapter
// under any other name would need the unimplemented copied-worktree
// degrade, and a planning firing has no worktree to copy.
func planningSupervisor(t *testing.T, f *planningFixture, sc *mock.Scenario) *Supervisor {
	t.Helper()
	adapter, err := mock.NewAdapter("mock", sc)
	require.NoError(t, err)
	cfg := f.Eff.Config
	cfg.ActiveCartridge = "mock"
	cfg.Cartridges = map[string]config.Cartridge{"mock": {Model: "opus"}}
	sel, err := cartridge.NewSelector(cfg,
		map[string]cartridge.Cartridge{"mock": adapter}, cartridge.WithClock(f.Clock.Now))
	require.NoError(t, err)
	sup, err := NewSupervisor(SupervisorOptions{
		Cartridges: sel, Bus: f.Bus, Timeout: time.Minute, Now: f.Clock.Now, NoShims: true,
		Sleep:  func(context.Context, time.Duration) error { return nil },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	sup.Attach(f.Run)
	t.Cleanup(func() { _ = sup.Close() })
	return sup
}

func TestPlannerFailsABriefWhoseProductOwnerAlwaysReturnsACyclicPlan(t *testing.T) {
	// P6-19 end to end, with nothing stubbed between the model's words and
	// the policy: a mock cartridge that always answers with a cyclic plan,
	// fired through the real supervisor. The rejection therefore reaches
	// Planner.record over the production error chain — *agents.AgentError
	// wrapping *structured.ParseError wrapping agents.ErrPlanRejected —
	// which is what record's errors.Is depends on.
	var sup *Supervisor
	var errs []error
	var mu sync.Mutex
	f := planningSetup(t, "ship the thing", func(o *PlannerOptions) {
		o.Runner = PlanRunnerFunc(func(ctx context.Context, fr Firing, req agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
			plan, tr, err := SupervisorPlanRunner(sup).RunPlan(ctx, fr, req)
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
			return plan, tr, err
		})
	})
	sc := mock.NewScenario("po-plan")
	sc.Otherwise("cyclic").Say("Here is the plan.\n").Structured(
		`{"brief":"a and b need each other","beads":[` +
			`{"key":"a","title":"A","acceptance":"a works","deps":["b"]},` +
			`{"key":"b","title":"B","acceptance":"b works","deps":["a"]}]}`)
	sup = planningSupervisor(t, f, sc)
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
	assert.ErrorIs(t, err, ErrPlanningGaveUp, "brief mode has no other way to scope the run (P6-19)")
	assert.Equal(t, events.FinishFailed, out.Reason)
	assert.Zero(t, f.Applier.count(), "a rejected plan creates nothing")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, errs, 2, "the plan is dropped after the second rejection in a row")
	for i, fired := range errs {
		require.Error(t, fired, "firing %d", i)
		assert.ErrorIs(t, fired, agents.ErrPlanRejected,
			"the sentinel must survive every wrapper, or P6-19 degrades into the cool-off branch")
		var agentErr *agents.AgentError
		require.ErrorAs(t, fired, &agentErr)
		assert.Equal(t, 2, agentErr.Attempts, "P5-08 retries a rule failure once inside the firing")
		var parseErr *structured.ParseError
		require.ErrorAs(t, fired, &parseErr)
		assert.Contains(t, parseErr.Feedback(), "dependency cycle: a -> b -> a")
	}
}

// --- P6-11: a plan that landed in bd but never reached state.json ---------

func TestPlannerAdoptsThePlanBdAlreadyHoldsOnResume(t *testing.T) {
	f := planningSetup(t, "ship the thing", func(o *PlannerOptions) { o.Config.Resume = true })
	// The kill landed between `bd create --graph` and Run.SetEpic, so bd
	// holds the epic and its beads while state.json still says the run
	// never planned.
	require.Empty(t, f.Run.Epic())
	f.Beads.set(
		domain.Bead{ID: "sc-epic", Type: domain.BeadTypeEpic, State: domain.BeadStateOpen},
		planningBead("sc-alpha", domain.BeadStateOpen),
	)

	res, err := f.Planner.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
	require.NoError(t, err)
	assert.Equal(t, domain.MessagePlan, res.Produced)
	assert.Zero(t, f.Runner.count(), "a resumed brief run must not plan its brief a second time")
	assert.Zero(t, f.Applier.count())
	assert.Equal(t, domain.BeadID("sc-epic"), f.Run.Epic(), "the epic bd holds is recorded on the run")

	st := f.Planner.Status()
	assert.Equal(t, 1, st.Plans)
	assert.True(t, st.ResumedPlan)

	ds, err := f.Planner.Demands(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ds, "brief mode plans once")
}

func TestPlannerPlansWhenAResumedRunHasNoBeadsInBd(t *testing.T) {
	f := planningSetup(t, "ship the thing", func(o *PlannerOptions) { o.Config.Resume = true })
	f.Runner.setPlan(planningPlan("alpha"))
	// bd has the run's epic but no work beads at all, which is not a plan
	// that landed: `bd create --graph` creates both or neither (D-22).
	f.Beads.set(domain.Bead{ID: "sc-epic", Type: domain.BeadTypeEpic, State: domain.BeadStateOpen})

	_, err := f.Planner.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
	require.NoError(t, err)
	assert.Equal(t, 1, f.Runner.count(), "the interrupted run never planned, so it plans now")
	assert.Equal(t, 1, f.Applier.count())
}

func TestPlannerDoesNotLookForAnAdoptedPlanWhenTheRunIsNotResumed(t *testing.T) {
	f := planningSetup(t, "ship the thing", nil)
	f.Runner.setPlan(planningPlan("alpha"))
	f.Beads.set(
		domain.Bead{ID: "sc-epic", Type: domain.BeadTypeEpic, State: domain.BeadStateOpen},
		planningBead("sc-alpha", domain.BeadStateOpen),
	)
	_, err := f.Planner.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
	require.NoError(t, err)
	assert.Equal(t, 1, f.Runner.count(), "a fresh run always plans, whatever bd already holds")
}

func TestInstallPlanShimReportsADirectoryItCannotWrite(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocked, []byte("in the way\n"), 0o600))
	_, err := InstallPlanShim(context.Background(), filepath.Join(blocked, "po-plan"), t.TempDir(),
		beads.ShimOptions{Real: planningFakeBD(t), SkipVerify: true})
	require.Error(t, err, "a shim that cannot be written is never silently skipped")
}

func TestPlanPrioritiesIsBounded(t *testing.T) {
	assert.Empty(t, planPriorities(nil))
	assert.Equal(t, "`sc-1` 2, `sc-2` 0", planPriorities([]domain.Bead{
		{ID: "sc-1", Priority: domain.PriorityDefault},
		{ID: "sc-2", Priority: domain.PriorityHighest},
	}))

	open := make([]domain.Bead, planPrioritiesShown+5)
	for i := range open {
		open[i] = domain.Bead{ID: domain.BeadID(fmt.Sprintf("sc-%d", i)), Priority: domain.PriorityDefault}
	}
	got := planPriorities(open)
	assert.Equal(t, planPrioritiesShown+1, strings.Count(got, ", ")+1,
		"a run with hundreds of open beads cannot crowd out the planning prompt")
	assert.True(t, strings.HasSuffix(got, "…"), got)
}

func TestPlannerReportsItsMode(t *testing.T) {
	brief, err := NewPlanner(PlannerOptions{Config: planningRunConfig(t, "ship it")})
	require.NoError(t, err)
	assert.Equal(t, domain.RunModeBrief, brief.Mode())

	auto, err := NewPlanner(PlannerOptions{Config: planningRunConfig(t, "")})
	require.NoError(t, err)
	assert.Equal(t, domain.RunModeAutopilot, auto.Mode())
}

// --- P6-19: planning that keeps failing for a reason that is not the plan --

// TestPlannerBriefModeGivesUpWhenPlanningItselfKeepsFailing is the brief-mode
// terminal condition for a planner that never gets as far as a plan: a
// cartridge that crashes, an agent that times out, a rules file that cannot
// be read. A rejected plan already ended such a run (P6-19); without a cap
// on the other failures a brief run fired the product owner every idle
// interval for ever, and only the user could end it.
func TestPlannerBriefModeGivesUpWhenPlanningItselfKeepsFailing(t *testing.T) {
	f := planningSetup(t, "ship the thing", nil)
	f.Runner.setErr(errors.New("the cartridge crashed"))
	f.start()

	planningEventually(t, "the brief run to give up on planning that never works", func() bool {
		f.Clock.Advance(time.Minute)
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
	assert.Contains(t, err.Error(), "the cartridge crashed", "the finish names what kept going wrong")
	assert.Equal(t, events.FinishFailed, out.Reason)
	assert.Contains(t, out.Summary, "could not produce a usable plan")

	st := f.Planner.Status()
	assert.Equal(t, DefaultPlanFailureLimit, f.Runner.count(), "the firings are bounded, not endless")
	assert.Equal(t, DefaultPlanFailureLimit, st.Attempts)
	assert.Equal(t, 1, st.GaveUp)
	assert.Zero(t, st.Failures, "the counter resets when the round is dropped")
	assert.Zero(t, st.Plans)
	assert.True(t, st.Finished)
	assert.Zero(t, f.Applier.count())

	var runs bool
	for _, ev := range f.events() {
		if w, ok := ev.(events.Warning); ok && w.Source == SourcePlanning &&
			strings.Contains(w.Message, "in a row") {
			runs = true
		}
	}
	assert.True(t, runs, "the warnings say how long the failure has been going on")
}

// TestPlannerAutopilotBacksOffWhenPlanningKeepsFailingAndComesBack is the
// same policy in autopilot, where D-18 forbids the run ending: the round is
// dropped, the planner cools off, and it fires again.
func TestPlannerAutopilotBacksOffWhenPlanningKeepsFailingAndComesBack(t *testing.T) {
	f := planningSetup(t, "", nil)
	f.Runner.setErr(errors.New("the cartridge crashed"))
	f.start()

	planningEventually(t, "the planner to give up on the round", func() bool {
		f.Clock.Advance(time.Minute)
		return f.Planner.Status().GaveUp >= 1
	})
	st := f.Planner.Status()
	assert.False(t, st.Finished, "autopilot never stops on its own (D-18)")
	assert.False(t, st.NextAt.IsZero(), "the next round is scheduled, not immediate")
	assert.Zero(t, st.Failures)

	before := f.Runner.count()
	planningEventually(t, "the planner to try again after the backoff", func() bool {
		f.Clock.Advance(time.Minute)
		return f.Runner.count() > before
	})
	assert.Equal(t, PhaseRunning, f.Run.Phase(), "the run is still going")
}

func TestPlannerFailureLimitIsConfigurable(t *testing.T) {
	rc := planningRunConfig(t, "ship it")
	clock := newPlanningClock()
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: &planningRunner{}, Applier: &planningApplier{}, Now: clock.Now, NoShim: true,
		FailureLimit: -1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	for i := 0; i < DefaultPlanFailureLimit+3; i++ {
		planningApply(p, nil, errors.New("the cartridge crashed"))
	}
	st := p.Status()
	assert.Zero(t, st.GaveUp, "a negative limit never gives up")
	assert.Equal(t, DefaultPlanFailureLimit+3, st.Failures)
	assert.False(t, st.Finished, "and a brief run with one goes on for ever, as asked")
}

// TestPlannerCountsOnlyConsecutiveRejectionsTowardsTheLimit pins the word
// "consecutive" in the P6-19 contract: a crash in the middle of two
// rejections breaks the run, and the two counters never add up to a
// give-up between them.
func TestPlannerCountsOnlyConsecutiveRejectionsTowardsTheLimit(t *testing.T) {
	rc := planningRunConfig(t, "ship it")
	clock := newPlanningClock()
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: &planningRunner{}, Applier: &planningApplier{}, Now: clock.Now, NoShim: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	rejected := fmt.Errorf("%w: bead \"a\" duplicates open bead sc-1", agents.ErrPlanRejected)

	planningApply(p, nil, rejected)
	planningApply(p, nil, errors.New("the cartridge crashed"))
	planningApply(p, nil, rejected)

	st := p.Status()
	assert.Zero(t, st.GaveUp, "the two rejections were not consecutive")
	assert.Equal(t, 1, st.Rejections)
	assert.Zero(t, st.Failures, "and a rejection breaks a run of failures too")
	assert.False(t, st.Finished)

	// Two in a row do end it.
	planningApply(p, nil, rejected)
	st = p.Status()
	assert.Equal(t, 1, st.GaveUp)
	assert.True(t, st.Finished)
}

// TestPlanningStopConditionDoesNotFloodWhenTheBeadListingKeepsFailing pins
// the rate limit on the brief-mode stop condition: the scheduler consults it
// after every dispatch, so a bd that is failing must not mean one listing
// and one warning per tick down the single serialised bd queue (D-21).
func TestPlanningStopConditionDoesNotFloodWhenTheBeadListingKeepsFailing(t *testing.T) {
	store := &planningBeadStore{}
	f := planningSetup(t, "ship it", func(o *PlannerOptions) { o.Beads = store.source() })
	planningApply(f.Planner, map[string]domain.BeadID{"a": "sc-a"}, nil)
	busy := errors.New("bd is busy")
	store.fail(busy)

	const polls = 40
	for i := 0; i < polls; i++ {
		f.Clock.Advance(time.Second)
		_, done, err := f.Planner.Done(context.Background(), f.Run, SchedulerStatus{Idle: true})
		require.NoError(t, err, "a listing failure must not fail the run")
		require.False(t, done)
	}
	listings := store.count()
	assert.LessOrEqual(t, listings, polls/int(DefaultSettleInterval/time.Second)+1,
		"one listing per settle interval, not one per tick")
	assert.Equal(t, 1, planningWarnings(f.events(), "settled"), "one warning, not one per poll")

	// A different failure is news, so it is warned about again.
	store.fail(errors.New("bd is gone"))
	f.Clock.Advance(DefaultSettleInterval)
	_, _, err := f.Planner.Done(context.Background(), f.Run, SchedulerStatus{Idle: true})
	require.NoError(t, err)
	assert.Equal(t, 1, planningWarnings(f.events(), "settled"))

	// So is the first failure after a listing that worked.
	store.set(planningBead("sc-a", domain.BeadStateOpen))
	f.Clock.Advance(DefaultSettleInterval)
	_, done, err := f.Planner.Done(context.Background(), f.Run, SchedulerStatus{Idle: true})
	require.NoError(t, err)
	assert.False(t, done, "an open bead is not settled")
	store.fail(busy)
	f.Clock.Advance(DefaultSettleInterval)
	_, _, err = f.Planner.Done(context.Background(), f.Run, SchedulerStatus{Idle: true})
	require.NoError(t, err)
	assert.Equal(t, 1, planningWarnings(f.events(), "settled"))
}

// planningWarnings counts the planning warnings whose message contains want.
func planningWarnings(evs []events.Event, want string) int {
	n := 0
	for _, ev := range evs {
		w, ok := ev.(events.Warning)
		if ok && w.Source == SourcePlanning && strings.Contains(w.Message, want) {
			n++
		}
	}
	return n
}

// TestPlannerPublishesTheDeprioritisationItCarriedOut covers the other half
// of Q-04: the bd write the cannon makes on the product owner's behalf is a
// change to a bead that already existed, and the run log is where it has to
// show up — events.PlanProduced carries the plan as applied, with the
// restated bead taken out of it, so without this event nothing would record
// that the priority moved.
func TestPlannerPublishesTheDeprioritisationItCarriedOut(t *testing.T) {
	pr := &planningPrioritiser{}
	f := planningSetup(t, "", func(o *PlannerOptions) { o.Prioritiser = pr })
	f.Beads.set(domain.Bead{
		ID: "sc-old", Title: "Do alpha", Type: domain.BeadTypeTask,
		State: domain.BeadStateOpen, Priority: domain.Priority(1),
	})
	f.Runner.setPlan(planningRestate(domain.PriorityLowest, "beta"))

	_, err := f.Planner.Fire(context.Background(), Firing{
		Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}, Run: f.Run,
	})
	require.NoError(t, err)
	require.Equal(t, []planningPriorityCall{{Bead: "sc-old", Priority: domain.PriorityLowest}}, pr.applied())

	var updates []events.BeadUpdated
	var plans []events.PlanProduced
	for _, ev := range f.events() {
		switch e := ev.(type) {
		case events.BeadUpdated:
			updates = append(updates, e)
		case events.PlanProduced:
			plans = append(plans, e)
		}
	}
	require.Len(t, updates, 1, "a bd write the cannon makes is never silent")
	assert.Equal(t, domain.BeadID("sc-old"), updates[0].Bead.ID)
	assert.Equal(t, domain.PriorityLowest, updates[0].Bead.Priority)
	assert.Equal(t, "Do alpha", updates[0].Bead.Title)
	assert.Equal(t, domain.BeadStateOpen, updates[0].Bead.State, "the bead bd echoed back, not a blank one")
	assert.Equal(t, []string{"priority"}, updates[0].Fields)
	assert.Equal(t, f.Run.ID(), updates[0].Run)

	require.Len(t, plans, 1)
	require.Len(t, plans[0].Plan.Beads, 1, "the announced plan is the one that was applied")
	assert.Equal(t, "beta", plans[0].Plan.Beads[0].Key)
}

// --- P6-39: the ErrEmptyPlan branch's brief arm ---------------------------

// TestPlannerBriefModeRefusesAnEmptyPlanRatherThanCallingItAnAnswer pins the
// upstream contract the brief arm of [Planner.record]'s ErrEmptyPlan branch
// rests on (P6-39): agents.ValidatePlan refuses a brief-mode plan that
// proposes nothing with agents.ErrPlanRejected, before
// agents.CreatePlanBeads can reach its own ErrEmptyPlan. "Nothing worth
// adding right now" is an autopilot answer (D-18); a brief that cannot be
// decomposed is a plan the product owner got wrong.
func TestPlannerBriefModeRefusesAnEmptyPlanRatherThanCallingItAnAnswer(t *testing.T) {
	rc := planningRunConfig(t, "ship the thing")
	runner := &planningRunner{}
	runner.setPlan(domain.Plan{Brief: "nothing worth adding right now"})
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: runner, Applier: &planningApplier{}, NoShim: true,
		Now: newPlanningClock().Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)

	_, err = p.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
	require.Error(t, err, "a brief run is never told its brief needs no work")
	assert.ErrorIs(t, err, agents.ErrPlanRejected)
	assert.NotErrorIs(t, err, agents.ErrEmptyPlan)

	st := p.Status()
	assert.Zero(t, st.Empty, "an empty brief plan is a rejection, not an empty plan")
	assert.Equal(t, 1, st.Rejections)
	assert.False(t, st.Finished, "one rejection is a retry, not the end of the run")
}

// TestPlannerBriefModeFinishesIfAnEmptyPlanReachesIt drives the arm P6-39
// kept as belt and braces. Nothing in production reaches it — the test above
// says why — but if that contract ever changed, a brief run told its plan
// was empty would idle for ever: an empty plan resets both P6-19 counters,
// so neither cap would ever end the run.
func TestPlannerBriefModeFinishesIfAnEmptyPlanReachesIt(t *testing.T) {
	p, err := NewPlanner(PlannerOptions{
		Config: planningRunConfig(t, "ship the thing"), Runner: &planningRunner{},
		Applier: &planningApplier{}, NoShim: true, Now: newPlanningClock().Now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)

	planningApply(p, nil, fmt.Errorf("%w (run 20260903-120000-abcd)", agents.ErrEmptyPlan))

	st := p.Status()
	assert.True(t, st.Finished, "the brief run ends rather than planning the same brief for ever")
	assert.Equal(t, 1, st.Empty)
	assert.Equal(t, 1, st.Plans)
	assert.Zero(t, st.GaveUp, "it is an answer, not a give-up")
	assert.Zero(t, st.Rejections)
}

// --- P1-19: the give-up caps come from the configuration -------------------

// TestPlannerTakesItsGiveUpCapsFromTheConfiguration is P1-19: the P6-19
// policy is `limits.max_plan_rejections` and `limits.max_plan_failures`, so
// a flaky cartridge can be given more rope and a short brief run less,
// without recompiling. An explicit PlannerOptions field still wins, which is
// what the e2e rigs and the tests above rely on.
func TestPlannerTakesItsGiveUpCapsFromTheConfiguration(t *testing.T) {
	rc := planningRunConfig(t, "ship it")
	rc.Effective.Limits.MaxPlanRejections = 3
	rc.Effective.Limits.MaxPlanFailures = 4
	newPlanner := func(t *testing.T, tune func(*PlannerOptions)) *Planner {
		t.Helper()
		opts := PlannerOptions{
			Config: rc, Runner: &planningRunner{}, Applier: &planningApplier{}, NoShim: true,
			Now: newPlanningClock().Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		if tune != nil {
			tune(&opts)
		}
		p, err := NewPlanner(opts)
		require.NoError(t, err)
		return p
	}
	rejected := fmt.Errorf("%w: a bead has no acceptance criteria", agents.ErrPlanRejected)
	crashed := errors.New("the cartridge crashed")

	rej := newPlanner(t, nil)
	planningApply(rej, nil, rejected)
	planningApply(rej, nil, rejected)
	assert.Zero(t, rej.Status().GaveUp, "the configured cap is three, not the built-in two")
	planningApply(rej, nil, rejected)
	assert.Equal(t, 1, rej.Status().GaveUp)

	fail := newPlanner(t, nil)
	for i := 0; i < 3; i++ {
		planningApply(fail, nil, crashed)
	}
	assert.Zero(t, fail.Status().GaveUp, "and four failures, not the built-in five")
	planningApply(fail, nil, crashed)
	assert.Equal(t, 1, fail.Status().GaveUp)

	opt := newPlanner(t, func(o *PlannerOptions) { o.RejectionLimit, o.FailureLimit = 1, 1 })
	planningApply(opt, nil, rejected)
	assert.Equal(t, 1, opt.Status().GaveUp, "an explicit option still wins over the file")

	defaults := newPlanner(t, func(o *PlannerOptions) {
		cfg := *rc
		eff := *rc.Effective
		eff.Limits.MaxPlanRejections, eff.Limits.MaxPlanFailures = 0, 0
		cfg.Effective = &eff
		o.Config = &cfg
	})
	for i := 0; i < config.DefaultMaxPlanRejections; i++ {
		planningApply(defaults, nil, rejected)
	}
	assert.Equal(t, 1, defaults.Status().GaveUp, "an unset limit is the built-in default")
	assert.Equal(t, config.DefaultMaxPlanRejections, DefaultPlanRejectionLimit, "one number, not two")
	assert.Equal(t, config.DefaultMaxPlanFailures, DefaultPlanFailureLimit)
}

// --- P8-28: the watermark on the tick after a plan lands ------------------

// TestPlannerRefusesASecondPlanningDemandOnTheTickAfterAPlanLands is P8-28.
// SchedulerStatus.Quiet is the *last* dispatch's verdict, so on the tick
// after a plan is applied the run still looks quiet — no engineer has
// started, because the beads the plan created have not reached the work
// source yet — and the pre-P8-28 planner offered the planning demand again
// straight away. That fired a whole second product owner at full model cost
// on every plan, over a repository whose new beads were not in flight, which
// is a duplicate plan by construction (P8-29).
func TestPlannerRefusesASecondPlanningDemandOnTheTickAfterAPlanLands(t *testing.T) {
	rc := planningRunConfig(t, "")
	clock := newPlanningClock()
	// The scheduler has fired one agent: the product owner that planned.
	status := SchedulerStatus{Live: 1, Fired: 1}
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: &planningRunner{plan: planningPlan("alpha")},
		Applier: &planningApplier{}, Now: clock.Now, NoShim: true,
		Status: func() SchedulerStatus { return status },
	})
	require.NoError(t, err)
	planningApply(p, map[string]domain.BeadID{"alpha": "sc-alpha"}, nil)

	// The tick right after: quiet, idle, nothing running, and nothing has
	// been fired since the plan landed.
	status = SchedulerStatus{Live: 0, Idle: true, Quiet: true, Fired: 1}
	ds, err := p.Demands(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ds, "the plan that just landed has not been acted on yet")

	// Ticking on changes nothing while the scheduler stays still.
	for i := 0; i < 5; i++ {
		clock.Advance(time.Second)
		ds, err = p.Demands(context.Background())
		require.NoError(t, err)
		require.Empty(t, ds, "still nothing fired for the plan")
	}

	// One agent fired for the plan releases the watermark; from there the
	// ordinary D-18 guards decide.
	status = SchedulerStatus{Live: 0, Idle: true, Quiet: true, Fired: 2}
	ds, err = p.Demands(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []Demand{planDemand()}, ds,
		"the plan was acted on and the queue is empty again: re-plan (D-18)")
}

// TestPlannerReplansOnTheIdleScheduleWhenNothingActsOnThePlan pins the
// backstop: the P8-28 watermark must never be able to stop an autopilot run
// planning for ever, because autopilot has no other way forward (D-18). A
// plan whose beads nothing picks up — every one of them blocked, or a bd
// that will not list them — waits out idle_replan_interval and plans again.
func TestPlannerReplansOnTheIdleScheduleWhenNothingActsOnThePlan(t *testing.T) {
	rc := planningRunConfig(t, "")
	clock := newPlanningClock()
	status := SchedulerStatus{Fired: 1}
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: &planningRunner{plan: planningPlan("alpha")},
		Applier: &planningApplier{}, Now: clock.Now, NoShim: true,
		Status: func() SchedulerStatus { return status },
	})
	require.NoError(t, err)
	planningApply(p, map[string]domain.BeadID{"alpha": "sc-alpha"}, nil)

	status = SchedulerStatus{Idle: true, Quiet: true, Fired: 1}
	clock.Advance(config.DefaultIdleReplanInterval - time.Second)
	ds, err := p.Demands(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ds, "one second short of the idle interval")

	clock.Advance(time.Second)
	ds, err = p.Demands(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []Demand{planDemand()}, ds, "the idle interval is the backstop (D-18)")
}

// TestPlannerFiresOneProductOwnerPerPlanInAWholeRun is the same guarantee
// through a live run rather than a hand-built status: the fixture's only
// firer is the planner, so after the first plan every tick is quiet for
// ever and the pre-P8-28 planner fired a product owner on each of them.
func TestPlannerFiresOneProductOwnerPerPlanInAWholeRun(t *testing.T) {
	f := planningSetup(t, "", nil)
	f.Runner.setPlan(planningPlan("alpha"))
	f.Beads.set(planningBead("sc-alpha", domain.BeadStateOpen))
	f.start()

	planningEventually(t, "the first plan to be applied", func() bool {
		f.Clock.Advance(time.Second)
		return f.Applier.count() >= 1
	})
	// Two hundred seconds of ticks, well inside the ten-minute idle
	// interval: not one of them may plan again.
	for i := 0; i < 20; i++ {
		f.Clock.Advance(10 * time.Second)
		schedPause(2 * time.Millisecond)
	}
	assert.Equal(t, 1, f.Runner.count(), "one plan, one product owner (P8-28)")
	assert.Equal(t, 1, f.Applier.count())

	// And the run is still a live autopilot run that plans again on the
	// idle schedule, not a run the watermark has wedged.
	planningEventually(t, "the idle re-plan", func() bool {
		f.Clock.Advance(config.DefaultIdleReplanInterval)
		return f.Runner.count() >= 2
	})
	assert.Equal(t, PhaseRunning, f.Run.Phase())
}

// --- P8-29: the product owner remembers what the run has shipped ----------

// TestPlannerTellsTheProductOwnerWhatTheRunHasAlreadyShipped is P8-29, the
// headline finding of the P8-03 autopilot runs: three pull requests in one
// ten-minute run re-fixed the same line, because the planning prompt's
// "Open beads / Do not propose work that duplicates any of these" block
// never mentioned finished work, and with safety on the repository the
// product owner reads still contains every bug the run has fixed and is
// waiting for a human to merge (D-10).
//
// The product owner here is scripted the way a model with no memory behaves:
// it proposes the same piece of work every time unless the request it is
// given says that work is done. Take the closed beads back out of the
// request and it re-proposes the shipped bead and this test fails, which is
// its own negative control.
func TestPlannerTellsTheProductOwnerWhatTheRunHasAlreadyShipped(t *testing.T) {
	f := planningSetup(t, "", nil)
	f.Runner.setNext(func(int) (domain.Plan, error) {
		req, ok := f.Runner.last()
		require.True(t, ok)
		for _, b := range req.OpenBeads {
			if b.State == domain.BeadStateClosed && b.Title == "Do alpha" {
				return planningPlan("beta"), nil // it is done; move on
			}
		}
		return planningPlan("alpha"), nil
	})
	f.Beads.set()
	f.start()

	planningEventually(t, "the first plan", func() bool {
		f.Clock.Advance(time.Second)
		return f.Applier.count() >= 1
	})
	assert.Equal(t, []string{"Do alpha"}, f.Applier.titles(1))

	// The run ships it: bd has the bead closed, under the title the plan
	// gave it. The repository has not changed, because safety on never
	// touches the default branch (D-10).
	f.Beads.set(domain.Bead{
		ID: "sc-alpha", Title: "Do alpha", Type: domain.BeadTypeTask, State: domain.BeadStateClosed,
	})

	planningEventually(t, "the second plan", func() bool {
		f.Clock.Advance(config.DefaultIdleReplanInterval)
		return f.Applier.count() >= 2
	})
	assert.Equal(t, []string{"Do beta"}, f.Applier.titles(2),
		"the second plan proposes none of the work the first one shipped (P8-29)")

	req, ok := f.Runner.last()
	require.True(t, ok)
	require.Len(t, req.OpenBeads, 1)
	assert.Equal(t, domain.BeadID("sc-alpha"), req.OpenBeads[0].ID)
	assert.Equal(t, domain.BeadStateClosed, req.OpenBeads[0].State,
		"the shipped bead is listed, marked closed")
	assert.Contains(t, strings.Join(req.Notes, "\n"), "already finished and delivered",
		"and the prompt says what that means")
}

func TestPlannerBoundsTheFinishedWorkItShowsTheProductOwner(t *testing.T) {
	rc := planningRunConfig(t, "")
	store := &planningBeadStore{}
	list := []domain.Bead{planningBead("sc-open", domain.BeadStateOpen)}
	for i := 0; i < planClosedShown+5; i++ {
		list = append(list, planningBead(domain.BeadID(fmt.Sprintf("sc-%02d", i)), domain.BeadStateClosed))
	}
	list = append(list, domain.Bead{ID: "sc-epic", Type: domain.BeadTypeEpic, State: domain.BeadStateClosed})
	store.set(list...)

	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: &planningRunner{}, Applier: &planningApplier{},
		Beads: store.source(), NoShim: true,
	})
	require.NoError(t, err)

	open, done, err := p.planBeads(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []domain.BeadID{"sc-open"}, planningIDs(open))
	require.Len(t, done, planClosedShown, "the closed list is bounded (P8-29)")
	assert.Equal(t, domain.BeadID("sc-05"), done[0].ID, "the newest end of the tracker's order is kept")
	assert.Equal(t, domain.BeadID("sc-24"), done[len(done)-1].ID)
	for _, b := range done {
		assert.NotEqual(t, domain.BeadTypeEpic, b.Type, "the run epic is bookkeeping, not work")
	}
}

// TestPlannerBriefModeShowsNoFinishedWork keeps the P8-29 list out of a
// brief prompt: a brief run plans once, at the start, when nothing of its
// own is closed yet, and Planner.notes says nothing at all in brief mode —
// so a [closed] entry there would be a line of the prompt with nothing to
// explain it.
func TestPlannerBriefModeShowsNoFinishedWork(t *testing.T) {
	rc := planningRunConfig(t, "ship the thing")
	store := &planningBeadStore{}
	store.set(
		planningBead("sc-open", domain.BeadStateOpen),
		planningBead("sc-done", domain.BeadStateClosed),
	)
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: &planningRunner{}, Applier: &planningApplier{},
		Beads: store.source(), NoShim: true,
	})
	require.NoError(t, err)

	open, done, err := p.planBeads(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []domain.BeadID{"sc-open"}, planningIDs(open))
	assert.Empty(t, done)
	assert.Empty(t, p.notes(open, done, false), "brief mode has no notes at all")
}

// planningIDs is the bead ids of a listing, for a readable assertion.
func planningIDs(list []domain.Bead) []domain.BeadID {
	out := make([]domain.BeadID, 0, len(list))
	for _, b := range list {
		out = append(out, b.ID)
	}
	return out
}
