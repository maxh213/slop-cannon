package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// Exit codes the headless renderer derives from [events.RunFinished.Reason]
// (D-15, P7-11). `sc run --headless` exits with whatever [Headless.ExitCode]
// returns, so a script can tell the three interesting outcomes apart.
const (
	// ExitOK is the code for a run that ended the way it was asked to:
	// [events.FinishCompleted], [events.FinishStopped] after a user stop,
	// `sc stop` or a signal, or [events.FinishBudgetReached] after a brief
	// run reached a cap the user set. Stopping a run is not a failure, and
	// neither is a budget doing exactly what it was set to do.
	ExitOK = 0
	// ExitFailed is the code for [events.FinishFailed], for a finish reason
	// this build does not know, and for a stream that ended without any
	// [events.RunFinished] at all (the orchestrator died mid-run).
	ExitFailed = 1
	// ExitCreditsExhausted is the code for [events.FinishCreditsExhausted],
	// kept distinct from ExitFailed so CI can retry later rather than treat
	// an empty wallet as a broken run (D-18).
	ExitCreditsExhausted = 3
)

// headlessTimeFormat is the clock stamp every headless line starts with.
const headlessTimeFormat = "15:04:05"

// ExitCodeFor maps a run's finish reason to a process exit code. An unknown
// or empty reason is treated as a failure.
func ExitCodeFor(reason events.FinishReason) int {
	switch reason {
	case events.FinishCompleted, events.FinishStopped, events.FinishBudgetReached:
		return ExitOK
	case events.FinishCreditsExhausted:
		return ExitCreditsExhausted
	case events.FinishFailed:
		return ExitFailed
	default:
		return ExitFailed
	}
}

// HeadlessOptions configure the headless renderer.
type HeadlessOptions struct {
	// Out is where lines are written; nil means os.Stdout. Output is never
	// coloured, so it is safe to pipe into a file or a CI log.
	Out io.Writer
	// JSON writes the raw event envelopes as JSONL (`--json`) instead of
	// human-readable lines. The exit code is derived either way.
	JSON bool
	// Location is the time zone of the clock stamp; nil means time.Local.
	// Tests pass time.UTC so their golden files are stable.
	Location *time.Location
	// Filter decides which events are rendered; nil renders every one. The
	// finish reason is recorded even for an event the filter drops, so the
	// exit code never depends on it. A caller that does not want an agent's
	// whole transcript on stdout filters out [events.KindAgentOutput] here.
	Filter func(events.Event) bool
}

// Headless renders a run's events as text: one line per event, or one JSON
// envelope per line with [HeadlessOptions.JSON] (D-15). Nothing is buffered
// inside the renderer and a writer that buffers is flushed after every
// event, so a killed process still leaves a complete log up to its last
// event. It is safe for concurrent use; the first write error is sticky and
// returned by every later call.
type Headless struct {
	mu      sync.Mutex
	out     io.Writer
	jsonl   *events.JSONLWriter
	loc     *time.Location
	filter  func(events.Event) bool
	n       uint64
	dropped uint64
	finish  *events.RunFinished
	err     error
}

// flusher is implemented by buffered writers such as *bufio.Writer.
type flusher interface{ Flush() error }

// NewHeadless builds a renderer writing to opts.Out (os.Stdout when nil).
func NewHeadless(opts HeadlessOptions) *Headless {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	h := &Headless{out: out, loc: opts.Location, filter: opts.Filter}
	if h.loc == nil {
		h.loc = time.Local
	}
	if opts.JSON {
		h.jsonl = events.NewJSONLWriter(out)
	}
	return h
}

// Write renders one event. A nil event is ignored. The event's finish reason
// (if it is an [events.RunFinished]) is recorded before the filter runs, so
// [Headless.ExitCode] sees every run outcome.
func (h *Headless) Write(ev events.Event) error {
	if ev == nil {
		return nil
	}
	if fin, ok := ev.(events.RunFinished); ok {
		h.mu.Lock()
		h.finish = &fin
		h.mu.Unlock()
	}
	if h.filter != nil && !h.filter(ev) {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return h.err
	}
	if h.jsonl != nil {
		if err := h.jsonl.Write(ev); err != nil {
			h.err = err
			return h.err
		}
	} else if _, err := io.WriteString(h.out, FormatHeadless(ev, h.loc)+"\n"); err != nil {
		h.err = fmt.Errorf("tui: headless write: %w", err)
		return h.err
	}
	if f, ok := h.out.(flusher); ok {
		if err := f.Flush(); err != nil {
			h.err = fmt.Errorf("tui: headless flush: %w", err)
			return h.err
		}
	}
	h.n++
	return nil
}

// Serve subscribes to bus and renders every event until ctx is done, the bus
// is closed, or a write fails. A buffer of zero or less means
// [DefaultBuffer]. Events the subscription dropped are reported inline as an
// [events.Warning] from source "headless" rather than silently lost.
//
// It returns nil when the bus closed, ctx.Err() when the context ended, and
// the write error otherwise. Serve blocks, so run it in its own goroutine
// when the caller also produces the events.
func (h *Headless) Serve(ctx context.Context, bus *events.Bus, buffer int) error {
	if bus == nil {
		return nil
	}
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	sub := bus.Subscribe(ctx, buffer)
	defer sub.Unsubscribe()
	return h.consume(ctx, sub.Events(), sub.Dropped)
}

// Stream renders every event of ch, which is what a recorded JSONL run or a
// test feeds it, until the channel closes or ctx is done. The semantics of
// the returned error match [Headless.Serve].
func (h *Headless) Stream(ctx context.Context, ch <-chan events.Event) error {
	return h.consume(ctx, ch, nil)
}

// consume is the shared read loop. dropped, when non-nil, is polled after
// every event so that a subscription falling behind is reported.
func (h *Headless) consume(ctx context.Context, ch <-chan events.Event, dropped func() uint64) error {
	if ch == nil {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				if err := ctx.Err(); err != nil {
					return err
				}
				return nil
			}
			if dropped != nil {
				if err := h.reportDrops(ev, dropped()); err != nil {
					return err
				}
			}
			if err := h.Write(ev); err != nil {
				return err
			}
		}
	}
}

// reportDrops renders a notice when the subscription's drop counter grew
// since the last event. The notice is a real [events.Warning] so it survives
// JSON mode as a parseable envelope.
func (h *Headless) reportDrops(ev events.Event, total uint64) error {
	h.mu.Lock()
	seen := h.dropped
	h.dropped = total
	h.mu.Unlock()
	if total <= seen {
		return nil
	}
	n := total - seen
	return h.Write(events.Warning{
		Meta:    events.NewMeta(ev.RunID()),
		Source:  "headless",
		Message: fmt.Sprintf("dropped %s: the renderer fell behind", headlessCount(int(n), "event")),
	})
}

// ExitCode returns the process exit code for the run so far: the mapping of
// the last [events.RunFinished] seen, or [ExitFailed] when the stream ended
// without one.
func (h *Headless) ExitCode() int {
	fin, ok := h.Finish()
	if !ok {
		return ExitFailed
	}
	return ExitCodeFor(fin.Reason)
}

// Finish returns the last [events.RunFinished] seen and whether there was one.
func (h *Headless) Finish() (events.RunFinished, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.finish == nil {
		return events.RunFinished{}, false
	}
	return *h.finish, true
}

// Written returns how many events have been rendered successfully.
func (h *Headless) Written() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// Err returns the sticky write error, if any.
func (h *Headless) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// RunHeadless renders a run without a TUI and returns the process exit code
// along with the first write error. It is the headless twin of [Run]: it
// takes the same [Options] and uses only Bus (preferred), Events, Buffer and
// Output from them; the TUI-only fields are ignored. A cancelled context is
// not an error, exactly as in [Run], but a run whose [events.RunFinished]
// never arrived still exits [ExitFailed].
func RunHeadless(ctx context.Context, opts Options, hopts HeadlessOptions) (int, error) {
	if hopts.Out == nil {
		hopts.Out = opts.Output
	}
	h := NewHeadless(hopts)
	var err error
	switch {
	case opts.Bus != nil:
		err = h.Serve(ctx, opts.Bus, opts.buffer())
	case opts.Events != nil:
		err = h.Stream(ctx, opts.Events)
	}
	if err != nil && ctx.Err() != nil {
		err = nil
	}
	return h.ExitCode(), err
}

// FormatHeadless renders one event as a headless log line: a clock stamp in
// loc (time.Local when nil), a glyph and the text, as in
//
//	12:01:03 ▶ fired engineer(claude/fable) at bd-1a2b "Add retry" [implement, round 1]
//
// The body is put through the package's scrubber before the stamp and the
// glyph are prefixed ([headlessText], P7-18), so the line is exactly one line
// of printable text however hostile the event was: `sc run --headless` writes
// straight to a terminal, and an agent's output line, a warning's message or
// a model's summary can carry an escape sequence that would repaint or clear
// the screen, a carriage return that would erase the stamp, or a newline that
// would break the one-event-per-line contract a script parses.
func FormatHeadless(ev events.Event, loc *time.Location) string {
	if ev == nil {
		return ""
	}
	at := ev.At()
	if loc == nil {
		loc = time.Local
	}
	at = at.In(loc)
	line := headlessText(HeadlessLine(ev))
	return at.Format(headlessTimeFormat) + " " + HeadlessGlyph(ev) + " " + line
}

// HeadlessGlyph returns the one-column marker that prefixes an event's
// headless line. It depends on the event's content as well as its kind: an
// approving verdict and a change request get different glyphs.
//
// Every glyph goes through [PadGlyph] (P7-16). The set below mixes
// East-Asian-ambiguous characters (◆ ◇ ‖ ▶ § ⤴ ⤵ ·) with unambiguous ones
// (✔ ✖ ✓ ✗ + │ →), so on a terminal configured for ambiguous-width 2 the
// text of a line would otherwise start one column further left or right
// depending on which event it reports.
func HeadlessGlyph(ev events.Event) string { return PadGlyph(headlessGlyph(ev)) }

// headlessGlyph is the glyph table itself, before the column padding.
func headlessGlyph(ev events.Event) string {
	switch e := ev.(type) {
	case events.RunStarted:
		return "◆"
	case events.RunPaused:
		return "‖"
	case events.RunResumed:
		return "▸"
	case events.RunStopped:
		return "■"
	case events.RunFinished:
		return "◇"
	case events.PlanProduced:
		return "✎"
	case events.PlanReviewed:
		return "≈"
	case events.BeadCreated:
		return "+"
	case events.BeadUpdated:
		return "·"
	case events.BeadStateChanged:
		return "→"
	case events.GraphChanged:
		return "⇄"
	case events.AgentSpawned:
		return "▶"
	case events.AgentOutput:
		return "│"
	case events.AgentFinished:
		return "✔"
	case events.AgentFailed:
		return "✖"
	case events.RoundStarted:
		return "↻"
	case events.VerdictReceived:
		if e.Verdict.Outcome == domain.VerdictApprove {
			return "✓"
		}
		return "✗"
	case events.ResponseReceived:
		return "↩"
	case events.ArbitrationReceived:
		return "§"
	case events.PROpened:
		return "⤴"
	case events.Merged:
		return "⤵"
	case events.Warning:
		return "!"
	case events.Error:
		return "✖"
	default:
		return "·"
	}
}

// HeadlessLine returns the human-readable body of an event's headless line,
// without the clock stamp and glyph [FormatHeadless] adds. It is richer than
// [FormatEvent], which renders the TUI's own log view: it keeps the fields a
// terminal-only reader has nothing else to fall back on, such as the agent's
// role and bead, the severities behind a verdict and the exit code a finish
// reason maps to.
func HeadlessLine(ev events.Event) string {
	switch e := ev.(type) {
	case events.RunStarted:
		return headlessRunStarted(e)
	case events.RunPaused:
		return "run paused " + parens(e.Reason)
	case events.RunResumed:
		return strings.TrimSpace("run resumed " + parens(e.Reason))
	case events.RunStopped:
		return strings.TrimSpace("run stopping " + parens(e.Reason))
	case events.RunFinished:
		return headlessRunFinished(e)
	case events.PlanProduced:
		return headlessPlan(e)
	case events.PlanReviewed:
		return strings.TrimSpace(fmt.Sprintf("plan review %s: %s %s %s", e.Planner, e.Candidate, e.Decision, e.Canonical))
	case events.BeadCreated:
		return fmt.Sprintf("bead %s created %q (%s, p%d, %s)",
			e.Bead.ID, e.Bead.Title, headlessBeadType(e.Bead.Type), int(e.Bead.Priority), orDash(string(e.Bead.State)))
	case events.BeadUpdated:
		return headlessBeadUpdated(e)
	case events.BeadStateChanged:
		return headlessBeadState(e)
	case events.GraphChanged:
		return headlessGraph(e)
	case events.AgentSpawned:
		return headlessSpawned(e)
	case events.AgentOutput:
		return fmt.Sprintf("%s %s: %s", e.Agent, e.Stream, e.Line)
	case events.AgentFinished:
		return headlessFinished(e)
	case events.AgentFailed:
		return headlessFailed(e)
	case events.RoundStarted:
		return headlessRound(e)
	case events.VerdictReceived:
		return headlessVerdict(e)
	case events.ResponseReceived:
		return headlessResponse(e)
	case events.ArbitrationReceived:
		return headlessArbitration(e)
	case events.PROpened:
		return headlessPR(e)
	case events.Merged:
		return headlessMerged(e)
	case events.Warning:
		return headlessNotice("warning", e.Source, e.Message, e.Err, e.Bead, e.Agent)
	case events.Error:
		return headlessNotice("error", e.Source, e.Message, e.Err, e.Bead, e.Agent)
	default:
		return string(ev.Kind())
	}
}

// headlessRunStarted renders the opening line of a run: how it was started
// and the settings it is frozen with.
func headlessRunStarted(e events.RunStarted) string {
	verb := "started"
	if e.Resumed {
		verb = "resumed"
	}
	cfg := e.Info.Config
	fields := []string{
		"mode " + orDash(string(e.Info.Mode)),
		fmt.Sprintf("ammo %d", cfg.Ammo),
		"safety " + onOff(cfg.Safety),
		"cartridge " + headlessCartridge(cfg),
	}
	if e.Info.RepoPath != "" {
		fields = append(fields, "repo "+e.Info.RepoPath)
	}
	if e.Info.Brief != "" {
		fields = append(fields, fmt.Sprintf("brief %q", e.Info.Brief))
	}
	return fmt.Sprintf("run %s %s: %s", e.Info.ID, verb, strings.Join(fields, ", "))
}

// headlessRunFinished renders the closing line, including the exit code the
// finish reason maps to (P7-11).
func headlessRunFinished(e events.RunFinished) string {
	line := fmt.Sprintf("run finished: %s", orDash(string(e.Reason)))
	if e.Duration > 0 {
		line += " after " + dur(e.Duration)
	}
	line += fmt.Sprintf(" (exit %d)", ExitCodeFor(e.Reason))
	if s := headlessFold(e.Summary); s != "" {
		line += " — " + s
	}
	return line
}

// headlessFold folds a multi-line report into one sentence (P7-25). A
// summary is written by whoever produced it, and some of them are little
// reports rather than sentences: gitx.CleanupResult names every worktree it
// kept on an indented line of its own, so the newline→space swap
// [headlessText] does to keep one event on one line used to leave the
// report's own indentation in the middle of the sentence ("kept 1:   sc-2
// (in_progress)"). Collapsing the whole run of whitespace is what `sc
// status`'s header already does with the same text (statusRowLines, P8-05),
// so the two read the same. Text that is already one sentence comes back
// unchanged.
func headlessFold(s string) string {
	if !strings.ContainsAny(s, "\n\r\t") && !strings.Contains(s, "  ") {
		return strings.TrimSpace(s)
	}
	return strings.Join(strings.Fields(s), " ")
}

// headlessPlan renders a produced plan with the plan-key to bead-id mapping,
// sorted so the line is stable.
func headlessPlan(e events.PlanProduced) string {
	line := fmt.Sprintf("plan from %s: %s", e.Agent, headlessCount(len(e.Plan.Beads), "bead"))
	if e.Epic != "" {
		line += " under epic " + string(e.Epic)
	}
	if len(e.Created) > 0 {
		keys := make([]string, 0, len(e.Created))
		for k := range e.Created {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pairs := make([]string, len(keys))
		for i, k := range keys {
			pairs[i] = k + "=" + string(e.Created[k])
		}
		line += " [" + strings.Join(pairs, " ") + "]"
	}
	return line
}

// headlessBeadUpdated names the fields that changed and the bead's state now.
func headlessBeadUpdated(e events.BeadUpdated) string {
	line := fmt.Sprintf("bead %s updated", e.Bead.ID)
	if len(e.Fields) > 0 {
		line += ": " + strings.Join(e.Fields, ", ")
	}
	now := []string{"now " + orDash(string(e.Bead.State))}
	if e.Bead.Round > 0 {
		now = append(now, fmt.Sprintf("round %d", e.Bead.Round))
	}
	return line + " (" + strings.Join(now, ", ") + ")"
}

// headlessBeadState renders a workflow transition (D-07).
func headlessBeadState(e events.BeadStateChanged) string {
	line := fmt.Sprintf("bead %s %s → %s", e.Bead, orDash(string(e.From)), orDash(string(e.To)))
	var extra []string
	if e.Round > 0 {
		extra = append(extra, fmt.Sprintf("round %d", e.Round))
	}
	if e.Reason != "" {
		extra = append(extra, e.Reason)
	}
	if len(extra) == 0 {
		return line
	}
	return line + " (" + strings.Join(extra, ", ") + ")"
}

// headlessGraph summarises a graph snapshot: how many beads and how many
// dependency edges they carry.
func headlessGraph(e events.GraphChanged) string {
	edges := 0
	for _, b := range e.Beads {
		edges += len(b.Deps)
	}
	line := "graph updated"
	if e.Epic != "" {
		line += " under epic " + string(e.Epic)
	}
	return fmt.Sprintf("%s: %s, %s", line, headlessCount(len(e.Beads), "bead"), headlessCount(edges, "edge"))
}

// headlessSpawned renders the shot: who was fired, at which bead, for what
// task, in which round, on which attempt and under which agent id, so the
// later lines of that agent can be tied back to it (P7-11).
func headlessSpawned(e events.AgentSpawned) string {
	who := fmt.Sprintf("%s(%s)", roleOf(e.Agent), cartridgeOf(e.Agent))
	line := "fired " + who
	if e.Agent.Bead != "" {
		line += " at " + target(e.Agent.Bead, e.BeadTitle)
	}
	detail := []string{orDash(string(e.Task))}
	if e.Round > 0 {
		detail = append(detail, fmt.Sprintf("round %d", e.Round))
	}
	if e.Attempt > 1 {
		detail = append(detail, fmt.Sprintf("attempt %d", e.Attempt))
	}
	if e.Agent.ID != "" {
		detail = append(detail, "agent "+string(e.Agent.ID))
	}
	return line + " [" + strings.Join(detail, ", ") + "]"
}

// headlessFinished renders a clean agent exit.
func headlessFinished(e events.AgentFinished) string {
	return fmt.Sprintf("%s finished in %s: %s, exit %d",
		headlessWho(e.Agent, e.Role, e.Bead), dur(e.Duration), orDash(string(e.Produced)), e.ExitCode)
}

// headlessFailed renders an agent failure with everything the supervisor
// decided from (P6-07).
func headlessFailed(e events.AgentFailed) string {
	line := fmt.Sprintf("%s failed: %s", headlessWho(e.Agent, e.Role, e.Bead), orDash(string(e.Failure)))
	var extra []string
	if e.Duration > 0 {
		extra = append(extra, "after "+dur(e.Duration))
	}
	if e.ExitCode != 0 {
		extra = append(extra, fmt.Sprintf("exit %d", e.ExitCode))
	}
	if e.Attempt > 0 {
		extra = append(extra, fmt.Sprintf("attempt %d", e.Attempt))
	}
	if e.WillRetry {
		extra = append(extra, "retrying")
	} else {
		extra = append(extra, "no retry")
	}
	line += " (" + strings.Join(extra, ", ") + ")"
	if e.Err != "" {
		line += ": " + e.Err
	}
	return line
}

// headlessRound renders the start of a review round (D-09).
func headlessRound(e events.RoundStarted) string {
	line := fmt.Sprintf("%s round %d (%s): lenses %s", e.Bead, e.Round, orDash(string(e.RoundKind)), lensList(e.Lenses))
	if e.Commit != "" {
		line += " at " + shortSHA(e.Commit)
	}
	return line
}

// headlessVerdict renders a tester's verdict with a severity breakdown.
func headlessVerdict(e events.VerdictReceived) string {
	line := fmt.Sprintf("%s round %d: %s (%s) says %s",
		e.Bead, e.Round, e.Agent, orDash(string(e.Verdict.Lens)), orDash(string(e.Verdict.Outcome)))
	if n := len(e.Verdict.Items); n > 0 {
		line += fmt.Sprintf(" (%s: %s)", headlessCount(n, "item"), headlessSeverities(e.Verdict.Items))
	}
	if s := headlessFold(e.Verdict.Summary); s != "" {
		line += " — " + s
	}
	return line
}

// headlessSeverities counts feedback items by severity, in the domain's
// declaration order (blocker first) so the line is stable.
func headlessSeverities(items []domain.FeedbackItem) string {
	counts := make(map[domain.Severity]int, len(items))
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	seen := 0
	for _, s := range domain.Severities() {
		if n := counts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
			seen += n
		}
	}
	if seen < len(items) {
		parts = append(parts, fmt.Sprintf("%d unclassified", len(items)-seen))
	}
	return strings.Join(parts, ", ")
}

// headlessResponse renders the engineer's answer to a round: the responses,
// the implementation report, or both.
func headlessResponse(e events.ResponseReceived) string {
	line := fmt.Sprintf("%s round %d: %s", e.Bead, e.Round, e.Agent)
	var parts []string
	if len(e.Responses) > 0 {
		parts = append(parts, fmt.Sprintf("answered %s (%s)", headlessCount(len(e.Responses), "item"), headlessActions(e.Responses)))
	}
	if e.Report != nil {
		report := fmt.Sprintf("reported %q", e.Report.Summary)
		var detail []string
		if n := len(e.Report.FilesChanged); n > 0 {
			detail = append(detail, headlessCount(n, "file"))
		}
		if e.Report.Commit != "" {
			detail = append(detail, "commit "+shortSHA(e.Report.Commit))
		}
		if len(detail) > 0 {
			report += " (" + strings.Join(detail, ", ") + ")"
		}
		parts = append(parts, report)
	}
	if len(parts) == 0 {
		parts = append(parts, "answered nothing")
	}
	return line + " " + strings.Join(parts, "; ")
}

// headlessActions counts responses by action, in the domain's declaration
// order so the line is stable.
func headlessActions(rs []domain.Response) string {
	counts := make(map[domain.ResponseAction]int, len(rs))
	for _, r := range rs {
		counts[r.Action]++
	}
	var parts []string
	seen := 0
	for _, a := range domain.ResponseActions() {
		if n := counts[a]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, a))
			seen += n
		}
	}
	if seen < len(rs) {
		parts = append(parts, fmt.Sprintf("%d unclassified", len(rs)-seen))
	}
	return strings.Join(parts, ", ")
}

// headlessArbitration renders the PO's ruling on held items, its escalation
// after the round cap, or both (D-08, D-25).
func headlessArbitration(e events.ArbitrationReceived) string {
	line := fmt.Sprintf("%s round %d: PO %s", e.Bead, e.Round, e.Agent)
	var parts []string
	if len(e.Rulings) > 0 {
		counts := make(map[domain.ArbitrationRuling]int, len(e.Rulings))
		order := make([]domain.ArbitrationRuling, 0, len(e.Rulings))
		for _, r := range e.Rulings {
			if counts[r.Ruling] == 0 {
				order = append(order, r.Ruling)
			}
			counts[r.Ruling]++
		}
		sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
		named := make([]string, len(order))
		for i, r := range order {
			named[i] = fmt.Sprintf("%d %s", counts[r], orDash(string(r)))
		}
		parts = append(parts, fmt.Sprintf("ruled on %s (%s)", headlessCount(len(e.Rulings), "item"), strings.Join(named, ", ")))
	}
	if e.Escalation != nil {
		esc := "escalation " + orDash(string(e.Escalation.Outcome))
		if s := headlessFold(e.Escalation.Summary); s != "" {
			esc += " — " + s
		}
		parts = append(parts, esc)
	}
	if len(parts) == 0 {
		parts = append(parts, "ruled on nothing")
	}
	return line + " " + strings.Join(parts, "; ")
}

// headlessPR renders an opened pull request, ours or an external one (D-11).
func headlessPR(e events.PROpened) string {
	kind := "PR"
	if e.External {
		kind = "external PR"
	}
	line := fmt.Sprintf("%s %s #%d opened: %s", e.Bead, kind, e.Number, orDash(e.URL))
	if e.Branch != "" || e.Base != "" {
		line += fmt.Sprintf(" (%s → %s)", orDash(e.Branch), orDash(e.Base))
	}
	return line
}

// headlessMerged renders code landing on the default branch (D-20).
func headlessMerged(e events.Merged) string {
	line := fmt.Sprintf("%s merged %s into %s", e.Bead, shortSHA(e.SHA), orDash(e.Branch))
	var extra []string
	if e.PRNumber > 0 {
		extra = append(extra, fmt.Sprintf("PR #%d", e.PRNumber))
	}
	if e.Pushed {
		extra = append(extra, "pushed")
	}
	if len(extra) == 0 {
		return line
	}
	return line + " (" + strings.Join(extra, ", ") + ")"
}

// headlessNotice renders a warning or an error, which share their shape.
func headlessNotice(kind, source, message, errText string, bead domain.BeadID, agent domain.AgentID) string {
	line := fmt.Sprintf("%s [%s] %s", kind, orDash(source), message)
	var extra []string
	if bead != "" {
		extra = append(extra, "bead "+string(bead))
	}
	if agent != "" {
		extra = append(extra, "agent "+string(agent))
	}
	if len(extra) > 0 {
		line += " (" + strings.Join(extra, ", ") + ")"
	}
	if errText != "" {
		line += ": " + errText
	}
	return line
}

// headlessWho names an agent with its role and bead: `eng-1 (engineer,
// bd-2)`, or `po-1 (po)` for an agent with no bead.
func headlessWho(id domain.AgentID, role domain.Role, bead domain.BeadID) string {
	who := orDash(role.String())
	if bead != "" {
		who += ", " + string(bead)
	}
	return fmt.Sprintf("%s (%s)", id, who)
}

// headlessCartridge renders a run's cartridge as "name/model".
func headlessCartridge(cfg domain.ConfigSnapshot) string {
	if cfg.Model == "" {
		return orDash(cfg.Cartridge)
	}
	return cfg.Cartridge + "/" + cfg.Model
}

// headlessBeadType renders a bead type, defaulting the empty one to task as
// bd does.
func headlessBeadType(t domain.BeadType) string {
	if t == "" {
		return domain.BeadTypeTask.String()
	}
	return t.String()
}

// headlessCount renders "1 bead" / "2 beads", pluralising by adding an s.
func headlessCount(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
