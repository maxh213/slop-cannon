package tui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"slop-cannon/internal/domain"
)

// TesterStatus is where one tester lens stands on a bead: the outcome of its
// most recent verdict and whether the current round is still waiting for it.
// It is what the Feedback view's "satisfied" tick is drawn from.
type TesterStatus struct {
	// Lens is the perspective the tester reviews from.
	Lens domain.Lens
	// Outcome is the latest verdict's outcome; empty when the tester has
	// not returned a verdict yet.
	Outcome domain.VerdictOutcome
	// Round is the round the latest verdict belongs to, 0 when there is none.
	Round int
	// Pending is true when the newest round assigned this lens and its
	// verdict has not arrived yet.
	Pending bool
}

// Satisfied reports whether the tester approved and is not being asked again
// (D-09: an approval stands until the diff moves enough to force a
// re-review).
func (t TesterStatus) Satisfied() bool {
	return t.Outcome == domain.VerdictApprove && !t.Pending
}

// Glyph returns the one-cell marker of the tester's position: ✓ satisfied,
// ✗ changes requested, ◐ reviewing, · not asked yet. It is the channel that
// survives NO_COLOR and colour blindness.
func (t TesterStatus) Glyph() string {
	switch {
	case t.Pending:
		return "◐"
	case t.Outcome == domain.VerdictApprove:
		return "✓"
	case t.Outcome == domain.VerdictChanges:
		return "✗"
	default:
		return "·"
	}
}

// String returns the word behind the glyph: "approve", "changes",
// "reviewing" or "waiting".
func (t TesterStatus) String() string {
	switch {
	case t.Pending:
		return "reviewing"
	case t.Outcome != "":
		return t.Outcome.String()
	default:
		return "waiting"
	}
}

// style returns the theme style the tester's tick is rendered in.
func (t TesterStatus) style(th *Theme) lipgloss.Style {
	switch {
	case t.Pending:
		return th.Warning()
	case t.Outcome == domain.VerdictApprove:
		return th.Success()
	case t.Outcome == domain.VerdictChanges:
		return th.Danger()
	default:
		return th.Muted()
	}
}

// FeedbackItemView is one statement of a feedback item — the same id raised
// again in a later round is a separate occurrence — together with the
// answers that occurrence drew: the engineer's response and the product
// owner's ruling, which arrive in their own events in a later round and are
// matched back onto the occurrence they reply to.
type FeedbackItemView struct {
	domain.FeedbackItem
	// Lens is the tester that raised the item.
	Lens domain.Lens
	// Round is the review round the item was raised in.
	Round int
	// Response is the engineer's answer to this occurrence, when one arrived.
	Response *domain.Response
	// ResponseRound is the round Response was given in — the round after the
	// one that raised the item, or the same round when the loop collected the
	// answer without re-opening one; 0 when there is no response.
	ResponseRound int
	// Ruling is the product owner's arbitration on this occurrence, when the
	// item was disputed all the way to one (D-08).
	Ruling *domain.Arbitration
	// RulingRound is the round Ruling was given in, 0 when there is none.
	RulingRound int
}

// Blocking reports whether the item still stands between the bead and an
// approval: it has not been settled and it is not a nit.
func (i FeedbackItemView) Blocking() bool {
	return !i.Status.Settled() && i.Severity != domain.SeverityNit
}

// FeedbackThread is the whole review history of one bead: its rounds in
// order, every item raised with its response and ruling, and where each
// tester stands.
type FeedbackThread struct {
	// Bead is the bead reviewed.
	Bead domain.BeadID
	// Title is the bead's title, when the graph carried it.
	Title string
	// BeadState is the bead's current workflow state, empty when unknown.
	BeadState domain.BeadState
	// Rounds are the bead's review rounds in the order they started.
	Rounds []Round
	// Testers is where each lens stands, canonical lenses first (ux, qa,
	// test) and any other lens after them in first-seen order.
	Testers []TesterStatus
	// Items is every item raised across the rounds, oldest first.
	Items []FeedbackItemView
	// Open counts the items nobody has settled yet.
	Open int
	// Escalation is the product owner's ruling after the round cap, when the
	// bead got that far (D-08).
	Escalation *domain.Escalation
	// Approved is true when every tester that reviewed is satisfied.
	Approved bool

	// at indexes Items by the round and lens that stated them, so a view can
	// find the exact occurrence a verdict is showing.
	at map[string]int
}

// feedbackItemKey identifies one statement of an item: the same id restated
// in a later round is a separate occurrence with its own status.
func feedbackItemKey(round int, lens domain.Lens, id string) string {
	return fmt.Sprintf("%d/%s/%s", round, lens, id)
}

// feedbackLensKey identifies one lens's take on an item id, across rounds:
// the same id raised by two lenses is two items, each settled on its own.
func feedbackLensKey(lens domain.Lens, id string) string {
	return fmt.Sprintf("%s/%s", lens, id)
}

// occurrence returns the item as the given round's verdict from that lens
// stated it.
func (t FeedbackThread) occurrence(round int, lens domain.Lens, id string) (FeedbackItemView, bool) {
	i, ok := t.at[feedbackItemKey(round, lens, id)]
	if !ok || i < 0 || i >= len(t.Items) {
		return FeedbackItemView{}, false
	}
	return t.Items[i], true
}

// feedbackStatedBy returns the indexes of the occurrences an answer given in
// round replies to: for every lens that raised the item with the given id,
// the newest statement that lens made before the round — or up to and
// including it, when inclusive.
//
// The result is a list because two lenses can raise the same item id in one
// round: P6-04 consolidates a round's items across the three lenses, so one
// engineer response or one ruling settles every occurrence of the item, not
// just the last lens to mention it. A response also falls back to a
// statement made in its own round when that lens made no earlier one, since
// P6-04 collects the engineer's answers inside the round that raised them.
// The indexes come back in item order, oldest occurrence first, and the
// result is empty when the item was never stated that early.
func feedbackStatedBy(items []FeedbackItemView, round int, id string, inclusive bool) []int {
	// tier 0 is a statement from a round before the answer, tier 1 one from
	// the answering round itself, which a response only falls back to.
	tier := func(it FeedbackItemView) int {
		if !inclusive && it.Round == round {
			return 1
		}
		return 0
	}
	best := make(map[domain.Lens]int, len(feedbackLensColumns))
	for i, it := range items {
		if it.ID != id || it.Round > round {
			continue
		}
		if j, seen := best[it.Lens]; seen {
			cur := items[j]
			if tier(it) > tier(cur) || (tier(it) == tier(cur) && it.Round < cur.Round) {
				continue
			}
		}
		best[it.Lens] = i
	}
	out := make([]int, 0, len(best))
	for _, i := range best {
		out = append(out, i)
	}
	slices.Sort(out)
	return out
}

// feedbackPick reads the occurrences at the given indexes, skipping any
// index that is not an item.
func feedbackPick(items []FeedbackItemView, idx []int) []FeedbackItemView {
	out := make([]FeedbackItemView, 0, len(idx))
	for _, i := range idx {
		if i >= 0 && i < len(items) {
			out = append(out, items[i])
		}
	}
	return out
}

// Answered returns the occurrences an engineer's response given in the given
// round replies to: per lens that raised the item, the newest statement from
// an earlier round — because the engineer answers the round it has just been
// handed — or the statement that lens made in this very round when it made
// no earlier one. It is empty when no verdict ever raised the item.
func (t FeedbackThread) Answered(round int, id string) []FeedbackItemView {
	return feedbackPick(t.Items, feedbackStatedBy(t.Items, round, id, false))
}

// Ruled returns the occurrences a product owner's ruling given in the given
// round settles: per lens that raised the item, the newest statement up to
// and including that round, because arbitration follows the verdict that
// held it (D-08). It is empty when no verdict ever raised the item.
func (t FeedbackThread) Ruled(round int, id string) []FeedbackItemView {
	return feedbackPick(t.Items, feedbackStatedBy(t.Items, round, id, true))
}

// Latest returns the newest round of the thread.
func (t FeedbackThread) Latest() (Round, bool) {
	if len(t.Rounds) == 0 {
		return Round{}, false
	}
	return t.Rounds[len(t.Rounds)-1], true
}

// Item returns the most recent occurrence of the item with the given id: a
// tester that repeats an item in a later round restates its status, so the
// last one is the one that counts. Answers stay on the occurrence they
// replied to, so the returned occurrence carries a response or a ruling
// only when that is the one it answered; use [FeedbackThread.Answered] and
// [FeedbackThread.Ruled] to walk the other way.
func (t FeedbackThread) Item(id string) (FeedbackItemView, bool) {
	for i := len(t.Items) - 1; i >= 0; i-- {
		if t.Items[i].ID == id {
			return t.Items[i], true
		}
	}
	return FeedbackItemView{}, false
}

// FeedbackThreads folds the run state into one thread per bead that has been
// reviewed, in the order the beads' first round started. It is the data
// model behind the Feedback view and is exported so headless output and
// `sc status` can say the same things the view does.
func FeedbackThreads(s *State) []FeedbackThread {
	if s == nil {
		return nil
	}
	at := make(map[domain.BeadID]int, len(s.Rounds))
	threads := make([]FeedbackThread, 0, len(s.Rounds))
	for _, r := range s.Rounds {
		i, ok := at[r.Bead]
		if !ok {
			i = len(threads)
			at[r.Bead] = i
			threads = append(threads, FeedbackThread{Bead: r.Bead})
		}
		threads[i].Rounds = append(threads[i].Rounds, r)
	}
	for i := range threads {
		threads[i] = feedbackBuildThread(s, threads[i])
	}
	return threads
}

// feedbackBuildThread fills in everything a thread derives from its rounds
// and from the bead behind them.
func feedbackBuildThread(s *State, th FeedbackThread) FeedbackThread {
	if b, ok := s.Bead(th.Bead); ok {
		th.Title, th.BeadState = b.Title, b.State
	}
	th.at = make(map[string]int)
	// latest is keyed by lens and item id, not by id alone: two lenses can
	// raise the same item in the same round, and each of them holds its own
	// open item until its own newest statement settles it.
	latest := make(map[string]int)
	for _, r := range th.Rounds {
		for _, v := range r.Verdicts {
			for _, it := range v.Items {
				th.Items = append(th.Items, FeedbackItemView{FeedbackItem: it, Lens: v.Lens, Round: r.Number})
				i := len(th.Items) - 1
				th.at[feedbackItemKey(r.Number, v.Lens, it.ID)] = i
				latest[feedbackLensKey(v.Lens, it.ID)] = i
			}
		}
	}
	// Responses and rulings name the item they answer but not the occurrence
	// of it, so each one is matched onto the statements it replied to: per
	// lens, the newest one raised before the round the engineer spoke in
	// (or, failing that, the one that lens raised in the same round), and —
	// for a ruling, which follows the verdict that held the item (D-08) —
	// the newest one up to and including its own round. Keying them by item
	// id alone would let a later round overwrite what was said in an earlier
	// one and lose the round it was said in; answering only one lens would
	// leave the other lens that raised the item looking unanswered.
	for _, r := range th.Rounds {
		for _, resp := range r.Responses {
			for _, i := range feedbackStatedBy(th.Items, r.Number, resp.ItemID, false) {
				th.Items[i].Response, th.Items[i].ResponseRound = &resp, r.Number
			}
		}
		for _, rule := range r.Rulings {
			for _, i := range feedbackStatedBy(th.Items, r.Number, rule.ItemID, true) {
				th.Items[i].Ruling, th.Items[i].RulingRound = &rule, r.Number
			}
		}
		if r.Escalation != nil {
			esc := *r.Escalation
			th.Escalation = &esc
		}
	}
	// A tester that restates an item in a later round restates where it
	// stands, so only its latest occurrence's status counts as open.
	for _, i := range latest {
		if !th.Items[i].Status.Settled() {
			th.Open++
		}
	}
	th.Testers = feedbackTesters(th.Rounds)
	th.Approved = len(th.Testers) > 0
	for _, t := range th.Testers {
		if !t.Satisfied() {
			th.Approved = false
		}
	}
	return th
}

// feedbackTesters works out where every lens that touched the bead stands.
func feedbackTesters(rounds []Round) []TesterStatus {
	at := make(map[domain.Lens]int)
	var out []TesterStatus
	touch := func(l domain.Lens) int {
		if i, ok := at[l]; ok {
			return i
		}
		at[l] = len(out)
		out = append(out, TesterStatus{Lens: l})
		return len(out) - 1
	}
	for _, r := range rounds {
		for _, l := range r.Lenses {
			touch(l)
		}
		for _, v := range r.Verdicts {
			i := touch(v.Lens)
			out[i].Outcome, out[i].Round = v.Outcome, r.Number
		}
	}
	// Only the newest round can still be waiting for a verdict.
	if len(rounds) > 0 {
		last := rounds[len(rounds)-1]
		returned := make(map[domain.Lens]bool, len(last.Verdicts))
		for _, v := range last.Verdicts {
			returned[v.Lens] = true
		}
		for _, l := range last.Lenses {
			if !returned[l] {
				out[at[l]].Pending = true
			}
		}
	}
	feedbackSortTesters(out)
	return out
}

// feedbackSortTesters puts the three known lenses first, in the order the
// domain declares them, and leaves anything else in first-seen order after
// them. It is a stable insertion sort: the lists are three or four long.
func feedbackSortTesters(ts []TesterStatus) {
	rank := func(l domain.Lens) int {
		for i, known := range domain.Lenses() {
			if known == l {
				return i
			}
		}
		return len(domain.Lenses())
	}
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0 && rank(ts[j].Lens) < rank(ts[j-1].Lens); j-- {
			ts[j], ts[j-1] = ts[j-1], ts[j]
		}
	}
}

// FeedbackKeyMap are the Feedback view's own bindings. The global Up and
// Down move between beads; these scroll the round history of the selected
// bead and open the full text of its items.
type FeedbackKeyMap struct {
	// Details toggles the long form of every item: detail, file, suggestion
	// and the rationale behind a response or ruling.
	Details key.Binding
	// PageUp scrolls the history one screen towards the first round.
	PageUp key.Binding
	// PageDown scrolls the history one screen towards the newest round.
	PageDown key.Binding
	// Top jumps to the first round.
	Top key.Binding
	// Bottom jumps to the newest round.
	Bottom key.Binding
}

// DefaultFeedbackKeyMap returns the standard bindings: enter for details,
// pgup/pgdown (ctrl+b/ctrl+f) to scroll and home/end (g/G) to jump.
func DefaultFeedbackKeyMap() FeedbackKeyMap {
	return FeedbackKeyMap{
		Details:  key.NewBinding(key.WithKeys("enter", "d"), key.WithHelp("enter", "details")),
		PageUp:   key.NewBinding(key.WithKeys("pgup", "ctrl+b"), key.WithHelp("pgup", "scroll up")),
		PageDown: key.NewBinding(key.WithKeys("pgdown", "ctrl+f"), key.WithHelp("pgdn", "scroll down")),
		Top:      key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("home", "first round")),
		Bottom:   key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("end", "last round")),
	}
}

// ShortHelp implements help.KeyMap.
func (k FeedbackKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Details, k.PageDown}
}

// FullHelp implements help.KeyMap.
func (k FeedbackKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Details}, {k.PageUp, k.PageDown}, {k.Top, k.Bottom}}
}

// FeedbackOptions configure the Feedback view.
type FeedbackOptions struct {
	// NoColor renders without any ANSI colour, like --no-color and NO_COLOR.
	// It is ignored when Theme is set.
	NoColor bool
	// Theme overrides the palette; nil means [EnvTheme] for NoColor.
	Theme *Theme
	// Keys overrides the view's own bindings; nil means
	// [DefaultFeedbackKeyMap].
	Keys *FeedbackKeyMap
	// Details starts the view with the long form of every item shown.
	Details bool
}

// feedbackDefaultWidth and feedbackDefaultHeight are the pane size the view
// assumes before [FeedbackModel.Resize] is called.
const (
	feedbackDefaultWidth  = 80
	feedbackDefaultHeight = 20
)

// feedbackListWidth is how many columns the bead list on the left takes:
// the cursor marker, a bead id and one tick per canonical lens.
const feedbackListWidth = 18

// feedbackMinDetail is the narrowest round history worth splitting the pane
// for; below it the bead list collapses to a single selector line.
const feedbackMinDetail = 40

// feedbackLensColumns are the lenses the list's tick columns stand for, in
// order. A lens outside this set still appears in the detail pane.
var feedbackLensColumns = [...]domain.Lens{domain.LensUX, domain.LensQA, domain.LensTest}

// FeedbackModel is the Feedback view (P7-04): every bead that went through
// review, with its rounds as sections, each tester's verdict and items
// coloured by severity, the engineer's responses inline, pushback and
// arbitration markers, and a satisfied tick per tester. It keeps only its
// own cursor, scroll offset and geometry; everything it draws comes from the
// [State] passed to [FeedbackModel.View] and [FeedbackModel.Update].
type FeedbackModel struct {
	theme   *Theme
	keys    FeedbackKeyMap
	details bool

	width, height int
	cursor        int
	offset        int
}

// NewFeedbackModel builds the Feedback view. The zero [FeedbackOptions] is
// valid.
func NewFeedbackModel(opts FeedbackOptions) FeedbackModel {
	theme := opts.Theme
	if theme == nil {
		theme = EnvTheme(opts.NoColor)
	}
	keys := DefaultFeedbackKeyMap()
	if opts.Keys != nil {
		keys = *opts.Keys
	}
	return FeedbackModel{
		theme:   theme,
		keys:    keys,
		details: opts.Details,
		width:   feedbackDefaultWidth,
		height:  feedbackDefaultHeight,
	}
}

// Resize tells the view how much room it has: the main pane, so the model's
// height minus the chrome.
func (v FeedbackModel) Resize(w, h int) FeedbackModel {
	v.width, v.height = max(w, 1), max(h, 1)
	return v
}

// Cursor returns the index of the selected bead in [FeedbackThreads].
func (v FeedbackModel) Cursor() int { return v.cursor }

// Offset returns the first visible line of the round history.
func (v FeedbackModel) Offset() int { return v.offset }

// Size returns the pane size the view was last resized to.
func (v FeedbackModel) Size() (int, int) { return v.width, v.height }

// Keys returns the view's own bindings.
func (v FeedbackModel) Keys() FeedbackKeyMap { return v.keys }

// ShowingDetails reports whether the long form of every item is shown.
func (v FeedbackModel) ShowingDetails() bool { return v.details }

// Selected returns the thread under the cursor, and false when no bead has
// been reviewed yet.
func (v FeedbackModel) Selected(st *State) (FeedbackThread, bool) {
	threads := FeedbackThreads(st)
	if len(threads) == 0 {
		return FeedbackThread{}, false
	}
	return threads[clampIndex(v.cursor, len(threads))], true
}

// Update handles one message and reports whether the view consumed it. It
// takes the run state because the cursor and the scroll offset are clamped
// to what exists, and the global [KeyMap] because Up and Down are shared
// with every other view.
func (v FeedbackModel) Update(msg tea.Msg, st *State, keys KeyMap) (FeedbackModel, bool) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return v, false
	}
	threads := FeedbackThreads(st)
	n := len(threads)
	v.cursor = clampIndex(v.cursor, n)
	switch {
	case key.Matches(km, keys.Up):
		v.cursor, v.offset = clampIndex(v.cursor-1, n), 0
	case key.Matches(km, keys.Down):
		v.cursor, v.offset = clampIndex(v.cursor+1, n), 0
	case key.Matches(km, v.keys.Details):
		v.details, v.offset = !v.details, 0
	case key.Matches(km, v.keys.PageUp):
		v.offset -= v.page()
	case key.Matches(km, v.keys.PageDown):
		v.offset += v.page()
	case key.Matches(km, v.keys.Top):
		v.offset = 0
	case key.Matches(km, v.keys.Bottom):
		v.offset = v.detailCount(threads)
	default:
		return v, false
	}
	v.offset = v.clampOffset(threads)
	return v, true
}

// page is how far one PageUp or PageDown scrolls: a screen of history less
// one line of overlap.
func (v FeedbackModel) page() int { return max(v.body()-1, 1) }

// body is how many lines the round history has, below the summary line and
// (in the narrow layout) the selector line.
func (v FeedbackModel) body() int {
	h := v.height - 1
	if !v.wide() {
		h--
	}
	return max(h, 1)
}

// wide reports whether the pane is roomy enough for the bead list column.
func (v FeedbackModel) wide() bool {
	return v.width-feedbackListWidth-3 >= feedbackMinDetail
}

// detailWidth is how many columns the round history gets.
func (v FeedbackModel) detailWidth() int {
	if v.wide() {
		return max(v.width-feedbackListWidth-3, 1)
	}
	return v.width
}

// detailCount is how many lines the selected thread's history renders to.
func (v FeedbackModel) detailCount(threads []FeedbackThread) int {
	if len(threads) == 0 {
		return 0
	}
	return len(v.detailLines(threads[clampIndex(v.cursor, len(threads))], v.detailWidth()))
}

// clampOffset keeps the scroll offset inside the rendered history.
func (v FeedbackModel) clampOffset(threads []FeedbackThread) int {
	return min(max(v.offset, 0), max(v.detailCount(threads)-v.body(), 0))
}

// View renders the pane: the summary line, the bead list and the round
// history of the selected bead. The result is always exactly the height the
// view was resized to.
func (v FeedbackModel) View(st *State) string {
	w, h := v.width, v.height
	threads := FeedbackThreads(st)
	if len(threads) == 0 {
		head := v.summary(threads, 0, 0)
		return fit(head+"\n"+v.theme.Muted().Render(emptyText[TabFeedback]), w, h)
	}
	cursor := clampIndex(v.cursor, len(threads))
	th := threads[cursor]
	body := v.body()
	detail := v.detailLines(th, v.detailWidth())
	offset := min(max(v.offset, 0), max(len(detail)-body, 0))

	lines := make([]string, 0, h)
	lines = append(lines, v.summary(threads, offset, len(detail)))
	if v.wide() {
		list := v.listLines(threads, cursor, body)
		rule := v.theme.Rule().Render("│")
		for i := range body {
			left := prsPad(feedbackAt(list, i), feedbackListWidth)
			lines = append(lines, strings.TrimRight(left+" "+rule+" "+feedbackAt(detail, offset+i), " "))
		}
	} else {
		lines = append(lines, v.selector(threads, cursor, w))
		for i := range body {
			lines = append(lines, feedbackAt(detail, offset+i))
		}
	}
	return fit(strings.Join(lines, "\n"), w, h)
}

// feedbackAt returns line i of lines, or "" past the end.
func feedbackAt(lines []string, i int) string {
	if i < 0 || i >= len(lines) {
		return ""
	}
	return lines[i]
}

// summary renders the first line: how many beads went through review, how
// they stand, and where the history is scrolled to.
func (v FeedbackModel) summary(threads []FeedbackThread, offset, total int) string {
	t := v.theme
	var approved, changes, escalated, open int
	for _, th := range threads {
		switch {
		case th.Escalation != nil:
			escalated++
		case th.Approved:
			approved++
		default:
			changes++
		}
		open += th.Open
	}
	fields := []string{fmt.Sprintf("%d %s reviewed", len(threads), feedbackPlural(len(threads), "bead"))}
	if approved > 0 {
		fields = append(fields, t.Success().Render(fmt.Sprintf("%d approved", approved)))
	}
	if changes > 0 {
		fields = append(fields, t.Warning().Render(fmt.Sprintf("%d in review", changes)))
	}
	if escalated > 0 {
		fields = append(fields, t.Danger().Render(fmt.Sprintf("%d escalated", escalated)))
	}
	if open > 0 {
		fields = append(fields, fmt.Sprintf("%d open %s", open, feedbackPlural(open, "item")))
	}
	if body := v.body(); total > body {
		fields = append(fields, t.Muted().Render(fmt.Sprintf("lines %d-%d/%d", offset+1, min(offset+body, total), total)))
	}
	return strings.Join(fields, t.Rule().Render(" · "))
}

// listLines renders the bead list column: a header and one row per bead,
// scrolled so the cursor stays visible.
func (v FeedbackModel) listLines(threads []FeedbackThread, cursor, h int) []string {
	t := v.theme
	lines := []string{t.Muted().Render("  " + prsPad("BEAD", 10) + " " + feedbackLensHeader())}
	rows := max(h-1, 1)
	start := 0
	if cursor >= rows {
		start = cursor - rows + 1
	}
	for i := start; i < min(start+rows, len(threads)); i++ {
		th := threads[i]
		row := t.Text().Render(prsPad(orDash(th.Bead.String()), 10)) + " " + v.ticksRow(th)
		if i == cursor {
			lines = append(lines, t.Selected().Render("▌ ")+row)
			continue
		}
		lines = append(lines, "  "+row)
	}
	return lines
}

// feedbackLensHeader is the tick columns' header: one letter per lens.
func feedbackLensHeader() string {
	cols := make([]string, 0, len(feedbackLensColumns))
	for _, l := range feedbackLensColumns {
		cols = append(cols, strings.ToUpper(l.String()[:1]))
	}
	return strings.Join(cols, " ")
}

// ticksRow renders one tick per canonical lens for the list column.
func (v FeedbackModel) ticksRow(th FeedbackThread) string {
	byLens := make(map[domain.Lens]TesterStatus, len(th.Testers))
	for _, t := range th.Testers {
		byLens[t.Lens] = t
	}
	cols := make([]string, 0, len(feedbackLensColumns))
	for _, l := range feedbackLensColumns {
		st := byLens[l]
		st.Lens = l
		cols = append(cols, st.style(v.theme).Render(st.Glyph()))
	}
	return strings.Join(cols, " ")
}

// testerTicks renders the satisfied tick of every tester of a thread, lens
// name included, for the detail pane and the narrow selector.
func (v FeedbackModel) testerTicks(th FeedbackThread) string {
	if len(th.Testers) == 0 {
		return v.theme.Muted().Render("no testers yet")
	}
	out := make([]string, 0, len(th.Testers))
	for _, t := range th.Testers {
		out = append(out, v.theme.LensStyle(t.Lens).Render(t.Lens.String())+" "+t.style(v.theme).Render(t.Glyph()))
	}
	return strings.Join(out, "  ")
}

// selector is the narrow layout's stand-in for the bead list: which bead of
// how many is shown, and its testers' ticks.
func (v FeedbackModel) selector(threads []FeedbackThread, cursor, w int) string {
	th := threads[cursor]
	head := v.theme.Selected().Render("▌ ") +
		v.theme.Muted().Render(fmt.Sprintf("%d/%d ", cursor+1, len(threads))) +
		v.theme.Accent().Render(orDash(th.Bead.String()))
	return clip(head+"  "+v.testerTicks(th), w)
}

// detailLines renders the selected bead's whole review history, one round
// per section, clipped to w columns.
func (v FeedbackModel) detailLines(th FeedbackThread, w int) []string {
	t := v.theme
	out := make([]string, 0, 8*len(th.Rounds)+4)
	add := func(s string) { out = append(out, clip(s, w)) }

	head := t.Accent().Render(orDash(th.Bead.String()))
	if th.Title != "" {
		head += "  " + t.Text().Render(prsTruncate(th.Title, max(w-lipgloss.Width(head)-2, 0)))
	}
	add(head)

	meta := make([]string, 0, 4)
	if th.BeadState != "" {
		meta = append(meta, t.StateLabel(th.BeadState))
	}
	meta = append(meta, fmt.Sprintf("%d %s", len(th.Rounds), feedbackPlural(len(th.Rounds), "round")))
	meta = append(meta, v.testerTicks(th))
	switch {
	case th.Open > 0:
		meta = append(meta, t.Warning().Render(fmt.Sprintf("%d open", th.Open)))
	case th.Approved:
		meta = append(meta, t.Success().Render("satisfied"))
	}
	add(strings.Join(meta, t.Rule().Render(" · ")))

	for i, r := range th.Rounds {
		add("")
		add(v.roundHead(r))
		out = append(out, v.roundBody(th, r, i == len(th.Rounds)-1, w)...)
	}
	return out
}

// roundHead renders a round's section heading.
func (v FeedbackModel) roundHead(r Round) string {
	t := v.theme
	fields := []string{string(r.Kind)}
	if r.Commit != "" {
		fields = append(fields, shortSHA(r.Commit))
	}
	if len(r.Lenses) > 0 {
		fields = append(fields, lensList(r.Lenses))
	}
	return t.Title().Render(fmt.Sprintf("round %d", r.Number)) +
		t.Muted().Render(" · "+strings.Join(fields, " · "))
}

// roundBody renders everything said in one round, in the order it was said:
// the engineer's report, its responses to the items the previous round
// raised, each tester's verdict with its items, the product owner's rulings
// and its escalation. Answers are drawn in the round they were given, not
// in the round the item they answer was raised in. The newest round also
// names the testers it is still waiting for.
func (v FeedbackModel) roundBody(th FeedbackThread, r Round, last bool, w int) []string {
	t := v.theme
	out := make([]string, 0, 8)
	add := func(s string) { out = append(out, clip(s, w)) }

	if r.Report != nil {
		out = append(out, v.flow("  "+t.Muted().Render("⚙ report")+" ", r.Report.Summary, t.Text(), 6, w)...)
		if v.details {
			if len(r.Report.FilesChanged) > 0 {
				out = append(out, v.flow("      ", "files: "+strings.Join(r.Report.FilesChanged, ", "), t.Muted(), 6, w)...)
			}
			if r.Report.HowToVerify != "" {
				out = append(out, v.flow("      ", "verify: "+r.Report.HowToVerify, t.Muted(), 6, w)...)
			}
			for _, risk := range r.Report.Risks {
				out = append(out, v.flow("      ", "risk: "+risk, t.Muted(), 6, w)...)
			}
		}
	}

	// A response normally answers the previous round, so it is drawn above
	// the round's own verdicts; one that answers an item this very round
	// raised belongs under the verdict that raised it instead, or it would
	// read as the engineer replying before the tester spoke.
	before, after := feedbackSplitResponses(th, r)
	for _, resp := range before {
		out = append(out, v.responseLines(th, r.Number, resp, w)...)
	}

	for _, verdict := range r.Verdicts {
		out = append(out, v.verdictLines(th, r.Number, verdict, w)...)
	}

	for _, resp := range after {
		out = append(out, v.responseLines(th, r.Number, resp, w)...)
	}

	for _, rule := range r.Rulings {
		out = append(out, v.rulingLine(rule, w)...)
		out = append(out, v.answerRefs(th.Ruled(r.Number, rule.ItemID), w)...)
	}

	if r.Escalation != nil {
		out = append(out, v.escalationLines(*r.Escalation, w)...)
	}

	if last {
		for _, tester := range th.Testers {
			if !tester.Pending {
				continue
			}
			add("  " + tester.style(t).Render(tester.Glyph()) + " " +
				t.LensStyle(tester.Lens).Render(prsPad(orDash(tester.Lens.String()), 4)) + " " +
				t.Muted().Render(tester.String()))
		}
	}

	if len(out) == 0 {
		add("  " + t.Muted().Render("waiting for verdicts"))
	}
	return out
}

// feedbackSplitResponses splits a round's responses into the ones that
// answer an earlier round, drawn above the round's verdicts, and the ones
// that answer an item this round raised, drawn under them. A response to an
// item no verdict ever raised has nothing to sit under, so it stays above.
func feedbackSplitResponses(th FeedbackThread, r Round) (before, after []domain.Response) {
	for _, resp := range r.Responses {
		answered := th.Answered(r.Number, resp.ItemID)
		same := len(answered) > 0
		for _, iv := range answered {
			if iv.Round != r.Number {
				same = false
			}
		}
		if same {
			after = append(after, resp)
			continue
		}
		before = append(before, resp)
	}
	return before, after
}

// verdictLines renders one tester's verdict and the items it raised. The
// round is the one the state filed the verdict under, which is what the
// items are indexed by; a cartridge may leave Verdict.Round unset.
func (v FeedbackModel) verdictLines(th FeedbackThread, round int, verdict domain.Verdict, w int) []string {
	t := v.theme
	status := TesterStatus{Lens: verdict.Lens, Outcome: verdict.Outcome}
	head := "  " + status.style(t).Render(status.Glyph()) + " " +
		t.LensStyle(verdict.Lens).Render(prsPad(orDash(verdict.Lens.String()), 4)) + " " +
		status.style(t).Render(prsPad(verdict.Outcome.String(), 7)) + " "
	out := v.flow(head, verdict.Summary, t.Muted(), 4, w)
	for _, it := range verdict.Items {
		iv, ok := th.occurrence(round, verdict.Lens, it.ID)
		if !ok {
			iv = FeedbackItemView{FeedbackItem: it, Lens: verdict.Lens, Round: round}
		}
		out = append(out, v.itemLines(iv, w)...)
	}
	return out
}

// itemLines renders one feedback item: severity, title, where it stands and
// what the engineer and the product owner said about it.
func (v FeedbackModel) itemLines(iv FeedbackItemView, w int) []string {
	t := v.theme
	out := make([]string, 0, 4)

	// The rules mark and the status bracket come before the title, which is
	// the only field long enough to run off the pane: they would otherwise
	// be the first thing a narrow pane cut.
	head := "    " + t.SeverityStyle(iv.Severity).Render(SeverityGlyph(iv.Severity)+" "+prsPad(iv.Severity.String(), 7)) +
		" " + t.Muted().Render(prsPad(iv.ID, 9))
	if iv.Category == domain.CategoryRules {
		head += " " + t.Danger().Render("(rules)")
	}
	if iv.Status != "" && iv.Status != domain.ItemOpen {
		head += " " + feedbackStatusStyle(t, iv.Status).Render("["+iv.Status.String()+"]")
	}
	out = append(out, v.flow(head+" ", iv.Title, t.Text(), 6, w)...)

	if v.details {
		if iv.File != "" {
			where := iv.File
			if iv.Line > 0 {
				where = fmt.Sprintf("%s:%d", iv.File, iv.Line)
			}
			out = append(out, v.flow("      ", where, t.Muted(), 6, w)...)
		}
		if iv.Detail != "" {
			out = append(out, v.flow("      ", iv.Detail, t.Muted(), 6, w)...)
		}
		if iv.Suggestion != "" {
			out = append(out, v.flow("      ", "suggestion: "+iv.Suggestion, t.Muted(), 6, w)...)
		}
	}

	return out
}

// responseLines render the engineer's answer to one item, in the round the
// engineer gave it: the arrow marks a fix, the hook marks a pushback (D-08)
// and the circle a wontfix. The item id says which item is being answered;
// [FeedbackModel.answerRefs] spells the occurrences out in the details view.
func (v FeedbackModel) responseLines(th FeedbackThread, round int, r domain.Response, w int) []string {
	t := v.theme
	glyph, style := "→", t.Success()
	switch r.Action {
	case domain.ActionPushback:
		glyph, style = "↩", t.Warning()
	case domain.ActionWontfix:
		glyph, style = "⊘", t.Muted()
	}
	head := "  " + style.Render(glyph+" "+prsPad(r.Action.String(), 8)) + " " + t.Muted().Render(prsPad(r.ItemID, 9)) + " "
	out := v.flow(head, r.Rationale, t.Muted(), 6, w)
	return append(out, v.answerRefs(th.Answered(round, r.ItemID), w)...)
}

// rulingLine renders the product owner's binding decision on a disputed
// item (D-08), in the round the ruling was made.
func (v FeedbackModel) rulingLine(a domain.Arbitration, w int) []string {
	t := v.theme
	style := t.Accent()
	switch a.Ruling {
	case domain.RulingUpholdTester:
		style = t.Warning()
	case domain.RulingUpholdEngineer:
		style = t.Success()
	}
	head := "  " + style.Render("⚖ "+prsPad(a.Ruling.String(), 16)) + " " + t.Muted().Render(prsPad(a.ItemID, 9)) + " "
	tail := a.Instruction
	if tail == "" {
		tail = a.Rationale
	}
	return v.flow(head, tail, t.Muted(), 6, w)
}

// answerRefs name the occurrences a response or a ruling replies to, so the
// details view can point back at the round and the wording of every item
// that drew it — two lenses can raise the same item id, and one answer
// settles both. It renders nothing outside the details view, and nothing
// when the answer names an item no verdict raised.
func (v FeedbackModel) answerRefs(ivs []FeedbackItemView, w int) []string {
	if !v.details {
		return nil
	}
	out := make([]string, 0, len(ivs))
	for _, iv := range ivs {
		head := "      " + v.theme.Muted().Render(fmt.Sprintf("re: round %d · %s · %s · ", iv.Round, iv.Lens, iv.Severity))
		out = append(out, v.flow(head, iv.Title, v.theme.Muted(), 6, w)...)
	}
	return out
}

// escalationLines render the product owner's ruling after the review round
// cap (D-08).
func (v FeedbackModel) escalationLines(e domain.Escalation, w int) []string {
	t := v.theme
	style := t.Warning()
	if e.Outcome == domain.EscalationBlock {
		style = t.Danger()
	}
	tail := e.Summary
	if tail == "" {
		tail = e.Notes
	}
	if tail == "" {
		tail = e.NewAcceptance
	}
	out := v.flow("  "+style.Render("▲ escalation "+e.Outcome.String())+" ", tail, t.Muted(), 6, w)
	if v.details && e.NewAcceptance != "" && tail != e.NewAcceptance {
		out = append(out, v.flow("      ", "acceptance: "+e.NewAcceptance, t.Muted(), 6, w)...)
	}
	return out
}

// flow renders a line whose last field is free text that can be longer than
// the pane: head is the styled prefix, up to and including the space the
// text starts after. The compact view cuts the text with an ellipsis, so a
// reader can see something was left out; the details view wraps the whole
// text onto continuation lines indented to indent columns, so the details
// toggle really does show the item's full text (P7-04 round 3). Nothing is
// ever hard-clipped without a mark.
func (v FeedbackModel) flow(head, text string, style lipgloss.Style, indent, w int) []string {
	avail := w - lipgloss.Width(head)
	if text == "" || avail <= 0 {
		return []string{clip(strings.TrimRight(head, " "), w)}
	}
	if !v.details {
		return []string{clip(head+style.Render(prsTruncate(text, avail)), w)}
	}
	segs := feedbackWrap(text, avail, w-indent)
	if len(segs) == 0 {
		return []string{clip(strings.TrimRight(head, " "), w)}
	}
	out := make([]string, 0, len(segs))
	out = append(out, clip(head+style.Render(segs[0]), w))
	for _, seg := range segs[1:] {
		out = append(out, clip(strings.Repeat(" ", max(indent, 0))+style.Render(seg), w))
	}
	return out
}

// feedbackWrap breaks s on spaces into segments that fit the given widths:
// first for the opening segment, which shares its line with a prefix, and
// rest for every continuation line. A single word wider than the line it
// starts is cut with an ellipsis rather than clipped silently.
func feedbackWrap(s string, first, rest int) []string {
	width, rest := max(first, 1), max(rest, 1)
	var out []string
	cur := ""
	flush := func() {
		out = append(out, cur)
		cur, width = "", rest
	}
	for _, word := range strings.Fields(s) {
		switch {
		case cur != "" && lipgloss.Width(cur)+1+lipgloss.Width(word) <= width:
			cur += " " + word
			continue
		case cur != "":
			flush()
		}
		if lipgloss.Width(word) > width {
			out = append(out, prsTruncate(word, width))
			width = rest
			continue
		}
		cur = word
	}
	if cur != "" {
		flush()
	}
	return out
}

// feedbackStatusStyle colours an item status: settled ones read as done,
// a held item as the dispute it is.
func feedbackStatusStyle(t *Theme, s domain.ItemStatus) lipgloss.Style {
	switch s {
	case domain.ItemResolved, domain.ItemAccepted:
		return t.Success()
	case domain.ItemOverruled:
		return t.Muted()
	case domain.ItemHeld:
		return t.Danger()
	default:
		return t.Warning()
	}
}

// feedbackPlural returns word with an "s" unless n is 1.
func feedbackPlural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
