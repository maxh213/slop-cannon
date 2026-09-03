package agents

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// engGoldenDir is where the engineer prompt goldens live, relative to this
// package. It is the same directory the prompt package's own goldens use;
// the engineer files are prefixed "agent-engineer-".
const engGoldenDir = "../../testdata/prompts"

// engGolden compares got with testdata/prompts/<name>.golden.md. Set
// UPDATE_GOLDEN=1 (make update-golden) to rewrite the file instead.
func engGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(engGoldenDir, name+".golden.md")
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

// engBody returns everything before the output contract. The contract is
// pinned separately (engRequireContract) so that a change to a message
// schema, which the schemas own, does not rewrite every engineer golden.
func engBody(t *testing.T, p prompt.Prompt, mode EngineerMode) string {
	t.Helper()
	c, err := mode.Contract()
	require.NoError(t, err)
	marker := "## Output contract (" + c.Name() + ")"
	i := strings.Index(p.Text, marker)
	require.GreaterOrEqual(t, i, 0, "prompt has no output contract section")
	return strings.TrimRight(p.Text[:i], "\n") + "\n"
}

// engRequireContract asserts the mode's real output contract is appended
// verbatim.
func engRequireContract(t *testing.T, p prompt.Prompt, mode EngineerMode) {
	t.Helper()
	c, err := mode.Contract()
	require.NoError(t, err)
	assert.True(t, c.HasPromptSuffix(p.Text), "output contract for %s is missing", mode)
}

// engRepo is a fixed repository context, hand-built so the goldens do not
// depend on a temporary directory.
func engRepo() prompt.RepoContext {
	return prompt.RepoContext{
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
		TreeDepth:  prompt.DefaultTreeDepth,
		ReadmeName: "README.md",
		ReadmeHead: "# fixture\n\nA tiny Go command-line tool that prints statistics for a list of numbers.",
	}
}

// engRules and engVibes are the configured prompt documents.
func engRules() []prompt.Doc {
	return []prompt.Doc{{
		Name: "rules.md",
		Path: "/work/fixture/rules.md",
		Content: "- Never disable or delete a test to make the suite pass.\n" +
			"- Every exported symbol has a doc comment.",
	}}
}

func engVibes() []prompt.Doc {
	return []prompt.Doc{{
		Name:    "vibes.md",
		Path:    "/work/fixture/vibes.md",
		Content: "- Prefer short functions with obvious names.\n- British spelling in prose.",
	}}
}

// engBead is the bead the engineer owns.
func engBead(round int) prompt.BeadContext {
	return prompt.BeadContext{
		Bead: domain.Bead{
			ID:          "sc-7a1",
			Title:       "Implement Median in stats",
			Description: "README promises a median but stats has none.",
			Acceptance:  "- stats.Median returns the middle value\n- table test covers even and odd lengths",
			Type:        domain.BeadTypeFeature,
			Priority:    domain.PriorityDefault,
			Size:        domain.SizeS,
			State:       domain.BeadStateInProgress,
		},
		Epic:     "sc-epic1",
		Worktree: "/state/runs/r1/wt/sc-7a1",
		Branch:   "sc/sc-7a1-implement-median",
		Round:    round,
	}
}

// engItems are the feedback items an address-feedback firing answers.
func engItems() []domain.FeedbackItem {
	return []domain.FeedbackItem{
		{
			ID:         "qa-r1-1",
			Severity:   domain.SeverityMajor,
			Category:   "correctness",
			Title:      "Median sorts the input in place",
			Detail:     "Callers see their slice reordered.",
			File:       "stats/stats.go",
			Line:       42,
			Suggestion: "Copy the slice before sorting.",
			Status:     domain.ItemOpen,
		},
		{
			ID:       "test-r1-1",
			Severity: domain.SeverityBlocker,
			Category: domain.CategoryRules,
			Title:    "Median has no doc comment",
			Detail:   "rules.md requires a doc comment on every exported symbol.",
			File:     "stats/stats.go",
			Line:     38,
			Status:   domain.ItemOpen,
		},
		{
			ID:       "ux-r1-1",
			Severity: domain.SeverityNit,
			Title:    "Error message starts with a capital letter",
			Status:   domain.ItemOpen,
		},
	}
}

// engHistory is one completed review round.
func engHistory() []prompt.Round {
	return []prompt.Round{{
		Number: 1,
		Report: &domain.ImplReport{
			Summary:      "Added stats.Median with a table test.",
			FilesChanged: []string{"stats/stats.go", "stats/stats_test.go"},
			HowToVerify:  "make test",
			Commit:       "1f0c9ab",
		},
		Verdicts: []domain.Verdict{{
			Lens:    domain.LensQA,
			Round:   1,
			Outcome: domain.VerdictChanges,
			Summary: "Sorting mutates the caller's slice.",
			Items:   engItems()[:1],
		}},
		Diff: "@@ stats/stats.go @@\n+func Median(xs []float64) float64 {",
	}}
}

// engRequest is the shared fixture request for mode.
func engRequest(mode EngineerMode) EngineerRequest {
	req := EngineerRequest{
		Mode:    mode,
		RunID:   "20260902-101500-1a2b",
		AgentID: "eng-sc-7a1-r1",
		Bead:    engBead(0),
		Repo:    engRepo(),
		Rules:   engRules(),
		Vibes:   engVibes(),
		Brief:   "Finish the statistics library and make the CLI pleasant to use.",
	}
	switch mode {
	case EngineerAddressFeedback:
		req.AgentID = "eng-sc-7a1-r2"
		req.Bead = engBead(2)
		req.History = engHistory()
		req.Items = engItems()
		req.Arbitrations = []domain.Arbitration{{
			ItemID:      "ux-r1-1",
			Ruling:      domain.RulingUpholdTester,
			Instruction: "Lower-case the message; it is appended to a sentence.",
			Rationale:   "The repository's other errors do the same.",
		}}
	case EngineerResolveConflict:
		req.AgentID = "eng-sc-7a1-conflict"
		req.Bead = engBead(2)
		req.History = engHistory()
		req.Conflict = EngineerConflict{
			Target: "main",
			Files:  []string{"stats/stats.go"},
			Output: "CONFLICT (content): Merge conflict in stats/stats.go\nerror: could not apply 1f0c9ab… Add Median",
		}
	}
	return req
}

func TestEngineerModes(t *testing.T) {
	assert.Equal(t, []EngineerMode{EngineerImplement, EngineerAddressFeedback, EngineerResolveConflict}, EngineerModes())

	tests := []struct {
		mode    EngineerMode
		task    events.Task
		message domain.MessageType
	}{
		{EngineerImplement, events.TaskImplement, domain.MessageImplReport},
		{EngineerAddressFeedback, events.TaskAddressFeedback, domain.MessageResponse},
		{EngineerResolveConflict, events.TaskResolveConflict, domain.MessageImplReport},
	}
	for _, tc := range tests {
		t.Run(tc.mode.String(), func(t *testing.T) {
			assert.True(t, tc.mode.Valid())
			assert.Equal(t, tc.task, tc.mode.Task())
			assert.Equal(t, tc.message, tc.mode.MessageType())

			c, err := tc.mode.Contract()
			require.NoError(t, err)
			assert.Equal(t, tc.message.String(), c.Name())

			got, err := EngineerModeForTask(tc.task)
			require.NoError(t, err)
			assert.Equal(t, tc.mode, got)

			// Every engineer task runs read_write (P5-06).
			p, err := Policy(domain.RoleEngineer, tc.task)
			require.NoError(t, err)
			assert.Equal(t, cartridge.ToolReadWrite, p)
		})
	}
}

func TestParseEngineerMode(t *testing.T) {
	for _, s := range []string{"implement", " Implement ", "ADDRESS-FEEDBACK", "resolve_conflict"} {
		m, err := ParseEngineerMode(s)
		require.NoError(t, err, s)
		assert.True(t, m.Valid())
	}
	for _, s := range []string{"", "review", "plan", "implementation"} {
		_, err := ParseEngineerMode(s)
		require.Error(t, err, s)
		assert.ErrorIs(t, err, domain.ErrUnknownValue)
		assert.Contains(t, err.Error(), "address_feedback")
	}

	_, err := EngineerModeForTask(events.TaskReview)
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrUnknownValue)

	_, err = EngineerMode("nope").Contract()
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrUnknownValue)
}

func TestEngineerRequestValidate(t *testing.T) {
	tests := map[string]struct {
		mutate func(*EngineerRequest)
		want   string
	}{
		"unknown mode":      {func(r *EngineerRequest) { r.Mode = "refactor" }, `engineer mode "refactor"`},
		"no agent id":       {func(r *EngineerRequest) { r.AgentID = "" }, "no agent id"},
		"no bead":           {func(r *EngineerRequest) { r.Bead.Bead.ID = "" }, "no bead"},
		"no worktree":       {func(r *EngineerRequest) { r.Bead.Worktree = "" }, "no worktree"},
		"relative worktree": {func(r *EngineerRequest) { r.Bead.Worktree = "wt/sc-7a1" }, "is not an absolute path"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req := engRequest(EngineerImplement)
			tc.mutate(&req)
			err := req.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)

			_, ferr := req.Firing()
			assert.Error(t, ferr)
		})
	}

	t.Run("valid", func(t *testing.T) {
		for _, m := range EngineerModes() {
			assert.NoError(t, engRequest(m).Validate(), m)
		}
	})

	t.Run("address feedback needs items", func(t *testing.T) {
		req := engRequest(EngineerAddressFeedback)
		req.Items = nil
		require.ErrorContains(t, req.Validate(), "no feedback items")

		req.Items = []domain.FeedbackItem{{Title: "nameless"}}
		require.ErrorContains(t, req.Validate(), "has no id")

		req.Items = []domain.FeedbackItem{{ID: "qa-1", Title: "one"}, {ID: "qa-1", Title: "two"}}
		require.ErrorContains(t, req.Validate(), `"qa-1" appears twice`)
	})

	t.Run("resolve conflict needs a conflict", func(t *testing.T) {
		req := engRequest(EngineerResolveConflict)
		req.Conflict = EngineerConflict{}
		assert.True(t, req.Conflict.Empty())
		require.ErrorContains(t, req.Validate(), "no conflict to resolve")
	})
}

func TestEngineerGoldenImplement(t *testing.T) {
	req := engRequest(EngineerImplement)
	p, err := req.Prompt()
	require.NoError(t, err)
	assert.False(t, p.OverBudget)
	assert.Empty(t, p.Truncations)
	engRequireContract(t, p, EngineerImplement)
	engGolden(t, "agent-engineer-implement", engBody(t, p, EngineerImplement))
}

func TestEngineerGoldenAddressFeedback(t *testing.T) {
	req := engRequest(EngineerAddressFeedback)
	p, err := req.Prompt()
	require.NoError(t, err)
	assert.False(t, p.OverBudget)
	engRequireContract(t, p, EngineerAddressFeedback)
	engGolden(t, "agent-engineer-address-feedback", engBody(t, p, EngineerAddressFeedback))
}

func TestEngineerGoldenResolveConflict(t *testing.T) {
	req := engRequest(EngineerResolveConflict)
	p, err := req.Prompt()
	require.NoError(t, err)
	assert.False(t, p.OverBudget)
	engRequireContract(t, p, EngineerResolveConflict)
	engGolden(t, "agent-engineer-resolve-conflict", engBody(t, p, EngineerResolveConflict))
}

// TestEngineerPromptContents checks what every engineer prompt must say,
// whatever the mode: the rules and vibes verbatim, the check command, the
// worktree bounds and the bead's acceptance criteria.
func TestEngineerPromptContents(t *testing.T) {
	for _, mode := range EngineerModes() {
		t.Run(mode.String(), func(t *testing.T) {
			req := engRequest(mode)
			p, err := req.Prompt()
			require.NoError(t, err)

			assert.Contains(t, p.Text, "# Engineer — "+strings.ReplaceAll(mode.Task().String(), "_", " "))
			assert.Contains(t, p.Text, "## Rules (MUST)")
			assert.Contains(t, p.Text, "Never disable or delete a test to make the suite pass.")
			assert.Contains(t, p.Text, "## Vibes (preferences)")
			assert.Contains(t, p.Text, "British spelling in prose.")
			assert.Contains(t, p.Text, "read_write")
			assert.Contains(t, p.Text, "make test")
			assert.Contains(t, p.Text, req.Bead.Worktree)
			assert.Contains(t, p.Text, "stats.Median returns the middle value")
			if mode != EngineerResolveConflict {
				assert.Contains(t, p.Text, `git commit -am "sc(sc-7a1): Implement Median in stats"`)
			}
		})
	}
}

// TestEngineerAddressFeedbackPrompt pins what the feedback round must show:
// every item by id, the pushback option, the rules-category blocker rule and
// the product owner's binding ruling.
func TestEngineerAddressFeedbackPrompt(t *testing.T) {
	req := engRequest(EngineerAddressFeedback)
	p, err := req.Prompt()
	require.NoError(t, err)

	for _, it := range req.Items {
		assert.Contains(t, p.Text, "`"+it.ID+"`")
		assert.Contains(t, p.Text, it.Title)
	}
	assert.Contains(t, p.Text, "stats/stats.go:42")
	assert.Contains(t, p.Text, "Suggestion: Copy the slice before sorting.")
	assert.Contains(t, p.Text, "`pushback`")
	assert.Contains(t, p.Text, "`wontfix`")
	assert.Contains(t, p.Text, "category is a violation of a rules file")
	assert.Contains(t, p.Text, "Binding rulings from the product owner")
	assert.Contains(t, p.Text, "uphold_tester")
	assert.Contains(t, p.Text, "Lower-case the message")
	// The round history is still there, so the engineer sees round 1.
	assert.Contains(t, p.Text, "### Round 1")
}

// TestEngineerResolveConflictPrompt pins the conflict block.
func TestEngineerResolveConflictPrompt(t *testing.T) {
	req := engRequest(EngineerResolveConflict)
	p, err := req.Prompt()
	require.NoError(t, err)

	assert.Contains(t, p.Text, "Rebasing onto: main")
	assert.Contains(t, p.Text, "Conflicted file: stats/stats.go")
	assert.Contains(t, p.Text, "`"+DefaultConflictContinue+"`")
	assert.Contains(t, p.Text, "CONFLICT (content): Merge conflict in stats/stats.go")
	assert.Contains(t, p.Text, "never force-push")

	t.Run("custom continue command", func(t *testing.T) {
		req := engRequest(EngineerResolveConflict)
		req.Conflict.Continue = "git cherry-pick --continue"
		p, err := req.Prompt()
		require.NoError(t, err)
		assert.Contains(t, p.Text, "`git cherry-pick --continue`")
	})
}

// TestEngineerPromptWithoutCheckCommand covers the repository where nothing
// was detected and the explicit override.
func TestEngineerPromptWithoutCheckCommand(t *testing.T) {
	req := engRequest(EngineerImplement)
	req.Repo.CheckCommand = ""
	req.Repo.CheckCommandDetected = false
	p, err := req.Prompt()
	require.NoError(t, err)
	assert.Contains(t, p.Text, "No check command is configured")

	req.CheckCommand = "just ci"
	p, err = req.Prompt()
	require.NoError(t, err)
	assert.Contains(t, p.Text, "`just ci`")
	assert.NotContains(t, p.Text, "No check command is configured")
}

// TestEngineerPromptFallbacks covers the wording when the caller has less to
// say: a bead with no title, and a conflict with no target branch.
func TestEngineerPromptFallbacks(t *testing.T) {
	req := engRequest(EngineerImplement)
	req.Bead.Bead.Title = ""
	p, err := req.Prompt()
	require.NoError(t, err)
	assert.Contains(t, p.Text, `git commit -am "sc(sc-7a1): work on sc-7a1"`)

	conflict := engRequest(EngineerResolveConflict)
	conflict.Conflict = EngineerConflict{Output: "CONFLICT (content): Merge conflict in stats/stats.go"}
	p, err = conflict.Prompt()
	require.NoError(t, err)
	assert.Contains(t, p.Text, "rebasing it onto the default branch stopped on conflicts")
	assert.NotContains(t, p.Text, "Rebasing onto:")
	assert.Contains(t, p.Text, "`"+DefaultConflictContinue+"`")
}

func TestEngineerFiring(t *testing.T) {
	req := engRequest(EngineerAddressFeedback)
	req.Model = "opus"
	req.MaxTurns = 40
	req.Timeout = 25 * time.Minute
	req.Env = map[string]string{"SC_TEST": "1"}

	f, err := req.Firing()
	require.NoError(t, err)

	assert.Equal(t, EngineerAddressFeedback, f.Mode)
	assert.Equal(t, events.TaskAddressFeedback, f.Task)
	assert.Equal(t, domain.MessageResponse, f.Message)
	require.NotNil(t, f.Contract)
	assert.Equal(t, "response", f.Contract.Name())

	assert.Equal(t, req.Bead.Worktree, f.Spec.Workdir)
	assert.Equal(t, cartridge.ToolReadWrite, f.Spec.ToolPolicy)
	assert.Equal(t, "opus", f.Spec.Model)
	assert.Equal(t, 40, f.Spec.MaxTurns)
	assert.Equal(t, 25*time.Minute, f.Spec.Timeout)
	assert.Equal(t, f.Prompt.Text, f.Spec.Prompt)
	assert.NoError(t, f.Spec.Validate())

	assert.Equal(t, map[string]string{
		cartridge.LabelRun:   "20260902-101500-1a2b",
		cartridge.LabelAgent: "eng-sc-7a1-r2",
		cartridge.LabelRole:  "engineer",
		cartridge.LabelTask:  "address_feedback",
		cartridge.LabelBead:  "sc-7a1",
		cartridge.LabelRound: "2",
	}, map[string]string(f.Spec.Labels))

	// The env map is copied, not retained.
	req.Env["SC_TEST"] = "2"
	assert.Equal(t, "1", f.Spec.Env["SC_TEST"])

	spec, err := req.RunSpec()
	require.NoError(t, err)
	assert.Equal(t, f.Spec.Prompt, spec.Prompt)
}

// TestEngineerFiringEnforcement is the P5-06 caller contract: the firing's
// policy must be one the cartridge can really hold.
func TestEngineerFiringEnforcement(t *testing.T) {
	f, err := engRequest(EngineerImplement).Firing()
	require.NoError(t, err)

	c, err := mock.NewAdapter(mock.Name, mock.NewScenario("engineer"))
	require.NoError(t, err)
	en, err := EnforcementFor(c, f.Spec.ToolPolicy)
	require.NoError(t, err)
	assert.NoError(t, en.Check())
	assert.Equal(t, cartridge.ToolReadWrite, en.Policy)
}

// TestEngineerPromptWrite covers the debug copy (D-23).
func TestEngineerPromptWrite(t *testing.T) {
	runDir := t.TempDir()
	p, err := engRequest(EngineerImplement).Prompt()
	require.NoError(t, err)
	require.NoError(t, p.Write(runDir))

	assert.Equal(t, prompt.PathFor(runDir, "eng-sc-7a1-r1"), p.Path)
	data, err := os.ReadFile(p.Path)
	require.NoError(t, err)
	assert.Equal(t, p.Text, string(data))
}

// engImplReport is a schema-valid impl_report reply.
const engImplReport = "Done, tests pass.\n\n```sc-result\n" + `{
  "summary": "Median copies the slice before sorting and has a doc comment.",
  "files_changed": ["stats/stats.go", "stats/stats_test.go"],
  "how_to_verify": "make test",
  "risks": ["Even-length behaviour is the mean of the middle pair."],
  "questions": [],
  "commit": "1f0c9ab"
}` + "\n```\n"

// engResponses is a schema-valid response reply answering every engItems id.
const engResponses = "Answered all three.\n\n```sc-result\n" + `{
  "responses": [
    {"item_id": "qa-r1-1", "action": "fixed", "rationale": "Median now sorts a copy."},
    {"item_id": "test-r1-1", "action": "fixed", "rationale": "Added the doc comment."},
    {"item_id": "ux-r1-1", "action": "pushback", "rationale": "The message is shown on its own line, not appended."}
  ]
}` + "\n```\n"

func TestEngineerParseResultImplement(t *testing.T) {
	for _, mode := range []EngineerMode{EngineerImplement, EngineerResolveConflict} {
		t.Run(mode.String(), func(t *testing.T) {
			req := engRequest(mode)
			res, err := req.ParseResult(engImplReport)
			require.NoError(t, err)

			assert.Equal(t, mode, res.Mode)
			assert.Equal(t, domain.MessageImplReport, res.MessageType())
			assert.Equal(t, "Median copies the slice before sorting and has a doc comment.", res.Report.Summary)
			assert.Equal(t, []string{"stats/stats.go", "stats/stats_test.go"}, res.Report.FilesChanged)
			assert.Equal(t, "1f0c9ab", res.Report.Commit)
			assert.Equal(t, res.Report, res.Payload())
			assert.Equal(t, "impl_report", res.Parsed.Contract)
			assert.NoError(t, ValidateJSON(domain.MessageImplReport, res.Parsed.JSON))
		})
	}
}

func TestEngineerParseResultAddressFeedback(t *testing.T) {
	req := engRequest(EngineerAddressFeedback)
	res, err := req.ParseResult(engResponses)
	require.NoError(t, err)

	assert.Equal(t, domain.MessageResponse, res.MessageType())
	require.Len(t, res.Responses, 3)
	assert.Equal(t, domain.ActionFixed, res.Responses[0].Action)
	assert.Equal(t, domain.ActionPushback, res.Responses[2].Action)
	assert.Equal(t, res.Responses, res.Payload())
	assert.NoError(t, ValidateJSON(domain.MessageResponse, res.Parsed.JSON))
}

// TestEngineerParseResultContractFailures covers the failures the schema
// itself catches; each one must be a retryable *structured.ParseError.
func TestEngineerParseResultContractFailures(t *testing.T) {
	tests := map[string]struct {
		mode   EngineerMode
		output string
		want   error
	}{
		"no block":     {EngineerImplement, "I finished the work.", structured.ErrMissing},
		"truncated":    {EngineerImplement, "ok\n```sc-result\n{\"summary\": \"half", structured.ErrTruncated},
		"bad json":     {EngineerImplement, "ok\n```sc-result\n{summary: nope}\n```\n", structured.ErrInvalidJSON},
		"empty report": {EngineerImplement, "ok\n```sc-result\n{}\n```\n", structured.ErrSchema},
		"bad action": {EngineerAddressFeedback,
			"ok\n```sc-result\n{\"responses\":[{\"item_id\":\"qa-r1-1\",\"action\":\"ignored\"}]}\n```\n",
			structured.ErrSchema},
		"pushback without a reason": {EngineerAddressFeedback,
			"ok\n```sc-result\n{\"responses\":[{\"item_id\":\"qa-r1-1\",\"action\":\"pushback\"}]}\n```\n",
			structured.ErrSchema},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := engRequest(tc.mode).ParseResult(tc.output)
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.want)

			pe, ok := structured.AsParseError(err)
			require.True(t, ok, "want a *structured.ParseError, got %T", err)
			assert.NotEmpty(t, pe.Feedback())
		})
	}
}

// TestEngineerResponsesMustMatchTheItems covers what the schema cannot
// express: the answers have to line up with the items that were raised.
func TestEngineerResponsesMustMatchTheItems(t *testing.T) {
	items := engItems()
	tests := map[string]struct {
		responses []domain.Response
		want      string
	}{
		"missing an item": {
			[]domain.Response{
				{ItemID: "qa-r1-1", Action: domain.ActionFixed},
				{ItemID: "test-r1-1", Action: domain.ActionFixed},
			},
			`feedback item "ux-r1-1" has no response`,
		},
		"unknown item": {
			[]domain.Response{
				{ItemID: "qa-r1-1", Action: domain.ActionFixed},
				{ItemID: "test-r1-1", Action: domain.ActionFixed},
				{ItemID: "ux-r1-1", Action: domain.ActionFixed},
				{ItemID: "qa-r1-9", Action: domain.ActionWontfix, Rationale: "invented"},
			},
			`"qa-r1-9" is not one of the feedback items`,
		},
		"answered twice": {
			[]domain.Response{
				{ItemID: "qa-r1-1", Action: domain.ActionFixed},
				{ItemID: "qa-r1-1", Action: domain.ActionWontfix, Rationale: "changed my mind"},
				{ItemID: "test-r1-1", Action: domain.ActionFixed},
				{ItemID: "ux-r1-1", Action: domain.ActionFixed},
			},
			`item "qa-r1-1" is answered more than once`,
		},
		"pushback on a rules violation": {
			[]domain.Response{
				{ItemID: "qa-r1-1", Action: domain.ActionFixed},
				{ItemID: "test-r1-1", Action: domain.ActionPushback, Rationale: "doc comments are noise"},
				{ItemID: "ux-r1-1", Action: domain.ActionFixed},
			},
			`item "test-r1-1" is a rules violation`,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := ValidateEngineerResponses(items, tc.responses)
			require.Error(t, err)
			assert.ErrorIs(t, err, structured.ErrSchema)
			assert.Contains(t, err.Error(), tc.want)

			pe, ok := structured.AsParseError(err)
			require.True(t, ok)
			assert.Equal(t, "response", pe.Contract)
			assert.Contains(t, pe.Feedback(), tc.want)
		})
	}

	t.Run("all answered", func(t *testing.T) {
		assert.NoError(t, ValidateEngineerResponses(items, []domain.Response{
			{ItemID: "ux-r1-1", Action: domain.ActionWontfix, Rationale: "out of scope"},
			{ItemID: "test-r1-1", Action: domain.ActionFixed},
			{ItemID: "qa-r1-1", Action: domain.ActionPushback, Rationale: "the slice is already a copy"},
		}))
	})

	t.Run("through ParseResult", func(t *testing.T) {
		req := engRequest(EngineerAddressFeedback)
		out := "ok\n```sc-result\n" +
			`{"responses":[{"item_id":"qa-r1-1","action":"fixed","rationale":"copied"}]}` + "\n```\n"
		_, err := req.ParseResult(out)
		require.Error(t, err)
		assert.ErrorIs(t, err, structured.ErrSchema)
		assert.Contains(t, err.Error(), `feedback item "test-r1-1" has no response`)
	})
}

func TestParseEngineerOutputUnknownMode(t *testing.T) {
	_, err := ParseEngineerOutput("refactor", engImplReport)
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrUnknownValue)
}

// TestEngineerThroughMockCartridge fires a real (mock) cartridge with the
// firing this package builds and reads the reply back, so the prompt, the
// spec and the parser are exercised together.
func TestEngineerThroughMockCartridge(t *testing.T) {
	wt := t.TempDir()
	req := engRequest(EngineerImplement)
	req.Bead.Worktree = wt
	req.Repo.Root = wt

	f, err := req.Firing()
	require.NoError(t, err)

	scenario := mock.NewScenario("engineer")
	scenario.Rule("implement", mock.Match{Role: "engineer", Task: "implement"}).
		Say("Reading the bead.").
		Exec("go", "test", "./...").
		Structured(domain.ImplReport{
			Summary:      "Added stats.Median with a table test.",
			FilesChanged: []string{"stats/stats.go"},
			HowToVerify:  "make test",
			Commit:       "1f0c9ab",
		})
	require.NoError(t, scenario.Validate())

	c, err := mock.NewAdapter(mock.Name, scenario)
	require.NoError(t, err)
	en, err := EnforcementFor(c, f.Spec.ToolPolicy)
	require.NoError(t, err)
	require.NoError(t, en.Check())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := structured.Fire(ctx, c, f.Spec, f.Contract)
	require.NoError(t, err)

	res, err := req.ParseResult(out.Result.Output)
	require.NoError(t, err)
	assert.Equal(t, "Added stats.Median with a table test.", res.Report.Summary)
	assert.Equal(t, domain.MessageImplReport, res.MessageType())

	calls := c.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, cartridge.ToolReadWrite, calls[0].Spec.ToolPolicy)
	assert.Equal(t, wt, calls[0].Spec.Workdir)
}

// TestEngineerRequestErrorsPropagate keeps every entry point honest about a
// bad request.
func TestEngineerRequestErrorsPropagate(t *testing.T) {
	var req EngineerRequest
	_, err := req.PromptRequest()
	require.Error(t, err)
	_, err = req.Prompt()
	require.Error(t, err)
	_, err = req.RunSpec()
	require.Error(t, err)
	_, err = req.ParseResult(engImplReport)
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrUnknownValue))
}
