package claude_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/claude"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
)

// Tests for P5-11: the adapter answers for its own tool-policy enforcement,
// so internal/agents no longer imports this package.

func toolPolicyAdapter(t *testing.T, name string) *claude.Adapter {
	t.Helper()
	c, err := claude.New(name, config.Cartridge{})
	require.NoError(t, err)
	a, ok := c.(*claude.Adapter)
	require.True(t, ok)
	return a
}

func TestToolPolicyEnforcementIsNativeForEveryPolicy(t *testing.T) {
	t.Parallel()
	a := toolPolicyAdapter(t, claude.Name)

	for _, p := range cartridge.ToolPolicies() {
		en, err := a.ToolPolicyEnforcement(p)
		require.NoError(t, err, p)
		assert.Equal(t, claude.Name, en.Cartridge, p)
		assert.Equal(t, p, en.Policy, p)
		assert.Equal(t, agents.EnforceNative, en.Mode, p)
		assert.NoError(t, en.Check(), p)
		assert.NotEmpty(t, en.Detail, p)
		assert.Equal(t, claude.ToolFlags(p), en.Controls, p)

		// Resolved through the interface, agents gets the same answer.
		via, err := agents.EnforcementFor(a, p)
		require.NoError(t, err, p)
		assert.Equal(t, en, via, p)
	}
}

func TestToolPolicyEnforcementKeepsTheRegistryName(t *testing.T) {
	t.Parallel()
	a := toolPolicyAdapter(t, "claude-fable")
	en, err := a.ToolPolicyEnforcement(cartridge.ToolReadOnly)
	require.NoError(t, err)
	assert.Equal(t, "claude-fable", en.Cartridge)
	assert.Equal(t, agents.EnforceNative, en.Mode)
}

func TestToolPolicyEnforcementRejectsAnUnknownPolicy(t *testing.T) {
	t.Parallel()
	a := toolPolicyAdapter(t, claude.Name)
	_, err := a.ToolPolicyEnforcement("read_everything")
	require.ErrorIs(t, err, domain.ErrUnknownValue)

	// agents turns the refusal into a refusal to fire.
	_, err = agents.EnforcementFor(a, "read_everything")
	require.ErrorIs(t, err, domain.ErrUnknownValue)
}

// TestAgentsDoesNotImportThisPackage is the other half of P5-11's "Done
// when": the mapping lives here, so the agents layer no longer drags the
// adapter (and its init-time registry registration) into every importer.
func TestAgentsDoesNotImportThisPackage(t *testing.T) {
	t.Parallel()
	const self = "slop-cannon/internal/cartridge/claude"
	dir := filepath.Join("..", "..", "agents")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	fset := token.NewFileSet()
	seen := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		seen++
		file, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		require.NoError(t, perr)
		for _, imp := range file.Imports {
			path, uerr := strconv.Unquote(imp.Path.Value)
			require.NoError(t, uerr)
			assert.NotEqual(t, self, path, "%s must not import %s (P5-11)", name, self)
		}
	}
	require.NotZero(t, seen, "no source files were checked")
}
