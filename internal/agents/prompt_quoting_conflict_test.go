package agents

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEngineerConflictQuotesGitOutputThatCarriesAFence pins that git output
// containing a Markdown fence (a conflict in a Markdown file prints the
// conflicted hunk) cannot close the quoted block early and turn the rest of
// the prompt into prose.
func TestEngineerConflictQuotesGitOutputThatCarriesAFence(t *testing.T) {
	t.Parallel()
	req := engRequest(EngineerResolveConflict)
	req.Conflict.Output = "CONFLICT (content): Merge conflict in README.md\n<<<<<<< HEAD\n```sh\nsc .\n```\n>>>>>>> theirs"

	p, err := req.Prompt()
	require.NoError(t, err)

	assert.Contains(t, p.Text, "````\nCONFLICT (content): Merge conflict in README.md",
		"the fence is longer than the one inside the output")
	assert.Contains(t, p.Text, "\n```sh\n", "the output is quoted verbatim")

	// Everything after the quoted output is still structure, not prose.
	_, after, ok := strings.Cut(p.Text, "````\nCONFLICT")
	require.True(t, ok)
	_, rest, ok := strings.Cut(after, "\n````\n")
	require.True(t, ok)
	assert.NotContains(t, rest, "```sh", "the whole output stayed inside the block")
}
