package beads

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

// graphIssue builds an open task with the given blockers, in edge form as
// bd export prints them.
func graphIssue(id string, status domain.BeadState, blockers ...string) Issue {
	is := Issue{ID: domain.BeadID(id), Title: id, Status: string(status), Type: string(domain.BeadTypeTask), Priority: domain.PriorityDefault}
	for _, b := range blockers {
		is.Dependencies = append(is.Dependencies, Dependency{IssueID: is.ID, DependsOnID: domain.BeadID(b), Type: DepBlocks})
	}
	return is
}

// graphIDs converts string ids to bead ids.
func graphIDs(ids ...string) []domain.BeadID {
	if len(ids) == 0 {
		return nil
	}
	out := make([]domain.BeadID, len(ids))
	for i, id := range ids {
		out[i] = domain.BeadID(id)
	}
	return out
}

// graphDiamond is a ← b, a ← c, {b, c} ← d (d depends on b and c, both
// depend on a), plus a closed e that d also depends on.
func graphDiamond(status map[string]domain.BeadState) *Graph {
	st := func(id string) domain.BeadState {
		if s, ok := status[id]; ok {
			return s
		}
		return domain.BeadStateOpen
	}
	return NewGraph([]Issue{
		graphIssue("a", st("a")),
		graphIssue("b", st("b"), "a"),
		graphIssue("c", st("c"), "a"),
		graphIssue("d", st("d"), "b", "e", "c"),
		graphIssue("e", domain.BeadStateClosed),
	})
}

func TestGraphDiamond(t *testing.T) {
	g := graphDiamond(nil)
	assert.Equal(t, 5, g.Len())
	assert.True(t, g.Has("d"))
	assert.False(t, g.Has("zz"))
	assert.Equal(t, graphIDs("a", "b", "c", "d", "e"), g.IDs())
	assert.Equal(t, graphIDs("b", "e", "c"), g.Blockers("d"))
	assert.Equal(t, graphIDs("b", "c"), g.Dependents("a"))
	assert.Nil(t, g.Blockers("a"))
	assert.Nil(t, g.Dependents("d"))
	assert.Equal(t, [][]domain.BeadID{graphIDs("a", "e"), graphIDs("b", "c"), graphIDs("d")}, g.Layers())
	assert.Equal(t, 0, g.Layer("a"))
	assert.Equal(t, 1, g.Layer("c"))
	assert.Equal(t, 2, g.Layer("d"))
	assert.Equal(t, -1, g.Layer("zz"))
	assert.NoError(t, g.Validate())
	assert.Empty(t, g.Cycles())
	assert.Empty(t, g.Dangling())

	assert.Equal(t, graphIDs("a"), IssueIDs(g.Ready()))
	assert.True(t, g.IsReady("a"))
	assert.False(t, g.IsReady("b"))
	assert.False(t, g.IsReady("e"), "closed beads are not ready")
	assert.False(t, g.IsReady("zz"))
	assert.Equal(t, graphIDs("b", "c", "d"), IssueIDs(g.Blocked()))
	assert.Equal(t, graphIDs("b", "c"), g.OpenBlockers("d"), "the closed blocker e is not open")
	assert.Equal(t, graphIDs("a", "b", "d"), g.CriticalPath(), "ties go to the earliest bead")
	assert.Equal(t, 3, g.CriticalPathLength())

	c := g.Counts()
	assert.Equal(t, Counts{Total: 5, Ready: 1, Blocked: 3, Closed: 1, ByStatus: map[string]int{"open": 4, "closed": 1}}, c)
	assert.Equal(t, "graph: 5 beads, 1 ready, 3 blocked, 1 closed, 3 layers", g.String())

	// Closing a frees b and c; closing b too still leaves d waiting on c.
	g = graphDiamond(map[string]domain.BeadState{"a": domain.BeadStateClosed})
	assert.Equal(t, graphIDs("b", "c"), IssueIDs(g.Ready()))
	assert.Equal(t, graphIDs("b", "d"), g.CriticalPath())
	g = graphDiamond(map[string]domain.BeadState{"a": domain.BeadStateClosed, "b": domain.BeadStateClosed})
	assert.Equal(t, graphIDs("c"), IssueIDs(g.Ready()))
	assert.Equal(t, graphIDs("d"), IssueIDs(g.Blocked()))
	assert.Equal(t, graphIDs("c"), g.OpenBlockers("d"))
	assert.Equal(t, graphIDs("c", "d"), g.CriticalPath())

	// A blocker in a custom (in-flight) status is open, and beads in one
	// are never ready themselves.
	g = graphDiamond(map[string]domain.BeadState{"a": domain.BeadStateInReview})
	assert.Empty(t, g.Ready())
	assert.Equal(t, graphIDs("a"), g.OpenBlockers("b"))
	assert.Equal(t, 1, g.Counts().ByStatus["in_review"])
}

func TestGraphCycle(t *testing.T) {
	// a -> b -> c -> a is a cycle; d depends on a so it cannot be layered
	// either; e is independent.
	g := NewGraph([]Issue{
		graphIssue("e", domain.BeadStateOpen),
		graphIssue("a", domain.BeadStateOpen, "b"),
		graphIssue("b", domain.BeadStateOpen, "c"),
		graphIssue("c", domain.BeadStateOpen, "a"),
		graphIssue("d", domain.BeadStateOpen, "a"),
	})
	err := g.Validate()
	require.Error(t, err)
	var cerr *CycleError
	require.ErrorAs(t, err, &cerr)
	assert.Equal(t, graphIDs("a", "b", "c"), cerr.Cycle)
	assert.EqualError(t, err, "beads: dependency cycle: a -> b -> c -> a")
	assert.ErrorIs(t, err, ErrCycle)
	assert.Equal(t, [][]domain.BeadID{graphIDs("a", "b", "c")}, g.Cycles())
	assert.Equal(t, [][]domain.BeadID{graphIDs("e")}, g.Layers())
	assert.Equal(t, -1, g.Layer("a"))
	assert.Equal(t, -1, g.Layer("d"), "downstream of a cycle")
	assert.Equal(t, graphIDs("e"), IssueIDs(g.Ready()))
	assert.Equal(t, graphIDs("e"), g.CriticalPath())
	assert.Contains(t, g.String(), "1 cycles")

	// Two disjoint cycles and a self-dependency are each reported once.
	g = NewGraph([]Issue{
		graphIssue("s", domain.BeadStateOpen, "s"),
		graphIssue("x", domain.BeadStateOpen, "y"),
		graphIssue("y", domain.BeadStateOpen, "x"),
		graphIssue("p", domain.BeadStateOpen, "q"),
		graphIssue("q", domain.BeadStateOpen, "r"),
		graphIssue("r", domain.BeadStateOpen, "p"),
	})
	assert.Equal(t, [][]domain.BeadID{graphIDs("s"), graphIDs("x", "y"), graphIDs("p", "q", "r")}, g.Cycles())
	assert.Empty(t, g.Layers())
	assert.EqualError(t, g.Validate(), "beads: dependency cycle: s -> s")

	assert.Equal(t, "", FormatCycle(nil))
}

func TestGraphDanglingBlocker(t *testing.T) {
	g := NewGraph([]Issue{
		graphIssue("a", domain.BeadStateOpen, "outside"),
		graphIssue("b", domain.BeadStateOpen, "a"),
	})
	assert.Equal(t, graphIDs("outside"), g.Dangling())
	assert.Equal(t, graphIDs("outside"), g.Blockers("a"))
	assert.False(t, g.IsReady("a"), "an unknown blocker counts as open")
	assert.Equal(t, graphIDs("outside"), g.OpenBlockers("a"))
	assert.Equal(t, graphIDs("a", "b"), IssueIDs(g.Blocked()))
	assert.Equal(t, 0, g.Layer("a"), "dangling blockers do not raise the layer")
	assert.Equal(t, 1, g.Layer("b"))
	assert.NoError(t, g.Validate())
}

func TestGraphReadyRules(t *testing.T) {
	epic := Issue{ID: "epic", Title: "Run", Status: "open", Type: "epic", Priority: 0}
	gate := Issue{ID: "gate", Title: "Gate: gh:pr 1", Status: "open", Type: "gate", Priority: 2}
	event := Issue{ID: "ev", Title: "state", Status: "open", Type: "event", Priority: 2}
	low := graphIssue("low", domain.BeadStateOpen)
	low.Priority = 3
	high := graphIssue("high", domain.BeadStateOpen)
	high.Priority = 1
	mid := graphIssue("mid", domain.BeadStateOpen)
	custom := graphIssue("custom", "someone_elses_status")
	blocked := Issue{ID: "blk", Title: "blocked by bd blocked", Status: "open", Type: "task", Priority: 2, BlockedBy: graphIDs("high")}
	g := NewGraph([]Issue{epic, gate, event, low, high, mid, custom, blocked})

	assert.Equal(t, graphIDs("high", "mid", "low"), IssueIDs(g.Ready()), "priority order, then insertion order")
	assert.False(t, g.IsReady("epic"), "epics are containers (P3-13)")
	assert.False(t, g.IsReady("gate"), "bd ready hides infrastructure beads")
	assert.False(t, g.IsReady("ev"))
	assert.False(t, g.IsReady("custom"), "only status open is ready")
	assert.Equal(t, graphIDs("high"), g.Blockers("blk"), "blocked_by from bd blocked is an edge")
	assert.Equal(t, graphIDs("blk"), IssueIDs(g.Blocked()), "epics and infra beads are never reported blocked")
	assert.Equal(t, 8, g.Counts().Total)
	assert.Equal(t, 3, g.Counts().Ready)
}

func TestGraphExplicitDeps(t *testing.T) {
	// Listings carry no edges; DepList supplies them. Other dependency
	// types and edges of unknown dependents are ignored, duplicates merge.
	issues := []Issue{
		graphIssue("epic", domain.BeadStateOpen),
		graphIssue("a", domain.BeadStateClosed),
		graphIssue("b", domain.BeadStateOpen),
		graphIssue("c", domain.BeadStateOpen),
	}
	deps := []Dependency{
		{IssueID: "b", DependsOnID: "a", Type: DepBlocks},
		{IssueID: "b", DependsOnID: "a", Type: DepBlocks},
		{IssueID: "b", DependsOnID: "epic", Type: DepParentChild},
		{IssueID: "c", DependsOnID: "b"},
		{IssueID: "c", DependsOnID: "epic", Type: DepRelated},
		{IssueID: "ghost", DependsOnID: "a", Type: DepBlocks},
		{IssueID: "", DependsOnID: "a", Type: DepBlocks},
		{IssueID: "c", DependsOnID: "", Type: DepBlocks},
	}
	g := NewGraph(issues, deps...)
	assert.Equal(t, graphIDs("a"), g.Blockers("b"))
	assert.Equal(t, graphIDs("b"), g.Blockers("c"), "an empty type means blocks")
	assert.Nil(t, g.Blockers("ghost"))
	assert.Empty(t, g.Dangling())
	assert.Equal(t, graphIDs("b"), g.Dependents("a"))
	assert.Equal(t, graphIDs("epic", "b"), IssueIDs(g.Ready()))
	assert.Equal(t, [][]domain.BeadID{graphIDs("epic", "a"), graphIDs("b"), graphIDs("c")}, g.Layers())

	// Edges from both sources merge without duplicates.
	withEdges := graphIssue("c", domain.BeadStateOpen, "b")
	g = NewGraph([]Issue{issues[1], issues[2], withEdges}, Dependency{IssueID: "c", DependsOnID: "b"}, Dependency{IssueID: "c", DependsOnID: "a"})
	assert.Equal(t, graphIDs("b", "a"), g.Blockers("c"))
}

func TestGraphDuplicatesAndEmpty(t *testing.T) {
	first := graphIssue("a", domain.BeadStateOpen)
	first.Title = "first"
	second := graphIssue("a", domain.BeadStateClosed)
	second.Title = "second"
	g := NewGraph([]Issue{first, graphIssue("b", domain.BeadStateOpen, "a"), second, {Title: "no id"}})
	assert.Equal(t, 2, g.Len())
	assert.Equal(t, graphIDs("a", "b"), g.IDs(), "the first position is kept")
	is, ok := g.Issue("a")
	require.True(t, ok)
	assert.Equal(t, "second", is.Title, "the last issue wins")
	assert.Equal(t, graphIDs("b"), IssueIDs(g.Ready()))
	_, ok = g.Issue("zz")
	assert.False(t, ok)

	empty := NewGraph(nil)
	assert.Zero(t, empty.Len())
	assert.Empty(t, empty.Ready())
	assert.Empty(t, empty.Blocked())
	assert.Empty(t, empty.Layers())
	assert.Empty(t, empty.CriticalPath())
	assert.Zero(t, empty.CriticalPathLength())
	assert.NoError(t, empty.Validate())
	assert.Equal(t, Counts{ByStatus: map[string]int{}}, empty.Counts())
	assert.Equal(t, "graph: 0 beads, 0 ready, 0 blocked, 0 closed, 0 layers", empty.String())
	assert.Empty(t, empty.Issues())
}

func TestGraphChildrenAndFilter(t *testing.T) {
	epic := Issue{ID: "epic", Title: "Run", Status: "open", Type: "epic", Priority: 1, Labels: []string{"sc:run:r1"}}
	a := graphIssue("a", domain.BeadStateOpen)
	a.Parent = "epic"
	a.Labels = []string{"sc:run:r1"}
	b := graphIssue("b", domain.BeadStateOpen, "a", "other")
	b.Dependencies = append(b.Dependencies, Dependency{IssueID: "b", DependsOnID: "epic", Type: DepParentChild})
	b.Labels = []string{"sc:run:r1"}
	other := graphIssue("other", domain.BeadStateOpen)
	g := NewGraph([]Issue{epic, a, b, other})
	assert.Equal(t, graphIDs("a", "b"), IssueIDs(g.Children("epic")))
	assert.Empty(t, g.Children("a"))

	run := g.Filter(func(is Issue) bool { return is.HasLabel("sc:run:r1") })
	assert.Equal(t, graphIDs("epic", "a", "b"), run.IDs())
	assert.Equal(t, graphIDs("a", "other"), run.Blockers("b"))
	assert.Equal(t, graphIDs("other"), run.Dangling(), "edges to dropped beads stay as dangling blockers")
	assert.False(t, run.IsReady("b"))
	assert.Equal(t, graphIDs("a"), IssueIDs(run.Ready()))
	assert.Equal(t, 4, g.Len(), "the original is untouched")

	// Explicit edges survive filtering too.
	g = NewGraph([]Issue{graphIssue("x", domain.BeadStateOpen), graphIssue("y", domain.BeadStateOpen)}, Dependency{IssueID: "y", DependsOnID: "x"})
	assert.Equal(t, graphIDs("x"), g.Filter(func(Issue) bool { return true }).Blockers("y"))
}

func TestGraphAccessorsReturnCopies(t *testing.T) {
	g := graphDiamond(nil)
	g.Blockers("d")[0] = "zz"
	g.Layers()[0][0] = "zz"
	g.CriticalPath()[0] = "zz"
	g.IDs()[0] = "zz"
	assert.Equal(t, graphIDs("b", "e", "c"), g.Blockers("d"))
	assert.Equal(t, graphIDs("a", "e"), g.Layers()[0])
	assert.Equal(t, graphIDs("a", "b", "d"), g.CriticalPath())
	assert.Equal(t, graphIDs("a", "b", "c", "d", "e"), g.IDs())
}

// graphRandomDAG builds n beads where bead i depends on up to fanIn
// earlier beads, with a share of them closed, so the result is a DAG by
// construction.
func graphRandomDAG(rng *rand.Rand, n, fanIn int) []Issue {
	issues := make([]Issue, n)
	for i := range issues {
		id := domain.BeadID("sc-" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26)))
		status := domain.BeadStateOpen
		if rng.Intn(4) == 0 {
			status = domain.BeadStateClosed
		}
		is := graphIssue(string(id), status)
		is.Priority = domain.Priority(rng.Intn(5))
		if i > 0 {
			for k := rng.Intn(fanIn + 1); k > 0; k-- {
				j := rng.Intn(i)
				is.Dependencies = append(is.Dependencies, Dependency{IssueID: id, DependsOnID: issues[j].ID, Type: DepBlocks})
			}
		}
		issues[i] = is
	}
	return issues
}

func TestGraphRandomDAG(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	issues := graphRandomDAG(rng, 200, 3)

	start := time.Now()
	g := NewGraph(issues)
	ready := g.Ready()
	layers := g.Layers()
	path := g.CriticalPath()
	counts := g.Counts()
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 50*time.Millisecond, "200-node graph took %s", elapsed)

	require.NoError(t, g.Validate())
	assert.Equal(t, 200, g.Len())
	assert.Equal(t, len(ready), counts.Ready)

	// Every layer only depends on lower layers and covers every bead.
	covered := 0
	for depth, wave := range layers {
		covered += len(wave)
		for _, id := range wave {
			assert.Equal(t, depth, g.Layer(id))
			for _, b := range g.Blockers(id) {
				assert.Less(t, g.Layer(b), depth, "%s depends on %s", id, b)
			}
		}
	}
	assert.Equal(t, 200, covered)

	// The ready set matches a brute-force evaluation of bd's rule.
	var want []domain.BeadID
	for _, is := range issues {
		ok := is.Status == "open"
		for _, b := range is.Blockers() {
			bi, _ := g.Issue(b)
			ok = ok && bi.Status == "closed"
		}
		if ok {
			want = append(want, is.ID)
		}
	}
	assert.ElementsMatch(t, want, IssueIDs(ready))
	for i := 1; i < len(ready); i++ {
		assert.LessOrEqual(t, ready[i-1].Priority, ready[i].Priority)
	}

	// The critical path is a chain of open beads, each depending on the
	// previous one.
	require.NotEmpty(t, path)
	for i, id := range path {
		is, ok := g.Issue(id)
		require.True(t, ok)
		assert.NotEqual(t, "closed", is.Status)
		if i > 0 {
			assert.Contains(t, g.Blockers(id), path[i-1])
		}
	}
	assert.Equal(t, len(path), g.CriticalPathLength())
}

func TestGraphRandomDAGWithCycle(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	issues := graphRandomDAG(rng, 200, 3)
	// Close the loop: the first bead depends on the last, which (like many
	// others, transitively) depends on the first.
	last := issues[len(issues)-1]
	issues[0].Dependencies = append(issues[0].Dependencies, Dependency{IssueID: issues[0].ID, DependsOnID: last.ID, Type: DepBlocks})
	issues[len(issues)-1].Dependencies = append(last.Dependencies, Dependency{IssueID: last.ID, DependsOnID: issues[0].ID, Type: DepBlocks})
	start := time.Now()
	g := NewGraph(issues)
	assert.Less(t, time.Since(start), 50*time.Millisecond)
	err := g.Validate()
	require.ErrorIs(t, err, ErrCycle)
	var cerr *CycleError
	require.ErrorAs(t, err, &cerr)
	assert.Contains(t, cerr.Cycle, issues[0].ID)
	assert.Contains(t, cerr.Cycle, last.ID)
	assert.Equal(t, -1, g.Layer(issues[0].ID))
	assert.Equal(t, -1, g.Layer(last.ID))
	for _, c := range g.Cycles() {
		for i, id := range c {
			next := c[(i+1)%len(c)]
			assert.Contains(t, g.Blockers(id), next, "each cycle entry depends on the next")
		}
	}
}

// TestGraphDifferentialReady asserts the model's ready set on the captured
// `bd export` snapshot equals what `bd ready` printed for the same state
// (the ready-after-close sample, taken right after the export).
func TestGraphDifferentialReady(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(envSamplesDir(t), "export.jsonl"))
	require.NoError(t, err)
	issues, err := ParseExport(strings.NewReader(string(data)))
	require.NoError(t, err)
	g := NewGraph(issues)
	require.NoError(t, g.Validate())

	c, bd := clientFake(t, map[string]string{"ready": "ready-after-close"})
	want, err := c.Ready(context.Background(), "")
	require.NoError(t, err)
	assert.Empty(t, testutil.MissingSamples(t, bd))
	require.NotEmpty(t, want)
	assert.ElementsMatch(t, IssueIDs(want), IssueIDs(g.Ready()))

	// The rest of the snapshot is consistent with the fixture's story:
	// fx-nvi.1 waits on fx-nvi.2, fx-ctt was freed by closing fx-so0.
	assert.Equal(t, graphIDs("fx-nvi.1"), IssueIDs(g.Blocked()))
	assert.Equal(t, graphIDs("fx-nvi.2"), g.OpenBlockers("fx-nvi.1"))
	assert.Equal(t, graphIDs("fx-so0"), g.Blockers("fx-ctt"))
	assert.Equal(t, 1, g.Layer("fx-ctt"))
	assert.Equal(t, graphIDs("fx-so0", "fx-ctt", "fx-nvi.1", "fx-nvi.2"), IssueIDs(g.Children("fx-nvi")))
	assert.Equal(t, graphIDs("fx-nvi.2", "fx-nvi.1"), g.CriticalPath())
}

func TestGraphIsSafeForConcurrentReads(t *testing.T) {
	g := graphDiamond(nil)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for k := 0; k < 100; k++ {
				_ = g.Ready()
				_ = g.Layers()
				_ = g.Counts()
				_ = g.CriticalPath()
				_ = g.Validate()
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	assert.Equal(t, graphIDs("a"), IssueIDs(g.Ready()))
}
