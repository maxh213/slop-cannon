package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/logging"
	"slop-cannon/internal/orchestrator"
)

// ---------------------------------------------------------------------------
// runPublisher: the seal at events.RunFinished and the settle hook (P8-01)
// ---------------------------------------------------------------------------

// runRecorder is an [orchestrator.Publisher] that keeps what it was given,
// standing in for the run log and the bus behind runPublisher.
type runRecorder struct {
	mu   sync.Mutex
	kind []events.Kind
}

func (r *runRecorder) Publish(ev events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kind = append(r.kind, ev.Kind())
}

func (r *runRecorder) kinds() []events.Kind {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Kind(nil), r.kind...)
}

// runTestPublisher builds a runPublisher over rec with the production
// redactor and no agent-transcript sink.
func runTestPublisher(t *testing.T, rec *runRecorder) *runPublisher {
	t.Helper()
	filter, err := agents.NewSecretFilter()
	require.NoError(t, err)
	return &runPublisher{to: rec, redactor: filter.Redactor()}
}

// runSealedRun builds a real orchestrator.Run whose Options.Bus is pub, so
// a publish takes the production route: the run counts it into state.json
// and only then hands it to the CLI's single publication point.
func runSealedRun(t *testing.T, pub *runPublisher) *orchestrator.Run {
	t.Helper()
	dir := t.TempDir()
	rc := &config.RunConfig{Run: domain.Run{
		ID: domain.RunID("r1"), RepoPath: dir, StateDir: filepath.Join(dir, "state"),
	}}
	run, err := orchestrator.New(rc, orchestrator.Options{
		Bus:    pub,
		Logger: slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)
	return run
}

// TestRunPublisherSealsTheStreamAtRunFinished is the CLI half of the
// run-log invariant: the run persists state.json for the last time as it
// publishes events.RunFinished, so an event published after it would land
// in events.jsonl uncounted — a log that no longer closes with
// run_finished and a last_seq that disagrees with it. The helpers that run
// beside the run (a conductor pass, a gate poll) can still be in flight at
// that moment, so nothing reaches the CLI's sinks after the terminator.
//
// The seal itself lives once, in orchestrator.Run.Publish, which is the
// one place that bumps LastSeq; this asserts that the assembled CLI stream
// is behind it (P8-01, unified in round 3).
func TestRunPublisherSealsTheStreamAtRunFinished(t *testing.T) {
	rec := &runRecorder{}
	pub := runTestPublisher(t, rec)
	run := runSealedRun(t, pub)

	run.Publish(events.AgentOutput{Line: "working"})
	run.Publish(events.RunFinished{Reason: events.FinishCompleted})
	// What a conductor pass already in flight would publish.
	run.Publish(events.GraphChanged{})
	run.Publish(events.BeadStateChanged{Bead: "sc-1"})

	require.Equal(t, []events.Kind{events.KindAgentOutput, events.KindRunFinished}, rec.kinds(),
		"the run log must close with run_finished")
	require.Equal(t, 2, run.PublishedAfterFinish(), "the events published after the run finished are counted")
}

// TestRunPublisherSealIsAFenceUnderConcurrency makes the same point with
// the timing the production failure needed: many publishers racing the one
// that finishes the run. Whatever the interleaving, run_finished is the
// last thing the CLI's sinks are given.
func TestRunPublisherSealIsAFenceUnderConcurrency(t *testing.T) {
	rec := &runRecorder{}
	pub := runTestPublisher(t, rec)
	run := runSealedRun(t, pub)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 200; j++ {
				run.Publish(events.GraphChanged{})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		run.Publish(events.RunFinished{Reason: events.FinishCompleted})
	}()
	close(start)
	wg.Wait()

	got := rec.kinds()
	require.NotEmpty(t, got)
	require.Equal(t, events.KindRunFinished, got[len(got)-1], "run_finished is the last line of the log")
	for _, k := range got[:len(got)-1] {
		require.NotEqual(t, events.KindRunFinished, k, "the run finishes once")
	}
}

// TestRunPublisherRedactsBeforeEverySink pins the P7-17/P9-09 property the
// seal was folded into: the sinks behind runPublisher are handed the
// scrubbed and redacted event, not the raw one.
func TestRunPublisherRedactsBeforeEverySink(t *testing.T) {
	rec := &runRecorder{}
	pub := runTestPublisher(t, rec)
	var seen []events.Event
	var mu sync.Mutex
	pub.watch(func(ev events.Event) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, ev)
	})
	pub.before = func(ev events.Event) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, ev)
	}

	const secret = "ghp_0123456789abcdefghijklmnopqrstuvwxyz"
	pub.Publish(events.AgentOutput{Line: "token " + secret})

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, seen, 2, "the transcript sink and the settle sink both see the event")
	for _, ev := range seen {
		out, ok := ev.(events.AgentOutput)
		require.True(t, ok)
		require.NotContains(t, out.Line, secret)
	}
}

// ---------------------------------------------------------------------------
// settleQueue: P6-41's event never gets dropped
// ---------------------------------------------------------------------------

// TestSettleQueueLosesNothingUnderABurst is why the settler is fed from the
// publication point instead of an events.Bus subscription: the bus drops
// rather than blocks when a subscriber is behind, so a burst of agent
// output while the settling goroutine is descheduled could lose the
// BeadStateChanged that tells the deliverer a safety-on delivery ended.
// The queue is asked to swallow far more than any bus buffer would hold,
// with an observer slow enough to fall behind, and still delivers all of
// it in order.
func TestSettleQueueLosesNothingUnderABurst(t *testing.T) {
	const burst = 2000
	var mu sync.Mutex
	var got []events.Kind
	seen := make(chan struct{}, 1)
	q := newSettleQueue(func(ev events.Event) {
		mu.Lock()
		got = append(got, ev.Kind())
		n := len(got)
		mu.Unlock()
		if n == burst+1 {
			seen <- struct{}{}
		}
	})

	for i := 0; i < burst; i++ {
		q.push(events.AgentOutput{Line: "chatter"})
	}
	q.push(events.BeadStateChanged{Bead: "sc-1", To: domain.BeadStateClosed})

	select {
	case <-seen:
	case <-time.After(10 * time.Second):
		t.Fatal("the settle queue never drained")
	}
	q.close()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, burst+1)
	require.Equal(t, events.KindBeadStateChanged, got[len(got)-1],
		"the event the settler exists for arrives after the burst, not instead of it")
}

// TestSettleQueueDrainsWhatItHoldsBeforeItCloses: close is what the
// assembled run calls on its way out, and a delivery that settled in the
// last moments of a run must still be observed.
func TestSettleQueueDrainsWhatItHoldsBeforeItCloses(t *testing.T) {
	var mu sync.Mutex
	var n int
	release := make(chan struct{})
	q := newSettleQueue(func(events.Event) {
		<-release
		mu.Lock()
		n++
		mu.Unlock()
	})
	for i := 0; i < 5; i++ {
		q.push(events.BeadStateChanged{Bead: domain.BeadID("sc-1")})
	}
	close(release)
	q.close()
	q.close() // idempotent

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 5, n)
}

// TestSettleQueuePushAfterCloseIsANoOp: the queue outlives nothing, and a
// late publisher must not panic the run on its way out.
func TestSettleQueuePushAfterCloseIsANoOp(t *testing.T) {
	q := newSettleQueue(func(events.Event) { t.Error("nothing was pushed before close") })
	q.close()
	q.push(events.BeadStateChanged{Bead: "sc-1"})
	q.push(nil)
}

// ---------------------------------------------------------------------------
// The assembly: what it wires, and what it releases when it fails (P8-01)
// ---------------------------------------------------------------------------

// runAssembleSetup resolves a run on the scripted machine and returns the
// configuration and the runSetup fireCannon would have built, without
// firing anything.
func runAssembleSetup(t *testing.T, env *runFireEnv) (*config.RunConfig, runSetup) {
	t.Helper()
	rc, err := ResolveRun(RunOptions{
		Path: env.Repo.Dir, Brief: runFireBrief, Headless: true,
		Overrides: config.Overrides{Cartridge: strPtr("mock")},
	})
	require.NoError(t, err)
	filter, err := agents.NewSecretFilter()
	require.NoError(t, err)
	runLog, err := logging.OpenRunLog(rc.Run.StateDir, rc.Run.ID, LogOptions(false))
	require.NoError(t, err)
	t.Cleanup(func() { _ = runLog.Close() })
	return rc, runSetup{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Out:      io.Discard,
		Lock:     orchestrator.NewLockPolicy(orchestrator.LockOptions{}),
		Filter:   filter,
		RunLog:   runLog,
		Headless: true,
	}
}

func strPtr(s string) *string { return &s }

// TestRunAssemblyWiresTheDeliverySettler is the CLI-level cover P6-41 never
// had: its own test is at orchestrator level, so nothing here would notice
// if `sc run` stopped feeding the settler altogether. The wiring is a field
// on the run's single publication point, so it can be asserted directly.
func TestRunAssemblyWiresTheDeliverySettler(t *testing.T) {
	env := runFireSetup(t, true)
	rc, set := runAssembleSetup(t, env)

	c, err := assembleRun(context.Background(), rc, set)
	require.NoError(t, err)
	defer c.close()

	require.NotNil(t, c.settleq, "the deliverer's settler must be fed by the assembled run")
	require.True(t, c.pub.watching(), "the settle queue must hang off the run's publication point")
}

// TestRunAssemblyReleasesWhatItBuiltWhenItFails: `defer c.close()` used to
// be registered by fireCannon only after a successful assembly, so a
// failure anywhere from orchestrator.New onwards left the event bus, the
// interrupt registration, the supervisor and the open event log behind,
// and left a run directory nothing had been written into for a later
// `--resume` to adopt. The seam fails the assembly at its very last step,
// where the most has been built.
func TestRunAssemblyReleasesWhatItBuiltWhenItFails(t *testing.T) {
	env := runFireSetup(t, true)
	rc, set := runAssembleSetup(t, env)

	var built *cannon
	saved := runOnAssemble
	t.Cleanup(func() { runOnAssemble = saved })
	runOnAssemble = func(c *cannon) error {
		built = c
		return errUnassembled
	}

	c, err := assembleRun(context.Background(), rc, set)
	require.ErrorIs(t, err, errUnassembled)
	require.Nil(t, c)
	require.NotNil(t, built, "the seam saw the assembled run")
	require.True(t, built.released, "a failed assembly releases what it built")
	require.Nil(t, built.log, "the event log is closed")
	require.Nil(t, built.stop, "the bus, the interrupt registration and the supervisor are released")

	// P8-01 review: and the D-23 run directory nothing was recorded into
	// is gone, so config.LatestRunID cannot hand its id to `--resume`.
	_, statErr := os.Stat(rc.Run.StateDir)
	require.ErrorIs(t, statErr, os.ErrNotExist)
	latest, found, err := config.LatestRunID(env.Repo.Dir)
	require.NoError(t, err)
	require.False(t, found, "an aborted assembly leaves no run to resume, got %q", latest)
}

// errUnassembled is what the assembly seam fails with.
var errUnassembled = errors.New("the test failed the assembly")

// TestDiscardRunDirKeepsAnythingEverRecorded: the release must never throw
// away a run that got as far as writing something, which is what keeps a
// resumed run whose assembly failed intact.
func TestDiscardRunDirKeepsAnythingEverRecorded(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	withState := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(withState, orchestrator.StateFileName), []byte("{}"), 0o644))
	discardRunDir(withState, logger)
	require.DirExists(t, withState)

	withLog := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(withLog, orchestrator.EventLogName), []byte("{}\n"), 0o644))
	discardRunDir(withLog, logger)
	require.DirExists(t, withLog)

	empty := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(empty, orchestrator.EventLogName), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(empty, logging.RunLogName), []byte("{}\n"), 0o644))
	discardRunDir(empty, logger)
	require.NoDirExists(t, empty)

	discardRunDir("", logger) // nothing to do, and no panic
}

// ---------------------------------------------------------------------------
// The invariant on a real run
// ---------------------------------------------------------------------------

// TestRunLogClosesWithRunFinishedAndAgreesWithTheState fires a whole
// scripted run with the conductor syncing as fast as it can, so a pass is
// almost certainly still in flight when the run finishes, and then checks
// the two things that failure broke: events.jsonl ends with run_finished,
// and state.json's last_seq is the number of lines in it.
func TestRunLogClosesWithRunFinishedAndAgreesWithTheState(t *testing.T) {
	env := runFireSetup(t, true)
	runConductorInterval = time.Millisecond

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)

	dir := config.RunDir(env.Repo.Dir, string(runLatestID(t, env.Repo.Dir)))
	data, err := os.ReadFile(filepath.Join(dir, orchestrator.EventLogName))
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	require.NotEmpty(t, lines)

	var last struct {
		Kind string `json:"kind"`
	}
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &last))
	require.Equal(t, string(events.KindRunFinished), last.Kind, "the run log closes with run_finished")

	st, err := orchestrator.LoadState(dir)
	require.NoError(t, err)
	require.Equal(t, uint64(len(lines)), st.LastSeq,
		"state.json and the run log must agree on the event count")
	require.Equal(t, events.KindRunFinished, st.LastEventKind)
}
