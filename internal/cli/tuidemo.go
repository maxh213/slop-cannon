package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

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
)

const tuiDemoLong = `Render the TUI against a scripted event stream instead of a real run.

The script covers every event kind: planning, a bead through a full feedback
loop (change request, pushback, arbitration, approval, delivery), a failing
agent, an escalation, and a pause/resume/stop. It touches no repository, no
configuration and no network, so TUI work never needs a live cannon.

The command is hidden: it is a development tool, not part of the interface.`

const tuiDemoExamples = `sc tui-demo
sc tui-demo --delay 0 --safety
sc tui-demo --events`

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
	listOnly, err := f.GetBool(FlagDemoEvents)
	if err != nil {
		return err
	}
	if listOnly {
		return writeDemoEvents(cmd, demo)
	}
	alt, err := f.GetBool(FlagDemoAltScreen)
	if err != nil {
		return err
	}

	stopped := false
	err = tui.Demo(cmd.Context(), demo, tui.Options{
		NoColor:   opts.NoColor,
		AltScreen: alt,
		OnStop:    func() { stopped = true },
	})
	if err != nil {
		return err
	}
	if stopped {
		_, err = fmt.Fprintln(cmd.ErrOrStderr(), "sc: stop confirmed (the demo has no orchestrator to stop; P7-10 wires the real one)")
	}
	return err
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
