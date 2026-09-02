package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"slop-cannon/internal/config"
)

// Flag names shared by `sc vibes` and `sc rules`.
const (
	flagPromptAdd    = "add"
	flagPromptRemove = "remove"
	flagPromptList   = "list"
	flagPromptShow   = "show"
)

// PromptFile is one entry of a vibes or rules list as reported by --list,
// --show and --json.
type PromptFile struct {
	// Path is the absolute path of the file.
	Path string `json:"path"`
	// Stored is how the path is written in the config file (relative to
	// the config directory when the file lives under the repo, else absolute).
	Stored string `json:"stored"`
	// Source is the config file the entry comes from ("global" or "repo").
	Source config.Source `json:"source"`
	// Exists reports whether the file is present on disk right now.
	Exists bool `json:"exists"`
	// Content is the file's text; filled in by --show only.
	Content string `json:"content,omitempty"`
}

// PromptResult is the --json shape of `sc vibes` and `sc rules`.
type PromptResult struct {
	// Kind is "vibes" or "rules".
	Kind string `json:"kind"`
	// Scope is the config file that --add/--remove wrote ("global" or
	// "repo"); empty for --list and --show.
	Scope config.Scope `json:"scope,omitempty"`
	// Path is the config file that was written; empty when nothing was written.
	Path string `json:"path,omitempty"`
	// Added lists the stored form of every path --add appended.
	Added []string `json:"added,omitempty"`
	// Unchanged lists --add paths that were already configured.
	Unchanged []string `json:"unchanged,omitempty"`
	// Removed lists the paths --remove dropped, as given on the command line.
	Removed []string `json:"removed,omitempty"`
	// Files is the resulting list: the edited file's entries after a write,
	// the effective (global then repo) list for --list and --show.
	Files []PromptFile `json:"files"`
}

// promptKind describes one of the two prompt-file lists so that vibes.go and
// rules.go share a single implementation.
type promptKind struct {
	// name is the list's key and command name: "vibes" or "rules".
	name string
	// short is the one-line command description.
	short string
	// long explains what the files are for; the shared usage text follows it.
	long string
	// add, remove and paths bind the Doc helpers of this list.
	add    func(*config.Doc, string) (stored string, err error)
	remove func(*config.Doc, string) error
	paths  func(*config.Doc) []string
	stored func(*config.Doc) []string
}

// promptVibes is the vibes list (README: "stylistic choices which get
// communicated to all agents").
var promptVibes = promptKind{
	name:  "vibes",
	short: "Manage the style files every agent is given",
	long: `Vibes are files of stylistic preferences (tone, naming, formatting, the
libraries you like) that are appended to every agent prompt as soft
guidance. Rules (sc rules) are the hard constraints; vibes are taste.`,
	add:    (*config.Doc).AddVibes,
	remove: (*config.Doc).RemoveVibes,
	paths:  (*config.Doc).VibesPaths,
	stored: func(d *config.Doc) []string { return d.File.Vibes },
}

// promptUsage is the part of the Long help that is the same for vibes and
// rules; %[1]s is the list name.
const promptUsage = `--add FILE appends FILE (resolved from the current directory) to the %[1]s
list of the repo config when --repo (default: the current directory) is
inside a git repository, otherwise of the global config; --global always
writes the global config. The file must exist, be UTF-8 text, and be at
most 64 KiB. Paths are stored relative to the config file's directory when
the file lives inside that tree, otherwise absolute, and a file listed
twice is kept once. --remove FILE drops it again from the same config file;
removing a file that is not listed is an error.

--list prints the effective %[1]s list (global entries first, then repo
entries, in the order the agents see them) with where each entry comes from;
--show prints the concatenated content of those files. Both honour --global
by looking at the global config only. With no flags, --list is assumed.`

func init() { register(newVibesCommand) }

// newVibesCommand builds `sc vibes`.
func newVibesCommand() *cobra.Command { return newPromptCommand(promptVibes) }

// newPromptCommand builds the `sc vibes` / `sc rules` command for kind.
func newPromptCommand(kind promptKind) *cobra.Command {
	cmd := &cobra.Command{
		Use:   kind.name + " [--add FILE | --remove FILE | --list | --show]",
		Short: kind.short,
		Long:  kind.long + "\n\n" + fmt.Sprintf(promptUsage, kind.name),
		Example: indent(fmt.Sprintf(`sc %[1]s --add %[1]s.md
sc --global %[1]s --add ~/%[1]s.md
sc %[1]s --list
sc %[1]s --show
sc %[1]s --remove %[1]s.md`, kind.name), "  "),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runPrompt(cmd, kind) },
	}
	f := cmd.Flags()
	f.StringArray(flagPromptAdd, nil, "add `FILE` to the "+kind.name+" list (repeatable)")
	f.StringArray(flagPromptRemove, nil, "remove `FILE` from the "+kind.name+" list (repeatable)")
	f.Bool(flagPromptList, false, "print the effective "+kind.name+" files and their sources")
	f.Bool(flagPromptShow, false, "print the concatenated content of the "+kind.name+" files")
	cmd.MarkFlagsMutuallyExclusive(flagPromptAdd, flagPromptRemove, flagPromptList, flagPromptShow)
	return cmd
}

// runPrompt dispatches `sc vibes` / `sc rules` to the write or read path.
func runPrompt(cmd *cobra.Command, kind promptKind) error {
	opts, err := GlobalOpts(cmd)
	if err != nil {
		return err
	}
	f := cmd.Flags()
	add, err := f.GetStringArray(flagPromptAdd)
	if err != nil {
		return err
	}
	remove, err := f.GetStringArray(flagPromptRemove)
	if err != nil {
		return err
	}
	show, err := f.GetBool(flagPromptShow)
	if err != nil {
		return err
	}
	// Changed, not len: `--add ""` must be rejected, not treated as --list.
	switch {
	case f.Changed(flagPromptAdd):
		return promptAdd(cmd, kind, opts, add)
	case f.Changed(flagPromptRemove):
		return promptRemove(cmd, kind, opts, remove)
	default:
		return promptReport(cmd, kind, opts, show)
	}
}

// promptFiles rejects an empty FILE argument to --add / --remove.
func promptFiles(flag string, files []string) error {
	if len(files) == 0 {
		return fmt.Errorf("--%s: file name must not be empty", flag)
	}
	for _, file := range files {
		if strings.TrimSpace(file) == "" {
			return fmt.Errorf("--%s: file name must not be empty", flag)
		}
	}
	return nil
}

// promptOpenTarget opens the config file a write goes to (D-03).
func promptOpenTarget(opts GlobalOptions) (*config.Doc, error) {
	root, inRepo := config.FindRepoRoot(opts.Repo)
	return config.Open(config.Target(inRepo, opts.Global), root)
}

// promptAdd implements --add: every file is validated before anything is
// written, so a bad file leaves the config untouched.
func promptAdd(cmd *cobra.Command, kind promptKind, opts GlobalOptions, files []string) error {
	if err := promptFiles(flagPromptAdd, files); err != nil {
		return err
	}
	doc, err := promptOpenTarget(opts)
	if err != nil {
		return err
	}
	res := PromptResult{Kind: kind.name, Scope: doc.Scope, Path: doc.Path}
	var lines []string
	for _, file := range files {
		abs, err := filepath.Abs(file)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		if containsPath(kind.paths(doc), abs) {
			res.Unchanged = append(res.Unchanged, file)
			lines = append(lines, fmt.Sprintf("%s is already in %s (%s); nothing added", file, kind.name, doc.Path))
			continue
		}
		stored, err := kind.add(doc, file)
		if err != nil {
			return err
		}
		res.Added = append(res.Added, stored)
		lines = append(lines, fmt.Sprintf("added %s to %s as %q", file, kind.name, stored))
	}
	if len(res.Added) > 0 {
		if err := doc.Save(); err != nil {
			return err
		}
		lines = append(lines, "saved to "+doc.Path)
	}
	res.Files = promptEntriesOf(kind, doc)
	return promptWrite(cmd, opts, res, lines)
}

// promptRemove implements --remove. A path that is not listed is an error
// and nothing is written.
func promptRemove(cmd *cobra.Command, kind promptKind, opts GlobalOptions, files []string) error {
	if err := promptFiles(flagPromptRemove, files); err != nil {
		return err
	}
	doc, err := promptOpenTarget(opts)
	if err != nil {
		return err
	}
	res := PromptResult{Kind: kind.name, Scope: doc.Scope, Path: doc.Path}
	var lines []string
	for _, file := range files {
		if err := kind.remove(doc, file); err != nil {
			return err
		}
		res.Removed = append(res.Removed, file)
		lines = append(lines, fmt.Sprintf("removed %s from %s", file, kind.name))
	}
	if err := doc.Save(); err != nil {
		return err
	}
	lines = append(lines, "saved to "+doc.Path)
	res.Files = promptEntriesOf(kind, doc)
	return promptWrite(cmd, opts, res, lines)
}

// promptWrite prints res as JSON or lines as text.
func promptWrite(cmd *cobra.Command, opts GlobalOptions, res PromptResult, lines []string) error {
	out := cmd.OutOrStdout()
	if opts.JSON {
		return writeJSON(out, res)
	}
	for _, l := range lines {
		if _, err := fmt.Fprintln(out, l); err != nil {
			return err
		}
	}
	return nil
}

// promptReport implements --list (and --show when show is true): the
// effective list is the global file's entries followed by the repo file's,
// deduplicated by absolute path, exactly as config.Load merges them.
// --global restricts it to the global file.
func promptReport(cmd *cobra.Command, kind promptKind, opts GlobalOptions, show bool) error {
	files, err := promptEffective(kind, opts)
	if err != nil {
		return err
	}
	if show {
		for i := range files {
			data, err := os.ReadFile(files[i].Path)
			if err != nil {
				return fmt.Errorf("%s: %s file %w", kind.name, files[i].Source, err)
			}
			files[i].Content = string(data)
		}
	}
	res := PromptResult{Kind: kind.name, Files: files}
	out := cmd.OutOrStdout()
	if opts.JSON {
		return writeJSON(out, res)
	}
	if show {
		return promptShowText(out, files)
	}
	return promptListText(out, kind, opts, files)
}

// promptEffective collects the entries of every config file that applies.
func promptEffective(kind promptKind, opts GlobalOptions) ([]PromptFile, error) {
	global, err := config.OpenGlobal()
	if err != nil {
		return nil, err
	}
	files := promptEntriesOf(kind, global)
	if opts.Global {
		return files, nil
	}
	root, inRepo := config.FindRepoRoot(opts.Repo)
	if !inRepo {
		return files, nil
	}
	repo, err := config.OpenRepo(root)
	if err != nil {
		return nil, err
	}
	for _, e := range promptEntriesOf(kind, repo) {
		if !containsPromptPath(files, e.Path) {
			files = append(files, e)
		}
	}
	return files, nil
}

// promptEntriesOf lists kind's entries of doc with their absolute paths,
// stored form, and whether each file is on disk.
func promptEntriesOf(kind promptKind, doc *config.Doc) []PromptFile {
	stored := kind.stored(doc)
	abs := kind.paths(doc)
	files := make([]PromptFile, 0, len(stored))
	for i, s := range stored {
		info, err := os.Stat(abs[i])
		files = append(files, PromptFile{
			Path:   abs[i],
			Stored: s,
			Source: config.Source(doc.Scope),
			Exists: err == nil && info.Mode().IsRegular(),
		})
	}
	return files
}

// promptListText renders --list as a table: path, source, and a missing
// marker for files that are no longer on disk.
func promptListText(w io.Writer, kind promptKind, opts GlobalOptions, files []PromptFile) error {
	if len(files) == 0 {
		where := "no " + kind.name + " configured"
		if opts.Global {
			where += " in the global config"
		}
		_, err := fmt.Fprintf(w, "%s; add one with: sc %s --add FILE\n", where, kind.name)
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, f := range files {
		note := ""
		if !f.Exists {
			note = "\t(missing)"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s%s\n", f.Path, f.Source, note); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// promptShowText writes the files' content one after another. Every file
// ends with a newline and files are separated by a blank line so the
// output reads as one document.
func promptShowText(w io.Writer, files []PromptFile) error {
	for i, f := range files {
		if i > 0 {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, f.Content); err != nil {
			return err
		}
		if f.Content != "" && !strings.HasSuffix(f.Content, "\n") {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
	}
	return nil
}

// containsPath reports whether abs is in paths.
func containsPath(paths []string, abs string) bool {
	for _, p := range paths {
		if p == abs {
			return true
		}
	}
	return false
}

// containsPromptPath reports whether an entry with path abs is in files.
func containsPromptPath(files []PromptFile, abs string) bool {
	for _, f := range files {
		if f.Path == abs {
			return true
		}
	}
	return false
}
