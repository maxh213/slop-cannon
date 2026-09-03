package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// Tab is one of the views of the main pane.
type Tab int

// The views, in the order they appear in the tab bar.
const (
	// TabGraph is the dependency graph of the run's beads (P7-02).
	TabGraph Tab = iota
	// TabAgents is one card per agent subprocess (P7-03).
	TabAgents
	// TabFeedback is the review rounds of each bead (P7-04).
	TabFeedback
	// TabPRs is the deliveries: pull requests and merges (P7-05).
	TabPRs
	// TabLog is the rolling event log.
	TabLog
)

// tabNames are the labels of the tab bar, indexed by [Tab].
var tabNames = [...]string{"Graph", "Agents", "Feedback", "PRs", "Log"}

// Tabs returns every view in tab-bar order.
func Tabs() []Tab {
	out := make([]Tab, len(tabNames))
	for i := range tabNames {
		out[i] = Tab(i)
	}
	return out
}

// String returns the tab-bar label.
func (t Tab) String() string {
	if t < 0 || int(t) >= len(tabNames) {
		return fmt.Sprintf("Tab(%d)", int(t))
	}
	return tabNames[t]
}

// Valid reports whether t names one of the views.
func (t Tab) Valid() bool { return t >= 0 && int(t) < len(tabNames) }

// DefaultLogLines is how many log entries [State] keeps by default.
const DefaultLogLines = 500

// AgentView is one agent subprocess as the event stream described it: the
// [domain.Agent] the orchestrator published plus the task, transcript tail
// and outcome that only the events carry.
type AgentView struct {
	domain.Agent
	// Task is what the agent was fired to do.
	Task events.Task
	// BeadTitle is the title of the bead it works on, when known.
	BeadTitle string
	// Round is the review round it works in, when relevant.
	Round int
	// Attempt is 1 for the first try, higher for retries.
	Attempt int
	// LastLine is the most recent line it wrote to stdout or stderr.
	LastLine string
	// Lines counts the output lines seen so far.
	Lines int
	// Duration is how long it ran; set once it is finished or failed.
	Duration time.Duration
	// ExitCode is the subprocess exit status once it exited.
	ExitCode int
	// Failure classifies why it failed; empty when it did not.
	Failure events.Failure
	// Err is the failure message, already redacted.
	Err string
	// Produced is the structured message it returned, when it finished.
	Produced domain.MessageType
}

// Live reports whether the subprocess is still running.
func (a AgentView) Live() bool { return !a.State.Terminal() }

// Round is one review round of a bead with everything said in it so far.
type Round struct {
	// Bead is the bead under review.
	Bead domain.BeadID
	// Number is the 1-based round number.
	Number int
	// Kind says what the round is for.
	Kind events.RoundKind
	// Lenses lists the tester lenses reviewing this round.
	Lenses []domain.Lens
	// Commit is the sha the testers review.
	Commit string
	// Verdicts are the testers' verdicts, in arrival order.
	Verdicts []domain.Verdict
	// Responses are the engineer's answers to the previous round's items.
	Responses []domain.Response
	// Report is the engineer's implementation report for this round.
	Report *domain.ImplReport
	// Rulings are the PO's per-item arbitrations (D-08).
	Rulings []domain.Arbitration
	// Escalation is the PO's decision after the round cap (D-08).
	Escalation *domain.Escalation
}

// Delivery is a bead on its way to the default branch: the pull request it
// was opened as (safety on) and the merge that landed it.
type Delivery struct {
	// Bead is the bead delivered, or the review bead of an external PR.
	Bead domain.BeadID
	// URL is the pull request URL, when there is one.
	URL string
	// Number is the pull request number.
	Number int
	// Branch is the head branch of the PR, or the branch merged into.
	Branch string
	// Base is the base branch of the PR.
	Base string
	// External is true for a PR the cannon did not create (D-11).
	External bool
	// Merged is true once the code landed on the default branch.
	Merged bool
	// SHA is the commit now on the default branch.
	SHA string
	// Pushed is true when the merge was pushed to a remote.
	Pushed bool
}

// LogEntry is one line of the rolling event log.
type LogEntry struct {
	// At is the event's timestamp.
	At time.Time
	// Kind is the event kind, so a view can filter or colour by it.
	Kind events.Kind
	// Text is the one-line rendering of the event.
	Text string
}

// Notice is a [events.Warning] or [events.Error] worth keeping on screen.
type Notice struct {
	// At is when it was raised.
	At time.Time
	// Kind is either [events.KindWarning] or [events.KindError].
	Kind events.Kind
	// Source is the component that raised it.
	Source string
	// Message is the human-readable text.
	Message string
	// Err is the underlying error message, if any.
	Err string
}

// State is the run as the TUI understands it, folded out of the event
// stream: the latest snapshot of every bead, agent, review round and
// delivery plus a rolling log. Views (P7-02…P7-05) read it; only
// [State.Apply] writes it.
type State struct {
	// Run is the run being watched, as [events.RunStarted] described it.
	Run domain.Run
	// Resumed is true when the run was picked up with --resume.
	Resumed bool
	// Epic is the run's parent bead (D-22).
	Epic domain.BeadID
	// Beads are the run's beads in creation order (graph snapshots keep
	// the order the orchestrator sent).
	Beads []domain.Bead
	// Agents are every agent seen, in spawn order.
	Agents []AgentView
	// Rounds are the review rounds, in the order they started.
	Rounds []Round
	// Deliveries are the pull requests and merges, in the order they opened.
	Deliveries []Delivery
	// Plan is the most recent plan the PO produced, if any.
	Plan *domain.Plan
	// Log is the rolling event log, oldest first, capped at MaxLog.
	Log []LogEntry
	// Notices are the warnings and errors of the run, newest last.
	Notices []Notice
	// MaxLog is how many log entries to keep; zero means DefaultLogLines.
	MaxLog int
	// Events counts every event applied.
	Events int
	// Dropped is how many events the subscription missed (bus backpressure).
	Dropped uint64
	// Live is true while the run can still do work: it goes false on
	// [events.RunFinished].
	Live bool
	// Paused is true between [events.RunPaused] and [events.RunResumed].
	Paused bool
	// Stopping is true once a stop was requested and shutdown began.
	Stopping bool
	// Reason is why the run paused, stopped or finished, when known.
	Reason string
	// Finish is the terminal event, once the run has ended.
	Finish *events.RunFinished

	beadAt  map[domain.BeadID]int
	agentAt map[domain.AgentID]int
}

// NewState returns the initial state of a run that has not ended yet.
func NewState(run domain.Run) *State {
	return &State{
		Run:     run,
		Live:    true,
		MaxLog:  DefaultLogLines,
		beadAt:  make(map[domain.BeadID]int),
		agentAt: make(map[domain.AgentID]int),
	}
}

// Bead returns the bead with the given id.
func (s *State) Bead(id domain.BeadID) (domain.Bead, bool) {
	i, ok := s.beadAt[id]
	if !ok {
		return domain.Bead{}, false
	}
	return s.Beads[i], true
}

// Agent returns the agent with the given id.
func (s *State) Agent(id domain.AgentID) (AgentView, bool) {
	i, ok := s.agentAt[id]
	if !ok {
		return AgentView{}, false
	}
	return s.Agents[i], true
}

// LiveAgents returns the agents whose subprocess is still running; its
// length is the ammo in use.
func (s *State) LiveAgents() []AgentView {
	out := make([]AgentView, 0, len(s.Agents))
	for _, a := range s.Agents {
		if a.Live() {
			out = append(out, a)
		}
	}
	return out
}

// Ammo returns the slots in use and the run's cap (D-05).
func (s *State) Ammo() (used, total int) {
	return len(s.LiveAgents()), s.Run.Config.Ammo
}

// Mode returns the run mode, defaulting to autopilot when the run has not
// been described yet.
func (s *State) Mode() domain.RunMode {
	if s.Run.Mode == "" {
		return domain.RunModeAutopilot
	}
	return s.Run.Mode
}

// Apply folds one event into the state. Unknown kinds are logged and
// otherwise ignored, so a newer orchestrator cannot break an older view.
func (s *State) Apply(ev events.Event) {
	if ev == nil {
		return
	}
	s.Events++
	s.appendLog(ev)

	switch e := ev.(type) {
	case events.RunStarted:
		s.Run = e.Info
		s.Resumed = e.Resumed
		s.Live = true
		if e.Info.Epic != "" {
			s.Epic = e.Info.Epic
		}
	case events.RunPaused:
		s.Paused, s.Reason = true, e.Reason
	case events.RunResumed:
		s.Paused = false
		if e.Reason != "" {
			s.Reason = e.Reason
		}
	case events.RunStopped:
		s.Stopping, s.Reason = true, e.Reason
	case events.RunFinished:
		s.Live, s.Paused = false, false
		s.Reason = e.Reason.String()
		fin := e
		s.Finish = &fin
	case events.PlanProduced:
		plan := e.Plan
		s.Plan = &plan
		if e.Epic != "" {
			s.Epic = e.Epic
		}
	case events.BeadCreated:
		s.putBead(e.Bead)
	case events.BeadUpdated:
		s.putBead(e.Bead)
	case events.BeadStateChanged:
		b, ok := s.Bead(e.Bead)
		if !ok {
			b = domain.Bead{ID: e.Bead, Priority: domain.PriorityDefault}
		}
		b.State = e.To
		if e.Round > 0 {
			b.Round = e.Round
		}
		s.putBead(b)
	case events.GraphChanged:
		if e.Epic != "" {
			s.Epic = e.Epic
		}
		s.replaceBeads(e.Beads)
	case events.AgentSpawned:
		a, _ := s.Agent(e.Agent.ID)
		a.Agent = e.Agent
		if a.State == "" {
			a.State = domain.AgentStateRunning
		}
		a.Task, a.BeadTitle, a.Round = e.Task, e.BeadTitle, e.Round
		a.Attempt = max(e.Attempt, 1)
		s.putAgent(a)
	case events.AgentOutput:
		a, ok := s.Agent(e.Agent)
		if !ok {
			a.Agent = domain.Agent{ID: e.Agent, State: domain.AgentStateRunning}
		}
		a.LastLine = e.Line
		a.Lines++
		s.putAgent(a)
	case events.AgentFinished:
		a, ok := s.Agent(e.Agent)
		if !ok {
			a.Agent = domain.Agent{ID: e.Agent, Role: e.Role, Bead: e.Bead}
		}
		a.State = domain.AgentStateFinished
		a.Duration, a.ExitCode, a.Produced = e.Duration, e.ExitCode, e.Produced
		s.putAgent(a)
	case events.AgentFailed:
		a, ok := s.Agent(e.Agent)
		if !ok {
			a.Agent = domain.Agent{ID: e.Agent, Role: e.Role, Bead: e.Bead}
		}
		a.State = domain.AgentStateFailed
		if e.Failure == events.FailureKilled {
			a.State = domain.AgentStateKilled
		}
		a.Failure, a.Err, a.Duration, a.ExitCode = e.Failure, e.Err, e.Duration, e.ExitCode
		if e.Attempt > 0 {
			a.Attempt = e.Attempt
		}
		s.putAgent(a)
	case events.RoundStarted:
		r := s.round(e.Bead, e.Round)
		r.Kind, r.Lenses, r.Commit = e.RoundKind, e.Lenses, e.Commit
		s.putRound(*r)
	case events.VerdictReceived:
		r := s.round(e.Bead, e.Round)
		r.Verdicts = append(r.Verdicts, e.Verdict)
		s.putRound(*r)
	case events.ResponseReceived:
		r := s.round(e.Bead, e.Round)
		r.Responses = append(r.Responses, e.Responses...)
		if e.Report != nil {
			report := *e.Report
			r.Report = &report
		}
		s.putRound(*r)
	case events.ArbitrationReceived:
		r := s.round(e.Bead, e.Round)
		r.Rulings = append(r.Rulings, e.Rulings...)
		if e.Escalation != nil {
			esc := *e.Escalation
			r.Escalation = &esc
		}
		s.putRound(*r)
	case events.PROpened:
		d := s.delivery(e.Bead)
		d.URL, d.Number, d.Branch, d.Base, d.External = e.URL, e.Number, e.Branch, e.Base, e.External
		s.putDelivery(*d)
	case events.Merged:
		d := s.delivery(e.Bead)
		d.Merged, d.SHA, d.Pushed = true, e.SHA, e.Pushed
		if e.Branch != "" {
			d.Branch = e.Branch
		}
		if e.PRNumber != 0 {
			d.Number = e.PRNumber
		}
		s.putDelivery(*d)
	case events.Warning:
		s.appendNotice(Notice{At: e.At(), Kind: e.Kind(), Source: e.Source, Message: e.Message, Err: e.Err})
	case events.Error:
		s.appendNotice(Notice{At: e.At(), Kind: e.Kind(), Source: e.Source, Message: e.Message, Err: e.Err})
	}
}

// putBead inserts or replaces a bead, keeping first-seen order.
func (s *State) putBead(b domain.Bead) {
	if i, ok := s.beadAt[b.ID]; ok {
		s.Beads[i] = b
		return
	}
	s.beadAt[b.ID] = len(s.Beads)
	s.Beads = append(s.Beads, b)
}

// replaceBeads swaps in a whole graph snapshot, dropping beads it omits.
func (s *State) replaceBeads(beads []domain.Bead) {
	s.Beads = make([]domain.Bead, 0, len(beads))
	s.beadAt = make(map[domain.BeadID]int, len(beads))
	for _, b := range beads {
		s.putBead(b)
	}
}

// putAgent inserts or replaces an agent, keeping spawn order.
func (s *State) putAgent(a AgentView) {
	if i, ok := s.agentAt[a.ID]; ok {
		s.Agents[i] = a
		return
	}
	s.agentAt[a.ID] = len(s.Agents)
	s.Agents = append(s.Agents, a)
}

// round returns a copy of the (bead, number) round, creating it when the
// round's start was never seen.
func (s *State) round(bead domain.BeadID, number int) *Round {
	for _, r := range s.Rounds {
		if r.Bead == bead && r.Number == number {
			out := r
			return &out
		}
	}
	return &Round{Bead: bead, Number: number, Kind: events.RoundReview}
}

// putRound inserts or replaces a round, keeping start order.
func (s *State) putRound(r Round) {
	for i := range s.Rounds {
		if s.Rounds[i].Bead == r.Bead && s.Rounds[i].Number == r.Number {
			s.Rounds[i] = r
			return
		}
	}
	s.Rounds = append(s.Rounds, r)
}

// delivery returns a copy of the bead's delivery, creating it when absent.
func (s *State) delivery(bead domain.BeadID) *Delivery {
	for _, d := range s.Deliveries {
		if d.Bead == bead {
			out := d
			return &out
		}
	}
	return &Delivery{Bead: bead}
}

// putDelivery inserts or replaces a delivery, keeping open order.
func (s *State) putDelivery(d Delivery) {
	for i := range s.Deliveries {
		if s.Deliveries[i].Bead == d.Bead {
			s.Deliveries[i] = d
			return
		}
	}
	s.Deliveries = append(s.Deliveries, d)
}

// logLimit is how many log entries and notices the state keeps.
func (s *State) logLimit() int {
	if s.MaxLog <= 0 {
		return DefaultLogLines
	}
	return s.MaxLog
}

// appendLog adds one line for ev, dropping the oldest when the cap is hit.
func (s *State) appendLog(ev events.Event) {
	limit := s.logLimit()
	s.Log = append(s.Log, LogEntry{At: ev.At(), Kind: ev.Kind(), Text: FormatEvent(ev)})
	if len(s.Log) > limit {
		s.Log = append(s.Log[:0], s.Log[len(s.Log)-limit:]...)
	}
}

// appendNotice records one warning or error, keeping the newest MaxLog of
// them. Nothing bounds a run — an autopilot run never ends on its own
// (D-18) — so a chatty warning source would otherwise grow this slice for as
// long as the process lives; it is capped exactly like [State.Log] and the
// agent transcripts. The message and the underlying error come from a
// subprocess, so both are sanitised on the way in ([logSanitize]) and are
// safe for any view to draw.
func (s *State) appendNotice(n Notice) {
	limit := s.logLimit()
	n.Message, n.Err = logSanitize(n.Message), logSanitize(n.Err)
	s.Notices = append(s.Notices, n)
	if len(s.Notices) > limit {
		s.Notices = append(s.Notices[:0], s.Notices[len(s.Notices)-limit:]...)
	}
}

// FormatEvent renders one event as a single line, without a timestamp. The
// log view and the agents view's transcript notes (P7-03) share it. The
// headless renderer (P7-08) has its own, richer format, and `sc tui-demo
// --events` writes the events themselves as JSONL rather than this.
//
// Several event fields are untrusted: an agent's output line, a warning's
// message and an implementation report's summary are whatever a cartridge
// subprocess wrote, and a real CLI writes ANSI on nearly every line. The
// returned string is therefore sanitised (see [logSanitize]): it is always
// one line, and it never carries an escape sequence or a control character,
// so pasting it into a frame cannot repaint the terminal or steal a row.
func FormatEvent(ev events.Event) string { return logSanitize(formatEvent(ev)) }

// formatEvent is [FormatEvent] before sanitising, so the rendering has one
// place and the escaping has another.
func formatEvent(ev events.Event) string {
	switch e := ev.(type) {
	case events.RunStarted:
		kind := "started"
		if e.Resumed {
			kind = "resumed"
		}
		return fmt.Sprintf("run %s %s (%s, ammo %d, safety %s)", e.Info.ID, kind, e.Info.Mode, e.Info.Config.Ammo, onOff(e.Info.Config.Safety))
	case events.RunPaused:
		return "run paused: " + e.Reason
	case events.RunResumed:
		return strings.TrimSuffix("run resumed "+e.Reason, " ")
	case events.RunStopped:
		return "run stopping: " + e.Reason
	case events.RunFinished:
		return strings.TrimSpace(fmt.Sprintf("run finished (%s) %s", e.Reason, e.Summary))
	case events.PlanProduced:
		return fmt.Sprintf("plan from %s: %d beads under %s", e.Agent, len(e.Plan.Beads), orDash(string(e.Epic)))
	case events.BeadCreated:
		return fmt.Sprintf("bead %s created %q [%s]", e.Bead.ID, e.Bead.Title, e.Bead.State)
	case events.BeadUpdated:
		return fmt.Sprintf("bead %s updated (%s)", e.Bead.ID, strings.Join(e.Fields, ", "))
	case events.BeadStateChanged:
		return strings.TrimSpace(fmt.Sprintf("bead %s %s → %s %s", e.Bead, orDash(string(e.From)), e.To, parens(e.Reason)))
	case events.GraphChanged:
		return fmt.Sprintf("graph changed: %d beads", len(e.Beads))
	case events.AgentSpawned:
		who := fmt.Sprintf("%s(%s)", roleOf(e.Agent), cartridgeOf(e.Agent))
		if e.Agent.Bead == "" {
			return fmt.Sprintf("fired %s to %s", who, e.Task)
		}
		return fmt.Sprintf("fired %s at %s", who, target(e.Agent.Bead, e.BeadTitle))
	case events.AgentOutput:
		return fmt.Sprintf("%s %s: %s", e.Agent, e.Stream, e.Line)
	case events.AgentFinished:
		return fmt.Sprintf("%s finished in %s (exit %d, %s)", e.Agent, dur(e.Duration), e.ExitCode, orDash(string(e.Produced)))
	case events.AgentFailed:
		return strings.TrimSpace(fmt.Sprintf("%s failed: %s %s", e.Agent, e.Failure, parens(e.Err)))
	case events.RoundStarted:
		return fmt.Sprintf("%s round %d (%s) lenses %s", e.Bead, e.Round, e.RoundKind, lensList(e.Lenses))
	case events.VerdictReceived:
		return fmt.Sprintf("%s round %d: %s says %s (%d items)", e.Bead, e.Round, orDash(string(e.Verdict.Lens)), e.Verdict.Outcome, len(e.Verdict.Items))
	case events.ResponseReceived:
		if len(e.Responses) == 0 && e.Report != nil {
			return fmt.Sprintf("%s round %d: %s reported %q", e.Bead, e.Round, e.Agent, e.Report.Summary)
		}
		return fmt.Sprintf("%s round %d: %s answered %d items", e.Bead, e.Round, e.Agent, len(e.Responses))
	case events.ArbitrationReceived:
		if e.Escalation != nil {
			return fmt.Sprintf("%s round %d: PO escalation %s", e.Bead, e.Round, e.Escalation.Outcome)
		}
		return fmt.Sprintf("%s round %d: PO ruled on %d items", e.Bead, e.Round, len(e.Rulings))
	case events.PROpened:
		kind := "PR"
		if e.External {
			kind = "external PR"
		}
		return fmt.Sprintf("%s %s #%d %s", e.Bead, kind, e.Number, e.URL)
	case events.Merged:
		return strings.TrimSpace(fmt.Sprintf("%s merged %s into %s", e.Bead, shortSHA(e.SHA), orDash(e.Branch)))
	case events.Warning:
		return strings.TrimSpace(fmt.Sprintf("warning [%s] %s %s", orDash(e.Source), e.Message, parens(e.Err)))
	case events.Error:
		return strings.TrimSpace(fmt.Sprintf("error [%s] %s %s", orDash(e.Source), e.Message, parens(e.Err)))
	default:
		return string(ev.Kind())
	}
}

// logSanitize makes one rendered line safe to draw. Event text is not all
// the cannon's own: an agent's output line, a warning or error message and
// an implementation report's summary come out of a subprocess, and a
// cartridge is a CLI that writes ANSI colour, carriage returns and cursor
// moves. Lip Gloss counts every one of those as zero width, so an escape
// pasted into a row would repaint or erase the whole frame, and an embedded
// newline would silently take a second body row that is drawn without the
// cursor gutter.
//
// Each line is put through [agentsSanitize], which the Agents view already
// uses for exactly these subprocess lines (P7-03): escape sequences go, a
// carriage return is honoured the way a terminal honours it, tabs become
// spaces and the remaining control characters are dropped. A line break
// then becomes a single space, because a log row is one row. Text that
// carries nothing but printable characters is returned unchanged, so a
// benign line never pays for the walk.
func logSanitize(s string) string {
	if !strings.ContainsFunc(s, logIsControl) {
		return s
	}
	parts := strings.Split(s, "\n")
	kept := parts[:0]
	for _, p := range parts {
		if p = agentsSanitize(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}

// logIsControl reports whether r is one of the characters a terminal reads as
// an instruction rather than as text: a C0 control (ESC, CR, LF and tab
// included), DEL, or a C1 control.
func logIsControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// EventMsg carries one bus event into the Bubble Tea update loop.
type EventMsg struct {
	// Event is the event that was published.
	Event events.Event
	// Dropped is the subscription's drop counter at the time it was read.
	Dropped uint64
}

// StreamClosedMsg is delivered once, when the event subscription ends: the
// bus was closed or the context was cancelled. The TUI keeps rendering the
// state it has.
type StreamClosedMsg struct{}

// StopRequestedMsg is emitted when the user confirms a stop. [Options.OnStop]
// is called before it is delivered; P7-10 replaces the hook with the real
// orchestrator call.
type StopRequestedMsg struct{}

// Model is the Bubble Tea model of the whole TUI: header, tab bar, main
// pane, footer, help overlay and stop confirmation. Build it with [New].
type Model struct {
	opts   Options
	keys   KeyMap
	theme  *Theme
	help   help.Model
	styles styles
	state  *State
	sub    subscription

	// The per-tab views (P7-02…P7-05) and the animation layer (P7-06).
	// Model owns them, forwards resizes, events and keys to them, and
	// renders the active one in the main pane.
	graph    GraphModel
	agents   AgentsModel
	feedback FeedbackModel
	prs      PRsModel
	anim     Animator

	tab           Tab
	cursor        int
	width, height int
	flash         string
	flashErr      bool
	follow        bool
	showHelp      bool
	confirmStop   bool
	streamClosed  bool
	quitting      bool
}

// Init implements [tea.Model]: it starts pumping the event subscription.
func (m Model) Init() tea.Cmd {
	return waitForEvent(m.sub)
}

// Update implements [tea.Model].
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if a, cmd, ok := m.anim.Update(msg); ok {
		m.anim = a
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.Width = msg.Width
		m.resizeViews()
		return m, nil
	case EventMsg:
		m.state.Apply(msg.Event)
		m.state.Dropped = msg.Dropped
		m.agents = m.agents.Apply(msg.Event)
		// A spawn starts the Agents view's spinner ticker, which is also
		// what keeps the elapsed time on a live card moving (P7-03).
		var agentsCmd tea.Cmd
		m.agents, agentsCmd = m.agents.Wake(m.state)
		var animCmd tea.Cmd
		// Aim before firing: the animation layer asks the graph view where
		// the event's bead is drawn right now (P7-06).
		m.anim, animCmd = m.anim.SetTargets(m.animTargets()).Apply(msg.Event)
		m.cursor = min(m.cursor, max(m.rows()-1, 0))
		return m, tea.Batch(waitForEvent(m.sub), agentsCmd, animCmd)
	case AgentsTickMsg:
		a, cmd, ok := m.agents.Tick(msg, m.state)
		if !ok {
			return m, nil
		}
		m.agents = a
		return m, cmd
	case StreamClosedMsg:
		m.streamClosed = true
		return m, nil
	case URLOpenedMsg:
		return m.opened(msg), nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleKey routes a key press through the overlays first: a confirmation
// prompt swallows everything but its answers, and the help overlay is closed
// by any of `?`, `esc` and `q`.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := m.keys
	m.flash, m.flashErr = "", false
	if key.Matches(msg, k.ForceQuit) {
		m.quitting = true
		return m, tea.Quit
	}
	if m.confirmStop {
		switch {
		case key.Matches(msg, k.Confirm):
			m.confirmStop = false
			m.quitting = true
			if m.opts.OnStop != nil {
				m.opts.OnStop()
			}
			return m, tea.Sequence(func() tea.Msg { return StopRequestedMsg{} }, tea.Quit)
		case key.Matches(msg, k.Cancel), key.Matches(msg, k.Quit):
			m.confirmStop = false
		}
		return m, nil
	}
	if m.showHelp {
		switch {
		case key.Matches(msg, k.Help), key.Matches(msg, k.Cancel), key.Matches(msg, k.Quit):
			m.showHelp = false
		}
		return m, nil
	}
	if mm, cmd, ok := m.updateView(msg); ok {
		return mm, cmd
	}
	switch {
	case key.Matches(msg, k.Help):
		m.showHelp = true
	case key.Matches(msg, k.Quit):
		if m.state.Live {
			m.confirmStop = true
			return m, nil
		}
		m.quitting = true
		return m, tea.Quit
	case key.Matches(msg, k.NextTab):
		m.selectTab(Tab((int(m.tab) + 1) % len(tabNames)))
	case key.Matches(msg, k.PrevTab):
		m.selectTab(Tab((int(m.tab) + len(tabNames) - 1) % len(tabNames)))
	case key.Matches(msg, k.Up):
		m.cursor, m.follow = max(m.cursorRow()-1, 0), false
	case key.Matches(msg, k.Down):
		m.cursor, m.follow = min(m.cursorRow()+1, max(m.rows()-1, 0)), false
	default:
		if t, ok := tabFor(msg, k); ok {
			m.selectTab(t)
		}
	}
	return m, nil
}

// selectTab switches view, putting the cursor back at the top and turning
// follow mode on again.
func (m *Model) selectTab(t Tab) {
	m.tab, m.cursor, m.follow = t, 0, true
	m.anim = m.anim.SetTab(t)
}

// resizeViews hands the current pane size to every view, and the whole
// terminal size to the animation layer, which draws over the chrome too.
func (m *Model) resizeViews() {
	w, h := m.size()
	pane := max(h-chromeHeight, 1)
	m.graph = m.graph.Resize(w, pane)
	m.agents = m.agents.Resize(w, pane)
	m.feedback = m.feedback.Resize(w, pane)
	m.prs = m.prs.Resize(w, pane)
	m.anim = m.anim.Resize(w, h)
}

// updateView offers a key press to the view that owns the active tab. The
// bool reports whether the view consumed it; when it did not, the global
// bindings get their turn.
func (m Model) updateView(msg tea.Msg) (Model, tea.Cmd, bool) {
	switch m.tab {
	case TabGraph:
		g, ok := m.graph.Update(msg, m.state, m.keys)
		m.graph = g
		return m, nil, ok
	case TabAgents:
		a, ok := m.agents.Update(msg, m.state, m.keys)
		m.agents = a
		return m, nil, ok
	case TabFeedback:
		f, ok := m.feedback.Update(msg, m.state, m.keys)
		m.feedback = f
		return m, nil, ok
	case TabPRs:
		p, cmd, ok := m.prs.Update(msg, m.state, m.keys)
		m.prs = p
		return m, cmd, ok
	default:
		return m, nil, false
	}
}

// cursorRow is the row the cursor is really on: the view's own cursor when
// the active tab has a view component, the last row while the log view is
// following the stream, and the clamped cursor otherwise.
func (m Model) cursorRow() int {
	switch m.tab {
	case TabGraph:
		return m.graph.Cursor()
	case TabAgents:
		return m.agents.Cursor()
	case TabFeedback:
		return m.feedback.Cursor()
	case TabPRs:
		return m.prs.Cursor()
	}
	last := max(m.rows()-1, 0)
	if m.tab == TabLog && m.follow {
		return last
	}
	return min(m.cursor, last)
}

// View implements [tea.Model]: header, tab bar, main pane, footer, with the
// help or confirmation overlay in place of the pane when one is up.
func (m Model) View() string {
	if m.quitting {
		return ""
	}
	w, h := m.size()
	body := m.body(w, h-chromeHeight)
	frame := m.anim.Overlay(strings.Join([]string{
		m.header(w),
		m.tabBar(w),
		m.styles.Rule.Render(strings.Repeat("─", max(w, 1))),
		body,
		m.footer(w),
	}, "\n"))
	return clipLines(frame, m.height)
}

// clipLines drops everything below the first h lines. [Model.size] gives the
// layout a floor of chromeHeight+1 rows so the chrome is always drawn; on a
// terminal shorter than that the frame would otherwise be taller than the
// screen, which scrolls the display (P7-09). A non-positive h means the size
// is not known yet, so nothing is clipped.
func clipLines(frame string, h int) string {
	if h <= 0 {
		return frame
	}
	lines := strings.Split(frame, "\n")
	if len(lines) <= h {
		return frame
	}
	return strings.Join(lines[:h], "\n")
}

// chromeHeight is how many lines the header, tab bar, rule and footer take,
// leaving the rest of the terminal to the main pane.
const chromeHeight = 4

// size returns the terminal size, falling back to 80×24 before the first
// [tea.WindowSizeMsg] arrives.
func (m Model) size() (int, int) {
	w, h := m.width, m.height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	return w, max(h, chromeHeight+1)
}

// header renders the cannon, run id, mode, ammo used/total, safety and the
// run status. P7-06 animates the cannon sprite; the layout keeps its width
// fixed so it can.
//
// The widest form of the line does not fit an 80-column terminal — the size
// the golden frames and the Phase 7 exit criteria pin — so the header is
// composed at the widest of [Model.headerVariants] that fits: what goes
// first is what the reader can do without (the "slop-cannon" title, which
// the sprite beside it already says; then the "run" label, then the finish
// reason), and every field P7-01 requires survives to 80 columns.
func (m Model) header(w int) string {
	cannon := m.anim.Cannon()
	sep := m.styles.Sep.Render(" · ")
	avail := max(w-lipgloss.Width(cannon), 0)
	variants := m.headerVariants()
	line := ""
	for i, fields := range variants {
		line = strings.Join(fields, sep)
		if lipgloss.Width(line) <= avail || i == len(variants)-1 {
			break
		}
	}
	return clip(cannon+line, w)
}

// headerVariants lists the header's field sets from widest to narrowest.
// [Model.header] draws the first one that fits the terminal, and clips the
// last one when even that does not.
func (m Model) headerVariants() [][]string {
	used, total := m.state.Ammo()
	title := m.styles.Title.Render("slop-cannon")
	id := m.styles.Value.Render(orDash(m.state.Run.ID.String()))
	mode := m.styles.Value.Render(m.state.Mode().String())
	ammo := "ammo " + m.styles.Value.Render(fmt.Sprintf("%d/%d", used, total))
	safety := "safety " + m.styles.Value.Render(onOff(m.state.Run.Config.Safety))
	long, short := m.status()
	full, brief := "", ""
	if long != "" {
		full, brief = m.styles.Status.Render(long), m.styles.Status.Render(short)
	}
	return [][]string{
		headerFields(title, "run "+id, mode, ammo, safety, full),
		headerFields("run "+id, mode, ammo, safety, full),
		headerFields(id, mode, ammo, safety, full),
		headerFields(id, mode, ammo, safety, brief),
		headerFields(id, ammo, safety, brief),
		headerFields(id, safety, brief),
		headerFields(id, safety),
		headerFields(id),
	}
}

// headerFields drops the empty fields of a header variant, so a run with no
// status word does not render a dangling separator.
func headerFields(fields ...string) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// CannonSprite is the static cannon drawn in the header. P7-06 replaces it
// with animated frames.
const CannonSprite = "▄█▓═▶"

// status is the run-status word shown in the header: long is the full form
// and short the one a narrow terminal falls back to. Both are empty while
// the run is simply running.
//
// A finished run is reported first because it is the last word on the run. A
// stream that ended comes next, ahead of paused and stopping: once the
// events stop arriving, "stopping" would be a state the TUI can no longer
// vouch for, and the reader needs to know the screen has gone stale.
func (m Model) status() (long, short string) {
	switch {
	case m.state.Finish != nil:
		return "finished: " + m.state.Finish.Reason.String(), "finished"
	case m.streamClosed:
		return "disconnected", "disconnected"
	case m.state.Stopping:
		return "stopping", "stopping"
	case m.state.Paused:
		return "paused", "paused"
	default:
		return "", ""
	}
}

// tabBar renders the view labels with the active one highlighted.
func (m Model) tabBar(w int) string {
	parts := make([]string, 0, len(tabNames))
	for i, t := range Tabs() {
		label := fmt.Sprintf(" %d %s ", i+1, t)
		if t == m.tab {
			parts = append(parts, m.styles.TabActive.Render(label))
			continue
		}
		parts = append(parts, m.styles.Tab.Render(label))
	}
	return clip(lipgloss.JoinHorizontal(lipgloss.Top, parts...), w)
}

// footer renders the short keymap plus the dropped-event notice and the
// result of the last action that had one to report.
func (m Model) footer(w int) string {
	line := m.help.ShortHelpView(m.keys.ShortHelp())
	if m.state.Dropped > 0 {
		line += m.styles.Sep.Render("  ") + m.styles.Warn.Render(fmt.Sprintf("missed %d events", m.state.Dropped))
	}
	if m.flash != "" {
		style := m.styles.Muted
		if m.flashErr {
			style = m.styles.Warn
		}
		line += m.styles.Sep.Render("  ") + style.Render(m.flash)
	}
	return clip(line, w)
}

// opened records the result of the PRs view's `o` key (P7-05): the footer
// says what happened until the next key press, and a failure is also kept in
// the run's notices, so a headless browser or a missing xdg-open is visible
// rather than a key that silently does nothing.
func (m Model) opened(msg URLOpenedMsg) Model {
	url := logSanitize(msg.URL)
	if msg.Err == nil {
		m.flash, m.flashErr = "opened "+url, false
		return m
	}
	m.flash, m.flashErr = "could not open "+url, true
	m.state.appendNotice(Notice{
		At:      m.now(),
		Kind:    events.KindError,
		Source:  "tui",
		Message: "could not open " + url,
		Err:     msg.Err.Error(),
	})
	return m
}

// now is the model's clock: [Options.Now] when the caller pinned one (the
// golden-frame tests do), otherwise the wall clock.
func (m Model) now() time.Time {
	if m.opts.Now != nil {
		return m.opts.Now()
	}
	return time.Now()
}

// body renders the main pane: an overlay when one is up, otherwise the
// current view, padded or truncated to exactly h lines.
func (m Model) body(w, h int) string {
	h = max(h, 1)
	var content string
	switch {
	case m.showHelp:
		content = m.helpOverlay(w, h)
	case m.confirmStop:
		content = m.confirmOverlay(w, h)
	default:
		content = m.view(w, h)
	}
	return fit(content, w, h)
}

// helpOverlay renders the `?` overlay: the full keymap in a box, plus the
// keys of the active view under their own title.
func (m Model) helpOverlay(w, h int) string {
	full := m.help.FullHelpView(m.keys.FullHelp())
	if cols := m.viewKeys(); len(cols) > 0 {
		full += "\n\n" + m.styles.Title.Render(m.tab.String()+" keys") + "\n" + m.help.FullHelpView(cols)
	}
	box := m.styles.Box.Render(m.styles.Title.Render("Keys") + "\n\n" + full)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, box)
}

// viewKeys returns the extra key columns of the active view, so the overlay
// documents the bindings only that view has instead of the global map alone:
// the Graph view's level and detail keys (P7-02), the Agents view's
// transcript keys (P7-03), the Feedback view's round keys (P7-04) and the
// PRs view's `o` (P7-05). Every view that adds a binding is listed, because
// docs/tui.md promises the overlay carries "the keys of the active view" —
// and on the Graph tab `h`/`l` shadow the global previous/next view, which
// nothing else would explain. The Log view adds none: it is drawn by
// [Model.view] itself and moves under the global cursor keys.
//
// The columns are rendered as their own block rather than appended to the
// global ones, which would make the box wider than a narrow terminal.
func (m Model) viewKeys() [][]key.Binding {
	switch m.tab {
	case TabGraph:
		return m.graph.Keys().FullHelp()
	case TabAgents:
		return m.agents.Keys().FullHelp()
	case TabFeedback:
		return m.feedback.Keys().FullHelp()
	case TabPRs:
		return m.prs.Keys().FullHelp()
	}
	return nil
}

// confirmOverlay renders the stop confirmation raised by `q` on a live run.
func (m Model) confirmOverlay(w, h int) string {
	msg := fmt.Sprintf("Stop run %s?", orDash(m.state.Run.ID.String()))
	used, _ := m.state.Ammo()
	detail := fmt.Sprintf("%d agent(s) will be killed and their leases released.", used)
	box := m.styles.Box.Render(m.styles.Title.Render(msg) + "\n" + detail + "\n\n" + m.styles.Value.Render("y") + " stop   " + m.styles.Value.Render("n") + " keep firing")
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, box)
}

// view renders the current tab: the view component that owns it, or the
// one-line-per-row list the log still uses.
func (m Model) view(w, h int) string {
	switch m.tab {
	case TabGraph:
		return m.graph.View(m.state)
	case TabAgents:
		return m.agents.View(m.state)
	case TabFeedback:
		return m.feedback.View(m.state)
	case TabPRs:
		return m.prs.View(m.state)
	}
	rows := m.logRows()
	if len(rows) == 0 {
		return m.styles.Muted.Render(emptyText[TabLog])
	}
	cursor := m.cursorRow()
	start := 0
	if h > 0 && cursor >= h {
		start = cursor - h + 1
	}
	end := min(start+h, len(rows))
	out := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		line := clip(rows[i], w-2)
		if i == cursor {
			out = append(out, m.styles.Cursor.Render("▌ ")+line)
			continue
		}
		out = append(out, "  "+line)
	}
	return strings.Join(out, "\n")
}

// emptyText is what a view shows before its first event arrives.
var emptyText = [...]string{
	"no beads yet — the product owner is still planning",
	"no agents fired yet",
	"no review rounds yet",
	"no deliveries yet",
	"no events yet",
}

// rows returns how many rows the model's own cursor may move over, which is
// only ever the log view's: every other tab is drawn by the view component
// that owns it and keeps its own cursor (see [Model.cursorRow]), so it
// reports none here. It counts the log rather than formatting it, so an
// event does not build a row list nothing renders.
func (m Model) rows() int {
	if m.tab != TabLog {
		return 0
	}
	return len(m.state.Log)
}

// logRows renders one line per log entry. The log is the only view Model
// still draws itself — the graph, agents, feedback and PRs tabs are
// dispatched to their own components by [Model.view] (P7-02…P7-05).
func (m Model) logRows() []string {
	out := make([]string, 0, len(m.state.Log))
	for _, l := range m.state.Log {
		out = append(out, fmt.Sprintf("%s %-20s %s", l.At.Format("15:04:05"), l.Kind, l.Text))
	}
	return out
}

// State returns the folded event state. Later views read it; nothing outside
// the update loop should write to it.
func (m Model) State() *State { return m.state }

// Tab returns the view currently shown.
func (m Model) Tab() Tab { return m.tab }

// Cursor returns the selected row of the current view. The log view follows
// the stream until the cursor is moved, so it reports the newest row.
func (m Model) Cursor() int { return m.cursorRow() }

// Following reports whether the log view is still following the stream.
func (m Model) Following() bool { return m.follow }

// Size returns the last terminal size the model was told about.
func (m Model) Size() (int, int) { return m.width, m.height }

// HelpVisible reports whether the `?` overlay is up.
func (m Model) HelpVisible() bool { return m.showHelp }

// ConfirmingStop reports whether the stop confirmation is up.
func (m Model) ConfirmingStop() bool { return m.confirmStop }

// Keys returns the model's key bindings.
func (m Model) Keys() KeyMap { return m.keys }

// waitForEvent reads the next event from the subscription. Returning a
// blocking read as a [tea.Cmd] is the standard Bubble Tea way to bridge a
// channel; the command is re-issued for every event received.
func waitForEvent(sub subscription) tea.Cmd {
	if sub == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-sub.Events()
		if !ok {
			return StreamClosedMsg{}
		}
		return EventMsg{Event: ev, Dropped: sub.Dropped()}
	}
}

// fit pads content with blank lines and truncates it so it is exactly h
// lines of at most w columns.
func fit(content string, w, h int) string {
	lines := strings.Split(content, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	for i, l := range lines {
		lines[i] = clip(l, w)
	}
	return strings.Join(lines, "\n")
}

// clip truncates a rendered line to w columns, ANSI sequences included. The
// style it uses is bound to a pinned renderer and sets no colour, so the
// truncation cannot pick up the global renderer's profile (P7-09).
func clip(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ThemeFor(ProfileNoColor).Base().MaxWidth(w).Render(s)
}

// onOff renders a safety flag the way the README speaks about it.
func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// orDash returns s, or "-" when it is empty, so columns line up.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// parens wraps s in brackets, or returns "" for empty input.
func parens(s string) string {
	if s == "" {
		return ""
	}
	return "(" + s + ")"
}

// dur renders a duration compactly.
func dur(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	return d.Round(time.Millisecond).String()
}

// shortSHA abbreviates a commit sha to 7 characters.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return orDash(sha)
}

// roleOf renders an agent's role, with its lens for testers.
func roleOf(a domain.Agent) string {
	if a.Lens == "" {
		return orDash(a.Role.String())
	}
	return a.Role.String() + "/" + a.Lens.String()
}

// cartridgeOf renders "cartridge/model", or just the cartridge.
func cartridgeOf(a domain.Agent) string {
	if a.Model == "" {
		return orDash(a.Cartridge)
	}
	return a.Cartridge + "/" + a.Model
}

// target renders an agent's bead as `id "title"`.
func target(bead domain.BeadID, title string) string {
	if bead == "" {
		return "the plan"
	}
	if title == "" {
		return string(bead)
	}
	return fmt.Sprintf("%s %q", bead, title)
}

// lensList renders tester lenses as a comma-separated list.
func lensList(ls []domain.Lens) string {
	if len(ls) == 0 {
		return "-"
	}
	parts := make([]string, len(ls))
	for i, l := range ls {
		parts[i] = l.String()
	}
	return strings.Join(parts, ",")
}

// beadList renders bead ids as a comma-separated list.
func beadList(ids []domain.BeadID) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = string(id)
	}
	return strings.Join(parts, ",")
}
