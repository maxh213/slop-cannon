package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/config"
	"slop-cannon/internal/testutil"
)

// doctorEnv isolates the test from the machine: HOME and the XDG dirs point
// at temp directories, and PATH holds only the coreutils the FakeBin shims
// need, so a tool is "installed" only when the test creates a shim for it.
// The returned directory is the isolated PATH entry.
func doctorEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	for _, v := range []string{"NO_COLOR", "GH_TOKEN", "GITHUB_TOKEN", "BEADS_DIR"} {
		t.Setenv(v, "")
		require.NoError(t, os.Unsetenv(v))
	}

	tools := t.TempDir()
	for _, name := range []string{"cat", "tr", "wc", "mkdir", "rmdir", "sleep", "printf", "echo", "sh"} {
		real, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		require.NoError(t, os.Symlink(real, filepath.Join(tools, name)))
	}
	t.Setenv("PATH", tools)
	return tools
}

// doctorRepoDir creates a target directory, optionally a git repository
// (a .git directory is all FindRepoRoot needs) with a .beads directory.
func doctorRepoDir(t *testing.T, git, beads bool) string {
	t.Helper()
	dir := t.TempDir()
	if git {
		require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
		// A repository that ignores its dotenv files, which is what the
		// .gitignore row (P9-05) wants to see; doctorRepoIgnores drops it
		// for the test that covers the warning.
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n.env\n"), 0o600))
	}
	if beads {
		require.NoError(t, os.Mkdir(filepath.Join(dir, ".beads"), 0o755))
	}
	return dir
}

// doctorGitShim answers `git --version` with version and `git status
// --porcelain` with status (empty means clean).
func doctorGitShim(t *testing.T, version, status string) *testutil.Shim {
	t.Helper()
	script := fmt.Sprintf(`for a in "$@"; do
  case "$a" in
    --version) echo "git version %s"; exit 0 ;;
    status) printf '%%s' %s; exit 0 ;;
  esac
done
echo "fake git: unexpected arguments: $*" >&2
exit 2`, version, doctorShellQuote(status))
	return testutil.FakeBin(t, "git", script)
}

// doctorBDVersionShim is a bd that only knows `bd version --json` and reports
// version in the BD_JSON_ENVELOPE shape.
func doctorBDVersionShim(t *testing.T, version string) *testutil.Shim {
	t.Helper()
	script := fmt.Sprintf(`for a in "$@"; do
  case "$a" in
    version) printf '{"data":{"build":"dev","version":"%s"},"schema_version":1}\n'; exit 0 ;;
    ping) echo "Error: no beads database found" >&2; exit 1 ;;
  esac
done
exit 2`, version)
	return testutil.FakeBin(t, "bd", script)
}

// doctorGHShim answers `gh --version` and `gh auth status` the way gh
// 2.98 does, authenticated or not.
func doctorGHShim(t *testing.T, authenticated bool) *testutil.Shim {
	t.Helper()
	auth := `echo "You are not logged into any GitHub hosts. To log in, run: gh auth login"; exit 1`
	if authenticated {
		auth = `printf '%s\n' "github.com" "  ✓ Logged in to github.com account octocat (/home/x/.config/gh/hosts.yml)" "  - Active account: true"; exit 0`
	}
	script := fmt.Sprintf(`case "$1" in
  --version) printf '%%s\n' "gh version 2.98.0 (2026-08-21)" "https://github.com/cli/cli/releases/tag/v2.98.0"; exit 0 ;;
  auth) %s ;;
esac
exit 2`, auth)
	return testutil.FakeBin(t, "gh", script)
}

// doctorClaudeShim answers `claude --version` like Claude Code.
func doctorClaudeShim(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, "claude", `echo "2.1.258 (Claude Code)"`)
}

// doctorAddCartridge writes name into the isolated global config.
func doctorAddCartridge(t *testing.T, name, model string) {
	t.Helper()
	doc, err := config.OpenGlobal()
	require.NoError(t, err)
	_, err = doc.AddCartridge(name, model)
	require.NoError(t, err)
	require.NoError(t, doc.Save())
}

func doctorShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// doctorNormalise replaces temp-directory paths with stable placeholders so
// the output can be compared with a golden file.
func doctorNormalise(out string, repo string, shims ...*testutil.Shim) string {
	for _, s := range shims {
		out = strings.ReplaceAll(out, s.Path, "/fake/bin/"+s.Name)
	}
	if repo != "" {
		out = strings.ReplaceAll(out, repo, "/repo")
	}
	return out
}

// doctorRun executes sc doctor with args and returns stdout, stderr, and the
// exit code.
func doctorRun(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := ExecuteContext(context.Background(), append([]string{"doctor"}, args...), &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func TestDoctorHelpGolden(t *testing.T) {
	out, err := rootRun(t, "doctor", "--help")
	require.NoError(t, err)
	rootGolden(t, "doctor-help", out)
}

func TestDoctorRejectsArgs(t *testing.T) {
	_, err := rootRun(t, "doctor", "extra")
	require.Error(t, err)
}

func TestDoctorAllPass(t *testing.T) {
	doctorEnv(t)
	doctorAddCartridge(t, "claude", "fable")
	repo := doctorRepoDir(t, true, true)
	git := doctorGitShim(t, "2.55.0", "")
	bd := testutil.FakeBD(t, testutil.BDSamplesDir(t))
	gh := doctorGHShim(t, true)
	claude := doctorClaudeShim(t)

	stdout, stderr, code := doctorRun(t, "--repo", repo)
	require.Equal(t, 0, code, stdout+stderr)
	require.Empty(t, stderr)
	rootGolden(t, "doctor-pass", doctorNormalise(stdout, repo, git, bd, gh, claude))

	// bd was called the D-21 way: version first, then ping inside the repo.
	require.Equal(t, [][]string{
		{"version", "--json"},
		{"-C", repo, "ping", "--json"},
	}, bd.ArgLists(t))
	require.Empty(t, testutil.MissingSamples(t, bd))
	require.Equal(t, [][]string{{"--version"}, {"-C", repo, "status", "--porcelain"}}, git.ArgLists(t))
	require.Equal(t, [][]string{{"--version"}, {"auth", "status"}}, gh.ArgLists(t))
	require.Equal(t, [][]string{{"--version"}}, claude.ArgLists(t))
}

func TestDoctorAllPassJSON(t *testing.T) {
	doctorEnv(t)
	doctorAddCartridge(t, "claude", "fable")
	repo := doctorRepoDir(t, true, true)
	git := doctorGitShim(t, "2.55.0", "")
	bd := testutil.FakeBD(t, testutil.BDSamplesDir(t))
	gh := doctorGHShim(t, true)
	claude := doctorClaudeShim(t)

	stdout, stderr, code := doctorRun(t, "--json", "--repo", repo)
	require.Equal(t, 0, code, stdout+stderr)
	var report DoctorReport
	require.NoError(t, json.Unmarshal([]byte(stdout), &report))
	require.True(t, report.OK)
	require.Equal(t, repo, report.Repo)
	require.Equal(t, 8, report.Passed)
	require.Zero(t, report.Warnings)
	require.Zero(t, report.Failed)
	var names []string
	for _, c := range report.Checks {
		names = append(names, c.Name)
		require.Equal(t, DoctorPass, c.Status, c.Name)
		require.Empty(t, c.Hint, c.Name)
	}
	require.Equal(t, []string{"git", "bd", "beads", "gh", "cartridge:claude", "repo", ".gitignore", ".beads"}, names)
	rootGolden(t, "doctor-pass-json", doctorNormalise(stdout, repo, git, bd, gh, claude))
}

func TestDoctorNothingInstalled(t *testing.T) {
	doctorEnv(t)
	doctorAddCartridge(t, "claude", "fable")
	repo := doctorRepoDir(t, false, false)

	stdout, stderr, code := doctorRun(t, "--repo", repo)
	require.Equal(t, 1, code, "exit 1 on any fail")
	require.Empty(t, stderr, "the summary line is the whole story; nothing extra on stderr")
	stdout = doctorNormalise(stdout, repo)
	rootGolden(t, "doctor-nothing-installed", stdout)

	require.Contains(t, stdout, "FAIL  git")
	require.Contains(t, stdout, "FAIL  bd")
	require.Contains(t, stdout, "brew install beads")
	require.Contains(t, stdout, "CGO_ENABLED=0 go install builds are server-mode only")
	require.Contains(t, stdout, "warn  gh")
	require.Contains(t, stdout, "FAIL  cartridge:claude")
	require.Contains(t, stdout, "warn  repo")
	require.Contains(t, stdout, "warn  .beads")
	require.NotContains(t, stdout, "  beads  ", "bd ping is skipped when bd is unusable")
	require.NotContains(t, stdout, "dolt", "the standalone dolt binary is never checked")
	require.Contains(t, stdout, "\n0 passed, 3 warnings, 3 failed\n")
}

func TestDoctorTooOld(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, true)
	git := doctorGitShim(t, "2.29.2", "")
	bd := doctorBDVersionShim(t, "1.1.0")
	gh := doctorGHShim(t, false)

	stdout, _, code := doctorRun(t, "--repo", repo)
	require.Equal(t, 1, code)
	stdout = doctorNormalise(stdout, repo, git, bd, gh)
	rootGolden(t, "doctor-too-old", stdout)

	require.Contains(t, stdout, "FAIL  git               2.29.2 is older than 2.30.0")
	require.Contains(t, stdout, "FAIL  bd                1.1.0 is older than 1.2.2")
	require.Contains(t, stdout, "warn  gh                2.98.0 (/fake/bin/gh), not authenticated: You are not logged into any GitHub hosts")
	require.Contains(t, stdout, "hint: run: gh auth login")
	require.Contains(t, stdout, "warn  cartridge         no cartridge configured")
	require.Contains(t, stdout, "hint: add one with: sc cartridge --add claude --model <model>")
	require.Equal(t, [][]string{{"version", "--json"}}, bd.ArgLists(t), "no ping when bd is too old")
	// An old git still runs the clean-tree check.
	require.Contains(t, stdout, "pass  repo              clean working tree at /repo")
	require.Contains(t, stdout, "pass  .beads            /repo/.beads present")
}

func TestDoctorWarnings(t *testing.T) {
	doctorEnv(t)
	doctorAddCartridge(t, "claude", "fable")
	repo := doctorRepoDir(t, true, false)
	git := doctorGitShim(t, "2.30.0", " M main.go\n?? notes.txt\n")
	bd := testutil.FakeBD(t, testutil.BDSamplesDir(t))
	gh := doctorGHShim(t, false)
	claude := doctorClaudeShim(t)

	stdout, _, code := doctorRun(t, "--repo", repo)
	require.Equal(t, 0, code, "warnings do not fail doctor")
	stdout = doctorNormalise(stdout, repo, git, bd, gh, claude)
	rootGolden(t, "doctor-warnings", stdout)

	require.Contains(t, stdout, "warn  repo              2 uncommitted changes in /repo")
	require.Contains(t, stdout, "warn  .beads            not initialised: /repo/.beads is missing")
	require.Contains(t, stdout, "hint: sc will run bd init --non-interactive --init-if-missing --skip-agents --skip-hooks --setup-exclude on the first run (D-13)")
	require.Equal(t, [][]string{{"version", "--json"}}, bd.ArgLists(t), "no ping without .beads/")
	require.Contains(t, stdout, "\n4 passed, 3 warnings, 0 failed\n")
}

func TestDoctorPingFails(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, true)
	doctorGitShim(t, "2.55.0", "")
	doctorGHShim(t, true)
	bd := doctorBDVersionShim(t, "1.2.2") // its ping exits 1 with a message on stderr

	stdout, _, code := doctorRun(t, "--repo", repo)
	require.Equal(t, 1, code)
	require.Contains(t, stdout, "FAIL  beads             bd ping failed: exit status 1: Error: no beads database found")
	require.Contains(t, stdout, "hint: embedded Dolt did not answer")
	require.Equal(t, [][]string{{"version", "--json"}, {"-C", repo, "ping", "--json"}}, bd.ArgLists(t))
}

func TestDoctorBDEnvironment(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, true)
	bd := testutil.FakeBin(t, "bd", `printf '%s\n' "$BD_JSON_ENVELOPE" "$BD_NON_INTERACTIVE" > "$SC_FAKEBIN_DIR/env.$SC_FAKEBIN_SEQ"
case "$1" in
  version) echo '{"data":{"build":"dev","version":"1.2.2"},"schema_version":1}' ;;
  *) echo '{"data":{"status":"ok","total_ms":3},"schema_version":1}' ;;
esac`)

	report := RunDoctor(context.Background(), repo)
	require.Len(t, bd.ArgLists(t), 2)
	for _, seq := range []string{"1", "2"} {
		env, err := os.ReadFile(filepath.Join(bd.Dir, "env."+seq))
		require.NoError(t, err)
		require.Equal(t, "1\n1\n", string(env), "BD_JSON_ENVELOPE and BD_NON_INTERACTIVE must be set")
	}
	require.Equal(t, DoctorPass, report.Checks[1].Status)
	require.Equal(t, "beads", report.Checks[2].Name)
	require.Equal(t, DoctorPass, report.Checks[2].Status)
	require.Equal(t, "embedded database answered bd ping in 3 ms", report.Checks[2].Detail)
}

func TestDoctorBDToleratesPreambleAndFlatJSON(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, true)
	// A metrics notice on stdout before the JSON, and a ping without the
	// envelope, both decode.
	testutil.FakeBin(t, "bd", `case "$1" in
  version) printf '%s\n' "Thanks for using bd! Quick heads-up: bd shares anonymous usage metrics {see bd metrics}" '{"data":{"build":"dev","version":"1.2.2"},"schema_version":1}' ;;
  *) printf '%s\n' "Checked 1 gates: 0 resolved" '{"status":"ok","total_ms":5}' ;;
esac`)

	report := RunDoctor(context.Background(), repo)
	require.Equal(t, "bd", report.Checks[1].Name)
	require.Equal(t, DoctorPass, report.Checks[1].Status, report.Checks[1].Detail)
	require.True(t, strings.HasPrefix(report.Checks[1].Detail, "1.2.2 (build dev, "), report.Checks[1].Detail)
	require.Equal(t, "beads", report.Checks[2].Name)
	require.Equal(t, DoctorPass, report.Checks[2].Status, report.Checks[2].Detail)
	require.Equal(t, "embedded database answered bd ping in 5 ms", report.Checks[2].Detail)
}

func TestDoctorBDBadOutput(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, true)
	testutil.FakeBin(t, "bd", `case "$1" in
  version) echo "bd version 1.2.2" ;;
  *) echo '{"data":{"status":"degraded"},"schema_version":1}' ;;
esac`)
	report := RunDoctor(context.Background(), repo)
	require.Equal(t, DoctorFail, report.Checks[1].Status)
	require.Contains(t, report.Checks[1].Detail, "no JSON object in output")
	require.NotEqual(t, "beads", report.Checks[2].Name, "no ping after a failed version check")

	testutil.FakeBin(t, "bd", `case "$1" in
  version) echo '{"data":{"build":"dev","version":"1.2.2"},"schema_version":1}' ;;
  *) echo '{"data":{"status":"degraded"},"schema_version":1}' ;;
esac`)
	report = RunDoctor(context.Background(), repo)
	require.Equal(t, DoctorPass, report.Checks[1].Status)
	require.Equal(t, "beads", report.Checks[2].Name)
	require.Equal(t, DoctorFail, report.Checks[2].Status)
	require.Equal(t, `bd ping reported status "degraded"`, report.Checks[2].Detail)
}

func TestDoctorCartridges(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, true, false)
	doctorGitShim(t, "2.55.0", "")
	doctorBDVersionShim(t, "1.2.2")
	doctorGHShim(t, true)

	// No cartridge configured: a warning with the README hint.
	report := RunDoctor(context.Background(), repo)
	require.Equal(t, DoctorCheck{Name: "cartridge", Status: DoctorWarn, Detail: "no cartridge configured", Hint: doctorHintNoCart}, report.Checks[3])
	require.True(t, report.OK)

	// The active cartridge's CLI is missing: fail, exit 1.
	doctorAddCartridge(t, "claude", "fable")
	report = RunDoctor(context.Background(), repo)
	require.Equal(t, "cartridge:claude", report.Checks[3].Name)
	require.Equal(t, DoctorFail, report.Checks[3].Status)
	require.Equal(t, `active: "claude" not found on PATH`, report.Checks[3].Detail)
	require.Contains(t, report.Checks[3].Hint, "[cartridges.claude] command")
	require.False(t, report.OK)
	_, _, code := doctorRun(t, "--repo", repo)
	require.Equal(t, 1, code)

	// A custom command name is what gets looked up, and the version probe
	// is best effort: a CLI without --version still passes.
	doc, err := config.OpenGlobal()
	require.NoError(t, err)
	doc.File.Cartridges["claude"] = config.Cartridge{Model: "fable", Command: "claude-wrapper"}
	require.NoError(t, doc.Save())
	wrapper := testutil.FakeBin(t, "claude-wrapper", `echo "usage: claude-wrapper" >&2; exit 64`)
	report = RunDoctor(context.Background(), repo)
	require.Equal(t, DoctorCheck{Name: "cartridge:claude", Status: DoctorPass, Detail: "active: " + wrapper.Path}, report.Checks[3])

	// A configured but inactive cartridge that is missing only warns (D-19
	// allows one name for now, so "inactive" means no active cartridge).
	doc, err = config.OpenGlobal()
	require.NoError(t, err)
	doc.File.ActiveCartridge = nil
	doc.File.Cartridges["claude"] = config.Cartridge{Model: "fable"}
	require.NoError(t, doc.Save())
	report = RunDoctor(context.Background(), repo)
	require.Equal(t, DoctorCheck{Name: "cartridge", Status: DoctorWarn, Detail: "no active cartridge", Hint: doctorHintNoActive}, report.Checks[3])
	require.Equal(t, "cartridge:claude", report.Checks[4].Name)
	require.Equal(t, DoctorWarn, report.Checks[4].Status)
	require.Equal(t, `inactive: "claude" not found on PATH`, report.Checks[4].Detail)
	require.True(t, report.OK)

	doctorClaudeShim(t)
	report = RunDoctor(context.Background(), repo)
	require.Equal(t, DoctorPass, report.Checks[4].Status)
	require.True(t, strings.HasPrefix(report.Checks[4].Detail, "inactive: 2.1.258 ("), report.Checks[4].Detail)
}

func TestDoctorNoActiveCartridge(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, false, false)
	doc, err := config.OpenGlobal()
	require.NoError(t, err)
	doc.File.Cartridges = map[string]config.Cartridge{"claude": {Model: "fable"}}
	require.NoError(t, doc.Save())
	doctorClaudeShim(t)

	report := RunDoctor(context.Background(), repo)
	var names []string
	for _, c := range report.Checks {
		names = append(names, c.Name)
	}
	require.Equal(t, []string{"git", "bd", "gh", "cartridge", "cartridge:claude", "repo", ".beads"}, names)
	require.Equal(t, DoctorCheck{Name: "cartridge", Status: DoctorWarn, Detail: "no active cartridge", Hint: doctorHintNoActive}, report.Checks[3])
	require.Equal(t, DoctorPass, report.Checks[4].Status)
	require.True(t, strings.HasPrefix(report.Checks[4].Detail, "inactive: 2.1.258 ("))
}

func TestDoctorConfigError(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, false, false)
	path := config.GlobalPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("schema_version = 1\nammo = 2\n"), 0o600))

	report := RunDoctor(context.Background(), repo)
	require.Equal(t, "config", report.Checks[3].Name)
	require.Equal(t, DoctorFail, report.Checks[3].Status)
	require.Contains(t, report.Checks[3].Detail, "ammo must be at least 3")
	require.False(t, report.OK)
}

func TestDoctorRepoWithRealGit(t *testing.T) {
	if _, err := testutil.RealBinary("git"); err != nil {
		t.Skip("git not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	fixture := testutil.NewFixtureRepo(t)
	ctx := context.Background()

	gitCheck, ok := doctorGit(ctx)
	require.True(t, ok, gitCheck.Detail)
	require.Equal(t, DoctorPass, gitCheck.Status, gitCheck.Detail)

	// A nested directory resolves to the repository root.
	nested := filepath.Join(fixture.Dir, "stats")
	root, inRepo := config.FindRepoRoot(nested)
	require.True(t, inRepo)
	require.Equal(t, fixture.Dir, root)
	c := doctorRepo(ctx, nested, root, inRepo, true)
	require.Equal(t, DoctorCheck{Name: "repo", Status: DoctorPass, Detail: "clean working tree at " + fixture.Dir}, c)

	fixture.WriteFile(t, "dirty.txt", "x\n")
	c = doctorRepo(ctx, nested, root, inRepo, true)
	require.Equal(t, DoctorWarn, c.Status)
	require.Equal(t, "1 uncommitted change in "+fixture.Dir, c.Detail)
	require.Equal(t, doctorHintDirty, c.Hint)

	// Without git the tree cannot be inspected; say so instead of guessing.
	c = doctorRepo(ctx, nested, root, inRepo, false)
	require.Equal(t, DoctorWarn, c.Status)
	require.Contains(t, c.Detail, "clean-tree check skipped")

	plain := t.TempDir()
	c = doctorRepo(ctx, plain, plain, false, true)
	require.Equal(t, DoctorCheck{Name: "repo", Status: DoctorWarn, Detail: "not a git repository: " + plain, Hint: doctorHintRepo}, c)
}

func TestDoctorGHAuthFailureWithOutputOnStderr(t *testing.T) {
	doctorEnv(t)
	// Older gh printed auth status on stderr; the first line still shows up.
	testutil.FakeBin(t, "gh", `case "$1" in
  --version) echo "gh version 2.30.0 (2023-06-01)" ;;
  auth) echo "You are not logged into any GitHub hosts. Run gh auth login to authenticate." >&2; exit 1 ;;
esac`)
	c := doctorGH(context.Background())
	require.Equal(t, DoctorWarn, c.Status)
	require.Contains(t, c.Detail, "2.30.0")
	require.Contains(t, c.Detail, "not authenticated: You are not logged into any GitHub hosts.")
	require.Equal(t, doctorHintGHAuth, c.Hint)

	testutil.FakeBin(t, "gh", `case "$1" in
  --version) echo "gh version 2.30.0 (2023-06-01)" ;;
  auth) echo "  ✓ Logged in to github.com as octocat (oauth_token)" >&2; exit 0 ;;
esac`)
	c = doctorGH(context.Background())
	require.Equal(t, DoctorPass, c.Status)
	require.True(t, strings.HasSuffix(c.Detail, ", authenticated"), c.Detail)
}

func TestDoctorReportCounts(t *testing.T) {
	t.Parallel()
	r := &DoctorReport{OK: true}
	r.add(DoctorCheck{Status: DoctorPass}, DoctorCheck{Status: DoctorWarn}, DoctorCheck{Status: DoctorWarn})
	require.True(t, r.OK)
	require.Equal(t, [3]int{1, 2, 0}, [3]int{r.Passed, r.Warnings, r.Failed})
	r.add(DoctorCheck{Status: DoctorFail})
	require.False(t, r.OK)
	require.Equal(t, 1, r.Failed)

	var out bytes.Buffer
	require.NoError(t, writeDoctorReport(&out, &DoctorReport{
		Checks: []DoctorCheck{
			{Name: "git", Status: DoctorPass, Detail: "fine"},
			{Name: ".beads", Status: DoctorFail, Detail: "bad", Hint: "fix it"},
		},
		Passed: 1, Failed: 1,
	}))
	require.Equal(t, "pass  git               fine\nFAIL  .beads            bad\n                        hint: fix it\n\n1 passed, 0 warnings, 1 failed\n", out.String())
}

// TestDoctorWarnsWhenTheRepoDoesNotIgnoreDotenv is P9-05's `sc doctor`
// clause. Keeping a .env out of an agent's prompt is only half the job: an
// engineer that commits one has leaked the credential whatever the prompt
// said, so the row asks the repository to ignore it.
func TestDoctorWarnsWhenTheRepoDoesNotIgnoreDotenv(t *testing.T) {
	doctorEnv(t)
	doctorAddCartridge(t, "claude", "fable")
	repo := doctorRepoDir(t, true, true)
	require.NoError(t, os.Remove(filepath.Join(repo, ".gitignore")))
	doctorGitShim(t, "2.55.0", "")
	testutil.FakeBD(t, testutil.BDSamplesDir(t))
	doctorGHShim(t, true)
	doctorClaudeShim(t)

	report := RunDoctor(context.Background(), repo)
	check := report.Check(".gitignore")
	require.NotNil(t, check)
	require.Equal(t, DoctorWarn, check.Status)
	require.Equal(t, "does not ignore .env", check.Detail)
	require.Equal(t, agents.GitignoreHint, check.Hint)
	require.True(t, report.OK, "a missing .gitignore warns, it never fails the run")

	// A .gitignore that un-ignores the file it ignored is not cover.
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".env\n!.env\n"), 0o600))
	require.Equal(t, DoctorWarn, RunDoctor(context.Background(), repo).Check(".gitignore").Status)

	require.NoError(t, os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".env*\n"), 0o600))
	require.Equal(t, DoctorPass, RunDoctor(context.Background(), repo).Check(".gitignore").Status)
}

// TestDoctorSkipsTheGitignoreRowOutsideARepository keeps the row honest:
// there is nothing to ignore where there is no repository.
func TestDoctorSkipsTheGitignoreRowOutsideARepository(t *testing.T) {
	doctorEnv(t)
	repo := doctorRepoDir(t, false, false)
	report := RunDoctor(context.Background(), repo)
	require.Nil(t, report.Check(".gitignore"))
}
