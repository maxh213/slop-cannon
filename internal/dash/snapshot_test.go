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
)

func TestSnapshotIsBoundedAndRedacted(t *testing.T) {
	run := observerTestRun("run-redacted")
	run.Brief = "PROMPT-SECRET"
	run.Config.Vibes = []string{"RULE-SECRET"}
	run.Config.Rules = []string{"RULE-SECRET"}
	o := NewObserver(Options{
		Run: run, RunDir: t.TempDir(), QuietAfter: time.Hour,
		Limits: Limits{Beads: 2, RecentAgents: 2, Reviews: 2, Deliveries: 2, Notices: 2, Activity: 3},
		Now:    func() time.Time { return run.StartedAt.Add(time.Hour) },
	})
	o.lock = orchestrator.LockProbe{State: orchestrator.LockLive, Record: orchestrator.LockRecord{
		LockHolder: orchestrator.LockHolder{Run: run.ID, PID: 42},
	}}
	o.liveness = orchestrator.Liveness{Held: true, Mine: true, Alive: true}
	at := run.StartedAt
	meta := func() events.Meta { at = at.Add(time.Second); return events.MetaAt(run.ID, at) }
	o.apply(events.RunStarted{Meta: meta(), Info: run})

	for i := 0; i < 3; i++ {
		id := domain.BeadID(fmt.Sprintf("sc-%d", i))
		bead := domain.Bead{
			ID: id, Title: fmt.Sprintf("title <script>alert('x')</script> %d", i),
			Description: "PROMPT-SECRET", Acceptance: "DIFF-SECRET",
			State: domain.BeadStateOpen, Priority: domain.PriorityDefault,
		}
		if i == 1 {
			bead.State = domain.BeadStateClosed
		}
		if i == 2 {
			for j := 0; j < 70; j++ {
				bead.Deps = append(bead.Deps, domain.BeadID(fmt.Sprintf("dep-%d", j)))
			}
		}
		o.apply(events.BeadCreated{Meta: meta(), Bead: bead})
	}

	for i := 0; i < 5; i++ {
		id := domain.AgentID(fmt.Sprintf("eng-%d", i))
		agent := domain.Agent{ID: id, Role: domain.RoleEngineer, Bead: "sc-2", State: domain.AgentStateRunning, StartedAt: at}
		o.apply(events.AgentSpawned{Meta: meta(), Agent: agent, Task: events.TaskImplement, BeadTitle: "safe title", Attempt: 1})
		if i == 0 {
			o.apply(events.AgentOutput{Meta: meta(), Agent: id, Stream: events.StreamStdout, Line: "AGENT-OUTPUT-SECRET"})
			continue
		}
		if i%2 == 0 {
			o.apply(events.AgentFailed{Meta: meta(), Agent: id, Role: agent.Role, Bead: agent.Bead,
				Failure: events.FailureCrash, Err: "AGENT-ERROR-SECRET", Duration: 3 * time.Second})
		} else {
			o.apply(events.AgentFinished{Meta: meta(), Agent: id, Role: agent.Role, Bead: agent.Bead,
				Duration: 2 * time.Second, Produced: domain.MessageImplReport})
		}
	}

	for i := 0; i < 3; i++ {
		bead := domain.BeadID(fmt.Sprintf("sc-%d", i))
		lenses := make([]domain.Lens, 40)
		knownLenses := domain.Lenses()
		for j := range lenses {
			lenses[j] = knownLenses[j%len(knownLenses)]
		}
		o.apply(events.RoundStarted{Meta: meta(), Bead: bead, Round: i + 1, RoundKind: events.RoundReview,
			Lenses: lenses, Commit: "0123456789abcdef"})
		o.apply(events.VerdictReceived{Meta: meta(), Bead: bead, Round: i + 1, Verdict: domain.Verdict{
			Lens: domain.LensQA, Outcome: domain.VerdictChanges, Summary: "VERDICT-SECRET",
			Items: []domain.FeedbackItem{{ID: "item-1", Title: "VERDICT-SECRET", Detail: "DIFF-SECRET"}},
		}})
		o.apply(events.ResponseReceived{Meta: meta(), Bead: bead, Round: i + 1,
			Responses: []domain.Response{{ItemID: "item-1", Action: domain.ActionPushback, Rationale: "RESPONSE-SECRET"}},
			Report:    &domain.ImplReport{Summary: "REPORT-SECRET", HowToVerify: "PROMPT-SECRET"},
		})
		o.apply(events.ArbitrationReceived{Meta: meta(), Bead: bead, Round: i + 1,
			Rulings:    []domain.Arbitration{{ItemID: "item-1", Ruling: domain.RulingCompromise, Rationale: "RULING-SECRET"}},
			Escalation: &domain.Escalation{Outcome: domain.EscalationApproveWithNotes, Notes: "ESCALATION-SECRET"},
		})
	}

	for i := 0; i < 3; i++ {
		bead := domain.BeadID(fmt.Sprintf("sc-%d", i))
		o.apply(events.PROpened{Meta: meta(), Bead: bead, Number: i + 10,
			URL: fmt.Sprintf("https://example.test/pull/%d", i+10), Branch: "sc/branch", Base: "main"})
		if i == 2 {
			o.apply(events.Merged{Meta: meta(), Bead: bead, PRNumber: i + 10, SHA: "abcdef123456", Branch: "main", Pushed: true})
		}
	}
	warnings := []events.Warning{
		{Source: "cartridge", Message: "NOTICE-MESSAGE-CANARY rate limited", Err: "NOTICE-ERROR-CANARY"},
		{Source: "review", Message: "NOTICE-REVIEW-CANARY review finding", Err: "NOTICE-REVIEW-ERROR-CANARY"},
		{Source: strings.Repeat("s", maxIDText*3), Message: strings.Repeat("w", maxIDText*3), Err: "NOTICE-GENERIC-CANARY"},
	}
	for _, warning := range warnings {
		warning.Meta = meta()
		o.apply(warning)
	}
	o.publish()
	s := o.Snapshot()

	require.Len(t, s.Beads, 2)
	require.Len(t, s.LiveAgents, 1, "an ordinary live set below the hard cap is returned in full")
	require.Zero(t, s.LiveAgentsOmitted)
	require.Len(t, s.RecentAgents, 2)
	require.Len(t, s.Reviews, 2)
	require.Len(t, s.Deliveries, 2)
	require.Len(t, s.Notices, 2)
	require.Len(t, s.Activity, 3)
	require.Equal(t, Counts{
		Events: s.Counts.Events, Beads: 3, Agents: 5, LiveAgents: 1,
		Reviews: 3, Deliveries: 3, PullRequests: 3, Merges: 1, Notices: 3,
		Activity: s.Counts.Activity, BeadsByState: s.Counts.BeadsByState,
	}, s.Counts)
	require.Equal(t, 1, s.Omitted.Beads)
	require.Equal(t, 2, s.Omitted.Agents)
	require.Equal(t, 1, s.Omitted.Reviews)
	require.Equal(t, 1, s.Omitted.Deliveries)
	require.Equal(t, 1, s.Omitted.Notices)
	require.Equal(t, s.Counts.Activity-3, s.Omitted.Activity)
	require.Equal(t, 2, s.Counts.BeadsByState["open"])
	require.Equal(t, 1, s.Counts.BeadsByState["closed"])
	require.Len(t, s.Beads[1].Deps, maxBeadDeps)
	require.Equal(t, 6, s.Beads[1].DepsOmitted)
	require.Len(t, s.Reviews[0].Lenses, maxReviewLens)
	require.Equal(t, 8, s.Reviews[0].LensesOmitted)
	require.Equal(t, 1, s.Reviews[0].Verdicts[0].ItemCount)
	require.Equal(t, 1, s.Reviews[0].ResponseCount)
	require.Equal(t, 1, s.Reviews[0].RulingCount)
	require.True(t, s.Reviews[0].HasReport)
	require.Equal(t, string(domain.EscalationApproveWithNotes), s.Reviews[0].Escalation)
	require.Equal(t, noticeReview, s.Notices[0].Category)
	require.Equal(t, noticeGeneric, s.Notices[1].Category)
	require.LessOrEqual(t, utf8.RuneCountInString(s.Notices[1].Source), maxIDText)
	for _, a := range s.Activity {
		require.NotEqual(t, events.KindAgentOutput.String(), a.Kind)
	}

	encoded, err := json.Marshal(s)
	require.NoError(t, err)
	for _, secret := range []string{
		"PROMPT-SECRET", "RULE-SECRET", "DIFF-SECRET", "AGENT-OUTPUT-SECRET", "AGENT-ERROR-SECRET",
		"VERDICT-SECRET", "RESPONSE-SECRET", "REPORT-SECRET", "RULING-SECRET", "ESCALATION-SECRET",
		"NOTICE-MESSAGE-CANARY", "NOTICE-ERROR-CANARY", "NOTICE-REVIEW-CANARY", "NOTICE-REVIEW-ERROR-CANARY",
		"NOTICE-GENERIC-CANARY",
	} {
		require.NotContains(t, string(encoded), secret)
	}
	require.NotContains(t, string(encoded), "last_line")
	require.NotContains(t, string(encoded), "agent_output")
	require.NotContains(t, string(encoded), `"message"`)
	require.NotContains(t, string(encoded), `"error"`)
	require.NotContains(t, string(encoded), "<script>alert", "JSON must HTML-escape a run-controlled bead title")
	require.Contains(t, string(encoded), `\u003cscript\u003ealert`)

	// A caller receives a deep copy, not mutable aliases into the cached value.
	s.Beads[0].Title = "mutated"
	s.Beads[1].Deps[0] = "mutated"
	s.Counts.BeadsByState["open"] = 999
	s.Reviews[0].Verdicts[0].Outcome = domain.VerdictApprove.String()
	again := o.Snapshot()
	require.NotEqual(t, "mutated", again.Beads[0].Title)
	require.NotEqual(t, domain.BeadID("mutated"), again.Beads[1].Deps[0])
	require.Equal(t, 2, again.Counts.BeadsByState["open"])
	require.Equal(t, domain.VerdictChanges.String(), again.Reviews[0].Verdicts[0].Outcome)
	require.Greater(t, again.Revision, uint64(1))
}

func TestRunLivenessUsesHardenedLockVerdict(t *testing.T) {
	run := observerTestRun("run-lock")
	for _, tc := range []struct {
		name      string
		lock      orchestrator.LockState
		alive     bool
		want      string
		running   bool
		pidReused bool
	}{
		{name: "live", lock: orchestrator.LockLive, alive: true, want: "running", running: true},
		{name: "mine", lock: orchestrator.LockMine, alive: true, want: "running", running: true},
		{name: "stale", lock: orchestrator.LockStale, want: "crashed"},
		{name: "pid reused", lock: orchestrator.LockStale, alive: true, want: "crashed", pidReused: true},
		{name: "elsewhere", lock: orchestrator.LockElsewhere, alive: true, want: "unknown"},
		{name: "free", lock: orchestrator.LockFree, want: "crashed"},
		{name: "corrupt", lock: orchestrator.LockCorrupt, want: "crashed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := NewObserver(Options{Run: run, RunDir: t.TempDir(), Now: func() time.Time { return run.StartedAt }})
			o.apply(observerTestStarted(run))
			o.lock = orchestrator.LockProbe{State: tc.lock, Record: orchestrator.LockRecord{
				LockHolder: orchestrator.LockHolder{Run: run.ID, PID: 42},
			}}
			if tc.lock == orchestrator.LockFree || tc.lock == orchestrator.LockCorrupt {
				o.lock.Record.PID = 0
			}
			o.liveness = orchestrator.Liveness{Alive: tc.alive}
			s := o.project(run.StartedAt)
			require.Equal(t, tc.want, s.Run.Status)
			require.Equal(t, tc.running, s.Run.Lock.Running)
			require.Equal(t, tc.pidReused, s.Run.Lock.PIDReused)
			require.Equal(t, "ok", s.Observer.Status)
		})
	}
}

func TestFinishedStateWinsOverAbsentLock(t *testing.T) {
	run := observerTestRun("run-finished-lock")
	o := NewObserver(Options{Run: run, RunDir: t.TempDir()})
	o.apply(observerTestStarted(run))
	o.hasDurable = true
	o.durable = orchestrator.State{Run: run, Phase: orchestrator.PhaseRunning}
	o.apply(events.RunFinished{Meta: events.MetaAt(run.ID, run.StartedAt.Add(time.Minute)), Reason: events.FinishStopped})
	s := o.project(run.StartedAt.Add(time.Hour))
	require.Equal(t, "finished", s.Run.Status)
	require.Equal(t, orchestrator.PhaseFinished.String(), s.Run.Phase, "the terminal event overrides a stale durable running phase")
	require.Equal(t, events.FinishStopped.String(), s.Run.FinishReason)
	require.False(t, s.Run.Lock.Running)
	require.Equal(t, int64(time.Minute.Milliseconds()), s.Run.ElapsedMillis)
}

func TestSnapshotEmptyCollectionsStayArrays(t *testing.T) {
	o := NewObserver(Options{Run: observerTestRun("run-empty"), RunDir: t.TempDir()})
	s := o.Snapshot()
	require.NotNil(t, s.Beads)
	require.NotNil(t, s.LiveAgents)
	require.NotNil(t, s.RecentAgents)
	require.NotNil(t, s.Reviews)
	require.NotNil(t, s.Deliveries)
	require.NotNil(t, s.Notices)
	require.NotNil(t, s.Activity)
	encoded, err := json.Marshal(s)
	require.NoError(t, err)
	for _, field := range []string{"beads", "live_agents", "recent_agents", "reviews", "deliveries", "notices", "activity"} {
		require.Contains(t, string(encoded), `"`+field+`":[]`)
	}
}
