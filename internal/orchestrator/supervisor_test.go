package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// Tests for the agent supervisor (P6-07) and the tasks folded into it: the
// read-only bd shim in every agent worktree (P6-17) and the copy-worktree
// degrade for a cartridge with no tool controls (P6-20). Every helper here
// is prefixed "sup" so it cannot collide with another Phase 6 task's file
// in this package.

// supAlpha and supBeta are the registry names of the two mock cartridges
// the multi-cartridge cases use (D-19: the fallback is implemented against
// the interface and tested with two mocks).
const (
	supAlpha = "sup-alpha"
	supBeta  = "sup-beta"
	supBare  = "sup-bare"
)

// The two mock cartridges enforce the tool policy in-process like the
// "mock" adapter does, but they are registered under their own names, so
// they say so themselves (agents.RegisterEnforcement). supBare deliberately
// registers nothing: it is the adapter with no tool controls that P6-20 is
// about.
func init() {
	for _, name := range []string{supAlpha, supBeta} {
		n := name
		err := agents.RegisterEnforcement(n, func(name string, p cartridge.ToolPolicy) (agents.Enforcement, error) {
			return agents.Enforcement{
				Cartridge: name, Policy: p, Mode: agents.EnforceNative,
				Detail: "the mock refuses forbidden steps in-process",
			}, nil
		})
		if err != nil && !strings.Contains(err.Error(), "already registered") {
			panic(err)
		}
	}
}

// supEpoch is the fixed start time the supervisor tests run from.
var supEpoch = time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)

// supClock is a fake clock the run and the supervisor share.
type supClock struct {
	mu sync.Mutex
	t  time.Time
}

func newSupClock() *supClock { return &supClock{t: supEpoch} }

func (c *supClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *supClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// supSleeper records the back-offs the supervisor waited instead of
// sleeping, so the tests are deterministic and instant.
type supSleeper struct {
	mu   sync.Mutex
	took []time.Duration
}

func (s *supSleeper) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.took = append(s.took, d)
	s.mu.Unlock()
	return nil
}

func (s *supSleeper) Waits() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.took...)
}

// supTickerHarness is a lease-renewal ticker the test drives by hand.
type supTickerHarness struct {
	ch      chan time.Time
	mu      sync.Mutex
	stopped bool
}

func newSupTicker() *supTickerHarness { return &supTickerHarness{ch: make(chan time.Time, 8)} }

func (h *supTickerHarness) New(_ time.Duration) (<-chan time.Time, func()) {
	return h.ch, func() {
		h.mu.Lock()
		h.stopped = true
		h.mu.Unlock()
	}
}

func (h *supTickerHarness) Tick() { h.ch <- supEpoch }

func (h *supTickerHarness) Stopped() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stopped
}

// supLeases records lease renewals and can fail them.
type supLeases struct {
	mu    sync.Mutex
	calls []string
	err   error
	seen  chan struct{}
}

func newSupLeases() *supLeases { return &supLeases{seen: make(chan struct{}, 16)} }

func (l *supLeases) Renew(_ context.Context, id domain.BeadID, agent domain.AgentID) (beads.Issue, error) {
	l.mu.Lock()
	l.calls = append(l.calls, fmt.Sprintf("%s/%s", id, agent))
	err := l.err
	l.mu.Unlock()
	select {
	case l.seen <- struct{}{}:
	default:
	}
	return beads.Issue{}, err
}

func (l *supLeases) Calls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

func (l *supLeases) Fail(err error) {
	l.mu.Lock()
	l.err = err
	l.mu.Unlock()
}

// supFixture is one hermetic run with a supervisor attached.
type supFixture struct {
	T        *testing.T
	Repo     string
	Run      *Run
	Bus      *events.Bus
	Sub      *events.Subscription
	Clock    *supClock
	Sleeper  *supSleeper
	Sel      *cartridge.Selector
	Adapters map[string]*mock.Adapter
	FakeBD   string
}

// supSetup builds a run whose state lives in temp directories (D-23: never
// the real home) with the given mock scenarios, one per cartridge name.
func supSetup(t *testing.T, scenarios map[string]*mock.Scenario) *supFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	repo := filepath.Join(home, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	bus := events.NewBus()
	sub := bus.Subscribe(context.Background(), 512)
	t.Cleanup(func() { sub.Unsubscribe(); bus.Close() })

	cfg := config.Defaults()
	cfg.ActiveCartridge = supAlpha
	cfg.Cartridges = map[string]config.Cartridge{}
	adapters := map[string]*mock.Adapter{}
	built := map[string]cartridge.Cartridge{}
	for name, sc := range scenarios {
		cfg.Cartridges[name] = config.Cartridge{}
		a, err := mock.NewAdapter(name, sc)
		require.NoError(t, err)
		adapters[name] = a
		built[name] = a
	}
	eff := &config.Effective{Config: cfg, Sources: map[string]config.Source{}}

	clock := newSupClock()
	rc, err := config.NewRunConfig(repo, eff, config.RunSettings{
		ID: "20260903-090000-sup0", Brief: "supervise me", StartedAt: supEpoch, Headless: true,
	})
	require.NoError(t, err)

	sel, err := cartridge.NewSelector(cfg, built, cartridge.WithClock(clock.Now))
	require.NoError(t, err)

	r, err := New(rc, Options{Bus: bus, Now: clock.Now, SkipLock: true})
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	t.Cleanup(func() {
		_ = r.Stop("test over")
		_, _ = r.Wait()
	})

	return &supFixture{
		T: t, Repo: repo, Run: r, Bus: bus, Sub: sub, Clock: clock,
		Sleeper: &supSleeper{}, Sel: sel, Adapters: adapters,
		FakeBD: supFakeBD(t, home),
	}
}

// supFakeBD writes a stand-in for the real bd binary, so the shim can be
// generated and exercised without a bd on this machine.
func supFakeBD(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, "fakebin")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "bd")
	script := "#!/bin/sh\necho \"real bd: $*\"\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o700))
	return path
}

// supervisor builds a supervisor attached to the fixture's run.
func (f *supFixture) supervisor(t *testing.T, tweak func(*SupervisorOptions)) *Supervisor {
	t.Helper()
	opts := SupervisorOptions{
		Cartridges:  f.Sel,
		Bus:         f.Bus,
		Timeout:     time.Minute,
		Now:         f.Clock.Now,
		Sleep:       f.Sleeper.Sleep,
		ShimOptions: beads.ShimOptions{Real: f.FakeBD, SkipVerify: true},
	}
	if tweak != nil {
		tweak(&opts)
	}
	s, err := NewSupervisor(opts)
	require.NoError(t, err)
	s.Attach(f.Run)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// worktree creates <run-dir>/wt/<bead> with a file in it, the D-23 layout
// the shim and the copy degrade both require.
func (f *supFixture) worktree(t *testing.T, bead domain.BeadID) string {
	t.Helper()
	wt := filepath.Join(f.Run.Dirs().Worktrees, bead.String())
	require.NoError(t, os.MkdirAll(filepath.Join(wt, "pkg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "source.txt"), []byte("the reviewed code\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "pkg", "deep.txt"), []byte("nested\n"), 0o600))
	// A worktree's .git is a file pointing back into the real repository.
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /elsewhere\n"), 0o644))
	return wt
}

// supTesterFiring is a tester review firing: read_exec, one verdict.
func supTesterFiring(agentID domain.AgentID, bead domain.BeadID, worktree string) AgentFiring {
	return AgentFiring{
		Agent: domain.Agent{
			ID: agentID, Role: domain.RoleTester, Lens: domain.LensQA, Bead: bead,
			State: domain.AgentStatePending,
		},
		Task:      events.TaskReview,
		Round:     1,
		BeadTitle: "make it good",
		Worktree:  worktree,
		Build: func(_ context.Context, fc FiringContext) (agents.AgentRun, error) {
			spec := cartridge.RunSpec{
				Prompt:  "review the diff",
				Workdir: fc.Workdir,
				Timeout: fc.Timeout,
				Env:     map[string]string{},
			}
			if err := agents.ApplyPolicy(&spec, fc.Agent.Role, fc.Task); err != nil {
				return agents.AgentRun{}, err
			}
			return agents.AgentRun{
				Cartridge: fc.Cartridge, Spec: spec, Message: domain.MessageVerdict,
				Agent: fc.Agent, Task: fc.Task, Run: fc.Run,
			}, nil
		},
	}
}

// supApproval is a scenario whose every firing approves.
func supApproval(name string) *mock.Scenario {
	s := mock.NewScenario(name)
	s.Otherwise("approve").Say("looks good").Structured(json.RawMessage(`{"outcome":"approve"}`))
	return s
}

// supEvents drains the bus and returns every event published so far.
func (f *supFixture) supEvents() []events.Event {
	var out []events.Event
	for {
		select {
		case ev, ok := <-f.Sub.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

// supFailures returns every AgentFailed published so far.
func (f *supFixture) supFailures() []events.AgentFailed {
	var out []events.AgentFailed
	for _, ev := range f.supEvents() {
		if af, ok := ev.(events.AgentFailed); ok {
			out = append(out, af)
		}
	}
	return out
}

func TestSupervisorFiresATesterAndReportsIt(t *testing.T) {
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: supApproval("alpha")})
	sup := f.supervisor(t, nil)
	wt := f.worktree(t, "bd-1")

	v, tr, err := FireAgent[domain.Verdict](context.Background(), sup, supTesterFiring("tester-qa-bd-1-r1", "bd-1", wt))
	require.NoError(t, err)
	assert.Equal(t, domain.VerdictApprove, v.Outcome)
	assert.Equal(t, supAlpha, tr.Cartridge)
	assert.Equal(t, agents.EnforceNative, tr.Enforcement.Mode)

	// The supervisor stamped the firing: workdir, timeout and the standard
	// labels.
	call, ok := f.Adapters[supAlpha].LastCall()
	require.True(t, ok)
	assert.Equal(t, wt, call.Spec.Workdir)
	assert.Equal(t, time.Minute, call.Spec.Timeout)
	assert.Equal(t, "tester-qa-bd-1-r1", call.Spec.Labels.Get(cartridge.LabelAgent))
	assert.Equal(t, "review", call.Spec.Labels.Get(cartridge.LabelTask))
	assert.Equal(t, "1", call.Spec.Labels.Get(cartridge.LabelRound))
	assert.Equal(t, string(cartridge.ToolReadExec), string(call.Spec.ToolPolicy))

	// It announced the firing and its end, and left no live agent behind.
	var spawned []events.AgentSpawned
	var finished []events.AgentFinished
	for _, ev := range f.supEvents() {
		switch e := ev.(type) {
		case events.AgentSpawned:
			spawned = append(spawned, e)
		case events.AgentFinished:
			finished = append(finished, e)
		}
	}
	require.Len(t, spawned, 1)
	assert.Equal(t, 1, spawned[0].Attempt)
	assert.Equal(t, supAlpha, spawned[0].Agent.Cartridge)
	assert.Equal(t, "make it good", spawned[0].BeadTitle)
	require.Len(t, finished, 1)
	assert.Equal(t, domain.MessageVerdict, finished[0].Produced)
	assert.Empty(t, f.Run.Agents())
	assert.Empty(t, sup.Live())
}

func TestSupervisorAppliesThePerAgentTimeout(t *testing.T) {
	sc := mock.NewScenario("slow")
	sc.Otherwise("hangs").FailWith(mock.Failure{Kind: mock.FailTimeout})
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: sc})
	sup := f.supervisor(t, func(o *SupervisorOptions) { o.Timeout = 20 * time.Millisecond })

	_, _, err := FireAgent[domain.Verdict](context.Background(), sup, supTesterFiring("tester-qa-bd-1-r1", "bd-1", f.worktree(t, "bd-1")))
	require.Error(t, err)
	require.ErrorIs(t, err, cartridge.ErrTimeout)
	assert.Equal(t, 20*time.Millisecond, sup.Timeout())

	call, ok := f.Adapters[supAlpha].LastCall()
	require.True(t, ok)
	assert.Equal(t, 20*time.Millisecond, call.Spec.Timeout, "the session carries the per-agent timeout")

	// A timeout is retryable, so it was tried 1 + DefaultCrashRetries times.
	fails := f.supFailures()
	require.Len(t, fails, 1+DefaultCrashRetries)
	for i, af := range fails {
		assert.Equal(t, events.FailureTimeout, af.Failure)
		assert.Equal(t, i+1, af.Attempt)
		assert.Equal(t, i < DefaultCrashRetries, af.WillRetry)
	}
	assert.Empty(t, f.Run.Agents(), "the failed agent is out of state.json")
}

func TestSupervisorRetriesACrashWithExponentialBackoff(t *testing.T) {
	sc := mock.NewScenario("flaky")
	sc.Rule("crash", mock.Match{}).Times(2).FailWith(mock.Failure{Kind: mock.FailCrash, ExitCode: 3})
	sc.Otherwise("approve").Say("fine").Structured(json.RawMessage(`{"outcome":"approve"}`))
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: sc})
	sup := f.supervisor(t, func(o *SupervisorOptions) {
		o.RetryBackoff = time.Second
		o.MaxRetryBackoff = 30 * time.Second
	})

	v, tr, err := FireAgent[domain.Verdict](context.Background(), sup, supTesterFiring("tester-qa-bd-2-r1", "bd-2", f.worktree(t, "bd-2")))
	require.NoError(t, err)
	assert.Equal(t, domain.VerdictApprove, v.Outcome)
	assert.NotEmpty(t, tr.Attempts)

	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second}, f.Sleeper.Waits(),
		"the back-off doubles between crash retries")

	fails := f.supFailures()
	require.Len(t, fails, 2)
	for _, af := range fails {
		assert.Equal(t, events.FailureCrash, af.Failure)
		assert.True(t, af.WillRetry)
	}
}

func TestSupervisorGivesUpAfterTheRetryBudget(t *testing.T) {
	sc := mock.NewScenario("always-crashes")
	sc.Otherwise("crash").FailWith(mock.Failure{Kind: mock.FailCrash})
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: sc})
	sup := f.supervisor(t, func(o *SupervisorOptions) {
		o.CrashRetries = 1
		o.RetryBackoff = 2 * time.Second
	})

	_, _, err := FireAgent[domain.Verdict](context.Background(), sup, supTesterFiring("tester-qa-bd-3-r1", "bd-3", f.worktree(t, "bd-3")))
	require.ErrorIs(t, err, cartridge.ErrCrash)
	assert.Equal(t, []time.Duration{2 * time.Second}, f.Sleeper.Waits())

	fails := f.supFailures()
	require.Len(t, fails, 2)
	assert.True(t, fails[0].WillRetry)
	assert.False(t, fails[1].WillRetry)
}

func TestSupervisorDoesNotRetryAnUnusableReply(t *testing.T) {
	sc := mock.NewScenario("babbles")
	sc.Otherwise("garbage").FailWith(mock.Failure{Kind: mock.FailMalformed})
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: sc})
	sup := f.supervisor(t, nil)

	_, _, err := FireAgent[domain.Verdict](context.Background(), sup, supTesterFiring("tester-qa-bd-4-r1", "bd-4", f.worktree(t, "bd-4")))
	require.Error(t, err)

	fails := f.supFailures()
	require.Len(t, fails, 1, "a bad reply has already had its schema retry inside RunAgent")
	assert.Equal(t, events.FailureBadOutput, fails[0].Failure)
	assert.False(t, fails[0].WillRetry)
	assert.Empty(t, f.Sleeper.Waits())
}

func TestSupervisorPausesARateLimitedCartridge(t *testing.T) {
	sc := mock.NewScenario("rate-limited")
	sc.Rule("limited", mock.Match{}).Once().FailWith(mock.Failure{Kind: mock.FailRateLimited})
	sc.Otherwise("approve").Say("fine").Structured(json.RawMessage(`{"outcome":"approve"}`))
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: sc})
	sup := f.supervisor(t, nil)

	_, _, err := FireAgent[domain.Verdict](context.Background(), sup, supTesterFiring("tester-qa-bd-5-r1", "bd-5", f.worktree(t, "bd-5")))
	require.NoError(t, err)

	assert.Equal(t, []time.Duration{DefaultRateLimitBackoff}, f.Sleeper.Waits(),
		"the whole cartridge is paused for the rate-limit back-off")
	assert.Zero(t, sup.Backoff(supAlpha), "a session that worked clears the back-off")

	fails := f.supFailures()
	require.Len(t, fails, 1)
	assert.Equal(t, events.FailureRateLimited, fails[0].Failure)
	assert.True(t, fails[0].WillRetry)
}

func TestSupervisorDoublesTheRateLimitBackoff(t *testing.T) {
	sc := mock.NewScenario("very-rate-limited")
	sc.Otherwise("limited").FailWith(mock.Failure{Kind: mock.FailRateLimited})
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: sc})
	sup := f.supervisor(t, func(o *SupervisorOptions) {
		o.RateLimitBackoff = time.Minute
		o.MaxRateLimitBackoff = 90 * time.Second
		o.CrashRetries = 3
	})

	_, _, err := FireAgent[domain.Verdict](context.Background(), sup, supTesterFiring("tester-qa-bd-6-r1", "bd-6", f.worktree(t, "bd-6")))
	require.ErrorIs(t, err, cartridge.ErrRateLimited)
	// Each failure pauses the whole cartridge, the last one included: the
	// firing is out of attempts but the cartridge is still rate limited for
	// everyone else. That last pause is served in the background, so only
	// the ladder up to the retries is asserted in order.
	waits := f.Sleeper.Waits()
	require.GreaterOrEqual(t, len(waits), 3)
	assert.Equal(t, []time.Duration{time.Minute, 90 * time.Second, 90 * time.Second}, waits[:3],
		"the pause doubles and is then capped")
	assert.Equal(t, 90*time.Second, sup.Backoff(supAlpha))
}

func TestSupervisorHonoursARetryAfterHint(t *testing.T) {
	sc := mock.NewScenario("polite-limit")
	sc.Rule("limited", mock.Match{}).Once().FailWith(mock.Failure{
		Kind: mock.FailRateLimited, RetryAfter: mock.Duration(7 * time.Second),
	})
	sc.Otherwise("approve").Say("fine").Structured(json.RawMessage(`{"outcome":"approve"}`))
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: sc})
	sup := f.supervisor(t, nil)

	_, _, err := FireAgent[domain.Verdict](context.Background(), sup, supTesterFiring("tester-qa-bd-7-r1", "bd-7", f.worktree(t, "bd-7")))
	require.NoError(t, err)
	assert.Equal(t, []time.Duration{7 * time.Second}, f.Sleeper.Waits())
}

func TestSupervisorRetiresAnExhaustedCartridgeAndFallsBack(t *testing.T) {
	broke := mock.NewScenario("broke")
	broke.Otherwise("no-credits").FailWith(mock.Failure{Kind: mock.FailCreditsExhausted})
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: broke, supBeta: supApproval("beta")})
	sup := f.supervisor(t, nil)

	v, tr, err := FireAgent[domain.Verdict](context.Background(), sup, supTesterFiring("tester-qa-bd-8-r1", "bd-8", f.worktree(t, "bd-8")))
	require.NoError(t, err)
	assert.Equal(t, domain.VerdictApprove, v.Outcome)
	assert.Equal(t, supBeta, tr.Cartridge, "the firing was re-routed to the live cartridge")

	assert.Equal(t, []string{supAlpha}, sup.DeadCartridges())
	assert.Equal(t, []string{supBeta}, sup.LiveCartridges())
	assert.False(t, sup.Exhausted())
	select {
	case <-sup.CreditsExhausted():
		t.Fatal("the run is not out of credits while one cartridge lives")
	default:
	}

	fails := f.supFailures()
	require.Len(t, fails, 1)
	assert.Equal(t, events.FailureCreditsExhausted, fails[0].Failure)
	assert.True(t, fails[0].WillRetry)
	assert.Empty(t, f.Sleeper.Waits(), "a re-route is not a back-off")
}

func TestSupervisorEndsTheRunWhenEveryCartridgeIsExhausted(t *testing.T) {
	broke := func(name string) *mock.Scenario {
		s := mock.NewScenario(name)
		s.Otherwise("no-credits").FailWith(mock.Failure{Kind: mock.FailCreditsExhausted})
		return s
	}
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: broke("a"), supBeta: broke("b")})
	sup := f.supervisor(t, nil)

	_, _, err := FireAgent[domain.Verdict](context.Background(), sup, supTesterFiring("tester-qa-bd-9-r1", "bd-9", f.worktree(t, "bd-9")))
	require.ErrorIs(t, err, ErrNoLiveCartridge)
	require.ErrorIs(t, err, cartridge.ErrCreditsExhausted)

	assert.Equal(t, []string{supAlpha, supBeta}, sup.DeadCartridges())
	assert.Empty(t, sup.LiveCartridges())
	assert.True(t, sup.Exhausted())
	select {
	case <-sup.CreditsExhausted():
	default:
		t.Fatal("the planning loop must be told the run is out of credits (D-18)")
	}

	var sawError bool
	for _, ev := range f.supEvents() {
		if e, ok := ev.(events.Error); ok && e.Source == SourceSupervisor {
			sawError = true
			assert.Contains(t, e.Message, "out of credits")
		}
	}
	assert.True(t, sawError, "the run says so on the event stream")

	// A later firing finds nothing to fire.
	_, _, err = FireAgent[domain.Verdict](context.Background(), sup, supTesterFiring("tester-qa-bd-9-r2", "bd-9", f.worktree(t, "bd-9")))
	require.ErrorIs(t, err, ErrNoLiveCartridge)
}

func TestSupervisorRenewsTheLeaseWhileTheAgentRuns(t *testing.T) {
	sc := mock.NewScenario("slow-approve")
	sc.Otherwise("approve").Sleep(30 * time.Millisecond).
		Say("fine").Structured(json.RawMessage(`{"outcome":"approve"}`))
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: sc})
	leases := newSupLeases()
	ticker := newSupTicker()
	sup := f.supervisor(t, func(o *SupervisorOptions) {
		o.Leases = leases
		o.Ticker = ticker.New
	})

	firing := supTesterFiring("eng-bd-10-r1", "bd-10", f.worktree(t, "bd-10"))
	done := make(chan error, 1)
	go func() {
		_, _, err := FireAgent[domain.Verdict](context.Background(), sup, firing)
		done <- err
	}()

	ticker.Tick()
	select {
	case <-leases.seen:
	case <-time.After(2 * time.Second):
		t.Fatal("the lease was never renewed")
	}
	require.NoError(t, <-done)

	assert.Equal(t, []string{"bd-10/eng-bd-10-r1"}, leases.Calls())
	assert.Equal(t, 1, sup.Renewals())
	assert.True(t, ticker.Stopped(), "the renewer stops with the agent")
}

func TestSupervisorReportsALostLease(t *testing.T) {
	sc := mock.NewScenario("slow-approve")
	sc.Otherwise("approve").Sleep(30 * time.Millisecond).
		Say("fine").Structured(json.RawMessage(`{"outcome":"approve"}`))
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: sc})
	leases := newSupLeases()
	leases.Fail(fmt.Errorf("beads: renew bd-11: %w", beads.ErrLeaseLost))
	ticker := newSupTicker()
	sup := f.supervisor(t, func(o *SupervisorOptions) {
		o.Leases = leases
		o.Ticker = ticker.New
	})

	done := make(chan error, 1)
	go func() {
		_, _, err := FireAgent[domain.Verdict](context.Background(), sup, supTesterFiring("eng-bd-11-r1", "bd-11", f.worktree(t, "bd-11")))
		done <- err
	}()
	ticker.Tick()
	select {
	case <-leases.seen:
	case <-time.After(2 * time.Second):
		t.Fatal("the lease renewal never ran")
	}
	require.NoError(t, <-done)

	var warned bool
	for _, ev := range f.supEvents() {
		if w, ok := ev.(events.Warning); ok && w.Source == SourceSupervisor && strings.Contains(w.Message, "renew the lease") {
			warned = true
		}
	}
	assert.True(t, warned, "a lost lease reaches the human")
}

func TestSupervisorStopAgentsKillsAndRefusesMore(t *testing.T) {
	sc := mock.NewScenario("long")
	sc.Otherwise("slow").Sleep(30 * time.Second).
		Say("fine").Structured(json.RawMessage(`{"outcome":"approve"}`))
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: sc})
	sup := f.supervisor(t, nil)
	wt := f.worktree(t, "bd-12")

	done := make(chan error, 1)
	go func() {
		_, _, err := FireAgent[domain.Verdict](context.Background(), sup, supTesterFiring("tester-qa-bd-12-r1", "bd-12", wt))
		done <- err
	}()
	require.Eventually(t, func() bool { return len(sup.Live()) == 1 }, 2*time.Second, time.Millisecond)

	require.NoError(t, sup.StopAgents(context.Background(), 0))
	select {
	case err := <-done:
		require.Error(t, err)
		require.ErrorIs(t, err, cartridge.ErrKilled)
	case <-time.After(2 * time.Second):
		t.Fatal("the killed firing never returned")
	}

	assert.Empty(t, sup.Live())
	assert.Empty(t, f.Run.Agents(), "the stop reaps the agent out of state.json")
	assert.True(t, sup.Stopped())

	_, _, err := FireAgent[domain.Verdict](context.Background(), sup, supTesterFiring("tester-qa-bd-12-r2", "bd-12", wt))
	require.ErrorIs(t, err, ErrSupervisorStopped)

	// The stop is also the shim cleanup path (P6-17).
	_, err = os.Stat(filepath.Join(f.Run.Dirs().Shims, "bd-12"))
	assert.True(t, os.IsNotExist(err), "the bead's shim directory is gone")
}

func TestSupervisorAbandonsARetryWhenItIsStopped(t *testing.T) {
	sc := mock.NewScenario("always-crashes")
	sc.Otherwise("crash").FailWith(mock.Failure{Kind: mock.FailCrash})
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: sc})

	var sup *Supervisor
	sup = f.supervisor(t, func(o *SupervisorOptions) {
		o.Sleep = func(_ context.Context, _ time.Duration) error {
			// The stop lands while the firing waits out its crash back-off.
			_ = sup.StopAgents(context.Background(), 0)
			return nil
		}
	})

	_, _, err := FireAgent[domain.Verdict](context.Background(), sup,
		supTesterFiring("tester-qa-bd-17-r1", "bd-17", f.worktree(t, "bd-17")))
	require.ErrorIs(t, err, ErrSupervisorStopped)
	require.ErrorIs(t, err, cartridge.ErrCrash, "the failure that was being retried is still reported")
	assert.Len(t, f.Adapters[supAlpha].Calls(), 1, "no session starts after the stop")
}

func TestSupervisorInstallsTheReadOnlyBDShim(t *testing.T) {
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: supApproval("alpha")})
	sup := f.supervisor(t, nil)
	wt := f.worktree(t, "bd-13")

	_, _, err := FireAgent[domain.Verdict](context.Background(), sup, supTesterFiring("tester-qa-bd-13-r1", "bd-13", wt))
	require.NoError(t, err)

	shim, ok := sup.Shim(wt)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(f.Run.Dirs().Shims, "bd-13"), shim.Dir,
		"the shim lives beside the worktree, never inside it (D-23)")

	call, ok := f.Adapters[supAlpha].LastCall()
	require.True(t, ok)
	path := call.Spec.Env["PATH"]
	require.NotEmpty(t, path)
	assert.Equal(t, shim.Dir, strings.SplitN(path, string(os.PathListSeparator), 2)[0],
		"the agent's PATH starts with the shim directory")

	// A read is allowed through to the real binary...
	out, err := exec.Command(shim.Path, "ready").CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "real bd: --readonly ready")

	// ...and a write is refused before bd ever runs (P3-09, P6-17).
	cmd := exec.Command(shim.Path, "create", "sneaky bead")
	out, err = cmd.CombinedOutput()
	require.Error(t, err, "a write through the shim must fail: %s", out)
	assert.Contains(t, string(out), beads.ReadOnlyMessage("create"))
	assert.NotContains(t, string(out), "real bd:")

	// And the run's cleanup path removes it.
	require.NoError(t, sup.RemoveShim("bd-13"))
	_, statErr := os.Stat(shim.Dir)
	assert.True(t, os.IsNotExist(statErr))
}

// supBareCartridge is an adapter with no tool controls at all: it ignores
// the tool policy and writes into whatever working directory it is given.
// It is the shape of a future CLI that cannot restrict itself, which is
// what the P6-20 degrade exists for.
type supBareCartridge struct {
	mu       sync.Mutex
	workdirs []string
	sawGit   bool
	sawFile  string
}

func (c *supBareCartridge) Name() string { return supBare }

func (c *supBareCartridge) Detect(_ context.Context) (cartridge.Status, error) {
	return cartridge.Status{Installed: true, Version: "bare/1", Auth: cartridge.AuthOK}, nil
}

func (c *supBareCartridge) Run(_ context.Context, spec cartridge.RunSpec) (cartridge.Session, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	p := cartridge.NewPipe(c.Name(), nil)
	go func() {
		c.mu.Lock()
		c.workdirs = append(c.workdirs, spec.Workdir)
		if _, err := os.Stat(filepath.Join(spec.Workdir, ".git")); err == nil {
			c.sawGit = true
		}
		if b, err := os.ReadFile(filepath.Join(spec.Workdir, "source.txt")); err == nil {
			c.sawFile = string(b)
		}
		c.mu.Unlock()
		// The whole point: it writes even under read_exec.
		_ = os.WriteFile(filepath.Join(spec.Workdir, "agent-wrote.txt"), []byte("mine now\n"), 0o644)

		block, err := mock.ResultBlock(json.RawMessage(`{"outcome":"approve"}`))
		if err != nil {
			p.Finish(cartridge.Result{ExitCode: 1}, err)
			return
		}
		out := "reviewed\n" + block
		p.Emit(cartridge.TextDelta(out))
		p.Finish(cartridge.Result{Output: out, ExitCode: 0}, nil)
	}()
	return p, nil
}

func (c *supBareCartridge) Workdirs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.workdirs...)
}

func TestSupervisorFiresACartridgeWithoutToolControlsInAWorktreeCopy(t *testing.T) {
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: supApproval("alpha")})
	bare := &supBareCartridge{}
	sel, err := cartridge.NewSelector(
		config.Config{ActiveCartridge: supBare, Cartridges: map[string]config.Cartridge{supBare: {}}, Roles: map[domain.Role]string{}},
		map[string]cartridge.Cartridge{supBare: bare},
	)
	require.NoError(t, err)
	sup := f.supervisor(t, func(o *SupervisorOptions) { o.Cartridges = sel })
	wt := f.worktree(t, "bd-14")

	// Without the degrade the firing would be refused outright.
	refused, err := agents.EnforcementFor(bare, cartridge.ToolReadExec)
	require.NoError(t, err)
	require.Equal(t, agents.EnforceCopyWorktree, refused.Mode)
	require.ErrorIs(t, refused.Check(), agents.ErrCopyWorktreeUnimplemented)

	v, tr, err := FireAgent[domain.Verdict](context.Background(), sup, supTesterFiring("tester-qa-bd-14-r1", "bd-14", wt))
	require.NoError(t, err)
	assert.Equal(t, domain.VerdictApprove, v.Outcome)
	assert.Equal(t, agents.EnforceCopiedWorktree, tr.Enforcement.Mode)
	assert.NoError(t, tr.Enforcement.Check())

	copyDir := filepath.Join(f.Run.Dirs().Dir, CopiesDirName, "tester-qa-bd-14-r1")
	assert.Equal(t, []string{copyDir}, bare.Workdirs(), "the session ran in the copy, not the worktree")

	// The real worktree is untouched, and the copy is gone.
	_, statErr := os.Stat(filepath.Join(wt, "agent-wrote.txt"))
	assert.True(t, os.IsNotExist(statErr), "the reviewed worktree must not have been written to")
	assert.FileExists(t, filepath.Join(wt, "source.txt"))
	_, statErr = os.Stat(copyDir)
	assert.True(t, os.IsNotExist(statErr), "the throwaway copy is discarded after the firing")

	// The copy carried the code but not the link back into the repository.
	bare.mu.Lock()
	defer bare.mu.Unlock()
	assert.Equal(t, "the reviewed code\n", bare.sawFile)
	assert.False(t, bare.sawGit, ".git must not be copied: it points into the real repository")
}

func TestCopyWorktreeCopiesTheTreeWithoutGit(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "a", "b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "a", "b", "f.txt"), []byte("hello\n"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, ".git"), []byte("gitdir: /elsewhere\n"), 0o644))
	require.NoError(t, os.Symlink("a/b/f.txt", filepath.Join(src, "link")))

	dst := filepath.Join(t.TempDir(), "copy")
	require.NoError(t, CopyWorktree(context.Background(), src, dst))

	body, err := os.ReadFile(filepath.Join(dst, "a", "b", "f.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello\n", string(body))

	fi, err := os.Stat(filepath.Join(dst, "run.sh"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), fi.Mode().Perm(), "the executable bit survives")

	li, err := os.Lstat(filepath.Join(dst, "link"))
	require.NoError(t, err)
	assert.NotZero(t, li.Mode()&os.ModeSymlink, "a symlink stays a symlink")

	_, err = os.Lstat(filepath.Join(dst, ".git"))
	assert.True(t, os.IsNotExist(err))

	// The source is untouched.
	assert.FileExists(t, filepath.Join(src, ".git"))
}

func TestSupervisorImplementsTheSchedulerFirer(t *testing.T) {
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: supApproval("alpha")})
	wt := f.worktree(t, "bd-15")

	var payload any
	sup := f.supervisor(t, func(o *SupervisorOptions) {
		o.Planner = FiringPlannerFunc(func(_ context.Context, sf Firing) (AgentFiring, error) {
			af := supTesterFiring(sf.Agent.ID, sf.Demand.Bead, wt)
			af.Agent = sf.Agent
			af.Result = func(p any, _ agents.Transcript) error {
				payload = p
				return nil
			}
			return af, nil
		})
	})

	var updated []domain.Agent
	res, err := sup.Fire(context.Background(), Firing{
		Agent:  domain.Agent{ID: "tester-qa-bd-15-r1", Role: domain.RoleTester, Lens: domain.LensQA, Bead: "bd-15"},
		Demand: Demand{Kind: DemandReview, Bead: "bd-15", Lens: domain.LensQA, Round: 1, BeadTitle: "review me"},
		Run:    f.Run,
		Update: func(a domain.Agent) { updated = append(updated, a) },
	})
	require.NoError(t, err)
	assert.Equal(t, domain.MessageVerdict, res.Produced)
	assert.Empty(t, res.Failure)

	v, ok := payload.(domain.Verdict)
	require.True(t, ok, "the planner is handed the decoded payload, got %T", payload)
	assert.Equal(t, domain.VerdictApprove, v.Outcome)

	require.Len(t, updated, 2, "the scheduler's own record hook is used, not state.json")
	assert.Equal(t, domain.AgentStateRunning, updated[0].State)
	assert.Equal(t, domain.AgentStateFinished, updated[1].State)
	assert.Empty(t, f.Run.Agents())
}

func TestSupervisorRefusesFiringsItCannotRun(t *testing.T) {
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: supApproval("alpha")})
	sup := f.supervisor(t, nil)

	_, _, err := FireAgent[domain.Verdict](context.Background(), sup, AgentFiring{})
	require.ErrorIs(t, err, ErrInvalidFiring)

	_, _, err = FireAgent[domain.Verdict](context.Background(), sup, AgentFiring{
		Agent: domain.Agent{ID: "a"}, Build: func(context.Context, FiringContext) (agents.AgentRun, error) {
			return agents.AgentRun{}, nil
		},
		Worktree: "relative/path",
	})
	require.ErrorIs(t, err, ErrInvalidFiring)

	// A supervisor with no run attached fires nothing.
	orphan, err := NewSupervisor(SupervisorOptions{Cartridges: f.Sel})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orphan.Close() })
	_, _, err = FireAgent[domain.Verdict](context.Background(), orphan,
		supTesterFiring("tester-qa-bd-16-r1", "bd-16", f.worktree(t, "bd-16")))
	require.ErrorIs(t, err, ErrNoSupervisorHost)

	// And a firing whose builder fails never starts a session.
	_, _, err = FireAgent[domain.Verdict](context.Background(), sup, AgentFiring{
		Agent: domain.Agent{ID: "tester-qa-bd-16-r2", Role: domain.RoleTester, Bead: "bd-16"},
		Task:  events.TaskReview,
		Build: func(context.Context, FiringContext) (agents.AgentRun, error) {
			return agents.AgentRun{}, errors.New("no prompt today")
		},
	})
	require.ErrorContains(t, err, "no prompt today")
	assert.Empty(t, f.Adapters[supAlpha].Calls())
}

func TestSupervisorBackoffLadder(t *testing.T) {
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: supApproval("alpha")})
	sup := f.supervisor(t, func(o *SupervisorOptions) {
		o.RetryBackoff = 10 * time.Second
		o.MaxRetryBackoff = 40 * time.Second
	})
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, 40 * time.Second},
	} {
		assert.Equal(t, tc.want, sup.retryBackoff(tc.attempt), "attempt %d", tc.attempt)
	}

	def := f.supervisor(t, nil)
	assert.Equal(t, DefaultRetryBackoff, def.retryBackoff(1))
	assert.Equal(t, DefaultCrashRetries, def.crashRetries())

	none := f.supervisor(t, func(o *SupervisorOptions) { o.CrashRetries = -1 })
	assert.Equal(t, 0, none.crashRetries())
}

// A firing with no worktree of its own — the planning product owner (P6-05)
// and the arbitrating product owner (P6-04) both read the repository rather
// than a worktree — keeps the directory its builder chose. The supervisor
// used to blank it, so agents.AgentRun.Validate refused the session.
func TestSupervisorKeepsTheWorkdirOfAWorktreeLessFiring(t *testing.T) {
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: supApproval("alpha")})
	sup := f.supervisor(t, nil)
	repo := t.TempDir()

	firing := supTesterFiring("po-plan-1", "bd-1", "")
	firing.Agent.Role = domain.RolePO
	firing.Agent.Lens = ""
	firing.Task = events.TaskPlan
	build := firing.Build
	firing.Build = func(ctx context.Context, fc FiringContext) (agents.AgentRun, error) {
		assert.Empty(t, fc.Workdir, "a worktree-less firing has no workdir to offer the builder")
		run, err := build(ctx, fc)
		if err != nil {
			return run, err
		}
		run.Spec.Workdir = repo
		return run, nil
	}

	_, _, err := FireAgent[domain.Verdict](context.Background(), sup, firing)
	require.NoError(t, err)

	call, ok := f.Adapters[supAlpha].LastCall()
	require.True(t, ok)
	assert.Equal(t, repo, call.Spec.Workdir)
}
