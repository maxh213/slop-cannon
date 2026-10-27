package dash

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/orchestrator"
)

const (
	observerTestPoll = 5 * time.Millisecond
	observerTestWait = 5 * time.Second
)

func observerTestRun(id domain.RunID) domain.Run {
	return domain.Run{
		ID: id, RepoPath: "/repo", Mode: domain.RunModeBrief,
		StartedAt: time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC),
		Config:    domain.ConfigSnapshot{Ammo: 4, Safety: true},
	}
}

func observerTestStarted(run domain.Run) events.Event {
	return events.RunStarted{Meta: events.MetaAt(run.ID, run.StartedAt), Info: run}
}

func observerTestAppend(t *testing.T, dir string, evs ...events.Event) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	for _, ev := range evs {
		line, err := events.Marshal(ev)
		require.NoError(t, err)
		observerTestAppendRaw(t, orchestrator.EventLogPath(dir), append(line, '\n'))
	}
}

func observerTestAppendRaw(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // test temp file
	require.NoError(t, err)
	_, err = f.Write(data)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func observerTestStart(t *testing.T, o *Observer) chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(observerTestWait):
			t.Error("observer did not stop with its context")
		}
	})
	return done
}

func observerTestLiveProbes(o *Observer, alive *atomic.Bool) {
	o.probeLiveness = func(_ string, run domain.RunID) orchestrator.Liveness {
		return orchestrator.Liveness{
			Holder: orchestrator.LockHolder{Run: run, PID: 42}, Held: true, Mine: true, Alive: alive.Load(),
		}
	}
	o.inspectLock = func(_ string, _ orchestrator.LockOptions) orchestrator.LockProbe {
		state := orchestrator.LockStale
		if alive.Load() {
			state = orchestrator.LockLive
		}
		return orchestrator.LockProbe{State: state, Record: orchestrator.LockRecord{
			LockHolder: orchestrator.LockHolder{Run: o.opts.Run.ID, PID: 42},
		}}
	}
}

func observerTestWaitFor(t *testing.T, o *Observer, condition func(Snapshot) bool) Snapshot {
	t.Helper()
	var latest Snapshot
	require.Eventually(t, func() bool {
		latest = o.Snapshot()
		return condition(latest)
	}, observerTestWait, observerTestPoll)
	return latest
}

func TestObserverReadsBacklogThenFollowsLive(t *testing.T) {
	dir := t.TempDir()
	run := observerTestRun("run-live")
	bead := events.BeadCreated{Meta: events.MetaAt(run.ID, run.StartedAt.Add(time.Second)),
		Bead: domain.Bead{ID: "sc-1", Title: "first", State: domain.BeadStateOpen}}
	observerTestAppend(t, dir, observerTestStarted(run), bead)
	o := NewObserver(Options{Run: run, RunDir: dir, RefreshInterval: observerTestPoll, FollowPoll: observerTestPoll})
	var alive atomic.Bool
	alive.Store(true)
	observerTestLiveProbes(o, &alive)
	observerTestStart(t, o)

	first := observerTestWaitFor(t, o, func(s Snapshot) bool { return s.Counts.Beads == 1 && s.Counts.Events == 2 })
	require.Equal(t, "running", first.Run.Status)
	spawned := events.AgentSpawned{Meta: events.MetaAt(run.ID, run.StartedAt.Add(2*time.Second)),
		Agent: domain.Agent{ID: "eng-sc-1", Role: domain.RoleEngineer, Bead: "sc-1", State: domain.AgentStateRunning,
			StartedAt: run.StartedAt.Add(2 * time.Second)}}
	output := events.AgentOutput{Meta: events.MetaAt(run.ID, run.StartedAt.Add(3*time.Second)),
		Agent: "eng-sc-1", Stream: events.StreamStdout, Line: "private transcript"}
	observerTestAppend(t, dir, spawned, output)
	second := observerTestWaitFor(t, o, func(s Snapshot) bool { return len(s.LiveAgents) == 1 && s.Counts.Events == 4 })
	require.Greater(t, second.Revision, first.Revision)
	require.Equal(t, domain.AgentID("eng-sc-1"), second.LiveAgents[0].ID)
	require.Equal(t, 3, second.Counts.Activity, "agent output is folded but is not lifecycle activity")
	require.NotContains(t, second.Activity[len(second.Activity)-1].Text, "private transcript")
}

func TestObserverHoldsPartialLineUntilNewline(t *testing.T) {
	dir := t.TempDir()
	run := observerTestRun("run-partial")
	observerTestAppend(t, dir, observerTestStarted(run))
	o := NewObserver(Options{Run: run, RunDir: dir, RefreshInterval: observerTestPoll, FollowPoll: observerTestPoll})
	var alive atomic.Bool
	alive.Store(true)
	observerTestLiveProbes(o, &alive)
	observerTestStart(t, o)
	observerTestWaitFor(t, o, func(s Snapshot) bool { return s.Counts.Events == 1 })

	line, err := events.Marshal(events.BeadCreated{Meta: events.MetaAt(run.ID, run.StartedAt.Add(time.Second)),
		Bead: domain.Bead{ID: "sc-partial", Title: "later", State: domain.BeadStateOpen}})
	require.NoError(t, err)
	line = append(line, '\n')
	path := orchestrator.EventLogPath(dir)
	observerTestAppendRaw(t, path, line[:len(line)/2])
	time.Sleep(40 * time.Millisecond)
	require.Zero(t, o.Snapshot().Counts.Beads, "an incomplete JSONL record must not be applied")
	observerTestAppendRaw(t, path, line[len(line)/2:])
	s := observerTestWaitFor(t, o, func(s Snapshot) bool { return s.Counts.Beads == 1 })
	require.Equal(t, domain.BeadID("sc-partial"), s.Beads[0].ID)
}

func TestObserverFollowsRotationWithoutLosingEvents(t *testing.T) {
	dir := t.TempDir()
	run := observerTestRun("run-rotation")
	observerTestAppend(t, dir, observerTestStarted(run))
	o := NewObserver(Options{Run: run, RunDir: dir, RefreshInterval: observerTestPoll, FollowPoll: observerTestPoll})
	var alive atomic.Bool
	alive.Store(true)
	observerTestLiveProbes(o, &alive)
	observerTestStart(t, o)
	observerTestWaitFor(t, o, func(s Snapshot) bool { return s.Counts.Events == 1 })

	observerTestAppend(t, dir, events.BeadCreated{Meta: events.MetaAt(run.ID, run.StartedAt.Add(time.Second)),
		Bead: domain.Bead{ID: "sc-old", Title: "old segment", State: domain.BeadStateOpen}})
	require.NoError(t, os.Rename(orchestrator.EventLogPath(dir), filepath.Join(dir, orchestrator.EventLogRotatedName)))
	observerTestAppend(t, dir, events.BeadCreated{Meta: events.MetaAt(run.ID, run.StartedAt.Add(2*time.Second)),
		Bead: domain.Bead{ID: "sc-new", Title: "new segment", State: domain.BeadStateOpen}})
	s := observerTestWaitFor(t, o, func(s Snapshot) bool { return s.Counts.Beads == 2 && s.Counts.Events == 3 })
	require.Equal(t, domain.BeadID("sc-old"), s.Beads[0].ID)
	require.Equal(t, domain.BeadID("sc-new"), s.Beads[1].ID)
}

func TestObserverFinishedRunKeepsServingWithHealthyObserver(t *testing.T) {
	dir := t.TempDir()
	run := observerTestRun("run-finished")
	finished := events.RunFinished{Meta: events.MetaAt(run.ID, run.StartedAt.Add(time.Minute)), Reason: events.FinishCompleted}
	observerTestAppend(t, dir, observerTestStarted(run), finished)
	o := NewObserver(Options{Run: run, RunDir: dir, RefreshInterval: observerTestPoll, FollowPoll: observerTestPoll})
	var alive atomic.Bool
	observerTestLiveProbes(o, &alive)
	done := observerTestStart(t, o)

	s := observerTestWaitFor(t, o, func(s Snapshot) bool { return s.Run.Status == "finished" && s.Observer.Stream == "complete" })
	require.Equal(t, events.FinishCompleted.String(), s.Run.FinishReason)
	require.Equal(t, "ok", s.Observer.Status)
	require.False(t, s.Run.Quiet)
	select {
	case <-done:
		t.Fatal("observer stopped serving when the event stream completed")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestObserverFollowsResumedRunWithoutReplay(t *testing.T) {
	dir := t.TempDir()
	run := observerTestRun("run-resumed")
	started, ok := observerTestStarted(run).(events.RunStarted)
	require.True(t, ok)
	finished := events.RunFinished{Meta: events.MetaAt(run.ID, run.StartedAt.Add(time.Minute)), Reason: events.FinishStopped}
	resumed := started
	resumed.Meta = events.MetaAt(run.ID, run.StartedAt.Add(2*time.Minute))
	resumed.Resumed = true
	observerTestAppend(t, dir, started, finished, resumed)
	o := NewObserver(Options{Run: run, RunDir: dir, RefreshInterval: observerTestPoll, FollowPoll: observerTestPoll})
	o.loadState = func(string) (orchestrator.State, error) {
		return orchestrator.State{
			Version: 1, Run: run, Phase: orchestrator.PhaseFinished,
			Finish: events.FinishStopped, FinishedAt: finished.At(),
		}, nil
	}
	var alive atomic.Bool
	alive.Store(true)
	observerTestLiveProbes(o, &alive)
	observerTestStart(t, o)

	s := observerTestWaitFor(t, o, func(s Snapshot) bool { return s.Counts.Events == 3 && s.Run.Status == "running" })
	require.True(t, s.Run.Resumed)
	require.Empty(t, s.Run.FinishReason)
	require.True(t, s.Run.FinishedAt.IsZero())
	require.False(t, s.Run.Stopping)
	require.Equal(t, orchestrator.PhaseRunning.String(), s.Run.Phase)
	require.Equal(t, "following", s.Observer.Stream)

	observerTestAppend(t, dir, events.BeadCreated{Meta: events.MetaAt(run.ID, run.StartedAt.Add(3*time.Minute)),
		Bead: domain.Bead{ID: "sc-after-resume", Title: "live append", State: domain.BeadStateOpen}})
	s = observerTestWaitFor(t, o, func(s Snapshot) bool { return s.Counts.Beads == 1 && s.Counts.Events == 4 })
	require.Equal(t, domain.BeadID("sc-after-resume"), s.Beads[0].ID)
	require.Equal(t, "running", s.Run.Status)
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, 4, o.Snapshot().Counts.Events, "refresh must not replay the historical incarnation")
}

func TestObserverCrashedRunIsNotObserverFailure(t *testing.T) {
	dir := t.TempDir()
	run := observerTestRun("run-crashed")
	observerTestAppend(t, dir, observerTestStarted(run))
	o := NewObserver(Options{Run: run, RunDir: dir, RefreshInterval: observerTestPoll, FollowPoll: observerTestPoll})
	o.loadState = func(string) (orchestrator.State, error) {
		return orchestrator.State{Version: 1, Run: run, Phase: orchestrator.PhaseRunning, LastEventAt: run.StartedAt}, nil
	}
	var alive atomic.Bool
	observerTestLiveProbes(o, &alive)
	done := observerTestStart(t, o)

	s := observerTestWaitFor(t, o, func(s Snapshot) bool { return s.Run.Status == "crashed" && s.Observer.Stream == "closed" })
	require.Equal(t, "ok", s.Observer.Status)
	require.Equal(t, "stale", s.Run.Lock.State)
	require.False(t, s.Run.Lock.Running)
	select {
	case <-done:
		t.Fatal("observer stopped serving a crashed run")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestObserverQuietIsSeparateFromRunningAndHealth(t *testing.T) {
	dir := t.TempDir()
	run := observerTestRun("run-quiet")
	observerTestAppend(t, dir, observerTestStarted(run))
	var clock atomic.Int64
	clock.Store(run.StartedAt.UnixNano())
	o := NewObserver(Options{
		Run: run, RunDir: dir, RefreshInterval: observerTestPoll, FollowPoll: observerTestPoll,
		QuietAfter: 2 * time.Minute, Now: func() time.Time { return time.Unix(0, clock.Load()).UTC() },
	})
	var alive atomic.Bool
	alive.Store(true)
	observerTestLiveProbes(o, &alive)
	observerTestStart(t, o)
	observerTestWaitFor(t, o, func(s Snapshot) bool { return s.Run.Status == "running" && !s.Run.Quiet })

	clock.Store(run.StartedAt.Add(10 * time.Minute).UnixNano())
	s := observerTestWaitFor(t, o, func(s Snapshot) bool { return s.Run.Quiet })
	require.Equal(t, "running", s.Run.Status)
	require.Equal(t, "ok", s.Observer.Status)
	require.GreaterOrEqual(t, s.Run.QuietForSeconds, int64(600))
}

func TestObserverDoesNotWriteRunOrLockFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "runs", "run-read-only")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	run := observerTestRun("run-read-only")
	finished := events.RunFinished{Meta: events.MetaAt(run.ID, run.StartedAt.Add(time.Minute)), Reason: events.FinishCompleted}
	observerTestAppend(t, dir, observerTestStarted(run), finished)
	state := orchestrator.State{Version: 1, Run: run, Phase: orchestrator.PhaseFinished, Finish: events.FinishCompleted, FinishedAt: finished.At()}
	require.NoError(t, orchestrator.WriteState(dir, state))
	beforeLog, err := os.ReadFile(orchestrator.EventLogPath(dir))
	require.NoError(t, err)
	beforeState, err := os.ReadFile(orchestrator.StatePath(dir))
	require.NoError(t, err)
	beforeEntries, err := os.ReadDir(dir)
	require.NoError(t, err)

	o := NewObserver(Options{Run: run, RunDir: dir, RefreshInterval: observerTestPoll, FollowPoll: observerTestPoll})
	observerTestStart(t, o)
	observerTestWaitFor(t, o, func(s Snapshot) bool { return s.Run.Status == "finished" })
	afterLog, err := os.ReadFile(orchestrator.EventLogPath(dir))
	require.NoError(t, err)
	afterState, err := os.ReadFile(orchestrator.StatePath(dir))
	require.NoError(t, err)
	afterEntries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Equal(t, beforeLog, afterLog)
	require.Equal(t, beforeState, afterState)
	require.Len(t, afterEntries, len(beforeEntries))
	_, err = os.Stat(orchestrator.LockPath(dir))
	require.True(t, errors.Is(err, os.ErrNotExist), "a read-only observer must not create a run lock")
}
