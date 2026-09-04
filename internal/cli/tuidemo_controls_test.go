package cli

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/tui"
)

// P7-10 — `sc tui-demo`'s stand-in orchestrator.
//
// The demo has no run to pause, stop or re-fire, so the controls are wired
// to a recorder: that is what makes the keys live in the demo rather than
// disabled, and what the human driving a pty reads on the way out. The
// command guards the whole branch behind a terminal check, so without these
// tests nothing in the repository executes a single one of its methods.

// tuiDemoDriveControls calls every [tui.Controller] method on the controller
// the command attached, in the order the help overlay lists the keys.
func tuiDemoDriveControls(t *testing.T, c tui.Controller) {
	t.Helper()
	require.NotNil(t, c, "the demo must attach a controller, or its keys are dead")
	ctx := t.Context()
	require.NoError(t, c.Pause(ctx, tui.ControlReason))
	require.NoError(t, c.Resume(ctx, tui.ControlReason))
	require.NoError(t, c.Stop(ctx, tui.ControlReason))
	require.NoError(t, c.Refire(ctx, domain.BeadID("demo-3"), tui.ControlReason))
	require.NoError(t, c.MarkDone(ctx, domain.BeadID("demo-2"), tui.ControlReason))
	require.NoError(t, c.SetSafety(ctx, true))
	require.NoError(t, c.SetAmmo(ctx, 6))
}

func TestTUIDemoReportsEveryRunControlItWasAskedFor(t *testing.T) {
	tuiDemoFakeTerminal(t, true)
	rec := tuiDemoFakeTUI(t, false)

	// The fake TUI stands in for the user pressing the keys: it drives the
	// controller the command handed the program.
	prev := demoTUI
	demoTUI = func(ctx context.Context, demo tui.DemoOptions, opts tui.Options) error {
		tuiDemoDriveControls(t, opts.Controller)
		return prev(ctx, demo, opts)
	}

	out, err := rootRun(t, "tui-demo")
	require.NoError(t, err)
	require.Equal(t, 1, rec.calls, "the command never started the TUI")
	for _, want := range []string{
		"sc: run control: pause (user)",
		"sc: run control: resume (user)",
		"sc: run control: stop (user)",
		"sc: run control: re-fire demo-3 (user)",
		"sc: run control: mark demo-2 done (user)",
		"sc: run control: safety on",
		"sc: run control: ammo 6",
	} {
		require.Contains(t, out, want)
	}
}

func TestTUIDemoSaysNothingWhenNoControlWasUsed(t *testing.T) {
	tuiDemoFakeTerminal(t, true)
	tuiDemoFakeTUI(t, false)

	out, err := rootRun(t, "tui-demo")
	require.NoError(t, err)
	require.NotContains(t, out, "sc: run control:")
}

func TestTUIDemoControllerRecordsSafetyBothWays(t *testing.T) {
	t.Parallel()

	c := &demoController{}
	require.NoError(t, c.SetSafety(t.Context(), false))
	require.NoError(t, c.SetSafety(t.Context(), true))
	require.Equal(t, []string{"safety off", "safety on"}, c.Calls())
}
