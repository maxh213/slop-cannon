package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/orchestrator"
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

// runNote is printed on stderr after the resolved run until the
// orchestrator is wired in (P8-01).
const runNote = "note: the orchestrator is not wired yet (P8-01); the run was resolved but nothing was started"

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
// the run and prints it. Starting the orchestrator lands in P8-01.
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
	rc, notes, err := ResolveRunWithNotes(ro)
	if err != nil {
		return err
	}
	for _, n := range notes {
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "note: "+n); err != nil {
			return err
		}
	}
	out := cmd.OutOrStdout()
	if opts.JSON {
		err = writeJSON(out, rc)
	} else {
		err = runWriteText(out, rc)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.ErrOrStderr(), runNote)
	return err
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
