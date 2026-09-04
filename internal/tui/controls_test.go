package tui

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
)

// ctlRunID names the run every control fixture drives.
const ctlRunID = domain.RunID("sc-ctl-0001")

// ctlAt returns a deterministic timestamp, n minutes after a fixed start.
func ctlAt(n int) time.Time {
	return time.Date(2026, 5, 6, 11, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Minute)
}

// ctlCall is one recorded [Controller] call.
type ctlCall struct {
	Action ControlAction
	Bead   domain.BeadID
	Reason string
	Safety bool
	Ammo   int
}

// ctlController is the fake orchestrator the control keys drive. It records
// every call and can be told to fail, so a test asserts what was asked for
// rather than what a run did.
type ctlController struct {
	mu    sync.Mutex
	calls []ctlCall
	err   error
	block chan struct{}
}

// record appends one call and returns the configured error.
func (c *ctlController) record(call ctlCall) error {
	c.mu.Lock()
	c.calls = append(c.calls, call)
	block, err := c.block, c.err
	c.mu.Unlock()
	if block != nil {
		<-block
	}
	return err
}

// Calls returns the calls made so far.
func (c *ctlController) Calls() []ctlCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ctlCall, len(c.calls))
	copy(out, c.calls)
	return out
}

// Pause implements [Controller].
func (c *ctlController) Pause(_ context.Context, reason string) error {
	return c.record(ctlCall{Action: ControlPause, Reason: reason})
}

// Resume implements [Controller].
func (c *ctlController) Resume(_ context.Context, reason string) error {
	return c.record(ctlCall{Action: ControlResume, Reason: reason})
}

// Stop implements [Controller].
func (c *ctlController) Stop(_ context.Context, reason string) error {
	return c.record(ctlCall{Action: ControlStop, Reason: reason})
}

// Refire implements [Controller].
func (c *ctlController) Refire(_ context.Context, bead domain.BeadID, reason string) error {
	return c.record(ctlCall{Action: ControlRefire, Bead: bead, Reason: reason})
}

// MarkDone implements [Controller].
func (c *ctlController) MarkDone(_ context.Context, bead domain.BeadID, reason string) error {
	return c.record(ctlCall{Action: ControlMarkDone, Bead: bead, Reason: reason})
}

// SetSafety implements [Controller].
func (c *ctlController) SetSafety(_ context.Context, on bool) error {
	return c.record(ctlCall{Action: ControlSafety, Safety: on})
}

// SetAmmo implements [Controller].
func (c *ctlController) SetAmmo(_ context.Context, ammo int) error {
	return c.record(ctlCall{Action: ControlAmmo, Ammo: ammo})
}

// ctlRunInfo describes the run the fixtures watch.
func ctlRunInfo(safety bool, ammo int) domain.Run {
	return domain.Run{
		ID:        ctlRunID,
		RepoPath:  "/tmp/repo",
		Mode:      domain.RunModeBrief,
		Brief:     "fix the retry bugs",
		StartedAt: ctlAt(0),
		Config:    domain.ConfigSnapshot{Ammo: ammo, Safety: safety, Cartridge: "mock"},
	}
}

// ctlBead builds one bead of the fixture graph.
func ctlBead(id domain.BeadID, title string, state domain.BeadState) domain.Bead {
	return domain.Bead{ID: id, Title: title, Type: domain.BeadTypeTask, Priority: domain.PriorityDefault, State: state}
}

// ctlState folds a small run into a [State]: one failed bead, one bead in
// review and one closed bead, with a live agent so a stop has something to
// kill.
func ctlState(t *testing.T, safety bool, ammo int) *State {
	t.Helper()
	st := NewState(ctlRunInfo(safety, ammo))
	for _, ev := range []events.Event{
		events.RunStarted{Meta: events.MetaAt(ctlRunID, ctlAt(0)), Info: ctlRunInfo(safety, ammo)},
		events.BeadCreated{Meta: events.MetaAt(ctlRunID, ctlAt(1)), Bead: ctlBead("sc-1a2b", "Retry failed requests", domain.BeadStateFailed)},
		events.BeadCreated{Meta: events.MetaAt(ctlRunID, ctlAt(2)), Bead: ctlBead("sc-3c4d", "Document the policy", domain.BeadStateInReview)},
		events.BeadCreated{Meta: events.MetaAt(ctlRunID, ctlAt(3)), Bead: ctlBead("sc-5e6f", "Drop the dead flag", domain.BeadStateClosed)},
		events.AgentSpawned{
			Meta:  events.MetaAt(ctlRunID, ctlAt(4)),
			Agent: domain.Agent{ID: "eng-sc-3c4d-r1", Role: domain.RoleEngineer, Bead: "sc-3c4d", State: domain.AgentStateRunning, Cartridge: "mock"},
			Task:  events.TaskImplement,
		},
	} {
		st.Apply(ev)
	}
	return st
}

// ctlModel builds a control layer wired to c, with a pinned no-colour theme
// so a rendered overlay never depends on the developer's terminal.
func ctlModel(c Controller) ControlsModel {
	return NewControlsModel(ControlsOptions{Theme: ThemeFor(ProfileNoColor), Controller: c})
}

func TestControlsTakeTheProcessThemeWhenGivenNone(t *testing.T) {
	t.Parallel()

	v := NewControlsModel(ControlsOptions{NoColor: true, Controller: &ctlController{}})
	require.True(t, v.Enabled())
	v.pending, v.confirming = ControlRequest{Action: ControlStop}, true
	out := ctlOverlay(t, v, ctlState(t, true, 4), 60, 8)
	for _, esc := range ctlEscapes.FindAllString(out, -1) {
		require.Regexp(t, ctlAttrEscape, esc, "a no-colour layer draws no colour")
	}
}

// ctlKey turns a key name into the message Bubble Tea would deliver.
func ctlKey(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// ctlPress sends one key and runs whatever command came back, folding the
// result in the way a program does. It returns the model, the result the
// command produced (nil when there was none) and whether the key was
// consumed.
func ctlPress(t *testing.T, v ControlsModel, st *State, key string, target ControlTarget) (ControlsModel, *ControlResultMsg, bool) {
	t.Helper()
	next, cmd, ok := v.Update(ctlKey(key), st, DefaultKeyMap(), target)
	if cmd == nil {
		return next, nil, ok
	}
	msg, isResult := cmd().(ControlResultMsg)
	require.True(t, isResult, "a control command must produce a ControlResultMsg")
	return next.Apply(msg), &msg, ok
}

// ctlType sends a run of keys, returning the last result.
func ctlType(t *testing.T, v ControlsModel, st *State, target ControlTarget, keys ...string) (ControlsModel, *ControlResultMsg) {
	t.Helper()
	var last *ControlResultMsg
	for _, k := range keys {
		next, res, ok := ctlPress(t, v, st, k, target)
		require.True(t, ok, "key %q must be consumed", k)
		v = next
		if res != nil {
			last = res
		}
	}
	return v, last
}

// ctlTarget is the bead a bead-scoped key acts on.
func ctlTarget(st *State, id domain.BeadID) ControlTarget { return TargetFor(st, id) }

func TestControlPauseAndResumeFollowTheRun(t *testing.T) {
	t.Parallel()

	c := &ctlController{}
	st := ctlState(t, true, 4)
	v, res, ok := ctlPress(t, ctlModel(c), st, "p", ControlTarget{})
	require.True(t, ok)
	require.NotNil(t, res)
	require.Equal(t, []ctlCall{{Action: ControlPause, Reason: ControlReason}}, c.Calls())
	flash, isErr := v.Flash()
	require.False(t, isErr)
	require.Equal(t, "run paused", flash)

	// The orchestrator's event, not the key, is what pauses the run.
	require.False(t, st.Paused)
	st.Apply(events.RunPaused{Meta: events.MetaAt(ctlRunID, ctlAt(5)), Reason: "user"})

	resumed := &ctlController{}
	v2, res2, ok := ctlPress(t, ctlModel(resumed), st, "p", ControlTarget{})
	require.True(t, ok)
	require.NotNil(t, res2)
	require.Equal(t, []ctlCall{{Action: ControlResume, Reason: ControlReason}}, resumed.Calls())
	flash, isErr = v2.Flash()
	require.False(t, isErr)
	require.Equal(t, "run resumed", flash)
}

func TestControlStopConfirmsBeforeItStops(t *testing.T) {
	t.Parallel()

	c := &ctlController{}
	st := ctlState(t, true, 4)
	v, _, ok := ctlPress(t, ctlModel(c), st, "s", ControlTarget{})
	require.True(t, ok)
	require.True(t, v.Active(), "s must raise a confirmation")
	req, confirming := v.Confirming()
	require.True(t, confirming)
	require.Equal(t, ControlStop, req.Action)
	require.Empty(t, c.Calls(), "nothing may be called before the confirmation is answered")

	overlay, drawn := v.Overlay(st, 60, 10)
	require.True(t, drawn)
	require.Contains(t, ctlPlain(overlay), "Stop run "+string(ctlRunID)+"?")
	require.Contains(t, ctlPlain(overlay), "1 agent(s) will be killed")

	// No keeps firing.
	kept, _, ok := ctlPress(t, v, st, "n", ControlTarget{})
	require.True(t, ok)
	require.False(t, kept.Active())
	require.Empty(t, c.Calls())

	// Yes stops.
	stopped, res, ok := ctlPress(t, v, st, "y", ControlTarget{})
	require.True(t, ok)
	require.NotNil(t, res)
	require.False(t, stopped.Active())
	require.Equal(t, []ctlCall{{Action: ControlStop, Reason: ControlReason}}, c.Calls())
	flash, isErr := stopped.Flash()
	require.False(t, isErr)
	require.Equal(t, "stop requested", flash)
}

func TestControlRefireOnlyFiresGivenUpBeads(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		state domain.BeadState
		fires bool
	}{
		{"failed", domain.BeadStateFailed, true},
		{"blocked", domain.BeadStateBlocked, true},
		{"in review", domain.BeadStateInReview, false},
		{"closed", domain.BeadStateClosed, false},
		{"in progress", domain.BeadStateInProgress, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &ctlController{}
			st := ctlState(t, true, 4)
			st.Apply(events.BeadStateChanged{Meta: events.MetaAt(ctlRunID, ctlAt(6)), Bead: "sc-1a2b", To: tc.state})

			v, _, ok := ctlPress(t, ctlModel(c), st, "r", ctlTarget(st, "sc-1a2b"))
			require.True(t, ok, "r is always consumed, so the reason is visible")
			require.False(t, v.Active(), "a re-fire is not confirmed")
			flash, isErr := v.Flash()
			if !tc.fires {
				require.Empty(t, c.Calls())
				require.True(t, isErr)
				require.Contains(t, flash, "only a failed or blocked bead can be re-fired")
				return
			}
			require.Equal(t, []ctlCall{{Action: ControlRefire, Bead: "sc-1a2b", Reason: ControlReason}}, c.Calls())
			require.False(t, isErr)
			require.Equal(t, "sc-1a2b will be fired again", flash)
		})
	}
}

func TestControlBeadKeysNeedASelection(t *testing.T) {
	t.Parallel()

	for _, k := range []string{"r", "x"} {
		c := &ctlController{}
		st := ctlState(t, true, 4)
		v, _, ok := ctlPress(t, ctlModel(c), st, k, ControlTarget{})
		require.True(t, ok)
		require.False(t, v.Active())
		require.Empty(t, c.Calls())
		flash, isErr := v.Flash()
		require.True(t, isErr)
		require.Equal(t, "select a bead first", flash, "%q needs a bead", k)
	}
}

func TestControlMarkDoneConfirmsAndClosesOneBead(t *testing.T) {
	t.Parallel()

	c := &ctlController{}
	st := ctlState(t, true, 4)
	v, _, ok := ctlPress(t, ctlModel(c), st, "x", ctlTarget(st, "sc-3c4d"))
	require.True(t, ok)
	require.True(t, v.Active())
	require.Empty(t, c.Calls())

	overlay, drawn := v.Overlay(st, 70, 10)
	require.True(t, drawn)
	plain := ctlPlain(overlay)
	require.Contains(t, plain, "Mark sc-3c4d done?")
	require.Contains(t, plain, "Document the policy")
	require.Contains(t, plain, "in_review")

	done, res, ok := ctlPress(t, v, st, "y", ctlTarget(st, "sc-3c4d"))
	require.True(t, ok)
	require.NotNil(t, res)
	require.Equal(t, []ctlCall{{Action: ControlMarkDone, Bead: "sc-3c4d", Reason: ControlReason}}, c.Calls())
	flash, isErr := done.Flash()
	require.False(t, isErr)
	require.Equal(t, "sc-3c4d marked done", flash)
}

func TestControlMarkDoneRefusesAClosedBead(t *testing.T) {
	t.Parallel()

	c := &ctlController{}
	st := ctlState(t, true, 4)
	v, _, ok := ctlPress(t, ctlModel(c), st, "x", ctlTarget(st, "sc-5e6f"))
	require.True(t, ok)
	require.False(t, v.Active())
	require.Empty(t, c.Calls())
	flash, isErr := v.Flash()
	require.True(t, isErr)
	require.Equal(t, "sc-5e6f is already closed", flash)
}

func TestControlSafetyTogglesAfterAConfirmation(t *testing.T) {
	t.Parallel()

	for _, safety := range []bool{true, false} {
		t.Run("safety "+onOff(safety), func(t *testing.T) {
			t.Parallel()
			c := &ctlController{}
			st := ctlState(t, safety, 4)
			v, _, ok := ctlPress(t, ctlModel(c), st, "S", ControlTarget{})
			require.True(t, ok)
			require.True(t, v.Active())
			req, confirming := v.Confirming()
			require.True(t, confirming)
			require.Equal(t, !safety, req.Safety, "the confirmation offers the opposite of the run's flag")

			overlay, drawn := v.Overlay(st, 70, 10)
			require.True(t, drawn)
			require.Contains(t, ctlPlain(overlay), "Turn safety "+onOff(!safety)+"?")

			toggled, res, ok := ctlPress(t, v, st, "y", ControlTarget{})
			require.True(t, ok)
			require.NotNil(t, res)
			require.Equal(t, []ctlCall{{Action: ControlSafety, Safety: !safety}}, c.Calls())
			flash, isErr := toggled.Flash()
			require.False(t, isErr)
			require.Equal(t, "safety "+onOff(!safety)+" for future deliveries", flash)
		})
	}
}

func TestControlSafetyConfirmationCanBeCancelled(t *testing.T) {
	t.Parallel()

	c := &ctlController{}
	st := ctlState(t, true, 4)
	v, _, _ := ctlPress(t, ctlModel(c), st, "S", ControlTarget{})
	require.True(t, v.Active())
	for _, k := range []string{"esc", "n"} {
		cancelled, _, ok := ctlPress(t, v, st, k, ControlTarget{})
		require.True(t, ok)
		require.False(t, cancelled.Active(), "%q must close the confirmation", k)
		require.Empty(t, c.Calls())
		flash, isErr := cancelled.Flash()
		require.False(t, isErr)
		require.Equal(t, "cancelled", flash)
	}
}

func TestControlAmmoPromptAppliesANewCap(t *testing.T) {
	t.Parallel()

	c := &ctlController{}
	st := ctlState(t, true, 4)
	v, _, ok := ctlPress(t, ctlModel(c), st, "a", ControlTarget{})
	require.True(t, ok)
	require.True(t, v.Prompting())
	require.True(t, v.Active())
	require.Equal(t, "4", v.AmmoInput(), "the prompt starts from the run's own ammo")

	overlay, drawn := v.Overlay(st, 60, 10)
	require.True(t, drawn)
	plain := ctlPlain(overlay)
	require.Contains(t, plain, "Ammo for future ticks")
	require.Contains(t, plain, fmt.Sprintf("%d–%d concurrent agents", ControlMinAmmo, ControlMaxAmmo))

	// Clear the field, type 12, and apply.
	v, res := ctlType(t, v, st, ControlTarget{}, "backspace", "1", "2", "enter")
	require.NotNil(t, res)
	require.False(t, v.Prompting())
	require.Equal(t, []ctlCall{{Action: ControlAmmo, Ammo: 12}}, c.Calls())
	flash, isErr := v.Flash()
	require.False(t, isErr)
	require.Equal(t, "ammo 12 for future ticks", flash)
}

func TestControlAmmoPromptIgnoresWhatIsNotANumber(t *testing.T) {
	t.Parallel()

	c := &ctlController{}
	st := ctlState(t, true, 4)
	v, _, _ := ctlPress(t, ctlModel(c), st, "a", ControlTarget{})
	v, _ = ctlType(t, v, st, ControlTarget{}, "backspace", "y", "q", "?", "-", "8")
	require.Equal(t, "8", v.AmmoInput())
	require.True(t, v.Prompting(), "no letter may submit or close the field")
	require.Empty(t, c.Calls())

	// The field never grows past the digits the maximum has.
	v, _ = ctlType(t, v, st, ControlTarget{}, "7", "6")
	require.Equal(t, "87", v.AmmoInput())
}

func TestControlAmmoPromptRefusesAnOutOfRangeCap(t *testing.T) {
	t.Parallel()

	c := &ctlController{}
	st := ctlState(t, true, 4)
	v, _, _ := ctlPress(t, ctlModel(c), st, "a", ControlTarget{})
	v, _ = ctlType(t, v, st, ControlTarget{}, "backspace", "1", "enter")
	require.True(t, v.Prompting(), "a refused value leaves the prompt open to be corrected")
	require.Empty(t, c.Calls())
	flash, isErr := v.Flash()
	require.True(t, isErr)
	require.Equal(t, fmt.Sprintf("ammo must be a number between %d and %d", ControlMinAmmo, ControlMaxAmmo), flash)
	require.Contains(t, ctlPlain(ctlOverlay(t, v, st, 60, 12)), flash, "the prompt says why it refused")

	// An empty field is refused too.
	empty, _ := ctlType(t, v, st, ControlTarget{}, "backspace", "enter")
	require.True(t, empty.Prompting())
	require.Empty(t, c.Calls())
}

func TestControlAmmoPromptDoesNotCallForTheSameCap(t *testing.T) {
	t.Parallel()

	c := &ctlController{}
	st := ctlState(t, true, 4)
	v, _, _ := ctlPress(t, ctlModel(c), st, "a", ControlTarget{})
	v, _ = ctlType(t, v, st, ControlTarget{}, "enter")
	require.False(t, v.Prompting())
	require.Empty(t, c.Calls())
	flash, isErr := v.Flash()
	require.False(t, isErr)
	require.Equal(t, "ammo is already 4", flash)
}

func TestControlAmmoPromptCancels(t *testing.T) {
	t.Parallel()

	c := &ctlController{}
	st := ctlState(t, true, 4)
	v, _, _ := ctlPress(t, ctlModel(c), st, "a", ControlTarget{})
	v, _ = ctlType(t, v, st, ControlTarget{}, "8", "esc")
	require.False(t, v.Prompting())
	require.False(t, v.Active())
	require.Empty(t, v.AmmoInput())
	require.Empty(t, c.Calls())
	_, drawn := v.Overlay(st, 60, 10)
	require.False(t, drawn)
}

func TestControlKeysRefuseAFinishedOrStoppingRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		event  events.Event
		reason string
	}{
		{
			name:   "finished",
			event:  events.RunFinished{Meta: events.MetaAt(ctlRunID, ctlAt(9)), Reason: events.FinishCompleted},
			reason: "the run has finished",
		},
		{
			name:   "stopping",
			event:  events.RunStopped{Meta: events.MetaAt(ctlRunID, ctlAt(9)), Reason: "user"},
			reason: "the run is stopping",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, k := range []string{"p", "s", "r", "x", "S", "a"} {
				c := &ctlController{}
				st := ctlState(t, true, 4)
				st.Apply(tc.event)
				v, _, ok := ctlPress(t, ctlModel(c), st, k, ctlTarget(st, "sc-1a2b"))
				require.True(t, ok, "%q is consumed so the refusal is visible", k)
				require.False(t, v.Active())
				require.Empty(t, c.Calls(), "%q must call nothing", k)
				flash, isErr := v.Flash()
				require.True(t, isErr)
				require.Equal(t, tc.reason, flash)
			}
		})
	}
}

func TestControlKeysDoNothingWithoutAController(t *testing.T) {
	t.Parallel()

	st := ctlState(t, true, 4)
	v := NewControlsModel(ControlsOptions{Theme: ThemeFor(ProfileNoColor)})
	require.False(t, v.Enabled())
	require.False(t, v.Keys().Enabled())
	for _, k := range []string{"p", "s", "r", "x", "S", "a"} {
		next, cmd, ok := v.Update(ctlKey(k), st, DefaultKeyMap(), ctlTarget(st, "sc-1a2b"))
		require.False(t, ok, "%q must fall through to the views when nothing can be controlled", k)
		require.Nil(t, cmd)
		require.False(t, next.Active())
	}

	// A disabled binding is not promised in the help either.
	for _, col := range v.Keys().FullHelp() {
		for _, b := range col {
			require.False(t, b.Enabled(), "%q must not be advertised", b.Help().Key)
		}
	}
}

func TestControlsConsumeEveryKeyWhileAnOverlayIsUp(t *testing.T) {
	t.Parallel()

	st := ctlState(t, true, 4)
	for _, open := range []string{"s", "a"} {
		c := &ctlController{}
		v, _, _ := ctlPress(t, ctlModel(c), st, open, ControlTarget{})
		require.True(t, v.Active())
		for _, k := range []string{"tab", "up", "1", "?", "j", "o"} {
			next, cmd, ok := v.Update(ctlKey(k), st, DefaultKeyMap(), ControlTarget{})
			require.True(t, ok, "%q must not reach the view behind the overlay", k)
			require.Nil(t, cmd)
			require.True(t, next.Active(), "%q must not close the overlay", k)
		}
		require.Empty(t, c.Calls())
	}
}

func TestControlsIgnoreEverythingThatIsNotAKey(t *testing.T) {
	t.Parallel()

	st := ctlState(t, true, 4)
	v := ctlModel(&ctlController{})
	_, cmd, ok := v.Update(tea.WindowSizeMsg{Width: 100, Height: 30}, st, DefaultKeyMap(), ControlTarget{})
	require.False(t, ok)
	require.Nil(t, cmd)
}

func TestControlsNeverMutateTheRunState(t *testing.T) {
	t.Parallel()

	c := &ctlController{}
	st := ctlState(t, true, 4)
	before := *st
	v := ctlModel(c)
	for _, k := range []string{"p", "s", "y", "S", "y", "x", "y", "r", "a", "9", "enter"} {
		v, _, _ = ctlPress(t, v, st, k, ctlTarget(st, "sc-1a2b"))
	}
	require.NotEmpty(t, c.Calls(), "the fixture must actually have driven the controller")
	require.Equal(t, before.Live, st.Live)
	require.Equal(t, before.Paused, st.Paused)
	require.Equal(t, before.Stopping, st.Stopping)
	require.Equal(t, before.Run.Config.Safety, st.Run.Config.Safety)
	require.Equal(t, before.Run.Config.Ammo, st.Run.Config.Ammo)
	require.Equal(t, before.Beads, st.Beads)
	require.Equal(t, before.Events, st.Events)
}

func TestControlResultReportsAFailure(t *testing.T) {
	t.Parallel()

	c := &ctlController{err: errors.New("bd update: \x1b[31mno such bead\x1b[0m")}
	st := ctlState(t, true, 4)
	v, res, ok := ctlPress(t, ctlModel(c), st, "r", ctlTarget(st, "sc-1a2b"))
	require.True(t, ok)
	require.NotNil(t, res)
	require.Error(t, res.Err)
	flash, isErr := v.Flash()
	require.True(t, isErr)
	require.Equal(t, "could not re-fire sc-1a2b: bd update: no such bead", flash,
		"a controller error is sanitised before it is drawn")
}

func TestControlsCountCallsInFlight(t *testing.T) {
	t.Parallel()

	c := &ctlController{block: make(chan struct{})}
	st := ctlState(t, true, 4)
	v, cmd, ok := ctlModel(c).Update(ctlKey("p"), st, DefaultKeyMap(), ControlTarget{})
	require.True(t, ok)
	require.NotNil(t, cmd)
	require.Equal(t, 1, v.InFlight())
	flash, isErr := v.Flash()
	require.False(t, isErr)
	require.Equal(t, "pausing…", flash)

	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	close(c.block)
	msg, isResult := (<-done).(ControlResultMsg)
	require.True(t, isResult)
	require.Equal(t, 0, v.Apply(msg).InFlight())
}

func TestControlRequestFlashesReadWell(t *testing.T) {
	t.Parallel()

	cases := []struct {
		req     ControlRequest
		pending string
		summary string
		failure string
	}{
		{ControlRequest{Action: ControlPause}, "pausing…", "run paused", "could not pause the run"},
		{ControlRequest{Action: ControlResume}, "resuming…", "run resumed", "could not resume the run"},
		{ControlRequest{Action: ControlStop}, "stopping…", "stop requested", "could not stop the run"},
		{ControlRequest{Action: ControlRefire, Bead: "sc-1a2b"}, "re-firing sc-1a2b…", "sc-1a2b will be fired again", "could not re-fire sc-1a2b"},
		{ControlRequest{Action: ControlMarkDone, Bead: "sc-1a2b"}, "marking sc-1a2b done…", "sc-1a2b marked done", "could not mark sc-1a2b done"},
		{ControlRequest{Action: ControlSafety}, "turning safety off…", "safety off for future deliveries", "could not turn safety off"},
		{ControlRequest{Action: ControlAmmo, Ammo: 6}, "setting ammo to 6…", "ammo 6 for future ticks", "could not set ammo to 6"},
	}
	for _, tc := range cases {
		t.Run(tc.req.Action.String(), func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.pending, tc.req.Pending())
			require.Equal(t, tc.summary, tc.req.Summary())
			require.Equal(t, tc.failure, tc.req.Failure())

			ok, isErr := ControlResultMsg{Request: tc.req}.Flash()
			require.False(t, isErr)
			require.Equal(t, tc.summary, ok)
			bad, isErr := ControlResultMsg{Request: tc.req, Err: errors.New("boom")}.Flash()
			require.True(t, isErr)
			require.Equal(t, tc.failure+": boom", bad)
		})
	}
}

func TestControlActionVocabulary(t *testing.T) {
	t.Parallel()

	require.Len(t, ControlActions(), 7)
	confirmed := 0
	for _, a := range ControlActions() {
		require.True(t, a.Valid(), "%s must be valid", a)
		require.Equal(t, string(a), a.String())
		if a.Confirms() {
			confirmed++
		}
	}
	require.Equal(t, 3, confirmed, "stop, mark-done and safety are the confirmed actions")
	require.False(t, ControlAction("").Valid())
	require.False(t, ControlAction("launch-missiles").Valid())
	require.Nil(t, ControlRequest{Action: "nonsense"}.Cmd(&ctlController{}, 0))
	require.Nil(t, ControlRequest{Action: ControlPause}.Cmd(nil, 0))
	require.NotNil(t, ControlRequest{Action: ControlPause}.Cmd(&ctlController{}, 0),
		"a zero timeout falls back to the default rather than expiring at once")
}

func TestControlTargetReadsTheBead(t *testing.T) {
	t.Parallel()

	st := ctlState(t, true, 4)
	got := TargetFor(st, "sc-1a2b")
	require.Equal(t, ControlTarget{Bead: "sc-1a2b", Title: "Retry failed requests", State: domain.BeadStateFailed}, got)
	require.False(t, got.Empty())

	unknown := TargetFor(st, "sc-nope")
	require.Equal(t, ControlTarget{Bead: "sc-nope"}, unknown, "an unknown bead is still nameable")
	require.True(t, TargetFor(st, "").Empty())
	require.True(t, TargetFor(nil, "").Empty())
	require.True(t, ControlRefirable(domain.BeadStateFailed))
	require.True(t, ControlRefirable(domain.BeadStateBlocked))
	require.False(t, ControlRefirable(domain.BeadStateApproved))
}

func TestControlOverlayFillsThePaneAndStaysPlain(t *testing.T) {
	t.Parallel()

	st := ctlState(t, true, 4)
	sizes := [][2]int{{40, 8}, {80, 24}, {120, 40}}
	for _, open := range []string{"s", "x", "S", "a"} {
		for _, size := range sizes {
			c := &ctlController{}
			v, _, _ := ctlPress(t, ctlModel(c), st, open, ctlTarget(st, "sc-3c4d"))
			require.True(t, v.Active(), "%q must raise an overlay", open)
			w, h := size[0], size[1]
			overlay, drawn := v.Overlay(st, w, h)
			require.True(t, drawn)
			require.Equal(t, h, lipgloss.Height(overlay), "%q at %dx%d", open, w, h)
			require.LessOrEqual(t, lipgloss.Width(overlay), w, "%q at %dx%d", open, w, h)
			for _, esc := range ctlEscapes.FindAllString(overlay, -1) {
				require.Regexp(t, ctlAttrEscape, esc, "%q at %dx%d draws a colour escape under a no-colour theme", open, w, h)
			}
		}
	}
}

func TestControlOverlaySanitisesABeadTitle(t *testing.T) {
	t.Parallel()

	st := ctlState(t, true, 4)
	nasty := "Fix \x1b[2Jthe\r\nlogger\x07"
	st.Apply(events.BeadUpdated{
		Meta:   events.MetaAt(ctlRunID, ctlAt(7)),
		Bead:   ctlBead("sc-3c4d", nasty, domain.BeadStateInReview),
		Fields: []string{"title"},
	})
	v, _, _ := ctlPress(t, ctlModel(&ctlController{}), st, "x", ctlTarget(st, "sc-3c4d"))
	req, confirming := v.Confirming()
	require.True(t, confirming)
	require.NotContains(t, req.Title, "\x1b")
	require.NotContains(t, req.Title, "\n")

	overlay := ctlOverlay(t, v, st, 80, 12)
	require.NotContains(t, ctlPlain(overlay), "\x1b[2J")
	require.Contains(t, ctlPlain(overlay), "logger")
}

func TestControlHelpNamesEveryControlKey(t *testing.T) {
	t.Parallel()

	v := ctlModel(&ctlController{})
	seen := map[string]string{}
	for _, col := range v.Keys().FullHelp() {
		for _, b := range col {
			require.True(t, b.Enabled())
			h := b.Help()
			require.NotEmpty(t, h.Desc, "%q needs a description for the overlay", h.Key)
			seen[h.Key] = h.Desc
		}
	}
	for _, k := range []string{"p", "s", "r", "x", "S", "a"} {
		require.Contains(t, seen, k, "the help overlay must name %q", k)
	}
	require.NotEmpty(t, v.Keys().ShortHelp())
	min, max := v.AmmoBounds()
	require.Equal(t, ControlMinAmmo, min)
	require.Equal(t, ControlMaxAmmo, max)
}

func TestControlKeysDoNotCollideWithTheRestOfTheTUI(t *testing.T) {
	t.Parallel()

	global := DefaultKeyMap()
	others := [][]key.Binding{
		{global.NextTab, global.PrevTab, global.SelectTab, global.Up, global.Down, global.Help, global.Quit, global.ForceQuit},
		DefaultGraphKeyMap().FullHelp()[0],
		DefaultAgentsKeyMap().FullHelp()[0],
		DefaultFeedbackKeyMap().FullHelp()[0],
		DefaultPRsKeyMap().FullHelp()[0],
	}
	controls := DefaultControlsKeyMap()
	for _, b := range []key.Binding{controls.PauseResume, controls.Stop, controls.Refire, controls.MarkDone, controls.Safety, controls.Ammo} {
		for _, k := range b.Keys() {
			for _, group := range others {
				for _, other := range group {
					for _, ok := range other.Keys() {
						require.NotEqual(t, k, ok, "control key %q already means %q elsewhere", k, other.Help().Desc)
					}
				}
			}
		}
	}
}

func TestControlKeyMapCanBeRebound(t *testing.T) {
	t.Parallel()

	km := DefaultControlsKeyMap()
	km.PauseResume = key.NewBinding(key.WithKeys("P"), key.WithHelp("P", "pause / resume"))
	c := &ctlController{}
	st := ctlState(t, true, 4)
	v := NewControlsModel(ControlsOptions{Theme: ThemeFor(ProfileNoColor), Controller: c, Keys: &km})

	_, _, ok := v.Update(ctlKey("p"), st, DefaultKeyMap(), ControlTarget{})
	require.False(t, ok, "the old key is free once it is rebound")

	next, cmd, ok := v.Update(ctlKey("P"), st, DefaultKeyMap(), ControlTarget{})
	require.True(t, ok)
	require.NotNil(t, cmd)
	require.Equal(t, 1, next.InFlight())
	cmd()
	require.Equal(t, []ctlCall{{Action: ControlPause, Reason: ControlReason}}, c.Calls())
}

// ctlOverlay renders the overlay and requires that there was one.
func ctlOverlay(t *testing.T, v ControlsModel, st *State, w, h int) string {
	t.Helper()
	out, drawn := v.Overlay(st, w, h)
	require.True(t, drawn, "an overlay must be up")
	return out
}

// ctlEscapes matches any ANSI escape sequence.
var ctlEscapes = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// ctlAttrEscape matches the attribute-only sequences a no-colour theme is
// still allowed to draw: bold and underline and their resets, which the
// theme keeps under NO_COLOR because they carry meaning without colour.
var ctlAttrEscape = regexp.MustCompile(`^\x1b\[(?:0|1|4|22|24)?(?:;(?:0|1|4|22|24))*m$`)

// ctlPlain strips every escape sequence, so a test can assert on the text a
// reader sees.
func ctlPlain(s string) string {
	return strings.TrimRight(ctlEscapes.ReplaceAllString(s, ""), " \n")
}

func TestControlsRefuseWhenThereIsNoRunYet(t *testing.T) {
	t.Parallel()

	c := &ctlController{}
	v := ctlModel(c)
	for _, k := range []string{"p", "s", "r", "x", "S", "a"} {
		next, cmd, ok := v.Update(ctlKey(k), nil, DefaultKeyMap(), ControlTarget{})
		require.True(t, ok, "%q is consumed so the refusal is visible", k)
		require.Nil(t, cmd)
		flash, isErr := next.Flash()
		require.True(t, isErr)
		require.Equal(t, "no run to control", flash)
	}
	require.Empty(t, c.Calls())
	require.Equal(t, 0, controlsAmmo(nil))
	require.Equal(t, 0, controlsLiveAgents(nil))
	require.Empty(t, controlsRunID(nil))
}

func TestControlStopConfirmationSurvivesAnEmptyRun(t *testing.T) {
	t.Parallel()

	// A stop raised against a run whose events have not arrived still has to
	// draw: the id falls back to a dash and no agent is at risk.
	v := ctlModel(&ctlController{})
	v.pending, v.confirming = ControlRequest{Action: ControlStop}, true
	plain := ctlPlain(ctlOverlay(t, v, nil, 50, 8))
	require.Contains(t, plain, "Stop run -?")
	require.Contains(t, plain, "0 agent(s)")
}

func TestControlMarkDoneDescribesATitlelessBead(t *testing.T) {
	t.Parallel()

	st := ctlState(t, true, 4)
	v, _, ok := ctlPress(t, ctlModel(&ctlController{}), st, "x", ControlTarget{Bead: "sc-9x0y"})
	require.True(t, ok)
	require.True(t, v.Active())
	plain := ctlPlain(ctlOverlay(t, v, st, 60, 8))
	require.Contains(t, plain, "Mark sc-9x0y done?")
	require.Contains(t, plain, "It is -.", "an unknown bead is described by what is known of it")
}

func TestControlUnknownActionIsInert(t *testing.T) {
	t.Parallel()

	req := ControlRequest{Action: "launch-missiles"}
	require.Equal(t, "launch-missiles…", req.Pending())
	require.Equal(t, "launch-missiles", req.Summary())
	require.Equal(t, "could not launch-missiles", req.Failure())
	require.Nil(t, req.Cmd(&ctlController{}, time.Second), "an unknown action never calls the orchestrator")

	c := &ctlController{}
	require.Error(t, req.call(context.Background(), c))
	require.Empty(t, c.Calls())

	v := ctlModel(c)
	require.Contains(t, ctlPlain(v.confirmBox(ctlState(t, true, 4), req, 60)), "Confirm launch-missiles?")

	// A request that cannot be dispatched says so instead of doing nothing.
	inert, cmd, ok := NewControlsModel(ControlsOptions{Theme: ThemeFor(ProfileNoColor)}).dispatch(ControlRequest{Action: ControlPause})
	require.True(t, ok)
	require.Nil(t, cmd)
	flash, isErr := inert.Flash()
	require.True(t, isErr)
	require.Equal(t, "no orchestrator to pause", flash)
}

func TestControlsOptionsBoundAmmoAndTimeout(t *testing.T) {
	t.Parallel()

	v := NewControlsModel(ControlsOptions{
		Theme:      ThemeFor(ProfileNoColor),
		Controller: &ctlController{},
		Timeout:    2 * time.Second,
		MinAmmo:    5,
		MaxAmmo:    4, // below the floor: the ceiling is raised, never inverted
	})
	min, max := v.AmmoBounds()
	require.Equal(t, 5, min)
	require.GreaterOrEqual(t, max, min)
	require.Equal(t, 2*time.Second, v.timeout)

	st := ctlState(t, true, 4)
	v, _, _ = ctlPress(t, v, st, "a", ControlTarget{})
	require.True(t, v.Prompting())
	require.Empty(t, v.AmmoInput(), "an ammo outside the prompt's own bounds is not offered back")
}

func TestControlsUseTheConfiguredTimeout(t *testing.T) {
	t.Parallel()

	// The context handed to the controller carries the deadline, which is
	// what keeps a wedged orchestrator call out of the frame loop.
	var deadline time.Time
	var had bool
	c := &ctlDeadlineController{seen: func(ctx context.Context) { deadline, had = ctx.Deadline() }}
	req := ControlRequest{Action: ControlPause}
	msg, ok := req.Cmd(c, 50*time.Millisecond)().(ControlResultMsg)
	require.True(t, ok)
	require.NoError(t, msg.Err)
	require.True(t, had, "the call must be bounded")
	require.WithinDuration(t, time.Now().Add(50*time.Millisecond), deadline, time.Second)
}

// ctlDeadlineController reports the context every call is made with.
type ctlDeadlineController struct {
	ctlController
	seen func(ctx context.Context)
}

// Pause implements [Controller].
func (c *ctlDeadlineController) Pause(ctx context.Context, reason string) error {
	c.seen(ctx)
	return c.ctlController.Pause(ctx, reason)
}
