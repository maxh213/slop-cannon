package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
)

// vibesTestEnv is a sandbox for the vibes/rules tests: a fake HOME with XDG
// dirs and a fake git repository, with the working directory moved into the
// repo so the README examples can be typed verbatim.
type vibesTestEnv struct {
	home string
	repo string
}

// vibesSetup isolates HOME and the XDG dirs under t.TempDir(), creates a
// fake git repo (a .git directory is enough for config.FindRepoRoot), and
// chdirs into it.
func vibesSetup(t *testing.T) vibesTestEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("NO_COLOR", "")
	require.NoError(t, os.Unsetenv("NO_COLOR"))

	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	t.Chdir(repo)
	// t.TempDir may be a symlink on some platforms; use what Getwd reports so
	// path comparisons match what the command computes.
	wd, err := os.Getwd()
	require.NoError(t, err)
	return vibesTestEnv{home: home, repo: wd}
}

// write creates a file under dir with content and returns its path.
func (e vibesTestEnv) write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// repoFile reads the repo config file.
func (e vibesTestEnv) repoFile(t *testing.T) *config.File {
	t.Helper()
	f, exists, err := config.ReadFile(config.RepoConfigPath(e.repo))
	require.NoError(t, err)
	require.True(t, exists, "repo config was not written")
	return f
}

// globalFile reads the global config file.
func (e vibesTestEnv) globalFile(t *testing.T) *config.File {
	t.Helper()
	f, exists, err := config.ReadFile(config.GlobalPath())
	require.NoError(t, err)
	require.True(t, exists, "global config was not written")
	return f
}

// vibesJSON runs the command with --json and decodes the result.
func vibesJSON(t *testing.T, args ...string) PromptResult {
	t.Helper()
	out, err := rootRun(t, append([]string{"--json"}, args...)...)
	require.NoError(t, err, out)
	var res PromptResult
	require.NoError(t, json.Unmarshal([]byte(out), &res), out)
	return res
}

func TestVibesHelpGolden(t *testing.T) {
	out, err := rootRun(t, "vibes", "--help")
	require.NoError(t, err)
	rootGolden(t, "vibes-help", out)
}

func TestRulesHelpGolden(t *testing.T) {
	out, err := rootRun(t, "rules", "--help")
	require.NoError(t, err)
	rootGolden(t, "rules-help", out)
}

// TestVibesRulesREADMEExamples runs the two README lines verbatim inside a
// git repo: the repo config gets the file stored relative to its directory.
func TestVibesRulesREADMEExamples(t *testing.T) {
	env := vibesSetup(t)
	env.write(t, env.repo, "vibes.md", "# vibes\nkeep it terse\n")
	env.write(t, env.repo, "rules.md", "# rules\nnever force-push\n")

	out, err := rootRun(t, "vibes", "--add", "vibes.md")
	require.NoError(t, err)
	cfg := config.RepoConfigPath(env.repo)
	require.Equal(t, "added vibes.md to vibes as \"../vibes.md\"\nsaved to "+cfg+"\n", out)

	out, err = rootRun(t, "rules", "--add", "rules.md")
	require.NoError(t, err)
	require.Equal(t, "added rules.md to rules as \"../rules.md\"\nsaved to "+cfg+"\n", out)

	f := env.repoFile(t)
	require.Equal(t, []string{"../vibes.md"}, f.Vibes)
	require.Equal(t, []string{"../rules.md"}, f.Rules)

	// The global config was never created.
	_, exists, err := config.ReadFile(config.GlobalPath())
	require.NoError(t, err)
	require.False(t, exists)

	// And the merged config resolves both back to absolute paths.
	eff, err := config.Load(env.repo)
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(env.repo, "vibes.md")}, eff.Vibes)
	require.Equal(t, []string{filepath.Join(env.repo, "rules.md")}, eff.Rules)
	require.Equal(t, config.SourceRepo, eff.Sources[config.KeyVibes])
}

func TestVibesAddJSON(t *testing.T) {
	env := vibesSetup(t)
	env.write(t, env.repo, "docs/vibes.md", "x\n")

	res := vibesJSON(t, "vibes", "--add", "docs/vibes.md")
	require.Equal(t, "vibes", res.Kind)
	require.Equal(t, config.ScopeRepo, res.Scope)
	require.Equal(t, config.RepoConfigPath(env.repo), res.Path)
	require.Equal(t, []string{"../docs/vibes.md"}, res.Added)
	require.Empty(t, res.Unchanged)
	require.Len(t, res.Files, 1)
	require.Equal(t, PromptFile{
		Path:   filepath.Join(env.repo, "docs", "vibes.md"),
		Stored: "../docs/vibes.md",
		Source: config.SourceRepo,
		Exists: true,
	}, res.Files[0])
}

func TestVibesAddOutsideRepoWritesGlobal(t *testing.T) {
	env := vibesSetup(t)
	outside := t.TempDir()
	t.Chdir(outside)
	path := env.write(t, outside, "style.md", "be nice\n")

	res := vibesJSON(t, "vibes", "--add", "style.md")
	require.Equal(t, config.ScopeGlobal, res.Scope)
	require.Equal(t, config.GlobalPath(), res.Path)
	// Not under the global config dir, so stored absolute.
	require.Len(t, res.Added, 1)
	require.True(t, filepath.IsAbs(res.Added[0]), res.Added[0])
	require.Equal(t, filepath.Clean(path), filepath.Clean(res.Added[0]))

	f := env.globalFile(t)
	require.Len(t, f.Vibes, 1)
	require.True(t, filepath.IsAbs(f.Vibes[0]))
	info, err := os.Stat(config.GlobalPath())
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestVibesAddGlobalFlagForcesGlobalAndStoresRelativeInsideConfigDir(t *testing.T) {
	env := vibesSetup(t)
	cfgDir := filepath.Dir(config.GlobalPath())
	env.write(t, cfgDir, "vibes.md", "global taste\n")

	res := vibesJSON(t, "--global", "rules", "--add", filepath.Join(cfgDir, "vibes.md"))
	require.Equal(t, "rules", res.Kind)
	require.Equal(t, config.ScopeGlobal, res.Scope)
	require.Equal(t, []string{"vibes.md"}, res.Added)
	require.Equal(t, []string{"vibes.md"}, env.globalFile(t).Rules)

	_, exists, err := config.ReadFile(config.RepoConfigPath(env.repo))
	require.NoError(t, err)
	require.False(t, exists, "repo config must not be touched with --global")
}

func TestVibesAddDedupes(t *testing.T) {
	env := vibesSetup(t)
	env.write(t, env.repo, "vibes.md", "x\n")

	_, err := rootRun(t, "vibes", "--add", "vibes.md")
	require.NoError(t, err)
	// Same file through a different spelling.
	out, err := rootRun(t, "vibes", "--add", "./sub/../vibes.md")
	require.NoError(t, err)
	cfg := config.RepoConfigPath(env.repo)
	require.Equal(t, "./sub/../vibes.md is already in vibes ("+cfg+"); nothing added\n", out)
	require.Equal(t, []string{"../vibes.md"}, env.repoFile(t).Vibes)

	res := vibesJSON(t, "vibes", "--add", "vibes.md")
	require.Empty(t, res.Added)
	require.Equal(t, []string{"vibes.md"}, res.Unchanged)
	require.Len(t, res.Files, 1)
}

func TestVibesAddRepeatableFlag(t *testing.T) {
	env := vibesSetup(t)
	env.write(t, env.repo, "a.md", "a\n")
	env.write(t, env.repo, "b.md", "b\n")

	res := vibesJSON(t, "vibes", "--add", "a.md", "--add", "b.md")
	require.Equal(t, []string{"../a.md", "../b.md"}, res.Added)
	require.Equal(t, []string{"../a.md", "../b.md"}, env.repoFile(t).Vibes)
}

func TestVibesAddRejectsBadFiles(t *testing.T) {
	env := vibesSetup(t)
	big := make([]byte, config.MaxPromptFileSize+1)
	for i := range big {
		big[i] = 'a'
	}
	env.write(t, env.repo, "big.md", string(big))
	env.write(t, env.repo, "binary.md", "ok\x00nope")
	env.write(t, env.repo, "latin1.md", "caf\xe9")
	require.NoError(t, os.Mkdir(filepath.Join(env.repo, "dir.md"), 0o755))

	cases := map[string]string{
		"missing.md": "missing.md",
		"big.md":     "exceeds the 64 KiB limit",
		"binary.md":  "NUL",
		"latin1.md":  "not valid UTF-8",
		"dir.md":     "not a regular file",
		"":           "must not be empty",
	}
	for file, want := range cases {
		_, err := rootRun(t, "rules", "--add", file)
		require.Error(t, err, file)
		require.Contains(t, err.Error(), want, file)
	}
	_, exists, err := config.ReadFile(config.RepoConfigPath(env.repo))
	require.NoError(t, err)
	require.False(t, exists, "a rejected file must not create the config")
}

func TestVibesAddBadFileLeavesExistingListUntouched(t *testing.T) {
	env := vibesSetup(t)
	env.write(t, env.repo, "good.md", "g\n")
	_, err := rootRun(t, "vibes", "--add", "good.md")
	require.NoError(t, err)

	_, err = rootRun(t, "vibes", "--add", "missing.md")
	require.Error(t, err)
	require.Equal(t, []string{"../good.md"}, env.repoFile(t).Vibes)
}

func TestVibesRemove(t *testing.T) {
	env := vibesSetup(t)
	env.write(t, env.repo, "a.md", "a\n")
	env.write(t, env.repo, "b.md", "b\n")
	_, err := rootRun(t, "vibes", "--add", "a.md", "--add", "b.md")
	require.NoError(t, err)

	cfg := config.RepoConfigPath(env.repo)
	out, err := rootRun(t, "vibes", "--remove", "./a.md")
	require.NoError(t, err)
	require.Equal(t, "removed ./a.md from vibes\nsaved to "+cfg+"\n", out)
	require.Equal(t, []string{"../b.md"}, env.repoFile(t).Vibes)

	res := vibesJSON(t, "vibes", "--remove", "b.md")
	require.Equal(t, []string{"b.md"}, res.Removed)
	require.Empty(t, res.Files)
	require.Nil(t, env.repoFile(t).Vibes)
}

func TestVibesRemoveDeletedFileStillWorks(t *testing.T) {
	env := vibesSetup(t)
	path := env.write(t, env.repo, "gone.md", "x\n")
	_, err := rootRun(t, "rules", "--add", "gone.md")
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))

	out, err := rootRun(t, "rules", "--list")
	require.NoError(t, err)
	require.Contains(t, out, "(missing)")

	_, err = rootRun(t, "rules", "--remove", "gone.md")
	require.NoError(t, err)
	require.Nil(t, env.repoFile(t).Rules)
}

func TestVibesRemoveNotListedIsClearError(t *testing.T) {
	env := vibesSetup(t)
	env.write(t, env.repo, "a.md", "a\n")
	_, err := rootRun(t, "vibes", "--add", "a.md")
	require.NoError(t, err)

	cfg := config.RepoConfigPath(env.repo)
	_, err = rootRun(t, "vibes", "--remove", "nope.md")
	require.Error(t, err)
	require.Equal(t, "nope.md is not in vibes ("+cfg+"); nothing removed", err.Error())
	require.Equal(t, []string{"../a.md"}, env.repoFile(t).Vibes, "a failed remove must not change the file")

	// The rules list is separate: a vibes file is not a rules file.
	_, err = rootRun(t, "rules", "--remove", "a.md")
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not in rules")

	// Removing from an absent config file is the same error, and creates nothing.
	_, err = rootRun(t, "--global", "vibes", "--remove", "a.md")
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not in vibes ("+config.GlobalPath()+"); nothing removed")
	_, exists, err := config.ReadFile(config.GlobalPath())
	require.NoError(t, err)
	require.False(t, exists)
}

func TestVibesListMergesGlobalThenRepo(t *testing.T) {
	env := vibesSetup(t)
	shared := env.write(t, env.home, "shared.md", "shared\n")
	local := env.write(t, env.repo, "vibes.md", "local\n")
	_, err := rootRun(t, "--global", "vibes", "--add", shared)
	require.NoError(t, err)
	_, err = rootRun(t, "vibes", "--add", "vibes.md")
	require.NoError(t, err)
	// The same file in both layers is listed once, under the global source.
	_, err = rootRun(t, "vibes", "--add", shared)
	require.NoError(t, err)
	require.Equal(t, []string{"../vibes.md", shared}, env.repoFile(t).Vibes)

	out, err := rootRun(t, "vibes", "--list")
	require.NoError(t, err)
	width := max(len(shared), len(local)) + 2
	pad := func(p string) string { return p + strings.Repeat(" ", width-len(p)) }
	require.Equal(t, pad(shared)+"global\n"+pad(local)+"repo\n", out)

	// No flags at all means --list.
	out2, err := rootRun(t, "vibes")
	require.NoError(t, err)
	require.Equal(t, out, out2)

	res := vibesJSON(t, "vibes", "--list")
	require.Equal(t, PromptResult{Kind: "vibes", Files: []PromptFile{
		{Path: shared, Stored: shared, Source: config.SourceGlobal, Exists: true},
		{Path: local, Stored: "../vibes.md", Source: config.SourceRepo, Exists: true},
	}}, res)

	// --global looks at the global file only.
	res = vibesJSON(t, "--global", "vibes", "--list")
	require.Len(t, res.Files, 1)
	require.Equal(t, config.SourceGlobal, res.Files[0].Source)

	// --repo pointing outside any git repo: global only as well.
	res = vibesJSON(t, "--repo", t.TempDir(), "vibes", "--list")
	require.Len(t, res.Files, 1)
}

func TestVibesListEmpty(t *testing.T) {
	vibesSetup(t)
	out, err := rootRun(t, "vibes", "--list")
	require.NoError(t, err)
	require.Equal(t, "no vibes configured; add one with: sc vibes --add FILE\n", out)

	out, err = rootRun(t, "--global", "rules")
	require.NoError(t, err)
	require.Equal(t, "no rules configured in the global config; add one with: sc rules --add FILE\n", out)

	res := vibesJSON(t, "rules", "--list")
	require.Equal(t, "rules", res.Kind)
	require.Empty(t, res.Files)
	out, err = rootRun(t, "--json", "rules")
	require.NoError(t, err)
	require.Contains(t, out, `"files": []`)
}

func TestVibesShowConcatenates(t *testing.T) {
	env := vibesSetup(t)
	shared := env.write(t, env.home, "shared.md", "# shared\nno trailing newline")
	env.write(t, env.repo, "rules.md", "# local\nline\n")
	_, err := rootRun(t, "--global", "rules", "--add", shared)
	require.NoError(t, err)
	_, err = rootRun(t, "rules", "--add", "rules.md")
	require.NoError(t, err)

	out, err := rootRun(t, "rules", "--show")
	require.NoError(t, err)
	require.Equal(t, "# shared\nno trailing newline\n\n# local\nline\n", out)

	out, err = rootRun(t, "--global", "rules", "--show")
	require.NoError(t, err)
	require.Equal(t, "# shared\nno trailing newline\n", out)

	res := vibesJSON(t, "rules", "--show")
	require.Len(t, res.Files, 2)
	require.Equal(t, "# shared\nno trailing newline", res.Files[0].Content)
	require.Equal(t, "# local\nline\n", res.Files[1].Content)

	// Nothing configured prints nothing.
	out, err = rootRun(t, "vibes", "--show")
	require.NoError(t, err)
	require.Empty(t, out)
}

func TestVibesShowMissingFileFails(t *testing.T) {
	env := vibesSetup(t)
	path := env.write(t, env.repo, "vibes.md", "x\n")
	_, err := rootRun(t, "vibes", "--add", "vibes.md")
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))

	_, err = rootRun(t, "vibes", "--show")
	require.Error(t, err)
	require.Contains(t, err.Error(), "vibes: repo file open "+path)
}

func TestVibesFlagsAreMutuallyExclusive(t *testing.T) {
	env := vibesSetup(t)
	env.write(t, env.repo, "a.md", "a\n")
	for _, args := range [][]string{
		{"vibes", "--add", "a.md", "--list"},
		{"vibes", "--add", "a.md", "--remove", "a.md"},
		{"vibes", "--list", "--show"},
		{"rules", "--remove", "a.md", "--show"},
	} {
		_, err := rootRun(t, args...)
		require.Error(t, err, strings.Join(args, " "))
		require.Contains(t, err.Error(), "none of the others can be", strings.Join(args, " "))
	}
	_, exists, err := config.ReadFile(config.RepoConfigPath(env.repo))
	require.NoError(t, err)
	require.False(t, exists)
}

func TestVibesRejectsPositionalArgs(t *testing.T) {
	vibesSetup(t)
	_, err := rootRun(t, "vibes", "vibes.md")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown command")
}

func TestVibesMalformedConfigIsReported(t *testing.T) {
	env := vibesSetup(t)
	env.write(t, env.repo, "vibes.md", "x\n")
	cfg := config.RepoConfigPath(env.repo)
	env.write(t, env.repo, filepath.Join(config.RepoDirName, config.FileName), "vibes = [\n")

	_, err := rootRun(t, "vibes", "--add", "vibes.md")
	require.Error(t, err)
	require.Contains(t, err.Error(), cfg+":")

	_, err = rootRun(t, "vibes", "--list")
	require.Error(t, err)
	require.Contains(t, err.Error(), cfg+":")
}

func TestVibesRulesRegisteredAndSorted(t *testing.T) {
	root := NewRootCommand(&bytes.Buffer{})
	var names []string
	for _, c := range root.Commands() {
		names = append(names, c.Name())
	}
	require.Contains(t, names, "vibes")
	require.Contains(t, names, "rules")
}
