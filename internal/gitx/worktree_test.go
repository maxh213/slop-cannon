package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

// requireDeleteBranch deletes the bead's branch and fails the test when
// git refuses.
func requireDeleteBranch(t *testing.T, m *Manager, bead domain.Bead, msgAndArgs ...any) {
	t.Helper()
	_, err := m.DeleteBranch(context.Background(), bead)
	require.NoError(t, err, msgAndArgs...)
}

// wtBead is the bead most worktree tests create a worktree for.
var wtBead = domain.Bead{ID: "fx-a0u", Title: "Fix the mean() bug!"}

// wtFixture bundles a fixture repo, its opened Repo, and a Manager whose
// worktrees live under the D-23 run directory of a temporary state home.
type wtFixture struct {
	r      *testutil.FixtureRepo
	repo   *Repo
	m      *Manager
	runDir string
}

// wtSetup builds a fixture repo and a manager for run "run-1" with
// XDG_STATE_HOME pointed at a temporary directory, so nothing touches the
// real home.
func wtSetup(t *testing.T, fixtureOpts []testutil.FixtureOption, mgrOpts ...ManagerOption) *wtFixture {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	r := testutil.NewFixtureRepo(t, fixtureOpts...)
	repo := repoOpen(t, r)
	runDir := config.RunDir(repo.Root, "run-1")
	m, err := NewManager(repo, runDir, mgrOpts...)
	require.NoError(t, err)
	return &wtFixture{r: r, repo: repo, m: m, runDir: runDir}
}

// wtReopen simulates a process restart: a fresh Repo and Manager for the
// same repository and run directory, sharing nothing with the old ones.
func (f *wtFixture) wtReopen(t *testing.T) *Manager {
	t.Helper()
	repo := repoOpen(t, f.r)
	m, err := NewManager(repo, f.runDir)
	require.NoError(t, err)
	return m
}

// wtUserWorktree adds a worktree the user made themselves, outside the run
// directory, on a branch of their own.
func (f *wtFixture) wtUserWorktree(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "users-own")
	f.r.Git(t, "worktree", "add", "-q", dir, "-b", "feature/theirs")
	return dir
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Fix the mean() bug!":          "fix-the-mean-bug",
		"  --Leading and trailing--  ": "leading-and-trailing",
		"UPPER lower 123":              "upper-lower-123",
		"ünïcödé only ★":               "n-c-d-only",
		"":                             "",
		"!!!":                          "",
		"a" + strings.Repeat("b", 60):  "a" + strings.Repeat("b", MaxSlugLen-1),
		strings.Repeat("ab ", 30):      strings.Repeat("ab-", 13) + "a",
	}
	for in, want := range cases {
		assert.Equal(t, want, Slug(in), "Slug(%q)", in)
		assert.LessOrEqual(t, len(Slug(in)), MaxSlugLen)
	}
}

func TestBranchName(t *testing.T) {
	assert.Equal(t, "sc/fx-a0u-fix-the-mean-bug", BranchName(wtBead))
	assert.Equal(t, "sc/fx-dml.1", BranchName(domain.Bead{ID: "fx-dml.1", Title: "???"}))
	assert.Equal(t, "sc/odd_id-x", BranchName(domain.Bead{ID: "odd id", Title: "x"}), "ids are made path-safe")
}

func TestNewManager(t *testing.T) {
	ctx := context.Background()

	t.Run("refuses a linked worktree as the target", func(t *testing.T) {
		f := wtSetup(t, nil)
		dir := f.wtUserWorktree(t)
		linked, err := Open(ctx, dir, WithEnv(repoTestEnv(f.r)))
		require.NoError(t, err)
		require.True(t, linked.IsWorktree())
		_, err = NewManager(linked, f.runDir)
		require.ErrorIs(t, err, ErrNestedWorktree)
	})

	t.Run("refuses a run directory inside the repository", func(t *testing.T) {
		f := wtSetup(t, nil)
		_, err := NewManager(f.repo, filepath.Join(f.repo.Root, ".slop-cannon", "run"))
		require.ErrorIs(t, err, ErrWorktreeInsideRepo)
		_, err = NewManager(f.repo, f.repo.Root)
		require.ErrorIs(t, err, ErrWorktreeInsideRepo)
	})

	t.Run("refuses a relative run directory", func(t *testing.T) {
		f := wtSetup(t, nil)
		_, err := NewManager(f.repo, "relative/run")
		require.ErrorIs(t, err, ErrWorktreeInsideRepo)
	})

	t.Run("nil repo", func(t *testing.T) {
		_, err := NewManager(nil, t.TempDir())
		require.Error(t, err)
	})

	t.Run("paths follow D-23", func(t *testing.T) {
		f := wtSetup(t, nil)
		assert.Equal(t, filepath.Join(f.runDir, WorktreesDirName), f.m.Dir())
		assert.Equal(t, filepath.Join(f.runDir, "wt", "fx-a0u"), f.m.PathFor("fx-a0u"))
		assert.True(t, strings.HasPrefix(f.m.Dir(), os.Getenv("XDG_STATE_HOME")), "worktrees live under the state home")
		assert.Same(t, f.repo, f.m.Repo())
		_, err := os.Stat(f.m.Dir())
		assert.True(t, os.IsNotExist(err), "NewManager creates nothing")
	})
}

func TestManager_Create(t *testing.T) {
	ctx := context.Background()

	t.Run("creates a linked worktree on a new branch outside the repo", func(t *testing.T) {
		f := wtSetup(t, nil)
		head := f.r.Head(t)

		wt, err := f.m.Create(ctx, wtBead)
		require.NoError(t, err)
		assert.Equal(t, wtBead.ID, wt.BeadID)
		assert.Equal(t, f.m.PathFor(wtBead.ID), wt.Path)
		assert.Equal(t, "sc/fx-a0u-fix-the-mean-bug", wt.Branch)
		assert.Equal(t, head, wt.Head, "the branch starts at the default branch")
		assert.False(t, wt.Prunable)
		assert.DirExists(t, wt.Path)
		assert.FileExists(t, filepath.Join(wt.Path, "go.mod"), "the fixture is checked out")

		// Nothing of ours inside the target repository (D-23).
		assert.Empty(t, f.r.Status(t), "the target repository stays clean")
		assert.Equal(t, "main", f.r.Git(t, "symbolic-ref", "--short", "HEAD"), "the target's checkout is untouched")

		// The worktree is a real linked worktree sharing the main .git.
		inner, err := f.m.Open(ctx, wtBead.ID)
		require.NoError(t, err)
		assert.True(t, inner.IsWorktree())
		repoSamePath(t, f.repo.GitDir, inner.CommonDir)
		branch, err := inner.CurrentBranch(ctx)
		require.NoError(t, err)
		assert.Equal(t, wt.Branch, branch)
		ok, err := f.repo.BranchExists(ctx, wt.Branch)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("refuses a dirty target repository", func(t *testing.T) {
		f := wtSetup(t, nil)
		f.r.WriteFile(t, "notes.txt", "uncommitted\n")
		_, err := f.m.Create(ctx, wtBead)
		require.ErrorIs(t, err, ErrRepoDirty)
		_, statErr := os.Stat(f.m.PathFor(wtBead.ID))
		assert.True(t, os.IsNotExist(statErr), "nothing is created on refusal")
		list, err := f.m.List(ctx)
		require.NoError(t, err)
		assert.Empty(t, list)
	})

	t.Run("AllowDirty overrides the dirty check", func(t *testing.T) {
		f := wtSetup(t, nil, AllowDirty())
		f.r.WriteFile(t, "notes.txt", "uncommitted\n")
		wt, err := f.m.Create(ctx, wtBead)
		require.NoError(t, err)
		assert.NoFileExists(t, filepath.Join(wt.Path, "notes.txt"), "untracked files of the target do not leak in")
	})

	t.Run("refuses a second worktree for the same bead", func(t *testing.T) {
		f := wtSetup(t, nil)
		_, err := f.m.Create(ctx, wtBead)
		require.NoError(t, err)
		_, err = f.m.Create(ctx, wtBead)
		require.ErrorIs(t, err, ErrWorktreeExists)
	})

	t.Run("refuses an occupied path", func(t *testing.T) {
		f := wtSetup(t, nil)
		path := f.m.PathFor(wtBead.ID)
		require.NoError(t, os.MkdirAll(path, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(path, "junk"), []byte("x"), 0o644))
		_, err := f.m.Create(ctx, wtBead)
		require.ErrorIs(t, err, ErrPathOccupied)

		require.NoError(t, os.RemoveAll(path))
		require.NoError(t, os.WriteFile(path, []byte("file"), 0o644))
		_, err = f.m.Create(ctx, wtBead)
		require.ErrorIs(t, err, ErrPathOccupied)
	})

	t.Run("accepts an empty directory at the path", func(t *testing.T) {
		f := wtSetup(t, nil)
		require.NoError(t, os.MkdirAll(f.m.PathFor(wtBead.ID), 0o755))
		_, err := f.m.Create(ctx, wtBead)
		require.NoError(t, err)
	})

	t.Run("refuses to nest inside another worktree", func(t *testing.T) {
		f := wtSetup(t, nil)
		theirs := f.wtUserWorktree(t)
		m, err := NewManager(f.repo, filepath.Join(theirs, "run"))
		require.NoError(t, err, "the user's worktree is outside the main repo, so construction passes")
		_, err = m.Create(ctx, wtBead)
		require.ErrorIs(t, err, ErrNestedWorktree)
	})

	t.Run("refuses when the branch is checked out elsewhere", func(t *testing.T) {
		f := wtSetup(t, nil)
		f.r.Git(t, "checkout", "-q", "-b", BranchName(wtBead))
		_, err := f.m.Create(ctx, wtBead)
		require.ErrorIs(t, err, ErrBranchInUse)
	})

	t.Run("reuses an existing branch", func(t *testing.T) {
		f := wtSetup(t, nil)
		branch := BranchName(wtBead)
		f.r.Git(t, "checkout", "-q", "-b", branch)
		f.r.WriteFile(t, "earlier.txt", "from an interrupted attempt\n")
		earlier := f.r.Commit(t, "earlier work")
		f.r.Git(t, "checkout", "-q", "main")

		wt, err := f.m.Create(ctx, wtBead)
		require.NoError(t, err)
		assert.Equal(t, branch, wt.Branch)
		assert.Equal(t, earlier, wt.Head, "the branch is checked out as is, not recreated")
		assert.FileExists(t, filepath.Join(wt.Path, "earlier.txt"))
	})

	t.Run("bases on the remote default branch when no local one exists", func(t *testing.T) {
		f := wtSetup(t, []testutil.FixtureOption{testutil.WithBareRemote()})
		f.r.Git(t, "remote", "set-head", "origin", "main")
		f.r.Git(t, "checkout", "-q", "-b", "elsewhere")
		f.r.Git(t, "branch", "-D", "-q", "main")
		remoteHead := f.r.Git(t, "rev-parse", "origin/main")

		wt, err := f.m.Create(ctx, wtBead)
		require.NoError(t, err)
		assert.Equal(t, remoteHead, wt.Head)
	})

	t.Run("no default branch", func(t *testing.T) {
		f := wtSetup(t, nil)
		f.r.Git(t, "checkout", "-q", "-b", "trunk")
		f.r.Git(t, "branch", "-D", "-q", "main")
		_, err := f.m.Create(ctx, wtBead)
		require.ErrorIs(t, err, ErrNoDefaultBranch)
	})

	t.Run("empty bead id", func(t *testing.T) {
		f := wtSetup(t, nil)
		_, err := f.m.Create(ctx, domain.Bead{Title: "no id"})
		require.Error(t, err)
	})
}

func TestManager_List(t *testing.T) {
	ctx := context.Background()
	f := wtSetup(t, nil)

	list, err := f.m.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list, "nothing before the first Create")

	f.wtUserWorktree(t)
	second := domain.Bead{ID: "fx-0uu", Title: "Fix max"}
	_, err = f.m.Create(ctx, wtBead)
	require.NoError(t, err)
	_, err = f.m.Create(ctx, second)
	require.NoError(t, err)

	list, err = f.m.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2, "the main tree and the user's own worktree are not listed")
	assert.Equal(t, second.ID, list[0].BeadID, "sorted by bead id")
	assert.Equal(t, wtBead.ID, list[1].BeadID)
	assert.Equal(t, f.m.PathFor(second.ID), list[0].Path)
	assert.Equal(t, "sc/fx-0uu-fix-max", list[0].Branch)
	assert.Equal(t, f.r.Head(t), list[0].Head)

	// A different run directory sees none of them.
	other, err := NewManager(f.repo, config.RunDir(f.repo.Root, "run-2"))
	require.NoError(t, err)
	list, err = other.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestManager_Remove(t *testing.T) {
	ctx := context.Background()

	t.Run("removes a clean worktree and keeps the branch", func(t *testing.T) {
		f := wtSetup(t, nil)
		wt, err := f.m.Create(ctx, wtBead)
		require.NoError(t, err)
		require.NoError(t, f.m.Remove(ctx, wtBead.ID, false))
		assert.NoDirExists(t, wt.Path)
		list, err := f.m.List(ctx)
		require.NoError(t, err)
		assert.Empty(t, list)
		ok, err := f.repo.BranchExists(ctx, wt.Branch)
		require.NoError(t, err)
		assert.True(t, ok, "the branch may still be under review or awaiting merge")

		requireDeleteBranch(t, f.m, wtBead)
		ok, err = f.repo.BranchExists(ctx, wt.Branch)
		require.NoError(t, err)
		assert.False(t, ok)
		requireDeleteBranch(t, f.m, wtBead, "deleting an absent branch is not an error")
	})

	t.Run("refuses a dirty worktree unless forced", func(t *testing.T) {
		f := wtSetup(t, nil)
		wt, err := f.m.Create(ctx, wtBead)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(wt.Path, "scratch.txt"), []byte("wip\n"), 0o644))

		err = f.m.Remove(ctx, wtBead.ID, false)
		require.ErrorIs(t, err, ErrWorktreeDirty)
		assert.DirExists(t, wt.Path)

		require.NoError(t, f.m.Remove(ctx, wtBead.ID, true))
		assert.NoDirExists(t, wt.Path)
	})

	t.Run("missing worktree", func(t *testing.T) {
		f := wtSetup(t, nil)
		require.ErrorIs(t, f.m.Remove(ctx, wtBead.ID, false), ErrWorktreeMissing)
		require.Error(t, f.m.Remove(ctx, "", false))
	})

	t.Run("cannot delete a checked-out branch", func(t *testing.T) {
		f := wtSetup(t, nil)
		_, err := f.m.Create(ctx, wtBead)
		require.NoError(t, err)
		_, err = f.m.DeleteBranch(ctx, wtBead)
		var ge *GitError
		require.ErrorAs(t, err, &ge)
	})
}

func TestManager_Prune(t *testing.T) {
	ctx := context.Background()
	f := wtSetup(t, nil)
	wt, err := f.m.Create(ctx, wtBead)
	require.NoError(t, err)

	// Simulate the state directory being wiped behind git's back.
	require.NoError(t, os.RemoveAll(wt.Path))
	list, err := f.m.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.True(t, list[0].Prunable)
	_, err = f.m.Reattach(ctx, wtBead.ID)
	require.ErrorIs(t, err, ErrWorktreeMissing)

	// A husk left by an interrupted removal is cleaned up too.
	require.NoError(t, os.MkdirAll(filepath.Join(f.m.Dir(), "fx-husk"), 0o755))

	require.NoError(t, f.m.Prune(ctx))
	list, err = f.m.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)
	assert.NoDirExists(t, filepath.Join(f.m.Dir(), "fx-husk"))

	// Create recovers from a stale registration on its own as well.
	_, err = f.m.Create(ctx, wtBead)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(f.m.PathFor(wtBead.ID)))
	again, err := f.m.Create(ctx, wtBead)
	require.NoError(t, err)
	assert.DirExists(t, again.Path)

	// Prune with no worktree directory at all is a no-op.
	empty, err := NewManager(f.repo, config.RunDir(f.repo.Root, "never-ran"))
	require.NoError(t, err)
	require.NoError(t, empty.Prune(ctx))
}

func TestManager_ReattachAfterRestart(t *testing.T) {
	ctx := context.Background()
	f := wtSetup(t, nil)

	created, err := f.m.Create(ctx, wtBead)
	require.NoError(t, err)
	// Some work happens in the worktree before the "crash".
	inner, err := f.m.Open(ctx, wtBead.ID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(inner.Root, "work.txt"), []byte("done\n"), 0o644))
	_, err = inner.Git(ctx, "add", "-A")
	require.NoError(t, err)
	_, err = inner.Git(ctx, "commit", "-q", "-m", "work in the worktree")
	require.NoError(t, err)
	workHead, err := inner.HeadSHA(ctx)
	require.NoError(t, err)

	// Restart: a brand-new Repo and Manager built only from the paths.
	m2 := f.wtReopen(t)
	wt, err := m2.Reattach(ctx, wtBead.ID)
	require.NoError(t, err)
	assert.Equal(t, created.Path, wt.Path)
	assert.Equal(t, created.Branch, wt.Branch)
	assert.Equal(t, wtBead.ID, wt.BeadID)
	assert.Equal(t, workHead, wt.Head, "the committed work survived")
	assert.NotEqual(t, created.Head, wt.Head)

	inner2, err := m2.Open(ctx, wtBead.ID)
	require.NoError(t, err)
	branch, err := inner2.CurrentBranch(ctx)
	require.NoError(t, err)
	assert.Equal(t, created.Branch, branch)
	clean, err := inner2.IsClean(ctx)
	require.NoError(t, err)
	assert.True(t, clean)
	assert.Equal(t, workHead, f.r.Git(t, "rev-parse", created.Branch), "the main repo sees the branch")
	assert.NotEqual(t, workHead, f.r.Head(t), "main is untouched")

	// Unknown beads and empty ids are refused.
	_, err = m2.Reattach(ctx, "fx-nope")
	require.ErrorIs(t, err, ErrWorktreeMissing)
	_, err = m2.Open(ctx, "fx-nope")
	require.ErrorIs(t, err, ErrWorktreeMissing)
	_, err = m2.Reattach(ctx, "")
	require.Error(t, err)
}

func TestParseWorktreeList(t *testing.T) {
	out := strings.Join([]string{
		"worktree /repo",
		"HEAD 1111111111111111111111111111111111111111",
		"branch refs/heads/main",
		"",
		"worktree /state/wt/fx-a0u",
		"HEAD 2222222222222222222222222222222222222222",
		"branch refs/heads/sc/fx-a0u-fix",
		"locked reason with spaces",
		"",
		"worktree /state/wt/fx-gone",
		"HEAD 3333333333333333333333333333333333333333",
		"detached",
		"prunable gitdir file points to non-existent location",
		"",
		"worktree /bare.git",
		"bare",
	}, "\n") + "\n"

	entries := parseWorktreeList(out)
	require.Len(t, entries, 4)
	assert.Equal(t, Worktree{Path: "/repo", Head: strings.Repeat("1", 40), Branch: "main"}, entries[0].Worktree)
	assert.Equal(t, "sc/fx-a0u-fix", entries[1].Branch)
	assert.Equal(t, "/state/wt/fx-gone", entries[2].Path)
	assert.Empty(t, entries[2].Branch, "detached HEAD has no branch")
	assert.True(t, entries[2].Prunable)
	assert.Equal(t, "gitdir file points to non-existent location", entries[2].prunableReason)
	assert.Equal(t, "/bare.git", entries[3].Path)
	assert.Empty(t, parseWorktreeList(""))
}

func TestWorktreePathHelpers(t *testing.T) {
	base := t.TempDir()
	assert.True(t, isWithin(base, base))
	assert.True(t, isWithin(base, filepath.Join(base, "a", "b")))
	assert.False(t, isWithin(base, filepath.Dir(base)))
	assert.False(t, isWithin(base, base+"-sibling"))
	assert.True(t, samePath(base, base+string(filepath.Separator)))

	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(base, link))
	assert.True(t, samePath(link, base))
	assert.True(t, isWithin(link, filepath.Join(base, "child", "not-yet-created")))
}
