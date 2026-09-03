package prompt

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

// pcxOpenFence and pcxCloseFence recognise CommonMark code fences the way
// structured.Extract does: three or more backticks or tildes, indented by at
// most three spaces.
var (
	pcxOpenFence  = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
	pcxCloseFence = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})\\s*$")
)

// pcxOutsideFences returns the lines of text that are not inside a fenced
// code block, which is exactly the set of lines a reader (or an agent) sees
// as prompt structure. An unbalanced block therefore swallows the headings
// after it and they disappear from the result.
func pcxOutsideFences(t *testing.T, text string) []string {
	t.Helper()
	var out []string
	var fence string
	for _, line := range strings.Split(text, "\n") {
		if fence == "" {
			if m := pcxOpenFence.FindStringSubmatch(line); m != nil {
				fence = m[1]
				continue
			}
			out = append(out, line)
			continue
		}
		if m := pcxCloseFence.FindStringSubmatch(line); m != nil && m[1][0] == fence[0] && len(m[1]) >= len(fence) {
			fence = ""
		}
	}
	if fence != "" {
		t.Fatalf("prompt ends with an unclosed %q block", fence)
	}
	return out
}

// pcxHasLine reports whether want is one of the lines.
func pcxHasLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// TestBuildQuotesAREADMEThatContainsCodeFences is the regression test for the
// fence-escaping defect: the project's own fixture README has ```sh blocks,
// and a three-backtick wrapper would let them close the block early, hoisting
// the README's own headings to prompt top level and swallowing everything
// from "## Open beads" to the output contract.
func TestBuildQuotesAREADMEThatContainsCodeFences(t *testing.T) {
	repo := testutil.NewFixtureRepo(t)
	rc, err := ScanRepo(repo.Dir, ScanOptions{DefaultBranch: repo.DefaultBranch})
	require.NoError(t, err)
	require.Contains(t, rc.ReadmeHead, "```sh", "the fixture README must contain a code fence")

	req := pbPORequest()
	req.Repo = rc
	p, err := Build(req)
	require.NoError(t, err)

	lines := pcxOutsideFences(t, p.Text)
	for _, heading := range []string{
		"# Product owner — plan",
		"## Repository context",
		"## Open beads",
		"## What to produce",
		"## Output contract (demo_result)",
	} {
		assert.True(t, pcxHasLine(lines, heading), "%q is not a top-level line of the prompt", heading)
	}
	assert.Contains(t, p.Text, "Do not propose work that duplicates any of these:")

	// The README's own structure stays inside the quoted block.
	for _, readmeLine := range []string{"## Usage", "## Development", "## Status", "```sh"} {
		assert.False(t, pcxHasLine(lines, readmeLine),
			"%q escaped the README block and reads as prompt structure", readmeLine)
	}
	assert.Contains(t, p.Text, "````\n# fixture", "the README is wrapped in a longer fence")
}

// TestBuildQuotesADiffThatContainsCodeFences covers the same defect on the
// round-history diff, which is where a change to a Markdown file lands.
func TestBuildQuotesADiffThatContainsCodeFences(t *testing.T) {
	req := pbEngineerRequest()
	req.History[0].Diff = "@@ README.md @@\n+```sh\n+make test\n+```"

	p, err := Build(req)
	require.NoError(t, err)

	lines := pcxOutsideFences(t, p.Text)
	for _, heading := range []string{"## Round history", "## How to finish", "## Output contract (demo_result)"} {
		assert.True(t, pcxHasLine(lines, heading), "%q is not a top-level line of the prompt", heading)
	}
	assert.Contains(t, p.Text, "````diff\n@@ README.md @@", "the diff is wrapped in a longer fence")
	assert.Contains(t, p.Text, "+make test")
}

// TestBuildQuotesATreeThatContainsBackticks covers a repository with a
// backtick in a path, which would otherwise break the tree block.
func TestBuildQuotesATreeThatContainsBackticks(t *testing.T) {
	req := pbPORequest()
	req.Repo.Tree = []string{"docs/", "docs/````weird`` name.md", "main.go"}
	req.Repo.TreeTotal = 3

	p, err := Build(req)
	require.NoError(t, err)

	lines := pcxOutsideFences(t, p.Text)
	assert.True(t, pcxHasLine(lines, "## Open beads"), "the tree block swallowed the sections after it")
	assert.Contains(t, p.Text, "`````\ndocs/", "the tree fence is longer than the longest run inside it")
}

func TestFencedPicksAFenceTheBodyCannotClose(t *testing.T) {
	cases := []struct {
		name, info, body, want string
	}{
		{"plain", "", "a\nb", "```\na\nb\n```\n"},
		{"tagged", "diff", "+x", "```diff\n+x\n```\n"},
		{"one backtick", "", "`x`", "```\n`x`\n```\n"},
		{"three backticks", "", "```sh\nls\n```", "````\n```sh\nls\n```\n````\n"},
		{"four backticks", "", "````", "`````\n````\n`````\n"},
		{"tildes do not close a backtick block", "", "~~~~", "```\n~~~~\n```\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Fenced(tc.info, tc.body))
		})
	}
}

func TestFenceForCountsTheLongestRun(t *testing.T) {
	assert.Equal(t, "```", fenceFor(""))
	assert.Equal(t, "```", fenceFor("a `b` c"))
	assert.Equal(t, "```", fenceFor("a ``b`` c"))
	assert.Equal(t, "````", fenceFor("a ```b``` c"))
	assert.Equal(t, "``````", fenceFor("`````"))
}

// TestLimitReadmeKeepsWholeRunes is the regression test for the byte-boundary
// cut: a README head whose kept window holds no newline used to end in half a
// rune.
func TestLimitReadmeKeepsWholeRunes(t *testing.T) {
	head := "é" + strings.Repeat("x", 10) + "…"
	got := limitReadme(head, len(head)-1)
	assert.True(t, utf8.ValidString(got), "limitReadme returned invalid UTF-8: %q", got)
	assert.Equal(t, "é"+strings.Repeat("x", 10), got)

	for limit := range len(head) + 2 {
		assert.True(t, utf8.ValidString(limitReadme(head, limit)),
			"limitReadme(head, %d) returned invalid UTF-8", limit)
	}
	assert.Equal(t, head, limitReadme(head, -1))
	assert.Empty(t, limitReadme(head, 0))
	assert.Equal(t, "first", limitReadme("first\nsecond line", 9), "a kept newline still cuts at the line")
}

// TestLimitReadmeThroughTheBudget checks the same cut through the degradation
// ladder, where limitReadme is actually reached (readmeSteps caps at 1024).
func TestLimitReadmeThroughTheBudget(t *testing.T) {
	head := "# é" + strings.Repeat("ü", 4000)
	req := Request{
		Role:       domain.RolePO,
		AgentID:    "po-1",
		ToolPolicy: cartridge.ToolReadOnly,
		Repo:       RepoContext{Root: "/work/fixture", ReadmeName: "README.md", ReadmeHead: head},
	}
	full, err := Build(req)
	require.NoError(t, err)
	require.Empty(t, full.Truncations)

	// A budget just under the untruncated prompt forces the README step.
	req.Budget = Budget{MaxTokens: full.Tokens - 500}
	p, err := Build(req)
	require.NoError(t, err)
	assert.False(t, p.OverBudget)
	assert.True(t, utf8.ValidString(p.Text), "the prompt is not valid UTF-8")
	assert.Equal(t, []string{fmt.Sprintf("README head cut to 1024 of %d characters", len(head))}, p.Truncations)
}

// pcxRulingRound is a round that ended in arbitration and an escalation, the
// D-08 rulings the next round's engineer and testers must see.
func pcxRulingRound() Round {
	return Round{
		Number: 2,
		Arbitrations: []domain.Arbitration{
			{
				ItemID:      "qa-r2-1",
				Ruling:      domain.RulingUpholdTester,
				Instruction: "Copy the slice before sorting.",
				Rationale:   "The caller's slice is the caller's.",
			},
			{ItemID: "ux-r2-1", Ruling: domain.RulingUpholdEngineer},
		},
		Escalation: &domain.Escalation{
			Outcome:       domain.EscalationNarrowScope,
			Notes:         "round cap reached",
			NewAcceptance: "Median only has to handle odd lengths.",
		},
	}
}

// TestRenderHistoryRendersRulings covers the arbitration and escalation
// branches, which no other test reaches.
func TestRenderHistoryRendersRulings(t *testing.T) {
	got := renderHistory([]Round{pcxRulingRound()}, -1)

	assert.Contains(t, got,
		"Product owner ruling on `qa-r2-1`: uphold_tester — Copy the slice before sorting. (The caller's slice is the caller's.)")
	assert.Contains(t, got, "Product owner ruling on `ux-r2-1`: uphold_engineer\n",
		"a ruling with no instruction leaves no stray punctuation")
	assert.NotContains(t, got, "uphold_engineer (")
	assert.Contains(t, got,
		"Product owner escalation ruling: narrow_scope — round cap reached; new acceptance: Median only has to handle odd lengths.")
}

// TestRenderHistoryEscalationClauses checks every combination of the
// escalation fields reads as one sentence rather than two run together.
func TestRenderHistoryEscalationClauses(t *testing.T) {
	cases := []struct {
		name string
		esc  domain.Escalation
		want string
	}{
		{"outcome only", domain.Escalation{Outcome: domain.EscalationApproveWithNotes},
			"Product owner escalation ruling: approve_with_notes\n"},
		{"notes only", domain.Escalation{Outcome: domain.EscalationApproveWithNotes, Notes: "ship it"},
			"Product owner escalation ruling: approve_with_notes — ship it\n"},
		{"new acceptance only", domain.Escalation{Outcome: domain.EscalationNarrowScope, NewAcceptance: "odd lengths"},
			"Product owner escalation ruling: narrow_scope — new acceptance: odd lengths\n"},
		{"block with a summary", domain.Escalation{Outcome: domain.EscalationBlock, Notes: "stuck", Summary: "needs a human"},
			"Product owner escalation ruling: block — stuck; for the human: needs a human\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			esc := tc.esc
			got := renderHistory([]Round{{Number: 1, Escalation: &esc}}, -1)
			assert.Contains(t, got+"\n", tc.want)
		})
	}
}

// TestBuildCarriesRulingsIntoTheNextPrompt is the point of the two branches:
// the rulings must reach the engineer and every tester lens (D-08).
func TestBuildCarriesRulingsIntoTheNextPrompt(t *testing.T) {
	requests := map[string]Request{
		"engineer": pbEngineerRequest(),
		"tester":   pbTesterRequest(domain.LensQA),
	}
	for name, req := range requests {
		t.Run(name, func(t *testing.T) {
			req.History = append(req.History, pcxRulingRound())
			p, err := Build(req)
			require.NoError(t, err)
			assert.Contains(t, p.Text, "Product owner ruling on `qa-r2-1`: uphold_tester")
			assert.Contains(t, p.Text, "Product owner escalation ruling: narrow_scope")
			assert.Contains(t, p.Text, "Median only has to handle odd lengths.")
		})
	}
}
