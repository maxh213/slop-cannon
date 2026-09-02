package events

import (
	"testing"
	"time"

	"slop-cannon/internal/domain"
)

// sampleTime is fixed so encoded samples are byte-stable across runs.
var sampleTime = time.Date(2026, 9, 2, 12, 1, 3, 500_000_000, time.UTC)

const sampleRun domain.RunID = "run-20260902-1201"

func sampleMeta() Meta { return MetaAt(sampleRun, sampleTime) }

func sampleBead() domain.Bead {
	return domain.Bead{
		ID:          "sc-1a2b",
		Title:       "Add retry",
		Description: "Retry transient failures with backoff.",
		Acceptance:  "Three attempts, exponential backoff, tests.",
		Type:        domain.BeadTypeFeature,
		Priority:    domain.PriorityDefault,
		Size:        domain.SizeM,
		Deps:        []domain.BeadID{"sc-0f0f"},
		State:       domain.BeadStateInReview,
		Assignee:    "eng-sc-1a2b-r1",
		Labels:      []string{"sc:run:" + string(sampleRun), "sc:role:engineer"},
		Round:       1,
		PRURL:       "https://github.com/o/r/pull/7",
		PRNumber:    7,
	}
}

// sampleEvents returns one fully populated event per kind. The test suite
// asserts it covers every registered kind, so adding an event type without
// a sample fails loudly here.
func sampleEvents() []Event {
	m := sampleMeta()
	bead := sampleBead()
	return []Event{
		RunStarted{Meta: m, Resumed: true, Info: domain.Run{
			ID: sampleRun, RepoPath: "/srv/repo", StateDir: "/home/u/.local/state/slop-cannon/x/runs/" + string(sampleRun),
			Mode: domain.RunModeBrief, Brief: "add retry", Epic: "sc-epic", StartedAt: sampleTime,
			Config: domain.ConfigSnapshot{Ammo: 10, Safety: true, Cartridge: "claude", Model: "fable", Vibes: []string{"vibes.md"}, ReviewPRs: true},
		}},
		RunPaused{Meta: m, Reason: "cost_cap"},
		RunResumed{Meta: m, Reason: "user"},
		RunStopped{Meta: m, Reason: "SIGINT"},
		RunFinished{Meta: m, Reason: FinishCompleted, Summary: "3 beads delivered", Duration: 90 * time.Minute},
		PlanProduced{Meta: m, Agent: "po-1", Epic: "sc-epic",
			Plan: domain.Plan{Brief: "retry", Beads: []domain.PlanBead{
				{Key: "a", Title: "Add retry", Acceptance: "tests", Type: domain.BeadTypeFeature, Priority: 1, Size: domain.SizeM},
				{Key: "b", Title: "Docs", Acceptance: "README", Priority: 3, Size: domain.SizeS, Deps: []string{"a"}},
			}},
			Created: map[string]domain.BeadID{"a": "sc-1a2b", "b": "sc-2c3d"}},
		BeadCreated{Meta: m, Bead: bead},
		BeadUpdated{Meta: m, Bead: bead, Fields: []string{"round", "pr_url"}},
		BeadStateChanged{Meta: m, Bead: "sc-1a2b", From: domain.BeadStateInProgress, To: domain.BeadStateInReview, Round: 1, Reason: "diff committed"},
		GraphChanged{Meta: m, Epic: "sc-epic", Beads: []domain.Bead{bead, {ID: "sc-0f0f", Title: "Dep", State: domain.BeadStateClosed}}},
		AgentSpawned{Meta: m, Task: TaskImplement, BeadTitle: "Add retry", Round: 1, Attempt: 2, Agent: domain.Agent{
			ID: "eng-sc-1a2b-r1", Role: domain.RoleEngineer, Cartridge: "claude", Model: "fable",
			Bead: "sc-1a2b", State: domain.AgentStateRunning, StartedAt: sampleTime, PID: 4242,
		}},
		AgentOutput{Meta: m, Agent: "eng-sc-1a2b-r1", Stream: StreamStderr, Line: "compiling…"},
		AgentFinished{Meta: m, Agent: "eng-sc-1a2b-r1", Role: domain.RoleEngineer, Bead: "sc-1a2b", Duration: 3 * time.Minute, ExitCode: 0, Produced: domain.MessageImplReport},
		AgentFailed{Meta: m, Agent: "tester-qa-sc-1a2b-r1", Role: domain.RoleTester, Bead: "sc-1a2b", Failure: FailureTimeout, Err: "deadline exceeded", Duration: 30 * time.Minute, ExitCode: -1, Attempt: 1, WillRetry: true},
		RoundStarted{Meta: m, Bead: "sc-1a2b", Round: 2, RoundKind: RoundReview, Lenses: []domain.Lens{domain.LensQA, domain.LensTest}, Commit: "abc1234"},
		VerdictReceived{Meta: m, Bead: "sc-1a2b", Round: 1, Agent: "tester-qa-sc-1a2b-r1", Verdict: domain.Verdict{
			Lens: domain.LensQA, Round: 1, Outcome: domain.VerdictChanges, Summary: "one blocker",
			Items: []domain.FeedbackItem{{ID: "qa-r1-1", Severity: domain.SeverityBlocker, Category: domain.CategoryRules, Title: "No tests", Detail: "retry path untested", File: "retry.go", Line: 12, Suggestion: "add table test", Status: domain.ItemOpen}},
		}},
		ResponseReceived{Meta: m, Bead: "sc-1a2b", Round: 1, Agent: "eng-sc-1a2b-r1",
			Responses: []domain.Response{{ItemID: "qa-r1-1", Action: domain.ActionFixed, Rationale: "added"}},
			Report:    &domain.ImplReport{Summary: "done", FilesChanged: []string{"retry.go"}, HowToVerify: "go test", Risks: []string{"none"}, Questions: []string{"backoff cap?"}, Commit: "abc1234"}},
		ArbitrationReceived{Meta: m, Bead: "sc-1a2b", Round: 3, Agent: "po-1",
			Rulings:    []domain.Arbitration{{ItemID: "ux-r2-1", Ruling: domain.RulingCompromise, Instruction: "rename flag", Rationale: "clarity"}},
			Escalation: &domain.Escalation{Outcome: domain.EscalationApproveWithNotes, Notes: "good enough"}},
		PROpened{Meta: m, Bead: "sc-1a2b", URL: "https://github.com/o/r/pull/7", Number: 7, Branch: "sc/sc-1a2b-add-retry", Base: "main", External: false},
		Merged{Meta: m, Bead: "sc-1a2b", SHA: "deadbeef", Branch: "main", PRNumber: 7, Pushed: true},
		Warning{Meta: m, Source: "gates", Message: "gh not installed; PR gates skipped", Err: "exec: gh: not found", Bead: "sc-1a2b", Agent: "po-1"},
		Error{Meta: m, Source: "delivery", Message: "merge failed", Err: "conflict in retry.go", Bead: "sc-1a2b", Agent: "eng-sc-1a2b-r1"},
	}
}

func TestSamplesCoverEveryKind(t *testing.T) {
	seen := map[Kind]bool{}
	for _, ev := range sampleEvents() {
		if seen[ev.Kind()] {
			t.Errorf("duplicate sample for %s", ev.Kind())
		}
		seen[ev.Kind()] = true
	}
	for _, k := range Kinds() {
		if !seen[k] {
			t.Errorf("no sample for kind %s", k)
		}
	}
	if got, want := len(seen), len(Kinds()); got != want {
		t.Errorf("samples: %d kinds, registry: %d", got, want)
	}
}
