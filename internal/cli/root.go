package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// commandFactory builds a fresh subcommand. Factories, not commands, are
// registered so that every call to NewRootCommand gets its own flag state
// (tests build many roots in one process).
type commandFactory func() *cobra.Command

// registry holds the factories of every subcommand. Each subcommand lives in
// its own file and appends itself from an init function via register, so
// files never have to edit each other:
//
//	func init() { register(newAmmoCommand) }
var registry []commandFactory

// register adds a subcommand factory to the root. Call it from init().
func register(f commandFactory) { registry = append(registry, f) }

// rootExamples mirrors the command block of README.md verbatim; a test checks
// the two never drift apart.
const rootExamples = `sc ./repo-path

sc ./repo-path -m "here's what I want done"

sc cartridge --add claude --model fable
sc cartridge --remove claude
sc cartridge --add animus
sc cartridge --add kimi --model 3
sc ammo 12
sc vibes --add vibes.md
sc rules --add rules.md
sc safety off`

const rootLong = `Slop Cannon (sc) fires a crew of LLM agents at a git repository: a product
owner plans the work as beads (bd issues), engineers implement each bead in
its own worktree, and UX/QA/test reviewers loop feedback until every tester
is satisfied. Run it with a repo path and watch the TUI, or add -m to start
from a written brief.

  safety     PRs (on, the default) or straight to the default branch (off)
  cartridge  the CLI LLM subscription that gets used (blank until you add one)
  ammo       number of sub agents (minimum 3, default 10)
  vibes      stylistic choices communicated to all agents
  rules      rules communicated to all agents
  -m         start work with this brief; a bare repo path means full autopilot`

// ErrNotImplemented marks behaviour that a later task fills in.
var ErrNotImplemented = errors.New("not implemented")

// ExitError lets a command choose the process exit code. Execute prints Err
// (when non-nil) and exits with Code.
type ExitError struct {
	Code int
	Err  error
}

// Error implements error.
func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return e.Err.Error()
}

// Unwrap exposes the wrapped error to errors.Is / errors.As.
func (e *ExitError) Unwrap() error { return e.Err }

// NewRootCommand builds the sc command tree with every registered subcommand
// attached. Output and errors are written to out so tests can capture them;
// Execute re-points errors at stderr.
func NewRootCommand(out io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "sc [flags] <repo-path>",
		Short:         "Fire LLM agents at a repository",
		Long:          rootLong,
		Example:       indent(rootExamples, "  "),
		Args:          rootArgs,
		RunE:          runRoot,
		SilenceUsage:  true,
		SilenceErrors: true,
		// SuggestionsFor only applies a threshold that is set explicitly;
		// 2 is cobra's own default for its unknown-command path.
		SuggestionsMinimumDistance: 2,
	}
	root.SetOut(out)
	root.SetErr(out)
	addGlobalFlags(root)
	addRunFlags(root)

	cmds := make([]*cobra.Command, 0, len(registry))
	seen := make(map[string]string, len(registry))
	for _, f := range registry {
		c := f()
		if prev, dup := seen[c.Name()]; dup {
			panic(fmt.Sprintf("cli: subcommand %q registered twice (%q and %q)", c.Name(), prev, c.Use))
		}
		seen[c.Name()] = c.Use
		cmds = append(cmds, c)
	}
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name() < cmds[j].Name() })
	root.AddCommand(cmds...)
	return root
}

// rootArgs validates positional arguments on the root. A path-like first
// argument (or an existing directory) is the run target handled by runRoot;
// anything else is treated as a mistyped subcommand and reported with the
// nearest suggestions, the way cobra does for roots without a Run.
func rootArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if looksLikePath(args[0]) {
		return cobra.ExactArgs(1)(cmd, args)
	}
	msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
	if s := cmd.SuggestionsFor(args[0]); len(s) > 0 {
		msg += "\n\nDid you mean this?\n"
		for _, name := range s {
			msg += "\t" + name + "\n"
		}
	}
	msg += fmt.Sprintf("\nRun '%s --help' for usage.", cmd.CommandPath())
	return errors.New(msg)
}

// indent prefixes every non-empty line of s with prefix.
func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}

// looksLikePath reports whether arg names a directory or is spelled like a
// path (absolute, dot-relative, home-relative, or containing a separator).
func looksLikePath(arg string) bool {
	if arg == "" {
		return false
	}
	if strings.ContainsAny(arg, `/\`) || strings.HasPrefix(arg, ".") || strings.HasPrefix(arg, "~") {
		return true
	}
	info, err := os.Stat(arg)
	return err == nil && info.IsDir()
}

// runRoot handles `sc` with no subcommand: without arguments it prints help;
// with a repo path it fires the cannon (see run.go).
func runRoot(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}
	return runTarget(cmd, args)
}

// writeJSON prints v as indented JSON followed by a newline. Commands use it
// for their --json output so every command formats JSON the same way.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ExecuteContext runs the command tree with the given arguments and writers
// and returns the process exit code. Errors are printed to stderr prefixed
// with "sc:"; an *ExitError selects the exit code, everything else is 1.
func ExecuteContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	root := NewRootCommand(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	var exit *ExitError
	if errors.As(err, &exit) {
		if exit.Err != nil {
			fmt.Fprintln(stderr, "sc:", exit.Err)
		}
		return exit.Code
	}
	fmt.Fprintln(stderr, "sc:", err)
	return 1
}

// Execute runs the root command with os.Args, cancelling on SIGINT/SIGTERM,
// and returns the process exit code.
func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return ExecuteContext(ctx, os.Args[1:], os.Stdout, os.Stderr)
}
