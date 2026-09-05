package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"slop-cannon/internal/proc"
)

// DefaultTimeout bounds one git invocation when no [WithTimeout] option is
// given. It is generous because fetches over the network can be slow; local
// commands finish in milliseconds regardless.
const DefaultTimeout = 5 * time.Minute

// Sentinel errors. Every error returned by this package that corresponds to
// one of these conditions matches it with errors.Is, even when wrapped in a
// [*GitError].
var (
	// ErrNotRepository means the path is not inside a git working tree.
	ErrNotRepository = errors.New("gitx: not a git repository")
	// ErrNoRemote means the repository has no remotes configured.
	ErrNoRemote = errors.New("gitx: repository has no remote")
	// ErrNoDefaultBranch means neither a remote HEAD nor a main/master
	// branch exists to serve as the default branch.
	ErrNoDefaultBranch = errors.New("gitx: cannot determine the default branch (no remote HEAD, main, or master)")
	// ErrNoCommits means HEAD points at an unborn branch.
	ErrNoCommits = errors.New("gitx: repository has no commits")
	// ErrNoMergeBase means the two revisions share no common ancestor.
	ErrNoMergeBase = errors.New("gitx: revisions have no common ancestor")
	// ErrUnknownRevision means a revision could not be resolved to a commit.
	ErrUnknownRevision = errors.New("gitx: unknown revision")
	// ErrDetachedHead means HEAD is not on a branch.
	ErrDetachedHead = errors.New("gitx: HEAD is detached")
)

// GitError describes a git command that exited non-zero. It wraps both the
// classifying sentinel (when one applies) and the underlying
// [*proc.ExitError], so errors.Is works for either and errors.As can reach
// the exit code and captured output.
type GitError struct {
	// Args are the git arguments that were run (without "git" itself).
	Args []string
	// Dir is the directory git ran in.
	Dir string
	// ExitCode is git's exit status, or -1 if it was killed.
	ExitCode int
	// Stderr is git's captured standard error, trimmed.
	Stderr string
	// Cause is the sentinel this failure was classified as, or nil.
	Cause error
	// Err is the error from the process runner.
	Err error
}

// Error renders "gitx: git <args>: exit status N: <last stderr line>".
func (e *GitError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "gitx: git %s", strings.Join(e.Args, " "))
	if e.Cause != nil {
		fmt.Fprintf(&b, ": %v", e.Cause)
	} else if e.ExitCode >= 0 {
		fmt.Fprintf(&b, ": exit status %d", e.ExitCode)
	} else {
		b.WriteString(": killed")
	}
	if tail := lastLine(e.Stderr); tail != "" {
		fmt.Fprintf(&b, ": %s", tail)
	}
	return b.String()
}

// Unwrap exposes Cause and Err so errors.Is matches both the sentinel and
// the runner's causes (proc.ErrTimeout, context cancellation).
func (e *GitError) Unwrap() []error {
	var errs []error
	if e.Cause != nil {
		errs = append(errs, e.Cause)
	}
	if e.Err != nil {
		errs = append(errs, e.Err)
	}
	return errs
}

// Repo is an opened git working tree. All methods run the git binary as a
// subprocess through [proc]; nothing links libgit. A Repo is safe for
// concurrent use because it holds no mutable state.
type Repo struct {
	// Root is the absolute path of the working tree's top level.
	Root string
	// GitDir is the absolute path of the repository's .git directory. For a
	// linked worktree this is the per-worktree directory under the main
	// repository's .git/worktrees/.
	GitDir string
	// CommonDir is the absolute path of the shared .git directory. It equals
	// GitDir for a main working tree and points at the main repository's
	// .git for a linked worktree.
	CommonDir string

	env     map[string]string
	inherit []string
	timeout time.Duration
}

// Option customises [Open].
type Option func(*Repo)

// WithEnv sets explicit environment variables for every git invocation.
// They override inherited values of the same name. Tests use it to give git
// an isolated HOME and a fixed author identity.
func WithEnv(env map[string]string) Option {
	return func(r *Repo) {
		if r.env == nil {
			r.env = map[string]string{}
		}
		for k, v := range env {
			r.env[k] = v
		}
	}
}

// WithTimeout bounds each git invocation. Zero or negative keeps
// [DefaultTimeout].
func WithTimeout(d time.Duration) Option {
	return func(r *Repo) {
		if d > 0 {
			r.timeout = d
		}
	}
}

// Open locates the git working tree containing path (a directory inside a
// checkout, or the checkout itself) and returns a [Repo] for it. It fails
// with [ErrNotRepository] when path is not inside a working tree, including
// for bare repositories, and with a plain error when path does not exist.
// Other git failures (a "dubious ownership" refusal, a corrupt repository)
// are returned as an unclassified [*GitError] carrying git's stderr.
//
// The repository is discovered from path alone: GIT_DIR, GIT_WORK_TREE and
// the other repository-location variables of the parent process are never
// forwarded to git (see [Repo.Git]).
func Open(ctx context.Context, path string, opts ...Option) (*Repo, error) {
	if path == "" {
		path = "."
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("gitx: %s: %w", path, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("gitx: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("gitx: %s: not a directory", abs)
	}

	r := &Repo{Root: abs, timeout: DefaultTimeout}
	for _, o := range opts {
		o(r)
	}
	r.inherit = inheritedEnv()

	// --show-toplevel is always absolute. --git-dir and --git-common-dir
	// are asked for separately from the top level so that a relative
	// answer (".git") resolves against the right directory on every git
	// since 2.30; --path-format=absolute would do it in one call but needs
	// git 2.31, newer than the floor sc doctor enforces.
	top, err := r.run(ctx, abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, classifyNotRepository(err)
	}
	if top == "" || strings.Contains(top, "\n") {
		return nil, fmt.Errorf("gitx: unexpected rev-parse output %q", top)
	}
	r.Root = filepath.Clean(top)

	out, err := r.Git(ctx, "rev-parse", "--git-dir", "--git-common-dir")
	if err != nil {
		return nil, classifyNotRepository(err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 2 || lines[0] == "" || lines[1] == "" {
		return nil, fmt.Errorf("gitx: unexpected rev-parse output %q", out)
	}
	r.GitDir = r.absolute(lines[0])
	r.CommonDir = r.absolute(lines[1])
	return r, nil
}

// absolute resolves a path git printed relative to the working tree's top
// level (the directory it ran in) to a clean absolute path.
func (r *Repo) absolute(p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.Root, p)
	}
	return filepath.Clean(p)
}

// classifyNotRepository tags err with [ErrNotRepository] when git refused
// because the directory is outside any repository or working tree. Other
// exit-128 failures (dubious ownership, corruption) keep a nil Cause so the
// real message reaches the user.
func classifyNotRepository(err error) error {
	var ge *GitError
	if !errors.As(err, &ge) || ge.ExitCode != 128 {
		return err
	}
	stderr := strings.ToLower(ge.Stderr)
	if strings.Contains(stderr, "not a git repository") ||
		strings.Contains(stderr, "must be run in a work tree") {
		ge.Cause = ErrNotRepository
	}
	return err
}

// IsWorktree reports whether the Repo is a linked worktree rather than the
// main working tree of its repository.
func (r *Repo) IsWorktree() bool { return r.GitDir != r.CommonDir }

// Git runs git with args inside the working tree and returns its standard
// output with every trailing carriage return and newline removed (leading
// whitespace is kept, so porcelain status columns survive); use
// [Repo.GitRaw] where those bytes carry meaning. A non-zero exit is reported as a
// [*GitError]; a failure to start git (missing binary) is returned as-is and
// matches proc.IsNotFound. The child sees only PATH, HOME, the XDG_* directories,
// the SSH agent and GnuPG variables, every GIT_* variable set in the parent
// except the ones that relocate the repository (GIT_DIR, GIT_WORK_TREE and
// friends, see [inheritedEnv]), fixed values that keep output stable and non-interactive (LC_ALL=C,
// GIT_TERMINAL_PROMPT=0, GIT_OPTIONAL_LOCKS=0, no pager or editor), and
// anything set with [WithEnv], which wins on conflict.
func (r *Repo) Git(ctx context.Context, args ...string) (string, error) {
	return r.run(ctx, r.Root, args...)
}

// GitRaw runs git exactly as [Repo.Git] does but returns its standard
// output byte for byte: no trailing newline is removed, so CRLF line
// endings and a meaningful empty last line survive (P4-11).
//
// Use it for every reader whose bytes are the answer — file content from
// "git show" or "git cat-file", a message body, the NUL-terminated
// output of a porcelain "-z" command — and [Repo.Git] for the
// line-oriented commands whose trailing newline is only a terminator.
// [Repo.Diff] does not go through either: it has git write the patch to a
// file so an oversized diff is never held in memory.
func (r *Repo) GitRaw(ctx context.Context, args ...string) (string, error) {
	return r.runRaw(ctx, r.Root, args...)
}

// run returns git's standard output with every trailing carriage return
// and newline removed; see [Repo.GitRaw] for the byte-exact form.
func (r *Repo) run(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := r.runRaw(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\r\n"), nil
}

func (r *Repo) runRaw(ctx context.Context, dir string, args ...string) (string, error) {
	env := map[string]string{
		// Stable, parseable output and no interactive stalls.
		"LC_ALL":              "C",
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_PAGER":           "cat",
		"PAGER":               "cat",
		"GIT_EDITOR":          "true",
		// Never let a status probe take the index lock from a human's git.
		"GIT_OPTIONAL_LOCKS": "0",
	}
	for k, v := range r.env {
		env[k] = v
	}
	spec := proc.Spec{
		Command:    "git",
		Args:       args,
		Dir:        dir,
		Env:        env,
		InheritEnv: r.inherit,
		Timeout:    r.timeout,
	}
	res, err := proc.Run(ctx, spec)
	if err != nil {
		var ee *proc.ExitError
		if errors.As(err, &ee) {
			return "", &GitError{
				Args:     args,
				Dir:      dir,
				ExitCode: ee.Result.ExitCode,
				Stderr:   strings.TrimSpace(ee.Result.Stderr),
				Err:      err,
			}
		}
		return "", fmt.Errorf("gitx: git: %w", err)
	}
	return res.Stdout, nil
}

// inheritedEnv names the parent variables git may see: enough to find its
// configuration, credentials, and SSH agent, plus every GIT_* override the
// user set. Tokens and unrelated variables are withheld, and so are the
// GIT_* variables that would point git at a different repository than the
// one Open was given (a git hook or alias exports GIT_DIR, GIT_WORK_TREE
// and GIT_INDEX_FILE for its own repository).
func inheritedEnv() []string {
	names := []string{
		"HOME", "USER", "LOGNAME",
		"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
		"SSH_AUTH_SOCK", "SSH_AGENT_PID", "GNUPGHOME",
		"TMPDIR", "TZ", "LANG",
	}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "GIT_") && !repoLocationEnv[name] {
			names = append(names, name)
		}
	}
	return names
}

// repoLocationEnv lists the GIT_* variables that change which repository,
// working tree, index, or object store git operates on. They are never
// inherited so that a Repo always addresses the path it was opened on.
var repoLocationEnv = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_COMMON_DIR":                   true,
	"GIT_NAMESPACE":                    true,
	"GIT_CEILING_DIRECTORIES":          true,
	"GIT_PREFIX":                       true,
}

// classify returns err with its GitError's Cause set to cause when err is a
// GitError whose exit status is in codes (any status when codes is empty),
// and err unchanged otherwise.
func classify(err error, cause error, codes ...int) error {
	var ge *GitError
	if !errors.As(err, &ge) {
		return err
	}
	if len(codes) == 0 {
		ge.Cause = cause
		return err
	}
	for _, c := range codes {
		if ge.ExitCode == c {
			ge.Cause = cause
			return err
		}
	}
	return err
}

// Remotes lists the configured remote names in git's order.
func (r *Repo) Remotes(ctx context.Context) ([]string, error) {
	out, err := r.Git(ctx, "remote")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// HasRemote reports whether the repository has at least one remote.
func (r *Repo) HasRemote(ctx context.Context) (bool, error) {
	remotes, err := r.Remotes(ctx)
	if err != nil {
		return false, err
	}
	return len(remotes) > 0, nil
}

// DefaultRemote returns the remote to fetch from and push to: "origin" when
// it exists, otherwise the first configured remote. It fails with
// [ErrNoRemote] when there are none.
func (r *Repo) DefaultRemote(ctx context.Context) (string, error) {
	remotes, err := r.Remotes(ctx)
	if err != nil {
		return "", err
	}
	if len(remotes) == 0 {
		return "", ErrNoRemote
	}
	for _, name := range remotes {
		if name == "origin" {
			return name, nil
		}
	}
	return remotes[0], nil
}

// DefaultBranch detects the branch that finished work is delivered to. It
// checks, in order: the remote HEAD (refs/remotes/<remote>/HEAD, origin
// first, then any other remote), then a local "main", then a local
// "master". A remote HEAD that points at a branch which no longer exists
// (neither as refs/remotes/<remote>/<name> nor as a local branch, as after
// the upstream renamed its default branch and a prune fetch removed the old
// tracking ref) is skipped as if it were unset. It fails with
// [ErrNoDefaultBranch] when none exists.
func (r *Repo) DefaultBranch(ctx context.Context) (string, error) {
	remotes, err := r.Remotes(ctx)
	if err != nil {
		return "", err
	}
	// origin first, then the others in git's order.
	ordered := make([]string, 0, len(remotes))
	for _, name := range remotes {
		if name == "origin" {
			ordered = append(ordered, name)
		}
	}
	for _, name := range remotes {
		if name != "origin" {
			ordered = append(ordered, name)
		}
	}
	for _, remote := range ordered {
		ref, err := r.Git(ctx, "symbolic-ref", "-q", "refs/remotes/"+remote+"/HEAD")
		if err != nil {
			if isExit(err, 1) {
				continue
			}
			return "", err
		}
		prefix := "refs/remotes/" + remote + "/"
		if !strings.HasPrefix(ref, prefix) || ref == prefix {
			continue
		}
		name := strings.TrimPrefix(ref, prefix)
		ok, err := r.refResolves(ctx, ref)
		if err != nil {
			return "", err
		}
		if !ok {
			ok, err = r.BranchExists(ctx, name)
			if err != nil {
				return "", err
			}
		}
		if ok {
			return name, nil
		}
	}
	for _, name := range []string{"main", "master"} {
		ok, err := r.BranchExists(ctx, name)
		if err != nil {
			return "", err
		}
		if ok {
			return name, nil
		}
	}
	return "", ErrNoDefaultBranch
}

// refResolves reports whether ref names a commit.
func (r *Repo) refResolves(ctx context.Context, ref string) (bool, error) {
	_, err := r.RevParse(ctx, ref)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrUnknownRevision) {
		return false, nil
	}
	return false, err
}

// BranchExists reports whether a local branch named name exists.
func (r *Repo) BranchExists(ctx context.Context, name string) (bool, error) {
	_, err := r.Git(ctx, "show-ref", "--verify", "--quiet", "refs/heads/"+name)
	if err == nil {
		return true, nil
	}
	if isExit(err, 1) {
		return false, nil
	}
	return false, err
}

// CurrentBranch returns the short name of the branch HEAD is on. It fails
// with [ErrDetachedHead] when HEAD is detached.
func (r *Repo) CurrentBranch(ctx context.Context) (string, error) {
	out, err := r.Git(ctx, "symbolic-ref", "-q", "--short", "HEAD")
	if err != nil {
		return "", classify(err, ErrDetachedHead, 1)
	}
	return out, nil
}

// Status returns the lines of "git status --porcelain" (modified, staged,
// and untracked entries). An empty slice means the tree is clean.
func (r *Repo) Status(ctx context.Context) ([]string, error) {
	out, err := r.Git(ctx, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// IsClean reports whether the working tree has no modified, staged, or
// untracked files. Files ignored by .gitignore or .git/info/exclude do not
// count.
func (r *Repo) IsClean(ctx context.Context) (bool, error) {
	lines, err := r.Status(ctx)
	if err != nil {
		return false, err
	}
	return len(lines) == 0, nil
}

// Fetch updates the remote-tracking refs of the default remote (see
// [DefaultRemote]) with "git fetch --prune". It fails with [ErrNoRemote]
// when the repository has no remote.
func (r *Repo) Fetch(ctx context.Context) error {
	remote, err := r.DefaultRemote(ctx)
	if err != nil {
		return err
	}
	_, err = r.Git(ctx, "fetch", "--quiet", "--prune", remote)
	return err
}

// HeadSHA returns the full SHA of the commit HEAD points at. It fails with
// [ErrNoCommits] on an unborn branch (rev-parse --verify exits 1 there);
// any other failure (timeout, corrupt or unreadable repository) keeps its
// own classification so callers never mistake it for an empty repository.
func (r *Repo) HeadSHA(ctx context.Context) (string, error) {
	sha, err := r.RevParse(ctx, "HEAD")
	if err != nil {
		return "", classify(err, ErrNoCommits, 1)
	}
	return sha, nil
}

// RevParse resolves rev (a branch, tag, SHA, or expression such as
// "origin/main") to a full commit SHA. It fails with [ErrUnknownRevision]
// when rev does not name a commit.
func (r *Repo) RevParse(ctx context.Context, rev string) (string, error) {
	out, err := r.Git(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", classify(err, ErrUnknownRevision, 1)
	}
	return out, nil
}

// MergeBase returns the best common ancestor of revisions a and b. It fails
// with [ErrNoMergeBase] when they share no history and with
// [ErrUnknownRevision] when either does not resolve.
func (r *Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	out, err := r.Git(ctx, "merge-base", "--", a, b)
	if err != nil {
		if isExit(err, 1) {
			return "", classify(err, ErrNoMergeBase)
		}
		return "", classify(err, ErrUnknownRevision, 128)
	}
	return out, nil
}

// isExit reports whether err is a GitError with the given exit status.
func isExit(err error, code int) bool {
	var ge *GitError
	return errors.As(err, &ge) && ge.ExitCode == code
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\r\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}
