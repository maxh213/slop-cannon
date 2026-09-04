package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/orchestrator"
)

// prSecretFakeCheckout is a [orchestrator.PRCheckout] that hands back a
// fixed workspace, so the test is about the decorator and nothing else.
type prSecretFakeCheckout struct {
	ws  orchestrator.PRWorkspace
	err error
	// calls counts what reached the inner checkout.
	calls int
}

func (c *prSecretFakeCheckout) Checkout(context.Context, domain.Bead, gitx.PullRequest) (orchestrator.PRWorkspace, error) {
	c.calls++
	return c.ws, c.err
}

// prSecretDiff is a pull request that touches one ordinary file and two the
// filter must hide: a dotenv file and a key, both with their contents in the
// patch, which is how a contributor's branch carries a credential.
func prSecretDiff() *gitx.Diff {
	patch := strings.Join([]string{
		"diff --git a/README.md b/README.md",
		"--- a/README.md",
		"+++ b/README.md",
		"@@ -1 +1,2 @@",
		" # project",
		"+a new line",
		"diff --git a/.env b/.env",
		"--- /dev/null",
		"+++ b/.env",
		"@@ -0,0 +1 @@",
		"+DATABASE_PASSWORD=" + prSecretValue,
		"diff --git a/deploy/id_rsa b/deploy/id_rsa",
		"--- /dev/null",
		"+++ b/deploy/id_rsa",
		"@@ -0,0 +1 @@",
		"+" + prSecretValue,
		"",
	}, "\n")
	return &gitx.Diff{
		Files: []gitx.DiffFile{{Path: "README.md"}, {Path: ".env"}, {Path: "deploy/id_rsa"}},
		Stats: gitx.DiffStats{
			Files: 3, Insertions: 3,
			Changed: []gitx.FileStat{
				{Path: "README.md", Insertions: 1},
				{Path: ".env", Insertions: 1},
				{Path: "deploy/id_rsa", Insertions: 1},
			},
		},
		Patch: patch,
	}
}

// prSecretValue is planted in the two files the filter must hide.
const prSecretValue = "PRLEAKCANARY-7e21"

// TestFilterPRDiffsHidesExcludedFilesFromAReviewer is the external half of
// P9-05's prompt clause. The run's own diffs are filtered where the
// conductor builds them; a pull request's diff is taken by the reviewer's
// own checkout, so nothing upstream can have filtered it — and it is the
// diff most likely to hold somebody else's credential.
func TestFilterPRDiffsHidesExcludedFilesFromAReviewer(t *testing.T) {
	inner := &prSecretFakeCheckout{ws: orchestrator.PRWorkspace{
		Worktree: "/tmp/wt", Branch: "contrib/feature", Base: "main", HeadSHA: "abc1234",
		Diff: prSecretDiff(),
	}}
	filter, err := agents.NewSecretFilter("deploy/*.txt")
	require.NoError(t, err)

	ws, err := filterPRDiffs(inner, filter).Checkout(context.Background(),
		domain.Bead{ID: "sc-pr-7"}, gitx.PullRequest{Number: 7})
	require.NoError(t, err)
	require.Equal(t, 1, inner.calls)

	require.NotContains(t, ws.Diff.Patch, prSecretValue, "the reviewer would have been shown the credential")
	require.Contains(t, ws.Diff.Patch, "a new line", "the rest of the pull request must still be reviewable")
	require.Contains(t, ws.Diff.Patch, agents.SecretPlaceholder, "and the reviewer is told something was withheld")

	paths := make([]string, 0, len(ws.Diff.Files))
	for _, f := range ws.Diff.Files {
		paths = append(paths, f.Path)
	}
	require.Equal(t, []string{"README.md"}, paths, "the excluded files are out of the file list too")
	require.Equal(t, 1, ws.Diff.Stats.Files, "and out of the stats, so '1 file changed' is honest")

	// Everything else about the workspace is untouched: the tester still
	// reviews the pull request's own checkout at its own commit.
	require.Equal(t, "/tmp/wt", ws.Worktree)
	require.Equal(t, "contrib/feature", ws.Branch)
	require.Equal(t, "main", ws.Base)
	require.Equal(t, "abc1234", ws.HeadSHA)
}

// TestFilterPRDiffsWithoutAConfiguredFilter: a run assembled without a
// filter still hides the built-in patterns, because they are the floor.
func TestFilterPRDiffsWithoutAConfiguredFilter(t *testing.T) {
	inner := &prSecretFakeCheckout{ws: orchestrator.PRWorkspace{Diff: prSecretDiff()}}
	ws, err := filterPRDiffs(inner, nil).Checkout(context.Background(), domain.Bead{ID: "sc-pr-7"}, gitx.PullRequest{})
	require.NoError(t, err)
	require.NotContains(t, ws.Diff.Patch, prSecretValue)
}

// TestFilterPRDiffsPassesFailuresThrough: the decorator adds filtering and
// nothing else, so a checkout that could not be made still fails, and a nil
// checkout stays nil for [orchestrator.ErrNoPRCheckout] to report.
func TestFilterPRDiffsPassesFailuresThrough(t *testing.T) {
	boom := errors.New("no such pull request")
	inner := &prSecretFakeCheckout{err: boom}
	_, err := filterPRDiffs(inner, nil).Checkout(context.Background(), domain.Bead{ID: "sc-pr-7"}, gitx.PullRequest{})
	require.ErrorIs(t, err, boom)

	require.Nil(t, filterPRDiffs(nil, nil), "a nil checkout must stay nil")
}
