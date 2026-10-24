package tui

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/proc"
)

// DeliveryStatus is where a delivery has got to: the PRs view's summary of a
// [Delivery] read together with the state of the bead behind it. It is
// derived, never published — the orchestrator reports facts (a pull request
// was opened, a commit landed) and the view turns them into a word.
type DeliveryStatus string

// Delivery statuses, in the order a delivery normally passes through them.
const (
	// DeliveryOpen means a pull request exists and nothing else is known
	// yet; with safety off it means no merge has been reported.
	DeliveryOpen DeliveryStatus = "open"
	// DeliveryAwaitingMerge means the pull request waits behind its `gh:pr`
	// gate for a human to merge it (safety on, D-20).
	DeliveryAwaitingMerge DeliveryStatus = "awaiting merge"
	// DeliveryReviewing means the cannon is still reviewing an external pull
	// request it did not open (D-11).
	DeliveryReviewing DeliveryStatus = "reviewing"
	// DeliveryReviewed means the consolidated review has been posted on an
	// external pull request; the cannon never merges one.
	DeliveryReviewed DeliveryStatus = "reviewed"
	// DeliveryMerged means the code is on the default branch.
	DeliveryMerged DeliveryStatus = "merged"
	// DeliveryClosed means the bead is closed although no merge was seen,
	// which is what a resumed run shows for work that landed earlier.
	DeliveryClosed DeliveryStatus = "closed"
	// DeliveryNeedsHuman means the delivery is parked for a person.
	DeliveryNeedsHuman DeliveryStatus = "needs human"
	// DeliveryFailed means the delivery was abandoned.
	DeliveryFailed DeliveryStatus = "failed"
)

// String returns the word shown in the STATUS column.
func (s DeliveryStatus) String() string { return string(s) }

// deliveryGlyphs are the one-cell markers of the statuses: the channel that
// survives NO_COLOR and colour blindness, like [StateGlyph] for beads.
var deliveryGlyphs = map[DeliveryStatus]string{
	DeliveryOpen:          "○",
	DeliveryAwaitingMerge: "⧗",
	DeliveryReviewing:     "◍",
	DeliveryReviewed:      "✓",
	DeliveryMerged:        "✔",
	DeliveryClosed:        "✔",
	DeliveryNeedsHuman:    "!",
	DeliveryFailed:        "✗",
}

// Glyph returns the status's one-cell marker, [UnknownGlyph] for anything
// unrecognised. It goes through [PadGlyph], like every glyph the palette
// hands out (P7-16): the set above mixes East-Asian-ambiguous characters
// (○ ⧗ ◍) with unambiguous ones (✓ ✔ ✗ !), so on a terminal configured for
// ambiguous-width 2 the STATUS column would otherwise shift by a cell
// depending on which status a delivery happens to be in.
func (s DeliveryStatus) Glyph() string {
	if g, ok := deliveryGlyphs[s]; ok {
		return PadGlyph(g)
	}
	return PadGlyph(UnknownGlyph)
}

// style returns the theme style a status is rendered in.
func (s DeliveryStatus) style(t *Theme) lipgloss.Style {
	switch s {
	case DeliveryMerged, DeliveryClosed, DeliveryReviewed:
		return t.Success()
	case DeliveryAwaitingMerge, DeliveryNeedsHuman:
		return t.Warning()
	case DeliveryFailed:
		return t.Danger()
	default:
		return t.Text()
	}
}

// CheckStatus is what the PRs view can say about a pull request's checks.
// The cannon does not poll GitHub for check runs: it learns about them when
// a failing check comes back as a `check_failed` review round (P6-06), so
// everything else is inferred from the delivery itself.
type CheckStatus string

// Check statuses.
const (
	// ChecksUnknown means nothing is known: no pull request, or none of the
	// signals below have arrived.
	ChecksUnknown CheckStatus = ""
	// ChecksPending means the pull request is open and no failure has come
	// back yet.
	ChecksPending CheckStatus = "pending"
	// ChecksFailing means a `check_failed` round is in flight for the bead.
	ChecksFailing CheckStatus = "failing"
	// ChecksConflict means the delivery hit a merge conflict and an engineer
	// is resolving it (D-10).
	ChecksConflict CheckStatus = "conflict"
	// ChecksPassed means the pull request was merged, so whatever gates the
	// repository requires were satisfied.
	ChecksPassed CheckStatus = "passed"
)

// String returns the word shown in the CHECKS column, "-" when unknown.
func (c CheckStatus) String() string {
	if c == ChecksUnknown {
		return "-"
	}
	return string(c)
}

// style returns the theme style a check status is rendered in.
func (c CheckStatus) style(t *Theme) lipgloss.Style {
	switch c {
	case ChecksPassed:
		return t.Success()
	case ChecksFailing:
		return t.Danger()
	case ChecksPending, ChecksConflict:
		return t.Warning()
	default:
		return t.Muted()
	}
}

// PRRow is one line of the PRs view: a [Delivery] plus everything the view
// derives from the rest of the run state — the bead's title and state, the
// status word, the check status and, for an external pull request, the
// review the cannon posted on it.
type PRRow struct {
	// Delivery is the pull request and the merge as the events described them.
	Delivery
	// Title is the bead's title, when the bead is known.
	Title string
	// BeadState is the bead's workflow state, when the bead is known.
	BeadState domain.BeadState
	// Status is where the delivery has got to.
	Status DeliveryStatus
	// Checks is what is known about the pull request's checks; it stays
	// [ChecksUnknown] for an external pull request, which has [PRRow.Review]
	// instead.
	Checks CheckStatus
	// Review is the outcome of the consolidated review the cannon posted on
	// an external pull request (D-11). It stays empty while the round is
	// still collecting verdicts, because a review is posted once, for the
	// whole round: reporting the outcome of a half-finished round would show
	// a review that was never posted and could still flip.
	Review domain.VerdictOutcome
	// ReviewsIn is how many of the round's verdicts have arrived.
	ReviewsIn int
	// ReviewsExpected is how many the round announced it was waiting for; it
	// is 0 when the round never said, so the view cannot tell whether the
	// review is complete.
	ReviewsExpected int
}

// PR renders the pull-request column: "#42", "ext #77" for a pull request
// the cannon only reviews, or "-" when there is none.
func (r PRRow) PR() string {
	if r.Number <= 0 {
		if r.External {
			return "ext"
		}
		return "-"
	}
	if r.External {
		return fmt.Sprintf("ext #%d", r.Number)
	}
	return fmt.Sprintf("#%d", r.Number)
}

// Note renders the CHECKS column: the check status for the cannon's own
// deliveries, and for an external pull request the outcome of the review the
// cannon posted on it. While the review round is still running it shows how
// many verdicts are in ("1/3", or "1/?" when the round never announced its
// lenses) rather than an outcome that has not been posted.
func (r PRRow) Note() string {
	if !r.External {
		return r.Checks.String()
	}
	switch {
	case r.Review != "":
		return r.Review.String()
	case r.ReviewsExpected > 0:
		return fmt.Sprintf("%d/%d", r.ReviewsIn, r.ReviewsExpected)
	case r.ReviewsIn > 0:
		return fmt.Sprintf("%d/?", r.ReviewsIn)
	default:
		return "-"
	}
}

// Ref renders the REF column: the pull request URL when there is one, else
// the short merge sha, else "-".
func (r PRRow) Ref() string {
	switch {
	case r.URL != "":
		return r.URL
	case r.SHA != "":
		return shortSHA(r.SHA)
	default:
		return "-"
	}
}

// OpenURL returns the URL the `o` key opens for this row, and whether there
// is one.
func (r PRRow) OpenURL() (string, bool) {
	if r.URL == "" {
		return "", false
	}
	return r.URL, true
}

// PRRows folds the run state into one row per delivery, in the order the
// deliveries were opened — the order [State.Deliveries] keeps.
func PRRows(s *State) []PRRow {
	if s == nil {
		return nil
	}
	out := make([]PRRow, 0, len(s.Deliveries))
	for _, d := range s.Deliveries {
		out = append(out, prRow(s, d))
	}
	return out
}

// prRow derives one row from a delivery and the bead and rounds behind it.
//
// The text it copies out of the state is not the cannon's own: a bead title
// is written by a model through bd, and the pull request URL, branch and
// merge sha come out of gh and git. All of it is drawn into the frame — the
// REF column, the detail line, the row's own columns — so it goes through
// the package's scrubber here, once, where the row is built, rather than at
// each of the four places that render it (P7-18). An escape sequence would
// repaint the frame, and Lip Gloss would still measure the row as if the
// bytes were not there.
func prRow(s *State, d Delivery) PRRow {
	r := PRRow{Delivery: d}
	r.URL, r.Branch, r.Base, r.SHA = rowText(d.URL), rowText(d.Branch), rowText(d.Base), rowText(d.SHA)
	if b, ok := s.Bead(d.Bead); ok {
		r.Title, r.BeadState = rowText(b.Title), b.State
	}
	outcome, in, expected := prReviewProgress(s, d)
	r.ReviewsIn, r.ReviewsExpected = in, expected
	// The review is posted once, when the round is complete; a closed review
	// bead is complete whatever the round said it was waiting for.
	if prReviewPosted(in, expected) || r.BeadState == domain.BeadStateClosed {
		r.Review = outcome
	}
	r.Status = prStatus(s, r)
	r.Checks = prChecks(s, r)
	return r
}

// prStatus picks the status word for a row. An external pull request only
// counts as reviewed once [PRRow.Review] is set, that is once the round it
// is being reviewed in has finished and the consolidated review has gone
// out; until then it is still being reviewed.
func prStatus(s *State, r PRRow) DeliveryStatus {
	switch {
	case r.External:
		if r.Review != "" || r.BeadState == domain.BeadStateClosed {
			return DeliveryReviewed
		}
		return DeliveryReviewing
	case r.Merged:
		return DeliveryMerged
	case r.BeadState == domain.BeadStateFailed:
		return DeliveryFailed
	case r.BeadState == domain.BeadStateNeedsHuman:
		return DeliveryNeedsHuman
	case r.BeadState == domain.BeadStateClosed:
		return DeliveryClosed
	case r.URL != "" && s.Run.Config.Safety:
		return DeliveryAwaitingMerge
	default:
		return DeliveryOpen
	}
}

// prChecks picks the check status for a row.
func prChecks(s *State, r PRRow) CheckStatus {
	if r.External {
		return ChecksUnknown
	}
	if !r.Merged {
		switch prLastRoundKind(s, r.Bead) {
		case events.RoundCheckFailed:
			return ChecksFailing
		case events.RoundConflict:
			return ChecksConflict
		}
	}
	switch {
	case r.Merged && r.Number > 0:
		return ChecksPassed
	// Nothing is running on a delivery nobody is working on any more, so an
	// abandoned or parked pull request keeps its last known check status
	// (failing, conflict) and otherwise says nothing.
	case r.Status == DeliveryFailed || r.Status == DeliveryNeedsHuman:
		return ChecksUnknown
	case r.URL != "":
		return ChecksPending
	default:
		return ChecksUnknown
	}
}

// prLastRoundKind returns the kind of the bead's highest-numbered round, or
// "" when it has none.
func prLastRoundKind(s *State, bead domain.BeadID) events.RoundKind {
	var kind events.RoundKind
	best := 0
	for _, rd := range s.Rounds {
		if rd.Bead == bead && rd.Number >= best {
			best, kind = rd.Number, rd.Kind
		}
	}
	return kind
}

// prReviewProgress consolidates the verdicts of the bead's latest round the
// way gitx.BuildReview does — one "changes" verdict makes the whole review a
// request for changes, otherwise it is an approval — and reports how far the
// round has got: how many verdicts arrived and how many the round said it
// was waiting for (0 when it never announced its lenses). The outcome is
// only what the verdicts so far add up to; [prReviewPosted] says whether it
// is the review that was actually posted.
func prReviewProgress(s *State, d Delivery) (outcome domain.VerdictOutcome, received, expected int) {
	best, found := 0, false
	var round Round
	for _, rd := range s.Rounds {
		if rd.Bead != d.Bead || len(rd.Verdicts) == 0 || rd.Number < best {
			continue
		}
		best, found, round = rd.Number, true, rd
	}
	if !found {
		return "", 0, 0
	}
	outcome = domain.VerdictApprove
	for _, v := range round.Verdicts {
		if v.Outcome == domain.VerdictChanges {
			outcome = domain.VerdictChanges
			break
		}
	}
	return outcome, len(round.Verdicts), len(round.Lenses)
}

// prReviewPosted reports whether a round with the given verdict counts is
// finished, which is when the testers' single consolidated review goes out
// (D-11). A round that never announced its lenses can never be known to be
// finished, so it is not posted on that evidence alone.
func prReviewPosted(received, expected int) bool {
	return expected > 0 && received >= expected
}

// PRsKeyMap are the bindings the PRs view adds to the global [KeyMap].
// Cursor movement comes from the global map's Up and Down.
type PRsKeyMap struct {
	// Open hands the selected pull request's URL to the OS opener.
	Open key.Binding
}

// DefaultPRsKeyMap returns the standard bindings of the PRs view.
func DefaultPRsKeyMap() PRsKeyMap {
	return PRsKeyMap{
		Open: key.NewBinding(
			key.WithKeys("o"),
			key.WithHelp("o", "open PR"),
		),
	}
}

// ShortHelp implements the [help.KeyMap] interface: the keys the footer can
// show while the PRs view is up.
func (k PRsKeyMap) ShortHelp() []key.Binding { return []key.Binding{k.Open} }

// FullHelp implements the [help.KeyMap] interface.
func (k PRsKeyMap) FullHelp() [][]key.Binding { return [][]key.Binding{{k.Open}} }

// URLOpener hands a URL to something that can show it to the user. The PRs
// view calls one when `o` is pressed; [OpenURL] is the real implementation
// and a test passes its own so nothing ever launches a browser.
type URLOpener func(ctx context.Context, rawURL string) error

// URLOpenedMsg reports the result of the `o` key: the URL handed to the
// opener and the error it returned, if any. A program that shows failures
// can turn a non-nil Err into a notice; ignoring the message is fine too.
type URLOpenedMsg struct {
	// URL is the pull request URL that was opened.
	URL string
	// Err is why it could not be opened, or nil on success.
	Err error
}

// PRsOptions configure the PRs view.
type PRsOptions struct {
	// NoColor renders without any ANSI colour, like --no-color and NO_COLOR.
	// It is ignored when Theme is set.
	NoColor bool
	// Theme overrides the palette; nil means [EnvTheme] for NoColor.
	Theme *Theme
	// Keys overrides the view's own bindings; nil means [DefaultPRsKeyMap].
	Keys *PRsKeyMap
	// Open replaces the OS opener the `o` key calls; nil means [OpenURL].
	Open URLOpener
}

// prsDefaultWidth and prsDefaultHeight are the pane size the view assumes
// before [PRsModel.Resize] is called.
const (
	prsDefaultWidth  = 80
	prsDefaultHeight = 20
)

// Column widths of the list, in the order the columns appear. The status
// column carries a glyph as well as a word.
const (
	prsBeadWidth   = 10
	prsPRWidth     = 8
	prsStatusWidth = 17
	prsNoteWidth   = 9
)

// prsMaxBeadWidth caps the BEAD column. It widens past [prsBeadWidth] to
// fit the longest bead id in the list — a repository's bd prefix is its
// name, so "PandaJump-5c5" is as ordinary as "sc-1a2b" — but no further than
// this, so one odd id cannot push every other column off the pane.
const prsMaxBeadWidth = 24

// prsBeadColumn is how wide the BEAD column is for rows: wide enough for
// the longest id, between [prsBeadWidth] and [prsMaxBeadWidth].
func prsBeadColumn(rows []PRRow) int {
	w := prsBeadWidth
	for _, r := range rows {
		w = max(w, lipgloss.Width(orDash(rowText(r.Bead.String()))))
	}
	return min(w, prsMaxBeadWidth)
}

// prsCursorWidth is the two cells every row spends on the cursor marker.
const prsCursorWidth = 2

// prsRefColumn is the column the REF field starts in: the four fixed
// columns plus the single space between each of them.
const prsRefColumn = prsBeadWidth + prsPRWidth + prsStatusWidth + prsNoteWidth + 4

// prsMinRef is the narrowest REF column worth having. A pull request URL is
// around forty columns ("https://github.com/acme/app/pull/1234"), and a REF
// column that cuts every URL is worse than none: the detail line shows the
// whole URL instead, where it can be read and copied.
const prsMinRef = 40

// prsWideWidth is the pane width at which the list shows the REF column;
// below it the reference moves to the detail line. At the canonical 80×24
// terminal there is no room for it, so 80 columns is a narrow pane.
const prsWideWidth = prsCursorWidth + prsRefColumn + prsMinRef

// prsRefWidth is how many columns the REF field has in a pane w wide.
func prsRefWidth(w int) int { return w - prsCursorWidth - prsRefColumn }

// PRsModel is the PRs view (P7-05): the safety indicator, the deliveries of
// the run with their pull request, check status and merge, the external
// pull requests the cannon reviewed, and `o` to open the selected one in a
// browser. It keeps only its own cursor and geometry; everything it draws
// comes from the [State] passed to [PRsModel.View] and [PRsModel.Update].
type PRsModel struct {
	theme *Theme
	keys  PRsKeyMap
	open  URLOpener

	width, height int
	cursor        int
}

// NewPRsModel builds the PRs view. The zero [PRsOptions] is valid.
func NewPRsModel(opts PRsOptions) PRsModel {
	theme := opts.Theme
	if theme == nil {
		theme = EnvTheme(opts.NoColor)
	}
	keys := DefaultPRsKeyMap()
	if opts.Keys != nil {
		keys = *opts.Keys
	}
	open := opts.Open
	if open == nil {
		open = OpenURL
	}
	return PRsModel{
		theme:  theme,
		keys:   keys,
		open:   open,
		width:  prsDefaultWidth,
		height: prsDefaultHeight,
	}
}

// Resize tells the view how much room it has.
func (v PRsModel) Resize(w, h int) PRsModel {
	v.width, v.height = max(w, 1), max(h, 1)
	return v
}

// Cursor returns the index of the selected delivery.
func (v PRsModel) Cursor() int { return v.cursor }

// Size returns the pane size the view was last resized to.
func (v PRsModel) Size() (int, int) { return v.width, v.height }

// Keys returns the view's own bindings.
func (v PRsModel) Keys() PRsKeyMap { return v.keys }

// Selected returns the row under the cursor, and false when the run has no
// deliveries yet.
func (v PRsModel) Selected(st *State) (PRRow, bool) {
	rows := PRRows(st)
	if len(rows) == 0 {
		return PRRow{}, false
	}
	return rows[clampIndex(v.cursor, len(rows))], true
}

// Update handles one message and reports whether the view consumed it. It
// takes the run state because the cursor is clamped to the deliveries that
// exist, and the global [KeyMap] because Up and Down are shared with every
// other view. The command it returns opens a pull request and reports the
// result as a [URLOpenedMsg].
func (v PRsModel) Update(msg tea.Msg, st *State, keys KeyMap) (PRsModel, tea.Cmd, bool) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return v, nil, false
	}
	n := len(PRRows(st))
	v.cursor = clampIndex(v.cursor, n)
	switch {
	case key.Matches(km, keys.Up):
		v.cursor = clampIndex(v.cursor-1, n)
	case key.Matches(km, keys.Down):
		v.cursor = clampIndex(v.cursor+1, n)
	case key.Matches(km, v.keys.Open):
		return v, v.OpenCmd(st), true
	default:
		return v, nil, false
	}
	return v, nil, true
}

// OpenCmd returns a command that hands the selected pull request's URL to
// the OS opener and reports the result as a [URLOpenedMsg]. It returns nil
// when nothing is selected or the selection has no URL, so `o` on a bead
// merged straight to the default branch does nothing.
func (v PRsModel) OpenCmd(st *State) tea.Cmd {
	row, ok := v.Selected(st)
	if !ok {
		return nil
	}
	target, ok := row.OpenURL()
	if !ok {
		return nil
	}
	open := v.open
	if open == nil {
		open = OpenURL
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), OpenTimeout)
		defer cancel()
		return URLOpenedMsg{URL: target, Err: open(ctx, target)}
	}
}

// View renders the pane: the safety indicator and delivery counts, the
// column header, the scrolled list, and a detail line for the selected row
// pinned to the bottom. The result is always exactly the height the view was
// resized to.
func (v PRsModel) View(st *State) string {
	w, h := v.width, v.height
	rows := PRRows(st)
	head := v.summary(st, rows, w)
	if len(rows) == 0 {
		return fit(head+"\n"+v.theme.Muted().Render(emptyText[TabPRs]), w, h)
	}

	bead := prsBeadColumn(rows)
	// A wider BEAD column moves every column after it, the REF field's
	// start included, so the wide layout needs that much more pane.
	extra := bead - prsBeadWidth
	wide := w >= prsWideWidth+extra
	lines := []string{head, v.theme.Muted().Render(prsHeaderLine(wide, bead))}
	cursor := clampIndex(v.cursor, len(rows))
	body := max(h-len(lines)-1, 1)
	start := 0
	if cursor >= body {
		start = cursor - body + 1
	}
	for i := start; i < min(start+body, len(rows)); i++ {
		line := clip(v.rowLine(rows[i], wide, bead), w-2)
		if i == cursor {
			lines = append(lines, v.theme.Selected().Render("▌ ")+line)
			continue
		}
		lines = append(lines, "  "+line)
	}
	// The detail sits on the last line, so it stays put as the list scrolls.
	for len(lines) < h-1 {
		lines = append(lines, "")
	}
	return fit(strings.Join(append(lines, v.detail(rows[cursor], w, wide, extra)), "\n"), w, h)
}

// summary renders the safety indicator, what safety means for a delivery,
// and the counts. When all three do not fit in w the sentence explaining
// safety gives way, because it is the only part the counts cannot replace
// and the only part that is the same on every run.
func (v PRsModel) summary(st *State, rows []PRRow, w int) string {
	safety := st != nil && st.Run.Config.Safety
	indicator := "safety " + v.theme.Accent().Render(onOff(safety))
	explain := "merged straight to the default branch"
	switch {
	case safety && st.Run.Config.AutoMerge:
		explain = "the run merges its own green PRs (D-31)"
	case safety:
		explain = "a human merges every PR (D-20)"
	}
	counts := prsCounts(rows)
	sep := v.theme.Rule().Render(" · ")
	full := strings.Join([]string{indicator, v.theme.Muted().Render(explain), counts}, sep)
	if lipgloss.Width(full) <= w {
		return full
	}
	return strings.Join([]string{indicator, counts}, sep)
}

// prsCounts renders the delivery counts. Work nobody is pushing any more is
// counted apart from the work in flight: an abandoned or parked delivery is
// not on its way anywhere.
func prsCounts(rows []PRRow) string {
	var merged, inFlight, parked, abandoned, external int
	for _, r := range rows {
		switch {
		case r.External:
			external++
		case r.Status == DeliveryMerged || r.Status == DeliveryClosed:
			merged++
		case r.Status == DeliveryNeedsHuman:
			parked++
		case r.Status == DeliveryFailed:
			abandoned++
		default:
			inFlight++
		}
	}
	counts := []string{fmt.Sprintf("%d delivered", len(rows)-external)}
	for _, c := range []struct {
		n    int
		word string
	}{
		{merged, "merged"},
		{inFlight, "in flight"},
		{parked, "needs a human"},
		{abandoned, "abandoned"},
		{external, "external"},
	} {
		if c.n > 0 {
			counts = append(counts, fmt.Sprintf("%d %s", c.n, c.word))
		}
	}
	return strings.Join(counts, ", ")
}

// prsHeaderLine renders the column header, matching rowLine's widths.
func prsHeaderLine(wide bool, bead int) string {
	cols := []string{
		prsPad("BEAD", bead),
		prsPad("PR", prsPRWidth),
		prsPad("STATUS", prsStatusWidth),
	}
	if !wide {
		return "  " + strings.Join(append(cols, "CHECKS"), " ")
	}
	cols = append(cols, prsPad("CHECKS", prsNoteWidth))
	return "  " + strings.Join(append(cols, "REF"), " ")
}

// rowLine renders one delivery as a column-aligned line. The narrow layout
// drops the reference, which the detail line then carries.
func (v PRsModel) rowLine(r PRRow, wide bool, bead int) string {
	status := r.Status.style(v.theme).Render(prsPad(r.Status.Glyph()+" "+r.Status.String(), prsStatusWidth))
	note := r.Note()
	if !r.External {
		note = r.Checks.style(v.theme).Render(prsPad(note, prsNoteWidth))
	} else {
		note = v.theme.Text().Render(prsPad(note, prsNoteWidth))
	}
	cols := []string{
		// The bead id is bd's, not the cannon's: it is scrubbed like any
		// other borrowed text, and a clean id costs one scan.
		v.theme.Text().Render(prsPad(orDash(rowText(r.Bead.String())), bead)),
		v.theme.Muted().Render(prsPad(r.PR(), prsPRWidth)),
		status,
		note,
	}
	if !wide {
		return strings.TrimRight(strings.Join(cols, " "), " ")
	}
	return strings.TrimRight(strings.Join(append(cols, v.theme.Muted().Render(r.Ref())), " "), " ")
}

// detail renders the pane's last line (extra is how much wider than
// [prsBeadWidth] the BEAD column is, which the REF column gives up): what the selected row is, and whether
// `o` has a URL to open. The URL goes here whenever the row cannot show it in
// full — always in the narrow layout, which has no REF column, and in the
// wide layout when the URL is longer than the REF column — so it can always
// be read and copied off the screen somewhere. The title gives way first when
// the line still does not fit.
func (v PRsModel) detail(r PRRow, w int, wide bool, extra int) string {
	var tail []string
	u, hasURL := r.OpenURL()
	if wide {
		if hasURL && lipgloss.Width(u) > prsRefWidth(w)-extra {
			tail = append(tail, u)
		}
		// The REF column carries the rest, so spend the line on what the
		// columns cannot say.
		if r.External {
			// D-11: the cannon reviews a pull request it did not open and
			// never merges it, so say which of the two it has got to.
			state := "reviewing"
			if r.Review != "" {
				state = "reviewed"
			}
			tail = append(tail, "external PR: "+state+", never merged")
		}
		if r.Merged && r.SHA != "" {
			landed := "merged " + shortSHA(r.SHA)
			if r.Pushed {
				landed += ", pushed"
			}
			tail = append(tail, landed)
		}
	} else if hasURL {
		tail = append(tail, u)
	}
	if hasURL {
		tail = append(tail, v.theme.Muted().Render("o open"))
	} else {
		tail = append(tail, v.theme.Muted().Render("no URL to open"))
	}

	head := v.theme.Accent().Render(orDash(rowText(r.Bead.String())))
	rest := strings.Join(tail, "  ")
	budget := w - lipgloss.Width(head) - lipgloss.Width(rest) - len("  ")*2 - len(`""`)
	if r.Title == "" || budget < prsMinTitle {
		return head + "  " + rest
	}
	return fmt.Sprintf("%s  %q  %s", head, prsTruncate(r.Title, budget), rest)
}

// prsMinTitle is the shortest title worth showing in the detail line; below
// it the line drops the title rather than render a stub.
const prsMinTitle = 8

// prsTruncate shortens s to at most n display columns, ending in an ellipsis
// when it had to cut.
func prsTruncate(s string, n int) string {
	if n <= 0 || lipgloss.Width(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:min(max(n-1, 0), len(r))]), " ") + "…"
}

// prsPad pads s with spaces to n display columns, leaving it alone when it
// is already at least that wide.
func prsPad(s string, n int) string {
	if pad := n - lipgloss.Width(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// OpenTimeout caps how long the OS opener may take before it is killed. It
// is expected to hand the URL to an already-running browser and return at
// once.
const OpenTimeout = 15 * time.Second

// OpenerEnv names the environment variable that replaces the OS opener with
// another program. The URL is appended to it as the only argument.
const OpenerEnv = "SC_OPENER"

// OpenURL hands rawURL to the platform's opener: `open` on macOS,
// `rundll32 url.dll,FileProtocolHandler` on Windows and `xdg-open`
// elsewhere, or the program named by [OpenerEnv] when it is set. Only http
// and https URLs are opened, so a malformed pull request URL from an event
// can never launch an arbitrary handler.
func OpenURL(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("tui: open %q: %w", rawURL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("tui: refusing to open %q: only http and https URLs are opened", rawURL)
	}
	command, args := openerCommand(rawURL)
	res, err := proc.Run(ctx, proc.Spec{
		Command:     command,
		Args:        args,
		Timeout:     OpenTimeout,
		GracePeriod: time.Second,
		InheritEnv:  openerEnvPassthrough,
	})
	if err != nil {
		return fmt.Errorf("tui: open %q with %s: %w", rawURL, command, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("tui: open %q with %s: exit %d", rawURL, command, res.ExitCode)
	}
	return nil
}

// openerCommand returns the program to run and the arguments up to and
// including the URL.
func openerCommand(rawURL string) (string, []string) {
	if custom := strings.TrimSpace(os.Getenv(OpenerEnv)); custom != "" {
		return custom, []string{rawURL}
	}
	switch runtime.GOOS {
	case "darwin":
		return "open", []string{rawURL}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", rawURL}
	default:
		return "xdg-open", []string{rawURL}
	}
}

// openerEnvPassthrough are the parent variables a desktop opener needs to
// find the user's session and browser; [proc.Spec] withholds everything else.
var openerEnvPassthrough = []string{
	"HOME", "USER", "LOGNAME", "TMPDIR", "SHELL", "LANG", "BROWSER",
	"DISPLAY", "WAYLAND_DISPLAY", "XAUTHORITY", "DBUS_SESSION_BUS_ADDRESS",
	"XDG_RUNTIME_DIR", "XDG_CURRENT_DESKTOP", "XDG_SESSION_TYPE", "XDG_DATA_DIRS",
	"SYSTEMROOT", "WINDIR",
}
