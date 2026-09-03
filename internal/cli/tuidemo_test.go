package cli

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/tui"
)

// tuiDemoEvents runs `sc tui-demo --events` with extra arguments and decodes
// the JSONL it printed.
func tuiDemoEvents(t *testing.T, args ...string) []events.Event {
	t.Helper()
	out, err := rootRun(t, append([]string{"tui-demo", "--events"}, args...)...)
	require.NoError(t, err)
	evs, err := events.ReadAll(strings.NewReader(out))
	require.NoError(t, err)
	return evs
}

func TestTUIDemoIsHidden(t *testing.T) {
	cmd, _, err := NewRootCommand(io.Discard).Find([]string{"tui-demo"})
	require.NoError(t, err)
	require.True(t, cmd.Hidden, "tui-demo is a development tool, not part of the interface")

	help, err := rootRun(t, "--help")
	require.NoError(t, err)
	require.NotContains(t, help, "tui-demo", "a hidden command must not change the help output")

	docs, err := Docs()
	require.NoError(t, err)
	require.NotContains(t, docs, "tui-demo", "a hidden command must not change docs/cli.md")
}

func TestTUIDemoHelpWorks(t *testing.T) {
	out, err := rootRun(t, "tui-demo", "--help")
	require.NoError(t, err)
	require.Contains(t, out, "scripted event stream")
	for _, flag := range []string{FlagDemoEvents, FlagDemoDelay, FlagDemoBrief, FlagDemoSafety, FlagDemoAmmo, FlagDemoAltScreen} {
		require.Contains(t, out, "--"+flag)
	}
}

func TestTUIDemoEventsPrintsTheWholeScript(t *testing.T) {
	got := tuiDemoEvents(t)
	require.Equal(t, tui.DemoEvents(tui.DemoOptions{}), got)

	kinds := make(map[events.Kind]bool, len(got))
	for _, ev := range got {
		kinds[ev.Kind()] = true
	}
	for _, k := range events.Kinds() {
		require.True(t, kinds[k], "the printed script never mentions %s", k)
	}
}

func TestTUIDemoEventsHonoursTheScriptFlags(t *testing.T) {
	got := tuiDemoEvents(t, "--brief", "make it faster", "--safety", "--ammo", "6")
	require.NotEmpty(t, got)
	started, ok := got[0].(events.RunStarted)
	require.True(t, ok)
	require.Equal(t, domain.RunModeBrief, started.Info.Mode)
	require.Equal(t, "make it faster", started.Info.Brief)
	require.True(t, started.Info.Config.Safety)
	require.Equal(t, 6, started.Info.Config.Ammo)
}

func TestTUIDemoEventsIgnoresPacing(t *testing.T) {
	// The pacing flag changes when events are published, never what they are.
	require.Equal(t, tuiDemoEvents(t), tuiDemoEvents(t, "--delay", "0"))
	require.Equal(t, tuiDemoEvents(t), tuiDemoEvents(t, "--delay", "1s"))
}

func TestTUIDemoRejectsANegativeDelay(t *testing.T) {
	_, err := rootRun(t, "tui-demo", "--delay", "-1s")
	require.Error(t, err)
	require.Contains(t, err.Error(), "--delay must not be negative")
}

func TestTUIDemoDelayFlagDefaultsToTheScriptPacing(t *testing.T) {
	cmd, _, err := NewRootCommand(io.Discard).Find([]string{"tui-demo"})
	require.NoError(t, err)
	d, err := cmd.Flags().GetDuration(FlagDemoDelay)
	require.NoError(t, err)
	require.Equal(t, tui.DemoDelay, d)
	require.Equal(t, tui.DemoDelay, 250*time.Millisecond)
}

// tuiDemoFakeTerminal makes the `sc tui-demo` terminal check answer tty for
// the rest of the test, so the TUI branch can be reached without a pty.
func tuiDemoFakeTerminal(t *testing.T, tty bool) {
	t.Helper()
	prev := isTerminal
	isTerminal = func(*os.File) bool { return tty }
	t.Cleanup(func() { isTerminal = prev })
}

// tuiDemoCall records what the command asked the TUI to render.
type tuiDemoCall struct {
	calls int
	demo  tui.DemoOptions
	opts  tui.Options
}

// tuiDemoFakeTUI replaces the TUI entry point for the rest of the test. When
// stop is set the fake confirms a stop, the way `q` then `y` does.
func tuiDemoFakeTUI(t *testing.T, stop bool) *tuiDemoCall {
	t.Helper()
	rec := &tuiDemoCall{}
	prev := demoTUI
	demoTUI = func(ctx context.Context, demo tui.DemoOptions, opts tui.Options) error {
		require.NotNil(t, ctx, "the demo must run under the command's context")
		rec.calls++
		rec.demo, rec.opts = demo, opts
		if stop && opts.OnStop != nil {
			opts.OnStop()
		}
		return nil
	}
	t.Cleanup(func() { demoTUI = prev })
	return rec
}

func TestTUIDemoStartsTheTUIWithTheFlagsItWasGiven(t *testing.T) {
	tuiDemoFakeTerminal(t, true)
	rec := tuiDemoFakeTUI(t, false)

	_, err := rootRun(t, "tui-demo", "--alt-screen=false", "--no-anim", "--ammo", "6",
		"--brief", "make the http client retry", "--safety", "--delay", "0")
	require.NoError(t, err)

	require.Equal(t, 1, rec.calls, "the command never started the TUI")
	require.False(t, rec.opts.AltScreen, "--alt-screen=false must reach the program")
	require.True(t, rec.opts.NoAnim)
	require.NotNil(t, rec.opts.OnStop, "the demo reports a confirmed stop through OnStop")
	require.Equal(t, 6, rec.demo.Ammo)
	require.Equal(t, "make the http client retry", rec.demo.Brief)
	require.True(t, rec.demo.Safety)
}

func TestTUIDemoDrawsOnTheAltScreenByDefault(t *testing.T) {
	tuiDemoFakeTerminal(t, true)
	rec := tuiDemoFakeTUI(t, false)

	_, err := rootRun(t, "tui-demo")
	require.NoError(t, err)
	require.True(t, rec.opts.AltScreen)
	require.Equal(t, tui.DemoAmmo, rec.demo.Ammo)
}

func TestTUIDemoReportsAConfirmedStop(t *testing.T) {
	tuiDemoFakeTerminal(t, true)
	tuiDemoFakeTUI(t, true)

	out, err := rootRun(t, "tui-demo")
	require.NoError(t, err)
	require.Contains(t, out, "sc: stop confirmed")
}

func TestTUIDemoWithoutATerminalSaysWhatToRunInstead(t *testing.T) {
	tuiDemoFakeTerminal(t, false)
	rec := tuiDemoFakeTUI(t, false)

	_, err := rootRun(t, "tui-demo")
	require.Error(t, err)
	require.Zero(t, rec.calls, "a program that cannot read the keyboard must not be started")
	require.Contains(t, err.Error(), "needs a terminal")
	require.Contains(t, err.Error(), "--"+FlagDemoEvents)
	require.Contains(t, err.Error(), "--"+FlagRunHeadless)
}

func TestTUIDemoEventsNeedsNoTerminal(t *testing.T) {
	tuiDemoFakeTerminal(t, false)
	rec := tuiDemoFakeTUI(t, false)

	out, err := rootRun(t, "tui-demo", "--events")
	require.NoError(t, err, "--events is how the script is read from a pipe")
	require.Zero(t, rec.calls)
	require.Contains(t, out, `"kind":"run_started"`)
}
