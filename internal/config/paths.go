package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"slop-cannon/internal/logging"
)

// Scope says which config file a write goes to (D-03).
type Scope string

// The two config scopes.
const (
	// ScopeGlobal is $XDG_CONFIG_HOME/slop-cannon/config.toml.
	ScopeGlobal Scope = "global"
	// ScopeRepo is <repo>/.slop-cannon/config.toml.
	ScopeRepo Scope = "repo"
)

// String returns the scope name.
func (s Scope) String() string { return string(s) }

// Target applies the D-03 rule for config subcommands: write the repo config
// when the working directory (or --repo) is inside a git repository, else
// the global config; --global always forces global.
func Target(inRepo, global bool) Scope {
	if global || !inRepo {
		return ScopeGlobal
	}
	return ScopeRepo
}

// homeDir returns $HOME, falling back to os.UserHomeDir and finally ".".
func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return "."
}

// xdgDir returns $<envVar> when it is set to an absolute path (the XDG spec
// says relative values must be ignored), else <home>/<fallback>.
func xdgDir(envVar, fallback string) string {
	if v := os.Getenv(envVar); v != "" && filepath.IsAbs(v) {
		return filepath.Clean(v)
	}
	return filepath.Join(homeDir(), fallback)
}

// Home is $XDG_CONFIG_HOME or ~/.config, the parent of the global config dir.
func Home() string { return xdgDir("XDG_CONFIG_HOME", ".config") }

// StateHome is $XDG_STATE_HOME or ~/.local/state.
func StateHome() string { return xdgDir("XDG_STATE_HOME", filepath.Join(".local", "state")) }

// GlobalPath is the global config file: <Home>/slop-cannon/config.toml.
func GlobalPath() string { return filepath.Join(Home(), AppName, FileName) }

// RepoConfigPath is the repo config file: <repoRoot>/.slop-cannon/config.toml.
func RepoConfigPath(repoRoot string) string {
	return filepath.Join(repoRoot, RepoDirName, FileName)
}

// PathFor returns the config file for scope; repoRoot is used only for
// ScopeRepo.
func PathFor(scope Scope, repoRoot string) string {
	if scope == ScopeRepo {
		return RepoConfigPath(repoRoot)
	}
	return GlobalPath()
}

// FindRepoRoot walks from path upwards looking for a .git entry (a directory
// for a normal checkout, a file for a worktree or submodule) and returns the
// directory that holds it. It needs no git binary; whether the directory is
// a healthy repository is for gitx to decide.
func FindRepoRoot(path string) (root string, ok bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	dir := abs
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// canonicalPath makes path absolute and resolves symlinks where possible so
// the same repository always yields the same RepoID (for example /tmp and
// /private/tmp on macOS).
func canonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs
}

// RepoID derives a stable identifier for a repository: the base name of its
// main working tree (made file-system safe) plus the first 12 hex digits of
// the SHA-256 of that tree's canonical absolute path, for example
// "my-app-3f2c9a1b7d0e". The same path always gets the same id, and two
// unrelated checkouts get different ones.
//
// A linked `git worktree` gets the id of the repository it belongs to, not
// its own (P9-04): a worktree shares the main checkout's .beads database,
// default branch and remote, so a run in one is a run in the other, and one
// state directory means one run lock covers both.
func RepoID(repoPath string) string {
	canon := repoIdentity(canonicalPath(repoPath))
	sum := sha256.Sum256([]byte(canon))
	base := logging.SafeFileName(filepath.Base(canon))
	return base + "-" + hex.EncodeToString(sum[:])[:12]
}

// repoIdentity maps a canonical checkout path to the path that identifies
// its repository. For a normal checkout, a bare repository or a submodule
// that is itself, and for a linked worktree it is the main working tree —
// found by following .git to the worktree's git directory and its
// `commondir` pointer, which is how git itself finds the shared directory.
// No git binary is needed, and anything unexpected leaves the path alone.
func repoIdentity(canon string) string {
	gitPath := filepath.Join(canon, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil || !info.Mode().IsRegular() {
		return canon // a .git directory (or no repository here at all).
	}
	gitDir, ok := gitDirPointer(gitPath)
	if !ok {
		return canon
	}
	common, err := os.ReadFile(filepath.Join(gitDir, "commondir")) //nolint:gosec // a fixed name inside the git directory this checkout points at.
	if err != nil {
		return canon // a submodule: its git directory is its own common one.
	}
	rel := strings.TrimSpace(string(common))
	if rel == "" {
		return canon
	}
	commonDir := rel
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(gitDir, commonDir)
	}
	commonDir = canonicalPath(commonDir)
	if filepath.Base(commonDir) == ".git" {
		return filepath.Dir(commonDir) // the main working tree.
	}
	return commonDir // a bare repository with worktrees hanging off it.
}

// gitDirPointer reads a .git *file* ("gitdir: <path>") and returns the
// directory it names, resolved against the checkout that holds it.
func gitDirPointer(gitFile string) (string, bool) {
	data, err := os.ReadFile(gitFile) //nolint:gosec // the .git entry of a repository the user pointed sc at.
	if err != nil {
		return "", false
	}
	rest, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return "", false
	}
	dir := strings.TrimSpace(rest)
	if dir == "" {
		return "", false
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(filepath.Dir(gitFile), dir)
	}
	return filepath.Clean(dir), true
}

// StateDir is where everything sc keeps for one repository lives (D-23):
// <StateHome>/slop-cannon/<RepoID>/. Runs go in RunDir below it; nothing is
// created here.
func StateDir(repoPath string) string {
	return filepath.Join(StateHome(), AppName, RepoID(repoPath))
}

// RunsDirName is the subdirectory of StateDir that holds one directory per run.
const RunsDirName = "runs"

// RunDir is the directory of one run: <StateDir>/runs/<run-id>/ (D-23). The
// id is passed through logging.SafeFileName so it is always one path element.
func RunDir(repoPath, runID string) string {
	return filepath.Join(StateDir(repoPath), RunsDirName, logging.SafeFileName(runID))
}
