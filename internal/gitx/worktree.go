package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/logging"
)

// WorktreesDirName is the subdirectory of a run directory that holds one
// linked worktree per bead (D-23): <state-dir>/runs/<run-id>/wt/<bead-id>.
const WorktreesDirName = "wt"

// BranchPrefix is the namespace of every branch slop-cannon creates.
const BranchPrefix = "sc/"

// MaxSlugLen caps the title slug appended to a branch name.
const MaxSlugLen = 40

// Worktree sentinel errors. They match with errors.Is even when wrapped in
// a [*GitError].
var (
	// ErrNestedWorktree means the manager was asked to work from inside a
	// linked worktree, or to create a worktree inside another one.
	ErrNestedWorktree = errors.New("gitx: refusing to nest a worktree inside another worktree")
	// ErrWorktreeInsideRepo means the run directory lies inside the target
	// repository, which D-23 forbids.
	ErrWorktreeInsideRepo = errors.New("gitx: worktree directory must not be inside the target repository")
	// ErrRepoDirty means the target repository has uncommitted or untracked
	// changes, so no worktree is created from it.
	ErrRepoDirty = errors.New("gitx: target repository has uncommitted changes")
	// ErrWorktreeExists means a worktree for the bead is already registered.
	ErrWorktreeExists = errors.New("gitx: worktree already exists")
	// ErrWorktreeMissing means no usable worktree is registered for the bead.
	ErrWorktreeMissing = errors.New("gitx: worktree is missing")
	// ErrWorktreeDirty means the worktree has uncommitted changes and removal
	// was not forced.
	ErrWorktreeDirty = errors.New("gitx: worktree has uncommitted changes")
	// ErrPathOccupied means the worktree path exists and is not an empty
	// directory, but is not a registered worktree either.
	ErrPathOccupied = errors.New("gitx: worktree path is occupied by something else")
	// ErrBranchInUse means the bead's branch is already checked out in a
	// worktree at another path.
	ErrBranchInUse = errors.New("gitx: branch is already checked out elsewhere")
)

// Worktree describes one linked worktree that belongs to a bead.
type Worktree struct {
	// BeadID is the bead the worktree was created for, derived from the
	// directory name.
	BeadID domain.BeadID
	// Path is the absolute path of the worktree.
	Path string
	// Branch is the short name of the branch checked out in it
	// ("sc/<bead-id>-<slug>"); empty when HEAD is detached.
	Branch string
	// Head is the commit HEAD pointed at when the worktree was listed.
	Head string
	// Prunable is set when git reports the worktree as prunable (its
	// directory is gone); Reattach refuses such entries.
	Prunable bool
}

// Slug turns a title into a short, branch-safe suffix: lower-case ASCII
// letters and digits separated by single dashes, at most [MaxSlugLen]
// bytes, never ending in a dash. Anything else, including all non-ASCII
// characters, is dropped. The result is empty for a title with nothing
// usable in it.
func Slug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		ok := r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r))
		switch {
		case ok:
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
		if b.Len() >= MaxSlugLen {
			break
		}
	}
	return strings.TrimRight(b.String()[:min(b.Len(), MaxSlugLen)], "-")
}

// BranchName returns the branch a bead's work lives on: "sc/<bead-id>-<slug>",
// or "sc/<bead-id>" when the title yields no slug (D-10).
func BranchName(bead domain.Bead) string {
	name := BranchPrefix + logging.SafeFileName(string(bead.ID))
	if slug := Slug(bead.Title); slug != "" {
		name += "-" + slug
	}
	return name
}

// Manager creates and tracks the linked worktrees of one run. Worktrees
// live under <runDir>/wt/<bead-id>, outside the target repository (D-23),
// so nothing is written into the repository except the worktree
// registration in its .git directory. It is safe for concurrent use.
type Manager struct {
	repo       *Repo
	dir        string
	allowDirty bool
}

// ManagerOption customises [NewManager].
type ManagerOption func(*Manager)

// AllowDirty lets Create proceed when the target repository has uncommitted
// changes. By default Create refuses with [ErrRepoDirty] so a run never
// starts from a tree the user is in the middle of editing.
func AllowDirty() ManagerOption {
	return func(m *Manager) { m.allowDirty = true }
}

// NewManager returns a Manager whose worktrees live under
// <runDir>/wt (see [WorktreesDirName]); callers pass config.RunDir for the
// run. It fails with [ErrNestedWorktree] when repo is itself a linked
// worktree and with [ErrWorktreeInsideRepo] when runDir is inside the
// repository or not absolute. Nothing is created on disk.
func NewManager(repo *Repo, runDir string, opts ...ManagerOption) (*Manager, error) {
	if repo == nil {
		return nil, errors.New("gitx: NewManager: nil repo")
	}
	if repo.IsWorktree() {
		return nil, fmt.Errorf("%w: %s is a linked worktree of %s", ErrNestedWorktree, repo.Root, repo.CommonDir)
	}
	if !filepath.IsAbs(runDir) {
		return nil, fmt.Errorf("%w: run directory %q is not absolute", ErrWorktreeInsideRepo, runDir)
	}
	dir := filepath.Join(filepath.Clean(runDir), WorktreesDirName)
	if isWithin(repo.Root, dir) {
		return nil, fmt.Errorf("%w: %s is inside %s", ErrWorktreeInsideRepo, dir, repo.Root)
	}
	m := &Manager{repo: repo, dir: dir}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// Repo returns the main repository the worktrees are linked to.
func (m *Manager) Repo() *Repo { return m.repo }

// Dir returns the directory that holds the run's worktrees.
func (m *Manager) Dir() string { return m.dir }

// PathFor returns the worktree path of a bead, whether or not it exists.
func (m *Manager) PathFor(id domain.BeadID) string {
	return filepath.Join(m.dir, logging.SafeFileName(string(id)))
}

// Create makes a linked worktree for bead at PathFor(bead.ID) with
// "git worktree add <path> -b <BranchName(bead)> <default branch>". When the
// branch already exists (a previous attempt was interrupted) it is checked
// out as is instead of being recreated. It fails with [ErrRepoDirty] when
// the target repository is dirty (unless [AllowDirty] was given),
// [ErrWorktreeExists] when the bead already has a registered worktree,
// [ErrPathOccupied] when the path holds something else, [ErrNestedWorktree]
// when the path lies inside another worktree, [ErrBranchInUse] when the
// branch is checked out elsewhere, and [ErrNoDefaultBranch] when no base
// branch can be found.
func (m *Manager) Create(ctx context.Context, bead domain.Bead) (*Worktree, error) {
	if bead.ID == "" {
		return nil, errors.New("gitx: Create: bead has no id")
	}
	path := m.PathFor(bead.ID)
	branch := BranchName(bead)

	if !m.allowDirty {
		clean, err := m.repo.IsClean(ctx)
		if err != nil {
			return nil, err
		}
		if !clean {
			return nil, fmt.Errorf("%w: %s", ErrRepoDirty, m.repo.Root)
		}
	}

	all, err := m.listAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, wt := range all {
		switch {
		case samePath(wt.Path, path) && wt.Prunable:
			// The directory vanished (crash, cleanup by hand): drop this
			// one stale registration and start over. "worktree remove"
			// addresses a single path; "worktree prune" would also drop
			// the registrations of worktrees the user keeps elsewhere
			// (D-23).
			if _, err := m.repo.Git(ctx, "worktree", "remove", "--force", "--force", wt.Path); err != nil {
				return nil, err
			}
		case samePath(wt.Path, path):
			return nil, fmt.Errorf("%w: %s", ErrWorktreeExists, path)
		case isWithin(wt.Path, path):
			return nil, fmt.Errorf("%w: %s is inside %s", ErrNestedWorktree, path, wt.Path)
		case wt.Branch == branch:
			return nil, fmt.Errorf("%w: %s at %s", ErrBranchInUse, branch, wt.Path)
		}
	}
	if err := checkPathFree(path); err != nil {
		return nil, err
	}

	base, err := m.baseRevision(ctx)
	if err != nil {
		return nil, err
	}
	exists, err := m.repo.BranchExists(ctx, branch)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return nil, fmt.Errorf("gitx: creating worktree directory: %w", err)
	}
	args := []string{"worktree", "add", "--quiet", path}
	if exists {
		args = append(args, branch)
	} else {
		args = append(args, "-b", branch, base)
	}
	if _, err := m.repo.Git(ctx, args...); err != nil {
		return nil, err
	}
	return m.Reattach(ctx, bead.ID)
}

// Reattach finds the worktree of a bead created by an earlier process, for
// example after a crash and "--resume". It fails with [ErrWorktreeMissing]
// when nothing is registered at PathFor(id), the directory is gone, or git
// reports it prunable.
func (m *Manager) Reattach(ctx context.Context, id domain.BeadID) (*Worktree, error) {
	if id == "" {
		return nil, errors.New("gitx: Reattach: empty bead id")
	}
	path := m.PathFor(id)
	all, err := m.listAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, wt := range all {
		if !samePath(wt.Path, path) {
			continue
		}
		if wt.Prunable {
			return nil, fmt.Errorf("%w: %s is prunable (%s)", ErrWorktreeMissing, path, wt.prunableReason)
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("%w: %s is not a directory", ErrWorktreeMissing, path)
		}
		w := wt.Worktree
		w.BeadID = id
		w.Path = path
		return &w, nil
	}
	return nil, fmt.Errorf("%w: no worktree registered at %s", ErrWorktreeMissing, path)
}

// Open returns a [Repo] rooted in the bead's worktree, sharing the
// manager's environment and timeout, so callers can run git (commit, diff,
// rebase) inside it. It fails with [ErrWorktreeMissing] like Reattach.
func (m *Manager) Open(ctx context.Context, id domain.BeadID) (*Repo, error) {
	wt, err := m.Reattach(ctx, id)
	if err != nil {
		return nil, err
	}
	return Open(ctx, wt.Path, WithEnv(m.repo.env), WithTimeout(m.repo.timeout))
}

// List returns every worktree registered under Dir, sorted by bead id.
// Worktrees of the repository that live elsewhere (the user's own) are
// not included.
func (m *Manager) List(ctx context.Context) ([]Worktree, error) {
	all, err := m.listAll(ctx)
	if err != nil {
		return nil, err
	}
	var out []Worktree
	for _, wt := range all {
		if !isWithin(m.dir, wt.Path) || samePath(m.dir, wt.Path) {
			continue
		}
		w := wt.Worktree
		w.BeadID = domain.BeadID(filepath.Base(w.Path))
		w.Path = filepath.Join(m.dir, filepath.Base(w.Path))
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BeadID < out[j].BeadID })
	return out, nil
}

// Remove deletes the bead's worktree directory and its registration with
// "git worktree remove". The branch is kept: it may be pushed, under review,
// or awaiting a merge. With force false a worktree holding uncommitted or
// untracked changes is refused with [ErrWorktreeDirty]; with force true it
// is deleted regardless. Removing a worktree that is not registered fails
// with [ErrWorktreeMissing].
func (m *Manager) Remove(ctx context.Context, id domain.BeadID, force bool) error {
	if id == "" {
		return errors.New("gitx: Remove: empty bead id")
	}
	path := m.PathFor(id)
	all, err := m.listAll(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, wt := range all {
		if samePath(wt.Path, path) {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: no worktree registered at %s", ErrWorktreeMissing, path)
	}
	args := []string{"worktree", "remove"}
	if force {
		// Twice: once for a dirty tree, once for a locked one.
		args = append(args, "--force", "--force")
	}
	args = append(args, path)
	if _, err := m.repo.Git(ctx, args...); err != nil {
		var ge *GitError
		if errors.As(err, &ge) && strings.Contains(ge.Stderr, "--force") {
			ge.Cause = ErrWorktreeDirty
		}
		return err
	}
	return nil
}

// DeleteBranch deletes the bead's branch from the main repository with
// "git branch -D". It is meant for cleanup after the work landed or was
// abandoned; the worktree must have been removed first (git refuses to
// delete a checked-out branch). A branch that no longer exists is not an
// error.
func (m *Manager) DeleteBranch(ctx context.Context, bead domain.Bead) error {
	branch := BranchName(bead)
	exists, err := m.repo.BranchExists(ctx, branch)
	if err != nil || !exists {
		return err
	}
	_, err = m.repo.Git(ctx, "branch", "-D", "--quiet", branch)
	return err
}

// Prune drops registrations whose directories no longer exist
// ("git worktree prune") and removes empty leftover directories under Dir.
// The git command is repository-wide: it also drops the stale
// registrations of worktrees the user keeps outside the run directory. Use
// it only when that is intended; the run-finish pass prunes just its own
// (see pruneRun).
func (m *Manager) Prune(ctx context.Context) error {
	if _, err := m.repo.Git(ctx, "worktree", "prune"); err != nil {
		return err
	}
	return m.removeHusks()
}

// removeHusks deletes the empty leftover directories under Dir, the husks
// an interrupted removal leaves behind. Non-empty directories are kept.
func (m *Manager) removeHusks() error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("gitx: reading %s: %w", m.dir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Remove fails on a non-empty directory, which is what we want:
		// only husks left by an interrupted removal go.
		_ = os.Remove(filepath.Join(m.dir, e.Name()))
	}
	return nil
}

// baseRevision returns the revision new branches start from: the default
// branch (see [Repo.DefaultBranch]) as a local branch when it exists,
// otherwise its remote-tracking ref.
func (m *Manager) baseRevision(ctx context.Context) (string, error) {
	name, err := m.repo.DefaultBranch(ctx)
	if err != nil {
		return "", err
	}
	ok, err := m.repo.BranchExists(ctx, name)
	if err != nil {
		return "", err
	}
	if ok {
		return name, nil
	}
	remote, err := m.repo.DefaultRemote(ctx)
	if err != nil {
		return "", err
	}
	return remote + "/" + name, nil
}

// worktreeEntry is one block of "git worktree list --porcelain".
type worktreeEntry struct {
	Worktree
	prunableReason string
}

// listAll parses every registered worktree of the repository, including
// the main working tree.
func (m *Manager) listAll(ctx context.Context) ([]worktreeEntry, error) {
	out, err := m.repo.Git(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreeList(out), nil
}

// parseWorktreeList parses the porcelain format: blocks separated by blank
// lines, each starting with "worktree <path>" followed by attribute lines
// ("HEAD <sha>", "branch refs/heads/<name>", "detached", "bare",
// "locked [reason]", "prunable [reason]").
func parseWorktreeList(out string) []worktreeEntry {
	var entries []worktreeEntry
	var cur *worktreeEntry
	flush := func() {
		if cur != nil {
			entries = append(entries, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			flush()
			cur = &worktreeEntry{Worktree: Worktree{Path: filepath.Clean(value)}}
		case "HEAD":
			if cur != nil {
				cur.Head = value
			}
		case "branch":
			if cur != nil {
				cur.Branch = strings.TrimPrefix(value, "refs/heads/")
			}
		case "prunable":
			if cur != nil {
				cur.Prunable = true
				cur.prunableReason = value
			}
		}
	}
	flush()
	return entries
}

// checkPathFree reports ErrPathOccupied unless path is absent or an empty
// directory (git worktree add accepts an empty directory).
func checkPathFree(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("gitx: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrPathOccupied, path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("gitx: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: %s is not empty", ErrPathOccupied, path)
	}
	return nil
}

// canonical resolves symlinks in path as far as it exists, so a path git
// printed (always resolved) compares equal to one we built from a
// symlinked temp directory.
func canonical(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	// Resolve the longest existing prefix and re-append the rest.
	dir, base := filepath.Split(path)
	dir = filepath.Clean(dir)
	if dir == path {
		return path
	}
	return filepath.Join(canonical(dir), base)
}

// samePath reports whether a and b name the same location.
func samePath(a, b string) bool {
	return canonical(a) == canonical(b)
}

// isWithin reports whether path is parent itself or lies below it.
func isWithin(parent, path string) bool {
	rel, err := filepath.Rel(canonical(parent), canonical(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
