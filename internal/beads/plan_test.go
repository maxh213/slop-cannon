package beads

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

// planDiamond is the six-bead plan behind testdata/bd/graph-plan.json: a
// fans out to b and c, both feed d (the diamond), then e and f follow.
func planDiamond() domain.Plan {
	return domain.Plan{
		Brief: "Stats fixes",
		Beads: []domain.PlanBead{
			{Key: "a", Title: "Add stats package", Type: domain.BeadTypeTask, Priority: 1, Description: "Package skeleton", Acceptance: "acceptance a", Size: domain.SizeS},
			{Key: "b", Title: "Fix Mean", Type: domain.BeadTypeBug, Priority: 1, Description: "Mean skips the first value", Acceptance: "acceptance b", Size: domain.SizeS, Deps: []string{"a"}},
			{Key: "c", Title: "Fix Max", Type: domain.BeadTypeBug, Priority: 2, Description: "Max starts from 0", Acceptance: "acceptance c", Size: domain.SizeM, Deps: []string{"a"}},
			{Key: "d", Title: "Add Median", Type: domain.BeadTypeFeature, Priority: 2, Description: "Median over sorted copy", Acceptance: "acceptance d", Size: domain.SizeM, Deps: []string{"b", "c"}},
			{Key: "e", Title: "Document stats", Type: domain.BeadTypeChore, Priority: 3, Description: "README section", Acceptance: "acceptance e", Size: domain.SizeS, Deps: []string{"d"}},
			{Key: "f", Title: "Benchmark stats", Type: domain.BeadTypeTask, Priority: 4, Description: "Benchmarks for the package", Acceptance: "acceptance f", Size: domain.SizeL, Deps: []string{"e"}},
		},
	}
}

// planRequest is the request the captured samples were produced from.
func planRequest(epic domain.BeadID) PlanRequest {
	return PlanRequest{Run: "r1", Plan: planDiamond(), Epic: epic, Roles: []domain.Role{domain.RolePO}}
}

// planFake installs the FakeBD shim replaying the captured graph samples
// for the create --graph keys, plus overrides.
func planFake(t *testing.T, overrides map[string]string) (*Client, *testutil.Shim) {
	t.Helper()
	all := map[string]string{"create-graph-dry-run": "graph-dry-run", "create-graph": "graph-create"}
	for k, v := range overrides {
		all[k] = v
	}
	return clientFake(t, all)
}

// planSampleJSON returns one captured sample as compact JSON text.
func planSampleJSON(t *testing.T, name string) string {
	t.Helper()
	return envSample(t, name+".json")
}

// planDecodeGraphResult decodes a captured dry-run sample.
func planDecodeGraphResult(t *testing.T, name string) GraphResult {
	t.Helper()
	var res GraphResult
	require.NoError(t, Decode(planSampleJSON(t, name), &res))
	return res
}

func TestBuildGraphPlanMatchesCapturedPlan(t *testing.T) {
	plan := planDiamond()
	require.NoError(t, plan.Validate())
	assert.Equal(t, 6, plan.DependencyCount())

	t.Run("epic created in the plan", func(t *testing.T) {
		g, err := BuildGraphPlan(planRequest(""))
		require.NoError(t, err)
		data, err := json.Marshal(g)
		require.NoError(t, err)
		assert.JSONEq(t, planSampleJSON(t, "graph-plan"), string(data), "the JSON bd 1.2.2 accepted (P3-11)")
		assert.Len(t, g.Edges, plan.DependencyCount(), "one blocks edge per dependency (D-24)")
		require.Len(t, g.Nodes, 7)
		assert.Equal(t, PlanEpicKey, g.Nodes[0].Key)
		assert.Equal(t, domain.BeadTypeEpic, g.Nodes[0].Type)
		assert.Equal(t, RunEpicPriority, g.Nodes[0].Priority)
		for _, n := range g.Nodes[1:] {
			assert.Equal(t, PlanEpicKey, n.ParentKey, "%s is a child of the epic", n.Key)
			assert.Equal(t, []string{"sc:run:r1", "sc:role:po"}, n.Labels)
			assert.Equal(t, "0", n.Metadata[MetaRound], "graph-plan metadata is string-valued")
		}
		for _, e := range g.Edges {
			assert.Equal(t, DepBlocks, e.Type)
		}
	})

	t.Run("existing epic", func(t *testing.T) {
		g, err := BuildGraphPlan(planRequest("px-26w"))
		require.NoError(t, err)
		data, err := json.Marshal(g)
		require.NoError(t, err)
		assert.JSONEq(t, planSampleJSON(t, "graph-plan-existing"), string(data))
		require.Len(t, g.Nodes, 6)
		for _, n := range g.Nodes {
			assert.Empty(t, n.ParentKey)
			assert.NotEqual(t, domain.BeadTypeEpic, n.Type)
		}
		assert.Len(t, g.Edges, plan.DependencyCount())
	})
}

func TestBuildGraphPlanDefaults(t *testing.T) {
	req := PlanRequest{
		Run: "r2",
		Plan: domain.Plan{Beads: []domain.PlanBead{
			{Key: "epic", Title: "Uses the epic key", Acceptance: "ok", Priority: domain.PriorityDefault},
			{Key: "x", Title: "X", Acceptance: "ok", Priority: domain.PriorityDefault, Deps: []string{"epic"}},
		}},
		Roles:  []domain.Role{domain.RolePO, "", domain.RolePO},
		Labels: []string{" extra ", "sc:run:r2", "", "extra"},
	}
	g, err := BuildGraphPlan(req)
	require.NoError(t, err)
	require.Len(t, g.Nodes, 3)
	epic := g.Nodes[0]
	assert.Equal(t, "run-epic", epic.Key, "the epic key steps aside for a plan bead called epic")
	assert.Equal(t, RunEpicTitle("r2"), epic.Title)
	assert.Empty(t, epic.Description, "no brief, no description")
	assert.Equal(t, []string{"sc:run:r2"}, epic.Labels)
	assert.Equal(t, map[string]any{MetaRun: "r2", MetaKind: KindEpic}, epic.Metadata)
	assert.Equal(t, "sc: plan 2 beads for run r2", g.CommitMessage)
	for _, n := range g.Nodes[1:] {
		assert.Equal(t, "run-epic", n.ParentKey)
		assert.Equal(t, domain.BeadTypeTask, n.Type, "empty type means task")
		assert.Equal(t, []string{"sc:run:r2", "sc:role:po", "extra"}, n.Labels, "deduplicated, trimmed, run label first")
		assert.Equal(t, map[string]any{MetaRun: "r2", MetaKind: KindWork, MetaRound: "0"}, n.Metadata, "no size, no sc_size")
	}
	assert.Equal(t, []GraphEdge{{FromKey: "x", ToKey: "epic", Type: DepBlocks}}, g.Edges)

	g, err = BuildGraphPlan(PlanRequest{Run: "r2", Plan: req.Plan, EpicTitle: " Custom ", EpicDescription: "body", CommitMessage: " msg "})
	require.NoError(t, err)
	assert.Equal(t, "Custom", g.Nodes[0].Title)
	assert.Equal(t, "body", g.Nodes[0].Description)
	assert.Equal(t, "msg", g.CommitMessage)
}

func TestBuildGraphPlanErrors(t *testing.T) {
	_, err := BuildGraphPlan(PlanRequest{Plan: planDiamond()})
	assert.ErrorIs(t, err, ErrInvalid, "run id is required")

	_, err = BuildGraphPlan(PlanRequest{Run: "r1"})
	assert.ErrorIs(t, err, ErrInvalid, "an empty plan creates nothing")

	bad := planDiamond()
	bad.Beads[0].Acceptance = ""
	bad.Beads[1].Deps = []string{"nope"}
	_, err = BuildGraphPlan(PlanRequest{Run: "r1", Plan: bad})
	assert.ErrorIs(t, err, domain.ErrInvalidPlan)
	assert.Contains(t, err.Error(), `dependency "nope"`)
}

func TestGraphMetadata(t *testing.T) {
	assert.Nil(t, GraphMetadata(nil))
	assert.Nil(t, GraphMetadata(map[string]any{}))
	got := GraphMetadata(map[string]any{"n": 0, "f": 1.5, "b": true, "s": "x", "nil": nil, "big": 7})
	assert.Equal(t, map[string]any{"n": "0", "f": "1.5", "b": "true", "s": "x", "big": "7"}, got)
	assert.Equal(t, map[string]any{MetaRun: "r1", MetaKind: KindWork, MetaRound: "0", MetaSize: "m"}, GraphMetadata(WorkMetadata("r1", domain.SizeM)))
}

func TestVerifyDryRun(t *testing.T) {
	g, err := BuildGraphPlan(planRequest(""))
	require.NoError(t, err)

	ok := planDecodeGraphResult(t, "graph-dry-run")
	assert.Equal(t, 7, ok.NodeCount)
	assert.Equal(t, planDiamond().DependencyCount(), ok.EdgeCount, "bd 1.2.2 counts every edges[] entry (P3-11)")
	assert.Equal(t, 6, ok.ParentDeps)
	assert.NoError(t, VerifyDryRun(g, ok))

	warned := ok
	warned.Warnings = []string{"warning: graph plan node[\"a\"] has unknown field(s): [deps]"}
	err = VerifyDryRun(g, warned)
	assert.ErrorIs(t, err, ErrPlanWarning)
	assert.Contains(t, err.Error(), "unknown field(s)")

	mismatch := planDecodeGraphResult(t, "graph-dry-run-mismatch")
	err = VerifyDryRun(g, mismatch)
	assert.ErrorIs(t, err, ErrPlanEdgeCount)
	assert.Contains(t, err.Error(), "bd counted 4 edges, plan has 6 dependencies")

	fewer := ok
	fewer.NodeCount = 6
	assert.ErrorIs(t, VerifyDryRun(g, fewer), ErrPlanNodeCount)

	existing, err := BuildGraphPlan(planRequest("px-26w"))
	require.NoError(t, err)
	assert.NoError(t, VerifyDryRun(existing, planDecodeGraphResult(t, "graph-dry-run-existing")))
	assert.ErrorIs(t, VerifyDryRun(existing, ok), ErrPlanNodeCount, "the epic-creating report does not fit the existing-epic plan")
}

func TestApplyPlanCreatesTheDiamond(t *testing.T) {
	c, bd := planFake(t, nil)
	plan := planDiamond()
	res, err := c.ApplyPlan(context.Background(), planRequest(""), false)
	require.NoError(t, err)
	assert.Empty(t, testutil.MissingSamples(t, bd))

	assert.Equal(t, domain.RunID("r1"), res.Run)
	assert.True(t, res.EpicCreated)
	assert.Equal(t, PlanEpicKey, res.EpicKey)
	assert.Equal(t, domain.BeadID("px-26w"), res.Epic)
	assert.Equal(t, plan.DependencyCount(), res.DryRun.EdgeCount, "edge_count == Plan.DependencyCount() (P3-15)")
	assert.Equal(t, len(res.Graph.Nodes), res.DryRun.NodeCount)
	assert.Equal(t, map[string]domain.BeadID{"a": "px-69u", "b": "px-euc", "c": "px-kvb", "d": "px-vey", "e": "px-19j", "f": "px-0st"}, res.IDs)
	assert.Equal(t, []domain.BeadID{"px-69u", "px-euc", "px-kvb", "px-vey", "px-19j", "px-0st"}, res.Beads, "plan order")
	assert.Empty(t, res.Unfinished)

	calls := bd.ArgLists(t)
	require.Len(t, calls, 8, "dry run, real run, six acceptance updates")
	assert.Equal(t, []string{"--dry-run", "--json"}, calls[0][5:])
	assert.Equal(t, "--graph", calls[0][3])
	assert.Equal(t, []string{"--json"}, calls[1][5:])
	assert.NotContains(t, calls[1], "--dry-run")
	for i, key := range []string{"a", "b", "c", "d", "e", "f"} {
		want := []string{"-C", "/tmp/fixture-repo", "update", string(res.IDs[key]), "--acceptance", "acceptance " + key, "--json"}
		assert.Equal(t, want, calls[2+i], "acceptance criteria are written after creation (bd 1.2.2 drops them from the plan)")
	}
}

func TestApplyPlanDryRun(t *testing.T) {
	c, bd := planFake(t, nil)
	res, err := c.ApplyPlan(context.Background(), planRequest(""), true)
	require.NoError(t, err)
	assert.True(t, res.DryRun.DryRun)
	assert.Equal(t, 6, res.DryRun.EdgeCount)
	assert.Nil(t, res.IDs)
	assert.Nil(t, res.Beads)
	assert.Empty(t, res.Epic, "nothing was minted")
	assert.True(t, res.EpicCreated, "the plan would create it")
	assert.Len(t, res.Graph.Nodes, 7)
	assert.Len(t, bd.ArgLists(t), 1, "a dry run is one bd call")
}

func TestApplyPlanExistingEpic(t *testing.T) {
	c, bd := planFake(t, map[string]string{
		"show":                 "graph-show-epic",
		"create-graph-dry-run": "graph-dry-run-existing",
		"create-graph":         "graph-create-existing",
		"update":               "graph-update-finish",
	})
	res, err := c.ApplyPlan(context.Background(), planRequest(" px-26w "), false)
	require.NoError(t, err)
	assert.Empty(t, testutil.MissingSamples(t, bd))
	assert.False(t, res.EpicCreated)
	assert.Empty(t, res.EpicKey)
	assert.Equal(t, domain.BeadID("px-26w"), res.Epic)
	assert.Equal(t, domain.BeadID("px-we1"), res.IDs["a"])
	assert.Len(t, res.Beads, 6)
	assert.Equal(t, 0, res.DryRun.ParentDeps)

	calls := bd.ArgLists(t)
	require.Len(t, calls, 9, "show epic, dry run, real run, six finishing updates")
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "show", "px-26w", "--json", "--include-comments"}, calls[0])
	assert.Equal(t, []string{"-C", "/tmp/fixture-repo", "update", "px-we1", "--acceptance", "acceptance a", "--parent", "px-26w", "--json"}, calls[3], "beads join the existing epic through --parent")
}

func TestApplyPlanRejectsBadEpic(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		c, bd := planFake(t, map[string]string{"show": "error-not-found"})
		res, err := c.ApplyPlan(context.Background(), planRequest("px-nope"), false)
		assert.ErrorIs(t, err, ErrNotFound)
		assert.Nil(t, res.IDs)
		assert.Len(t, bd.ArgLists(t), 1, "nothing is created")
	})
	t.Run("not an epic", func(t *testing.T) {
		c, bd := planFake(t, nil) // show.json answers with a bug
		_, err := c.ApplyPlan(context.Background(), planRequest("fx-so0"), false)
		assert.ErrorIs(t, err, ErrInvalid)
		assert.Contains(t, err.Error(), "not an epic")
		assert.Len(t, bd.ArgLists(t), 1)
	})
}

func TestApplyPlanFailsOnDryRunWarning(t *testing.T) {
	c, bd := planFake(t, map[string]string{"create-graph-dry-run": "create-graph-dry-run-warning"})
	res, err := c.ApplyPlan(context.Background(), planRequest(""), false)
	assert.ErrorIs(t, err, ErrPlanWarning)
	assert.Contains(t, err.Error(), "silently dropped")
	assert.Len(t, res.DryRun.Warnings, 1)
	assert.Nil(t, res.IDs)
	assert.Len(t, bd.ArgLists(t), 1, "the real run never happens")
}

func TestApplyPlanFailsOnEdgeCountMismatch(t *testing.T) {
	c, bd := planFake(t, map[string]string{"create-graph-dry-run": "graph-dry-run-mismatch"})
	res, err := c.ApplyPlan(context.Background(), planRequest(""), false)
	assert.ErrorIs(t, err, ErrPlanEdgeCount)
	assert.Contains(t, err.Error(), "plan has 6 dependencies")
	assert.Equal(t, 4, res.DryRun.EdgeCount)
	assert.Nil(t, res.IDs)
	assert.Len(t, bd.ArgLists(t), 1)
}

func TestApplyPlanFailsBeforeCreateOnBadInput(t *testing.T) {
	c, bd := planFake(t, nil)
	_, err := c.ApplyPlan(context.Background(), PlanRequest{Run: "r1"}, false)
	assert.ErrorIs(t, err, ErrInvalid)
	bad := planRequest("")
	bad.Plan.Beads[2].Deps = []string{"a", "a"}
	_, err = c.ApplyPlan(context.Background(), bad, false)
	assert.ErrorIs(t, err, domain.ErrInvalidPlan)
	assert.Empty(t, bd.ArgLists(t), "bd is never called for an invalid request")
}

func TestApplyPlanReportsUnfinishedBeads(t *testing.T) {
	c, bd := planFake(t, map[string]string{"update": "error-update-not-found"})
	res, err := c.ApplyPlan(context.Background(), planRequest(""), false)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Contains(t, err.Error(), `finalising px-69u (key "a")`)
	assert.Equal(t, domain.BeadID("px-26w"), res.Epic, "the beads exist")
	assert.Len(t, res.IDs, 6)
	assert.Equal(t, res.Beads, res.Unfinished, "the first update failed, so every bead is unfinished")
	assert.Len(t, bd.ArgLists(t), 3)
}

func TestApplyPlanMissingIDs(t *testing.T) {
	c, _ := planFake(t, map[string]string{"create-graph": "create-graph"}) // ids for epic/mean/max only
	res, err := c.ApplyPlan(context.Background(), planRequest(""), false)
	assert.ErrorIs(t, err, ErrPlanIDs)
	assert.Contains(t, err.Error(), "missing a, b, c, d, e, f")
	assert.Nil(t, res.Beads)
	assert.Equal(t, domain.BeadID("fx-nvi"), res.Epic, "the ids bd did return are kept")
}

func TestApplyPlanFileShape(t *testing.T) {
	envIsolate(t)
	samples := envSamplesDir(t)
	// A shim that keeps every plan file it is handed and answers the
	// create calls with the captured samples, so the JSON bd would parse
	// can be compared with what bd 1.2.2 accepted.
	bd := testutil.FakeBin(t, "bd", `samples=`+samples+`
dry=0; cmd=''
while [ $# -gt 0 ]; do
  case "$1" in
    --graph) cp "$2" "$SC_FAKEBIN_DIR/plan.$SC_FAKEBIN_SEQ.json"; shift ;;
    --dry-run) dry=1 ;;
    create|update) cmd=$1 ;;
  esac
  shift
done
if [ "$cmd" = update ]; then cat "$samples/update-acceptance.json"; exit 0; fi
if [ "$dry" = 1 ]; then cat "$samples/graph-dry-run.json"; else cat "$samples/graph-create.json"; fi`)
	c := New("/repo", Options{})
	res, err := c.ApplyPlan(context.Background(), planRequest(""), false)
	require.NoError(t, err)
	assert.Len(t, res.Beads, 6)
	for _, seq := range []string{"1", "2"} {
		data, err := os.ReadFile(filepath.Join(bd.Dir, "plan."+seq+".json"))
		require.NoError(t, err)
		assert.JSONEq(t, planSampleJSON(t, "graph-plan"), string(data), "call %s sent the captured plan", seq)
	}
}

func TestGraphBlockedSampleShowsTheDiamond(t *testing.T) {
	var issues []Issue
	require.NoError(t, Decode(planSampleJSON(t, "graph-blocked"), &issues))
	var create GraphResult
	require.NoError(t, Decode(planSampleJSON(t, "graph-create"), &create))
	id := create.IDs
	blockedBy := map[domain.BeadID][]domain.BeadID{}
	for _, is := range issues {
		blockedBy[is.ID] = is.Blockers()
	}
	assert.Equal(t, map[domain.BeadID][]domain.BeadID{
		id["b"]: {id["a"]},
		id["c"]: {id["a"]},
		id["d"]: {id["c"], id["b"]},
		id["e"]: {id["d"]},
		id["f"]: {id["e"]},
	}, blockedBy, "bd blocked after the real run: the diamond landed with the D-24 edges")
	assert.NotContains(t, blockedBy, id["a"], "the root is ready")
}
