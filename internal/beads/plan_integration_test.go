//go:build integration

package beads

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/proc"
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

	// A second plan joins the existing epic in the create itself: every
	// node carries parent_id, so the dry run counts one parent dependency
	// per bead and no `bd update --parent` follows (P3-18).
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
	assert.Equal(t, len(more.Plan.Beads), res2.DryRun.ParentDeps, "parent_deps == the bead count (P3-18)")
	for _, key := range []string{"g", "h"} {
		issues, err := c.Show(ctx, res2.IDs[key])
		require.NoError(t, err)
		require.Len(t, issues, 1)
		assert.Equal(t, res.Epic, issues[0].ParentID(), "%s was created inside the run epic", key)
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

// planRawDryRun runs `bd create --graph <file> --dry-run --json` outside
// the typed GraphPlan struct, so a test can send fields BuildGraphPlan
// cannot express, and returns the decoded report plus the raw result.
func planRawDryRun(t *testing.T, repo, body string) (GraphResult, proc.Result, error) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "plan.json")
	require.NoError(t, os.WriteFile(file, []byte(body), 0o600))
	res, err := Run(context.Background(), CallOptions{Repo: repo, Timeout: time.Minute}, "create", "--graph", file, "--dry-run", "--json")
	if err != nil {
		return GraphResult{}, res, err
	}
	var out GraphResult
	if err := Decode(res.Stdout, &out); err != nil {
		return GraphResult{}, res, err
	}
	out.Warnings = GraphWarnings(res.Stderr)
	return out, res, nil
}

// TestIntegrationGraphPlanNodeFields re-verifies the P3-11 facts recorded
// in Appendix A against the installed bd: which node fields the graph-plan
// parser accepts, that node-level deps are dropped while top-level edges
// are counted (D-24), that metadata values must be strings, and that
// VerifyDryRun fails loudly on the resulting warning.
func TestIntegrationGraphPlanNodeFields(t *testing.T) {
	_, repo := integrationClient(t)

	t.Run("accepted node fields", func(t *testing.T) {
		for _, field := range []string{
			`"description":"d"`, `"labels":["sc:run:r1"]`, `"metadata":{"sc_run":"r1"}`,
			`"parent_key":"p"`, `"parent_id":"fx-nope"`, `"assignee":"sc"`,
		} {
			body := `{"nodes":[{"key":"p","title":"P","type":"epic","priority":1},{"key":"n","title":"N","type":"task","priority":2,` + field + `}]}`
			out, _, err := planRawDryRun(t, repo.Dir, body)
			require.NoError(t, err, field)
			assert.Empty(t, out.Warnings, "%s is an accepted node field", field)
		}
	})

	t.Run("no acceptance field exists", func(t *testing.T) {
		for _, field := range []string{"acceptance", "acceptance_criteria", "acceptanceCriteria", "criteria", "notes", "design", "owner", "id", "status", "deps"} {
			body := `{"nodes":[{"key":"n","title":"N","type":"task","priority":2,"` + field + `":"x"}]}`
			out, _, err := planRawDryRun(t, repo.Dir, body)
			require.NoError(t, err, field)
			require.Len(t, out.Warnings, 1, "%s is dropped with a warning", field)
			assert.Contains(t, out.Warnings[0], "unknown field(s): ["+field+"]")
			assert.ErrorIs(t, VerifyDryRun(GraphPlan{Nodes: make([]GraphNode, out.NodeCount)}, out), ErrPlanWarning)
		}
	})

	t.Run("node deps are dropped, top-level edges are counted", func(t *testing.T) {
		nodes := `{"key":"a","title":"A","type":"task","priority":2},{"key":"b","title":"B","type":"task","priority":2}`
		out, _, err := planRawDryRun(t, repo.Dir, `{"nodes":[`+nodes+`],"edges":[{"from_key":"b","to_key":"a","type":"blocks"}]}`)
		require.NoError(t, err)
		assert.Empty(t, out.Warnings)
		assert.Equal(t, 1, out.EdgeCount, "top-level edges[] are the D-24 dependency channel")

		dropped, _, err := planRawDryRun(t, repo.Dir, `{"nodes":[{"key":"a","title":"A","type":"task","priority":2,"deps":[{"target":"b","type":"blocks"}]},{"key":"b","title":"B","type":"task","priority":2}]}`)
		require.NoError(t, err)
		assert.Equal(t, 0, dropped.EdgeCount, "node-level deps create nothing (D-24)")
		assert.NotEmpty(t, dropped.Warnings)
	})

	t.Run("metadata values must be strings", func(t *testing.T) {
		_, _, err := planRawDryRun(t, repo.Dir, `{"nodes":[{"key":"n","title":"N","type":"task","priority":2,"metadata":{"sc_round":0}}]}`)
		require.Error(t, err, "a numeric metadata value fails the whole call; GraphMetadata stringifies")

		plan, err := BuildGraphPlan(planRequest(""))
		require.NoError(t, err)
		for _, n := range plan.Nodes {
			for k, v := range n.Metadata {
				assert.IsType(t, "", v, "node %s metadata %s", n.Key, k)
			}
		}
	})

	t.Run("the captured plan dry runs clean", func(t *testing.T) {
		plan, err := BuildGraphPlan(planRequest(""))
		require.NoError(t, err)
		body, err := json.Marshal(plan)
		require.NoError(t, err)
		out, _, err := planRawDryRun(t, repo.Dir, string(body))
		require.NoError(t, err)
		assert.NoError(t, VerifyDryRun(plan, out))
		assert.Equal(t, planDiamond().DependencyCount(), out.EdgeCount, "edge_count == Plan.DependencyCount() (P3-15)")
		assert.Equal(t, len(plan.Nodes), out.NodeCount)
	})
}
