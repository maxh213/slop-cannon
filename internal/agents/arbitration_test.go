package agents_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// arbGoldenDir holds the two ruling-prompt goldens, relative to this package.
const arbGoldenDir = "testdata/arbitration"

// arbGolden compares got with testdata/arbitration/<name>.golden.md. Set
// UPDATE_GOLDEN=1 (make update-golden) to rewrite the file instead.
func arbGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(arbGoldenDir, name+".golden.md")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden %s; run `make update-golden`", path)
	require.Equal(t, string(want), got,
		"prompt differs from %s; run `make update-golden` if the change is intended", path)
}

// arbContext is the run and repository context both ruling prompts are built
// from: fixed values, so the goldens never depend on a temporary directory.
func arbContext() agents.RulingContext {
	return agents.RulingContext{
		RunID:   "r-2026-09-03-01",
		AgentID: "po-1",
		Brief:   "Make the stats command trustworthy: no panics on empty input.",
		Rules: []prompt.Doc{{
			Name:    "rules.md",
			Path:    "/work/fixture/rules.md",
			Content: "- Never disable or delete a test to make the suite pass.\n- Every behaviour change lands with a test.",
		}},
		Vibes: []prompt.Doc{{
			Name:    "vibes.md",
			Path:    "/work/fixture/vibes.md",
			Content: "- Prefer short functions with obvious names.",
		}},
		Repo: prompt.RepoContext{
			Root:                 "/work/fixture",
			DefaultBranch:        "main",
			Stack:                []string{"Go", "Make"},
			CheckCommand:         "make test",
			CheckCommandDetected: true,
			Tree:                 []string{"Makefile", "README.md", "go.mod", "stats/", "stats/stats.go", "stats/stats_test.go"},
			TreeTotal:            6,
			TreeDepth:            prompt.DefaultTreeDepth,
			ReadmeName:           "README.md",
			ReadmeHead:           "# fixture\n\nA tiny Go command-line tool that prints statistics for a list of numbers.",
		},
		Bead: &prompt.BeadContext{
			Bead: domain.Bead{
				ID:          "sc-7a1",
				Title:       "Mean must not panic on an empty slice",
				Description: "stats.Mean divides by len(xs) without checking it.",
				Acceptance:  "- Mean(nil) and Mean([]float64{}) return (0, false)\n- a table test covers both",
				Type:        domain.BeadTypeBug,
				Priority:    domain.PriorityDefault,
				Size:        domain.SizeS,
				State:       domain.BeadStateInReview,
			},
			Epic:     "sc-epic1",
			Worktree: "/state/runs/r-2026-09-03-01/wt/sc-7a1",
			Branch:   "sc/sc-7a1-mean-empty",
			Round:    2,
		},
		History: []prompt.Round{{
			Number: 1,
			Report: &domain.ImplReport{
				Summary:      "Mean returns (0, false) for an empty slice.",
				FilesChanged: []string{"stats/stats.go"},
				HowToVerify:  "make test",
				Commit:       "1f0c9ab",
			},
			Verdicts: []domain.Verdict{{
				Lens:    domain.LensTest,
				Round:   1,
				Outcome: domain.VerdictChanges,
				Summary: "The behaviour change has no test.",
				Items:   []domain.FeedbackItem{arbItem()},
			}},
			Responses: []domain.Response{arbResponse()},
			Diff:      " stats/stats.go | 4 +++-",
		}},
	}
}

// arbItem is the disputed feedback item: a rules-file violation, which no
// ruling may decide in the engineer's favour.
func arbItem() domain.FeedbackItem {
	return domain.FeedbackItem{
		ID:         "test-r1-1",
		Severity:   domain.SeverityBlocker,
		Category:   domain.CategoryRules,
		Title:      "The empty-slice behaviour lands without a test",
		Detail:     "rules.md says every behaviour change lands with a test; stats_test.go still only covers the happy path.",
		File:       "stats/stats_test.go",
		Line:       31,
		Suggestion: "Add two rows to the existing table: nil and an empty slice, both expecting (0, false).",
		Status:     domain.ItemHeld,
	}
}

// arbResponse is the engineer's pushback on that item.
func arbResponse() domain.Response {
	return domain.Response{
		ItemID:    "test-r1-1",
		Action:    domain.ActionPushback,
		Rationale: "The empty case is covered indirectly by TestReport, which feeds an empty file through the command.",
	}
}

// arbDispute is the whole dispute the product owner is fired to settle.
func arbDispute() agents.ItemDispute {
	return agents.ItemDispute{
		Item:           arbItem(),
		Lens:           domain.LensTest,
		Round:          1,
		Response:       arbResponse(),
		HoldReason:     "TestReport asserts the printed output, not Mean's second return value; a rename would keep it green.",
		PushbackRounds: 2,
	}
}

// arbRequest is the arbitration firing used by the golden and the prompt
// assertions.
func arbRequest() agents.ArbitrationRequest {
	return agents.ArbitrationRequest{
		RulingContext:     arbContext(),
		Dispute:           arbDispute(),
		MaxPushbackRounds: 2,
	}
}

// escRequest is the escalation firing used by the golden and the prompt
// assertions.
func escRequest() agents.EscalationRequest {
	ctx := arbContext()
	ctx.AgentID = "po-2"
	return agents.EscalationRequest{
		RulingContext: ctx,
		Round:         5,
		MaxRounds:     5,
		Unresolved: []agents.ItemDispute{
			arbDispute(),
			{
				Item: domain.FeedbackItem{
					ID:       "ux-r4-2",
					Severity: domain.SeverityMinor,
					Category: "naming",
					Title:    "The second return value is called ok but documented as defined",
					Detail:   "Pick one word and use it in both places.",
					File:     "stats/stats.go",
					Line:     12,
					Status:   domain.ItemOpen,
				},
				Lens:  domain.LensUX,
				Round: 4,
				Response: domain.Response{
					ItemID:    "ux-r4-2",
					Action:    domain.ActionWontfix,
					Rationale: "ok is the idiomatic Go name for a comma-ok return.",
				},
				HoldReason: "Then fix the doc comment instead; the two must agree.",
			},
		},
		Notes: []string{"The check command has passed in every round."},
	}
}

// arbResult wraps v in the sc-result block an agent's output carries.
func arbResult(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	require.NoError(t, err)
	return "Ruling below.\n\n```" + structured.Fence + "\n" + string(raw) + "\n```\n"
}

// arbParseError asserts err is a schema-level *structured.ParseError whose
// problems mention want, and that it can be fed back to the model.
func arbParseError(t *testing.T, err error, want string) {
	t.Helper()
	require.Error(t, err)
	pe, ok := structured.AsParseError(err)
	require.True(t, ok, "want a *structured.ParseError, got %T: %v", err, err)
	assert.True(t, errors.Is(err, structured.ErrSchema), "want errors.Is(err, structured.ErrSchema)")
	assert.Contains(t, strings.Join(pe.Problems, "\n"), want)
	assert.NotEmpty(t, pe.Feedback())
}

func TestArbitrationPromptGolden(t *testing.T) {
	p, err := arbRequest().Prompt()
	require.NoError(t, err)
	assert.Equal(t, domain.RolePO, p.Role)
	assert.Equal(t, events.TaskArbitrate, p.Task)
	assert.True(t, p.Fits(), "the fixture prompt should fit the default budget")
	assert.Empty(t, p.Truncations)
	arbGolden(t, "po-arbitrate", p.Text)
}

func TestEscalationPromptGolden(t *testing.T) {
	p, err := escRequest().Prompt()
	require.NoError(t, err)
	assert.Equal(t, domain.RolePO, p.Role)
	assert.Equal(t, events.TaskEscalate, p.Task)
	assert.True(t, p.Fits(), "the fixture prompt should fit the default budget")
	assert.Empty(t, p.Truncations)
	arbGolden(t, "po-escalate", p.Text)
}

func TestArbitrationPromptContents(t *testing.T) {
	req, err := arbRequest().PromptRequest()
	require.NoError(t, err)
	assert.Equal(t, cartridge.ToolReadOnly, req.ToolPolicy, "the product owner rules read-only (P5-06)")
	require.NotNil(t, req.Contract)
	assert.Equal(t, domain.MessageArbitration.String(), req.Contract.Name())

	p, err := prompt.Build(req)
	require.NoError(t, err)
	for _, want := range []string{
		"# Product owner — arbitrate",
		"Task: arbitrate",
		"read_only",
		"## The disputed item",
		"`test-r1-1`",
		"stats/stats_test.go:31",
		"## The engineer's position",
		"TestReport, which feeds an empty file",
		"## The tester's position",
		"a rename would keep it green",
		"the limit is 2, which is why it reached you",
		"## How to rule",
		"`uphold_tester`",
		"`uphold_engineer` is not available to you",
		"Never disable or delete a test", // the rules file, verbatim
		"Prefer short functions",         // the vibes file, verbatim
		"Mean must not panic",            // the bead
		"### Round 1",                    // the round history
		"\"uphold_tester\"",              // the output contract's schema
	} {
		assert.Contains(t, p.Text, want)
	}
	assert.True(t, req.Contract.HasPromptSuffix(p.Text), "the arbitration contract must be appended")
}

func TestEscalationPromptContents(t *testing.T) {
	req, err := escRequest().PromptRequest()
	require.NoError(t, err)
	assert.Equal(t, cartridge.ToolReadOnly, req.ToolPolicy)
	require.NotNil(t, req.Contract)
	assert.Equal(t, domain.MessageEscalation.String(), req.Contract.Name())

	p, err := prompt.Build(req)
	require.NoError(t, err)
	for _, want := range []string{
		"# Product owner — escalate",
		"Task: escalate",
		"read_only",
		"## Why this reached you",
		"Review round 5 of 5 ended without every tester approving",
		"Note: The check command has passed in every round.",
		"## What is still unresolved",
		"2 item(s) still block approval",
		"`test-r1-1`",
		"`ux-r4-2`",
		"raised by the ux tester",
		"This is a rules-file violation: it cannot be shipped with notes.",
		"## How to rule",
		"`approve_with_notes` is not available to you",
		"Never disable or delete a test",
		"\"narrow_scope\"",
	} {
		assert.Contains(t, p.Text, want)
	}
	assert.True(t, req.Contract.HasPromptSuffix(p.Text))
}

func TestEscalationPromptWithoutRulesViolation(t *testing.T) {
	r := escRequest()
	r.Unresolved = r.Unresolved[1:] // drop the rules item
	p, err := r.Prompt()
	require.NoError(t, err)
	assert.Contains(t, p.Text, "1 item(s) still block approval")
	assert.Contains(t, p.Text, "is never approved with notes")
	assert.NotContains(t, p.Text, "is not available to you")
}

func TestEscalationPromptWithoutUnresolvedItems(t *testing.T) {
	r := escRequest()
	r.Unresolved = nil
	p, err := r.Prompt()
	require.NoError(t, err)
	assert.Contains(t, p.Text, "The testers recorded no unresolved items")
}

func TestArbitrationPromptWithoutAnEngineerAnswer(t *testing.T) {
	r := arbRequest()
	r.Dispute.Response = domain.Response{}
	r.Dispute.HoldReason = ""
	r.Dispute.PushbackRounds = 0
	p, err := r.Prompt()
	require.NoError(t, err)
	assert.Contains(t, p.Text, "The engineer has not answered this item in writing")
	assert.Contains(t, p.Text, "without giving a further reason")
	assert.Contains(t, p.Text, "The pushback limit for one item is 2 rounds")
}

func TestRulingPromptRequestRejectsIncompleteRequests(t *testing.T) {
	t.Run("arbitration without an item id", func(t *testing.T) {
		r := arbRequest()
		r.Dispute.Item.ID = "  "
		_, err := r.Prompt()
		require.ErrorIs(t, err, agents.ErrIncompleteRuling)
	})
	t.Run("escalation without a round", func(t *testing.T) {
		r := escRequest()
		r.Round = 0
		_, err := r.Prompt()
		require.ErrorIs(t, err, agents.ErrIncompleteRuling)
	})
}

func TestArbitrationParseAcceptsEveryRuling(t *testing.T) {
	r := arbRequest()
	r.Dispute.Item.Category = "" // not a rules item, so every ruling is open
	for _, ruling := range domain.ArbitrationRulings() {
		t.Run(ruling.String(), func(t *testing.T) {
			want := domain.Arbitration{
				ItemID:    r.Dispute.Item.ID,
				Ruling:    ruling,
				Rationale: "One sentence the losing side can act on.",
			}
			if ruling == domain.RulingCompromise {
				want.Instruction = "Add the nil row to the existing table and leave the empty-slice case to TestReport."
			}
			got, parsed, err := r.Parse(arbResult(t, want))
			require.NoError(t, err)
			assert.Equal(t, want, got)
			assert.NotEmpty(t, parsed.JSON)
			assert.NoError(t, agents.ValidateJSON(domain.MessageArbitration, parsed.JSON))
		})
	}
}

func TestArbitrationParseRejectsBadRulings(t *testing.T) {
	r := arbRequest()
	t.Run("a compromise without an instruction", func(t *testing.T) {
		_, _, err := r.Parse(arbResult(t, map[string]any{"item_id": "test-r1-1", "ruling": "compromise"}))
		arbParseError(t, err, "instruction")
	})
	t.Run("a ruling on another item", func(t *testing.T) {
		_, _, err := r.Parse(arbResult(t, domain.Arbitration{ItemID: "qa-r2-9", Ruling: domain.RulingUpholdTester}))
		arbParseError(t, err, "the disputed item is \"test-r1-1\"")
	})
	t.Run("a rules violation ruled for the engineer", func(t *testing.T) {
		_, _, err := r.Parse(arbResult(t, domain.Arbitration{ItemID: "test-r1-1", Ruling: domain.RulingUpholdEngineer}))
		arbParseError(t, err, "is a \"rules\" violation")
	})
	t.Run("an unknown ruling", func(t *testing.T) {
		_, _, err := r.Parse(arbResult(t, map[string]any{"item_id": "test-r1-1", "ruling": "coin_flip"}))
		arbParseError(t, err, "ruling")
	})
	t.Run("no result block at all", func(t *testing.T) {
		_, _, err := r.Parse("I would rather not decide this one.")
		require.ErrorIs(t, err, structured.ErrMissing)
	})
}

func TestValidateArbitration(t *testing.T) {
	rulesDispute := arbDispute()
	plainDispute := arbDispute()
	plainDispute.Item.Category = "correctness"

	tests := []struct {
		name    string
		dispute agents.ItemDispute
		ruling  domain.Arbitration
		want    string // empty means the ruling is valid
	}{
		{"uphold the tester", rulesDispute, domain.Arbitration{ItemID: "test-r1-1", Ruling: domain.RulingUpholdTester}, ""},
		{"compromise with an instruction", rulesDispute,
			domain.Arbitration{ItemID: "test-r1-1", Ruling: domain.RulingCompromise, Instruction: "Add the nil row only."}, ""},
		{"uphold the engineer on a plain item", plainDispute,
			domain.Arbitration{ItemID: "test-r1-1", Ruling: domain.RulingUpholdEngineer}, ""},
		{"uphold the engineer on a rules item", rulesDispute,
			domain.Arbitration{ItemID: "test-r1-1", Ruling: domain.RulingUpholdEngineer}, "hard constraint"},
		{"compromise without an instruction", plainDispute,
			domain.Arbitration{ItemID: "test-r1-1", Ruling: domain.RulingCompromise}, "a compromise needs the instruction"},
		{"no item id", plainDispute, domain.Arbitration{Ruling: domain.RulingUpholdTester}, "must name the item"},
		{"another item's id", plainDispute,
			domain.Arbitration{ItemID: "qa-r1-2", Ruling: domain.RulingUpholdTester}, "the disputed item is"},
		{"an unknown ruling", plainDispute, domain.Arbitration{ItemID: "test-r1-1", Ruling: "coin_flip"}, "is not one of"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := agents.ValidateArbitration(tc.ruling, tc.dispute)
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			arbParseError(t, err, tc.want)
		})
	}
}

func TestEscalationParseAcceptsEveryOutcome(t *testing.T) {
	r := escRequest()
	r.Unresolved = r.Unresolved[1:] // no rules violation, so notes are allowed
	cases := map[domain.EscalationOutcome]domain.Escalation{
		domain.EscalationApproveWithNotes: {Outcome: domain.EscalationApproveWithNotes, Notes: "The naming objection is unresolved and tracked as a follow-up."},
		domain.EscalationNarrowScope:      {Outcome: domain.EscalationNarrowScope, NewAcceptance: "Mean(nil) returns (0, false); the naming clean-up moves to a new bead."},
		domain.EscalationBlock:            {Outcome: domain.EscalationBlock, Summary: "The ux and test lenses disagree about the API; a human should choose."},
	}
	for _, outcome := range domain.EscalationOutcomes() {
		t.Run(outcome.String(), func(t *testing.T) {
			want := cases[outcome]
			got, parsed, err := r.Parse(arbResult(t, want))
			require.NoError(t, err)
			assert.Equal(t, want, got)
			assert.NoError(t, agents.ValidateJSON(domain.MessageEscalation, parsed.JSON))
		})
	}
}

func TestEscalationParseRejectsBadRulings(t *testing.T) {
	r := escRequest()
	t.Run("approve with notes over a rules violation", func(t *testing.T) {
		_, _, err := r.Parse(arbResult(t, domain.Escalation{
			Outcome: domain.EscalationApproveWithNotes,
			Notes:   "The missing test is a nit and the code works.",
		}))
		arbParseError(t, err, "still violates a rules file")
	})
	t.Run("narrow scope without new acceptance", func(t *testing.T) {
		_, _, err := r.Parse(arbResult(t, map[string]any{"outcome": "narrow_scope"}))
		arbParseError(t, err, "new_acceptance")
	})
	t.Run("block without a summary", func(t *testing.T) {
		_, _, err := r.Parse(arbResult(t, map[string]any{"outcome": "block"}))
		arbParseError(t, err, "summary")
	})
}

func TestValidateEscalation(t *testing.T) {
	rules := []agents.ItemDispute{arbDispute()}
	plain := []agents.ItemDispute{{Item: domain.FeedbackItem{ID: "ux-r4-2", Category: "naming"}}}

	tests := []struct {
		name       string
		unresolved []agents.ItemDispute
		ruling     domain.Escalation
		want       string
	}{
		{"approve with notes", plain, domain.Escalation{Outcome: domain.EscalationApproveWithNotes, Notes: "Naming is unresolved."}, ""},
		{"approve with notes over a rules item", rules,
			domain.Escalation{Outcome: domain.EscalationApproveWithNotes, Notes: "Naming is unresolved."}, "`test-r1-1` still violates a rules file"},
		{"approve without notes", plain, domain.Escalation{Outcome: domain.EscalationApproveWithNotes}, "must record what is left"},
		{"narrow scope", rules, domain.Escalation{Outcome: domain.EscalationNarrowScope, NewAcceptance: "Mean(nil) returns (0, false)."}, ""},
		{"narrow scope without acceptance", rules, domain.Escalation{Outcome: domain.EscalationNarrowScope}, "new_acceptance"},
		{"block", rules, domain.Escalation{Outcome: domain.EscalationBlock, Summary: "A human must choose the API."}, ""},
		{"block without a summary", rules, domain.Escalation{Outcome: domain.EscalationBlock}, "summarise the situation"},
		{"an unknown outcome", plain, domain.Escalation{Outcome: "shrug"}, "is not one of"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := agents.ValidateEscalation(tc.ruling, tc.unresolved)
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			arbParseError(t, err, tc.want)
		})
	}
}

func TestPublishedExamplesSurviveTheRulingValidators(t *testing.T) {
	// The docs examples are the schemas' own goldens (P5-07). They must also
	// pass the semantic checks this file adds, with the dispute they describe.
	dispute := func(id, category string) agents.ItemDispute {
		return agents.ItemDispute{Item: domain.FeedbackItem{ID: id, Category: category}}
	}
	arbitrations := map[string]agents.ItemDispute{
		"arbitration-uphold-tester.json": dispute("test-r1-1", domain.CategoryRules),
		"arbitration-compromise.json":    dispute("test-r1-2", "coverage"),
	}
	for file, d := range arbitrations {
		t.Run(file, func(t *testing.T) {
			var a domain.Arbitration
			arbReadExample(t, file, &a)
			assert.NoError(t, agents.ValidateArbitration(a, d))
		})
	}
	escalations := map[string][]agents.ItemDispute{
		"escalation-approve-with-notes.json": {dispute("ux-r4-2", "naming")},
		"escalation-narrow-scope.json":       {dispute("qa-r3-1", "scope")},
		"escalation-block.json":              {dispute("ux-r2-1", domain.CategoryRules)},
	}
	for file, unresolved := range escalations {
		t.Run(file, func(t *testing.T) {
			var e domain.Escalation
			arbReadExample(t, file, &e)
			assert.NoError(t, agents.ValidateEscalation(e, unresolved))
		})
	}
}

// arbReadExample decodes one published valid example into v.
func arbReadExample(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../../docs/schemas/examples/valid", name))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, v))
}

func TestArbitrationItemStatus(t *testing.T) {
	tests := map[domain.ArbitrationRuling]domain.ItemStatus{
		domain.RulingUpholdTester:   domain.ItemOpen,
		domain.RulingCompromise:     domain.ItemOpen,
		domain.RulingUpholdEngineer: domain.ItemOverruled,
	}
	for _, ruling := range domain.ArbitrationRulings() {
		t.Run(ruling.String(), func(t *testing.T) {
			got, err := agents.ArbitrationItemStatus(ruling)
			require.NoError(t, err)
			assert.Equal(t, tests[ruling], got)
			assert.Equal(t, ruling == domain.RulingUpholdEngineer, got.Settled())
		})
	}
	_, err := agents.ArbitrationItemStatus("coin_flip")
	require.ErrorIs(t, err, domain.ErrUnknownValue)
}

func TestEscalationBeadState(t *testing.T) {
	tests := map[domain.EscalationOutcome]domain.BeadState{
		domain.EscalationApproveWithNotes: domain.BeadStateApproved,
		domain.EscalationNarrowScope:      domain.BeadStateInReview,
		domain.EscalationBlock:            domain.BeadStateNeedsHuman,
	}
	for _, outcome := range domain.EscalationOutcomes() {
		t.Run(outcome.String(), func(t *testing.T) {
			got, err := agents.EscalationBeadState(outcome)
			require.NoError(t, err)
			assert.Equal(t, tests[outcome], got)
			assert.True(t, got.Valid())
		})
	}
	_, err := agents.EscalationBeadState("shrug")
	require.ErrorIs(t, err, domain.ErrUnknownValue)
}

func TestArbitrationDirective(t *testing.T) {
	d := arbDispute()
	tests := []struct {
		name   string
		ruling domain.Arbitration
		want   []string
	}{
		{"uphold the tester", domain.Arbitration{ItemID: "test-r1-1", Ruling: domain.RulingUpholdTester, Rationale: "A rules file is a hard constraint"},
			[]string{"upheld the tester on `test-r1-1`", "address it as raised", "hard constraint."}},
		{"uphold the engineer", domain.Arbitration{ItemID: "test-r1-1", Ruling: domain.RulingUpholdEngineer},
			[]string{"overruled `test-r1-1`", "no longer blocks"}},
		{"compromise", domain.Arbitration{ItemID: "test-r1-1", Ruling: domain.RulingCompromise, Instruction: "Add the nil row only"},
			[]string{"compromise on `test-r1-1`", "Add the nil row only."}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := agents.ArbitrationDirective(tc.ruling, d)
			for _, want := range tc.want {
				assert.Contains(t, got, want)
			}
			assert.Contains(t, got, "The empty-slice behaviour lands without a test")
		})
	}
	assert.Empty(t, agents.ArbitrationDirective(domain.Arbitration{ItemID: "x", Ruling: "coin_flip"}, d))
}

func TestArbitrationThroughTheMockCartridge(t *testing.T) {
	r := arbRequest()
	p, err := r.Prompt()
	require.NoError(t, err)

	ruling := domain.Arbitration{
		ItemID:      r.Dispute.Item.ID,
		Ruling:      domain.RulingCompromise,
		Instruction: "Add the nil row to the existing table; the empty-slice row can wait for the reporting bead.",
		Rationale:   "The rules file wins, but one row is enough to cover the behaviour that changed.",
	}
	raw, err := json.Marshal(ruling)
	require.NoError(t, err)

	scenario := mock.NewScenario("arbitration")
	rule := scenario.Rule("rule-on-the-item", mock.Match{Task: events.TaskArbitrate.String(), PromptContains: "test-r1-1"})
	rule.Steps = []mock.Step{{Kind: mock.StepText, Text: "Reading stats_test.go before I rule."}}
	rule.Result = mock.ResultSpec{Structured: raw}
	adapter, err := mock.NewAdapter("mock", scenario)
	require.NoError(t, err)

	spec := cartridge.RunSpec{
		Prompt:  p.Text,
		Workdir: t.TempDir(),
		Labels: cartridge.LabelsFor(r.RunID, domain.Agent{
			ID: r.AgentID, Role: domain.RolePO, Bead: r.Bead.Bead.ID,
		}, events.TaskArbitrate, 2),
	}
	require.NoError(t, agents.ApplyPolicy(&spec, domain.RolePO, events.TaskArbitrate))
	assert.Equal(t, cartridge.ToolReadOnly, spec.ToolPolicy)

	contract, err := agents.ContractFor(domain.MessageArbitration)
	require.NoError(t, err)
	out, err := structured.Fire(context.Background(), adapter, spec, contract, structured.WithoutPromptSuffix())
	require.NoError(t, err)

	got, _, err := r.Parse(out.Result.Output)
	require.NoError(t, err)
	assert.Equal(t, ruling, got)

	status, err := agents.ArbitrationItemStatus(got.Ruling)
	require.NoError(t, err)
	assert.Equal(t, domain.ItemOpen, status)
	assert.Contains(t, agents.ArbitrationDirective(got, r.Dispute), "Add the nil row")

	require.Equal(t, 1, adapter.Uses("rule-on-the-item"))
	call, ok := adapter.LastCall()
	require.True(t, ok)
	assert.Contains(t, call.Spec.Prompt, "Answer with exactly one `arbitration` result")
}
