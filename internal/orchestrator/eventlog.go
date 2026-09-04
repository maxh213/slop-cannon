package orchestrator

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
// [TeeRunLog] is that wiring as one call, for whoever assembles the run.
//
// The file is bounded (P6-33): past [DefaultEventLogMaxBytes] it is rolled
// into [EventLogRotatedName] and a fresh one is started, so a multi-day
// autopilot run costs two segments rather than everything it ever said.
// [ReadEventLog] and [SummariseLog] read across the rotation.

// EventLogName is the run log file inside the run directory.
const EventLogName = "events.jsonl"

// EventLogRotatedName is the previous segment of the run log, the one
// [EventLog] rolls the live file into when it passes its size cap (P6-33).
// Exactly one rotated segment is kept: a run whose log has rolled twice has
// lost its oldest events, which is the price of a bounded run directory.
const EventLogRotatedName = EventLogName + ".1"

// DefaultEventLogMaxBytes is the size at which the run log rotates. One
// [events.AgentOutput] line per cartridge output line (P5-08), and an
// autopilot run that never stops on its own (D-18), make an uncapped log a
// disk-filling machine; 64 MiB of JSONL is a few hundred thousand events, far
// more than any reader needs and small enough to keep. Two segments are kept,
// so a run directory's log costs at most twice this.
const DefaultEventLogMaxBytes int64 = 64 << 20

// DefaultRecentEvents is how many recent events [Summarise] keeps, and the
// number `sc status` prints.
const DefaultRecentEvents = 10

// BeadStateUnrecorded is the placeholder [RunSummary.Counts] uses for a
// bead the run log names without ever giving it a state — an AgentSpawned
// carrying a bead title before the scheduler's claim, say. Every bead in
// [RunSummary.Beads] is counted, so the total and the per-state breakdown
// under it always agree. It is not a bd status and never reaches bd.
const BeadStateUnrecorded domain.BeadState = "unrecorded"

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
	max  int64

	mu        sync.Mutex
	f         *os.File
	cw        *countingWriter
	w         *events.JSONLWriter
	size      int64
	rotations int
	err       error
	warned    bool
	closed    bool
}

// EventLogOptions configures [OpenEventLogWith]. The zero value is what
// [OpenEventLog] uses: slog.Default() and [DefaultEventLogMaxBytes].
type EventLogOptions struct {
	// Logger receives the one line a write failure and each rotation
	// produce; nil means slog.Default().
	Logger *slog.Logger
	// MaxBytes is the size at which the live file is rolled into
	// [EventLogRotatedName] (P6-33). Zero means [DefaultEventLogMaxBytes];
	// a negative value never rotates, which is what a test that wants one
	// file wants.
	MaxBytes int64
}

// OpenEventLog creates runDir if needed and opens <run-dir>/events.jsonl
// for appending, private to the user (0600), with the default size cap. Use
// [OpenEventLogWith] to change the cap.
func OpenEventLog(runDir string, logger *slog.Logger) (*EventLog, error) {
	return OpenEventLogWith(runDir, EventLogOptions{Logger: logger})
}

// OpenEventLogWith opens the run log of runDir, creating the directory if
// needed. Re-opening an existing log of a resumed run appends to it rather
// than truncating, so one file holds the whole history of a run across
// restarts.
//
// The file is capped: once it passes MaxBytes the live file is renamed to
// [EventLogRotatedName] — replacing the previous rotated segment — and a
// fresh one is started (P6-33). Every reader in this package
// ([ReadEventLog], [SummariseLog], and so `sc status`) reads the rotated
// segment before the live one, so a rotation is invisible to them except in
// the events that fell off the end.
func OpenEventLogWith(runDir string, opts EventLogOptions) (*EventLog, error) {
	if runDir == "" {
		return nil, errors.New("orchestrator: OpenEventLog: empty run directory")
	}
	dir := filepath.Clean(runDir)
	if err := os.MkdirAll(dir, runDirPerm); err != nil {
		return nil, fmt.Errorf("orchestrator: create run directory: %w", err)
	}
	path := EventLogPath(dir)
	f, size, err := openEventLogFile(path)
	if err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	max := opts.MaxBytes
	if max == 0 {
		max = DefaultEventLogMaxBytes
	}
	cw := &countingWriter{w: f}
	return &EventLog{
		path: path, log: logger, max: max,
		f: f, cw: cw, w: events.NewJSONLWriter(cw), size: size,
	}, nil
}

// openEventLogFile opens one segment for appending and returns its size,
// having closed off a record a previous run was cut short in the middle of.
func openEventLogFile(path string) (*os.File, int64, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, runFilePerm) //nolint:gosec // the run directory sc owns (D-23).
	if err != nil {
		return nil, 0, fmt.Errorf("orchestrator: open %s: %w", path, err)
	}
	if err := terminateLastLine(path, f); err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("orchestrator: stat %s: %w", path, err)
	}
	return f, info.Size(), nil
}

// countingWriter counts the bytes written to the file underneath it, so the
// log knows its size without stat-ing on every event. The target is swapped
// on rotation.
type countingWriter struct {
	w io.Writer
	n int64
}

// Write implements [io.Writer].
func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// terminateLastLine closes off a record the previous run was cut short in
// the middle of. A short write (the ENOSPC this log is built to survive) or
// a kill between a record and its newline leaves the file without a final
// newline; appending straight onto it would glue the next event to the
// wreck and lose that one too. One newline turns the damage into exactly
// one unreadable line, which the reader skips.
func terminateLastLine(path string, f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("orchestrator: stat %s: %w", path, err)
	}
	if info.Size() == 0 {
		return nil
	}
	r, err := os.Open(path) //nolint:gosec // the run directory sc owns (D-23).
	if err != nil {
		return fmt.Errorf("orchestrator: read %s: %w", path, err)
	}
	defer func() { _ = r.Close() }()
	var last [1]byte
	if _, err := r.ReadAt(last[:], info.Size()-1); err != nil {
		return fmt.Errorf("orchestrator: read %s: %w", path, err)
	}
	if last[0] == '\n' {
		return nil
	}
	if _, err := f.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("orchestrator: terminate %s: %w", path, err)
	}
	return nil
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
	if err := l.rotateLocked(); err != nil {
		l.err = err
		return err
	}
	if err := l.w.Write(ev); err != nil {
		l.err = err
		return err
	}
	l.size = l.cw.n
	return nil
}

// rotateLocked rolls the live file into [EventLogRotatedName] when it has
// passed the cap, and starts a fresh one (P6-33). The caller holds l.mu.
//
// The rotation happens before the write that would have gone over, so a
// segment never exceeds the cap by more than the last event that fitted
// under it, and the reader's "the rotated segment came first" rule holds
// without any bookkeeping in the file itself.
func (l *EventLog) rotateLocked() error {
	if l.max <= 0 || l.size < l.max {
		return nil
	}
	rotated := filepath.Join(filepath.Dir(l.path), EventLogRotatedName)
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("orchestrator: sync %s: %w", l.path, err)
	}
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("orchestrator: close %s: %w", l.path, err)
	}
	if err := os.Rename(l.path, rotated); err != nil {
		// The live file is closed and the rename failed: re-open it so the
		// run keeps its log rather than losing it to a failed tidy-up.
		f, size, oerr := openEventLogFile(l.path)
		if oerr != nil {
			return fmt.Errorf("orchestrator: rotate %s: %w", l.path, errors.Join(err, oerr))
		}
		l.f, l.cw.w, l.cw.n, l.size = f, f, size, size
		return fmt.Errorf("orchestrator: rotate %s: %w", l.path, err)
	}
	f, _, err := openEventLogFile(l.path)
	if err != nil {
		return err
	}
	l.f, l.cw.w, l.cw.n, l.size = f, f, 0, 0
	l.rotations++
	l.log.Info("run log rotated", slog.String("path", l.path),
		slog.String("rotated", rotated), slog.Int64("bytes", l.max))
	return nil
}

// Rotations returns how many times the log has rolled over its cap.
func (l *EventLog) Rotations() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rotations
}

// Size returns the size of the live segment in bytes.
func (l *EventLog) Size() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.size
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
// is [ErrNoEventLog].
//
// A line that does not parse is skipped rather than ending the read: the
// events after a damaged line are as real as the ones before it, and a log
// written by a newer sc (one carrying an event kind this binary does not
// know) must not blank out everything that follows. The damaged lines are
// described in the returned error, which comes back alongside every event
// that did parse, so a caller that wants the wreckage can ignore it. A log
// whose last line was cut short mid-write is a run still writing, not
// damage, and is not an error.
func ReadEventLog(runDir string) ([]events.Event, error) {
	path := EventLogPath(runDir)
	r, closeAll, err := openEventLogStream(runDir)
	if err != nil {
		return nil, err
	}
	defer closeAll()
	var evs []events.Event
	d, err := scanLog(r, func(ev events.Event) { evs = append(evs, ev) })
	if err != nil {
		return evs, fmt.Errorf("orchestrator: read %s: %w", path, err)
	}
	if note := d.Note(); note != "" {
		return evs, fmt.Errorf("orchestrator: read %s: %s", path, note)
	}
	return evs, nil
}

// EventLogSegments returns the run log's files in publication order: the
// rotated segment (P6-33) first, then the live one, leaving out the ones
// that do not exist. An empty result means the run has no log at all.
func EventLogSegments(runDir string) []string {
	dir := filepath.Clean(runDir)
	var out []string
	for _, name := range []string{EventLogRotatedName, EventLogName} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			out = append(out, path)
		}
	}
	return out
}

// openEventLogStream opens every segment of the run log and returns them as
// one stream, oldest first, so a reader sees the run's events in the order
// they were published whether or not the log has rotated. The returned func
// closes the files. A run with no log at all is [ErrNoEventLog].
func openEventLogStream(runDir string) (io.Reader, func(), error) {
	segments := EventLogSegments(runDir)
	if len(segments) == 0 {
		return nil, nil, fmt.Errorf("%w at %s", ErrNoEventLog, EventLogPath(runDir))
	}
	files := make([]*os.File, 0, len(segments))
	readers := make([]io.Reader, 0, len(segments))
	closeAll := func() {
		for _, f := range files {
			_ = f.Close()
		}
	}
	for _, path := range segments {
		f, err := os.Open(path) //nolint:gosec // the run directory sc owns (D-23).
		if errors.Is(err, os.ErrNotExist) {
			// It rotated between the listing and the open; the events are
			// in the segment we are about to read.
			continue
		}
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("orchestrator: read %s: %w", path, err)
		}
		files = append(files, f)
		readers = append(readers, f)
	}
	if len(readers) == 0 {
		closeAll()
		return nil, nil, fmt.Errorf("%w at %s", ErrNoEventLog, EventLogPath(runDir))
	}
	return io.MultiReader(readers...), closeAll, nil
}

// logDamage is what a run log turned out to be missing: lines that did not
// parse, and whether its last line was cut short. It is a report, never a
// reason to stop reading — `sc status` exists to read the wreckage.
type logDamage struct {
	// Lines is how many lines did not parse and were skipped. The
	// incomplete last line of a run still being written is not counted.
	Lines int
	// First is the parse error of the first of them, which names the line.
	First string
	// PartialTail is true when the log does not end in a newline and its
	// last line did not parse: a run mid-write, or one killed between a
	// record and its newline.
	PartialTail bool
}

// Any reports whether the log was anything but clean.
func (d logDamage) Any() bool { return d.Lines > 0 || d.PartialTail }

// Note describes the skipped lines for [RunSummary.Notes], or "" when none
// were skipped. The incomplete last line has its own note ([logPartialTailNote]).
func (d logDamage) Note() string {
	switch {
	case d.Lines == 1:
		return fmt.Sprintf("1 line of the run log did not parse and was skipped; "+
			"everything after it was still read (%s)", d.First)
	case d.Lines > 1:
		return fmt.Sprintf("%d lines of the run log did not parse and were skipped; "+
			"everything after them was still read (first: %s)", d.Lines, d.First)
	}
	return ""
}

// logPartialTailNote is what an unterminated last line means: not damage,
// but a record that was still being written when the log was read.
const logPartialTailNote = "the last line of the run log is incomplete: " +
	"the run was still writing it, or was killed mid-write"

// logTailReader passes a stream through while remembering its last byte, so
// [scanLog] can tell a log that stops mid-record from one whose final line
// is terminated and merely unreadable.
type logTailReader struct {
	r    io.Reader
	last byte
	read bool
}

// Read implements [io.Reader].
func (t *logTailReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.last = p[n-1]
		t.read = true
	}
	return n, err
}

// endsWithNewline reports whether the stream ended on a complete line. An
// empty stream counts as complete.
func (t *logTailReader) endsWithNewline() bool { return !t.read || t.last == '\n' }

// scanLog reads a run log from r and calls fn for every event that parses,
// skipping the lines that do not. [events.JSONLReader] is resumable by
// design, and this is where that matters: a torn write (the ENOSPC the
// EventLog is built to survive, or a crash between a record and its
// newline) leaves one bad line with good lines appended after it on the
// next run, and an event kind written by a newer sc is a bad line in the
// middle of a perfectly good log. Stopping at the first one would report a
// stale run as if it were current.
//
// The returned error is reserved for a stream that cannot be read on: an
// I/O failure, or a line past the reader's size limit. Those stop the scan
// because the reader makes no further progress; every per-line failure is
// counted in the returned [logDamage] instead.
func scanLog(r io.Reader, fn func(events.Event)) (logDamage, error) {
	var d logDamage
	tail := &logTailReader{r: r}
	jr := events.NewJSONLReader(tail)
	line, bad := 0, 0
	for {
		ev, err := jr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if jr.Line() <= line {
				// No line was consumed, so retrying would spin: the
				// stream itself failed rather than one record in it.
				return d, err
			}
			line = jr.Line()
			bad = line
			d.Lines++
			if d.First == "" {
				d.First = err.Error()
			}
			continue
		}
		line = jr.Line()
		fn(ev)
	}
	if d.Lines > 0 && bad == jr.Line() && !tail.endsWithNewline() {
		// The only unreadable line is the last one, and the log does not
		// end in a newline: a record still being written, not damage.
		d.Lines--
		d.PartialTail = true
		if d.Lines == 0 {
			d.First = ""
		}
	}
	return d, nil
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
	// Foreign counts the beads the run passed over because bd holds them
	// in a status it did not register (P6-12, P6-28). It is folded from
	// the Warning's Source rather than its wording, and each bead is
	// counted once however many components met it.
	Foreign []domain.BeadID `json:"foreign_beads,omitempty"`
	// Beads are the run's beads in the order the log first mentioned them.
	Beads []BeadSummary `json:"beads,omitempty"`
	// Counts is how many beads are in each state, over Beads.
	Counts map[domain.BeadState]int `json:"bead_counts,omitempty"`
	// Agents are the agents still live at the end of the log, in the order
	// they were fired.
	Agents []domain.Agent `json:"live_agents,omitempty"`
	// Notes report what the fold could not use: a line that did not parse,
	// a last line cut short mid-write, a record that parses as an event but
	// carries no bead id. Each one costs itself and nothing after it — a
	// damaged run log is reported, never fatal, and never truncates the
	// report to the events before the damage. `sc status` exists to read
	// the wreckage.
	Notes []string `json:"notes,omitempty"`
	// SkippedLines is how many lines of the log did not parse and were left
	// out of the fold, SkippedRecords how many parsed as an event but named
	// no bead, and PartialTail says the last line was cut short mid-write.
	// They are the machine-readable form of the corresponding [Notes] (a
	// consumer of `sc status --json` must not have to grep English to learn
	// that the fold is incomplete, P6-42); the notes stay for the human.
	SkippedLines   int  `json:"skipped_lines,omitempty"`
	SkippedRecords int  `json:"skipped_records,omitempty"`
	PartialTail    bool `json:"partial_tail,omitempty"`
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
// line (a run mid-write, or one killed) and a line that does not parse at
// all are skipped and reported in [RunSummary.Notes]; neither ends the fold,
// so the summary always describes the whole log rather than its first
// undamaged stretch.
//
// The file is streamed rather than slurped: a run log grows for the life of
// the run — one line per cartridge output line (P5-08), and under D-18 an
// autopilot run never stops on its own — while `sc status` reads it only to
// print a fold and the last few events. Nothing but the summary and those
// few events is ever held in memory. A log that has rotated (P6-33) is read
// across its segments, oldest first, so the fold does not change shape the
// moment the file rolls over.
func SummariseLog(runDir string, recent int) (RunSummary, error) {
	path := EventLogPath(runDir)
	r, closeAll, err := openEventLogStream(runDir)
	if err != nil {
		return RunSummary{}, err
	}
	defer closeAll()
	s, err := SummariseReader(r, recent)
	if err != nil {
		return s, fmt.Errorf("orchestrator: read %s: %w", path, err)
	}
	return s, nil
}

// SummariseReader folds a run log read from r, following [Summarise] for
// recent. It holds the summary and at most recent events, never the log, so
// the cost of a status report does not grow with the run's output.
//
// A line that does not parse is skipped, counted in [RunSummary.Notes] and
// the fold carries on: a damaged line costs itself and nothing after it, so
// a run whose log was torn hours ago is still reported as it stands now
// rather than as it was at the tear. An unterminated last line is a run
// still writing, and says so. The error is returned only when the stream
// itself cannot be read on.
func SummariseReader(r io.Reader, recent int) (RunSummary, error) {
	f := newLogFold(recent)
	d, err := scanLog(r, f.add)
	f.damage = d
	return f.done(), err
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
	// damage is what the log itself turned out to be missing, when the fold
	// read one ([SummariseReader] sets it; [Summarise] folds events that
	// have already parsed, so it never has any).
	damage logDamage
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
		if id, ok := ForeignStatusOf(ev); ok && !slices.Contains(s.Foreign, id) {
			s.Foreign = append(s.Foreign, id)
		}
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
			state := b.State
			if state == "" {
				// A bead only ever named by an event that carried no state
				// (an AgentSpawned with a bead title, say) still has to be
				// somewhere, or the total above the breakdown would not
				// match the rows under it.
				state = BeadStateUnrecorded
			}
			s.Counts[state]++
		}
	}
	if len(s.Agents) == 0 {
		s.Agents = nil
	}
	s.SkippedLines = f.damage.Lines
	s.PartialTail = f.damage.PartialTail
	for _, n := range f.noBead {
		s.SkippedRecords += n
	}
	if note := f.damage.Note(); note != "" {
		s.Notes = append(s.Notes, note)
	}
	if f.damage.PartialTail {
		s.Notes = append(s.Notes, logPartialTailNote)
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
