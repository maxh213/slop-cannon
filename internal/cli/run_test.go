package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
)

// runTestEnv isolates HOME and the XDG directories under a temp base and
// creates a fake git repository (a .git directory plus a sub/ directory)
// in it. The clock and id hooks are pinned so output is deterministic.
func runTestEnv(t *testing.T) (base, repo string) {
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
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "sub"), 0o755))
	runFixClock(t)
	return base, repo
}

// runTestStart is the pinned start time of every run in these tests.
var runTestStart = time.Date(2026, 9, 2, 1, 2, 3, 0, time.UTC)

// runFixClock pins the clock and the id generator for the test.
func runFixClock(t *testing.T) {
	t.Helper()
	savedNow, savedID := runNow, runNewID
	t.Cleanup(func() { runNow, runNewID = savedNow, savedID })
	runNow = func() time.Time { return runTestStart }
	runNewID = func(now time.Time) domain.RunID { return domain.RunID(now.UTC().Format("20060102-150405") + "-abcd") }
}

// runAddCartridge writes a global config with one active cartridge.
func runAddCartridge(t *testing.T, name, model string) {
	t.Helper()
	doc, err := config.OpenGlobal()
	require.NoError(t, err)
	_, err = doc.AddCartridge(name, model)
	require.NoError(t, err)
	require.NoError(t, doc.Save())
}

// runExec runs the command tree the way main does and returns stdout,
// stderr and the exit code.
func runExec(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = ExecuteContext(context.Background(), args, &out, &errOut)
	return out.String(), errOut.String(), code
}

// runJSON runs the command with --json and decodes the RunConfig.
func runJSON(t *testing.T, args ...string) config.RunConfig {
	t.Helper()
	stdout, stderr, code := runExec(t, append([]string{"--json"}, args...)...)
	require.Equal(t, 0, code, stderr)
	require.Equal(t, runNote+"\n", stderr)
	var rc config.RunConfig
	require.NoError(t, json.Unmarshal([]byte(stdout), &rc), stdout)
	return rc
}

// runNormalise replaces the run state directory with <state> and the temp
// base with <tmp> so goldens are stable.
func runNormalise(out, base, repo string) string {
	out = strings.ReplaceAll(out, config.StateDir(repo), "<state>")
	return strings.ReplaceAll(out, base, "<tmp>")
}

func TestRunHelpGolden(t *testing.T) {
	out, err := rootRun(t, "run", "--help")
	require.NoError(t, err)
	rootGolden(t, "run-help", out)
}

func TestRunREADMEExamples(t *testing.T) {
	_, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	t.Chdir(filepath.Dir(repo))
	require.NoError(t, os.Rename(repo, filepath.Join(filepath.Dir(repo), "repo-path")))
	repo = filepath.Join(filepath.Dir(repo), "repo-path")

	// sc ./repo-path — autopilot with the defaults.
	rc := runJSON(t, "./repo-path")
	require.Equal(t, domain.RunModeAutopilot, rc.Run.Mode)
	require.Empty(t, rc.Run.Brief)
	require.Equal(t, repo, rc.Run.RepoPath)
	require.Equal(t, domain.RunID("20260902-010203-abcd"), rc.Run.ID)
	require.Equal(t, config.RunDir(repo, "20260902-010203-abcd"), rc.Run.StateDir)
	require.Equal(t, runTestStart, rc.Run.StartedAt)
	require.Equal(t, "claude", rc.CartridgeName)
	require.Equal(t, "fable", rc.Cartridge.Model)
	want := domain.ConfigSnapshot{Ammo: 10, Safety: true, Cartridge: "claude", Model: "fable", ReviewPRs: true}
	require.Equal(t, want, rc.Run.Config)
	require.Equal(t, config.Allocation{Ammo: 10, PO: 1, MaxEngineers: 2, MaxTesters: 7}, rc.Allocation)
	require.False(t, rc.Resume)
	require.False(t, rc.NoAnim)

	// sc ./repo-path -m "here's what I want done" — brief mode.
	rc = runJSON(t, "./repo-path", "-m", "here's what I want done")
	require.Equal(t, domain.RunModeBrief, rc.Run.Mode)
	require.Equal(t, "here's what I want done", rc.Run.Brief)
	rc = runJSON(t, "./repo-path", "--message", "long form")
	require.Equal(t, "long form", rc.Run.Brief)
}

func TestRunTextGolden(t *testing.T) {
	base, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	vibes := filepath.Join(base, "vibes.md")
	require.NoError(t, os.WriteFile(vibes, []byte("be nice\n"), 0o644))
	configWriteFile(t, config.RepoConfigPath(repo), "ammo = 12\nvibes = [\"../vibes.md\"]\n")

	stdout, stderr, code := runExec(t, repo, "-m", "here's what I want done", "--safety", "off", "--ammo", "8", "--no-anim", "--headless")
	require.Equal(t, 0, code, stderr)
	require.Equal(t, runNote+"\n", stderr)
	rootGolden(t, "run-brief", runNormalise(stdout, base, repo))

	// Autopilot has no brief row and keeps the defaults.
	stdout, _, code = runExec(t, "run", repo)
	require.Equal(t, 0, code)
	rootGolden(t, "run-autopilot", runNormalise(stdout, base, repo))
}

func TestRunJSONGolden(t *testing.T) {
	base, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	stdout, stderr, code := runExec(t, "--json", repo, "-m", "fix the bugs", "--review-prs=false")
	require.Equal(t, 0, code, stderr)
	require.Equal(t, runNote+"\n", stderr)
	rootGolden(t, "run-json", runNormalise(stdout, base, repo))
}

func TestRunRefusesWithoutCartridge(t *testing.T) {
	_, repo := runTestEnv(t)
	stdout, stderr, code := runExec(t, repo)
	require.Equal(t, 1, code)
	require.Empty(t, stdout)
	require.Equal(t, "sc: no cartridge loaded — add one with: sc cartridge --add claude --model <model>\n", stderr)

	// A configured but inactive cartridge does not count.
	configWriteFile(t, config.GlobalPath(), "[cartridges.claude]\nmodel = \"fable\"\n")
	_, stderr, code = runExec(t, repo)
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "no cartridge loaded — add one with: "+CartridgeAddHint)

	_, err := ResolveRun(RunOptions{Path: repo})
	require.ErrorIs(t, err, config.ErrNoCartridge)
}

func TestRunRefusesNonRepo(t *testing.T) {
	base, _ := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	plain := filepath.Join(base, "plain")
	require.NoError(t, os.MkdirAll(plain, 0o755))

	_, stderr, code := runExec(t, plain)
	require.Equal(t, 1, code)
	require.Equal(t, "sc: "+plain+" is not a git repository (no .git in it or any parent directory)\n", stderr)

	_, stderr, code = runExec(t, filepath.Join(base, "missing"))
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "missing: no such directory")

	file := filepath.Join(base, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	_, stderr, code = runExec(t, "run", file)
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "file: not a directory")

	// The git check comes before the cartridge check.
	configWriteFile(t, config.GlobalPath(), "")
	_, stderr, code = runExec(t, plain)
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "is not a git repository")
}

func TestRunSubdirectoryResolvesToRoot(t *testing.T) {
	_, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	rc := runJSON(t, filepath.Join(repo, "sub"))
	require.Equal(t, repo, rc.Run.RepoPath)

	// The run subcommand without a path uses --repo (default: cwd).
	t.Chdir(filepath.Join(repo, "sub"))
	rc = runJSON(t, "run")
	require.Equal(t, repo, rc.Run.RepoPath)
	rc = runJSON(t, "--repo", repo, "run")
	require.Equal(t, repo, rc.Run.RepoPath)
	rc = runJSON(t, "run", ".")
	require.Equal(t, repo, rc.Run.RepoPath)

	_, err := rootRun(t, "run", repo, repo)
	require.Error(t, err)
	require.Contains(t, err.Error(), "accepts at most 1 arg(s)")
}

func TestRunFlagOverridesAndSources(t *testing.T) {
	_, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	configWriteFile(t, config.GlobalPath(), "safety = false\n"+configTestGlobalTOML)
	configWriteFile(t, config.RepoConfigPath(repo), "ammo = 12\n")

	// Files only: repo ammo beats global ammo; global safety applies.
	rc := runJSON(t, repo)
	require.Equal(t, 12, rc.Run.Config.Ammo)
	require.False(t, rc.Run.Config.Safety)
	require.Equal(t, config.SourceRepo, rc.Effective.Sources[config.KeyAmmo])
	require.Equal(t, config.SourceGlobal, rc.Effective.Sources[config.KeySafety])

	// Flags beat both files.
	rc = runJSON(t, repo, "--ammo", "6", "--safety", "on")
	require.Equal(t, 6, rc.Run.Config.Ammo)
	require.Equal(t, config.Allocation{Ammo: 6, PO: 1, MaxEngineers: 1, MaxTesters: 4}, rc.Allocation)
	require.True(t, rc.Run.Config.Safety)
	require.Equal(t, config.SourceFlag, rc.Effective.Sources[config.KeyAmmo])
	require.Equal(t, config.SourceFlag, rc.Effective.Sources[config.KeySafety])

	stdout, _, code := runExec(t, repo, "--ammo", "6", "--safety", "on")
	require.Equal(t, 0, code)
	require.Contains(t, stdout, "ammo       6 → 1 PO, up to 1 engineer, up to 4 testers (from flag)\n")
	require.Contains(t, stdout, "safety     on (from flag): "+SafetyOnMeaning+"\n")

	// Every spelling ParseSafety accepts works on the flag too.
	for _, word := range []string{"off", "false", "no", "0"} {
		rc = runJSON(t, repo, "--safety", word)
		require.False(t, rc.Run.Config.Safety, word)
	}
	for _, word := range []string{"on", "true", "yes", "1"} {
		rc = runJSON(t, repo, "--safety", word)
		require.True(t, rc.Run.Config.Safety, word)
	}
}

func TestRunPresentationFlags(t *testing.T) {
	_, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")

	rc := runJSON(t, repo)
	require.False(t, rc.Run.Config.Headless)
	require.True(t, rc.Run.Config.ReviewPRs)
	require.False(t, rc.NoAnim)

	rc = runJSON(t, repo, "--headless", "--no-anim", "--review-prs=false")
	require.True(t, rc.Run.Config.Headless)
	require.False(t, rc.Run.Config.ReviewPRs)
	require.True(t, rc.NoAnim)

	rc = runJSON(t, repo, "--review-prs=true")
	require.True(t, rc.Run.Config.ReviewPRs)

	stdout, _, code := runExec(t, repo, "--headless", "--no-anim")
	require.Equal(t, 0, code)
	require.Contains(t, stdout, "headless   true\n")
	require.Contains(t, stdout, "animation  off\n")
}

func TestRunCartridgeFlag(t *testing.T) {
	_, repo := runTestEnv(t)
	cartridgeAllowMock(t)
	runAddCartridge(t, "claude", "fable")
	runAddCartridge(t, "mock", "")

	rc := runJSON(t, repo)
	require.Equal(t, "claude", rc.CartridgeName)
	require.Equal(t, "fable", rc.Run.Config.Model)

	rc = runJSON(t, repo, "--cartridge", "mock")
	require.Equal(t, "mock", rc.CartridgeName)
	require.Equal(t, "mock", rc.Run.Config.Cartridge)
	require.Empty(t, rc.Run.Config.Model)
	require.Equal(t, config.SourceFlag, rc.Effective.Sources[config.KeyActiveCartridge])

	stdout, _, code := runExec(t, repo, "--cartridge", "mock")
	require.Equal(t, 0, code)
	require.Contains(t, stdout, "cartridge  mock (default model, command mock) (from flag)\n")

	_, stderr, code := runExec(t, repo, "--cartridge", "kimi")
	require.Equal(t, 1, code)
	require.Equal(t, "sc: cartridge \"kimi\" is not supported yet (only \"claude\", \"mock\" for now)\n", stderr)

	_, stderr, code = runExec(t, repo, "--cartridge", "")
	require.Equal(t, 1, code)
	require.Equal(t, "sc: --cartridge must not be empty\n", stderr)
}

func TestRunCartridgeFlagMustBeConfigured(t *testing.T) {
	_, repo := runTestEnv(t)
	cartridgeAllowMock(t)
	runAddCartridge(t, "claude", "fable")
	_, stderr, code := runExec(t, repo, "--cartridge", "mock")
	require.Equal(t, 1, code)
	require.Equal(t, "sc: cartridge \"mock\" is not configured; add it with: sc cartridge --add mock\n", stderr)
}

func TestRunFlagErrors(t *testing.T) {
	_, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	cases := map[string][]string{
		"sc: --ammo 2: ammo must be at least 3: one engineer, one tester, one product owner\n":     {repo, "--ammo", "2"},
		"sc: --safety: safety must be on or off, got \"maybe\"\n":                                  {repo, "--safety", "maybe"},
		"sc: --message must not be empty\n":                                                        {repo, "-m", "  "},
		"sc: --message cannot be combined with --resume: a resumed run keeps its original brief\n": {repo, "-m", "x", "--resume"},
	}
	for want, args := range cases {
		stdout, stderr, code := runExec(t, args...)
		require.Equal(t, 1, code, args)
		require.Empty(t, stdout, args)
		require.Equal(t, want, stderr, args)
	}
	_, err := rootRun(t, repo, "--ammo", "lots")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid argument")
}

func TestRunResume(t *testing.T) {
	base, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")

	_, stderr, code := runExec(t, repo, "--resume")
	require.Equal(t, 1, code)
	require.Equal(t, "sc: nothing to resume: no runs recorded for "+repo+" (looked in "+
		filepath.Join(config.StateDir(repo), config.RunsDirName)+")\n", stderr)

	for _, id := range []string{"20260901-000000-1111", "20260902-000000-2222"} {
		require.NoError(t, os.MkdirAll(config.RunDir(repo, id), 0o755))
	}
	rc := runJSON(t, repo, "--resume")
	require.True(t, rc.Resume)
	require.Equal(t, domain.RunID("20260902-000000-2222"), rc.Run.ID)
	require.Equal(t, config.RunDir(repo, "20260902-000000-2222"), rc.Run.StateDir)
	require.Equal(t, domain.RunModeAutopilot, rc.Run.Mode)

	stdout, _, code := runExec(t, repo, "--resume")
	require.Equal(t, 0, code)
	require.Contains(t, runNormalise(stdout, base, repo), "run        20260902-000000-2222 (resumed)\n")
}

func TestRunReportsConfigErrors(t *testing.T) {
	_, repo := runTestEnv(t)
	configWriteFile(t, config.GlobalPath(), "ammo = [\n")
	_, stderr, code := runExec(t, repo)
	require.Equal(t, 1, code)
	require.True(t, strings.HasPrefix(stderr, "sc: "+config.GlobalPath()+":1:"), stderr)

	configWriteFile(t, config.GlobalPath(), "ammo = 1\n")
	_, stderr, code = runExec(t, repo)
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "ammo must be at least 3")
}

func TestRunRootAndSubcommandAgree(t *testing.T) {
	_, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	args := []string{"-m", "same", "--ammo", "9", "--safety", "off", "--headless", "--no-anim", "--review-prs=false"}
	viaRoot := runJSON(t, append([]string{repo}, args...)...)
	viaRun := runJSON(t, append([]string{"run", repo}, args...)...)
	require.Equal(t, viaRoot, viaRun)

	// Flags may come before the path as well.
	viaFlagsFirst := runJSON(t, append(args, repo)...)
	require.Equal(t, viaRoot, viaFlagsFirst)
}

func TestRunOptionsAreNotPersistent(t *testing.T) {
	// Run flags belong to the run form only; config subcommands reject them.
	_, err := rootRun(t, "ammo", "-m", "x")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown shorthand flag")
}

func TestResolveRunDirect(t *testing.T) {
	_, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	ammo, safety := 4, false
	rc, err := ResolveRun(RunOptions{
		Path:      filepath.Join(repo, "sub"),
		Brief:     "do it",
		Headless:  true,
		ReviewPRs: true,
		Overrides: config.Overrides{Ammo: &ammo, Safety: &safety},
	})
	require.NoError(t, err)
	require.Equal(t, repo, rc.Run.RepoPath)
	require.Equal(t, domain.RunModeBrief, rc.Run.Mode)
	require.Equal(t, 4, rc.Run.Config.Ammo)
	require.False(t, rc.Run.Config.Safety)
	require.True(t, rc.Run.Config.Headless)
	require.Equal(t, domain.RunID("20260902-010203-abcd"), rc.Run.ID)

	_, err = ResolveRun(RunOptions{Path: filepath.Join(repo, "nope")})
	require.Error(t, err)
}
