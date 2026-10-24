package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
)

// Tests for production signals (P6-71, D-32): the runner, the planner's
// cache and interval, autopilot only, and the rules for a plan a changed
// reading asks for. Helpers are prefixed "signals".

// --- the runner -------------------------------------------------------------

func TestRunSignalsKeepsStandardOutputAndRunsInTheRepository(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SC_SIGNALS_TEST_TOKEN", "from-the-users-environment")
	read := RunSignals(context.Background(), SignalsOptions{
		Command: `pwd; echo "$SC_SIGNALS_TEST_TOKEN"; echo progress >&2`,
		Dir:     dir, Timeout: time.Minute,
		Now: func() time.Time { return planningEpoch },
	})
	require.False(t, read.Failed(), read.Err)
	assert.Equal(t, 0, read.ExitCode)
	assert.Equal(t, planningEpoch, read.RanAt)
	assert.Contains(t, read.Output, dir+"\n", "it runs in the repository root")
	assert.Contains(t, read.Output, "from-the-users-environment\n", "with the user's environment, like check_command")
	assert.NotContains(t, read.Output, "progress", "standard error is not evidence")
	assert.False(t, read.Truncated)
}

func TestRunSignalsTimesOutWithoutFailing(t *testing.T) {
	start := time.Now()
	read := RunSignals(context.Background(), SignalsOptions{
		Command: "echo started; sleep 30", Dir: t.TempDir(), Timeout: 200 * time.Millisecond,
	})
	assert.Less(t, time.Since(start), 20*time.Second, "the timeout kills the command and what it started")
	require.True(t, read.Failed())
	assert.Equal(t, -1, read.ExitCode)
	assert.Equal(t, "timed out after 200ms", read.Err)
}

func TestRunSignalsReportsANonZeroExitWithTheLastErrorLine(t *testing.T) {
	read := RunSignals(context.Background(), SignalsOptions{
		Command: "echo some evidence; echo first >&2; echo 'sentry: 401 token ghp_ABCDEFGHIJKLMNOPQRST' >&2; exit 3",
		Dir:     t.TempDir(), Timeout: time.Minute,
	})
	require.True(t, read.Failed())
	assert.Equal(t, 3, read.ExitCode)
	assert.Equal(t, "exit status 3: sentry: 401 token ghp_[REDACTED]", read.Err,
		"the failure is one line, with the secret masked")
}

func TestRunSignalsScrubsAndFiltersTheOutput(t *testing.T) {
	f, err := agents.NewSecretFilter()
	require.NoError(t, err)
	read := RunSignals(context.Background(), SignalsOptions{
		Command: `printf '\033[31mERROR\033[0m boom\r\n\tat main.go:3\nprogress 10%%\rprogress 100%%\nAPI_TOKEN=hunter2hunter2 sk-abcdefghijklmnop\a\n'`,
		Dir:     t.TempDir(), Timeout: time.Minute, Filter: f,
	})
	require.False(t, read.Failed(), read.Err)
	assert.Equal(t, "ERROR boom\n\tat main.go:3\nprogress 100%\nAPI_TOKEN=[REDACTED] sk-[REDACTED]\n", read.Output)
}

func TestRunSignalsKeepsTheLast32KiBCutAtALineBoundary(t *testing.T) {
	read := RunSignals(context.Background(), SignalsOptions{
		Command: `i=0; while [ $i -lt 4000 ]; do printf 'line %05d %s\n' $i xxxxxxxxxxxxxxxxxxxx; i=$((i+1)); done`,
		Dir:     t.TempDir(), Timeout: time.Minute,
	})
	require.False(t, read.Failed(), read.Err)
	assert.True(t, read.Truncated)
	assert.LessOrEqual(t, len(read.Output), agents.MaxSignalsBytes)
	assert.Greater(t, len(read.Output), agents.MaxSignalsBytes-64, "only the partial first line is dropped")
	assert.True(t, strings.HasSuffix(read.Output, "line 03999 xxxxxxxxxxxxxxxxxxxx\n"), "the newest lines are kept")
	for _, l := range strings.Split(strings.TrimSuffix(read.Output, "\n"), "\n") {
		require.Regexp(t, `^line \d{5} x{20}$`, l, "every kept line is whole")
	}
}

func TestRunSignalsHoldsATailOfAHugeOutput(t *testing.T) {
	// About 12 MiB: what `cat` of a production log prints.
	read := RunSignals(context.Background(), SignalsOptions{
		Command: `yes 'GET /checkout 500 upstream timed out' | head -n 300000; echo last line`,
		Dir:     t.TempDir(), Timeout: time.Minute,
	})
	require.False(t, read.Failed(), read.Err)
	assert.True(t, read.Truncated)
	assert.LessOrEqual(t, len(read.Output), agents.MaxSignalsBytes)
	assert.True(t, strings.HasSuffix(read.Output, "upstream timed out\nlast line\n"))
}

func TestSignalsSpecHoldsOnlyATailOfEachStream(t *testing.T) {
	t.Parallel()
	spec := signalsSpec("cat app.log", "/repo", time.Minute)
	assert.Equal(t, []string{"-c", "cat app.log"}, spec.Args)
	assert.True(t, spec.ReapDescendants)
	assert.Equal(t, 4*agents.MaxSignalsBytes+1, spec.StdoutTail, "a whole log is never held in memory")
	assert.Equal(t, signalsStderrMax, spec.StderrTail)
}

func TestRunSignalsRecognisesARepeatedFailureByItsKind(t *testing.T) {
	run := func(command string) SignalsReading {
		return RunSignals(context.Background(), SignalsOptions{Command: command, Dir: t.TempDir(), Timeout: time.Minute})
	}
	a, b := run("echo 'request r-1: 503' >&2; exit 2"), run("echo 'request r-2: 503' >&2; exit 2")
	assert.Equal(t, "exit status 2: request r-1: 503", a.Err)
	assert.NotEqual(t, a.Err, b.Err)
	assert.Equal(t, a.failureKey(), b.failureKey(), "a request id on the error line is the same failure")
	assert.NotEqual(t, a.failureKey(), run("exit 3").failureKey(), "a different exit status is not")
}

func TestSignalsTail(t *testing.T) {
	t.Parallel()
	got, cut := signalsTail("abc\ndef\n", 100)
	assert.Equal(t, "abc\ndef\n", got)
	assert.False(t, cut)

	got, cut = signalsTail("abc\ndef\nghi\n", 6)
	assert.Equal(t, "ghi\n", got, "the partial first line goes")
	assert.True(t, cut)

	got, _ = signalsTail("abc\ndef\n", 4)
	assert.Equal(t, "def\n", got, "a window that starts on a line keeps it")

	got, _ = signalsTail("ééééé", 3)
	assert.Equal(t, "é", got, "one long line keeps its tail from a whole rune")
}

// --- the planner ------------------------------------------------------------

// signalsFake is a scripted signals command: what it prints next, and how
// often it has run.
type signalsFake struct {
	mu    sync.Mutex
	out   string
	err   string
	class string
	calls int
	now   func() time.Time
}

func (s *signalsFake) run(context.Context) SignalsReading {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	r := SignalsReading{Command: "signals", RanAt: s.now(), Output: s.out, Err: s.err, class: s.class}
	if s.err != "" {
		r.ExitCode = 1
	}
	return r
}

func (s *signalsFake) set(out, err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out, s.err = out, err
}

// fail makes the next readings fail with err, recognised as class.
func (s *signalsFake) fail(err, class string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out, s.err, s.class = "", err, class
}

func (s *signalsFake) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// signalsFixture is a planner with a fake clock, a fake signals command, a
// fake bead store and a scheduler status the test sets.
type signalsFixture struct {
	t      *testing.T
	p      *Planner
	clock  *planningClock
	sig    *signalsFake
	runner *planningRunner
	beads  *planningBeadStore
	mu     sync.Mutex
	status SchedulerStatus
	logs   *bytes.Buffer
	logMu  *sync.Mutex
}

const signalsInterval = 30 * time.Minute

func signalsSetup(t *testing.T, brief string) *signalsFixture {
	t.Helper()
	rc := planningRunConfig(t, brief)
	f := &signalsFixture{
		t: t, clock: newPlanningClock(), runner: &planningRunner{plan: planningPlan("alpha")},
		beads: &planningBeadStore{}, logs: &bytes.Buffer{}, logMu: &sync.Mutex{},
	}
	f.sig = &signalsFake{out: "ERROR E-1 boom\n", now: f.clock.Now}
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: f.runner, Applier: &planningApplier{}, Beads: f.beads.source(),
		Now: f.clock.Now, NoShim: true, Signals: f.sig.run, SignalsInterval: signalsInterval,
		Status: func() SchedulerStatus {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.status
		},
		Logger: slog.New(slog.NewTextHandler(signalsSyncWriter{f.logs, f.logMu}, nil)),
	})
	require.NoError(t, err)
	f.p = p
	return f
}

type signalsSyncWriter struct {
	b  *bytes.Buffer
	mu *sync.Mutex
}

func (w signalsSyncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (f *signalsFixture) setStatus(st SchedulerStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = st
}

func (f *signalsFixture) log() string {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	return f.logs.String()
}

// fire runs one planning firing and returns the request the product owner
// was given.
func (f *signalsFixture) fire() agents.PlanRequest {
	f.t.Helper()
	_, err := f.p.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
	require.NoError(f.t, err)
	req, ok := f.runner.last()
	require.True(f.t, ok)
	return req
}

// demands asks the planner once and waits for any background reading it
// started, so the next call sees it.
func (f *signalsFixture) demands() []Demand {
	f.t.Helper()
	ds, err := f.p.Demands(context.Background())
	require.NoError(f.t, err)
	f.settle()
	return ds
}

// settle waits for the reading in flight, if there is one.
func (f *signalsFixture) settle() {
	f.t.Helper()
	f.p.sigMu.Lock()
	done := f.p.sig.done
	f.p.sigMu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(schedWait(schedPollBudget)):
		f.t.Fatal("the background signals reading never finished")
	}
}

// busy is a scheduler status with an engineer at work and the P8-28
// watermark behind it (the scheduler has fired more since the last plan).
func (f *signalsFixture) busy() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = SchedulerStatus{Live: 1, Fired: f.status.Fired + 1, ByRole: map[domain.Role]int{domain.RoleEngineer: 1}}
}

func TestPlannerGivesTheProductOwnerTheCachedSignalsUntilTheIntervalPasses(t *testing.T) {
	f := signalsSetup(t, "")
	req := f.fire()
	require.NotNil(t, req.Signals)
	assert.Equal(t, "ERROR E-1 boom\n", req.Signals.Output)
	assert.Equal(t, planningEpoch, req.Signals.RanAt)
	assert.Equal(t, 1, f.sig.count())
	for _, n := range req.Notes {
		assert.NotContains(t, n, "E-1", "signals never go into the unfenced notes")
	}
	assert.NoError(t, req.Validate())

	f.sig.set("ERROR E-2 new\n", "")
	f.clock.Advance(signalsInterval - time.Second)
	req = f.fire()
	assert.Equal(t, 1, f.sig.count(), "within the interval the cached reading is reused")
	assert.Equal(t, "ERROR E-1 boom\n", req.Signals.Output)

	f.clock.Advance(time.Second)
	req = f.fire()
	assert.Equal(t, 2, f.sig.count(), "once the interval has passed the command runs again")
	assert.Equal(t, "ERROR E-2 new\n", req.Signals.Output)

	st := f.p.Status().Signals
	require.NotNil(t, st)
	assert.Equal(t, f.clock.Now(), st.LastAt)
	assert.Equal(t, len("ERROR E-2 new\n"), st.Bytes)
	assert.Equal(t, f.clock.Now().Add(signalsInterval), st.NextAt)
}

func TestPlannerTakesNoSignalsInBriefMode(t *testing.T) {
	f := signalsSetup(t, "make the tests pass")
	req := f.fire()
	assert.Nil(t, req.Signals, "a brief is the whole scope")
	f.busy()
	f.clock.Advance(2 * signalsInterval)
	assert.Empty(t, f.demands())
	assert.Equal(t, 0, f.sig.count(), "brief mode never runs the command")
	assert.Nil(t, f.p.Status().Signals)
}

func TestPlannerWithoutASignalsCommandTakesNone(t *testing.T) {
	rc := planningRunConfig(t, "")
	p, err := NewPlanner(PlannerOptions{Config: rc, Runner: &planningRunner{}, NoShim: true})
	require.NoError(t, err)
	assert.False(t, p.signalsOn(), "signals_command is empty by default")
	assert.Nil(t, p.Status().Signals)
}

func TestPlannerWarnsOncePerDistinctSignalsFailureAndPlansAnyway(t *testing.T) {
	f := signalsSetup(t, "")
	f.sig.set("", "exit status 1: unauthorized")
	req := f.fire()
	require.NotNil(t, req.Signals)
	assert.Equal(t, "exit status 1: unauthorized", req.Signals.Failure, "the failure reaches the prompt as a note")

	f.clock.Advance(signalsInterval)
	f.fire()
	assert.Equal(t, 2, f.sig.count())
	assert.Equal(t, 1, strings.Count(f.log(), "the signals command gave no reading"), "the same failure warns once")

	f.sig.set("", "timed out after 5m0s")
	f.clock.Advance(signalsInterval)
	f.fire()
	assert.Equal(t, 2, strings.Count(f.log(), "the signals command gave no reading"), "a different failure warns again")
	assert.Equal(t, 3, f.runner.count(), "no failure ever stopped a plan")
}

func TestPlannerWarnsOnceForAFailureWhoseErrorLineKeepsChanging(t *testing.T) {
	f := signalsSetup(t, "")
	f.sig.fail("exit status 1: request r-1", "exit status 1")
	f.fire()
	f.sig.fail("exit status 1: request r-2", "exit status 1")
	f.clock.Advance(signalsInterval)
	f.fire()
	assert.Equal(t, 2, f.sig.count())
	assert.Equal(t, 1, strings.Count(f.log(), "the signals command gave no reading"))
}

func TestPlannerDropsAReadingCutShortByItsContext(t *testing.T) {
	f := signalsSetup(t, "")
	f.fire()
	f.sig.fail("stopped with the run", "stopped with the run")
	f.clock.Advance(signalsInterval)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Nil(t, f.p.signalsForPlan(ctx))
	f.settle()
	assert.Equal(t, 2, f.sig.count())
	assert.NotContains(t, f.log(), "the signals command gave no reading", "a run stopping is no news about production")
	f.p.sigMu.Lock()
	defer f.p.sigMu.Unlock()
	require.NotNil(t, f.p.sig.last)
	assert.Equal(t, "ERROR E-1 boom\n", f.p.sig.last.Output, "the reading before it stays cached")
}

func TestPlannerMarksTheRunsStateWhenABackgroundReadingLands(t *testing.T) {
	f := signalsSetup(t, "")
	run, err := New(planningRunConfig(t, ""), Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: f.clock.Now})
	require.NoError(t, err)
	run.UseVitals(func() RunVitals { return RunVitals{} })
	run.mu.Lock()
	run.stateDirty = false
	run.mu.Unlock()
	f.p.Attach(run)

	f.p.sigMu.Lock()
	f.p.startSignalsLocked(context.Background())
	f.p.sigMu.Unlock()
	f.settle()
	run.mu.Lock()
	defer run.mu.Unlock()
	assert.True(t, run.stateDirty, "the reading reaches state.json, so sc status shows it")
}

// signalsFirstPlan fires the run's first plan (which takes the first
// reading) and puts an engineer to work on it.
func signalsFirstPlan(t *testing.T) *signalsFixture {
	t.Helper()
	f := signalsSetup(t, "")
	f.fire()
	f.beads.set(planningBead("sc-alpha", domain.BeadStateInProgress))
	f.busy()
	require.Empty(t, f.demands(), "a fresh reading the plan just saw asks for nothing")
	return f
}

func TestPlannerOffersAPlanForAChangedReadingWhileEngineersAreBusy(t *testing.T) {
	f := signalsFirstPlan(t)
	f.sig.set("ERROR E-2 checkout fails\n", "")
	f.clock.Advance(signalsInterval)

	assert.Empty(t, f.demands(), "the due reading is taken in the background, not on the loop")
	assert.Equal(t, 2, f.sig.count())
	assert.Equal(t, []Demand{planDemand()}, f.demands(), "due, changed and the planner idle: plan")
	assert.Equal(t, []Demand{planDemand()}, f.demands(), "the demand stays offered until a plan fires")
	assert.Equal(t, 2, f.sig.count(), "and does not run the command again")

	req := f.fire()
	assert.Equal(t, "ERROR E-2 checkout fails\n", req.Signals.Output)
	f.busy()
	assert.Empty(t, f.demands(), "the plan saw the reading, so it asks for nothing more")
}

func TestPlannerOffersNothingForAnUnchangedReading(t *testing.T) {
	f := signalsFirstPlan(t)
	f.clock.Advance(signalsInterval)
	assert.Empty(t, f.demands())
	assert.Equal(t, 2, f.sig.count(), "the reading was taken")
	assert.Empty(t, f.demands(), "but it is what the last plan saw")
}

func TestPlannerOffersNothingWhenTheReadingChangesToEmpty(t *testing.T) {
	f := signalsFirstPlan(t)
	f.sig.set("", "")
	f.clock.Advance(signalsInterval)
	f.demands()
	assert.Equal(t, 2, f.sig.count())
	assert.Empty(t, f.demands(), "production went quiet: nothing for a plan of its own to do")
}

func TestPlannerTakesTheFirstReadingOfAResumedRunAsTheBaseline(t *testing.T) {
	f := signalsSetup(t, "")
	// What adoptResumedPlan leaves: a plan counted as made, none of whose
	// firings this process ran, so no plan has been given a reading.
	f.p.mu.Lock()
	f.p.attempts, f.p.plans, f.p.resumedPlan = 1, 1, true
	f.p.mu.Unlock()
	f.beads.set(planningBead("sc-alpha", domain.BeadStateInProgress))
	f.busy()

	f.demands()
	assert.Equal(t, 1, f.sig.count())
	assert.Empty(t, f.demands(), "the same output as before the restart is no change")

	f.sig.set("ERROR E-2\n", "")
	f.clock.Advance(signalsInterval)
	f.demands()
	assert.Equal(t, []Demand{planDemand()}, f.demands(), "a change after the baseline still plans")
}

func TestPlannerHoldsSignalsDuringTheIdleHoldAfterAnEmptyPlan(t *testing.T) {
	f := signalsFirstPlan(t)
	f.p.mu.Lock()
	f.p.nextAt = f.clock.Now().Add(2 * signalsInterval)
	f.p.mu.Unlock()
	f.sig.set("ERROR E-2\n", "")
	f.clock.Advance(signalsInterval)
	f.demands()
	assert.Empty(t, f.demands(), "idle_replan_interval's hold holds a signals plan too")
	assert.Equal(t, 1, f.sig.count(), "and no reading is taken during it")
}

func TestPlannerOffersNothingForAFailedReading(t *testing.T) {
	f := signalsFirstPlan(t)
	f.sig.set("", "exit status 2")
	f.clock.Advance(signalsInterval)
	f.demands()
	assert.Empty(t, f.demands(), "a failed reading is not evidence")
}

func TestPlannerOffersAtMostOneSignalsPlanPerInterval(t *testing.T) {
	f := signalsFirstPlan(t)
	f.sig.set("ERROR E-2\n", "")
	f.clock.Advance(signalsInterval)
	f.demands()
	require.Equal(t, []Demand{planDemand()}, f.demands())

	// The signals-driven plan fires a minute after the reading it uses.
	f.clock.Advance(time.Minute)
	f.fire()
	f.beads.set(planningBead("sc-alpha", domain.BeadStateInProgress), planningBead("sc-alpha-2", domain.BeadStateInProgress))
	f.busy()

	// The next reading is due before a whole interval has passed since that
	// plan fired.
	f.sig.set("ERROR E-3\n", "")
	f.clock.Advance(signalsInterval - time.Minute)
	f.demands()
	assert.Equal(t, 3, f.sig.count())
	assert.Empty(t, f.demands(), "changed, but a signals plan fired within the interval")

	f.clock.Advance(time.Minute)
	assert.Equal(t, []Demand{planDemand()}, f.demands(), "once the interval since that plan has passed")
}

func TestPlannerWaitsWhileThePreviousSignalsPlanIsUntouched(t *testing.T) {
	f := signalsFirstPlan(t)
	f.sig.set("ERROR E-2\n", "")
	f.clock.Advance(signalsInterval)
	f.demands()
	require.Equal(t, []Demand{planDemand()}, f.demands())
	f.runner.setPlan(planningPlan("fix-e2"))
	f.fire()
	// The signals plan's bead is still open: nobody has picked it up.
	f.beads.set(planningBead("sc-alpha", domain.BeadStateInProgress), planningBead("sc-fix-e2", domain.BeadStateOpen))
	f.busy()

	f.sig.set("ERROR E-3\n", "")
	f.clock.Advance(signalsInterval)
	f.demands()
	assert.Empty(t, f.demands(), "every bead of the previous signals plan is still open: wait")

	f.beads.set(planningBead("sc-alpha", domain.BeadStateInProgress), planningBead("sc-fix-e2", domain.BeadStateInProgress))
	f.sig.set("ERROR E-4\n", "")
	f.clock.Advance(signalsInterval)
	f.demands()
	assert.Equal(t, []Demand{planDemand()}, f.demands(), "one of them has been picked up")
}

func TestPlannerWaitsWhenItCannotListThePreviousSignalsPlansBeads(t *testing.T) {
	f := signalsFirstPlan(t)
	f.sig.set("ERROR E-2\n", "")
	f.clock.Advance(signalsInterval)
	f.demands()
	require.Equal(t, []Demand{planDemand()}, f.demands())
	f.runner.setPlan(planningPlan("fix-e2"))
	f.fire()
	f.busy()

	f.beads.mu.Lock()
	f.beads.err = errors.New("bd is not answering")
	f.beads.mu.Unlock()
	f.sig.set("ERROR E-3\n", "")
	f.clock.Advance(signalsInterval)
	f.demands()
	assert.Empty(t, f.demands(), "when it cannot tell, it waits rather than plans")
}

// A signals-driven plan is made while engineers are busy, so it is not told
// the run is idle and asked for follow-up work: it is told to add fixes for
// the new evidence only. An idle replan still gets the idle note.
func TestPlannerTellsASignalsPlanThatEngineersAreBusy(t *testing.T) {
	f := signalsFirstPlan(t)
	f.beads.set(planningBead("sc-alpha", domain.BeadStateInProgress), planningBead("sc-beta", domain.BeadStateOpen))
	f.sig.set("ERROR E-2 checkout fails\n", "")
	f.clock.Advance(signalsInterval)
	f.demands()
	require.Equal(t, []Demand{planDemand()}, f.demands())
	notes := strings.Join(f.fire().Notes, "\n")
	assert.NotContains(t, notes, "no engineer is running", "an engineer is running")
	assert.NotContains(t, notes, "propose the follow-up work")
	assert.Contains(t, notes, "Engineers are still working through the beads listed below")
	assert.Contains(t, notes, "Propose fixes for the new evidence alone")

	// The next plan the run asks for because it went quiet is an idle one.
	f.setStatus(SchedulerStatus{Quiet: true, Idle: true, Fired: 10})
	f.clock.Advance(2 * signalsInterval)
	notes = strings.Join(f.fire().Notes, "\n")
	assert.Contains(t, notes, "no engineer is running")
	assert.NotContains(t, notes, "Engineers are still working")
}

// With engineers busy and a reading that changes every interval (any
// timestamped output does), a plan every signals_interval would queue
// beads far faster than the run ships them. Once the run has as many beads
// still to ship as one plan may create, a changed reading waits; once the
// backlog is worked down it plans again.
func TestPlannerHoldsSignalsPlansWhileTheBacklogIsFull(t *testing.T) {
	f := signalsFirstPlan(t)
	all := []domain.Bead{planningBead("sc-alpha", domain.BeadStateInProgress)}
	plans := 0
	for round := range 10 {
		f.sig.set(fmt.Sprintf("2026-10-24T%02d:00Z ERROR E-%d\n", round, round), "")
		f.clock.Advance(signalsInterval)
		f.demands()
		if len(f.demands()) == 0 {
			continue
		}
		keys := make([]string, f.p.MaxBeads())
		for i := range keys {
			keys[i] = fmt.Sprintf("r%d-b%d", round, i)
		}
		f.runner.setPlan(planningPlan(keys...))
		f.fire()
		plans++
		// One bead of the new plan is picked up; the rest stay open.
		for i, k := range keys {
			st := domain.BeadStateOpen
			if i == 0 {
				st = domain.BeadStateInProgress
			}
			all = append(all, planningBead(domain.BeadID("sc-"+k), st))
		}
		f.beads.set(all...)
		f.busy()
	}
	assert.Equal(t, 1, plans, "the first plan filled the backlog")

	// The run works it down: everything but two beads ships.
	for i := range all {
		if i > 1 {
			all[i].State = domain.BeadStateClosed
		}
	}
	all[1].State = domain.BeadStateNeedsHuman // parked for a person: not backlog
	f.beads.set(all...)
	f.sig.set("2026-10-24T11:00Z ERROR E-11\n", "")
	f.clock.Advance(signalsInterval)
	f.demands()
	assert.Equal(t, []Demand{planDemand()}, f.demands(), "the backlog is below one plan again")
}

func TestSignalsOutstandingCountsWorkStillToShip(t *testing.T) {
	for _, st := range []domain.BeadState{domain.BeadStateOpen, domain.BeadStateInProgress, domain.BeadStateInReview,
		domain.BeadStateChangesRequested, domain.BeadStateApproved, domain.BeadStateDelivering, domain.BeadStateEscalated} {
		assert.True(t, signalsOutstanding(planningBead("sc-x", st)), st)
	}
	for _, st := range []domain.BeadState{domain.BeadStateClosed, domain.BeadStateNeedsHuman, domain.BeadStateFailed, domain.BeadStateBlocked} {
		assert.False(t, signalsOutstanding(planningBead("sc-x", st)), st)
	}
	epic := planningBead("sc-epic", domain.BeadStateOpen)
	epic.Type = domain.BeadTypeEpic
	assert.False(t, signalsOutstanding(epic), "the run epic is not work")
}

// Personal data in the signals that the product owner copies into a bead
// never reaches bd, and so never a pull request's title or body: the plan
// is rejected before anything is applied.
func TestPlannerNeverAppliesAPlanThatCopiesPersonalDataFromTheSignals(t *testing.T) {
	f := signalsSetup(t, "")
	f.sig.set("2026-10-24T10:00Z checkout 500 for alice.smith@example.com: \"my card was charged twice\"\n", "")
	plan := planningPlan("fix")
	plan.Beads[0].Title = "Fix double charge reported by alice.smith@example.com"
	f.runner.setPlan(plan)
	_, err := f.p.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
	require.ErrorIs(t, err, agents.ErrPlanRejected)
	assert.Contains(t, err.Error(), "copies an email address from the production signals")
	app, ok := f.p.opts.Applier.(*planningApplier)
	require.True(t, ok)
	assert.Zero(t, app.count(), "nothing reached bd")
}

func TestPlannerRecordsNoBeadsForAFailedSignalsPlan(t *testing.T) {
	f := signalsSetup(t, "")
	f.p.sig.firing = true
	f.p.signalsPlanned(map[string]domain.BeadID{"fix": "sc-fix"}, errors.New("apply failed"))
	assert.False(t, f.p.sig.firing)
	assert.Empty(t, f.p.sig.planBeads)
	assert.False(t, f.p.sig.untouched)

	f.p.sig.firing = true
	f.p.signalsPlanned(map[string]domain.BeadID{"fix": "sc-fix"}, nil)
	assert.Equal(t, []domain.BeadID{"sc-fix"}, f.p.sig.planBeads)
	assert.True(t, f.p.sig.untouched)
}

func TestPlannerSignalsStatusOnceTheReadingIsOverdue(t *testing.T) {
	f := signalsSetup(t, "")
	f.fire()
	f.clock.Advance(signalsInterval + time.Minute)
	st := f.p.Status().Signals
	require.NotNil(t, st)
	assert.True(t, st.NextAt.IsZero(), "a due time gone by is shown as the next plan")
	v := RunVitals{At: f.clock.Now(), Planning: f.p.Status()}
	assert.Contains(t, v.SignalsLine(), "· due on the next plan")
}

func TestPlannerKeepsTheP828HoldForASignalsPlan(t *testing.T) {
	f := signalsFirstPlan(t)
	// The hold's backstop is the idle interval; make it outlast the
	// signals interval so the hold is what is being tested.
	f.p.idle = 4 * signalsInterval
	f.sig.set("ERROR E-2\n", "")
	f.clock.Advance(signalsInterval)
	f.demands()
	require.Equal(t, []Demand{planDemand()}, f.demands())
	f.fire()

	// A plan has just landed and the scheduler has fired nothing since:
	// the P8-28 watermark holds every replan, signals or not.
	f.mu.Lock()
	f.status = SchedulerStatus{Live: 1, Fired: f.status.Fired, ByRole: map[domain.Role]int{domain.RoleEngineer: 1}}
	f.mu.Unlock()
	f.p.sigMu.Lock()
	f.p.sig.firedAt, f.p.sig.untouched = time.Time{}, false
	f.p.sigMu.Unlock()
	f.sig.set("ERROR E-3\n", "")
	f.clock.Advance(signalsInterval)
	f.demands()
	f.demands()
	assert.Empty(t, f.demands(), "a changed reading never bypasses the P8-28 hold")
}

func TestPlannerSignalsStatusBeforeTheFirstReading(t *testing.T) {
	f := signalsSetup(t, "")
	st := f.p.Status().Signals
	require.NotNil(t, st)
	assert.True(t, st.LastAt.IsZero())
	v := RunVitals{At: planningEpoch, Planning: f.p.Status()}
	assert.Equal(t, "signals: not run yet, due on the next plan", v.SignalsLine())
	assert.Equal(t, "", v.Header())
}

func TestRunVitalsSignalsLine(t *testing.T) {
	t.Parallel()
	v := RunVitals{At: planningEpoch, Planning: PlanningStatus{Signals: &SignalsStatus{
		LastAt: planningEpoch.Add(-5 * time.Minute), ExitCode: 0, Bytes: 2048,
		NextAt: planningEpoch.Add(25 * time.Minute),
	}}}
	assert.Equal(t, "signals: last 5m0s ago (2026-09-03 11:55:00Z) · exit 0 · 2048 bytes · next in 25m0s (2026-09-03 12:25:00Z)",
		v.SignalsLine())
	assert.Contains(t, v.Lines(), v.SignalsLine())
	assert.Equal(t, "signals exit 0, 2048 bytes, next in 25m0s (2026-09-03 12:25:00Z)", v.Header())

	v.Planning.Signals.ExitCode, v.Planning.Signals.LastError = 2, "exit status 2"
	assert.Contains(t, v.SignalsLine(), "exit 2 · 2048 bytes")
	assert.Contains(t, v.SignalsLine(), "last error: exit status 2")

	v.Planning.Signals = nil
	assert.Equal(t, "", v.SignalsLine())
	assert.NotContains(t, strings.Join(v.Lines(), "\n"), "signals")
}

// The configured command reaches the runner: signals_command, the timeout
// and the repository root, with the secrets filter on the way back.
func TestPlannerRunsTheConfiguredSignalsCommand(t *testing.T) {
	rc := planningRunConfig(t, "")
	rc.Effective.SignalsCommand = `echo "ran in $(pwd)"; echo "token ghp_ABCDEFGHIJKLMNOPQRST"`
	runner := &planningRunner{plan: planningPlan("alpha")}
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: runner, Applier: &planningApplier{}, NoShim: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	require.True(t, p.signalsOn())
	_, err = p.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
	require.NoError(t, err)
	req, _ := runner.last()
	require.NotNil(t, req.Signals)
	assert.Equal(t, fmt.Sprintf("ran in %s\ntoken ghp_[REDACTED]\n", rc.Run.RepoPath), req.Signals.Output)
	assert.NotContains(t, req.Signals.Command, "ghp_ABCDEFGHIJKLMNOPQRST", "the command the prompt echoes is filtered too")
	assert.Equal(t, config.DefaultSignalsInterval, p.sigInterval)
}

// signals_interval and limits.signals_timeout come from the configuration
// when no option overrides them.
func TestPlannerTakesTheConfiguredSignalsIntervalAndTimeout(t *testing.T) {
	rc := planningRunConfig(t, "")
	rc.Effective.SignalsCommand = "echo started; sleep 30"
	rc.Effective.SignalsInterval = config.NewDuration(7 * time.Minute)
	rc.Effective.Limits.SignalsTimeout = config.NewDuration(200 * time.Millisecond)
	runner := &planningRunner{plan: planningPlan("alpha")}
	p, err := NewPlanner(PlannerOptions{
		Config: rc, Runner: runner, Applier: &planningApplier{}, NoShim: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	assert.Equal(t, 7*time.Minute, p.sigInterval)
	_, err = p.Fire(context.Background(), Firing{Demand: planDemand(), Agent: domain.Agent{ID: "po-plan-1"}})
	require.NoError(t, err)
	req, _ := runner.last()
	require.NotNil(t, req.Signals)
	assert.Equal(t, "timed out after 200ms", req.Signals.Failure)
}
