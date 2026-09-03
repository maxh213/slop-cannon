package agents_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/domain"
)

// The budget is reported by prompt.Build and acted on by every firing
// builder, not just the tester's. Rules, vibes and the output contract are
// never truncated by design (P5-01), so a large rules file alone can push a
// planning or an implementing prompt past its budget; when it does, the
// caller is told rather than handed a prompt nobody measured.

// pbtHugeRules is a rules file too large for any small budget. Rules are
// pasted verbatim, so the builder cannot degrade its way under.
func pbtHugeRules() []prompt.Doc {
	return []prompt.Doc{{
		Name:    "rules.md",
		Path:    "/work/fixture/rules.md",
		Content: strings.Repeat("- Never disable or delete a test to make the suite pass.\n", 400),
	}}
}

// pbtPlanRequest is a valid planning firing with an unsatisfiable budget.
func pbtPlanRequest(t *testing.T) agents.PlanRequest {
	t.Helper()
	return agents.PlanRequest{
		Run:     "20260902-101500-1a2b",
		AgentID: "po-1",
		Mode:    domain.RunModeAutopilot,
		Repo:    prompt.RepoContext{Root: t.TempDir(), DefaultBranch: "main"},
		Rules:   pbtHugeRules(),
		Budget:  prompt.Budget{MaxTokens: 200},
	}
}

// pbtEngineerRequest is a valid implement firing with the same problem.
func pbtEngineerRequest(t *testing.T) agents.EngineerRequest {
	t.Helper()
	dir := t.TempDir()
	return agents.EngineerRequest{
		Mode:    agents.EngineerImplement,
		RunID:   "20260902-101500-1a2b",
		AgentID: "eng-sc-7a1-r1",
		Bead: prompt.BeadContext{
			Bead:     domain.Bead{ID: "sc-7a1", Title: "Implement Median in stats"},
			Worktree: dir,
			Branch:   "sc/sc-7a1-implement-median",
		},
		Repo:   prompt.RepoContext{Root: dir, DefaultBranch: "main", CheckCommand: "make test"},
		Rules:  pbtHugeRules(),
		Budget: prompt.Budget{MaxTokens: 200},
	}
}

// TestPlanRunSpecReportsAnOverBudgetPrompt: the planning firing reports the
// budget the way TesterRunSpec does — the valid spec, the prompt and an
// ErrPromptOverBudget error together.
func TestPlanRunSpecReportsAnOverBudgetPrompt(t *testing.T) {
	t.Parallel()
	req := pbtPlanRequest(t)

	p, err := agents.PlanPrompt(req)
	require.NoError(t, err)
	require.False(t, p.Fits(), "the fixture must be over budget for this test to mean anything")

	spec, p, err := agents.PlanRunSpec(req)
	require.Error(t, err)
	assert.ErrorIs(t, err, agents.ErrPromptOverBudget)
	assert.Contains(t, err.Error(), "planning")
	assert.Contains(t, err.Error(), "200")
	assert.NoError(t, spec.Validate(), "the spec is still usable; the caller decides whether to fire it")
	assert.Equal(t, p.Text, spec.Prompt)
	assert.Contains(t, p.Text, "Never disable or delete a test", "the rules are never truncated")
}

// TestEngineerFiringReportsAnOverBudgetPrompt is the same for the engineer,
// through both Firing and RunSpec.
func TestEngineerFiringReportsAnOverBudgetPrompt(t *testing.T) {
	t.Parallel()
	req := pbtEngineerRequest(t)

	p, err := req.Prompt()
	require.NoError(t, err)
	require.False(t, p.Fits(), "the fixture must be over budget for this test to mean anything")

	f, err := req.Firing()
	require.Error(t, err)
	assert.ErrorIs(t, err, agents.ErrPromptOverBudget)
	assert.Contains(t, err.Error(), agents.EngineerImplement.String())
	assert.NoError(t, f.Spec.Validate(), "the firing is complete; only the budget is blown")
	assert.Equal(t, f.Prompt.Text, f.Spec.Prompt)
	assert.NotNil(t, f.Contract, "the contract still reached the prompt")

	spec, err := req.RunSpec()
	require.Error(t, err)
	assert.ErrorIs(t, err, agents.ErrPromptOverBudget)
	assert.NoError(t, spec.Validate())
}

// TestFiringsWithinBudgetReportNothing keeps the signal honest: the same
// firings with the default budget come back clean.
func TestFiringsWithinBudgetReportNothing(t *testing.T) {
	t.Parallel()
	plan := pbtPlanRequest(t)
	plan.Rules, plan.Budget = nil, prompt.Budget{}
	_, p, err := agents.PlanRunSpec(plan)
	require.NoError(t, err)
	assert.True(t, p.Fits())

	eng := pbtEngineerRequest(t)
	eng.Rules, eng.Budget = nil, prompt.Budget{}
	f, err := eng.Firing()
	require.NoError(t, err)
	assert.True(t, f.Prompt.Fits())
}
