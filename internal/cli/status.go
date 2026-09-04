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
	"slop-cannon/internal/textsafe"
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
	FlagStatusLive   = "live"
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

The bead states are then reconciled with bd, so a bead a human closed or
reopened by hand is reported as it stands rather than as the run last left
it; --live=false skips that and reports the run log alone, which is also
what happens when the repository has no beads database or bd is not
installed.

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
	f.Bool(FlagStatusLive, true, "reconcile the bead states with bd (--live=false reports the run log alone)")
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
	// Live is what the reconciliation against bd found (P8-15), absent
	// when it did not run: --live=false, no beads database, or no bd.
	Live *StatusLive `json:"live,omitempty"`
	// Notes explain anything missing or unreadable.
	Notes []string `json:"notes,omitempty"`
}

// StatusLive is the result of reconciling the folded run log against bd
// (P8-15). The fold only knows what the run published, so a bead a human
// moved with `bd update` — or closed by hand after taking the work over —
// keeps its last sc-published state for ever. Asking bd for the run's beads
// fixes that without giving up the log-only path: `sc status` still works
// with bd absent, on another machine, or against a copied run directory.
type StatusLive struct {
	// Checked is how many beads bd answered for.
	Checked int `json:"checked"`
	// Changed are the beads whose state in bd differs from the state the
	// run log recorded; the report already carries the bd state.
	Changed []StatusLiveChange `json:"changed,omitempty"`
	// Added are beads bd knows for this run that the run log never
	// mentioned — one created by hand, or by something that published no
	// event (P6-32).
	Added []StatusLiveChange `json:"added,omitempty"`
}

// StatusLiveChange is one bead the reconciliation corrected.
type StatusLiveChange struct {
	// Bead is the bead.
	Bead domain.BeadID `json:"bead"`
	// Title is its title as bd holds it.
	Title string `json:"title,omitempty"`
	// From is the state the run log recorded, empty for an added bead.
	From domain.BeadState `json:"from,omitempty"`
	// To is the state bd holds it in now.
	To domain.BeadState `json:"to"`
	// Status is bd's own status name, kept because a repository may use a
	// custom one that maps onto To (P3-14).
	Status string `json:"status,omitempty"`
	// Foreign marks a status the cannon did not register, mapped by its
	// category rather than by name.
	Foreign bool `json:"foreign,omitempty"`
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

// statusLiveReader is the slice of the beads client the P8-15
// reconciliation needs: the run's beads as bd holds them now, and the
// repository's status vocabulary so a custom status is mapped rather than
// misread (P3-14).
type statusLiveReader interface {
	ListRun(ctx context.Context, run domain.RunID, all bool) ([]beads.Issue, error)
	StatusMap(ctx context.Context) (beads.StatusMap, error)
}

// statusLiveBeads builds the read-only bd client the reconciliation uses.
// Read-only is the whole contract of this command: `sc status` may run
// while another sc holds the run lock and is firing, so it must not be able
// to write to the database that run is using. It is a variable so tests can
// inject a reader.
var statusLiveBeads = func(repo string) (statusLiveReader, error) {
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
	live, err := cmd.Flags().GetBool(FlagStatusLive)
	if err != nil {
		return err
	}
	report, err := RunStatus(repo, domain.RunID(runID), recent)
	if err != nil {
		return err
	}
	if live {
		statusReconcile(cmd.Context(), repo, report)
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

// RunStatusLive is [RunStatus] followed by the P8-15 reconciliation
// against bd: the states in the report are then the ones bd holds now, not
// only the ones the run got as far as publishing. It never fails for want
// of bd — a repository with no beads database, a bd that is not installed
// and a bd call that fails all leave the log-only report standing (the last
// with a note) — so a caller can use it wherever it used [RunStatus].
func RunStatusLive(ctx context.Context, repo string, runID domain.RunID, recent int) (*StatusReport, error) {
	rep, err := RunStatus(repo, runID, recent)
	if err != nil {
		return nil, err
	}
	statusReconcile(ctx, repo, rep)
	return rep, nil
}

// statusReconcile corrects the folded bead states against bd (P8-15).
//
// The fold is a record of what the run published, which is the right
// default — it needs nothing but the run directory — but it is not the
// truth about the beads: a human who closes a bead with `bd close`, marks
// one blocked, or reopens one the cannon gave up on changes the database
// and publishes no event, so `sc status` would go on reporting the state
// the run last wrote. Asking bd for the run's beads (`bd list -l
// sc:run:<id> --all`, [beads.Client.ListRun]) reconciles the two.
//
// Nothing here is fatal. No beads database means a run that never got as
// far as creating one; bd missing from PATH means a machine that is only
// reading the run directory; a failing bd is worth a note but not a refusal
// to report. In each case the fold stands as it was, which is what keeps
// the goldens deterministic and the command usable without bd.
func statusReconcile(ctx context.Context, repo string, rep *StatusReport) {
	if rep == nil || !rep.Found || rep.Run == "" || rep.Summary == nil {
		return
	}
	if _, err := os.Stat(filepath.Join(repo, statusBeadsDirName)); err != nil {
		return // no database: the run log is all there is to read
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client, err := statusLiveBeads(repo)
	if err != nil || client == nil {
		if err != nil {
			rep.Notes = append(rep.Notes, "bead states not reconciled with bd: "+err.Error())
		}
		return
	}
	issues, err := client.ListRun(ctx, rep.Run, true)
	if err != nil {
		if !proc.IsNotFound(err) {
			rep.Notes = append(rep.Notes, "bead states not reconciled with bd: "+err.Error())
		}
		return
	}
	// A repository's own statuses are mapped by category, never guessed
	// (P3-14); without the vocabulary the built-ins still resolve.
	statuses, err := client.StatusMap(ctx)
	if err != nil {
		statuses = nil
	}

	live := &StatusLive{}
	byID := make(map[domain.BeadID]beads.Issue, len(issues))
	for _, is := range issues {
		if is.IsEpic() {
			continue // the run's own epic is not one of its beads (D-22)
		}
		byID[is.ID] = is
	}
	seen := make(map[domain.BeadID]bool, len(rep.Summary.Beads))
	for i := range rep.Summary.Beads {
		b := &rep.Summary.Beads[i]
		seen[b.ID] = true
		is, ok := byID[b.ID]
		if !ok {
			continue
		}
		live.Checked++
		bead, origin := is.SafeBead(statuses)
		if b.Title == "" {
			b.Title = bead.Title
		}
		if bead.State == b.State {
			continue
		}
		live.Changed = append(live.Changed, StatusLiveChange{
			Bead: b.ID, Title: bead.Title, From: b.State, To: bead.State,
			Status: is.Status, Foreign: origin == beads.OriginForeign,
		})
		b.State = bead.State
		if !is.UpdatedAt.IsZero() {
			b.UpdatedAt = is.UpdatedAt
		}
	}
	// A bead bd knows for this run that no event ever named: created by
	// hand, or by something that publishes nothing (P6-32).
	for _, is := range issues {
		if is.IsEpic() || seen[is.ID] {
			continue
		}
		live.Checked++
		bead, origin := is.SafeBead(statuses)
		rep.Summary.Beads = append(rep.Summary.Beads, orchestrator.BeadSummary{
			ID: is.ID, Title: bead.Title, State: bead.State, UpdatedAt: is.UpdatedAt,
		})
		live.Added = append(live.Added, StatusLiveChange{
			Bead: is.ID, Title: bead.Title, To: bead.State,
			Status: is.Status, Foreign: origin == beads.OriginForeign,
		})
	}
	if len(live.Changed) > 0 || len(live.Added) > 0 {
		rep.Summary.Counts = statusCount(rep.Summary.Beads)
	}
	rep.Live = live
}

// statusCount recounts the per-state totals after a reconciliation, the way
// the fold counts them: a bead no event gave a state to is counted under
// [orchestrator.BeadStateUnrecorded] so the rows add up to the total.
func statusCount(beadList []orchestrator.BeadSummary) map[domain.BeadState]int {
	if len(beadList) == 0 {
		return nil
	}
	counts := make(map[domain.BeadState]int, len(beadList))
	for _, b := range beadList {
		state := b.State
		if state == "" {
			state = orchestrator.BeadStateUnrecorded
		}
		counts[state]++
	}
	return counts
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
	statusVitalsSection(&b, r)
	statusBeadsSection(&b, r)
	statusAgentsSection(&b, r)
	statusEventsSection(&b, r)
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "\nnote: %s\n", statusScrub(n))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// statusVitalsSection prints the run's vitals — what the planner is doing,
// what the gate poller has seen — from the snapshot state.json carries
// (P6-29). It is what answers "why is nothing happening?" without reading
// the run log: an idle autopilot run's next planning time, a gh outage and
// how long it has lasted, and a merge bd keeps refusing to close.
func statusVitalsSection(b *strings.Builder, r *StatusReport) {
	if r.State == nil || r.State.Vitals == nil {
		return
	}
	lines := r.State.Vitals.Lines()
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%-*s%s\n", statusLabelWidth, "vitals", statusScrub(lines[0]))
	for _, l := range lines[1:] {
		fmt.Fprintf(b, "%-*s%s\n", statusLabelWidth, "", statusScrub(l))
	}
}

// statusHeader writes the label/value block that opens the report.
func statusHeader(b *strings.Builder, r *StatusReport) {
	row := func(label, value string) {
		// Values quote what a run recorded, which quotes what a cartridge
		// and bd printed: the brief, a stop reason, a finish summary. Any
		// of them can be several lines — a worktree cleanup names every
		// worktree it kept, one per line — so a value keeps its shape and
		// its continuation lines are indented under the label, the way
		// statusVitalsSection lays its own out.
		lines := statusRowLines(value)
		if len(lines) == 0 {
			return
		}
		fmt.Fprintf(b, "%-*s%s\n", statusLabelWidth, label, lines[0])
		for _, l := range lines[1:] {
			fmt.Fprintf(b, "%-*s%s\n", statusLabelWidth, "", l)
		}
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

// statusRowLines scrubs one header value into the lines it is printed as.
// A single-line value is one line, which is the common case; a multi-line
// one keeps its breaks, because [statusScrub] would otherwise drop them as
// control characters and run the sentences of a finish summary together
// ("2 beads (2 closed)removed 2 worktrees"). Blank lines are dropped: they
// carry nothing and a header block is dense on purpose.
func statusRowLines(value string) []string {
	scrubbed := statusScrubBlock(value)
	if !strings.Contains(scrubbed, "\n") {
		if strings.TrimSpace(scrubbed) == "" {
			return nil
		}
		return []string{strings.TrimRight(scrubbed, " \t")}
	}
	var out []string
	for _, l := range strings.Split(scrubbed, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, strings.TrimRight(l, " \t"))
	}
	return out
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
	// The summary is a report a run wrote, so it can end in a newline; the
	// error is appended to its last line, not to a blank one.
	if s := strings.TrimSpace(st.FinishSummary); s != "" {
		parts = append(parts, s)
	}
	if e := strings.TrimSpace(st.FinishError); e != "" {
		parts = append(parts, "error: "+e)
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
	fmt.Fprintf(b, "\n%-*s%d%s\n", statusLabelWidth, "beads", len(s.Beads), statusLiveHead(r.Live))
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
	statusForeignLine(b, s)
	statusLiveLines(b, r.Live)
}

// statusForeignLine names the beads the run passed over because bd holds
// them in a status it never registered (P6-12, P6-28). A run fired at a
// repository whose beads all sit in someone else's workflow otherwise looks
// like a run doing nothing at all.
func statusForeignLine(b *strings.Builder, s *orchestrator.RunSummary) {
	if len(s.Foreign) == 0 {
		return
	}
	ids := make([]string, len(s.Foreign))
	for i, id := range s.Foreign {
		ids[i] = statusScrub(string(id))
	}
	word := "beads"
	if len(ids) == 1 {
		word = "bead"
	}
	fmt.Fprintf(b, "  %d %s in a status this run does not use: %s\n",
		len(ids), word, strings.Join(ids, ", "))
}

// statusLiveHead is what the beads header says about the reconciliation:
// nothing at all when it did not run, so a report read without bd looks
// exactly as it always did.
func statusLiveHead(l *StatusLive) string {
	if l == nil {
		return ""
	}
	var parts []string
	if n := len(l.Changed); n > 0 {
		parts = append(parts, fmt.Sprintf("%d changed in bd", n))
	}
	if n := len(l.Added); n > 0 {
		parts = append(parts, fmt.Sprintf("%d not in the run log", n))
	}
	if len(parts) == 0 {
		return " (checked against bd)"
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// statusLiveLines names the beads the reconciliation corrected, under the
// counts they are already part of. A user who sees a count they did not
// expect wants to know which bead moved and who moved it; "outside the run"
// is as much as sc can honestly say, since bd records the change but not
// that it was a person.
func statusLiveLines(b *strings.Builder, l *StatusLive) {
	if l == nil {
		return
	}
	for _, c := range l.Changed {
		fmt.Fprintf(b, "  %s  %s -> %s (changed outside the run%s)\n",
			statusScrub(string(c.Bead)), statusScrub(string(c.From)),
			statusScrub(string(c.To)), statusForeignGloss(c))
	}
	for _, c := range l.Added {
		fmt.Fprintf(b, "  %s  %s (created outside the run%s)\n",
			statusScrub(string(c.Bead)), statusScrub(string(c.To)), statusForeignGloss(c))
	}
}

// statusForeignGloss names bd's own status when it is one the cannon did
// not register, so a reader can tell "blocked" the D-07 state from a
// repository's custom status that merely maps onto it (P3-14).
func statusForeignGloss(c StatusLiveChange) string {
	if !c.Foreign || c.Status == "" {
		return ""
	}
	return ", bd status " + statusScrub(c.Status)
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
// The scanner is [textsafe.Scrub], the one implementation the whole binary
// shares (P9-10); `sc status` supplies the line policy — a tab is one space
// and a carriage return is dropped rather than honoured, so a headless line
// and a status line read the same.
func statusScrub(s string) string { return textsafe.ScrubLine(s) }

// statusScrubBlock scrubs a multi-line report — [beads.FormatTimeline]'s
// output — line by line, keeping the line structure the report laid out and
// making every line as safe as [statusScrub] makes one. A timeline prints a
// bead title and the first line of a message's prose, both written by a
// model into bd, so the same guarantee has to hold there.
func statusScrubBlock(s string) string { return textsafe.ScrubBlock(s, textsafe.LinePolicy()) }

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
