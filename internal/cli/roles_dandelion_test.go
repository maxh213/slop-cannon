package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/dandelion"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

// D-30: `sc cartridge --add dandelion --model high` configures the router,
// a run builds it from the default registry, every role can select it
// (Detect needs only the router on PATH and Claude Code), and the product
// owner's read_only firing is held natively, so planning needs no worktree
// copy. Detect never asks the router for a route.
func TestCartridgeDandelionAddAndSelect(t *testing.T) {
	home := cartridgeTestEnv(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	router := testutil.FakeBin(t, dandelion.DefaultBinary, `echo "dandelion was asked for a route" >&2; exit 9`)
	testutil.FakeBin(t, "claude", `
case "$1" in
  --version) echo "2.1.309 (Claude Code)";;
  auth) echo '{"loggedIn": true, "authMethod": "claude.ai"}';;
esac`)

	_, err := cartridgeRun(t, "--add", "dandelion", "--model", "opus")
	require.EqualError(t, err, `cartridge "dandelion" takes model "high" or "standard", not "opus": the router picks the real model`)
	_, exists := cartridgeFile(t)
	require.False(t, exists, "a rejected model must not write the config")

	out, err := cartridgeRun(t, "--add", "dandelion", "--model", "high")
	require.NoError(t, err)
	require.Equal(t, "added cartridge \"dandelion\" (model high); it is now the active cartridge\n", out)

	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	require.DirExists(t, home)
	eff, err := config.Load(repo)
	require.NoError(t, err)

	sel, err := cartridge.BuildSelector(cartridge.Default, eff.Config)
	require.NoError(t, err)
	for _, role := range []domain.Role{domain.RolePO, domain.RoleEngineer, domain.RoleTester} {
		s, err := sel.Choose(context.Background(), role)
		require.NoError(t, err, role)
		assert.Equal(t, dandelion.Name, s.Name, role)
		assert.False(t, s.FellBack, role)
	}
	c, ok := sel.Cartridge(dandelion.Name)
	require.True(t, ok)
	da, ok := c.(*dandelion.Adapter)
	require.True(t, ok)
	assert.Equal(t, dandelion.ModeHigh, da.Mode())

	en, err := agents.EnforcementFor(c, cartridge.ToolReadOnly)
	require.NoError(t, err)
	assert.True(t, en.Native(), "a planning product owner on dandelion needs native enforcement")
	assert.Empty(t, router.ArgLists(t), "selecting the cartridge ran a probe round")
}
