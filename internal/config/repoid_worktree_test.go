package config

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// "One run per repository" has to mean the repository, not the directory
// (P9-04). A linked `git worktree` is a second checkout of the *same* .git:
// it shares the beads database, the default branch and the remote, so a run
// in one is a run in the other — and if the two got different RepoIDs they
// would get different state directories, different run locks, and both
// would fire at once.

// TestRepoIDFollowsAWorktreeToItsRepository builds the layout git makes,
// by hand, so the test needs no git binary: a main checkout with a .git
// directory, and a linked worktree whose .git is a *file* pointing into
// <main>/.git/worktrees/<name>, where a `commondir` file points back.
func TestRepoIDFollowsAWorktreeToItsRepository(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "repo")
	gitDir := filepath.Join(main, ".git", "worktrees", "side")
	require.NoError(t, os.MkdirAll(gitDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0o644))

	side := filepath.Join(base, "side")
	require.NoError(t, os.MkdirAll(side, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(side, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644))

	require.Equal(t, RepoID(main), RepoID(side), "a worktree is the same repository")
	require.Equal(t, StateDir(main), StateDir(side), "so it has the same state directory and run lock")
	require.True(t, strings.HasPrefix(RepoID(side), "repo-"), "named after the main checkout, got %s", RepoID(side))

	// An unrelated checkout is still an unrelated repository.
	other := filepath.Join(base, "other")
	require.NoError(t, os.MkdirAll(filepath.Join(other, ".git"), 0o755))
	require.NotEqual(t, RepoID(main), RepoID(other))
}

// TestRepoIDLeavesEverythingElseAlone: only a .git file that really points
// at a worktree git directory redirects. A submodule (a .git file whose git
// directory has no commondir), a broken pointer and a plain directory all
// keep the id of the path they are.
func TestRepoIDLeavesEverythingElseAlone(t *testing.T) {
	base := t.TempDir()

	plain := filepath.Join(base, "plain")
	require.NoError(t, os.MkdirAll(filepath.Join(plain, ".git"), 0o755))

	sub := filepath.Join(base, "sub")
	subGit := filepath.Join(base, "super", ".git", "modules", "sub")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.MkdirAll(subGit, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: "+subGit+"\n"), 0o644))

	broken := filepath.Join(base, "broken")
	require.NoError(t, os.MkdirAll(broken, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(broken, ".git"), []byte("not a pointer\n"), 0o644))

	bare := filepath.Join(base, "bare")
	require.NoError(t, os.MkdirAll(bare, 0o755))

	for _, dir := range []string{plain, sub, broken, bare} {
		id := RepoID(dir)
		require.True(t, strings.HasPrefix(id, filepath.Base(dir)+"-"), "%s got %s", dir, id)
		require.Equal(t, id, RepoID(dir), "stable")
	}
	require.NotEqual(t, RepoID(sub), RepoID(filepath.Join(base, "super")), "a submodule is its own repository")
}

// TestRepoIDMatchesRealGitWorktrees checks the hand-built layout above
// against the one git actually writes.
func TestRepoIDMatchesRealGitWorktrees(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH")
	}
	base := t.TempDir()
	home := filepath.Join(base, "home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	main := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(main, 0o755))

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = []string{
			"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "PATH=" + os.Getenv("PATH"),
		}
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		require.NoError(t, cmd.Run(), fmt.Sprintf("git %s: %s", strings.Join(args, " "), errBuf.String()))
	}
	run(main, "init", "-q", "-b", "main")
	run(main, "commit", "-q", "--allow-empty", "-m", "first")
	side := filepath.Join(base, "side")
	run(main, "worktree", "add", "-q", "-b", "side", side)

	info, err := os.Lstat(filepath.Join(side, ".git"))
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular(), "git writes a .git file in a linked worktree")
	require.Equal(t, RepoID(main), RepoID(side), "the worktree shares the repository's state directory")
	require.Equal(t, StateDir(main), StateDir(side))
}
