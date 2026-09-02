package beads

import (
	"fmt"
	"sort"
	"strings"

	"slop-cannon/internal/domain"
)

// Graph is an in-memory model of a set of beads and their `blocks`
// dependencies, built from one Client.Export snapshot (or Client.List plus
// Client.DepList for the run's beads) with NewGraph. It is a pure data
// structure: it does no I/O, never changes after construction, and every
// accessor returns copies, so it may be shared between goroutines.
//
// Only `blocks` edges are modelled (P3-04): `parent-child`, `related`,
// `discovered-from` and the other bd dependency types never affect
// readiness (Appendix A) and are ignored here. In every method the edge
// direction is "IssueID depends on DependsOnID", matching
// `bd dep add <issue> <depends-on>` (D-20).
//
// The readiness rule mirrors `bd ready` (D-20): a bead is ready when its
// status is open, every blocker is closed, and it is not an infrastructure
// bead (a gate or event bd minted itself, which `bd ready` hides without
// --include-infra). Epics are additionally excluded, as Client.Ready does
// (P3-13): they are containers, never work.
//
// A blocker that is referenced by an edge but absent from the snapshot (a
// List of one run whose bead depends on something outside it) is recorded
// as dangling and counts as open, so the model never reports a bead ready
// that bd could refuse to close; Dangling lists such ids so callers can
// fetch them and rebuild.
type Graph struct {
	issues map[domain.BeadID]Issue
	index  map[domain.BeadID]int
	order  []domain.BeadID

	blockers   map[domain.BeadID][]domain.BeadID
	dependents map[domain.BeadID][]domain.BeadID
	dangling   []domain.BeadID

	layer  map[domain.BeadID]int
	layers [][]domain.BeadID
	cycles [][]domain.BeadID

	critical []domain.BeadID
}

// CycleError reports a dependency cycle found by Graph.Validate. It
// unwraps to ErrCycle so errors.Is(err, ErrCycle) holds for both the
// model's detection and bd's own write-time refusal.
type CycleError struct {
	// Cycle lists the beads on the cycle in dependency order: each entry
	// depends on the next, and the last depends on the first.
	Cycle []domain.BeadID
}

// Error returns "beads: dependency cycle: a -> b -> a".
func (e *CycleError) Error() string {
	return "beads: dependency cycle: " + FormatCycle(e.Cycle)
}

// Unwrap returns ErrCycle.
func (e *CycleError) Unwrap() error { return ErrCycle }

// FormatCycle renders a cycle as "a -> b -> c -> a", repeating the first
// bead at the end so the closing edge is visible.
func FormatCycle(cycle []domain.BeadID) string {
	if len(cycle) == 0 {
		return ""
	}
	parts := make([]string, 0, len(cycle)+1)
	for _, id := range cycle {
		parts = append(parts, string(id))
	}
	parts = append(parts, string(cycle[0]))
	return strings.Join(parts, " -> ")
}

// Counts summarises a Graph.
type Counts struct {
	// Total is the number of beads in the graph.
	Total int
	// Ready is the number of beads Graph.Ready would list.
	Ready int
	// Blocked is the number of beads Graph.Blocked would list: not closed,
	// with at least one open blocker.
	Blocked int
	// Closed is the number of beads in status closed.
	Closed int
	// ByStatus counts beads per raw bd status name, custom statuses
	// included.
	ByStatus map[string]int
}

// NewGraph builds a Graph from issues and their `blocks` edges. Edges come
// from each issue's Dependencies and BlockedBy (as Issue.Blockers reports
// them, which is what Client.Export and Client.Blocked fill) plus any deps
// passed explicitly (the Client.DepList result when the issues came from
// Client.List, whose listings carry no edges). Edges of other types, edges
// whose dependent issue is not in the graph, and duplicates are dropped; an
// edge whose blocker is not in the graph is kept and the blocker recorded
// as dangling. When an id appears twice the last issue wins but keeps the
// first position. Order is preserved everywhere: issues in the order given,
// blockers in the order their edges were seen.
func NewGraph(issues []Issue, deps ...Dependency) *Graph {
	g := &Graph{
		issues:     make(map[domain.BeadID]Issue, len(issues)),
		index:      make(map[domain.BeadID]int, len(issues)),
		blockers:   make(map[domain.BeadID][]domain.BeadID, len(issues)),
		dependents: make(map[domain.BeadID][]domain.BeadID, len(issues)),
		layer:      make(map[domain.BeadID]int, len(issues)),
	}
	for _, is := range issues {
		if is.ID == "" {
			continue
		}
		if _, seen := g.index[is.ID]; !seen {
			g.index[is.ID] = len(g.order)
			g.order = append(g.order, is.ID)
		}
		g.issues[is.ID] = is
	}
	seenEdge := map[[2]domain.BeadID]bool{}
	danglingSeen := map[domain.BeadID]bool{}
	addEdge := func(issue, blocker domain.BeadID) {
		if issue == "" || blocker == "" {
			return
		}
		if _, ok := g.index[issue]; !ok {
			return
		}
		key := [2]domain.BeadID{issue, blocker}
		if seenEdge[key] {
			return
		}
		seenEdge[key] = true
		g.blockers[issue] = append(g.blockers[issue], blocker)
		if _, ok := g.index[blocker]; ok {
			g.dependents[blocker] = append(g.dependents[blocker], issue)
		} else if !danglingSeen[blocker] {
			danglingSeen[blocker] = true
			g.dangling = append(g.dangling, blocker)
		}
	}
	for _, id := range g.order {
		for _, b := range g.issues[id].Blockers() {
			addEdge(id, b)
		}
	}
	for _, d := range deps {
		if d.Type != "" && d.Type != DepBlocks {
			continue
		}
		addEdge(d.IssueID, d.DependsOnID)
	}
	g.computeLayers()
	g.computeCycles()
	g.computeCriticalPath()
	return g
}

// computeLayers assigns topological layers with Kahn's algorithm: layer 0
// holds the beads with no in-graph blockers and every other bead sits one
// above its highest blocker. Beads on or downstream of a cycle never reach
// in-degree zero and get layer -1.
func (g *Graph) computeLayers() {
	indegree := make(map[domain.BeadID]int, len(g.order))
	for _, id := range g.order {
		g.layer[id] = -1
		for _, b := range g.blockers[id] {
			if _, ok := g.index[b]; ok {
				indegree[id]++
			}
		}
	}
	var wave []domain.BeadID
	for _, id := range g.order {
		if indegree[id] == 0 {
			wave = append(wave, id)
		}
	}
	for depth := 0; len(wave) > 0; depth++ {
		g.sortByIndex(wave)
		g.layers = append(g.layers, wave)
		var next []domain.BeadID
		for _, id := range wave {
			g.layer[id] = depth
			for _, d := range g.dependents[id] {
				indegree[d]--
				if indegree[d] == 0 {
					next = append(next, d)
				}
			}
		}
		wave = next
	}
}

// computeCycles walks the beads left without a layer (every cycle lives
// among them) with a depth-first search along blocker edges and records one
// cycle per back edge, each rotated to start at its earliest bead and
// deduplicated.
func (g *Graph) computeCycles() {
	const (
		white = iota
		grey
		black
	)
	colour := make(map[domain.BeadID]int, len(g.order))
	var stack []domain.BeadID
	seen := map[string]bool{}
	var visit func(id domain.BeadID)
	visit = func(id domain.BeadID) {
		colour[id] = grey
		stack = append(stack, id)
		for _, b := range g.blockers[id] {
			if g.layer[b] != -1 {
				continue // layered beads cannot be on a cycle
			}
			if _, ok := g.index[b]; !ok {
				continue
			}
			switch colour[b] {
			case white:
				visit(b)
			case grey:
				start := len(stack) - 1
				for start >= 0 && stack[start] != b {
					start--
				}
				cycle := g.rotateCycle(append([]domain.BeadID(nil), stack[start:]...))
				key := FormatCycle(cycle)
				if !seen[key] {
					seen[key] = true
					g.cycles = append(g.cycles, cycle)
				}
			}
		}
		stack = stack[:len(stack)-1]
		colour[id] = black
	}
	for _, id := range g.order {
		if g.layer[id] == -1 && colour[id] == white {
			visit(id)
		}
	}
}

// rotateCycle rotates cycle so it starts at the bead with the lowest
// insertion index, keeping the dependency order.
func (g *Graph) rotateCycle(cycle []domain.BeadID) []domain.BeadID {
	if len(cycle) < 2 {
		return cycle
	}
	best := 0
	for i, id := range cycle {
		if g.index[id] < g.index[cycle[best]] {
			best = i
		}
	}
	return append(append([]domain.BeadID(nil), cycle[best:]...), cycle[:best]...)
}

// computeCriticalPath finds the longest chain of remaining (not closed)
// beads along blocker edges among the layered beads, blocker first. Ties
// go to the earliest bead so the result is deterministic.
func (g *Graph) computeCriticalPath() {
	best := make(map[domain.BeadID]int, len(g.order))
	prev := make(map[domain.BeadID]domain.BeadID, len(g.order))
	var top domain.BeadID
	for _, wave := range g.layers {
		for _, id := range wave {
			if g.isClosed(id) {
				continue
			}
			best[id] = 1
			for _, b := range g.blockers[id] {
				if n, ok := best[b]; ok && n+1 > best[id] {
					best[id] = n + 1
					prev[id] = b
				}
			}
			if top == "" || best[id] > best[top] {
				top = id
			}
		}
	}
	if top == "" {
		return
	}
	var path []domain.BeadID
	for id := top; id != ""; id = prev[id] {
		path = append(path, id)
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	g.critical = path
}

// sortByIndex orders ids by their insertion position.
func (g *Graph) sortByIndex(ids []domain.BeadID) {
	sort.SliceStable(ids, func(i, j int) bool { return g.index[ids[i]] < g.index[ids[j]] })
}

// isClosed reports whether the bead is in the graph with status closed.
func (g *Graph) isClosed(id domain.BeadID) bool {
	is, ok := g.issues[id]
	return ok && is.Status == string(domain.BeadStateClosed)
}

// isInfra reports whether the issue is a bd infrastructure bead (gate or
// event), which `bd ready` hides.
func isInfra(is Issue) bool {
	return is.Type == string(domain.BeadTypeGate) || is.Type == string(domain.BeadTypeEvent)
}

// Len returns the number of beads in the graph.
func (g *Graph) Len() int { return len(g.order) }

// Has reports whether id is in the graph.
func (g *Graph) Has(id domain.BeadID) bool {
	_, ok := g.index[id]
	return ok
}

// Issue returns the bead with the given id.
func (g *Graph) Issue(id domain.BeadID) (Issue, bool) {
	is, ok := g.issues[id]
	return is, ok
}

// Issues returns every bead in insertion order.
func (g *Graph) Issues() []Issue {
	out := make([]Issue, 0, len(g.order))
	for _, id := range g.order {
		out = append(out, g.issues[id])
	}
	return out
}

// IDs returns every bead id in insertion order.
func (g *Graph) IDs() []domain.BeadID { return append([]domain.BeadID(nil), g.order...) }

// Children returns the beads whose parent (Issue.ParentID) is parent, in
// insertion order.
func (g *Graph) Children(parent domain.BeadID) []Issue {
	var out []Issue
	for _, id := range g.order {
		if is := g.issues[id]; is.ParentID() == parent {
			out = append(out, is)
		}
	}
	return out
}

// Blockers returns the beads id depends on through `blocks` edges, in edge
// order, dangling ids included.
func (g *Graph) Blockers(id domain.BeadID) []domain.BeadID {
	return append([]domain.BeadID(nil), g.blockers[id]...)
}

// Dependents returns the beads that depend on id, in insertion order.
func (g *Graph) Dependents(id domain.BeadID) []domain.BeadID {
	return append([]domain.BeadID(nil), g.dependents[id]...)
}

// OpenBlockers returns the blockers of id that are not closed: beads in the
// graph with any status other than closed, and every dangling blocker.
func (g *Graph) OpenBlockers(id domain.BeadID) []domain.BeadID {
	var out []domain.BeadID
	for _, b := range g.blockers[id] {
		if !g.isClosed(b) {
			out = append(out, b)
		}
	}
	return out
}

// Dangling returns the blockers referenced by edges but absent from the
// snapshot, in first-reference order. They count as open; fetch them and
// rebuild the graph to know their real state.
func (g *Graph) Dangling() []domain.BeadID { return append([]domain.BeadID(nil), g.dangling...) }

// IsReady reports whether id would be listed by `bd ready` as claimable
// work: it is in the graph in status open, every blocker is closed, and it
// is neither an epic nor an infrastructure bead (see Graph).
func (g *Graph) IsReady(id domain.BeadID) bool {
	is, ok := g.issues[id]
	if !ok || is.Status != string(domain.BeadStateOpen) || is.IsEpic() || isInfra(is) {
		return false
	}
	for _, b := range g.blockers[id] {
		if !g.isClosed(b) {
			return false
		}
	}
	return true
}

// Ready returns the claimable beads (IsReady), most urgent priority first
// and insertion order within a priority, the way the scheduler consumes
// them.
func (g *Graph) Ready() []Issue {
	var out []Issue
	for _, id := range g.order {
		if g.IsReady(id) {
			out = append(out, g.issues[id])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out
}

// Blocked returns the beads that cannot proceed: not closed, not an epic or
// infrastructure bead, with at least one open blocker (as `bd blocked`
// reports them), in insertion order.
func (g *Graph) Blocked() []Issue {
	var out []Issue
	for _, id := range g.order {
		is := g.issues[id]
		if g.isClosed(id) || is.IsEpic() || isInfra(is) {
			continue
		}
		if len(g.OpenBlockers(id)) > 0 {
			out = append(out, is)
		}
	}
	return out
}

// Layers returns the topological layers: layer 0 holds the beads with no
// blocker in the graph, and every other bead sits one layer above its
// highest blocker, so a layer only depends on lower layers. Beads within a
// layer are in insertion order. Beads on or downstream of a cycle are in no
// layer (see Layer and Cycles). Dangling blockers do not raise a layer.
func (g *Graph) Layers() [][]domain.BeadID {
	out := make([][]domain.BeadID, len(g.layers))
	for i, wave := range g.layers {
		out[i] = append([]domain.BeadID(nil), wave...)
	}
	return out
}

// Layer returns the topological layer of id, or -1 when id is unknown or
// cannot be layered because it is on or downstream of a cycle.
func (g *Graph) Layer(id domain.BeadID) int {
	if l, ok := g.layer[id]; ok {
		return l
	}
	return -1
}

// Cycles returns every dependency cycle found, each in dependency order
// starting at its earliest bead (a self-dependency is a cycle of one). bd
// rejects cycles at write time, so a non-empty result means the snapshot
// came from a repository edited by other tools.
func (g *Graph) Cycles() [][]domain.BeadID {
	out := make([][]domain.BeadID, len(g.cycles))
	for i, c := range g.cycles {
		out[i] = append([]domain.BeadID(nil), c...)
	}
	return out
}

// Validate returns a *CycleError naming the first cycle, or nil when the
// graph is a DAG.
func (g *Graph) Validate() error {
	if len(g.cycles) == 0 {
		return nil
	}
	return &CycleError{Cycle: append([]domain.BeadID(nil), g.cycles[0]...)}
}

// CriticalPath returns the longest chain of remaining work: beads that are
// not closed, each depending on the previous one, blocker first. It is the
// minimum number of sequential beads left before the run can finish. Beads
// on or downstream of a cycle are not considered.
func (g *Graph) CriticalPath() []domain.BeadID {
	return append([]domain.BeadID(nil), g.critical...)
}

// CriticalPathLength returns len(CriticalPath()).
func (g *Graph) CriticalPathLength() int { return len(g.critical) }

// Counts summarises the graph: totals, ready and blocked beads, and the
// number of beads per status.
func (g *Graph) Counts() Counts {
	c := Counts{Total: len(g.order), ByStatus: make(map[string]int)}
	for _, id := range g.order {
		is := g.issues[id]
		c.ByStatus[is.Status]++
		if g.isClosed(id) {
			c.Closed++
		}
		if g.IsReady(id) {
			c.Ready++
		}
	}
	c.Blocked = len(g.Blocked())
	return c
}

// Filter returns a new graph holding only the beads keep accepts, in the
// same order, with the edges between them. Edges to dropped beads make
// those beads dangling blockers of the new graph, so readiness stays
// conservative; use it to narrow an Export snapshot to one run (for example
// by the sc:run:<id> label).
func (g *Graph) Filter(keep func(Issue) bool) *Graph {
	var issues []Issue
	var deps []Dependency
	for _, id := range g.order {
		is := g.issues[id]
		if !keep(is) {
			continue
		}
		issues = append(issues, is)
		for _, b := range g.blockers[id] {
			deps = append(deps, Dependency{IssueID: id, DependsOnID: b, Type: DepBlocks})
		}
	}
	return NewGraph(issues, deps...)
}

// String renders a one-line summary for logs: bead, ready, blocked and
// closed counts, plus the cycle count when there is one.
func (g *Graph) String() string {
	c := g.Counts()
	s := fmt.Sprintf("graph: %d beads, %d ready, %d blocked, %d closed, %d layers", c.Total, c.Ready, c.Blocked, c.Closed, len(g.layers))
	if len(g.cycles) > 0 {
		s += fmt.Sprintf(", %d cycles", len(g.cycles))
	}
	return s
}
