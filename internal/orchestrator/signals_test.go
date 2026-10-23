package orchestrator

import (
	"bytes"
	"context"
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
	calls int
	now   func() time.Time
}

func (s *signalsFake) run(context.Context) SignalsReading {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	r := SignalsReading{Command: "signals", RanAt: s.now(), Output: s.out, Err: s.err}
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
}
