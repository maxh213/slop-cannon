package agy

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
)

func newAdapter(t testing.TB, cfg config.Cartridge) *Adapter {
	t.Helper()
	c, err := New(Name, cfg)
	require.NoError(t, err)
	a, ok := c.(*Adapter)
	require.True(t, ok)
	return a
}

func TestNewAndBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	c, err := New("", config.Cartridge{Command: "/opt/agy/bin/agy"})
	require.NoError(t, err, "the factory performs no lookup or other I/O")
	assert.Equal(t, Name, c.Name())
	custom, ok := c.(*Adapter)
	require.True(t, ok)
	assert.Equal(t, "/opt/agy/bin/agy", custom.Binary())

	c, err = New("agy-work", config.Cartridge{})
	require.NoError(t, err)
	assert.Equal(t, "agy-work", c.Name())
	custom, ok = c.(*Adapter)
	require.True(t, ok)
	assert.Equal(t, "agy", custom.Binary())
}

func TestArgsFullSpecAndPromptLast(t *testing.T) {
	a := newAdapter(t, config.Cartridge{Model: "cfg-model", Args: []string{"--effort", "high"}})
	spec := cartridge.RunSpec{
		SystemPrompt: "be terse", Prompt: "do it", Workdir: "/w",
		Model: "routed-model", ToolPolicy: cartridge.ToolReadWrite,
		MaxTurns: 7, Timeout: 90 * time.Second,
	}
	assert.Equal(t, []string{
		"--output-format", "json", "--add-dir", "/w",
		"--model", "routed-model", "--print-timeout", "1m30s",
		"--effort", "high", "--mode", "accept-edits", "--dangerously-skip-permissions",
		"--print", "be terse" + SystemPromptSeparator + "do it",
	}, a.Args(spec))
	assert.NotContains(t, strings.Join(a.Args(spec), " "), "max-turns")
}

func TestArgsModelFallbackAndZeroTimeout(t *testing.T) {
	a := newAdapter(t, config.Cartridge{Model: "cfg-model"})
	got := a.Args(cartridge.RunSpec{Prompt: "p", Workdir: "/w", ToolPolicy: cartridge.ToolReadWrite})
	assert.Contains(t, strings.Join(got, " "), "--model cfg-model")
	assert.NotContains(t, got, flagPrintTimeout, "zero preserves agy's unlimited print timeout")

	plain := newAdapter(t, config.Cartridge{})
	got = plain.Args(cartridge.RunSpec{Prompt: "p", Workdir: "/w", ToolPolicy: cartridge.ToolReadWrite})
	assert.NotContains(t, got, flagModel)
	assert.Equal(t, []string{flagPrint, "p"}, got[len(got)-2:])
}

func TestArgsReadOnlyPlanWriteRefusalCandidate(t *testing.T) {
	a := newAdapter(t, config.Cartridge{Args: []string{"--mode", "auto"}})
	got := a.Args(cartridge.RunSpec{Prompt: "write a file", Workdir: "/w", ToolPolicy: cartridge.ToolReadOnly})
	assert.Equal(t, []string{flagMode, modePlan}, got[len(got)-4:len(got)-2])
	assert.NotContains(t, got, flagSkipPermissions)
	assert.NotContains(t, got, flagDisableSlashCommands)
	assert.Equal(t, []string{flagPrint, "write a file"}, got[len(got)-2:])
	assert.Equal(t, []string{flagMode, modePlan}, ToolFlags(cartridge.ToolReadExec), "unsupported policies fail closed")
}

func TestInheritEnv(t *testing.T) {
	t.Setenv("AGY_ACCOUNT", "work")
	t.Setenv("AGY_LLM_GATEWAY_API_KEY", "test-key")
	t.Setenv("GEMINI_API_KEY", "gemini-test")
	t.Setenv("GOOGLE_GEMINI_BASE_URL", "https://example.invalid")
	t.Setenv("SC_UNRELATED_SECRET", "do-not-inherit")
	got := InheritEnv([]string{"HOME", "AGY_ACCOUNT"})
	assert.True(t, sort.StringsAreSorted(got))
	for _, name := range []string{"HOME", "AGY_ACCOUNT", "AGY_LLM_GATEWAY_API_KEY", "GEMINI_API_KEY", "GOOGLE_GEMINI_BASE_URL"} {
		assert.Contains(t, got, name)
	}
	assert.NotContains(t, got, "SC_UNRELATED_SECRET")
	seen := map[string]int{}
	for _, name := range got {
		seen[name]++
		assert.Equal(t, 1, seen[name])
	}
}

func TestChildEnvLayering(t *testing.T) {
	a := newAdapter(t, config.Cartridge{Env: map[string]string{"FROM_CFG": "cfg", "SHARED": "cfg"}})
	assert.Equal(t, map[string]string{"FROM_CFG": "cfg", "SHARED": "spec", "FROM_SPEC": "spec"},
		a.childEnv(map[string]string{"SHARED": "spec", "FROM_SPEC": "spec"}))
}
