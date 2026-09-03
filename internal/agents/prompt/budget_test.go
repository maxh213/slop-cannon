package prompt

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
)

// pbBigRepo returns a repository context with a large tree and README, so
// the budget has something to cut.
func pbBigRepo(entries int) RepoContext {
	rc := pbRepo()
	rc.Tree = make([]string, 0, entries)
	for i := range entries {
		rc.Tree = append(rc.Tree, fmt.Sprintf("pkg/mod%03d/file%03d.go", i/10, i))
	}
	rc.TreeTotal = entries
	rc.ReadmeHead = strings.Repeat("a paragraph of readme prose that says very little\n", 200)
	return rc
}

// pbRounds returns n review rounds, each carrying a verdict big enough to
// matter to the budget.
func pbRounds(n int) []Round {
	rounds := make([]Round, 0, n)
	for i := 1; i <= n; i++ {
		rounds = append(rounds, Round{
			Number: i,
			Verdicts: []domain.Verdict{{
				Lens:    domain.LensQA,
				Round:   i,
				Outcome: domain.VerdictChanges,
				Summary: strings.Repeat("something to fix in this round. ", 40),
				Items: []domain.FeedbackItem{{
					ID:       fmt.Sprintf("qa-r%d-1", i),
					Severity: domain.SeverityMajor,
					Title:    strings.Repeat("a long finding title ", 20),
					Status:   domain.ItemOpen,
				}},
			}},
		})
	}
	return rounds
}

func TestBudgetEstimate(t *testing.T) {
	assert.Equal(t, 0, EstimateTokens(""))
	assert.Equal(t, 1, EstimateTokens("abc"))
	assert.Equal(t, 1, EstimateTokens("abcd"))
	assert.Equal(t, 2, EstimateTokens("abcde"))
	assert.Equal(t, 5, Budget{CharsPerToken: 2}.Estimate("0123456789"))
	assert.Equal(t, DefaultMaxTokens, Budget{}.withDefaults().MaxTokens)
	assert.Equal(t, DefaultCharsPerToken, Budget{}.withDefaults().CharsPerToken)
}

// TestBuildTruncatesTreeFirst is the P5-01 truncation order: the repository
// tree goes before the round history.
func TestBuildTruncatesTreeFirst(t *testing.T) {
	req := pbEngineerRequest()
	req.Repo = pbBigRepo(1200)
	req.History = pbRounds(6)
	full, err := Build(req)
	require.NoError(t, err)
	require.Empty(t, full.Truncations, "the fixture must fit at the default budget")

	// A budget that only the tree needs to give way for.
	req.Budget = Budget{MaxTokens: full.Tokens - 1000}
	p, err := Build(req)
	require.NoError(t, err)
	require.NotEmpty(t, p.Truncations)
	assert.Contains(t, p.Truncations[0], "repository tree cut to")
	assert.False(t, p.OverBudget)
	assert.LessOrEqual(t, p.Tokens, p.MaxTokens)
	assert.Contains(t, p.Text, "### Round 1", "history survives while the tree can still shrink")
}

func TestBuildTruncationLadder(t *testing.T) {
	req := pbEngineerRequest()
	req.Repo = pbBigRepo(1200)
	req.History = pbRounds(6)
	req.OpenBeads = []domain.Bead{{ID: "sc-1", Title: "one"}, {ID: "sc-2", Title: "two"}}
	req.Budget = Budget{MaxTokens: 900}

	p, err := Build(req)
	require.NoError(t, err)
	require.NotEmpty(t, p.Truncations)
	joined := strings.Join(p.Truncations, "\n")
	assert.Contains(t, joined, "repository tree omitted")
	assert.Contains(t, joined, "dropped the")
	assert.NotContains(t, p.Text, "pkg/mod000/file000.go")
	assert.NotContains(t, p.Text, "### Round 1")
	assert.Contains(t, p.Text, "### Round 6", "the newest round is kept longest")
	assert.Contains(t, p.Text, "round(s) omitted to fit the prompt budget")

	// Rules, vibes and the output contract are never truncated.
	assert.Contains(t, p.Text, "Never disable or delete a test")
	assert.Contains(t, p.Text, "British spelling in prose")
	assert.True(t, pbContract.HasPromptSuffix(p.Text))
}

// TestBuildOverBudgetIsReportedNotFixed: when even the smallest prompt is
// too big the caller is told, and the prompt is still complete.
func TestBuildOverBudgetIsReportedNotFixed(t *testing.T) {
	req := pbEngineerRequest()
	req.Budget = Budget{MaxTokens: 10}
	p, err := Build(req)
	require.NoError(t, err)
	assert.True(t, p.OverBudget)
	assert.False(t, p.Fits())
	assert.Contains(t, p.Text, "Never disable or delete a test")
	assert.True(t, pbContract.HasPromptSuffix(p.Text))
}

func TestDegradationsSkipUselessSteps(t *testing.T) {
	req := Request{Role: domain.RolePO}
	steps := degradations(req, fullKnobs(req))
	assert.Len(t, steps, 1, "nothing to degrade means one candidate")

	req.Repo = pbBigRepo(300)
	req.History = pbRounds(2)
	req.OpenBeads = []domain.Bead{{ID: "sc-1"}}
	steps = degradations(req, fullKnobs(req))
	assert.Greater(t, len(steps), 1)
	seen := map[knobs]bool{}
	for _, s := range steps {
		assert.False(t, seen[s], "duplicate candidate %+v", s)
		seen[s] = true
	}
	last := steps[len(steps)-1]
	assert.Equal(t, 0, last.tree)
	assert.Equal(t, 1, last.rounds)
}

func TestDescribeTruncations(t *testing.T) {
	req := pbEngineerRequest()
	req.Repo = pbBigRepo(100)
	req.History = pbRounds(4)
	req.OpenBeads = []domain.Bead{{ID: "sc-1"}, {ID: "sc-2"}}
	full := fullKnobs(req)

	got := describeTruncations(req, full, knobs{tree: 20, rounds: 2, readme: 1024, openBeads: 1})
	require.Len(t, got, 4)
	assert.Equal(t, "repository tree cut to 20 of 100 entries", got[0])
	assert.Equal(t, "dropped the 2 oldest review round(s), kept 2", got[1])
	assert.Contains(t, got[2], "README head cut to 1024 of")
	assert.Equal(t, "open-bead list cut to 1 of 2 beads", got[3])

	got = describeTruncations(req, full, knobs{tree: 0, rounds: -1, readme: 0, openBeads: 0})
	assert.Equal(t, []string{
		"repository tree omitted (100 entries)",
		"README head omitted",
		"open-bead list omitted (2 beads)",
	}, got)

	assert.Empty(t, describeTruncations(req, full, full))
}

func TestRenderOpenBeadsLimit(t *testing.T) {
	beads := []domain.Bead{
		{ID: "sc-1", Title: "one", State: domain.BeadStateOpen},
		{ID: "sc-2", Title: "two", State: domain.BeadStateOpen},
		{ID: "sc-3", Title: "three", State: domain.BeadStateOpen},
	}
	out := renderOpenBeads(beads, 2)
	assert.Contains(t, out, "`sc-1`")
	assert.Contains(t, out, "… and 1 more not listed here")
	assert.NotContains(t, out, "three")
	assert.Empty(t, renderOpenBeads(beads, 0))
	assert.Empty(t, renderOpenBeads(nil, -1))
}
