package agents_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/gitx"
)

// fenceMarkdownDiff is a patch that changes a Markdown file, so the diff
// body itself contains a three-backtick fence. Quoted with a plain "```"
// wrapper it would close the block early and the sections after it — the
// output contract among them — would read as prose.
func fenceMarkdownDiff() *gitx.Diff {
	patch := "--- a/README.md\n+++ b/README.md\n@@ -1,3 +1,7 @@\n" +
		"+Run it with:\n+\n+```sh\n+sc .\n+```\n"
	return &gitx.Diff{
		Stats: gitx.DiffStats{
			Base: "1a2b3c4", Head: "9f8e7d6", Files: 1, Insertions: 5,
			Changed: []gitx.FileStat{{Path: "README.md", Insertions: 5}},
		},
		Files:    []gitx.DiffFile{{FileStat: gitx.FileStat{Path: "README.md", Insertions: 5}, Status: gitx.DiffModified}},
		Bytes:    len(patch),
		MaxBytes: gitx.DefaultMaxDiffBytes,
		Patch:    patch,
	}
}

// TestTesterPromptQuotesADiffThatCarriesAFence pins that the tester's diff
// block survives a patch containing its own Markdown fence: the wrapper
// grows past the longest run inside the body (prompt.Fenced), so the
// headings after the diff stay top-level and the output contract is still
// the last thing in the prompt.
func TestTesterPromptQuotesADiffThatCarriesAFence(t *testing.T) {
	t.Parallel()
	req := tsRequest(domain.LensQA, agents.TesterModeReview)
	req.Diff = fenceMarkdownDiff()
	p, err := agents.BuildTesterPrompt(req)
	require.NoError(t, err)

	assert.Contains(t, p.Text, "````diff\n", "the fence is longer than the one inside the patch")
	assert.Contains(t, p.Text, "+```sh", "the patch is quoted verbatim")
	contract, err := agents.ContractFor(domain.MessageVerdict)
	require.NoError(t, err)
	assert.True(t, contract.HasPromptSuffix(p.Text), "the verdict contract is still last")
	assert.Equal(t, prompt.Fenced("diff", strings.TrimRight(fenceMarkdownDiff().Patch, "\n")),
		fenceSectionOf(t, p.Text, "````diff"), "the whole patch sits in one block")
}

// fenceSectionOf returns the fenced block that starts at the first line
// equal to open, fence line included.
func fenceSectionOf(t *testing.T, text, open string) string {
	t.Helper()
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		if l == open {
			start = i
			break
		}
	}
	require.GreaterOrEqual(t, start, 0, "no %q line in the prompt", open)
	fence := strings.TrimSuffix(open, "diff")
	for i := start + 1; i < len(lines); i++ {
		if lines[i] == fence {
			return strings.Join(lines[start:i+1], "\n") + "\n"
		}
	}
	t.Fatalf("the %q block is never closed", open)
	return ""
}
