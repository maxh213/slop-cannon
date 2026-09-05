package cli

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/orchestrator"
)

// `sc stop` (P9-24): stop a run from another terminal.
//
// D-18 names `sc stop` as one of the three ways an autopilot run ends (the
// others are the TUI's `s` key and a signal), but it did not exist, so the
// run-lock refusal and docs/troubleshooting.md had to tell the user "press
// Ctrl-C in the terminal it is running in, or kill the pid" — advice that
// is no use when that terminal is gone or was never theirs.
//
// The command is deliberately thin: the run lock (P6-01, P9-04) already
// records the run id, the pid of the `sc` holding it and the machine it
// runs on, and [orchestrator.Run] already turns SIGINT into a graceful stop
// (and a second one into a kill). So this reads the lock, decides whether
// the holder is something it may signal, sends the interrupt, and waits for
// the run to let the lock go. Nothing here writes the lock or touches bd,
// git or the run directory: the run itself owns its shutdown, exactly as it
// does for Ctrl-C.

// Flag names of `sc stop`.
const (
	FlagStopWait = "wait"
	FlagStopNow  = "now"
)

// DefaultStopWait is how long `sc stop` waits for the run to finish before
// it reports that it is still going. A graceful stop lets the agents in
// flight finish their session, so it is minutes rather than seconds; `--wait
// 0` returns as soon as the interrupt is delivered.
const DefaultStopWait = 2 * time.Minute

// stopPollInterval is how often the wait re-reads the run lock. It is a
// variable so a test need not wait a fifth of a second per poll.
var stopPollInterval = 200 * time.Millisecond

// stopSignal delivers sig to pid. It is a variable so a test can watch what
// was sent without a real process to send it to.
var stopSignal = func(pid int, sig os.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(sig)
}

func init() { register(newStopCommand) }

const stopLong = `Stop the run going in a repository, from anywhere.

PATH may be the repository root or any directory inside it; without it the
--repo directory (the current one by default) is used.

It reads the repository's run lock, interrupts the sc process holding it —
the same graceful stop Ctrl-C asks for, so the agents in flight finish,
their leases are released and the worktrees are tidied — and waits for the
run to end. --now interrupts twice, which kills the agents where they
stand; --wait 0 returns as soon as the interrupt is delivered.

A run on another machine (a state directory on a shared file system) cannot
be signalled from here, and neither can a run that has already gone. Both
are reported rather than guessed at. Use sc status to see the run first.`

const stopExamples = `sc stop
sc stop ./repo-path
sc stop --now
sc stop --wait 0`

// newStopCommand builds `sc stop [PATH]`.
func newStopCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "stop [PATH]",
		Short:   "Stop the run going in a repository",
		Long:    stopLong,
		Example: indent(stopExamples, "  "),
		Args:    cobra.MaximumNArgs(1),
		RunE:    runStop,
	}
	f := cmd.Flags()
	f.Duration(FlagStopWait, DefaultStopWait, "wait up to `DURATION` for the run to finish (0 returns as soon as it is asked to stop)")
	f.Bool(FlagStopNow, false, "stop immediately: interrupt twice, so the agents are killed rather than finished")
	return cmd
}

// StopReport is what `sc stop` did, for --json.
type StopReport struct {
	// Repo is the repository whose run was stopped.
	Repo string `json:"repo"`
	// Run is the run the lock named.
	Run domain.RunID `json:"run"`
	// PID is the process that was interrupted.
	PID int `json:"pid"`
	// Interrupts is how many interrupts were sent (2 with --now).
	Interrupts int `json:"interrupts"`
	// Stopped is true when the run had ended by the time the wait was over.
	Stopped bool `json:"stopped"`
}

// runStop is the RunE of `sc stop`.
func runStop(cmd *cobra.Command, args []string) error {
	opts, err := GlobalOpts(cmd)
	if err != nil {
		return err
	}
	path := opts.Repo
	if len(args) > 0 {
		path = args[0]
	}
	repo, err := runRepoRoot(path)
	if err != nil {
		return err
	}
	wait, err := cmd.Flags().GetDuration(FlagStopWait)
	if err != nil {
		return err
	}
	now, err := cmd.Flags().GetBool(FlagStopNow)
	if err != nil {
		return err
	}

	state := config.StateDir(repo)
	probe := orchestrator.InspectLock(state, orchestrator.LockOptions{})
	if err := stopHolder(probe, repo); err != nil {
		return err
	}

	rec := probe.Record
	report := StopReport{Repo: repo, Run: rec.Run, PID: rec.PID, Interrupts: 1}
	out := cmd.OutOrStdout()
	if !opts.JSON {
		if _, err := fmt.Fprintf(out, "stopping run %s (pid %d)\n", rec.Run, rec.PID); err != nil {
			return err
		}
	}
	if err := stopSignal(rec.PID, os.Interrupt); err != nil {
		return fmt.Errorf("could not interrupt run %s (pid %d): %w", rec.Run, rec.PID, err)
	}
	if now {
		// The run escalates on the second interrupt: agents are killed
		// rather than given their grace period (D-18, Run.Interrupt).
		report.Interrupts = 2
		if err := stopSignal(rec.PID, os.Interrupt); err != nil {
			return fmt.Errorf("could not interrupt run %s (pid %d): %w", rec.Run, rec.PID, err)
		}
	}

	report.Stopped = stopWait(state, rec, wait)
	if opts.JSON {
		if err := writeJSON(out, report); err != nil {
			return err
		}
	}
	switch {
	case report.Stopped:
		return stopSay(opts, out, fmt.Sprintf("run %s stopped\n", rec.Run))
	case wait <= 0:
		return stopSay(opts, out, fmt.Sprintf("run %s was asked to stop\n", rec.Run))
	default:
		return fmt.Errorf("run %s did not stop within %s; it is still going (pid %d)", rec.Run, wait, rec.PID)
	}
}

// stopSay prints line unless --json already reported the same thing.
func stopSay(opts GlobalOptions, w io.Writer, line string) error {
	if opts.JSON {
		return nil
	}
	_, err := fmt.Fprint(w, line)
	return err
}

// stopHolder reports why the lock's holder cannot be interrupted, or nil
// when it can.
func stopHolder(p orchestrator.LockProbe, repo string) error {
	switch p.State {
	case orchestrator.LockFree:
		return fmt.Errorf("no run is going in %s: nothing holds %s", repo, p.Path)
	case orchestrator.LockStale:
		return fmt.Errorf("run %s is not going any more (pid %d is gone), so there is nothing to stop; "+
			"the next run in this repository removes its lock", p.Record.Run, p.Record.PID)
	case orchestrator.LockElsewhere:
		return fmt.Errorf("run %s is going on %s (pid %d), which this machine cannot signal; "+
			"stop it there, or on this machine start again with --force once you are sure it stopped",
			p.Record.Run, p.Record.Host, p.Record.PID)
	case orchestrator.LockCorrupt:
		return fmt.Errorf("the run lock %s cannot be read (%v), so the run holding it cannot be named; "+
			"if no run is going, delete it and try again", p.Path, p.Err)
	case orchestrator.LockMine:
		// The lock names this very process, so the run is inside this
		// command: signalling it would take the terminal down with it.
		return fmt.Errorf("run %s is this very process (pid %d), so it cannot stop itself; press Ctrl-C instead",
			p.Record.Run, p.Record.PID)
	case orchestrator.LockLive:
		return nil
	default:
		return nil
	}
}

// stopWait waits up to d for the run the lock names to end, and reports
// whether it did. Zero or less returns at once.
//
// It watches both halves of the interlock: the lock file the run releases
// when it closes, and the process itself, because a run killed while it was
// stopping leaves the file behind (the next run's stale-lock notice removes
// it) and that is still a stopped run.
func stopWait(stateDir string, rec orchestrator.LockRecord, d time.Duration) bool {
	if d <= 0 {
		return false
	}
	deadline := time.Now().Add(d)
	for {
		if stopEnded(stateDir, rec) {
			return true
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		time.Sleep(min(left, stopPollInterval))
	}
}

// stopEnded reports whether the run the lock named is over: the lock is
// gone, some other run holds it now, or the holder's process has died.
func stopEnded(stateDir string, rec orchestrator.LockRecord) bool {
	p := orchestrator.InspectLock(stateDir, orchestrator.LockOptions{})
	switch p.State {
	case orchestrator.LockFree, orchestrator.LockStale:
		return true
	case orchestrator.LockCorrupt:
		// Something is rewriting the file; the process is the better
		// witness.
		return !orchestrator.ProcessAlive(rec.PID)
	case orchestrator.LockLive, orchestrator.LockMine, orchestrator.LockElsewhere:
		return p.Record.Run != rec.Run || p.Record.PID != rec.PID
	default:
		return false
	}
}
