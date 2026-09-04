package orchestrator

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// The run log (P6-08). Every event a run publishes is appended, as it is
// published, to <run-dir>/events.jsonl in the [events.JSONLWriter] format,
// so a crashed run leaves a complete record up to its last event and
// `sc status` can reconstruct what happened without bd, without git and
// without the process that wrote it.
//
// [Run] publishes through [Options.Bus], so the wiring is one line:
//
//	log, err := orchestrator.OpenEventLog(runDir, logger)
//	run, err := orchestrator.New(cfg, orchestrator.Options{Bus: orchestrator.Tee(log, bus), …})
//	defer log.Close()
//
// [Tee] keeps the live bus (the TUI, the headless renderer) and the file in
// step. The log comes first in that argument list on purpose: [Tee]
// forwards in argument order, so the file is written before any subscriber
// is told, and anything a subscriber renders is already durable.

// EventLogName is the run log file inside the run directory.
const EventLogName = "events.jsonl"

// DefaultRecentEvents is how many recent events [Summarise] keeps, and the
// number `sc status` prints.
const DefaultRecentEvents = 10

// ErrNoEventLog is returned by [ReadEventLog] and [SummariseLog] when the
// run directory has no events.jsonl: a run that was never started, or one
// whose directory was pruned.
var ErrNoEventLog = errors.New("orchestrator: no event log")

// EventLogPath returns <runDir>/events.jsonl. Nothing is created.
func EventLogPath(runDir string) string {
	return filepath.Join(filepath.Clean(runDir), EventLogName)
}

// EventLog appends a run's events to <run-dir>/events.jsonl. It satisfies
// [Publisher], so it can be handed to [Options.Bus] directly or combined
// with the live bus through [Tee].
//
// Writes are unbuffered and serialised, so the file on disk is complete up
// to the last event even if the process is killed. A write failure is
// sticky: it is logged once, reported by [EventLog.Err], and never turned
// into a panic or a dropped run — a full disk must not take the cannon
// down mid-flight.
type EventLog struct {
	path string
	log  *slog.Logger

	mu     sync.Mutex
	f      *os.File
	w      *events.JSONLWriter
	err    error
	warned bool
	closed bool
}

// OpenEventLog creates runDir if needed and opens <run-dir>/events.jsonl
// for appending, private to the user (0600). Re-opening an existing log of
// a resumed run appends to it rather than truncating, so one file holds the
// whole history of a run across restarts. A nil logger means slog.Default().
func OpenEventLog(runDir string, logger *slog.Logger) (*EventLog, error) {
	if runDir == "" {
		return nil, errors.New("orchestrator: OpenEventLog: empty run directory")
	}
	dir := filepath.Clean(runDir)
	if err := os.MkdirAll(dir, runDirPerm); err != nil {
		return nil, fmt.Errorf("orchestrator: create run directory: %w", err)
	}
	path := EventLogPath(dir)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, runFilePerm) //nolint:gosec // the run directory sc owns (D-23).
	if err != nil {
		return nil, fmt.Errorf("orchestrator: open %s: %w", path, err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &EventLog{path: path, log: logger, f: f, w: events.NewJSONLWriter(f)}, nil
}

// Path returns the file the log writes to.
func (l *EventLog) Path() string { return l.path }

// Write appends one event and returns the write error, if any. A closed log
// reports [os.ErrClosed]; a log that already failed reports that first
// failure again without touching the file.
func (l *EventLog) Write(ev events.Event) error {
	if l == nil || ev == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	if l.closed {
		l.err = fmt.Errorf("orchestrator: write %s: %w", l.path, os.ErrClosed)
		return l.err
	}
	if err := l.w.Write(ev); err != nil {
		l.err = err
		return err
	}
	return nil
}

// Publish implements [Publisher]: it appends the event and swallows the
// error after logging it once, so a broken run log degrades to no run log
// instead of stopping the run. Use [EventLog.Err] to find out.
func (l *EventLog) Publish(ev events.Event) {
	if l == nil {
		return
	}
	if err := l.Write(ev); err != nil {
		l.mu.Lock()
		first := !l.warned
		l.warned = true
		l.mu.Unlock()
		if first {
			l.log.Error("run log write failed; the run continues without it",
				slog.String("path", l.path), slog.Any("err", err))
		}
	}
}

// Written returns how many events reached the file.
func (l *EventLog) Written() uint64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Written()
}

// Err returns the first write failure, or nil.
func (l *EventLog) Err() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Close flushes the file to disk and closes it. It is idempotent.
func (l *EventLog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if err := l.f.Sync(); err != nil && l.err == nil {
		l.err = fmt.Errorf("orchestrator: sync %s: %w", l.path, err)
	}
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("orchestrator: close %s: %w", l.path, err)
	}
	return nil
}

// Tee returns a [Publisher] that forwards every event to each of pubs in
// order, skipping nil ones. It is how a run reaches both the live bus and
// the run log. Tee(nil...) and Tee() return nil, which [Options.Bus]
// accepts.
func Tee(pubs ...Publisher) Publisher {
	live := make(multiPublisher, 0, len(pubs))
	for _, p := range pubs {
		if p == nil {
			continue
		}
		live = append(live, p)
	}
	switch len(live) {
	case 0:
		return nil
	case 1:
		return live[0]
	default:
		return live
	}
}

// multiPublisher fans one event out to several publishers.
type multiPublisher []Publisher

// Publish implements [Publisher].
func (m multiPublisher) Publish(ev events.Event) {
	for _, p := range m {
		p.Publish(ev)
	}
}

// ReadEventLog reads every event of <runDir>/events.jsonl. A missing file
// is [ErrNoEventLog]; a corrupt line returns the events read before it
// along with the error, so `sc status` can still report what it has.
func ReadEventLog(runDir string) ([]events.Event, error) {
	path := EventLogPath(runDir)
	f, err := os.Open(path) //nolint:gosec // the run directory sc owns (D-23).
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w at %s", ErrNoEventLog, path)
	}
	if err != nil {
		return nil, fmt.Errorf("orchestrator: read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	evs, err := events.ReadAll(f)
	if err != nil {
		return evs, fmt.Errorf("orchestrator: read %s: %w", path, err)
	}
	return evs, nil
}

// BeadSummary is what the run log says about one bead: the last state it
// was seen in and the round, PR and merge it reached. It is derived purely
// from events, so it needs no bd call; `sc status <bead-id>` goes to bd for
// the authoritative history.
type BeadSummary struct {
	// ID is the bead.
	ID domain.BeadID `json:"id"`
	// Title is its title, when an event carried one.
	Title string `json:"title,omitempty"`
	// State is the last state the run log recorded.
	State domain.BeadState `json:"state,omitempty"`
	// Round is the last review round it entered.
	Round int `json:"round,omitempty"`
	// PR is the pull request URL, when one was opened.
	PR string `json:"pr,omitempty"`
	// PRNumber is that pull request's number.
	PRNumber int `json:"pr_number,omitempty"`
	// Merged is true once the bead's code landed on the default branch.
	Merged bool `json:"merged,omitempty"`
	// UpdatedAt is when the last event about it was published.
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// RunSummary is a run folded out of its event log: the shape `sc status`
// prints and the TUI header shows for a run it did not itself watch.
//
// Everything here is derived from events alone. A field that no event
// carried stays at its zero value rather than being guessed.
type RunSummary struct {
	// Run is the run the events belong to.
	Run domain.RunID `json:"run"`
	// Epic is the run's parent bead (D-22), once a plan created one.
	Epic domain.BeadID `json:"epic,omitempty"`
	// Phase is the lifecycle phase the events imply.
	Phase Phase `json:"phase"`
	// Resumed is true when the last RunStarted was a resume.
	Resumed bool `json:"resumed,omitempty"`
	// StartedAt is when the run started, LastAt when its last event was
	// published, FinishedAt when it ended.
	StartedAt  time.Time `json:"started_at,omitempty"`
	LastAt     time.Time `json:"last_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	// Finish, FinishSummary and Duration come from RunFinished.
	Finish        events.FinishReason `json:"finish_reason,omitempty"`
	FinishSummary string              `json:"finish_summary,omitempty"`
	Duration      time.Duration       `json:"duration,omitempty"`
	// PauseReason and StopReason are the reasons of the last RunPaused and
	// RunStopped.
	PauseReason string `json:"pause_reason,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
	// Events counts every event in the log, Output the AgentOutput lines
	// among them (which Recent leaves out).
	Events int `json:"events"`
	Output int `json:"output_lines,omitempty"`
	// Plans counts PlanProduced, Rounds counts RoundStarted.
	Plans  int `json:"plans,omitempty"`
	Rounds int `json:"rounds,omitempty"`
	// Fired, Completed and Failed count AgentSpawned, AgentFinished and
	// AgentFailed. A crash retry counts once in each of Fired and Failed.
	Fired     int `json:"agents_fired,omitempty"`
	Completed int `json:"agents_finished,omitempty"`
	Failed    int `json:"agents_failed,omitempty"`
	// PRs counts PROpened, Merges counts Merged.
	PRs    int `json:"prs_opened,omitempty"`
	Merges int `json:"merged,omitempty"`
	// Warnings and Errors count Warning and Error events.
	Warnings int `json:"warnings,omitempty"`
	Errors   int `json:"errors,omitempty"`
	// Beads are the run's beads in the order the log first mentioned them.
	Beads []BeadSummary `json:"beads,omitempty"`
	// Counts is how many beads are in each state, over Beads.
	Counts map[domain.BeadState]int `json:"bead_counts,omitempty"`
	// Agents are the agents still live at the end of the log, in the order
	// they were fired.
	Agents []domain.Agent `json:"live_agents,omitempty"`
	// Notes report what the fold could not use: a record that parses as an
	// event but carries no bead id, for instance. A damaged run log is
	// reported, never fatal — `sc status` exists to read the wreckage.
	Notes []string `json:"notes,omitempty"`
	// Recent are the last events, AgentOutput excluded, oldest first. They
	// are not marshalled: an events.Event is an interface, so a caller that
	// wants them in JSON renders them itself (tui.FormatHeadless) or
	// encodes them with events.Marshal.
	Recent []events.Event `json:"-"`
}

// Bead returns the summary of one bead.
func (s RunSummary) Bead(id domain.BeadID) (BeadSummary, bool) {
	for _, b := range s.Beads {
		if b.ID == id {
			return b, true
		}
	}
	return BeadSummary{}, false
}

// Live reports how many agents were still running at the end of the log.
func (s RunSummary) Live() int { return len(s.Agents) }

// Settled reports how many beads reached a state the cannon owes nothing
// more for (see [BeadSettled]'s states: closed, blocked, failed and
// needs_human).
func (s RunSummary) Settled() int {
	n := 0
	for _, b := range s.Beads {
		if BeadStateSettled(b.State) {
			n++
		}
	}
	return n
}

// SummariseLog folds <runDir>/events.jsonl as it reads it. A truncated last
// line (a run killed mid-write) still yields a summary of everything before
// it, together with the parse error.
//
// The file is streamed rather than slurped: a run log grows for the life of
// the run — one line per cartridge output line (P5-08), and under D-18 an
// autopilot run never stops on its own — while `sc status` reads it only to
// print a fold and the last few events. Nothing but the summary and those
// few events is ever held in memory. (Trimming or rolling the file itself is
// P6-33.)
func SummariseLog(runDir string, recent int) (RunSummary, error) {
	path := EventLogPath(runDir)
	f, err := os.Open(path) //nolint:gosec // the run directory sc owns (D-23).
	if errors.Is(err, os.ErrNotExist) {
		return RunSummary{}, fmt.Errorf("%w at %s", ErrNoEventLog, path)
	}
	if err != nil {
		return RunSummary{}, fmt.Errorf("orchestrator: read %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	s, err := SummariseReader(f, recent)
	if err != nil {
		return s, fmt.Errorf("orchestrator: read %s: %w", path, err)
	}
	return s, nil
}

// SummariseReader folds a run log read from r, following [Summarise] for
// recent. It holds the summary and at most recent events, never the log, so
// the cost of a status report does not grow with the run's output. A line
// that does not parse stops the read and is returned along with the summary
// of everything before it.
func SummariseReader(r io.Reader, recent int) (RunSummary, error) {
	f := newLogFold(recent)
	jr := events.NewJSONLReader(r)
	for {
		ev, err := jr.Next()
		if errors.Is(err, io.EOF) {
			return f.done(), nil
		}
		if err != nil {
			return f.done(), err
		}
		f.add(ev)
	}
}

// Summarise folds a run's events into a [RunSummary]. recent is how many
// events to keep in [RunSummary.Recent]; zero means [DefaultRecentEvents]
// and a negative number keeps none. Events are assumed to be in publication
// order, which is the order the run log holds them.
func Summarise(evs []events.Event, recent int) RunSummary {
	f := newLogFold(recent)
	for _, ev := range evs {
		f.add(ev)
	}
	return f.done()
}

// logFold accumulates a [RunSummary] one event at a time. It is the shared
// engine of [Summarise] and [SummariseReader]; everything it holds is
// bounded by the run's beads, its live agents and recent, never by the
// number of events folded.
type logFold struct {
	s      RunSummary
	recent int
	index  map[domain.BeadID]int
	live   map[domain.AgentID]int
	// noBead counts records that parse as an event but name no bead, by
	// kind. They are skipped and reported in [RunSummary.Notes] rather than
	// dereferenced (a nil bead used to crash `sc status`).
	noBead map[events.Kind]int
}

// newLogFold starts a fold. recent follows [Summarise].
func newLogFold(recent int) *logFold {
	if recent == 0 {
		recent = DefaultRecentEvents
	}
	return &logFold{
		s:      RunSummary{Phase: PhaseNew},
		recent: recent,
		index:  make(map[domain.BeadID]int),
		live:   make(map[domain.AgentID]int),
	}
}

// bead returns the summary of id, creating it on first mention, and stamps
// it with at. A record with no bead id yields nil: every caller checks, and
// reports it through skip.
func (f *logFold) bead(id domain.BeadID, at time.Time) *BeadSummary {
	if id == "" {
		return nil
	}
	i, ok := f.index[id]
	if !ok {
		f.s.Beads = append(f.s.Beads, BeadSummary{ID: id})
		i = len(f.s.Beads) - 1
		f.index[id] = i
	}
	b := &f.s.Beads[i]
	if at.After(b.UpdatedAt) {
		b.UpdatedAt = at
	}
	return b
}

// skip records that one event named no bead and could not be folded.
func (f *logFold) skip(kind events.Kind) {
	if f.noBead == nil {
		f.noBead = make(map[events.Kind]int, 2)
	}
	f.noBead[kind]++
}

// dropAgent removes a finished agent from the live list, keeping the order
// of the rest.
func (f *logFold) dropAgent(id domain.AgentID) {
	i, ok := f.live[id]
	if !ok {
		return
	}
	f.s.Agents = append(f.s.Agents[:i], f.s.Agents[i+1:]...)
	delete(f.live, id)
	for k, j := range f.live {
		if j > i {
			f.live[k] = j - 1
		}
	}
}

// planBeads registers every bead a plan created, in the plan's own order, so
// a bead that has been planned but not yet claimed is counted too. This is
// the only inventory a real run publishes: BeadCreated and GraphChanged
// exist but nothing in production emits them (P6-32), so without this a bead
// would enter the summary only when the scheduler claimed it, and `sc status`
// on a ten-bead plan would report the three beads agents had touched.
//
// A created bead is recorded open, which is the state bd mints one in and
// the entry state of the D-07 flow; any later event overwrites it.
func (f *logFold) planBeads(e events.PlanProduced, at time.Time) {
	if len(e.Created) == 0 {
		return
	}
	seen := make(map[string]bool, len(e.Created))
	add := func(key, title string) {
		id, ok := e.Created[key]
		if !ok || seen[key] {
			return
		}
		seen[key] = true
		b := f.bead(id, at)
		if b == nil {
			f.skip(e.Kind())
			return
		}
		if title != "" && b.Title == "" {
			b.Title = title
		}
		if b.State == "" {
			b.State = domain.BeadStateOpen
		}
	}
	for _, pb := range e.Plan.Beads {
		add(pb.Key, pb.Title)
	}
	// Anything Created holds that the plan does not name (it should not
	// happen) is folded in too, in a fixed order so the summary is stable.
	rest := make([]string, 0, len(e.Created))
	for k := range e.Created {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		add(k, "")
	}
}

// add folds one event. A nil event is ignored.
func (f *logFold) add(ev events.Event) {
	if ev == nil {
		return
	}
	s := &f.s
	s.Events++
	at := ev.At()
	if s.Run == "" {
		s.Run = ev.RunID()
	}
	if at.After(s.LastAt) {
		s.LastAt = at
	}
	switch e := ev.(type) {
	case events.RunStarted:
		s.Phase = PhaseRunning
		s.Resumed = e.Resumed
		s.StartedAt = e.Info.StartedAt
		if s.StartedAt.IsZero() {
			s.StartedAt = at
		}
		if e.Info.Epic != "" {
			s.Epic = e.Info.Epic
		}
	case events.RunPaused:
		s.Phase = PhasePaused
		s.PauseReason = e.Reason
	case events.RunResumed:
		s.Phase = PhaseRunning
		s.PauseReason = ""
	case events.RunStopped:
		s.Phase = PhaseStopping
		s.StopReason = e.Reason
	case events.RunFinished:
		s.Phase = PhaseFinished
		s.Finish = e.Reason
		s.FinishSummary = e.Summary
		s.Duration = e.Duration
		s.FinishedAt = at
	case events.PlanProduced:
		s.Plans++
		if e.Epic != "" {
			s.Epic = e.Epic
		}
		f.planBeads(e, at)
	case events.BeadCreated:
		b := f.bead(e.Bead.ID, at)
		if b == nil {
			f.skip(e.Kind())
			break
		}
		b.Title = e.Bead.Title
		if e.Bead.State != "" {
			b.State = e.Bead.State
		}
	case events.BeadUpdated:
		b := f.bead(e.Bead.ID, at)
		if b == nil {
			f.skip(e.Kind())
			break
		}
		if e.Bead.Title != "" {
			b.Title = e.Bead.Title
		}
		if e.Bead.State != "" {
			b.State = e.Bead.State
		}
		if e.Bead.Round > 0 {
			b.Round = e.Bead.Round
		}
	case events.BeadStateChanged:
		b := f.bead(e.Bead, at)
		if b == nil {
			f.skip(e.Kind())
			break
		}
		b.State = e.To
		if e.Round > 0 {
			b.Round = e.Round
		}
	case events.GraphChanged:
		if e.Epic != "" {
			s.Epic = e.Epic
		}
		for _, gb := range e.Beads {
			b := f.bead(gb.ID, at)
			if b == nil {
				f.skip(e.Kind())
				continue
			}
			if gb.Title != "" {
				b.Title = gb.Title
			}
			if gb.State != "" {
				b.State = gb.State
			}
			if gb.Round > 0 {
				b.Round = gb.Round
			}
		}
	case events.RoundStarted:
		s.Rounds++
		b := f.bead(e.Bead, at)
		if b == nil {
			f.skip(e.Kind())
			break
		}
		if e.Round > 0 {
			b.Round = e.Round
		}
	case events.AgentSpawned:
		s.Fired++
		a := e.Agent
		if i, ok := f.live[a.ID]; ok {
			s.Agents[i] = a
		} else {
			s.Agents = append(s.Agents, a)
			f.live[a.ID] = len(s.Agents) - 1
		}
		if e.BeadTitle != "" {
			if b := f.bead(a.Bead, at); b != nil && b.Title == "" {
				b.Title = e.BeadTitle
			}
		}
	case events.AgentOutput:
		s.Output++
	case events.AgentFinished:
		s.Completed++
		f.dropAgent(e.Agent)
	case events.AgentFailed:
		s.Failed++
		f.dropAgent(e.Agent)
	case events.PROpened:
		s.PRs++
		b := f.bead(e.Bead, at)
		if b == nil {
			f.skip(e.Kind())
			break
		}
		b.PR = e.URL
		b.PRNumber = e.Number
	case events.Merged:
		s.Merges++
		b := f.bead(e.Bead, at)
		if b == nil {
			f.skip(e.Kind())
			break
		}
		b.Merged = true
		if e.PRNumber > 0 && b.PRNumber == 0 {
			b.PRNumber = e.PRNumber
		}
	case events.Warning:
		s.Warnings++
	case events.Error:
		s.Errors++
	}
	if f.recent > 0 && ev.Kind() != events.KindAgentOutput {
		s.Recent = append(s.Recent, ev)
		if len(s.Recent) > f.recent {
			s.Recent = s.Recent[len(s.Recent)-f.recent:]
		}
	}
}

// done finishes the fold: the per-state counts, the live-agent list and the
// notes about anything skipped.
func (f *logFold) done() RunSummary {
	s := f.s
	if len(s.Beads) > 0 {
		s.Counts = make(map[domain.BeadState]int, len(s.Beads))
		for _, b := range s.Beads {
			if b.State != "" {
				s.Counts[b.State]++
			}
		}
	}
	if len(s.Agents) == 0 {
		s.Agents = nil
	}
	if note := f.noBeadNote(); note != "" {
		s.Notes = append(s.Notes, note)
	}
	return s
}

// noBeadNote describes the records that named no bead, or "" if none did.
func (f *logFold) noBeadNote() string {
	if len(f.noBead) == 0 {
		return ""
	}
	total := 0
	kinds := make([]string, 0, len(f.noBead))
	for k, n := range f.noBead {
		total += n
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	if total == 1 {
		return fmt.Sprintf("1 record in the run log names no bead and was skipped (%s)",
			strings.Join(kinds, ", "))
	}
	return fmt.Sprintf("%d records in the run log name no bead and were skipped (%s)",
		total, strings.Join(kinds, ", "))
}

// Liveness is what the repository's run lock (see [LockPath]) says about a
// run: who holds it and whether that process still exists. A run directory
// on its own cannot tell a run that is still firing from one that was
// killed — both leave `phase: running` behind — so `sc status` asks the lock
// (P6-08).
type Liveness struct {
	// Holder is what the lock file records; the zero value when Held is
	// false.
	Holder LockHolder `json:"holder,omitempty"`
	// Held is true when a lock file exists and could be read.
	Held bool `json:"held"`
	// Mine is true when the lock names the run that was asked about.
	Mine bool `json:"mine,omitempty"`
	// Alive is true when a process with the holder's id exists. It cannot
	// say the process is *that* process: pids are reused, which P9-04
	// hardens.
	Alive bool `json:"alive"`
}

// Running reports whether a live process still holds the lock for this run.
func (l Liveness) Running() bool { return l.Held && l.Mine && l.Alive }

// ProbeLiveness reads the run lock of the repository whose run directory is
// runDir and reports what it says about run (an empty run matches whatever
// holds the lock). A missing or unreadable lock is not an error: it means
// nothing holds it, which is the normal end of a finished run and the
// tell-tale of a crashed one.
func ProbeLiveness(runDir string, run domain.RunID) Liveness {
	h, err := readLock(LockPath(runDir))
	if err != nil {
		return Liveness{}
	}
	l := Liveness{Holder: h, Held: true, Mine: run == "" || h.Run == run}
	l.Alive = h.PID > 0 && processAlive(h.PID)
	return l
}

// WriteEventLog writes evs to w as a run log, for tests and for tools that
// synthesise one. It is the inverse of [ReadEventLog].
func WriteEventLog(w io.Writer, evs []events.Event) error {
	jw := events.NewJSONLWriter(w)
	for _, ev := range evs {
		if err := jw.Write(ev); err != nil {
			return err
		}
	}
	return nil
}
