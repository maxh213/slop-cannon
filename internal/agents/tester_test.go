package agents_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/cartridge/structured"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
)

// tsGoldenDir is where the tester prompt goldens live, relative to this
// package.
const tsGoldenDir = "../../testdata/agents/tester"

// tsGolden compares got with testdata/agents/tester/<name>.golden.md. Set
// UPDATE_GOLDEN=1 (make update-golden) to rewrite the file instead.
func tsGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(tsGoldenDir, name+".golden.md")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden %s; run `make update-golden`", path)
	require.Equal(t, string(want), got,
		"tester prompt differs from %s; run `make update-golden` if the change is intended", path)
}

// tsRepo is a fixed repository context, hand-built so the goldens do not
// depend on a temporary directory.
func tsRepo() prompt.RepoContext {
	return prompt.RepoContext{
		Root:                 "/work/fixture",
		DefaultBranch:        "main",
		Stack:                []string{"Go", "Make"},
		CheckCommand:         "make test",
		CheckCommandDetected: true,
		Tree:                 []string{"Makefile", "README.md", "go.mod", "stats/stats.go", "stats/stats_test.go"},
		TreeTotal:            5,
		TreeDepth:            prompt.DefaultTreeDepth,
		ReadmeName:           "README.md",
		ReadmeHead:           "# fixture\n\nA tiny Go tool that prints statistics for a list of numbers.",
	}
}

// tsBead is the bead under review in every fixture.
func tsBead() prompt.BeadContext {
	return prompt.BeadContext{
		Bead: domain.Bead{
			ID:          "sc-7a1",
			Title:       "Implement Median in stats",
			Description: "README promises a median but stats has none.",
			Acceptance:  "- stats.Median returns the middle value\n- a table test covers even and odd lengths",
			Type:        domain.BeadTypeFeature,
			Priority:    domain.PriorityDefault,
			Size:        domain.SizeS,
			State:       domain.BeadStateInReview,
		},
		Epic:     "sc-epic1",
		Worktree: "/state/runs/r1/wt/sc-7a1",
		Branch:   "sc/sc-7a1-implement-median",
	}
}

// tsRules and tsVibes are the configured docs every prompt must carry.
func tsRules() []prompt.Doc {
	return []prompt.Doc{{
		Name: "rules.md", Path: "/work/fixture/rules.md",
		Content: "- Never disable or delete a test to make the suite pass.\n- Every exported symbol has a doc comment.",
	}}
}

func tsVibes() []prompt.Doc {
	return []prompt.Doc{{
		Name: "vibes.md", Path: "/work/fixture/vibes.md",
		Content: "- Prefer short functions with obvious names.\n- British spelling in prose.",
	}}
}

// tsDiff is the change set under review.
func tsDiff() *gitx.Diff {
	return &gitx.Diff{
		Stats: gitx.DiffStats{
			Base: "1a2b3c4", Head: "9f8e7d6", Files: 2, Insertions: 24, Deletions: 1,
			Changed: []gitx.FileStat{
				{Path: "stats/stats.go", Insertions: 12, Deletions: 1},
				{Path: "stats/stats_test.go", Insertions: 12},
			},
		},
		Files: []gitx.DiffFile{
			{FileStat: gitx.FileStat{Path: "stats/stats.go", Insertions: 12, Deletions: 1}, Status: gitx.DiffModified},
			{FileStat: gitx.FileStat{Path: "stats/stats_test.go", Insertions: 12}, Status: gitx.DiffModified},
		},
		Bytes:    412,
		MaxBytes: gitx.DefaultMaxDiffBytes,
		Patch: "--- a/stats/stats.go\n+++ b/stats/stats.go\n@@ -40,6 +40,17 @@\n" +
			"+func Median(xs []float64) float64 {\n+\tsort.Float64s(xs)\n+\treturn xs[len(xs)/2]\n+}\n",
	}
}

// tsReport is the engineer's report for the round under review.
func tsReport() *domain.ImplReport {
	return &domain.ImplReport{
		Summary:      "Added stats.Median with a table test for odd and even lengths.",
		FilesChanged: []string{"stats/stats.go", "stats/stats_test.go"},
		HowToVerify:  "make test",
		Risks:        []string{"Even-length behaviour returns the upper middle value."},
		Commit:       "9f8e7d6",
	}
}

// tsReport2 is the engineer's report for the second round, after it
// answered the first round's feedback.
func tsReport2() *domain.ImplReport {
	return &domain.ImplReport{
		Summary:      "Median now sorts a copy of the slice; added a test that the input is untouched.",
		FilesChanged: []string{"stats/stats.go", "stats/stats_test.go"},
		HowToVerify:  "make test",
		Commit:       "4c5d6e7",
	}
}

// tsItems are the items a tester carries into a re-review or a pushback
// evaluation; tsResponses are the engineer's answers to them.
func tsItems(lens domain.Lens) []domain.FeedbackItem {
	return []domain.FeedbackItem{
		{
			ID: string(lens) + "-r1-1", Severity: domain.SeverityMajor, Category: "correctness",
			Title: "Median sorts the caller's slice in place", File: "stats/stats.go", Line: 42,
			Detail: "Callers see their slice reordered.", Suggestion: "Sort a copy.", Status: domain.ItemOpen,
		},
		{
			ID: string(lens) + "-r1-2", Severity: domain.SeverityBlocker, Category: domain.CategoryRules,
			Title: "Median has no doc comment", File: "stats/stats.go", Line: 40,
			Detail: "rules.md requires a doc comment on every exported symbol.", Status: domain.ItemOpen,
		},
	}
}

func tsResponses(lens domain.Lens) []domain.Response {
	return []domain.Response{
		{ItemID: string(lens) + "-r1-1", Action: domain.ActionFixed, Rationale: "Median now sorts a copy."},
		{ItemID: string(lens) + "-r1-2", Action: domain.ActionPushback, Rationale: "The function is obvious from its name."},
	}
}

// tsHistory is one completed round, so the goldens show the history layer.
func tsHistory(lens domain.Lens) []prompt.Round {
	return []prompt.Round{{
		Number:    1,
		Report:    tsReport(),
		Verdicts:  []domain.Verdict{{Lens: lens, Round: 1, Outcome: domain.VerdictChanges, Summary: "Two problems.", Items: tsItems(lens)}},
		Responses: tsResponses(lens),
	}}
}

// tsRequest builds a complete request for one lens and mode.
func tsRequest(lens domain.Lens, mode agents.TesterMode) agents.TesterRequest {
	r := agents.TesterRequest{
		Mode:    mode,
		Lens:    lens,
		RunID:   "20260902-101500-1a2b",
		AgentID: domain.AgentID("tester-" + lens.String() + "-sc-7a1"),
		Workdir: "/state/runs/r1/wt/sc-7a1",
		Round:   1,
		Bead:    tsBead(),
		Repo:    tsRepo(),
		Rules:   tsRules(),
		Vibes:   tsVibes(),
		Report:  tsReport(),
		Diff:    tsDiff(),
	}
	switch mode {
	case agents.TesterModeReReview:
		r.Round = 2
		r.History = tsHistory(lens)
		r.Report = tsReport2()
		r.Items = tsItems(lens)
		r.Responses = tsResponses(lens)
	case agents.TesterModeEvaluatePushback:
		r.Round = 2
		r.History = tsHistory(lens)
		r.Items = agents.TesterPushbackItems(tsItems(lens), tsResponses(lens))
		r.Responses = tsResponses(lens)
		r.Diff = nil
		r.Report = nil
	}
	return r
}

// tsVerdictJSON renders a verdict as the JSON body of an sc-result block.
func tsVerdictJSON(t *testing.T, v domain.Verdict) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// tsOutput wraps a verdict in the transcript an agent would produce.
func tsOutput(t *testing.T, v domain.Verdict) string {
	t.Helper()
	return "I ran the suite and read the diff.\n\n```sc-result\n" + string(tsVerdictJSON(t, v)) + "\n```\n"
}

// tsGoodVerdict is a verdict that satisfies the mode's rules, so each mode
// has one known-good answer to validate against the schema.
func tsGoodVerdict(lens domain.Lens, mode agents.TesterMode) domain.Verdict {
	items := tsItems(lens)
	switch mode {
	case agents.TesterModeReReview:
		return domain.Verdict{
			Lens: lens, Round: 2, Outcome: domain.VerdictChanges,
			Summary: "The sort is fixed; the doc comment is not.",
			Items: []domain.FeedbackItem{
				tsWithStatus(items[0], domain.ItemResolved, "Median now copies before sorting."),
				tsWithStatus(items[1], domain.ItemHeld, "rules.md is not optional; add the comment."),
			},
		}
	case agents.TesterModeEvaluatePushback:
		return domain.Verdict{
			Lens: lens, Round: 2, Outcome: domain.VerdictChanges,
			Summary: "The rules item stands.",
			Items: []domain.FeedbackItem{
				tsWithStatus(items[1], domain.ItemHeld, "A rules violation is not settled by the name being obvious."),
			},
		}
	default:
		return domain.Verdict{
			Lens: lens, Round: 1, Outcome: domain.VerdictChanges,
			Summary: "Two problems, one of them a rules violation.",
			Items:   items,
		}
	}
}

// tsWithStatus returns a copy of it with a new status and detail.
func tsWithStatus(it domain.FeedbackItem, s domain.ItemStatus, detail string) domain.FeedbackItem {
	it.Status = s
	it.Detail = detail
	return it
}

func TestTesterModes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []agents.TesterMode{agents.TesterModeReview, agents.TesterModeReReview, agents.TesterModeEvaluatePushback},
		agents.TesterModes())
	for _, m := range agents.TesterModes() {
		assert.True(t, m.Valid(), m)
		got, err := agents.ParseTesterMode(strings.ToUpper(strings.ReplaceAll(m.String(), "_", "-")))
		require.NoError(t, err)
		assert.Equal(t, m, got)
	}
	assert.Equal(t, events.TaskReview, agents.TesterModeReview.Task())
	assert.Equal(t, events.TaskReview, agents.TesterModeReReview.Task())
	assert.Equal(t, events.TaskEvaluatePushback, agents.TesterModeEvaluatePushback.Task())

	_, err := agents.ParseTesterMode("arbitrate")
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrUnknownValue)
	assert.Contains(t, err.Error(), "evaluate_pushback")
	assert.False(t, agents.TesterMode("").Valid())
}

// tsWithoutContract returns the prompt without the output contract, which
// the goldens leave out so that an edit to verdict.schema.json does not
// rewrite all nine of them. It fails the test when the contract is absent.
func tsWithoutContract(t *testing.T, text string) string {
	t.Helper()
	contract, err := agents.ContractFor(domain.MessageVerdict)
	require.NoError(t, err)
	body := strings.TrimSuffix(text, "\n\n"+contract.PromptSuffix())
	require.NotEqual(t, text, body, "the verdict contract is not the last section of the prompt")
	return body + "\n"
}

// TestTesterPromptGoldens pins one assembled prompt per lens and mode: the
// nine firings a tester can have.
func TestTesterPromptGoldens(t *testing.T) {
	t.Parallel()
	for _, lens := range domain.Lenses() {
		for _, mode := range agents.TesterModes() {
			name := lens.String() + "-" + mode.String()
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				p, err := agents.BuildTesterPrompt(tsRequest(lens, mode))
				require.NoError(t, err)
				assert.Equal(t, domain.RoleTester, p.Role)
				assert.Equal(t, lens, p.Lens)
				assert.Equal(t, mode.Task(), p.Task)
				assert.True(t, p.Fits(), "prompt over budget: %d/%d tokens", p.Tokens, p.MaxTokens)
				tsGolden(t, name, tsWithoutContract(t, p.Text))
			})
		}
	}
}

// TestTesterPromptCarriesEveryLayer checks the things every tester prompt
// must contain, whatever its lens and mode.
func TestTesterPromptCarriesEveryLayer(t *testing.T) {
	t.Parallel()
	for _, lens := range domain.Lenses() {
		for _, mode := range agents.TesterModes() {
			t.Run(lens.String()+"-"+mode.String(), func(t *testing.T) {
				t.Parallel()
				req := tsRequest(lens, mode)
				p, err := agents.BuildTesterPrompt(req)
				require.NoError(t, err)
				text := p.Text

				lensTmpl, err := prompt.LensTemplate(lens)
				require.NoError(t, err)
				assert.Contains(t, text, lensTmpl, "the lens template is missing")
				assert.Contains(t, text, tsRules()[0].Content, "rules are missing")
				assert.Contains(t, text, tsVibes()[0].Content, "vibes are missing")
				assert.Contains(t, text, "read_exec", "the tool policy is missing")
				assert.Contains(t, text, "category `rules`", "the mandatory rules category is missing")
				assert.Contains(t, text, "severity `blocker`", "rules items must be blockers")
				assert.Contains(t, text, "stats.Median returns the middle value", "the acceptance criteria are missing")
				assert.Contains(t, text, "```sc-result", "the output contract is missing")
				// P5-20: a branch behind the default branch is the delivery's
				// to bring up to date; testers chasing it sent PandaJump-1di
				// round after round back for rebases master kept outrunning.
				assert.Contains(t, text, "Never ask for a rebase or a\n  merge of the default branch", "every tester is told not to chase the default branch")

				contract, err := agents.ContractFor(domain.MessageVerdict)
				require.NoError(t, err)
				assert.True(t, contract.HasPromptSuffix(text), "the verdict contract is not appended")
			})
		}
	}
}

// TestTesterPromptModeContent checks the wording each mode must carry.
func TestTesterPromptModeContent(t *testing.T) {
	t.Parallel()
	review, err := agents.BuildTesterPrompt(tsRequest(domain.LensQA, agents.TesterModeReview))
	require.NoError(t, err)
	assert.Contains(t, review.Text, "```diff", "the diff is missing")
	assert.Contains(t, review.Text, "func Median(xs []float64) float64 {")
	assert.Contains(t, review.Text, "Added stats.Median with a table test", "the impl report is missing")
	assert.Contains(t, review.Text, "`qa-r1-1`", "the item id convention is missing")
	assert.NotContains(t, review.Text, "The items you raised last round")

	rereview, err := agents.BuildTesterPrompt(tsRequest(domain.LensQA, agents.TesterModeReReview))
	require.NoError(t, err)
	assert.Contains(t, rereview.Text, "The items you raised last round")
	assert.Contains(t, rereview.Text, "Median sorts the caller's slice in place")
	assert.Contains(t, rereview.Text, "Engineer: **fixed**")
	assert.Contains(t, rereview.Text, "Median now sorts a copy of the slice", "this round's report is missing")
	assert.Contains(t, rereview.Text, "original id")
	assert.Contains(t, rereview.Text, "```diff", "a re-review still reviews a diff")

	pushback, err := agents.BuildTesterPrompt(tsRequest(domain.LensQA, agents.TesterModeEvaluatePushback))
	require.NoError(t, err)
	assert.Contains(t, pushback.Text, "The items the engineer pushed back on")
	assert.Contains(t, pushback.Text, "Engineer: **pushback**")
	assert.Contains(t, pushback.Text, "cannot be accepted")
	assert.Contains(t, pushback.Text, "category `rules`")
	assert.NotContains(t, pushback.Text, "```diff", "pushback evaluation has no new diff")
	assert.Contains(t, pushback.Text, "### `qa-r1-2`")
	assert.NotContains(t, pushback.Text, "### `qa-r1-1`", "only the disputed items are listed")
}

// TestTesterPromptRendersATruncatedDiff covers the oversized-diff path.
func TestTesterPromptRendersATruncatedDiff(t *testing.T) {
	t.Parallel()
	req := tsRequest(domain.LensUX, agents.TesterModeReview)
	d := tsDiff()
	d.Patch = ""
	d.Truncated = true
	d.Bytes = 1 << 20
	req.Diff = d
	p, err := agents.BuildTesterPrompt(req)
	require.NoError(t, err)
	assert.NotContains(t, p.Text, "```diff")
	assert.Contains(t, p.Text, "over the 204800-byte cap")
	assert.Contains(t, p.Text, "M stats/stats.go  +12 -1")
}

// TestTesterPromptRendersAnEmptyDiff makes the "nothing to review" case
// explicit rather than silently showing an empty block.
func TestTesterPromptRendersAnEmptyDiff(t *testing.T) {
	t.Parallel()
	req := tsRequest(domain.LensTest, agents.TesterModeReview)
	req.Diff = &gitx.Diff{Stats: gitx.DiffStats{Base: "1a2b3c4", Head: "1a2b3c4"}, MaxBytes: gitx.DefaultMaxDiffBytes}
	p, err := agents.BuildTesterPrompt(req)
	require.NoError(t, err)
	assert.Contains(t, p.Text, "no committed changes")
	assert.NotContains(t, p.Text, "```diff")
}

func TestTesterRequestValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fix  func(*agents.TesterRequest)
		want string
	}{
		{"unknown mode", func(r *agents.TesterRequest) { r.Mode = "approve" }, "tester mode"},
		{"no lens", func(r *agents.TesterRequest) { r.Lens = "" }, "tester lens"},
		{"bad lens", func(r *agents.TesterRequest) { r.Lens = "security" }, "tester lens"},
		{"no bead", func(r *agents.TesterRequest) { r.Bead = prompt.BeadContext{} }, "no bead"},
		{"round zero", func(r *agents.TesterRequest) { r.Round = 0 }, "rounds are 1-based"},
		{"no diff", func(r *agents.TesterRequest) { r.Diff = nil }, "needs the diff"},
		{"re-review without items", func(r *agents.TesterRequest) {
			r.Mode = agents.TesterModeReReview
			r.Items = nil
		}, "items raised in the previous round"},
		{"pushback without items", func(r *agents.TesterRequest) {
			r.Mode = agents.TesterModeEvaluatePushback
			r.Items = nil
		}, "items the engineer pushed back on"},
		{"pushback without a pushback", func(r *agents.TesterRequest) {
			r.Mode = agents.TesterModeEvaluatePushback
			r.Items = tsItems(domain.LensQA)
			r.Responses = []domain.Response{{ItemID: "qa-r1-1", Action: domain.ActionFixed}}
		}, "at least one pushback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := tsRequest(domain.LensQA, agents.TesterModeReview)
			tc.fix(&req)
			err := req.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			_, buildErr := agents.BuildTesterPrompt(req)
			assert.Error(t, buildErr, "BuildTesterPrompt must reject what Validate rejects")
		})
	}
	require.NoError(t, tsRequest(domain.LensUX, agents.TesterModeReview).Validate())
	require.NoError(t, tsRequest(domain.LensTest, agents.TesterModeReReview).Validate())
	require.NoError(t, tsRequest(domain.LensQA, agents.TesterModeEvaluatePushback).Validate())
}

func TestTesterRunSpec(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	req := tsRequest(domain.LensQA, agents.TesterModeReReview)
	req.Workdir = dir
	req.Model = "sonnet"
	req.MaxTurns = 12
	req.Timeout = 3 * time.Minute

	spec, p, err := agents.TesterRunSpec(req)
	require.NoError(t, err)
	require.NoError(t, spec.Validate())
	assert.Equal(t, cartridge.ToolReadExec, spec.ToolPolicy)
	assert.Equal(t, p.Text, spec.Prompt)
	assert.Equal(t, dir, spec.Workdir)
	assert.Equal(t, "sonnet", spec.Model)
	assert.Equal(t, 12, spec.MaxTurns)
	assert.Equal(t, 3*time.Minute, spec.Timeout)
	assert.Equal(t, "tester", spec.Labels.Get(cartridge.LabelRole))
	assert.Equal(t, "qa", spec.Labels.Get(cartridge.LabelLens))
	assert.Equal(t, "review", spec.Labels.Get(cartridge.LabelTask))
	assert.Equal(t, "sc-7a1", spec.Labels.Get(cartridge.LabelBead))
	assert.Equal(t, "2", spec.Labels.Get(cartridge.LabelRound))
	assert.Equal(t, "20260902-101500-1a2b", spec.Labels.Get(cartridge.LabelRun))

	pb := tsRequest(domain.LensQA, agents.TesterModeEvaluatePushback)
	pb.Workdir = dir
	spec, _, err = agents.TesterRunSpec(pb)
	require.NoError(t, err)
	assert.Equal(t, "evaluate_pushback", spec.Labels.Get(cartridge.LabelTask))
	assert.Equal(t, cartridge.ToolReadExec, spec.ToolPolicy)

	t.Run("rejects a relative workdir", func(t *testing.T) {
		bad := tsRequest(domain.LensUX, agents.TesterModeReview)
		bad.Workdir = "wt/sc-7a1"
		_, _, err := agents.TesterRunSpec(bad)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "absolute")
	})
	t.Run("rejects a missing agent id", func(t *testing.T) {
		bad := tsRequest(domain.LensUX, agents.TesterModeReview)
		bad.Workdir = dir
		bad.AgentID = ""
		_, _, err := agents.TesterRunSpec(bad)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "agent id")
	})
}

// TestTesterFiringThroughTheMock fires a scripted tester through the mock
// cartridge and reads the verdict back, which is the whole round-trip P6-04
// depends on.
func TestTesterFiringThroughTheMock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	req := tsRequest(domain.LensQA, agents.TesterModeReview)
	req.Workdir = dir

	want := tsGoodVerdict(domain.LensQA, agents.TesterModeReview)
	sc := mock.NewScenario("tester review")
	rule := sc.Rule("verdict", mock.Match{Role: "tester", Lens: "qa", Task: "review", ToolPolicy: "read_exec"})
	rule.Steps = []mock.Step{{Kind: mock.StepText, Text: "Ran the suite; two findings."}}
	rule.Result.Structured = tsVerdictJSON(t, want)
	adapter, err := mock.NewAdapter(mock.Name, sc)
	require.NoError(t, err)

	spec, p, en, err := agents.TesterFiring(req, adapter)
	require.NoError(t, err)
	require.NoError(t, en.Check())
	assert.True(t, en.Native(), "the mock enforces the policy itself: %s", en)
	assert.Equal(t, cartridge.ToolReadExec, en.Policy)
	assert.NotEmpty(t, p.Text)

	sess, err := adapter.Run(context.Background(), spec)
	require.NoError(t, err)
	res, err := cartridge.Collect(sess, nil)
	require.NoError(t, err)

	got, parsed, err := agents.ParseTesterVerdict(req, res.Output)
	require.NoError(t, err)
	assert.Equal(t, structured.SourceFence, parsed.Source)
	assert.Equal(t, want, got)
	require.Len(t, adapter.Calls(), 1, "the rule matched the labels and policy the spec carries")
}

// TestTesterVerdictExamplesMatchTheSchema is the schema half of the "schema
// and golden tests per lens and mode" criterion: the known-good answer for
// every lens and mode validates against verdict.schema.json and survives the
// mode's own rules.
func TestTesterVerdictExamplesMatchTheSchema(t *testing.T) {
	t.Parallel()
	for _, lens := range domain.Lenses() {
		for _, mode := range agents.TesterModes() {
			t.Run(lens.String()+"-"+mode.String(), func(t *testing.T) {
				t.Parallel()
				req := tsRequest(lens, mode)
				want := tsGoodVerdict(lens, mode)
				require.NoError(t, agents.ValidateJSON(domain.MessageVerdict, tsVerdictJSON(t, want)))

				got, _, err := agents.ParseTesterVerdict(req, tsOutput(t, want))
				require.NoError(t, err)
				assert.Equal(t, want, got)
				assert.Equal(t, lens, got.Lens)
				assert.Equal(t, req.Round, got.Round)
			})
		}
	}
}

// TestParseTesterVerdictFillsInWhatTheOrchestratorKnows covers the
// normalisation: a tester may omit its own lens, round and item statuses.
func TestParseTesterVerdictFillsInWhatTheOrchestratorKnows(t *testing.T) {
	t.Parallel()
	req := tsRequest(domain.LensUX, agents.TesterModeReview)
	out := "```sc-result\n" + `{"outcome":"changes","summary":"One problem.",` +
		`"items":[{"id":"ux-r1-1","severity":"minor","title":"The error message has no next step"}]}` + "\n```"
	got, _, err := agents.ParseTesterVerdict(req, out)
	require.NoError(t, err)
	assert.Equal(t, domain.LensUX, got.Lens)
	assert.Equal(t, 1, got.Round)
	require.Len(t, got.Items, 1)
	assert.Equal(t, domain.ItemOpen, got.Items[0].Status)

	t.Run("normalise does not mutate the input", func(t *testing.T) {
		v := domain.Verdict{Outcome: domain.VerdictChanges, Items: []domain.FeedbackItem{{ID: "ux-r1-1"}}}
		out := agents.NormaliseTesterVerdict(req, v)
		assert.Empty(t, v.Items[0].Status, "the caller's verdict must be untouched")
		assert.Equal(t, domain.ItemOpen, out.Items[0].Status)
	})
}

// TestParseTesterVerdictRejectsBadOutput covers every rule
// ValidateTesterVerdict adds on top of the JSON Schema, plus the schema
// failures the contract itself catches. Each one must come back as a
// *structured.ParseError so P5-08 can retry with its feedback.
func TestParseTesterVerdictRejectsBadOutput(t *testing.T) {
	t.Parallel()
	items := tsItems(domain.LensQA)
	cases := []struct {
		name    string
		mode    agents.TesterMode
		verdict domain.Verdict
		want    string
	}{
		{
			name: "a rules item that is not a blocker", mode: agents.TesterModeReview,
			verdict: domain.Verdict{Outcome: domain.VerdictChanges, Items: []domain.FeedbackItem{
				{ID: "qa-r1-1", Severity: domain.SeverityMinor, Category: domain.CategoryRules, Title: "No doc comment", Status: domain.ItemOpen},
			}},
			want: "blocker",
		},
		{
			name: "approve with an open item", mode: agents.TesterModeReview,
			verdict: domain.Verdict{Outcome: domain.VerdictApprove, Items: []domain.FeedbackItem{
				{ID: "qa-r1-1", Severity: domain.SeverityMajor, Title: "Sorts in place", Status: domain.ItemOpen},
			}},
			want: "still open or held",
		},
		{
			name: "duplicate item ids", mode: agents.TesterModeReview,
			verdict: domain.Verdict{Outcome: domain.VerdictChanges, Items: []domain.FeedbackItem{
				{ID: "qa-r1-1", Severity: domain.SeverityMajor, Title: "Sorts in place", Status: domain.ItemOpen},
				{ID: "qa-r1-1", Severity: domain.SeverityNit, Title: "Naming", Status: domain.ItemOpen},
			}},
			want: "appears twice",
		},
		{
			name: "another tester's lens", mode: agents.TesterModeReview,
			verdict: domain.Verdict{Lens: domain.LensUX, Outcome: domain.VerdictChanges, Items: []domain.FeedbackItem{
				{ID: "qa-r1-1", Severity: domain.SeverityMajor, Title: "Sorts in place", Status: domain.ItemOpen},
			}},
			want: "you are the \"qa\" tester",
		},
		{
			name: "a first review that settles an item", mode: agents.TesterModeReview,
			verdict: domain.Verdict{Outcome: domain.VerdictChanges, Items: []domain.FeedbackItem{
				{ID: "qa-r1-1", Severity: domain.SeverityMajor, Title: "Sorts in place", Status: domain.ItemResolved},
				{ID: "qa-r1-2", Severity: domain.SeverityMajor, Title: "Still wrong", Status: domain.ItemOpen},
			}},
			want: "first review",
		},
		{
			name: "a tester overruling itself", mode: agents.TesterModeReview,
			verdict: domain.Verdict{Outcome: domain.VerdictChanges, Items: []domain.FeedbackItem{
				{ID: "qa-r1-1", Severity: domain.SeverityMajor, Title: "Sorts in place", Status: domain.ItemOverruled},
				{ID: "qa-r1-2", Severity: domain.SeverityMajor, Title: "Still wrong", Status: domain.ItemOpen},
			}},
			want: "only the product owner overrules",
		},
		{
			name: "changes with nothing left open", mode: agents.TesterModeReReview,
			verdict: domain.Verdict{Outcome: domain.VerdictChanges, Items: []domain.FeedbackItem{
				tsWithStatus(items[0], domain.ItemResolved, "fixed"),
				tsWithStatus(items[1], domain.ItemResolved, "fixed"),
			}},
			want: "every item is settled",
		},
		{
			name: "a re-review that forgets an item", mode: agents.TesterModeReReview,
			verdict: domain.Verdict{Outcome: domain.VerdictChanges, Items: []domain.FeedbackItem{
				tsWithStatus(items[0], domain.ItemOpen, "still sorts in place"),
			}},
			want: "`qa-r1-2` are missing",
		},
		{
			name: "a pushback evaluation that invents an item", mode: agents.TesterModeEvaluatePushback,
			verdict: domain.Verdict{Outcome: domain.VerdictChanges, Items: []domain.FeedbackItem{
				tsWithStatus(items[1], domain.ItemHeld, "rules are rules"),
				{ID: "qa-r2-9", Severity: domain.SeverityMinor, Title: "While I was here", Status: domain.ItemOpen},
			}},
			want: "not one of the items under dispute",
		},
		{
			name: "a pushback evaluation that holds without a reason", mode: agents.TesterModeEvaluatePushback,
			verdict: domain.Verdict{Outcome: domain.VerdictChanges, Items: []domain.FeedbackItem{
				tsWithStatus(items[1], domain.ItemHeld, ""),
			}},
			want: "gives no reason",
		},
		{
			name: "a pushback evaluation that accepts a rules violation", mode: agents.TesterModeEvaluatePushback,
			verdict: domain.Verdict{Outcome: domain.VerdictApprove, Items: []domain.FeedbackItem{
				tsWithStatus(items[1], domain.ItemAccepted, "fair enough"),
			}},
			want: "cannot be accepted",
		},
		{
			name: "a pushback evaluation that leaves an item open", mode: agents.TesterModeEvaluatePushback,
			verdict: domain.Verdict{Outcome: domain.VerdictChanges, Items: []domain.FeedbackItem{
				tsWithStatus(items[1], domain.ItemOpen, "not sure"),
			}},
			want: "accepted or held",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := tsRequest(domain.LensQA, tc.mode)
			_, _, err := agents.ParseTesterVerdict(req, tsOutput(t, tc.verdict))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)

			pe, ok := structured.AsParseError(err)
			require.True(t, ok, "want a *structured.ParseError so P5-08 can retry: %v", err)
			assert.ErrorIs(t, err, structured.ErrSchema)
			assert.Equal(t, "verdict", pe.Contract)
			assert.NotEmpty(t, pe.Feedback())
		})
	}
}

// TestParseTesterVerdictReportsAMissingBlock keeps the contract-level
// failures typed too.
func TestParseTesterVerdictReportsAMissingBlock(t *testing.T) {
	t.Parallel()
	req := tsRequest(domain.LensQA, agents.TesterModeReview)
	_, _, err := agents.ParseTesterVerdict(req, "I had a look and it seems fine to me.")
	require.Error(t, err)
	assert.ErrorIs(t, err, structured.ErrMissing)

	_, _, err = agents.ParseTesterVerdict(agents.TesterRequest{}, "```sc-result\n{}\n```")
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrUnknownValue, "an unusable request fails before parsing")
}

// TestValidateTesterVerdictListsEveryProblem checks that one bad verdict
// comes back with all of its problems, not just the first.
func TestValidateTesterVerdictListsEveryProblem(t *testing.T) {
	t.Parallel()
	req := tsRequest(domain.LensQA, agents.TesterModeReview)
	v := domain.Verdict{Lens: domain.LensTest, Outcome: domain.VerdictApprove, Items: []domain.FeedbackItem{
		{ID: "qa-r1-1", Severity: domain.SeverityMinor, Category: domain.CategoryRules, Title: "No doc comment", Status: domain.ItemOpen},
		{ID: "qa-r1-1", Severity: domain.SeverityMajor, Title: "", Status: domain.ItemOpen},
	}}
	err := agents.ValidateTesterVerdict(req, v)
	require.Error(t, err)
	pe, ok := structured.AsParseError(err)
	require.True(t, ok)
	assert.GreaterOrEqual(t, len(pe.Problems), 4, "want every problem listed: %v", pe.Problems)
	joined := strings.Join(pe.Problems, "\n")
	assert.Contains(t, joined, "lens")
	assert.Contains(t, joined, "always a blocker")
	assert.Contains(t, joined, "appears twice")
	assert.Contains(t, joined, "no title")
	assert.Contains(t, joined, "still open or held")
	assert.NoError(t, agents.ValidateTesterVerdict(req, tsGoodVerdict(domain.LensQA, agents.TesterModeReview)))
}

func TestTesterPushbackAndOpenItems(t *testing.T) {
	t.Parallel()
	items := tsItems(domain.LensQA)
	pushed := agents.TesterPushbackItems(items, tsResponses(domain.LensQA))
	require.Len(t, pushed, 1)
	assert.Equal(t, "qa-r1-2", pushed[0].ID)
	assert.Empty(t, agents.TesterPushbackItems(items, nil), "no responses, no dispute")
	assert.Len(t, agents.TesterPushbackItems(items, []domain.Response{
		{ItemID: "qa-r1-1", Action: domain.ActionWontfix, Rationale: "out of scope"},
		{ItemID: "qa-r1-2", Action: domain.ActionPushback, Rationale: "obvious"},
	}), 2)

	open := agents.TesterOpenItems(domain.Verdict{Items: []domain.FeedbackItem{
		tsWithStatus(items[0], domain.ItemResolved, "fixed"),
		tsWithStatus(items[1], domain.ItemHeld, "rules are rules"),
		{ID: "qa-r2-1", Severity: domain.SeverityMinor, Title: "New", Status: domain.ItemOpen},
	}})
	require.Len(t, open, 2)
	assert.Equal(t, "qa-r1-2", open[0].ID)
	assert.Equal(t, "qa-r2-1", open[1].ID)
	assert.Empty(t, agents.TesterOpenItems(domain.Verdict{Outcome: domain.VerdictApprove}))
}

// TestTesterRequestAgent checks the identity the labels and the event stream
// are built from.
func TestTesterRequestAgent(t *testing.T) {
	t.Parallel()
	req := tsRequest(domain.LensTest, agents.TesterModeReview)
	req.Model = "opus"
	a := req.Agent()
	assert.Equal(t, domain.RoleTester, a.Role)
	assert.Equal(t, domain.LensTest, a.Lens)
	assert.Equal(t, domain.BeadID("sc-7a1"), a.Bead)
	assert.Equal(t, "opus", a.Model)
	assert.Equal(t, domain.AgentID("tester-test-sc-7a1"), a.ID)
}

// TestTesterPromptIsWrittenForDebugging checks the D-23 debug copy path
// works for a tester prompt (the run dir is never inside the target repo).
func TestTesterPromptIsWrittenForDebugging(t *testing.T) {
	t.Parallel()
	runDir := t.TempDir()
	p, err := agents.BuildTesterPrompt(tsRequest(domain.LensQA, agents.TesterModeReview))
	require.NoError(t, err)
	require.NoError(t, p.Write(runDir))
	assert.Equal(t, filepath.Join(runDir, prompt.PromptsDirName, "tester-qa-sc-7a1"+prompt.PromptFileExt), p.Path)
	b, err := os.ReadFile(p.Path)
	require.NoError(t, err)
	assert.Equal(t, p.Text, string(b))
}

// TestTesterPromptIsDeterministic keeps the goldens meaningful.
func TestTesterPromptIsDeterministic(t *testing.T) {
	t.Parallel()
	for _, mode := range agents.TesterModes() {
		a, err := agents.BuildTesterPrompt(tsRequest(domain.LensQA, mode))
		require.NoError(t, err)
		b, err := agents.BuildTesterPrompt(tsRequest(domain.LensQA, mode))
		require.NoError(t, err)
		assert.Equal(t, a.Text, b.Text, mode)
	}
}

// TestTesterFiringRefusesAnUnenforceablePolicy checks the P5-06 caller
// contract: a cartridge that cannot hold read_exec never gets fired.
func TestTesterFiringRefusesAnUnenforceablePolicy(t *testing.T) {
	t.Parallel()
	req := tsRequest(domain.LensQA, agents.TesterModeReview)
	req.Workdir = t.TempDir()
	_, _, en, err := agents.TesterFiring(req, tsToollessCartridge{})
	require.Error(t, err)
	assert.ErrorIs(t, err, agents.ErrCopyWorktreeUnimplemented)
	assert.Equal(t, agents.EnforceCopyWorktree, en.Mode)

	_, _, _, err = agents.TesterFiring(req, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil cartridge")
}

// tsToollessCartridge stands in for a future adapter with no tool controls.
type tsToollessCartridge struct{}

func (tsToollessCartridge) Name() string { return "toolless-tester-stub" }

func (tsToollessCartridge) Detect(context.Context) (cartridge.Status, error) {
	return cartridge.Status{Installed: true, Detail: "toolless-tester-stub"}, nil
}

func (tsToollessCartridge) Run(context.Context, cartridge.RunSpec) (cartridge.Session, error) {
	return nil, errors.New("toolless-tester-stub: never fired")
}

// TestParseTesterVerdictAcceptsASoundPushback is the positive half of the
// pushback rules: an ordinary item may be accepted, which settles it, and a
// verdict with nothing left unsettled approves.
func TestParseTesterVerdictAcceptsASoundPushback(t *testing.T) {
	t.Parallel()
	item := domain.FeedbackItem{
		ID: "ux-r1-1", Severity: domain.SeverityMinor, Category: "wording",
		Title: "The flag name reads oddly", Status: domain.ItemOpen,
	}
	req := tsRequest(domain.LensUX, agents.TesterModeEvaluatePushback)
	req.Items = []domain.FeedbackItem{item}
	req.Responses = []domain.Response{{ItemID: item.ID, Action: domain.ActionWontfix, Rationale: "The name matches the docs."}}
	require.NoError(t, req.Validate())

	v := domain.Verdict{Outcome: domain.VerdictApprove, Summary: "Fair point.", Items: []domain.FeedbackItem{
		tsWithStatus(item, domain.ItemAccepted, "The docs do use that name; I withdraw the item."),
	}}
	got, _, err := agents.ParseTesterVerdict(req, tsOutput(t, v))
	require.NoError(t, err)
	assert.Equal(t, domain.VerdictApprove, got.Outcome)
	assert.Empty(t, agents.TesterOpenItems(got))

	p, err := agents.BuildTesterPrompt(req)
	require.NoError(t, err)
	assert.Contains(t, p.Text, "Engineer: **wontfix**")
}

// TestTesterPromptShowsOrchestratorNotes covers the notes layer and an item
// the engineer never answered.
func TestTesterPromptShowsOrchestratorNotes(t *testing.T) {
	t.Parallel()
	req := tsRequest(domain.LensQA, agents.TesterModeReReview)
	req.Notes = []string{"The check command failed at delivery time.", "  ", ""}
	req.Responses = req.Responses[:1]
	p, err := agents.BuildTesterPrompt(req)
	require.NoError(t, err)
	assert.Contains(t, p.Text, "## Notes from the orchestrator")
	assert.Contains(t, p.Text, "- The check command failed at delivery time.")
	assert.Contains(t, p.Text, "Engineer: no response to this item.")

	req.Notes = []string{" ", ""}
	p, err = agents.BuildTesterPrompt(req)
	require.NoError(t, err)
	assert.NotContains(t, p.Text, "Notes from the orchestrator", "blank notes add no section")
}

// tsLargePatch builds a plausible unified diff of at least n bytes, so the
// prompt budget has something real to push back on.
func tsLargePatch(n int) string {
	var b strings.Builder
	b.WriteString("--- a/stats/stats.go\n+++ b/stats/stats.go\n@@ -40,6 +40,17 @@\n")
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "+\tsum += xs[%d] * weights[%d] // accumulate the weighted sample %d\n", i, i, i)
	}
	return b.String()
}

// tsLargeDiff is a diff whose patch is under gitx's byte cap — so gitx does
// not mark it truncated — but far over any sane prompt budget.
func tsLargeDiff(n int) *gitx.Diff {
	d := tsDiff()
	d.Patch = tsLargePatch(n)
	d.Bytes = len(d.Patch)
	return d
}

// TestTesterReReviewRefusesASettledRulesItem is the re-review half of the
// rules rule: a `rules` blocker can never be accepted, whichever mode is
// judging it, and it may not be rewritten into something smaller either.
func TestTesterReReviewRefusesASettledRulesItem(t *testing.T) {
	t.Parallel()
	items := tsItems(domain.LensQA)
	rules := items[1] // qa-r1-2, category rules, severity blocker

	cases := []struct {
		name    string
		items   []domain.FeedbackItem
		outcome domain.VerdictOutcome
		want    string
	}{
		{
			name:    "accepting the rules item",
			outcome: domain.VerdictApprove,
			items: []domain.FeedbackItem{
				tsWithStatus(items[0], domain.ItemResolved, "Median now copies before sorting."),
				tsWithStatus(rules, domain.ItemAccepted, "Fair enough, the name says it all."),
			},
			want: "cannot be accepted",
		},
		{
			name:    "downgrading the rules item and calling it resolved",
			outcome: domain.VerdictApprove,
			items: []domain.FeedbackItem{
				tsWithStatus(items[0], domain.ItemResolved, "Median now copies before sorting."),
				func() domain.FeedbackItem {
					it := tsWithStatus(rules, domain.ItemResolved, "Downgraded to taste.")
					it.Category = "style"
					it.Severity = domain.SeverityNit
					return it
				}(),
			},
			want: "original severity",
		},
		{
			name:    "dropping the rules category off a carried item",
			outcome: domain.VerdictChanges,
			items: []domain.FeedbackItem{
				tsWithStatus(items[0], domain.ItemOpen, "still sorts in place"),
				func() domain.FeedbackItem {
					it := tsWithStatus(rules, domain.ItemAccepted, "Not really a rules thing.")
					it.Category = "docs"
					return it
				}(),
			},
			want: "original category",
		},
		{
			name:    "a new item born settled",
			outcome: domain.VerdictApprove,
			items: []domain.FeedbackItem{
				tsWithStatus(items[0], domain.ItemResolved, "Median now copies before sorting."),
				tsWithStatus(rules, domain.ItemResolved, "The doc comment is there now."),
				{ID: "qa-r2-9", Severity: domain.SeverityMajor, Title: "New thing", Detail: "Noticed in passing.", Status: domain.ItemAccepted},
			},
			want: "a new problem starts open",
		},
		{
			name:    "holding a carried item without a reason",
			outcome: domain.VerdictChanges,
			items: []domain.FeedbackItem{
				tsWithStatus(items[0], domain.ItemResolved, "Median now copies before sorting."),
				tsWithStatus(rules, domain.ItemHeld, "   "),
			},
			want: "gives no reason",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := tsRequest(domain.LensQA, agents.TesterModeReReview)
			v := domain.Verdict{Outcome: tc.outcome, Summary: "Round two.", Items: tc.items}

			err := agents.ValidateTesterVerdict(req, agents.NormaliseTesterVerdict(req, v))
			require.Error(t, err, "the verdict must not validate")
			assert.Contains(t, err.Error(), tc.want)
			pe, ok := structured.AsParseError(err)
			require.True(t, ok, "want a *structured.ParseError so P5-08 can retry: %v", err)
			assert.NotEmpty(t, pe.Feedback())

			_, _, err = agents.ParseTesterVerdict(req, tsOutput(t, v))
			require.Error(t, err, "ParseTesterVerdict must refuse what ValidateTesterVerdict refuses")
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestTesterReReviewAcceptsAWellFormedRound is the positive half: an
// ordinary item may be accepted on a re-review, a new item may be raised as
// long as it is open, and a carried item that does not restate its category
// is not treated as a rewrite.
func TestTesterReReviewAcceptsAWellFormedRound(t *testing.T) {
	t.Parallel()
	req := tsRequest(domain.LensUX, agents.TesterModeReReview)
	items := tsItems(domain.LensUX)
	carried := tsWithStatus(items[0], domain.ItemAccepted, "The pushback is right: callers do own the slice.")
	carried.Category = "" // the schema lets a tester omit it

	v := domain.Verdict{Outcome: domain.VerdictChanges, Summary: "One left.", Items: []domain.FeedbackItem{
		carried,
		tsWithStatus(items[1], domain.ItemOpen, "The doc comment is still missing."),
		{ID: "ux-r2-1", Severity: domain.SeverityMinor, Title: "The error text has no next step", Detail: "Say what to do.", Status: domain.ItemOpen},
	}}
	got, _, err := agents.ParseTesterVerdict(req, tsOutput(t, v))
	require.NoError(t, err)
	assert.Equal(t, "correctness", got.Items[0].Category, "an omitted category is filled in from the item you were given")
	assert.Equal(t, domain.ItemAccepted, got.Items[0].Status)
	require.Len(t, agents.TesterOpenItems(got), 2)
}

// TestTesterReReviewPromptSaysRulesCannotBeAccepted pins the wording that
// keeps the model out of the hole the validator now closes.
func TestTesterReReviewPromptSaysRulesCannotBeAccepted(t *testing.T) {
	t.Parallel()
	for _, lens := range domain.Lenses() {
		p, err := agents.BuildTesterPrompt(tsRequest(lens, agents.TesterModeReReview))
		require.NoError(t, err)
		assert.Contains(t, p.Text, "cannot be `accepted` and cannot be downgraded", lens)
		assert.Contains(t, p.Text, "**original id**, `severity` and `category`", lens)
		assert.Contains(t, p.Text, "only an item you raised before may be settled", lens)
	}
}

// TestTesterPromptShrinksAnOversizedDiff covers the diff ladder: a patch
// under gitx's cap but over the prompt budget is cut down rather than fired
// whole, and what was cut is reported in Truncations.
func TestTesterPromptShrinksAnOversizedDiff(t *testing.T) {
	t.Parallel()
	req := tsRequest(domain.LensQA, agents.TesterModeReview)
	req.Diff = tsLargeDiff(150 << 10)
	require.False(t, req.Diff.Truncated, "gitx keeps a patch this size, so only the prompt budget can bound it")

	p, err := agents.BuildTesterPrompt(req)
	require.NoError(t, err)
	assert.True(t, p.Fits(), "the ladder must bring the prompt inside the budget: %d/%d", p.Tokens, p.MaxTokens)
	assert.LessOrEqual(t, p.Tokens, p.MaxTokens)
	assert.Less(t, len(p.Text), len(req.Diff.Patch), "the whole patch is still in the prompt")
	assert.Contains(t, p.Text, "too large for this prompt's budget")
	assert.Contains(t, strings.Join(p.Truncations, "\n"), "diff patch",
		"the caller must be told the diff was cut: %v", p.Truncations)

	t.Run("the summary survives when even a slice does not fit", func(t *testing.T) {
		t.Parallel()
		small := tsRequest(domain.LensQA, agents.TesterModeReview)
		small.Diff = tsLargeDiff(150 << 10)
		small.Budget = prompt.Budget{MaxTokens: 2500}
		p, err := agents.BuildTesterPrompt(small)
		require.NoError(t, err)
		assert.NotContains(t, p.Text, "```diff", "the patch is gone")
		assert.Contains(t, p.Text, "M stats/stats.go  +12 -1", "the per-file summary stays")
		assert.Contains(t, strings.Join(p.Truncations, "\n"), "diff patch omitted")
	})

	t.Run("a diff that fits is untouched", func(t *testing.T) {
		t.Parallel()
		p, err := agents.BuildTesterPrompt(tsRequest(domain.LensQA, agents.TesterModeReview))
		require.NoError(t, err)
		assert.Contains(t, p.Text, "func Median(xs []float64) float64 {")
		assert.NotContains(t, p.Text, "too large for this prompt's budget")
		assert.Empty(t, p.Truncations)
	})
}

// TestTesterRunSpecRefusesAnOverBudgetPrompt is the other half of the budget
// fix: when even the most degraded prompt is over budget the caller is told,
// rather than firing a prompt nobody measured.
func TestTesterRunSpecRefusesAnOverBudgetPrompt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	req := tsRequest(domain.LensQA, agents.TesterModeReview)
	req.Workdir = dir
	req.Diff = tsLargeDiff(150 << 10)
	req.Budget = prompt.Budget{MaxTokens: 200}

	p, err := agents.BuildTesterPrompt(req)
	require.NoError(t, err)
	require.False(t, p.Fits(), "the fixture must be over budget for this test to mean anything")

	spec, p, err := agents.TesterRunSpec(req)
	require.Error(t, err)
	assert.ErrorIs(t, err, agents.ErrPromptOverBudget)
	assert.Contains(t, err.Error(), "200")
	assert.NoError(t, spec.Validate(), "the spec is still usable; the caller decides whether to fire it")
	assert.Equal(t, p.Text, spec.Prompt)

	t.Run("the firing checks the tool policy first", func(t *testing.T) {
		t.Parallel()
		adapter, err := mock.NewAdapter(mock.Name, mock.NewScenario("over budget"))
		require.NoError(t, err)
		spec, _, en, err := agents.TesterFiring(req, adapter)
		require.Error(t, err)
		assert.ErrorIs(t, err, agents.ErrPromptOverBudget)
		assert.Equal(t, cartridge.ToolReadExec, en.Policy)
		assert.NoError(t, en.Check(), "the policy holds; only the budget does not")
		assert.NoError(t, spec.Validate())

		_, _, _, err = agents.TesterFiring(req, tsToollessCartridge{})
		require.Error(t, err)
		assert.ErrorIs(t, err, agents.ErrCopyWorktreeUnimplemented,
			"an unenforceable policy outranks an over-budget prompt")
	})

	t.Run("a prompt inside its budget fires without an error", func(t *testing.T) {
		t.Parallel()
		ok := tsRequest(domain.LensQA, agents.TesterModeReview)
		ok.Workdir = dir
		_, p, err := agents.TesterRunSpec(ok)
		require.NoError(t, err)
		assert.True(t, p.Fits())
	})
}
