//go:build integration

package beads

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
)

// TestIntegrationApplyPlanDiamond applies the six-bead diamond plan to a
// fresh fixture repository with the real bd (P3-10, P3-11, P3-15): the
// dry run counts one edge per dependency, the beads land as children of
// the minted epic with the expected blocked_by lists, acceptance criteria
// are written afterwards, and a second plan joins the existing epic.
func TestIntegrationApplyPlanDiamond(t *testing.T) {
	c, _ := integrationClient(t)
	ctx := context.Background()
	plan := planDiamond()

	res, err := c.ApplyPlan(ctx, planRequest(""), false)
	require.NoError(t, err)
	assert.Equal(t, plan.DependencyCount(), res.DryRun.EdgeCount, "edge_count == Plan.DependencyCount() (D-24)")
	assert.Empty(t, res.DryRun.Warnings)
	assert.Equal(t, len(plan.Beads), res.DryRun.ParentDeps, "every work bead is a child of the epic")
	require.True(t, res.EpicCreated)
	require.NotEmpty(t, res.Epic)
	require.Len(t, res.Beads, len(plan.Beads))
	assert.Empty(t, res.Unfinished)

	epic, err := c.Show(ctx, res.Epic)
	require.NoError(t, err)
	require.Len(t, epic, 1)
	assert.True(t, epic[0].IsRunEpic())
	assert.Equal(t, domain.RunID("r1"), epic[0].RunID())
	assert.Equal(t, "Stats fixes", epic[0].Description)

	id := res.IDs
	blocked, err := c.Blocked(ctx)
	require.NoError(t, err)
	got := map[domain.BeadID][]domain.BeadID{}
	for _, is := range blocked {
		got[is.ID] = is.Blockers()
	}
	assert.Equal(t, sortedBlockers(map[domain.BeadID][]domain.BeadID{
		id["b"]: {id["a"]},
		id["c"]: {id["a"]},
		id["d"]: {id["b"], id["c"]},
		id["e"]: {id["d"]},
		id["f"]: {id["e"]},
	}), sortedBlockers(got), "the diamond's blocked_by lists")

	ready, err := c.Ready(ctx, res.Epic)
	require.NoError(t, err)
	assert.Equal(t, []domain.BeadID{id["a"]}, IssueIDs(ready), "only the root is ready")

	for _, pb := range plan.Beads {
		issues, err := c.Show(ctx, id[pb.Key])
		require.NoError(t, err)
		require.Len(t, issues, 1)
		is := issues[0]
		assert.Equal(t, pb.Acceptance, is.Acceptance, "%s: acceptance written after creation", pb.Key)
		assert.Equal(t, res.Epic, is.ParentID(), "%s: child of the epic", pb.Key)
		assert.Equal(t, string(pb.Type), is.Type)
		assert.Equal(t, pb.Priority, is.Priority)
		assert.True(t, is.InRun("r1"))
		assert.Equal(t, KindWork, is.Kind())
		assert.Equal(t, 0, is.Round())
		size, ok := is.Size()
		assert.True(t, ok)
		assert.Equal(t, pb.Size, size)
		assert.True(t, is.HasLabel(RoleLabel(domain.RolePO)))
		assert.Equal(t, []domain.Role{domain.RolePO}, is.Roles())
	}

	// Ready-set agreement with the graph model built from an export.
	all, err := c.Export(ctx)
	require.NoError(t, err)
	g := NewGraph(all)
	assert.Equal(t, []domain.BeadID{id["a"]}, IssueIDs(g.Ready()))
	assert.NoError(t, g.Validate())

	// A second plan joins the existing epic through --parent.
	more := PlanRequest{
		Run: "r1", Epic: res.Epic, Roles: []domain.Role{domain.RolePO},
		Plan: domain.Plan{Beads: []domain.PlanBead{
			{Key: "g", Title: "Add Mode", Type: domain.BeadTypeFeature, Priority: 2, Acceptance: "acceptance g", Size: domain.SizeS},
			{Key: "h", Title: "Test Mode", Type: domain.BeadTypeTask, Priority: 2, Acceptance: "acceptance h", Deps: []string{"g"}},
		}},
	}
	res2, err := c.ApplyPlan(ctx, more, false)
	require.NoError(t, err)
	assert.False(t, res2.EpicCreated)
	assert.Equal(t, res.Epic, res2.Epic)
	assert.Equal(t, 1, res2.DryRun.EdgeCount)
	assert.Equal(t, 0, res2.DryRun.ParentDeps)
	for _, key := range []string{"g", "h"} {
		issues, err := c.Show(ctx, res2.IDs[key])
		require.NoError(t, err)
		require.Len(t, issues, 1)
		assert.Equal(t, res.Epic, issues[0].ParentID(), "%s reparented to the run epic", key)
		assert.Equal(t, "acceptance "+key, issues[0].Acceptance)
	}
	children, err := c.List(ctx, ListFilter{Parent: res.Epic})
	require.NoError(t, err)
	assert.Len(t, children, 8)

	run, err := c.ListRun(ctx, "r1", true)
	require.NoError(t, err)
	assert.Len(t, run, 9, "epic plus eight work beads carry the run label")

	// A dry run against the real binary creates nothing.
	dry, err := c.ApplyPlan(ctx, PlanRequest{Run: "r9", Plan: plan}, true)
	require.NoError(t, err)
	assert.Equal(t, plan.DependencyCount(), dry.DryRun.EdgeCount)
	none, err := c.ListRun(ctx, "r9", true)
	require.NoError(t, err)
	assert.Empty(t, none)

	// An unknown epic fails before anything is created.
	_, err = c.ApplyPlan(ctx, PlanRequest{Run: "r9", Plan: plan, Epic: "fx-nope"}, false)
	assert.ErrorIs(t, err, ErrNotFound)
	none, err = c.ListRun(ctx, "r9", true)
	require.NoError(t, err)
	assert.Empty(t, none)
}

// sortedBlockers orders every blocker list so map comparisons ignore the
// order bd reports blocked_by in.
func sortedBlockers(m map[domain.BeadID][]domain.BeadID) map[domain.BeadID][]domain.BeadID {
	out := make(map[domain.BeadID][]domain.BeadID, len(m))
	for k, v := range m {
		ids := append([]domain.BeadID(nil), v...)
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		out[k] = ids
	}
	return out
}
