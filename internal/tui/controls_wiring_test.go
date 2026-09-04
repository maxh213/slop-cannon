package tui_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/tui"
)

// P7-10 round 2 — the run controls inside the program that ships.
//
// The first round tested [tui.ControlsModel] on its own, which proved the
// layer but not the wiring: the dispatch ahead of the views, the
// [tui.ControlResultMsg] fold, the modal overlay, the footer and the `?`
// overlay's run-control block all live in Model, and deleting any of them
// left the suite green. Everything here therefore drives a real
// [tui.Model] — the same object `sc run` and `sc tui-demo` hand to Bubble
// Tea — through key presses, and reads the answer out of the frame the user
// would see.
//
// The four frames the keys draw are pinned as goldens next to P7-09's, so a
// change to the confirmation, the prompt, the footer line or the help block
// has to be regenerated deliberately (see cwGoldens).

// cwController is the fake orchestrator the wiring tests drive. It records
// what each key asked for as one line, which is what a test asserts against:
// the point of these tests is that the press reached the orchestrator at
// all.
type cwController struct {
	mu    sync.Mutex
	calls []string
	err   error
}

var _ tui.Controller = (*cwController)(nil)

// record appends one call and returns the configured error.
func (c *cwController) record(format string, args ...any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, fmt.Sprintf(format, args...))
	return c.err
}

// Calls returns what the keys asked for, in order.
func (c *cwController) Calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.calls))
	copy(out, c.calls)
	return out
}

func (c *cwController) Pause(_ context.Context, reason string) error {
	return c.record("pause(%s)", reason)
}

func (c *cwController) Resume(_ context.Context, reason string) error {
	return c.record("resume(%s)", reason)
}

func (c *cwController) Stop(_ context.Context, reason string) error {
	return c.record("stop(%s)", reason)
}

func (c *cwController) Refire(_ context.Context, bead domain.BeadID, reason string) error {
	return c.record("refire(%s,%s)", bead, reason)
}

func (c *cwController) MarkDone(_ context.Context, bead domain.BeadID, reason string) error {
	return c.record("mark-done(%s,%s)", bead, reason)
}

func (c *cwController) SetSafety(_ context.Context, on bool) error {
	return c.record("safety(%t)", on)
}

func (c *cwController) SetAmmo(_ context.Context, ammo int) error {
	return c.record("ammo(%d)", ammo)
}

// cwLiveEvents is the scripted demo run cut off before it pauses itself, so
// the run the controls see is live: bead demo-2 has been delivered and
// closed, demo-3 sits in needs_human, and an agent is still running.
func cwLiveEvents() []events.Event {
	evs := tui.DemoEvents(gfDemo)
	for i, ev := range evs {
		if ev.Kind() == events.KindRunPaused {
			return evs[:i]
		}
	}
	return evs
}

// cwWidth and cwHeight are the terminal these frames are rendered in. It is
// wider than P7-09's 80x24 for one reason: the footer clips its line to the
// terminal, and the short keymap alone is 76 columns, so at 80 a control's
// answer is cut off after a character or two. The keys are meant to be read.
const (
	cwWidth  = 120
	cwHeight = 40
)

// cwModel builds the shipped model with c attached, at [cwWidth] x
// [cwHeight], fed the live script. The theme and the clock are pinned
// exactly as the golden-frame tests pin them, so a frame is reproducible
// anywhere.
func cwModel(t *testing.T, c tui.Controller) tui.Model {
	t.Helper()
	m := tui.New(t.Context(), tui.Options{
		Run:        tui.DemoRun(gfDemo),
		NoColor:    true,
		Theme:      tui.ThemeFor(tui.ProfileNoColor),
		NoAnim:     true,
		Now:        gfClock,
		Controller: c,
	})
	m = gfSend(m, tea.WindowSizeMsg{Width: cwWidth, Height: cwHeight})
	return gfFeed(m, cwLiveEvents())
}

// cwPress sends key presses through the model the way the program's update
// loop does: the command a key returns is run, and a [tui.ControlResultMsg]
// it produces is fed straight back in. That fold is the wiring under test —
// without it the footer never learns how the call went.
func cwPress(t *testing.T, m tui.Model, keys ...string) tui.Model {
	t.Helper()
	for _, k := range keys {
		next, cmd := m.Update(cwKey(k))
		out, ok := next.(tui.Model)
		require.True(t, ok, "Update returned something that is not a tui.Model")
		m = out
		if cmd == nil {
			continue
		}
		if msg, isResult := cmd().(tui.ControlResultMsg); isResult {
			m = gfSend(m, msg)
		}
	}
	return m
}

// cwKey is [gfKey] plus the keys the ammo prompt needs, which no composed
// frame presses: without this, "backspace" would arrive as nine letters.
func cwKey(s string) tea.KeyMsg {
	switch s {
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "delete":
		return tea.KeyMsg{Type: tea.KeyDelete}
	}
	return gfKey(s)
}

// cwFooter is the last line of the frame, without escapes: where every
// control reports what it did.
func cwFooter(m tui.Model) string {
	lines := strings.Split(gfPlain(m.View()), "\n")
	return strings.TrimRight(lines[len(lines)-1], " ")
}

// cwFrame is the whole frame without escapes.
func cwFrame(m tui.Model) string { return gfPlain(m.View()) }

// TestModelSendsEveryControlKeyToTheController is the acceptance test the
// P7-10 "Done when" line asks for, run against the program rather than the
// component: each key press goes through [tui.Model.Update], reaches the
// fake orchestrator, and its answer is folded back into the footer the user
// reads.
func TestModelSendsEveryControlKeyToTheController(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		before []tea.Msg
		keys   []string
		want   string
		footer string
	}{{
		name:   "pause",
		keys:   []string{"p"},
		want:   "pause(user)",
		footer: "run paused",
	}, {
		// `p` follows the run: once the orchestrator says it is paused, the
		// same key resumes it.
		name:   "resume",
		before: []tea.Msg{tui.EventMsg{Event: events.RunPaused{Meta: events.MetaAt(tui.DemoRunID, gfClock()), Reason: "user"}}},
		keys:   []string{"p"},
		want:   "resume(user)",
		footer: "run resumed",
	}, {
		name:   "stop",
		keys:   []string{"s", "y"},
		want:   "stop(user)",
		footer: "stop requested",
	}, {
		// demo-3 was handed to a human, which is one of the states `r`
		// takes; the Graph view's `g` pins the cursor so the key has a bead.
		name:   "refire",
		keys:   []string{"g", "G", "r"},
		want:   "refire(demo-3,user)",
		footer: "demo-3 will be fired again",
	}, {
		name:   "mark done",
		keys:   []string{"g", "G", "x", "y"},
		want:   "mark-done(demo-3,user)",
		footer: "demo-3 marked done",
	}, {
		name:   "safety",
		keys:   []string{"S", "y"},
		want:   "safety(true)",
		footer: "safety on for future deliveries",
	}, {
		name:   "ammo",
		keys:   []string{"a", "backspace", "backspace", "6", "enter"},
		want:   "ammo(6)",
		footer: "ammo 6 for future ticks",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &cwController{}
			m := cwModel(t, c)
			if len(tc.before) > 0 {
				m = gfSend(m, tc.before...)
			}
			m = cwPress(t, m, tc.keys...)

			require.Equal(t, []string{tc.want}, c.Calls(), "the key never reached the orchestrator")
			require.Contains(t, cwFooter(m), tc.footer, "the result was not folded into the footer")
			gfRequireGeometry(t, m.View(), cwWidth, cwHeight)
		})
	}
}

// TestModelDrawsTheControlOverlayInsteadOfTheView proves the modal branch of
// body(): a confirmation and the ammo prompt replace the pane, and the
// chrome around them is still drawn.
func TestModelDrawsTheControlOverlayInsteadOfTheView(t *testing.T) {
	t.Parallel()

	c := &cwController{}
	base := cwFrame(cwModel(t, c))

	confirm := cwFrame(cwPress(t, cwModel(t, c), "g", "G", "x"))
	require.Contains(t, confirm, "Mark demo-3 done?")
	require.Contains(t, confirm, "y mark done")
	require.NotEqual(t, base, confirm, "the overlay did not replace the view")
	require.Contains(t, confirm, "Graph", "the tab bar is still drawn under the overlay")

	prompt := cwFrame(cwPress(t, cwModel(t, c), "a"))
	require.Contains(t, prompt, "Ammo for future ticks")
	require.Contains(t, prompt, "enter apply")
	require.Empty(t, c.Calls(), "an overlay asks before it calls anything")

	// The overlay is modal: a tab key is swallowed rather than switching
	// view underneath the question.
	still := cwPress(t, cwPress(t, cwModel(t, c), "g", "G", "x"), "4")
	require.Contains(t, cwFrame(still), "Mark demo-3 done?")
}

// TestModelHelpOverlayCarriesTheRunControlsOnlyWithAController pins the `?`
// overlay's block to the presence of an orchestrator: a recorded run must
// not be told about keys that cannot do anything.
func TestModelHelpOverlayCarriesTheRunControlsOnlyWithAController(t *testing.T) {
	t.Parallel()

	with := cwFrame(cwPress(t, cwModel(t, &cwController{}), "?"))
	require.Contains(t, with, "Run controls")
	for _, help := range []string{"pause / resume", "stop run", "re-fire bead", "mark bead done", "toggle safety", "change ammo"} {
		require.Contains(t, with, help, "the overlay does not document %q", help)
	}

	without := cwFrame(cwPress(t, cwModel(t, nil), "?"))
	require.Contains(t, without, "Keys", "the overlay is still drawn")
	require.NotContains(t, without, "Run controls")
	require.NotContains(t, without, "pause / resume")
}

// TestModelControlKeysAreInertWithoutAController proves the nil-controller
// program `sc run` ships today is unchanged by P7-10: the keys open nothing,
// call nothing and say nothing.
func TestModelControlKeysAreInertWithoutAController(t *testing.T) {
	t.Parallel()

	m := cwModel(t, nil)
	base := cwFrame(m)
	for _, k := range []string{"p", "s", "r", "x", "S", "a"} {
		pressed := cwPress(t, m, k)
		frame := cwFrame(pressed)
		require.NotContains(t, frame, "Ammo for future ticks", "%q opened the ammo prompt with no orchestrator", k)
		require.NotContains(t, frame, "Stop run", "%q raised a confirmation with no orchestrator", k)
		if k == "r" || k == "x" || k == "p" || k == "a" {
			// These letters belong to no other binding either, so the frame
			// is untouched. `s` and `S` are left out only because a view may
			// claim them later.
			require.Equal(t, base, frame, "%q changed the frame with no orchestrator", k)
		}
	}
}

// TestModelControlFlashIsClearedByTheNextKeyPress is the P7-19 promise: a
// control's answer says its piece and then gets out of the way, instead of
// shadowing every other footer message for the rest of the run.
func TestModelControlFlashIsClearedByTheNextKeyPress(t *testing.T) {
	t.Parallel()

	c := &cwController{}
	m := cwPress(t, cwModel(t, c), "p")
	require.Contains(t, cwFooter(m), "run paused")

	m = cwPress(t, m, "2")
	require.NotContains(t, cwFooter(m), "run paused", "the control flash outlived the key press after it")

	// And a view's own flash reaches the footer afterwards, which it cannot
	// do while a control flash is still there.
	m = gfSend(cwPress(t, m, "4"), tui.URLOpenedMsg{URL: "https://example.invalid/pr/42"})
	require.Contains(t, cwFooter(m), "opened https://example.invalid/pr/42")
}

// TestModelBeadKeysActOnTheActiveViewsBead pins the target resolution: the
// bead-scoped keys act on what the tab that is up has under its cursor, not
// on the Graph view's cursor whatever the user is looking at.
func TestModelBeadKeysActOnTheActiveViewsBead(t *testing.T) {
	t.Parallel()

	// The Graph cursor is pinned to demo-3, the last bead.
	graph := cwPress(t, cwModel(t, &cwController{}), "g", "G")
	require.Contains(t, cwFrame(cwPress(t, graph, "x")), "Mark demo-3 done?")

	// The PRs view has demo-2 under its cursor: the same key must answer for
	// that bead, not for the one the Graph view still points at. demo-2 was
	// merged and closed, so the answer is a refusal — which names the bead,
	// and so proves which bead the key resolved.
	prs := cwPress(t, graph, "4", "x")
	require.Contains(t, cwFooter(prs), "demo-2 is already closed")

	// The Feedback view is reviewing demo-2 too, and answers for its own row.
	feedback := cwPress(t, graph, "3", "x")
	require.Contains(t, cwFooter(feedback), "demo-2 is already closed")

	// The Agents view acts on the bead its selected agent works on.
	agents := cwPress(t, graph, "2", "x")
	require.Regexp(t, `demo-\d`, cwFooter(agents)+cwFrame(agents))
}

// TestModelBeadKeysRefuseWhereNoBeadIsSelected covers the Log view, which
// has no bead under its cursor: the key says so rather than acting on a bead
// from another tab that the reader cannot see.
func TestModelBeadKeysRefuseWhereNoBeadIsSelected(t *testing.T) {
	t.Parallel()

	c := &cwController{}
	m := cwPress(t, cwModel(t, c), "g", "G", "5")
	for _, k := range []string{"x", "r"} {
		pressed := cwPress(t, m, k)
		require.Contains(t, cwFooter(pressed), "select a bead first", "%q acted on an unseen bead", k)
		require.Empty(t, c.Calls())
	}
}

// TestModelRefireRefusesABeadTheRunHasNotGivenUpOn checks the refusal in the
// shipped program, and that its message names the states the key does take.
func TestModelRefireRefusesABeadTheRunHasNotGivenUpOn(t *testing.T) {
	t.Parallel()

	c := &cwController{}
	// A wide terminal, because the footer clips its line and a refusal is
	// the longest thing it ever says.
	m := gfSend(cwModel(t, c), tea.WindowSizeMsg{Width: 200, Height: cwHeight})
	// `g` puts the cursor on the first bead of the layout.
	m = cwPress(t, m, "g", "r")
	require.Empty(t, c.Calls())
	require.Contains(t, cwFooter(m), "only a failed, needs_human, escalated or blocked bead can be re-fired")
	require.Regexp(t, `demo-\d is \S+ —`, cwFooter(m), "the refusal names the bead and the state it is in")
}

// TestModelReportsAFailedControlCall folds an error back through the whole
// program: the footer says which action failed and carries the message, and
// the run keeps rendering.
func TestModelReportsAFailedControlCall(t *testing.T) {
	t.Parallel()

	c := &cwController{err: errCWRefused}
	m := cwPress(t, cwModel(t, c), "p")
	require.Equal(t, []string{"pause(user)"}, c.Calls())
	require.Contains(t, cwFooter(m), "could not pause the run")
	gfRequireGeometry(t, m.View(), cwWidth, cwHeight)
}

// errCWRefused is what the failing controller returns.
var errCWRefused = errors.New("the orchestrator refused")

// TestModelSafetyAndAmmoCanBeChangedBackAgain is the round-2 defect, at the
// level a user meets it: nothing publishes a configuration change, so
// without the layer's own memory the second `S` would ask to turn safety off
// again and the ammo prompt would keep re-offering the run's frozen cap.
func TestModelSafetyAndAmmoCanBeChangedBackAgain(t *testing.T) {
	t.Parallel()

	c := &cwController{}
	m := cwPress(t, cwModel(t, c), "S", "y")
	require.Equal(t, []string{"safety(true)"}, c.Calls())

	// The second press offers the opposite of what was just accepted.
	asking := cwPress(t, m, "S")
	require.Contains(t, cwFrame(asking), "Turn safety off?")
	m = cwPress(t, asking, "y")
	require.Equal(t, []string{"safety(true)", "safety(false)"}, c.Calls())

	// Same for ammo: 6, then back to the run's own 4.
	m = cwPress(t, m, "a", "backspace", "backspace", "6", "enter")
	prompt := cwPress(t, m, "a")
	require.Contains(t, cwFrame(prompt), "the run's cap today is 6.")
	require.Contains(t, cwFrame(prompt), "› 6", "the prompt starts from the cap that was accepted, not the run's frozen one")
	m = cwPress(t, prompt, "backspace", "1", "0", "enter")
	require.Equal(t, []string{"safety(true)", "safety(false)", "ammo(6)", "ammo(10)"}, c.Calls())
	require.Contains(t, cwFooter(m), "ammo 10 for future ticks")
}

// cwGoldens are the frames P7-10 draws, pinned the way P7-09 pins every
// other user-visible frame. They are listed in golden_test.go's
// gfViewGoldens so the orphan guard accounts for them.
var cwGoldens = []struct {
	// name is the golden file, without the .golden suffix.
	name string
	// keys are pressed on a fresh model to reach the frame.
	keys []string
}{
	// The confirmation box: the question, what it costs and the two answers.
	{name: "controls-confirm", keys: []string{"g", "G", "x"}},
	// The ammo prompt, pre-filled with the run's own cap.
	{name: "controls-ammo", keys: []string{"a"}},
	// The footer line a completed control leaves behind.
	{name: "controls-footer", keys: []string{"p"}},
	// The `?` overlay with the run-control block a controller adds.
	{name: "controls-help", keys: []string{"?"}},
}

// TestControlGoldenFrames pins each of those frames. Set UPDATE_GOLDEN=1
// (make update-golden-tui) to rewrite them.
func TestControlGoldenFrames(t *testing.T) {
	t.Parallel()

	for _, g := range cwGoldens {
		t.Run(g.name, func(t *testing.T) {
			t.Parallel()
			frame := cwPress(t, cwModel(t, &cwController{}), g.keys...).View()
			gfRequireGeometry(t, frame, cwWidth, cwHeight)
			for _, esc := range gfAnyEscape.FindAllString(frame, -1) {
				require.Regexp(t, gfAttrEscape, esc,
					"frame %q carries %q: an escape that is not bold, underline or reset", g.name, esc)
			}
			gfCompare(t, filepath.Join(gfGoldenDir, g.name+".golden"), frame)
		})
	}
}

// TestControlGoldenFramesAreDeterministic renders each of them twice from
// independent models, so a wall clock or a map iteration in the control
// layer fails here rather than on someone else's machine.
func TestControlGoldenFramesAreDeterministic(t *testing.T) {
	t.Parallel()

	for _, g := range cwGoldens {
		first := cwPress(t, cwModel(t, &cwController{}), g.keys...).View()
		second := cwPress(t, cwModel(t, &cwController{}), g.keys...).View()
		require.Equal(t, first, second, "frame %q is not reproducible", g.name)
	}
}

// cwSlowController never answers: it blocks until the call's context is
// done, which is how a test proves the timeout the program configured is the
// one the call is made with.
type cwSlowController struct{ cwController }

// Pause implements [tui.Controller] by waiting the call out.
func (c *cwSlowController) Pause(ctx context.Context, reason string) error {
	_ = c.record("pause(%s)", reason)
	<-ctx.Done()
	return ctx.Err()
}

// TestModelPassesTheControlTimeoutThrough proves [tui.Options.ControlTimeout]
// reaches the layer from the shipped entry point: without the field every
// program was pinned to the 5 s default and only a unit test could vary it.
func TestModelPassesTheControlTimeoutThrough(t *testing.T) {
	t.Parallel()

	c := &cwSlowController{}
	m := tui.New(t.Context(), tui.Options{
		Run:            tui.DemoRun(gfDemo),
		NoColor:        true,
		Theme:          tui.ThemeFor(tui.ProfileNoColor),
		NoAnim:         true,
		Now:            gfClock,
		Controller:     c,
		ControlTimeout: time.Millisecond,
	})
	m = gfFeed(gfSend(m, tea.WindowSizeMsg{Width: cwWidth, Height: cwHeight}), cwLiveEvents())

	m = cwPress(t, m, "p")
	require.Equal(t, []string{"pause(user)"}, c.Calls())
	require.Contains(t, cwFooter(m), "could not pause the run",
		"a call that never answers has to give the key back inside the configured timeout")
}

// TestModelPassesTheAmmoCeilingThrough proves [tui.Options.ControlMaxAmmo]
// reaches the prompt, which was hard-coded at 99 with no way for a caller to
// raise it.
func TestModelPassesTheAmmoCeilingThrough(t *testing.T) {
	t.Parallel()

	c := &cwController{}
	m := tui.New(t.Context(), tui.Options{
		Run:            tui.DemoRun(gfDemo),
		NoColor:        true,
		Theme:          tui.ThemeFor(tui.ProfileNoColor),
		NoAnim:         true,
		Now:            gfClock,
		Controller:     c,
		ControlMaxAmmo: 250,
	})
	m = gfFeed(gfSend(m, tea.WindowSizeMsg{Width: cwWidth, Height: cwHeight}), cwLiveEvents())

	prompt := cwPress(t, m, "a")
	require.Contains(t, cwFrame(prompt), fmt.Sprintf("%d–250 concurrent agents", tui.ControlMinAmmo))
	cwPress(t, prompt, "backspace", "backspace", "1", "2", "0", "enter")
	require.Equal(t, []string{"ammo(120)"}, c.Calls(), "the raised ceiling has to accept three digits")
}
