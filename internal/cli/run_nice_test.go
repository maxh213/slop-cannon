package cli

import (
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
)

// A fired run lowers its own priority to the configured niceness before
// anything is started, so every agent and test browser inherits it.
func TestRunRenicesItselfBeforeFiring(t *testing.T) {
	env := runFireSetup(t, false)
	configWriteFile(t, config.RepoConfigPath(env.Repo.Dir), "nice = 13\n")

	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief, "--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
	reniced.Lock()
	defer reniced.Unlock()
	assert.True(t, slices.Contains(reniced.values, 13), "renice calls: %v", reniced.values)
}

// A priority that cannot be lowered is worth a note, not a failed run.
func TestRunFiresEvenWhenItCannotRenice(t *testing.T) {
	saved := runRenice
	t.Cleanup(func() { runRenice = saved })
	runRenice = func(int) error { return errors.New("operation not permitted") }

	env := runFireSetup(t, false)
	stdout, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief, "--headless", "--cartridge", "mock")
	require.Equal(t, 0, code, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
	assert.Contains(t, stderr, "note: could not lower the run's priority to nice 10: operation not permitted")
}

// --dry-run says what the run would do and changes nothing.
func TestRunDryRunShowsNiceAndDoesNotRenice(t *testing.T) {
	_, repo := resumeSetup(t)
	reniced.Lock()
	before := len(reniced.values)
	reniced.Unlock()
	stdout, stderr, code := runResolve(t, repo, "-m", "b")
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "nice       10: the run and every process it starts run at a lower priority (from default)")
	reniced.Lock()
	defer reniced.Unlock()
	assert.Len(t, reniced.values, before)
}

func TestNiceMustBeANiceness(t *testing.T) {
	_, repo := resumeSetup(t)
	for _, bad := range []string{"-1", "20"} {
		configWriteFile(t, config.RepoConfigPath(repo), "nice = "+bad+"\n")
		_, stderr, code := runResolve(t, repo, "-m", "b")
		require.Equal(t, 1, code)
		assert.Contains(t, stderr, "nice: must be between 0 (leave the priority alone) and 19 (the lowest)")
	}
	configWriteFile(t, config.RepoConfigPath(repo), "nice = 0\n")
	stdout, stderr, code := runResolve(t, repo, "-m", "b")
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "nice       0: the run's priority is left alone (from repo)")
}
