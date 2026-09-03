package prompt

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"slop-cannon/internal/gitx"
)

// Repository-context defaults (P5-01).
const (
	// DefaultTreeDepth is how deep the tree summary goes: files directly in
	// the root are depth 1.
	DefaultTreeDepth = 3
	// DefaultTreeEntries caps the tree summary.
	DefaultTreeEntries = 400
	// DefaultReadmeBytes is how much of the README is quoted.
	DefaultReadmeBytes = 4096
)

// skippedDirs are directory names the tree summary never descends into:
// version control, dependency and build output, editor state, and
// slop-cannon's own bookkeeping. They tell an agent nothing about the
// project and would swallow the entry budget.
var skippedDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	".beads": true, ".slop-cannon": true,
	"node_modules": true, "vendor": true, "target": true, "dist": true, "build": true,
	".venv": true, "venv": true, "__pycache__": true, ".tox": true, ".mypy_cache": true,
	".pytest_cache": true, ".gradle": true, ".idea": true, ".vscode": true,
	".next": true, ".nuxt": true, ".cache": true, ".terraform": true,
}

// readmeNames are the README files looked for, in preference order.
var readmeNames = []string{"README.md", "README.MD", "readme.md", "README", "README.rst", "README.txt"}

// stackSignature maps a marker file to the stack it implies.
type stackSignature struct {
	name  string
	files []string
}

// stackSignatures are checked in order; every match is reported, so a Go
// service with a Dockerfile is described as both.
var stackSignatures = []stackSignature{
	{"Go", []string{"go.mod", "go.work"}},
	{"Rust", []string{"Cargo.toml"}},
	{"TypeScript", []string{"tsconfig.json"}},
	{"Node.js", []string{"package.json"}},
	{"Python", []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt"}},
	{"Ruby", []string{"Gemfile"}},
	{"PHP", []string{"composer.json"}},
	{"Java/Kotlin (Maven)", []string{"pom.xml"}},
	{"Java/Kotlin (Gradle)", []string{"build.gradle", "build.gradle.kts"}},
	{"C/C++ (CMake)", []string{"CMakeLists.txt"}},
	{"Make", []string{"Makefile", "makefile", "GNUmakefile"}},
	{"Docker", []string{"Dockerfile", "docker-compose.yml", "compose.yaml"}},
}

// RepoContext is what an agent is told about the repository it was fired at.
// Build renders it; ScanRepo fills it from the file system.
type RepoContext struct {
	// Root is the absolute path of the repository (or worktree) the agent
	// works in.
	Root string
	// DefaultBranch is the branch work lands on (gitx.Repo.DefaultBranch);
	// empty when the caller did not resolve it.
	DefaultBranch string
	// Stack lists the technologies detected from marker files, most
	// specific first.
	Stack []string
	// CheckCommand is the command that must pass before work is delivered
	// (config check_command or gitx.DetectCheckCommand); empty when there is
	// none.
	CheckCommand string
	// CheckCommandDetected is true when CheckCommand was autodetected rather
	// than configured, so the prompt can say so.
	CheckCommandDetected bool
	// Tree holds repository-relative paths, sorted, directories with a
	// trailing "/". It is already capped at ScanOptions.MaxEntries.
	Tree []string
	// TreeTotal is how many entries the walk found within the depth limit,
	// so a truncated tree can say "400 of 1832".
	TreeTotal int
	// TreeDepth is the depth the walk used.
	TreeDepth int
	// ReadmeName is the README file quoted in ReadmeHead, if any.
	ReadmeName string
	// ReadmeHead is the first ScanOptions.ReadmeBytes of the README.
	ReadmeHead string
	// ReadmeTruncated is true when the README was longer than that.
	ReadmeTruncated bool
}

// empty reports whether there is nothing to say about the repository, in
// which case the whole section is left out rather than rendered blank.
func (rc RepoContext) empty() bool {
	return rc.Root == "" && rc.DefaultBranch == "" && rc.CheckCommand == "" &&
		len(rc.Stack) == 0 && len(rc.Tree) == 0 && rc.ReadmeHead == ""
}

// ScanOptions tunes ScanRepo. The zero value means the defaults.
type ScanOptions struct {
	// Depth is the tree depth; 0 means DefaultTreeDepth, negative means
	// unlimited.
	Depth int
	// MaxEntries caps the tree; 0 means DefaultTreeEntries, negative means
	// unlimited.
	MaxEntries int
	// ReadmeBytes bounds the quoted README; 0 means DefaultReadmeBytes,
	// negative means "do not read the README".
	ReadmeBytes int
	// DefaultBranch is copied into the result; ScanRepo runs no git commands.
	DefaultBranch string
	// CheckCommand overrides detection (config check_command). Empty means
	// gitx.DetectCheckCommand decides.
	CheckCommand string
}

// withDefaults resolves the zero values.
func (o ScanOptions) withDefaults() ScanOptions {
	if o.Depth == 0 {
		o.Depth = DefaultTreeDepth
	}
	if o.MaxEntries == 0 {
		o.MaxEntries = DefaultTreeEntries
	}
	if o.ReadmeBytes == 0 {
		o.ReadmeBytes = DefaultReadmeBytes
	}
	return o
}

// ScanRepo describes the repository at dir: its tree (depth 3, capped at 400
// entries by default), the head of its README, the stack its marker files
// imply, and the check command. It only reads the file system — no git
// binary, no network — so the caller supplies the default branch it already
// resolved with gitx. A missing README or an unreadable subdirectory is not
// an error; a dir that does not exist is.
func ScanRepo(dir string, opts ScanOptions) (RepoContext, error) {
	opts = opts.withDefaults()
	abs, err := filepath.Abs(dir)
	if err != nil {
		return RepoContext{}, fmt.Errorf("prompt: repository path %q: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return RepoContext{}, fmt.Errorf("prompt: scanning repository: %w", err)
	}
	if !info.IsDir() {
		return RepoContext{}, fmt.Errorf("prompt: scanning repository: %s is not a directory", abs)
	}

	rc := RepoContext{
		Root:          abs,
		DefaultBranch: opts.DefaultBranch,
		TreeDepth:     opts.Depth,
		Stack:         detectStack(abs),
		CheckCommand:  strings.TrimSpace(opts.CheckCommand),
	}
	if rc.CheckCommand == "" {
		if cmd, ok := gitx.DetectCheckCommand(abs); ok {
			rc.CheckCommand, rc.CheckCommandDetected = cmd, true
		}
	}
	rc.Tree, rc.TreeTotal = scanTree(abs, opts.Depth, opts.MaxEntries)
	if opts.ReadmeBytes > 0 {
		rc.ReadmeName, rc.ReadmeHead, rc.ReadmeTruncated = readReadme(abs, opts.ReadmeBytes)
	}
	return rc, nil
}

// detectStack returns the technologies whose marker files are in the
// repository root, in stackSignatures order.
func detectStack(root string) []string {
	var out []string
	for _, sig := range stackSignatures {
		for _, name := range sig.files {
			if info, err := os.Stat(filepath.Join(root, name)); err == nil && !info.IsDir() {
				out = append(out, sig.name)
				break
			}
		}
	}
	return out
}

// scanTree walks root to depth and returns the first max entries in sorted
// order together with the total number of entries within the depth limit.
// A depth or max below zero means unlimited.
func scanTree(root string, depth, max int) (entries []string, total int) {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is skipped, not fatal: the tree is a
			// summary, not an inventory.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		level := strings.Count(rel, "/") + 1
		if d.IsDir() && skippedDirs[d.Name()] {
			return fs.SkipDir
		}
		if depth >= 0 && level > depth {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			rel += "/"
		} else if !d.Type().IsRegular() {
			// Symlinks and devices say nothing useful and are never followed.
			return nil
		}
		total++
		if max < 0 || len(entries) < max {
			entries = append(entries, rel)
		}
		return nil
	})
	// A walk error is not fatal: whatever the walk managed to see is still a
	// useful summary, and fs.SkipDir escaping the walk is not an error at all.
	_ = err
	sort.Strings(entries)
	return entries, total
}

// readReadme returns the name and first limit bytes of the repository's
// README, cut at a line boundary and at a rune boundary. It returns empty
// strings when there is none or it is not text.
func readReadme(root string, limit int) (name, head string, truncated bool) {
	for _, candidate := range readmeNames {
		path := filepath.Join(root, candidate)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(path) //nolint:gosec // a README inside the target repository is exactly what we want
		if err != nil {
			continue
		}
		if len(data) > limit {
			data, truncated = data[:limit], true
		}
		text := strings.ToValidUTF8(string(data), "")
		if truncated {
			text = cutAtLine(text)
		}
		if !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
			continue
		}
		return candidate, strings.TrimRight(text, "\n"), truncated
	}
	return "", "", false
}

// cutAtLine drops the last, probably partial, line of a truncated file.
func cutAtLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}
