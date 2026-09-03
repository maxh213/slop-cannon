package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// pbGoldenDir is where the prompt goldens live, relative to this package.
const pbGoldenDir = "../../../testdata/prompts"

// pbGolden compares got with testdata/prompts/<name>.golden.md. Set
// UPDATE_GOLDEN=1 (make update-golden) to rewrite the file instead.
func pbGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(pbGoldenDir, name+".golden.md")
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

// pbContract is a tiny stand-in for a real message contract. The goldens use
// it rather than one of the agents package's schemas so that a change to a
// message schema does not rewrite every prompt golden; TestBuildRealContract
// covers the real ones.
var pbContract = structured.MustContract("demo_result", []byte(
	`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`))

// pbRepo is a fixed repository context: hand-built rather than scanned, so
// the goldens do not depend on a temporary directory.
func pbRepo() RepoContext {
	return RepoContext{
		Root:                 "/work/fixture",
		DefaultBranch:        "main",
		Stack:                []string{"Go", "Make"},
		CheckCommand:         "make test",
		CheckCommandDetected: true,
		Tree: []string{
			"Makefile", "README.md", "go.mod", "main.go",
			"stats/", "stats/stats.go", "stats/stats_test.go",
		},
		TreeTotal:  7,
		TreeDepth:  DefaultTreeDepth,
		ReadmeName: "README.md",
		ReadmeHead: "# fixture\n\nA tiny Go command-line tool that prints statistics for a list of numbers.",
	}
}

// pbRules and pbVibes are the configured rules and vibes files.
func pbRules() []Doc {
	return []Doc{{
		Name: "rules.md",
		Path: "/work/fixture/rules.md",
		Content: "- Never disable or delete a test to make the suite pass.\n" +
			"- Every exported symbol has a doc comment.",
	}}
}

func pbVibes() []Doc {
	return []Doc{{
		Name:    "vibes.md",
		Path:    "/work/fixture/vibes.md",
		Content: "- Prefer short functions with obvious names.\n- British spelling in prose.",
	}}
}

// pbBead is the bead an engineer or tester works on.
func pbBead() *BeadContext {
	return &BeadContext{
		Bead: domain.Bead{
			ID:          "sc-7a1",
			Title:       "Implement Median in stats",
			Description: "README promises a median but stats has none.",
			Acceptance:  "- stats.Median returns the middle value\n- table test covers even and odd lengths",
			Type:        domain.BeadTypeFeature,
			Priority:    domain.PriorityDefault,
			Size:        domain.SizeS,
			State:       domain.BeadStateInProgress,
			Deps:        []domain.BeadID{"sc-7a0"},
		},
		Epic:     "sc-epic1",
		Worktree: "/state/runs/r1/wt/sc-7a1",
		Branch:   "sc/sc-7a1-implement-median",
		Round:    2,
	}
}

// pbHistory is one completed review round.
func pbHistory() []Round {
	return []Round{{
		Number: 1,
		Report: &domain.ImplReport{
			Summary:      "Added stats.Median with a table test.",
			FilesChanged: []string{"stats/stats.go", "stats/stats_test.go"},
			HowToVerify:  "make test",
			Risks:        []string{"Even-length behaviour is the mean of the middle pair."},
			Commit:       "1f0c9ab",
		},
		Verdicts: []domain.Verdict{{
			Lens:    domain.LensQA,
			Round:   1,
			Outcome: domain.VerdictChanges,
			Summary: "Sorting mutates the caller's slice.",
			Items: []domain.FeedbackItem{{
				ID:         "qa-r1-1",
				Severity:   domain.SeverityMajor,
				Category:   "correctness",
				Title:      "Median sorts the input in place",
				Detail:     "Callers see their slice reordered.",
				File:       "stats/stats.go",
				Line:       42,
				Suggestion: "Copy the slice before sorting.",
				Status:     domain.ItemOpen,
			}},
		}},
		Responses: []domain.Response{{
			ItemID:    "qa-r1-1",
			Action:    domain.ActionFixed,
			Rationale: "Median now sorts a copy.",
		}},
		Diff: "@@ stats/stats.go @@\n+func Median(xs []float64) float64 {",
	}}
}

// pbRequest is the shared fixture request for role.
func pbRequest(role domain.Role) Request {
	return Request{
		Role:       role,
		RunID:      "20260902-101500-1a2b",
		Repo:       pbRepo(),
		Rules:      pbRules(),
		Vibes:      pbVibes(),
		Contract:   pbContract,
		ToolPolicy: cartridge.ToolReadWrite,
		AgentID:    "agent-1",
	}
}

// pbPORequest, pbEngineerRequest and pbTesterRequest are the three role
// fixtures the goldens are built from.
func pbPORequest() Request {
	req := pbRequest(domain.RolePO)
	req.Task = events.TaskPlan
	req.AgentID = "po-1"
	req.ToolPolicy = cartridge.ToolReadOnly
	req.Objective = "Plan the next slice of work for this repository."
	req.Brief = "Finish the statistics library and make the CLI pleasant to use."
	req.OpenBeads = []domain.Bead{
		{ID: "sc-7a0", Title: "Add a --json flag", State: domain.BeadStateOpen},
		{ID: "sc-7a1", Title: "Implement Median in stats", State: domain.BeadStateInProgress},
	}
	req.Instructions = []Section{{
		Title: "What to produce",
		Body:  "At most 8 beads. Order them with dependencies, not prose.",
	}}
	return req
}

func pbEngineerRequest() Request {
	req := pbRequest(domain.RoleEngineer)
	req.Task = events.TaskImplement
	req.AgentID = "eng-sc-7a1-r2"
	req.Objective = "Implement the bead below and leave the check command green."
	req.Bead = pbBead()
	req.History = pbHistory()
	req.Instructions = []Section{{
		Title: "How to finish",
		Body:  "Run `make test`, commit, then report what you did.",
	}}
	return req
}

func pbTesterRequest(lens domain.Lens) Request {
	req := pbRequest(domain.RoleTester)
	req.Lens = lens
	req.Task = events.TaskReview
	req.AgentID = "tester-" + domain.AgentID(lens) + "-sc-7a1-r2"
	req.ToolPolicy = cartridge.ToolReadExec
	req.Objective = "Review round 2 of this bead through your lens."
	req.Bead = pbBead()
	req.History = pbHistory()
	req.Instructions = []Section{{
		Title: "How to review",
		Body:  "Read the diff, run what you can, then give one verdict.",
	}}
	return req
}

func TestBuildGoldenPO(t *testing.T) {
	p, err := Build(pbPORequest())
	require.NoError(t, err)
	assert.False(t, p.OverBudget)
	assert.Empty(t, p.Truncations)
	pbGolden(t, "po-plan", p.Text)
}

func TestBuildGoldenEngineer(t *testing.T) {
	p, err := Build(pbEngineerRequest())
	require.NoError(t, err)
	assert.False(t, p.OverBudget)
	pbGolden(t, "engineer-implement", p.Text)
}

func TestBuildGoldenTester(t *testing.T) {
	p, err := Build(pbTesterRequest(domain.LensQA))
	require.NoError(t, err)
	assert.False(t, p.OverBudget)
	pbGolden(t, "tester-qa-review", p.Text)
}

// TestBuildRulesAndVibesInEveryRole is the P5-01 acceptance test: rules and
// vibes reach all three roles, verbatim and correctly framed, and so does
// the read-only beads note (D-21, P3-09).
func TestBuildRulesAndVibesInEveryRole(t *testing.T) {
	requests := map[string]Request{
		"po":       pbPORequest(),
		"engineer": pbEngineerRequest(),
		"tester":   pbTesterRequest(domain.LensUX),
	}
	for name, req := range requests {
		t.Run(name, func(t *testing.T) {
			p, err := Build(req)
			require.NoError(t, err)

			assert.Contains(t, p.Text, "## Rules (MUST)")
			assert.Contains(t, p.Text, "### rules.md")
			for _, line := range strings.Split(pbRules()[0].Content, "\n") {
				assert.Contains(t, p.Text, line)
			}
			assert.Contains(t, p.Text, "## Vibes (preferences)")
			assert.Contains(t, p.Text, "### vibes.md")
			for _, line := range strings.Split(pbVibes()[0].Content, "\n") {
				assert.Contains(t, p.Text, line)
			}
			assert.Contains(t, p.Text, beads.PromptNote)
			assert.True(t, pbContract.HasPromptSuffix(p.Text), "output contract missing")
			assert.Contains(t, p.Text, "## Repository context")
			assert.Contains(t, p.Text, "make test")
		})
	}
}

// TestBuildOmitsEmptyLayers keeps a prompt free of headings with nothing
// under them.
func TestBuildOmitsEmptyLayers(t *testing.T) {
	req := Request{Role: domain.RolePO, AgentID: "po-1", ToolPolicy: cartridge.ToolReadOnly}
	p, err := Build(req)
	require.NoError(t, err)
	for _, heading := range []string{
		"## Rules (MUST)", "## Vibes (preferences)", "## Repository context",
		"## Open beads", "## Your bead", "## Round history", "## Brief from the human",
	} {
		assert.NotContains(t, p.Text, heading)
	}
	assert.Contains(t, p.Text, "# Product owner")
	assert.Contains(t, p.Text, beads.PromptNote)
	assert.True(t, strings.HasSuffix(p.Text, "\n"))
	assert.False(t, strings.HasSuffix(p.Text, "\n\n"))
}

// TestBuildSectionOrder pins the layer order the package documents.
func TestBuildSectionOrder(t *testing.T) {
	p, err := Build(pbEngineerRequest())
	require.NoError(t, err)
	order := []string{
		"# Engineer — implement",
		"## Your assignment",
		"## Rules (MUST)",
		"## Vibes (preferences)",
		"## Repository context",
		"## Your bead",
		"## Round history",
		"## How to finish",
		"## Output contract (demo_result)",
	}
	at := -1
	for _, marker := range order {
		i := strings.Index(p.Text, marker)
		require.GreaterOrEqual(t, i, 0, "missing %q", marker)
		assert.Greater(t, i, at, "%q is out of order", marker)
		at = i
	}
}

// TestBuildBriefAndOpenBeads covers the two product-owner-only layers.
func TestBuildBriefAndOpenBeads(t *testing.T) {
	p, err := Build(pbPORequest())
	require.NoError(t, err)
	assert.Contains(t, p.Text, "## Brief from the human")
	assert.Contains(t, p.Text, "Finish the statistics library")
	assert.Contains(t, p.Text, "## Open beads")
	assert.Contains(t, p.Text, "`sc-7a0` [open] Add a --json flag")
}

// TestBuildLensTemplates checks each tester lens gets its own framing.
func TestBuildLensTemplates(t *testing.T) {
	want := map[domain.Lens]string{
		domain.LensUX:   "Your lens is **UX**",
		domain.LensQA:   "Your lens is **QA**",
		domain.LensTest: "Your lens is **tests**",
	}
	for lens, marker := range want {
		t.Run(lens.String(), func(t *testing.T) {
			p, err := Build(pbTesterRequest(lens))
			require.NoError(t, err)
			assert.Contains(t, p.Text, marker)
			assert.Contains(t, p.Text, "# Tester ("+lens.String()+") — review")
			for other, otherMarker := range want {
				if other != lens {
					assert.NotContains(t, p.Text, otherMarker)
				}
			}
		})
	}
}

// TestBuildToolPolicyLine checks every policy explains itself.
func TestBuildToolPolicyLine(t *testing.T) {
	for _, policy := range cartridge.ToolPolicies() {
		t.Run(policy.String(), func(t *testing.T) {
			req := pbRequest(domain.RoleEngineer)
			req.ToolPolicy = policy
			p, err := Build(req)
			require.NoError(t, err)
			assert.Contains(t, p.Text, "Tool policy: "+policy.String()+" —")
		})
	}
}

func TestBuildRejectsBadRoleAndLens(t *testing.T) {
	t.Run("unknown role", func(t *testing.T) {
		_, err := Build(Request{Role: "wizard"})
		require.Error(t, err)
		assert.ErrorIs(t, err, domain.ErrUnknownValue)
	})
	t.Run("tester without a lens", func(t *testing.T) {
		_, err := Build(Request{Role: domain.RoleTester})
		require.Error(t, err)
		assert.ErrorIs(t, err, domain.ErrUnknownValue)
		assert.Contains(t, err.Error(), "needs a lens")
	})
	t.Run("lens on an engineer", func(t *testing.T) {
		req := pbRequest(domain.RoleEngineer)
		req.Lens = domain.LensQA
		_, err := Build(req)
		require.Error(t, err)
		assert.ErrorIs(t, err, domain.ErrUnknownValue)
		assert.Contains(t, err.Error(), "only testers have a lens")
	})
	t.Run("unknown lens", func(t *testing.T) {
		req := pbRequest(domain.RoleTester)
		req.Lens = "vibes"
		_, err := Build(req)
		require.Error(t, err)
		assert.ErrorIs(t, err, domain.ErrUnknownValue)
	})
}

// TestBuildIsDeterministic guards the golden tests: the same request must
// always produce the same bytes.
func TestBuildIsDeterministic(t *testing.T) {
	first, err := Build(pbEngineerRequest())
	require.NoError(t, err)
	for range 5 {
		again, err := Build(pbEngineerRequest())
		require.NoError(t, err)
		require.Equal(t, first.Text, again.Text)
	}
}

// TestBuildContractAppendedOnce checks the idempotent suffix helper is used:
// a trailing section that already is the contract is not repeated.
func TestBuildContractAppendedOnce(t *testing.T) {
	req := pbEngineerRequest()
	req.Instructions = append(req.Instructions, Section{
		Title: "Reminder",
		Body:  pbContract.PromptSuffix(),
	})
	p, err := Build(req)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(p.Text, "## Output contract (demo_result)"))
	assert.True(t, pbContract.EndsWithPromptSuffix(p.Text))
}

// TestBuildContractIsAlwaysLast: an earlier layer that quotes the contract —
// a bead description written by an agent, a rules file, an instruction
// section — must not suppress the real copy. Whatever else the prompt says,
// the output contract is the last thing in it, because its own first rule is
// that the result block is the last thing in the reply.
func TestBuildContractIsAlwaysLast(t *testing.T) {
	earlier := map[string]func(req *Request){
		"bead description": func(req *Request) { req.Bead.Bead.Description = pbContract.PromptSuffix() },
		"rules file": func(req *Request) {
			req.Rules = []Doc{{Name: "rules.md", Content: pbContract.PromptSuffix()}}
		},
		"instruction section": func(req *Request) {
			req.Instructions = append([]Section{{Title: "Reminder", Body: pbContract.PromptSuffix()}}, req.Instructions...)
		},
	}
	for name, inject := range earlier {
		t.Run(name, func(t *testing.T) {
			req := pbEngineerRequest()
			inject(&req)
			p, err := Build(req)
			require.NoError(t, err)
			assert.True(t, pbContract.EndsWithPromptSuffix(p.Text),
				"the contract is not the last thing in the prompt")
			assert.True(t, strings.HasSuffix(strings.TrimRight(p.Text, "\n"), "```"),
				"the schema block must close the prompt")
			assert.Equal(t, 2, strings.Count(p.Text, "## Output contract (demo_result)"),
				"the quoted copy is kept and the real one is still appended")
		})
	}
}

// TestBuildWithoutContract leaves the suffix out entirely.
func TestBuildWithoutContract(t *testing.T) {
	req := pbEngineerRequest()
	req.Contract = nil
	p, err := Build(req)
	require.NoError(t, err)
	assert.NotContains(t, p.Text, "## Output contract")
	assert.Contains(t, p.Text, "## How to finish")
}

func TestMustBuildPanics(t *testing.T) {
	assert.Panics(t, func() { MustBuild(Request{Role: "wizard"}) })
	assert.NotPanics(t, func() { MustBuild(pbPORequest()) })
}

func TestPromptStringAndFits(t *testing.T) {
	p := MustBuild(pbPORequest())
	assert.Equal(t, p.Text, p.String())
	assert.True(t, p.Fits())
	assert.Positive(t, p.Tokens)
	assert.Equal(t, DefaultMaxTokens, p.MaxTokens)
	assert.Equal(t, domain.RolePO, p.Role)
	assert.Equal(t, events.TaskPlan, p.Task)
	assert.Equal(t, domain.AgentID("po-1"), p.AgentID)
}

func TestRoleAndLensTemplates(t *testing.T) {
	for _, r := range domain.Roles() {
		body, err := RoleTemplate(r)
		require.NoError(t, err)
		assert.NotEmpty(t, body)
		assert.False(t, strings.HasSuffix(body, "\n"), "templates are trimmed")
	}
	for _, l := range domain.Lenses() {
		body, err := LensTemplate(l)
		require.NoError(t, err)
		assert.NotEmpty(t, body)
	}
	_, err := RoleTemplate("wizard")
	assert.ErrorIs(t, err, domain.ErrUnknownValue)
	_, err = LensTemplate("vibes")
	assert.ErrorIs(t, err, domain.ErrUnknownValue)

	entries, err := os.ReadDir("templates")
	require.NoError(t, err)
	assert.Len(t, entries, len(domain.Roles())+len(domain.Lenses()))
}
