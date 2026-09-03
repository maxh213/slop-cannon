package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
)

// GraphOrientation is how the graph view lays the dependency DAG out.
type GraphOrientation string

// The layouts. [GraphAuto] is the zero value: the view picks one from the
// pane's shape every time it renders.
const (
	// GraphAuto lets the view choose: the indented list below
	// [GraphNarrowWidth] columns, top→bottom when the pane is taller than it
	// is wide (terminal cells being about twice as tall as they are wide),
	// and left→right otherwise.
	GraphAuto GraphOrientation = ""
	// GraphLeftToRight draws topological layers as columns, blockers on the
	// left, with the edges running rightwards.
	GraphLeftToRight GraphOrientation = "left→right"
	// GraphTopToBottom draws topological layers as rows, blockers on top,
	// with the edges running downwards.
	GraphTopToBottom GraphOrientation = "top→bottom"
	// GraphIndentedList is the fallback for narrow terminals: one line per
	// bead, grouped and indented by level.
	GraphIndentedList GraphOrientation = "list"
)

// String returns the layout name shown in the view's summary line.
func (o GraphOrientation) String() string {
	if o == GraphAuto {
		return "auto"
	}
	return string(o)
}

// Geometry of the boxed layouts, in terminal cells.
const (
	// GraphNarrowWidth is the pane width below which the boxed layouts are
	// replaced by the indented list: two boxes and a gutter no longer fit.
	GraphNarrowWidth = 80
	// GraphBoxWidth is the outer width of a bead box, borders included.
	GraphBoxWidth = 22
	// GraphBoxHeight is the outer height of a bead box: border, the id line,
	// the title line, border.
	GraphBoxHeight = 4
	// GraphGutter is how many columns separate two layers drawn left→right;
	// the edges turn in the middle of it.
	GraphGutter = 4
	// GraphRowGap is how many blank rows separate two boxes of the same
	// layer drawn left→right.
	GraphRowGap = 1
	// GraphColGap is how many blank columns separate two boxes of the same
	// layer drawn top→bottom.
	GraphColGap = 2
	// GraphLayerGap is how many rows separate two layers drawn top→bottom;
	// the edges turn in the middle of them.
	GraphLayerGap = 3
)

// graphCycleLabel is written into the top border of a bead the layering could
// not place, which is how the boxed layouts show a dependency cycle.
const graphCycleLabel = " cycle "

// GraphNode is one bead as the graph view sees it: the bead itself, where the
// layering put it, and the facts the view draws around it — whether it is
// claimable, blocked or stuck on a cycle, and which agent is on it.
type GraphNode struct {
	// Bead is the bead as the last graph snapshot described it.
	domain.Bead
	// Layer is the topological level: 0 for a bead with no blocker in the
	// run, one more than its highest blocker otherwise. Beads on or after a
	// dependency cycle cannot be layered and are put in a final level of
	// their own, with Cycle set.
	Layer int
	// Slot is the position of the bead within its level, in creation order.
	Slot int
	// Ready is true when `bd ready` would offer the bead as claimable work.
	Ready bool
	// Blocked is true when the bead still waits on an open blocker.
	Blocked bool
	// Cycle is true when the bead sits on or downstream of a dependency
	// cycle, so it has no honest level.
	Cycle bool
	// Blockers are the beads this one waits on, dangling ids included.
	Blockers []domain.BeadID
	// Dependents are the beads waiting on this one.
	Dependents []domain.BeadID
	// Agent is the newest agent seen working on the bead; HasAgent says
	// whether there was one.
	Agent AgentView
	// HasAgent is true when Agent is set.
	HasAgent bool
}

// Badge renders the review-round badge drawn in the box's top right corner,
// empty before the first round.
func (n GraphNode) Badge() string {
	if n.Round <= 0 {
		return ""
	}
	return fmt.Sprintf("r%d", n.Round)
}

// Glyph returns the bead state's one-cell marker.
func (n GraphNode) Glyph() string { return StateGlyph(n.State) }

// AgentGlyph returns the role marker of the agent on the bead, and a space
// when no agent has been fired at it, so boxes stay aligned.
func (n GraphNode) AgentGlyph() string {
	if !n.HasAgent {
		return " "
	}
	return RoleGlyph(n.Agent.Role)
}

// GraphLayout is the layered DAG of a run: the beads in level order with the
// levels that hold them. It is derived from [State] and the [beads.Graph]
// layering (P3-04), never from the view's geometry, so the headless renderer
// and the tests can use it too.
type GraphLayout struct {
	// Nodes are the beads in level order: level 0 first, and creation order
	// within a level.
	Nodes []GraphNode
	// Layers index into Nodes, one entry per topological level. The last
	// entry holds the beads on a dependency cycle, when there are any.
	Layers [][]int
	// Graph is the dependency model the layering came from.
	Graph *beads.Graph
	// Cycles are the dependency cycles found, if any; bd refuses to create
	// one, so a non-empty list means the repository was edited elsewhere.
	Cycles [][]domain.BeadID
	// Dangling are blockers referenced by a bead but missing from the run's
	// snapshot; they count as open.
	Dangling []domain.BeadID

	index map[domain.BeadID]int
}

// Len returns how many beads the layout holds.
func (l GraphLayout) Len() int { return len(l.Nodes) }

// Index returns the position of a bead in [GraphLayout.Nodes].
func (l GraphLayout) Index(id domain.BeadID) (int, bool) {
	i, ok := l.index[id]
	return i, ok
}

// Node returns the bead with the given id.
func (l GraphLayout) Node(id domain.BeadID) (GraphNode, bool) {
	i, ok := l.index[id]
	if !ok {
		return GraphNode{}, false
	}
	return l.Nodes[i], true
}

// Counts returns how many beads are claimable, blocked and closed.
func (l GraphLayout) Counts() (ready, blocked, closed int) {
	for _, n := range l.Nodes {
		switch {
		case n.Ready:
			ready++
		case n.Blocked:
			blocked++
		}
		if n.State == domain.BeadStateClosed {
			closed++
		}
	}
	return ready, blocked, closed
}

// BuildGraphLayout folds the run state into a layered DAG. The layering, the
// readiness rule and the cycle detection all come from [beads.NewGraph], so
// the picture agrees with what `bd ready` and the scheduler see.
func BuildGraphLayout(s *State) GraphLayout {
	if s == nil || len(s.Beads) == 0 {
		return GraphLayout{index: map[domain.BeadID]int{}}
	}
	issues := make([]beads.Issue, 0, len(s.Beads))
	var deps []beads.Dependency
	for _, b := range s.Beads {
		issues = append(issues, graphIssue(b))
		for _, d := range b.Deps {
			deps = append(deps, beads.Dependency{IssueID: b.ID, DependsOnID: d, Type: beads.DepBlocks})
		}
	}
	g := beads.NewGraph(issues, deps...)

	lay := GraphLayout{
		Graph:    g,
		Cycles:   g.Cycles(),
		Dangling: g.Dangling(),
		index:    make(map[domain.BeadID]int, len(s.Beads)),
	}
	layers := g.Layers()
	// Beads on or downstream of a cycle never reach a level; they still have
	// to be drawn, so they become one last level of their own.
	var stuck []domain.BeadID
	for _, b := range s.Beads {
		if g.Layer(b.ID) < 0 {
			stuck = append(stuck, b.ID)
		}
	}
	if len(stuck) > 0 {
		layers = append(layers, stuck)
	}
	agents := graphAgents(s)
	for depth, wave := range layers {
		row := make([]int, 0, len(wave))
		for slot, id := range wave {
			b, ok := s.Bead(id)
			if !ok {
				continue
			}
			n := GraphNode{
				Bead:       b,
				Layer:      depth,
				Slot:       slot,
				Ready:      g.IsReady(id),
				Blocked:    len(g.OpenBlockers(id)) > 0,
				Cycle:      g.Layer(id) < 0,
				Blockers:   g.Blockers(id),
				Dependents: g.Dependents(id),
			}
			if a, ok := agents[id]; ok {
				n.Agent, n.HasAgent = a, true
			}
			lay.index[id] = len(lay.Nodes)
			row = append(row, len(lay.Nodes))
			lay.Nodes = append(lay.Nodes, n)
		}
		lay.Layers = append(lay.Layers, row)
	}
	return lay
}

// graphIssue converts a bead into the bd issue shape [beads.NewGraph] takes.
// Only the fields the layering and the readiness rule read are filled.
func graphIssue(b domain.Bead) beads.Issue {
	typ := b.Type
	if typ == "" {
		typ = domain.BeadTypeTask
	}
	return beads.Issue{
		ID:       b.ID,
		Title:    b.Title,
		Status:   string(b.State),
		Type:     string(typ),
		Priority: b.Priority,
		Assignee: b.Assignee,
	}
}

// graphAgents indexes the newest agent seen on each bead: a live one wins
// over a finished one, so the box shows who is on the bead now.
func graphAgents(s *State) map[domain.BeadID]AgentView {
	out := make(map[domain.BeadID]AgentView, len(s.Agents))
	for _, a := range s.Agents {
		if a.Bead == "" {
			continue
		}
		if prev, ok := out[a.Bead]; ok && prev.Live() && !a.Live() {
			continue
		}
		out[a.Bead] = a
	}
	return out
}

// GraphKeyMap are the bindings the graph view adds to the global [KeyMap].
// Up and Down come from the global map and step through the beads in level
// order; these move between levels and open the detail panel.
type GraphKeyMap struct {
	// Left moves to the previous level.
	Left key.Binding
	// Right moves to the next level.
	Right key.Binding
	// Open shows the selected bead's detail panel.
	Open key.Binding
	// Close hides the detail panel.
	Close key.Binding
	// Top selects the first bead of the first level.
	Top key.Binding
	// Bottom selects the last bead of the last level.
	Bottom key.Binding
}

// DefaultGraphKeyMap returns the standard bindings of the graph view.
func DefaultGraphKeyMap() GraphKeyMap {
	return GraphKeyMap{
		Left:   key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←/h", "prev level")),
		Right:  key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→/l", "next level")),
		Open:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "bead detail")),
		Close:  key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "close detail")),
		Top:    key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("g", "first bead")),
		Bottom: key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("G", "last bead")),
	}
}

// ShortHelp implements the [help.KeyMap] interface: the keys the footer can
// show while the graph view is up.
func (k GraphKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Left, k.Right, k.Open}
}

// FullHelp implements the [help.KeyMap] interface.
func (k GraphKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Left, k.Right}, {k.Open, k.Close}, {k.Top, k.Bottom}}
}

// GraphOptions configure the graph view.
type GraphOptions struct {
	// NoColor renders without any ANSI colour, like --no-color and NO_COLOR.
	// It is ignored when Theme is set.
	NoColor bool
	// Theme overrides the palette; nil means [EnvTheme] for NoColor.
	Theme *Theme
	// Keys overrides the view's own bindings; nil means [DefaultGraphKeyMap].
	Keys *GraphKeyMap
	// Orientation pins the layout; the zero value [GraphAuto] picks one from
	// the pane's shape.
	Orientation GraphOrientation
}

// graphDefaultWidth and graphDefaultHeight are the pane size the view
// assumes before [GraphModel.Resize] is called.
const (
	graphDefaultWidth  = 80
	graphDefaultHeight = 20
)

// GraphModel is the graph view (P7-02): the run's beads laid out as a
// layered DAG — levels left→right, or top→bottom when the pane is taller
// than it is wide — with box-drawing edges, a cursor that walks the beads,
// and `enter` for a bead's description, acceptance criteria and history. A
// terminal narrower than [GraphNarrowWidth] gets an indented list grouped by
// level instead.
//
// It keeps only its own cursor and geometry: everything it draws comes from
// the [State] handed to [GraphModel.View] and [GraphModel.Update], so a copy
// of the model is as good as the original.
type GraphModel struct {
	theme       *Theme
	keys        GraphKeyMap
	orientation GraphOrientation

	width, height int
	cursor        int
	selected      domain.BeadID
	detail        bool
	detailOff     int
}

// NewGraphModel builds the graph view. The zero [GraphOptions] is valid.
func NewGraphModel(opts GraphOptions) GraphModel {
	theme := opts.Theme
	if theme == nil {
		theme = EnvTheme(opts.NoColor)
	}
	keys := DefaultGraphKeyMap()
	if opts.Keys != nil {
		keys = *opts.Keys
	}
	return GraphModel{
		theme:       theme,
		keys:        keys,
		orientation: opts.Orientation,
		width:       graphDefaultWidth,
		height:      graphDefaultHeight,
	}
}

// Resize tells the view how much room it has: the main pane's size, which is
// the terminal height less [chromeHeight].
func (v GraphModel) Resize(w, h int) GraphModel {
	v.width, v.height = max(w, 1), max(h, 1)
	return v
}

// Size returns the pane size the view was last resized to.
func (v GraphModel) Size() (int, int) { return v.width, v.height }

// Cursor returns the row the last key press put the cursor on, as an index
// into [GraphLayout.Nodes]. [GraphModel.Selected] is what follows the pinned
// bead through a re-layering; this is the row number the chrome asks for.
func (v GraphModel) Cursor() int { return v.cursor }

// SelectedID returns the bead id the selection is pinned to, empty until the
// cursor has been moved or a detail panel opened.
func (v GraphModel) SelectedID() domain.BeadID { return v.selected }

// index is the position of the selection in the layout: the position of the
// pinned bead id while that bead is still in the run, and the clamped cursor
// otherwise. The layering moves beads under the cursor — the product owner
// re-plans whenever the ready queue empties (D-18), and a single new root
// bead renumbers every level after it — so the pinned id, not the index, is
// what keeps the selection (and an open detail panel) on the bead that was
// chosen.
func (v GraphModel) index(lay GraphLayout) int {
	if v.selected != "" {
		if i, ok := lay.Index(v.selected); ok {
			return i
		}
	}
	return clampIndex(v.cursor, lay.Len())
}

// moveTo puts the cursor on the i-th bead of the layout (clamped) and pins
// the selection to that bead's id.
func (v *GraphModel) moveTo(lay GraphLayout, i int) {
	v.cursor = clampIndex(i, lay.Len())
	if lay.Len() > 0 {
		v.selected = lay.Nodes[v.cursor].ID
	}
}

// Keys returns the view's own bindings.
func (v GraphModel) Keys() GraphKeyMap { return v.keys }

// ShowingDetail reports whether the bead detail panel is up.
func (v GraphModel) ShowingDetail() bool { return v.detail }

// DetailOffset returns the first line of the detail panel that is visible.
func (v GraphModel) DetailOffset() int { return v.detailOff }

// Orientation returns the layout the view would draw at its current size:
// the pinned one, or the one [GraphAuto] picks from the pane's shape.
func (v GraphModel) Orientation() GraphOrientation {
	if v.orientation != GraphAuto {
		return v.orientation
	}
	if v.width < GraphNarrowWidth {
		return GraphIndentedList
	}
	// A terminal cell is about twice as tall as it is wide, so a pane is
	// "taller than wide" when twice its height beats its width.
	if v.height*2 > v.width {
		return GraphTopToBottom
	}
	return GraphLeftToRight
}

// Selected returns the bead under the cursor, and false when the run has no
// beads yet.
func (v GraphModel) Selected(st *State) (GraphNode, bool) {
	lay := BuildGraphLayout(st)
	if lay.Len() == 0 {
		return GraphNode{}, false
	}
	return lay.Nodes[v.index(lay)], true
}

// Viewport returns the top-left cell of the drawn graph that is visible: the
// scroll offset the view uses to keep the selected box on screen. It is
// always (0, 0) for the indented list and the detail panel.
func (v GraphModel) Viewport(st *State) (x, y int) {
	o := v.Orientation()
	if o == GraphIndentedList || v.detail {
		return 0, 0
	}
	lay := BuildGraphLayout(st)
	if lay.Len() == 0 {
		return 0, 0
	}
	cursor := v.index(lay)
	_, boxes := v.paint(lay, o == GraphTopToBottom, cursor)
	return v.scroll(boxes[cursor])
}

// Update handles one message and reports whether the view consumed it. It
// takes the run state because the cursor is clamped to the beads that exist,
// and the global [KeyMap] because Up and Down are shared with every other
// view.
func (v GraphModel) Update(msg tea.Msg, st *State, keys KeyMap) (GraphModel, bool) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return v, false
	}
	lay := BuildGraphLayout(st)
	n := lay.Len()
	// Start from where the pinned bead is now, not from where the cursor was
	// when it was last moved: the graph is re-planned under the view.
	cur := v.index(lay)
	v.cursor = cur
	if v.detail {
		switch {
		case key.Matches(km, v.keys.Close), key.Matches(km, v.keys.Open):
			v.detail, v.detailOff = false, 0
		case key.Matches(km, keys.Up):
			v.detailOff = max(v.detailOff-1, 0)
		case key.Matches(km, keys.Down):
			// Stop at the last page: an offset past it would show nothing
			// new, and every press past it would have to be undone before a
			// scroll back up moved the panel again.
			v.detailOff = min(v.detailOff+1, v.detailMaxOffset(lay, st, cur))
		default:
			return v, false
		}
		return v, true
	}
	switch {
	case key.Matches(km, keys.Up):
		v.moveTo(lay, cur-1)
	case key.Matches(km, keys.Down):
		v.moveTo(lay, cur+1)
	case key.Matches(km, v.keys.Left):
		v.moveTo(lay, graphStep(lay, cur, -1))
	case key.Matches(km, v.keys.Right):
		v.moveTo(lay, graphStep(lay, cur, +1))
	case key.Matches(km, v.keys.Top):
		v.moveTo(lay, 0)
	case key.Matches(km, v.keys.Bottom):
		v.moveTo(lay, n-1)
	case key.Matches(km, v.keys.Open):
		if n == 0 {
			return v, false
		}
		// Pin the selection before the panel opens, so a re-plan cannot make
		// an open panel describe a different bead.
		v.moveTo(lay, cur)
		v.detail, v.detailOff = true, 0
	default:
		return v, false
	}
	return v, true
}

// graphStep moves the cursor one level along, keeping the slot within the
// level as close as the new level allows.
func graphStep(lay GraphLayout, cursor, delta int) int {
	if lay.Len() == 0 {
		return 0
	}
	cur := lay.Nodes[clampIndex(cursor, lay.Len())]
	layer := cur.Layer + delta
	if layer < 0 || layer >= len(lay.Layers) {
		return clampIndex(cursor, lay.Len())
	}
	row := lay.Layers[layer]
	if len(row) == 0 {
		return clampIndex(cursor, lay.Len())
	}
	return row[clampIndex(cur.Slot, len(row))]
}

// View renders the pane: the summary line and the graph, or the bead detail
// panel when one is open. The result is always exactly the height the view
// was resized to.
func (v GraphModel) View(st *State) string {
	w, h := v.width, v.height
	lay := BuildGraphLayout(st)
	if lay.Len() == 0 {
		return fit(v.summary(lay, "")+"\n"+v.theme.Muted().Render(emptyText[TabGraph]), w, h)
	}
	cursor := v.index(lay)
	if v.detail {
		return fit(strings.Join(v.detailLines(lay, st, cursor, w, h), "\n"), w, h)
	}
	body := max(h-1, 1)
	var lines []string
	var hint string
	switch v.Orientation() {
	case GraphIndentedList:
		lines = v.listLines(lay, cursor, w, body)
	case GraphTopToBottom:
		lines, hint = v.boxLines(lay, true, cursor, w, body)
	default:
		lines, hint = v.boxLines(lay, false, cursor, w, body)
	}
	return fit(strings.Join(append([]string{v.summary(lay, hint)}, lines...), "\n"), w, h)
}

// summary renders the counts, the layout name and the scroll hint above the
// graph.
func (v GraphModel) summary(lay GraphLayout, hint string) string {
	ready, blocked, closed := lay.Counts()
	fields := []string{
		fmt.Sprintf("%d %s", lay.Len(), graphPlural(lay.Len(), "bead", "beads")),
		fmt.Sprintf("%d %s", len(lay.Layers), graphPlural(len(lay.Layers), "level", "levels")),
		v.theme.Success().Render(fmt.Sprintf("%d ready", ready)),
		v.theme.Warning().Render(fmt.Sprintf("%d blocked", blocked)),
		fmt.Sprintf("%d closed", closed),
	}
	if n := len(lay.Cycles); n > 0 {
		fields = append(fields, v.theme.Danger().Render(fmt.Sprintf("%d %s", n, graphPlural(n, "cycle", "cycles"))))
	}
	if len(lay.Dangling) > 0 {
		fields = append(fields, v.theme.Muted().Render(fmt.Sprintf("%d outside the run", len(lay.Dangling))))
	}
	fields = append(fields, v.theme.Muted().Render(v.Orientation().String()))
	if hint != "" {
		fields = append(fields, v.theme.Accent().Render(hint))
	}
	return strings.Join(fields, v.theme.Rule().Render(" · "))
}

// boxLines draws the boxed layout and clips it to the pane, scrolled so the
// selected box stays visible. The second result marks the directions the
// graph carries on past the edge of the pane.
func (v GraphModel) boxLines(lay GraphLayout, vertical bool, cursor, w, h int) ([]string, string) {
	canvas, boxes := v.paint(lay, vertical, cursor)
	offX, offY := v.scroll(boxes[cursor])
	out := make([]string, 0, h)
	for y := offY; y < offY+h; y++ {
		out = append(out, canvas.row(y, offX, w))
	}
	return out, graphScrollHint(offX, offY, w, h, canvas.w, canvas.h)
}

// graphScrollHint returns the arrows for the parts of the graph the pane
// cannot show, so nobody mistakes a clipped graph for the whole run.
func graphScrollHint(offX, offY, w, h, cw, ch int) string {
	hint := ""
	if offY > 0 {
		hint += "▲"
	}
	if offY+h < ch {
		hint += "▼"
	}
	if offX > 0 {
		hint += "◀"
	}
	if offX+w < cw {
		hint += "▶"
	}
	return hint
}

// scroll returns the top-left cell of the canvas to show so that box is
// fully visible, or as much of it as the pane can hold.
func (v GraphModel) scroll(b graphBox) (x, y int) {
	w, h := v.width, max(v.height-1, 1)
	if b.x+b.w > w {
		x = b.x + b.w - w
	}
	if b.x < x {
		x = b.x
	}
	if b.y+b.h > h {
		y = b.y + b.h - h
	}
	if b.y < y {
		y = b.y
	}
	return max(x, 0), max(y, 0)
}

// graphBox is where one bead's box sits on the canvas.
type graphBox struct {
	x, y, w, h int
}

// midY is the row an edge attaches to on the box's left or right border:
// the line carrying the bead id.
func (b graphBox) midY() int { return b.y + 1 }

// midX is the column an edge attaches to on the box's top or bottom border.
func (b graphBox) midX() int { return b.x + b.w/2 }

// graphEdgeKind is how a dependency has to get from its blocker's box to its
// dependent's box without ever crossing a third box.
type graphEdgeKind int

const (
	// graphEdgeDirect joins two boxes in neighbouring levels: straight
	// through the channel in the gap between them.
	graphEdgeDirect graphEdgeKind = iota
	// graphEdgeSkip joins a blocker to a dependent more than one level
	// along. The levels in between sit right on the straight line, so the
	// route detours through a bypass lane past the end of the layers.
	graphEdgeSkip
	// graphEdgeBack joins two boxes that the layering put on the same level,
	// which only happens inside the dependency-cycle bucket. The route
	// leaves and re-enters on the far side, through a bypass lane of its
	// own.
	graphEdgeBack
)

// graphEdge is one dependency ready to draw: the blocker's node index, the
// dependent's, how the route has to run, and which bypass lane of that kind
// the route owns (-1 when it needs none).
type graphEdge struct {
	src, dst int
	kind     graphEdgeKind
	lane     int
}

// graphEdges lists the dependencies the boxed layouts draw, in a fixed order,
// and hands each one that cannot run straight through a gap a bypass lane of
// its own so two detours are never mistaken for one. Blockers outside the run
// are skipped: there is no box to draw from.
func graphEdges(lay GraphLayout) (edges []graphEdge, skips, backs int) {
	for idx, n := range lay.Nodes {
		for _, dep := range n.Blockers {
			src, ok := lay.Index(dep)
			if !ok {
				continue // dangling: the blocker is outside the run
			}
			e := graphEdge{src: src, dst: idx, lane: -1}
			switch sl, dl := lay.Nodes[src].Layer, n.Layer; {
			case dl == sl+1:
				e.kind = graphEdgeDirect
			case dl > sl+1:
				e.kind, e.lane = graphEdgeSkip, skips
				skips++
			default:
				// Only the cycle bucket can hold an edge that does not move
				// forward a level, and it holds every bead downstream of the
				// cycle too, so both ends are in that last level together.
				e.kind, e.lane = graphEdgeBack, backs
				backs++
			}
			edges = append(edges, e)
		}
	}
	return edges, skips, backs
}

// paint draws every box and edge of the layout onto a fresh canvas and
// returns it with the position of each node's box, indexed like
// [GraphLayout.Nodes].
func (v GraphModel) paint(lay GraphLayout, vertical bool, cursor int) (*graphCanvas, []graphBox) {
	boxes := make([]graphBox, len(lay.Nodes))
	edges, skips, backs := graphEdges(lay)

	span := 0
	for _, row := range lay.Layers {
		span = max(span, len(row))
	}
	var boxW, boxH int
	if vertical {
		boxW = max(span*(GraphBoxWidth+GraphColGap)-GraphColGap, 1)
		boxH = max(len(lay.Layers)*(GraphBoxHeight+GraphLayerGap)-GraphLayerGap, 1)
	} else {
		boxW = max(len(lay.Layers)*(GraphBoxWidth+GraphGutter)-GraphGutter, 1)
		boxH = max(span*(GraphBoxHeight+GraphRowGap)-GraphRowGap, 1)
	}
	// The bypass lanes run past the end of the layers, where no box can ever
	// be, so a detour is drawn whole instead of being chopped by the boxes it
	// would otherwise cross. Skip edges run along the far side of the levels,
	// back edges along the far end of them; the canvas only grows when there
	// is such an edge to draw.
	cw, ch := boxW, boxH
	skipLane, backLane := boxH+GraphRowGap, boxW+1
	if vertical {
		skipLane, backLane = boxW+GraphColGap, boxH+1
		if skips > 0 {
			cw = skipLane + skips
		}
		if backs > 0 {
			ch = backLane + backs
		}
	} else {
		if skips > 0 {
			ch = skipLane + skips
		}
		if backs > 0 {
			cw = backLane + backs
		}
	}
	canvas := newGraphCanvas(cw, ch)
	for depth, row := range lay.Layers {
		for slot, idx := range row {
			b := graphBox{w: GraphBoxWidth, h: GraphBoxHeight}
			if vertical {
				b.x = slot * (GraphBoxWidth + GraphColGap)
				b.y = depth * (GraphBoxHeight + GraphLayerGap)
			} else {
				b.x = depth * (GraphBoxWidth + GraphGutter)
				b.y = slot * (GraphBoxHeight + GraphRowGap)
			}
			boxes[idx] = b
			v.drawBox(canvas, b, lay.Nodes[idx], idx == cursor)
		}
	}
	// Edges after the boxes, so a route never overwrites a border; the
	// selected bead's edges go last and in the accent colour.
	muted := v.theme.Muted()
	accent := v.theme.Accent()
	for _, pass := range []bool{false, true} {
		for _, e := range edges {
			strong := e.dst == cursor || e.src == cursor
			if strong != pass {
				continue
			}
			style, force := &muted, false
			if strong {
				style, force = &accent, true
			}
			lane := skipLane + e.lane
			if e.kind == graphEdgeBack {
				lane = backLane + e.lane
			}
			canvas.polyline(graphRoute(boxes[e.src], boxes[e.dst], vertical, e.kind, lane), style, force)
		}
	}
	return canvas, boxes
}

// graphRoute returns the polyline joining two boxes: out of the blocker,
// along the channel in the gap before the dependent, and into it. A
// [graphEdgeSkip] or [graphEdgeBack] route detours through the bypass lane at
// lane, which runs past the end of the layers, so that no segment of any
// route ever lands on a box.
func graphRoute(src, dst graphBox, vertical bool, kind graphEdgeKind, lane int) []graphPoint {
	if vertical {
		channel := dst.y - GraphLayerGap + 1
		switch kind {
		case graphEdgeSkip:
			// Leave along the row right under the source level, which the
			// direct routes only ever touch one cell of.
			exit := src.y + src.h
			return []graphPoint{
				{src.midX(), exit},
				{lane, exit},
				{lane, channel},
				{dst.midX(), channel},
				{dst.midX(), dst.y - 1},
			}
		case graphEdgeBack:
			return []graphPoint{
				{src.midX(), src.y + src.h},
				{src.midX(), lane},
				{dst.midX(), lane},
				{dst.midX(), dst.y + dst.h},
			}
		}
		return []graphPoint{
			{src.midX(), src.y + src.h},
			{src.midX(), channel},
			{dst.midX(), channel},
			{dst.midX(), dst.y - 1},
		}
	}
	channel := dst.x - GraphGutter/2
	switch kind {
	case graphEdgeSkip:
		// Leave along the column right beside the source box, which the
		// direct routes only ever touch one cell of.
		exit := src.x + src.w + 1
		return []graphPoint{
			{src.x + src.w, src.midY()},
			{exit, src.midY()},
			{exit, lane},
			{channel, lane},
			{channel, dst.midY()},
			{dst.x - 1, dst.midY()},
		}
	case graphEdgeBack:
		return []graphPoint{
			{src.x + src.w, src.midY()},
			{lane, src.midY()},
			{lane, dst.midY()},
			{dst.x + dst.w, dst.midY()},
		}
	}
	return []graphPoint{
		{src.x + src.w, src.midY()},
		{channel, src.midY()},
		{channel, dst.midY()},
		{dst.x - 1, dst.midY()},
	}
}

// drawBox draws one bead as a box: state glyph and id on the first line, the
// truncated title on the second, a round badge and the role glyph of the
// agent on it in the top right. The selected bead gets a heavy border, so it
// is recognisable with NO_COLOR too.
func (v GraphModel) drawBox(c *graphCanvas, b graphBox, n GraphNode, selected bool) {
	border := v.theme.StateStyle(n.State)
	head := border
	title := v.theme.Text()
	if n.State == domain.BeadStateClosed {
		title = v.theme.Muted()
	}
	tl, tr, bl, br, hz, vt := "┌", "┐", "└", "┘", "─", "│"
	if selected {
		tl, tr, bl, br, hz, vt = "┏", "┓", "┗", "┛", "━", "┃"
		border = v.theme.Accent()
	}
	// A bead the layering could not place carries the reason on its top
	// border, so a cyclic graph is not mistaken for a healthy one when the
	// colours are off.
	// Every part of the box is measured in terminal cells, not in runes: a
	// terminal configured for ambiguous width draws a box-drawing character
	// two cells wide, and a border counted in runes would then be twice as
	// wide as the content it frames.
	side := lipgloss.Width(vt)
	inner := max(b.w-2*side, 0)
	top := graphRule(hz, inner)
	if label := lipgloss.Width(graphCycleLabel); n.Cycle && inner > label+lipgloss.Width(hz) {
		top = hz + graphCycleLabel + graphRule(hz, inner-label-lipgloss.Width(hz))
	}
	c.text(b.x, b.y, tl+top+tr, &border)
	c.text(b.x, b.y+b.h-1, bl+graphRule(hz, inner)+br, &border)
	for y := b.y + 1; y < b.y+b.h-1; y++ {
		c.text(b.x, y, vt, &border)
		c.text(b.x+b.w-side, y, vt, &border)
	}

	right := graphPad(n.Badge(), 2) + " " + n.AgentGlyph()
	left := graphTrunc(n.Glyph()+" "+graphText(string(n.ID)), inner-lipgloss.Width(right))
	c.text(b.x+side, b.y+1, graphPad(left, inner-lipgloss.Width(right))+right, &head)
	c.text(b.x+side, b.y+2, graphPad(" "+graphTrunc(graphText(n.Title), inner-1), inner), &title)
}

// listLines renders the narrow fallback: one line per bead, grouped and
// indented by level, scrolled so the cursor stays visible.
func (v GraphModel) listLines(lay GraphLayout, cursor, w, h int) []string {
	type row struct {
		text string
		node int
	}
	rows := make([]row, 0, lay.Len()+len(lay.Layers))
	for depth, wave := range lay.Layers {
		label := fmt.Sprintf("level %d", depth)
		if len(wave) > 0 && lay.Nodes[wave[0]].Cycle {
			label = "cycle"
		}
		rows = append(rows, row{v.theme.Muted().Render(label + fmt.Sprintf(" (%d)", len(wave))), -1})
		for _, idx := range wave {
			rows = append(rows, row{v.listRow(lay.Nodes[idx]), idx})
		}
	}
	// Keep the cursor's line on screen, with its level heading when it fits.
	at := 0
	for i, r := range rows {
		if r.node == cursor {
			at = i
			break
		}
	}
	start := 0
	if at >= h {
		start = at - h + 1
	}
	out := make([]string, 0, h)
	for i := start; i < min(start+h, len(rows)); i++ {
		line := clip(rows[i].text, w-2)
		if rows[i].node == cursor {
			out = append(out, v.theme.Selected().Render("▌ ")+line)
			continue
		}
		out = append(out, "  "+line)
	}
	for len(out) < h {
		out = append(out, "")
	}
	return out
}

// listRow renders one bead of the narrow fallback.
func (v GraphModel) listRow(n GraphNode) string {
	badge := graphPad(n.Badge(), 2)
	head := v.theme.StateStyle(n.State).Render(graphPad(n.Glyph()+" "+graphText(string(n.ID)), 13))
	line := head + " " + v.theme.Muted().Render(badge) + " " + n.AgentGlyph() + " " + graphText(n.Title)
	if len(n.Blockers) > 0 {
		line += v.theme.Muted().Render("  ← " + graphText(beadList(n.Blockers)))
	}
	return line
}

// detailBody is how many lines of the detail panel are on screen at once: the
// pane less the row the closing hint is pinned to.
func (v GraphModel) detailBody() int { return max(v.height-1, 1) }

// detailMaxOffset is the largest scroll offset that still shows text: past it
// the panel would only add blank lines, so [GraphModel.Update] stops there and
// a scroll back up moves on the first press.
func (v GraphModel) detailMaxOffset(lay GraphLayout, st *State, cursor int) int {
	return max(len(v.detailContent(lay, st, cursor, v.width))-v.detailBody(), 0)
}

// detailLines renders the bead detail panel, scrolled to the view's offset and
// with the closing hint pinned to the bottom of the pane.
func (v GraphModel) detailLines(lay GraphLayout, st *State, cursor, w, h int) []string {
	lines := v.detailContent(lay, st, cursor, w)
	body := max(h-1, 1)
	off := min(v.detailOff, max(len(lines)-body, 0))
	end := min(off+body, len(lines))
	out := append([]string{}, lines[off:end]...)
	for len(out) < body {
		out = append(out, "")
	}
	hint := v.theme.Muted().Render("esc close")
	if len(lines) > body {
		hint += v.theme.Muted().Render(fmt.Sprintf("   lines %d-%d of %d", off+1, end, len(lines)))
	}
	return append(out, hint)
}

// detailContent renders the bead detail panel's text before it is scrolled:
// what the bead asks for, and what has happened to it so far.
func (v GraphModel) detailContent(lay GraphLayout, st *State, cursor, w int) []string {
	n := lay.Nodes[cursor]
	inner := max(w-2, 1)
	var lines []string
	add := func(s string) { lines = append(lines, s) }
	wrap := func(text string, indent string) {
		for _, l := range graphWrap(text, inner-lipgloss.Width(indent)) {
			add(indent + l)
		}
	}

	add(v.theme.Title().Render(graphText(string(n.ID))) + "  " + graphText(n.Title))
	facts := []string{
		// StateLabel already renders through StateStyle; styling it again
		// would nest a second SGR pair around text that is already escaped.
		v.theme.StateLabel(n.State),
		fmt.Sprintf("level %d", n.Layer),
		fmt.Sprintf("priority %d", int(n.Priority)),
	}
	if n.Size != "" {
		facts = append(facts, "size "+graphText(n.Size.String()))
	}
	if n.Round > 0 {
		facts = append(facts, fmt.Sprintf("round %d", n.Round))
	}
	switch {
	case n.Cycle:
		facts = append(facts, v.theme.Danger().Render("on a dependency cycle"))
	case n.Ready:
		facts = append(facts, v.theme.Success().Render("ready"))
	case n.Blocked:
		facts = append(facts, v.theme.Warning().Render("blocked"))
	}
	add(strings.Join(facts, v.theme.Rule().Render(" · ")))
	if n.HasAgent {
		a := n.Agent
		add(v.theme.Muted().Render("agent ") + string(a.ID) + " " +
			v.theme.RoleStyle(a.Role).Render(roleOf(a.Agent)) + " " +
			v.theme.AgentStateStyle(a.State).Render(AgentStateGlyph(a.State)+" "+a.State.String()))
	}
	if len(n.Blockers) > 0 {
		wrap("depends on "+v.detailDeps(lay, n.Blockers), "")
	}
	if len(n.Dependents) > 0 {
		wrap("blocks "+v.detailDeps(lay, n.Dependents), "")
	}
	if n.Description != "" {
		add("")
		add(v.theme.Title().Render("Description"))
		wrap(graphBlock(n.Description), "  ")
	}
	if n.Acceptance != "" {
		add("")
		add(v.theme.Title().Render("Acceptance"))
		wrap(graphBlock(n.Acceptance), "  ")
	}
	add("")
	add(v.theme.Title().Render("History"))
	hist := v.detailHistory(st, n)
	if len(hist) == 0 {
		add(v.theme.Muted().Render("  nothing has happened to this bead yet"))
	}
	for _, l := range hist {
		add("  " + l)
	}
	return lines
}

// detailDeps renders a dependency list with each bead's state, marking the
// ones that are not part of the run.
func (v GraphModel) detailDeps(lay GraphLayout, ids []domain.BeadID) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		dep, ok := lay.Node(id)
		if !ok {
			parts = append(parts, graphText(string(id))+" (outside the run)")
			continue
		}
		parts = append(parts, graphText(string(id))+" "+v.theme.StateStyle(dep.State).Render(StateGlyph(dep.State)+" "+string(dep.State)))
	}
	return strings.Join(parts, ", ")
}

// detailHistory renders what the event stream has said about the bead: its
// review rounds with their verdicts and responses, the arbitration and
// escalation of a round that could not agree, and the delivery.
func (v GraphModel) detailHistory(st *State, n GraphNode) []string {
	if st == nil {
		return nil
	}
	var out []string
	for _, r := range st.Rounds {
		if r.Bead != n.ID {
			continue
		}
		head := fmt.Sprintf("round %d %s", r.Number, orDash(string(r.Kind)))
		if len(r.Lenses) > 0 {
			head += " " + v.theme.Muted().Render(lensList(r.Lenses))
		}
		if r.Commit != "" {
			head += " " + v.theme.Muted().Render(shortSHA(r.Commit))
		}
		out = append(out, head)
		for _, vd := range r.Verdicts {
			style := v.theme.Success()
			if vd.Outcome == domain.VerdictChanges {
				style = v.theme.Warning()
			}
			line := fmt.Sprintf("  %s %s", v.theme.LensStyle(vd.Lens).Render(graphPad(orDash(string(vd.Lens)), 12)), style.Render(string(vd.Outcome)))
			if vd.Summary != "" {
				line += " " + graphText(vd.Summary)
			}
			out = append(out, line)
			for _, item := range vd.Items {
				out = append(out, "    "+v.theme.SeverityStyle(item.Severity).Render(SeverityGlyph(item.Severity))+" "+graphText(item.Title))
			}
		}
		for _, resp := range r.Responses {
			out = append(out, fmt.Sprintf("  %s %s", v.theme.Muted().Render(graphPad("response", 12)), graphText(strings.TrimSpace(string(resp.Action)+" "+resp.Rationale))))
		}
		for _, ruling := range r.Rulings {
			out = append(out, fmt.Sprintf("  %s %s", v.theme.Muted().Render(graphPad("arbiter", 12)), graphText(strings.TrimSpace(string(ruling.Ruling)+" "+ruling.Rationale))))
		}
		if r.Escalation != nil {
			out = append(out, fmt.Sprintf("  %s %s", v.theme.Danger().Render(graphPad("escalated", 12)), graphText(strings.TrimSpace(string(r.Escalation.Outcome)+" "+graphEscalationNote(*r.Escalation)))))
		}
	}
	for _, d := range st.Deliveries {
		if d.Bead != n.ID {
			continue
		}
		line := "delivery"
		if d.Number > 0 {
			line += fmt.Sprintf(" PR #%d", d.Number)
		}
		if d.URL != "" {
			line += " " + graphText(d.URL)
		}
		if d.Merged {
			line += " " + v.theme.Success().Render("merged "+shortSHA(d.SHA))
		}
		if d.External {
			line += " " + v.theme.Muted().Render("(external)")
		}
		out = append(out, line)
	}
	return out
}

// graphPoint is one cell of an edge route.
type graphPoint struct {
	x, y int
}

// Direction bits of a drawn edge cell, used to pick its box-drawing rune.
const (
	graphNorth uint8 = 1 << iota
	graphEast
	graphSouth
	graphWest
)

// graphLineRunes maps a set of directions to the box-drawing rune that joins
// them, so crossings and corners come out right whatever order the edges
// were drawn in.
var graphLineRunes = map[uint8]rune{
	graphNorth:                                      '│',
	graphSouth:                                      '│',
	graphNorth | graphSouth:                         '│',
	graphEast:                                       '─',
	graphWest:                                       '─',
	graphEast | graphWest:                           '─',
	graphNorth | graphEast:                          '└',
	graphNorth | graphWest:                          '┘',
	graphEast | graphSouth:                          '┌',
	graphSouth | graphWest:                          '┐',
	graphNorth | graphEast | graphSouth:             '├',
	graphNorth | graphEast | graphWest:              '┴',
	graphNorth | graphSouth | graphWest:             '┤',
	graphEast | graphSouth | graphWest:              '┬',
	graphNorth | graphEast | graphSouth | graphWest: '┼',
}

// graphCanvas is the character grid the boxed layouts are drawn on: a rune
// and a style per cell for the boxes, and a direction mask per cell for the
// edges, so an edge can cross another and still be drawn as a junction.
// Rendering groups neighbouring cells that share a style into one styled
// span, which keeps the escape sequences down and lets the viewport clip by
// column without ever cutting one in half.
type graphCanvas struct {
	w, h   int
	runes  []rune
	styles []*lipgloss.Style
	// cont marks a cell covered by the right half of the double-width rune
	// in the cell before it: a bead title can hold CJK or emoji, which take
	// two terminal cells each, and a canvas that assumed one rune per cell
	// would push the rest of the row — the box's right border included — out
	// of place.
	cont  []bool
	masks []uint8
	edges []*lipgloss.Style
}

// newGraphCanvas returns an empty canvas of w×h cells.
func newGraphCanvas(w, h int) *graphCanvas {
	n := max(w, 1) * max(h, 1)
	return &graphCanvas{
		w:      max(w, 1),
		h:      max(h, 1),
		runes:  make([]rune, n),
		styles: make([]*lipgloss.Style, n),
		cont:   make([]bool, n),
		masks:  make([]uint8, n),
		edges:  make([]*lipgloss.Style, n),
	}
}

// graphRuneCells is how many terminal cells a rune occupies, measured the
// same way Lip Gloss measures the strings the boxes are padded and truncated
// with, so a canvas cell and a padded string agree on every terminal —
// including one configured for ambiguous-width 2, where [PadGlyph] widens the
// glyph column.
func graphRuneCells(r rune) int { return lipgloss.Width(string(r)) }

// inside reports whether the cell is on the canvas.
func (c *graphCanvas) inside(x, y int) bool { return x >= 0 && y >= 0 && x < c.w && y < c.h }

// text writes a string starting at (x, y), advancing by the number of cells
// each rune occupies. A rune that occupies none — a control character, an
// escape byte, a combining mark — is dropped rather than written: it would
// take no column but could still split or repaint the row it landed on. Bead
// text is scrubbed by [graphText] before it gets here; this is the backstop.
func (c *graphCanvas) text(x, y int, s string, style *lipgloss.Style) {
	for _, r := range s {
		cells := graphRuneCells(r)
		if cells <= 0 {
			continue
		}
		c.write(x, y, r, style)
		for k := 1; k < cells; k++ {
			c.writeCont(x+k, y, style)
		}
		x += cells
	}
}

// write puts one rune in a cell.
func (c *graphCanvas) write(x, y int, r rune, style *lipgloss.Style) {
	if !c.inside(x, y) {
		return
	}
	c.blank(x, y)
	i := y*c.w + x
	c.runes[i], c.styles[i] = r, style
}

// writeCont marks the cell as the right half of the double-width rune before
// it. It carries the same style so the two halves render as one span.
func (c *graphCanvas) writeCont(x, y int, style *lipgloss.Style) {
	if !c.inside(x, y) {
		return
	}
	c.blank(x, y)
	i := y*c.w + x
	c.styles[i], c.cont[i] = style, true
}

// blank empties a cell and, when it holds either half of a double-width
// rune, the other half with it, so writing over one half never leaves the
// other stranded.
func (c *graphCanvas) blank(x, y int) {
	i := y*c.w + x
	if c.cont[i] && x > 0 {
		c.runes[i-1], c.styles[i-1] = 0, nil
	}
	if x+1 < c.w && c.cont[i+1] {
		c.styles[i+1], c.cont[i+1] = nil, false
	}
	c.runes[i], c.styles[i], c.cont[i] = 0, nil, false
}

// line adds the given directions to an edge cell. A cell already holding a
// box rune is left alone, so an edge can never overwrite a border: [graphRoute]
// keeps every segment clear of the boxes, and this is the backstop that keeps
// a box readable if it ever fails to.
func (c *graphCanvas) line(x, y int, mask uint8, style *lipgloss.Style, force bool) {
	if !c.inside(x, y) {
		return
	}
	i := y*c.w + x
	if c.runes[i] != 0 || c.cont[i] {
		return
	}
	c.masks[i] |= mask
	if force || c.edges[i] == nil {
		c.edges[i] = style
	}
}

// polyline draws a chain of horizontal and vertical segments, marking every
// cell with the directions of its neighbours on the path so corners and
// crossings pick the right rune.
func (c *graphCanvas) polyline(pts []graphPoint, style *lipgloss.Style, force bool) {
	cells := graphCells(pts)
	for i, p := range cells {
		var mask uint8
		if i > 0 {
			mask |= graphDirection(p, cells[i-1])
		}
		if i < len(cells)-1 {
			mask |= graphDirection(p, cells[i+1])
		}
		c.line(p.x, p.y, mask, style, force)
	}
}

// graphCells expands a polyline into the cells it covers, without repeating
// the corners.
func graphCells(pts []graphPoint) []graphPoint {
	var out []graphPoint
	for i := 0; i+1 < len(pts); i++ {
		a, b := pts[i], pts[i+1]
		dx, dy := sign(b.x-a.x), sign(b.y-a.y)
		if dx != 0 && dy != 0 {
			continue // only orthogonal segments are routed
		}
		for p := a; ; p.x, p.y = p.x+dx, p.y+dy {
			if len(out) == 0 || out[len(out)-1] != p {
				out = append(out, p)
			}
			if p == b || (dx == 0 && dy == 0) {
				break
			}
		}
	}
	return out
}

// graphDirection returns the direction bit pointing from a to its
// neighbour b.
func graphDirection(a, b graphPoint) uint8 {
	switch {
	case b.y < a.y:
		return graphNorth
	case b.y > a.y:
		return graphSouth
	case b.x > a.x:
		return graphEast
	case b.x < a.x:
		return graphWest
	}
	return 0
}

// sign returns -1, 0 or 1.
func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// row renders row y of the canvas from column offX, w columns wide, with
// neighbouring cells of the same style rendered as one span.
func (c *graphCanvas) row(y, offX, w int) string {
	if y < 0 || y >= c.h || w <= 0 {
		return ""
	}
	var b strings.Builder
	var run strings.Builder
	var cur *lipgloss.Style
	flush := func() {
		if run.Len() == 0 {
			return
		}
		if cur == nil {
			b.WriteString(run.String())
		} else {
			b.WriteString(cur.Render(run.String()))
		}
		run.Reset()
	}
	for x := offX; x < offX+w; {
		r, style, cells := ' ', (*lipgloss.Style)(nil), 1
		if c.inside(x, y) {
			i := y*c.w + x
			switch {
			case c.cont[i]:
				// The right half of a double-width rune whose left half is
				// off the left edge of the window: a space stands in for it,
				// so the columns after it still line up.
			case c.runes[i] != 0:
				r, style = c.runes[i], c.styles[i]
				cells = graphRuneCells(r)
			case c.masks[i] != 0:
				if lr, ok := graphLineRunes[c.masks[i]]; ok {
					r, style = lr, c.edges[i]
				}
			}
		}
		if cells > 1 && x+cells > offX+w {
			// A double-width rune the right edge of the window cuts in half:
			// drawing it would make the row one column too wide.
			r, style, cells = ' ', nil, 1
		}
		if style != cur {
			flush()
			cur = style
		}
		run.WriteRune(r)
		x += cells
	}
	flush()
	return strings.TrimRight(b.String(), " ")
}

// graphText makes one piece of run data safe to draw on a single row. A
// bead's title, description and acceptance criteria — and the text of a
// verdict, a response or a ruling — are written by an LLM, so they are
// exactly as untrusted as the transcript lines the Agents view scrubs
// (P7-03): a newline would split the row and shift the rest of the frame, an
// escape sequence would repaint it, and Lip Gloss counts both as zero width,
// so nothing else would notice. It keeps the printable text only — through
// the transcript's own scrubber, so there is one definition of printable —
// and turns the line breaks into spaces so the row keeps its width.
func graphText(s string) string {
	if !strings.ContainsFunc(s, graphControl) {
		return s
	}
	return agentsSanitize(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s))
}

// graphBlock scrubs a field the detail panel wraps itself, keeping the
// paragraph breaks its author wrote: every line is [graphText], the newlines
// between them survive.
func graphBlock(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return graphControl(r) && r != '\n' }) {
		return s
	}
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = graphText(l)
	}
	return strings.Join(lines, "\n")
}

// graphControl reports whether a rune is one a frame cannot carry: a C0
// control (the escape byte, a newline and a tab included), DEL, or one of the
// C1 controls that a terminal reading UTF-8 still acts on.
func graphControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// graphPlural picks the singular or the plural spelling of a word for a
// count, so the summary line reads "1 bead · 1 level" rather than "1 beads".
func graphPlural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// graphRule repeats s until it is w cells wide, padding with spaces when the
// glyph does not divide the width evenly. It is what draws a box border: on
// an ordinary terminal a box-drawing character is one cell and this is a
// plain repeat, but on one configured for ambiguous-width 2 it is two, and a
// border of w runes would overhang the box by its own width again.
func graphRule(s string, w int) string {
	n := lipgloss.Width(s)
	if w <= 0 || n <= 0 {
		return ""
	}
	out := strings.Repeat(s, w/n)
	if rest := w - (w/n)*n; rest > 0 {
		out += strings.Repeat(" ", rest)
	}
	return out
}

// graphPad clips s to w columns and pads it with spaces to exactly w.
func graphPad(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = clip(s, w)
	if n := lipgloss.Width(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

// graphTrunc clips s to w columns, marking a cut with an ellipsis.
func graphTrunc(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return clip(s, w-1) + "…"
}

// graphWrap breaks text into lines of at most w columns, keeping the
// paragraph breaks the bead's author wrote and hard-splitting a word that
// cannot fit on a line of its own.
func graphWrap(text string, w int) []string {
	if w <= 0 {
		return nil
	}
	var out []string
	for _, para := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		fields := strings.Fields(para)
		if len(fields) == 0 {
			out = append(out, "")
			continue
		}
		line := ""
		for _, word := range fields {
			for lipgloss.Width(word) > w {
				if line != "" {
					out = append(out, line)
					line = ""
				}
				out = append(out, clip(word, w))
				word = graphDrop(word, w)
			}
			switch {
			case line == "":
				line = word
			case lipgloss.Width(line)+1+lipgloss.Width(word) <= w:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// graphDrop returns s without its first w columns.
func graphDrop(s string, w int) string {
	rs := []rune(s)
	for i := range rs {
		if lipgloss.Width(string(rs[:i])) >= w {
			return string(rs[i:])
		}
	}
	return ""
}

// graphEscalationNote returns the text the PO wrote with an escalation,
// whichever field the outcome put it in.
func graphEscalationNote(e domain.Escalation) string {
	switch {
	case e.Summary != "":
		return e.Summary
	case e.Notes != "":
		return e.Notes
	case e.NewAcceptance != "":
		return e.NewAcceptance
	}
	return ""
}
