package prompt

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/testutil"
)

// pbWrite writes a file inside dir, creating parents.
func pbWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestScanRepoFixture(t *testing.T) {
	repo := testutil.NewFixtureRepo(t)
	rc, err := ScanRepo(repo.Dir, ScanOptions{DefaultBranch: "main"})
	require.NoError(t, err)

	assert.Equal(t, "main", rc.DefaultBranch)
	assert.Equal(t, DefaultTreeDepth, rc.TreeDepth)
	assert.Equal(t, []string{"Go", "Make"}, rc.Stack)
	assert.Equal(t, "make test", rc.CheckCommand)
	assert.True(t, rc.CheckCommandDetected)
	assert.Equal(t, "README.md", rc.ReadmeName)
	assert.Contains(t, rc.ReadmeHead, "A tiny Go command-line tool")
	assert.False(t, rc.ReadmeTruncated)
	assert.Contains(t, rc.Tree, "stats/")
	assert.Contains(t, rc.Tree, "stats/stats.go")
	assert.Equal(t, len(rc.Tree), rc.TreeTotal)
	for _, entry := range rc.Tree {
		assert.False(t, strings.HasPrefix(entry, ".git/"), "the git directory must not be listed: %s", entry)
		assert.NotEqual(t, ".git/", entry)
	}
	assert.True(t, filepath.IsAbs(rc.Root))
}

func TestScanRepoDepthAndCap(t *testing.T) {
	dir := t.TempDir()
	pbWrite(t, dir, "a/b/c/deep.txt", "deep")
	pbWrite(t, dir, "top.txt", "top")

	rc, err := ScanRepo(dir, ScanOptions{ReadmeBytes: -1})
	require.NoError(t, err)
	assert.Equal(t, []string{"a/", "a/b/", "a/b/c/", "top.txt"}, rc.Tree)
	assert.Empty(t, rc.ReadmeHead)

	deeper, err := ScanRepo(dir, ScanOptions{Depth: 4, ReadmeBytes: -1})
	require.NoError(t, err)
	assert.Contains(t, deeper.Tree, "a/b/c/deep.txt")

	for i := range 30 {
		pbWrite(t, dir, "many/f"+strconv.Itoa(i)+".txt", "x")
	}
	capped, err := ScanRepo(dir, ScanOptions{MaxEntries: 5, ReadmeBytes: -1})
	require.NoError(t, err)
	assert.Len(t, capped.Tree, 5)
	assert.Greater(t, capped.TreeTotal, 5)
}

func TestScanRepoSkipsNoiseDirectories(t *testing.T) {
	dir := t.TempDir()
	pbWrite(t, dir, "node_modules/left-pad/index.js", "x")
	pbWrite(t, dir, ".beads/beads.db", "x")
	pbWrite(t, dir, ".slop-cannon/config.toml", "x")
	pbWrite(t, dir, "src/main.go", "package main")

	rc, err := ScanRepo(dir, ScanOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"src/", "src/main.go"}, rc.Tree)
}

func TestScanRepoReadmeTruncation(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("line of readme text\n", 200)
	pbWrite(t, dir, "README.md", body)

	rc, err := ScanRepo(dir, ScanOptions{ReadmeBytes: 100})
	require.NoError(t, err)
	assert.True(t, rc.ReadmeTruncated)
	assert.LessOrEqual(t, len(rc.ReadmeHead), 100)
	assert.True(t, strings.HasSuffix(rc.ReadmeHead, "readme text"), rc.ReadmeHead)
}

func TestScanRepoStackDetection(t *testing.T) {
	cases := map[string][]string{
		"package.json":   {"Node.js"},
		"Cargo.toml":     {"Rust"},
		"pyproject.toml": {"Python"},
		"Gemfile":        {"Ruby"},
	}
	for file, want := range cases {
		t.Run(file, func(t *testing.T) {
			dir := t.TempDir()
			pbWrite(t, dir, file, "{}")
			rc, err := ScanRepo(dir, ScanOptions{})
			require.NoError(t, err)
			assert.Equal(t, want, rc.Stack)
		})
	}
}

func TestScanRepoConfiguredCheckCommand(t *testing.T) {
	dir := t.TempDir()
	pbWrite(t, dir, "go.mod", "module x\n")
	rc, err := ScanRepo(dir, ScanOptions{CheckCommand: " make verify "})
	require.NoError(t, err)
	assert.Equal(t, "make verify", rc.CheckCommand)
	assert.False(t, rc.CheckCommandDetected)
}

func TestScanRepoErrors(t *testing.T) {
	_, err := ScanRepo(filepath.Join(t.TempDir(), "missing"), ScanOptions{})
	require.Error(t, err)

	file := filepath.Join(t.TempDir(), "f.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	_, err = ScanRepo(file, ScanOptions{})
	require.ErrorContains(t, err, "not a directory")
}

// TestRenderRepoNoCheckCommand keeps the "none detected" wording covered.
func TestRenderRepoNoCheckCommand(t *testing.T) {
	out := renderRepo(RepoContext{Root: "/r", DefaultBranch: "main"}, -1, -1)
	assert.Contains(t, out, "none detected")
	assert.Empty(t, renderRepo(RepoContext{}, -1, -1))
}
