package junie

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
)

func junieAdapter(t testing.TB, cfg config.Cartridge) *Adapter {
	t.Helper()
	c, err := New(Name, cfg)
	require.NoError(t, err)
	a, ok := c.(*Adapter)
	require.True(t, ok)
	return a
}

func TestNewAndRegistered(t *testing.T) {
	t.Parallel()
	a := junieAdapter(t, config.Cartridge{})
	assert.Equal(t, Name, a.Name())
	assert.Equal(t, DefaultBinary, a.Binary())
	assert.True(t, cartridge.Default.Has(Name))

	c, err := New("", config.Cartridge{Command: "/opt/junie"})
	require.NoError(t, err)
	assert.Equal(t, Name, c.Name())
	custom, ok := c.(*Adapter)
	require.True(t, ok)
	assert.Equal(t, "/opt/junie", custom.Binary())
}

func TestNewRejectsUnsupportedEffort(t *testing.T) {
	t.Parallel()
	for _, effort := range []string{"xhigh", "max", "extreme", ""} {
		_, err := New(Name, config.Cartridge{Args: []string{"--effort", effort}})
		require.Error(t, err, effort)
		assert.Contains(t, err.Error(), "effort")
	}
	for _, effort := range Efforts {
		_, err := New(Name, config.Cartridge{Args: []string{"--effort=" + effort}})
		require.NoError(t, err, effort)
	}
}

func TestArgsFullSpec(t *testing.T) {
	t.Parallel()
	a := junieAdapter(t, config.Cartridge{
		Model: "config-model",
		Args:  []string{"--effort", "high", "--provider", "anthropic"},
	})
	spec := cartridge.RunSpec{
		Prompt: "do the task", SystemPrompt: "follow the rules",
		Workdir: "/work", Model: "routed-model", ToolPolicy: cartridge.ToolReadWrite, MaxTurns: 9,
	}
	got := a.Args(spec, "/empty-extensions")
	assert.Equal(t, []string{
		"--project", "/work", "--output-format", "json", "--skip-update-check",
		"--share-anonymous-statistics", "false",
		"--config-default-locations", "false", "--mcp-default-locations", "false",
		"--model-default-locations", "false", "--skill-default-locations", "false",
		"--command-default-location", "false", "--agent-default-location", "false",
		"--extensions-default-location", "/empty-extensions",
		"--model", "routed-model", "--system-prompt", "follow the rules",
		"--effort", "high", "--provider", "anthropic", "--task", "do the task",
	}, got)
	assert.NotContains(t, got, "--max-steps", "26.9.22 has no MaxTurns equivalent")
	assert.NotContains(t, got, "--plan")
	assert.NotContains(t, got, "--gateway-stop")
}

func TestArgsConfiguredArgsCannotOverrideControls(t *testing.T) {
	t.Parallel()
	a := junieAdapter(t, config.Cartridge{Args: []string{
		"--output-format", "json-stream", "--project=/elsewhere", "--model", "other",
		"--task", "other task", "--plan", "--gateway-stop", "--gateway-status", "--goal", "--verbose",
		"--mcp-default-locations", "true", "--extensions-default-location", "/plugins",
		"--effort=medium", "--provider", "openai",
	}})
	got := a.Args(cartridge.RunSpec{
		Prompt: "real task", Workdir: "/real", Model: "routed", ToolPolicy: cartridge.ToolReadWrite,
	}, "/isolated")
	joined := strings.Join(got, "\x00")
	for _, unwanted := range []string{"json-stream", "/elsewhere", "other task", "--plan", "--gateway-stop", "--gateway-status", "--goal", "--verbose", "/plugins"} {
		assert.NotContains(t, joined, unwanted)
	}
	assert.Equal(t, 1, strings.Count(joined, "--output-format"))
	assert.Equal(t, 1, strings.Count(joined, "--mcp-default-locations"))
	assert.Contains(t, got, "--effort=medium")
	assert.True(t, slices.Contains(got, "--provider"))
	assert.Equal(t, "real task", got[len(got)-1])
}

func TestInheritEnv(t *testing.T) {
	t.Setenv("JUNIE_API_KEY", "token")
	t.Setenv("JUNIE_OPENAI_API_KEY", "provider-token")
	t.Setenv("JUNIE_HOME", "/junie-home")
	t.Setenv("SC_UNRELATED", "secret")
	got := InheritEnv([]string{"HOME", "JUNIE_API_KEY"})
	assert.Contains(t, got, "JUNIE_API_KEY")
	assert.Contains(t, got, "JUNIE_OPENAI_API_KEY")
	assert.Contains(t, got, "JUNIE_HOME")
	assert.NotContains(t, got, "SC_UNRELATED")
	assert.True(t, sortStringsAreSorted(got))
}

func sortStringsAreSorted(values []string) bool {
	return slices.IsSorted(values)
}
