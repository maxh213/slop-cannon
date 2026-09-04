package orchestrator

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// Tests for the run log and the run-log fold (P6-08). Every helper here is
// prefixed "elog" so it cannot collide with another Phase 6 task's file in
// this package.

// elogEpoch is the fixed time the scripted runs below start at.
var elogEpoch = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

// elogAt stamps an event's Meta n seconds into the run.
func elogAt(n int) events.Meta {
	return events.MetaAt("20260903-120000-abcd", elogEpoch.Add(time.Duration(n)*time.Second))
}

// elogRun is a small but complete scripted run: two beads, one of them
// delivered and merged, one still in review, one live tester, a warning and
// some agent output. It exercises every branch of Summarise.
func elogRun() []events.Event {
	run := domain.Run{
		ID: "20260903-120000-abcd", RepoPath: "/repo", Mode: domain.RunModeBrief,
		Brief: "add retries to the client", StartedAt: elogEpoch,
		Config: domain.ConfigSnapshot{Ammo: 10, Safety: true, Cartridge: "claude", Model: "fable"},
	}
	po := domain.Agent{ID: "po-plan-1", Role: domain.RolePO, Cartridge: "claude", Model: "fable", State: domain.AgentStateRunning, StartedAt: elogEpoch.Add(time.Second), PID: 4241}
	eng := domain.Agent{ID: "eng-sc-1-r1", Role: domain.RoleEngineer, Cartridge: "claude", Model: "fable", Bead: "sc-1", State: domain.AgentStateRunning, StartedAt: elogEpoch.Add(20 * time.Second), PID: 4242}
	tester := domain.Agent{ID: "tester-qa-sc-2-r1", Role: domain.RoleTester, Lens: domain.LensQA, Cartridge: "claude", Model: "fable", Bead: "sc-2", State: domain.AgentStateRunning, StartedAt: elogEpoch.Add(60 * time.Second), PID: 4243}
	return []events.Event{
		events.RunStarted{Meta: elogAt(0), Info: run},
		events.AgentSpawned{Meta: elogAt(1), Agent: po, Task: events.TaskPlan},
		events.AgentOutput{Meta: elogAt(2), Agent: po.ID, Stream: events.StreamStdout, Line: "researching"},
		events.PlanProduced{Meta: elogAt(9), Agent: po.ID, Epic: "sc-0",
			Created: map[string]domain.BeadID{"a": "sc-1", "b": "sc-2"}},
		events.AgentFinished{Meta: elogAt(10), Agent: po.ID, Role: domain.RolePO, Duration: 9 * time.Second, Produced: domain.MessagePlan},
		events.BeadCreated{Meta: elogAt(11), Bead: domain.Bead{ID: "sc-1", Title: "Add retry", State: domain.BeadStateOpen}},
		events.BeadCreated{Meta: elogAt(12), Bead: domain.Bead{ID: "sc-2", Title: "Document retries", State: domain.BeadStateOpen}},
		events.AgentSpawned{Meta: elogAt(20), Agent: eng, Task: events.TaskImplement, BeadTitle: "Add retry", Round: 1},
		events.BeadStateChanged{Meta: elogAt(21), Bead: "sc-1", From: domain.BeadStateOpen, To: domain.BeadStateInProgress, Round: 1},
		events.AgentFinished{Meta: elogAt(40), Agent: eng.ID, Role: domain.RoleEngineer, Bead: "sc-1", Duration: 20 * time.Second, Produced: domain.MessageImplReport},
		events.RoundStarted{Meta: elogAt(41), Bead: "sc-1", Round: 1, RoundKind: events.RoundReview, Lenses: []domain.Lens{domain.LensQA}},
		events.BeadStateChanged{Meta: elogAt(42), Bead: "sc-1", From: domain.BeadStateInProgress, To: domain.BeadStateInReview, Round: 1},
		events.BeadStateChanged{Meta: elogAt(50), Bead: "sc-1", From: domain.BeadStateInReview, To: domain.BeadStateApproved, Round: 1, Reason: "all testers approved"},
		events.PROpened{Meta: elogAt(51), Bead: "sc-1", URL: "https://example.test/pr/7", Number: 7, Branch: "sc/sc-1-add-retry", Base: "main"},
		events.BeadStateChanged{Meta: elogAt(52), Bead: "sc-1", From: domain.BeadStateApproved, To: domain.BeadStateDelivering, Round: 1},
		events.Merged{Meta: elogAt(55), Bead: "sc-1", SHA: "0123456789abcdef", Branch: "main", PRNumber: 7, Pushed: true},
		events.BeadStateChanged{Meta: elogAt(56), Bead: "sc-1", From: domain.BeadStateDelivering, To: domain.BeadStateClosed, Round: 1},
		events.Warning{Meta: elogAt(58), Source: "gates", Message: "gh is unavailable; gate polling skipped"},
		events.AgentSpawned{Meta: elogAt(60), Agent: tester, Task: events.TaskReview, BeadTitle: "Document retries", Round: 1},
		events.BeadStateChanged{Meta: elogAt(61), Bead: "sc-2", From: domain.BeadStateInProgress, To: domain.BeadStateInReview, Round: 1},
	}
}

// elogWrite writes evs to a fresh run directory and returns it.
func elogWrite(t *testing.T, evs []events.Event) string {
	t.Helper()
	dir := t.TempDir()
	log, err := OpenEventLog(dir, nil)
	require.NoError(t, err)
	for _, ev := range evs {
		require.NoError(t, log.Write(ev))
	}
	require.NoError(t, log.Close())
	return dir
}

func TestEventLogRoundTrip(t *testing.T) {
	want := elogRun()
	dir := elogWrite(t, want)

	require.Equal(t, filepath.Join(dir, EventLogName), EventLogPath(dir))
	info, err := os.Stat(EventLogPath(dir))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(runFilePerm), info.Mode().Perm(), "the run log holds prompts and diffs; keep it private")

	got, err := ReadEventLog(dir)
	require.NoError(t, err)
	require.Len(t, got, len(want))
	for i := range want {
		require.Equal(t, want[i].Kind(), got[i].Kind(), "event %d", i)
		require.Equal(t, want[i].At().UTC(), got[i].At(), "event %d", i)
		require.Equal(t, want[i], got[i], "event %d", i)
	}
}

func TestEventLogAppendsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenEventLog(dir, nil)
	require.NoError(t, err)
	first.Publish(events.RunStarted{Meta: elogAt(0)})
	require.Equal(t, uint64(1), first.Written())
	require.NoError(t, first.Close())
	require.NoError(t, first.Close(), "Close is idempotent")

	second, err := OpenEventLog(dir, nil)
	require.NoError(t, err)
	second.Publish(events.RunResumed{Meta: elogAt(1), Reason: "user"})
	require.NoError(t, second.Close())

	got, err := ReadEventLog(dir)
	require.NoError(t, err)
	require.Equal(t, []events.Kind{events.KindRunStarted, events.KindRunResumed},
		[]events.Kind{got[0].Kind(), got[1].Kind()})
}

func TestEventLogWriteFailureIsStickyAndSilent(t *testing.T) {
	dir := t.TempDir()
	log, err := OpenEventLog(dir, nil)
	require.NoError(t, err)
	require.NoError(t, log.Close())

	require.NoError(t, log.Err())
	log.Publish(events.RunStarted{Meta: elogAt(0)}) // must not panic
	require.ErrorIs(t, log.Err(), os.ErrClosed)
	require.ErrorIs(t, log.Write(events.RunStopped{Meta: elogAt(1)}), os.ErrClosed)
	require.Zero(t, log.Written())

	var nilLog *EventLog
	nilLog.Publish(events.RunStarted{Meta: elogAt(0)})
	require.NoError(t, nilLog.Write(nil))
	require.NoError(t, nilLog.Close())
	require.Zero(t, nilLog.Written())
	require.NoError(t, nilLog.Err())
}

func TestEventLogRejectsEmptyDir(t *testing.T) {
	_, err := OpenEventLog("", nil)
	require.Error(t, err)
}

// elogRecorder is a Publisher that remembers what it was given.
type elogRecorder struct{ got []events.Event }

func (r *elogRecorder) Publish(ev events.Event) { r.got = append(r.got, ev) }

func TestTee(t *testing.T) {
	require.Nil(t, Tee())
	require.Nil(t, Tee(nil, nil))

	one := &elogRecorder{}
	require.Same(t, one, Tee(nil, one), "a single publisher is not wrapped")

	a, b := &elogRecorder{}, &elogRecorder{}
	tee := Tee(a, nil, b)
	ev := events.RunStarted{Meta: elogAt(0)}
	tee.Publish(ev)
	require.Equal(t, []events.Event{ev}, a.got)
	require.Equal(t, []events.Event{ev}, b.got)
}

func TestSummariseScriptedRun(t *testing.T) {
	s := Summarise(elogRun(), 0)

	require.Equal(t, domain.RunID("20260903-120000-abcd"), s.Run)
	require.Equal(t, PhaseRunning, s.Phase)
	require.False(t, s.Resumed)
	require.Equal(t, domain.BeadID("sc-0"), s.Epic)
	require.Equal(t, elogEpoch, s.StartedAt)
	require.Equal(t, elogAt(61).Time, s.LastAt)
	require.Equal(t, len(elogRun()), s.Events)
	require.Equal(t, 1, s.Output)
	require.Equal(t, 1, s.Plans)
	require.Equal(t, 1, s.Rounds)
	require.Equal(t, 3, s.Fired)
	require.Equal(t, 2, s.Completed)
	require.Zero(t, s.Failed)
	require.Equal(t, 1, s.PRs)
	require.Equal(t, 1, s.Merges)
	require.Equal(t, 1, s.Warnings)
	require.Zero(t, s.Errors)

	require.Equal(t, map[domain.BeadState]int{
		domain.BeadStateClosed: 1, domain.BeadStateInReview: 1,
	}, s.Counts)
	require.Equal(t, 1, s.Settled())

	sc1, ok := s.Bead("sc-1")
	require.True(t, ok)
	require.Equal(t, "Add retry", sc1.Title)
	require.Equal(t, domain.BeadStateClosed, sc1.State)
	require.Equal(t, 1, sc1.Round)
	require.Equal(t, "https://example.test/pr/7", sc1.PR)
	require.Equal(t, 7, sc1.PRNumber)
	require.True(t, sc1.Merged)
	require.Equal(t, elogAt(56).Time, sc1.UpdatedAt)

	sc2, ok := s.Bead("sc-2")
	require.True(t, ok)
	require.Equal(t, "Document retries", sc2.Title)
	require.Equal(t, domain.BeadStateInReview, sc2.State)

	_, ok = s.Bead("sc-99")
	require.False(t, ok)

	// Only the tester is still running; the PO and the engineer finished.
	require.Equal(t, 1, s.Live())
	require.Equal(t, domain.AgentID("tester-qa-sc-2-r1"), s.Agents[0].ID)

	// Recent keeps the tail, oldest first, and skips agent output.
	require.Len(t, s.Recent, DefaultRecentEvents)
	require.Equal(t, events.KindBeadStateChanged, s.Recent[len(s.Recent)-1].Kind())
	for _, ev := range s.Recent {
		require.NotEqual(t, events.KindAgentOutput, ev.Kind())
	}
}

func TestSummariseRecentBounds(t *testing.T) {
	evs := elogRun()
	require.Len(t, Summarise(evs, 3).Recent, 3)
	require.Empty(t, Summarise(evs, -1).Recent)
	require.Len(t, Summarise(evs, 1000).Recent, len(evs)-1, "the one AgentOutput line is skipped")
}

func TestSummarisePhases(t *testing.T) {
	base := events.RunStarted{Meta: elogAt(0)}
	require.Equal(t, PhaseNew, Summarise(nil, 0).Phase)
	require.Equal(t, PhaseRunning, Summarise([]events.Event{base}, 0).Phase)

	paused := Summarise([]events.Event{base, events.RunPaused{Meta: elogAt(1), Reason: "user"}}, 0)
	require.Equal(t, PhasePaused, paused.Phase)
	require.Equal(t, "user", paused.PauseReason)

	resumed := Summarise([]events.Event{base,
		events.RunPaused{Meta: elogAt(1), Reason: "user"},
		events.RunResumed{Meta: elogAt(2), Reason: "user"}}, 0)
	require.Equal(t, PhaseRunning, resumed.Phase)
	require.Empty(t, resumed.PauseReason)

	stopped := Summarise([]events.Event{base, events.RunStopped{Meta: elogAt(3), Reason: "SIGINT"}}, 0)
	require.Equal(t, PhaseStopping, stopped.Phase)
	require.Equal(t, "SIGINT", stopped.StopReason)

	finished := Summarise([]events.Event{base,
		events.RunFinished{Meta: elogAt(4), Reason: events.FinishCompleted, Summary: "2 beads landed", Duration: 4 * time.Second}}, 0)
	require.Equal(t, PhaseFinished, finished.Phase)
	require.Equal(t, events.FinishCompleted, finished.Finish)
	require.Equal(t, "2 beads landed", finished.FinishSummary)
	require.Equal(t, 4*time.Second, finished.Duration)
	require.Equal(t, elogAt(4).Time, finished.FinishedAt)
}

func TestSummariseAgentBookkeeping(t *testing.T) {
	a := domain.Agent{ID: "eng-sc-1-r1", Role: domain.RoleEngineer, Bead: "sc-1", State: domain.AgentStateRunning, PID: 1}
	b := domain.Agent{ID: "eng-sc-2-r1", Role: domain.RoleEngineer, Bead: "sc-2", State: domain.AgentStateRunning, PID: 2}
	c := domain.Agent{ID: "tester-qa-sc-3-r1", Role: domain.RoleTester, Lens: domain.LensQA, Bead: "sc-3", State: domain.AgentStateRunning, PID: 3}

	s := Summarise([]events.Event{
		events.AgentSpawned{Meta: elogAt(1), Agent: a},
		events.AgentSpawned{Meta: elogAt(2), Agent: b},
		events.AgentSpawned{Meta: elogAt(3), Agent: c},
		// A crash retry: the first attempt fails, then the same id is
		// spawned again and must not appear twice.
		events.AgentFailed{Meta: elogAt(4), Agent: b.ID, Failure: events.FailureCrash, Attempt: 1, WillRetry: true},
		events.AgentSpawned{Meta: elogAt(5), Agent: b, Attempt: 2},
		events.AgentFinished{Meta: elogAt(6), Agent: a.ID},
	}, 0)

	require.Equal(t, 4, s.Fired)
	require.Equal(t, 1, s.Failed)
	require.Equal(t, 1, s.Completed)
	require.Equal(t, []domain.AgentID{c.ID, b.ID}, []domain.AgentID{s.Agents[0].ID, s.Agents[1].ID},
		"dropping the middle agent must not disturb the others")
	require.Len(t, s.Agents, 2)

	// Finishing the rest empties the list rather than leaving a stale one.
	s = Summarise([]events.Event{
		events.AgentSpawned{Meta: elogAt(1), Agent: a},
		events.AgentFinished{Meta: elogAt(2), Agent: a.ID},
		events.AgentFinished{Meta: elogAt(3), Agent: "never-spawned"},
	}, 0)
	require.Nil(t, s.Agents)
	require.Zero(t, s.Live())
}

func TestSummariseGraphAndUpdates(t *testing.T) {
	s := Summarise([]events.Event{
		events.GraphChanged{Meta: elogAt(1), Epic: "sc-0", Beads: []domain.Bead{
			{ID: "sc-1", Title: "Add retry", State: domain.BeadStateOpen},
			{ID: "sc-2", Title: "Docs", State: domain.BeadStateBlocked, Round: 2},
		}},
		events.BeadUpdated{Meta: elogAt(2), Bead: domain.Bead{ID: "sc-1", Title: "Add retry with backoff", State: domain.BeadStateInProgress, Round: 1}, Fields: []string{"title"}},
	}, 0)
	require.Equal(t, domain.BeadID("sc-0"), s.Epic)
	require.Equal(t, []domain.BeadID{"sc-1", "sc-2"}, []domain.BeadID{s.Beads[0].ID, s.Beads[1].ID})
	require.Equal(t, "Add retry with backoff", s.Beads[0].Title)
	require.Equal(t, domain.BeadStateInProgress, s.Beads[0].State)
	require.Equal(t, 1, s.Beads[0].Round)
	require.Equal(t, 2, s.Beads[1].Round)
	require.Equal(t, 1, s.Settled(), "blocked is settled")
}

func TestSummariseIgnoresNilEvents(t *testing.T) {
	s := Summarise([]events.Event{nil, events.RunStarted{Meta: elogAt(0)}, nil}, 0)
	require.Equal(t, 1, s.Events)
	require.Equal(t, PhaseRunning, s.Phase)
}

func TestSummariseLog(t *testing.T) {
	dir := elogWrite(t, elogRun())
	s, err := SummariseLog(dir, 0)
	require.NoError(t, err)
	require.Equal(t, len(elogRun()), s.Events)
	require.Len(t, s.Recent, DefaultRecentEvents)
}

func TestSummariseLogMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := SummariseLog(dir, 0)
	require.ErrorIs(t, err, ErrNoEventLog)
	_, err = ReadEventLog(dir)
	require.ErrorIs(t, err, ErrNoEventLog)
}

func TestSummariseLogTruncatedTail(t *testing.T) {
	dir := elogWrite(t, elogRun()[:3])
	// A run killed mid-write leaves half a line behind.
	f, err := os.OpenFile(EventLogPath(dir), os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"kind":"run_fin`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	s, err := SummariseLog(dir, 0)
	require.Error(t, err, "the caller is told the log is damaged")
	require.Equal(t, 3, s.Events, "everything before the damage is still reported")
}

func TestWriteEventLogMatchesTheReader(t *testing.T) {
	var buf bytes.Buffer
	want := elogRun()
	require.NoError(t, WriteEventLog(&buf, want))
	got, err := events.ReadAll(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.Equal(t, len(want), len(got))
}

// TestRunPublishesEveryTransitionToTheLog is the P6-08 acceptance for the
// writing half: wire the log into the run through Tee and every lifecycle
// transition ends up on disk, in order, without the run knowing.
func TestRunPublishesEveryTransitionToTheLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	repo := filepath.Join(home, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	cfg := config.Defaults()
	cfg.Ammo = 10
	cfg.ActiveCartridge = "claude"
	cfg.Cartridges = map[string]config.Cartridge{"claude": {Model: "fable"}}
	eff := &config.Effective{Config: cfg, Sources: map[string]config.Source{}}
	rc, err := config.NewRunConfig(repo, eff, config.RunSettings{
		ID: "20260903-120000-abcd", Brief: "add retries", StartedAt: elogEpoch,
	})
	require.NoError(t, err)

	log, err := OpenEventLog(rc.Run.StateDir, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })

	bus := events.NewBus()
	t.Cleanup(bus.Close)

	release := make(chan struct{})
	run, err := New(rc, Options{
		Bus: Tee(bus, log),
		Now: func() time.Time { return elogEpoch },
		Loop: LoopFunc(func(_ context.Context, _ *Run) (Outcome, error) {
			<-release
			return Outcome{Reason: events.FinishCompleted, Summary: "nothing to do"}, nil
		}),
	})
	require.NoError(t, err)

	require.NoError(t, run.Start(context.Background()))
	require.NoError(t, run.Pause("user"))
	require.NoError(t, run.Resume("user"))
	close(release)
	_, err = run.Wait()
	require.NoError(t, err)
	require.NoError(t, log.Close())

	got, err := ReadEventLog(rc.Run.StateDir)
	require.NoError(t, err)
	kinds := make([]events.Kind, 0, len(got))
	for _, ev := range got {
		kinds = append(kinds, ev.Kind())
	}
	require.Equal(t, []events.Kind{
		events.KindRunStarted, events.KindRunPaused, events.KindRunResumed, events.KindRunFinished,
	}, kinds)
	require.NoError(t, log.Err())

	s := Summarise(got, 0)
	require.Equal(t, PhaseFinished, s.Phase)
	require.Equal(t, events.FinishCompleted, s.Finish)
	require.Equal(t, rc.Run.ID, s.Run)

	// state.json and the run log agree on how many events were published.
	st, err := LoadState(rc.Run.StateDir)
	require.NoError(t, err)
	require.Equal(t, uint64(len(got)), st.LastSeq)
}
