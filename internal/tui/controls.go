package tui

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
)

// Controller is the slice of the orchestrator the run-control keys (P7-10)
// drive: pause and resume the run, stop it, re-fire a bead the run gave up
// on, close one by hand, and change the safety flag and the ammo for the
// work that has not been fired yet.
//
// It is deliberately narrow and deliberately declared here: the TUI never
// touches [State] — the folded event stream is what the orchestrator says
// happened, so a key that changed it directly would draw a run that does not
// exist. Every key calls one of these methods and then waits for the events
// the orchestrator publishes in answer.
//
// Each call is made from a [tea.Cmd] on its own goroutine with a
// [ControlsOptions.Timeout] context, so a slow bd or git call cannot block
// the frame loop; an implementation may therefore be called concurrently and
// must be safe for that. Returning an error is normal — it is shown in the
// footer, not treated as fatal.
//
// The integrator wires a real run to it with a small adapter (the run's own
// Pause/Resume/Stop take no context and no error-free variant exists for the
// rest); see the package documentation of internal/orchestrator.
type Controller interface {
	// Pause stops the scheduler firing new agents; running ones finish.
	Pause(ctx context.Context, reason string) error
	// Resume lets a paused run fire again.
	Resume(ctx context.Context, reason string) error
	// Stop asks the run to shut down gracefully.
	Stop(ctx context.Context, reason string) error
	// Refire puts a bead the run gave up on back in the ready queue; see
	// [ControlRefirable] for which states those are.
	Refire(ctx context.Context, bead domain.BeadID, reason string) error
	// MarkDone closes a bead by hand, without delivering it.
	MarkDone(ctx context.Context, bead domain.BeadID, reason string) error
	// SetSafety changes the safety flag for deliveries not yet made (D-20).
	SetSafety(ctx context.Context, on bool) error
	// SetAmmo changes the concurrency cap for ticks not yet dispatched.
	SetAmmo(ctx context.Context, ammo int) error
}

// ControlReason is the reason string the keys hand the orchestrator, so an
// event log can tell a pause a human asked for from one a cost cap forced.
const ControlReason = "user"

// ControlTimeout bounds one [Controller] call. It is generous because a
// re-fire writes to bd, and mean because the key must feel like a key: a
// call that has not answered by then reports a timeout in the footer, and
// the events tell the truth if it lands late anyway.
const ControlTimeout = 5 * time.Second

// Ammo bounds the `a` prompt accepts. The floor is the configuration's own
// minimum (one PO, one engineer, one tester); the ceiling is a two-digit
// number because ammo counts concurrent subprocesses on one machine, and a
// typo of three digits is far more likely than an intention.
//
// The ceiling is a default, not a limit on the run: a caller raises it with
// [ControlsOptions.MaxAmmo] (or [Options.ControlMaxAmmo]), and a run whose
// own cap is higher — the configuration enforces only a minimum — is always
// allowed to have that number back.
const (
	ControlMinAmmo = config.MinAmmo
	ControlMaxAmmo = 99
)

// ControlAction names one thing a control key does. It is the identity of a
// request from the key press through the confirmation to the footer line
// that reports how it went.
type ControlAction string

// The control actions, in the order the keys appear in the help overlay.
const (
	// ControlPause pauses a running run (`p`).
	ControlPause ControlAction = "pause"
	// ControlResume resumes a paused run (`p` again).
	ControlResume ControlAction = "resume"
	// ControlStop stops the run (`s`, confirmed).
	ControlStop ControlAction = "stop"
	// ControlRefire fires a bead the run gave up on again (`r`).
	ControlRefire ControlAction = "refire"
	// ControlMarkDone closes a bead by hand (`x`, confirmed).
	ControlMarkDone ControlAction = "mark-done"
	// ControlSafety toggles the safety flag (`S`, confirmed).
	ControlSafety ControlAction = "safety"
	// ControlAmmo changes the ammo of future ticks (`a`, prompted).
	ControlAmmo ControlAction = "ammo"
)

// controlActions is every action, in key order.
var controlActions = []ControlAction{
	ControlPause, ControlResume, ControlStop, ControlRefire,
	ControlMarkDone, ControlSafety, ControlAmmo,
}

// ControlActions returns every action a control key can request.
func ControlActions() []ControlAction {
	out := make([]ControlAction, len(controlActions))
	copy(out, controlActions)
	return out
}

// String returns the action's name.
func (a ControlAction) String() string { return string(a) }

// Valid reports whether a names one of the actions.
func (a ControlAction) Valid() bool {
	for _, c := range controlActions {
		if a == c {
			return true
		}
	}
	return false
}

// Confirms reports whether the action asks the user before it runs. The
// three that cannot be undone from the TUI do: stopping the run kills its
// agents, marking a bead done takes it out of the run for good, and turning
// safety off lets the next delivery merge to the default branch (D-20).
func (a ControlAction) Confirms() bool {
	switch a {
	case ControlStop, ControlMarkDone, ControlSafety:
		return true
	default:
		return false
	}
}

// ControlRequest is one control key press, resolved: what to do, to which
// bead, with which value. It travels from the key through the confirmation
// overlay into the [tea.Cmd] that calls the [Controller], and back out again
// inside a [ControlResultMsg].
type ControlRequest struct {
	// Action is what the key asked for.
	Action ControlAction
	// Bead is the bead the action is about, for the bead-scoped actions.
	Bead domain.BeadID
	// Title is the bead's title, already sanitised, for the confirmation.
	Title string
	// State is the bead's state when the key was pressed.
	State domain.BeadState
	// Safety is the value ControlSafety sets.
	Safety bool
	// Ammo is the value ControlAmmo sets.
	Ammo int
}

// Pending is the footer line shown while the call is in flight.
func (r ControlRequest) Pending() string {
	switch r.Action {
	case ControlPause:
		return "pausing…"
	case ControlResume:
		return "resuming…"
	case ControlStop:
		return "stopping…"
	case ControlRefire:
		return "re-firing " + string(r.Bead) + "…"
	case ControlMarkDone:
		return "marking " + string(r.Bead) + " done…"
	case ControlSafety:
		return "turning safety " + onOff(r.Safety) + "…"
	case ControlAmmo:
		return fmt.Sprintf("setting ammo to %d…", r.Ammo)
	default:
		return string(r.Action) + "…"
	}
}

// Summary is the footer line shown when the call returned no error. It says
// what was asked for rather than what happened: the orchestrator's events
// are the record of the run, and they arrive on their own.
func (r ControlRequest) Summary() string {
	switch r.Action {
	case ControlPause:
		return "run paused"
	case ControlResume:
		return "run resumed"
	case ControlStop:
		return "stop requested"
	case ControlRefire:
		return string(r.Bead) + " will be fired again"
	case ControlMarkDone:
		return string(r.Bead) + " marked done"
	case ControlSafety:
		return "safety " + onOff(r.Safety) + " for future deliveries"
	case ControlAmmo:
		return fmt.Sprintf("ammo %d for future ticks", r.Ammo)
	default:
		return string(r.Action)
	}
}

// Failure is the footer line shown when the call returned an error, without
// the error itself.
func (r ControlRequest) Failure() string {
	switch r.Action {
	case ControlPause:
		return "could not pause the run"
	case ControlResume:
		return "could not resume the run"
	case ControlStop:
		return "could not stop the run"
	case ControlRefire:
		return "could not re-fire " + string(r.Bead)
	case ControlMarkDone:
		return "could not mark " + string(r.Bead) + " done"
	case ControlSafety:
		return "could not turn safety " + onOff(r.Safety)
	case ControlAmmo:
		return fmt.Sprintf("could not set ammo to %d", r.Ammo)
	default:
		return "could not " + string(r.Action)
	}
}

// Cmd returns the command that makes the call and reports the outcome as a
// [ControlResultMsg]. It returns nil when there is no controller or the
// action is not one of the known ones, so a program with no orchestrator
// behind it (a recorded run, `sc tui-demo`) does nothing at all.
func (r ControlRequest) Cmd(c Controller, timeout time.Duration) tea.Cmd {
	if c == nil || !r.Action.Valid() {
		return nil
	}
	if timeout <= 0 {
		timeout = ControlTimeout
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return ControlResultMsg{Request: r, Err: r.call(ctx, c)}
	}
}

// call dispatches the request to the controller.
func (r ControlRequest) call(ctx context.Context, c Controller) error {
	switch r.Action {
	case ControlPause:
		return c.Pause(ctx, ControlReason)
	case ControlResume:
		return c.Resume(ctx, ControlReason)
	case ControlStop:
		return c.Stop(ctx, ControlReason)
	case ControlRefire:
		return c.Refire(ctx, r.Bead, ControlReason)
	case ControlMarkDone:
		return c.MarkDone(ctx, r.Bead, ControlReason)
	case ControlSafety:
		return c.SetSafety(ctx, r.Safety)
	case ControlAmmo:
		return c.SetAmmo(ctx, r.Ammo)
	default:
		return fmt.Errorf("tui: unknown control action %q", r.Action)
	}
}

// ControlResultMsg reports what a [Controller] call returned. A program that
// shows the footer feeds it back to [ControlsModel.Apply]; ignoring it is
// safe, and only costs the user the confirmation line.
type ControlResultMsg struct {
	// Request is the request that was made.
	Request ControlRequest
	// Err is what the controller returned, or nil on success.
	Err error
}

// Flash returns the footer line for the result and whether it is a failure.
// The error text is sanitised: a controller wraps errors from bd, git and
// the cartridges, and those carry whatever a subprocess wrote.
func (m ControlResultMsg) Flash() (string, bool) {
	if m.Err == nil {
		return m.Request.Summary(), false
	}
	return m.Request.Failure() + ": " + logSanitize(m.Err.Error()), true
}

// ControlTarget is the bead the bead-scoped keys (`r`, `x`) act on: whatever
// the active view has under its cursor. The caller resolves it, because only
// it knows which view is up; [TargetFor] fills one in from a bead id.
type ControlTarget struct {
	// Bead is the selected bead, empty when nothing is selected.
	Bead domain.BeadID
	// Title is its title, as the plan gave it.
	Title string
	// State is the state the run last reported for it.
	State domain.BeadState
}

// Empty reports whether nothing is selected.
func (t ControlTarget) Empty() bool { return t.Bead == "" }

// TargetFor looks a bead up in the run state. An unknown id still yields a
// target with that id, so a key can refuse it by name.
func TargetFor(st *State, id domain.BeadID) ControlTarget {
	t := ControlTarget{Bead: id}
	if st == nil || id == "" {
		return t
	}
	if b, ok := st.Bead(id); ok {
		t.Title, t.State = b.Title, b.State
	}
	return t
}

// controlRefirableStates are the states a bead can be put back in the queue
// from, in the order the refusal message lists them. They are the three the
// run itself parks abandoned work in — `failed` when a firing ran out of
// attempts, `needs_human` when a delivery or a `gh:pr` gate handed the bead
// to a person (D-20), and `escalated` when the review-round cap was reached
// (D-08) — plus bd's own `blocked`, which `sc` never writes (D-07) but a
// bead edited outside the run can still carry through [beads.SafeState].
//
// The P7-10 task text says "failed/blocked", which predates D-07's state
// vocabulary: `blocked` is kept for that foreign-status case, and the two
// states a run really does hand to a human were added, because refusing
// those is refusing the only situations `r` exists for.
var controlRefirableStates = []domain.BeadState{
	domain.BeadStateFailed,
	domain.BeadStateNeedsHuman,
	domain.BeadStateEscalated,
	domain.BeadStateBlocked,
}

// ControlRefirableStates returns the bead states the `r` key accepts.
func ControlRefirableStates() []domain.BeadState {
	return slices.Clone(controlRefirableStates)
}

// controlTarget resolves the bead the bead-scoped control keys (`r`, `x`)
// act on from the view that is up, which is what [ControlTarget] promises
// and what docs/tui.md's run-control table says: the Graph view has the bead
// under its cursor, the Agents view has the bead its selected agent works
// on, and the Feedback and PRs views have the bead of their selected row.
//
// The Log view has no bead under its cursor, so it yields nothing and the
// keys refuse with "select a bead first". That is deliberate: falling back
// to another tab's cursor would raise a confirmation about a bead that is
// not on the screen.
func (m Model) controlTarget() ControlTarget {
	switch m.tab {
	case TabGraph:
		return TargetFor(m.state, m.graph.SelectedID())
	case TabAgents:
		if a, ok := m.state.Agent(m.agents.SelectedID()); ok {
			return TargetFor(m.state, a.Bead)
		}
	case TabFeedback:
		if th, ok := m.feedback.Selected(m.state); ok {
			return TargetFor(m.state, th.Bead)
		}
	case TabPRs:
		if row, ok := m.prs.Selected(m.state); ok {
			return TargetFor(m.state, row.Bead)
		}
	}
	return ControlTarget{}
}

// ControlRefirable reports whether a bead in state s can be fired again.
// Only work the run gave up on qualifies (see [controlRefirableStates]);
// anything else is either in flight or finished, and re-firing it would
// duplicate an agent or undo a delivery.
func ControlRefirable(s domain.BeadState) bool {
	return slices.Contains(controlRefirableStates, s)
}

// controlRefirableList names the re-firable states for the refusal message,
// so the message can never promise a state the predicate refuses.
func controlRefirableList() string {
	names := make([]string, 0, len(controlRefirableStates))
	for _, s := range controlRefirableStates {
		names = append(names, string(s))
	}
	last := len(names) - 1
	if last <= 0 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:last], ", ") + " or " + names[last]
}

// ControlsKeyMap are the run-control bindings (P7-10). They are global —
// every view has them — but they live in their own map so [ControlsModel]
// can disable the lot when there is no orchestrator to call, which also
// takes them out of the help overlay.
type ControlsKeyMap struct {
	// PauseResume pauses a running run and resumes a paused one.
	PauseResume key.Binding
	// Stop asks the run to shut down, after a confirmation.
	Stop key.Binding
	// Refire fires a bead the run gave up on again.
	Refire key.Binding
	// MarkDone closes the selected bead by hand, after a confirmation.
	MarkDone key.Binding
	// Safety toggles the safety flag, after a confirmation.
	Safety key.Binding
	// Ammo opens the ammo prompt.
	Ammo key.Binding
	// Apply submits the ammo prompt.
	Apply key.Binding
	// Dismiss closes the ammo prompt without changing anything.
	Dismiss key.Binding
}

// DefaultControlsKeyMap returns the standard control bindings.
func DefaultControlsKeyMap() ControlsKeyMap {
	return ControlsKeyMap{
		PauseResume: key.NewBinding(
			key.WithKeys("p"),
			key.WithHelp("p", "pause / resume"),
		),
		Stop: key.NewBinding(
			key.WithKeys("s"),
			key.WithHelp("s", "stop run"),
		),
		Refire: key.NewBinding(
			key.WithKeys("r"),
			key.WithHelp("r", "re-fire bead"),
		),
		MarkDone: key.NewBinding(
			key.WithKeys("x"),
			key.WithHelp("x", "mark bead done"),
		),
		Safety: key.NewBinding(
			key.WithKeys("S"),
			key.WithHelp("S", "toggle safety"),
		),
		Ammo: key.NewBinding(
			key.WithKeys("a"),
			key.WithHelp("a", "change ammo"),
		),
		Apply: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "apply"),
		),
		Dismiss: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "cancel"),
		),
	}
}

// bindings returns pointers to every binding of the map, in help order. The
// receiver is a pointer so the pointers address the caller's map rather than
// a copy of it.
func (k *ControlsKeyMap) bindings() []*key.Binding {
	return []*key.Binding{
		&k.PauseResume, &k.Stop, &k.Refire, &k.MarkDone,
		&k.Safety, &k.Ammo, &k.Apply, &k.Dismiss,
	}
}

// setEnabled returns a copy of the map with every binding enabled or
// disabled. A disabled binding matches nothing ([key.Matches] checks it) and
// the help renderer skips it, so one call both stops the keys working and
// stops the overlay promising they do.
func (k ControlsKeyMap) setEnabled(on bool) ControlsKeyMap {
	out := k
	for _, b := range out.bindings() {
		b.SetEnabled(on)
	}
	return out
}

// Enabled reports whether the control keys are live.
func (k ControlsKeyMap) Enabled() bool { return k.PauseResume.Enabled() }

// ShortHelp implements the [help.KeyMap] interface: the keys worth a footer
// slot. The prompt keys are left out — they are shown by the prompt itself.
func (k ControlsKeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.PauseResume, k.Stop}
}

// FullHelp implements the [help.KeyMap] interface: the columns the `?`
// overlay shows under the run-control heading.
func (k ControlsKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.PauseResume, k.Stop},
		{k.Refire, k.MarkDone},
		{k.Safety, k.Ammo},
	}
}

// ControlsOptions configure the run controls.
type ControlsOptions struct {
	// NoColor renders without any ANSI colour. It is ignored when Theme is
	// set.
	NoColor bool
	// Theme overrides the palette; nil means [EnvTheme] for NoColor.
	Theme *Theme
	// Keys overrides the bindings; nil means [DefaultControlsKeyMap].
	Keys *ControlsKeyMap
	// Controller is the orchestrator the keys drive. A nil controller
	// disables every control key: a recorded run has nothing to control.
	Controller Controller
	// Timeout bounds one controller call; zero means [ControlTimeout].
	Timeout time.Duration
	// MinAmmo and MaxAmmo bound the ammo prompt; zero means
	// [ControlMinAmmo] and [ControlMaxAmmo].
	MinAmmo, MaxAmmo int
}

// ControlsModel is the run-control layer (P7-10): the `p`, `s`, `r`, `x`,
// `S` and `a` keys, the confirmations two of them and the safety toggle
// raise, the ammo prompt and the footer line that reports how a call went.
//
// It is a component like the views, and for the same reason: it holds only
// its own overlay state and calls the [Controller]. It never writes to
// [State] — the run's own events do that — and it never renders the run,
// only its own overlay.
//
// The caller offers it a key before the active view sees one, so an open
// confirmation swallows the keys the view would otherwise take, and feeds
// the [ControlResultMsg] its command produces back through
// [ControlsModel.Apply].
type ControlsModel struct {
	styles  controlsStyles
	keys    ControlsKeyMap
	ctrl    Controller
	timeout time.Duration

	minAmmo, maxAmmo int

	// pending is the request a confirmation is about; confirming says a
	// confirmation is up. prompting and buf are the ammo prompt.
	pending    ControlRequest
	confirming bool
	prompting  bool
	buf        string

	flash    string
	flashErr bool
	inflight int

	// safetySet and ammoSet are the last values the orchestrator accepted
	// for `S` and `a`; safetyBase and ammoBase are what the run's own
	// configuration said when they were asked for.
	//
	// They exist because nothing reports a configuration change back: the
	// events have no kind for one, so [State.Run] keeps the snapshot
	// [events.RunStarted] carried for the life of the run (P7-20 is the
	// orchestrator half of these keys). Without the override `S` would ask
	// for the same flip for ever — safety could be turned off but never back
	// on — and `a` could never restore a value it had already set, because
	// the prompt would re-offer the stale cap and then refuse it as
	// unchanged.
	//
	// The run still wins the moment it says anything: the override applies
	// only while the run's own value is the one it was taken against, so an
	// event that finally moves the snapshot drops it.
	safetyKnown bool
	safetySet   bool
	safetyBase  bool
	ammoSet     int
	ammoBase    int
}

// NewControlsModel builds the control layer. The zero [ControlsOptions] is
// valid and yields a layer with no controller, which consumes nothing.
func NewControlsModel(opts ControlsOptions) ControlsModel {
	theme := opts.Theme
	if theme == nil {
		theme = EnvTheme(opts.NoColor)
	}
	keys := DefaultControlsKeyMap()
	if opts.Keys != nil {
		keys = *opts.Keys
	}
	keys = keys.setEnabled(opts.Controller != nil)
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = ControlTimeout
	}
	minAmmo, maxAmmo := opts.MinAmmo, opts.MaxAmmo
	if minAmmo <= 0 {
		minAmmo = ControlMinAmmo
	}
	if maxAmmo < minAmmo {
		maxAmmo = max(ControlMaxAmmo, minAmmo)
	}
	return ControlsModel{
		styles:  newControlsStyles(theme),
		keys:    keys,
		ctrl:    opts.Controller,
		timeout: timeout,
		minAmmo: minAmmo,
		maxAmmo: maxAmmo,
	}
}

// Keys returns the control bindings, disabled when there is no controller.
func (v ControlsModel) Keys() ControlsKeyMap { return v.keys }

// Enabled reports whether a controller is attached, which is what decides
// whether the keys do anything.
func (v ControlsModel) Enabled() bool { return v.ctrl != nil }

// Active reports whether an overlay of this layer is up: the caller draws
// [ControlsModel.Overlay] instead of the view when it is.
func (v ControlsModel) Active() bool { return v.confirming || v.prompting }

// Confirming returns the request a confirmation is waiting on.
func (v ControlsModel) Confirming() (ControlRequest, bool) {
	return v.pending, v.confirming
}

// Prompting reports whether the ammo prompt is up.
func (v ControlsModel) Prompting() bool { return v.prompting }

// AmmoInput returns what has been typed into the ammo prompt so far.
func (v ControlsModel) AmmoInput() string { return v.buf }

// AmmoBounds returns the smallest and largest ammo the prompt accepts.
func (v ControlsModel) AmmoBounds() (min, max int) { return v.minAmmo, v.maxAmmo }

// Flash returns the footer line of the last control key and whether it
// reports a failure. It is empty when there is nothing to say.
func (v ControlsModel) Flash() (string, bool) { return v.flash, v.flashErr }

// ClearFlash drops that line. A program calls it where it clears its own
// footer flash — on the next key press — so a control's answer cannot
// outlive the key after it and shadow everything a view has to say for the
// rest of the run.
func (v ControlsModel) ClearFlash() ControlsModel {
	v.flash, v.flashErr = "", false
	return v
}

// Safety returns the safety flag the next `S` press would flip: the value
// the run published, or the last one the orchestrator accepted from this
// layer while the run's own value has not moved since.
func (v ControlsModel) Safety(st *State) bool {
	base := controlsSafety(st)
	if v.safetyKnown && base == v.safetyBase {
		return v.safetySet
	}
	return base
}

// Ammo returns the cap the `a` prompt starts from and measures a new value
// against: the run's own, or the last one the orchestrator accepted while
// the run's own has not moved since.
func (v ControlsModel) Ammo(st *State) int {
	base := controlsAmmo(st)
	if v.ammoSet > 0 && base == v.ammoBase {
		return v.ammoSet
	}
	return base
}

// maxAmmoFor is the largest ammo the prompt accepts for this run: the
// configured ceiling, the run's own cap, or the last cap the orchestrator
// accepted — whichever is largest. The configuration enforces only a
// minimum, so a run fired with more ammo than the ceiling must still be able
// to get its own number back, including after it has been lowered.
func (v ControlsModel) maxAmmoFor(st *State) int {
	return max(v.maxAmmo, controlsAmmo(st), v.Ammo(st))
}

// InFlight returns how many controller calls have been made and not yet
// answered. It is what a caller can show as a spinner, and what a test uses
// to know a command is still outstanding.
func (v ControlsModel) InFlight() int { return v.inflight }

// Update handles one message and reports whether the control layer consumed
// it. st is the run as the events describe it (read, never written), keys is
// the global map, whose Confirm and Cancel answer the confirmations, and
// target is the bead the active view has under its cursor — [TargetFor]
// builds one.
//
// It consumes every key while one of its overlays is up, and otherwise only
// its own; a caller can therefore offer it a key before the active view sees
// one, which is what makes a confirmation modal.
func (v ControlsModel) Update(msg tea.Msg, st *State, keys KeyMap, target ControlTarget) (ControlsModel, tea.Cmd, bool) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return v, nil, false
	}
	switch {
	case v.confirming:
		return v.answer(km, keys)
	case v.prompting:
		return v.typeAmmo(km, st)
	case v.ctrl == nil:
		return v, nil, false
	}
	switch {
	case key.Matches(km, v.keys.PauseResume):
		return v.pauseResume(st)
	case key.Matches(km, v.keys.Stop):
		return v.stop(st)
	case key.Matches(km, v.keys.Refire):
		return v.refire(st, target)
	case key.Matches(km, v.keys.MarkDone):
		return v.markDone(st, target)
	case key.Matches(km, v.keys.Safety):
		return v.safety(st)
	case key.Matches(km, v.keys.Ammo):
		return v.openAmmo(st)
	}
	return v, nil, false
}

// Apply folds a controller's answer back in: the call is no longer in
// flight, the footer says how it went, and a configuration change the
// orchestrator accepted is remembered, because no event will report it back
// (see [ControlsModel]'s safetySet and ammoSet).
func (v ControlsModel) Apply(msg ControlResultMsg) ControlsModel {
	v.inflight = max(v.inflight-1, 0)
	v.flash, v.flashErr = msg.Flash()
	if msg.Err != nil {
		return v
	}
	switch msg.Request.Action {
	case ControlSafety:
		v.safetyKnown, v.safetySet = true, msg.Request.Safety
	case ControlAmmo:
		v.ammoSet = msg.Request.Ammo
	}
	return v
}

// pauseResume asks for the opposite of the run's current pause state.
func (v ControlsModel) pauseResume(st *State) (ControlsModel, tea.Cmd, bool) {
	if reason, ok := v.refuse(st); !ok {
		return v.deny(reason), nil, true
	}
	action := ControlPause
	if st.Paused {
		action = ControlResume
	}
	return v.request(ControlRequest{Action: action})
}

// stop raises the stop confirmation.
func (v ControlsModel) stop(st *State) (ControlsModel, tea.Cmd, bool) {
	if reason, ok := v.refuse(st); !ok {
		return v.deny(reason), nil, true
	}
	return v.request(ControlRequest{Action: ControlStop})
}

// refire asks for the selected bead to be fired again, refusing anything
// that is not one of the states [ControlRefirable] takes.
func (v ControlsModel) refire(st *State, t ControlTarget) (ControlsModel, tea.Cmd, bool) {
	if reason, ok := v.refuse(st); !ok {
		return v.deny(reason), nil, true
	}
	if t.Empty() {
		return v.deny("select a bead first"), nil, true
	}
	if !ControlRefirable(t.State) {
		return v.deny(fmt.Sprintf("%s is %s — only a %s bead can be re-fired", t.Bead, orDash(string(t.State)), controlRefirableList())), nil, true
	}
	return v.request(controlBeadRequest(ControlRefire, t))
}

// markDone raises the confirmation for closing the selected bead by hand.
func (v ControlsModel) markDone(st *State, t ControlTarget) (ControlsModel, tea.Cmd, bool) {
	if reason, ok := v.refuse(st); !ok {
		return v.deny(reason), nil, true
	}
	if t.Empty() {
		return v.deny("select a bead first"), nil, true
	}
	if t.State == domain.BeadStateClosed {
		return v.deny(string(t.Bead) + " is already closed"), nil, true
	}
	return v.request(controlBeadRequest(ControlMarkDone, t))
}

// safety raises the confirmation for flipping the safety flag.
func (v ControlsModel) safety(st *State) (ControlsModel, tea.Cmd, bool) {
	if reason, ok := v.refuse(st); !ok {
		return v.deny(reason), nil, true
	}
	want := !v.Safety(st)
	v.safetyBase = controlsSafety(st)
	return v.request(ControlRequest{Action: ControlSafety, Safety: want})
}

// openAmmo opens the ammo prompt, pre-filled with the run's current ammo so
// the common change is one keystroke away from what is already there.
func (v ControlsModel) openAmmo(st *State) (ControlsModel, tea.Cmd, bool) {
	if reason, ok := v.refuse(st); !ok {
		return v.deny(reason), nil, true
	}
	v.prompting, v.buf = true, ""
	total, ceiling := v.Ammo(st), v.maxAmmoFor(st)
	v.ammoBase = controlsAmmo(st)
	if total >= v.minAmmo && total <= ceiling {
		v.buf = strconv.Itoa(total)
	}
	v.flash, v.flashErr = "", false
	return v, nil, true
}

// controlBeadRequest builds a bead-scoped request with a title that is safe
// to draw: a bead title comes from the product owner, which is a subprocess.
func controlBeadRequest(a ControlAction, t ControlTarget) ControlRequest {
	return ControlRequest{Action: a, Bead: t.Bead, Title: logSanitize(t.Title), State: t.State}
}

// request either raises the confirmation for a request or dispatches it.
func (v ControlsModel) request(req ControlRequest) (ControlsModel, tea.Cmd, bool) {
	if req.Action.Confirms() {
		v.pending, v.confirming = req, true
		v.flash, v.flashErr = "", false
		return v, nil, true
	}
	return v.dispatch(req)
}

// dispatch makes the call, leaving the footer saying it is under way.
func (v ControlsModel) dispatch(req ControlRequest) (ControlsModel, tea.Cmd, bool) {
	cmd := req.Cmd(v.ctrl, v.timeout)
	if cmd == nil {
		return v.deny("no orchestrator to " + string(req.Action)), nil, true
	}
	v.flash, v.flashErr = req.Pending(), false
	v.inflight++
	return v, cmd, true
}

// answer handles a key while a confirmation is up: yes runs the request, no
// drops it, and everything else is swallowed so a stray key cannot leak
// through to the view underneath.
func (v ControlsModel) answer(km tea.KeyMsg, keys KeyMap) (ControlsModel, tea.Cmd, bool) {
	switch {
	case key.Matches(km, keys.Confirm):
		req := v.pending
		v.pending, v.confirming = ControlRequest{}, false
		return v.dispatch(req)
	case key.Matches(km, keys.Cancel), key.Matches(km, v.keys.Dismiss):
		v.pending, v.confirming = ControlRequest{}, false
		v.flash, v.flashErr = "cancelled", false
	}
	return v, nil, true
}

// typeAmmo handles a key while the ammo prompt is up. Only digits, delete,
// enter and escape mean anything; the rest are swallowed, prompt-style.
//
// The prompt does not answer to the global Confirm and Cancel bindings the
// confirmations use, because those are `y` and `n`: in a field the user
// types a number into, a letter must not submit it.
func (v ControlsModel) typeAmmo(km tea.KeyMsg, st *State) (ControlsModel, tea.Cmd, bool) {
	switch {
	case key.Matches(km, v.keys.Apply):
		ceiling := v.maxAmmoFor(st)
		ammo, err := strconv.Atoi(v.buf)
		if v.buf == "" || err != nil || ammo < v.minAmmo || ammo > ceiling {
			v.flash, v.flashErr = fmt.Sprintf("ammo must be a number between %d and %d", v.minAmmo, ceiling), true
			return v, nil, true
		}
		if v.Ammo(st) == ammo {
			v.prompting, v.buf = false, ""
			v.flash, v.flashErr = fmt.Sprintf("ammo is already %d", ammo), false
			return v, nil, true
		}
		v.prompting, v.buf = false, ""
		return v.dispatch(ControlRequest{Action: ControlAmmo, Ammo: ammo})
	case key.Matches(km, v.keys.Dismiss):
		v.prompting, v.buf = false, ""
		v.flash, v.flashErr = "cancelled", false
	case km.Type == tea.KeyBackspace || km.Type == tea.KeyDelete:
		if v.buf != "" {
			v.buf = v.buf[:len(v.buf)-1]
		}
		v.flash, v.flashErr = "", false
	case km.Type == tea.KeyRunes:
		v.buf, v.flash, v.flashErr = controlAppendDigits(v.buf, km.Runes, v.maxAmmoFor(st)), "", false
	}
	return v, nil, true
}

// controlAppendDigits appends the digits of runes to buf, up to as many
// digits as the largest allowed ammo has. Anything else is ignored.
func controlAppendDigits(buf string, runes []rune, maxAmmo int) string {
	limit := len(strconv.Itoa(maxAmmo))
	for _, r := range runes {
		if r < '0' || r > '9' || len(buf) >= limit {
			continue
		}
		buf += string(r)
	}
	return buf
}

// refuse reports whether the run can be controlled at all, and why not when
// it cannot.
func (v ControlsModel) refuse(st *State) (string, bool) {
	switch {
	case st == nil:
		return "no run to control", false
	case st.Finish != nil, !st.Live:
		return "the run has finished", false
	case st.Stopping:
		return "the run is stopping", false
	default:
		return "", true
	}
}

// deny records a refusal in the footer without calling anything.
func (v ControlsModel) deny(reason string) ControlsModel {
	v.flash, v.flashErr = reason, true
	return v
}

// Overlay renders the confirmation or the ammo prompt, centred in a w×h
// pane, and reports whether there was one to draw. A caller renders it in
// place of the active view, the way the help overlay is rendered.
func (v ControlsModel) Overlay(st *State, w, h int) (string, bool) {
	w, h = max(w, 1), max(h, 1)
	var box string
	switch {
	case v.confirming:
		box = v.confirmBox(st, v.pending, w)
	case v.prompting:
		box = v.ammoBox(st, w)
	default:
		return "", false
	}
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, box), true
}

// controlsBoxInset is how many columns the overlay box spends on its border
// and padding, which the text inside it has to give back.
const controlsBoxInset = 6

// confirmBox renders the confirmation of one request: what it will do, what
// that costs, and the two keys that answer it.
func (v ControlsModel) confirmBox(st *State, req ControlRequest, w int) string {
	inner := max(w-controlsBoxInset, 1)
	title, detail, yes := "", "", ""
	switch req.Action {
	case ControlStop:
		title = "Stop run " + orDash(controlsRunID(st)) + "?"
		detail = fmt.Sprintf("%d agent(s) will be killed and their leases released.", controlsLiveAgents(st))
		yes = "stop"
	case ControlMarkDone:
		title = "Mark " + string(req.Bead) + " done?"
		detail = controlsBeadLine(req) + " The cannon will not work on it again."
		yes = "mark done"
	case ControlSafety:
		title = "Turn safety " + onOff(req.Safety) + "?"
		detail = "Deliveries from now on open a pull request for a human to merge."
		if !req.Safety {
			detail = "Deliveries from now on merge straight to the default branch."
		}
		yes = "safety " + onOff(req.Safety)
	default:
		title = "Confirm " + orDash(string(req.Action)) + "?"
		yes = "yes"
	}
	lines := []string{v.styles.Title.Render(clip(title, inner))}
	if detail != "" {
		lines = append(lines, v.styles.Text.Render(clip(detail, inner)))
	}
	lines = append(lines, "", clip(v.styles.Value.Render("y")+" "+yes+"   "+v.styles.Value.Render("n")+" cancel", inner))
	return v.styles.Box.Render(strings.Join(lines, "\n"))
}

// controlsBeadLine describes the bead a mark-done confirmation is about. The
// title is already sanitised ([controlBeadRequest]) and is quoted so an
// empty one is still visibly a title.
func controlsBeadLine(req ControlRequest) string {
	if req.Title == "" {
		return fmt.Sprintf("It is %s.", orDash(string(req.State)))
	}
	return fmt.Sprintf("%q is %s.", req.Title, orDash(string(req.State)))
}

// ammoBox renders the ammo prompt: the field, the bounds and the keys.
func (v ControlsModel) ammoBox(st *State, w int) string {
	inner := max(w-controlsBoxInset, 1)
	total := v.Ammo(st)
	field := v.styles.Value.Render(v.buf) + v.styles.Cursor.Render("▏")
	lines := []string{
		v.styles.Title.Render(clip("Ammo for future ticks", inner)),
		clip(v.styles.Muted.Render("› ")+field, inner),
		v.styles.Text.Render(clip(fmt.Sprintf("%d–%d concurrent agents; the run's cap today is %d.", v.minAmmo, v.maxAmmoFor(st), total), inner)),
		"",
		clip(v.styles.Value.Render("enter")+" apply   "+v.styles.Value.Render("esc")+" cancel", inner),
	}
	if v.flashErr && v.flash != "" {
		lines = append(lines, v.styles.Warn.Render(clip(v.flash, inner)))
	}
	return v.styles.Box.Render(strings.Join(lines, "\n"))
}

// controlsRunID is the run's id, empty when nothing has been seen yet.
func controlsRunID(st *State) string {
	if st == nil {
		return ""
	}
	return st.Run.ID.String()
}

// controlsAmmo is the run's ammo cap, zero when nothing has been seen yet.
// It is the run's own number: [ControlsModel.Ammo] is what a key reads,
// because it also knows about a change the orchestrator has accepted since.
func controlsAmmo(st *State) int {
	if st == nil {
		return 0
	}
	_, total := st.Ammo()
	return total
}

// controlsSafety is the run's safety flag, false when nothing has been seen
// yet. [ControlsModel.Safety] is what a key reads, for the same reason.
func controlsSafety(st *State) bool {
	if st == nil {
		return false
	}
	return st.Run.Config.Safety
}

// controlsLiveAgents counts the agents a stop would kill.
func controlsLiveAgents(st *State) int {
	if st == nil {
		return 0
	}
	return len(st.LiveAgents())
}

// controlsStyles is the palette of the control overlays: the semantic
// entries of the shared [Theme] (P7-07), named for what they draw here.
type controlsStyles struct {
	// Title is the question at the top of an overlay.
	Title lipgloss.Style
	// Text is the sentence under it.
	Text lipgloss.Style
	// Value is a key name or a typed value.
	Value lipgloss.Style
	// Muted is the prompt marker.
	Muted lipgloss.Style
	// Warn is a refused input.
	Warn lipgloss.Style
	// Cursor is the caret of the ammo field.
	Cursor lipgloss.Style
	// Box frames an overlay.
	Box lipgloss.Style
}

// newControlsStyles returns the palette, bound to t's renderer so an overlay
// never depends on the ambient terminal (P7-09).
func newControlsStyles(t *Theme) controlsStyles {
	return controlsStyles{
		Title:  t.Title(),
		Text:   t.Text(),
		Value:  t.Accent(),
		Muted:  t.Muted(),
		Warn:   t.Warning(),
		Cursor: t.Accent(),
		Box:    t.Box().Padding(0, 2),
	}
}
