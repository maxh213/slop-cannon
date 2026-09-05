package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// Names of the global (persistent) flags, shared by every subcommand.
const (
	FlagRepo    = "repo"
	FlagGlobal  = "global"
	FlagVerbose = "verbose"
	FlagJSON    = "json"
	FlagNoColor = "no-color"
)

// GlobalOptions is the resolved value of the global flags for one invocation.
type GlobalOptions struct {
	// Repo is the absolute path of the target repository (--repo, default cwd).
	Repo string
	// Global forces config commands to read and write the global config
	// instead of the repo config (--global, D-03).
	Global bool
	// Verbose raises the log level to debug (--verbose / -v).
	Verbose bool
	// JSON asks commands to print machine-readable JSON instead of text (--json).
	JSON bool
	// NoColor disables ANSI colour; also set by the NO_COLOR environment
	// variable (https://no-color.org).
	NoColor bool
}

// addGlobalFlags defines the persistent flags on the root command.
func addGlobalFlags(root *cobra.Command) {
	pf := root.PersistentFlags()
	pf.String(FlagRepo, ".", "target repository `PATH`")
	pf.Bool(FlagGlobal, false, "read and write the global config only, not the repo config")
	pf.BoolP(FlagVerbose, "v", false, "enable debug logging")
	pf.Bool(FlagJSON, false, "print machine-readable JSON instead of text")
	pf.Bool(FlagNoColor, false, "disable colour output (also honours NO_COLOR)")
}

// GlobalOpts resolves the global flags for cmd (or any command in its tree).
// --repo is made absolute and must name an existing directory; the git-repo
// check belongs to the commands that need one.
func GlobalOpts(cmd *cobra.Command) (GlobalOptions, error) {
	pf := cmd.Root().PersistentFlags()
	var opts GlobalOptions
	var err error
	if opts.Repo, err = pf.GetString(FlagRepo); err != nil {
		return opts, err
	}
	if opts.Global, err = pf.GetBool(FlagGlobal); err != nil {
		return opts, err
	}
	if opts.Verbose, err = pf.GetBool(FlagVerbose); err != nil {
		return opts, err
	}
	if opts.JSON, err = pf.GetBool(FlagJSON); err != nil {
		return opts, err
	}
	if opts.NoColor, err = pf.GetBool(FlagNoColor); err != nil {
		return opts, err
	}
	if _, set := os.LookupEnv("NO_COLOR"); set {
		opts.NoColor = true
	}
	if opts.Repo, err = resolveRepo(opts.Repo); err != nil {
		return opts, err
	}
	return opts, nil
}

// resolveRepo turns the --repo value into an absolute path to an existing
// directory. An empty value means the current directory.
func resolveRepo(repo string) (string, error) {
	if repo == "" {
		repo = "."
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return "", fmt.Errorf("--%s %q: %w", FlagRepo, repo, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("--%s %q: %w", FlagRepo, repo, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("--%s %q: not a directory; the flag takes the repository directory, not a file inside it: --%s /path/to/repo", FlagRepo, repo, FlagRepo)
	}
	return abs, nil
}
