package cli

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/tui"
)

// P7-06 — `sc tui-demo --no-anim`.
//
// `sc run --no-anim` is parsed into the run config, but Phase 8 is what
// starts a TUI from it; until then `sc tui-demo` is the only command that
// draws one, so it is the only place the flag can be exercised. Without it
// `SC_NO_ANIM` would be the sole way to turn the cannon off.

// tuiDemoAnimRun runs `sc tui-demo` with the TUI stubbed out and returns the
// options the command would have started it with. It restores the stubs
// afterwards, so it must not run in parallel with another test.
func tuiDemoAnimRun(t *testing.T, args ...string) tui.Options {
	t.Helper()
	demo, term := demoTUI, isTerminal
	t.Cleanup(func() { demoTUI, isTerminal = demo, term })

	var got tui.Options
	demoTUI = func(_ context.Context, _ tui.DemoOptions, opts tui.Options) error {
		got = opts
		return nil
	}
	isTerminal = func(*os.File) bool { return true }

	_, err := rootRun(t, append([]string{"tui-demo", "--delay", "0"}, args...)...)
	require.NoError(t, err)
	return got
}

func TestTUIDemoNoAnimFlagReachesTheTUI(t *testing.T) {
	require.False(t, tuiDemoAnimRun(t).NoAnim, "the animations are on by default")
	require.True(t, tuiDemoAnimRun(t, "--"+FlagDemoNoAnim).NoAnim)
	require.False(t, tuiDemoAnimRun(t, "--"+FlagDemoNoAnim+"=false").NoAnim)
}

func TestTUIDemoNoAnimIsDocumentedInTheHelp(t *testing.T) {
	out, err := rootRun(t, "tui-demo", "--help")
	require.NoError(t, err)
	require.Contains(t, out, "--"+FlagDemoNoAnim)
	require.Contains(t, out, tui.NoAnimEnv, "the help says the environment variable does the same")
}
