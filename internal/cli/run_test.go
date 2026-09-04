package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/logging"
	"slop-cannon/internal/orchestrator"
	"slop-cannon/internal/testutil"
	"slop-cannon/internal/tui"
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
// stderr and the exit code. Other commands' tests use it too, so it adds
// nothing to the arguments.
func runExec(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = ExecuteContext(context.Background(), args, &out, &errOut)
	return out.String(), errOut.String(), code
}

// runResolve is runExec with the hidden --dry-run, so the run form resolves
// its configuration and prints it without firing anything (P8-01).
func runResolve(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return runExec(t, append(append([]string{}, args...), "--"+FlagRunDryRun)...)
}

// runJSON runs the command with --json and decodes the RunConfig.
func runJSON(t *testing.T, args ...string) config.RunConfig {
	t.Helper()
	stdout, stderr, code := runResolve(t, append([]string{"--json"}, args...)...)
	require.Equal(t, 0, code, stderr)
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

	stdout, stderr, code := runResolve(t, repo, "-m", "here's what I want done", "--safety", "off", "--ammo", "8", "--no-anim", "--headless")
	require.Equal(t, 0, code, stderr)
	require.Empty(t, stderr)
	rootGolden(t, "run-brief", runNormalise(stdout, base, repo))

	// Autopilot has no brief row and keeps the defaults.
	stdout, _, code = runResolve(t, "run", repo)
	require.Equal(t, 0, code)
	rootGolden(t, "run-autopilot", runNormalise(stdout, base, repo))
}

func TestRunJSONGolden(t *testing.T) {
	base, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	stdout, stderr, code := runResolve(t, "--json", repo, "-m", "fix the bugs", "--review-prs=false")
	require.Equal(t, 0, code, stderr)
	require.Empty(t, stderr)
	rootGolden(t, "run-json", runNormalise(stdout, base, repo))
}

func TestRunRefusesWithoutCartridge(t *testing.T) {
	_, repo := runTestEnv(t)
	stdout, stderr, code := runResolve(t, repo)
	require.Equal(t, 1, code)
	require.Empty(t, stdout)
	require.Equal(t, "sc: no cartridge loaded — add one with: sc cartridge --add claude --model <model>\n", stderr)

	// A configured but inactive cartridge does not count.
	configWriteFile(t, config.GlobalPath(), "[cartridges.claude]\nmodel = \"fable\"\n")
	_, stderr, code = runResolve(t, repo)
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

	_, stderr, code := runResolve(t, plain)
	require.Equal(t, 1, code)
	require.Equal(t, "sc: "+plain+" is not a git repository (no .git in it or any parent directory)\n", stderr)

	_, stderr, code = runResolve(t, filepath.Join(base, "missing"))
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "missing: no such directory")

	file := filepath.Join(base, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	_, stderr, code = runResolve(t, "run", file)
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "file: not a directory")

	// The git check comes before the cartridge check.
	configWriteFile(t, config.GlobalPath(), "")
	_, stderr, code = runResolve(t, plain)
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

	stdout, _, code := runResolve(t, repo, "--ammo", "6", "--safety", "on")
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

	stdout, _, code := runResolve(t, repo, "--headless", "--no-anim")
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

	stdout, _, code := runResolve(t, repo, "--cartridge", "mock")
	require.Equal(t, 0, code)
	require.Contains(t, stdout, "cartridge  mock (default model, command mock) (from flag)\n")

	_, stderr, code := runResolve(t, repo, "--cartridge", "kimi")
	require.Equal(t, 1, code)
	require.Equal(t, "sc: cartridge \"kimi\" is not supported yet (only \"claude\", \"mock\" for now)\n", stderr)

	_, stderr, code = runResolve(t, repo, "--cartridge", "")
	require.Equal(t, 1, code)
	require.Equal(t, "sc: --cartridge must not be empty\n", stderr)
}

func TestRunCartridgeFlagMustBeConfigured(t *testing.T) {
	_, repo := runTestEnv(t)
	cartridgeAllowMock(t)
	runAddCartridge(t, "claude", "fable")
	_, stderr, code := runResolve(t, repo, "--cartridge", "mock")
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
		stdout, stderr, code := runResolve(t, args...)
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

	_, stderr, code := runResolve(t, repo, "--resume")
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

	stdout, _, code := runResolve(t, repo, "--resume")
	require.Equal(t, 0, code)
	require.Contains(t, runNormalise(stdout, base, repo), "run        20260902-000000-2222 (resumed)\n")
}

// TestRunResumeRestoresTheFrozenSnapshot is P6-11's Done-when at the
// cli.ResolveRun level: a run recorded ammo 10, the config file then says 4,
// and the resumed run still fires with 10 — while an explicit --ammo wins
// and says so.
func TestRunResumeRestoresTheFrozenSnapshot(t *testing.T) {
	_, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")

	const id = "20260903-000000-3333"
	dir := config.RunDir(repo, id)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	rc0, err := ResolveRun(RunOptions{Path: repo, Brief: "ship it"})
	require.NoError(t, err)
	rc0.Run.ID, rc0.Run.StateDir = id, dir
	rc0.Run.Config.Ammo = 10
	require.NoError(t, orchestrator.WriteState(dir, orchestrator.State{
		Version: orchestrator.StateFileVersion,
		Run:     rc0.Run,
		Phase:   orchestrator.PhaseRunning,
	}))

	configWriteFile(t, config.RepoConfigPath(repo), "ammo = 4\n")

	rc, notes, err := ResolveRunWithNotes(RunOptions{Path: repo, Resume: true})
	require.NoError(t, err)
	require.Equal(t, 10, rc.Run.Config.Ammo, "the frozen snapshot wins over the edited config file")
	require.Equal(t, domain.RunModeBrief, rc.Run.Mode)
	require.Equal(t, "ship it", rc.Run.Brief)
	require.Empty(t, notes)

	ammo := 6
	rc, notes, err = ResolveRunWithNotes(RunOptions{
		Path:      repo,
		Resume:    true,
		Overrides: config.Overrides{Ammo: &ammo},
	})
	require.NoError(t, err)
	require.Equal(t, 6, rc.Run.Config.Ammo, "an explicit --ammo wins over the snapshot")
	require.Len(t, notes, 1)
	require.Contains(t, notes[0], "--ammo 6 overrides the run's recorded ammo 10")
}

func TestRunReportsConfigErrors(t *testing.T) {
	_, repo := runTestEnv(t)
	configWriteFile(t, config.GlobalPath(), "ammo = [\n")
	_, stderr, code := runResolve(t, repo)
	require.Equal(t, 1, code)
	require.True(t, strings.HasPrefix(stderr, "sc: "+config.GlobalPath()+":1:"), stderr)

	configWriteFile(t, config.GlobalPath(), "ammo = 1\n")
	_, stderr, code = runResolve(t, repo)
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

// ---------------------------------------------------------------------------
// Firing the cannon for real (P8-01, P8-08, P8-10, P8-12, P8-13)
// ---------------------------------------------------------------------------

// runFireEnv is a machine a whole run can happen on: an isolated home, the
// fixture repository with a bare remote, a stateful `bd` shim, a `gh` shim
// that merges every pull request, and a global configuration whose active
// cartridge is the mock.
//
// The `bd` stand-in is testdata/orchestrator/fakebd, the same stateful
// tracker internal/orchestrator's end-to-end test drives, so `sc run` talks
// to the real beads.Client and the real D-21 argument contract without
// needing bd (and its embedded Dolt) on the machine.
type runFireEnv struct {
	Repo *testutil.FixtureRepo
	BD   *testutil.Shim
	GH   *testutil.Shim
	// Scenario is the mock cartridge's script, so a test can rewrite it
	// before firing.
	Scenario string
}

// runFireBrief is the -m text of the scripted run.
const runFireBrief = "add a short notes file"

// runFireWork is the file the scripted engineer adds.
const runFireWork = "notes/first.md"

// runFireSetup builds that machine and pins the run's timings so a whole
// run takes a fraction of a second.
func runFireSetup(t *testing.T, safety bool) *runFireEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("firing the cannon spawns subprocesses")
	}
	// Build the fake tracker before HOME moves: `go build` would otherwise
	// put its caches inside the test's temporary home.
	bin := runFireBuildBD(t)

	repo := testutil.NewFixtureRepo(t, testutil.WithBareRemote())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	// git runs as a subprocess of the run and inherits this identity, the
	// way testutil.FixtureRepo.Env gives it to its own calls.
	for _, kv := range repo.Env() {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(k, "GIT_") {
			t.Setenv(k, v)
		}
	}

	state := t.TempDir()
	env := &runFireEnv{
		Repo: repo,
		BD:   testutil.FakeBin(t, "bd", fmt.Sprintf(runFireBDScript, state, bin)),
		GH:   testutil.FakeBin(t, "gh", runFireGHScript),
	}

	// The repository already has a bead tracker, so nothing runs `bd init`
	// (D-13); the fake tracker has no init to run.
	require.NoError(t, os.MkdirAll(filepath.Join(repo.Dir, ".beads"), 0o755))

	// The mock adapter registers itself in cartridge.Default from its own
	// init, so importing internal/cartridge/mock is all a test binary needs
	// to select it; `mock` still has to be a known name (P8-08).
	cartridgeAllowMock(t)
	scenario := filepath.Join(t.TempDir(), "scenario.json")
	require.NoError(t, runFireScenario().WriteFile(scenario))
	env.Scenario = scenario
	configWriteFile(t, config.GlobalPath(), fmt.Sprintf(`active_cartridge = "mock"
ammo = 4
safety = %t
check_command = "true"
gate_poll_interval = "20ms"

[cartridges.mock]
env = { SC_MOCK_SCENARIO = %q }
`, safety, scenario))

	saved := [3]time.Duration{runConductorInterval, runTickInterval, runSettleInterval}
	savedShim := runShimOptions
	t.Cleanup(func() {
		runConductorInterval, runTickInterval, runSettleInterval = saved[0], saved[1], saved[2]
		runShimOptions = savedShim
	})
	runConductorInterval, runTickInterval, runSettleInterval = 20*time.Millisecond, 15*time.Millisecond, 20*time.Millisecond
	// The shim's self-check probes the real bd's own --readonly
	// enforcement, which the fake tracker does not implement; the shim
	// itself is still written and still first on every agent's PATH.
	runShimOptions = beads.ShimOptions{SkipVerify: true}
	return env
}

// runFireBuildBD compiles testdata/orchestrator/fakebd and returns the
// binary, cached in the system temporary directory under a name derived
// from the source and the toolchain.
func runFireBuildBD(t *testing.T) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "testdata", "orchestrator", "fakebd"))
	require.NoError(t, err)
	entries, err := os.ReadDir(src)
	require.NoError(t, err, "the fake bd tracker must be in the repository")
	h := sha256.New()
	fmt.Fprintln(h, runtime.Version())
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(src, name))
		require.NoError(t, err)
		fmt.Fprintln(h, name)
		h.Write(data)
	}
	bin := filepath.Join(os.TempDir(), "sc-cli-fakebd-"+hex.EncodeToString(h.Sum(nil))[:16])
	if info, err := os.Stat(bin); err == nil && info.Mode().Perm()&0o111 != 0 {
		return bin
	}
	tmp := fmt.Sprintf("%s.%d", bin, os.Getpid())
	goTool, err := exec.LookPath("go")
	require.NoError(t, err)
	out, err := exec.Command(goTool, "build", "-o", tmp, src).CombinedOutput()
	require.NoErrorf(t, err, "building the fake bd: %s", out)
	require.NoError(t, os.Rename(tmp, bin))
	return bin
}

// runFireBDScript wraps the fake tracker with the one command it does not
// implement: `bd ping`, the health check beads.EnsureInitialised makes
// before a run touches the database (D-13).
const runFireBDScript = `SC_E2E_FAKEBD_DIR=%q
export SC_E2E_FAKEBD_DIR
for a in "$@"; do
  if [ "$a" = "ping" ]; then
    echo '{"schema_version":1,"data":{"status":"ok","total_ms":1}}'
    exit 0
  fi
done
exec %q "$@"
`

// runFireGHScript is a `gh` that opens numbered pull requests and reports
// every one of them as merged, which is what closes a safety-on bead (D-20).
const runFireGHScript = `
case "$1 $2" in
  "pr list") echo '[]' ;;
  "label create") ;;
  "pr create")
    cat > "$SC_FAKEBIN_DIR/body.$SC_FAKEBIN_SEQ"
    n=$(cat "$SC_FAKEBIN_DIR/prcount" 2>/dev/null || echo 0)
    n=$((n + 1))
    echo "$n" > "$SC_FAKEBIN_DIR/prcount"
    echo "https://github.com/acme/widgets/pull/$n"
    ;;
  "pr view") echo '{"state":"MERGED"}' ;;
  *) echo "gh shim: unexpected command: $*" >&2; exit 1 ;;
esac
`

// runFireScenario scripts the whole run: one plan of one bead, one
// engineer, and three approving testers.
func runFireScenario() *mock.Scenario { return runFireScenarioSaying("Adding the note.") }

// runFireScenarioSaying is [runFireScenario] with the engineer's own output
// line under the test's control, so a test can plant something in what a
// cartridge prints.
func runFireScenarioSaying(engineerSays string) *mock.Scenario {
	sc := mock.NewScenario("sc run end to end")
	sc.Rule("plan", mock.Match{Role: string(domain.RolePO), Task: string(events.TaskPlan)}).
		Say("One bead is enough.").
		Structured(domain.Plan{
			Brief: runFireBrief,
			Beads: []domain.PlanBead{{
				Key: "notes", Title: "Add a notes file", Acceptance: runFireWork + " exists",
				Description: "Start the notes directory.", Priority: domain.PriorityDefault,
				Type: domain.BeadTypeTask,
			}},
		})
	sc.Rule("engineer", mock.Match{Role: string(domain.RoleEngineer)}).
		Say(engineerSays).
		Write(runFireWork, "the first note\n").
		Structured(domain.ImplReport{
			Summary: "Added " + runFireWork + ".", FilesChanged: []string{runFireWork},
			HowToVerify: "ls " + runFireWork,
		})
	sc.Rule("tester", mock.Match{Role: string(domain.RoleTester)}).
		Say("Reading the diff.").
		Structured(domain.Verdict{Outcome: domain.VerdictApprove, Summary: "The note reads well."})
	sc.Otherwise("unscripted").
		Say("no rule matched this firing").
		Fail(mock.FailCrash, "the run fired an agent this scenario does not script")
	return sc
}

// runFireTimeout bounds a scripted run. It is generous: the point of the
// test is that the run finishes at all, and a loaded machine running the
// whole package under -race is slow.
const runFireTimeout = 90 * time.Second

// runFireExec runs `sc <args>` with a deadline and returns stdout, stderr
// and the exit code.
func runFireExec(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runFireTimeout)
	defer cancel()
	var out, errOut bytes.Buffer
	code = ExecuteContext(ctx, args, &out, &errOut)
	if ctx.Err() != nil {
		t.Fatalf("the run did not finish within %s\nstdout:\n%s\nstderr:\n%s", runFireTimeout, out.String(), errOut.String())
	}
	return out.String(), errOut.String(), code
}

// TestRunHeadlessWithTheMockCartridge is P8-01's Done-when, and P8-08's and
// P8-10's with it: `sc <fixture> -m "…" --headless --cartridge mock`
// completes, leaves the D-23 run directory with a run log and an event log
// in it, and prints the final summary.
func TestRunHeadlessWithTheMockCartridge(t *testing.T) {
	env := runFireSetup(t, true)

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock", "--verbose")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)

	// The headless renderer wrote the run to stdout, one line per event.
	require.Contains(t, stdout, "fired po(mock) [plan")
	require.Contains(t, stdout, "fired engineer(mock)")
	require.Contains(t, stdout, "says approve")
	require.Contains(t, stdout, "run finished: completed")

	// The final summary is on stderr, so --json's stdout stays parseable.
	require.Contains(t, stderr, "run        "+string(runLatestID(t, env.Repo.Dir)))
	require.Regexp(t, `(?m)^beads      1: 1 done,`, stderr)
	require.Regexp(t, `(?m)^delivery   1 pull request,`, stderr)
	require.Regexp(t, `(?m)^spend      `, stderr)

	// P8-20: the run log is there and folds.
	dir := config.RunDir(env.Repo.Dir, string(runLatestID(t, env.Repo.Dir)))
	sum, err := orchestrator.SummariseLog(dir, -1)
	require.NoError(t, err)
	require.Equal(t, events.FinishCompleted, sum.Finish)
	require.Equal(t, 1, sum.PRs)
	require.Len(t, sum.Beads, 1)
	require.Equal(t, domain.BeadStateClosed, sum.Beads[0].State)

	// P8-10: sc.log is under the D-23 run dir and carries the same lines
	// the terminal handler wrote to stderr.
	data, err := os.ReadFile(filepath.Join(dir, logging.RunLogName))
	require.NoError(t, err)
	require.Contains(t, string(data), `"run":"`+string(runLatestID(t, env.Repo.Dir))+`"`)
	for _, msg := range runSharedLogLines(t, string(data), stderr) {
		require.Contains(t, stderr, msg, "every run-log message must also reach stderr in headless mode")
	}
}

// runSharedLogLines returns a few messages the run log recorded, for the
// P8-10 assertion that the file and the terminal say the same things.
func runSharedLogLines(t *testing.T, runLog, stderr string) []string {
	t.Helper()
	var msgs []string
	for _, line := range strings.Split(runLog, "\n") {
		var rec struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil || rec.Msg == "" {
			continue
		}
		msgs = append(msgs, rec.Msg)
		if len(msgs) == 5 {
			break
		}
	}
	require.NotEmpty(t, msgs, "the run log recorded nothing")
	require.NotEmpty(t, stderr)
	return msgs
}

// runLatestID is the id of the most recent run recorded for repo.
func runLatestID(t *testing.T, repo string) domain.RunID {
	t.Helper()
	id, ok, err := config.LatestRunID(repo)
	require.NoError(t, err)
	require.True(t, ok, "the run left no run directory")
	return id
}

// TestRunJSONRendersRawEnvelopes is P8-12: --headless --json prints one
// JSON envelope per event and events.ReadAll parses its own output.
func TestRunJSONRendersRawEnvelopes(t *testing.T) {
	env := runFireSetup(t, false)

	stdout, stderr, code := runFireExec(t, "run", env.Repo.Dir, "-m", runFireBrief, "--headless", "--json")
	require.Equal(t, 0, code, stderr)

	evs, err := events.ReadAll(strings.NewReader(stdout))
	require.NoError(t, err, "the renderer's own output must parse")
	require.NotEmpty(t, evs)
	var finished bool
	for _, ev := range evs {
		if fin, ok := ev.(events.RunFinished); ok {
			finished = true
			require.Equal(t, events.FinishCompleted, fin.Reason)
		}
	}
	require.True(t, finished, "the stream must end with a RunFinished")
	require.NotContains(t, stdout, "\nrun        ", "the summary must not pollute the JSONL stream")
	require.Contains(t, stderr, "run        ")

	// Safety off merged the work to the default branch (D-10).
	require.FileExists(t, filepath.Join(env.Repo.Dir, runFireWork))
}

// TestRunWithoutATerminalRefusesTheTUI is P8-13: `sc run` without a TTY
// says what to do instead and exits 1, rather than failing inside Bubble
// Tea's input reader.
func TestRunWithoutATerminalRefusesTheTUI(t *testing.T) {
	_, repo := runTestEnv(t)
	runAddCartridge(t, "claude", "fable")
	runStubTerminal(t, false)

	stdout, stderr, code := runExec(t, repo)
	require.Equal(t, 1, code)
	require.Empty(t, stdout)
	require.Contains(t, stderr, "the TUI needs a terminal")
	require.Contains(t, stderr, "--"+FlagRunHeadless)
}

// runStubTerminal makes the terminal check answer yes or no without a real
// TTY, and puts the real check back afterwards.
func runStubTerminal(t *testing.T, tty bool) {
	t.Helper()
	saved := isTerminal
	t.Cleanup(func() { isTerminal = saved })
	isTerminal = func(*os.File) bool { return tty }
}

// TestRunPassesNoAnimIntoTheTUI is the second half of P8-13: --no-anim
// reaches tui.Options rather than staying cosmetic.
func TestRunPassesNoAnimIntoTheTUI(t *testing.T) {
	env := runFireSetup(t, true)
	runStubTerminal(t, true)

	var got tui.Options
	saved := runTUI
	t.Cleanup(func() { runTUI = saved })
	runTUI = func(ctx context.Context, opts tui.Options) error {
		got = opts
		<-ctx.Done() // the TUI lives until the run is over
		return nil
	}

	_, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief, "--no-anim")
	require.Equal(t, 0, code, stderr)
	require.True(t, got.NoAnim, "--no-anim must reach tui.Options")
	require.NotNil(t, got.Bus, "the TUI must watch the run's bus")
	require.NotNil(t, got.Controller, "a live run wires the P7-10 run controls")
	require.Equal(t, domain.RunModeBrief, got.Run.Mode)
}

// TestRunAddMockCartridgeStillFailsForUsers is the other half of P8-08: the
// mock is selectable in tests without loosening D-19 for users.
func TestRunAddMockCartridgeStillFailsForUsers(t *testing.T) {
	cartridgeTestEnv(t)
	_, err := cartridgeRun(t, "--add", "mock")
	require.Error(t, err)
	require.Contains(t, err.Error(), `cartridge "mock" is not supported yet`)
}

// TestRunHeadlessExitCodeComesFromTheRenderer pins the exit-code half of
// P8-01: the headless renderer maps the run's finish reason (D-15's
// contract, 0 completed or stopped, 1 failed, 3 credits exhausted) and
// `sc run` exits with it, with the final summary still printed.
func TestRunHeadlessExitCodeComesFromTheRenderer(t *testing.T) {
	env := runFireSetup(t, true)
	saved := runHeadless
	t.Cleanup(func() { runHeadless = saved })
	runHeadless = func(_ context.Context, opts tui.Options, hopts tui.HeadlessOptions) (int, error) {
		require.NotNil(t, opts.Bus)
		require.False(t, hopts.JSON)
		return tui.ExitCreditsExhausted, nil
	}

	_, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief, "--headless")
	require.Equal(t, tui.ExitCreditsExhausted, code)
	require.Contains(t, stderr, "run        ", "the summary is printed whatever the exit code")
}

// TestRunRefusesARepositoryAnotherRunHolds is P9-04's user-facing half: the
// preflight names the live run and points at `sc status`, and --force is
// the only way past it.
func TestRunRefusesARepositoryAnotherRunHolds(t *testing.T) {
	env := runFireSetup(t, false)
	// A lock held by a live process that is not this one: a child that
	// outlives the refusal.
	held := exec.Command("sleep", "60")
	require.NoError(t, held.Start())
	t.Cleanup(func() { _ = held.Process.Kill(); _, _ = held.Process.Wait() })

	lock := orchestrator.RepoLockPath(config.StateDir(env.Repo.Dir))
	require.NoError(t, os.MkdirAll(filepath.Dir(lock), 0o700))
	host, _ := os.Hostname()
	rec, err := json.Marshal(orchestrator.LockHolder{
		Run: "20260101-000000-aaaa", PID: held.Process.Pid, Host: host, TakenAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(lock, rec, 0o600))

	_, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief, "--headless", "--cartridge", "mock")
	require.Equal(t, 1, code, stderr)
	require.Contains(t, stderr, "20260101-000000-aaaa")
	require.Contains(t, stderr, "sc status")
	require.FileExists(t, lock, "a refused run leaves the other run's lock alone")

	// --force takes it: the run starts and finishes normally.
	_, stderr, code = runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock", "--force")
	require.Equal(t, 0, code, stderr)
}

// runFireSecret is a credential shaped like the ones the redaction rules
// know, planted in what an agent prints.
const runFireSecret = "sk-ant-api03-DEADBEEFDEADBEEFDEADBEEF"

// TestRunRedactsWhatAnAgentPrints is P9-05's events-and-logs clause driven
// through a real run: a cartridge that prints a credential must not leave
// it in the run log, the events file or the terminal. The scrub and the
// redaction happen once, where the run publishes, so every consumer of the
// stream is covered by the same line.
func TestRunRedactsWhatAnAgentPrints(t *testing.T) {
	env := runFireSetup(t, false)
	sc := runFireScenarioSaying("exporting ANTHROPIC_API_KEY=" + runFireSecret + " \x1b[31mnow\x1b[0m")
	require.NoError(t, sc.WriteFile(env.Scenario))

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief,
		"--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, stderr)
	require.NotContains(t, stdout, runFireSecret, "the terminal must not carry the credential")
	require.NotContains(t, stderr, runFireSecret)
	require.Contains(t, stdout, "[REDACTED]", "the line itself still reaches the reader")
	require.NotContains(t, stdout, "\x1b[31m", "and its escape sequences are gone")

	dir := config.RunDir(env.Repo.Dir, string(runLatestID(t, env.Repo.Dir)))
	for _, name := range []string{"events.jsonl", "sc.log"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err, name)
		require.NotContains(t, string(data), runFireSecret, "%s must not carry the credential", name)
	}
}

// TestRunPrintsTheFirstRunNotice is P8-06's second half wired into the run:
// the first run in a repository says where its logs, prompts, worktrees and
// config live, and a later run does not repeat it.
func TestRunPrintsTheFirstRunNotice(t *testing.T) {
	env := runFireSetup(t, false)

	_, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief, "--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stderr, "That was the first sc run in")
	require.Contains(t, stderr, config.StateDir(env.Repo.Dir))

	_, stderr, code = runFireExec(t, env.Repo.Dir, "-m", runFireBrief, "--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, stderr)
	require.NotContains(t, stderr, "That was the first sc run in",
		"only the first run explains where things live")
}
