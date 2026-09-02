package beads

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"slop-cannon/internal/config"
)

// InitFlags are the `bd init` flags D-13 mandates, verified against bd
// 1.2.2 (Appendix A): no prompts, a no-op when .beads/ exists, no agent
// instruction files, no git hooks, and .beads/ kept out of the repository
// through .git/info/exclude instead of a commit.
var InitFlags = []string{"--non-interactive", "--init-if-missing", "--skip-agents", "--skip-hooks", "--setup-exclude"}

// InitTimeout bounds `bd init`, which creates the embedded Dolt database
// (about 1.5 s on a laptop).
const InitTimeout = 2 * time.Minute

// GitignoreMarker is the comment bd init writes above the block it appends
// to the repository's .gitignore. EnsureInitialised recognises the block by
// it and removes it again.
const GitignoreMarker = "# Beads / Dolt files (added by bd init)"

// ErrNotGitRepo is returned by EnsureInitialised when repo is not inside a
// git repository. bd init would otherwise run `git init` itself.
var ErrNotGitRepo = errors.New("not a git repository")

// InitOptions customises EnsureInitialised.
type InitOptions struct {
	// Prefix is the issue prefix passed as `-p`. Empty means
	// DefaultPrefix(root). Passing it explicitly keeps ids stable even if
	// the directory is renamed (D-13).
	Prefix string
	// Actor is the BEADS_ACTOR for the calls; empty means DefaultActor.
	Actor string
	// Logger receives one Info line per action and a Warn line when the
	// .gitignore block could not be reverted. nil means slog.Default().
	Logger *slog.Logger
	// Timeout bounds the init call; zero means InitTimeout.
	Timeout time.Duration
}

// InitResult reports what EnsureInitialised did.
type InitResult struct {
	// Root is the repository root the checks ran in.
	Root string
	// Initialised is true when `bd init` was run because .beads/ was
	// missing; false when the repository already had one.
	Initialised bool
	// Prefix is the issue prefix in effect after an init, as bd reported
	// it (bd rewrites characters it does not accept). Empty when no init
	// ran.
	Prefix string
	// Output is the trimmed text bd init printed (it ignores --json).
	Output string
	// GitignoreReverted is true when bd init's block was removed from the
	// repository's .gitignore (or the file it created was deleted).
	GitignoreReverted bool
	// GitignoreWarning is set when .gitignore changed in a way sc did not
	// recognise as only bd's block, so it was left alone.
	GitignoreWarning string
	// Ping is the health check that ran last.
	Ping PingResult
}

// EnsureInitialised implements D-13. If repo (or the git repository that
// contains it) has no .beads/ directory it runs
//
//	bd init --non-interactive --init-if-missing --skip-agents --skip-hooks --setup-exclude -p <prefix>
//
// in the repository root, reverts the "Beads / Dolt files" block bd appends
// to .gitignore when that block is the only change (deleting the file if bd
// created it), and logs every step. Whether or not init ran, it finishes
// with `bd ping --json` so a stale or unusable database is caught early.
//
// bd init is run with the root as working directory rather than `-C`,
// because bd 1.2.2 refuses `-C` on a directory without a beads project.
// When init fails, the .beads/ it may have started and the .gitignore block
// are removed again so a retry starts clean. The result is a *CheckError
// for ping failures, ErrNotGitRepo when repo is not a git repository, and
// a wrapped *proc.ExitError when init itself fails.
func EnsureInitialised(ctx context.Context, repo string, opts InitOptions) (InitResult, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	root, ok := config.FindRepoRoot(repo)
	if !ok {
		return InitResult{}, fmt.Errorf("beads: %w: %s", ErrNotGitRepo, repo)
	}
	res := InitResult{Root: root}
	beadsDir := filepath.Join(root, ".beads")
	if info, err := os.Stat(beadsDir); err == nil && info.IsDir() {
		logger.InfoContext(ctx, "beads already initialised", "repo", root)
	} else {
		if err := runInit(ctx, root, opts, logger, &res); err != nil {
			return res, err
		}
	}
	ping, err := Ping(ctx, root)
	res.Ping = ping
	if err != nil {
		logger.ErrorContext(ctx, "beads health check failed", "repo", root, "error", err.Error())
		return res, err
	}
	logger.InfoContext(ctx, "beads database answered", "repo", root, "total_ms", ping.TotalMS)
	return res, nil
}

// runInit performs the init step of EnsureInitialised: snapshot .gitignore,
// run bd init in root, revert the block, clean up on failure.
func runInit(ctx context.Context, root string, opts InitOptions, logger *slog.Logger, res *InitResult) error {
	prefix := opts.Prefix
	if prefix == "" {
		prefix = DefaultPrefix(root)
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = InitTimeout
	}
	gitignore := filepath.Join(root, ".gitignore")
	before, mode, existed, err := readGitignore(gitignore)
	if err != nil {
		return err
	}
	args := InitArgs(prefix)
	logger.InfoContext(ctx, "initialising beads", "repo", root, "command", Binary+" "+strings.Join(args, " "))
	started := time.Now()
	out, runErr := Run(ctx, CallOptions{Dir: root, Actor: opts.Actor, Timeout: timeout}, args...)
	res.Initialised = runErr == nil
	res.Output = strings.TrimSpace(out.Stdout)
	res.Prefix = reportedPrefix(out.Stdout, prefix)

	reverted, warning, giErr := revertGitignore(gitignore, before, mode, existed)
	res.GitignoreReverted = reverted
	res.GitignoreWarning = warning
	switch {
	case giErr != nil:
		logger.WarnContext(ctx, "could not revert .gitignore after bd init", "path", gitignore, "error", giErr.Error())
	case reverted:
		logger.InfoContext(ctx, "reverted the .gitignore block bd init appended", "path", gitignore)
	case warning != "":
		logger.WarnContext(ctx, warning, "path", gitignore)
	}

	if runErr != nil {
		beadsDir := filepath.Join(root, ".beads")
		if rmErr := os.RemoveAll(beadsDir); rmErr == nil {
			logger.WarnContext(ctx, "removed the partial .beads directory left by a failed bd init", "path", beadsDir)
		}
		logger.ErrorContext(ctx, "bd init failed", "repo", root, "error", ErrorSummary(runErr))
		return fmt.Errorf("beads: bd init in %s: %w", root, runErr)
	}
	logger.InfoContext(ctx, "beads initialised", "repo", root, "prefix", res.Prefix, "duration", time.Since(started).Round(time.Millisecond))
	if giErr != nil {
		return fmt.Errorf("beads: reverting %s after bd init: %w", gitignore, giErr)
	}
	return nil
}

// InitArgs returns the argument list for `bd init` with the D-13 flags and
// the given prefix: init, InitFlags, -p prefix.
func InitArgs(prefix string) []string {
	args := make([]string, 0, len(InitFlags)+3)
	args = append(args, "init")
	args = append(args, InitFlags...)
	return append(args, "-p", prefix)
}

// InitCommandLine is the human-readable init command sc runs, for hints and
// docs: "bd init --non-interactive ... --setup-exclude".
func InitCommandLine() string {
	return Binary + " init " + strings.Join(InitFlags, " ")
}

var prefixInvalid = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// DefaultPrefix derives the issue prefix from the repository directory
// name, the way bd does by default, but sanitised so bd never rejects or
// rewrites it: runs of characters outside [A-Za-z0-9_-] become "_",
// leading and trailing "-" or "_" are dropped, and an empty result falls
// back to "sc". bd may still rewrite what it stores (a leading digit gains
// "bd_"); InitResult.Prefix reports the final value.
func DefaultPrefix(root string) string {
	base := filepath.Base(filepath.Clean(root))
	if base == "." || base == string(filepath.Separator) {
		base = ""
	}
	p := prefixInvalid.ReplaceAllString(base, "_")
	p = strings.Trim(p, "_-")
	if p == "" {
		return "sc"
	}
	return p
}

var prefixLine = regexp.MustCompile(`(?m)^\s*Issue prefix:\s*(\S+)\s*$`)

// reportedPrefix extracts "Issue prefix: X" from bd init's text output,
// falling back to the requested prefix.
func reportedPrefix(stdout, requested string) string {
	if m := prefixLine.FindStringSubmatch(stdout); m != nil {
		return m[1]
	}
	return requested
}

// readGitignore snapshots the repository .gitignore: its bytes, mode, and
// whether it exists at all.
func readGitignore(path string) (content []byte, mode os.FileMode, existed bool, err error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("beads: reading %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, false, fmt.Errorf("beads: %s is not a regular file", path)
	}
	content, err = os.ReadFile(path)
	if err != nil {
		return nil, 0, false, fmt.Errorf("beads: reading %s: %w", path, err)
	}
	return content, info.Mode().Perm(), true, nil
}

// revertGitignore restores .gitignore to before when the only difference is
// the block bd init appends (deleting the file when it did not exist). Any
// other difference is left alone and described in warning.
func revertGitignore(path string, before []byte, mode os.FileMode, existed bool) (reverted bool, warning string, err error) {
	after, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if existed {
			return false, ".gitignore disappeared during bd init; left as is", nil
		}
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("reading %s: %w", path, err)
	}
	if existed && bytes.Equal(after, before) {
		return false, "", nil
	}
	tail, ok := bytes.CutPrefix(after, before)
	if !ok {
		return false, "bd init changed .gitignore beyond appending its block; left as is, review it before committing", nil
	}
	if !IsGitignoreBlock(tail) {
		return false, "bd init appended more than its \"Beads / Dolt files\" block to .gitignore; left as is, review it before committing", nil
	}
	if !existed {
		if err := os.Remove(path); err != nil {
			return false, "", fmt.Errorf("removing %s: %w", path, err)
		}
		return true, "", nil
	}
	if err := os.WriteFile(path, before, mode); err != nil {
		return false, "", fmt.Errorf("restoring %s: %w", path, err)
	}
	return true, "", nil
}

// IsGitignoreBlock reports whether text is exactly the block bd init
// appends to .gitignore: blank lines, one comment line carrying
// GitignoreMarker (or naming bd init), and ignore patterns, nothing else.
func IsGitignoreBlock(text []byte) bool {
	marker := false
	patterns := 0
	for _, line := range strings.Split(string(text), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "#"):
			if line != GitignoreMarker && !strings.Contains(line, "bd init") {
				return false
			}
			marker = true
		default:
			if !marker {
				return false
			}
			patterns++
		}
	}
	return marker && patterns > 0
}
