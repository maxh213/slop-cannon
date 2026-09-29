package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/cursor"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

// The D-27 split this wiring exists for: the product owner stays on Claude
// with its role model while engineers and testers fire on Cursor, whose
// argv carries its own configured model and never Claude's.
func TestRoleCartridgesSplitClaudeAndCursor(t *testing.T) {
	home := cartridgeTestEnv(t)
	testutil.FakeBin(t, "claude", `
case "$1" in
  --version) echo "2.1.258 (Claude Code)";;
  auth) echo '{"loggedIn": true, "authMethod": "claude.ai"}';;
esac`)
	testutil.FakeBin(t, cursor.DefaultBinary, `
case "$1" in
  --version) echo "2026.09.28-64d2043";;
  status) echo "✓ Logged in as user@example.com";;
esac`)

	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	path := config.GlobalPath()
	require.True(t, filepath.IsAbs(path) && strings.HasPrefix(path, home), path)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`schema_version = 1
active_cartridge = 'claude'

[cartridges]
[cartridges.claude]
model = 'claude-sonnet-5-5'

[cartridges.cursor]
model = 'auto'

[roles.po]
model = "claude-opus-5-5"

[roles.engineer]
cartridge = "cursor"

[roles.tester]
cartridge = "cursor"
`), 0o600))

	eff, err := config.Load(repo)
	require.NoError(t, err)
	assert.Equal(t, "claude-opus-5-5", eff.RoleModels[domain.RolePO])
	assert.Empty(t, eff.RoleModels[domain.RoleEngineer])

	sel, err := cartridge.BuildSelector(cartridge.Default, eff.Config)
	require.NoError(t, err)
	chosen := map[domain.Role]string{}
	for _, role := range []domain.Role{domain.RolePO, domain.RoleEngineer, domain.RoleTester} {
		s, err := sel.Choose(context.Background(), role)
		require.NoError(t, err, role)
		assert.False(t, s.FellBack, role)
		chosen[role] = s.Name
	}
	assert.Equal(t, map[domain.Role]string{
		domain.RolePO: "claude", domain.RoleEngineer: "cursor", domain.RoleTester: "cursor",
	}, chosen)

	c, ok := sel.Cartridge("cursor")
	require.True(t, ok)
	// The supervisor clears the active cartridge's model on a foreign
	// cartridge (TestSupervisorDropsTheActiveModelOnAnotherCartridge), so
	// the spec arrives with no model and the adapter uses "auto".
	ca, ok := c.(*cursor.Adapter)
	require.True(t, ok)
	args := ca.Args(cartridge.RunSpec{Workdir: repo, Prompt: "p", ToolPolicy: cartridge.ToolReadWrite})
	assert.Contains(t, args, "auto")
	assert.NotContains(t, args, "claude-sonnet-5-5")
	assert.NotContains(t, args, "claude-opus-5-5")
}
