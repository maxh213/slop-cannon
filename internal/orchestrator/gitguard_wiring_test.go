package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/domain"
)

// TestSupervisorInstallsTheGitGuard is the wiring half of P9-22. The guard
// itself is proven in internal/agents (a real firing whose agent tries to
// push, check out and clean the default branch); what this asserts is that
// a firing the supervisor prepares actually gets it — in the same directory
// the read-only bd shim uses, which is already first on the agent's PATH,
// and confined to that firing's own worktree.
func TestSupervisorInstallsTheGitGuard(t *testing.T) {
	f := supSetup(t, map[string]*mock.Scenario{supAlpha: supApproval("alpha")})
	sup := f.supervisor(t, nil)
	wt := f.worktree(t, "bd-22")

	_, _, err := FireAgent[domain.Verdict](context.Background(), sup, supTesterFiring("tester-qa-bd-22-r1", "bd-22", wt))
	require.NoError(t, err)

	shim, ok := sup.Shim(wt)
	require.True(t, ok)
	guard := filepath.Join(shim.Dir, "git")
	require.FileExists(t, guard, "every firing with a worktree gets a git guard beside its bd shim (P9-22)")
	assert.True(t, agents.IsGitGuard(guard), "%s is the generated guard, not something else called git", guard)

	call, ok := f.Adapters[supAlpha].LastCall()
	require.True(t, ok)
	path := call.Spec.Env["PATH"]
	require.NotEmpty(t, path)
	assert.Equal(t, shim.Dir, strings.SplitN(path, string(os.PathListSeparator), 2)[0],
		"so the guard is the first git on the agent's PATH, as the bd shim is the first bd")

	// The damaging verbs are refused from inside the worktree...
	cmd := exec.Command(guard, "push", "origin", "HEAD:refs/heads/main") //nolint:gosec // the guard this test just installed
	cmd.Dir = wt
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "a push through the guard must fail: %s", out)
	assert.Contains(t, string(out), agents.GitGuardDeniedMessage("push"))

	// ...and every git is refused from outside it, so an agent cannot step
	// into the repository's own working tree and commit on the branch that
	// is checked out there.
	cmd = exec.Command(guard, "status") //nolint:gosec // the same guard
	cmd.Dir = f.Repo
	out, err = cmd.CombinedOutput()
	require.Error(t, err, "git outside the worktree must fail: %s", out)
	assert.Contains(t, string(out), "may only be run inside")

	// And the run's cleanup path takes the guard with the shim directory.
	require.NoError(t, sup.RemoveShim("bd-22"))
	assert.NoFileExists(t, guard)
}
