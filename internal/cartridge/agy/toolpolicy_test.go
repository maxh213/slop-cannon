package agy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
)

func TestToolPolicyEnforcement(t *testing.T) {
	a := newAdapter(t, config.Cartridge{})
	en, err := a.ToolPolicyEnforcement(cartridge.ToolReadWrite)
	require.NoError(t, err)
	assert.Equal(t, agents.EnforceNative, en.Mode)
	assert.Equal(t, Name, en.Cartridge)
	assert.Equal(t, []string{flagMode, modeAcceptEdits, flagSkipPermissions}, en.Controls)
	assert.NoError(t, en.Check())

	_, err = a.ToolPolicyEnforcement(cartridge.ToolReadOnly)
	require.ErrorIs(t, err, ErrReadOnlyUnverified)
	_, err = agents.EnforcementFor(a, cartridge.ToolReadOnly)
	require.ErrorIs(t, err, ErrReadOnlyUnverified)

	_, err = a.ToolPolicyEnforcement(cartridge.ToolReadExec)
	require.ErrorIs(t, err, ErrReadExecUnsupported)
	_, err = a.ToolPolicyEnforcement("read_everything")
	require.ErrorIs(t, err, domain.ErrUnknownValue)
}
