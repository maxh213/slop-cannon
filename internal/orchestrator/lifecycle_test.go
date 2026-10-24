package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/testutil"
)

// Tests for the run lifecycle (P6-01) and the tasks folded into it: the
// frozen config snapshot restored on --resume (P6-11), acting on
// gitx.ResumeResult (P6-15), and the worktree cleanup at finish (P6-18).
// Every helper here is prefixed "lifecycle" so it cannot collide with
// another Phase 6 task's file in this package.

// lifecycleEpoch is the fixed start time every lifecycle test runs from, so
// state.json and the events are byte-stable.
var lifecycleEpoch = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

// lifecycleClock is a fake clock; the run never reads the wall clock.
type lifecycleClock struct {
	mu sync.Mutex
	t  time.Time
}

func newLifecycleClock() *lifecycleClock { return &lifecycleClock{t: lifecycleEpoch} }

func (c *lifecycleClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *lifecycleClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// lifecycleFixture is one hermetic run: a temp state home, a temp
// repository path, an effective configuration and a bus.
type lifecycleFixture struct {
	Repo  string
	State string
	Eff   *config.Effective
	Bus   *events.Bus
	Sub   *events.Subscription
	Clock *lifecycleClock
}

// lifecycleSetup points XDG at temp directories so nothing touches the real
// home (D-23) and returns a fixture.
func lifecycleSetup(t *testing.T) *lifecycleFixture {
	t.Helper()
	home := t.TempDir()
	state := filepath.Join(home, "state")
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	repo := filepath.Join(home, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	bus := events.NewBus()
	sub := bus.Subscribe(context.Background(), 256)
	t.Cleanup(func() { sub.Unsubscribe(); bus.Close() })

	return &lifecycleFixture{
		Repo:  repo,
		State: state,
		Eff:   lifecycleEffective(10, true),
		Bus:   bus,
		Sub:   sub,
		Clock: newLifecycleClock(),
	}
}

// lifecycleEffective builds an effective configuration with one cartridge.
func lifecycleEffective(ammo int, safety bool) *config.Effective {
	cfg := config.Defaults()
	cfg.Ammo = ammo
	cfg.Safety = safety
	cfg.ActiveCartridge = "claude"
	cfg.Cartridges = map[string]config.Cartridge{"claude": {Model: "opus"}}
	return &config.Effective{Config: cfg, Sources: map[string]config.Source{}}
}

// runConfig assembles a RunConfig for the fixture.
func (f *lifecycleFixture) runConfig(t *testing.T, s config.RunSettings) *config.RunConfig {
	t.Helper()
	if s.ID == "" && !s.Resume {
		s.ID = "20260903-120000-abcd"
	}
	if s.StartedAt.IsZero() {
		s.StartedAt = lifecycleEpoch
	}
	rc, err := config.NewRunConfig(f.Repo, f.Eff, s)
	require.NoError(t, err)
	return rc
}

// options returns Options wired to the fixture's bus and fake clock.
func (f *lifecycleFixture) options() Options {
	return Options{Bus: f.Bus, Now: f.Clock.Now, StopGrace: 10 * time.Second}
}

// kinds drains the bus and returns the event kinds seen so far, in order.
func (f *lifecycleFixture) kinds() []events.Kind {
	var out []events.Kind
	for {
		select {
		case ev, ok := <-f.Sub.Events():
			if !ok {
				return out
			}
			out = append(out, ev.Kind())
		default:
			return out
		}
	}
}

// drain returns every event published so far.
func (f *lifecycleFixture) drain() []events.Event {
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

// lifecycleStopper is a fake AgentStopper.
type lifecycleStopper struct {
	mu     sync.Mutex
	graces []time.Duration
	err    error
}

func (s *lifecycleStopper) StopAgents(_ context.Context, grace time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.graces = append(s.graces, grace)
	return s.err
}

func (s *lifecycleStopper) calls() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.graces...)
}

// lifecycleLeases is a fake LeaseReleaser.
type lifecycleLeases struct {
	mu       sync.Mutex
	released []domain.BeadID
	err      error
}

func (l *lifecycleLeases) Release(_ context.Context, id domain.BeadID) (beads.Issue, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.released = append(l.released, id)
	return beads.Issue{ID: id}, l.err
}

func (l *lifecycleLeases) ids() []domain.BeadID {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]domain.BeadID(nil), l.released...)
}

// lifecycleCleaner is a fake WorktreeCleaner that prints its summary the
// way *gitx.Manager does.
type lifecycleCleaner struct {
	mu    sync.Mutex
	got   []domain.Bead
	calls int
	res   *gitx.CleanupResult
	err   error
}

func (c *lifecycleCleaner) Cleanup(_ context.Context, beadList []domain.Bead, opts gitx.CleanupOptions) (*gitx.CleanupResult, error) {
	c.mu.Lock()
	c.calls++
	c.got = append([]domain.Bead(nil), beadList...)
	c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	if opts.Out != nil {
		_, _ = fmt.Fprintln(opts.Out, c.res.Summary())
	}
	return c.res, nil
}

// lifecycleResumer is a fake GitResumer.
type lifecycleResumer struct {
	mu  sync.Mutex
	got []domain.Bead
	res *gitx.ResumeResult
	err error
}

func (r *lifecycleResumer) Resume(_ context.Context, beadList []domain.Bead) (*gitx.ResumeResult, error) {
	r.mu.Lock()
	r.got = append([]domain.Bead(nil), beadList...)
	r.mu.Unlock()
	return r.res, r.err
}

// lifecycleResumeHandler records what the resume asked for.
type lifecycleResumeHandler struct {
	mu        sync.Mutex
	redeliver []domain.BeadID
	finished  []domain.BeadID
	conflicts []domain.BeadID
	err       error
}

func (h *lifecycleResumeHandler) RedeliverPR(_ context.Context, e gitx.ResumeEntry) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.redeliver = append(h.redeliver, e.Bead.ID)
	return h.err
}

func (h *lifecycleResumeHandler) Redeliver(_ context.Context, e gitx.ResumeEntry) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.finished = append(h.finished, e.Bead.ID)
	return h.err
}

func (h *lifecycleResumeHandler) ConflictRound(_ context.Context, e gitx.ResumeEntry) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conflicts = append(h.conflicts, e.Bead.ID)
	return h.err
}

func (h *lifecycleResumeHandler) seen() ([]domain.BeadID, []domain.BeadID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]domain.BeadID(nil), h.redeliver...), append([]domain.BeadID(nil), h.conflicts...)
}

// finishedDeliveries returns the beads handed to Redeliver (P9-03).
func (h *lifecycleResumeHandler) finishedDeliveries() []domain.BeadID {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]domain.BeadID(nil), h.finished...)
}

// ---------------------------------------------------------------- lifecycle

func TestLifecycleNewValidatesItsConfig(t *testing.T) {
	f := lifecycleSetup(t)
	_, err := New(nil, Options{})
	require.ErrorContains(t, err, "nil run config")

	rc := f.runConfig(t, config.RunSettings{})
	bad := *rc
	bad.Run.ID = ""
	_, err = New(&bad, Options{})
	require.ErrorContains(t, err, "no id")

	bad = *rc
	bad.Run.StateDir = ""
	_, err = New(&bad, Options{})
	require.ErrorContains(t, err, "no state directory")

	bad = *rc
	bad.Run.RepoPath = ""
	_, err = New(&bad, Options{})
	require.ErrorContains(t, err, "repository path")
}

func TestLifecycleStartCreatesTheRunDirectoryOutsideTheRepo(t *testing.T) {
	f := lifecycleSetup(t)
	rc := f.runConfig(t, config.RunSettings{Brief: "do the thing"})
	r, err := New(rc, f.options())
	require.NoError(t, err)

	require.NoError(t, r.Start(context.Background()))
	t.Cleanup(func() { _ = r.Stop("cleanup"); _, _ = r.Wait() })

	d := r.Dirs()
	require.Equal(t, config.RunDir(f.Repo, rc.Run.ID.String()), d.Dir)
	for _, p := range []string{d.Dir, d.Agents, d.Worktrees, d.Shims} {
		info, err := os.Stat(p)
		require.NoError(t, err, p)
		require.True(t, info.IsDir(), p)
		require.Equal(t, os.FileMode(0o700), info.Mode().Perm(), p)
	}
	require.FileExists(t, StatePath(d.Dir))

	// D-23: nothing of ours is inside the target repository.
	entries, err := os.ReadDir(f.Repo)
	require.NoError(t, err)
	require.Empty(t, entries, "the run wrote into the target repository")
}

func TestLifecycleTransitionsWriteStateJSON(t *testing.T) {
	f := lifecycleSetup(t)
	rc := f.runConfig(t, config.RunSettings{Brief: "brief"})
	r, err := New(rc, f.options())
	require.NoError(t, err)
	require.Equal(t, PhaseNew, r.Phase())

	require.NoError(t, r.Start(context.Background()))
	require.Equal(t, PhaseRunning, r.Phase())
	require.Equal(t, PhaseRunning, lifecycleRead(t, r).Phase)

	f.Clock.Advance(time.Minute)
	require.NoError(t, r.Pause("user"))
	require.True(t, r.Paused())
	st := lifecycleRead(t, r)
	require.Equal(t, PhasePaused, st.Phase)
	require.Equal(t, lifecycleEpoch.Add(time.Minute), st.UpdatedAt.UTC())

	require.NoError(t, r.Pause("user"), "pausing twice is a no-op")
	require.NoError(t, r.Resume("user"))
	require.False(t, r.Paused())
	require.Equal(t, PhaseRunning, lifecycleRead(t, r).Phase)
	require.NoError(t, r.Resume("user"), "resuming twice is a no-op")

	require.NoError(t, r.Stop("sc stop"))
	out, err := r.Wait()
	require.NoError(t, err)
	require.Equal(t, events.FinishStopped, out.Reason)
	require.Equal(t, PhaseFinished, r.Phase())

	final := lifecycleRead(t, r)
	require.Equal(t, PhaseFinished, final.Phase)
	require.Equal(t, events.FinishStopped, final.Finish)
	require.Equal(t, "sc stop", final.StopReason)
	require.False(t, final.FinishedAt.IsZero())
	require.Positive(t, final.LastSeq)

	require.Equal(t, []events.Kind{
		events.KindRunStarted, events.KindRunPaused, events.KindRunResumed,
		events.KindRunStopped, events.KindRunFinished,
	}, f.kinds())
}

func TestLifecycleWaitBeforeStart(t *testing.T) {
	f := lifecycleSetup(t)
	r, err := New(f.runConfig(t, config.RunSettings{}), f.options())
	require.NoError(t, err)
	_, err = r.Wait()
	require.ErrorIs(t, err, ErrNotStarted)
	require.ErrorIs(t, r.Pause("x"), ErrNotStarted)
	require.ErrorIs(t, r.Resume("x"), ErrNotStarted)
}

func TestLifecycleStartTwiceAndAfterFinish(t *testing.T) {
	f := lifecycleSetup(t)
	r, err := New(f.runConfig(t, config.RunSettings{}), f.options())
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.ErrorIs(t, r.Start(context.Background()), ErrAlreadyStarted)
	require.NoError(t, r.Stop("done"))
	_, err = r.Wait()
	require.NoError(t, err)
	require.ErrorIs(t, r.Start(context.Background()), ErrFinished)
	require.ErrorIs(t, r.Pause("x"), ErrFinished)
	require.ErrorIs(t, r.Resume("x"), ErrFinished)
}

func TestLifecycleStopBeforeStartFinishesTheRun(t *testing.T) {
	f := lifecycleSetup(t)
	r, err := New(f.runConfig(t, config.RunSettings{}), f.options())
	require.NoError(t, err)
	require.NoError(t, r.Stop("user"))
	out, err := r.Wait()
	require.NoError(t, err)
	require.Equal(t, events.FinishStopped, out.Reason)
}

func TestLifecycleLoopCompletesTheRun(t *testing.T) {
	f := lifecycleSetup(t)
	opts := f.options()
	opts.Loop = LoopFunc(func(_ context.Context, run *Run) (Outcome, error) {
		f.Clock.Advance(2 * time.Minute)
		require.NoError(t, run.SetEpic("sc-1"))
		return Outcome{Reason: events.FinishCompleted, Summary: "3 beads landed"}, nil
	})
	r, err := New(f.runConfig(t, config.RunSettings{Brief: "b"}), opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))

	out, err := r.Wait()
	require.NoError(t, err)
	require.Equal(t, events.FinishCompleted, out.Reason)
	require.Equal(t, "3 beads landed", out.Summary)
	require.Equal(t, 2*time.Minute, out.Duration)
	require.Equal(t, domain.BeadID("sc-1"), r.Epic())
	require.Equal(t, domain.BeadID("sc-1"), lifecycleRead(t, r).Run.Epic)
}

func TestLifecycleLoopFailureFinishesFailed(t *testing.T) {
	f := lifecycleSetup(t)
	opts := f.options()
	boom := fmt.Errorf("the scheduler exploded")
	opts.Loop = LoopFunc(func(context.Context, *Run) (Outcome, error) { return Outcome{}, boom })
	r, err := New(f.runConfig(t, config.RunSettings{}), opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))

	out, err := r.Wait()
	require.ErrorIs(t, err, boom)
	require.Equal(t, events.FinishFailed, out.Reason)
	st := lifecycleRead(t, r)
	require.Equal(t, events.FinishFailed, st.Finish)
	require.Contains(t, st.FinishError, "exploded")
	require.Contains(t, f.kinds(), events.KindError)
}

func TestLifecycleStopCancelsTheLoop(t *testing.T) {
	f := lifecycleSetup(t)
	opts := f.options()
	entered := make(chan struct{})
	opts.Loop = LoopFunc(func(ctx context.Context, _ *Run) (Outcome, error) {
		close(entered)
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	})
	r, err := New(f.runConfig(t, config.RunSettings{}), opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	<-entered
	require.NoError(t, r.Stop("user"))

	out, err := r.Wait()
	require.NoError(t, err, "a loop that returns because we cancelled it is not a failure")
	require.Equal(t, events.FinishStopped, out.Reason)
}

func TestLifecycleCancellingTheContextStopsTheRun(t *testing.T) {
	f := lifecycleSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	opts := f.options()
	opts.Loop = LoopFunc(func(ctx context.Context, _ *Run) (Outcome, error) {
		<-ctx.Done()
		return Outcome{}, nil
	})
	r, err := New(f.runConfig(t, config.RunSettings{}), opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(ctx))
	cancel()

	out, err := r.Wait()
	require.NoError(t, err)
	require.Equal(t, events.FinishStopped, out.Reason)
	require.Equal(t, "context cancelled", lifecycleRead(t, r).StopReason)
}

func TestLifecyclePauseGatesTheLoop(t *testing.T) {
	f := lifecycleSetup(t)
	opts := f.options()
	paused := make(chan struct{})
	fired := make(chan struct{})
	opts.Loop = LoopFunc(func(ctx context.Context, run *Run) (Outcome, error) {
		<-paused // only reach the gate once the run really is paused
		require.True(t, run.Paused())
		require.NoError(t, run.AwaitResume(ctx))
		close(fired)
		<-ctx.Done()
		return Outcome{}, nil
	})
	r, err := New(f.runConfig(t, config.RunSettings{}), opts)
	require.NoError(t, err)
	require.ErrorIs(t, r.Pause("early"), ErrNotStarted, "pause before start is refused")
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.Pause("user"))
	close(paused)

	select {
	case <-fired:
		t.Fatal("the loop passed the pause gate while the run was paused")
	case <-time.After(20 * time.Millisecond):
	}
	require.NoError(t, r.Resume("user"))
	select {
	case <-fired:
	case <-time.After(testutil.Patience(2 * time.Second)):
		t.Fatal("the loop never got past the pause gate")
	}
	require.NoError(t, r.Stop("done"))
	_, err = r.Wait()
	require.NoError(t, err)
}

func TestLifecycleAwaitResumeReturnsOnStopAndOnContext(t *testing.T) {
	f := lifecycleSetup(t)
	r, err := New(f.runConfig(t, config.RunSettings{}), f.options())
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.Pause("user"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, r.AwaitResume(ctx), context.Canceled)

	done := make(chan error, 1)
	go func() { done <- r.AwaitResume(context.Background()) }()
	require.NoError(t, r.Stop("user"))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(testutil.Patience(2 * time.Second)):
		t.Fatal("AwaitResume did not return when the run was stopped")
	}
	_, err = r.Wait()
	require.NoError(t, err)
}

// ------------------------------------------------------------------- agents

func TestLifecycleAgentBookkeepingIsPersisted(t *testing.T) {
	f := lifecycleSetup(t)
	r, err := New(f.runConfig(t, config.RunSettings{}), f.options())
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))

	// P9-11: the agent list is coalesced into one write rather than one
	// write per agent, so the file is caught up with Run.FlushState before
	// it is read. Everything the run reports in memory is current at once.
	a := domain.Agent{ID: "eng-sc-1", Role: domain.RoleEngineer, Cartridge: "claude",
		Bead: "sc-1", State: domain.AgentStateRunning, StartedAt: lifecycleEpoch, PID: 4242}
	require.NoError(t, r.AgentStarted(a))
	require.Len(t, r.Agents(), 1)
	require.NoError(t, r.FlushState())
	require.Len(t, lifecycleRead(t, r).Agents, 1)

	a.PID = 4243
	require.NoError(t, r.AgentUpdated(a))
	require.NoError(t, r.FlushState())
	st := lifecycleRead(t, r)
	require.Len(t, st.Agents, 1)
	require.Equal(t, 4243, st.Agents[0].PID)

	require.NoError(t, r.AgentStopped("nobody"), "an unknown agent is ignored")
	require.NoError(t, r.AgentStopped(a.ID))
	require.Empty(t, r.Agents())
	require.NoError(t, r.FlushState())
	require.Empty(t, lifecycleRead(t, r).Agents)

	require.NoError(t, r.Stop("done"))
	_, err = r.Wait()
	require.NoError(t, err)
}

func TestLifecycleStopStopsAgentsAndReleasesLeases(t *testing.T) {
	f := lifecycleSetup(t)
	stopper := &lifecycleStopper{}
	leases := &lifecycleLeases{}
	opts := f.options()
	opts.Agents, opts.Leases = stopper, leases
	// sc-1 is being implemented, sc-2 is being reviewed (P6-03: only the
	// first has a released edge out of it).
	opts.Beads = func(context.Context) ([]domain.Bead, error) {
		return []domain.Bead{
			{ID: "sc-1", State: domain.BeadStateInProgress},
			{ID: "sc-2", State: domain.BeadStateInReview},
		}, nil
	}

	r, err := New(f.runConfig(t, config.RunSettings{}), opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.AgentStarted(domain.Agent{ID: "eng-1", Role: domain.RoleEngineer,
		Bead: "sc-1", State: domain.AgentStateRunning}))
	require.NoError(t, r.AgentStarted(domain.Agent{ID: "ux-1", Role: domain.RoleTester, Lens: domain.LensUX,
		Bead: "sc-1", State: domain.AgentStateRunning}))
	require.NoError(t, r.AgentStarted(domain.Agent{ID: "qa-2", Role: domain.RoleTester, Lens: domain.LensQA,
		Bead: "sc-2", State: domain.AgentStateRunning}))
	require.NoError(t, r.AgentStarted(domain.Agent{ID: "po-1", Role: domain.RolePO, State: domain.AgentStateRunning}))

	require.NoError(t, r.Stop("user"))
	_, err = r.Wait()
	require.NoError(t, err)

	require.Equal(t, []time.Duration{10 * time.Second}, stopper.calls(), "SIGTERM then a 10 s grace")
	require.Equal(t, []domain.BeadID{"sc-1"}, leases.ids(),
		"the engineer's bead is released once; the testers and the PO hold no claim")

	var moved []events.BeadStateChanged
	for _, ev := range f.drain() {
		if c, ok := ev.(events.BeadStateChanged); ok {
			moved = append(moved, c)
		}
	}
	require.Len(t, moved, 1, "the release publishes exactly one state change")
	require.Equal(t, domain.BeadID("sc-1"), moved[0].Bead)
	require.Equal(t, domain.BeadStateInProgress, moved[0].From)
	require.Equal(t, domain.BeadStateOpen, moved[0].To)
	require.Empty(t, lifecycleRead(t, r).Agents)
}

// TestLifecycleStopKeepsABeadThatCannotTakeTheReleasedEdge is the D-07 half
// of the shutdown release (P6-03): a stop while a bead is in review or in
// front of the product owner must not force it back to open. Both beads here
// carry a live agent with the engineer's role — the role that does hold the
// claim — so nothing but the transition table can save them.
func TestLifecycleStopKeepsABeadThatCannotTakeTheReleasedEdge(t *testing.T) {
	f := lifecycleSetup(t)
	leases := &lifecycleLeases{}
	opts := f.options()
	opts.Leases = leases
	opts.Beads = func(context.Context) ([]domain.Bead, error) {
		return []domain.Bead{
			{ID: "sc-1", State: domain.BeadStateInReview},
			{ID: "sc-2", State: domain.BeadStateEscalated},
			{ID: "sc-3", State: domain.BeadStateInProgress},
		}, nil
	}

	r, err := New(f.runConfig(t, config.RunSettings{}), opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	for _, b := range []domain.BeadID{"sc-1", "sc-2", "sc-3"} {
		require.NoError(t, r.AgentStarted(domain.Agent{ID: domain.AgentID("eng-" + b), Role: domain.RoleEngineer,
			Bead: b, State: domain.AgentStateRunning}))
	}

	require.NoError(t, r.Stop("user"))
	_, err = r.Wait()
	require.NoError(t, err)

	require.Equal(t, []domain.BeadID{"sc-3"}, leases.ids(),
		"only the in_progress bead has a released edge out of it")
	for _, ev := range f.drain() {
		if c, ok := ev.(events.BeadStateChanged); ok {
			require.Equal(t, domain.BeadID("sc-3"), c.Bead, "no other bead may be announced as moved")
		}
		if w, ok := ev.(events.Warning); ok {
			require.NotEqual(t, SourceState, w.Source,
				"a bead that cannot be released is kept quietly, not refused noisily")
		}
	}
}

// TestLifecycleStopFallsBackWhenTheBeadListingFails covers the other half of
// the release: bd could not say where the beads are, so the engineer's bead
// falls back to the in_progress its claim set and still goes back to the
// ready set, with one warning to say the listing failed.
func TestLifecycleStopFallsBackWhenTheBeadListingFails(t *testing.T) {
	f := lifecycleSetup(t)
	leases := &lifecycleLeases{}
	opts := f.options()
	opts.Leases = leases
	opts.Beads = func(context.Context) ([]domain.Bead, error) { return nil, fmt.Errorf("bd is down") }

	r, err := New(f.runConfig(t, config.RunSettings{}), opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.AgentStarted(domain.Agent{ID: "eng-1", Role: domain.RoleEngineer,
		Bead: "sc-1", State: domain.AgentStateRunning}))

	require.NoError(t, r.Stop("user"))
	_, err = r.Wait()
	require.NoError(t, err)

	require.Equal(t, []domain.BeadID{"sc-1"}, leases.ids())
	require.Contains(t, lifecycleWarnings(f), "could not read the run's beads before releasing their leases")
}

func TestLifecycleSecondInterruptKillsImmediately(t *testing.T) {
	f := lifecycleSetup(t)
	stopper := &lifecycleStopper{}
	leases := &lifecycleLeases{}
	cleaner := &lifecycleCleaner{res: &gitx.CleanupResult{}}
	opts := f.options()
	opts.Agents, opts.Leases, opts.Cleaner = stopper, leases, cleaner
	opts.Beads = func(context.Context) ([]domain.Bead, error) { return nil, nil }

	release := make(chan struct{})
	opts.Loop = LoopFunc(func(ctx context.Context, _ *Run) (Outcome, error) {
		<-ctx.Done()
		<-release // hold the run in flight until both interrupts have landed
		return Outcome{}, ctx.Err()
	})
	r, err := New(f.runConfig(t, config.RunSettings{}), opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.AgentStarted(domain.Agent{ID: "eng-1", Bead: "sc-1", State: domain.AgentStateRunning}))

	r.Interrupt("SIGINT")
	r.Interrupt("SIGINT")
	r.Interrupt("SIGINT") // a third has nothing left to escalate
	close(release)

	out, err := r.Wait()
	require.NoError(t, err)
	require.Equal(t, events.FinishStopped, out.Reason)
	require.Equal(t, []time.Duration{0}, stopper.calls(), "an aborted run gives no grace")
	require.Empty(t, leases.ids(), "leases are left to beads.RecoverStale on the next resume")
	require.Zero(t, cleaner.calls, "an aborted run skips the worktree cleanup")

	var warnings []string
	for _, ev := range f.drain() {
		if w, ok := ev.(events.Warning); ok {
			warnings = append(warnings, w.Message)
		}
	}
	require.Contains(t, strings.Join(warnings, "\n"), "stopping immediately")
}

func TestLifecycleSignalChannelDrivesInterrupts(t *testing.T) {
	f := lifecycleSetup(t)
	sigs := make(chan os.Signal, 2)
	opts := f.options()
	opts.Signals = sigs
	r, err := New(f.runConfig(t, config.RunSettings{}), opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))

	sigs <- os.Interrupt
	out, err := r.Wait()
	require.NoError(t, err)
	require.Equal(t, events.FinishStopped, out.Reason)
	require.Equal(t, os.Interrupt.String(), lifecycleRead(t, r).StopReason)
}

func TestLifecycleInterruptChannelInstallsAHandler(t *testing.T) {
	ch, stop := InterruptChannel()
	require.NotNil(t, ch)
	stop()
}

// -------------------------------------------------------------- P6-18 finish

func TestLifecycleCleanupPrunesAndPrintsKeptPaths(t *testing.T) {
	f := lifecycleSetup(t)
	beadList := []domain.Bead{
		{ID: "sc-1", State: domain.BeadStateClosed},
		{ID: "sc-2", State: domain.BeadStateFailed},
	}
	cleaner := &lifecycleCleaner{res: &gitx.CleanupResult{
		Default: "main",
		Entries: []gitx.CleanupEntry{
			{BeadID: "sc-1", Path: "/wt/sc-1", State: domain.BeadStateClosed, Action: gitx.CleanupRemoved},
			{BeadID: "sc-2", Path: "/wt/sc-2", State: domain.BeadStateFailed, Action: gitx.CleanupKept,
				Reason: "the bead is failed"},
		},
	}}
	var out strings.Builder
	opts := f.options()
	opts.Cleaner = cleaner
	opts.Out = &out
	opts.Beads = func(context.Context) ([]domain.Bead, error) { return beadList, nil }

	r, err := New(f.runConfig(t, config.RunSettings{}), opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.Stop("user"))
	outcome, err := r.Wait()
	require.NoError(t, err)

	require.Equal(t, 1, cleaner.calls)
	require.Equal(t, beadList, cleaner.got, "the run's own beads with their final states")
	require.Contains(t, out.String(), "removed 1 worktree, kept 1:")
	require.Contains(t, out.String(), "sc-2 (failed): /wt/sc-2")
	require.Contains(t, outcome.Summary, "/wt/sc-2", "the kept paths reach RunFinished.Summary too")
	require.Contains(t, lifecycleRead(t, r).FinishSummary, "/wt/sc-2")
}

func TestLifecycleCleanupFailureIsAWarningNotAFailure(t *testing.T) {
	f := lifecycleSetup(t)
	opts := f.options()
	opts.Cleaner = &lifecycleCleaner{err: fmt.Errorf("git is on fire")}
	opts.Beads = func(context.Context) ([]domain.Bead, error) { return nil, nil }
	r, err := New(f.runConfig(t, config.RunSettings{}), opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.Stop("user"))
	out, err := r.Wait()
	require.NoError(t, err)
	require.Equal(t, events.FinishStopped, out.Reason)
	require.Contains(t, lifecycleWarnings(f), "the worktree cleanup did not run")
}

func TestLifecycleHeadlessPrintsTheReportToStdoutByDefault(t *testing.T) {
	f := lifecycleSetup(t)
	rc := f.runConfig(t, config.RunSettings{Headless: true})
	r, err := New(rc, f.options())
	require.NoError(t, err)
	require.True(t, r.Snapshot().Headless)
	require.Equal(t, os.Stdout, r.cleanupOut())

	tui := f.runConfig(t, config.RunSettings{})
	rt, err := New(tui, f.options())
	require.NoError(t, err)
	require.Nil(t, rt.cleanupOut(), "the TUI owns the terminal; the report goes to the log")
}

// ---------------------------------------------------------------- P6-15 resume

func TestLifecycleResumeActsOnTheGitResumeResult(t *testing.T) {
	f := lifecycleSetup(t)
	// A finished run to resume.
	first, err := New(f.runConfig(t, config.RunSettings{Brief: "b"}), f.options())
	require.NoError(t, err)
	require.NoError(t, first.Start(context.Background()))
	require.NoError(t, first.SetEpic("sc-epic"))
	require.NoError(t, first.Stop("crash"))
	_, err = first.Wait()
	require.NoError(t, err)

	beadList := []domain.Bead{
		{ID: "sc-1", State: domain.BeadStateDelivering},
		{ID: "sc-2", State: domain.BeadStateInProgress},
		{ID: "sc-3", State: domain.BeadStateInReview},
	}
	resumer := &lifecycleResumer{res: &gitx.ResumeResult{
		Default:  "main",
		Warnings: []string{"gh is unavailable"},
		Entries: []gitx.ResumeEntry{
			{Bead: beadList[0], State: domain.BeadStateDelivering, PRState: "OPEN", Reason: "the pull request is open"},
			{Bead: beadList[1], State: domain.BeadStateInProgress, Conflict: true,
				ConflictFiles: []string{"a.go"}, Reason: "the worktree is mid-rebase"},
			// Applied: gitx wrote the close to bd, so the run announces it.
			{Bead: beadList[2], State: domain.BeadStateClosed, Applied: true, Reason: "the branch landed on main"},
		},
	}}
	handler := &lifecycleResumeHandler{}

	opts := f.options()
	opts.Resumer, opts.OnResume = resumer, handler
	opts.Beads = func(context.Context) ([]domain.Bead, error) { return beadList, nil }

	rc := f.runConfig(t, config.RunSettings{Resume: true})
	require.True(t, rc.Resume)
	res, err := RestoreRunConfig(rc)
	require.NoError(t, err)
	require.True(t, res.Restored)

	r, err := New(rc, opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))

	redeliver, conflicts := handler.seen()
	require.Equal(t, []domain.BeadID{"sc-1"}, redeliver)
	require.Equal(t, []domain.BeadID{"sc-2"}, conflicts)
	require.Same(t, resumer.res, r.ResumeResult())
	require.Equal(t, beadList, resumer.got)

	kinds := f.kinds()
	require.Equal(t, events.KindRunStarted, kinds[0])
	require.Contains(t, kinds, events.KindWarning, "the resume warning is published")
	require.Contains(t, kinds, events.KindBeadStateChanged, "sc-3 was reconciled to closed")

	st := lifecycleRead(t, r)
	require.Equal(t, 1, st.Resumes)
	require.Equal(t, domain.BeadID("sc-epic"), st.Run.Epic, "the epic survives the resume")
	require.Equal(t, "b", st.Run.Brief)

	require.NoError(t, r.Stop("done"))
	_, err = r.Wait()
	require.NoError(t, err)
}

func TestLifecycleResumeConflictWinsOverAPullRequest(t *testing.T) {
	f := lifecycleSetup(t)
	bead := domain.Bead{ID: "sc-9", State: domain.BeadStateDelivering}
	handler := &lifecycleResumeHandler{}
	opts := f.options()
	opts.Resumer = &lifecycleResumer{res: &gitx.ResumeResult{Entries: []gitx.ResumeEntry{
		{Bead: bead, State: bead.State, Conflict: true, PRState: "OPEN"},
	}}}
	opts.OnResume = handler
	opts.Beads = func(context.Context) ([]domain.Bead, error) { return []domain.Bead{bead}, nil }

	rc := f.runConfig(t, config.RunSettings{})
	rc.Resume = true
	r, err := New(rc, opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	redeliver, conflicts := handler.seen()
	require.Empty(t, redeliver, "a worktree left mid-rebase cannot be delivered")
	require.Equal(t, []domain.BeadID{"sc-9"}, conflicts)
	require.NoError(t, r.Stop("done"))
	_, err = r.Wait()
	require.NoError(t, err)
}

// TestLifecycleResumeFinishesAnInterruptedDelivery is the P9-03 dispatch:
// a bead the crash left in delivering with nothing to point at — no pull
// request, no conflict, nothing landed — is handed to Redeliver, because
// nothing else in the run ships a delivering bead and the run would
// otherwise spin until it was stopped (D-20).
func TestLifecycleResumeFinishesAnInterruptedDelivery(t *testing.T) {
	f := lifecycleSetup(t)
	stuck := domain.Bead{ID: "sc-1", State: domain.BeadStateDelivering}
	withPR := domain.Bead{ID: "sc-2", State: domain.BeadStateDelivering}
	rebasing := domain.Bead{ID: "sc-3", State: domain.BeadStateDelivering}
	blind := domain.Bead{ID: "sc-4", State: domain.BeadStateDelivering}
	working := domain.Bead{ID: "sc-5", State: domain.BeadStateInProgress}
	beadList := []domain.Bead{stuck, withPR, rebasing, blind, working}

	handler := &lifecycleResumeHandler{}
	opts := f.options()
	opts.Resumer = &lifecycleResumer{res: &gitx.ResumeResult{Entries: []gitx.ResumeEntry{
		{Bead: stuck, State: stuck.State, Reason: "the delivery did not complete; deliver again"},
		{Bead: withPR, State: withPR.State, PRState: "OPEN"},
		{Bead: rebasing, State: rebasing.State, Conflict: true},
		// Inspection failed, so the branch state is a guess: delivering
		// from a guess is how work gets lost.
		{Bead: blind, State: blind.State, Err: fmt.Errorf("git is unwell")},
		{Bead: working, State: working.State},
	}}}
	opts.OnResume = handler
	opts.Beads = func(context.Context) ([]domain.Bead, error) { return beadList, nil }

	rc := f.runConfig(t, config.RunSettings{})
	rc.Resume = true
	r, err := New(rc, opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))

	redeliver, conflicts := handler.seen()
	require.Equal(t, []domain.BeadID{"sc-1"}, handler.finishedDeliveries(),
		"only the bead with nothing to point at is delivered again")
	require.Equal(t, []domain.BeadID{"sc-2"}, redeliver, "the pull request keeps its own hand-off")
	require.Equal(t, []domain.BeadID{"sc-3"}, conflicts, "and the conflict keeps its own")

	require.NoError(t, r.Stop("done"))
	_, err = r.Wait()
	require.NoError(t, err)
}

func TestLifecycleResumeHandlerFailuresAreWarnings(t *testing.T) {
	f := lifecycleSetup(t)
	bead := domain.Bead{ID: "sc-9", State: domain.BeadStateDelivering}
	opts := f.options()
	opts.Resumer = &lifecycleResumer{res: &gitx.ResumeResult{Entries: []gitx.ResumeEntry{
		{Bead: bead, State: bead.State, PRState: "MERGED", Err: fmt.Errorf("worktree gone")},
	}}}
	opts.OnResume = &lifecycleResumeHandler{err: fmt.Errorf("gh exploded")}
	opts.Beads = func(context.Context) ([]domain.Bead, error) { return []domain.Bead{bead}, nil }

	rc := f.runConfig(t, config.RunSettings{})
	rc.Resume = true
	r, err := New(rc, opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	w := lifecycleWarnings(f)
	require.Contains(t, w, "could not be reattached")
	require.Contains(t, w, "could not re-run the pull request delivery")
	require.NoError(t, r.Stop("done"))
	_, err = r.Wait()
	require.NoError(t, err)
}

func TestLifecycleResumeThatCannotInspectTheRepoFails(t *testing.T) {
	f := lifecycleSetup(t)
	opts := f.options()
	opts.Resumer = &lifecycleResumer{err: fmt.Errorf("not a git repository")}
	opts.Beads = func(context.Context) ([]domain.Bead, error) { return nil, nil }

	rc := f.runConfig(t, config.RunSettings{})
	rc.Resume = true
	r, err := New(rc, opts)
	require.NoError(t, err)
	err = r.Start(context.Background())
	require.ErrorContains(t, err, "not a git repository")
	out, waitErr := r.Wait()
	require.Error(t, waitErr)
	require.Equal(t, events.FinishFailed, out.Reason)
}

// ------------------------------------------------------------ P6-11 snapshot

func TestLifecycleResumeRestoresTheFrozenConfigSnapshot(t *testing.T) {
	f := lifecycleSetup(t)
	rc := f.runConfig(t, config.RunSettings{Brief: "the original brief"})
	require.Equal(t, 10, rc.Run.Config.Ammo)
	r, err := New(rc, f.options())
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.SetEpic("sc-epic"))
	require.NoError(t, r.Stop("crash"))
	_, err = r.Wait()
	require.NoError(t, err)

	// The user edits the config files between the two invocations.
	f.Eff = lifecycleEffective(4, false)
	f.Eff.Cartridges["claude"] = config.Cartridge{Model: "haiku"}
	resumed := f.runConfig(t, config.RunSettings{Resume: true, Headless: true})
	require.Equal(t, 4, resumed.Run.Config.Ammo, "rebuilt from the current files, as P1-08 leaves it")

	res, err := RestoreRunConfig(resumed)
	require.NoError(t, err)
	require.True(t, res.Restored)
	require.Empty(t, res.Overridden)

	require.Equal(t, 10, resumed.Run.Config.Ammo, "the frozen ammo wins over the edited file")
	require.True(t, resumed.Run.Config.Safety)
	require.Equal(t, 10, resumed.Allocation.Ammo)
	require.Equal(t, config.Allocation{Ammo: 10, PO: 1, MaxEngineers: 2, MaxTesters: 7}, resumed.Allocation)
	require.Equal(t, "opus", resumed.Run.Config.Model, "the frozen model wins")
	require.Equal(t, "opus", resumed.Cartridge.Model)
	require.Equal(t, "the original brief", resumed.Run.Brief)
	require.Equal(t, domain.RunModeBrief, resumed.Run.Mode)
	require.Equal(t, domain.BeadID("sc-epic"), resumed.Run.Epic)
	require.Equal(t, lifecycleEpoch, resumed.Run.StartedAt.UTC())
	require.True(t, resumed.Run.Config.Headless, "this invocation's presentation flags still win")
}

func TestLifecycleResumeHonoursExplicitFlagOverrides(t *testing.T) {
	f := lifecycleSetup(t)
	r, err := New(f.runConfig(t, config.RunSettings{Brief: "b"}), f.options())
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.Stop("crash"))
	_, err = r.Wait()
	require.NoError(t, err)

	f.Eff = lifecycleEffective(10, true)
	ammo, safety := 6, false
	require.NoError(t, f.Eff.Apply(config.Overrides{Ammo: &ammo, Safety: &safety}))
	resumed := f.runConfig(t, config.RunSettings{Resume: true})

	res, err := RestoreRunConfig(resumed)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{config.KeyAmmo, config.KeySafety}, res.Overridden)
	require.Equal(t, 6, resumed.Run.Config.Ammo)
	require.False(t, resumed.Run.Config.Safety)
	require.Equal(t, 6, resumed.Allocation.Ammo)
	require.Contains(t, strings.Join(res.Notes, "\n"), "--ammo 6 overrides")
	require.NotContains(t, strings.Join(res.Notes, "\n"), "config now says", "a flag is not drift")
}

// P6-11 keeps a resumed run on its recorded footing, but a config file
// that now says otherwise is named, with the flag that would apply it: a
// repo config switched to safety off must not be ignored silently.
func TestLifecycleResumeNamesAConfigFileThatDrifted(t *testing.T) {
	f := lifecycleSetup(t)
	r, err := New(f.runConfig(t, config.RunSettings{Brief: "b"}), f.options())
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.Stop("crash"))
	_, err = r.Wait()
	require.NoError(t, err)

	f.Eff = lifecycleEffective(4, false)
	f.Eff.Sources[config.KeySafety] = config.SourceRepo
	f.Eff.Sources[config.KeyAmmo] = config.SourceGlobal
	resumed := f.runConfig(t, config.RunSettings{Resume: true})

	res, err := RestoreRunConfig(resumed)
	require.NoError(t, err)
	require.Empty(t, res.Overridden)
	require.True(t, resumed.Run.Config.Safety, "the recorded safety still wins")
	require.Equal(t, 10, resumed.Run.Config.Ammo)
	notes := strings.Join(res.Notes, "\n")
	require.Contains(t, notes, "the repo config now says safety off, but run "+string(resumed.Run.ID)+" keeps its recorded safety on; pass --safety off to change it")
	require.Contains(t, notes, "the global config now says ammo 4")
	require.Contains(t, notes, "pass --ammo 4")
	require.NotContains(t, notes, "cartridge")
}

// D-31: what a run may merge is frozen like safety. A config file edited
// between the invocations does not change it, --auto-merge does, and a
// resume that would leave auto-merge on with safety off is refused.
func TestLifecycleResumeKeepsTheRecordedAutoMerge(t *testing.T) {
	f := lifecycleSetup(t)
	f.Eff.AutoMerge = true
	f.Eff.AutoMergeChecks = []string{"test"}
	f.Eff.AutoMergeSpacing = config.NewDuration(15 * time.Minute)
	r, err := New(f.runConfig(t, config.RunSettings{Brief: "b"}), f.options())
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.Stop("crash"))
	_, err = r.Wait()
	require.NoError(t, err)

	// The repo config turns it off again: the run keeps merging, and says so.
	f.Eff = lifecycleEffective(10, true)
	f.Eff.Sources[config.KeyAutoMerge] = config.SourceRepo
	resumed := f.runConfig(t, config.RunSettings{Resume: true})
	res, err := RestoreRunConfig(resumed)
	require.NoError(t, err)
	require.True(t, resumed.Run.Config.AutoMerge, "the recorded auto_merge wins over the edited file")
	require.Equal(t, []string{"test"}, resumed.Run.Config.AutoMergeChecks)
	require.Equal(t, 15*time.Minute, resumed.Run.Config.AutoMergeSpacing)
	require.Contains(t, strings.Join(res.Notes, "\n"), "keeps its recorded auto_merge true; pass --auto-merge=false to change it")

	// --auto-merge=false is a deliberate act and wins.
	f.Eff = lifecycleEffective(10, true)
	off := false
	require.NoError(t, f.Eff.Apply(config.Overrides{AutoMerge: &off}))
	resumed = f.runConfig(t, config.RunSettings{Resume: true})
	res, err = RestoreRunConfig(resumed)
	require.NoError(t, err)
	require.Contains(t, res.Overridden, config.KeyAutoMerge)
	require.False(t, resumed.Run.Config.AutoMerge)
	require.Empty(t, resumed.Run.Config.AutoMergeChecks)

	// --safety off alone would leave the recorded auto-merge on with no
	// pull requests to merge, so it is refused.
	f.Eff = lifecycleEffective(10, true)
	require.NoError(t, f.Eff.Apply(config.Overrides{Safety: &off}))
	resumed = f.runConfig(t, config.RunSettings{Resume: true})
	_, err = RestoreRunConfig(resumed)
	require.ErrorIs(t, err, config.ErrAutoMergeNeedsSafety)
}

// D-31: a run recorded with auto-merge on and no required check (an older
// sc allowed it) is not resumed as one that merges on whatever checks
// happen to have registered.
func TestLifecycleResumeRefusesAutoMergeWithoutARequiredCheck(t *testing.T) {
	f := lifecycleSetup(t)
	f.Eff.AutoMerge = true
	r, err := New(f.runConfig(t, config.RunSettings{Brief: "b"}), f.options())
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.Stop("crash"))
	_, err = r.Wait()
	require.NoError(t, err)

	f.Eff = lifecycleEffective(10, true)
	_, err = RestoreRunConfig(f.runConfig(t, config.RunSettings{Resume: true}))
	require.ErrorIs(t, err, config.ErrAutoMergeNeedsChecks)

	off := false
	f.Eff = lifecycleEffective(10, true)
	require.NoError(t, f.Eff.Apply(config.Overrides{AutoMerge: &off}))
	_, err = RestoreRunConfig(f.runConfig(t, config.RunSettings{Resume: true}))
	require.NoError(t, err, "--auto-merge=false resumes it without merging")
}

// D-31: the time of the run's last auto-merge is in state.json before the
// merge is asked for, and a resumed run reads it back, so
// auto_merge_spacing is kept between invocations.
func TestLifecycleAutoMergeLedgerSurvivesAResume(t *testing.T) {
	f := lifecycleSetup(t)
	rc := f.runConfig(t, config.RunSettings{Brief: "b"})
	r, err := New(rc, f.options())
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.True(t, r.LastAutoMerge().IsZero())
	at := lifecycleEpoch.Add(3 * time.Minute)
	require.NoError(t, r.RecordAutoMerge(at, "fx-1", 7))

	// Durable at once, not at the next transition.
	st, err := LoadState(rc.Run.StateDir)
	require.NoError(t, err)
	require.NotNil(t, st.AutoMerge)
	require.Equal(t, AutoMergeState{LastAt: at, LastBead: "fx-1", LastPR: 7, Merges: 1}, *st.AutoMerge)

	require.NoError(t, r.Stop("crash"))
	_, err = r.Wait()
	require.NoError(t, err)

	resumed := f.runConfig(t, config.RunSettings{Resume: true})
	resumed.Resume = true
	r2, err := New(resumed, f.options())
	require.NoError(t, err)
	require.True(t, at.Equal(r2.LastAutoMerge()), "the resumed run spaces its merges from the last one")
	require.NoError(t, r2.RecordAutoMerge(at.Add(time.Hour), "fx-2", 8))
	require.Equal(t, 2, r2.State().AutoMerge.Merges)

	// The heads sent back are durable too, and bounded.
	require.NoError(t, r2.RecordAutoMergeSentBack([]string{"check:abc:test"}))
	st, err = LoadState(resumed.Run.StateDir)
	require.NoError(t, err)
	require.Equal(t, []string{"check:abc:test"}, st.AutoMerge.SentBack)
	require.Equal(t, 2, st.AutoMerge.Merges, "the merge record is kept")
	for i := range autoMergeSentBackLimit {
		require.NoError(t, r2.RecordAutoMergeSentBack([]string{fmt.Sprintf("conflict:%d", i)}))
	}
	keys := r2.AutoMergeSentBack()
	require.Len(t, keys, autoMergeSentBackLimit)
	require.Equal(t, "conflict:0", keys[0], "the oldest key went first")
	require.NoError(t, r2.Close())
}

// The update requests behind the approved-tip guard (D-31) are written to
// state.json at once, read back by a resumed run, deduplicated and bounded.
func TestLifecycleAutoMergeUpdateRequestsSurviveAResume(t *testing.T) {
	f := lifecycleSetup(t)
	rc := f.runConfig(t, config.RunSettings{Brief: "b"})
	r, err := New(rc, f.options())
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.Empty(t, r.AutoMergeUpdates())
	key := AutoMergeUpdateKey("fx-1", "1111111111111111111111111111111111111111")
	require.NoError(t, r.RecordAutoMergeUpdate(key))
	require.NoError(t, r.RecordAutoMergeUpdate(key))

	st, err := LoadState(rc.Run.StateDir)
	require.NoError(t, err)
	require.NotNil(t, st.AutoMerge)
	require.Equal(t, []string{key}, st.AutoMerge.Updated, "durable at once, and recorded once")

	require.NoError(t, r.Stop("crash"))
	_, err = r.Wait()
	require.NoError(t, err)

	resumed := f.runConfig(t, config.RunSettings{Resume: true})
	resumed.Resume = true
	r2, err := New(resumed, f.options())
	require.NoError(t, err)
	require.Equal(t, []string{key}, r2.AutoMergeUpdates(), "the resumed run still knows what it asked for")
	for i := range autoMergeUpdatesPerBead + 5 {
		require.NoError(t, r2.RecordAutoMergeUpdate(AutoMergeUpdateKey("fx-1", fmt.Sprint(i))))
	}
	keys := r2.AutoMergeUpdates()
	require.Len(t, keys, autoMergeUpdatesPerBead, "bounded per bead")
	require.Equal(t, AutoMergeUpdateKey("fx-1", "5"), keys[0], "the bead's oldest requests went first")
	require.NoError(t, r2.Close())
}

// The update record is not a run-wide FIFO: with many pull requests queued
// every merge makes each of the others stale and asks for an update of
// each, and a pull request still waiting must keep the request its update
// merge rests on, or the approved-tip guard would send it to needs_human.
func TestLifecycleAutoMergeUpdateRecordKeepsAWaitingPullRequest(t *testing.T) {
	f := lifecycleSetup(t)
	rc := f.runConfig(t, config.RunSettings{Brief: "b"})
	r, err := New(rc, f.options())
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	defer func() { _ = r.Stop("over"); _, _ = r.Wait() }()

	require.NoError(t, r.RecordAutoMergeUpdate(AutoMergeUpdateKey(amBead, amHead)))
	for i := range 2 * autoMergeSentBackLimit {
		require.NoError(t, r.RecordAutoMergeUpdate(AutoMergeUpdateKey(domain.BeadID(fmt.Sprintf("fx-o%d", i%20)), fmt.Sprintf("%040d", i))))
	}

	const pushed = "5555555555555555555555555555555555555555"
	h := newAMHarness(t, nil)
	h.Ledger.updated = r.AutoMergeUpdates() // what state.json now holds
	h.Branches.markUpdate(pushed, amHead)   // GitHub's update the run asked for
	h.GH.set(amOwnedView(pushed))
	h.pending()
	h.poll()
	assert.Equal(t, string(domain.BeadStateDelivering), h.BD.State(amBead), h.status().Reason)
	assert.Equal(t, []string{"7@" + pushed}, h.GH.Merges)
}

func TestPruneAutoMergeUpdatesDropsTheLeastRecentlyUpdatedBead(t *testing.T) {
	keys := []string{"a@1", "b@1", "a@2", "c@1", "b@2", "a@3"}
	assert.Equal(t, []string{"b@1", "a@2", "c@1", "b@2", "a@3"}, pruneAutoMergeUpdates(keys, 2, 10), "a keeps its newest two")
	assert.Equal(t, []string{"a@1", "b@1", "a@2", "b@2", "a@3"}, pruneAutoMergeUpdates(keys, 5, 5),
		"c's newest request is the oldest of any bead's, so c goes, whole")
	assert.Equal(t, []string{"a@1", "a@2", "a@3"}, pruneAutoMergeUpdates(keys, 5, 3), "then b")
	assert.Equal(t, keys, pruneAutoMergeUpdates(keys, 5, 10))
}

func TestLifecycleRestoreWithoutAStateFile(t *testing.T) {
	f := lifecycleSetup(t)
	dir := config.RunDir(f.Repo, "20260903-110000-0001")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	resumed := f.runConfig(t, config.RunSettings{Resume: true})

	res, err := RestoreRunConfig(resumed)
	require.NoError(t, err)
	require.False(t, res.Restored)
	require.Contains(t, strings.Join(res.Notes, "\n"), "has no state.json")
	require.Equal(t, 10, resumed.Run.Config.Ammo)

	_, err = RestoreRunConfig(nil)
	require.ErrorContains(t, err, "nil run config")
}

func TestLifecycleRestoreRefusesAForeignStateFile(t *testing.T) {
	f := lifecycleSetup(t)
	rc := f.runConfig(t, config.RunSettings{})
	require.NoError(t, os.MkdirAll(rc.Run.StateDir, 0o700))
	st := State{Version: StateFileVersion, Run: domain.Run{ID: "somebody-else"}}
	require.NoError(t, WriteState(rc.Run.StateDir, st))

	_, err := RestoreRunConfig(rc)
	require.ErrorContains(t, err, "belongs to run somebody-else")
}

func TestLifecycleRestoreRefusesAMissingCartridge(t *testing.T) {
	f := lifecycleSetup(t)
	r, err := New(f.runConfig(t, config.RunSettings{}), f.options())
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.Stop("crash"))
	_, err = r.Wait()
	require.NoError(t, err)

	f.Eff = lifecycleEffective(10, true)
	f.Eff.ActiveCartridge = "other"
	f.Eff.Cartridges = map[string]config.Cartridge{"other": {}}
	resumed := f.runConfig(t, config.RunSettings{Resume: true})
	_, err = RestoreRunConfig(resumed)
	require.ErrorContains(t, err, `was started with cartridge "claude", which is no longer configured`)
}

func TestLifecycleRestoreReinstatesTheRecordedCartridge(t *testing.T) {
	f := lifecycleSetup(t)
	r, err := New(f.runConfig(t, config.RunSettings{}), f.options())
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.Stop("crash"))
	_, err = r.Wait()
	require.NoError(t, err)

	f.Eff = lifecycleEffective(10, true)
	f.Eff.Cartridges["other"] = config.Cartridge{Model: "m", Args: []string{"-x"}, Env: map[string]string{"K": "V"}}
	f.Eff.ActiveCartridge = "other"
	resumed := f.runConfig(t, config.RunSettings{Resume: true})
	require.Equal(t, "other", resumed.CartridgeName)

	res, err := RestoreRunConfig(resumed)
	require.NoError(t, err)
	require.Equal(t, "claude", resumed.CartridgeName)
	require.Equal(t, "opus", resumed.Cartridge.Model)
	require.Contains(t, strings.Join(res.Notes, "\n"), "restored cartridge claude")
}

// A run frozen on Claude keeps Claude as its active cartridge on resume, but
// the per-role cartridges added to the configuration since (D-27) are not
// part of the snapshot, so they take effect: engineers and testers move to
// Cursor while the product owner stays where the run started.
func TestLifecycleRestoreKeepsNewRoleCartridges(t *testing.T) {
	f := lifecycleSetup(t)
	r, err := New(f.runConfig(t, config.RunSettings{}), f.options())
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.Stop("crash"))
	_, err = r.Wait()
	require.NoError(t, err)

	f.Eff = lifecycleEffective(10, true)
	f.Eff.Cartridges["cursor"] = config.Cartridge{Model: "auto"}
	f.Eff.Roles[domain.RoleEngineer] = "cursor"
	f.Eff.Roles[domain.RoleTester] = "cursor"
	resumed := f.runConfig(t, config.RunSettings{Resume: true})

	_, err = RestoreRunConfig(resumed)
	require.NoError(t, err)
	require.Equal(t, "claude", resumed.CartridgeName)
	require.Equal(t, "opus", resumed.Cartridge.Model, "the frozen model stays Claude's")
	require.Equal(t, "cursor", resumed.Effective.Roles[domain.RoleEngineer])
	require.Equal(t, "cursor", resumed.Effective.Roles[domain.RoleTester])
	require.Equal(t, "auto", resumed.Effective.Cartridges["cursor"].Model)
}

// ------------------------------------------------------------- state on disk

func TestLifecycleStateFileRoundTripsAndRefusesTheFuture(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadState(dir)
	require.ErrorIs(t, err, ErrNoState)

	st := State{
		Version: StateFileVersion,
		Run:     domain.Run{ID: "r1", RepoPath: "/repo", Mode: domain.RunModeAutopilot, Config: domain.ConfigSnapshot{Ammo: 10, Vibes: []string{"v"}}},
		Phase:   PhaseRunning,
	}
	require.NoError(t, WriteState(dir, st))
	info, err := os.Stat(StatePath(dir))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	got, err := LoadState(dir)
	require.NoError(t, err)
	require.Equal(t, st, got)

	// A copy that no longer aliases the original's slices.
	clone := got.clone()
	clone.Run.Config.Vibes[0] = "changed"
	require.Equal(t, "v", got.Run.Config.Vibes[0])

	st.Version = StateFileVersion + 1
	require.NoError(t, WriteState(dir, st))
	_, err = LoadState(dir)
	require.ErrorContains(t, err, "written by a newer sc")

	require.NoError(t, os.WriteFile(StatePath(dir), []byte("{not json"), 0o600))
	_, err = LoadState(dir)
	require.ErrorContains(t, err, "parse")
}

func TestLifecycleStateFileGolden(t *testing.T) {
	f := lifecycleSetup(t)
	rc := f.runConfig(t, config.RunSettings{Brief: "add a --version flag", Headless: true})
	opts := f.options()
	opts.Loop = LoopFunc(func(_ context.Context, run *Run) (Outcome, error) {
		require.NoError(t, run.SetEpic("sc-epic"))
		require.NoError(t, run.AgentStarted(domain.Agent{
			ID: "eng-sc-1", Role: domain.RoleEngineer, Cartridge: "claude", Model: "opus",
			Bead: "sc-1", State: domain.AgentStateRunning, StartedAt: lifecycleEpoch, PID: 4242,
		}))
		f.Clock.Advance(90 * time.Second)
		require.NoError(t, run.AgentStopped("eng-sc-1"))
		return Outcome{Reason: events.FinishCompleted, Summary: "1 bead landed"}, nil
	})
	r, err := New(rc, opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	_, err = r.Wait()
	require.NoError(t, err)

	data, err := os.ReadFile(StatePath(r.Dir()))
	require.NoError(t, err)
	got := strings.ReplaceAll(string(data), rc.Run.StateDir, "<run-dir>")
	got = strings.ReplaceAll(got, f.Repo, "<repo>")
	lifecycleGolden(t, "state.golden.json", got)
}

func TestLifecycleDirsFor(t *testing.T) {
	d := DirsFor("/state/runs/r1/")
	require.Equal(t, "/state/runs/r1", d.Dir)
	require.Equal(t, "/state/runs/r1/agents", d.Agents)
	require.Equal(t, "/state/runs/r1/wt", d.Worktrees)
	require.Equal(t, "/state/runs/r1/bin", d.Shims)
	require.Equal(t, "/state/runs/r1/state.json", StatePath("/state/runs/r1"))
}

// ------------------------------------------------------------------ the lock

func TestLifecycleLockLivesInTheStateDirNotTheRepo(t *testing.T) {
	f := lifecycleSetup(t)
	rc := f.runConfig(t, config.RunSettings{})
	require.Equal(t, filepath.Join(config.StateDir(f.Repo), LockFileName), LockPath(rc.Run.StateDir))

	r, err := New(rc, f.options())
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.FileExists(t, LockPath(rc.Run.StateDir))
	require.NoFileExists(t, filepath.Join(f.Repo, ".slop-cannon", "run.lock"))

	require.NoError(t, r.Stop("done"))
	_, err = r.Wait()
	require.NoError(t, err)
	require.NoFileExists(t, LockPath(rc.Run.StateDir), "the lock is released when the run finishes")
}

// lifecycleLivePolicy never treats a lock as stale, standing in for P9-04's
// view of another sc that is genuinely alive.
type lifecycleLivePolicy struct{}

func (lifecycleLivePolicy) Stale(LockHolder) bool { return false }

func TestLifecycleLockRefusesASecondLiveRun(t *testing.T) {
	f := lifecycleSetup(t)
	opts := f.options()
	opts.LockPolicy = lifecycleLivePolicy{}

	first, err := New(f.runConfig(t, config.RunSettings{ID: "20260903-120000-0001"}), opts)
	require.NoError(t, err)
	require.NoError(t, first.Start(context.Background()))

	second, err := New(f.runConfig(t, config.RunSettings{ID: "20260903-120001-0002"}), opts)
	require.NoError(t, err)
	err = second.Start(context.Background())
	require.ErrorIs(t, err, ErrLocked)
	require.ErrorContains(t, err, "20260903-120000-0001")
	out, waitErr := second.Wait()
	require.ErrorIs(t, waitErr, ErrLocked)
	require.Equal(t, events.FinishFailed, out.Reason)

	require.NoError(t, first.Stop("done"))
	_, err = first.Wait()
	require.NoError(t, err)
}

func TestLifecycleLockTakesOverAStaleOne(t *testing.T) {
	f := lifecycleSetup(t)
	rc := f.runConfig(t, config.RunSettings{})
	path := LockPath(rc.Run.StateDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	stale, err := json.Marshal(LockHolder{Run: "old", PID: 1 << 30, TakenAt: lifecycleEpoch})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, stale, 0o600))

	r, err := New(rc, f.options())
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()), "a lock whose process is gone is taken over")
	holder, err := lifecycleReadLock(path)
	require.NoError(t, err)
	require.Equal(t, rc.Run.ID, holder.Run)
	require.Equal(t, os.Getpid(), holder.PID)

	require.NoError(t, r.Stop("done"))
	_, err = r.Wait()
	require.NoError(t, err)
}

func TestLifecycleDefaultLockPolicy(t *testing.T) {
	p := DefaultLockPolicy{}
	require.True(t, p.Stale(LockHolder{PID: 0}))
	require.True(t, p.Stale(LockHolder{PID: os.Getpid()}), "our own lock, resumed in-process")
	require.True(t, p.Stale(LockHolder{PID: 1 << 30}), "no such process")
	require.False(t, p.Stale(LockHolder{PID: os.Getppid()}))
}

func TestLifecycleSkipLockLeavesTheLockAlone(t *testing.T) {
	f := lifecycleSetup(t)
	opts := f.options()
	opts.SkipLock = true
	rc := f.runConfig(t, config.RunSettings{})
	r, err := New(rc, opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoFileExists(t, LockPath(rc.Run.StateDir))
	require.NoError(t, r.Close())
	require.NoError(t, r.Stop("done"))
	_, err = r.Wait()
	require.NoError(t, err)
}

func TestLifecycleStartFailsWhenTheRunDirCannotBeCreated(t *testing.T) {
	f := lifecycleSetup(t)
	rc := f.runConfig(t, config.RunSettings{})
	// A file where the run directory should be.
	require.NoError(t, os.MkdirAll(filepath.Dir(rc.Run.StateDir), 0o700))
	require.NoError(t, os.WriteFile(rc.Run.StateDir, []byte("x"), 0o600))

	r, err := New(rc, f.options())
	require.NoError(t, err)
	require.Error(t, r.Start(context.Background()))
	out, waitErr := r.Wait()
	require.Error(t, waitErr)
	require.Equal(t, events.FinishFailed, out.Reason)
}

// -------------------------------------------------------------------- helpers

// lifecycleRead loads the run's state file.
func lifecycleRead(t *testing.T, r *Run) State {
	t.Helper()
	st, err := LoadState(r.Dir())
	require.NoError(t, err)
	return st
}

// lifecycleWarnings joins the messages of every Warning published so far.
func lifecycleWarnings(f *lifecycleFixture) string {
	var msgs []string
	for _, ev := range f.drain() {
		if w, ok := ev.(events.Warning); ok {
			msgs = append(msgs, w.Message)
		}
	}
	return strings.Join(msgs, "\n")
}

// lifecycleReadLock reads a run lock file.
func lifecycleReadLock(path string) (LockHolder, error) { return readLock(path) }

// lifecycleGoldenDir is where this package's goldens live.
const lifecycleGoldenDir = "../../testdata/orchestrator"

// lifecycleGolden compares got with testdata/orchestrator/<name>. Set
// UPDATE_GOLDEN=1 (make update-golden) to rewrite the file instead.
func lifecycleGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(lifecycleGoldenDir, name)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden %s; run `make update-golden`", path)
	require.Equal(t, string(want), got, "state.json differs from %s; run `make update-golden` if the change is intended", path)
}

// ---------------------------------------------------------------- accessors

func TestLifecyclePhaseHelpers(t *testing.T) {
	require.Equal(t, "running", PhaseRunning.String())
	for _, p := range []Phase{PhaseRunning, PhasePaused, PhaseStopping} {
		require.True(t, p.Live(), p)
	}
	for _, p := range []Phase{PhaseNew, PhaseFinished, Phase("nonsense")} {
		require.False(t, p.Live(), p)
	}
}

func TestLifecycleAccessors(t *testing.T) {
	f := lifecycleSetup(t)
	rc := f.runConfig(t, config.RunSettings{Brief: "b"})
	r, err := New(rc, f.options())
	require.NoError(t, err)

	require.Equal(t, rc.Run.ID, r.ID())
	require.Equal(t, f.Repo, r.RepoPath())
	require.Same(t, rc, r.RunConfig())
	require.Equal(t, rc.Allocation, r.Allocation())
	require.Equal(t, lifecycleEpoch, r.Now())
	require.Equal(t, lifecycleEpoch, r.Meta().Time)
	require.Equal(t, rc.Run.ID, r.Meta().Run)
	require.NotNil(t, r.Log())
	require.Equal(t, 10, r.Snapshot().Ammo)
	require.Empty(t, r.Epic())
	require.Nil(t, r.ResumeResult())

	select {
	case <-r.Done():
		t.Fatal("a run that never started is not done")
	default:
	}
	r.Publish(nil) // ignored
	require.Zero(t, r.State().LastSeq)
}

func TestLifecycleAgentAndLeaseFailuresAreWarnings(t *testing.T) {
	f := lifecycleSetup(t)
	opts := f.options()
	opts.Agents = &lifecycleStopper{err: fmt.Errorf("pid 1 will not die")}
	opts.Leases = &lifecycleLeases{err: fmt.Errorf("bd is busy")}
	opts.Cleaner = &lifecycleCleaner{res: &gitx.CleanupResult{}}
	// No Beads source at all: the cleanup still runs, with an empty list.
	r, err := New(f.runConfig(t, config.RunSettings{}), opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.AgentStarted(domain.Agent{ID: "eng-1", Role: domain.RoleEngineer,
		Bead: "sc-1", State: domain.AgentStateRunning}))
	require.NoError(t, r.Stop("user"))

	out, err := r.Wait()
	require.NoError(t, err, "a messy shutdown is still a clean stop")
	require.Equal(t, events.FinishStopped, out.Reason)
	w := lifecycleWarnings(f)
	require.Contains(t, w, "some agents could not be stopped cleanly")
	require.Contains(t, w, "could not release the lease on sc-1")
}

func TestLifecycleCleanupEntryFailuresAreWarnings(t *testing.T) {
	f := lifecycleSetup(t)
	opts := f.options()
	opts.Cleaner = &lifecycleCleaner{res: &gitx.CleanupResult{Entries: []gitx.CleanupEntry{
		{BeadID: "sc-1", Action: gitx.CleanupKept, Err: fmt.Errorf("permission denied")},
	}}}
	opts.Beads = func(context.Context) ([]domain.Bead, error) { return nil, fmt.Errorf("bd is down") }
	r, err := New(f.runConfig(t, config.RunSettings{}), opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.Stop("user"))
	_, err = r.Wait()
	require.NoError(t, err)
	require.Contains(t, lifecycleWarnings(f), "could not list the run's beads for the worktree cleanup")
}

// --- the forwarding bus (P6-27) ------------------------------------------

// TestLifecycleAgentBusCountsEveryEventItForwards is P6-27: the agents
// layer can only publish to a concrete *events.Bus, so the run offers one
// that republishes through Run.Publish. Without it the AgentOutput of every
// session went straight past the run and state.json's LastSeq undercounted
// the run log by however much the agents said.
func TestLifecycleAgentBusCountsEveryEventItForwards(t *testing.T) {
	f := lifecycleSetup(t)
	r, err := New(f.runConfig(t, config.RunSettings{}), f.options())
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))

	before := lifecycleRead(t, r).LastSeq
	require.Same(t, r.AgentBus(), r.AgentBus(), "the run has exactly one forwarding bus")
	for i := 0; i < 5; i++ {
		r.AgentBus().Publish(events.AgentOutput{Meta: r.Meta(), Agent: "eng-sc-1-r1",
			Stream: events.StreamStdout, Line: "working"})
	}
	// A nil event is ignored rather than counted.
	r.AgentBus().Publish(nil)

	require.NoError(t, r.Stop("user"))
	_, err = r.Wait()
	require.NoError(t, err)

	st := lifecycleRead(t, r)
	// state.json is stamped at each transition rather than at each event,
	// so the file said zero while RunStarted had already been counted: the
	// run's whole sequence is RunStarted, the five output lines, RunStopped
	// and RunFinished.
	require.Equal(t, before+8, st.LastSeq, "every forwarded event is counted")

	output := 0
	for _, ev := range f.drain() {
		if ev.Kind() == events.KindAgentOutput {
			output++
		}
	}
	require.Equal(t, 5, output, "and reaches the live bus through the run")
}

// The forwarding bus is drained before the run finishes, so an event
// published just before the end is in the file the final state.json counts.
func TestLifecycleAgentBusIsDrainedBeforeTheRunFinishes(t *testing.T) {
	f := lifecycleSetup(t)
	opts := f.options()
	opts.Loop = LoopFunc(func(_ context.Context, r *Run) (Outcome, error) {
		for i := 0; i < 50; i++ {
			r.AgentBus().Publish(events.AgentOutput{Meta: r.Meta(), Agent: "eng-1", Line: "line"})
		}
		return Outcome{Summary: "done"}, nil
	})
	r, err := New(f.runConfig(t, config.RunSettings{}), opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	_, err = r.Wait()
	require.NoError(t, err)

	st := lifecycleRead(t, r)
	require.Equal(t, uint64(52), st.LastSeq, "RunStarted, fifty output lines and RunFinished")
}

// Closing an abandoned run releases the forwarder too.
func TestLifecycleCloseStopsTheForwardingBus(t *testing.T) {
	f := lifecycleSetup(t)
	r, err := New(f.runConfig(t, config.RunSettings{}), f.options())
	require.NoError(t, err)
	bus := r.AgentBus()
	bus.Publish(events.AgentOutput{Meta: r.Meta(), Agent: "eng-1", Line: "line"})
	require.NoError(t, r.Close())
	require.NoError(t, r.Close(), "closing twice is fine")
	bus.Publish(events.AgentOutput{Meta: r.Meta(), Agent: "eng-1", Line: "after the close"})
}

// --- the bead source (P8-17) ---------------------------------------------

func TestLifecycleUseBeadsRefusesAStartedRun(t *testing.T) {
	f := lifecycleSetup(t)
	src := BeadSource(func(context.Context) ([]domain.Bead, error) { return nil, nil })
	r, err := New(f.runConfig(t, config.RunSettings{}), f.options())
	require.NoError(t, err)
	require.Nil(t, r.BeadSource())
	require.NoError(t, r.UseBeads(src))
	require.NotNil(t, r.BeadSource())

	require.NoError(t, r.Start(context.Background()))
	require.ErrorIs(t, r.UseBeads(src), ErrAlreadyStarted)
	require.NoError(t, r.Stop("user"))
	_, err = r.Wait()
	require.NoError(t, err)
	require.ErrorIs(t, r.UseBeads(src), ErrFinished)
}

// --- the run log wiring (P8-20) ------------------------------------------

func TestLifecycleTeeRunLogPutsTheLogInFrontOfTheBus(t *testing.T) {
	f := lifecycleSetup(t)
	rc := f.runConfig(t, config.RunSettings{})
	opts := f.options()
	log, err := TeeRunLog(&opts, rc.Run.StateDir, f.Bus)
	require.NoError(t, err)
	require.Equal(t, EventLogPath(rc.Run.StateDir), log.Path())

	r, err := New(rc, opts)
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background()))
	require.NoError(t, r.Stop("user"))
	_, err = r.Wait()
	require.NoError(t, err)
	require.NoError(t, log.Close())

	s, err := SummariseLog(rc.Run.StateDir, 0)
	require.NoError(t, err, "a real run leaves a log sc status can fold")
	require.Equal(t, PhaseFinished, s.Phase)
	require.Equal(t, events.FinishStopped, s.Finish)
	require.NotEmpty(t, f.drain(), "and the live bus still sees everything")

	_, err = TeeRunLog(nil, rc.Run.StateDir, f.Bus)
	require.Error(t, err)
}
