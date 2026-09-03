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

// graphBoxInner is the writable width inside a box.
const graphBoxInner = GraphBoxWidth - 2

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

// Cursor returns the index of the selected bead in [GraphLayout.Nodes].
func (v GraphModel) Cursor() int { return v.cursor }

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
	return lay.Nodes[clampIndex(v.cursor, lay.Len())], true
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
	_, boxes := v.paint(lay, o == GraphTopToBottom, clampIndex(v.cursor, lay.Len()))
	return v.scroll(boxes[clampIndex(v.cursor, lay.Len())])
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
	v.cursor = clampIndex(v.cursor, n)
	if v.detail {
		switch {
		case key.Matches(km, v.keys.Close), key.Matches(km, v.keys.Open):
			v.detail, v.detailOff = false, 0
		case key.Matches(km, keys.Up):
			v.detailOff = max(v.detailOff-1, 0)
		case key.Matches(km, keys.Down):
			v.detailOff++
		default:
			return v, false
		}
		return v, true
	}
	switch {
	case key.Matches(km, keys.Up):
		v.cursor = clampIndex(v.cursor-1, n)
	case key.Matches(km, keys.Down):
		v.cursor = clampIndex(v.cursor+1, n)
	case key.Matches(km, v.keys.Left):
		v.cursor = graphStep(lay, v.cursor, -1)
	case key.Matches(km, v.keys.Right):
		v.cursor = graphStep(lay, v.cursor, +1)
	case key.Matches(km, v.keys.Top):
		v.cursor = 0
	case key.Matches(km, v.keys.Bottom):
		v.cursor = clampIndex(n-1, n)
	case key.Matches(km, v.keys.Open):
		if n == 0 {
			return v, false
		}
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
	cursor := clampIndex(v.cursor, lay.Len())
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
		fmt.Sprintf("%d beads", lay.Len()),
		fmt.Sprintf("%d levels", len(lay.Layers)),
		v.theme.Success().Render(fmt.Sprintf("%d ready", ready)),
		v.theme.Warning().Render(fmt.Sprintf("%d blocked", blocked)),
		fmt.Sprintf("%d closed", closed),
	}
	if len(lay.Cycles) > 0 {
		fields = append(fields, v.theme.Danger().Render(fmt.Sprintf("%d cycles", len(lay.Cycles))))
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

// paint draws every box and edge of the layout onto a fresh canvas and
// returns it with the position of each node's box, indexed like
// [GraphLayout.Nodes].
func (v GraphModel) paint(lay GraphLayout, vertical bool, cursor int) (*graphCanvas, []graphBox) {
	boxes := make([]graphBox, len(lay.Nodes))
	var cw, ch int
	if vertical {
		widest := 0
		for _, row := range lay.Layers {
			widest = max(widest, len(row))
		}
		cw = max(widest*(GraphBoxWidth+GraphColGap)-GraphColGap, 1)
		ch = max(len(lay.Layers)*(GraphBoxHeight+GraphLayerGap)-GraphLayerGap, 1)
	} else {
		tallest := 0
		for _, row := range lay.Layers {
			tallest = max(tallest, len(row))
		}
		cw = max(len(lay.Layers)*(GraphBoxWidth+GraphGutter)-GraphGutter, 1)
		ch = max(tallest*(GraphBoxHeight+GraphRowGap)-GraphRowGap, 1)
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
		for idx, n := range lay.Nodes {
			for _, dep := range n.Blockers {
				src, ok := lay.Index(dep)
				if !ok {
					continue // dangling: the blocker is outside the run
				}
				if lay.Nodes[src].Layer >= n.Layer {
					continue // a cycle edge has no honest route
				}
				strong := idx == cursor || src == cursor
				if strong != pass {
					continue
				}
				style, force := &muted, false
				if strong {
					style, force = &accent, true
				}
				canvas.polyline(graphRoute(boxes[src], boxes[idx], vertical), style, force)
			}
		}
	}
	return canvas, boxes
}

// graphRoute returns the polyline joining two boxes: out of the blocker,
// along the channel in the gap before the dependent, and into it.
func graphRoute(src, dst graphBox, vertical bool) []graphPoint {
	if vertical {
		channel := dst.y - GraphLayerGap + 1
		return []graphPoint{
			{src.midX(), src.y + src.h},
			{src.midX(), channel},
			{dst.midX(), channel},
			{dst.midX(), dst.y - 1},
		}
	}
	channel := dst.x - GraphGutter/2
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
	c.text(b.x, b.y, tl+strings.Repeat(hz, b.w-2)+tr, &border)
	c.text(b.x, b.y+b.h-1, bl+strings.Repeat(hz, b.w-2)+br, &border)
	for y := b.y + 1; y < b.y+b.h-1; y++ {
		c.text(b.x, y, vt, &border)
		c.text(b.x+b.w-1, y, vt, &border)
	}

	right := graphPad(n.Badge(), 2) + " " + n.AgentGlyph()
	left := graphTrunc(n.Glyph()+" "+string(n.ID), graphBoxInner-lipgloss.Width(right))
	c.text(b.x+1, b.y+1, graphPad(left, graphBoxInner-lipgloss.Width(right))+right, &head)
	c.text(b.x+1, b.y+2, graphPad(" "+graphTrunc(n.Title, graphBoxInner-1), graphBoxInner), &title)
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
	head := v.theme.StateStyle(n.State).Render(graphPad(n.Glyph()+" "+string(n.ID), 13))
	line := head + " " + v.theme.Muted().Render(badge) + " " + n.AgentGlyph() + " " + n.Title
	if len(n.Blockers) > 0 {
		line += v.theme.Muted().Render("  ← " + beadList(n.Blockers))
	}
	return line
}

// detailLines renders the bead detail panel: what the bead asks for, and
// what has happened to it so far.
func (v GraphModel) detailLines(lay GraphLayout, st *State, cursor, w, h int) []string {
	n := lay.Nodes[cursor]
	inner := max(w-2, 1)
	var lines []string
	add := func(s string) { lines = append(lines, s) }
	wrap := func(text string, indent string) {
		for _, l := range graphWrap(text, inner-lipgloss.Width(indent)) {
			add(indent + l)
		}
	}

	add(v.theme.Title().Render(string(n.ID)) + "  " + n.Title)
	facts := []string{
		v.theme.StateStyle(n.State).Render(v.theme.StateLabel(n.State)),
		fmt.Sprintf("level %d", n.Layer),
		fmt.Sprintf("priority %d", int(n.Priority)),
	}
	if n.Size != "" {
		facts = append(facts, "size "+n.Size.String())
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
		wrap(n.Description, "  ")
	}
	if n.Acceptance != "" {
		add("")
		add(v.theme.Title().Render("Acceptance"))
		wrap(n.Acceptance, "  ")
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

	// Scroll, then pin the closing hint to the bottom of the pane.
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

// detailDeps renders a dependency list with each bead's state, marking the
// ones that are not part of the run.
func (v GraphModel) detailDeps(lay GraphLayout, ids []domain.BeadID) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		dep, ok := lay.Node(id)
		if !ok {
			parts = append(parts, string(id)+" (outside the run)")
			continue
		}
		parts = append(parts, string(id)+" "+v.theme.StateStyle(dep.State).Render(StateGlyph(dep.State)+" "+string(dep.State)))
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
				line += " " + vd.Summary
			}
			out = append(out, line)
			for _, item := range vd.Items {
				out = append(out, "    "+v.theme.SeverityStyle(item.Severity).Render(SeverityGlyph(item.Severity))+" "+item.Title)
			}
		}
		for _, resp := range r.Responses {
			out = append(out, fmt.Sprintf("  %s %s", v.theme.Muted().Render(graphPad("response", 12)), strings.TrimSpace(string(resp.Action)+" "+resp.Rationale)))
		}
		for _, ruling := range r.Rulings {
			out = append(out, fmt.Sprintf("  %s %s", v.theme.Muted().Render(graphPad("arbiter", 12)), strings.TrimSpace(string(ruling.Ruling)+" "+ruling.Rationale)))
		}
		if r.Escalation != nil {
			out = append(out, fmt.Sprintf("  %s %s", v.theme.Danger().Render(graphPad("escalated", 12)), strings.TrimSpace(string(r.Escalation.Outcome)+" "+graphEscalationNote(*r.Escalation))))
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
			line += " " + d.URL
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
	masks  []uint8
	edges  []*lipgloss.Style
}

// newGraphCanvas returns an empty canvas of w×h cells.
func newGraphCanvas(w, h int) *graphCanvas {
	n := max(w, 1) * max(h, 1)
	return &graphCanvas{
		w:      max(w, 1),
		h:      max(h, 1),
		runes:  make([]rune, n),
		styles: make([]*lipgloss.Style, n),
		masks:  make([]uint8, n),
		edges:  make([]*lipgloss.Style, n),
	}
}

// inside reports whether the cell is on the canvas.
func (c *graphCanvas) inside(x, y int) bool { return x >= 0 && y >= 0 && x < c.w && y < c.h }

// text writes a string starting at (x, y), one rune per cell.
func (c *graphCanvas) text(x, y int, s string, style *lipgloss.Style) {
	for _, r := range s {
		if !c.inside(x, y) {
			x++
			continue
		}
		i := y*c.w + x
		c.runes[i], c.styles[i] = r, style
		x++
	}
}

// line adds the given directions to an edge cell. A cell already holding a
// box rune is left alone: an edge crossing a box is drawn around it rather
// than through it.
func (c *graphCanvas) line(x, y int, mask uint8, style *lipgloss.Style, force bool) {
	if !c.inside(x, y) {
		return
	}
	i := y*c.w + x
	if c.runes[i] != 0 {
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
	for x := offX; x < offX+w; x++ {
		r, style := ' ', (*lipgloss.Style)(nil)
		if c.inside(x, y) {
			i := y*c.w + x
			switch {
			case c.runes[i] != 0:
				r, style = c.runes[i], c.styles[i]
			case c.masks[i] != 0:
				if lr, ok := graphLineRunes[c.masks[i]]; ok {
					r, style = lr, c.edges[i]
				}
			}
		}
		if style != cur {
			flush()
			cur = style
		}
		run.WriteRune(r)
	}
	flush()
	return strings.TrimRight(b.String(), " ")
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
