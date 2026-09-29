package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/orchestrator"
	"slop-cannon/internal/tui"
)

// `sc watch`: the run TUI, attached to a run another process is firing.
//
// A run started with --headless, from a script, or in a terminal that has
// since gone had no way to be seen except `sc status` and `sc logs`. The
// run already writes every event it publishes to <run-dir>/events.jsonl
// (P6-08), and the TUI already renders a stream of events that did not come
// from its own bus (P7-12's replay), so watching is those two joined:
// [orchestrator.FollowEventLog] tails the log into [tui.Options.Events].
//
// A watcher only reads. It holds no lock, writes nothing and attaches no
// [tui.Controller], so it can never pause, stop or re-fire the run, and
// leaving it (`q`) leaves the run firing. `sc stop` is how to stop it.

// Flag names of `sc watch`.
const (
	FlagWatchRun       = "run"
	FlagWatchNoAnim    = "no-anim"
	FlagWatchAltScreen = "alt-screen"
)

func init() { register(newWatchCommand) }

const watchLong = `Open the run TUI on a run that is already firing, from any terminal.

PATH may be the repository root or any directory inside it; without it the
--repo directory (the current one by default) is used. The most recent run
is watched unless --run names another.

It is the same TUI sc run draws: the bead graph, what every agent is
working on, the review rounds and verdicts, the pull requests, and the log.
It starts with everything the run has done so far and then follows it live,
so it is how a run fired with --headless, from a script, or in a terminal
that has since closed is watched.

Watching only reads the run's event log. The run controls are not offered,
and q leaves the TUI while the run keeps firing; use sc stop to stop it. A
run that has already finished is shown as it ended.`

const watchExamples = `sc watch
sc watch ./repo-path
sc watch --run 20260924-214444-42c0
sc watch --no-anim`

// newWatchCommand builds `sc watch [PATH]`.
func newWatchCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "watch [PATH]",
		Short:   "Open the TUI on a run that is already firing",
		Long:    watchLong,
		Example: indent(watchExamples, "  "),
		Args:    cobra.MaximumNArgs(1),
		RunE:    runWatch,
	}
	f := cmd.Flags()
	f.String(FlagWatchRun, "", "watch run `ID` instead of the most recent one")
	f.Bool(FlagWatchNoAnim, false, "disable the cannon animations (also honours SC_NO_ANIM)")
	f.Bool(FlagWatchAltScreen, true, "draw on the alternate screen buffer")
	return cmd
}

// watchTUI renders the watcher. It is a variable so a test can see the
// options the command built without a terminal to draw on.
var watchTUI = tui.Run

// watchLiveness reports whether the run's process still holds its lock. It
// is a variable so a test can end a run without a real process.
var watchLiveness = func(runDir string, run domain.RunID) bool {
	return orchestrator.ProbeLiveness(runDir, run).Running()
}

// runWatch is the RunE of `sc watch`.
func runWatch(cmd *cobra.Command, args []string) error {
	opts, err := GlobalOpts(cmd)
	if err != nil {
		return err
	}
	path := opts.Repo
	if len(args) == 1 {
		path = args[0]
	}
	repo, err := runRepoRoot(path)
	if err != nil {
		return err
	}
	f := cmd.Flags()
	runID, err := f.GetString(FlagWatchRun)
	if err != nil {
		return err
	}
	noAnim, err := f.GetBool(FlagWatchNoAnim)
	if err != nil {
		return err
	}
	alt, err := f.GetBool(FlagWatchAltScreen)
	if err != nil {
		return err
	}
	id, dir, err := logsRunDir(repo, domain.RunID(runID))
	if err != nil {
		return err
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return fmt.Errorf("watch needs a terminal: stdin and stdout are not a TTY (sc status and sc logs --follow report the same run as text)")
	}

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	stream := orchestrator.FollowEventLog(ctx, dir, orchestrator.FollowOptions{
		Ended: func() bool { return !watchLiveness(dir, id) },
	})
	return watchTUI(ctx, tui.Options{
		Events:    stream,
		Run:       domain.Run{ID: id, RepoPath: repo},
		NoColor:   opts.NoColor,
		NoAnim:    noAnim,
		AltScreen: alt,
		Watch:     true,
	})
}
