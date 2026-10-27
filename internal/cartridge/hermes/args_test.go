package hermes

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
)

func hermesAdapter(t testing.TB, cfg config.Cartridge) *Adapter {
	t.Helper()
	c, err := New(Name, cfg)
	require.NoError(t, err)
	a, ok := c.(*Adapter)
	require.True(t, ok)
	return a
}

func TestArgsFullSpec(t *testing.T) {
	t.Parallel()
	a := hermesAdapter(t, config.Cartridge{Model: "cfg-model", Args: []string{"--reasoning", "high"}})
	spec := cartridge.RunSpec{
		Prompt: "do it", SystemPrompt: "be terse", Workdir: "/work",
		Model: "routed-model", ToolPolicy: cartridge.ToolReadExec,
		Web: true, MaxTurns: 7, Timeout: 2500 * time.Millisecond,
	}
	assert.Equal(t, []string{
		"chat", "--query-file", "-", "--oneshot", "--format", "stream-json",
		"--ignore-rules", "--source", "tool", "--in", "/work", "--provider", "nous",
		"--model", "routed-model", "--max-turns", "7",
		"--toolsets", "terminal,web", "--reasoning", "high",
	}, a.Args(spec))
	assert.Equal(t, "be terse"+SystemPromptSeparator+"do it", Stdin(spec))
	assert.NotContains(t, a.Args(spec), spec.Prompt)
}

func TestArgsDefaultsAndPolicies(t *testing.T) {
	t.Parallel()
	a := hermesAdapter(t, config.Cartridge{Model: "cfg-model"})
	args := a.Args(cartridge.RunSpec{Workdir: "/w", ToolPolicy: cartridge.ToolReadWrite})
	assert.Contains(t, args, "cfg-model")
	assert.Contains(t, args, "terminal,file")
	assert.NotContains(t, args, "--max-turns")
	assert.NotContains(t, args, "--run-budget")
	assert.NotContains(t, args, "terminal,file,web")

	spec := cartridge.RunSpec{Workdir: "/w", Model: "spec-model", ToolPolicy: cartridge.ToolReadWrite}
	assert.Contains(t, a.Args(spec), "spec-model")
	assert.Equal(t, "prompt", Stdin(cartridge.RunSpec{Prompt: "prompt", SystemPrompt: "  "}))
	assert.Nil(t, ToolFlags(cartridge.ToolReadOnly))
}

func TestToolPolicyEnforcement(t *testing.T) {
	t.Parallel()
	a := hermesAdapter(t, config.Cartridge{})
	for _, policy := range []cartridge.ToolPolicy{cartridge.ToolReadExec, cartridge.ToolReadWrite} {
		enforcement, err := a.ToolPolicyEnforcement(policy)
		require.NoError(t, err)
		assert.Equal(t, agents.EnforceNative, enforcement.Mode)
		assert.Equal(t, ToolFlags(policy), enforcement.Controls)
		assert.NoError(t, enforcement.Check())
		via, err := agents.EnforcementFor(a, policy)
		require.NoError(t, err)
		assert.Equal(t, enforcement, via)
	}
	_, err := a.ToolPolicyEnforcement(cartridge.ToolReadOnly)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot enforce read_only")
	_, err = agents.EnforcementFor(a, cartridge.ToolReadOnly)
	require.Error(t, err)
	_, err = a.ToolPolicyEnforcement("everything")
	assert.ErrorIs(t, err, domain.ErrUnknownValue)
}

func TestInheritEnvDoesNotInheritHermesState(t *testing.T) {
	t.Parallel()
	got := InheritEnv([]string{"HOME"})
	for _, name := range []string{"NOUS_API_KEY", "NOUS_INFERENCE_BASE_URL", "HERMES_PORTAL_BASE_URL"} {
		assert.Contains(t, got, name)
	}
	for _, name := range []string{"HERMES_HOME", "HERMES_PROFILE", "HERMES_PROFILE_NAME", "HERMES_SESSION_SOURCE"} {
		assert.NotContains(t, got, name)
	}
}

func TestSharedAuthDirPrecedenceAndProfileRoot(t *testing.T) {
	t.Setenv("HERMES_SHARED_AUTH_DIR", "/parent/shared")
	t.Setenv("HERMES_HOME", "/parent/.hermes/profiles/work")
	assert.Equal(t, "/explicit/shared", sharedAuthDir(
		map[string]string{"HERMES_SHARED_AUTH_DIR": "/cfg/shared"},
		map[string]string{"HERMES_SHARED_AUTH_DIR": "/explicit/shared"},
	))
	assert.Equal(t, "/cfg/shared", sharedAuthDir(map[string]string{"HERMES_SHARED_AUTH_DIR": "/cfg/shared"}, nil))
	assert.Equal(t, "/parent/shared", sharedAuthDir(nil, nil))

	t.Setenv("HERMES_SHARED_AUTH_DIR", "")
	assert.Equal(t, "/parent/.hermes/shared", sharedAuthDir(nil, nil))
	t.Setenv("HERMES_HOME", "")
	t.Setenv("HOME", "/home/example")
	assert.Equal(t, "/home/example/.hermes/shared", sharedAuthDir(nil, nil))
}

func TestSessionHomeLifecycle(t *testing.T) {
	t.Setenv("HERMES_HOME", filepath.Join(t.TempDir(), "root"))
	home, err := newSessionHome(nil, nil, 0)
	require.NoError(t, err)
	assert.NotEqual(t, home.home, home.hermes)
	config, err := os.ReadFile(filepath.Join(home.hermes, "config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "model:\n  provider: nous\n", string(config))
	for _, dir := range []string{home.root, home.hermes, home.home} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.True(t, info.IsDir())
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	}
	require.NoError(t, os.WriteFile(filepath.Join(home.hermes, "state.db"), []byte("state"), 0o600))
	home.cleanup()
	_, err = os.Stat(home.root)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestSessionHomeGatewayBudget(t *testing.T) {
	t.Setenv("HERMES_HOME", filepath.Join(t.TempDir(), "root"))
	home, err := newSessionHome(nil, nil, 2500*time.Millisecond)
	require.NoError(t, err)
	t.Cleanup(home.cleanup)
	config, err := os.ReadFile(filepath.Join(home.hermes, "config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "model:\n  provider: nous\nagent:\n  run_budget_seconds: 2.5\n", string(config))
}

func TestRegisteredAndBinary(t *testing.T) {
	t.Parallel()
	assert.True(t, cartridge.Default.Has(Name))
	a := hermesAdapter(t, config.Cartridge{})
	assert.Equal(t, Name, a.Name())
	assert.Equal(t, Name, a.Binary())
	custom := hermesAdapter(t, config.Cartridge{Command: "/opt/hermes-custom"})
	assert.Equal(t, "/opt/hermes-custom", custom.Binary())
}
