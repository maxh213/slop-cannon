package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"slop-cannon/internal/config"
)

// ConfigShow is the --json shape of `sc config show`: the merged
// configuration with per-key sources and file paths (config.Effective) plus
// the same values as display rows.
type ConfigShow struct {
	config.Effective
	// Entries are the rows of the text table, in display order.
	Entries []config.Entry `json:"entries"`
}

// ConfigPaths is the --json shape of `sc config path`.
type ConfigPaths struct {
	// GlobalPath is the global config file.
	GlobalPath string `json:"global_path"`
	// GlobalExists reports whether the global file is present.
	GlobalExists bool `json:"global_exists"`
	// RepoRoot is the repository whose config is consulted: the git root
	// containing --repo, or --repo itself when it is not inside one.
	RepoRoot string `json:"repo_root"`
	// RepoPath is the repo config file.
	RepoPath string `json:"repo_path"`
	// RepoExists reports whether the repo file is present.
	RepoExists bool `json:"repo_exists"`
	// InGitRepo reports whether --repo is inside a git repository, which is
	// what decides where config subcommands write (D-03).
	InGitRepo bool `json:"in_git_repo"`
	// Target is the scope config subcommands write to for this invocation.
	Target config.Scope `json:"target"`
	// TargetPath is the file config subcommands write to for this invocation.
	TargetPath string `json:"target_path"`
}

// ConfigEdit is the --json shape of `sc config edit`.
type ConfigEdit struct {
	// Scope is the config file that was edited ("global" or "repo").
	Scope config.Scope `json:"scope"`
	// Path is the file that was opened in the editor.
	Path string `json:"path"`
	// Created reports whether the file was created before editing.
	Created bool `json:"created"`
	// Editor is the command line the file was opened with.
	Editor string `json:"editor"`
}

const configLong = `Inspect and edit the two configuration files.

sc reads the global config ($XDG_CONFIG_HOME/slop-cannon/config.toml, by
default ~/.config/slop-cannon/config.toml) and the repo config
(<repo>/.slop-cannon/config.toml) and merges them over the built-in
defaults: flags beat the repo file, which beats the global file (D-03).
Cartridges live in the global file only.

Config subcommands write the repo file when --repo (default: the current
directory) is inside a git repository, otherwise the global file; --global
always picks the global file.`

const configShowLong = `Print the effective configuration: every key, its merged value, and the
layer that supplied it (default, global, repo, global+repo for the vibes and
rules lists that are unioned across files). The files consulted are listed
after the table; warnings such as cartridges found in a repo file are printed
to stderr. With --json the merged config, the sources, the file paths, the
warnings and the rows are printed as one object.`

const configPathLong = `Print the global and repo config file paths, whether each exists, and which
one config subcommands write to for this invocation (D-03: the repo file
when --repo is inside a git repository, otherwise the global file; --global
forces the global file).`

const configEditLong = `Open the config file that config subcommands would write to (see sc config
path) in $EDITOR, falling back to $VISUAL. The file is created with the
current schema version when it does not exist yet. After the editor exits
the file is parsed and the merged configuration is validated, so a typo or
an invalid value is reported immediately with its line number; the file is
left as edited.`

func init() { register(newConfigCommand) }

func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show, locate, or edit the configuration files",
		Long:  configLong,
		Example: indent(`sc config show
sc --json config show
sc config path
sc --global config edit`, "  "),
		// A RunE is needed for cobra to validate the arguments: a bare
		// `sc config` prints help, `sc config shwo` is an unknown command.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "show",
			Short: "Print the merged configuration with the source of every value",
			Long:  configShowLong,
			Args:  cobra.NoArgs,
			RunE:  runConfigShow,
		},
		&cobra.Command{
			Use:   "path",
			Short: "Print the global and repo config file paths",
			Long:  configPathLong,
			Args:  cobra.NoArgs,
			RunE:  runConfigPath,
		},
		&cobra.Command{
			Use:   "edit",
			Short: "Open the config file in $EDITOR",
			Long:  configEditLong,
			Args:  cobra.NoArgs,
			RunE:  runConfigEdit,
		},
	)
	return cmd
}

// configRepoRoot resolves the repository root for repo (an absolute
// directory from GlobalOpts) the way config.Load does: the enclosing git
// root when there is one, else repo itself. inRepo is false in the latter
// case.
func configRepoRoot(repo string) (root string, inRepo bool) {
	if r, ok := config.FindRepoRoot(repo); ok {
		return r, true
	}
	return repo, false
}

// configOpenTarget opens the config file that config subcommands write to
// under D-03: the repo file when --repo is inside a git repository and
// --global is not given, else the global file.
func configOpenTarget(opts GlobalOptions) (*config.Doc, error) {
	root, inRepo := config.FindRepoRoot(opts.Repo)
	return config.Open(config.Target(inRepo, opts.Global), root)
}

// runConfigShow implements `sc config show`.
func runConfigShow(cmd *cobra.Command, _ []string) error {
	opts, err := GlobalOpts(cmd)
	if err != nil {
		return err
	}
	eff, err := config.Load(opts.Repo)
	if err != nil {
		return err
	}
	if opts.JSON {
		return writeJSON(cmd.OutOrStdout(), ConfigShow{Effective: *eff, Entries: eff.Entries()})
	}
	return configShowText(cmd.OutOrStdout(), cmd.ErrOrStderr(), eff)
}

// configShowText prints the key/value/source table, the files consulted,
// and any warnings (to errw).
func configShowText(w, errw io.Writer, eff *config.Effective) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, e := range eff.Entries() {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Key, e.Value, e.Source)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "global config  %s%s\n", eff.GlobalPath, configExistsNote(eff.GlobalExists))
	if eff.RepoPath != "" {
		fmt.Fprintf(w, "repo config    %s%s\n", eff.RepoPath, configExistsNote(eff.RepoExists))
	}
	for _, warn := range eff.Warnings {
		fmt.Fprintf(errw, "warning: %s\n", warn)
	}
	return nil
}

// configExistsNote annotates a path that is not present.
func configExistsNote(exists bool) string {
	if exists {
		return ""
	}
	return " (not found)"
}

// configPaths resolves both config file paths and the D-03 write target
// for opts without reading either file's content.
func configPaths(opts GlobalOptions) (ConfigPaths, error) {
	root, inRepo := configRepoRoot(opts.Repo)
	p := ConfigPaths{
		GlobalPath: config.GlobalPath(),
		RepoRoot:   root,
		RepoPath:   config.RepoConfigPath(root),
		InGitRepo:  inRepo,
		Target:     config.Target(inRepo, opts.Global),
	}
	p.TargetPath = config.PathFor(p.Target, root)
	var err error
	if p.GlobalExists, err = configFileExists(p.GlobalPath); err != nil {
		return p, err
	}
	if p.RepoExists, err = configFileExists(p.RepoPath); err != nil {
		return p, err
	}
	return p, nil
}

// configFileExists reports whether path exists; errors other than
// "not found" are returned.
func configFileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// runConfigPath implements `sc config path`.
func runConfigPath(cmd *cobra.Command, _ []string) error {
	opts, err := GlobalOpts(cmd)
	if err != nil {
		return err
	}
	p, err := configPaths(opts)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if opts.JSON {
		return writeJSON(out, p)
	}
	repoNote := configExistsNote(p.RepoExists)
	if !p.InGitRepo {
		repoNote += " (" + p.RepoRoot + " is not inside a git repository)"
	}
	_, err = fmt.Fprintf(out, "global  %s%s\nrepo    %s%s\nwrites  %s (%s)\n",
		p.GlobalPath, configExistsNote(p.GlobalExists), p.RepoPath, repoNote, p.TargetPath, p.Target)
	return err
}

// configEditor returns the editor command line: $EDITOR, else $VISUAL,
// else "".
func configEditor() string {
	for _, v := range []string{"EDITOR", "VISUAL"} {
		if e := strings.TrimSpace(os.Getenv(v)); e != "" {
			return e
		}
	}
	return ""
}

// runConfigEdit implements `sc config edit`.
func runConfigEdit(cmd *cobra.Command, _ []string) error {
	opts, err := GlobalOpts(cmd)
	if err != nil {
		return err
	}
	editor := configEditor()
	if editor == "" {
		return fmt.Errorf("no editor configured: set $EDITOR (for example: export EDITOR=vim)")
	}
	// The file is located but not parsed before the editor runs, so a
	// broken file can be opened to repair it.
	root, inRepo := config.FindRepoRoot(opts.Repo)
	doc := &config.Doc{Scope: config.Target(inRepo, opts.Global)}
	doc.Path = config.PathFor(doc.Scope, root)
	exists, err := configFileExists(doc.Path)
	if err != nil {
		return err
	}
	res := ConfigEdit{Scope: doc.Scope, Path: doc.Path, Editor: editor}
	if !exists {
		if err := doc.Save(); err != nil {
			return err
		}
		res.Created = true
	}

	fields := strings.Fields(editor)
	ed := exec.CommandContext(cmd.Context(), fields[0], append(fields[1:], doc.Path)...)
	ed.Stdin = os.Stdin
	ed.Stdout = cmd.OutOrStdout()
	ed.Stderr = cmd.ErrOrStderr()
	if err := ed.Run(); err != nil {
		return fmt.Errorf("editor %q: %w", editor, err)
	}

	// Report problems with the edited file right away: first the file on
	// its own (syntax, unknown keys), then the merge it takes part in
	// (value rules such as the ammo minimum).
	if _, _, err := config.ReadFile(doc.Path); err != nil {
		return err
	}
	loadPath := opts.Repo
	if doc.Scope == config.ScopeGlobal {
		loadPath = ""
	}
	if _, err := config.Load(loadPath); err != nil {
		return err
	}
	if opts.JSON {
		return writeJSON(cmd.OutOrStdout(), res)
	}
	return nil
}
