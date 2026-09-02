package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"slop-cannon/internal/config"
)

// SafetyResult is the --json shape of `sc safety`: the value, what it means
// (D-10), and the config file it was written to (Path is the writing file;
// when the command only reported the effective value, Source says which
// layer supplied it and Path is that layer's file, empty for the default).
type SafetyResult struct {
	// Safety is the value: true means PRs, false means straight to the
	// default branch.
	Safety bool `json:"safety"`
	// Meaning is the D-10 description of the value.
	Meaning string `json:"meaning"`
	// Scope is the config file that holds the value ("global" or "repo");
	// empty when the effective value is the built-in default.
	Scope config.Scope `json:"scope,omitempty"`
	// Path is the config file that was written, or the file that supplied
	// the effective value when reporting only; empty for the default.
	Path string `json:"path,omitempty"`
	// Source is where the effective value came from when reporting only.
	Source config.Source `json:"source,omitempty"`
}

// Safety meanings (D-10), printed whenever the value is shown or changed.
const (
	// SafetyOnMeaning describes safety = true.
	SafetyOnMeaning = "finished beads are pushed to an sc/<bead-id>-<slug> branch and opened as a pull request for a human to merge; the default branch is never touched"
	// SafetyOffMeaning describes safety = false.
	SafetyOffMeaning = "finished beads are rebased onto the default branch, the check command is run, then they are fast-forward merged and pushed when a remote exists"
)

// SafetyMeaning returns the D-10 description of the safety value.
func SafetyMeaning(on bool) string {
	if on {
		return SafetyOnMeaning
	}
	return SafetyOffMeaning
}

// safetyWord renders the value as the on/off word the command accepts.
func safetyWord(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

const safetyLong = `Set the safety switch (D-10).

  on   finished beads are pushed to an sc/<bead-id>-<slug> branch and opened
       as a pull request for a human to merge; the default branch is never
       touched. This is the default.
  off  finished beads are rebased onto the default branch, the check command
       is run, then they are fast-forward merged and pushed when a remote
       exists.

true/false, yes/no and 1/0 are accepted as well. The value is written to the
repo config when --repo (default: the current directory) is inside a git
repository, otherwise to the global config; --global always writes the
global config.

Without a value the effective setting and where it comes from is printed
instead.`

func init() { register(newSafetyCommand) }

func newSafetyCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "safety [on|off]",
		Short: "Open PRs (on, the default) or merge straight to the default branch (off)",
		Long:  safetyLong,
		Example: indent(`sc safety off
sc --global safety on
sc safety`, "  "),
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE:      runSafety,
	}
}

// runSafety implements `sc safety [on|off]`.
func runSafety(cmd *cobra.Command, args []string) error {
	opts, err := GlobalOpts(cmd)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return showSafety(cmd, opts)
	}
	on, err := config.ParseSafety(args[0])
	if err != nil {
		return err
	}
	doc, err := configOpenTarget(opts)
	if err != nil {
		return err
	}
	doc.SetSafety(on)
	if err := doc.Save(); err != nil {
		return err
	}
	res := SafetyResult{Safety: on, Meaning: SafetyMeaning(on), Scope: doc.Scope, Path: doc.Path}
	out := cmd.OutOrStdout()
	if opts.JSON {
		return writeJSON(out, res)
	}
	_, err = fmt.Fprintf(out, "safety %s: %s\nsaved to %s\n", safetyWord(on), res.Meaning, doc.Path)
	return err
}

// showSafety prints the effective safety value and its source without
// writing.
func showSafety(cmd *cobra.Command, opts GlobalOptions) error {
	eff, err := config.Load(opts.Repo)
	if err != nil {
		return err
	}
	src := eff.Sources[config.KeySafety]
	if src == "" {
		src = config.SourceDefault
	}
	res := SafetyResult{Safety: eff.Safety, Meaning: SafetyMeaning(eff.Safety), Source: src}
	switch src {
	case config.SourceRepo:
		res.Scope, res.Path = config.ScopeRepo, eff.RepoPath
	case config.SourceGlobal:
		res.Scope, res.Path = config.ScopeGlobal, eff.GlobalPath
	}
	out := cmd.OutOrStdout()
	if opts.JSON {
		return writeJSON(out, res)
	}
	_, err = fmt.Fprintf(out, "safety %s (from %s): %s\n", safetyWord(eff.Safety), src, res.Meaning)
	return err
}
