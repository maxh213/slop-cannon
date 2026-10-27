package dash

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/orchestrator"
	"slop-cannon/internal/tui"
)

func TestNoticeCategoryWhitelist(t *testing.T) {
	for _, tc := range []struct {
		name     string
		message  string
		err      string
		category string
	}{
		{name: "rate limited", message: "cartridge rate limited: MESSAGE-CANARY", category: noticeRateLimited},
		{name: "conflict", message: "rebase hit a merge conflict: MESSAGE-CANARY", category: noticeConflict},
		{name: "check", message: "check_command failed: MESSAGE-CANARY", category: noticeCheck},
		{name: "review", message: "tester returned a verdict: MESSAGE-CANARY", category: noticeReview},
		{name: "generic", message: "bookkeeping stopped: MESSAGE-CANARY", err: "ERROR-CANARY", category: noticeGeneric},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := projectNotice(tui.Notice{
				At: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC), Kind: events.KindWarning,
				Source: "test", Message: tc.message, Err: tc.err,
			})
			require.Equal(t, tc.category, n.Category)
			require.Equal(t, "warning", n.Kind)
			require.Equal(t, "test", n.Source)
			encoded, err := json.Marshal(n)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "MESSAGE-CANARY")
			require.NotContains(t, string(encoded), "ERROR-CANARY")
			require.NotContains(t, string(encoded), "message")
			require.NotContains(t, string(encoded), "error")
		})
	}
}

func TestSnapshotBoundsEveryEventDerivedString(t *testing.T) {
	huge := strings.Repeat("x", 8*1024)
	run := observerTestRun(domain.RunID(huge))
	run.Mode = domain.RunMode(huge)
	run.Epic = domain.BeadID(huge)
	o := NewObserver(Options{
		Run: run, RunDir: t.TempDir(), Now: func() time.Time { return run.StartedAt.Add(time.Hour) },
		Limits: Limits{Beads: 2, RecentAgents: 2, Reviews: 2, Deliveries: 2, Notices: 2, Activity: 3},
	})
	o.lock = orchestrator.LockProbe{State: orchestrator.LockLive, Record: orchestrator.LockRecord{
		LockHolder: orchestrator.LockHolder{Run: run.ID, PID: 42},
	}}
	o.liveness = orchestrator.Liveness{Held: true, Mine: true, Alive: true}
	o.apply(events.RunStarted{Meta: events.MetaAt(run.ID, run.StartedAt), Info: run})

	for i := 0; i < 40; i++ {
		state := domain.BeadState(fmt.Sprintf("state-%d-%s", i, huge))
		o.apply(events.BeadCreated{Meta: events.MetaAt(run.ID, run.StartedAt.Add(time.Second)), Bead: domain.Bead{
			ID: domain.BeadID(fmt.Sprintf("bead-%d-%s", i, huge)), Title: huge, State: state, Type: domain.BeadType(huge), Size: domain.Size(huge),
			PRURL: "https://example.test/" + huge, Priority: domain.PriorityDefault,
		}})
	}
	failed := domain.Agent{
		ID: domain.AgentID(huge), Role: domain.Role(huge), Lens: domain.Lens(huge), Cartridge: huge, Model: huge,
		Bead: domain.BeadID(huge), State: domain.AgentStateRunning, StartedAt: run.StartedAt,
	}
	o.apply(events.AgentSpawned{Meta: events.MetaAt(run.ID, run.StartedAt), Agent: failed,
		Task: events.Task(huge), BeadTitle: huge})
	o.apply(events.AgentFailed{Meta: events.MetaAt(run.ID, run.StartedAt), Agent: failed.ID,
		Failure: events.Failure(huge), Err: "FAILURE-PROSE-CANARY"})
	o.apply(events.AgentFinished{Meta: events.MetaAt(run.ID, run.StartedAt), Agent: failed.ID,
		Produced: domain.MessageType(huge)})
	live := domain.Agent{ID: "live", Role: domain.RoleEngineer, State: domain.AgentState(huge), StartedAt: run.StartedAt}
	o.apply(events.AgentSpawned{Meta: events.MetaAt(run.ID, run.StartedAt), Agent: live, Task: events.TaskImplement})

	o.apply(events.RoundStarted{Meta: events.MetaAt(run.ID, run.StartedAt), Bead: "sc-1", Round: 1,
		RoundKind: events.RoundKind(huge), Lenses: []domain.Lens{domain.Lens(huge)}, Commit: huge})
	o.apply(events.VerdictReceived{Meta: events.MetaAt(run.ID, run.StartedAt), Bead: "sc-1", Round: 1,
		Verdict: domain.Verdict{Lens: domain.Lens(huge), Outcome: domain.VerdictOutcome(huge), Summary: "VERDICT-CANARY"}})
	o.apply(events.ArbitrationReceived{Meta: events.MetaAt(run.ID, run.StartedAt), Bead: "sc-1", Round: 1,
		Escalation: &domain.Escalation{Outcome: domain.EscalationOutcome(huge), Notes: "ESCALATION-CANARY"}})
	o.apply(events.PROpened{Meta: events.MetaAt(run.ID, run.StartedAt), Bead: "sc-1", Number: 1,
		URL: "https://example.test/" + huge, Branch: huge, Base: huge})
	o.apply(events.Merged{Meta: events.MetaAt(run.ID, run.StartedAt), Bead: "sc-1", SHA: huge})
	o.apply(events.Warning{Meta: events.MetaAt(run.ID, run.StartedAt), Source: huge,
		Message: "NOTICE-RAW-CANARY", Err: "NOTICE-ERROR-CANARY"})
	o.apply(events.RunFinished{Meta: events.MetaAt(run.ID, run.StartedAt.Add(time.Minute)), Reason: events.FinishReason(huge)})
	o.publish()
	s := o.Snapshot()

	require.Equal(t, maxIDText, utf8.RuneCountInString(string(s.Run.ID)))
	require.Equal(t, maxIDText, utf8.RuneCountInString(s.Run.Mode))
	require.Equal(t, maxIDText, utf8.RuneCountInString(s.Run.FinishReason))
	require.True(t, s.Run.Lock.Mine, "liveness compares the full internal run id before projection truncates it")
	require.Len(t, s.Counts.BeadsByState, maxBeadStates)
	total := 0
	for state, count := range s.Counts.BeadsByState {
		require.LessOrEqual(t, utf8.RuneCountInString(state), maxIDText)
		total += count
	}
	require.Equal(t, 40, total)
	require.Empty(t, s.LiveAgents)
	require.Len(t, s.RecentAgents, 2)
	require.Equal(t, maxIDText, utf8.RuneCountInString(s.RecentAgents[0].State))
	agent := s.RecentAgents[1]
	for name, value := range map[string]string{
		"role": agent.Role, "lens": agent.Lens, "task": agent.Task, "failure": agent.Failure, "produced": agent.Produced,
	} {
		require.Equal(t, maxIDText, utf8.RuneCountInString(value), name)
	}
	require.Equal(t, maxIDText, utf8.RuneCountInString(s.Reviews[0].Kind))
	require.Equal(t, maxIDText, utf8.RuneCountInString(s.Reviews[0].Lenses[0]))
	require.Equal(t, maxIDText, utf8.RuneCountInString(s.Reviews[0].Verdicts[0].Outcome))
	require.Equal(t, maxIDText, utf8.RuneCountInString(s.Reviews[0].Escalation))
	require.Equal(t, maxURLText, utf8.RuneCountInString(s.Deliveries[0].URL))

	encoded, err := json.Marshal(s)
	require.NoError(t, err, "plain DTO strings must not invoke domain enum validation")
	require.NotContains(t, string(encoded), huge)
	for _, canary := range []string{"FAILURE-PROSE-CANARY", "VERDICT-CANARY", "ESCALATION-CANARY", "NOTICE-RAW-CANARY", "NOTICE-ERROR-CANARY"} {
		require.NotContains(t, string(encoded), canary)
	}
	require.Less(t, len(encoded), 100_000, "oversized event fields must not create an oversized response")
	var decoded any
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assertBoundedJSONStrings(t, decoded)
}

func assertBoundedJSONStrings(t *testing.T, value any) {
	t.Helper()
	switch v := value.(type) {
	case string:
		require.LessOrEqual(t, utf8.RuneCountInString(v), maxURLText)
	case []any:
		for _, item := range v {
			assertBoundedJSONStrings(t, item)
		}
	case map[string]any:
		for key, item := range v {
			require.LessOrEqual(t, utf8.RuneCountInString(key), maxURLText)
			assertBoundedJSONStrings(t, item)
		}
	}
}
