package beads

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

func TestGraphEdgeTargetEncoding(t *testing.T) {
	local, err := json.Marshal(GraphEdge{FromKey: "a", ToKey: "b", Type: DepBlocks})
	require.NoError(t, err)
	assert.JSONEq(t, `{"from_key":"a","to_key":"b","type":"blocks"}`, string(local))
	external, err := json.Marshal(GraphEdge{FromKey: "a", ToID: "bd-old", Type: DepBlocks})
	require.NoError(t, err)
	assert.JSONEq(t, `{"from_key":"a","to_id":"bd-old","type":"blocks"}`, string(external))
}

func TestBuildAndVerifyCombinedGraph(t *testing.T) {
	req := PlanRequest{
		Run: "r-combined",
		Plan: domain.Plan{Beads: []domain.PlanBead{
			{Key: "a", Title: "A", Acceptance: "ok", Priority: domain.PriorityDefault},
			{Key: "b", Title: "B", Acceptance: "ok", Priority: domain.PriorityDefault, Deps: []string{"a"}},
		}},
		ExternalDeps: []domain.PlanExternalDependency{{FromKey: "b", ToID: "bd-old"}},
	}
	graph, err := BuildGraphPlan(req)
	require.NoError(t, err)
	require.Len(t, graph.Edges, 2)
	assert.Equal(t, GraphEdge{FromKey: "b", ToKey: "a", Type: DepBlocks}, graph.Edges[0])
	assert.Equal(t, GraphEdge{FromKey: "b", ToID: "bd-old", Type: DepBlocks}, graph.Edges[1])
	assert.NoError(t, VerifyDryRun(graph, GraphResult{NodeCount: len(graph.Nodes), EdgeCount: 2}))

	ids := map[string]domain.BeadID{"epic": "bd-epic", "a": "bd-a", "b": "bd-b"}
	issues := []Issue{
		graphIssue("bd-a", domain.BeadStateOpen),
		graphIssue("bd-b", domain.BeadStateOpen, "bd-a", "bd-old"),
	}
	assert.NoError(t, VerifyAppliedGraph(graph, ids, issues))
	assert.ErrorIs(t, VerifyAppliedGraph(graph, ids, []Issue{issues[0], graphIssue("bd-b", domain.BeadStateOpen, "bd-a")}), ErrPlanDependencies)
	assert.ErrorIs(t, VerifyAppliedGraph(graph, ids, []Issue{issues[0], graphIssue("bd-b", domain.BeadStateOpen, "bd-a", "bd-other")}), ErrPlanDependencies)
}

func TestValidateGraphPlanRejectsUnsafeEdges(t *testing.T) {
	nodes := []GraphNode{{Key: "a"}, {Key: "b"}}
	for _, edge := range []GraphEdge{
		{FromKey: "a"},
		{FromKey: "a", ToKey: "b", ToID: "bd-old"},
		{FromKey: "a", ToKey: "missing"},
		{FromKey: "missing", ToKey: "a"},
		{FromKey: "a", ToKey: "a"},
		{FromKey: "a", ToID: "bd old"},
	} {
		assert.ErrorIs(t, ValidateGraphPlan(GraphPlan{Nodes: nodes, Edges: []GraphEdge{edge}}), ErrPlanGraph, edge)
	}
	assert.ErrorIs(t, ValidateGraphPlan(GraphPlan{Nodes: nodes, Edges: []GraphEdge{
		{FromKey: "a", ToKey: "b"}, {FromKey: "b", ToKey: "a"},
	}}), ErrPlanGraph)
	assert.ErrorIs(t, ValidateGraphPlan(GraphPlan{Nodes: nodes, Edges: []GraphEdge{
		{FromKey: "a", ToKey: "b"}, {FromKey: "a", ToKey: "b"},
	}}), ErrPlanGraph)
}

func TestApplyPlanRejectsMissingFreshCanonicalTarget(t *testing.T) {
	client, bd := planFake(t, map[string]string{"show": "error-not-found"})
	req := planRequest("")
	req.Plan.Beads = req.Plan.Beads[:1]
	req.ExternalDeps = []domain.PlanExternalDependency{{FromKey: "a", ToID: "bd-missing"}}
	_, err := client.ApplyPlan(context.Background(), req, false)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Len(t, bd.ArgLists(t), 1, "a stale canonical target fails before dry-run or create")
}

func TestApplyPlanVerifiesExternalDependencies(t *testing.T) {
	envIsolate(t)
	bd := testutil.FakeBin(t, "bd", `dry=0; cmd=''; id=''
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) dry=1 ;;
    show|create|update) cmd=$1; if [ "$cmd" = show ]; then shift; id=$1; fi ;;
  esac
  shift
done
if [ "$cmd" = show ] && [ "$id" = bd-old ]; then
  echo '[{"id":"bd-old","title":"old","status":"open","priority":2}]'; exit 0
fi
if [ "$cmd" = show ]; then
  echo '[{"id":"bd-new","title":"new","status":"open","priority":2,"dependencies":[{"issue_id":"bd-new","depends_on_id":"bd-old","type":"blocks"}]}]'; exit 0
fi
if [ "$cmd" = update ]; then echo '[{"id":"bd-new","title":"new","status":"open","priority":2}]'; exit 0; fi
if [ "$dry" = 1 ]; then
  echo '{"dry_run":true,"node_count":2,"edge_count":1}'
else
  echo '{"ids":{"a":"bd-new","epic":"bd-epic"}}'
fi`)
	client := New("/repo", Options{})
	req := PlanRequest{
		Run:          "r-external",
		Plan:         domain.Plan{Beads: []domain.PlanBead{{Key: "a", Title: "New", Acceptance: "ok", Priority: domain.PriorityDefault}}},
		ExternalDeps: []domain.PlanExternalDependency{{FromKey: "a", ToID: "bd-old"}},
	}
	result, err := client.ApplyPlan(context.Background(), req, false)
	require.NoError(t, err)
	assert.Equal(t, []domain.BeadID{"bd-new"}, result.Beads)
	calls := bd.ArgLists(t)
	require.Len(t, calls, 5, "target show, dry-run, create, acceptance, applied show")
	assert.Equal(t, "bd-old", calls[0][3])
	assert.Equal(t, "bd-new", calls[4][3])
	for _, call := range calls[1:4] {
		assert.NotContains(t, call, "bd-old", "the existing canonical bead is referenced, never updated")
	}
}
