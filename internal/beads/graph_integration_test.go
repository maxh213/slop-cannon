//go:build integration

package beads

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
)

// TestIntegrationGraphMatchesBD is the differential test of P3-04 against
// the real bd: a diamond plan plus hand-made beads is driven through a
// close and a claim, then the Graph built from `bd export` (and the one
// built from `bd list --all` + `bd dep list`) must report the same ready and
// blocked sets as `bd ready` and `bd blocked`.
func TestIntegrationGraphMatchesBD(t *testing.T) {
	c, _ := integrationClient(t)
	ctx := context.Background()
	require.NoError(t, c.SetCustomStatuses(ctx, WorkflowCustomStatuses()))

	plan := GraphPlan{
		Nodes: []GraphNode{
			{Key: "epic", Title: "Run r1", Type: domain.BeadTypeEpic, Priority: 1, Labels: []string{"sc:run:r1"}},
			{Key: "a", Title: "A", Priority: 1, ParentKey: "epic", Labels: []string{"sc:run:r1"}},
			{Key: "b", Title: "B", Priority: 2, ParentKey: "epic", Labels: []string{"sc:run:r1"}},
			{Key: "c", Title: "C", Priority: 3, ParentKey: "epic", Labels: []string{"sc:run:r1"}},
			{Key: "d", Title: "D", Priority: 0, ParentKey: "epic", Labels: []string{"sc:run:r1"}},
		},
		Edges: []GraphEdge{
			{FromKey: "b", ToKey: "a", Type: DepBlocks},
			{FromKey: "c", ToKey: "a", Type: DepBlocks},
			{FromKey: "d", ToKey: "b", Type: DepBlocks},
			{FromKey: "d", ToKey: "c", Type: DepBlocks},
		},
	}
	res, err := c.CreateGraph(ctx, plan, false)
	require.NoError(t, err)
	a, b, cID, d := res.IDs["a"], res.IDs["b"], res.IDs["c"], res.IDs["d"]
	loose, err := c.Create(ctx, BeadInput{Title: "Loose", Priority: 2})
	require.NoError(t, err)
	review, err := c.Create(ctx, BeadInput{Title: "Reviewing", Priority: 2})
	require.NoError(t, err)
	_, err = c.Update(ctx, review.ID, Patch{Status: domain.BeadStateInReview})
	require.NoError(t, err)

	compare := func(t *testing.T, stage string) {
		t.Helper()
		exported, err := c.Export(ctx)
		require.NoError(t, err)
		all, err := c.List(ctx, ListFilter{All: true})
		require.NoError(t, err)
		deps, err := c.DepList(ctx, IssueIDs(all)...)
		require.NoError(t, err)
		wantReady, err := c.Ready(ctx, "")
		require.NoError(t, err)
		wantBlocked, err := c.Blocked(ctx)
		require.NoError(t, err)

		for name, g := range map[string]*Graph{"export": NewGraph(exported), "list+deps": NewGraph(all, deps...)} {
			require.NoError(t, g.Validate(), "%s/%s", stage, name)
			assert.Empty(t, g.Dangling(), "%s/%s", stage, name)
			assert.ElementsMatch(t, IssueIDs(wantReady), IssueIDs(g.Ready()), "%s/%s ready", stage, name)
			assert.ElementsMatch(t, IssueIDs(wantBlocked), IssueIDs(g.Blocked()), "%s/%s blocked", stage, name)
			for _, is := range wantBlocked {
				assert.ElementsMatch(t, is.BlockedBy, g.OpenBlockers(is.ID), "%s/%s open blockers of %s", stage, name, is.ID)
			}
		}
	}

	compare(t, "initial")
	g := NewGraph(mustExport(t, c))
	assert.ElementsMatch(t, []domain.BeadID{a, loose.ID}, IssueIDs(g.Ready()))
	assert.Equal(t, 3, g.CriticalPathLength())
	assert.Equal(t, 2, g.Layer(d))

	_, err = c.Claim(ctx, a, "sc:engineer-1")
	require.NoError(t, err)
	compare(t, "claimed")

	_, err = c.Close(ctx, a, "done")
	require.NoError(t, err)
	compare(t, "a closed")
	g = NewGraph(mustExport(t, c))
	assert.ElementsMatch(t, []domain.BeadID{b, cID, loose.ID}, IssueIDs(g.Ready()))
	assert.Equal(t, 2, g.CriticalPathLength())

	_, err = c.Close(ctx, b, "done")
	require.NoError(t, err)
	_, err = c.Close(ctx, d, "too early")
	require.ErrorIs(t, err, ErrBlockedOpen, "the model agrees: d still waits on c")
	compare(t, "b closed")

	_, err = c.Close(ctx, cID, "done")
	require.NoError(t, err)
	compare(t, "c closed")
	g = NewGraph(mustExport(t, c))
	assert.True(t, g.IsReady(d))
	assert.False(t, g.IsReady(review.ID), "custom statuses are never ready")
}

// mustExport returns the current export snapshot.
func mustExport(t *testing.T, c *Client) []Issue {
	t.Helper()
	issues, err := c.Export(context.Background())
	require.NoError(t, err)
	return issues
}
