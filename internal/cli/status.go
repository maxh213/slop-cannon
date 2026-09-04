package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/orchestrator"
	"slop-cannon/internal/proc"
	"slop-cannon/internal/tui"
)

// `sc status` (P6-08, P6-15). Two reports live here:
//
//   - `sc status` folds the run log at <run-dir>/events.jsonl together with
//     <run-dir>/state.json into the state of a run: phase, per-state bead
//     counts, live agents and the last events. It needs neither bd nor git
//     nor the process that wrote them, so it works on a run that crashed —
//     and says so, by asking the run lock whether anybody is still running
//     it ([orchestrator.ProbeLiveness]) rather than believing a `running`
//     that nothing got the chance to overwrite.
//   - `sc status <bead-id>` asks bd for the bead and prints
//     [beads.FormatTimeline] over its structured messages (P6-15), rather
//     than re-deriving a history the messages already record (D-06).
//
// Every line either report prints is scrubbed before it reaches the
// terminal ([statusScrub], [statusScrubBlock]): a run log quotes what a
// cartridge and bd printed and a bead timeline quotes what an agent wrote
// into a bd comment — a bead title the product owner invented, the prose of
// a verdict — and none of that text may be able to repaint the user's
// screen. Nothing upstream validates control characters in what a model
// produced, so the guarantee is made here, at the one place it is printed.

// Flag names of `sc status`.
const (
	FlagStatusRun    = "run"
	FlagStatusEvents = "events"
)

// statusLabelWidth is the column the header values start in.
const statusLabelWidth = 11

// statusTimeFormat is how absolute times are printed: UTC, to the second.
const statusTimeFormat = "2006-01-02 15:04:05Z"

// statusBeadsDirName is the beads database directory inside a repository.
const statusBeadsDirName = ".beads"

func init() { register(newStatusCommand) }

const statusLong = `Report what a run did and is doing.

Without arguments it describes the most recent run recorded for the
repository (--run ID picks another): its phase, how its beads are spread
across the workflow states, which agents are live, and the last few events.
Everything comes from the run directory — state.json and events.jsonl — so
it also works after a crash and while another process holds the run.

With a bead id it prints that bead's timeline from beads: creation, the
claim, every structured message sc recorded (verdicts, responses,
arbitrations, deliveries) and where the bead stands now. This form needs bd
on PATH and a beads database in the repository.`

const statusExamples = `sc status
sc status --repo ./repo-path
sc status --run 20260902-010203-abcd --events 25
sc status sc-1a2b
sc status --json`

// newStatusCommand builds `sc status [BEAD-ID]`.
func newStatusCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "status [BEAD-ID]",
		Short:   "Show the state of a run, or one bead's timeline",
		Long:    statusLong,
		Example: indent(statusExamples, "  "),
		Args:    cobra.MaximumNArgs(1),
		RunE:    runStatus,
	}
	f := cmd.Flags()
	f.String(FlagStatusRun, "", "report on run `ID` instead of the most recent one")
	f.Int(FlagStatusEvents, orchestrator.DefaultRecentEvents, "how many recent events to print (`N`, 0 for none)")
	return cmd
}

// StatusTimelineEntry is one line of a bead's history in --json form; the
// text form is [beads.FormatTimeline].
type StatusTimelineEntry struct {
	// At is when it happened (UTC).
	At time.Time `json:"at"`
	// Actor is the bd actor responsible, when known.
	Actor string `json:"actor,omitempty"`
	// Event is the entry kind ("created", "claimed", "message", "closed",
	// "status").
	Event string `json:"event"`
	// Summary is the one-line description.
	Summary string `json:"summary"`
}

// StatusBeadReport is the --json shape of `sc status <bead-id>`.
type StatusBeadReport struct {
	// Repo is the repository the bead was read from.
	Repo string `json:"repo"`
	// Bead is the bd issue, including its comments.
	Bead beads.Issue `json:"bead"`
	// Timeline is the bead's history in time order.
	Timeline []StatusTimelineEntry `json:"timeline,omitempty"`
}

// StatusReport is the --json shape of `sc status` and the value
// [RunStatus] returns.
type StatusReport struct {
	// Repo is the repository root the run belongs to.
	Repo string `json:"repo"`
	// Run is the run reported on; empty when none was found.
	Run domain.RunID `json:"run,omitempty"`
	// Dir is its run directory (D-23).
	Dir string `json:"run_dir,omitempty"`
	// Found is false when the repository has no runs at all.
	Found bool `json:"found"`
	// State is <run-dir>/state.json, absent for a run that died before its
	// first lifecycle transition.
	State *orchestrator.State `json:"state,omitempty"`
	// Summary is the fold of <run-dir>/events.jsonl, absent when there is
	// no run log.
	Summary *orchestrator.RunSummary `json:"summary,omitempty"`
	// Liveness is what the repository's run lock says about this run:
	// whether a process is still running it. A run directory alone cannot
	// tell a live run from a killed one — both say "running".
	Liveness orchestrator.Liveness `json:"liveness"`
	// Recent are the last events as one-line text, oldest first
	// (tui.FormatHeadless, scrubbed); events are an interface, so JSON gets
	// the rendered lines rather than half-decoded objects.
	Recent []string `json:"recent_events,omitempty"`
	// Notes explain anything missing or unreadable.
	Notes []string `json:"notes,omitempty"`
}

// Phase returns the run's lifecycle phase: state.json's when it exists,
// otherwise the one the run log implies.
func (r *StatusReport) Phase() orchestrator.Phase {
	switch {
	case r.State != nil && r.State.Phase != "":
		return r.State.Phase
	case r.Summary != nil:
		return r.Summary.Phase
	default:
		return orchestrator.PhaseNew
	}
}

// Running reports whether a live process still holds this run's lock.
func (r *StatusReport) Running() bool { return r.Liveness.Running() }

// Crashed reports a run whose last recorded phase is still an active one
// but which nothing is running any more: it was killed, its machine went
// away, or it died without writing a finish. Nothing in the run directory
// can say this on its own, which is why the lock is probed.
func (r *StatusReport) Crashed() bool {
	return r.Found && r.Phase().Live() && !r.Running()
}

// statusBeadReader is the slice of the beads client `sc status <bead-id>`
// needs; tests substitute one so the golden runs without bd.
type statusBeadReader interface {
	Show(ctx context.Context, ids ...domain.BeadID) ([]beads.Issue, error)
}

// statusBeads builds the bd client for a repository. It is a variable so
// tests can inject a reader.
var statusBeads = func(repo string) (statusBeadReader, error) {
	return beads.New(repo, beads.Options{ReadOnly: true}), nil
}

// runStatus is the RunE of `sc status`.
func runStatus(cmd *cobra.Command, args []string) error {
	opts, err := GlobalOpts(cmd)
	if err != nil {
		return err
	}
	repo, err := runRepoRoot(opts.Repo)
	if err != nil {
		return err
	}
	if len(args) == 1 {
		return statusBead(cmd, repo, domain.BeadID(args[0]), opts.JSON)
	}

	runID, err := cmd.Flags().GetString(FlagStatusRun)
	if err != nil {
		return err
	}
	recent, err := cmd.Flags().GetInt(FlagStatusEvents)
	if err != nil {
		return err
	}
	if recent <= 0 {
		// Summarise reads 0 as "the default"; --events 0 means none.
		recent = -1
	}
	report, err := RunStatus(repo, domain.RunID(runID), recent)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if opts.JSON {
		return writeJSON(out, report)
	}
	return WriteStatus(out, report)
}

// RunStatus collects the status of one run of repo. An empty runID means
// the most recent run (config.LatestRunID). A repository with no runs
// yields a report with Found false and no error; a named run that does not
// exist is an error.
//
// recent is how many events to keep, following [orchestrator.Summarise]:
// zero means [orchestrator.DefaultRecentEvents] and a negative number keeps
// none. Neither a missing state.json nor a missing run log is an error —
// both become notes, because a run that crashed early has one and not the
// other, and reporting what there is beats refusing to report at all.
func RunStatus(repo string, runID domain.RunID, recent int) (*StatusReport, error) {
	rep := &StatusReport{Repo: repo, Run: runID}
	if runID == "" {
		latest, found, err := config.LatestRunID(repo)
		if err != nil {
			return nil, err
		}
		if !found {
			return rep, nil
		}
		rep.Run = latest
	}
	rep.Dir = config.RunDir(repo, string(rep.Run))
	if info, err := os.Stat(rep.Dir); err != nil || !info.IsDir() {
		if runID != "" {
			return nil, fmt.Errorf("run %s: no run directory at %s", rep.Run, rep.Dir)
		}
		return rep, nil
	}
	rep.Found = true
	rep.Liveness = orchestrator.ProbeLiveness(rep.Dir, rep.Run)
	if rep.Liveness.Held && !rep.Liveness.Mine {
		rep.Notes = append(rep.Notes, fmt.Sprintf("the run lock is held by run %s (pid %d)",
			rep.Liveness.Holder.Run, rep.Liveness.Holder.PID))
	}

	switch st, err := orchestrator.LoadState(rep.Dir); {
	case err == nil:
		rep.State = &st
	case errors.Is(err, orchestrator.ErrNoState):
		rep.Notes = append(rep.Notes, "no "+orchestrator.StateFileName+
			" yet: the run did not reach its first lifecycle transition")
	default:
		rep.Notes = append(rep.Notes, err.Error())
	}

	summary, err := orchestrator.SummariseLog(rep.Dir, recent)
	switch {
	case err == nil:
		rep.Summary = &summary
	case errors.Is(err, orchestrator.ErrNoEventLog):
		rep.Notes = append(rep.Notes, "no "+orchestrator.EventLogName+" in "+rep.Dir)
	default:
		// Only a stream that cannot be read on gets here — an I/O failure,
		// or a line past the reader's size limit. A line that does not
		// parse is skipped by the fold and reported in Summary.Notes
		// below, so a damaged log still describes the run as it stands.
		if summary.Events > 0 {
			rep.Summary = &summary
		}
		rep.Notes = append(rep.Notes, err.Error())
	}
	if rep.Summary != nil {
		rep.Recent = make([]string, 0, len(rep.Summary.Recent))
		for _, ev := range rep.Summary.Recent {
			rep.Recent = append(rep.Recent, statusScrub(tui.FormatHeadless(ev, time.UTC)))
		}
		// A damaged record is a note, not a missing report.
		rep.Notes = append(rep.Notes, rep.Summary.Notes...)
	}
	return rep, nil
}

// WriteStatus prints a run report as text: an aligned header, the per-state
// bead counts, the live agents and the last events.
func WriteStatus(w io.Writer, r *StatusReport) error {
	if !r.Found {
		_, err := fmt.Fprintf(w, "no runs recorded for %s\n", r.Repo)
		return err
	}
	var b strings.Builder
	statusHeader(&b, r)
	statusBeadsSection(&b, r)
	statusAgentsSection(&b, r)
	statusEventsSection(&b, r)
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "\nnote: %s\n", statusScrub(n))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// statusHeader writes the label/value block that opens the report.
func statusHeader(b *strings.Builder, r *StatusReport) {
	row := func(label, value string) {
		if value == "" {
			return
		}
		// Values quote what a run recorded, which quotes what a cartridge
		// and bd printed: the brief, a stop reason, a finish summary.
		fmt.Fprintf(b, "%-*s%s\n", statusLabelWidth, label, statusScrub(value))
	}
	row("run", string(r.Run))
	row("phase", statusPhaseText(r))
	row("process", statusProcessText(r))
	if st := r.State; st != nil {
		row("mode", st.Run.Mode.String())
		row("brief", st.Run.Brief)
		row("epic", string(st.Run.Epic))
	} else if r.Summary != nil {
		row("epic", string(r.Summary.Epic))
	}
	row("repo", r.Repo)
	row("run dir", r.Dir)
	row("started", statusTime(statusStartedAt(r)))
	if r.Summary != nil {
		row("last event", statusTime(r.Summary.LastAt))
	}
	if st := r.State; st != nil {
		row("finished", statusTime(st.FinishedAt))
		row("outcome", statusOutcome(st))
		row("stopped by", st.StopReason)
		if st.Resumes > 0 {
			row("resumes", fmt.Sprint(st.Resumes))
		}
		snap := st.Run.Config
		if snap.Cartridge != "" {
			row("cartridge", fmt.Sprintf("%s (%s)", snap.Cartridge, cartridgeModelText(snap.Model)))
		}
		if alloc, err := config.Allocate(snap.Ammo); err == nil {
			row("ammo", strings.TrimPrefix(alloc.String(), "ammo "))
		}
		row("safety", fmt.Sprintf("%s (%s)", safetyWord(snap.Safety), statusSafetyGloss(snap.Safety)))
	}
}

// statusSafetyGloss is the short form of the D-10 meaning; the long one
// (SafetyMeaning) belongs to `sc safety`, not to a status header.
func statusSafetyGloss(on bool) string {
	if on {
		return "pull requests"
	}
	return "merge to the default branch"
}

// statusPhaseText renders the phase, adding the finish reason once the run
// has ended and marking a phase no live process backs any more.
func statusPhaseText(r *StatusReport) string {
	phase := r.Phase().String()
	if reason := statusFinishReason(r); reason != "" {
		return phase + " (" + reason.String() + ")"
	}
	if r.Crashed() {
		return phase + " (no live process: the run was killed or crashed)"
	}
	return phase
}

// statusProcessText describes the process behind the run, or "" when there
// is nothing worth saying: a finished run releases its lock, and that is
// not news. An active phase always gets a line, because "who is running
// this" is the first thing a reader wants after a crash.
func statusProcessText(r *StatusReport) string {
	l := r.Liveness
	switch {
	case r.Running():
		who := fmt.Sprintf("pid %d", l.Holder.PID)
		if l.Holder.Host != "" {
			who += " on " + l.Holder.Host
		}
		return who + " holds the run lock"
	case !r.Phase().Live():
		return "" // a run that ended owes no explanation for releasing it
	case l.Held && !l.Mine:
		return fmt.Sprintf("none; the run lock is held by run %s (pid %d)", l.Holder.Run, l.Holder.PID)
	case l.Held:
		return fmt.Sprintf("none; pid %d took the run lock and is gone", l.Holder.PID)
	default:
		return "none; nothing holds the run lock"
	}
}

// statusFinishReason returns the run's finish reason from whichever source
// recorded one.
func statusFinishReason(r *StatusReport) events.FinishReason {
	if r.State != nil && r.State.Finish != "" {
		return r.State.Finish
	}
	if r.Summary != nil {
		return r.Summary.Finish
	}
	return ""
}

// statusOutcome renders the finish summary, which carries what cleanup kept.
func statusOutcome(st *orchestrator.State) string {
	parts := make([]string, 0, 2)
	if st.FinishSummary != "" {
		parts = append(parts, st.FinishSummary)
	}
	if st.FinishError != "" {
		parts = append(parts, "error: "+st.FinishError)
	}
	return strings.Join(parts, "; ")
}

// statusStartedAt is when the run began, from state.json or the run log.
func statusStartedAt(r *StatusReport) time.Time {
	if r.State != nil && !r.State.Run.StartedAt.IsZero() {
		return r.State.Run.StartedAt
	}
	if r.Summary != nil {
		return r.Summary.StartedAt
	}
	return time.Time{}
}

// statusBeadsSection writes the per-state bead counts in D-07 flow order.
func statusBeadsSection(b *strings.Builder, r *StatusReport) {
	if r.Summary == nil {
		return
	}
	s := r.Summary
	fmt.Fprintf(b, "\n%-*s%d\n", statusLabelWidth, "beads", len(s.Beads))
	if len(s.Beads) == 0 {
		return
	}
	// A state is bd's word for it, and bd's words include whatever a
	// repository (or an agent talking to bd) invented, so they are scrubbed
	// like everything else and measured after scrubbing.
	order := statusStateOrder(s.Counts)
	names := make([]string, len(order))
	width := 0
	for i, state := range order {
		names[i] = statusScrub(string(state))
		if n := len(names[i]); n > width {
			width = n
		}
	}
	for i, state := range order {
		fmt.Fprintf(b, "  %-*s  %d\n", width, names[i], s.Counts[state])
	}
}

// statusStateOrder lists the states present in counts in D-07 flow order,
// with anything unrecognised (a repository's own status) sorted after.
func statusStateOrder(counts map[domain.BeadState]int) []domain.BeadState {
	known := domain.BeadStates()
	rank := make(map[domain.BeadState]int, len(known))
	for i, s := range known {
		rank[s] = i
	}
	out := make([]domain.BeadState, 0, len(counts))
	for s := range counts {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, oki := rank[out[i]]
		rj, okj := rank[out[j]]
		switch {
		case oki && okj:
			return ri < rj
		case oki != okj:
			return oki
		default:
			return out[i] < out[j]
		}
	})
	return out
}

// statusAgentsSection writes the agents that were live at the last event:
// the run log's un-finished spawns, or state.json's record of them when
// there is no run log to fold. A run nothing is running any more has no
// live agents at all, however many the log ends on, so those are reported
// as the last ones seen rather than as processes the reader could go and
// look at.
func statusAgentsSection(b *strings.Builder, r *StatusReport) {
	var live []domain.Agent
	switch {
	case r.Summary != nil:
		live = r.Summary.Agents
	case r.State != nil:
		live = r.State.Agents
	default:
		return
	}
	word := "live"
	if r.Crashed() {
		word = "last seen"
	}
	head := fmt.Sprintf("%d %s", len(live), word)
	if r.State != nil && r.State.Run.Config.Ammo > 0 {
		head += fmt.Sprintf(" of %d ammo", r.State.Run.Config.Ammo)
	}
	fmt.Fprintf(b, "\n%-*s%s\n", statusLabelWidth, "agents", head)
	idW, roleW := 0, 0
	rows := make([][3]string, 0, len(live))
	for _, a := range live {
		role := a.Role.String()
		if a.Lens != "" {
			role += "/" + a.Lens.String()
		}
		// An agent id carries a bd bead id and a role carries a configured
		// lens name, so both quote text sc did not write: scrub before
		// measuring, or an escape would also break the column widths.
		id, role := statusScrub(string(a.ID)), statusScrub(role)
		rows = append(rows, [3]string{id, role, statusAgentDetail(a)})
		if n := len(id); n > idW {
			idW = n
		}
		if n := len(role); n > roleW {
			roleW = n
		}
	}
	for _, row := range rows {
		fmt.Fprintf(b, "  %-*s  %-*s  %s\n", idW, row[0], roleW, row[1], row[2])
	}
}

// statusAgentDetail renders the bead, state and pid of one live agent. The
// bead id is bd's, so it is scrubbed like every other borrowed string.
func statusAgentDetail(a domain.Agent) string {
	parts := make([]string, 0, 3)
	if a.Bead != "" {
		parts = append(parts, statusScrub(string(a.Bead)))
	}
	if a.State != "" {
		parts = append(parts, statusScrub(a.State.String()))
	}
	if a.PID > 0 {
		parts = append(parts, fmt.Sprintf("pid %d", a.PID))
	}
	return strings.Join(parts, "  ")
}

// statusEventsSection writes the last events, newest last.
func statusEventsSection(b *strings.Builder, r *StatusReport) {
	if r.Summary == nil {
		return
	}
	s := r.Summary
	head := fmt.Sprintf("%d of %d", len(r.Recent), s.Events)
	if extra := statusNotices(s); extra != "" {
		head += " (" + extra + ")"
	}
	fmt.Fprintf(b, "\n%-*s%s\n", statusLabelWidth, "events", head)
	for _, line := range r.Recent {
		fmt.Fprintf(b, "  %s\n", line)
	}
}

// statusNotices summarises the counts a reader wants next to the event tail.
func statusNotices(s *orchestrator.RunSummary) string {
	var parts []string
	add := func(n int, one, many string) {
		switch {
		case n == 1:
			parts = append(parts, "1 "+one)
		case n > 1:
			parts = append(parts, fmt.Sprintf("%d %s", n, many))
		}
	}
	add(s.Output, "output line", "output lines")
	add(s.Warnings, "warning", "warnings")
	add(s.Errors, "error", "errors")
	return strings.Join(parts, ", ")
}

// statusTime renders an absolute time in UTC, or nothing for the zero time.
func statusTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(statusTimeFormat)
}

// statusScrub makes text from a run log safe to print. Event lines quote
// what a cartridge and bd wrote — a Warning's message, an Error's err, an
// agent's own output — and `sc status` writes them straight to a terminal,
// where an ANSI escape would repaint the screen, a carriage return would
// erase the line already printed and a backspace would rewrite it. Escape
// sequences are removed whole, a tab becomes a space, and every other
// control character (C0, DEL and C1) is dropped. Text that carries none of
// them is returned unchanged, so the common line pays only one scan.
//
// tui.FormatHeadless promises a line free of newlines but not of escapes
// (P7-18 fixes the renderer itself); this is the same guarantee applied at
// the one place `sc status` prints.
func statusScrub(s string) string {
	if !strings.ContainsFunc(s, statusIsControl) {
		return s
	}
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; {
		case r == 0x1b:
			i += statusEscapeRunes(rs[i:]) - 1
		case r == '\t':
			b.WriteRune(' ')
		case statusIsControl(r):
			// dropped: it is an instruction to the terminal, not text
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// statusScrubBlock scrubs a multi-line report — [beads.FormatTimeline]'s
// output — line by line, keeping the line structure the report laid out and
// making every line as safe as [statusScrub] makes one. A timeline prints a
// bead title and the first line of a message's prose, both written by a
// model into bd, so the same guarantee has to hold there.
func statusScrubBlock(s string) string {
	if !strings.ContainsFunc(s, statusIsControlInBlock) {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = statusScrub(line)
	}
	return strings.Join(lines, "\n")
}

// statusIsControlInBlock is [statusIsControl] minus the newline the block
// itself is built from.
func statusIsControlInBlock(r rune) bool { return r != '\n' && statusIsControl(r) }

// statusIsControl reports whether r is read by a terminal as an instruction
// rather than as text: a C0 control (ESC, CR, LF and tab included), DEL, or
// a C1 control.
func statusIsControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// statusEscapeRunes returns how many runes the escape sequence starting at
// rs[0] (an ESC) occupies: a CSI up to its final byte, an OSC up to its
// terminator, and otherwise the ESC and the one character after it. It is
// never less than 1, so the caller always makes progress.
func statusEscapeRunes(rs []rune) int {
	if len(rs) < 2 {
		return 1
	}
	switch rs[1] {
	case '[': // CSI: parameters and intermediates, then a final byte.
		for i := 2; i < len(rs); i++ {
			if rs[i] >= 0x40 && rs[i] <= 0x7e {
				return i + 1
			}
		}
		return len(rs)
	case ']': // OSC: a string terminated by BEL or ESC \.
		for i := 2; i < len(rs); i++ {
			if rs[i] == 0x07 {
				return i + 1
			}
			if rs[i] == 0x1b && i+1 < len(rs) && rs[i+1] == '\\' {
				return i + 2
			}
		}
		return len(rs)
	default:
		return 2
	}
}

// statusBead prints one bead's timeline from beads (P6-15).
func statusBead(cmd *cobra.Command, repo string, id domain.BeadID, asJSON bool) error {
	if strings.TrimSpace(string(id)) == "" {
		return errors.New("status: empty bead id")
	}
	if _, err := os.Stat(filepath.Join(repo, statusBeadsDirName)); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s has no beads database yet (no %s directory); sc creates one on the first run (D-13)",
			repo, statusBeadsDirName)
	}
	client, err := statusBeads(repo)
	if err != nil {
		return err
	}
	issues, err := client.Show(cmd.Context(), id)
	if err != nil {
		if proc.IsNotFound(err) {
			return fmt.Errorf("%w\n%s", err, beads.InstallHint)
		}
		return err
	}
	if len(issues) == 0 {
		return fmt.Errorf("bead %s not found in %s", id, repo)
	}
	issue := issues[0]
	msgs := beads.ParseMessages(issue.Comments)
	out := cmd.OutOrStdout()
	if asJSON {
		rep := StatusBeadReport{Repo: repo, Bead: issue}
		for _, e := range beads.Timeline(issue, msgs) {
			rep.Timeline = append(rep.Timeline, StatusTimelineEntry{
				At: e.At.UTC(), Actor: e.Actor, Event: e.Event, Summary: e.Summary,
			})
		}
		return writeJSON(out, rep)
	}
	// The timeline quotes a bead title and the prose of every message an
	// agent wrote: scrub it before it reaches the terminal, exactly as the
	// run report scrubs the events it prints.
	_, err = io.WriteString(out, statusScrubBlock(beads.FormatTimeline(issue, msgs)))
	return err
}
