package beads

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/proc"
	"slop-cannon/internal/testutil"
)

// initFakeBD installs a bd shim that reproduces the side effects observed
// from bd 1.2.2 (Appendix A, D-13) on top of the captured samples:
//
//   - `bd init` refuses -C on a directory without a beads project, creates
//     .beads/ in the working directory, appends the captured .gitignore
//     block (after a newline when the file lacks one, and not at all when
//     the block is already there), prints init.txt with the requested
//     prefix and init.stderr; a second run prints init-again.stderr only.
//   - `bd ping` answers ping.json when the -C directory has .beads/, else
//     the captured refusal with exit 1.
//   - `bd version` answers version.json.
//
// A file named "mode" in the shim directory alters init: "fail" exits 1
// after the side effects, "foreign" appends another tool's comment and
// pattern after bd's block, "rewrite" edits .gitignore elsewhere,
// "ping-fail" makes ping refuse.
func initFakeBD(t *testing.T) *testutil.Shim {
	t.Helper()
	envIsolate(t)
	samples := envSamplesDir(t)
	script := `samples='` + samples + `'
mode=$(cat "$SC_FAKEBIN_DIR/mode" 2>/dev/null || true)
repo="$PWD"; cmd=''; prefix=''; next=''; hasC=0
for a in "$@"; do
  case "$next" in
    C) repo="$a"; next=''; continue ;;
    p) prefix="$a"; next=''; continue ;;
  esac
  case "$a" in
    -C) next=C; hasC=1 ;;
    -p|--prefix) next=p ;;
    -*) ;;
    *) [ -z "$cmd" ] && cmd="$a" ;;
  esac
done
case "$cmd" in
  version) cat "$samples/version.json" ;;
  ping)
    if [ "$mode" = ping-fail ] || [ ! -d "$repo/.beads" ]; then cat "$samples/ping-no-db.stderr" >&2; exit 1; fi
    cat "$samples/ping.json" ;;
  init)
    if [ "$hasC" = 1 ]; then cat "$samples/init-dash-C.stderr" >&2; exit 1; fi
    if [ -d "$PWD/.beads" ]; then cat "$samples/init-again.stderr" >&2; exit 0; fi
    mkdir "$PWD/.beads"
    printf '{"dolt_mode":"embedded","dolt_database":"%s"}\n' "$prefix" > "$PWD/.beads/metadata.json"
    gi="$PWD/.gitignore"
    if [ "$mode" = rewrite ]; then
      { echo '# rewritten by a surprising bd'; cat "$gi" 2>/dev/null; } > "$gi.tmp" && mv "$gi.tmp" "$gi"
    elif ! grep -q 'Beads / Dolt files' "$gi" 2>/dev/null; then
      if [ -s "$gi" ] && [ -n "$(tail -c1 "$gi")" ]; then printf '\n' >> "$gi"; fi
      cat "$samples/gitignore-hunk.txt" >> "$gi"
      if [ "$mode" = foreign ]; then printf '# added by another tool\nother/\n' >> "$gi"; fi
    fi
    sed "s/: fx\$/: $prefix/; s/fx-<hash>/$prefix-<hash>/; s/fx-a3f2dd/$prefix-a3f2dd/" "$samples/init.txt"
    cat "$samples/init.stderr" >&2
    if [ "$mode" = fail ]; then echo "Error: simulated init failure" >&2; exit 1; fi ;;
  *) echo "initFakeBD: unexpected command $cmd" >&2; exit 3 ;;
esac`
	return testutil.FakeBin(t, "bd", script)
}

// initSetMode configures the shim's next init behaviour.
func initSetMode(t *testing.T, bd *testutil.Shim, mode string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(bd.Dir, "mode"), []byte(mode), 0o644))
}

// initRepo creates a directory that looks like a git repository root (a
// .git directory is all FindRepoRoot needs) named name, with the given
// .gitignore content ("" means no file), and returns its path.
func initRepo(t *testing.T, name, gitignore string, hasGitignore bool) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	if hasGitignore {
		require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte(gitignore), 0o644))
	}
	return root
}

// initLogger returns a logger writing to buf so tests can assert on lines.
func initLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func initReadGitignore(t *testing.T, root string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	require.NoError(t, err)
	return string(data), true
}

func TestEnsureInitialisedFreshRepo(t *testing.T) {
	bd := initFakeBD(t)
	root := initRepo(t, "my.repo", "bin/\n*.log\n", true)
	var logs bytes.Buffer
	res, err := EnsureInitialised(context.Background(), root, InitOptions{Logger: initLogger(&logs)})
	require.NoError(t, err)

	assert.Equal(t, root, res.Root)
	assert.True(t, res.Initialised)
	assert.Equal(t, "my_repo", res.Prefix, "the directory name, sanitised, is the default prefix")
	assert.True(t, res.GitignoreReverted)
	assert.Empty(t, res.GitignoreWarning)
	assert.Equal(t, "ok", res.Ping.Status)
	assert.Contains(t, res.Output, "bd initialized successfully")
	assert.DirExists(t, filepath.Join(root, ".beads"))

	got, exists := initReadGitignore(t, root)
	assert.True(t, exists)
	assert.Equal(t, "bin/\n*.log\n", got, "the block bd appended is reverted")

	calls := bd.Invocations(t)
	require.Len(t, calls, 2)
	assert.Equal(t, []string{"init", "--non-interactive", "--init-if-missing", "--skip-agents", "--skip-hooks", "--setup-exclude", "-p", "my_repo"}, calls[0].Args)
	wantDir, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	gotDir, err := filepath.EvalSymlinks(calls[0].Dir)
	require.NoError(t, err)
	assert.Equal(t, wantDir, gotDir, "bd init runs in the repository root, without -C")
	assert.Equal(t, []string{"-C", root, "ping", "--json"}, calls[1].Args)

	assert.Contains(t, logs.String(), "initialising beads")
	assert.Contains(t, logs.String(), "command=\"bd init --non-interactive --init-if-missing --skip-agents --skip-hooks --setup-exclude -p my_repo\"")
	assert.Contains(t, logs.String(), "reverted the .gitignore block")
	assert.Contains(t, logs.String(), "beads initialised")
	assert.Contains(t, logs.String(), "beads database answered")
	assert.NotContains(t, logs.String(), "level=WARN")

	// A second call is a no-op: no init, just the health check.
	bd.Reset(t)
	res, err = EnsureInitialised(context.Background(), root, InitOptions{Logger: initLogger(&logs)})
	require.NoError(t, err)
	assert.False(t, res.Initialised)
	assert.Empty(t, res.Prefix)
	assert.False(t, res.GitignoreReverted)
	assert.Equal(t, "ok", res.Ping.Status)
	assert.Equal(t, [][]string{{"-C", root, "ping", "--json"}}, bd.ArgLists(t))
	assert.Contains(t, logs.String(), "beads already initialised")
	got, _ = initReadGitignore(t, root)
	assert.Equal(t, "bin/\n*.log\n", got)
}

func TestEnsureInitialisedNestedPathAndExplicitPrefix(t *testing.T) {
	bd := initFakeBD(t)
	root := initRepo(t, "repo", "bin/\n", true)
	nested := filepath.Join(root, "pkg", "sub")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	res, err := EnsureInitialised(context.Background(), nested, InitOptions{Prefix: "sc-test", Actor: "sc:orchestrator", Logger: initLogger(&bytes.Buffer{})})
	require.NoError(t, err)
	assert.Equal(t, root, res.Root)
	assert.Equal(t, "sc-test", res.Prefix)
	calls := bd.ArgLists(t)
	require.Len(t, calls, 2)
	assert.Equal(t, "sc-test", calls[0][len(calls[0])-1])
	assert.Equal(t, []string{"-C", root, "ping", "--json"}, calls[1])
}

func TestEnsureInitialisedGitignoreVariants(t *testing.T) {
	t.Run("no .gitignore: the file bd creates is removed", func(t *testing.T) {
		initFakeBD(t)
		root := initRepo(t, "repo", "", false)
		res, err := EnsureInitialised(context.Background(), root, InitOptions{Logger: initLogger(&bytes.Buffer{})})
		require.NoError(t, err)
		assert.True(t, res.GitignoreReverted)
		_, exists := initReadGitignore(t, root)
		assert.False(t, exists)
	})
	t.Run("no trailing newline: restored byte for byte", func(t *testing.T) {
		initFakeBD(t)
		root := initRepo(t, "repo", "bin/", true)
		res, err := EnsureInitialised(context.Background(), root, InitOptions{Logger: initLogger(&bytes.Buffer{})})
		require.NoError(t, err)
		assert.True(t, res.GitignoreReverted)
		got, _ := initReadGitignore(t, root)
		assert.Equal(t, "bin/", got)
	})
	t.Run("block already present: nothing to revert", func(t *testing.T) {
		initFakeBD(t)
		hunk := envSample(t, "gitignore-hunk.txt")
		before := "bin/\n" + hunk
		root := initRepo(t, "repo", before, true)
		res, err := EnsureInitialised(context.Background(), root, InitOptions{Logger: initLogger(&bytes.Buffer{})})
		require.NoError(t, err)
		assert.False(t, res.GitignoreReverted)
		assert.Empty(t, res.GitignoreWarning)
		got, _ := initReadGitignore(t, root)
		assert.Equal(t, before, got)
	})
	t.Run("file mode is preserved", func(t *testing.T) {
		initFakeBD(t)
		root := initRepo(t, "repo", "bin/\n", true)
		path := filepath.Join(root, ".gitignore")
		require.NoError(t, os.Chmod(path, 0o600))
		_, err := EnsureInitialised(context.Background(), root, InitOptions{Logger: initLogger(&bytes.Buffer{})})
		require.NoError(t, err)
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	})
	t.Run("more than the block appended: left alone with a warning", func(t *testing.T) {
		bd := initFakeBD(t)
		initSetMode(t, bd, "foreign")
		root := initRepo(t, "repo", "bin/\n", true)
		var logs bytes.Buffer
		res, err := EnsureInitialised(context.Background(), root, InitOptions{Logger: initLogger(&logs)})
		require.NoError(t, err, "a leftover .gitignore edit is a warning, not a failure")
		assert.False(t, res.GitignoreReverted)
		assert.Contains(t, res.GitignoreWarning, "left as is")
		got, _ := initReadGitignore(t, root)
		assert.True(t, strings.HasPrefix(got, "bin/\n"))
		assert.Contains(t, got, GitignoreMarker)
		assert.Contains(t, got, "# added by another tool\nother/\n")
		assert.Contains(t, logs.String(), "level=WARN")
		assert.Contains(t, logs.String(), "left as is")
	})
	t.Run("a new pattern under bd's own header still counts as its block", func(t *testing.T) {
		// A future bd may ignore one more path; anything it appends under
		// its marker during the init call is still its block.
		root := initRepo(t, "repo", "bin/\n", true)
		path := filepath.Join(root, ".gitignore")
		hunk := envSample(t, "gitignore-hunk.txt")
		require.NoError(t, os.WriteFile(path, []byte("bin/\n"+hunk+".beads/new-cache/\n"), 0o644))
		reverted, warning, err := revertGitignore(path, []byte("bin/\n"), 0o644, true)
		require.NoError(t, err)
		assert.True(t, reverted)
		assert.Empty(t, warning)
		got, _ := initReadGitignore(t, root)
		assert.Equal(t, "bin/\n", got)
	})
	t.Run("edited elsewhere: left alone with a warning", func(t *testing.T) {
		bd := initFakeBD(t)
		initSetMode(t, bd, "rewrite")
		root := initRepo(t, "repo", "bin/\n", true)
		res, err := EnsureInitialised(context.Background(), root, InitOptions{Logger: initLogger(&bytes.Buffer{})})
		require.NoError(t, err)
		assert.False(t, res.GitignoreReverted)
		assert.Contains(t, res.GitignoreWarning, "beyond appending its block")
		got, _ := initReadGitignore(t, root)
		assert.True(t, strings.HasPrefix(got, "# rewritten by a surprising bd\nbin/\n"), got)
	})
}

func TestEnsureInitialisedInitFails(t *testing.T) {
	bd := initFakeBD(t)
	initSetMode(t, bd, "fail")
	root := initRepo(t, "repo", "bin/\n", true)
	var logs bytes.Buffer
	res, err := EnsureInitialised(context.Background(), root, InitOptions{Logger: initLogger(&logs)})
	require.Error(t, err)
	var exit *proc.ExitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 1, exit.Result.ExitCode)
	assert.Contains(t, err.Error(), "bd init in "+root)
	assert.Contains(t, err.Error(), "simulated init failure")
	assert.False(t, res.Initialised)
	assert.True(t, res.GitignoreReverted, "the .gitignore block is reverted even when init fails")
	got, _ := initReadGitignore(t, root)
	assert.Equal(t, "bin/\n", got)
	assert.NoDirExists(t, filepath.Join(root, ".beads"), "a partial .beads/ is removed so the retry starts clean")
	assert.Equal(t, [][]string{{"init", "--non-interactive", "--init-if-missing", "--skip-agents", "--skip-hooks", "--setup-exclude", "-p", "repo"}}, bd.ArgLists(t), "no ping after a failed init")
	assert.Contains(t, logs.String(), "bd init failed")
	assert.Contains(t, logs.String(), "removed the partial .beads directory")
}

func TestEnsureInitialisedPingFails(t *testing.T) {
	bd := initFakeBD(t)
	initSetMode(t, bd, "ping-fail")
	root := initRepo(t, "repo", "bin/\n", true)
	var logs bytes.Buffer
	res, err := EnsureInitialised(context.Background(), root, InitOptions{Logger: initLogger(&logs)})
	var ce *CheckError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, ReasonFailed, ce.Reason)
	assert.Equal(t, PingHint, ce.Hint())
	assert.True(t, res.Initialised, "init itself succeeded")
	assert.True(t, res.GitignoreReverted)
	assert.Contains(t, logs.String(), "beads health check failed")
}

func TestEnsureInitialisedNotAGitRepo(t *testing.T) {
	bd := initFakeBD(t)
	dir := t.TempDir()
	_, err := EnsureInitialised(context.Background(), dir, InitOptions{Logger: initLogger(&bytes.Buffer{})})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotGitRepo), "got %v", err)
	assert.Contains(t, err.Error(), dir)
	assert.Empty(t, bd.ArgLists(t), "bd is never run outside a git repository (bd init would git init)")
}

func TestEnsureInitialisedBDMissing(t *testing.T) {
	envIsolate(t)
	t.Setenv("PATH", t.TempDir())
	root := initRepo(t, "repo", "bin/\n", true)
	res, err := EnsureInitialised(context.Background(), root, InitOptions{Logger: initLogger(&bytes.Buffer{})})
	require.Error(t, err)
	assert.True(t, proc.IsNotFound(err), "got %v", err)
	assert.False(t, res.Initialised)
	got, _ := initReadGitignore(t, root)
	assert.Equal(t, "bin/\n", got)
	assert.NoDirExists(t, filepath.Join(root, ".beads"))
}

func TestEnsureInitialisedDefaultLogger(t *testing.T) {
	// A nil Logger falls back to slog.Default; capture it to prove the
	// lines still go somewhere.
	initFakeBD(t)
	root := initRepo(t, "repo", "bin/\n", true)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(initLogger(&logs))
	t.Cleanup(func() { slog.SetDefault(prev) })
	_, err := EnsureInitialised(context.Background(), root, InitOptions{})
	require.NoError(t, err)
	assert.Contains(t, logs.String(), "beads initialised")
}

func TestInitArgsAndCommandLine(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"init", "--non-interactive", "--init-if-missing", "--skip-agents", "--skip-hooks", "--setup-exclude", "-p", "fx"}, InitArgs("fx"))
	assert.Equal(t, "bd init --non-interactive --init-if-missing --skip-agents --skip-hooks --setup-exclude", InitCommandLine())
}

func TestDefaultPrefix(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/home/u/slop-cannon":  "slop-cannon",
		"/home/u/my.repo":      "my_repo",
		"/home/u/My Repo (2)/": "My_Repo_2",
		"/home/u/repo_":        "repo",
		"/home/u/---":          "sc",
		"/home/u/-abc":         "abc",
		"/home/u/_abc-":        "abc",
		"/":                    "sc",
		".":                    "sc",
		"/home/u/日本":           "sc",
		"/home/u/x":            "x",
	}
	for in, want := range cases {
		assert.Equal(t, want, DefaultPrefix(filepath.FromSlash(in)), in)
	}
}

func TestReportedPrefix(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "fx", reportedPrefix(envSample(t, "init.txt"), "other"))
	assert.Equal(t, "bd_1abc", reportedPrefix("  Database: bd_1abc\n  Issue prefix: bd_1abc\n", "1abc"))
	assert.Equal(t, "requested", reportedPrefix("Skipping init: workspace already initialized.\n", "requested"))
}

func TestIsGitignoreBlock(t *testing.T) {
	t.Parallel()
	hunk := envSample(t, "gitignore-hunk.txt")
	assert.True(t, IsGitignoreBlock([]byte(hunk)))
	assert.True(t, IsGitignoreBlock([]byte("\n"+hunk)), "an extra newline inserted for a file without one")
	assert.True(t, IsGitignoreBlock([]byte("# Beads files (bd init)\n.dolt/\n")))
	assert.False(t, IsGitignoreBlock(nil))
	assert.False(t, IsGitignoreBlock([]byte("\n\n")))
	assert.False(t, IsGitignoreBlock([]byte(GitignoreMarker+"\n")), "a header with no patterns")
	assert.False(t, IsGitignoreBlock([]byte(".dolt/\n")), "patterns without the marker")
	assert.False(t, IsGitignoreBlock([]byte("# something else\n.dolt/\n")))
	assert.False(t, IsGitignoreBlock([]byte(hunk+"# user note\n")))
	assert.False(t, IsGitignoreBlock([]byte("user/\n"+hunk)))
}

func TestRevertGitignoreEdgeCases(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")

	// Absent before and after: nothing happened.
	reverted, warning, err := revertGitignore(path, nil, 0, false)
	require.NoError(t, err)
	assert.False(t, reverted)
	assert.Empty(t, warning)

	// Present before, gone after: warn, do not recreate.
	reverted, warning, err = revertGitignore(path, []byte("x\n"), 0o644, true)
	require.NoError(t, err)
	assert.False(t, reverted)
	assert.Contains(t, warning, "disappeared")

	// Unchanged: nothing to do.
	require.NoError(t, os.WriteFile(path, []byte("x\n"), 0o644))
	reverted, warning, err = revertGitignore(path, []byte("x\n"), 0o644, true)
	require.NoError(t, err)
	assert.False(t, reverted)
	assert.Empty(t, warning)

	// Created by bd with something other than its block: left alone.
	require.NoError(t, os.WriteFile(path, []byte("mystery/\n"), 0o644))
	reverted, warning, err = revertGitignore(path, nil, 0, false)
	require.NoError(t, err)
	assert.False(t, reverted)
	assert.Contains(t, warning, "left as is")
	assert.FileExists(t, path)
}

func TestReadGitignoreRejectsNonRegularFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")
	require.NoError(t, os.Mkdir(path, 0o755))
	_, _, _, err := readGitignore(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a regular file")
}
