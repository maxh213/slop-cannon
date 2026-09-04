package orchestrator

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
//	run, err := orchestrator.New(cfg, orchestrator.Options{Bus: orchestrator.Tee(bus, log), …})
//	defer log.Close()
//
// [Tee] keeps the live bus (the TUI, the headless renderer) and the file in
// step: the file is written first, so anything a subscriber renders is
// already durable.

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

// SummariseLog reads <runDir>/events.jsonl and folds it. A truncated last
// line (a run killed mid-write) still yields a summary of everything before
// it, together with the parse error.
func SummariseLog(runDir string, recent int) (RunSummary, error) {
	evs, readErr := ReadEventLog(runDir)
	if readErr != nil && len(evs) == 0 {
		return RunSummary{}, readErr
	}
	return Summarise(evs, recent), readErr
}

// Summarise folds a run's events into a [RunSummary]. recent is how many
// events to keep in [RunSummary.Recent]; zero means [DefaultRecentEvents]
// and a negative number keeps none. Events are assumed to be in publication
// order, which is the order the run log holds them.
func Summarise(evs []events.Event, recent int) RunSummary {
	if recent == 0 {
		recent = DefaultRecentEvents
	}
	s := RunSummary{Phase: PhaseNew}
	index := make(map[domain.BeadID]int)
	live := make(map[domain.AgentID]int)

	bead := func(id domain.BeadID, at time.Time) *BeadSummary {
		if id == "" {
			return nil
		}
		i, ok := index[id]
		if !ok {
			s.Beads = append(s.Beads, BeadSummary{ID: id})
			i = len(s.Beads) - 1
			index[id] = i
		}
		b := &s.Beads[i]
		if at.After(b.UpdatedAt) {
			b.UpdatedAt = at
		}
		return b
	}
	drop := func(id domain.AgentID) {
		i, ok := live[id]
		if !ok {
			return
		}
		s.Agents = append(s.Agents[:i], s.Agents[i+1:]...)
		delete(live, id)
		for k, j := range live {
			if j > i {
				live[k] = j - 1
			}
		}
	}

	for _, ev := range evs {
		if ev == nil {
			continue
		}
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
		case events.BeadCreated:
			b := bead(e.Bead.ID, at)
			b.Title = e.Bead.Title
			if e.Bead.State != "" {
				b.State = e.Bead.State
			}
		case events.BeadUpdated:
			b := bead(e.Bead.ID, at)
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
			b := bead(e.Bead, at)
			b.State = e.To
			if e.Round > 0 {
				b.Round = e.Round
			}
		case events.GraphChanged:
			if e.Epic != "" {
				s.Epic = e.Epic
			}
			for _, gb := range e.Beads {
				b := bead(gb.ID, at)
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
			b := bead(e.Bead, at)
			if e.Round > 0 {
				b.Round = e.Round
			}
		case events.AgentSpawned:
			s.Fired++
			a := e.Agent
			if i, ok := live[a.ID]; ok {
				s.Agents[i] = a
			} else {
				s.Agents = append(s.Agents, a)
				live[a.ID] = len(s.Agents) - 1
			}
			if e.BeadTitle != "" {
				if b := bead(a.Bead, at); b != nil && b.Title == "" {
					b.Title = e.BeadTitle
				}
			}
		case events.AgentOutput:
			s.Output++
		case events.AgentFinished:
			s.Completed++
			drop(e.Agent)
		case events.AgentFailed:
			s.Failed++
			drop(e.Agent)
		case events.PROpened:
			s.PRs++
			b := bead(e.Bead, at)
			b.PR = e.URL
			b.PRNumber = e.Number
		case events.Merged:
			s.Merges++
			b := bead(e.Bead, at)
			b.Merged = true
			if e.PRNumber > 0 && b.PRNumber == 0 {
				b.PRNumber = e.PRNumber
			}
		case events.Warning:
			s.Warnings++
		case events.Error:
			s.Errors++
		}
		if recent > 0 && ev.Kind() != events.KindAgentOutput {
			s.Recent = append(s.Recent, ev)
			if len(s.Recent) > recent {
				s.Recent = s.Recent[len(s.Recent)-recent:]
			}
		}
	}

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
	return s
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
