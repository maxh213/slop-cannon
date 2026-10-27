package grok

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
)

func TestToolPolicyEnforcementNative(t *testing.T) {
	t.Parallel()
	a := grokNewAdapter(t, config.Cartridge{})
	for _, policy := range []cartridge.ToolPolicy{cartridge.ToolReadExec, cartridge.ToolReadWrite} {
		enforcement, err := a.ToolPolicyEnforcement(policy)
		require.NoError(t, err, policy)
		assert.Equal(t, Name, enforcement.Cartridge)
		assert.Equal(t, policy, enforcement.Policy)
		assert.Equal(t, agents.EnforceNative, enforcement.Mode)
		assert.Equal(t, ToolFlags(policy), enforcement.Controls)
		assert.NoError(t, enforcement.Check())
		via, err := agents.EnforcementFor(a, policy)
		require.NoError(t, err)
		assert.Equal(t, enforcement, via)
	}
}

func TestToolPolicyEnforcementReadOnlyFailsClosed(t *testing.T) {
	t.Parallel()
	a := grokNewAdapter(t, config.Cartridge{})
	_, err := a.ToolPolicyEnforcement(cartridge.ToolReadOnly)
	require.ErrorIs(t, err, errReadOnlyUnsupported)
	_, err = agents.EnforcementFor(a, cartridge.ToolReadOnly)
	require.ErrorIs(t, err, errReadOnlyUnsupported)
}

func TestToolPolicyEnforcementRejectsUnknown(t *testing.T) {
	t.Parallel()
	a := grokNewAdapter(t, config.Cartridge{})
	_, err := a.ToolPolicyEnforcement("read_everything")
	require.ErrorIs(t, err, domain.ErrUnknownValue)
}

func TestToolPolicyEnforcementKeepsRegistryName(t *testing.T) {
	t.Parallel()
	c, err := New("grok-work", config.Cartridge{})
	require.NoError(t, err)
	a, ok := c.(*Adapter)
	require.True(t, ok)
	enforcement, err := a.ToolPolicyEnforcement(cartridge.ToolReadWrite)
	require.NoError(t, err)
	assert.Equal(t, "grok-work", enforcement.Cartridge)
}
