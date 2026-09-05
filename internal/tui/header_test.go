package tui_test

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/events"
	"slop-cannon/internal/tui"
)

// P7-01 — the chrome at the size the TUI is specified in.
//
// The Phase 7 exit criteria, the golden frames and every terminal that has
// not been resized are 80×24, so that is the size the header has to show the
// run in: id, mode, ammo used/total, safety, and the run's status word. The
// other header tests use wider terminals, where the line has room to spare.

// hdrTerm is the terminal the header must fit.
const (
	hdrWidth  = 80
	hdrHeight = 24
)

// hdrModel builds a model of the scripted run, sized w×h, with colour and
// animation off so the assertions are about the header alone (P7-06 draws
// sprites over the chrome).
func hdrModel(t *testing.T, w, h int) tui.Model {
	t.Helper()
	m := tui.New(t.Context(), tui.Options{
		Run:     tui.DemoRun(tui.DemoOptions{}),
		NoColor: true,
		NoAnim:  true,
	})
	return tuiResize(m, w, h)
}

// hdrLine returns the header line of a rendered frame.
func hdrLine(m tui.Model) string {
	return strings.SplitN(m.View(), "\n", 2)[0]
}

// hdrEvent wraps an event as the message the update loop takes.
func hdrEvent(ev events.Event) tea.Msg { return tui.EventMsg{Event: ev} }

func TestModelHeaderShowsEveryFieldAtEightyColumns(t *testing.T) {
	t.Parallel()
	meta := events.MetaAt(tui.DemoRunID, tui.DemoStart)
	for _, tc := range []struct {
		name   string
		msg    tea.Msg
		status string
	}{
		{name: "running"},
		{name: "paused", msg: hdrEvent(events.RunPaused{Meta: meta, Reason: "cost_cap"}), status: "paused"},
		{name: "stopping", msg: hdrEvent(events.RunStopped{Meta: meta, Reason: "key"}), status: "stopping"},
		{
			name:   "finished",
			msg:    hdrEvent(events.RunFinished{Meta: meta, Reason: events.FinishCreditsExhausted}),
			status: "finished",
		},
		{name: "disconnected", msg: tui.StreamClosedMsg{}, status: "disconnected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := hdrModel(t, hdrWidth, hdrHeight)
			if tc.msg != nil {
				m = tuiUpdate(m, tc.msg)
			}
			line := hdrLine(m)
			require.LessOrEqual(t, lipgloss.Width(line), hdrWidth, "the header overflows the terminal: %q", line)
			for _, want := range []string{string(tui.DemoRunID), "autopilot", "ammo 0/10", "safety off"} {
				require.Contains(t, line, want, "the 80-column header drops a field P7-01 requires")
			}
			if tc.status != "" {
				require.Contains(t, line, tc.status, "the 80-column header never says the run is %s", tc.status)
			}
		})
	}
}

func TestModelHeaderNeverOverflowsTheTerminal(t *testing.T) {
	t.Parallel()
	for _, w := range []int{20, 24, 40, 60, 72, 80, 81, 100, 120, 200} {
		m := tuiUpdate(hdrModel(t, w, hdrHeight), tui.StreamClosedMsg{})
		line := hdrLine(m)
		require.LessOrEqual(t, lipgloss.Width(line), w, "the header overflows %d columns: %q", w, line)
		if w >= tui.CannonFieldWidth+len(tui.DemoRunID) {
			require.Contains(t, line, string(tui.DemoRunID), "the run id is the last field standing")
		}
	}
}

func TestModelHeaderKeepsTheTitleWhenThereIsRoom(t *testing.T) {
	t.Parallel()
	// The title is the first thing dropped: the sprite beside it already
	// says which tool this is.
	require.Contains(t, hdrLine(hdrModel(t, 120, hdrHeight)), "slop-cannon")
	require.NotContains(t, hdrLine(hdrModel(t, hdrWidth, hdrHeight)), "slop-cannon")
}

func TestModelHeaderPrefersDisconnectedToStopping(t *testing.T) {
	t.Parallel()
	// Once the events stop arriving the TUI cannot vouch for "stopping"; the
	// stale screen is the more important thing to say.
	m := tuiUpdate(hdrModel(t, 120, hdrHeight), hdrEvent(events.RunStopped{
		Meta: events.MetaAt(tui.DemoRunID, tui.DemoStart), Reason: "key",
	}))
	require.Contains(t, hdrLine(m), "stopping")
	require.Contains(t, hdrLine(tuiUpdate(m, tui.StreamClosedMsg{})), "disconnected")
}

func TestModelHeaderShowsTheFinishReasonWhenItFits(t *testing.T) {
	t.Parallel()
	finished := hdrEvent(events.RunFinished{
		Meta:   events.MetaAt(tui.DemoRunID, tui.DemoStart),
		Reason: events.FinishCreditsExhausted,
	})
	require.Contains(t, hdrLine(tuiUpdate(hdrModel(t, 140, hdrHeight), finished)), "finished: credits_exhausted")
	require.Contains(t, hdrLine(tuiUpdate(hdrModel(t, hdrWidth, hdrHeight), finished)), "finished")
}

func TestModelFollowingStopsWhenTheLogCursorMoves(t *testing.T) {
	t.Parallel()
	m := tuiFeed(hdrModel(t, hdrWidth, hdrHeight))
	m = tuiUpdate(m, tuiKey("5"))
	require.Equal(t, tui.TabLog, m.Tab())
	require.True(t, m.Following(), "the log follows the stream until the cursor is moved")

	m = tuiUpdate(m, tuiKey("up"))
	require.False(t, m.Following())

	// Leaving and coming back starts following again.
	m = tuiUpdate(tuiUpdate(m, tuiKey("1")), tuiKey("5"))
	require.True(t, m.Following())
}

// TestRunDrawsTheRunAndQuitsOnConfirmation drives the exported entry point,
// [tui.Run], rather than a model under teatest: it is what `sc run` and `sc
// tui-demo` call, and nothing else covers the program it builds.
func TestRunDrawsTheRunAndQuitsOnConfirmation(t *testing.T) {
	t.Parallel()
	var out tuiSyncBuffer
	in, keys := io.Pipe()
	t.Cleanup(func() { _ = keys.Close() })

	stopped := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- tui.Run(t.Context(), tui.Options{
			Run:     tui.DemoRun(tui.DemoOptions{}),
			NoColor: true,
			NoAnim:  true,
			Input:   in,
			Output:  &out,
			OnStop:  func() { close(stopped) },
		})
	}()

	hdrWaitFor(t, &out, string(tui.DemoRunID))
	hdrType(t, keys, "q")
	hdrWaitFor(t, &out, "Stop run")
	hdrType(t, keys, "y")

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(tuiWaitTimeout):
		t.Fatal("Run never came back after the stop was confirmed")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("confirming the stop never reached OnStop")
	}
}

// TestDemoRendersElapsedTimesFromTheScript is the regression test for a demo
// that measured a live agent against the wall clock: the script starts on
// 2026-01-01, so the agent cards read `5885h30m57s` instead of the seconds
// the scripted run has been going.
func TestDemoRendersElapsedTimesFromTheScript(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var out tuiSyncBuffer
	in, keys := io.Pipe()
	t.Cleanup(func() { _ = keys.Close() })

	done := make(chan error, 1)
	go func() {
		done <- tui.Demo(ctx, tui.DemoOptions{Delay: 15 * time.Millisecond}, tui.Options{
			NoColor: true,
			NoAnim:  true,
			Input:   in,
			Output:  &out,
		})
	}()
	// The agents view is where a live agent's elapsed time is drawn.
	go func() { _, _ = keys.Write([]byte("2")) }()

	hdrWaitFor(t, &out, "engineer")
	cancel()
	select {
	case <-done:
	case <-time.After(tuiWaitTimeout):
		t.Fatal("Demo ignored the cancellation")
	}
	require.NotRegexp(t, `[0-9]+h[0-9]+m`, out.String(),
		"an elapsed time in hours means the views are reading the wall clock, not the script")
}

// hdrWaitFor blocks until the program has drawn want.
func hdrWaitFor(t *testing.T, out *tuiSyncBuffer, want string) {
	t.Helper()
	require.Eventually(t, func() bool { return strings.Contains(out.String(), want) },
		tuiWaitTimeout, 10*time.Millisecond, "the program never drew %q; it drew:\n%s", want, out.String())
}

// hdrType sends key presses to a running program.
func hdrType(t *testing.T, keys io.Writer, s string) {
	t.Helper()
	_, err := keys.Write([]byte(s))
	require.NoError(t, err)
}

// --- P6-51: the run vitals in the header -----------------------------------

// hdrVitalsLine is what orchestrator.RunVitals.Header() renders for a run
// whose `gh` has been unreachable for five minutes and which is still
// waiting on one gate. The TUI takes it as a string so this package need not
// import the orchestrator.
const hdrVitalsLine = "gh down 5m0s · 1 pending"

// hdrVitalsFull is the widest the line gets: every clause at once. It does
// not fit an 80-column terminal beside the run's own fields, which is what
// the header ladder is for.
const hdrVitalsFull = "plan in 1m30s · gh down 5m0s · 1 pending · 2 left alone"

// hdrVitalsModel is [hdrModel] with a vitals source attached.
func hdrVitalsModel(t *testing.T, w, h int, vitals string) tui.Model {
	t.Helper()
	m := tui.New(t.Context(), tui.Options{
		Run:     tui.DemoRun(tui.DemoOptions{}),
		NoColor: true,
		NoAnim:  true,
		Vitals:  func() string { return vitals },
	})
	return tuiResize(m, w, h)
}

// TestModelHeaderShowsTheRunVitals is P6-51's exit criterion: a `gh` outage
// is on the header line, at the 80 columns the phase is specified in, with
// no log to read.
func TestModelHeaderShowsTheRunVitals(t *testing.T) {
	t.Parallel()
	for _, w := range []int{hdrWidth, 100, 140, 200} {
		line := hdrLine(hdrVitalsModel(t, w, hdrHeight, hdrVitalsLine))
		require.Contains(t, line, "gh down 5m0s", "the %d-column header hides the outage", w)
		require.Contains(t, line, "1 pending")
		require.LessOrEqual(t, lipgloss.Width(line), w, "the header overflows %d columns: %q", w, line)
		require.Contains(t, line, string(tui.DemoRunID), "the run id is never given up for the vitals")
		require.Contains(t, line, "safety off", "the safety flag outranks the vitals")
	}
}

// TestModelHeaderWithoutVitalsIsUnchanged pins the other side of it: a run
// with nothing to explain — and a caller that wired no source in at all —
// draws exactly the header it drew before P6-51, so none of P7-09's frames
// move.
func TestModelHeaderWithoutVitalsIsUnchanged(t *testing.T) {
	t.Parallel()
	for _, w := range []int{40, 60, hdrWidth, 120, 200} {
		want := hdrLine(hdrModel(t, w, hdrHeight))
		require.Equal(t, want, hdrLine(hdrVitalsModel(t, w, hdrHeight, "")),
			"an empty vitals line changes the %d-column header", w)
		require.Equal(t, want, hdrLine(hdrVitalsModel(t, w, hdrHeight, "   ")),
			"a blank vitals line changes the %d-column header", w)
	}
}

// TestModelHeaderVitalsAreSanitised keeps the rule the rest of the chrome
// follows: the line comes from outside this package, so it may not carry an
// escape sequence or a newline into the frame.
func TestModelHeaderVitalsAreSanitised(t *testing.T) {
	t.Parallel()
	m := hdrVitalsModel(t, 200, hdrHeight, "gh down\x1b[31m 5m0s\nplan now")
	line := hdrLine(m)
	require.NotContains(t, line, "\x1b[31m")
	require.Contains(t, line, "gh down")
	require.Len(t, strings.Split(m.View(), "\n"), hdrHeight, "the vitals line added a row")
}

// TestModelHeaderVitalsGiveWayToTheRunIdentity checks the ladder both ways:
// the vitals outrank the mode and the ammo, which hardly move and are in the
// views anyway, but they are dropped before the run id and the safety flag —
// a header that names no run, or claims the wrong delivery mode, is worse
// than one that cannot explain the quiet.
func TestModelHeaderVitalsGiveWayToTheRunIdentity(t *testing.T) {
	t.Parallel()
	// Every clause at once does not fit 80 columns beside a 20-character run
	// id, so it goes; the identity fields stay.
	line := hdrLine(hdrVitalsModel(t, hdrWidth, hdrHeight, hdrVitalsFull))
	require.LessOrEqual(t, lipgloss.Width(line), hdrWidth)
	require.Contains(t, line, string(tui.DemoRunID))
	require.Contains(t, line, "safety off", "the vitals are dropped before the safety flag")
	require.NotContains(t, line, "left alone", "the whole vitals field is dropped, never half of it")

	// Given the room it is drawn in full, and the mode and the ammo come
	// back with it.
	wide := hdrLine(hdrVitalsModel(t, 200, hdrHeight, hdrVitalsFull))
	require.Contains(t, wide, hdrVitalsFull)
	require.Contains(t, wide, "autopilot")
	require.Contains(t, wide, "ammo 0/10")

	// In between, the vitals win the last columns from the mode and the ammo.
	mid := hdrLine(hdrVitalsModel(t, 100, hdrHeight, hdrVitalsFull))
	require.Contains(t, mid, hdrVitalsFull)
	require.NotContains(t, mid, "autopilot")
}

// TestHeaderVitalsGoldenFrame is the golden P6-51 asks for: the whole 80x24
// frame of a run whose gh has been down for five minutes.
func TestHeaderVitalsGoldenFrame(t *testing.T) {
	t.Parallel()
	m := tui.New(t.Context(), tui.Options{
		Run:     tui.DemoRun(gfDemo),
		NoColor: true,
		Theme:   tui.ThemeFor(tui.ProfileNoColor),
		NoAnim:  true,
		Now:     gfClock,
		Vitals:  func() string { return hdrVitalsLine },
	})
	m = gfFeed(gfSend(m, tea.WindowSizeMsg{Width: hdrWidth, Height: hdrHeight}), tui.DemoEvents(gfDemo))
	frame := m.View()
	gfRequireGeometry(t, frame, hdrWidth, hdrHeight)
	require.Contains(t, gfPlain(frame), "gh down 5m0s")
	gfCompare(t, filepath.Join(gfGoldenDir, "header-vitals.golden"), frame)
}

// --- P7-22: the header and the run controls agree --------------------------

// hdrPlainLine is the header line of a frame with the styling stripped.
func hdrPlainLine(m tui.Model) string {
	return strings.SplitN(gfPlain(m.View()), "\n", 2)[0]
}

// TestModelHeaderFollowsAnAmmoOrSafetyChange is P7-22's first clause:
// changing ammo or safety moves the header. Both keys go through the real
// program and a real controller, and the header is read out of the frame the
// user would see.
func TestModelHeaderFollowsAnAmmoOrSafetyChange(t *testing.T) {
	t.Parallel()
	c := &cwController{}
	m := cwModel(t, c)
	require.Contains(t, hdrPlainLine(m), "/10", "the run starts with the ammo the demo run was fired with")
	require.Contains(t, hdrPlainLine(m), "safety off")

	m = cwPress(t, m, "a", "6", "enter")
	require.Equal(t, []string{"ammo(6)"}, c.Calls(), "the key never reached the orchestrator")
	require.Contains(t, hdrPlainLine(m), "/6", "the header still draws the ammo the run was fired with")
	require.NotContains(t, hdrPlainLine(m), "/10")

	m = cwPress(t, m, "S", "y")
	require.Equal(t, []string{"ammo(6)", "safety(true)"}, c.Calls())
	require.Contains(t, hdrPlainLine(m), "safety on", "the header still draws the run's original safety flag")
}

// TestModelHeaderIgnoresARefusedChange is the other half: a controller that
// refuses the change leaves the header alone, because nothing was accepted.
func TestModelHeaderIgnoresARefusedChange(t *testing.T) {
	t.Parallel()
	c := &cwController{err: errors.New("no")}
	m := cwModel(t, c)
	m = cwPress(t, m, "a", "6", "enter")
	m = cwPress(t, m, "S", "y")
	require.Contains(t, hdrPlainLine(m), "/10", "a refused change moved the header")
	require.Contains(t, hdrPlainLine(m), "safety off")
}
