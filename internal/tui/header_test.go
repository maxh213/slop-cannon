package tui_test

import (
	"context"
	"io"
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
