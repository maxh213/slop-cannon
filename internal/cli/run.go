package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	// The Claude and Cursor adapters are the ones a user may fire (D-29);
	// importing them here is what puts them in cartridge.Default. The mock
	// adapter registers itself the same way, so a test binary that imports
	// internal/cartridge/mock can select it without loosening that for
	// users (P8-08).
	_ "slop-cannon/internal/cartridge/claude"
	_ "slop-cannon/internal/cartridge/cursor"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/logging"
	"slop-cannon/internal/orchestrator"
	"slop-cannon/internal/proc"
	"slop-cannon/internal/tui"
)

// Flag names of the run form (`sc <repo-path>` and `sc run <repo-path>`).
const (
	FlagRunMessage   = "message"
	FlagRunHeadless  = "headless"
	FlagRunResume    = "resume"
	FlagRunRun       = "run"
	FlagRunAmmo      = "ammo"
	FlagRunSafety    = "safety"
	FlagRunCartridge = "cartridge"
	FlagRunNoAnim    = "no-anim"
	FlagRunReviewPRs = "review-prs"
	// FlagRunForce takes the repository's run lock from a run that may
	// still be alive (P9-04). It is never sc's own decision.
	FlagRunForce = "force"
	// FlagRunDryRun resolves the run and prints it without firing
	// anything: what this repository, this configuration and these flags
	// add up to. It is how the resolution tests and `--json`'s RunConfig
	// output read a run without starting one, and it is the answer to
	// "show me what this run would do" — which is why P8-21 took it out of
	// hiding and into `sc run --help` and docs/cli.md.
	FlagRunDryRun = "dry-run"
)

// RunOptions are the parsed run flags plus the target path, the input of
// ResolveRun. Nil override pointers mean "not given on the command line".
type RunOptions struct {
	// Path is the repository (or any directory inside it) to fire at, as
	// given by the user; ResolveRun makes it absolute and finds the root.
	Path string
	// Brief is the -m text; empty means autopilot.
	Brief string
	// Headless renders events as log lines instead of the TUI (D-15).
	Headless bool
	// Resume continues the most recent run of the repository.
	Resume bool
	// RunID, with Resume, names the run to continue instead of the most
	// recent one.
	RunID domain.RunID
	// NoAnim disables the TUI cannon animations.
	NoAnim bool
	// ReviewPRs enables reviewing open pull requests sc did not create
	// (D-11); on by default.
	ReviewPRs bool
	// Overrides are the --ammo, --safety and --cartridge values that sit
	// above both config files (D-03).
	Overrides config.Overrides
}

// Hooks the tests replace to make run ids and start times deterministic.
var (
	runNow   = time.Now
	runNewID = config.NewRunID
	// runOnAssemble is a test seam: assembleRun hands it the cannon it has
	// just built and fails the assembly when it returns an error, so what a
	// failed assembly releases can be proven rather than reasoned about. It
	// is nil in production.
	runOnAssemble func(*cannon) error
	// runRenice lowers the run's priority before it fires; the tests
	// replace it so the test binary itself is never reniced.
	runRenice = proc.Renice
)

const runLong = `Fire the cannon at a repository. PATH may be the repository root or any
directory inside it. Without -m the product owner researches the repository
and invents the work itself (autopilot, which never stops on its own); with
-m it decomposes your brief into beads and finishes when they are all
settled.

The run needs an active cartridge (sc cartridge --add claude --model M) and
PATH must be inside a git repository. --ammo, --safety and --cartridge
override the config files for this run only; everything else comes from the
effective configuration (sc config show). --resume continues the most
recent run recorded for the repository instead of starting a new one.

The TUI holds the finished run on screen until you press q, so the last
graph is there to read; --headless ends when the run does, and a reader
that closes early (sc run --headless | head) stops the run rather than
killing it. sc stop ends a run from another terminal.

This is the same as the bare form "sc PATH [flags]".`

const runExamples = `sc ./repo-path
sc ./repo-path -m "here's what I want done"
sc run ./repo-path --headless --safety off --ammo 6
sc ./repo-path --resume`

func init() { register(newRunCommand) }

// newRunCommand builds `sc run [PATH]`, the spelled-out alias of the root
// form `sc PATH`.
func newRunCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "run [PATH]",
		Short:   "Fire the cannon at a repository (same as `sc PATH`)",
		Long:    runLong,
		Example: indent(runExamples, "  "),
		Args:    cobra.MaximumNArgs(1),
		RunE:    runTarget,
	}
	addRunFlags(cmd)
	return cmd
}

// addRunFlags defines the run flags on cmd. The root command and `sc run`
// share them so both forms parse identically.
func addRunFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringP(FlagRunMessage, "m", "", "start work with this `BRIEF`; without it the run is full autopilot")
	f.Bool(FlagRunHeadless, false, "no TUI: render events as log lines (for CI and scripts)")
	f.Bool(FlagRunResume, false, "continue the most recent run of this repository instead of starting a new one")
	f.String(FlagRunRun, "", "with --resume, continue run `ID` instead of the most recent one")
	f.Int(FlagRunAmmo, 0, "override ammo for this run (`N` agents at once, minimum 3)")
	f.String(FlagRunSafety, "", "override safety for this run (`on|off`): on opens PRs, off merges to the default branch")
	f.String(FlagRunCartridge, "", "fire cartridge `NAME` instead of the active one")
	f.Bool(FlagRunNoAnim, false, "disable the cannon animations in the TUI (also honours SC_NO_ANIM)")
	f.Bool(FlagRunReviewPRs, true, "also review open pull requests sc did not create (needs gh and a remote)")
	f.Bool(FlagRunForce, false, "start even though another run holds this repository's lock (only when you know it is gone)")
	f.Bool(FlagRunDryRun, false, "print the run this repository, config and flags add up to, without firing anything")
}

// runOptionsFrom reads the run flags of cmd. path is the positional target;
// empty means --repo (the current directory by default).
func runOptionsFrom(cmd *cobra.Command, path string, opts GlobalOptions) (RunOptions, error) {
	f := cmd.Flags()
	var ro RunOptions
	var err error
	if ro.Path = path; ro.Path == "" {
		ro.Path = opts.Repo
	}
	if ro.Brief, err = f.GetString(FlagRunMessage); err != nil {
		return ro, err
	}
	if ro.Headless, err = f.GetBool(FlagRunHeadless); err != nil {
		return ro, err
	}
	if ro.Resume, err = f.GetBool(FlagRunResume); err != nil {
		return ro, err
	}
	if f.Changed(FlagRunRun) {
		id, err := f.GetString(FlagRunRun)
		if err != nil {
			return ro, err
		}
		if ro.RunID = domain.RunID(strings.TrimSpace(id)); ro.RunID == "" {
			return ro, fmt.Errorf("--%s must not be empty", FlagRunRun)
		}
		if !ro.Resume {
			return ro, fmt.Errorf("--%s names a run to continue, so it needs --%s (or sc resume)", FlagRunRun, FlagRunResume)
		}
	}
	if ro.NoAnim, err = f.GetBool(FlagRunNoAnim); err != nil {
		return ro, err
	}
	if ro.ReviewPRs, err = f.GetBool(FlagRunReviewPRs); err != nil {
		return ro, err
	}
	if f.Changed(FlagRunMessage) {
		ro.Brief = strings.TrimSpace(ro.Brief)
		if ro.Brief == "" {
			return ro, fmt.Errorf("--%s must not be empty", FlagRunMessage)
		}
		if ro.Resume {
			return ro, fmt.Errorf("--%s cannot be combined with --%s: a resumed run keeps its original brief", FlagRunMessage, FlagRunResume)
		}
	}
	if f.Changed(FlagRunAmmo) {
		n, err := f.GetInt(FlagRunAmmo)
		if err != nil {
			return ro, err
		}
		ro.Overrides.Ammo = &n
	}
	if f.Changed(FlagRunSafety) {
		s, err := f.GetString(FlagRunSafety)
		if err != nil {
			return ro, err
		}
		on, err := config.ParseSafety(s)
		if err != nil {
			return ro, fmt.Errorf("--%s: %w", FlagRunSafety, err)
		}
		ro.Overrides.Safety = &on
	}
	if f.Changed(FlagRunCartridge) {
		name, err := f.GetString(FlagRunCartridge)
		if err != nil {
			return ro, err
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return ro, fmt.Errorf("--%s must not be empty", FlagRunCartridge)
		}
		ro.Overrides.Cartridge = &name
	}
	return ro, nil
}

// ResolveRun turns the run options into a RunConfig without starting
// anything: it finds the git repository containing opts.Path (refusing when
// there is none), loads the effective configuration, applies the flag
// overrides, and refuses when no cartridge is active. P8-01 hands the
// result to the orchestrator; P8-06 may call it directly.
//
// Use [ResolveRunWithNotes] when the notes a resumed run produces matter.
func ResolveRun(opts RunOptions) (*config.RunConfig, error) {
	rc, _, err := ResolveRunWithNotes(opts)
	return rc, err
}

// ResolveRunWithNotes is [ResolveRun] plus whatever the caller should tell
// the user about the run it resolved.
//
// On `--resume` it hands the rebuilt config to
// [orchestrator.RestoreRunConfig], which puts back the frozen
// [domain.ConfigSnapshot] the earlier invocation recorded (P6-11): a config
// file edited between the two invocations does not change the run, while an
// explicit --ammo/--safety/--cartridge does and says so in the notes.
func ResolveRunWithNotes(opts RunOptions) (*config.RunConfig, []string, error) {
	root, err := runRepoRoot(opts.Path)
	if err != nil {
		return nil, nil, err
	}
	eff, err := config.Load(root)
	if err != nil {
		return nil, nil, err
	}
	if err := eff.Apply(opts.Overrides); err != nil {
		return nil, nil, err
	}
	now := runNow()
	settings := config.RunSettings{
		Brief:     opts.Brief,
		Headless:  opts.Headless,
		Resume:    opts.Resume,
		NoAnim:    opts.NoAnim,
		ReviewPRs: opts.ReviewPRs,
		StartedAt: now,
	}
	var notes []string
	switch {
	case !opts.Resume:
		settings.ID = runNewID(now)
	case opts.RunID != "":
		if _, err := os.Stat(filepath.Join(config.RunDir(root, string(opts.RunID)), orchestrator.StateFileName)); err != nil {
			return nil, nil, fmt.Errorf("nothing to resume: run %s of %s has no %s (sc status lists the runs)",
				opts.RunID, root, orchestrator.StateFileName)
		}
		settings.ID = opts.RunID
	default:
		// P8-31: `--resume` continues the newest run that actually recorded
		// something, not simply the greatest directory name on disk.
		target, note, err := runResumeTarget(root)
		if err != nil {
			return nil, nil, err
		}
		if note != "" {
			notes = append(notes, note)
		}
		switch {
		case target != "":
			settings.ID = target
		case note != "":
			// Nothing on disk is resumable, so this is a new run under a
			// new id; the note above says so.
			settings.Resume = false
			settings.ID = runNewID(now)
		}
	}
	rc, err := config.NewRunConfig(root, eff, settings)
	if errors.Is(err, config.ErrNoCartridge) {
		return nil, nil, fmt.Errorf("%w — add one with: %s", config.ErrNoCartridge, CartridgeAddHint)
	}
	if err != nil {
		return nil, nil, err
	}
	if !settings.Resume {
		return rc, notes, nil
	}
	res, err := orchestrator.RestoreRunConfig(rc)
	if err != nil {
		return nil, nil, err
	}
	return rc, append(notes, res.Notes...), nil
}

// runResumeTarget picks the run `--resume` continues: the newest run
// directory of the repository that actually holds a state.json.
//
// P8-31: [config.LatestRunID] takes the greatest directory name whatever is
// in it, so a directory left behind by a kill between [logging.OpenRunLog]
// and the first state write — or by a half-pruned state directory — was
// adopted, and `--resume` fired a brand-new autopilot run under a dead
// run's id. A run that never wrote a state file has nothing to resume, so
// it is passed over.
//
// It returns the run to continue and, when the choice is not simply the
// newest directory, the note explaining it. An empty id with a note means
// nothing on disk is resumable and the caller should start a new run; an
// empty id with no note means the repository has no runs at all, which is
// [config.NewRunConfig]'s own refusal to phrase.
func runResumeTarget(root string) (id domain.RunID, note string, err error) {
	dir := filepath.Join(config.StateDir(root), config.RunsDirName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("list runs: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", "", nil
	}
	// Run ids sort chronologically as text (config.NewRunID), so the last
	// name is the newest run.
	slices.Sort(names)
	slices.Reverse(names)
	for i, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name, orchestrator.StateFileName)); err != nil {
			continue
		}
		if i == 0 {
			return domain.RunID(name), "", nil
		}
		return domain.RunID(name), fmt.Sprintf(
			"run %s recorded no %s, so there is nothing of it to resume; continuing run %s, the newest one that did",
			names[0], orchestrator.StateFileName, name), nil
	}
	return "", fmt.Sprintf("run %s recorded no %s, and neither did any older run; starting a new run instead",
		names[0], orchestrator.StateFileName), nil
}

// runRepoRoot makes path absolute, checks it is a directory, and returns
// the root of the git repository containing it.
func runRepoRoot(path string) (string, error) {
	if path == "" {
		path = "."
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	info, err := os.Stat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%s: no such directory; sc fires at a git repository that already exists, so check the path (a relative one is resolved against the current directory) or name another: sc /path/to/repo", path)
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s: not a directory; sc fires at a repository, not at one of its files, so name the directory: sc /path/to/repo", path)
	}
	root, ok := config.FindRepoRoot(abs)
	if !ok {
		return "", fmt.Errorf("%s is not a git repository (no .git in it or any parent directory)", abs)
	}
	return root, nil
}

// runTarget is the RunE of both `sc PATH` and `sc run [PATH]`: it resolves
// the run and then fires it (P8-01). With --dry-run it stops at the
// resolved configuration and prints it, which is what a user asking what a
// run would do, the resolution tests and `sc --json <repo> --dry-run` read.
func runTarget(cmd *cobra.Command, args []string) error {
	opts, err := GlobalOpts(cmd)
	if err != nil {
		return err
	}
	path := ""
	if len(args) > 0 {
		path = args[0]
	}
	ro, err := runOptionsFrom(cmd, path, opts)
	if err != nil {
		return err
	}
	dry, err := cmd.Flags().GetBool(FlagRunDryRun)
	if err != nil {
		return err
	}
	rc, notes, err := ResolveRunWithNotes(ro)
	if err != nil {
		if dry {
			// --dry-run resolves and prints; a caller asking what the run
			// would be wants the error itself, not a fix list.
			return err
		}
		// P8-06: the two commonest startup failures (no cartridge, not a
		// git repository) surface here, and a numbered fix list is far
		// more use than the bare error.
		return StartupFailure(cmd.Context(), cmd.ErrOrStderr(), ro.Path, err)
	}
	for _, n := range notes {
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "note: "+n); err != nil {
			return err
		}
	}
	if !dry {
		// Every agent, check command and test browser the run starts
		// inherits this niceness, so a busy run does not starve the machine.
		if err := runRenice(rc.Effective.Nice); err != nil {
			if _, werr := fmt.Fprintf(cmd.ErrOrStderr(), "note: could not lower the run's priority to nice %d: %v\n", rc.Effective.Nice, err); werr != nil {
				return werr
			}
		}
		return fireCannon(cmd, opts, rc)
	}
	out := cmd.OutOrStdout()
	if opts.JSON {
		return writeJSON(out, rc)
	}
	return runWriteText(out, rc)
}

// runWriteText prints the resolved run as an aligned label/value list with
// the source of every configurable value.
func runWriteText(w io.Writer, rc *config.RunConfig) error {
	eff := rc.Effective
	src := func(key string) string { return fmt.Sprintf(" (from %s)", runSourceOf(eff, key)) }
	kind := "new"
	if rc.Resume {
		kind = "resumed"
	}
	anim := "on"
	if rc.NoAnim {
		anim = "off"
	}
	rows := [][2]string{
		{"run", fmt.Sprintf("%s (%s)", rc.Run.ID, kind)},
		{"mode", rc.Run.Mode.String()},
	}
	if rc.Run.Mode == domain.RunModeBrief {
		rows = append(rows, [2]string{"brief", rc.Run.Brief})
	}
	rows = append(rows,
		[2]string{"repo", rc.Run.RepoPath},
		[2]string{"state", rc.Run.StateDir},
		[2]string{"started", rc.Run.StartedAt.UTC().Format(time.RFC3339)},
		[2]string{"cartridge", fmt.Sprintf("%s (%s, command %s)%s", rc.CartridgeName,
			cartridgeModelText(rc.Cartridge.Model), rc.Cartridge.Binary(rc.CartridgeName), src(config.KeyActiveCartridge))},
		[2]string{"ammo", strings.TrimPrefix(rc.Allocation.String(), "ammo ") + src(config.KeyAmmo)},
		[2]string{"safety", fmt.Sprintf("%s%s: %s", safetyWord(eff.Safety), src(config.KeySafety), SafetyMeaning(eff.Safety))},
		[2]string{"vibes", runList(eff.Vibes) + src(config.KeyVibes)},
		[2]string{"rules", runList(eff.Rules) + src(config.KeyRules)},
		[2]string{"nice", niceText(eff.Nice) + src(config.KeyNice)},
		[2]string{"review-prs", fmt.Sprintf("%t", rc.Run.Config.ReviewPRs)},
		[2]string{"headless", fmt.Sprintf("%t", rc.Run.Config.Headless)},
		[2]string{"animation", anim},
	)
	for _, r := range rows {
		if _, err := fmt.Fprintf(w, "%-11s%s\n", r[0], r[1]); err != nil {
			return err
		}
	}
	return nil
}

// niceText describes the niceness the run will give itself.
func niceText(n int) string {
	if n <= 0 {
		return "0: the run's priority is left alone"
	}
	return fmt.Sprintf("%d: the run and every process it starts run at a lower priority", n)
}

// runSourceOf returns the layer that supplied key, "default" when untracked.
func runSourceOf(eff *config.Effective, key string) config.Source {
	if s := eff.Sources[key]; s != "" {
		return s
	}
	return config.SourceDefault
}

// runList renders a path list as "[]" or "[a, b]".
func runList(list []string) string {
	if len(list) == 0 {
		return "[]"
	}
	return "[" + strings.Join(list, ", ") + "]"
}

// ---------------------------------------------------------------------------
// Firing the cannon (P8-01)
// ---------------------------------------------------------------------------

// The renderers, as variables so a test can drive `sc run` without a real
// terminal and without a real event stream.
var (
	runTUI      = tui.Run
	runHeadless = tui.RunHeadless
)

// The two intervals the assembly chooses that have no configuration key.
// They are variables so a test can drive a whole run in a fraction of a
// second; nothing but a test ever changes them.
//
// runConductorInterval is how often the run-level glue syncs bd with the
// feedback loop and the delivery handoff. It is deliberately faster than
// gate_poll_interval: the glue is the run's inner bookkeeping loop, while a
// gate check shells out to `gh`. runTickInterval is the scheduler's
// backstop dispatch interval and runSettleInterval bounds how often brief
// mode asks bd whether its beads have settled; zero means each component's
// own default.
var (
	runConductorInterval = orchestrator.DefaultConductorInterval
	runTickInterval      time.Duration
	runSettleInterval    time.Duration
	// runHelperGrace bounds how long the run waits for the conductor, the
	// gate poller and the external reviewer to stop before it closes the
	// event stream the renderers are reading. A helper that will not stop
	// must not be able to hold the terminal.
	runHelperGrace = 5 * time.Second
)

// runShimOptions configures the read-only bd shim every agent's worktree
// and the planning product owner get (D-21, P6-17). It is a variable for
// the same reason as the intervals above: a test whose `bd` is a stand-in
// cannot pass the shim's self-check, which probes the real binary's own
// --readonly enforcement. Production always verifies.
var runShimOptions beads.ShimOptions

// fireCannon is P8-01: it assembles the orchestrator from the resolved
// configuration, starts it, renders it (TUI or headless), prints the final
// summary and chooses the process exit code.
//
// The order is the one internal/orchestrator's package documentation
// prescribes and internal/orchestrator/e2e_test.go proves: the run log goes
// in front of the bus (P8-20), the conductor gives the run its bead source
// (P8-17), and the parts that cannot exist before the run is built are
// attached afterwards.
func fireCannon(cmd *cobra.Command, gopts GlobalOptions, rc *config.RunConfig) error {
	// P8-01: stderr has more than one producer the moment the run starts —
	// the headless terminal log handler (every line the conductor, the gate
	// poller and the external reviewer write), the run's own P6-18 worktree
	// cleanup report, which gitx prints from the finish path while those
	// helpers are still going, and the P9-04 lock notice. io.Writer
	// promises nothing about concurrent use, so it is wrapped once here and
	// every one of those producers is handed the wrapped writer.
	stderr := newSyncWriter(cmd.ErrOrStderr())
	// --json is only meaningful as a stream of event envelopes, and a TUI
	// cannot produce one: it implies --headless (D-15, P8-12).
	headless := rc.Run.Config.Headless || gopts.JSON
	if headless {
		// P9-20: a closed stdout must reach the run as an error it can stop
		// on, not as the signal that kills the process before it can
		// release its lock or clean its worktrees.
		defer runIgnoreBrokenPipe()()
	}
	if !headless {
		if err := runRequireTerminal(); err != nil {
			// P8-06: the refusal goes through the same numbered fix list
			// as every other startup failure, so "run it headless" is a
			// step rather than a parenthesis.
			return StartupFailure(cmd.Context(), stderr, rc.Run.RepoPath, err)
		}
	}

	// P9-04: refuse a repository another sc is firing at before anything is
	// built, and hand the same policy to the run so Start re-applies it.
	// --force is the only way past a live lock, or past one written on
	// another machine, whose process this one cannot probe.
	force, err := cmd.Flags().GetBool(FlagRunForce)
	if err != nil {
		return err
	}
	policy := orchestrator.NewLockPolicy(orchestrator.LockOptions{
		Force:  force,
		Notice: func(s string) { fmt.Fprintln(stderr, "sc: "+s) },
	})
	if _, err := policy.Check(config.StateDir(rc.Run.RepoPath)); err != nil {
		// P8-06: the refusal is printed with what to do about it.
		return StartupFailure(cmd.Context(), stderr, rc.Run.RepoPath, err)
	}

	verbose, err := cmd.Root().PersistentFlags().GetBool(FlagVerbose)
	if err != nil {
		return err
	}
	// P9-05: one filter hides the configured `exclude` paths and the
	// built-in secret patterns from the prompts, and redacts the logs. A
	// malformed pattern refuses the run rather than leaving the user
	// believing a file is hidden when it is not, and the refusal is printed
	// with what to do about it, like every other startup failure (P8-06).
	filter, err := agents.NewSecretFilter(rc.Effective.Exclude...)
	if err != nil {
		return StartupFailure(cmd.Context(), stderr, rc.Run.RepoPath, err)
	}

	// P8-10: the root installed a terminal-only default logger; the run
	// swaps in the per-run file under the D-23 run directory, teed with the
	// terminal in headless mode (D-15). The TUI owns the terminal, so there
	// the file is the only handler.
	logOpts := LogOptions(verbose)
	logOpts.Redactor = filter.Redactor()
	runLog, err := logging.OpenRunLog(rc.Run.StateDir, rc.Run.ID, logOpts)
	if err != nil {
		return err
	}
	defer func() { _ = runLog.Close() }()
	handler := runLog.Logger().Handler()
	if headless {
		// Only the terminal half needs the run id stamped on it: the run
		// log's handler already carries one.
		handler = logging.NewMultiHandler(
			logging.NewTerminalHandler(stderr, logOpts).
				WithAttrs([]slog.Attr{slog.String("run", rc.Run.ID.String())}),
			handler,
		)
	}
	// P8-05: three layers stamp the run id — this one, logging.OpenRunLog
	// and orchestrator.New — so every line used to carry "run" two or
	// three times, and a repeated JSON key is ambiguous to whoever reads
	// what `sc logs` prints. The first value stands; later copies are
	// dropped (logattr.go).
	logger := slog.New(logAttrOnce(handler, "run"))
	prev := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(prev)

	c, err := assembleRun(cmd.Context(), rc, runSetup{
		Logger:   logger,
		Out:      stderr,
		Screen:   runScreen(cmd, stderr, headless),
		Lock:     policy,
		Filter:   filter,
		RunLog:   runLog,
		Headless: headless,
	})
	if err != nil {
		// P8-06: "preparing the bead tracker", "resolving the default
		// branch" and the rest become a numbered fix list.
		return StartupFailure(cmd.Context(), stderr, rc.Run.RepoPath, err)
	}
	defer c.close()

	code, err := c.fire(cmd, gopts, headless)
	switch {
	case err != nil && code != 0:
		return &ExitError{Code: code, Err: err}
	case err != nil:
		return err
	case code != 0:
		return &ExitError{Code: code}
	}
	return nil
}

// runRequireTerminal is P8-13: the TUI is a Bubble Tea program, so it needs
// a real terminal on both stdin and stdout. Without this the program fails
// deep inside its input reader ("error creating cancelreader"), which says
// nothing about what to do instead. It reuses [requireTerminal]'s predicate
// and replaces its `sc tui-demo` wording.
//
// What to do instead is not in the message: fireCannon hands this to
// [StartupFailure], whose "terminal" class prints `--headless` as a
// numbered step (P8-06).
func runRequireTerminal() error {
	if err := requireTerminal(); err == nil {
		return nil
	}
	return errors.New("the TUI needs a terminal: stdin and stdout are not a TTY")
}

// runSetup is what fireCannon resolved before any orchestrator part exists.
type runSetup struct {
	// Logger is the run's logger: the run log, plus the terminal in
	// headless mode.
	Logger *slog.Logger
	// Out is where the run prints its own reports (the worktree cleanup,
	// P6-18) and its final summary. It is stderr, wrapped so the several
	// goroutines that write to it cannot interleave (P8-01), so `--json`'s
	// stdout stays parseable.
	Out io.Writer
	// Screen is where the renderer draws: stdout. It is Out itself when
	// the caller passed one writer for both and the run is headless, so
	// the rendered events and the logged lines share one lock.
	Screen io.Writer
	// Lock is the P9-04 policy the preflight already applied.
	Lock orchestrator.LockPolicy
	// Filter is the P9-05 secret filter.
	Filter *agents.SecretFilter
	// RunLog owns the run's log files. The run writes one log per agent
	// through it (P9-18), which is what `sc logs <agent-id>` reads back.
	RunLog *logging.RunLogger
	// Headless says whether the terminal is ours to write to.
	Headless bool
}

// reportWriter is where the run prints its own finish report, or nil when
// the TUI owns the terminal.
func (s runSetup) reportWriter() io.Writer {
	if s.Headless {
		return s.Out
	}
	return nil
}

// syncWriter serialises writes to one io.Writer so several goroutines may
// share it.
//
// P8-01: an assembled run has three producers on stderr — the headless
// terminal log handler, the P9-04 lock notice and the run's own P6-18
// worktree cleanup report, which gitx.Manager.Cleanup prints from the
// finish path before Run.Done closes, i.e. while the conductor and the gate
// poller are still logging. Neither *os.File (interleaved lines) nor the
// *bytes.Buffer this package's own tests pass (a genuine data race) is safe
// under that, so fireCannon wraps the writer once and hands the wrapper to
// all three.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// newSyncWriter guards w with a mutex. Wrapping a *syncWriter again is
// harmless but pointless, so it is unwrapped instead.
func newSyncWriter(w io.Writer) *syncWriter {
	if s, ok := w.(*syncWriter); ok {
		return s
	}
	return &syncWriter{w: w}
}

// Write implements [io.Writer]. One call to Write is one call on the
// wrapped writer, so a whole log line or report line lands undivided.
func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// unwrap returns the writer this one guards. The field is written once, at
// construction, so reading it needs no lock.
func (s *syncWriter) unwrap() io.Writer { return s.w }

// runIgnoreBrokenPipe makes a write to a closed stdout or stderr an error
// this process can act on rather than a signal that kills it, and returns
// the function that puts the default behaviour back.
//
// P9-20: `sc run --headless | head` closes the pipe as soon as head has its
// ten lines. The Go runtime turns EPIPE on file descriptor 1 or 2 into a
// SIGPIPE with the default disposition, so the process died where it stood
// — the run lock was never released and the worktrees were never cleaned,
// and the mess stood until the next run's stale-lock notice removed it.
// Asking for the signal on a channel (which is then simply dropped) makes
// the runtime return EPIPE from the write instead, which
// [closedOutputWriter] turns into an ordinary stop, so the shutdown is the
// same one SIGTERM gets.
func runIgnoreBrokenPipe() (restore func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGPIPE)
	return func() { signal.Stop(ch) }
}

// closedOutputWriter is the screen of a headless run: it writes through
// until the reader goes away, and then tells the run so and swallows
// everything after that (P9-20).
//
// Swallowing is deliberate. The renderer is mid-stream when the pipe
// breaks, and failing it there would end the run as an error, with no
// summary and a non-zero exit code, for something the user did on purpose
// (`| head`, `| grep -m1`). The run stops instead, exactly as it would on a
// signal, and its finish reason decides the exit code.
type closedOutputWriter struct {
	w io.Writer
	// onClosed is called once, the first time a write finds the reader
	// gone.
	onClosed func(error)

	mu     sync.Mutex
	closed bool
}

// Write implements [io.Writer].
func (c *closedOutputWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return len(p), nil
	}
	n, err := c.w.Write(p)
	if err != nil && isClosedPipe(err) {
		c.closed = true
		if c.onClosed != nil {
			c.onClosed(err)
		}
		return len(p), nil
	}
	return n, err
}

// isClosedPipe reports whether err says the other end of the output has
// gone away: EPIPE from the write, or a file this process has closed.
func isClosedPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrClosed)
}

// stopOnClosedOutput wraps the run's screen so that a reader going away
// stops the run gracefully (P9-20).
func (c *cannon) stopOnClosedOutput(w io.Writer) io.Writer {
	return &closedOutputWriter{w: w, onClosed: func(err error) {
		c.set.Logger.Info("the output was closed; stopping the run", slog.Any("err", err))
		_ = c.run.Stop("its output was closed")
	}}
}

// runScreen is where the renderer draws: stdout.
//
// It is the guarded stderr writer itself when the caller passed one writer
// for both — cli.NewRootCommand does, and so does anything that merges the
// two streams — because then the renderer's lines and the log's would race
// on the same writer as surely as two log lines would.
//
// The TUI always gets the raw stdout: Bubble Tea reads the terminal's size
// and colour profile off the writer it is given, and a wrapper is not an
// *os.File, so wrapping would cost it both.
func runScreen(cmd *cobra.Command, out *syncWriter, headless bool) io.Writer {
	if headless && sameWriter(cmd.OutOrStdout(), out.unwrap()) {
		return out
	}
	return cmd.OutOrStdout()
}

// sameWriter reports whether a and b are the same writer. It cannot use ==
// directly: comparing two interfaces whose dynamic type is identical and
// not comparable panics, and `sc run` writes wherever the caller of
// [ExecuteContext] pointed it.
func sameWriter(a, b io.Writer) bool {
	ra, rb := reflect.ValueOf(a), reflect.ValueOf(b)
	if !ra.IsValid() || !rb.IsValid() || ra.Type() != rb.Type() || !ra.Comparable() {
		return false
	}
	return ra.Equal(rb)
}

// cannon is one assembled run: every part, plus what has to be shut down
// when it ends.
type cannon struct {
	rc *config.RunConfig
	// header is the run as it was before it started, for the renderers'
	// headers. It is a copy on purpose: a renderer reads it on its own
	// goroutine for the whole life of the run, so it must not be a window
	// onto anything the scheduler writes. (The run itself stopped writing
	// back into rc.Run with P8-23; the copy is what keeps the header
	// stable, not what keeps it safe.)
	header domain.Run
	set    runSetup
	bus    *events.Bus
	log    *orchestrator.EventLog
	run    *orchestrator.Run
	cond   *orchestrator.Conductor
	gates  *orchestrator.GatePoller
	// prs is the external pull request reviewer (D-11, P8-04), or nil when
	// this run does not review them.
	prs *orchestrator.PRReview
	// pub is the one place every event the run publishes passes through:
	// the redaction, the seal at events.RunFinished, the agent transcripts
	// and the P6-41 settle hook all hang off it.
	pub *runPublisher
	// settleq tells the deliverer when a bead settles, so it forgets a
	// safety-on delivery the gate poller closed (P6-41).
	settleq *settleQueue
	spend   *runSpend
	// budget is the P9-01 run budget, kept so the TUI's `b` key can raise
	// it while the run is paused on a cap (P9-14).
	budget *orchestrator.RunBudget
	// vitals is the "why is nothing happening?" snapshot the run publishes
	// into state.json (P6-29), kept so the TUI header can show it too
	// (P6-51). It is pulled per frame, never cached.
	vitals func() orchestrator.RunVitals
	// gate and states are P9-06's two decorated parts, kept so the
	// assembly can be asked what it wrapped rather than read: the gate
	// guards `states`' store and the scheduler's firing planner, and the
	// conductor holds the same gate for the resume. Nothing else reads
	// them; they are here because deleting a wire point used to leave the
	// whole suite green (P9-06 review, round 4).
	gate   *orchestrator.RulesGate
	states *orchestrator.StateMachine
	stop   []func()
	// released records that close has already run, so the release a failed
	// assembly does and fireCannon's own defer cannot double up.
	released bool
}

// close shuts the assembled run's resources down, in reverse order. The
// event log is closed first, since a summary folded from it must see every
// line (P8-20). It is idempotent: a failed assembly releases what it had
// already built through this same path, and fireCannon's defer then finds
// nothing left to do.
func (c *cannon) close() {
	if c.released {
		return
	}
	c.released = true
	c.closeLog()
	for i := len(c.stop) - 1; i >= 0; i-- {
		c.stop[i]()
	}
	c.stop = nil
}

// assembleRun builds every part of the run over the resolved configuration.
// It touches bd and git (registering the D-07 statuses, opening the
// repository, creating the run directory) but starts nothing.
func assembleRun(ctx context.Context, rc *config.RunConfig, set runSetup) (_ *cannon, err error) {
	eff := rc.Effective
	cfg := eff.Config
	logger := set.Logger
	c := &cannon{rc: rc, header: rc.Run, set: set, spend: &runSpend{}}

	// Every failure below releases what the assembly had already built —
	// the event bus, the interrupt registration, the supervisor, the settle
	// queue and the event log — instead of leaving that to the process
	// exiting, and then discards the run directory when nothing was ever
	// recorded into it, so a later `--resume` cannot adopt the id of a run
	// that never started.
	defer func() {
		if err != nil {
			c.close()
			discardRunDir(rc.Run.StateDir, logger)
		}
	}()

	// bd: initialise the tracker if the repository has none (D-13), then
	// register the D-07 custom statuses and read back what bd holds, so a
	// status the cannon never registered is never transitioned (P6-12).
	client := beads.New(rc.Run.RepoPath, beads.Options{Logger: logger})
	if _, err := beads.EnsureInitialised(ctx, rc.Run.RepoPath, beads.InitOptions{Logger: logger}); err != nil {
		return nil, fmt.Errorf("preparing the bead tracker: %w", err)
	}
	statuses, err := client.EnsureStatuses(ctx)
	if err != nil {
		return nil, fmt.Errorf("registering the bead statuses: %w", err)
	}
	statusMap := beads.NewStatusMap(statuses.Statuses)

	// git: the run directory and the worktrees live outside the repository
	// (D-23).
	dirs, err := orchestrator.EnsureRunDir(rc.Run.StateDir)
	if err != nil {
		return nil, err
	}
	repo, err := gitx.Open(ctx, rc.Run.RepoPath)
	if err != nil {
		return nil, err
	}
	branch, err := repo.DefaultBranch(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving the default branch: %w", err)
	}
	manager, err := gitx.NewManager(repo, dirs.Dir)
	if err != nil {
		return nil, err
	}
	// P1-14: `max_diff_bytes` caps every diff a reviewer is shown.
	dgit := orchestrator.NewDeliveryGit(manager, gitx.WithMaxDiffBytes(cfg.MaxDiffBytes))

	// The prompt context every firing shares.
	scan, err := set.Filter.Scan(rc.Run.RepoPath, prompt.ScanOptions{
		DefaultBranch: branch,
		CheckCommand:  cfg.CheckCommand,
	})
	if err != nil {
		return nil, err
	}
	rules, err := prompt.LoadDocs(cfg.Rules)
	if err != nil {
		return nil, err
	}
	vibes, err := prompt.LoadDocs(cfg.Vibes)
	if err != nil {
		return nil, err
	}

	// The cartridges: only what the registry holds (D-19, P8-08).
	sel, err := cartridge.BuildSelector(cartridge.Default, cfg)
	if err != nil {
		return nil, err
	}

	limits := orchestrator.EffectiveLimits(rc)
	bus := events.NewBus()
	c.bus = bus
	c.stop = append(c.stop, bus.Close)

	// Everything publishes through the run once it exists, so the run log
	// and state.json's sequence number see every event (P6-01, P6-08); the
	// run is built further down, hence the indirection.
	sink := &lateRun{bus: bus}

	// P9-06: the rules gate guards both the state store and the firing
	// planner, so a bead with an open rules violation is never argued or
	// shipped past it.
	// The clock is the run's own (sink.Now), like every other stamp in the
	// run: the gate is built before the run exists, so it reads it through
	// the same late binding its bus does. Without that a harness with a
	// frozen or offset clock would get the gate's warnings on a different
	// time base from the rest of events.jsonl.
	gate := orchestrator.NewRulesGate(orchestrator.RulesGateOptions{
		Run: rc.Run.ID, Bus: sink, Logger: logger, Now: sink.Now,
	})
	states := orchestrator.NewStateMachine(gate.GuardStore(client),
		orchestrator.WithStatePublisher(sink), orchestrator.WithStateRun(rc.Run.ID))
	c.gate, c.states = gate, states

	resumer, err := runResumer(manager, client, statusMap, logger)
	if err != nil {
		return nil, err
	}

	// The run, the supervisor and the scheduler each need one of the
	// others before it exists, so the three cycles are closed with
	// late-bound indirections rather than by building anything twice: the
	// run's agent stopper and loop read the parts when the run is already
	// going, and the supervisor's firing planner is set before the first
	// firing. Every one of them is written before Start and read after it.
	var (
		sup      *orchestrator.Supervisor
		sched    *orchestrator.Scheduler
		planFire orchestrator.FiringPlanner
		// deliverer is the P6-15 resume hand-off as well as the delivery
		// path: the run is given it through runResumeHandler below, and it
		// is built further down (it needs the run).
		deliverer *orchestrator.Deliverer
	)
	opts := orchestrator.Options{
		Logger: logger,
		// The run's own finish report (the worktree cleanup, P6-18) goes
		// to stderr in headless mode, so `--json`'s stdout stays a
		// stream of envelopes; in the TUI it goes nowhere, because the
		// program owns the terminal and the report reaches the human
		// through the run log and events.RunFinished.
		Out: set.reportWriter(),
		Loop: orchestrator.LoopFunc(func(ctx context.Context, r *orchestrator.Run) (orchestrator.Outcome, error) {
			return sched.Run(ctx, r)
		}),
		Agents: stopperFunc(func(ctx context.Context, grace time.Duration) error {
			return sup.StopAgents(ctx, grace)
		}),
		Leases:  client,
		Cleaner: manager,
		Resumer: resumer,
		// P6-15/P9-03: acting on what the resumer found is the other half
		// of a resume. Without this the run reconciles the worktrees,
		// announces the states it wrote and then does nothing about the
		// three entries that need doing something about: a bead left
		// `delivering` with its pull request open and no gh:pr gate stays
		// delivering for ever (nothing polls a gate that does not exist,
		// so its dependents never become ready, D-20), a bead left
		// `delivering` before the pull request or the merge even happened
		// is nobody else's to ship, and a worktree left mid-rebase is
		// never handed back to its engineer.
		OnResume: orchestrator.DeliveryResumeHandler{
			Deliverer: func() *orchestrator.Deliverer { return deliverer },
		},
		LockPolicy: set.Lock,
	}
	signals, stopSignals := orchestrator.InterruptChannel()
	opts.Signals = signals
	c.stop = append(c.stop, stopSignals)

	// P8-20: the run log goes in front of the bus, so an event is durable
	// before any subscriber renders it.
	elog, err := orchestrator.TeeRunLog(&opts, rc.Run.StateDir, bus)
	if err != nil {
		return nil, err
	}
	c.log = elog

	// P7-17/P9-09: every event is scrubbed of terminal escapes and put
	// through the run's redaction hook once, here — the single point every
	// event the run publishes passes through, in front of both the run log
	// and the bus. A view's own scrub is then an idempotent second pass.
	// P9-18: the per-agent log files are written from the same point, before
	// the event is published, so a transcript cannot lose lines to a slow
	// bus subscriber. The seal at events.RunFinished and the P6-41 settle
	// hook need that same single point, so it is a type rather than a
	// closure now (see runPublisher).
	c.pub = &runPublisher{
		to:       opts.Bus,
		redactor: set.Filter.Redactor(),
		before:   newAgentLogSink(set.RunLog, logger).Observe,
	}
	opts.Bus = c.pub

	run, err := orchestrator.New(rc, opts)
	if err != nil {
		return nil, err
	}
	c.run = run
	sink.attach(run)

	// P6-27: the supervisor publishes every agent line through the run's
	// own agent bus, so state.json's sequence number and the run log see
	// them.
	sup, err = orchestrator.NewSupervisor(orchestrator.SupervisorOptions{
		Cartridges:      sel,
		ActiveCartridge: rc.CartridgeName,
		RoleModels:      rc.Effective.RoleModels,
		Research:        rc.Effective.Research,
		MaxTimeout:      limits.MaxAgentTimeout.Duration(),
		BeadTimeout: func(ctx context.Context, bead domain.BeadID) time.Duration {
			issues, err := client.Show(ctx, bead)
			if err != nil || len(issues) == 0 {
				return 0
			}
			d, _ := issues[0].Timeout()
			return d
		},
		ShimOptions: runShimOptions,
		Bus:         run.AgentBus(),
		Timeout:     limits.AgentTimeout.Duration(),
		Logger:      logger,
		Planner: orchestrator.FiringPlannerFunc(func(ctx context.Context, fi orchestrator.Firing) (orchestrator.AgentFiring, error) {
			return planFire.PlanFiring(ctx, fi)
		}),
	})
	if err != nil {
		return nil, err
	}
	sup.Attach(run)
	c.stop = append(c.stop, func() { _ = sup.Close() })

	// P6-28: one reporter for the whole run, so a bead in a status this
	// run never registered is named once — whether the conductor's
	// listing, the gate poller or both meet it.
	foreign := orchestrator.NewForeignStatusReporter(orchestrator.ForeignStatusOptions{
		Host: run, Statuses: statusMap, Logger: logger,
	})

	cond, err := orchestrator.NewConductor(orchestrator.ConductorOptions{
		Run:       run,
		Beads:     client,
		Statuses:  statusMap,
		Foreign:   foreign,
		Worktrees: manager,
		Git:       dgit,
		// P9-05: the conductor produces the only diff an agent's prompt
		// renders, so the run's filter — the built-in patterns plus the
		// configured `exclude` list — goes with it.
		Filter:       set.Filter,
		Repo:         scan,
		Brief:        rc.Run.Brief,
		Rules:        rules,
		Vibes:        vibes,
		CheckCommand: scan.CheckCommand,
		Model:        rc.Cartridge.Model,
		Timeout:      limits.AgentTimeout.Duration(),
		Interval:     runConductorInterval,
		Logger:       logger,
		// P9-06: a resume replays bd's comments without re-running any
		// role check, so the gate reads the thread before the loop does.
		// Without this the gate's memory is empty on `sc run --resume` and
		// a violation an old comment settled the wrong way ships.
		RulesGate: gate,
		// ... and the machine it parks a bead with when that replay is
		// refused: a rules refusal is permanent, so the bead goes to
		// needs_human instead of sitting in in_review with a thread the
		// loop believes is settled.
		States: states,
	})
	if err != nil {
		return nil, err
	}
	c.cond = cond

	feedback, err := orchestrator.NewFeedback(orchestrator.FeedbackOptions{
		Context:          cond.BeadContext,
		Ratio:            cond.ChangeRatio,
		ConflictResolved: cond.ConflictResolved,
		// P9-05: the loop is the last hand a prompt passes through — the
		// quoted history, the round's notes and the prose an agent replied
		// with are all assembled here — and it is where an echoed
		// credential is masked before bd stores it.
		Filter:     set.Filter,
		States:     states,
		Messages:   client,
		Acceptance: client,
		Bus:        orchestrator.PublisherFunc(run.Publish),
		Run:        rc.Run.ID,
		Logger:     logger,
		Limits:     limits,
		// P1-14: D-09's re-review threshold comes from the configuration.
		ReReviewThreshold: cfg.ReReviewThreshold,
	})
	if err != nil {
		return nil, err
	}

	// P9-01: a resumed run carries the spend of its earlier invocations —
	// [orchestrator.New] read it out of state.json — so `limits.max_tokens`
	// and `limits.max_cost_usd` cap the run rather than one `sc run`, and
	// UseBudget writes it back on every transition.
	budget := orchestrator.NewRunBudget(orchestrator.RunBudgetOptions{
		Limits: limits, Mode: rc.Run.Mode, Run: run, Host: run, Logger: logger,
		Restore: run.State().Spent(),
	})
	run.UseBudget(budget.Snapshot)
	c.budget = budget
	c.spend.carry(run.State().Spent().Spend)

	// D-11/P8-04: external pull requests are reviewed when `gh` and a
	// GitHub remote are both there and the user has not said otherwise.
	// The reviewer is a work source and a firing planner like any other,
	// and both wrappers are no-ops when it is nil.
	reviewer, _, err := newPRReviewer(ctx, prReviewSetup{
		RC: rc, Repo: repo, Worktrees: manager, Beads: client, States: states,
		Host: run, Statuses: statusMap, Context: cond.BeadContext, Logger: logger,
		// P8-04: `pr_poll_interval` moves the watcher and the reviewer
		// together, the way `gate_poll_interval` moves the gate poller.
		// Without it the interval was a constant no run could change, so a
		// run shorter than a minute polled once and P8-11's stale path was
		// out of reach.
		Interval: cfg.PRPollInterval.Duration(),
		// P9-05: the reviewer's own diff of somebody else's pull request
		// goes through the same filter as the run's own change sets.
		Filter: set.Filter,
	})
	if err != nil {
		return nil, err
	}
	c.prs = reviewer

	planFire = gate.GuardPlanner(c.spend.meter(budget.MeterPlanner(
		prReviewPlanner(reviewer, runFeedbackPlanner(feedback)))))

	planner, err := orchestrator.NewPlanner(orchestrator.PlannerOptions{
		Config:      rc,
		ShimOptions: runShimOptions,
		// The planning firing bypasses the scheduler's firing planner, so
		// it is metered here or nowhere (P9-17).
		Runner:  c.spend.meterPlan(budget.MeterPlanRunner(orchestrator.SupervisorPlanRunner(sup))),
		Applier: client,
		Beads:   cond.BeadSource(),
		Credits: sup,
		Repo:    scan,
		// P9-05: the scan above is already filtered; the planner is given
		// the filter as well, so its own fallback scan cannot become the
		// one prompt of the run that lists .env.
		Filter:         set.Filter,
		Rules:          rules,
		Vibes:          vibes,
		Logger:         logger,
		SettleInterval: runSettleInterval,
		Status: func() orchestrator.SchedulerStatus {
			if sched == nil {
				return orchestrator.SchedulerStatus{}
			}
			return sched.Status()
		},
	})
	if err != nil {
		return nil, err
	}
	planner.Attach(run)

	deliverer, err = runDeliverer(run, states, dgit, client, manager, rc, set.Filter, logger)
	if err != nil {
		return nil, err
	}
	// P6-41: the deliverer keeps one entry per bead it delivered until it
	// hears that the delivery ended, which for a safety-on delivery is the
	// gate poller closing the bead — an event rather than a return value.
	// The settler is fed from the run's single publication point rather
	// than from a bus subscription, because events.Bus drops for a
	// subscriber that is behind and a burst of agent output must not be
	// able to lose the one event this exists for. It is asynchronous
	// because Observe takes the deliverer's own lock and the deliverer
	// publishes while holding it.
	c.settleq = newSettleQueue(deliverer.Observe)
	c.stop = append(c.stop, c.settleq.close)
	c.pub.watch(c.settleq.push)

	sched = orchestrator.NewScheduler(orchestrator.SchedulerOptions{
		Allocation: rc.Allocation,
		// The feedback loop is offered nothing on a bead the deliverer
		// holds for a conflict round (P6-66).
		Work:                budget.Work(prReviewSources(reviewer, planner, orchestrator.ExceptHeld(deliverer, feedback), deliverer)),
		Fire:                orchestrator.PlanRouter(planner, sup),
		Claims:              client,
		Stop:                budget.Stop(planner),
		Tick:                runTickInterval,
		Park:                states,
		SuppressAgentEvents: true,
	})

	poller, err := orchestrator.NewGatePoller(orchestrator.GatePollerOptions{
		Gates: client, Beads: client, States: states, Host: run,
		Scope: rc.Run.ID, Statuses: statusMap, Foreign: foreign,
		Branch: branch, Interval: cfg.GatePollInterval.Duration(), Logger: logger,
	})
	if err != nil {
		return nil, err
	}
	c.gates = poller

	cond.Attach(orchestrator.ConductorParts{
		Feedback: feedback, Delivery: deliverer, Gates: poller,
	})

	// P6-29: every state.json carries the "why is nothing happening?"
	// snapshot, so `sc status` — which reads a run off disk and has no
	// live planner or poller to ask — can say when the next plan is due,
	// that gh has been down for an hour, or that a merge is still waiting
	// on bd.
	// P9-06's counters ride with them, so `sc status` can say how much the
	// rules gate refused and how many beads it is still holding.
	c.vitals = func() orchestrator.RunVitals {
		return orchestrator.Vitals(run.Now(), planner, poller, foreign).WithRules(gate)
	}
	run.UseVitals(c.vitals)

	if runOnAssemble != nil {
		if err := runOnAssemble(c); err != nil {
			return nil, err
		}
	}

	if rc.Resume {
		// A killed run left its beads claimed; the leases come back before
		// the run acts on what gitx finds in the worktrees (P3-05, P6-15).
		rec, err := client.RecoverStale(ctx, beads.RecoverOptions{Run: rc.Run.ID})
		if err != nil {
			return nil, fmt.Errorf("recovering the previous run's leases: %w", err)
		}
		if ids := rec.IDs(); len(ids) > 0 {
			logger.Info("recovered stale bead leases", slog.Int("beads", len(ids)))
		}
	}
	return c, nil
}

// runFeedbackPlanner is the [orchestrator.FiringPlanner] the supervisor
// plans every scheduler firing through: the feedback loop owns every demand
// that names a bead, and a planning demand never reaches here (the
// [orchestrator.PlanRouter] sends it to the planner).
func runFeedbackPlanner(f *orchestrator.Feedback) orchestrator.FiringPlanner {
	return orchestrator.FiringPlannerFunc(func(ctx context.Context, fi orchestrator.Firing) (orchestrator.AgentFiring, error) {
		if f.Handles(fi.Demand) {
			return f.PlanFiring(ctx, fi)
		}
		return orchestrator.AgentFiring{}, fmt.Errorf("%w: %s", orchestrator.ErrUnknownFeedbackWork, fi.Demand.Kind)
	})
}

// runResumer builds the P6-37 guard around the git resume: a worktree is
// only reattached for a bead bd still holds in a state the edge is defined
// for.
func runResumer(m *gitx.Manager, client *beads.Client, statuses beads.StatusMap, logger *slog.Logger) (*gitx.Resumer, error) {
	tracker, err := orchestrator.NewResumeTracker(orchestrator.ResumeTrackerOptions{
		Store: client, Beads: client, Statuses: statuses, Logger: logger,
	})
	if err != nil {
		return nil, err
	}
	return gitx.NewResumer(m, tracker, gitx.WithResumeLogger(logger))
}

// runDeliverer builds the delivery handoff with the path the run's safety
// setting needs (D-10): a pull request with safety on, a rebase and
// fast-forward merge with safety off.
func runDeliverer(run *orchestrator.Run, states *orchestrator.StateMachine, dgit orchestrator.DeliveryGit,
	client *beads.Client, m *gitx.Manager, rc *config.RunConfig, filter *agents.SecretFilter,
	logger *slog.Logger) (*orchestrator.Deliverer, error) {
	opts := orchestrator.DeliveryOptions{
		Host: run, States: states, Git: dgit, Beads: client,
		CheckCommand: rc.Effective.CheckCommand,
		// P1-14 and P1-18: D-09's threshold and the conflict-round cap
		// come from the configuration, not from the package constants.
		ReReviewThreshold: rc.Effective.ReReviewThreshold,
		MaxConflictRounds: rc.Effective.Limits.MaxConflictRounds,
		// P9-05: the pull request body is the only sink of agent prose
		// that leaves the machine.
		Filter: filter,
	}
	if rc.Run.Config.Safety {
		pr, err := gitx.NewPRDeliverer(m, client, gitx.WithPRLogger(logger))
		if err != nil {
			return nil, err
		}
		opts.PR = pr
	} else {
		opts.Merge = m
		settler, err := gitx.NewPRSettler(m, client)
		if err != nil {
			return nil, err
		}
		opts.Settler = settler
	}
	return orchestrator.NewDeliverer(opts)
}

// stopperFunc adapts a function to [orchestrator.AgentStopper]. It is how
// the run is given a supervisor that does not exist yet: the run only ever
// calls it while it is shutting down, long after the assembly finished.
type stopperFunc func(ctx context.Context, grace time.Duration) error

// StopAgents implements [orchestrator.AgentStopper].
func (f stopperFunc) StopAgents(ctx context.Context, grace time.Duration) error {
	return f(ctx, grace)
}

// lateRun is the [orchestrator.Publisher] the parts built before the run
// publish through. Once the run exists every event goes through it, so the
// run log and state.json's sequence number see it (P6-01, P6-08); before
// then — nothing publishes at that point — it falls back to the bare bus.
type lateRun struct {
	bus *events.Bus
	mu  sync.RWMutex
	run *orchestrator.Run
}

// attach names the run every later event goes through.
func (l *lateRun) attach(r *orchestrator.Run) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.run = r
}

// Now is the run's clock, or the package's own until the run exists. It is
// what the P9-06 rules gate stamps its warnings with, so a gate built
// before the run is still on the run's time base.
func (l *lateRun) Now() time.Time {
	l.mu.RLock()
	run := l.run
	l.mu.RUnlock()
	if run != nil {
		return run.Now()
	}
	return runNow()
}

// Publish implements [orchestrator.Publisher].
func (l *lateRun) Publish(ev events.Event) {
	l.mu.RLock()
	run := l.run
	l.mu.RUnlock()
	if run != nil {
		run.Publish(ev)
		return
	}
	l.bus.Publish(ev)
}

// runPublisher is [orchestrator.Options.Bus]: the one place every event the
// run publishes passes through, with the run log, the agent transcripts,
// the event bus and the delivery settler all behind it. Four things need
// exactly this point and nothing looser.
//
//   - P7-17/P9-09: the scrub and the redaction happen once, in front of
//     every sink, so no view can forget to do them.
//   - P9-18: an agent's transcript is written before the event is
//     published, so it cannot lose lines to a slow bus subscriber.
//   - P6-41: the delivery settler is fed from here rather than from a bus
//     subscription, because [events.Bus] drops for a subscriber that is
//     behind and the one event the settler exists for must never be one of
//     them.
//
// Publishing is serialised on one mutex, so the sinks behind it are handed
// the events in one order. The stream's seal at [events.RunFinished] is
// *not* here: [orchestrator.Run.Publish] is the one place that bumps
// state.json's LastSeq and it drops everything published after the
// terminator, so nothing reaches this point once the run is over however
// the helpers beside it are timed (P8-01, unified in round 3).
type runPublisher struct {
	to       orchestrator.Publisher
	redactor *logging.Redactor
	// before is handed every event before it is published (the agent
	// transcripts). It must not block and must not publish.
	before func(events.Event)

	mu      sync.Mutex
	watcher func(events.Event)
}

var _ orchestrator.Publisher = (*runPublisher)(nil)

// watch names a sink every published event is handed to after the log and
// the bus have it. Like before, it must not block and must not publish;
// [settleQueue] is the one caller.
func (p *runPublisher) watch(fn func(events.Event)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.watcher = fn
}

// Publish implements [orchestrator.Publisher].
func (p *runPublisher) Publish(ev events.Event) {
	if ev == nil {
		return
	}
	// Redaction is pure and can be expensive, so it happens off the lock.
	safe := p.redactor.SafeEvent(ev)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.before != nil {
		p.before(safe)
	}
	p.to.Publish(safe)
	if p.watcher != nil {
		p.watcher(safe)
	}
}

// watching reports whether a [watch] sink is attached, which is how a test
// proves the P6-41 settler is still wired into the assembled run.
func (p *runPublisher) watching() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.watcher != nil
}

// settleQueue hands the run's events to the deliverer's settler (P6-41) in
// the order they were published, on a goroutine of its own and without ever
// dropping one.
//
// It is neither synchronous nor a bus subscription, and both halves matter.
// Synchronous would deadlock: [orchestrator.Deliverer.Observe] takes the
// deliverer's lock, and the deliverer publishes while holding it. A bus
// subscription drops: [events.Bus] never blocks a publisher, so a burst of
// agent output while this goroutine is descheduled could lose the very
// BeadStateChanged that says a safety-on delivery ended — the one event the
// settler exists for. The queue grows instead, which is bounded in practice
// by how fast Observe returns (a type switch and a map delete).
type settleQueue struct {
	observe func(events.Event)
	done    chan struct{}

	mu     sync.Mutex
	cond   *sync.Cond
	queue  []events.Event
	closed bool
}

// newSettleQueue starts a queue that hands every pushed event to observe.
func newSettleQueue(observe func(events.Event)) *settleQueue {
	q := &settleQueue{observe: observe, done: make(chan struct{})}
	q.cond = sync.NewCond(&q.mu)
	go q.loop()
	return q
}

// push queues one event. It never blocks and never drops.
func (q *settleQueue) push(ev events.Event) {
	if ev == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.queue = append(q.queue, ev)
	q.cond.Signal()
}

// loop hands queued events to the observer until the queue is closed and
// empty.
func (q *settleQueue) loop() {
	defer close(q.done)
	for {
		q.mu.Lock()
		for len(q.queue) == 0 && !q.closed {
			q.cond.Wait()
		}
		if len(q.queue) == 0 {
			q.mu.Unlock()
			return
		}
		ev := q.queue[0]
		q.queue[0] = nil
		q.queue = q.queue[1:]
		q.mu.Unlock()
		q.observe(ev)
	}
}

// close stops the queue once everything already pushed has been observed.
// It is idempotent.
func (q *settleQueue) close() {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		q.cond.Broadcast()
	}
	q.mu.Unlock()
	<-q.done
}

// discardRunDir removes the run directory of a run that never started, so
// that `--resume` — which adopts the greatest run id on disk
// (config.LatestRunID) — cannot later take the id of an assembly that
// aborted and fire a fresh run under a dead run's name.
//
// It only ever removes a directory nothing was recorded into: a directory
// with a state file, or with a single line in its event log, is left
// exactly as it is, which is why a resumed run whose assembly failed keeps
// everything the earlier invocation wrote. Best effort — what it cannot
// remove is only ever a stale directory.
func discardRunDir(dir string, logger *slog.Logger) {
	if dir == "" {
		return
	}
	if _, err := os.Stat(filepath.Join(dir, orchestrator.StateFileName)); err == nil {
		return
	}
	if info, err := os.Stat(filepath.Join(dir, orchestrator.EventLogName)); err == nil && info.Size() > 0 {
		return
	}
	if err := os.RemoveAll(dir); err != nil && logger != nil {
		logger.Warn("could not remove the directory of a run that never started",
			slog.String("dir", dir), slog.Any("err", err))
	}
}

// runSpend totals what the run's agents cost. The P9-01 budget meters the
// same numbers, but only when a cap is configured; the final summary wants
// them either way, so this chains onto the same hook.
type runSpend struct {
	mu      sync.Mutex
	usage   cartridge.Usage
	firings int
	// prior is what earlier invocations of a resumed run spent (P9-01).
	// Without it the summary of a resumed run counted its agents over the
	// whole run log and its tokens over this process alone, and said "8
	// agents fired" beside "over 2 sessions".
	prior orchestrator.Spend
}

// carry adopts the spend an earlier invocation of the same run left behind.
func (s *runSpend) carry(prior orchestrator.Spend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prior = prior
}

// meter wraps a firing planner so every session that finishes and decodes
// adds its usage to the total.
func (s *runSpend) meter(inner orchestrator.FiringPlanner) orchestrator.FiringPlanner {
	if inner == nil {
		return nil
	}
	return orchestrator.FiringPlannerFunc(func(ctx context.Context, f orchestrator.Firing) (orchestrator.AgentFiring, error) {
		af, err := inner.PlanFiring(ctx, f)
		if err != nil {
			return af, err
		}
		prev := af.Result
		af.Result = func(payload any, tr agents.Transcript) error {
			s.add(tr.Usage)
			if prev != nil {
				return prev(payload, tr)
			}
			return nil
		}
		return af, nil
	})
}

// meterPlan is meter for the planning product owner, whose firing the
// planner builds itself rather than through a firing planner (P9-17):
// without it the run summary reports one fewer session than it fired, and
// the missing one is always the product owner.
func (s *runSpend) meterPlan(inner orchestrator.PlanRunner) orchestrator.PlanRunner {
	if inner == nil {
		return nil
	}
	return orchestrator.PlanRunnerFunc(func(ctx context.Context, f orchestrator.Firing, req agents.PlanRequest) (domain.Plan, agents.Transcript, error) {
		plan, tr, err := inner.RunPlan(ctx, f, req)
		if orchestrator.UsageReported(tr.Usage) {
			s.add(tr.Usage)
		}
		return plan, tr, err
	})
}

// add records one session's usage.
func (s *runSpend) add(u cartridge.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage = s.usage.Add(u)
	s.firings++
}

// total returns the usage of every metered session and how many there were,
// including the sessions a resumed run inherited from its earlier
// invocations.
func (s *runSpend) total() (tokens int64, cost float64, hasCost bool, firings int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tokens = int64(s.usage.TotalTokens()) + s.prior.Tokens
	cost = s.usage.CostUSD + s.prior.CostUSD
	hasCost = s.usage.HasCost || s.prior.HasCost
	return tokens, cost, hasCost, s.firings + s.prior.Firings
}

// runHelper is one of the goroutines that live beside the run, under the
// name the run calls it by. P8-35: the tasks used to be anonymous
// `func(context.Context) error` values, so a helper that would not stop
// could only be reported as "a helper".
type runHelper struct {
	// Name is what the warning calls it ("the conductor").
	Name string
	// Run is the goroutine's body; it returns when its context is
	// cancelled.
	Run func(context.Context) error
}

// runHelpers are the goroutines that live beside the run — the conductor,
// the gate poller and the external pull request reviewer (P8-04) — and the
// bound on how long anything waits for them.
type runHelpers struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu sync.Mutex
	// names is every helper, in start order; done names the ones that have
	// returned. Together they are what [runHelpers.outstanding] reports.
	names []string
	done  map[string]bool
}

// startRunHelpers fires every task in its own goroutine under a context
// this one cancels. The context is detached from ctx's cancellation on
// purpose: a SIGINT stops the run through its own lifecycle, and the
// helpers have to keep going until the run says it is over.
func startRunHelpers(ctx context.Context, tasks ...runHelper) *runHelpers {
	hctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	h := &runHelpers{cancel: cancel, done: make(map[string]bool, len(tasks))}
	for _, task := range tasks {
		h.names = append(h.names, task.Name)
		h.wg.Add(1)
		go func() {
			defer h.finished(task.Name)
			_ = task.Run(hctx)
		}()
	}
	return h
}

// finished records that one helper's goroutine has returned.
func (h *runHelpers) finished(name string) {
	h.mu.Lock()
	h.done[name] = true
	h.mu.Unlock()
	h.wg.Done()
}

// outstanding names the helpers that have not returned yet, in start order.
func (h *runHelpers) outstanding() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, name := range h.names {
		if !h.done[name] {
			out = append(out, name)
		}
	}
	return out
}

// stop cancels the helpers and waits up to grace for them to return,
// reporting whether they all did. It may be called more than once, and
// concurrently: `sc run` stops them from the goroutine that closes the
// event bus and again on the way out.
//
// The wait is bounded on both paths (P8-01). A helper is inside bd, git or
// gh, none of which this process can abort, so an unbounded wait would hang
// `sc run` on a wedged subprocess — the first time with the renderers still
// open, the second with the summary unprinted. Giving up leaks that one
// goroutine until the process exits: by then the run has finished, its
// event stream is sealed and every writer the goroutine shares with the
// exit path is locked, so it can delay nothing and corrupt nothing.
func (h *runHelpers) stop(grace time.Duration) bool {
	h.cancel()
	stopped := make(chan struct{})
	go func() { defer close(stopped); h.wg.Wait() }()
	select {
	case <-stopped:
		return true
	case <-time.After(grace):
		return false
	}
}

// stopOrWarn is [runHelpers.stop] plus P8-35's report: a helper that will
// not return is named, so the line says which of the conductor, the gate
// poller and the external pull request reviewer is wedged rather than
// leaving the reader to guess.
func (h *runHelpers) stopOrWarn(log *slog.Logger, grace time.Duration) {
	if h.stop(grace) {
		return
	}
	if log == nil {
		return
	}
	log.Warn("a helper did not stop; ending the run without it",
		slog.String("helpers", strings.Join(h.outstanding(), ", ")),
		slog.Duration("waited", grace))
}

// fire starts the run and everything that runs beside it, renders it, and
// returns the process exit code (D-15's contract: 0 completed or stopped, 1
// failed, 3 credits exhausted).
func (c *cannon) fire(cmd *cobra.Command, gopts GlobalOptions, headless bool) (int, error) {
	ctx := cmd.Context()
	if err := c.run.Start(ctx); err != nil {
		// P9-04: the run never started, so the run directory this
		// assembly created must not outlive it. config.LatestRunID takes
		// the greatest run id on disk, so an empty directory whose random
		// suffix sorts after the live run's is what `sc status` would
		// report and what `sc resume` would adopt — a brand-new autopilot
		// run under an id that never ran (P8-31) — instead of the run this
		// very refusal points at. Losing the lock race here is the
		// systematic producer of such directories, so the release
		// assembleRun does on its own failure path is done here too: the
		// log is closed first, so nothing is still writing into what is
		// being removed, and discardRunDir keeps any directory that has a
		// state file or a line in its event log.
		c.close()
		discardRunDir(c.rc.Run.StateDir, c.set.Logger)
		// P8-06: Start re-takes the repository lock (P9-04), so another sc
		// can win the window between fireCannon's preflight check and this
		// call. That refusal is the one startup failure the fix list exists
		// for, and it goes through the same numbered advice as the check
		// that lost it earlier — never as a bare error line.
		return 0, StartupFailure(ctx, c.reports(cmd), c.rc.Run.RepoPath, err)
	}
	// P9-04: record this process's start stamp on the repository's run
	// lock, so a recycled pid cannot look like a live run. Best effort:
	// the run is already going.
	if err := orchestrator.StampRunLock(config.StateDir(c.rc.Run.RepoPath), orchestrator.LockOptions{}); err != nil {
		c.set.Logger.Warn("could not stamp the run lock", slog.Any("err", err))
	}

	// P8-35: every helper is named where it is started, so the warning a
	// wedged one raises can say which it was.
	tasks := []runHelper{
		{Name: "the gate poller", Run: c.gates.Run},
		{Name: "the conductor", Run: c.cond.Run},
	}
	if c.prs != nil {
		tasks = append(tasks, runHelper{Name: "the external pull request reviewer", Run: c.prs.Run})
	}
	helpers := startRunHelpers(ctx, tasks...)

	// The renderers end when the event stream does, so the bus is closed
	// the moment the run is over rather than when fireCannon returns. A
	// closed channel still yields what it holds, so the last events —
	// events.RunFinished among them — are rendered before the stream ends.
	//
	// The helpers are stopped first. The run persists state.json for the
	// last time as it publishes events.RunFinished, so a conductor pass or
	// a gate poll already in flight when the run finished would otherwise
	// write an event into events.jsonl that state.json's last_seq never
	// counts — a log that no longer closes with run_finished.
	// orchestrator.Run.Publish seals the stream so that cannot happen
	// whatever the timing; stopping the helpers here is what keeps the
	// seal from having to drop anything real. They are not waited for
	// indefinitely: a helper that will not stop must not hold the
	// renderers open.
	go func() {
		<-c.run.Done()
		helpers.stop(runHelperGrace)
		c.bus.Close()
	}()

	var (
		code int
		rerr error
	)
	if headless {
		// P9-20: `sc run --headless | head` closes the pipe under the
		// renderer. The screen is wrapped so that a reader going away stops
		// the run instead of leaving it to die on SIGPIPE with its lock
		// held and its worktrees behind it.
		screen := c.stopOnClosedOutput(c.screen(cmd))
		// The renderer deliberately outlives a cancelled context: a
		// SIGINT stops the run, and the run's last events are what say so.
		code, rerr = runHeadless(context.WithoutCancel(ctx), tui.Options{
			Bus:    c.bus,
			Run:    c.header,
			Output: screen,
		}, tui.HeadlessOptions{
			Out:  screen,
			JSON: gopts.JSON,
		})
	} else {
		rerr = c.renderTUI(ctx, cmd, gopts)
	}

	out, runErr := c.run.Wait()
	// P8-01: bounded here too. The renderer has already ended, so a helper
	// that will not stop would otherwise hang `sc run` forever with the
	// summary unprinted — exactly what the grace above exists to prevent,
	// one wait later. Giving up leaks the helper's goroutine (and the bd
	// or gh call it is inside) until the process exits moments later; it
	// cannot corrupt anything it is racing with, because the run is
	// finished, orchestrator.Run.Publish has sealed the event stream and
	// every writer it shares with this path is locked.
	helpers.stopOrWarn(c.set.Logger, runHelperGrace)
	c.closeLog()
	if n := c.run.PublishedAfterFinish(); n > 0 {
		// Not an error: the run was over. It is worth a line, because it
		// means a helper was still going after events.RunFinished.
		c.set.Logger.Debug("dropped events published after the run finished", slog.Int("events", n))
	}

	if !headless {
		code = tui.ExitCodeFor(out.Reason)
	}
	if err := c.writeSummary(c.reports(cmd), out); err != nil {
		return code, err
	}
	// P8-06: the first run that succeeds in a repository says where
	// everything it wrote lives; a stopped or failed run leaves the notice
	// for the run that gets there (out.Reason is what decides). c.header is
	// the pre-Start copy, so this cannot race the planner's epic write.
	if err := ReportFirstRun(c.reports(cmd), c.header, out.Reason); err != nil {
		return code, err
	}
	if rerr != nil {
		return code, rerr
	}
	return code, runErr
}

// screen is where the renderer draws and reports is where the run's own
// lines go. Both come from the setup fireCannon built, which guards the
// shared writer (P8-01); a test that assembles a run by hand and names
// neither falls back to the command's own streams.
func (c *cannon) screen(cmd *cobra.Command) io.Writer {
	if c.set.Screen != nil {
		return c.set.Screen
	}
	return cmd.OutOrStdout()
}

func (c *cannon) reports(cmd *cobra.Command) io.Writer {
	if c.set.Out != nil {
		return c.set.Out
	}
	return cmd.ErrOrStderr()
}

// renderTUI draws the run until the user quits. Quitting the TUI stops the
// run: the program owns the terminal, and a cannon still firing with
// nothing watching it is not what `q` means.
//
// P8-32: a finished run does *not* take the TUI down with it. The program
// used to be cancelled the moment the orchestrator finished, so a run ended
// with the alt screen already torn down: the user never saw the completed
// graph, and the keys were unreachable exactly when there was something
// worth looking at. The run's own event stream ends (the bus is closed once
// the run is over), the header says "finished: <reason>", and `q` on a run
// that is no longer live quits at once instead of asking whether to stop it
// — so the last frame is held until the user asks to leave. A signal still
// takes the program down, because ctx is the command's own context and
// cli.Execute cancels it on SIGINT/SIGTERM.
func (c *cannon) renderTUI(ctx context.Context, cmd *cobra.Command, gopts GlobalOptions) error {
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	err := runTUI(tctx, tui.Options{
		Bus:        c.bus,
		Run:        c.header,
		NoAnim:     c.rc.NoAnim,
		NoColor:    gopts.NoColor,
		AltScreen:  true,
		Output:     c.screen(cmd),
		Controller: &runController{run: c.run, budget: c.budget},
		Vitals:     c.vitalsLine,
	})
	_ = c.run.Stop("the TUI was closed")
	return err
}

// vitalsLine is the run's "why is nothing happening?" line for the TUI
// header (P6-51). It is pulled on the render goroutine each frame, so a
// header can never show a snapshot older than the frame it is in.
func (c *cannon) vitalsLine() string {
	if c.vitals == nil {
		return ""
	}
	return c.vitals().Header()
}

// closeLog closes the run log once, before the summary is folded from it.
func (c *cannon) closeLog() {
	if c.log == nil {
		return
	}
	_ = c.log.Close()
	c.log = nil
}

// writeSummary prints the final account of the run: what happened to the
// beads, how many agents were fired, what was delivered, how long it took
// and what it cost. It folds the run log rather than counting events a
// second time, so `sc status` and this report cannot disagree (P6-08).
func (c *cannon) writeSummary(w io.Writer, out orchestrator.Outcome) error {
	rows := [][2]string{
		{"run", fmt.Sprintf("%s %s", c.rc.Run.ID, runFinishText(out))},
	}
	sum, err := orchestrator.SummariseLog(c.rc.Run.StateDir, -1)
	if err == nil {
		rows = append(rows,
			[2]string{"beads", runBeadsLine(sum)},
			[2]string{"agents", fmt.Sprintf("%s fired, %d finished, %d failed",
				runPlural(sum.Fired, "agent"), sum.Completed, sum.Failed)},
			[2]string{"delivery", runDeliveryLine(sum)},
		)
		if sum.Warnings > 0 || sum.Errors > 0 {
			rows = append(rows, [2]string{"notices", fmt.Sprintf("%s, %s",
				runPlural(sum.Warnings, "warning"), runPlural(sum.Errors, "error"))})
		}
	} else {
		rows = append(rows, [2]string{"beads", "unknown: " + err.Error()})
	}
	if line := runSpendLine(c.spend); line != "" {
		rows = append(rows, [2]string{"spend", line})
	}
	rows = append(rows, [2]string{"state", c.rc.Run.StateDir})
	for _, r := range rows {
		if _, err := fmt.Fprintf(w, "%-11s%s\n", r[0], r[1]); err != nil {
			return err
		}
	}
	if s := strings.TrimSpace(out.Summary); s != "" {
		if _, err := fmt.Fprintln(w, s); err != nil {
			return err
		}
	}
	return nil
}

// runFinishText renders the outcome's reason and duration.
func runFinishText(out orchestrator.Outcome) string {
	reason := string(out.Reason)
	if reason == "" {
		reason = "ended"
	}
	return fmt.Sprintf("%s in %s", reason, out.Duration.Round(time.Second))
}

// runBeadsLine counts the run's beads by what became of them.
func runBeadsLine(sum orchestrator.RunSummary) string {
	total := len(sum.Beads)
	if total == 0 {
		return "none"
	}
	parts := []string{
		fmt.Sprintf("%d done", sum.Counts[domain.BeadStateClosed]),
		fmt.Sprintf("%d for a human", sum.Counts[domain.BeadStateNeedsHuman]+sum.Counts[domain.BeadStateEscalated]),
		fmt.Sprintf("%d failed", sum.Counts[domain.BeadStateFailed]),
	}
	unfinished := total - sum.Counts[domain.BeadStateClosed] -
		sum.Counts[domain.BeadStateNeedsHuman] - sum.Counts[domain.BeadStateEscalated] -
		sum.Counts[domain.BeadStateFailed]
	if unfinished > 0 {
		parts = append(parts, fmt.Sprintf("%d unfinished", unfinished))
	}
	return fmt.Sprintf("%d: %s", total, strings.Join(parts, ", "))
}

// runSpendLine renders the token and cost totals, or "" when no session
// reported any.
func runSpendLine(s *runSpend) string {
	tokens, cost, hasCost, firings := s.total()
	if firings == 0 {
		return ""
	}
	parts := []string{fmt.Sprintf("%d tokens", tokens)}
	if hasCost {
		parts = append(parts, fmt.Sprintf("$%.2f", cost))
	}
	parts = append(parts, "over "+runPlural(firings, "session"))
	return strings.Join(parts, ", ")
}

// runDeliveryLine is the summary's delivery row: what the run opened and
// merged, and — for a D-11 run whose work was reviewing somebody else's
// pull requests — how many of those it reviewed.
//
// P8-01: the reviewed count used to survive only in `--json`, so a run that
// posted five reviews and opened nothing reported "0 pull requests, 0
// merges" and said nothing about the five. It is left out when it is zero,
// so an ordinary run's row is the one it always was.
func runDeliveryLine(sum orchestrator.RunSummary) string {
	line := fmt.Sprintf("%s, %s",
		runPlural(sum.PRs, "pull request"), runPlural(sum.Merges, "merge"))
	if sum.Reviewing > 0 {
		line += fmt.Sprintf(", %s reviewed", runPlural(sum.Reviewing, "external pull request"))
	}
	return line
}

// runPlural renders "1 thing" / "2 things".
func runPlural(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

// runController is the orchestrator behind the TUI's run-control keys
// (P7-10). Pause, resume and stop are the run's own lifecycle; the rest
// need a seam no component offers yet, so they say so rather than pretend.
// P7-20 is the task that owns those four seams, and it is the id the
// messages name.
type runController struct {
	run    *orchestrator.Run
	budget *orchestrator.RunBudget
}

var (
	_ tui.Controller       = (*runController)(nil)
	_ tui.BudgetController = (*runController)(nil)
)

// Pause implements [tui.Controller].
func (c *runController) Pause(_ context.Context, reason string) error { return c.run.Pause(reason) }

// Resume implements [tui.Controller].
func (c *runController) Resume(_ context.Context, reason string) error { return c.run.Resume(reason) }

// Stop implements [tui.Controller].
func (c *runController) Stop(_ context.Context, reason string) error { return c.run.Stop(reason) }

// Refire implements [tui.Controller].
func (c *runController) Refire(_ context.Context, bead domain.BeadID, _ string) error {
	return fmt.Errorf("re-firing %s is not wired up yet (P7-20)", bead)
}

// MarkDone implements [tui.Controller].
func (c *runController) MarkDone(_ context.Context, bead domain.BeadID, _ string) error {
	return fmt.Errorf("marking %s done is not wired up yet (P7-20)", bead)
}

// SetSafety implements [tui.Controller].
func (c *runController) SetSafety(_ context.Context, _ bool) error {
	return errors.New("changing safety mid-run is not wired up yet (P7-20)")
}

// SetAmmo implements [tui.Controller].
func (c *runController) SetAmmo(_ context.Context, _ int) error {
	return errors.New("changing ammo mid-run is not wired up yet (P7-20)")
}

// SetBudget implements [tui.BudgetController] (P9-14): the `b` key raises
// the cap that stopped the run and lets it carry on. Unlike the four seams
// above this one exists — [orchestrator.RunBudget] owns the live ceiling —
// so the key really works, and the orchestrator's own refusals (no budget
// set, an amount under what the run has spent) are what the footer shows.
func (c *runController) SetBudget(_ context.Context, amount float64) error {
	_, err := c.budget.Raise(amount)
	return err
}
