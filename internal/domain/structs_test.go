package domain

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// roundTrip marshals v, unmarshals into a fresh T and asserts equality, then
// returns the JSON so callers can check the wire shape.
func roundTrip[T any](t *testing.T, v T) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	var back T
	require.NoError(t, json.Unmarshal(data, &back))
	assert.Equal(t, v, back, "round trip")
	var shape map[string]any
	require.NoError(t, json.Unmarshal(data, &shape))
	return shape
}

var fixedTime = time.Date(2026, 9, 2, 15, 4, 5, 0, time.UTC)

func sampleBead() Bead {
	return Bead{
		ID:          "sc-68s",
		Title:       "Fix the off-by-one in pagination",
		Description: "The last page is never shown.",
		Acceptance:  "All pages render; test added.",
		Type:        BeadTypeBug,
		Priority:    PriorityHighest,
		Size:        SizeS,
		Deps:        []BeadID{"sc-a1", "sc-b2"},
		State:       BeadStateInReview,
		Assignee:    "sc:eng-sc-68s-r1",
		Labels:      []string{"sc:run:r1", "sc:role:po"},
		Round:       2,
		PRURL:       "https://example.com/pr/7",
		PRNumber:    7,
	}
}

func sampleVerdict() Verdict {
	return Verdict{
		Lens:    LensQA,
		Round:   1,
		Outcome: VerdictChanges,
		Summary: "One real bug, one nit.",
		Items: []FeedbackItem{
			{ID: "qa-r1-1", Severity: SeverityBlocker, Category: CategoryRules, Title: "Writes to main", Detail: "Rules forbid it.", File: "cmd/x.go", Line: 12, Suggestion: "Use a branch.", Status: ItemOpen},
			{ID: "qa-r1-2", Severity: SeverityNit, Title: "Typo", Status: ItemResolved},
		},
	}
}

func samplePlan() Plan {
	return Plan{
		Brief: "Make pagination reliable.",
		Beads: []PlanBead{
			{Key: "a", Title: "Add test", Acceptance: "Test fails on current code.", Type: BeadTypeChore, Priority: PriorityDefault, Size: SizeS},
			{Key: "b", Title: "Fix bug", Description: "Off by one.", Acceptance: "Test passes.", Type: BeadTypeBug, Priority: PriorityHighest, Size: SizeM, Deps: []string{"a"}},
		},
	}
}

func TestBeadJSON(t *testing.T) {
	shape := roundTrip(t, sampleBead())
	assert.Equal(t, "sc-68s", shape["id"])
	assert.Equal(t, "in_review", shape["state"])
	assert.Equal(t, "bug", shape["type"])
	assert.Equal(t, float64(0), shape["priority"])
	assert.Equal(t, "s", shape["size"])
	assert.Equal(t, "https://example.com/pr/7", shape["pr_url"])
	assert.Equal(t, float64(7), shape["pr_number"])
	assert.Equal(t, []any{"sc-a1", "sc-b2"}, shape["deps"])

	t.Run("minimal", func(t *testing.T) {
		shape := roundTrip(t, Bead{ID: "sc-1", Title: "t", State: BeadStateOpen})
		assert.NotContains(t, shape, "pr_url")
		assert.NotContains(t, shape, "size")
		assert.NotContains(t, shape, "type")
		assert.Contains(t, shape, "priority")
		assert.Contains(t, shape, "round")
	})

	t.Run("rejects unknown state", func(t *testing.T) {
		var b Bead
		err := json.Unmarshal([]byte(`{"id":"sc-1","state":"limbo"}`), &b)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUnknownValue)
	})
}

func TestAgentJSON(t *testing.T) {
	a := Agent{
		ID: "tester-qa-sc-68s-r1", Role: RoleTester, Lens: LensQA,
		Cartridge: "claude", Model: "fable", Bead: "sc-68s",
		State: AgentStateRunning, StartedAt: fixedTime, PID: 4242,
	}
	shape := roundTrip(t, a)
	assert.Equal(t, "tester", shape["role"])
	assert.Equal(t, "qa", shape["lens"])
	assert.Equal(t, "running", shape["state"])
	assert.Equal(t, "2026-09-02T15:04:05Z", shape["started_at"])
	assert.Equal(t, float64(4242), shape["pid"])

	t.Run("engineer has no lens", func(t *testing.T) {
		shape := roundTrip(t, Agent{ID: "eng-1", Role: RoleEngineer, Cartridge: "claude", State: AgentStatePending})
		assert.NotContains(t, shape, "lens")
		assert.NotContains(t, shape, "pid")
		assert.NotContains(t, shape, "bead")
	})

	t.Run("explicit empty lens decodes to zero", func(t *testing.T) {
		var a Agent
		require.NoError(t, json.Unmarshal([]byte(`{"id":"eng-1","role":"engineer","lens":"","state":"pending"}`), &a))
		assert.Equal(t, Lens(""), a.Lens)
		assert.Equal(t, RoleEngineer, a.Role)
	})
}

func TestRunJSON(t *testing.T) {
	r := Run{
		ID: "20260902-150405-abcd", RepoPath: "/srv/repo", StateDir: "/state/repo/runs/x",
		Mode: RunModeBrief, Brief: "Ship pagination fix.", Epic: "sc-epic1", StartedAt: fixedTime,
		Config: ConfigSnapshot{
			Ammo: 10, Safety: true, Cartridge: "claude", Model: "fable",
			Vibes: []string{"vibes.md"}, Rules: []string{"rules.md"}, ReviewPRs: true, Headless: false,
		},
	}
	shape := roundTrip(t, r)
	assert.Equal(t, "brief", shape["mode"])
	assert.Equal(t, "/srv/repo", shape["repo_path"])
	cfg, ok := shape["config"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(10), cfg["ammo"])
	assert.Equal(t, true, cfg["safety"])
	assert.Equal(t, []any{"vibes.md"}, cfg["vibes"])
	assert.Contains(t, cfg, "review_prs")
	assert.Contains(t, cfg, "headless")

	t.Run("autopilot", func(t *testing.T) {
		shape := roundTrip(t, Run{ID: "r2", RepoPath: "/r", Mode: RunModeAutopilot, StartedAt: fixedTime, Config: ConfigSnapshot{Ammo: 3, Cartridge: "claude"}})
		assert.Equal(t, "autopilot", shape["mode"])
		assert.NotContains(t, shape, "brief")
		assert.NotContains(t, shape, "epic")
	})
}

func TestVerdictJSON(t *testing.T) {
	shape := roundTrip(t, sampleVerdict())
	assert.Equal(t, "changes", shape["outcome"])
	assert.Equal(t, "qa", shape["lens"])
	items, ok := shape["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, 2)
	first, ok := items[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "blocker", first["severity"])
	assert.Equal(t, "rules", first["category"])
	assert.Equal(t, float64(12), first["line"])
	assert.Equal(t, "open", first["status"])
	second, ok := items[1].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, second, "line")
	assert.NotContains(t, second, "file")

	t.Run("clean approval", func(t *testing.T) {
		shape := roundTrip(t, Verdict{Outcome: VerdictApprove})
		assert.Equal(t, map[string]any{"outcome": "approve"}, shape)
	})
}

func TestFeedbackItemJSON(t *testing.T) {
	item := FeedbackItem{ID: "ux-r2-1", Severity: SeverityMajor, Title: "Confusing error", Detail: "Says 'oops'.", File: "main.go", Line: 3, Suggestion: "Explain what to do.", Status: ItemHeld}
	shape := roundTrip(t, item)
	assert.Equal(t, "major", shape["severity"])
	assert.Equal(t, "held", shape["status"])
	assert.Equal(t, "main.go", shape["file"])
}

func TestResponseJSON(t *testing.T) {
	for _, r := range []Response{
		{ItemID: "qa-r1-1", Action: ActionFixed},
		{ItemID: "qa-r1-2", Action: ActionPushback, Rationale: "Intentional; matches the spec."},
		{ItemID: "qa-r1-3", Action: ActionWontfix, Rationale: "Out of scope for this bead."},
	} {
		shape := roundTrip(t, r)
		assert.Equal(t, r.Action.String(), shape["action"])
		assert.Equal(t, r.ItemID, shape["item_id"])
	}
	t.Run("list", func(t *testing.T) {
		list := []Response{{ItemID: "a", Action: ActionFixed}, {ItemID: "b", Action: ActionWontfix, Rationale: "no"}}
		data, err := json.Marshal(list)
		require.NoError(t, err)
		var back []Response
		require.NoError(t, json.Unmarshal(data, &back))
		assert.Equal(t, list, back)
	})
}

func TestArbitrationJSON(t *testing.T) {
	a := Arbitration{ItemID: "qa-r1-2", Ruling: RulingCompromise, Instruction: "Keep the behaviour, add a doc note.", Rationale: "Both have a point."}
	shape := roundTrip(t, a)
	assert.Equal(t, "compromise", shape["ruling"])
	assert.Equal(t, "qa-r1-2", shape["item_id"])
	shape = roundTrip(t, Arbitration{ItemID: "x", Ruling: RulingUpholdTester})
	assert.NotContains(t, shape, "instruction")
}

func TestEscalationJSON(t *testing.T) {
	for _, e := range []Escalation{
		{Outcome: EscalationApproveWithNotes, Notes: "Good enough; follow-up filed."},
		{Outcome: EscalationNarrowScope, NewAcceptance: "Only the CLI path must work."},
		{Outcome: EscalationBlock, Summary: "Testers and engineer disagree on the spec."},
	} {
		shape := roundTrip(t, e)
		assert.Equal(t, e.Outcome.String(), shape["outcome"])
	}
}

func TestImplReportJSON(t *testing.T) {
	r := ImplReport{
		Summary:      "Fixed the off-by-one and added a regression test.",
		FilesChanged: []string{"internal/page/page.go", "internal/page/page_test.go"},
		HowToVerify:  "go test ./internal/page/...",
		Risks:        []string{"Pagination size is now inclusive."},
		Questions:    []string{"Should page size be configurable?"},
		Commit:       "deadbeef",
	}
	shape := roundTrip(t, r)
	assert.Equal(t, "go test ./internal/page/...", shape["how_to_verify"])
	assert.Len(t, shape["files_changed"], 2)
	shape = roundTrip(t, ImplReport{Summary: "Nothing to do."})
	assert.Equal(t, map[string]any{"summary": "Nothing to do."}, shape)
}

func TestPlanJSON(t *testing.T) {
	shape := roundTrip(t, samplePlan())
	beads, ok := shape["beads"].([]any)
	require.True(t, ok)
	require.Len(t, beads, 2)
	second, ok := beads[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "b", second["key"])
	assert.Equal(t, []any{"a"}, second["deps"])
	assert.Equal(t, "bug", second["type"])
	assert.Equal(t, float64(0), second["priority"])
	assert.Equal(t, "m", second["size"])

	t.Run("empty plan keeps beads key", func(t *testing.T) {
		data, err := json.Marshal(Plan{})
		require.NoError(t, err)
		assert.JSONEq(t, `{"beads":null}`, string(data))
	})
}

func TestMessageEnvelope(t *testing.T) {
	// The D-06 comment shape: a struct with a "type" tag alongside the payload.
	type envelope struct {
		Type    MessageType `json:"type"`
		Verdict *Verdict    `json:"verdict,omitempty"`
	}
	v := sampleVerdict()
	shape := roundTrip(t, envelope{Type: MessageVerdict, Verdict: &v})
	assert.Equal(t, "verdict", shape["type"])
}
