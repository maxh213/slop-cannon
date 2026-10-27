package junie

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
)

func TestToolPolicyReadWriteOnly(t *testing.T) {
	t.Parallel()
	a := junieAdapter(t, config.Cartridge{})
	en, err := agents.EnforcementFor(a, cartridge.ToolReadWrite)
	require.NoError(t, err)
	assert.Equal(t, agents.EnforceNone, en.Mode)

	for _, p := range []cartridge.ToolPolicy{cartridge.ToolReadOnly, cartridge.ToolReadExec} {
		_, err := agents.EnforcementFor(a, p)
		require.Error(t, err, p)
		assert.Contains(t, err.Error(), "refused tool policy")
		assert.Error(t, validatePolicy(p))
	}
	assert.Error(t, validatePolicy(cartridge.ToolPolicy("unknown")))
}
