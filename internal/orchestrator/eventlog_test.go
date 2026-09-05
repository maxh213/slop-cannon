package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/testutil"
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

// TestEventLogTerminatesATornLineOnReopen is the writing half of surviving
// a short write: a resumed run re-opens the log with O_APPEND, and if the
// last record was cut short (ENOSPC, or a kill between the record and its
// newline) the next event must not be glued onto the wreck. One newline
// keeps the damage to the one line that was already lost.
func TestEventLogTerminatesATornLineOnReopen(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenEventLog(dir, nil)
	require.NoError(t, err)
	first.Publish(events.RunStarted{Meta: elogAt(0)})
	require.NoError(t, first.Close())
	f, err := os.OpenFile(EventLogPath(dir), os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"kind":"run_fin`) // the write that ran out of disk
	require.NoError(t, err)
	require.NoError(t, f.Close())

	second, err := OpenEventLog(dir, nil)
	require.NoError(t, err)
	second.Publish(events.RunResumed{Meta: elogAt(1), Reason: "user"})
	require.NoError(t, second.Close())

	got, err := ReadEventLog(dir)
	require.Error(t, err, "the torn record is gone and is reported")
	require.Equal(t, []events.Kind{events.KindRunStarted, events.KindRunResumed},
		[]events.Kind{got[0].Kind(), got[1].Kind()},
		"the event written after the tear survives it")
	require.Len(t, got, 2)
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

// elogOrder is a Publisher that appends its name to a shared list, so the
// order Tee forwards in can be asserted rather than assumed.
type elogOrder struct {
	name string
	seen *[]string
}

// Publish implements Publisher.
func (r *elogOrder) Publish(events.Event) { *r.seen = append(*r.seen, r.name) }

// TestTeeForwardsInArgumentOrder pins the invariant the run's wiring rests
// on: Options.Bus = Tee(log, bus) writes the file before any subscriber is
// told, so anything the TUI or the headless renderer shows is already
// durable. Reverse the arguments and this fails.
func TestTeeForwardsInArgumentOrder(t *testing.T) {
	var seen []string
	tee := Tee(&elogOrder{name: "log", seen: &seen}, nil, &elogOrder{name: "bus", seen: &seen})
	tee.Publish(events.RunStarted{Meta: elogAt(0)})
	tee.Publish(events.RunFinished{Meta: elogAt(1)})
	require.Equal(t, []string{"log", "bus", "log", "bus"}, seen)
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
	require.NoError(t, err, "an unfinished last line is a note, not a failure")
	require.Equal(t, 3, s.Events, "everything before the unfinished record is reported")
	require.Equal(t, []string{logPartialTailNote}, s.Notes,
		"a log that stops mid-record is a run still writing, not damage")
}

func TestWriteEventLogMatchesTheReader(t *testing.T) {
	var buf bytes.Buffer
	want := elogRun()
	require.NoError(t, WriteEventLog(&buf, want))
	got, err := events.ReadAll(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.Equal(t, len(want), len(got))
}

// elogAfterLog stands where a subscriber stands in Tee(log, bus): it
// records how many events the run log already held when it was told, which
// is the ordering guarantee Tee's argument order exists to give.
type elogAfterLog struct {
	log *EventLog
	mu  sync.Mutex
	at  []uint64
}

// Publish implements Publisher. It runs on whichever goroutine published,
// so it records rather than asserts.
func (p *elogAfterLog) Publish(events.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.at = append(p.at, p.log.Written())
}

// written returns the recorded counts.
func (p *elogAfterLog) written() []uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]uint64(nil), p.at...)
}

// TestRunPublishesEveryTransitionToTheLog is the P6-08 acceptance for the
// writing half: wire the log into the run through Tee, in the documented
// order, and every lifecycle transition ends up on disk — before any
// subscriber hears about it — without the run knowing.
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

	// The log comes first in the tee, so every event is on disk before the
	// live bus is told. after records what the log held at each hand-off.
	after := &elogAfterLog{log: log}

	release := make(chan struct{})
	run, err := New(rc, Options{
		Bus: Tee(log, after, bus),
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

	require.Equal(t, []uint64{1, 2, 3, 4}, after.written(),
		"Tee(log, bus): each event is durable before a subscriber is told")

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

// elogPlan is the plan a PO produced: three beads, of which the scheduler
// has only reached one. It is the inventory a real run publishes — nothing
// in production emits BeadCreated or GraphChanged (P6-32) — so it is the
// only thing that can tell `sc status` about a bead no agent has touched.
func elogPlan() domain.Plan {
	return domain.Plan{
		Brief: "add retries to the client",
		Beads: []domain.PlanBead{
			{Key: "retry", Title: "Add retry", Acceptance: "retries are bounded", Priority: domain.PriorityDefault},
			{Key: "docs", Title: "Document retries", Acceptance: "the README says so", Priority: domain.PriorityDefault},
			{Key: "retire", Title: "Retire the old client", Acceptance: "no caller is left", Priority: domain.PriorityDefault},
		},
	}
}

// elogPlanned is a run that got as far as claiming one of three planned
// beads: RunStarted, a PO firing, the plan, and one engineer claim.
func elogPlanned() []events.Event {
	po := domain.Agent{ID: "po-plan-1", Role: domain.RolePO, State: domain.AgentStateRunning, StartedAt: elogEpoch.Add(time.Second)}
	return []events.Event{
		events.RunStarted{Meta: elogAt(0)},
		events.AgentSpawned{Meta: elogAt(1), Agent: po, Task: events.TaskPlan},
		events.PlanProduced{Meta: elogAt(9), Agent: po.ID, Epic: "sc-0", Plan: elogPlan(),
			Created: map[string]domain.BeadID{"retry": "sc-1", "docs": "sc-2", "retire": "sc-3"}},
		events.AgentFinished{Meta: elogAt(10), Agent: po.ID, Role: domain.RolePO, Produced: domain.MessagePlan},
		events.BeadStateChanged{Meta: elogAt(20), Bead: "sc-1", From: domain.BeadStateOpen, To: domain.BeadStateInProgress, Round: 1},
	}
}

// TestSummariseCountsPlannedBeads is the P6-08 acceptance for the per-state
// counts: every bead the plan created is in the summary from the moment the
// plan lands, not from the moment an agent touches it.
func TestSummariseCountsPlannedBeads(t *testing.T) {
	s := Summarise(elogPlanned(), 0)

	require.Len(t, s.Beads, 3, "a bead the scheduler has not reached is still one of the run's beads")
	require.Equal(t, []domain.BeadID{"sc-1", "sc-2", "sc-3"},
		[]domain.BeadID{s.Beads[0].ID, s.Beads[1].ID, s.Beads[2].ID}, "the plan's own order")
	require.Equal(t, map[domain.BeadState]int{
		domain.BeadStateInProgress: 1, domain.BeadStateOpen: 2,
	}, s.Counts)
	require.Equal(t, "Document retries", s.Beads[1].Title, "titles come from the plan")
	require.Equal(t, "Retire the old client", s.Beads[2].Title)
	require.Equal(t, domain.BeadID("sc-0"), s.Epic)
	require.Zero(t, s.Settled())

	// The order must not depend on Go's map iteration.
	for i := 0; i < 20; i++ {
		again := Summarise(elogPlanned(), 0)
		require.Equal(t, s.Beads, again.Beads)
	}
}

// TestSummarisePlanBeadsMissingFromThePlan folds a Created map that names a
// key the plan does not, which must still be counted and in a fixed order.
func TestSummarisePlanBeadsMissingFromThePlan(t *testing.T) {
	evs := []events.Event{events.PlanProduced{Meta: elogAt(1),
		Created: map[string]domain.BeadID{"z": "sc-9", "a": "sc-7", "m": "sc-8"}}}
	s := Summarise(evs, 0)
	require.Equal(t, []domain.BeadID{"sc-7", "sc-8", "sc-9"},
		[]domain.BeadID{s.Beads[0].ID, s.Beads[1].ID, s.Beads[2].ID})
	require.Equal(t, map[domain.BeadState]int{domain.BeadStateOpen: 3}, s.Counts)

	// An empty plan (autopilot's "nothing to add", D-18) adds no beads.
	require.Empty(t, Summarise([]events.Event{events.PlanProduced{Meta: elogAt(1)}}, 0).Beads)
}

// TestSummariseAPlanDoesNotOverwriteALaterState guards the ordering rule:
// the plan seeds a bead as open, and everything after it wins.
func TestSummariseAPlanDoesNotOverwriteALaterState(t *testing.T) {
	s := Summarise([]events.Event{
		events.PlanProduced{Meta: elogAt(1), Plan: elogPlan(),
			Created: map[string]domain.BeadID{"retry": "sc-1"}},
		events.BeadStateChanged{Meta: elogAt(2), Bead: "sc-1", From: domain.BeadStateOpen, To: domain.BeadStateClosed},
		events.BeadUpdated{Meta: elogAt(3), Bead: domain.Bead{ID: "sc-1", Title: "Add retry with backoff"}},
	}, 0)
	require.Equal(t, domain.BeadStateClosed, s.Beads[0].State)
	require.Equal(t, "Add retry with backoff", s.Beads[0].Title)
}

// elogNoBead is one record of every kind that names a bead, with the bead
// left empty: what a half-written or hand-edited run log looks like.
func elogNoBead() []events.Event {
	return []events.Event{
		events.BeadCreated{Meta: elogAt(1)},
		events.BeadUpdated{Meta: elogAt(2), Bead: domain.Bead{Title: "no id"}},
		events.BeadStateChanged{Meta: elogAt(3), To: domain.BeadStateInReview},
		events.GraphChanged{Meta: elogAt(4), Beads: []domain.Bead{{Title: "no id"}}},
		events.RoundStarted{Meta: elogAt(5), Round: 1, RoundKind: events.RoundReview},
		events.AgentSpawned{Meta: elogAt(6), Agent: domain.Agent{ID: "eng-1"}, BeadTitle: "no id"},
		events.PROpened{Meta: elogAt(7), URL: "https://example.test/pr/1", Number: 1},
		events.Merged{Meta: elogAt(8), SHA: "0123456"},
		events.PlanProduced{Meta: elogAt(9), Created: map[string]domain.BeadID{"a": ""}},
	}
}

// TestSummariseSurvivesRecordsWithoutABeadID is the regression for the
// crash `sc status` used to take on a damaged run log: a record that parses
// as an event but names no bead is a note, not a nil dereference.
func TestSummariseSurvivesRecordsWithoutABeadID(t *testing.T) {
	evs := elogNoBead()
	s := Summarise(evs, 0)

	require.Equal(t, len(evs), s.Events, "every record is still counted")
	require.Empty(t, s.Beads)
	require.Empty(t, s.Counts)
	require.Len(t, s.Recent, len(evs))
	require.Len(t, s.Notes, 1)
	require.Contains(t, s.Notes[0], "8 records in the run log name no bead and were skipped",
		"every kind that names a bead, except the AgentSpawned that has always checked")
	for _, kind := range []events.Kind{
		events.KindBeadCreated, events.KindBeadUpdated, events.KindBeadStateChanged,
		events.KindGraphChanged, events.KindRoundStarted, events.KindPROpened,
		events.KindMerged, events.KindPlanProduced,
	} {
		require.Contains(t, s.Notes[0], string(kind))
	}
	// AgentSpawned has always tolerated it, so it is not in the note.
	require.NotContains(t, s.Notes[0], string(events.KindAgentSpawned))
	require.Equal(t, 1, s.Fired)

	// One record on its own is reported in the singular.
	one := Summarise([]events.Event{events.BeadStateChanged{Meta: elogAt(1), To: domain.BeadStateInReview}}, 0)
	require.Contains(t, one.Notes[0], "1 record in the run log names no bead")
}

// TestSummariseLogSurvivesARecordWithoutABeadID drives the same damage
// through the file path `sc status` uses.
func TestSummariseLogSurvivesARecordWithoutABeadID(t *testing.T) {
	dir := elogWrite(t, elogRun())
	f, err := os.OpenFile(EventLogPath(dir), os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"kind":"bead_state_changed","run_id":"20260903-120000-abcd","at":"2026-09-03T12:00:00Z","to":"in_review"}` + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	s, err := SummariseLog(dir, 0)
	require.NoError(t, err, "the record parses; it is just useless")
	require.Equal(t, len(elogRun())+1, s.Events)
	require.Len(t, s.Beads, 2, "the damaged record adds no bead")
	require.Len(t, s.Notes, 1)
	require.Contains(t, s.Notes[0], "no bead")
}

func TestSummariseReaderMatchesSummarise(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteEventLog(&buf, elogRun()))
	got, err := SummariseReader(bytes.NewReader(buf.Bytes()), 0)
	require.NoError(t, err)

	want := Summarise(elogRun(), 0)
	// Events compare by value, so only the decoded Recent needs its own check.
	require.Equal(t, len(want.Recent), len(got.Recent))
	want.Recent, got.Recent = nil, nil
	require.Equal(t, want, got)
}

func TestSummariseReaderReportsADamagedLine(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteEventLog(&buf, elogRun()[:3]))
	buf.WriteString(`{"kind":"run_fin`)
	s, err := SummariseReader(bytes.NewReader(buf.Bytes()), 0)
	require.NoError(t, err)
	require.Equal(t, 3, s.Events, "everything before the unfinished record is reported")
	require.Equal(t, []string{logPartialTailNote}, s.Notes)

	// The same half-record with a newline after it is not a run mid-write:
	// it is a line that will never parse, and it is reported as damage.
	buf.WriteString("\n")
	s, err = SummariseReader(bytes.NewReader(buf.Bytes()), 0)
	require.NoError(t, err)
	require.Equal(t, 3, s.Events)
	require.Len(t, s.Notes, 1)
	require.Contains(t, s.Notes[0], "1 line of the run log did not parse")
	require.Contains(t, s.Notes[0], "line 4")
}

// elogDamaged writes the run log a torn write leaves behind: the events up
// to k, then a record cut short with no newline (the ENOSPC or crash the
// EventLog is built to survive), then — because OpenEventLog re-opens an
// existing log with O_APPEND — every later event of the run appended
// straight onto it. The half-record swallows the line that follows it, so
// exactly one event is lost and everything after it is intact.
func elogDamaged(t *testing.T, evs []events.Event, k int) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, WriteEventLog(&buf, evs[:k]))
	buf.WriteString(`{"kind":"bead_state_chan`)
	require.NoError(t, WriteEventLog(&buf, evs[k:]))
	return &buf
}

// TestSummariseReaderKeepsFoldingPastADamagedLine is the P6-08 regression
// for a fold that used to stop at the first unreadable line: a run log
// damaged in the middle made `sc status` describe the run as it stood at
// the damage — a bead still in_progress that had long since closed — and
// say nothing about it. Everything after a bad line is folded, and the bad
// line becomes a note.
func TestSummariseReaderKeepsFoldingPastADamagedLine(t *testing.T) {
	evs := elogRun()
	const torn = 8 // BeadStateChanged sc-1 open -> in_progress

	t.Run("torn write", func(t *testing.T) {
		s, err := SummariseReader(elogDamaged(t, evs, torn), 0)
		require.NoError(t, err, "a damaged line is a note, not a failure")

		survived := append(append([]events.Event{}, evs[:torn]...), evs[torn+1:]...)
		want := Summarise(survived, 0)
		require.Equal(t, want.Events, s.Events, "every event after the damage is folded")
		require.Equal(t, want.Counts, s.Counts)
		require.Equal(t, want.LastAt, s.LastAt)
		require.Equal(t, want.Beads, s.Beads)

		sc1, ok := s.Bead("sc-1")
		require.True(t, ok)
		require.Equal(t, domain.BeadStateClosed, sc1.State, "the closure after the damage still counts")
		sc2, ok := s.Bead("sc-2")
		require.True(t, ok)
		require.Equal(t, domain.BeadStateInReview, sc2.State)

		require.Len(t, s.Notes, 1)
		require.Contains(t, s.Notes[0], "1 line of the run log did not parse and was skipped")
		require.Contains(t, s.Notes[0], "everything after it was still read")
	})

	// A log written by a newer sc carries kinds this binary has no type
	// for. One unknown line must cost one line, not the rest of the run.
	t.Run("kind from a newer sc", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, WriteEventLog(&buf, evs[:torn]))
		buf.WriteString(`{"kind":"credits_paused","run_id":"20260903-120000-abcd","at":"2026-09-03T12:00:30Z"}` + "\n")
		require.NoError(t, WriteEventLog(&buf, evs[torn:]))

		s, err := SummariseReader(&buf, 0)
		require.NoError(t, err)
		want := Summarise(evs, 0)
		require.Equal(t, want.Events, s.Events, "only the unknown line is lost")
		require.Equal(t, want.Counts, s.Counts)
		require.Len(t, s.Notes, 1)
		require.Contains(t, s.Notes[0], "credits_paused")
	})

	// Several bad lines are counted, and the first is quoted.
	t.Run("several", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, WriteEventLog(&buf, evs[:2]))
		buf.WriteString("{\n{\n")
		require.NoError(t, WriteEventLog(&buf, evs[2:]))
		s, err := SummariseReader(&buf, 0)
		require.NoError(t, err)
		require.Equal(t, len(evs), s.Events)
		require.Len(t, s.Notes, 1)
		require.Contains(t, s.Notes[0], "2 lines of the run log did not parse")
		require.Contains(t, s.Notes[0], "line 3")
	})
}

// elogBroken is a reader that fails part way through, the way a truncated
// file or a bad sector does.
type elogBroken struct {
	data []byte
	err  error
}

// Read implements io.Reader.
func (r *elogBroken) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// TestSummariseReaderStopsOnAStreamThatCannotBeRead pins the other half of
// skipping bad lines: a failure that consumes no line (an I/O error, or a
// line past the reader's size limit) must end the fold rather than be
// retried for ever.
func TestSummariseReaderStopsOnAStreamThatCannotBeRead(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteEventLog(&buf, elogRun()[:3]))
	boom := errors.New("i/o error")

	done := make(chan struct{})
	var s RunSummary
	var err error
	go func() {
		defer close(done)
		s, err = SummariseReader(&elogBroken{data: buf.Bytes(), err: boom}, 0)
	}()
	select {
	case <-done:
	case <-time.After(testutil.Patience(10 * time.Second)):
		t.Fatal("SummariseReader did not return: a read failure must not be retried")
	}
	require.ErrorIs(t, err, boom)
	require.Equal(t, 3, s.Events, "everything read before the failure is reported")
}

// TestReadEventLogSkipsDamagedLines: the slice-returning reader follows the
// same rule as the fold — every event that parses comes back, and the lines
// that did not are described by the error rather than hiding the rest.
func TestReadEventLogSkipsDamagedLines(t *testing.T) {
	evs := elogRun()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(EventLogPath(dir), elogDamaged(t, evs, 8).Bytes(), 0o600))

	got, err := ReadEventLog(dir)
	require.Error(t, err, "the caller is told the log is damaged")
	require.Contains(t, err.Error(), "1 line of the run log did not parse")
	require.Len(t, got, len(evs)-1, "only the torn record is lost")
	require.Equal(t, events.KindBeadStateChanged, got[len(got)-1].Kind())

	// An unfinished last line is a run still writing, not an error.
	var buf bytes.Buffer
	require.NoError(t, WriteEventLog(&buf, evs[:3]))
	buf.WriteString(`{"kind":"run_fin`)
	live := t.TempDir()
	require.NoError(t, os.WriteFile(EventLogPath(live), buf.Bytes(), 0o600))
	got, err = ReadEventLog(live)
	require.NoError(t, err)
	require.Len(t, got, 3)
}

// TestSummariseCountsABeadWithNoState: every bead in Beads is counted, so
// the total `sc status` prints and the per-state rows under it agree even
// for a bead the log has named but never given a state.
func TestSummariseCountsABeadWithNoState(t *testing.T) {
	s := Summarise([]events.Event{
		events.RunStarted{Meta: elogAt(0)},
		events.AgentSpawned{Meta: elogAt(1), BeadTitle: "Add retry", Agent: domain.Agent{
			ID: "eng-sc-1-r1", Role: domain.RoleEngineer, Bead: "sc-1",
		}},
	}, 0)
	require.Len(t, s.Beads, 1)
	require.Equal(t, "Add retry", s.Beads[0].Title)
	require.Empty(t, s.Beads[0].State)
	require.Equal(t, map[domain.BeadState]int{BeadStateUnrecorded: 1}, s.Counts)

	total := 0
	for _, n := range s.Counts {
		total += n
	}
	require.Equal(t, len(s.Beads), total, "the breakdown must add up to the total")
}

// elogRepeat is an io.Reader that serves the same log line n times without
// ever holding more than the line: a run log too big to slurp.
type elogRepeat struct {
	line []byte
	left int
	off  int
}

// Read implements io.Reader.
func (r *elogRepeat) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.line[r.off:])
	r.off += n
	if r.off == len(r.line) {
		r.off = 0
		r.left--
	}
	return n, nil
}

// TestSummariseReaderIsBoundedByRecentNotByTheLog is the P6-08 acceptance
// for reading a long run: a run log grows for the life of the run (one line
// per cartridge output line), and folding it must cost the summary, not the
// file.
func TestSummariseReaderIsBoundedByRecentNotByTheLog(t *testing.T) {
	const n = 50_000
	var one bytes.Buffer
	require.NoError(t, WriteEventLog(&one, []events.Event{events.Warning{
		Meta: elogAt(1), Source: "cartridge",
		Message: "the cartridge said something long enough to be worth measuring, repeatedly",
	}}))
	line := one.Bytes()
	require.Greater(t, len(line)*n, 8<<20, "the fixture log must be far bigger than the budget below")

	// Live heap after a collection: what the fold is still holding on to.
	live := func() int64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return int64(m.HeapAlloc) //nolint:gosec // a heap size fits an int64.
	}

	before := live()
	s, err := SummariseReader(&elogRepeat{line: line, left: n}, 5)
	require.NoError(t, err)
	grew := live() - before
	runtime.KeepAlive(s)

	require.Equal(t, n, s.Events)
	require.Equal(t, n, s.Warnings)
	require.Len(t, s.Recent, 5, "only the tail is kept")
	t.Logf("folding %d events (%d bytes of log) kept %d bytes live", n, len(line)*n, grew)
	require.Less(t, grew, int64(4<<20),
		"folding %d events kept %d bytes live; the whole log must not be held", n, grew)
}

func TestProbeLiveness(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs", "20260903-120000-abcd")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := LockPath(dir)

	// No lock at all: a finished run, or a crashed one.
	l := ProbeLiveness(dir, "20260903-120000-abcd")
	require.False(t, l.Held)
	require.False(t, l.Running())

	write := func(h LockHolder) {
		t.Helper()
		data, err := json.Marshal(h)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o600))
	}

	// Held by this very process: the run is live.
	write(LockHolder{Run: "20260903-120000-abcd", PID: os.Getpid(), TakenAt: elogEpoch})
	l = ProbeLiveness(dir, "20260903-120000-abcd")
	require.True(t, l.Held)
	require.True(t, l.Mine)
	require.True(t, l.Alive)
	require.True(t, l.Running())
	require.Equal(t, os.Getpid(), l.Holder.PID)

	// Held for another run: this one is not running.
	write(LockHolder{Run: "20260904-000000-ffff", PID: os.Getpid(), TakenAt: elogEpoch})
	l = ProbeLiveness(dir, "20260903-120000-abcd")
	require.True(t, l.Held)
	require.False(t, l.Mine)
	require.False(t, l.Running())
	require.True(t, ProbeLiveness(dir, "").Mine, "an empty run matches whatever holds the lock")

	// A pid nothing can be behind, and a lock that is not JSON at all.
	write(LockHolder{Run: "20260903-120000-abcd", PID: 0, TakenAt: elogEpoch})
	l = ProbeLiveness(dir, "20260903-120000-abcd")
	require.True(t, l.Held)
	require.False(t, l.Alive)
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o600))
	require.False(t, ProbeLiveness(dir, "20260903-120000-abcd").Held)
}

// --- the cap and the rotation (P6-33) ------------------------------------

// TestEventLogRotatesPastTheCap: a run log that grows past its cap is
// rolled into events.jsonl.1 and a fresh file is started, so a multi-day
// autopilot run costs two segments rather than everything it ever said.
func TestEventLogRotatesPastTheCap(t *testing.T) {
	dir := t.TempDir()
	log, err := OpenEventLogWith(dir, EventLogOptions{MaxBytes: 512})
	require.NoError(t, err)

	for i := 0; i < 40; i++ {
		require.NoError(t, log.Write(events.AgentOutput{
			Meta: elogAt(i), Agent: "eng-sc-1-r1", Stream: events.StreamStdout,
			Line: "a line of the agent's output that is not especially short",
		}))
	}
	require.Positive(t, log.Rotations(), "the log passed its cap and rolled over")
	require.Less(t, log.Size(), int64(1024), "the live segment is bounded by the cap")
	require.NoError(t, log.Close())

	rotated := filepath.Join(dir, EventLogRotatedName)
	require.FileExists(t, rotated)
	require.Equal(t, []string{rotated, EventLogPath(dir)}, EventLogSegments(dir),
		"the rotated segment is read first: it holds the older events")

	info, err := os.Stat(rotated)
	require.NoError(t, err)
	require.LessOrEqual(t, info.Size(), int64(1024))
}

// A rotation is invisible to the readers except in the events that fell off
// the end: `sc status` folds across the segments.
func TestSummariseReadsAcrossTheRotation(t *testing.T) {
	dir := t.TempDir()
	evs := elogRun()
	log, err := OpenEventLogWith(dir, EventLogOptions{MaxBytes: 700})
	require.NoError(t, err)
	for _, ev := range evs {
		require.NoError(t, log.Write(ev))
	}
	require.NoError(t, log.Close())
	require.Positive(t, log.Rotations(), "the scripted run is longer than the cap")

	got, err := ReadEventLog(dir)
	require.NoError(t, err)
	require.NotEqual(t, len(evs), len(got), "the events before the last rotation are gone")
	require.Positive(t, len(got))

	s, err := SummariseLog(dir, 0)
	require.NoError(t, err)
	require.Equal(t, len(got), s.Events, "the fold spans both segments")
	require.Equal(t, Summarise(got, 0).Counts, s.Counts)
	require.Empty(t, s.Notes, "a rotation is not damage")
}

// The cap can be switched off, which is what a test that wants one file
// wants; the default is DefaultEventLogMaxBytes.
func TestEventLogCapDefaults(t *testing.T) {
	dir := t.TempDir()
	off, err := OpenEventLogWith(dir, EventLogOptions{MaxBytes: -1})
	require.NoError(t, err)
	require.Equal(t, int64(-1), off.max)
	require.NoError(t, off.Close())

	on, err := OpenEventLog(t.TempDir(), nil)
	require.NoError(t, err)
	require.Equal(t, DefaultEventLogMaxBytes, on.max)
	require.NoError(t, on.Close())
}

// A resumed run appends to the live segment and leaves the rotated one
// alone: one log, across restarts and across the rotation.
func TestEventLogReopenKeepsTheRotatedSegment(t *testing.T) {
	dir := t.TempDir()
	log, err := OpenEventLogWith(dir, EventLogOptions{MaxBytes: 512})
	require.NoError(t, err)
	for i := 0; i < 40; i++ {
		require.NoError(t, log.Write(events.AgentOutput{Meta: elogAt(i), Agent: "eng-1", Line: "output line here"}))
	}
	require.NoError(t, log.Close())
	before, err := ReadEventLog(dir)
	require.NoError(t, err)

	again, err := OpenEventLogWith(dir, EventLogOptions{MaxBytes: 512})
	require.NoError(t, err)
	require.NoError(t, again.Write(events.RunFinished{Meta: elogAt(99), Reason: events.FinishCompleted}))
	require.NoError(t, again.Close())

	after, err := ReadEventLog(dir)
	require.NoError(t, err)
	require.Positive(t, len(before))
	require.Equal(t, events.KindRunFinished, after[len(after)-1].Kind(),
		"the resumed run appended to the live segment rather than starting a new log")
	require.FileExists(t, filepath.Join(dir, EventLogRotatedName),
		"and the rotated segment is still there to be read with it")
	require.Greater(t, len(after), 1, "the log spans both segments")
}

// --- the skipped-line counts (P6-42) -------------------------------------

// TestSummariseReportsTheSkippedLinesAsNumbers is P6-42: a machine consumer
// of `sc status --json` must not have to grep English to learn the fold is
// incomplete.
func TestSummariseReportsTheSkippedLinesAsNumbers(t *testing.T) {
	evs := elogRun()

	t.Run("a torn tail", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, WriteEventLog(&buf, evs[:3]))
		buf.WriteString(`{"kind":"run_fin`)
		s, err := SummariseReader(bytes.NewReader(buf.Bytes()), 0)
		require.NoError(t, err)
		require.True(t, s.PartialTail)
		require.Zero(t, s.SkippedLines, "a record still being written is not a skipped line")
	})

	t.Run("a damaged line", func(t *testing.T) {
		s, err := SummariseReader(elogDamaged(t, evs, 8), 0)
		require.NoError(t, err)
		require.Equal(t, 1, s.SkippedLines)
		require.False(t, s.PartialTail)
		require.NotEmpty(t, s.Notes, "the prose stays for the human")
	})

	t.Run("a record that names no bead", func(t *testing.T) {
		s := Summarise(elogNoBead(), 0)
		require.Positive(t, s.SkippedRecords)
		require.Zero(t, s.SkippedLines)
	})

	t.Run("a clean log", func(t *testing.T) {
		s := Summarise(evs, 0)
		require.Zero(t, s.SkippedLines)
		require.Zero(t, s.SkippedRecords)
		require.False(t, s.PartialTail)
	})
}

// The numbers survive the JSON `sc status --json` renders.
func TestRunSummarySkipCountsMarshal(t *testing.T) {
	s, err := SummariseReader(elogDamaged(t, elogRun(), 8), 0)
	require.NoError(t, err)
	data, err := json.Marshal(s)
	require.NoError(t, err)
	var back map[string]any
	require.NoError(t, json.Unmarshal(data, &back))
	require.Equal(t, float64(1), back["skipped_lines"])
	_, has := back["partial_tail"]
	require.False(t, has, "a clean tail is not reported at all")
}
