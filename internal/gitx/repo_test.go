package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/proc"
	"slop-cannon/internal/testutil"
)

// repoTestEnv converts the fixture's isolated git environment into the map
// form WithEnv takes, so gitx never reads the developer's own git config.
func repoTestEnv(r *testutil.FixtureRepo) map[string]string {
	env := map[string]string{}
	for _, kv := range r.Env() {
		k, v, _ := strings.Cut(kv, "=")
		if k != "PATH" {
			env[k] = v
		}
	}
	return env
}

// repoOpen opens the fixture with the isolated environment.
func repoOpen(t *testing.T, r *testutil.FixtureRepo) *Repo {
	t.Helper()
	repo, err := Open(context.Background(), r.Dir, WithEnv(repoTestEnv(r)))
	require.NoError(t, err)
	return repo
}

// repoSamePath compares two paths after resolving symlinks, so a temp dir
// under a symlinked /tmp still matches what git reports.
func repoSamePath(t *testing.T, want, got string) {
	t.Helper()
	w, err := filepath.EvalSymlinks(want)
	require.NoError(t, err)
	g, err := filepath.EvalSymlinks(got)
	require.NoError(t, err)
	assert.Equal(t, w, g)
}

// repoInitEmpty creates an empty repository (no commits) with the isolated
// environment of r and returns its path.
func repoInitEmpty(t *testing.T, r *testutil.FixtureRepo) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "empty")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	bin, err := testutil.RealBinary("git")
	require.NoError(t, err)
	cmd := exec.Command(bin, "init", "-q", "-b", "main")
	cmd.Dir = dir
	cmd.Env = r.Env()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return dir
}

func TestOpen(t *testing.T) {
	ctx := context.Background()

	t.Run("fixture without remote", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		repo := repoOpen(t, r)
		repoSamePath(t, r.Dir, repo.Root)
		repoSamePath(t, filepath.Join(r.Dir, ".git"), repo.GitDir)
		assert.Equal(t, repo.GitDir, repo.CommonDir)
		assert.False(t, repo.IsWorktree())
		assert.True(t, filepath.IsAbs(repo.Root))
	})

	t.Run("subdirectory resolves to the root", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		sub := filepath.Join(r.Dir, "nested", "deeper")
		require.NoError(t, os.MkdirAll(sub, 0o755))
		repo, err := Open(ctx, sub, WithEnv(repoTestEnv(r)))
		require.NoError(t, err)
		repoSamePath(t, r.Dir, repo.Root)
	})

	t.Run("linked worktree reports the common dir", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		wt := filepath.Join(t.TempDir(), "wt")
		r.Git(t, "worktree", "add", "-q", wt, "-b", "sc/wt-test")
		repo, err := Open(ctx, wt, WithEnv(repoTestEnv(r)))
		require.NoError(t, err)
		repoSamePath(t, wt, repo.Root)
		assert.True(t, repo.IsWorktree())
		repoSamePath(t, filepath.Join(r.Dir, ".git"), repo.CommonDir)
		assert.NotEqual(t, repo.GitDir, repo.CommonDir)
	})

	t.Run("not a repository", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		_, err := Open(ctx, t.TempDir(), WithEnv(repoTestEnv(r)))
		require.ErrorIs(t, err, ErrNotRepository)
		var ge *GitError
		require.ErrorAs(t, err, &ge)
		assert.Equal(t, 128, ge.ExitCode)
		assert.Contains(t, ge.Stderr, "not a git repository")
	})

	t.Run("bare repository is not a working tree", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t, testutil.WithBareRemote())
		_, err := Open(ctx, r.RemoteDir, WithEnv(repoTestEnv(r)))
		require.ErrorIs(t, err, ErrNotRepository)
	})

	t.Run("missing path", func(t *testing.T) {
		_, err := Open(ctx, filepath.Join(t.TempDir(), "nope"))
		require.Error(t, err)
		assert.ErrorIs(t, err, os.ErrNotExist)
		assert.NotErrorIs(t, err, ErrNotRepository)
	})

	t.Run("file path", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		_, err := Open(ctx, filepath.Join(r.Dir, "go.mod"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a directory")
	})

	t.Run("empty path means cwd", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		t.Chdir(r.Dir)
		repo, err := Open(ctx, "", WithEnv(repoTestEnv(r)))
		require.NoError(t, err)
		repoSamePath(t, r.Dir, repo.Root)
	})
}

func TestRepo_DefaultBranch(t *testing.T) {
	ctx := context.Background()

	t.Run("main without remote", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		branch, err := repoOpen(t, r).DefaultBranch(ctx)
		require.NoError(t, err)
		assert.Equal(t, "main", branch)
	})

	t.Run("main with remote but no remote HEAD", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t, testutil.WithBareRemote())
		branch, err := repoOpen(t, r).DefaultBranch(ctx)
		require.NoError(t, err)
		assert.Equal(t, "main", branch)
	})

	t.Run("remote HEAD wins over local main", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t, testutil.WithBareRemote())
		r.Git(t, "branch", "develop")
		r.Git(t, "push", "-q", "origin", "develop")
		r.Git(t, "remote", "set-head", "origin", "develop")
		branch, err := repoOpen(t, r).DefaultBranch(ctx)
		require.NoError(t, err)
		assert.Equal(t, "develop", branch)
	})

	t.Run("non-origin remote HEAD", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t, testutil.WithBareRemote())
		r.Git(t, "remote", "rename", "origin", "upstream")
		r.Git(t, "branch", "-m", "main", "trunk")
		r.Git(t, "push", "-q", "upstream", "trunk")
		r.Git(t, "remote", "set-head", "upstream", "trunk")
		branch, err := repoOpen(t, r).DefaultBranch(ctx)
		require.NoError(t, err)
		assert.Equal(t, "trunk", branch)
	})

	t.Run("master fallback", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		r.Git(t, "branch", "-m", "main", "master")
		branch, err := repoOpen(t, r).DefaultBranch(ctx)
		require.NoError(t, err)
		assert.Equal(t, "master", branch)
	})

	t.Run("main preferred over master", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		r.Git(t, "branch", "master")
		branch, err := repoOpen(t, r).DefaultBranch(ctx)
		require.NoError(t, err)
		assert.Equal(t, "main", branch)
	})

	t.Run("none", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		r.Git(t, "branch", "-m", "main", "trunk")
		_, err := repoOpen(t, r).DefaultBranch(ctx)
		require.ErrorIs(t, err, ErrNoDefaultBranch)
	})
}

func TestRepo_IsCleanAndStatus(t *testing.T) {
	ctx := context.Background()
	r := testutil.NewFixtureRepo(t)
	repo := repoOpen(t, r)

	clean, err := repo.IsClean(ctx)
	require.NoError(t, err)
	assert.True(t, clean, "fresh fixture is clean")

	r.WriteFile(t, "notes.txt", "untracked\n")
	clean, err = repo.IsClean(ctx)
	require.NoError(t, err)
	assert.False(t, clean, "untracked files count as dirty")
	lines, err := repo.Status(ctx)
	require.NoError(t, err)
	require.Len(t, lines, 1)
	assert.Equal(t, "?? notes.txt", lines[0])

	r.Commit(t, "add notes")
	clean, err = repo.IsClean(ctx)
	require.NoError(t, err)
	assert.True(t, clean)

	r.WriteFile(t, "notes.txt", "modified\n")
	lines, err = repo.Status(ctx)
	require.NoError(t, err)
	require.Len(t, lines, 1)
	assert.Equal(t, " M notes.txt", lines[0])

	r.WriteFile(t, ".gitignore", "ignored.txt\n")
	r.Commit(t, "ignore")
	r.WriteFile(t, "ignored.txt", "x\n")
	clean, err = repo.IsClean(ctx)
	require.NoError(t, err)
	assert.True(t, clean, "ignored files do not count")
}

func TestRepo_Remotes(t *testing.T) {
	ctx := context.Background()

	t.Run("without remote", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		repo := repoOpen(t, r)
		has, err := repo.HasRemote(ctx)
		require.NoError(t, err)
		assert.False(t, has)
		remotes, err := repo.Remotes(ctx)
		require.NoError(t, err)
		assert.Empty(t, remotes)
		_, err = repo.DefaultRemote(ctx)
		require.ErrorIs(t, err, ErrNoRemote)
	})

	t.Run("with remote", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t, testutil.WithBareRemote())
		repo := repoOpen(t, r)
		has, err := repo.HasRemote(ctx)
		require.NoError(t, err)
		assert.True(t, has)
		remotes, err := repo.Remotes(ctx)
		require.NoError(t, err)
		assert.Equal(t, []string{"origin"}, remotes)
		name, err := repo.DefaultRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, "origin", name)
	})

	t.Run("origin preferred, else first", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t, testutil.WithBareRemote())
		r.Git(t, "remote", "add", "alpha", r.RemoteDir)
		repo := repoOpen(t, r)
		name, err := repo.DefaultRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, "origin", name)

		r.Git(t, "remote", "remove", "origin")
		name, err = repo.DefaultRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, "alpha", name)
	})
}

func TestRepo_Fetch(t *testing.T) {
	ctx := context.Background()

	t.Run("without remote", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t)
		err := repoOpen(t, r).Fetch(ctx)
		require.ErrorIs(t, err, ErrNoRemote)
	})

	t.Run("with remote", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t, testutil.WithBareRemote())
		repo := repoOpen(t, r)

		r.WriteFile(t, "new.txt", "hello\n")
		head := r.Commit(t, "new file")
		r.Git(t, "push", "-q", "origin", "main")
		// Forget the remote-tracking ref locally so only a fetch can restore it.
		r.Git(t, "update-ref", "-d", "refs/remotes/origin/main")
		_, err := repo.RevParse(ctx, "origin/main")
		require.ErrorIs(t, err, ErrUnknownRevision)

		require.NoError(t, repo.Fetch(ctx))
		sha, err := repo.RevParse(ctx, "origin/main")
		require.NoError(t, err)
		assert.Equal(t, head, sha)
	})

	t.Run("prunes deleted remote branches", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t, testutil.WithBareRemote())
		repo := repoOpen(t, r)
		r.Git(t, "branch", "gone")
		r.Git(t, "push", "-q", "origin", "gone")
		_, err := repo.RevParse(ctx, "origin/gone")
		require.NoError(t, err)
		r.Git(t, "push", "-q", "origin", "--delete", "gone")
		require.NoError(t, repo.Fetch(ctx))
		_, err = repo.RevParse(ctx, "origin/gone")
		require.ErrorIs(t, err, ErrUnknownRevision)
	})

	t.Run("unreachable remote is a GitError", func(t *testing.T) {
		r := testutil.NewFixtureRepo(t, testutil.WithBareRemote())
		repo := repoOpen(t, r)
		require.NoError(t, os.RemoveAll(r.RemoteDir))
		err := repo.Fetch(ctx)
		var ge *GitError
		require.ErrorAs(t, err, &ge)
		assert.Equal(t, []string{"fetch", "--quiet", "--prune", "origin"}, ge.Args)
		assert.NotEqual(t, 0, ge.ExitCode)
		assert.NotEmpty(t, ge.Stderr)
		assert.Contains(t, err.Error(), "gitx: git fetch --quiet --prune origin: exit status")
	})
}

func TestRepo_HeadSHAAndRevParse(t *testing.T) {
	ctx := context.Background()
	r := testutil.NewFixtureRepo(t)
	repo := repoOpen(t, r)

	sha, err := repo.HeadSHA(ctx)
	require.NoError(t, err)
	assert.Equal(t, r.Head(t), sha)
	assert.Len(t, sha, 40)

	viaBranch, err := repo.RevParse(ctx, "main")
	require.NoError(t, err)
	assert.Equal(t, sha, viaBranch)

	short, err := repo.RevParse(ctx, sha[:8])
	require.NoError(t, err)
	assert.Equal(t, sha, short)

	_, err = repo.RevParse(ctx, "no-such-branch")
	require.ErrorIs(t, err, ErrUnknownRevision)
	var ge *GitError
	require.ErrorAs(t, err, &ge)
	assert.Equal(t, 1, ge.ExitCode)
	assert.Contains(t, err.Error(), "gitx: git rev-parse")
	assert.Contains(t, err.Error(), ErrUnknownRevision.Error())

	_, err = repo.RevParse(ctx, "--output=/etc/passwd")
	require.ErrorIs(t, err, ErrUnknownRevision, "option-looking revisions are never parsed as options")

	t.Run("unborn branch", func(t *testing.T) {
		dir := repoInitEmpty(t, r)
		empty, err := Open(ctx, dir, WithEnv(repoTestEnv(r)))
		require.NoError(t, err)
		_, err = empty.HeadSHA(ctx)
		require.ErrorIs(t, err, ErrNoCommits)
		branch, err := empty.CurrentBranch(ctx)
		require.NoError(t, err)
		assert.Equal(t, "main", branch)
		clean, err := empty.IsClean(ctx)
		require.NoError(t, err)
		assert.True(t, clean)
	})
}

func TestRepo_BranchesAndCurrentBranch(t *testing.T) {
	ctx := context.Background()
	r := testutil.NewFixtureRepo(t)
	repo := repoOpen(t, r)

	ok, err := repo.BranchExists(ctx, "main")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = repo.BranchExists(ctx, "feature")
	require.NoError(t, err)
	assert.False(t, ok)

	branch, err := repo.CurrentBranch(ctx)
	require.NoError(t, err)
	assert.Equal(t, "main", branch)

	r.Git(t, "checkout", "-q", "--detach", "HEAD")
	_, err = repo.CurrentBranch(ctx)
	require.ErrorIs(t, err, ErrDetachedHead)
}

func TestRepo_MergeBase(t *testing.T) {
	ctx := context.Background()
	r := testutil.NewFixtureRepo(t)
	repo := repoOpen(t, r)
	base := r.Head(t)

	r.Git(t, "checkout", "-q", "-b", "feature")
	r.WriteFile(t, "feature.txt", "f\n")
	featureHead := r.Commit(t, "feature work")
	r.Git(t, "checkout", "-q", "main")
	r.WriteFile(t, "main.txt", "m\n")
	mainHead := r.Commit(t, "main work")
	require.NotEqual(t, featureHead, mainHead)

	got, err := repo.MergeBase(ctx, "main", "feature")
	require.NoError(t, err)
	assert.Equal(t, base, got)

	got, err = repo.MergeBase(ctx, featureHead, mainHead)
	require.NoError(t, err)
	assert.Equal(t, base, got)

	got, err = repo.MergeBase(ctx, "main", "main")
	require.NoError(t, err)
	assert.Equal(t, mainHead, got)

	_, err = repo.MergeBase(ctx, "main", "no-such-branch")
	require.ErrorIs(t, err, ErrUnknownRevision)

	r.Git(t, "checkout", "-q", "--orphan", "island")
	r.Git(t, "rm", "-rfq", ".")
	r.WriteFile(t, "island.txt", "i\n")
	r.Commit(t, "island")
	_, err = repo.MergeBase(ctx, "main", "island")
	require.ErrorIs(t, err, ErrNoMergeBase)
}

func TestRepo_GitEnvironment(t *testing.T) {
	ctx := context.Background()
	r := testutil.NewFixtureRepo(t)
	t.Setenv("SC_GITX_SECRET_TOKEN", "hunter2")
	t.Setenv("GIT_GITX_TEST_MARKER", "passthrough")
	repo := repoOpen(t, r)

	out, err := repo.Git(ctx, "var", "GIT_AUTHOR_IDENT")
	require.NoError(t, err)
	assert.Contains(t, out, "Fixture Author <fixture@example.com>", "WithEnv identity reaches git")

	// A shim named git observes exactly the environment the child receives.
	// It is looked up through the same PATH the real git is, so re-open the
	// repo afterwards with the real binary untouched.
	shim := testutil.FakeBin(t, "git", `env | sort`)
	out, err = repo.Git(ctx, "status")
	require.NoError(t, err)
	assert.NotContains(t, out, "SC_GITX_SECRET_TOKEN=", "unlisted variables are withheld")
	assert.Contains(t, out, "GIT_GITX_TEST_MARKER=passthrough", "GIT_* variables pass through")
	assert.Contains(t, out, "LC_ALL=C")
	assert.Contains(t, out, "GIT_TERMINAL_PROMPT=0")
	assert.Contains(t, out, "GIT_OPTIONAL_LOCKS=0")
	assert.Contains(t, out, "HOME="+repoTestEnv(r)["HOME"]+"\n", "the fixture's isolated HOME wins over the parent's")
	assert.NotContains(t, out, "HOME="+os.Getenv("HOME")+"\n")
	calls := shim.ArgLists(t)
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"status"}, calls[0])
	inv := shim.Invocations(t)
	repoSamePath(t, r.Dir, inv[0].Dir)
}

func TestRepo_Timeout(t *testing.T) {
	r := testutil.NewFixtureRepo(t)
	repo, err := Open(context.Background(), r.Dir, WithEnv(repoTestEnv(r)), WithTimeout(100*time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, 100*time.Millisecond, repo.timeout)

	unbounded, err := Open(context.Background(), r.Dir, WithEnv(repoTestEnv(r)), WithTimeout(0))
	require.NoError(t, err)
	assert.Equal(t, DefaultTimeout, unbounded.timeout, "zero keeps the default")

	testutil.FakeBin(t, "git", `sleep 5`)
	_, err = repo.Git(context.Background(), "status")
	require.ErrorIs(t, err, proc.ErrTimeout)
	var ge *GitError
	require.ErrorAs(t, err, &ge)
	assert.NotNil(t, ge.Err)
	assert.Equal(t, -1, ge.ExitCode)
}

func TestRepo_MissingGit(t *testing.T) {
	r := testutil.NewFixtureRepo(t)
	t.Setenv("PATH", t.TempDir())
	_, err := Open(context.Background(), r.Dir, WithEnv(repoTestEnv(r)))
	require.Error(t, err)
	assert.True(t, proc.IsNotFound(err), "missing git surfaces as not-found: %v", err)
	assert.NotErrorIs(t, err, ErrNotRepository)
}

func TestGitError_Error(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  GitError
		want string
	}{
		{
			name: "exit with stderr",
			err:  GitError{Args: []string{"fetch", "origin"}, ExitCode: 128, Stderr: "fatal: could not read\nfatal: bad remote"},
			want: "gitx: git fetch origin: exit status 128: fatal: bad remote",
		},
		{
			name: "classified",
			err:  GitError{Args: []string{"merge-base", "a", "b"}, ExitCode: 1, Cause: ErrNoMergeBase},
			want: "gitx: git merge-base a b: " + ErrNoMergeBase.Error(),
		},
		{
			name: "killed",
			err:  GitError{Args: []string{"status"}, ExitCode: -1},
			want: "gitx: git status: killed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.err.Error())
		})
	}

	inner := errors.New("inner")
	e := &GitError{Cause: ErrNoRemote, Err: inner}
	assert.ErrorIs(t, e, ErrNoRemote)
	assert.ErrorIs(t, e, inner)
	assert.Empty(t, (&GitError{}).Unwrap())
}

// TestRepo_GitRaw covers P4-11: Git trims every trailing carriage return
// and newline, which silently rewrites any output whose bytes are the
// answer, so GitRaw hands those callers exactly what git wrote.
func TestRepo_GitRaw(t *testing.T) {
	ctx := context.Background()
	r := testutil.NewFixtureRepo(t)
	repo := repoOpen(t, r)

	// A file with CRLF line endings and a meaningful empty last line.
	const content = "first\r\nsecond\r\n\r\n"
	r.WriteFile(t, "crlf.txt", content)
	// --no-filters so no autocrlf or clean filter can rewrite the blob:
	// the bytes on disk are the bytes in the object.
	sha, err := repo.Git(ctx, "hash-object", "-w", "--no-filters", "--", "crlf.txt")
	require.NoError(t, err)

	raw, err := repo.GitRaw(ctx, "cat-file", "blob", sha)
	require.NoError(t, err)
	assert.Equal(t, content, raw, "GitRaw returns the blob byte for byte")

	trimmed, err := repo.Git(ctx, "cat-file", "blob", sha)
	require.NoError(t, err)
	assert.Equal(t, "first\r\nsecond", trimmed, "Git trims the trailing CR/LF run")

	t.Run("line-oriented callers are unchanged", func(t *testing.T) {
		// Git still strips the terminator every parser in this package
		// relies on being absent.
		head, err := repo.Git(ctx, "rev-parse", "HEAD")
		require.NoError(t, err)
		assert.NotContains(t, head, "\n")
		rawHead, err := repo.GitRaw(ctx, "rev-parse", "HEAD")
		require.NoError(t, err)
		assert.Equal(t, head+"\n", rawHead)
	})

	t.Run("failures classify like Git", func(t *testing.T) {
		_, err := repo.GitRaw(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", "nope^{commit}")
		var ge *GitError
		require.ErrorAs(t, err, &ge)
		assert.Equal(t, 1, ge.ExitCode)
	})
}

// TestRepo_ByteSensitiveReaders covers the other half of P4-11: a path
// whose own last byte is a newline survives the NUL-terminated readers,
// which would lose it if the trailing bytes were trimmed.
func TestRepo_ByteSensitiveReaders(t *testing.T) {
	ctx := context.Background()
	r := testutil.NewFixtureRepo(t)
	repo := repoOpen(t, r)
	const odd = "odd\nname.txt" // a newline inside the path, not at its end
	require.NoError(t, os.WriteFile(filepath.Join(r.Dir, odd), []byte("x\n"), 0o644))

	paths, err := repo.changedPaths(ctx)
	require.NoError(t, err)
	assert.Contains(t, paths, odd, "the -z reader keeps a newline inside a path")

	base := r.Head(t)
	_, err = repo.CommitAll(ctx, "a file with a newline in its name")
	require.NoError(t, err)
	stats, err := repo.diffStatsBetween(ctx, base, "HEAD")
	require.NoError(t, err)
	require.Len(t, stats.Changed, 1)
	assert.Equal(t, odd, stats.Changed[0].Path)
	statuses, err := repo.diffStatuses(ctx, base, "HEAD")
	require.NoError(t, err)
	assert.Equal(t, fileStatus{Status: DiffAdded}, statuses[odd])
}
