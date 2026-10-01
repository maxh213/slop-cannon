package agents

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/testutil"
)

// P9-22: the engineer runs read_write, which is Bash with no deny list, in a
// linked worktree of the user's own repository. These tests are the
// enforcement the task asks for: the guard on the firing's PATH, judged both
// as a script and through a real firing.

// ggInstall installs a guard confined to wt (empty for none) and returns
// it. The real git is resolved with testutil.RealBinary rather than through
// RealGit, so a shim another test in this package has on PATH cannot end up
// behind the guard.
func ggInstall(t *testing.T, wt string) GitGuard {
	t.Helper()
	real, err := testutil.RealBinary("git")
	require.NoError(t, err)
	g, err := WriteGitGuard(t.TempDir(), GitGuardOptions{Real: real, Worktree: wt})
	require.NoError(t, err)
	return g
}

// ggRun runs the guard from dir with args and returns stdout+stderr and the
// exit code.
func ggRun(t *testing.T, g GitGuard, dir string, args ...string) (string, int) {
	t.Helper()
	return ggExec(t, g, dir, nil, args...)
}

// ggExec runs the guard and reports what it printed and what it exited
// with. The first execution of a freshly written script can come back
// ETXTBSY — another goroutine in this process still holds a write
// descriptor on it while fork/exec runs — so it goes through the same
// bounded retry the bd shim uses (P8-02, P3-20). Nothing else is retried:
// a real refusal is an *exec.ExitError, not a PathError.
func ggExec(t *testing.T, g GitGuard, dir string, env []string, args ...string) (string, int) {
	t.Helper()
	var out []byte
	var ee *exec.ExitError
	err := beads.RetryTextBusy(t.Context(), func() error {
		cmd := exec.Command(g.Path, args...) //nolint:gosec // the guard is written by the test
		cmd.Dir = dir
		cmd.Env = env
		var runErr error
		out, runErr = cmd.CombinedOutput()
		if errors.As(runErr, &ee) {
			return nil // the guard ran and refused: that is an answer
		}
		return runErr
	})
	require.NoError(t, err, "running the guard: %s", out)
	if ee != nil {
		return string(out), ee.ExitCode()
	}
	return string(out), 0
}

func TestGitGuardRefusesTheVerbsThatCanReachAnotherBranch(t *testing.T) {
	t.Parallel()
	g := ggInstall(t, "")
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		args []string
		word string
	}{
		{"push", []string{"push", "origin", "HEAD:refs/heads/main"}, "push"},
		{"force push", []string{"push", "--force", "origin", "main"}, "push"},
		{"pull", []string{"pull"}, "pull"},
		{"fetch a branch into a branch", []string{"fetch", "origin", "main:main"}, "fetch"},
		{"checkout", []string{"checkout", "main"}, "checkout"},
		{"switch", []string{"switch", "main"}, "switch"},
		{"clean", []string{"clean", "-fdx"}, "clean"},
		{"worktree", []string{"worktree", "add", "/tmp/elsewhere", "main"}, "worktree"},
		{"remote", []string{"remote", "set-url", "origin", "http://elsewhere"}, "remote"},
		{"update-ref", []string{"update-ref", "refs/heads/main", "HEAD"}, "update-ref"},
		{"symbolic-ref", []string{"symbolic-ref", "HEAD", "refs/heads/main"}, "symbolic-ref"},
		{"tag", []string{"tag", "-f", "v1"}, "tag"},
		{"config", []string{"config", "core.hooksPath", "/tmp/hooks"}, "config"},
		{"gc", []string{"gc", "--prune=now"}, "gc"},
		{"init", []string{"init"}, "init"},
		{"a verb git has not grown yet", []string{"teleport", "main"}, "teleport"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := ggRun(t, g, dir, tc.args...)
			assert.Equal(t, GitGuardExitCode, code)
			assert.Contains(t, out, GitGuardDeniedMessage(tc.word))
			assert.Contains(t, out, GitGuardHint)
		})
	}
}

func TestGitGuardRefusesTheOptionsThatRedirectGit(t *testing.T) {
	t.Parallel()
	g := ggInstall(t, "")
	dir := t.TempDir()
	for _, args := range [][]string{
		{"-C", "/elsewhere", "status"},
		{"--git-dir=/elsewhere/.git", "log"},
		{"--work-tree", "/elsewhere", "status"},
		{"-c", "core.hooksPath=/tmp/hooks", "commit", "-m", "x"},
		{"--namespace", "other", "log"},
		{"status", "--git-dir=/elsewhere/.git"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out, code := ggRun(t, g, dir, args...)
			assert.Equal(t, GitGuardExitCode, code)
			assert.Contains(t, out, "is not allowed from an agent session")
			assert.Contains(t, out, GitGuardHint)
		})
	}
}

func TestGitGuardAllowsTheWorkTheRolePromptsAskFor(t *testing.T) {
	t.Parallel()
	repo := testutil.NewFixtureRepo(t)
	g := ggInstall(t, "")
	for _, args := range [][]string{
		{"status", "--porcelain"},
		{"log", "--oneline", "-n", "1"},
		{"diff", "--stat"},
		{"branch", "--show-current"},
		{"rev-parse", "HEAD"},
		{"--version"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out, code := ggRunIn(t, g, repo, args...)
			assert.Equal(t, 0, code, out)
			assert.NotContains(t, out, "is not allowed from an agent session")
		})
	}
}

// ggRunIn runs the guard inside the fixture repository, with the fixture's
// own isolated git identity, so the allowed calls reach real git.
func ggRunIn(t *testing.T, g GitGuard, repo *testutil.FixtureRepo, args ...string) (string, int) {
	t.Helper()
	return ggExec(t, g, repo.Dir, append(repo.Env(), "PATH="+os.Getenv("PATH")), args...)
}

func TestGitGuardAllowsBranchToListButNotToWrite(t *testing.T) {
	t.Parallel()
	repo := testutil.NewFixtureRepo(t)
	g := ggInstall(t, "")

	out, code := ggRunIn(t, g, repo, "branch", "--list")
	require.Equal(t, 0, code, out)
	require.Contains(t, out, "main")

	for _, args := range [][]string{
		{"branch", "-D", "main"},
		{"branch", "-f", "main", "HEAD~0"},
		{"branch", "-M", "main", "trunk"},
		{"branch", "--set-upstream-to", "origin/main"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out, code := ggRunIn(t, g, repo, args...)
			assert.Equal(t, GitGuardExitCode, code)
			assert.Contains(t, out, "is not allowed from an agent session")
		})
	}
	assert.Equal(t, "main", strings.TrimSpace(repo.Git(t, "rev-parse", "--abbrev-ref", "HEAD")))
}

func TestGitGuardRefusesGitRunFromOutsideTheWorktree(t *testing.T) {
	t.Parallel()
	wt := t.TempDir()
	inside := filepath.Join(wt, "pkg")
	require.NoError(t, os.MkdirAll(inside, 0o755))
	g := ggInstall(t, wt)

	out, _ := ggRun(t, g, inside, "status")
	assert.NotContains(t, out, "may only be run inside",
		"a subdirectory of the worktree is inside it")

	out, code := ggRun(t, g, t.TempDir(), "status")
	assert.Equal(t, GitGuardExitCode, code)
	assert.Contains(t, out, GitGuardOutsideMessage(g.Worktree))
}

func TestGitGuardApplyPutsItselfFirstOnPathExactlyOnce(t *testing.T) {
	t.Parallel()
	g := ggInstall(t, "")
	env := map[string]string{"PATH": "/usr/bin:/bin"}
	g.Apply(env)
	g.Apply(env)
	assert.Equal(t, g.Dir+string(os.PathListSeparator)+"/usr/bin:/bin", env["PATH"])
	g.Apply(nil) // must not panic
	assert.Empty(t, GitGuard{}.Dir)
	empty := map[string]string{}
	GitGuard{}.Apply(empty)
	assert.Empty(t, empty)
}

func TestGitGuardIsRecognisedAndNeverChained(t *testing.T) {
	t.Parallel()
	g := ggInstall(t, "")
	assert.True(t, IsGitGuard(g.Path))
	assert.False(t, IsGitGuard(g.Real))
	assert.False(t, IsGitGuard(filepath.Join(t.TempDir(), "nothing")))

	_, err := WriteGitGuard(t.TempDir(), GitGuardOptions{Real: g.Path})
	require.ErrorContains(t, err, "refusing to chain guards")

	real, err := RealGit()
	require.NoError(t, err)
	assert.False(t, IsGitGuard(real))
}

func TestInstallGitGuardForWorktreeUsesTheShimDirectory(t *testing.T) {
	t.Parallel()
	run := t.TempDir()
	wt := filepath.Join(run, "wt", "sc-1")
	require.NoError(t, os.MkdirAll(wt, 0o755))

	g, err := InstallGitGuardForWorktree(wt, "")
	require.NoError(t, err)
	shimDir, err := beads.ShimDirFor(wt)
	require.NoError(t, err)
	assert.Equal(t, shimDir, g.Dir, "the guard shares the per-agent PATH entry the bd shim already has")
	assert.Equal(t, filepath.Join(shimDir, "git"), g.Path)
	assert.Equal(t, testutil.RealPath(t, wt), g.Worktree, "a firing's git is always confined to its own worktree")

	_, err = InstallGitGuardForWorktree("", "")
	require.ErrorContains(t, err, "empty worktree path")
	_, err = InstallGitGuardForWorktree("relative/wt", "")
	require.ErrorContains(t, err, "is not absolute")
}

// ggAgent is the engineer the firing below runs as.
func ggAgent() domain.Agent {
	return domain.Agent{
		ID: domain.AgentID("eng-sc-1a2b-r1"), Role: domain.RoleEngineer,
		Cartridge: mock.Name, Bead: domain.BeadID("sc-1a2b"), State: domain.AgentStateRunning,
	}
}

// ggEnvMap turns a KEY=VALUE environment into a map for a RunSpec.
func ggEnvMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			out[k] = v
		}
	}
	return out
}

// TestAFiringCannotMoveTheDefaultBranch is P9-22's acceptance test. A real
// firing — the mock cartridge under the engineer's read_write policy, in a
// linked worktree of a repository with a bare origin — runs the three
// commands the task names plus three more ways out of the worktree, and
// neither the local nor the remote default branch moves. The work the
// engineer is supposed to do still lands on its own branch.
func TestAFiringCannotMoveTheDefaultBranch(t *testing.T) {
	t.Parallel()
	repo := testutil.NewFixtureRepo(t, testutil.WithBareRemote())
	run := t.TempDir()
	wt := filepath.Join(run, "wt", "sc-1a2b")
	repo.Git(t, "worktree", "add", "-q", "-b", "sc/sc-1a2b", wt, "main")

	realGit, err := testutil.RealBinary("git")
	require.NoError(t, err)
	guard, err := WriteGitGuard(filepath.Join(run, "bin"), GitGuardOptions{Real: realGit, Worktree: wt})
	require.NoError(t, err)

	mainBefore := strings.TrimSpace(repo.Git(t, "rev-parse", "main"))
	remoteBefore := strings.TrimSpace(repo.Git(t, "ls-remote", "origin", "refs/heads/main"))
	require.NotEmpty(t, remoteBefore)

	sh := func(line string) []string { return []string{"/bin/sh", "-c", line} }
	attempts := []string{
		"git push origin HEAD:refs/heads/main",
		"git checkout main",
		"git clean -fdx",
		"git branch -f main HEAD",
		"git -C " + repo.Dir + " commit --allow-empty -m pwned",
		"cd " + repo.Dir + " && git commit --allow-empty -m pwned",
	}

	s := mock.NewScenario("git-guard")
	rule := s.Rule("implement", mock.Match{Role: "engineer"}).Say("Working.\n")
	for _, line := range attempts {
		args := sh(line)
		rule = rule.Exec(args[0], args[1:]...)
	}
	rule = rule.Exec("/bin/sh", "-c", "echo work >> NOTES.md && git add -A && git commit -q -m 'the work'")
	rule.Structured(json.RawMessage(`{"summary":"Added a note, as the bead asked."}`))

	adapter, err := mock.NewAdapter(mock.Name, s)
	require.NoError(t, err)

	env := ggEnvMap(repo.Env())
	guard.Apply(env)
	spec := cartridge.RunSpec{
		Prompt:  "Implement the bead.",
		Workdir: wt,
		Env:     env,
		Timeout: time.Minute,
		Labels:  cartridge.LabelsFor("run-1", ggAgent(), events.TaskImplement, 1),
	}
	require.NoError(t, ApplyPolicy(&spec, domain.RoleEngineer, events.TaskImplement))
	require.Equal(t, cartridge.ToolReadWrite, spec.ToolPolicy, "the engineer really does run read_write")

	_, tr, err := RunAgent[domain.ImplReport](context.Background(), AgentRun{
		Cartridge: adapter,
		Spec:      spec,
		Message:   domain.MessageImplReport,
		Agent:     ggAgent(),
		Task:      events.TaskImplement,
		Run:       domain.RunID("run-1"),
	})
	require.NoError(t, err)

	text := tr.Text()
	for _, line := range attempts {
		assert.Contains(t, text, "is not allowed from an agent session",
			"the guard said nothing about %q", line)
	}
	assert.Contains(t, text, GitGuardDeniedMessage("push"))
	assert.Contains(t, text, GitGuardDeniedMessage("checkout"))
	assert.Contains(t, text, GitGuardDeniedMessage("clean"))
	assert.Contains(t, text, GitGuardOutsideMessage(testutil.RealPath(t, wt)))

	assert.Equal(t, mainBefore, strings.TrimSpace(repo.Git(t, "rev-parse", "main")),
		"the local default branch moved")
	assert.Equal(t, remoteBefore, strings.TrimSpace(repo.Git(t, "ls-remote", "origin", "refs/heads/main")),
		"the remote default branch moved")
	assert.Equal(t, "main", strings.TrimSpace(repo.Git(t, "rev-parse", "--abbrev-ref", "HEAD")),
		"the repository's own working tree was moved off the default branch")

	// The engineer's own work still landed, on its own branch.
	assert.NotEqual(t, mainBefore, strings.TrimSpace(repo.Git(t, "rev-parse", "sc/sc-1a2b")),
		"the guard blocked the commit the engineer is meant to make")
	assert.Contains(t, repo.Git(t, "log", "-1", "--format=%s", "sc/sc-1a2b"), "the work")
}
