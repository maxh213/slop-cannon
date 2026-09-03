package cli

import (
	"io"
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
