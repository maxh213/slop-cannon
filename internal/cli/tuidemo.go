package cli

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/tui"
)

// Flag names of the hidden `sc tui-demo` command.
const (
	FlagDemoEvents    = "events"
	FlagDemoDelay     = "delay"
	FlagDemoBrief     = "brief"
	FlagDemoSafety    = "safety"
	FlagDemoAmmo      = "ammo"
	FlagDemoAltScreen = "alt-screen"
	FlagDemoNoAnim    = "no-anim"
	FlagDemoReplay    = "replay"
)

const tuiDemoLong = `Render the TUI against a scripted event stream instead of a real run.

The script covers every event kind: planning, a bead through a full feedback
loop (change request, pushback, arbitration, approval, delivery), a failing
agent, an escalation, and a pause/resume/stop. It touches no repository, no
configuration and no network, so TUI work never needs a live cannon.

With --replay it renders a recorded run instead of the script: any JSONL run
log, such as the events.jsonl of a run directory or the output of sc run
--headless --json, which is how a real run can be watched again without
firing one.

The command is hidden: it is a development tool, not part of the interface.`

const tuiDemoExamples = `sc tui-demo
sc tui-demo --delay 0 --safety
sc tui-demo --no-anim
sc tui-demo --events
sc tui-demo --replay ~/.local/state/slop-cannon/<repo>/runs/<run-id>/events.jsonl`

func init() { register(newTUIDemoCommand) }

// newTUIDemoCommand builds the hidden `sc tui-demo` command (P7-01). It is
// Hidden, so it appears in neither the help output nor docs/cli.md.
func newTUIDemoCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tui-demo",
		Short:   "Run the TUI against a scripted event stream (development tool)",
		Long:    tuiDemoLong,
		Example: indent(tuiDemoExamples, "  "),
		Hidden:  true,
		Args:    cobra.NoArgs,
		RunE:    runTUIDemo,
	}
	f := cmd.Flags()
	f.Bool(FlagDemoEvents, false, "print the scripted events as JSONL and exit, without starting the TUI")
	f.Duration(FlagDemoDelay, tui.DemoDelay, "wall-clock pause between scripted events (0 plays the whole script at once)")
	f.String(FlagDemoBrief, "", "run the script in brief mode with this `BRIEF` instead of autopilot")
	f.Bool(FlagDemoSafety, false, "script a safety-on run (pull request) instead of a direct merge")
	f.Int(FlagDemoAmmo, tui.DemoAmmo, "ammo shown in the header (`N` agents at once)")
	f.Bool(FlagDemoAltScreen, true, "draw on the alternate screen buffer")
	f.Bool(FlagDemoNoAnim, false, "disable the cannon animations (also honours SC_NO_ANIM), like `sc run --no-anim`")
	f.String(FlagDemoReplay, "", "render the recorded run in this JSONL `FILE` instead of the scripted stream (paced by --"+FlagDemoDelay+")")
	return cmd
}

// runTUIDemo implements `sc tui-demo`.
func runTUIDemo(cmd *cobra.Command, _ []string) error {
	opts, err := GlobalOpts(cmd)
	if err != nil {
		return err
	}
	demo, err := demoOptionsFrom(cmd)
	if err != nil {
		return err
	}
	f := cmd.Flags()
	replay, err := f.GetString(FlagDemoReplay)
	if err != nil {
		return err
	}
	listOnly, err := f.GetBool(FlagDemoEvents)
	if err != nil {
		return err
	}
	if listOnly {
		if replay != "" {
			return fmt.Errorf("--%s prints the scripted stream; a recording is already JSONL, so read the file itself", FlagDemoEvents)
		}
		return writeDemoEvents(cmd, demo)
	}
	// A recording is read before the terminal is claimed, so a missing or
	// damaged file is an ordinary error rather than a blank alternate screen.
	var script []tui.ScriptedEvent
	if replay != "" {
		if script, err = replayScript(replay, demo.Delay); err != nil {
			return err
		}
	}
	alt, err := f.GetBool(FlagDemoAltScreen)
	if err != nil {
		return err
	}
	noAnim, err := f.GetBool(FlagDemoNoAnim)
	if err != nil {
		return err
	}
	if err := requireTerminal(); err != nil {
		return err
	}

	stopped := false
	ctrl := &demoController{}
	tuiOpts := tui.Options{
		NoColor:    opts.NoColor,
		NoAnim:     noAnim,
		AltScreen:  alt,
		OnStop:     func() { stopped = true },
		Controller: ctrl,
	}
	if replay != "" {
		// A recording has its own run, clock and controls: nothing here can
		// pause or re-fire a run that has already happened, so the keys stay
		// disabled rather than reporting a call that means nothing.
		tuiOpts.Controller = nil
		err = replayTUI(cmd.Context(), script, tuiOpts)
	} else {
		err = demoTUI(cmd.Context(), demo, tuiOpts)
	}
	if err != nil {
		return err
	}
	out := cmd.ErrOrStderr()
	if stopped {
		if _, err = fmt.Fprintln(out, "sc: stop confirmed (the demo has no orchestrator to stop; `sc run` wires the real one)"); err != nil {
			return err
		}
	}
	for _, c := range ctrl.Calls() {
		if _, err = fmt.Fprintln(out, "sc: run control: "+c); err != nil {
			return err
		}
	}
	return nil
}

// demoController is the stand-in orchestrator behind `sc tui-demo`'s run
// controls (P7-10). The demo has no run to pause, stop or re-fire, so every
// call is recorded and reported when the program exits: that is what makes
// the keys live in the demo instead of disabled, which is how a nil
// [tui.Controller] would render them.
type demoController struct {
	mu    sync.Mutex
	calls []string
}

var _ tui.Controller = (*demoController)(nil)

func (c *demoController) record(format string, args ...any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, fmt.Sprintf(format, args...))
	return nil
}

// Calls returns what the user asked for, in order.
func (c *demoController) Calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

func (c *demoController) Pause(_ context.Context, reason string) error {
	return c.record("pause (%s)", reason)
}

func (c *demoController) Resume(_ context.Context, reason string) error {
	return c.record("resume (%s)", reason)
}

func (c *demoController) Stop(_ context.Context, reason string) error {
	return c.record("stop (%s)", reason)
}

func (c *demoController) Refire(_ context.Context, bead domain.BeadID, reason string) error {
	return c.record("re-fire %s (%s)", bead, reason)
}

func (c *demoController) MarkDone(_ context.Context, bead domain.BeadID, reason string) error {
	return c.record("mark %s done (%s)", bead, reason)
}

func (c *demoController) SetSafety(_ context.Context, on bool) error {
	return c.record("safety %s", map[bool]string{true: "on", false: "off"}[on])
}

func (c *demoController) SetAmmo(_ context.Context, ammo int) error {
	return c.record("ammo %d", ammo)
}

// demoTUI is [tui.Demo], and isTerminal reports whether a file is a
// terminal. They are variables so a test can drive the command's TUI branch,
// which otherwise needs a real terminal to reach at all.
var (
	demoTUI      = tui.Demo
	replayTUI    = tui.Replay
	replayScript = tui.ReplayFile
	isTerminal   = func(f *os.File) bool { return term.IsTerminal(f.Fd()) }
)

// requireTerminal refuses to start a Bubble Tea program when stdin or stdout
// is not a terminal. Without this the program fails deep inside its input
// reader ("error creating cancelreader"), which says nothing about what to
// do instead; D-15's headless path and `--events` are what a pipe, a CI job
// or a test wants.
func requireTerminal() error {
	if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		return nil
	}
	return fmt.Errorf("tui-demo needs a terminal: stdin and stdout are not a TTY (use --%s to print the scripted stream as JSONL, or `sc run --%s` for a run without a TUI)",
		FlagDemoEvents, FlagRunHeadless)
}

// demoOptionsFrom reads the script flags.
func demoOptionsFrom(cmd *cobra.Command) (tui.DemoOptions, error) {
	f := cmd.Flags()
	var demo tui.DemoOptions
	var err error
	if demo.Brief, err = f.GetString(FlagDemoBrief); err != nil {
		return demo, err
	}
	if demo.Safety, err = f.GetBool(FlagDemoSafety); err != nil {
		return demo, err
	}
	if demo.Ammo, err = f.GetInt(FlagDemoAmmo); err != nil {
		return demo, err
	}
	delay, err := f.GetDuration(FlagDemoDelay)
	if err != nil {
		return demo, err
	}
	if delay < 0 {
		return demo, fmt.Errorf("--%s must not be negative", FlagDemoDelay)
	}
	// DemoOptions reads a zero delay as "use the default" and a negative one
	// as "no pause at all", so translate an explicit 0 into that.
	demo.Delay = delay
	if delay == 0 {
		demo.Delay = -time.Nanosecond
	}
	return demo, nil
}

// writeDemoEvents prints the scripted stream as JSONL, one event per line,
// the same shape the run log is written in.
func writeDemoEvents(cmd *cobra.Command, demo tui.DemoOptions) error {
	w := events.NewJSONLWriter(cmd.OutOrStdout())
	for _, ev := range tui.DemoEvents(demo) {
		if err := w.Write(ev); err != nil {
			return err
		}
	}
	return w.Err()
}
