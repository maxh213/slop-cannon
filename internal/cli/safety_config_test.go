package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
)

// configTestEnv points HOME and the XDG directories at a fresh temp tree and
// creates a fake git repository (a directory with a .git entry) in it. base
// is the parent of both so tests can replace it in captured output.
func configTestEnv(t *testing.T) (base, repo string) {
	t.Helper()
	base = t.TempDir()
	home := filepath.Join(base, "home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("NO_COLOR", "")
	require.NoError(t, os.Unsetenv("NO_COLOR"))
	repo = filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	return base, repo
}

// configWriteFile writes content to path, creating parent directories.
func configWriteFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// configReadDoc parses the config file at path.
func configReadDoc(t *testing.T, path string) *config.File {
	t.Helper()
	f, exists, err := config.ReadFile(path)
	require.NoError(t, err)
	require.True(t, exists, "config file %s missing", path)
	return f
}

// configNormalise replaces the temp base directory with <tmp> so goldens
// are stable across runs.
func configNormalise(out, base string) string {
	return strings.ReplaceAll(out, base, "<tmp>")
}

const configTestGlobalTOML = `schema_version = 1
active_cartridge = "claude"
ammo = 5

[cartridges.claude]
model = "fable"
`

const configTestRepoTOML = `schema_version = 1
ammo = 12
safety = false
check_command = "make check"

[limits]
max_review_rounds = 3

[roles.tester]
cartridge = "claude"
`

func TestConfigShowGoldenDefaults(t *testing.T) {
	base, repo := configTestEnv(t)
	out, err := rootRun(t, "--repo", repo, "config", "show")
	require.NoError(t, err)
	rootGolden(t, "config_show_defaults", configNormalise(out, base))
}

func TestConfigShowGolden(t *testing.T) {
	base, repo := configTestEnv(t)
	configWriteFile(t, config.GlobalPath(), configTestGlobalTOML)
	configWriteFile(t, config.RepoConfigPath(repo), configTestRepoTOML)

	out, err := rootRun(t, "--repo", repo, "config", "show")
	require.NoError(t, err)
	rootGolden(t, "config_show", configNormalise(out, base))
}

func TestConfigShowJSONGolden(t *testing.T) {
	base, repo := configTestEnv(t)
	rules := filepath.Join(base, "home", "rules.md")
	configWriteFile(t, rules, "# rules\n")
	configWriteFile(t, filepath.Join(repo, "vibes.md"), "# vibes\n")
	configWriteFile(t, filepath.Join(repo, "docs", "rules.md"), "# repo rules\n")
	// Top-level keys must precede the tables in the fixtures.
	configWriteFile(t, config.GlobalPath(), "rules = [\"../../rules.md\"]\n"+configTestGlobalTOML)
	// active_cartridge in the repo file is ignored with a warning (D-03).
	configWriteFile(t, config.RepoConfigPath(repo),
		"vibes = [\"../vibes.md\"]\nrules = [\"../docs/rules.md\"]\nactive_cartridge = \"claude\"\n"+configTestRepoTOML)

	out, err := rootRun(t, "--repo", repo, "--json", "config", "show")
	require.NoError(t, err)
	var got ConfigShow
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.Equal(t, 12, got.Ammo)
	require.Equal(t, config.SourceMerged, got.Sources[config.KeyRules])
	require.Len(t, got.Warnings, 1)
	rootGolden(t, "config_show_json", configNormalise(out, base))
}

func TestConfigShowMergedListsAndWarnings(t *testing.T) {
	base, repo := configTestEnv(t)
	rules := filepath.Join(base, "home", "rules.md")
	configWriteFile(t, rules, "# rules\n")
	configWriteFile(t, filepath.Join(repo, "vibes.md"), "# vibes\n")
	configWriteFile(t, config.GlobalPath(), "rules = [\"../../rules.md\"]\n")
	configWriteFile(t, config.RepoConfigPath(repo), "vibes = [\"../vibes.md\"]\nrules = [\"../vibes.md\"]\n[cartridges.claude]\nmodel = \"x\"\n")

	out, err := rootRun(t, "--repo", repo, "config", "show")
	require.NoError(t, err)
	require.Regexp(t, `(?m)^vibes +\[`+filepath.Join(repo, "vibes.md")+`\] +repo$`, out)
	require.Regexp(t, `(?m)^rules +\[`+rules+`, `+filepath.Join(repo, "vibes.md")+`\] +global\+repo$`, out)
	require.Contains(t, out, "warning: "+config.RepoConfigPath(repo)+": cartridges and active_cartridge are ignored here")
	require.Contains(t, out, "\nglobal config  "+config.GlobalPath()+"\n")
	require.Contains(t, out, "\nrepo config    "+config.RepoConfigPath(repo)+"\n")
}

func TestConfigShowOutsideGitRepo(t *testing.T) {
	base, _ := configTestEnv(t)
	dir := filepath.Join(base, "plain")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	out, err := rootRun(t, "--repo", dir, "config", "show")
	require.NoError(t, err)
	require.Contains(t, out, "global config  "+config.GlobalPath()+" (not found)\n")
	require.Contains(t, out, "repo config    "+config.RepoConfigPath(dir)+" (not found)\n")
}

func TestConfigShowReportsMalformedFile(t *testing.T) {
	_, repo := configTestEnv(t)
	configWriteFile(t, config.RepoConfigPath(repo), "ammo = \"lots\"\n")
	_, err := rootRun(t, "--repo", repo, "config", "show")
	require.Error(t, err)
	var perr *config.ParseError
	require.ErrorAs(t, err, &perr)
	require.Contains(t, err.Error(), config.RepoConfigPath(repo)+":1:")

	configWriteFile(t, config.RepoConfigPath(repo), "ammo = 2\n")
	_, err = rootRun(t, "--repo", repo, "config", "show")
	require.Error(t, err)
	require.Contains(t, err.Error(), "ammo must be at least 3")
}

func TestConfigBareCommandPrintsHelp(t *testing.T) {
	out, err := rootRun(t, "config")
	require.NoError(t, err)
	require.Contains(t, out, "Available Commands:")
	require.Contains(t, out, "  show ")
	require.Contains(t, out, "  path ")
	require.Contains(t, out, "  edit ")

	_, err = rootRun(t, "config", "shwo")
	require.Error(t, err)
	require.Contains(t, err.Error(), `unknown command "shwo" for "sc config"`)
}

func TestConfigPath(t *testing.T) {
	_, repo := configTestEnv(t)
	configWriteFile(t, config.GlobalPath(), "ammo = 4\n")
	global, repoPath := config.GlobalPath(), config.RepoConfigPath(repo)

	out, err := rootRun(t, "--repo", repo, "config", "path")
	require.NoError(t, err)
	require.Equal(t, "global  "+global+"\nrepo    "+repoPath+" (not found)\nwrites  "+repoPath+" (repo)\n", out)

	out, err = rootRun(t, "--repo", repo, "--global", "config", "path")
	require.NoError(t, err)
	require.Contains(t, out, "\nwrites  "+global+" (global)\n")

	out, err = rootRun(t, "--repo", repo, "--json", "config", "path")
	require.NoError(t, err)
	var got ConfigPaths
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Equal(t, ConfigPaths{
		GlobalPath: global, GlobalExists: true,
		RepoRoot: repo, RepoPath: repoPath, RepoExists: false,
		InGitRepo: true, Target: config.ScopeRepo, TargetPath: repoPath,
	}, got)
}

func TestConfigPathFindsRepoRootFromSubdirectory(t *testing.T) {
	_, repo := configTestEnv(t)
	sub := filepath.Join(repo, "a", "b")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	out, err := rootRun(t, "--repo", sub, "--json", "config", "path")
	require.NoError(t, err)
	var got ConfigPaths
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Equal(t, repo, got.RepoRoot)
	require.Equal(t, config.RepoConfigPath(repo), got.RepoPath)
}

func TestConfigPathOutsideGitRepo(t *testing.T) {
	base, _ := configTestEnv(t)
	dir := filepath.Join(base, "plain")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	out, err := rootRun(t, "--repo", dir, "config", "path")
	require.NoError(t, err)
	require.Contains(t, out, "repo    "+config.RepoConfigPath(dir)+" (not found) ("+dir+" is not inside a git repository)\n")
	require.Contains(t, out, "writes  "+config.GlobalPath()+" (global)\n")

	out, err = rootRun(t, "--repo", dir, "--json", "config", "path")
	require.NoError(t, err)
	var got ConfigPaths
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.False(t, got.InGitRepo)
	require.Equal(t, config.ScopeGlobal, got.Target)
	require.Equal(t, config.GlobalPath(), got.TargetPath)
}

// configFakeEditor installs a shell script as $EDITOR that runs body with
// the file to edit in $1 and returns its path.
func configFakeEditor(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake editor needs a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "fake-editor")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	t.Setenv("EDITOR", path)
	t.Setenv("VISUAL", "")
	return path
}

func TestConfigEditCreatesAndValidates(t *testing.T) {
	_, repo := configTestEnv(t)
	configFakeEditor(t, `echo "opening $1"; printf 'ammo = 7\n' >> "$1"`)
	repoPath := config.RepoConfigPath(repo)

	out, err := rootRun(t, "--repo", repo, "config", "edit")
	require.NoError(t, err)
	require.Equal(t, "opening "+repoPath+"\n", out, "the editor's own output is passed through; sc adds nothing")
	f := configReadDoc(t, repoPath)
	require.Equal(t, config.SchemaVersion, f.SchemaVersion, "a new file is created with the schema version before editing")
	require.NotNil(t, f.Ammo)
	require.Equal(t, 7, *f.Ammo)

	// A second edit reports the existing file and --json describes the call.
	editor := configFakeEditor(t, `:`)
	out, err = rootRun(t, "--repo", repo, "--json", "config", "edit")
	require.NoError(t, err)
	var got ConfigEdit
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.Equal(t, ConfigEdit{Scope: config.ScopeRepo, Path: repoPath, Created: false, Editor: editor}, got)
}

func TestConfigEditGlobalAndEditorArguments(t *testing.T) {
	_, repo := configTestEnv(t)
	editor := configFakeEditor(t, `[ "$1" = "--wait" ] || exit 9; printf 'safety = false\n' >> "$2"`)
	t.Setenv("EDITOR", editor+" --wait")

	out, err := rootRun(t, "--repo", repo, "--global", "--json", "config", "edit")
	require.NoError(t, err)
	var got ConfigEdit
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Equal(t, ConfigEdit{Scope: config.ScopeGlobal, Path: config.GlobalPath(), Created: true, Editor: editor + " --wait"}, got)
	f := configReadDoc(t, config.GlobalPath())
	require.NotNil(t, f.Safety)
	require.False(t, *f.Safety)
	_, err = os.Stat(config.RepoConfigPath(repo))
	require.True(t, os.IsNotExist(err), "--global must not touch the repo file")
}

func TestConfigEditFallsBackToVisual(t *testing.T) {
	_, repo := configTestEnv(t)
	editor := configFakeEditor(t, `printf 'ammo = 8\n' >> "$1"`)
	t.Setenv("EDITOR", "")
	t.Setenv("VISUAL", editor)
	_, err := rootRun(t, "--repo", repo, "config", "edit")
	require.NoError(t, err)
	f := configReadDoc(t, config.RepoConfigPath(repo))
	require.NotNil(t, f.Ammo)
	require.Equal(t, 8, *f.Ammo)
}

func TestConfigEditReportsBrokenResult(t *testing.T) {
	_, repo := configTestEnv(t)
	repoPath := config.RepoConfigPath(repo)

	configFakeEditor(t, `printf 'colour = "blue"\n' >> "$1"`)
	_, err := rootRun(t, "--repo", repo, "config", "edit")
	require.Error(t, err)
	var perr *config.ParseError
	require.ErrorAs(t, err, &perr)
	// Line 1 is the header comment, line 2 the schema_version the file was
	// created with, so the appended key sits on line 3.
	require.Contains(t, err.Error(), repoPath+":3:1: ")
	require.Contains(t, err.Error(), `unknown key "colour"`)
	require.Contains(t, string(configMustRead(t, repoPath)), "colour", "the file is left as edited")

	// A broken file can still be opened, here to replace it with one whose
	// values fail validation.
	configFakeEditor(t, `printf 'schema_version = 1\nammo = 1\n' > "$1"`)
	_, err = rootRun(t, "--repo", repo, "config", "edit")
	require.Error(t, err)
	var verr *config.ValidationError
	require.ErrorAs(t, err, &verr)
	require.Contains(t, err.Error(), "ammo must be at least 3")
}

// configMustRead returns the content of path.
func configMustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func TestConfigEditErrors(t *testing.T) {
	_, repo := configTestEnv(t)
	t.Setenv("EDITOR", "")
	t.Setenv("VISUAL", "")
	_, err := rootRun(t, "--repo", repo, "config", "edit")
	require.Error(t, err)
	require.Contains(t, err.Error(), "set $EDITOR")
	_, statErr := os.Stat(config.RepoConfigPath(repo))
	require.True(t, os.IsNotExist(statErr), "nothing is created when there is no editor")

	configFakeEditor(t, `exit 3`)
	_, err = rootRun(t, "--repo", repo, "config", "edit")
	require.Error(t, err)
	require.Contains(t, err.Error(), "editor ")
	require.Contains(t, err.Error(), "exit status 3")
}

func TestSafetySetWritesRepoConfig(t *testing.T) {
	_, repo := configTestEnv(t)
	repoPath := config.RepoConfigPath(repo)

	out, err := rootRun(t, "--repo", repo, "safety", "off")
	require.NoError(t, err)
	require.Equal(t, "safety off: "+SafetyOffMeaning+"\nsaved to "+repoPath+"\n", out)
	f := configReadDoc(t, repoPath)
	require.NotNil(t, f.Safety)
	require.False(t, *f.Safety)
	_, statErr := os.Stat(config.GlobalPath())
	require.True(t, os.IsNotExist(statErr), "inside a repo the global file is untouched")

	out, err = rootRun(t, "--repo", repo, "safety", "on")
	require.NoError(t, err)
	require.Equal(t, "safety on: "+SafetyOnMeaning+"\nsaved to "+repoPath+"\n", out)
	f = configReadDoc(t, repoPath)
	require.True(t, *f.Safety)
}

func TestSafetySetGlobal(t *testing.T) {
	base, repo := configTestEnv(t)

	// --global forces the global file even inside a repo.
	out, err := rootRun(t, "--repo", repo, "--global", "safety", "off")
	require.NoError(t, err)
	require.Contains(t, out, "saved to "+config.GlobalPath()+"\n")
	f := configReadDoc(t, config.GlobalPath())
	require.False(t, *f.Safety)
	_, statErr := os.Stat(config.RepoConfigPath(repo))
	require.True(t, os.IsNotExist(statErr))

	// Outside a git repo the global file is the target without --global.
	plain := filepath.Join(base, "plain")
	require.NoError(t, os.MkdirAll(plain, 0o755))
	out, err = rootRun(t, "--repo", plain, "safety", "on")
	require.NoError(t, err)
	require.Contains(t, out, "saved to "+config.GlobalPath()+"\n")
	f = configReadDoc(t, config.GlobalPath())
	require.True(t, *f.Safety)
}

func TestSafetyAcceptsBooleanSpellings(t *testing.T) {
	_, repo := configTestEnv(t)
	for arg, want := range map[string]bool{"true": true, "false": false, "yes": true, "no": false, "1": true, "0": false, "OFF": false, "On": true} {
		out, err := rootRun(t, "--repo", repo, "safety", arg)
		require.NoError(t, err, arg)
		require.True(t, strings.HasPrefix(out, "safety "+safetyWord(want)+": "), "%s: %s", arg, out)
		f := configReadDoc(t, config.RepoConfigPath(repo))
		require.Equal(t, want, *f.Safety, arg)
	}
}

func TestSafetyRejectsBadValues(t *testing.T) {
	_, repo := configTestEnv(t)
	for _, arg := range []string{"maybe", "", "2"} {
		_, err := rootRun(t, "--repo", repo, "safety", arg)
		require.Error(t, err, arg)
		require.Contains(t, err.Error(), "safety must be on or off", arg)
	}
	_, err := rootRun(t, "--repo", repo, "safety", "on", "off")
	require.Error(t, err)
	require.Contains(t, err.Error(), "accepts at most 1 arg(s)")
	_, statErr := os.Stat(config.RepoConfigPath(repo))
	require.True(t, os.IsNotExist(statErr), "nothing is written on a bad value")
}

func TestSafetyJSON(t *testing.T) {
	_, repo := configTestEnv(t)
	out, err := rootRun(t, "--repo", repo, "--json", "safety", "off")
	require.NoError(t, err)
	var got SafetyResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Equal(t, SafetyResult{Safety: false, Meaning: SafetyOffMeaning, Scope: config.ScopeRepo, Path: config.RepoConfigPath(repo)}, got)
}

func TestSafetyShow(t *testing.T) {
	_, repo := configTestEnv(t)

	out, err := rootRun(t, "--repo", repo, "safety")
	require.NoError(t, err)
	require.Equal(t, "safety on (from default): "+SafetyOnMeaning+"\n", out)

	out, err = rootRun(t, "--repo", repo, "--json", "safety")
	require.NoError(t, err)
	var got SafetyResult
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Equal(t, SafetyResult{Safety: true, Meaning: SafetyOnMeaning, Source: config.SourceDefault}, got)

	configWriteFile(t, config.GlobalPath(), "safety = false\n")
	out, err = rootRun(t, "--repo", repo, "safety")
	require.NoError(t, err)
	require.Equal(t, "safety off (from global): "+SafetyOffMeaning+"\n", out)

	configWriteFile(t, config.RepoConfigPath(repo), "safety = true\n")
	out, err = rootRun(t, "--repo", repo, "--json", "safety")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Equal(t, SafetyResult{Safety: true, Meaning: SafetyOnMeaning, Scope: config.ScopeRepo, Path: config.RepoConfigPath(repo), Source: config.SourceRepo}, got)
}

func TestSafetyMeaning(t *testing.T) {
	require.Equal(t, SafetyOnMeaning, SafetyMeaning(true))
	require.Equal(t, SafetyOffMeaning, SafetyMeaning(false))
	require.Contains(t, SafetyOnMeaning, "pull request")
	require.Contains(t, SafetyOffMeaning, "fast-forward")
}
