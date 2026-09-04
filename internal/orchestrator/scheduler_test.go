package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// Tests for the scheduler and the D-05 ammo allocation (P6-02) and for the
// claim protocol folded into it (P6-13). Every helper here is prefixed
// "sched" so it cannot collide with another Phase 6 task's file in this
// package.

// schedEpoch is the fixed start time every scheduler test runs from.
var schedEpoch = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

// schedClock is a fake [beads.Clock]: time only moves when a test says so,
// and every pending After is delivered when its deadline passes.
type schedClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []schedWaiter
}

type schedWaiter struct {
	deadline time.Time
	ch       chan time.Time
}

func newSchedClock() *schedClock { return &schedClock{now: schedEpoch} }

func (c *schedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *schedClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, schedWaiter{deadline: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves the clock and delivers every waiter that has come due.
func (c *schedClock) Advance(d time.Duration) {
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

// schedFixture is one hermetic run driven by a scheduler.
type schedFixture struct {
	t      *testing.T
	Repo   string
	Bus    *events.Bus
	Sub    *events.Subscription
	Clock  *schedClock
	Run    *Run
	Sched  *Scheduler
	Claims *schedClaims
}

// schedSetup points XDG at temp directories (D-23), builds a run with ammo
// and starts it with a scheduler built from opts (which the mutator may
// adjust). The run is stopped and waited for on cleanup.
func schedSetup(t *testing.T, ammo int, opts SchedulerOptions) *schedFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	repo := filepath.Join(home, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	cfg := config.Defaults()
	cfg.Ammo = ammo
	cfg.ActiveCartridge = "claude"
	cfg.Cartridges = map[string]config.Cartridge{"claude": {Model: "opus"}}
	eff := &config.Effective{Config: cfg, Sources: map[string]config.Source{}}
	rc, err := config.NewRunConfig(repo, eff, config.RunSettings{
		ID: "20260903-120000-abcd", Brief: "do the thing", StartedAt: schedEpoch,
	})
	require.NoError(t, err)

	clock := newSchedClock()
	bus := events.NewBus()
	sub := bus.Subscribe(context.Background(), 2048)
	t.Cleanup(func() { sub.Unsubscribe(); bus.Close() })

	if opts.Clock == nil {
		opts.Clock = clock
	}
	sched := NewScheduler(opts)
	run, err := New(rc, Options{
		Loop:   sched,
		Bus:    bus,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    clock.Now,
	})
	require.NoError(t, err)

	f := &schedFixture{t: t, Repo: repo, Bus: bus, Sub: sub, Clock: clock, Run: run, Sched: sched}
	require.NoError(t, run.Start(context.Background()))
	t.Cleanup(func() {
		_ = run.Stop("test cleanup")
		select {
		case <-run.Done():
		case <-time.After(5 * time.Second):
			t.Error("the run did not finish")
		}
	})
	return f
}

// Wait stops waiting once the run has finished.
func (f *schedFixture) Wait() (Outcome, error) {
	f.t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = f.Run.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		f.t.Fatal("the run did not finish")
	}
	return f.Run.Wait()
}

// events drains the bus and returns everything published so far.
func (f *schedFixture) events() []events.Event {
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

// warnings returns the Warning events published so far.
func (f *schedFixture) warnings() []events.Warning {
	var out []events.Warning
	for _, ev := range f.events() {
		if w, ok := ev.(events.Warning); ok {
			out = append(out, w)
		}
	}
	return out
}

// schedEventually polls cond until it holds, so a test never sleeps for a
// fixed time. A cond that advances the fake clock drives the tick loop from
// inside the poll.
func schedEventually(t *testing.T, what string, cond func() bool) {
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

// schedGate blocks every agent until the test releases it.
type schedGate struct {
	ch     chan struct{}
	once   sync.Once
	live   atomic.Int64
	peak   atomic.Int64
	total  atomic.Int64
	mu     sync.Mutex
	fired  []Firing
	byRole map[domain.Role]int64
	peakBy map[domain.Role]int64
}

func newSchedGate() *schedGate {
	return &schedGate{ch: make(chan struct{}), byRole: map[domain.Role]int64{}, peakBy: map[domain.Role]int64{}}
}

// Fire implements [Firer]: it records the agent, holds the slot until the
// gate opens or the context ends, and tracks the peak concurrency.
func (g *schedGate) Fire(ctx context.Context, f Firing) (FireResult, error) {
	g.enter(f)
	defer g.exit(f)
	select {
	case <-g.ch:
	case <-ctx.Done():
		return FireResult{Failure: events.FailureKilled}, ctx.Err()
	}
	return FireResult{Produced: domain.MessageImplReport}, nil
}

func (g *schedGate) enter(f Firing) {
	n := g.live.Add(1)
	g.total.Add(1)
	for {
		peak := g.peak.Load()
		if n <= peak || g.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fired = append(g.fired, f)
	g.byRole[f.Agent.Role]++
	if g.byRole[f.Agent.Role] > g.peakBy[f.Agent.Role] {
		g.peakBy[f.Agent.Role] = g.byRole[f.Agent.Role]
	}
}

func (g *schedGate) exit(f Firing) {
	g.live.Add(-1)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.byRole[f.Agent.Role]--
}

// Open releases every agent waiting on the gate.
func (g *schedGate) Open() { g.once.Do(func() { close(g.ch) }) }

// Firings returns the agents fired so far, in order.
func (g *schedGate) Firings() []Firing {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Firing(nil), g.fired...)
}

// AgentIDs returns the ids of the agents fired so far, in order.
func (g *schedGate) AgentIDs() []domain.AgentID {
	out := []domain.AgentID{}
	for _, f := range g.Firings() {
		out = append(out, f.Agent.ID)
	}
	return out
}

// PeakByRole is the highest number of that role live at once.
func (g *schedGate) PeakByRole(role domain.Role) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return int(g.peakBy[role])
}

// schedBead is one bead in the fake claim store.
type schedBead struct {
	status   domain.BeadState
	assignee domain.AgentID
}

// schedClaims is a [ClaimStore] that reproduces the bd 1.2.2 claim rules
// verified in Appendix A: `--claim` works only on an open unassigned bead
// or the same actor's in_progress bead, so a changes_requested bead whose
// assignee is the previous round's engineer answers ErrAlreadyClaimed.
type schedClaims struct {
	mu    sync.Mutex
	beads map[domain.BeadID]*schedBead
	calls []string
	fail  map[string]error
}

func newSchedClaims(state map[domain.BeadID]schedBead) *schedClaims {
	c := &schedClaims{beads: map[domain.BeadID]*schedBead{}, fail: map[string]error{}}
	for id, b := range state {
		b := b
		c.beads[id] = &b
	}
	return c
}

func (c *schedClaims) record(op string, id domain.BeadID, agent domain.AgentID) {
	if agent == "" {
		c.calls = append(c.calls, fmt.Sprintf("%s %s", op, id))
		return
	}
	c.calls = append(c.calls, fmt.Sprintf("%s %s %s", op, id, agent))
}

// Calls returns the bd writes made so far, in order.
func (c *schedClaims) Calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// Count returns how many calls of one operation were made.
func (c *schedClaims) Count(op string) int {
	n := 0
	for _, call := range c.Calls() {
		if len(call) >= len(op) && call[:len(op)] == op {
			n++
		}
	}
	return n
}

// State returns a bead's status and assignee.
func (c *schedClaims) State(id domain.BeadID) schedBead {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b, ok := c.beads[id]; ok {
		return *b
	}
	return schedBead{}
}

// Set forces a bead's status and assignee, standing in for the state
// machine (P6-03) writing through beads.
func (c *schedClaims) Set(id domain.BeadID, status domain.BeadState, assignee domain.AgentID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.beads[id] = &schedBead{status: status, assignee: assignee}
}

func (c *schedClaims) get(id domain.BeadID) *schedBead {
	b, ok := c.beads[id]
	if !ok {
		b = &schedBead{status: domain.BeadStateOpen}
		c.beads[id] = b
	}
	return b
}

// ClaimFor implements [ClaimStore] with bd 1.2.2's rules.
func (c *schedClaims) ClaimFor(_ context.Context, id domain.BeadID, agent domain.AgentID) (beads.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("claim", id, agent)
	if err := c.fail["claim"]; err != nil {
		return beads.Issue{}, err
	}
	b := c.get(id)
	switch {
	case b.status == domain.BeadStateInProgress && b.assignee == agent:
		return beads.Issue{ID: id}, nil
	case b.assignee != "" && b.assignee != agent:
		return beads.Issue{}, fmt.Errorf("bd: %w", beads.ErrAlreadyClaimed)
	case b.status != domain.BeadStateOpen:
		return beads.Issue{}, fmt.Errorf("bd: issue not claimable: status %s: %w", b.status, beads.ErrNotClaimable)
	}
	b.status = domain.BeadStateInProgress
	b.assignee = agent
	return beads.Issue{ID: id}, nil
}

// Reclaim implements [ClaimStore]: a plain update, valid from any
// non-closed status.
func (c *schedClaims) Reclaim(_ context.Context, id domain.BeadID, agent domain.AgentID) (beads.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("reclaim", id, agent)
	if err := c.fail["reclaim"]; err != nil {
		return beads.Issue{}, err
	}
	b := c.get(id)
	if b.status == domain.BeadStateClosed {
		return beads.Issue{}, fmt.Errorf("bd: %w", beads.ErrNotClaimable)
	}
	b.status = domain.BeadStateInProgress
	b.assignee = agent
	return beads.Issue{ID: id}, nil
}

// Release implements [ClaimStore].
func (c *schedClaims) Release(_ context.Context, id domain.BeadID) (beads.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("release", id, "")
	if err := c.fail["release"]; err != nil {
		return beads.Issue{}, err
	}
	b := c.get(id)
	b.status = domain.BeadStateOpen
	b.assignee = ""
	return beads.Issue{ID: id}, nil
}

// Renew implements [ClaimStore].
func (c *schedClaims) Renew(_ context.Context, id domain.BeadID, agent domain.AgentID) (beads.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("renew", id, agent)
	if err := c.fail["renew"]; err != nil {
		return beads.Issue{}, err
	}
	b := c.get(id)
	if b.status != domain.BeadStateInProgress || b.assignee != agent {
		return beads.Issue{}, fmt.Errorf("bd: %w", beads.ErrLeaseLost)
	}
	return beads.Issue{ID: id}, nil
}

// *schedClaims is a ClaimStore, like *beads.Client.
var _ ClaimStore = (*schedClaims)(nil)

// schedParker records the D-07 transitions the scheduler asks for when it
// parks a demand it has given up on (P6-31), standing in for the state
// machine.
type schedParker struct {
	mu   sync.Mutex
	reqs []StateRequest
	err  error
}

// Apply implements [BeadParker].
func (p *schedParker) Apply(_ context.Context, req StateRequest) (StateResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, req)
	if p.err != nil {
		return StateResult{From: req.From}, p.err
	}
	return StateResult{From: req.From, To: domain.BeadStateFailed, Persisted: true}, nil
}

// Requests returns the transitions asked for so far, in order.
func (p *schedParker) Requests() []StateRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]StateRequest(nil), p.reqs...)
}

// *schedParker is a BeadParker, like *StateMachine.
var _ BeadParker = (*schedParker)(nil)

// schedFinishWhenIdle stops the run as soon as a dispatch has nothing left
// to fire, which is what a parked demand leaves behind.
func schedFinishWhenIdle() StopCondition {
	return StopConditionFunc(func(_ context.Context, _ *Run, st SchedulerStatus) (Outcome, bool, error) {
		if st.Idle {
			return Outcome{Reason: events.FinishCompleted, Summary: "nothing left to fire"}, true, nil
		}
		return Outcome{}, false, nil
	})
}

// schedOnceSource offers each demand until it has been fired, the way a
// real source does: work in flight, or done, is not offered again. A source
// that keeps offering finished work would have the scheduler re-fire it on
// every kick.
type schedOnceSource struct {
	mu      sync.Mutex
	demands []Demand
	done    map[string]bool
}

func schedOnce(ds ...Demand) *schedOnceSource {
	return &schedOnceSource{demands: ds, done: map[string]bool{}}
}

// Demands implements [WorkSource].
func (o *schedOnceSource) Demands(context.Context) ([]Demand, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []Demand
	for _, d := range o.demands {
		if !o.done[d.Key()] {
			out = append(out, d)
		}
	}
	return out, nil
}

// Fire implements [Firer] by marking the demand done and calling next.
func (o *schedOnceSource) Fire(next Firer) Firer {
	return FirerFunc(func(ctx context.Context, f Firing) (FireResult, error) {
		o.mu.Lock()
		o.done[f.Demand.Key()] = true
		o.mu.Unlock()
		if next == nil {
			return FireResult{}, nil
		}
		return next.Fire(ctx, f)
	})
}

// schedStatic is a work source that always offers the same demands.
func schedStatic(ds ...Demand) WorkSource {
	return WorkSourceFunc(func(context.Context) ([]Demand, error) { return ds, nil })
}

// schedImplement builds an engineer demand for a bead.
func schedImplement(bead domain.BeadID, round int) Demand {
	return Demand{Kind: DemandImplement, Bead: bead, Round: round, Claim: ClaimNew}
}

// schedReview builds a tester demand for a bead and lens.
func schedReview(bead domain.BeadID, lens domain.Lens, round int) Demand {
	return Demand{Kind: DemandReview, Bead: bead, Lens: lens, Round: round}
}

// schedPushback builds the other tester demand of a round: judging the
// engineer's pushback on that lens's own item (D-08).
func schedPushback(bead domain.BeadID, lens domain.Lens, round int) Demand {
	d := schedReview(bead, lens, round)
	d.Task = events.TaskEvaluatePushback
	return d
}

// --- the D-05 allocation table -------------------------------------------

// TestSchedulerAllocationTable pins the D-05 examples the task asks for:
// ammo 3, 4, 10, 12, 13. The caps are config.Allocate's, and the review
// width is the "one tester per lens, fewer if ammo cannot afford, never
// zero" rule.
func TestSchedulerAllocationTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ammo                        int
		po, engineers, testers, per int
		line                        string
	}{
		{3, 1, 1, 1, 1, "ammo 3 → 1 PO, up to 1 engineers, up to 1 testers, 1 testers per bead"},
		{4, 1, 1, 2, 2, "ammo 4 → 1 PO, up to 1 engineers, up to 2 testers, 2 testers per bead"},
		{10, 1, 2, 7, 3, "ammo 10 → 1 PO, up to 2 engineers, up to 7 testers, 3 testers per bead"},
		{12, 1, 2, 9, 3, "ammo 12 → 1 PO, up to 2 engineers, up to 9 testers, 3 testers per bead"},
		{13, 1, 3, 9, 3, "ammo 13 → 1 PO, up to 3 engineers, up to 9 testers, 3 testers per bead"},
	}
	for _, tc := range tests {
		alloc, err := config.Allocate(tc.ammo)
		require.NoError(t, err, tc.ammo)
		caps := CapsFor(alloc)
		assert.Equal(t, SlotCaps{Ammo: tc.ammo, PO: tc.po, Engineers: tc.engineers, Testers: tc.testers}, caps, tc.ammo)
		assert.Equal(t, tc.per, caps.TestersPerBead(), tc.ammo)
		assert.Equal(t, tc.line, caps.String(), tc.ammo)
		assert.Equal(t, tc.ammo, caps.PO+caps.Engineers+caps.Testers, "the pools add up to ammo")
		assert.Equal(t, tc.po, caps.For(domain.RolePO))
		assert.Equal(t, tc.engineers, caps.For(domain.RoleEngineer))
		assert.Equal(t, tc.testers, caps.For(domain.RoleTester))
		assert.Zero(t, caps.For(domain.Role("nobody")))
	}
}

// TestSchedulerCapsFallBackToTheMinimum keeps a misconfigured scheduler
// firing one agent of each role rather than nothing at all.
func TestSchedulerCapsFallBackToTheMinimum(t *testing.T) {
	t.Parallel()
	assert.Equal(t, SlotCaps{Ammo: 3, PO: 1, Engineers: 1, Testers: 1}, CapsFor(config.Allocation{}))
	assert.Equal(t, SlotCaps{Ammo: 3, PO: 1, Engineers: 1, Testers: 1}, CapsFor(config.Allocation{Ammo: 2}))
	assert.Equal(t, 1, CapsFor(config.Allocation{}).TestersPerBead())
}

// TestSchedulerTakesItsAllocationFromTheRun checks the zero Allocation
// means "whatever the run was configured with".
func TestSchedulerTakesItsAllocationFromTheRun(t *testing.T) {
	f := schedSetup(t, 13, SchedulerOptions{})
	schedEventually(t, "the scheduler to read the run's allocation", func() bool {
		return f.Sched.Caps().Ammo == 13
	})
	assert.Equal(t, SlotCaps{Ammo: 13, PO: 1, Engineers: 3, Testers: 9}, f.Sched.Caps())
}

// TestSchedulerSaturatesEveryPool fires against unlimited demand of every
// kind and asserts the run ends up with exactly the D-05 allocation live:
// the ammo is filled, and not by one agent more.
func TestSchedulerSaturatesEveryPool(t *testing.T) {
	for _, ammo := range []int{3, 4, 10, 12, 13} {
		t.Run(fmt.Sprintf("ammo-%d", ammo), func(t *testing.T) {
			alloc, err := config.Allocate(ammo)
			require.NoError(t, err)
			caps := CapsFor(alloc)

			gate := newSchedGate()
			var demands []Demand
			demands = append(demands, Demand{Kind: DemandArbitrate, Bead: "sc-arb", Round: 3})
			demands = append(demands, Demand{Kind: DemandPlan})
			// Enough beads that the per-bead review width is never the
			// binding constraint on the tester pool.
			for i := 1; i <= 8; i++ {
				bead := domain.BeadID(fmt.Sprintf("sc-%02d", i))
				demands = append(demands, schedImplement(bead, 1))
				for _, lens := range domain.Lenses() {
					demands = append(demands, schedReview(bead, lens, 1))
				}
			}
			f := schedSetup(t, ammo, SchedulerOptions{Work: schedStatic(demands...), Fire: gate})
			t.Cleanup(gate.Open)

			schedEventually(t, "the ammo to fill up", func() bool { return len(gate.Firings()) == ammo })
			st := f.Sched.Status()
			assert.Equal(t, ammo, st.Live)
			assert.Equal(t, caps.PO, st.ByRole[domain.RolePO], "one PO slot, taken by the arbitration")
			assert.Equal(t, caps.Engineers, st.ByRole[domain.RoleEngineer])
			assert.Equal(t, caps.Testers, st.ByRole[domain.RoleTester])
			assert.Len(t, st.Agents, ammo)

			// Nothing more is ever fired while the gate holds them.
			for i := 0; i < 20; i++ {
				f.Clock.Advance(2 * DefaultTickInterval)
				require.LessOrEqual(t, f.Sched.Live(), ammo)
			}
			assert.Equal(t, int64(ammo), gate.peak.Load(), "peak concurrency is the ammo, no more and no less")
			assert.Equal(t, 0, gate.PeakByRole(domain.Role("")), "no agent without a role")

			// The PO slot went to the arbitration, not to planning.
			var poAgents []domain.AgentID
			for _, fired := range gate.Firings() {
				if fired.Agent.Role == domain.RolePO {
					poAgents = append(poAgents, fired.Agent.ID)
				}
			}
			assert.Equal(t, []domain.AgentID{"po-sc-arb-r3"}, poAgents)
		})
	}
}

// TestSchedulerNeverExceedsAmmoUnderRandomEvents is the P6-02 property
// test: whatever sequence of demands and completions the world throws at
// it, the number of live agents never exceeds the ammo, and no role pool
// ever overflows either.
func TestSchedulerNeverExceedsAmmoUnderRandomEvents(t *testing.T) {
	for _, ammo := range []int{3, 4, 10, 12, 13} {
		for seed := int64(1); seed <= 5; seed++ {
			t.Run(fmt.Sprintf("ammo-%d-seed-%d", ammo, seed), func(t *testing.T) {
				caps := CapsFor(mustAllocate(t, ammo))
				rng := rand.New(rand.NewSource(seed * int64(ammo)))
				var rngMu sync.Mutex
				roll := func(n int) int {
					rngMu.Lock()
					defer rngMu.Unlock()
					return rng.Intn(n)
				}

				var live atomic.Int64
				var peak atomic.Int64
				var peakPO, peakEng, peakTest atomic.Int64
				byRole := map[domain.Role]*atomic.Int64{
					domain.RolePO:       {},
					domain.RoleEngineer: {},
					domain.RoleTester:   {},
				}
				peaks := map[domain.Role]*atomic.Int64{
					domain.RolePO:       &peakPO,
					domain.RoleEngineer: &peakEng,
					domain.RoleTester:   &peakTest,
				}
				bump := func(v *atomic.Int64, n int64) {
					for {
						cur := v.Load()
						if n <= cur || v.CompareAndSwap(cur, n) {
							return
						}
					}
				}

				fire := FirerFunc(func(ctx context.Context, f Firing) (FireResult, error) {
					n := live.Add(1)
					bump(&peak, n)
					r := byRole[f.Agent.Role].Add(1)
					bump(peaks[f.Agent.Role], r)
					defer func() { live.Add(-1); byRole[f.Agent.Role].Add(-1) }()
					// A random agent life: some return at once, some take a
					// while, some fail, some panic. All of them free a slot.
					switch roll(8) {
					case 0:
						return FireResult{}, errors.New("the cartridge fell over")
					case 1:
						panic("the cartridge exploded")
					case 2, 3:
						select {
						case <-ctx.Done():
							return FireResult{}, ctx.Err()
						case <-time.After(time.Duration(roll(200)) * time.Microsecond):
						}
					}
					return FireResult{Produced: domain.MessageImplReport}, nil
				})

				// The world: a random, changing set of demands.
				work := WorkSourceFunc(func(context.Context) ([]Demand, error) {
					var ds []Demand
					if roll(2) == 0 {
						ds = append(ds, Demand{Kind: DemandPlan, Round: roll(3)})
					}
					if roll(3) == 0 {
						ds = append(ds, Demand{Kind: DemandArbitrate, Bead: domain.BeadID(fmt.Sprintf("sc-%02d", roll(6))), Round: roll(4)})
					}
					for i := 0; i < roll(12); i++ {
						bead := domain.BeadID(fmt.Sprintf("sc-%02d", roll(6)))
						round := roll(4)
						if roll(2) == 0 {
							ds = append(ds, schedImplement(bead, round))
							continue
						}
						lens := domain.Lenses()[roll(len(domain.Lenses()))]
						ds = append(ds, schedReview(bead, lens, round))
					}
					if roll(20) == 0 {
						return ds, errors.New("bd hiccuped")
					}
					return ds, nil
				})

				f := schedSetup(t, ammo, SchedulerOptions{
					Work: work,
					Fire: fire,
					Tick: DefaultTickInterval,
				})

				// Drive the world for a while: every tick re-rolls the
				// demands, and finished agents kick the loop themselves.
				for i := 0; i < 300; i++ {
					require.LessOrEqual(t, f.Sched.Live(), ammo, "live agents must never exceed ammo")
					f.Clock.Advance(2 * DefaultTickInterval)
					time.Sleep(50 * time.Microsecond)
				}
				schedEventually(t, "agents to have been fired", func() bool { return f.Sched.Status().Fired > 0 })

				require.NoError(t, f.Run.Stop("test over"))
				_, err := f.Wait()
				require.NoError(t, err)

				assert.LessOrEqual(t, peak.Load(), int64(ammo), "peak live agents ≤ ammo")
				assert.LessOrEqual(t, peakPO.Load(), int64(caps.PO), "peak product owners ≤ 1")
				assert.LessOrEqual(t, peakEng.Load(), int64(caps.Engineers), "peak engineers ≤ max_engineers_in_flight")
				assert.LessOrEqual(t, peakTest.Load(), int64(caps.Testers), "peak testers ≤ the tester pool")
				assert.Zero(t, f.Sched.Live(), "every slot is given back")
			})
		}
	}
}

func mustAllocate(t *testing.T, ammo int) config.Allocation {
	t.Helper()
	a, err := config.Allocate(ammo)
	require.NoError(t, err)
	return a
}

// --- priority order ------------------------------------------------------

// TestSchedulerFillsThePOSlotWithArbitrationFirst proves the D-05 priority:
// with one PO slot and both an arbitration and a plan waiting, arbitration
// wins.
func TestSchedulerFillsThePOSlotWithArbitrationFirst(t *testing.T) {
	gate := newSchedGate()
	schedSetup(t, 3, SchedulerOptions{
		Work: schedStatic(
			Demand{Kind: DemandPlan},
			Demand{Kind: DemandArbitrate, Bead: "sc-1", Round: 2},
		),
		Fire: gate,
	})
	t.Cleanup(gate.Open)
	schedEventually(t, "the PO to be fired", func() bool { return len(gate.Firings()) == 1 })
	assert.Equal(t, []domain.AgentID{"po-sc-1-r2"}, gate.AgentIDs())
}

// TestSchedulerSortsDemandsInTheD05Order pins the ordering the dispatcher
// hands slots out in, without running anything.
func TestSchedulerSortsDemandsInTheD05Order(t *testing.T) {
	t.Parallel()
	ds := []Demand{
		{Kind: DemandPlan},
		schedImplement("sc-2", 1),
		schedReview("sc-1", domain.LensTest, 1),
		{Kind: DemandImplement, Bead: "sc-3", Round: 2, Task: events.TaskAddressFeedback},
		schedReview("sc-1", domain.LensUX, 1),
		{Kind: DemandArbitrate, Bead: "sc-9", Round: 4},
		{Kind: DemandImplement, Bead: "sc-4", Round: 2, Task: events.TaskResolveConflict},
		schedReview("sc-1", domain.LensQA, 1),
	}
	sortDemands(ds)
	got := make([]string, 0, len(ds))
	for _, d := range ds {
		got = append(got, string(d.Kind)+"/"+string(d.Bead)+"/"+string(d.Lens))
	}
	assert.Equal(t, []string{
		"arbitrate/sc-9/",
		"review/sc-1/ux",
		"review/sc-1/qa",
		"review/sc-1/test",
		"implement/sc-4/", // a conflict blocks a delivery
		"implement/sc-3/", // then the feedback round
		"implement/sc-2/", // then fresh work
		"plan//",
	}, got)
}

// TestSchedulerHonoursTheSourcesOwnOrder checks Order beats the default
// tie-break inside a kind.
func TestSchedulerHonoursTheSourcesOwnOrder(t *testing.T) {
	t.Parallel()
	ds := []Demand{
		{Kind: DemandImplement, Bead: "sc-1", Order: 9},
		{Kind: DemandImplement, Bead: "sc-2", Order: 1},
	}
	sortDemands(ds)
	assert.Equal(t, domain.BeadID("sc-2"), ds[0].Bead)
}

// TestSchedulerReviewWidthShrinksWithAmmo is the D-05 rule that a review
// gets one tester per lens, fewer when ammo cannot afford three, never
// zero: with ammo 4 there are two tester slots, so the first two lenses go
// out and the third waits.
func TestSchedulerReviewWidthShrinksWithAmmo(t *testing.T) {
	gate := newSchedGate()
	f := schedSetup(t, 4, SchedulerOptions{
		Work: schedStatic(
			schedReview("sc-1", domain.LensUX, 1),
			schedReview("sc-1", domain.LensQA, 1),
			schedReview("sc-1", domain.LensTest, 1),
		),
		Fire: gate,
	})
	t.Cleanup(gate.Open)
	schedEventually(t, "both tester slots to fill", func() bool { return len(gate.Firings()) == 2 })
	for i := 0; i < 10; i++ {
		f.Clock.Advance(2 * DefaultTickInterval)
	}
	// The two of three lenses that got a slot are the first two in lens
	// order; which goroutine records itself first is not the scheduler's
	// business, so this is a set comparison.
	assert.ElementsMatch(t, []domain.AgentID{"tester-ux-sc-1-r1", "tester-qa-sc-1-r1"}, gate.AgentIDs())
	assert.Equal(t, 2, f.Sched.Status().ByRole[domain.RoleTester])
}

// TestSchedulerSpreadsTestersAcrossBeads checks the per-bead review width
// never lets one bead swallow the tester pool.
func TestSchedulerSpreadsTestersAcrossBeads(t *testing.T) {
	gate := newSchedGate()
	var demands []Demand
	for _, bead := range []domain.BeadID{"sc-1", "sc-2", "sc-3"} {
		for _, lens := range domain.Lenses() {
			demands = append(demands, schedReview(bead, lens, 1))
		}
	}
	f := schedSetup(t, 10, SchedulerOptions{Work: schedStatic(demands...), Fire: gate})
	t.Cleanup(gate.Open)
	schedEventually(t, "seven tester slots to fill", func() bool { return len(gate.Firings()) == 7 })
	perBead := map[domain.BeadID]int{}
	for _, fired := range gate.Firings() {
		perBead[fired.Agent.Bead]++
	}
	assert.Equal(t, 7, f.Sched.Status().ByRole[domain.RoleTester])
	assert.Equal(t, 3, perBead["sc-1"])
	assert.Equal(t, 3, perBead["sc-2"])
	assert.Equal(t, 1, perBead["sc-3"], "the last slot of the pool, not a fourth tester on sc-1")
}

// TestSchedulerNeverFiresTheSameWorkTwice checks the in-flight key.
func TestSchedulerNeverFiresTheSameWorkTwice(t *testing.T) {
	gate := newSchedGate()
	f := schedSetup(t, 10, SchedulerOptions{
		Work: schedStatic(schedReview("sc-1", domain.LensQA, 1)),
		Fire: gate,
	})
	t.Cleanup(gate.Open)
	schedEventually(t, "the tester to be fired", func() bool { return len(gate.Firings()) == 1 })
	for i := 0; i < 10; i++ {
		f.Clock.Advance(2 * DefaultTickInterval)
	}
	assert.Len(t, gate.AgentIDs(), 1)
	assert.Equal(t, uint64(1), f.Sched.Status().Fired)
}

// --- slot accounting -----------------------------------------------------

// TestSchedulerFreesTheSlotOfACrashingAgent covers the "a crashing agent
// frees its slot" clause for both an error and a panic, and checks the
// AgentFailed events that go with them.
func TestSchedulerFreesTheSlotOfACrashingAgent(t *testing.T) {
	// Round 1 crashes, round 2 panics, round 3 succeeds; the round only
	// moves on once the agent is done, so nothing here has to be timed.
	round := &atomic.Int64{}
	round.Store(1)
	fired := make(chan domain.AgentID, 8)
	fire := FirerFunc(func(_ context.Context, f Firing) (FireResult, error) {
		defer round.Add(1)
		fired <- f.Agent.ID
		switch f.Demand.Round {
		case 1:
			return FireResult{Failure: events.FailureCrash, ExitCode: 2}, errors.New("boom")
		case 2:
			panic("the cartridge exploded")
		default:
			return FireResult{Produced: domain.MessageVerdict}, nil
		}
	})
	work := WorkSourceFunc(func(context.Context) ([]Demand, error) {
		n := int(round.Load())
		if n > 3 {
			return nil, nil
		}
		return []Demand{schedReview("sc-1", domain.LensQA, n)}, nil
	})
	f := schedSetup(t, 10, SchedulerOptions{Work: work, Fire: fire})

	for _, want := range []domain.AgentID{"tester-qa-sc-1-r1", "tester-qa-sc-1-r2", "tester-qa-sc-1-r3"} {
		schedEventually(t, "the next round to be fired", func() bool {
			f.Clock.Advance(2 * DefaultTickInterval)
			return len(fired) > 0
		})
		require.Equal(t, want, <-fired)
	}
	schedEventually(t, "every slot to come back", func() bool {
		f.Clock.Advance(2 * DefaultTickInterval)
		return f.Sched.Live() == 0 && f.Sched.Status().Fired == 3
	})

	require.NoError(t, f.Run.Stop("test over"))
	_, err := f.Wait()
	require.NoError(t, err)

	var failed []events.AgentFailed
	var finished []events.AgentFinished
	for _, ev := range f.events() {
		switch e := ev.(type) {
		case events.AgentFailed:
			failed = append(failed, e)
		case events.AgentFinished:
			finished = append(finished, e)
		}
	}
	require.Len(t, failed, 2)
	assert.Equal(t, events.FailureCrash, failed[0].Failure)
	assert.Equal(t, 2, failed[0].ExitCode)
	assert.Equal(t, "boom", failed[0].Err)
	assert.Equal(t, events.FailureCrash, failed[1].Failure)
	assert.Contains(t, failed[1].Err, "agent panicked")
	assert.Contains(t, failed[1].Err, "the cartridge exploded")
	require.Len(t, finished, 1)
	assert.Equal(t, domain.MessageVerdict, finished[0].Produced)
	assert.Equal(t, domain.RoleTester, finished[0].Role)
}

// TestSchedulerPanicIsAnErrorNotACrash checks the recovered panic is
// reported as ErrAgentPanic through the firer boundary.
func TestSchedulerPanicIsAnErrorNotACrash(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerOptions{Fire: FirerFunc(func(context.Context, Firing) (FireResult, error) {
		panic("nope")
	})})
	res, err := s.call(context.Background(), Firing{})
	assert.ErrorIs(t, err, ErrAgentPanic)
	assert.Equal(t, events.FailureCrash, res.Failure)
}

// TestSchedulerRefusesWorkWithoutAFirer fails fast rather than silently
// scheduling nothing.
func TestSchedulerRefusesWorkWithoutAFirer(t *testing.T) {
	f := schedSetup(t, 10, SchedulerOptions{Work: schedStatic(schedImplement("sc-1", 1))})
	_, err := f.Wait()
	assert.ErrorIs(t, err, ErrNoFirer)
}

// TestSchedulerRecordsAgentsInTheRunState checks state.json follows the
// live agents, which is what a resumed run and `sc status` read.
func TestSchedulerRecordsAgentsInTheRunState(t *testing.T) {
	gate := newSchedGate()
	f := schedSetup(t, 10, SchedulerOptions{
		Work: schedStatic(schedReview("sc-1", domain.LensQA, 1)),
		Fire: gate,
	})
	// The run records the agent on the loop goroutine, before the agent's
	// own goroutine reaches the firer, so a test that indexes the firings
	// has to wait for the firing and not only for the record (P6-43).
	schedEventually(t, "the agent to be recorded and fired", func() bool {
		return len(f.Run.Agents()) == 1 && len(gate.Firings()) == 1
	})
	recorded := f.Run.Agents()[0]
	assert.Equal(t, domain.AgentID("tester-qa-sc-1-r1"), recorded.ID)
	assert.Equal(t, domain.RoleTester, recorded.Role)
	assert.Equal(t, domain.LensQA, recorded.Lens)
	assert.Equal(t, domain.BeadID("sc-1"), recorded.Bead)
	assert.Equal(t, "claude", recorded.Cartridge)
	assert.Equal(t, "opus", recorded.Model)
	assert.Equal(t, domain.AgentStateRunning, recorded.State)
	assert.Equal(t, schedEpoch, recorded.StartedAt, "the fake clock, never the wall clock")

	// The firer reports the pid once the subprocess exists.
	firing := gate.Firings()[0]
	updated := firing.Agent
	updated.PID = 4242
	firing.Update(updated)
	schedEventually(t, "the pid to reach state.json", func() bool {
		agents := f.Run.Agents()
		return len(agents) == 1 && agents[0].PID == 4242
	})
	assert.Equal(t, 4242, f.Sched.Status().Agents[0].PID)

	gate.Open()
	schedEventually(t, "the agent to be cleared", func() bool { return len(f.Run.Agents()) == 0 })

	st, err := LoadState(f.Run.Dir())
	require.NoError(t, err)
	assert.Empty(t, st.Agents)
}

// TestSchedulerSuppressAgentEventsLeavesThemToTheFirer covers the
// supervisor that publishes its own, richer agent events. Announcing the
// agent through Firing.Update is how a firer says it did (P6-23).
func TestSchedulerSuppressAgentEventsLeavesThemToTheFirer(t *testing.T) {
	done := make(chan struct{})
	var once sync.Once
	work := schedOnce(schedReview("sc-1", domain.LensQA, 1))
	f := schedSetup(t, 10, SchedulerOptions{
		Work: work,
		Fire: work.Fire(FirerFunc(func(_ context.Context, fr Firing) (FireResult, error) {
			// What the supervisor does as it publishes its own AgentSpawned.
			fr.Update(fr.Agent)
			once.Do(func() { close(done) })
			return FireResult{}, nil
		})),
		SuppressAgentEvents: true,
	})
	<-done
	schedEventually(t, "the agent to finish", func() bool { return f.Sched.Status().Fired >= 1 })
	require.NoError(t, f.Run.Stop("test over"))
	_, err := f.Wait()
	require.NoError(t, err)
	for _, ev := range f.events() {
		switch ev.(type) {
		case events.AgentSpawned, events.AgentFinished, events.AgentFailed:
			t.Fatalf("the scheduler published %s despite SuppressAgentEvents", ev.Kind())
		}
	}
}

// TestSchedulerPublishesTheEventsAFirerNeverDid is P6-23: a planning firer
// of its own publishes no agent events, so with the supervisor's suppressed
// as well the product owner that plans the run would be invisible in the
// run log, in `sc status`'s agent counts and on the TUI's Agents tab. A
// firing that announced nothing gets its pair from the scheduler.
func TestSchedulerPublishesTheEventsAFirerNeverDid(t *testing.T) {
	plan := Demand{Kind: DemandPlan}
	work := schedOnce(plan)
	f := schedSetup(t, 10, SchedulerOptions{
		Work: work,
		Fire: work.Fire(FirerFunc(func(context.Context, Firing) (FireResult, error) {
			// A PlanRunner that is not the supervisor: it publishes nothing
			// and never records the agent.
			return FireResult{Produced: domain.MessagePlan}, nil
		})),
		SuppressAgentEvents: true,
	})
	schedEventually(t, "the planning firing to finish", func() bool { return f.Sched.Status().Fired == 1 })
	require.NoError(t, f.Run.Stop("test over"))
	_, err := f.Wait()
	require.NoError(t, err)

	var (
		spawned  []events.AgentSpawned
		finished []events.AgentFinished
	)
	for _, ev := range f.events() {
		switch e := ev.(type) {
		case events.AgentSpawned:
			spawned = append(spawned, e)
		case events.AgentFinished:
			finished = append(finished, e)
		case events.AgentFailed:
			t.Fatalf("the planning firing did not fail: %+v", e)
		}
	}
	require.Len(t, spawned, 1, "one AgentSpawned per planning firing")
	require.Len(t, finished, 1, "and one AgentFinished")
	assert.Equal(t, domain.AgentID("po-plan-1"), spawned[0].Agent.ID)
	assert.Equal(t, domain.RolePO, spawned[0].Agent.Role)
	assert.Equal(t, events.TaskPlan, spawned[0].Task)
	assert.Equal(t, 1, spawned[0].Attempt)
	assert.Equal(t, domain.AgentID("po-plan-1"), finished[0].Agent)
	assert.Equal(t, domain.MessagePlan, finished[0].Produced)
}

// TestSchedulerReportsAFiringThatFailedBeforeItsAgentExisted is the second
// half of P6-31: the supervisor returns a planning error before it registers
// the agent or publishes anything, so a firing that never reaches a
// cartridge showed up nowhere at all. It is not a crash — nothing ran — so
// it is reported as internal.
func TestSchedulerReportsAFiringThatFailedBeforeItsAgentExisted(t *testing.T) {
	boom := errors.New("the worktree is locked")
	work := schedOnce(schedImplement("sc-1", 1))
	f := schedSetup(t, 10, SchedulerOptions{
		Work: work,
		Fire: work.Fire(FirerFunc(func(context.Context, Firing) (FireResult, error) {
			return FireResult{}, boom
		})),
		SuppressAgentEvents: true,
	})
	schedEventually(t, "the firing to fail", func() bool { return f.Sched.Status().Fired == 1 })
	require.NoError(t, f.Run.Stop("test over"))
	_, err := f.Wait()
	require.NoError(t, err)

	var (
		spawned []events.AgentSpawned
		failed  []events.AgentFailed
	)
	for _, ev := range f.events() {
		switch e := ev.(type) {
		case events.AgentSpawned:
			spawned = append(spawned, e)
		case events.AgentFailed:
			failed = append(failed, e)
		}
	}
	require.Len(t, spawned, 1)
	require.Len(t, failed, 1)
	assert.Equal(t, domain.AgentID("eng-sc-1-r1"), failed[0].Agent)
	assert.Equal(t, domain.BeadID("sc-1"), failed[0].Bead)
	assert.Equal(t, events.FailureInternal, failed[0].Failure, "nothing crashed: no cartridge ran")
	assert.Equal(t, boom.Error(), failed[0].Err)
}

// --- the demand failure budget (P6-31) -----------------------------------

// TestSchedulerParksADemandWhoseFiringsKeepFailing is P6-31. A work source
// re-offers a demand every tick until the work behind it is done, so a
// firing that always fails used to be claimed and fired for ever: the
// integrator watched 612 attempts in 45 s over a stale .git/index.lock.
// The budget bounds it, the bead is parked with the reason, and the run
// finishes instead of spinning.
func TestSchedulerParksADemandWhoseFiringsKeepFailing(t *testing.T) {
	boom := errors.New("git: index.lock exists")
	var fires atomic.Int64
	claims := newSchedClaims(nil)
	parker := &schedParker{}
	f := schedSetup(t, 10, SchedulerOptions{
		// A source that never gives up, which is what every real one does.
		Work:   schedStatic(schedImplement("sc-1", 1)),
		Claims: claims,
		Fire: FirerFunc(func(context.Context, Firing) (FireResult, error) {
			fires.Add(1)
			return FireResult{}, boom
		}),
		Park: parker,
		Stop: schedFinishWhenIdle(),
	})
	out, err := f.Wait()
	require.NoError(t, err)
	assert.Equal(t, events.FinishCompleted, out.Reason, "the run finished rather than spinning")
	assert.Equal(t, int64(DefaultDemandFailureLimit), fires.Load(), "the budget bounds the retries")
	assert.Equal(t, 1, f.Sched.Status().Parked)

	require.Equal(t, []StateRequest{{
		Bead:   "sc-1",
		From:   domain.BeadStateInProgress,
		Event:  BeadEventFailed,
		Reason: "3 firings of the implement failed in a row",
		Round:  1,
	}}, parker.Requests(), "the bead is parked once, through the D-07 table")

	var parked []events.Warning
	failures := 0
	for _, ev := range f.events() {
		switch e := ev.(type) {
		case events.Warning:
			if strings.Contains(e.Message, "gave up") {
				parked = append(parked, e)
			}
		case events.AgentFailed:
			failures++
		}
	}
	require.Len(t, parked, 1, "one warning, not one a tick")
	assert.Contains(t, parked[0].Message, "3 firings failed in a row")
	assert.Equal(t, boom.Error(), parked[0].Err, "the warning names the last error")
	assert.Equal(t, domain.BeadID("sc-1"), parked[0].Bead)
	assert.Equal(t, domain.AgentID("eng-sc-1-r1-a3"), parked[0].Agent)
	assert.Equal(t, DefaultDemandFailureLimit, failures, "every failed firing was reported")
}

// TestSchedulerBudgetCountsFailuresInARowNotInARun checks the budget is
// consecutive: a firing that works clears what the failures before it
// spent, so a flaky cartridge is not parked for its history.
func TestSchedulerBudgetCountsFailuresInARowNotInARun(t *testing.T) {
	boom := errors.New("bd timed out")
	var fires atomic.Int64
	parker := &schedParker{}
	review := schedReview("sc-1", domain.LensQA, 1)
	review.From = domain.BeadStateInReview // a tester claims nothing (P6-13)
	f := schedSetup(t, 10, SchedulerOptions{
		Work: schedStatic(review),
		Fire: FirerFunc(func(context.Context, Firing) (FireResult, error) {
			// fail, fail, work, fail, fail, fail
			if fires.Add(1) == 3 {
				return FireResult{Produced: domain.MessageVerdict}, nil
			}
			return FireResult{}, boom
		}),
		Park: parker,
		Stop: schedFinishWhenIdle(),
	})
	_, err := f.Wait()
	require.NoError(t, err)
	assert.Equal(t, int64(6), fires.Load(), "the third firing reset the budget the first two spent")
	require.Len(t, parker.Requests(), 1)
	assert.Equal(t, domain.BeadStateInReview, parker.Requests()[0].From,
		"a demand that claims nothing is parked from where the source said the bead was")
}

// TestSchedulerParkNamesTheBookkeepingNotTheCartridge covers the round-3
// widening of P6-31: the supervisor refuses to re-run a session whose
// Result hook failed, so the demand comes back to the scheduler and is
// fired from scratch. It spends the budget like any other failure, and the
// warning says the agent answered.
func TestSchedulerParkNamesTheBookkeepingNotTheCartridge(t *testing.T) {
	hook := fmt.Errorf("%w: bd refused the comment", ErrPostSession)
	parker := &schedParker{}
	f := schedSetup(t, 10, SchedulerOptions{
		Work:         schedStatic(schedImplement("sc-1", 2)),
		Claims:       newSchedClaims(nil),
		FailureLimit: 1,
		Fire: FirerFunc(func(context.Context, Firing) (FireResult, error) {
			return FireResult{Failure: events.FailureInternal}, hook
		}),
		Park: parker,
		Stop: schedFinishWhenIdle(),
	})
	_, err := f.Wait()
	require.NoError(t, err)
	require.Len(t, parker.Requests(), 1)

	var parked *events.Warning
	for _, ev := range f.events() {
		if w, ok := ev.(events.Warning); ok && strings.Contains(w.Message, "gave up") {
			w := w
			parked = &w
		}
	}
	require.NotNil(t, parked)
	assert.Contains(t, parked.Message, "bookkeeping")
	assert.NotContains(t, parked.Message, "crash")
	assert.Contains(t, parked.Err, ErrPostSession.Error())
}

// TestSchedulerDoesNotSpendTheBudgetOnAStoppedFiring keeps a stop from
// looking like a failure of the work: the agents a stop cancels say nothing
// about whether their demands can ever succeed, and a resumed run must find
// its beads where it left them.
func TestSchedulerDoesNotSpendTheBudgetOnAStoppedFiring(t *testing.T) {
	gate := newSchedGate()
	parker := &schedParker{}
	f := schedSetup(t, 10, SchedulerOptions{
		Work:         schedStatic(schedImplement("sc-1", 1)),
		Claims:       newSchedClaims(nil),
		FailureLimit: 1,
		Fire:         gate,
		Park:         parker,
	})
	schedEventually(t, "the engineer to fire", func() bool { return len(gate.Firings()) == 1 })
	require.NoError(t, f.Run.Stop("test over"))
	_, err := f.Wait()
	require.NoError(t, err)
	assert.Empty(t, parker.Requests(), "a cancelled firing is not a failed demand")
	assert.Zero(t, f.Sched.Status().Parked)
}

// TestSchedulerBudgetLeavesPlanningToThePlanner keeps the scheduler out of
// P6-19's way: a product owner that cannot produce a plan is bounded by the
// planner's own give-up policy (which is longer than this budget), and a
// planning demand has no bead to park anyway.
func TestSchedulerBudgetLeavesPlanningToThePlanner(t *testing.T) {
	var fires atomic.Int64
	parker := &schedParker{}
	f := schedSetup(t, 10, SchedulerOptions{
		Work: schedStatic(Demand{Kind: DemandPlan}),
		Fire: FirerFunc(func(context.Context, Firing) (FireResult, error) {
			fires.Add(1)
			return FireResult{}, errors.New("the product owner cannot plan")
		}),
		Park: parker,
	})
	schedEventually(t, "more firings than the budget would allow", func() bool {
		return fires.Load() > int64(DefaultDemandFailureLimit)
	})
	require.NoError(t, f.Run.Stop("test over"))
	_, err := f.Wait()
	require.NoError(t, err)
	assert.Empty(t, parker.Requests())
	assert.Zero(t, f.Sched.Status().Parked)
}

// TestSchedulerFailureLimitDefaultsAndOff pins the two ends of the option.
func TestSchedulerFailureLimitDefaultsAndOff(t *testing.T) {
	t.Parallel()
	assert.Equal(t, DefaultDemandFailureLimit, NewScheduler(SchedulerOptions{}).limit)
	assert.Equal(t, 7, NewScheduler(SchedulerOptions{FailureLimit: 7}).limit)
	assert.Zero(t, NewScheduler(SchedulerOptions{FailureLimit: -1}).limit, "negative turns the budget off")
}

// --- pause, stop and finish ----------------------------------------------

// TestSchedulerFiresNothingWhilePaused checks the pause gate: running
// agents finish, no new one starts, and a resume picks the work up again.
func TestSchedulerFiresNothingWhilePaused(t *testing.T) {
	gate := newSchedGate()
	f := schedSetup(t, 10, SchedulerOptions{
		Work: schedStatic(schedReview("sc-1", domain.LensUX, 1), schedReview("sc-2", domain.LensUX, 1)),
		Fire: gate,
	})
	t.Cleanup(gate.Open)
	schedEventually(t, "both testers to fire", func() bool { return len(gate.Firings()) == 2 })

	require.NoError(t, f.Run.Pause("test"))
	// New work appears while paused; nothing may be fired for it.
	f.Sched.Kick()
	for i := 0; i < 10; i++ {
		f.Clock.Advance(2 * DefaultTickInterval)
	}
	assert.Equal(t, uint64(2), f.Sched.Status().Fired)

	require.NoError(t, f.Run.Resume("test"))
	schedEventually(t, "the loop to wake up", func() bool {
		f.Clock.Advance(2 * DefaultTickInterval)
		return f.Sched.Status().Ticks > 2
	})
	assert.Equal(t, uint64(2), f.Sched.Status().Fired, "the same work is still in flight")
}

// TestSchedulerStopsPromptlyAndCancelsItsAgents covers the shutdown
// contract: Stop cancels the loop, the agents' contexts are cancelled and
// their goroutines are waited for before the run finishes.
func TestSchedulerStopsPromptlyAndCancelsItsAgents(t *testing.T) {
	cancelled := make(chan domain.AgentID, 4)
	fire := FirerFunc(func(ctx context.Context, f Firing) (FireResult, error) {
		<-ctx.Done()
		cancelled <- f.Agent.ID
		return FireResult{Failure: events.FailureKilled}, ctx.Err()
	})
	f := schedSetup(t, 10, SchedulerOptions{
		Work: schedStatic(schedReview("sc-1", domain.LensUX, 1)),
		Fire: fire,
	})
	schedEventually(t, "the tester to fire", func() bool { return f.Sched.Live() == 1 })

	require.NoError(t, f.Run.Stop("user"))
	out, err := f.Wait()
	require.NoError(t, err)
	assert.Equal(t, events.FinishStopped, out.Reason)
	assert.Equal(t, domain.AgentID("tester-ux-sc-1-r1"), <-cancelled)
	assert.Zero(t, f.Sched.Live(), "the drain waited for the goroutine")

	var failed []events.AgentFailed
	for _, ev := range f.events() {
		if e, ok := ev.(events.AgentFailed); ok {
			failed = append(failed, e)
		}
	}
	require.Len(t, failed, 1)
	assert.Equal(t, events.FailureKilled, failed[0].Failure)
}

// TestSchedulerFinishesOnItsStopCondition checks the P6-05 hook: the
// planning loop's verdict becomes the run's outcome.
func TestSchedulerFinishesOnItsStopCondition(t *testing.T) {
	stop := StopConditionFunc(func(_ context.Context, _ *Run, st SchedulerStatus) (Outcome, bool, error) {
		if st.Fired >= 2 && st.Idle {
			return Outcome{Reason: events.FinishCompleted, Summary: "every bead settled"}, true, nil
		}
		return Outcome{}, false, nil
	})
	round := &atomic.Int64{}
	round.Store(1)
	work := WorkSourceFunc(func(context.Context) ([]Demand, error) {
		n := round.Load()
		if n > 2 {
			return nil, nil
		}
		return []Demand{schedReview("sc-1", domain.LensQA, int(n))}, nil
	})
	fire := FirerFunc(func(context.Context, Firing) (FireResult, error) {
		round.Add(1)
		return FireResult{}, nil
	})
	f := schedSetup(t, 10, SchedulerOptions{Work: work, Fire: fire, Stop: stop})
	schedEventually(t, "the run to finish", func() bool {
		f.Clock.Advance(2 * DefaultTickInterval)
		select {
		case <-f.Run.Done():
			return true
		default:
			return false
		}
	})
	out, err := f.Wait()
	require.NoError(t, err)
	assert.Equal(t, events.FinishCompleted, out.Reason)
	assert.Contains(t, out.Summary, "every bead settled")
}

// TestSchedulerStopConditionErrorFailsTheRun checks an error from the stop
// condition ends the run rather than being swallowed.
func TestSchedulerStopConditionErrorFailsTheRun(t *testing.T) {
	boom := errors.New("the planner fell over")
	f := schedSetup(t, 10, SchedulerOptions{
		Stop: StopConditionFunc(func(context.Context, *Run, SchedulerStatus) (Outcome, bool, error) {
			return Outcome{}, false, boom
		}),
	})
	_, err := f.Wait()
	assert.ErrorIs(t, err, boom)
}

// TestSchedulerWakesOnItsWakeChannel checks the event-driven half of the
// tick loop: a bead watcher's change dispatches at once, without waiting
// for the interval.
func TestSchedulerWakesOnItsWakeChannel(t *testing.T) {
	wake := make(chan struct{}, 1)
	f := schedSetup(t, 10, SchedulerOptions{
		Work: schedStatic(),
		Fire: FirerFunc(func(context.Context, Firing) (FireResult, error) { return FireResult{}, nil }),
		Wake: wake,
	})
	schedEventually(t, "the first dispatch", func() bool { return f.Sched.Status().Ticks >= 1 })
	before := f.Sched.Status().Ticks
	wake <- struct{}{}
	schedEventually(t, "a dispatch without a tick", func() bool { return f.Sched.Status().Ticks > before })
}

// --- P6-13: claims, reclaims, releases and lease renewal -----------------

// TestSchedulerMovesOneBeadThroughTwoEngineers is P6-13's Done-when: an
// engineer claims a bead, the review sends it back as changes_requested
// with the first engineer still its assignee, and a second engineer picks
// it up through Reclaim — with no ErrAlreadyClaimed anywhere.
func TestSchedulerMovesOneBeadThroughTwoEngineers(t *testing.T) {
	claims := newSchedClaims(map[domain.BeadID]schedBead{"sc-1": {status: domain.BeadStateOpen}})
	round := &atomic.Int64{}
	round.Store(1)

	work := WorkSourceFunc(func(context.Context) ([]Demand, error) {
		n := int(round.Load())
		if n > 2 {
			return nil, nil
		}
		d := schedImplement("sc-1", n)
		if n > 1 {
			// The bead came back from review: it is changes_requested and
			// still assigned to the previous engineer (D-07, Appendix A).
			d.Claim = ClaimRework
			d.Task = events.TaskAddressFeedback
		}
		return []Demand{d}, nil
	})
	fire := FirerFunc(func(_ context.Context, f Firing) (FireResult, error) {
		// The engineer submits; the review asks for changes.
		claims.Set(f.Demand.Bead, domain.BeadStateChangesRequested, f.Agent.ID)
		round.Add(1)
		return FireResult{Produced: domain.MessageImplReport}, nil
	})

	f := schedSetup(t, 10, SchedulerOptions{Work: work, Fire: fire, Claims: claims})
	schedEventually(t, "both engineers to run", func() bool {
		f.Clock.Advance(2 * DefaultTickInterval)
		return f.Sched.Status().Fired == 2
	})
	require.NoError(t, f.Run.Stop("test over"))
	_, err := f.Wait()
	require.NoError(t, err)

	assert.Equal(t, []string{
		"claim sc-1 eng-sc-1-r1",
		"reclaim sc-1 eng-sc-1-r2",
	}, claims.Calls())
	assert.Equal(t, schedBead{status: domain.BeadStateChangesRequested, assignee: "eng-sc-1-r2"}, claims.State("sc-1"))
	for _, w := range f.warnings() {
		assert.NotEqual(t, SourceScheduler, w.Source, "no claim was refused: %s", w.Message)
	}
}

// TestSchedulerReleasesBeforeHandingABeadOver checks the hand-over rule: a
// bead the source says is held by another agent is released before it is
// claimed, because bd refuses a claim on somebody else's bead.
func TestSchedulerReleasesBeforeHandingABeadOver(t *testing.T) {
	claims := newSchedClaims(map[domain.BeadID]schedBead{
		"sc-1": {status: domain.BeadStateOpen, assignee: "eng-sc-1-r1"},
	})
	d := schedImplement("sc-1", 2)
	d.HeldBy = "eng-sc-1-r1"
	fired := make(chan domain.AgentID, 2)
	f := schedSetup(t, 10, SchedulerOptions{
		Work:   schedStatic(d),
		Claims: claims,
		Fire: FirerFunc(func(ctx context.Context, fr Firing) (FireResult, error) {
			fired <- fr.Agent.ID
			<-ctx.Done()
			return FireResult{}, ctx.Err()
		}),
	})
	assert.Equal(t, domain.AgentID("eng-sc-1-r2"), <-fired)
	assert.Equal(t, []string{"release sc-1", "claim sc-1 eng-sc-1-r2"}, claims.Calls())
	assert.Equal(t, schedBead{status: domain.BeadStateInProgress, assignee: "eng-sc-1-r2"}, claims.State("sc-1"))
	require.NoError(t, f.Run.Stop("test over"))
	_, err := f.Wait()
	require.NoError(t, err)
}

// TestSchedulerRecoversFromAnUnknownAssignee covers the same hand-over when
// the source did not know who held the bead: bd answers ErrAlreadyClaimed,
// the scheduler releases and claims once more.
func TestSchedulerRecoversFromAnUnknownAssignee(t *testing.T) {
	claims := newSchedClaims(map[domain.BeadID]schedBead{
		"sc-1": {status: domain.BeadStateOpen, assignee: "eng-sc-1-r1"},
	})
	fired := make(chan domain.AgentID, 2)
	f := schedSetup(t, 10, SchedulerOptions{
		Work:   schedStatic(schedImplement("sc-1", 2)),
		Claims: claims,
		Fire: FirerFunc(func(ctx context.Context, fr Firing) (FireResult, error) {
			fired <- fr.Agent.ID
			<-ctx.Done()
			return FireResult{}, ctx.Err()
		}),
	})
	assert.Equal(t, domain.AgentID("eng-sc-1-r2"), <-fired)
	assert.Equal(t, []string{
		"claim sc-1 eng-sc-1-r2",
		"release sc-1",
		"claim sc-1 eng-sc-1-r2",
	}, claims.Calls())
	require.NoError(t, f.Run.Stop("test over"))
	_, err := f.Wait()
	require.NoError(t, err)
}

// TestSchedulerSkipsADemandItCannotClaim checks a refused claim costs a
// warning and the slot, not the run.
func TestSchedulerSkipsADemandItCannotClaim(t *testing.T) {
	claims := newSchedClaims(map[domain.BeadID]schedBead{
		"sc-1": {status: domain.BeadStateDelivering},
	})
	f := schedSetup(t, 10, SchedulerOptions{
		Work:   schedStatic(schedImplement("sc-1", 1)),
		Claims: claims,
		Fire:   FirerFunc(func(context.Context, Firing) (FireResult, error) { return FireResult{}, nil }),
	})
	schedEventually(t, "the claim to be refused", func() bool {
		f.Clock.Advance(2 * DefaultTickInterval)
		return len(claims.Calls()) > 0
	})
	require.NoError(t, f.Run.Stop("test over"))
	_, err := f.Wait()
	require.NoError(t, err)

	assert.Zero(t, f.Sched.Status().Fired, "nothing was fired")
	assert.Zero(t, f.Sched.Live(), "the reservation was given back")
	var found bool
	for _, w := range f.warnings() {
		if w.Source == SourceScheduler {
			found = true
			assert.Contains(t, w.Message, "could not claim sc-1")
			assert.Contains(t, w.Err, "not claimable")
		}
	}
	assert.True(t, found, "a refused claim owes a warning")
}

// TestSchedulerRenewsTheLeaseWhileAnEngineerRuns covers the last third of
// P6-13.
func TestSchedulerRenewsTheLeaseWhileAnEngineerRuns(t *testing.T) {
	claims := newSchedClaims(map[domain.BeadID]schedBead{"sc-1": {status: domain.BeadStateOpen}})
	f := schedSetup(t, 10, SchedulerOptions{
		Work:          schedStatic(schedImplement("sc-1", 1)),
		Claims:        claims,
		RenewInterval: beads.DefaultRenewInterval,
		Fire: FirerFunc(func(ctx context.Context, _ Firing) (FireResult, error) {
			<-ctx.Done()
			return FireResult{}, ctx.Err()
		}),
	})
	schedEventually(t, "the lease to be renewed twice", func() bool {
		f.Clock.Advance(beads.DefaultRenewInterval)
		return claims.Count("renew") >= 2
	})
	require.NoError(t, f.Run.Stop("test over"))
	_, err := f.Wait()
	require.NoError(t, err)
	assert.Equal(t, "renew sc-1 eng-sc-1-r1", claims.Calls()[1])
}

// TestSchedulerWarnsWhenTheLeaseIsLost checks a lease that went away is a
// warning, not a kill: what to do about it is the supervisor's call.
func TestSchedulerWarnsWhenTheLeaseIsLost(t *testing.T) {
	claims := newSchedClaims(map[domain.BeadID]schedBead{"sc-1": {status: domain.BeadStateOpen}})
	f := schedSetup(t, 10, SchedulerOptions{
		Work:          schedStatic(schedImplement("sc-1", 1)),
		Claims:        claims,
		RenewInterval: beads.DefaultRenewInterval,
		Fire: FirerFunc(func(ctx context.Context, _ Firing) (FireResult, error) {
			<-ctx.Done()
			return FireResult{}, ctx.Err()
		}),
	})
	schedEventually(t, "the bead to be claimed", func() bool { return claims.Count("claim") == 1 })
	// Somebody else takes the bead away.
	claims.Set("sc-1", domain.BeadStateInProgress, "eng-other")
	schedEventually(t, "the renewal to fail", func() bool {
		f.Clock.Advance(beads.DefaultRenewInterval)
		return claims.Count("renew") >= 1
	})
	schedEventually(t, "the warning", func() bool {
		for _, w := range f.warnings() {
			if w.Source == SourceScheduler {
				return true
			}
		}
		return false
	})
	assert.Equal(t, 1, f.Sched.Live(), "the agent keeps running")
	require.NoError(t, f.Run.Stop("test over"))
	_, err := f.Wait()
	require.NoError(t, err)
}

// TestSchedulerClaimsOnlyWhatTheDemandAsksFor checks the modes that write
// nothing: a tester reads a bead, it does not own it.
func TestSchedulerClaimsOnlyWhatTheDemandAsksFor(t *testing.T) {
	claims := newSchedClaims(nil)
	work := schedOnce(schedReview("sc-1", domain.LensQA, 1), Demand{Kind: DemandPlan})
	f := schedSetup(t, 10, SchedulerOptions{
		Work:   work,
		Claims: claims,
		Fire:   work.Fire(nil),
	})
	schedEventually(t, "both agents to run", func() bool {
		f.Clock.Advance(2 * DefaultTickInterval)
		return f.Sched.Status().Fired >= 2
	})
	assert.Empty(t, claims.Calls(), "no bd write for work that takes no lease")
	require.NoError(t, f.Run.Stop("test over"))
	_, err := f.Wait()
	require.NoError(t, err)
}

// TestSchedulerClaimModes exercises the claim helper directly, including
// the modes and the errors it passes through untouched.
func TestSchedulerClaimModes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("no store is a no-op", func(t *testing.T) {
		s := NewScheduler(SchedulerOptions{})
		require.NoError(t, s.claim(ctx, schedImplement("sc-1", 1), "eng-1"))
	})
	t.Run("no bead is a no-op", func(t *testing.T) {
		claims := newSchedClaims(nil)
		s := NewScheduler(SchedulerOptions{Claims: claims})
		require.NoError(t, s.claim(ctx, Demand{Kind: DemandPlan, Claim: ClaimNew}, "po-plan-1"))
		assert.Empty(t, claims.Calls())
	})
	t.Run("an unknown mode is an error", func(t *testing.T) {
		s := NewScheduler(SchedulerOptions{Claims: newSchedClaims(nil)})
		err := s.claim(ctx, Demand{Kind: DemandImplement, Bead: "sc-1", Claim: ClaimMode("borrow")}, "eng-1")
		assert.ErrorContains(t, err, "unknown claim mode")
	})
	t.Run("a failing release is reported", func(t *testing.T) {
		claims := newSchedClaims(nil)
		claims.fail["release"] = errors.New("bd is read-only")
		s := NewScheduler(SchedulerOptions{Claims: claims})
		d := schedImplement("sc-1", 1)
		d.HeldBy = "eng-other"
		err := s.claim(ctx, d, "eng-1")
		assert.ErrorContains(t, err, "release sc-1 before handing it to eng-1")
	})
	t.Run("a claim that is not claimable is passed through", func(t *testing.T) {
		claims := newSchedClaims(map[domain.BeadID]schedBead{"sc-1": {status: domain.BeadStateInReview}})
		s := NewScheduler(SchedulerOptions{Claims: claims})
		err := s.claim(ctx, schedImplement("sc-1", 1), "eng-1")
		assert.ErrorIs(t, err, beads.ErrNotClaimable)
	})
	t.Run("reclaim works from changes_requested", func(t *testing.T) {
		claims := newSchedClaims(map[domain.BeadID]schedBead{
			"sc-1": {status: domain.BeadStateChangesRequested, assignee: "eng-1"},
		})
		s := NewScheduler(SchedulerOptions{Claims: claims})
		d := schedImplement("sc-1", 2)
		d.Claim = ClaimRework
		require.NoError(t, s.claim(ctx, d, "eng-2"))
		assert.Equal(t, schedBead{status: domain.BeadStateInProgress, assignee: "eng-2"}, claims.State("sc-1"))
	})
}

// --- demands, kinds and ids ----------------------------------------------

// TestDemandKinds pins the vocabulary and the priority order.
func TestDemandKinds(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []DemandKind{DemandArbitrate, DemandReview, DemandImplement, DemandPlan}, DemandKinds())
	for i, k := range DemandKinds() {
		assert.True(t, k.Valid(), k)
		assert.Equal(t, i, k.Priority(), k)
		assert.Equal(t, string(k), k.String())
	}
	unknown := DemandKind("nap")
	assert.False(t, unknown.Valid())
	assert.Equal(t, len(DemandKinds()), unknown.Priority(), "an unknown kind sorts last")
	assert.Equal(t, domain.Role(""), unknown.Role())
	assert.Equal(t, events.Task("nap"), unknown.Task())

	assert.Equal(t, domain.RolePO, DemandArbitrate.Role())
	assert.Equal(t, domain.RolePO, DemandPlan.Role())
	assert.Equal(t, domain.RoleEngineer, DemandImplement.Role())
	assert.Equal(t, domain.RoleTester, DemandReview.Role())

	assert.Equal(t, events.TaskArbitrate, DemandArbitrate.Task())
	assert.Equal(t, events.TaskReview, DemandReview.Task())
	assert.Equal(t, events.TaskImplement, DemandImplement.Task())
	assert.Equal(t, events.TaskPlan, DemandPlan.Task())

	d := Demand{Kind: DemandImplement, Task: events.TaskResolveConflict}
	assert.Equal(t, events.TaskResolveConflict, d.FiringTask())
	assert.Equal(t, events.TaskImplement, Demand{Kind: DemandImplement}.FiringTask())
}

// TestDemandKeyIdentifiesTheWorkNotTheAttempt is what stops two agents
// landing on one job and what makes a new round new work.
func TestDemandKeyIdentifiesTheWorkNotTheAttempt(t *testing.T) {
	t.Parallel()
	a := schedReview("sc-1", domain.LensQA, 1)
	b := a
	b.Payload = "different payload"
	b.Order = 7
	assert.Equal(t, a.Key(), b.Key())

	c := a
	c.Round = 2
	assert.NotEqual(t, a.Key(), c.Key())
	d := a
	d.Lens = domain.LensUX
	assert.NotEqual(t, a.Key(), d.Key())
	assert.Equal(t, "review|sc-1|qa|1", a.Key())
	assert.Equal(t, "plan|||0", Demand{Kind: DemandPlan}.Key())

	// The task is not part of the key, so the two firings one lens owes in
	// a round that judges a pushback and re-reviews the fix (D-08) still
	// never run as two agents at once. They are two jobs for naming
	// (P6-35), which is what jobKey separates.
	pushback := a
	pushback.Task = events.TaskEvaluatePushback
	assert.Equal(t, a.Key(), pushback.Key())
	assert.NotEqual(t, jobKey(a), jobKey(pushback))
	assert.Equal(t, "review|sc-1|qa|1|review", jobKey(a))
}

// TestMintAgentIDNamesAgentsAfterTheirJob pins the ids the run log, the
// bead comments (`--actor sc:<agent-id>`) and the agent log files use.
func TestMintAgentIDNamesAgentsAfterTheirJob(t *testing.T) {
	t.Parallel()
	tests := []struct {
		demand  Demand
		attempt int
		want    domain.AgentID
	}{
		{schedImplement("sc-68s", 2), 1, "eng-sc-68s-r2"},
		{schedImplement("sc-68s", 0), 1, "eng-sc-68s"},
		{schedImplement("sc-68s", 2), 3, "eng-sc-68s-r2-a3"},
		{schedReview("sc-68s", domain.LensQA, 1), 1, "tester-qa-sc-68s-r1"},
		{Demand{Kind: DemandArbitrate, Bead: "sc-68s", Round: 3}, 1, "po-sc-68s-r3"},
		{Demand{Kind: DemandPlan}, 1, "po-plan-1"},
		{Demand{Kind: DemandPlan}, 4, "po-plan-4"},
		{Demand{Kind: DemandKind("nap"), Bead: "sc-1"}, 1, "nap-sc-1"},
		{schedPushback("sc-68s", domain.LensQA, 2), 1, "tester-qa-sc-68s-r2-pushback"},
		{schedPushback("sc-68s", domain.LensQA, 2), 2, "tester-qa-sc-68s-r2-pushback-a2"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, mintAgentID(tc.demand, tc.attempt))
	}
	// Only reviews are marked: an engineer answering feedback keeps the one
	// name its bead and round already give it.
	feedback := schedImplement("sc-68s", 2)
	feedback.Task = events.TaskAddressFeedback
	assert.Equal(t, domain.AgentID("eng-sc-68s-r2"), mintAgentID(feedback, 1))
}

// TestSchedulerNamesBothFiringsOfOneLensInOneRound is P6-35: a round that
// both judges the engineer's pushback and re-reviews the fix fires the same
// lens twice at the same round number, and the second one used to be called
// "-a2" — the suffix that means "a retry of the same job".
func TestSchedulerNamesBothFiringsOfOneLensInOneRound(t *testing.T) {
	var (
		mu    sync.Mutex
		fired []domain.AgentID
	)
	ids := func() []domain.AgentID {
		mu.Lock()
		defer mu.Unlock()
		return append([]domain.AgentID(nil), fired...)
	}
	// The feedback loop offers the pushback judgement first and the
	// re-review once it has an answer; both are round 2 of one lens.
	work := WorkSourceFunc(func(context.Context) ([]Demand, error) {
		switch len(ids()) {
		case 0:
			return []Demand{schedPushback("sc-1", domain.LensQA, 2)}, nil
		case 1:
			return []Demand{schedReview("sc-1", domain.LensQA, 2)}, nil
		default:
			return nil, nil // the round is answered
		}
	})
	f := schedSetup(t, 10, SchedulerOptions{
		Work: work,
		Fire: FirerFunc(func(_ context.Context, fr Firing) (FireResult, error) {
			mu.Lock()
			fired = append(fired, fr.Agent.ID)
			mu.Unlock()
			return FireResult{Produced: domain.MessageVerdict}, nil
		}),
	})
	schedEventually(t, "both firings", func() bool { return len(ids()) >= 2 })
	require.NoError(t, f.Run.Stop("test over"))
	_, err := f.Wait()
	require.NoError(t, err)

	all := ids()
	require.GreaterOrEqual(t, len(all), 2)
	got := all[:2]
	assert.Equal(t, []domain.AgentID{"tester-qa-sc-1-r2-pushback", "tester-qa-sc-1-r2"}, got)
	for _, id := range got {
		assert.NotContains(t, string(id), "-a2", "neither firing is a retry of the other")
	}
}

// TestSchedulerUsesTheDemandsOwnAgentID lets a source name the agent, which
// is how a resumed run keeps the id a bead already carries.
func TestSchedulerUsesTheDemandsOwnAgentID(t *testing.T) {
	d := schedImplement("sc-1", 1)
	d.Agent = "eng-from-the-resume"
	claims := newSchedClaims(nil)
	fired := make(chan domain.AgentID, 1)
	work := schedOnce(d)
	f := schedSetup(t, 10, SchedulerOptions{
		Work:   work,
		Claims: claims,
		Fire: work.Fire(FirerFunc(func(_ context.Context, fr Firing) (FireResult, error) {
			fired <- fr.Agent.ID
			return FireResult{}, nil
		})),
	})
	assert.Equal(t, domain.AgentID("eng-from-the-resume"), <-fired)
	assert.Equal(t, []string{"claim sc-1 eng-from-the-resume"}, claims.Calls())
	require.NoError(t, f.Run.Stop("test over"))
	_, err := f.Wait()
	require.NoError(t, err)
}

// TestWorkSourcesConcatenates checks the composition helper the run uses to
// join the planning, feedback and delivery sources.
func TestWorkSourcesConcatenates(t *testing.T) {
	t.Parallel()
	boom := errors.New("bd hiccuped")
	src := WorkSources(
		schedStatic(schedImplement("sc-1", 1)),
		nil,
		WorkSourceFunc(func(context.Context) ([]Demand, error) {
			return []Demand{schedReview("sc-2", domain.LensQA, 1)}, boom
		}),
	)
	ds, err := src.Demands(context.Background())
	assert.ErrorIs(t, err, boom)
	require.Len(t, ds, 2, "one broken source does not silence the others")
	assert.Equal(t, domain.BeadID("sc-1"), ds[0].Bead)
	assert.Equal(t, domain.BeadID("sc-2"), ds[1].Bead)

	empty, err := WorkSources().Demands(context.Background())
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// TestSchedulerWarnsAboutAWorkSourceError keeps a failing source from
// ending the run.
func TestSchedulerWarnsAboutAWorkSourceError(t *testing.T) {
	f := schedSetup(t, 10, SchedulerOptions{
		Work: WorkSourceFunc(func(context.Context) ([]Demand, error) {
			return []Demand{{Kind: DemandKind("nap")}}, errors.New("bd hiccuped")
		}),
		Fire: FirerFunc(func(context.Context, Firing) (FireResult, error) { return FireResult{}, nil }),
	})
	schedEventually(t, "the warnings", func() bool {
		f.Clock.Advance(2 * DefaultTickInterval)
		messages := map[string]bool{}
		for _, w := range f.warnings() {
			messages[w.Message] = true
		}
		return messages["could not work out what to schedule"] &&
			messages[`ignored a demand of unknown kind "nap"`]
	})
	assert.Zero(t, f.Sched.Status().Fired)
	require.NoError(t, f.Run.Stop("test over"))
	_, err := f.Wait()
	require.NoError(t, err)
}

// TestSchedulerStatusReportsTheAmmoInUse covers what the TUI header and
// `sc status` read.
func TestSchedulerStatusReportsTheAmmoInUse(t *testing.T) {
	gate := newSchedGate()
	f := schedSetup(t, 10, SchedulerOptions{
		Work: schedStatic(schedImplement("sc-1", 1), schedReview("sc-2", domain.LensQA, 1)),
		Fire: gate,
	})
	t.Cleanup(gate.Open)
	schedEventually(t, "both agents to fire", func() bool { return len(gate.Firings()) == 2 })
	st := f.Sched.Status()
	assert.Equal(t, 2, st.Live)
	assert.Equal(t, 1, st.ByRole[domain.RoleEngineer])
	assert.Equal(t, 1, st.ByRole[domain.RoleTester])
	assert.NotContains(t, st.ByRole, domain.RolePO, "roles with nothing live are left out")
	assert.Equal(t, 1, st.Free(domain.RoleEngineer), "2 engineer slots, 1 in use")
	assert.Equal(t, 6, st.Free(domain.RoleTester))
	assert.Equal(t, 1, st.Free(domain.RolePO))
	assert.Zero(t, st.Free(domain.Role("nobody")))
	assert.False(t, st.Idle, "agents are running")

	ids := make([]domain.AgentID, 0, len(st.Agents))
	for _, a := range st.Agents {
		ids = append(ids, a.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	assert.Equal(t, []domain.AgentID{"eng-sc-1-r1", "tester-qa-sc-2-r1"}, ids)
}

// TestSchedulerIsIdleWithNothingToDo is what a stop condition waits for.
func TestSchedulerIsIdleWithNothingToDo(t *testing.T) {
	f := schedSetup(t, 10, SchedulerOptions{
		Work: schedStatic(),
		Fire: FirerFunc(func(context.Context, Firing) (FireResult, error) { return FireResult{}, nil }),
	})
	schedEventually(t, "the scheduler to go idle", func() bool {
		f.Clock.Advance(2 * DefaultTickInterval)
		return f.Sched.Status().Idle
	})
	assert.Zero(t, f.Sched.Status().Fired)
}

// TestSchedulerRejectsANilRun is the guard on the Loop contract.
func TestSchedulerRejectsANilRun(t *testing.T) {
	t.Parallel()
	_, err := NewScheduler(SchedulerOptions{}).Run(context.Background(), nil)
	assert.ErrorContains(t, err, "nil run")
}

// TestSchedulerDefaults documents the zero SchedulerOptions.
func TestSchedulerDefaults(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerOptions{})
	assert.Equal(t, DefaultTickInterval, s.tick)
	assert.Equal(t, beads.DefaultRenewInterval, s.renew)
	assert.Equal(t, DefaultDrainGrace, s.drain)
	assert.NotNil(t, s.clock)
	assert.Equal(t, SlotCaps{Ammo: 3, PO: 1, Engineers: 1, Testers: 1}, s.Caps())
	assert.Zero(t, s.Live())

	// A kick that nobody is waiting for is dropped, never blocked on.
	s.Kick()
	s.Kick()

	custom := NewScheduler(SchedulerOptions{Tick: time.Minute, RenewInterval: -1, DrainGrace: time.Hour})
	assert.Equal(t, time.Minute, custom.tick)
	assert.Equal(t, time.Duration(-1), custom.renew, "a negative interval disables renewal")
	assert.Equal(t, time.Hour, custom.drain)
}

// TestSchedulerRunReturnsWhenItsContextEnds checks the Loop contract
// directly: a cancelled context returns promptly, with a stopped outcome.
func TestSchedulerRunReturnsWhenItsContextEnds(t *testing.T) {
	f := schedSetup(t, 10, SchedulerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := NewScheduler(SchedulerOptions{Clock: f.Clock}).Run(ctx, f.Run)
	require.NoError(t, err)
	assert.Equal(t, events.FinishStopped, out.Reason)
	assert.Contains(t, out.Summary, "stopped")
}

// TestSchedulerDrainGivesUpAfterItsGrace covers the shutdown backstop: an
// agent that ignores its context does not hold the run open forever, it is
// left to the supervisor's AgentStopper with a warning.
func TestSchedulerDrainGivesUpAfterItsGrace(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f := schedSetup(t, 10, SchedulerOptions{
		Work:       schedStatic(schedReview("sc-1", domain.LensQA, 1)),
		DrainGrace: 30 * time.Second,
		Fire: FirerFunc(func(context.Context, Firing) (FireResult, error) {
			<-release // a wedged subprocess, deaf to its context
			return FireResult{}, nil
		}),
	})
	schedEventually(t, "the tester to fire", func() bool { return f.Sched.Live() == 1 })

	require.NoError(t, f.Run.Stop("user"))
	schedEventually(t, "the drain to give up", func() bool {
		f.Clock.Advance(30 * time.Second)
		select {
		case <-f.Run.Done():
			return true
		default:
			return false
		}
	})
	out, err := f.Wait()
	require.NoError(t, err)
	assert.Equal(t, events.FinishStopped, out.Reason)

	var warned bool
	for _, w := range f.warnings() {
		if w.Source == SourceScheduler {
			warned = true
			assert.Contains(t, w.Message, "still running after 30s")
			assert.Contains(t, w.Message, "leaving them to the supervisor")
		}
	}
	assert.True(t, warned, "an abandoned agent owes a warning")
}

// TestSchedulerSortsAnUnknownLensLast keeps a demand nobody understands
// from jumping the queue.
func TestSchedulerSortsAnUnknownLensLast(t *testing.T) {
	t.Parallel()
	ds := []Demand{
		schedReview("sc-1", domain.Lens("vibes"), 1),
		schedReview("sc-1", domain.LensTest, 1),
	}
	sortDemands(ds)
	assert.Equal(t, domain.LensTest, ds[0].Lens)
}

// TestSchedulerClaimAfterAReleaseCanStillFail checks the recovery path
// reports the second refusal rather than pretending the bead is claimed.
func TestSchedulerClaimAfterAReleaseCanStillFail(t *testing.T) {
	t.Parallel()
	claims := newSchedClaims(map[domain.BeadID]schedBead{
		"sc-1": {status: domain.BeadStateOpen, assignee: "eng-other"},
	})
	claims.fail["claim"] = fmt.Errorf("bd: %w", beads.ErrAlreadyClaimed)
	s := NewScheduler(SchedulerOptions{Claims: claims})
	err := s.claim(context.Background(), schedImplement("sc-1", 1), "eng-1")
	assert.ErrorIs(t, err, beads.ErrAlreadyClaimed)
	assert.Equal(t, []string{"claim sc-1 eng-1", "release sc-1", "claim sc-1 eng-1"}, claims.Calls())

	claims.fail["release"] = errors.New("bd is read-only")
	err = s.claim(context.Background(), schedImplement("sc-1", 1), "eng-1")
	assert.ErrorContains(t, err, "release sc-1 after a lost claim")
	assert.ErrorIs(t, err, beads.ErrAlreadyClaimed)
}

// The claim is a D-07 transition bd performs atomically, so the scheduler
// publishes it itself: without this the run log, `sc status` and the TUI
// graph show a bead jumping from open straight to in_review.
func TestSchedulerAnnouncesTheClaimTransition(t *testing.T) {
	fresh := schedImplement("sc-1", 1)
	fresh.From = domain.BeadStateOpen
	rework := schedImplement("sc-2", 2)
	rework.Claim, rework.From = ClaimRework, domain.BeadStateChangesRequested
	// A conflict round: the deliverer already moved the bead, so the claim
	// is a hand-over and announces nothing.
	handover := schedImplement("sc-3", 2)
	handover.Claim, handover.From = ClaimRework, domain.BeadStateInProgress
	// A source that does not say where the bead was announces nothing.
	silent := schedImplement("sc-4", 1)

	work := schedOnce(fresh, rework, handover, silent)
	f := schedSetup(t, 10, SchedulerOptions{
		Work:   work,
		Claims: newSchedClaims(nil),
		Fire:   work.Fire(nil),
	})
	schedEventually(t, "every agent to run", func() bool {
		f.Clock.Advance(2 * DefaultTickInterval)
		return f.Sched.Status().Fired >= 4
	})
	require.NoError(t, f.Run.Stop("test over"))
	_, err := f.Wait()
	require.NoError(t, err)

	got := map[domain.BeadID]events.BeadStateChanged{}
	for _, ev := range f.events() {
		if c, ok := ev.(events.BeadStateChanged); ok {
			got[c.Bead] = c
		}
	}
	require.Contains(t, got, domain.BeadID("sc-1"))
	assert.Equal(t, domain.BeadStateOpen, got["sc-1"].From)
	assert.Equal(t, domain.BeadStateInProgress, got["sc-1"].To)
	assert.Equal(t, "claimed by eng-sc-1-r1", got["sc-1"].Reason)
	require.Contains(t, got, domain.BeadID("sc-2"))
	assert.Equal(t, domain.BeadStateChangesRequested, got["sc-2"].From)
	assert.Equal(t, 2, got["sc-2"].Round)
	assert.NotContains(t, got, domain.BeadID("sc-3"), "a bead already in_progress did not move")
	assert.NotContains(t, got, domain.BeadID("sc-4"), "a source that does not say where the bead was")
}

// TestSchedulerTellsTwoHeldItemsOfOneRoundApart is P6-25's exit criterion
// at the scheduler. A round can hold two items and each is its own dispute
// with its own product owner, so the two arbitrations are two jobs: a
// source that tracks what it has already had fired (as the feedback loop
// does) must still be owed the second after the first goes out, and the
// same dispute offered twice is still fired once. The D-05 table gives a
// run one product-owner slot, so the two rulings are made one after the
// other rather than together.
func TestSchedulerTellsTwoHeldItemsOfOneRoundApart(t *testing.T) {
	first := Demand{
		Kind: DemandArbitrate, Bead: "sc-1", Round: 2, Agent: "po-sc-1-r2-i1",
		Payload: FeedbackWork{Bead: "sc-1", Round: 2, Item: "i1"},
	}
	second := first
	second.Agent = "po-sc-1-r2-i2"
	second.Payload = FeedbackWork{Bead: "sc-1", Round: 2, Item: "i2"}
	require.NotEqual(t, first.Key(), second.Key(), "two disputed items are two jobs")

	// The same dispute is offered twice in one dispatch, as a source that
	// has not yet seen the firing would offer it.
	src := schedOnce(first, second, first)
	var mu sync.Mutex
	var ids []domain.AgentID
	record := FirerFunc(func(_ context.Context, fi Firing) (FireResult, error) {
		mu.Lock()
		defer mu.Unlock()
		ids = append(ids, fi.Agent.ID)
		return FireResult{}, nil
	})
	f := schedSetup(t, 10, SchedulerOptions{Work: src, Fire: src.Fire(record)})
	schedEventually(t, "both arbitrations to fire", func() bool {
		f.Clock.Advance(2 * DefaultTickInterval)
		return f.Sched.Status().Fired == 2
	})
	for i := 0; i < 10; i++ {
		f.Clock.Advance(2 * DefaultTickInterval)
	}
	assert.Equal(t, uint64(2), f.Sched.Status().Fired, "the same arbitration is never fired twice")
	mu.Lock()
	defer mu.Unlock()
	assert.ElementsMatch(t, []domain.AgentID{"po-sc-1-r2-i1", "po-sc-1-r2-i2"}, ids)
}
