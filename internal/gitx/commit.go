package gitx

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"slop-cannon/internal/domain"
)

// Fallback identity for auto-commits when git cannot determine one (no
// user.name/user.email, no GIT_* identity variables, and no usable
// auto-detected address). Engineers' own commits are unaffected: this only
// applies to the leftovers commit slop-cannon makes itself.
const (
	FallbackAuthorName  = "slop-cannon"
	FallbackAuthorEmail = "slop-cannon@localhost"
)

// LeftoverMessage returns the conventional commit message of an
// auto-commit made after an engineer session (D-14):
// "sc(<bead-id>): round <n> leftovers".
func LeftoverMessage(id domain.BeadID, round int) string {
	return fmt.Sprintf("sc(%s): round %d leftovers", id, round)
}

// Commit describes the outcome of [Repo.CommitAll].
type Commit struct {
	// Committed is true when a new commit was made; false when the working
	// tree was already clean and nothing was done.
	Committed bool
	// SHA is HEAD after the call: the new commit, or the existing HEAD when
	// nothing was committed. It is empty on an unborn branch.
	SHA string
	// Message is the commit message used (empty when nothing was committed).
	Message string
	// Paths lists the working-tree paths that were committed, relative to
	// the repository root, in git status order; a rename is listed under
	// its new name. Nil when nothing was committed.
	Paths []string
}

// FileStat is the change to one file in a [DiffStats].
type FileStat struct {
	// Path is the file's path relative to the repository root (the new
	// path for a rename).
	Path string
	// OldPath is the previous path when git detected a rename, else empty.
	OldPath string
	// Insertions and Deletions count changed lines; both are zero for a
	// binary file.
	Insertions int
	Deletions  int
	// Binary is set when git could not count lines for the file.
	Binary bool
}

// DiffStats summarises what a branch changed relative to the merge-base
// with its base branch: the same numbers "git diff --shortstat" prints,
// plus the per-file breakdown of "git diff --numstat" with rename
// detection.
type DiffStats struct {
	// Base is the merge-base commit the diff starts from.
	Base string
	// Head is the commit the diff ends at.
	Head string
	// Files is the number of changed files (including binary ones).
	Files int
	// Insertions is the total number of added lines.
	Insertions int
	// Deletions is the total number of removed lines.
	Deletions int
	// Changed lists every changed file in git's order.
	Changed []FileStat
}

// Empty reports whether nothing changed between Base and Head.
func (s DiffStats) Empty() bool { return s.Files == 0 }

// Lines returns the total number of changed lines (insertions plus
// deletions), the quantity D-09's re-review threshold is measured in.
func (s DiffStats) Lines() int { return s.Insertions + s.Deletions }

// String renders the stats the way "git diff --shortstat" does, for example
// "3 files changed, 10 insertions(+), 2 deletions(-)"; "no changes" when
// empty.
func (s DiffStats) String() string {
	if s.Empty() {
		return "no changes"
	}
	parts := []string{fmt.Sprintf("%d %s changed", s.Files, plural(s.Files, "file", "files"))}
	if s.Insertions > 0 || s.Deletions == 0 {
		parts = append(parts, fmt.Sprintf("%d %s(+)", s.Insertions, plural(s.Insertions, "insertion", "insertions")))
	}
	if s.Deletions > 0 {
		parts = append(parts, fmt.Sprintf("%d %s(-)", s.Deletions, plural(s.Deletions, "deletion", "deletions")))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// CommitAll stages every change in the working tree ("git add -A", which
// honours .gitignore and .git/info/exclude) and commits it with message.
// A clean tree is not an error: the result has Committed false and SHA set
// to the current HEAD. The commit skips hooks and GPG signing because it is
// bookkeeping made on the engineer's behalf, not a commit the user
// authored; when git has no identity to commit with, [FallbackAuthorName]
// and [FallbackAuthorEmail] are used.
func (r *Repo) CommitAll(ctx context.Context, message string) (*Commit, error) {
	if strings.TrimSpace(message) == "" {
		return nil, errors.New("gitx: CommitAll: empty commit message")
	}
	paths, err := r.changedPaths(ctx)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		sha, err := r.HeadSHA(ctx)
		if err != nil && !errors.Is(err, ErrNoCommits) {
			return nil, err
		}
		return &Commit{SHA: sha}, nil
	}
	if _, err := r.Git(ctx, "add", "-A", "--", "."); err != nil {
		return nil, err
	}
	committer, err := r.withIdentity(ctx)
	if err != nil {
		return nil, err
	}
	_, err = committer.Git(ctx, "commit", "--quiet", "--no-verify", "--no-gpg-sign", "-m", message)
	if err != nil {
		return nil, err
	}
	sha, err := r.HeadSHA(ctx)
	if err != nil {
		return nil, err
	}
	return &Commit{Committed: true, SHA: sha, Message: message, Paths: paths}, nil
}

// withIdentity returns r, or a copy of r whose git runs with the fallback
// author and committer identity when git cannot determine one itself.
func (r *Repo) withIdentity(ctx context.Context) (*Repo, error) {
	_, err := r.Git(ctx, "var", "GIT_COMMITTER_IDENT")
	if err == nil {
		return r, nil
	}
	if !isExit(err, 128) {
		return nil, err
	}
	env := map[string]string{}
	for k, v := range r.env {
		env[k] = v
	}
	env["GIT_AUTHOR_NAME"] = FallbackAuthorName
	env["GIT_AUTHOR_EMAIL"] = FallbackAuthorEmail
	env["GIT_COMMITTER_NAME"] = FallbackAuthorName
	env["GIT_COMMITTER_EMAIL"] = FallbackAuthorEmail
	c := *r
	c.env = env
	return &c, nil
}

// changedPaths lists the paths "git status --porcelain -z" reports as
// modified, staged, or untracked; renames are listed under the new name.
func (r *Repo) changedPaths(ctx context.Context) ([]string, error) {
	out, err := r.Git(ctx, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var paths []string
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 4 {
			continue
		}
		x, y, path := entry[0], entry[1], entry[3:]
		paths = append(paths, path)
		if x == 'R' || x == 'C' || y == 'R' || y == 'C' {
			i++ // the original path follows as its own field
		}
	}
	return paths, nil
}

// DiffStats measures what HEAD changed since it diverged from base (a
// branch, remote-tracking ref, or commit): the diff from the merge-base of
// base and HEAD to HEAD, with rename detection. It fails with
// [ErrUnknownRevision] when base does not resolve, [ErrNoMergeBase] when
// the histories are unrelated, and [ErrNoCommits] on an unborn branch.
func (r *Repo) DiffStats(ctx context.Context, base string) (*DiffStats, error) {
	head, err := r.HeadSHA(ctx)
	if err != nil {
		return nil, err
	}
	mb, err := r.MergeBase(ctx, base, head)
	if err != nil {
		return nil, err
	}
	return r.diffStatsBetween(ctx, mb, head)
}

// diffStatsBetween runs "git diff --numstat -M -z from to" and parses it.
func (r *Repo) diffStatsBetween(ctx context.Context, from, to string) (*DiffStats, error) {
	out, err := r.Git(ctx, "diff", "--numstat", "-M", "-z", from, to, "--")
	if err != nil {
		return nil, err
	}
	stats := &DiffStats{Base: from, Head: to}
	changed, err := parseNumstat(out)
	if err != nil {
		return nil, err
	}
	stats.Changed = changed
	for _, f := range changed {
		stats.Files++
		stats.Insertions += f.Insertions
		stats.Deletions += f.Deletions
	}
	return stats, nil
}

// parseNumstat parses NUL-terminated "git diff --numstat -z" output. Each
// entry is "<added>\t<deleted>\t<path>"; a rename is
// "<added>\t<deleted>\t" followed by the old and new paths as two further
// NUL-terminated fields; binary files report "-" for both counts.
func parseNumstat(out string) ([]FileStat, error) {
	if out == "" {
		return nil, nil
	}
	fields := strings.Split(out, "\x00")
	var files []FileStat
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("gitx: unexpected numstat entry %q", entry)
		}
		f := FileStat{Path: parts[2]}
		if parts[0] == "-" && parts[1] == "-" {
			f.Binary = true
		} else {
			var err error
			if f.Insertions, err = strconv.Atoi(parts[0]); err != nil {
				return nil, fmt.Errorf("gitx: unexpected numstat entry %q: %w", entry, err)
			}
			if f.Deletions, err = strconv.Atoi(parts[1]); err != nil {
				return nil, fmt.Errorf("gitx: unexpected numstat entry %q: %w", entry, err)
			}
		}
		if f.Path == "" {
			if i+2 >= len(fields) || fields[i+1] == "" || fields[i+2] == "" {
				return nil, fmt.Errorf("gitx: truncated numstat rename entry %q", entry)
			}
			f.OldPath = fields[i+1]
			f.Path = fields[i+2]
			i += 2
		}
		files = append(files, f)
	}
	return files, nil
}

// Leftovers is the outcome of [Manager.CommitLeftovers]: the auto-commit
// (if any) and the bead's cumulative diff stats against the default
// branch.
type Leftovers struct {
	Commit
	// Stats measures every commit on the bead's branch since it diverged
	// from the default branch, including the leftovers commit.
	Stats DiffStats
}

// CommitLeftovers implements D-14 for one bead: after an engineer session
// ends it commits whatever the engineer left uncommitted in the bead's
// worktree as "sc(<bead-id>): round <n> leftovers" (see [Repo.CommitAll];
// ignored files are never included), so testers always review a committed
// diff, and returns the [DiffStats] of the branch against its merge-base
// with the default branch. A clean worktree yields Committed false and the
// stats alone. It fails with [ErrWorktreeMissing] when the bead has no
// worktree.
func (m *Manager) CommitLeftovers(ctx context.Context, bead domain.Bead, round int) (*Leftovers, error) {
	if bead.ID == "" {
		return nil, errors.New("gitx: CommitLeftovers: bead has no id")
	}
	wt, err := m.Open(ctx, bead.ID)
	if err != nil {
		return nil, err
	}
	commit, err := wt.CommitAll(ctx, LeftoverMessage(bead.ID, round))
	if err != nil {
		return nil, err
	}
	base, err := m.baseRevision(ctx)
	if err != nil {
		return nil, err
	}
	stats, err := wt.DiffStats(ctx, base)
	if err != nil {
		return nil, err
	}
	return &Leftovers{Commit: *commit, Stats: *stats}, nil
}
