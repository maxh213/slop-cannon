package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/agents/prompt"
	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	// The Claude adapter is the only one a user may fire (D-19); importing
	// it here is what puts it in cartridge.Default. The mock adapter
	// registers itself the same way, so a test binary that imports
	// internal/cartridge/mock can select it without loosening D-19 for
	// users (P8-08).
	_ "slop-cannon/internal/cartridge/claude"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/logging"
	"slop-cannon/internal/orchestrator"
	"slop-cannon/internal/tui"
)

// Flag names of the run form (`sc <repo-path>` and `sc run <repo-path>`).
const (
	FlagRunMessage   = "message"
	FlagRunHeadless  = "headless"
	FlagRunResume    = "resume"
	FlagRunAmmo      = "ammo"
	FlagRunSafety    = "safety"
	FlagRunCartridge = "cartridge"
	FlagRunNoAnim    = "no-anim"
	FlagRunReviewPRs = "review-prs"
	// FlagRunForce takes the repository's run lock from a run that may
	// still be alive (P9-04). It is never sc's own decision.
	FlagRunForce = "force"
	// FlagRunDryRun is hidden: it resolves the run and prints it without
	// firing anything. It is how the resolution tests and `--json`'s
	// RunConfig output survive P8-01, and it is deliberately not in the
	// help output or docs/cli.md until someone documents it.
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
	f.Int(FlagRunAmmo, 0, "override ammo for this run (`N` agents at once, minimum 3)")
	f.String(FlagRunSafety, "", "override safety for this run (`on|off`): on opens PRs, off merges to the default branch")
	f.String(FlagRunCartridge, "", "fire cartridge `NAME` instead of the active one")
	f.Bool(FlagRunNoAnim, false, "disable the cannon animations in the TUI (also honours SC_NO_ANIM)")
	f.Bool(FlagRunReviewPRs, true, "also review open pull requests sc did not create (needs gh and a remote)")
	f.Bool(FlagRunForce, false, "start even though another run holds this repository's lock (only when you know it is gone)")
	f.Bool(FlagRunDryRun, false, "resolve the run and print it without firing anything")
	_ = f.MarkHidden(FlagRunDryRun)
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
	if !opts.Resume {
		settings.ID = runNewID(now)
	}
	rc, err := config.NewRunConfig(root, eff, settings)
	if errors.Is(err, config.ErrNoCartridge) {
		return nil, nil, fmt.Errorf("%w — add one with: %s", config.ErrNoCartridge, CartridgeAddHint)
	}
	if err != nil {
		return nil, nil, err
	}
	if !opts.Resume {
		return rc, nil, nil
	}
	res, err := orchestrator.RestoreRunConfig(rc)
	if err != nil {
		return nil, nil, err
	}
	return rc, res.Notes, nil
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
		return "", fmt.Errorf("%s: no such directory", path)
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s: not a directory", path)
	}
	root, ok := config.FindRepoRoot(abs)
	if !ok {
		return "", fmt.Errorf("%s is not a git repository (no .git in it or any parent directory)", abs)
	}
	return root, nil
}

// runTarget is the RunE of both `sc PATH` and `sc run [PATH]`: it resolves
// the run and then fires it (P8-01). With the hidden --dry-run it stops at
// the resolved configuration and prints it, which is what the resolution
// tests and `sc --json <repo> --dry-run` read.
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
	stderr := cmd.ErrOrStderr()
	// --json is only meaningful as a stream of event envelopes, and a TUI
	// cannot produce one: it implies --headless (D-15, P8-12).
	headless := rc.Run.Config.Headless || gopts.JSON
	if !headless {
		if err := runRequireTerminal(); err != nil {
			return err
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
	// built-in secret patterns from the prompts, and redacts the logs.
	filter, err := agents.NewSecretFilter(rc.Effective.Exclude...)
	if err != nil {
		return err
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
	logger := runLog.Logger()
	if headless {
		logger = slog.New(logging.NewMultiHandler(
			logging.NewTerminalHandler(stderr, logOpts),
			runLog.Logger().Handler(),
		)).With(slog.String("run", rc.Run.ID.String()))
	}
	prev := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(prev)

	c, err := assembleRun(cmd.Context(), rc, runSetup{
		Logger:   logger,
		Out:      stderr,
		Lock:     policy,
		Filter:   filter,
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
func runRequireTerminal() error {
	if err := requireTerminal(); err == nil {
		return nil
	}
	return fmt.Errorf("the TUI needs a terminal: stdin and stdout are not a TTY (use --%s to render the run as log lines, or --%s --json for one JSON envelope per event)",
		FlagRunHeadless, FlagRunHeadless)
}

// runSetup is what fireCannon resolved before any orchestrator part exists.
type runSetup struct {
	// Logger is the run's logger: the run log, plus the terminal in
	// headless mode.
	Logger *slog.Logger
	// Out is where the run prints its own reports (the worktree cleanup,
	// P6-18). It is stderr, so `--json`'s stdout stays parseable.
	Out io.Writer
	// Lock is the P9-04 policy the preflight already applied.
	Lock orchestrator.LockPolicy
	// Filter is the P9-05 secret filter.
	Filter *agents.SecretFilter
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

// cannon is one assembled run: every part, plus what has to be shut down
// when it ends.
type cannon struct {
	rc *config.RunConfig
	// header is the run as it was before it started, for the renderers'
	// headers. It is a copy on purpose: the run mutates rc.Run.Epic when
	// the first plan lands, and a renderer reading rc.Run while the
	// scheduler is going would race with it.
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
	// settle is told when a bead settles, so the deliverer forgets a
	// safety-on delivery the gate poller closed (P6-41).
	settle *orchestrator.Deliverer
	spend  *runSpend
	stop   []func()
}

// close shuts the assembled run's resources down, in reverse order. The
// event log is closed first, since a summary folded from it must see every
// line (P8-20).
func (c *cannon) close() {
	c.closeLog()
	for i := len(c.stop) - 1; i >= 0; i-- {
		c.stop[i]()
	}
}

// assembleRun builds every part of the run over the resolved configuration.
// It touches bd and git (registering the D-07 statuses, opening the
// repository, creating the run directory) but starts nothing.
func assembleRun(ctx context.Context, rc *config.RunConfig, set runSetup) (*cannon, error) {
	eff := rc.Effective
	cfg := eff.Config
	logger := set.Logger
	c := &cannon{rc: rc, header: rc.Run, set: set, spend: &runSpend{}}

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
	dgit := orchestrator.NewDeliveryGit(manager)

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
	gate := orchestrator.NewRulesGate(orchestrator.RulesGateOptions{
		Run: rc.Run.ID, Bus: sink, Logger: logger,
	})
	states := orchestrator.NewStateMachine(gate.GuardStore(client),
		orchestrator.WithStatePublisher(sink), orchestrator.WithStateRun(rc.Run.ID))

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
		Leases:     client,
		Cleaner:    manager,
		Resumer:    resumer,
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
	published := opts.Bus
	redactor := set.Filter.Redactor()
	opts.Bus = orchestrator.PublisherFunc(func(ev events.Event) {
		published.Publish(redactor.SafeEvent(ev))
	})

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
		Cartridges:  sel,
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
		Run:          run,
		Beads:        client,
		Statuses:     statusMap,
		Foreign:      foreign,
		Worktrees:    manager,
		Git:          dgit,
		Repo:         scan,
		Brief:        rc.Run.Brief,
		Rules:        rules,
		Vibes:        vibes,
		CheckCommand: scan.CheckCommand,
		Model:        rc.Cartridge.Model,
		Timeout:      limits.AgentTimeout.Duration(),
		Interval:     runConductorInterval,
		Logger:       logger,
	})
	if err != nil {
		return nil, err
	}
	c.cond = cond

	feedback, err := orchestrator.NewFeedback(orchestrator.FeedbackOptions{
		Context:          cond.BeadContext,
		Ratio:            cond.ChangeRatio,
		ConflictResolved: cond.ConflictResolved,
		States:           states,
		Messages:         client,
		Acceptance:       client,
		Bus:              orchestrator.PublisherFunc(run.Publish),
		Run:              rc.Run.ID,
		Logger:           logger,
		Limits:           limits,
	})
	if err != nil {
		return nil, err
	}

	budget := orchestrator.NewRunBudget(orchestrator.RunBudgetOptions{
		Limits: limits, Mode: rc.Run.Mode, Run: run, Host: run, Logger: logger,
	})

	// D-11/P8-04: external pull requests are reviewed when `gh` and a
	// GitHub remote are both there and the user has not said otherwise.
	// The reviewer is a work source and a firing planner like any other,
	// and both wrappers are no-ops when it is nil.
	reviewer, _, err := newPRReviewer(ctx, prReviewSetup{
		RC: rc, Repo: repo, Worktrees: manager, Beads: client, States: states,
		Host: run, Statuses: statusMap, Context: cond.BeadContext, Logger: logger,
	})
	if err != nil {
		return nil, err
	}
	c.prs = reviewer

	planFire = gate.GuardPlanner(c.spend.meter(budget.MeterPlanner(
		prReviewPlanner(reviewer, runFeedbackPlanner(feedback)))))

	planner, err := orchestrator.NewPlanner(orchestrator.PlannerOptions{
		Config:         rc,
		ShimOptions:    runShimOptions,
		Runner:         orchestrator.SupervisorPlanRunner(sup),
		Applier:        client,
		Beads:          cond.BeadSource(),
		Credits:        sup,
		Repo:           scan,
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

	deliverer, err := runDeliverer(run, states, dgit, client, manager, rc, logger)
	if err != nil {
		return nil, err
	}
	c.settle = deliverer

	sched = orchestrator.NewScheduler(orchestrator.SchedulerOptions{
		Allocation:          rc.Allocation,
		Work:                budget.Work(prReviewSources(reviewer, planner, feedback, deliverer)),
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
	run.UseVitals(func() orchestrator.RunVitals {
		return orchestrator.Vitals(run.Now(), planner, poller, foreign)
	})

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
	client *beads.Client, m *gitx.Manager, rc *config.RunConfig, logger *slog.Logger) (*orchestrator.Deliverer, error) {
	opts := orchestrator.DeliveryOptions{
		Host: run, States: states, Git: dgit, Beads: client,
		CheckCommand: rc.Effective.CheckCommand,
	}
	if rc.Run.Config.Safety {
		pr, err := gitx.NewPRDeliverer(m, client, gitx.WithPRLogger(logger))
		if err != nil {
			return nil, err
		}
		opts.PR = pr
	} else {
		opts.Merge = m
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

// runSpend totals what the run's agents cost. The P9-01 budget meters the
// same numbers, but only when a cap is configured; the final summary wants
// them either way, so this chains onto the same hook.
type runSpend struct {
	mu      sync.Mutex
	usage   cartridge.Usage
	firings int
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

// add records one session's usage.
func (s *runSpend) add(u cartridge.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage = s.usage.Add(u)
	s.firings++
}

// total returns the usage of every metered session and how many there were.
func (s *runSpend) total() (cartridge.Usage, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage, s.firings
}

// observeSettled hands one event to the deliverer's settler, so a bead the
// gate poller closed (or that reached any settled state) leaves nothing
// behind in the deliverer's bookkeeping (P6-41).
func (c *cannon) observeSettled(ev events.Event) {
	if c.settle == nil || ev == nil {
		return
	}
	c.settle.Observe(ev)
}

// fire starts the run and everything that runs beside it, renders it, and
// returns the process exit code (D-15's contract: 0 completed or stopped, 1
// failed, 3 credits exhausted).
func (c *cannon) fire(cmd *cobra.Command, gopts GlobalOptions, headless bool) (int, error) {
	ctx := cmd.Context()
	if err := c.run.Start(ctx); err != nil {
		return 0, err
	}
	// P9-04: record this process's start stamp on the repository's run
	// lock, so a recycled pid cannot look like a live run. Best effort:
	// the run is already going.
	if err := orchestrator.StampRunLock(config.StateDir(c.rc.Run.RepoPath), orchestrator.LockOptions{}); err != nil {
		c.set.Logger.Warn("could not stamp the run lock", slog.Any("err", err))
	}

	helpers, stopHelpers := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Add(3)
	// P6-41: the deliverer keeps one entry per bead it delivered until it
	// hears the delivery ended. A safety-on delivery ends when the gate
	// poller closes the bead, which is an event rather than a return
	// value, so the run watches its own stream for it. The subscription is
	// asynchronous on purpose: Observe takes the deliverer's own lock, and
	// the deliverer publishes while holding it.
	settling := c.bus.Subscribe(helpers, 256)
	go func() {
		defer wg.Done()
		defer settling.Unsubscribe()
		for ev := range settling.Events() {
			c.observeSettled(ev)
		}
	}()
	go func() { defer wg.Done(); _ = c.gates.Run(helpers) }()
	go func() { defer wg.Done(); _ = c.cond.Run(helpers) }()
	if c.prs != nil {
		wg.Add(1)
		go func() { defer wg.Done(); _ = c.prs.Run(helpers) }()
	}

	// The renderers end when the event stream does, so the bus is closed
	// the moment the run is over rather than when fireCannon returns. A
	// closed channel still yields what it holds, so the last events —
	// events.RunFinished among them — are rendered before the stream ends.
	go func() {
		<-c.run.Done()
		c.bus.Close()
	}()

	var (
		code int
		rerr error
	)
	if headless {
		// The renderer deliberately outlives a cancelled context: a
		// SIGINT stops the run, and the run's last events are what say so.
		code, rerr = runHeadless(context.WithoutCancel(ctx), tui.Options{
			Bus:    c.bus,
			Run:    c.header,
			Output: cmd.OutOrStdout(),
		}, tui.HeadlessOptions{
			Out:  cmd.OutOrStdout(),
			JSON: gopts.JSON,
		})
	} else {
		rerr = c.renderTUI(ctx, cmd, gopts)
	}

	out, runErr := c.run.Wait()
	stopHelpers()
	wg.Wait()
	c.closeLog()

	if !headless {
		code = tui.ExitCodeFor(out.Reason)
	}
	if err := c.writeSummary(cmd.ErrOrStderr(), out); err != nil {
		return code, err
	}
	// P8-06: the first run in a repository says where everything it wrote
	// lives. c.header is the pre-Start copy, so this cannot race the
	// planner's epic write.
	if err := ReportFirstRun(cmd.ErrOrStderr(), c.header); err != nil {
		return code, err
	}
	if rerr != nil {
		return code, rerr
	}
	return code, runErr
}

// renderTUI draws the run until it finishes or the user quits. Quitting the
// TUI stops the run: the program owns the terminal, and a cannon still
// firing with nothing watching it is not what `q` means.
func (c *cannon) renderTUI(ctx context.Context, cmd *cobra.Command, gopts GlobalOptions) error {
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-c.run.Done():
			cancel()
		case <-tctx.Done():
		}
	}()
	err := runTUI(tctx, tui.Options{
		Bus:        c.bus,
		Run:        c.header,
		NoAnim:     c.rc.NoAnim,
		NoColor:    gopts.NoColor,
		AltScreen:  true,
		Output:     cmd.OutOrStdout(),
		Controller: &runController{run: c.run},
	})
	_ = c.run.Stop("the TUI was closed")
	return err
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
			[2]string{"delivery", fmt.Sprintf("%s, %s",
				runPlural(sum.PRs, "pull request"), runPlural(sum.Merges, "merge"))},
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
	u, firings := s.total()
	if firings == 0 {
		return ""
	}
	parts := []string{fmt.Sprintf("%d tokens", u.TotalTokens())}
	if u.HasCost {
		parts = append(parts, fmt.Sprintf("$%.2f", u.CostUSD))
	}
	parts = append(parts, "over "+runPlural(firings, "session"))
	return strings.Join(parts, ", ")
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
// need a seam no component offers yet, so they say so rather than pretend
// (P8-21).
type runController struct{ run *orchestrator.Run }

var _ tui.Controller = (*runController)(nil)

// Pause implements [tui.Controller].
func (c *runController) Pause(_ context.Context, reason string) error { return c.run.Pause(reason) }

// Resume implements [tui.Controller].
func (c *runController) Resume(_ context.Context, reason string) error { return c.run.Resume(reason) }

// Stop implements [tui.Controller].
func (c *runController) Stop(_ context.Context, reason string) error { return c.run.Stop(reason) }

// Refire implements [tui.Controller].
func (c *runController) Refire(_ context.Context, bead domain.BeadID, _ string) error {
	return fmt.Errorf("re-firing %s is not wired up yet (P8-21)", bead)
}

// MarkDone implements [tui.Controller].
func (c *runController) MarkDone(_ context.Context, bead domain.BeadID, _ string) error {
	return fmt.Errorf("marking %s done is not wired up yet (P8-21)", bead)
}

// SetSafety implements [tui.Controller].
func (c *runController) SetSafety(_ context.Context, _ bool) error {
	return errors.New("changing safety mid-run is not wired up yet (P8-21)")
}

// SetAmmo implements [tui.Controller].
func (c *runController) SetAmmo(_ context.Context, _ int) error {
	return errors.New("changing ammo mid-run is not wired up yet (P8-21)")
}
