package gitx

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

// acWrite writes content to rel under dir, creating parent directories.
func acWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// acFileStat finds the FileStat for path or fails the test.
func acFileStat(t *testing.T, stats *DiffStats, path string) FileStat {
	t.Helper()
	for _, f := range stats.Changed {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no FileStat for %q in %+v", path, stats.Changed)
	return FileStat{}
}

func TestLeftoverMessage(t *testing.T) {
	assert.Equal(t, "sc(fx-a0u): round 1 leftovers", LeftoverMessage("fx-a0u", 1))
	assert.Equal(t, "sc(fx-dml.1): round 3 leftovers", LeftoverMessage("fx-dml.1", 3))
}

func TestDiffStats_Summary(t *testing.T) {
	assert.Equal(t, "no changes", DiffStats{}.String())
	assert.True(t, DiffStats{}.Empty())
	s := DiffStats{Files: 1, Insertions: 1}
	assert.Equal(t, "1 file changed, 1 insertion(+)", s.String())
	s = DiffStats{Files: 3, Insertions: 10, Deletions: 2}
	assert.Equal(t, "3 files changed, 10 insertions(+), 2 deletions(-)", s.String())
	assert.Equal(t, 12, s.Lines())
	assert.False(t, s.Empty())
	s = DiffStats{Files: 2, Deletions: 1}
	assert.Equal(t, "2 files changed, 1 deletion(-)", s.String())
	s = DiffStats{Files: 1}
	assert.Equal(t, "1 file changed, 0 insertions(+)", s.String(), "a binary-only change still reports a count")
}

func TestParseNumstat(t *testing.T) {
	t.Run("plain, binary, and renamed entries", func(t *testing.T) {
		out := "-\t-\tbin.dat\x00" + "1\t0\t\x00f.txt\x00g h.txt\x00" + "3\t2\tnew.txt\x00"
		files, err := parseNumstat(out)
		require.NoError(t, err)
		require.Equal(t, []FileStat{
			{Path: "bin.dat", Binary: true},
			{Path: "g h.txt", OldPath: "f.txt", Insertions: 1},
			{Path: "new.txt", Insertions: 3, Deletions: 2},
		}, files)
	})

	t.Run("empty output", func(t *testing.T) {
		files, err := parseNumstat("")
		require.NoError(t, err)
		assert.Nil(t, files)
	})

	t.Run("malformed entries", func(t *testing.T) {
		_, err := parseNumstat("garbage\x00")
		require.Error(t, err)
		_, err = parseNumstat("x\ty\tpath\x00")
		require.Error(t, err)
		_, err = parseNumstat("1\t0\t\x00old\x00")
		require.Error(t, err, "rename with a missing new path")
	})
}

func TestRepo_CommitAll(t *testing.T) {
	ctx := context.Background()

	t.Run("commits modified, new, and renamed files but not ignored ones", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		repo := repoOpen(t, r)
		before := r.Head(t)
		r.WriteFile(t, "main.go", "package main\n\nfunc main() {}\n")
		r.WriteFile(t, "notes/todo.txt", "later\n")
		r.WriteFile(t, "fixture", "built binary, matched by /fixture in .gitignore\n")
		r.Git(t, "mv", "README.md", "docs.md")

		c, err := repo.CommitAll(ctx, "sc(fx-a0u): round 1 leftovers")
		require.NoError(t, err)
		assert.True(t, c.Committed)
		assert.Equal(t, "sc(fx-a0u): round 1 leftovers", c.Message)
		assert.NotEqual(t, before, c.SHA)
		assert.Equal(t, r.Head(t), c.SHA)
		assert.ElementsMatch(t, []string{"docs.md", "main.go", "notes/todo.txt"}, c.Paths)

		assert.Equal(t, "sc(fx-a0u): round 1 leftovers", r.Log(t)[0])
		assert.Empty(t, r.Status(t), "the tree is clean afterwards")
		assert.NotContains(t, r.Git(t, "ls-files"), "fixture", "ignored files are not committed")
		assert.FileExists(t, filepath.Join(r.Dir, "fixture"), "ignored files are left in place")
		assert.Equal(t, "1", r.Git(t, "rev-list", "--count", before+".."+c.SHA), "exactly one commit was added")
	})

	t.Run("commits deletions and paths git would otherwise quote", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		repo := repoOpen(t, r)
		require.NoError(t, os.Remove(filepath.Join(r.Dir, "README.md")))
		acWrite(t, r.Dir, "notes/a note.txt", "spaces\n")
		acWrite(t, r.Dir, "notes/café.txt", "unicode\n")

		c, err := repo.CommitAll(ctx, "leftovers")
		require.NoError(t, err)
		assert.True(t, c.Committed)
		assert.ElementsMatch(t, []string{"README.md", "notes/a note.txt", "notes/café.txt"}, c.Paths,
			"paths are reported raw, never in git's quoted form")
		assert.Empty(t, r.Status(t))
		tracked := r.Git(t, "ls-files", "-z")
		assert.NotContains(t, tracked, "README.md", "the deletion was committed")
		assert.Contains(t, tracked, "notes/café.txt")
	})

	t.Run("clean tree makes no commit", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		repo := repoOpen(t, r)
		r.WriteFile(t, "fixture", "ignored\n")
		head := r.Head(t)

		c, err := repo.CommitAll(ctx, "sc(fx-a0u): round 1 leftovers")
		require.NoError(t, err)
		assert.False(t, c.Committed)
		assert.Equal(t, head, c.SHA)
		assert.Empty(t, c.Message)
		assert.Nil(t, c.Paths)
		assert.Equal(t, head, r.Head(t))
		assert.Len(t, r.Log(t), 1)
	})

	t.Run("rejects an empty message", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		repo := repoOpen(t, r)
		r.WriteFile(t, "x.txt", "x\n")
		_, err := repo.CommitAll(ctx, "  ")
		require.Error(t, err)
		assert.NotEmpty(t, r.Status(t), "nothing was staged or committed")
	})

	t.Run("falls back to a fixed identity when git has none", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		env := repoTestEnv(r)
		for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
			env[k] = ""
		}
		repo, err := Open(ctx, r.Dir, WithEnv(env))
		require.NoError(t, err)
		_, err = repo.Git(ctx, "var", "GIT_COMMITTER_IDENT")
		require.Error(t, err, "precondition: git has no identity in this environment")

		r.WriteFile(t, "x.txt", "x\n")
		c, err := repo.CommitAll(ctx, "leftovers")
		require.NoError(t, err)
		assert.True(t, c.Committed)
		assert.Equal(t, FallbackAuthorName+" <"+FallbackAuthorEmail+">", r.Git(t, "log", "-1", "--format=%an <%ae>"))
		assert.Equal(t, FallbackAuthorName+" <"+FallbackAuthorEmail+">", r.Git(t, "log", "-1", "--format=%cn <%ce>"))
	})

	t.Run("skips hooks and gpg signing", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		repo := repoOpen(t, r)
		hooks := filepath.Join(r.Dir, ".git", "hooks")
		require.NoError(t, os.MkdirAll(hooks, 0o755))
		for _, hook := range []string{"pre-commit", "commit-msg"} {
			require.NoError(t, os.WriteFile(filepath.Join(hooks, hook), []byte("#!/bin/sh\necho hook ran >&2\nexit 1\n"), 0o755))
		}
		r.Git(t, "config", "commit.gpgsign", "true")
		r.Git(t, "config", "user.signingkey", "0000000000000000")
		r.WriteFile(t, "x.txt", "x\n")
		_, err := repo.Git(ctx, "commit", "-q", "-a", "-m", "blocked by hook")
		require.Error(t, err, "precondition: a plain commit is blocked by the hook or the signing key")
		assert.NotEmpty(t, r.Status(t))

		c, err := repo.CommitAll(ctx, "leftovers")
		require.NoError(t, err)
		assert.True(t, c.Committed)
		assert.Equal(t, "leftovers", r.Git(t, "log", "-1", "--format=%s"))
		assert.Equal(t, "N", r.Git(t, "log", "-1", "--format=%G?"), "the commit is unsigned")
	})

	t.Run("unborn branch", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		dir := repoInitEmpty(t, r)
		repo, err := Open(ctx, dir, WithEnv(repoTestEnv(r)))
		require.NoError(t, err)

		c, err := repo.CommitAll(ctx, "leftovers")
		require.NoError(t, err)
		assert.False(t, c.Committed)
		assert.Empty(t, c.SHA)

		acWrite(t, dir, "first.txt", "hello\n")
		c, err = repo.CommitAll(ctx, "leftovers")
		require.NoError(t, err)
		assert.True(t, c.Committed)
		assert.Equal(t, []string{"first.txt"}, c.Paths)
		sha, err := repo.HeadSHA(ctx)
		require.NoError(t, err)
		assert.Equal(t, sha, c.SHA, "the root commit was made")
	})
}

func TestRepo_DiffStats(t *testing.T) {
	ctx := context.Background()

	t.Run("measures the branch against its merge-base", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		repo := repoOpen(t, r)
		base := r.Head(t)
		r.Git(t, "checkout", "-q", "-b", "sc/fx-a0u-work")
		r.WriteFile(t, "main.go", "package main\n\nfunc main() {}\n")
		r.WriteFile(t, "notes/todo.txt", "one\ntwo\nthree\n")
		r.WriteFile(t, "blob.bin", "\x00\x01\x02")
		r.Git(t, "mv", "README.md", "docs.md")
		r.Commit(t, "work")
		head := r.Head(t)

		stats, err := repo.DiffStats(ctx, "main")
		require.NoError(t, err)
		assert.Equal(t, base, stats.Base)
		assert.Equal(t, head, stats.Head)
		assert.False(t, stats.Empty())
		assert.Equal(t, 4, stats.Files)
		assert.Len(t, stats.Changed, 4)

		todo := acFileStat(t, stats, "notes/todo.txt")
		assert.Equal(t, FileStat{Path: "notes/todo.txt", Insertions: 3}, todo)
		docs := acFileStat(t, stats, "docs.md")
		assert.Equal(t, "README.md", docs.OldPath, "renames are detected")
		assert.Zero(t, docs.Insertions+docs.Deletions, "a pure rename changes no lines")
		blob := acFileStat(t, stats, "blob.bin")
		assert.True(t, blob.Binary)
		mainGo := acFileStat(t, stats, "main.go")
		assert.Positive(t, mainGo.Insertions+mainGo.Deletions)

		total := 0
		for _, f := range stats.Changed {
			total += f.Insertions + f.Deletions
		}
		assert.Equal(t, total, stats.Lines())
		assert.Equal(t, stats.Insertions, todo.Insertions+docs.Insertions+mainGo.Insertions)
		assert.Equal(t, stats.Deletions, todo.Deletions+docs.Deletions+mainGo.Deletions)

		// The base moving on does not change the branch's own stats.
		r.Git(t, "checkout", "-q", "main")
		r.WriteFile(t, "main-only.txt", "on main\n")
		r.Commit(t, "main moves on")
		r.Git(t, "checkout", "-q", "sc/fx-a0u-work")
		again, err := repo.DiffStats(ctx, "main")
		require.NoError(t, err)
		assert.Equal(t, stats, again)
	})

	t.Run("counts deleted files", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		repo := repoOpen(t, r)
		r.Git(t, "checkout", "-q", "-b", "sc/fx-a0u-del")
		require.NoError(t, os.Remove(filepath.Join(r.Dir, "README.md")))
		r.Commit(t, "drop the readme")

		stats, err := repo.DiffStats(ctx, "main")
		require.NoError(t, err)
		require.Equal(t, 1, stats.Files)
		deleted := acFileStat(t, stats, "README.md")
		assert.Zero(t, deleted.Insertions)
		assert.Positive(t, deleted.Deletions)
		assert.Equal(t, deleted.Deletions, stats.Deletions)
		assert.Equal(t, deleted.Deletions, stats.Lines())
	})

	t.Run("no divergence", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		repo := repoOpen(t, r)
		stats, err := repo.DiffStats(ctx, "main")
		require.NoError(t, err)
		assert.True(t, stats.Empty())
		assert.Equal(t, "no changes", stats.String())
		assert.Equal(t, r.Head(t), stats.Base)
		assert.Equal(t, r.Head(t), stats.Head)
		assert.Empty(t, stats.Changed)
	})

	t.Run("uncommitted changes do not count", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		repo := repoOpen(t, r)
		r.WriteFile(t, "dirty.txt", "not committed\n")
		stats, err := repo.DiffStats(ctx, "main")
		require.NoError(t, err)
		assert.True(t, stats.Empty())
	})

	t.Run("unknown base", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		repo := repoOpen(t, r)
		_, err := repo.DiffStats(ctx, "no-such-branch")
		require.ErrorIs(t, err, ErrUnknownRevision)
	})

	t.Run("unborn branch", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		repo, err := Open(ctx, repoInitEmpty(t, r), WithEnv(repoTestEnv(r)))
		require.NoError(t, err)
		_, err = repo.DiffStats(ctx, "main")
		require.ErrorIs(t, err, ErrNoCommits)
	})
}

func TestManager_CommitLeftovers(t *testing.T) {
	ctx := context.Background()

	t.Run("dirty worktree is committed and measured", func(t *testing.T) {
		f := wtSetup(t, nil)
		wt, err := f.m.Create(ctx, wtBead)
		require.NoError(t, err)
		mainHead := f.r.Head(t)

		acWrite(t, wt.Path, "main.go", "package main\n\nfunc main() {}\n")
		acWrite(t, wt.Path, "stats/extra.go", "package stats\n")
		acWrite(t, wt.Path, "fixture", "ignored build output\n")

		left, err := f.m.CommitLeftovers(ctx, wtBead, 2)
		require.NoError(t, err)
		assert.True(t, left.Committed)
		assert.Equal(t, "sc(fx-a0u): round 2 leftovers", left.Message)
		assert.ElementsMatch(t, []string{"main.go", "stats/extra.go"}, left.Paths)
		assert.Equal(t, left.SHA, left.Stats.Head)
		assert.Equal(t, mainHead, left.Stats.Base)
		assert.Equal(t, 2, left.Stats.Files)
		assert.Equal(t, 1, acFileStat(t, &left.Stats, "stats/extra.go").Insertions)
		for _, fs := range left.Stats.Changed {
			assert.NotEqual(t, "fixture", fs.Path, "ignored files never reach the diff")
		}

		inner, err := f.m.Open(ctx, wtBead.ID)
		require.NoError(t, err)
		clean, err := inner.IsClean(ctx)
		require.NoError(t, err)
		assert.True(t, clean, "the worktree is clean after the auto-commit")
		sha, err := inner.HeadSHA(ctx)
		require.NoError(t, err)
		assert.Equal(t, left.SHA, sha)
		assert.Equal(t, "sc(fx-a0u): round 2 leftovers", f.r.Git(t, "log", "-1", "--format=%s", "sc/fx-a0u-fix-the-mean-bug"))

		assert.Equal(t, mainHead, f.r.Head(t), "the default branch is untouched")
		assert.Empty(t, f.r.Status(t), "the target repository stays clean")
	})

	t.Run("clean worktree makes no commit but still reports stats", func(t *testing.T) {
		f := wtSetup(t, nil)
		wt, err := f.m.Create(ctx, wtBead)
		require.NoError(t, err)
		acWrite(t, wt.Path, "fixture", "ignored\n")

		left, err := f.m.CommitLeftovers(ctx, wtBead, 1)
		require.NoError(t, err)
		assert.False(t, left.Committed)
		assert.Equal(t, wt.Head, left.SHA)
		assert.True(t, left.Stats.Empty())

		// An engineer commit followed by a clean exit: stats cover the
		// branch, no leftovers commit is added.
		acWrite(t, wt.Path, "engineer.txt", "a\nb\n")
		inner, err := f.m.Open(ctx, wtBead.ID)
		require.NoError(t, err)
		c, err := inner.CommitAll(ctx, "feat: engineer's own commit")
		require.NoError(t, err)
		left, err = f.m.CommitLeftovers(ctx, wtBead, 1)
		require.NoError(t, err)
		assert.False(t, left.Committed)
		assert.Equal(t, c.SHA, left.SHA)
		assert.Equal(t, 1, left.Stats.Files)
		assert.Equal(t, 2, left.Stats.Insertions)
		assert.Equal(t, "feat: engineer's own commit", f.r.Git(t, "log", "-1", "--format=%s", "sc/fx-a0u-fix-the-mean-bug"))
	})

	t.Run("stats accumulate across rounds", func(t *testing.T) {
		f := wtSetup(t, nil)
		wt, err := f.m.Create(ctx, wtBead)
		require.NoError(t, err)
		acWrite(t, wt.Path, "a.txt", "1\n")
		first, err := f.m.CommitLeftovers(ctx, wtBead, 1)
		require.NoError(t, err)
		acWrite(t, wt.Path, "b.txt", "1\n2\n")
		second, err := f.m.CommitLeftovers(ctx, wtBead, 2)
		require.NoError(t, err)
		assert.True(t, second.Committed)
		assert.Equal(t, first.Stats.Base, second.Stats.Base)
		assert.Equal(t, 2, second.Stats.Files)
		assert.Equal(t, 3, second.Stats.Insertions)
		assert.Equal(t, "2", f.r.Git(t, "rev-list", "--count", "main..sc/fx-a0u-fix-the-mean-bug"))
	})

	t.Run("works with a remote-tracking default branch", func(t *testing.T) {
		f := wtSetup(t, []testutil.FixtureOption{testutil.WithBareRemote()})
		wt, err := f.m.Create(ctx, wtBead)
		require.NoError(t, err)
		acWrite(t, wt.Path, "a.txt", "1\n")
		left, err := f.m.CommitLeftovers(ctx, wtBead, 1)
		require.NoError(t, err)
		assert.True(t, left.Committed)
		assert.Equal(t, 1, left.Stats.Files)
	})

	t.Run("missing worktree", func(t *testing.T) {
		f := wtSetup(t, nil)
		_, err := f.m.CommitLeftovers(ctx, wtBead, 1)
		require.ErrorIs(t, err, ErrWorktreeMissing)
		_, err = f.m.CommitLeftovers(ctx, domain.Bead{}, 1)
		require.Error(t, err)
	})
}
