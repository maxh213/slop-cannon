package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTarget(t *testing.T) {
	assert.Equal(t, ScopeRepo, Target(true, false), "inside a repo: repo config")
	assert.Equal(t, ScopeGlobal, Target(false, false), "outside a repo: global config")
	assert.Equal(t, ScopeGlobal, Target(true, true), "--global forces global")
	assert.Equal(t, ScopeGlobal, Target(false, true))
	assert.Equal(t, "repo", ScopeRepo.String())
}

func TestXDGPaths(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	assert.Equal(t, "/home/tester/.config/slop-cannon/config.toml", GlobalPath())
	assert.Equal(t, "/home/tester/.local/state", StateHome())

	t.Setenv("XDG_CONFIG_HOME", "/xdg/cfg/")
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	assert.Equal(t, "/xdg/cfg/slop-cannon/config.toml", GlobalPath())
	assert.Equal(t, "/xdg/state", StateHome())

	t.Setenv("XDG_CONFIG_HOME", "relative/ignored")
	assert.Equal(t, "/home/tester/.config", Home(), "relative XDG values are ignored per the spec")

	assert.Equal(t, "/r/.slop-cannon/config.toml", RepoConfigPath("/r"))
	assert.Equal(t, "/r/.slop-cannon/config.toml", PathFor(ScopeRepo, "/r"))
	assert.Equal(t, GlobalPath(), PathFor(ScopeGlobal, "/r"))
}

func TestFindRepoRoot(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	sub := filepath.Join(repo, "pkg", "deep")
	require.NoError(t, os.MkdirAll(sub, 0o755))

	root, ok := FindRepoRoot(sub)
	assert.True(t, ok)
	assert.Equal(t, repo, root)
	root, ok = FindRepoRoot(repo)
	assert.True(t, ok)
	assert.Equal(t, repo, root)

	// A worktree has a .git file rather than a directory.
	wt := filepath.Join(base, "wt")
	require.NoError(t, os.MkdirAll(wt, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: elsewhere\n"), 0o644))
	root, ok = FindRepoRoot(wt)
	assert.True(t, ok)
	assert.Equal(t, wt, root)

	_, ok = FindRepoRoot("/")
	assert.False(t, ok)
}

func TestRepoIDAndStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	base := t.TempDir()
	repo := filepath.Join(base, "My App")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	id := RepoID(repo)
	assert.Regexp(t, regexp.MustCompile(`^My_App-[0-9a-f]{12}$`), id)
	assert.Equal(t, id, RepoID(repo), "stable")
	assert.Equal(t, id, RepoID(repo+string(filepath.Separator)), "trailing separator does not matter")
	assert.Equal(t, id, RepoID(filepath.Join(repo, "..", "My App")), "cleaned")
	assert.NotEqual(t, id, RepoID(filepath.Join(base, "other")), "different path, different id")

	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(repo, link))
	assert.Equal(t, id, RepoID(link), "symlinks resolve to the same id")

	assert.Equal(t, filepath.Join("/state", "slop-cannon", id), StateDir(repo))
	assert.Equal(t, filepath.Join("/state", "slop-cannon", id, "runs", "run-1"), RunDir(repo, "run-1"))
	assert.Equal(t, filepath.Join("/state", "slop-cannon", id, "runs", "a_b"), RunDir(repo, "a/b"), "run ids are sanitised")

	// Relative paths are resolved against the working directory.
	wd, err := os.Getwd()
	require.NoError(t, err)
	assert.Equal(t, RepoID(wd), RepoID("."))
	assert.True(t, strings.HasPrefix(RepoID("."), filepath.Base(wd)+"-"))
}
