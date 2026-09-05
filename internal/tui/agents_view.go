package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// AgentsTranscriptLines is how many output lines the Agents view keeps per
// agent by default. A transcript longer than this loses its oldest lines.
const AgentsTranscriptLines = 500

// agentsDefaultWidth and agentsDefaultHeight are the pane size the view
// assumes before [AgentsModel.Resize] is called.
const (
	agentsDefaultWidth  = 80
	agentsDefaultHeight = 20
)

// agentsSpinner are the frames of the live-agent spinner, advanced by
// [AgentsModel.Tick].
var agentsSpinner = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// AgentsFrame is how long one spinner frame lasts: ten frames, so the
// spinner turns once a second. Every frame is also what re-renders the
// elapsed time of a live card, so the clock on a card never stands still
// while an agent runs.
const AgentsFrame = 100 * time.Millisecond

// AgentsTickMsg advances the Agents view one spinner frame. Gen identifies
// the ticker that produced it, so a tick left over from a ticker that has
// already stopped is ignored rather than starting a second one.
type AgentsTickMsg struct {
	// Gen is the ticker generation the message was scheduled by.
	Gen uint64
}

// AgentsOptions configure the Agents view.
type AgentsOptions struct {
	// NoColor renders without any ANSI colour, like --no-color and NO_COLOR.
	NoColor bool
	// Theme overrides the palette; nil means [EnvTheme] for NoColor.
	Theme *Theme
	// Now is the clock the elapsed time of a live agent is measured against;
	// nil means [time.Now]. Tests pass a fixed clock so frames are stable.
	Now func() time.Time
	// MaxLines caps each agent's transcript; zero means
	// [AgentsTranscriptLines].
	MaxLines int
	// Keys overrides the view's own bindings; nil means
	// [DefaultAgentsKeyMap].
	Keys *AgentsKeyMap
}

// AgentsKeyMap are the bindings the Agents view adds to the global [KeyMap].
// Cursor movement comes from the global map's Up and Down; everything here
// is specific to the transcript pane.
type AgentsKeyMap struct {
	// Open shows the selected agent's transcript.
	Open key.Binding
	// Close returns from the transcript to the card list.
	Close key.Binding
	// Follow toggles following the stream in the transcript pane.
	Follow key.Binding
	// PageUp scrolls the transcript up by a screenful.
	PageUp key.Binding
	// PageDown scrolls the transcript down by a screenful.
	PageDown key.Binding
	// Top jumps to the first transcript line.
	Top key.Binding
	// Bottom jumps to the newest transcript line and follows again.
	Bottom key.Binding
}

// DefaultAgentsKeyMap returns the standard bindings of the Agents view.
func DefaultAgentsKeyMap() AgentsKeyMap {
	return AgentsKeyMap{
		Open: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "transcript"),
		),
		Close: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "back"),
		),
		Follow: key.NewBinding(
			key.WithKeys("f"),
			key.WithHelp("f", "follow"),
		),
		PageUp: key.NewBinding(
			key.WithKeys("pgup", "ctrl+b"),
			key.WithHelp("pgup", "page up"),
		),
		PageDown: key.NewBinding(
			key.WithKeys("pgdown", "ctrl+f"),
			key.WithHelp("pgdn", "page down"),
		),
		Top: key.NewBinding(
			key.WithKeys("home", "g"),
			key.WithHelp("home", "top"),
		),
		Bottom: key.NewBinding(
			key.WithKeys("end", "G"),
			key.WithHelp("end", "newest"),
		),
	}
}

// ShortHelp implements the [help.KeyMap] interface: the transcript keys the
// footer can show while the Agents view is up.
func (k AgentsKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Open, k.Close, k.Follow}
}

// FullHelp implements the [help.KeyMap] interface.
func (k AgentsKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Open, k.Close},
		{k.Follow, k.Top, k.Bottom},
		{k.PageUp, k.PageDown},
	}
}

// agentsStreamKind says how a transcript line reached the view, which is how
// it is coloured.
type agentsStreamKind int

const (
	// agentsStdout is a line the agent wrote to standard output.
	agentsStdout agentsStreamKind = iota
	// agentsStderr is a line it wrote to standard error.
	agentsStderr
	// agentsMeta is the view's own note: spawned, finished, failed.
	agentsMeta
)

// agentsLine is one line of an agent's transcript.
type agentsLine struct {
	kind agentsStreamKind
	text string
}

// AgentsModel is the Agents view (P7-03): one card per live agent, a history
// list of the ones that ended, and a scrollable transcript with follow mode
// for the selected agent.
//
// It is a Bubble Tea style value: every method that changes it returns the
// new model. The transcripts it collects are shared between copies, so a
// copy made before an [AgentsModel.Apply] still sees the line.
//
// [Model] drives it with six calls: [AgentsModel.Resize] on a
// [tea.WindowSizeMsg], [AgentsModel.Apply] and [AgentsModel.Wake] on every
// [EventMsg], [AgentsModel.Tick] on every [AgentsTickMsg],
// [AgentsModel.Update] on a key press while [TabAgents] is up (before the
// global bindings, so the transcript pane can claim up and down), and
// [AgentsModel.View] for the main pane.
type AgentsModel struct {
	styles   agentsStyles
	keys     AgentsKeyMap
	now      func() time.Time
	maxLines int
	lines    map[domain.AgentID][]agentsLine

	width, height int
	cursor        int
	selected      domain.AgentID
	detail        bool
	follow        bool
	offset        int
	frame         int
	ticking       bool
	gen           uint64
}

// theme returns the palette the view renders with.
func (o AgentsOptions) theme() *Theme {
	if o.Theme != nil {
		return o.Theme
	}
	return EnvTheme(o.NoColor)
}

// NewAgentsModel builds the Agents view. The zero [AgentsOptions] is valid.
func NewAgentsModel(opts AgentsOptions) AgentsModel {
	keys := DefaultAgentsKeyMap()
	if opts.Keys != nil {
		keys = *opts.Keys
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	maxLines := opts.MaxLines
	if maxLines <= 0 {
		maxLines = AgentsTranscriptLines
	}
	return AgentsModel{
		styles:   newAgentsStyles(opts.theme()),
		keys:     keys,
		now:      now,
		maxLines: maxLines,
		lines:    make(map[domain.AgentID][]agentsLine),
		width:    agentsDefaultWidth,
		height:   agentsDefaultHeight,
		follow:   true,
	}
}

// Resize tells the view how much room it has. The pane size decides how far
// a page scrolls and how much of a transcript is visible.
func (v AgentsModel) Resize(w, h int) AgentsModel {
	v.width, v.height = max(w, 1), max(h, 1)
	return v
}

// Wake starts the spinner ticker when st has at least one live agent and no
// ticker is already in flight, and returns the command that schedules the
// first frame (nil when there is nothing to animate). [Model] calls it after
// folding every event, so the ticker the last exit stopped starts again on
// the next spawn.
func (v AgentsModel) Wake(st *State) (AgentsModel, tea.Cmd) {
	if v.ticking || !agentsAnyLive(st) {
		return v, nil
	}
	v.gen++
	v.ticking = true
	return v, v.tickCmd()
}

// Ticking reports whether a spinner ticker is in flight.
func (v AgentsModel) Ticking() bool { return v.ticking }

// Generation is the ticker generation the view accepts an [AgentsTickMsg]
// from. It goes up every time the ticker is restarted, so a tick scheduled
// by a ticker that has since stopped is ignored.
func (v AgentsModel) Generation() uint64 { return v.gen }

// tickCmd schedules the next spinner frame for this ticker generation.
func (v AgentsModel) tickCmd() tea.Cmd {
	gen := v.gen
	return tea.Tick(AgentsFrame, func(time.Time) tea.Msg { return AgentsTickMsg{Gen: gen} })
}

// Tick advances the spinner of the live cards by one frame and returns the
// command that schedules the next one. It stops (returning a nil command)
// once nothing is live, so an idle run costs nothing. ok is false for a tick
// from a ticker that has already been superseded or stopped, which the
// caller should drop without redrawing.
//
// A still frame never ticks, so goldens stay reproducible.
func (v AgentsModel) Tick(msg AgentsTickMsg, st *State) (AgentsModel, tea.Cmd, bool) {
	if !v.ticking || msg.Gen != v.gen {
		return v, nil, false
	}
	if !agentsAnyLive(st) {
		v.ticking = false
		return v, nil, true
	}
	v.frame = (v.frame + 1) % len(agentsSpinner)
	return v, v.tickCmd(), true
}

// agentsAnyLive reports whether any agent's subprocess is still running.
func agentsAnyLive(st *State) bool {
	if st == nil {
		return false
	}
	for _, a := range st.Agents {
		if a.Live() {
			return true
		}
	}
	return false
}

// Apply records what an event says about an agent's transcript: its output
// lines plus a note for the spawn, the exit and the failure. Every other
// event is ignored — the rest of the view reads [State].
func (v AgentsModel) Apply(ev events.Event) AgentsModel {
	switch e := ev.(type) {
	case events.AgentSpawned:
		v.push(e.Agent.ID, agentsMeta, FormatEvent(e))
	case events.AgentOutput:
		kind := agentsStdout
		if e.Stream == events.StreamStderr {
			kind = agentsStderr
		}
		v.push(e.Agent, kind, e.Line)
	case events.AgentFinished:
		v.push(e.Agent, agentsMeta, FormatEvent(e))
	case events.AgentFailed:
		v.push(e.Agent, agentsMeta, FormatEvent(e))
	}
	return v
}

// push appends one transcript line, dropping the oldest past the cap.
func (v AgentsModel) push(id domain.AgentID, kind agentsStreamKind, text string) {
	if id == "" || v.lines == nil {
		return
	}
	got := append(v.lines[id], agentsLine{kind: kind, text: agentsSanitize(text)})
	if len(got) > v.maxLines {
		got = append(got[:0], got[len(got)-v.maxLines:]...)
	}
	v.lines[id] = got
}

// Transcript returns the agent's recorded lines as plain text, oldest first.
func (v AgentsModel) Transcript(id domain.AgentID) []string {
	got := v.lines[id]
	out := make([]string, len(got))
	for i, l := range got {
		out[i] = l.text
	}
	return out
}

// Update handles one message and reports whether the view consumed it. It
// takes the run state because the cursor is clamped to the agents that
// exist, and the global [KeyMap] because Up and Down are shared with every
// other view.
func (v AgentsModel) Update(msg tea.Msg, st *State, keys KeyMap) (AgentsModel, bool) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return v, false
	}
	order := agentsOrder(st)
	v.cursor = v.index(order)
	if v.detail {
		return v.updateTranscript(km, st, keys)
	}
	switch {
	case key.Matches(km, keys.Up):
		v.moveTo(v.cursor-1, order)
	case key.Matches(km, keys.Down):
		v.moveTo(v.cursor+1, order)
	case key.Matches(km, v.keys.Open):
		if len(order) == 0 {
			return v, true
		}
		v.moveTo(v.cursor, order)
		v.detail, v.follow, v.offset = true, true, 0
	default:
		return v, false
	}
	return v, true
}

// updateTranscript handles a key while the transcript pane is open.
func (v AgentsModel) updateTranscript(km tea.KeyMsg, st *State, keys KeyMap) (AgentsModel, bool) {
	n := v.transcriptLen(st)
	h := v.transcriptHeight()
	page := max(h-1, 1)
	switch {
	case key.Matches(km, v.keys.Close):
		v.detail = false
	case key.Matches(km, v.keys.Follow):
		v.follow = !v.follow
		if v.follow {
			v.offset = agentsBottom(n, h)
		}
	case key.Matches(km, v.keys.Top):
		v.offset, v.follow = 0, false
	case key.Matches(km, v.keys.Bottom):
		v.offset, v.follow = agentsBottom(n, h), true
	case key.Matches(km, keys.Up):
		v.scroll(-1, n, h)
	case key.Matches(km, keys.Down):
		v.scroll(1, n, h)
	case key.Matches(km, v.keys.PageUp):
		v.scroll(-page, n, h)
	case key.Matches(km, v.keys.PageDown):
		v.scroll(page, n, h)
	default:
		return v, false
	}
	return v, true
}

// scroll moves the transcript viewport by delta lines. Scrolling up leaves
// follow mode; scrolling back to the newest line re-enters it, so the pane
// resumes streaming without a key of its own.
func (v *AgentsModel) scroll(delta, n, h int) {
	bottom := agentsBottom(n, h)
	from := v.viewport(n, h)
	to := min(max(from+delta, 0), bottom)
	v.offset = to
	v.follow = to >= bottom
}

// viewport is the first visible transcript line: pinned to the bottom while
// following, and the clamped offset otherwise.
func (v AgentsModel) viewport(n, h int) int {
	bottom := agentsBottom(n, h)
	if v.follow {
		return bottom
	}
	return min(max(v.offset, 0), bottom)
}

// agentsBottom is the offset that shows the newest h lines of n.
func agentsBottom(n, h int) int { return max(n-max(h, 1), 0) }

// transcriptLen is how many lines the selected agent has written.
func (v AgentsModel) transcriptLen(st *State) int {
	a, ok := v.Selected(st)
	if !ok {
		return 0
	}
	return len(v.lines[a.ID])
}

// transcriptHeight is how many transcript lines fit under the selected
// agent's card and the pane's rule.
func (v AgentsModel) transcriptHeight() int {
	return max(v.height-agentsCardHeight-1, 1)
}

// agentsCardHeight is how many lines one bordered agent card takes.
const agentsCardHeight = 4

// Selected returns the agent the cursor is on.
func (v AgentsModel) Selected(st *State) (AgentView, bool) {
	order := agentsOrder(st)
	if len(order) == 0 {
		return AgentView{}, false
	}
	return order[v.index(order)], true
}

// index is the position of the selected agent in order: the position of the
// pinned agent id while that agent is still listed, and the clamped cursor
// otherwise. The order changes under the cursor — a live agent that ends
// leaves the live section and joins the head of the history — so the pinned
// id, not the index, is what keeps the selection (and an open transcript) on
// the same agent.
func (v AgentsModel) index(order []AgentView) int {
	if v.selected != "" {
		for i, a := range order {
			if a.ID == v.selected {
				return i
			}
		}
	}
	return clampIndex(v.cursor, len(order))
}

// moveTo puts the cursor on the i-th agent of order (clamped) and pins the
// selection to that agent's id.
func (v *AgentsModel) moveTo(i int, order []AgentView) {
	v.cursor = clampIndex(i, len(order))
	if len(order) > 0 {
		v.selected = order[v.cursor].ID
	}
}

// Cursor returns the index of the selected agent in the view's own order —
// the live agents in spawn order, then the finished ones, newest first — as
// of the last key press. [AgentsModel.Selected] is what follows the pinned
// agent through a reordering; this is the row number the chrome asks for.
func (v AgentsModel) Cursor() int { return v.cursor }

// SelectedID returns the agent id the selection is pinned to, empty until
// the cursor has been moved or a transcript opened.
func (v AgentsModel) SelectedID() domain.AgentID { return v.selected }

// Keys returns the view's own bindings, which [Model] adds to the `?`
// overlay while the Agents view is up.
func (v AgentsModel) Keys() AgentsKeyMap { return v.keys }

// ShowingTranscript reports whether the transcript pane is open.
func (v AgentsModel) ShowingTranscript() bool { return v.detail }

// Following reports whether the transcript pane is pinned to the newest
// line.
func (v AgentsModel) Following() bool { return v.follow }

// Offset returns the first transcript line the pane shows, counting from the
// oldest line the view still keeps. It takes the state because a following
// pane is pinned to the newest line, which moves as output arrives.
func (v AgentsModel) Offset(st *State) int {
	return v.viewport(v.transcriptLen(st), v.transcriptHeight())
}

// Size returns the pane size the view renders into.
func (v AgentsModel) Size() (int, int) { return v.width, v.height }

// View renders the pane: the transcript of the selected agent when it is
// open, otherwise the live cards and the history list. The result is always
// exactly the height it was resized to.
func (v AgentsModel) View(st *State) string {
	w, h := v.width, v.height
	if st == nil || len(st.Agents) == 0 {
		return fit(v.styles.Muted.Render("no agents fired yet"), w, h)
	}
	if v.detail {
		return fit(strings.Join(v.transcriptPane(st, w), "\n"), w, h)
	}
	return fit(strings.Join(v.cardList(st, w, h), "\n"), w, h)
}

// agentsBlock is one selectable (or decorative) run of lines in the card
// list, so the list scrolls without ever cutting a card in half.
type agentsBlock struct {
	lines []string
	index int // the agent's index in the view order, or -1 for a heading
}

// cardList renders the live cards and the history list, windowed so the
// selected entry is visible.
func (v AgentsModel) cardList(st *State, w, h int) []string {
	order := agentsOrder(st)
	cursor := v.index(order)
	used, total := st.Ammo()

	blocks := make([]agentsBlock, 0, len(order)+2)
	live := 0
	for _, a := range order {
		if a.Live() {
			live++
		}
	}
	// The first heading carries the hint for the key that opens a
	// transcript: the pane is where the binding is discovered, since the
	// footer has no room for the per-view keys (the `?` overlay lists them
	// all).
	hint := []key.Binding{v.keys.Open}
	if live > 0 {
		blocks = append(blocks, agentsBlock{lines: []string{v.heading(agentsWithHint(fmt.Sprintf("live · ammo %d/%d", used, total), w, hint...), w)}, index: -1})
		hint = nil
	}
	for i := 0; i < live; i++ {
		blocks = append(blocks, agentsBlock{lines: v.card(order[i], w, i == cursor), index: i})
	}
	if past := len(order) - live; past > 0 {
		blocks = append(blocks, agentsBlock{lines: []string{v.heading(agentsWithHint(fmt.Sprintf("history · %d", past), w, hint...), w)}, index: -1})
		for i := live; i < len(order); i++ {
			blocks = append(blocks, agentsBlock{lines: []string{v.historyRow(order[i], w, i == cursor)}, index: i})
		}
	}
	return agentsWindow(blocks, cursor, h)
}

// agentsWindow flattens blocks into at most h lines, scrolled so the block
// of the selected agent is wholly visible.
func agentsWindow(blocks []agentsBlock, cursor, h int) []string {
	all := make([]string, 0, h*2)
	start, end := 0, 0
	for _, b := range blocks {
		if b.index == cursor {
			start, end = len(all), len(all)+len(b.lines)
		}
		all = append(all, b.lines...)
	}
	if len(all) <= h {
		return all
	}
	top := 0
	if end > h {
		top = end - h
	}
	top = min(top, start)
	return all[top:min(top+h, len(all))]
}

// agentsWithHint appends "<key> <what it does>" for each binding to a
// heading, so the transcript keys are on screen rather than only in the
// help overlay. A pane too narrow to show the hint and still leave room for
// the heading's rule keeps the bare heading.
func agentsWithHint(text string, w int, hints ...key.Binding) string {
	parts := make([]string, 0, len(hints))
	for _, b := range hints {
		if h := b.Help(); h.Key != "" {
			parts = append(parts, h.Key+" "+h.Desc)
		}
	}
	if len(parts) == 0 {
		return text
	}
	full := text + " · " + strings.Join(parts, " · ")
	if lipgloss.Width(full)+4 > w {
		return text
	}
	return full
}

// heading renders a section title with a rule filling the rest of the line.
func (v AgentsModel) heading(text string, w int) string {
	label := v.styles.Heading.Render(strings.ToUpper(text))
	rest := w - lipgloss.Width(label) - 1
	if rest < 1 {
		return clip(label, w)
	}
	return clip(label+" "+v.styles.Rule.Render(strings.Repeat("─", rest)), w)
}

// card renders one live agent as a bordered two-line card: who it is, what
// it is on and how long it has run, then its last line of output.
func (v AgentsModel) card(a AgentView, w int, selected bool) []string {
	content := max(w-4, 8)
	head := agentsRow(v.cardLeft(a, content), v.cardRight(a), content)
	tail := v.styles.Muted.Render(clip(agentsTail(a), content))
	style := v.styles.Card
	if selected {
		style = v.styles.CardSelected
	}
	// Width is the block width, padding included; the border adds the last
	// two columns, so the card is exactly w wide.
	return strings.Split(style.Width(content+2).Render(head+"\n"+tail), "\n")
}

// cardLeft is the identity half of a card's first line. A narrow pane keeps
// the fields that identify the agent and drops the rest, rather than
// truncating every field in the middle of a word.
func (v AgentsModel) cardLeft(a AgentView, w int) string {
	fields := []string{
		v.styles.For(a).Render(agentsGlyph(a)) + " " + v.styles.Who.Render(roleOf(a.Agent)),
		v.styles.Value.Render(orDash(string(a.Bead))),
	}
	if w >= agentsCartridgeWidth {
		fields = append(fields, v.styles.Muted.Render(cartridgeOf(a.Agent)))
	}
	if w >= agentsTaskWidth {
		fields = append(fields, v.styles.Muted.Render(agentsWork(a)))
	}
	return strings.Join(fields, v.styles.Muted.Render(" · "))
}

// The content widths at which a card or a history row can afford its
// optional fields.
const (
	// agentsCartridgeWidth is where the cartridge/model field fits.
	agentsCartridgeWidth = 46
	// agentsTaskWidth is where the task and round field fits too.
	agentsTaskWidth = 58
)

// cardRight is the elapsed time and, while the agent still runs, the
// spinner of a card's first line. An agent that has ended gets a blank in
// the spinner's column instead: it keeps the elapsed clock in the same
// column as the live cards above it, and it stops a finished, failed or
// killed agent from contradicting its own lifecycle glyph by appearing to
// still be turning (the transcript pane draws the card of whichever agent
// is selected, ended ones included).
func (v AgentsModel) cardRight(a AgentView) string {
	spin := PadGlyph(" ")
	if a.Live() {
		spin = v.styles.Spinner.Render(PadGlyph(agentsSpinner[v.frame]))
	}
	return v.styles.Value.Render(v.elapsed(a)) + " " + spin
}

// historyRow renders one finished, failed or killed agent as a single line.
func (v AgentsModel) historyRow(a AgentView, w int, selected bool) string {
	gutter := "  "
	if selected {
		gutter = v.styles.Cursor.Render("▌ ")
	}
	content := max(w-2, 8)
	outcome := agentsOutcome(a)
	if content < agentsTaskWidth {
		outcome = agentsShortOutcome(a)
	}
	left := strings.Join([]string{
		v.styles.For(a).Render(agentsGlyph(a)) + " " + v.styles.Who.Render(roleOf(a.Agent)),
		v.styles.Value.Render(orDash(string(a.Bead))),
		v.styles.Muted.Render(outcome),
	}, v.styles.Muted.Render(" · "))
	right := v.styles.Muted.Render(v.elapsed(a))
	return clip(gutter+agentsRow(left, right, content), w)
}

// transcriptPane renders the selected agent's card, a rule carrying the
// follow state, and the visible slice of its transcript.
func (v AgentsModel) transcriptPane(st *State, w int) []string {
	a, ok := v.Selected(st)
	if !ok {
		return []string{v.styles.Muted.Render("no agents fired yet")}
	}
	out := v.card(a, w, true)
	lines := v.lines[a.ID]
	body := v.transcriptHeight()
	top := v.viewport(len(lines), body)
	out = append(out, v.heading(agentsWithHint(agentsScrollLabel(len(lines), top, body, v.follow), w, v.keys.Follow, v.keys.Close), w))
	if len(lines) == 0 {
		return append(out, v.styles.Muted.Render("no output yet"))
	}
	for _, l := range lines[top:min(top+body, len(lines))] {
		out = append(out, clip(v.styles.Line(l.kind).Render(agentsPrefix(l.kind)+l.text), w))
	}
	return out
}

// agentsScrollLabel describes what part of the transcript is on screen.
func agentsScrollLabel(n, top, h int, follow bool) string {
	mode := "scrolled"
	if follow {
		mode = "following"
	}
	if n == 0 {
		return fmt.Sprintf("transcript · 0 lines · %s", mode)
	}
	return fmt.Sprintf("transcript · %d-%d/%d · %s", top+1, min(top+h, n), n, mode)
}

// agentsOrder is the order the view lists agents in: the live ones in spawn
// order, then the ones that ended, newest first.
func agentsOrder(st *State) []AgentView {
	if st == nil {
		return nil
	}
	out := make([]AgentView, 0, len(st.Agents))
	for _, a := range st.Agents {
		if a.Live() {
			out = append(out, a)
		}
	}
	for i := len(st.Agents) - 1; i >= 0; i-- {
		if !st.Agents[i].Live() {
			out = append(out, st.Agents[i])
		}
	}
	return out
}

// elapsed is how long the agent has been running (live) or ran (ended).
func (v AgentsModel) elapsed(a AgentView) string {
	if !a.Live() {
		return dur(a.Duration.Round(time.Second))
	}
	if a.StartedAt.IsZero() {
		return "-"
	}
	d := v.now().Sub(a.StartedAt)
	if d < 0 {
		d = 0
	}
	return dur(d.Round(time.Second))
}

// agentsWork names what the agent was fired to do, with its round.
func agentsWork(a AgentView) string {
	task := orDash(a.Task.String())
	if a.Round > 0 {
		task += fmt.Sprintf(" r%d", a.Round)
	}
	if a.Attempt > 1 {
		task += fmt.Sprintf(" try %d", a.Attempt)
	}
	return task
}

// agentsTail is the second line of a card: the agent's last output line, or
// what it produced once it ended. The last line comes straight off the
// cartridge's stdout or stderr — [State] keeps it verbatim — so it is
// sanitised here for the same reason the transcript is sanitised on ingest:
// raw subprocess bytes would otherwise repaint or corrupt the frame.
func agentsTail(a AgentView) string {
	if a.LastLine != "" {
		return agentsSanitize(a.LastLine)
	}
	if !a.Live() {
		return agentsOutcome(a)
	}
	return "…"
}

// agentsOutcome summarises how an agent ended.
func agentsOutcome(a AgentView) string {
	switch a.State {
	case domain.AgentStateFinished:
		return fmt.Sprintf("%s (exit %d)", orDash(string(a.Produced)), a.ExitCode)
	case domain.AgentStateFailed, domain.AgentStateKilled:
		// The error text can quote what the process printed before it died,
		// so it is sanitised like any other subprocess bytes.
		return strings.TrimSpace(fmt.Sprintf("%s %s", orDash(string(a.Failure)), parens(agentsSanitize(a.Err))))
	default:
		return string(a.State)
	}
}

// agentsShortOutcome is [agentsOutcome] without its detail, for a narrow
// history row.
func agentsShortOutcome(a AgentView) string {
	switch a.State {
	case domain.AgentStateFinished:
		return orDash(string(a.Produced))
	case domain.AgentStateFailed, domain.AgentStateKilled:
		return orDash(string(a.Failure))
	default:
		return string(a.State)
	}
}

// agentsGlyph is the role glyph of a live agent and the lifecycle glyph of
// one that ended. Both come from the theme (P7-07), so an agent reads the
// same in this view as in the Graph tab; an agent with no role yet gets a
// neutral dot rather than the theme's question mark, because its role is not
// unknown, only unreported.
func agentsGlyph(a AgentView) string {
	switch a.State {
	case domain.AgentStateFinished, domain.AgentStateFailed, domain.AgentStateKilled:
		return AgentStateGlyph(a.State)
	}
	if g := RoleGlyph(a.Role); g != PadGlyph(UnknownGlyph) {
		return g
	}
	// The neutral dot shares the column with the theme's glyphs, so it is
	// padded the same way (P7-16): • is East-Asian-ambiguous and ✓, ✗ and !
	// are not, so an unpadded one would shift the card's first line by a
	// cell on a terminal configured for ambiguous-width 2.
	return PadGlyph("•")
}

// agentsPrefix marks stderr and the view's own notes in a transcript. The
// marker is one glyph column wide plus a space, and the blank one is padded
// to the same width as the two glyphs (P7-16), so a stderr line, a note and
// an ordinary line still start in the same column on a terminal configured
// for ambiguous-width 2 — where · is two cells and ! is one.
func agentsPrefix(k agentsStreamKind) string {
	switch k {
	case agentsStderr:
		return PadGlyph("!") + " "
	case agentsMeta:
		return PadGlyph("·") + " "
	default:
		return PadGlyph(" ") + " "
	}
}

// agentsRow puts left and right on one line of exactly w columns, truncating
// left when the two do not fit.
func agentsRow(left, right string, w int) string {
	rw := lipgloss.Width(right)
	if rw >= w {
		return clip(right, w)
	}
	left = clip(left, w-rw-1)
	gap := w - rw - lipgloss.Width(left)
	return left + strings.Repeat(" ", max(gap, 1)) + right
}

// clampIndex keeps i inside a list of n items.
func clampIndex(i, n int) int {
	if n <= 0 {
		return 0
	}
	return min(max(i, 0), n-1)
}

// agentsStyles is the palette of the Agents view: the semantic entries of
// the shared [Theme], named for what this view draws with them.
type agentsStyles struct {
	// Heading is a section title (live, history, transcript).
	Heading lipgloss.Style
	// Rule is the line that fills a heading out to the pane width.
	Rule lipgloss.Style
	// Who is an agent's role and lens.
	Who lipgloss.Style
	// Value is a highlighted field: bead id, elapsed time.
	Value lipgloss.Style
	// Muted is de-emphasised text: cartridge, task, last line.
	Muted lipgloss.Style
	// Glyph is the fallback style of an agent's glyph, used only by a zero
	// value that has no theme; every constructed palette colours a glyph by
	// role or by state through the theme.
	Glyph lipgloss.Style
	// Spinner is the live-agent spinner.
	Spinner lipgloss.Style
	// Cursor marks the selected history row.
	Cursor lipgloss.Style
	// Card frames one live agent.
	Card lipgloss.Style
	// CardSelected frames the agent the cursor is on.
	CardSelected lipgloss.Style
	// Out is a stdout transcript line.
	Out lipgloss.Style
	// Err is a stderr transcript line.
	Err lipgloss.Style
	// Meta is a transcript note the view wrote itself.
	Meta lipgloss.Style
	// theme is where the per-agent colours come from, so an agent state and
	// a role read the same here as in every other view.
	theme *Theme
}

// State returns the theme's style for an agent lifecycle state.
func (s agentsStyles) State(st domain.AgentState) lipgloss.Style {
	if s.theme == nil {
		return s.Glyph
	}
	return s.theme.AgentStateStyle(st)
}

// Role returns the theme's style for an agent role.
func (s agentsStyles) Role(r domain.Role) lipgloss.Style {
	if s.theme == nil {
		return s.Glyph
	}
	return s.theme.RoleStyle(r)
}

// For returns the style of an agent's glyph: its role while it is live, and
// how it ended once it has.
func (s agentsStyles) For(a AgentView) lipgloss.Style {
	if a.Live() {
		return s.Role(a.Role)
	}
	return s.State(a.State)
}

// Line returns the style of one transcript line.
func (s agentsStyles) Line(k agentsStreamKind) lipgloss.Style {
	switch k {
	case agentsStderr:
		return s.Err
	case agentsMeta:
		return s.Meta
	default:
		return s.Out
	}
}

// newAgentsStyles returns the palette, bound to t's renderer. Nothing here
// may call [lipgloss.NewStyle]: that style would use the global renderer and
// the view would then render for the ambient terminal rather than for t
// (P7-09).
func newAgentsStyles(t *Theme) agentsStyles {
	return agentsStyles{
		Heading:      t.Title(),
		Rule:         t.Rule(),
		Who:          t.Title(),
		Value:        t.Accent(),
		Muted:        t.Muted(),
		Glyph:        t.Accent(),
		Spinner:      t.Accent(),
		Cursor:       t.Accent(),
		Card:         t.Box(),
		CardSelected: t.Box().BorderForeground(ColorAccent.Resolve(t.Profile())),
		Out:          t.Text(),
		Err:          t.Warning(),
		Meta:         t.Muted(),
		theme:        t,
	}
}
