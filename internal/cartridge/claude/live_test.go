package claude

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
)

// claudeLiveModel is the cheapest model for a smoke test against the real
// CLI; one turn costs a fraction of a cent.
const claudeLiveModel = "claude-haiku-4-5-20251001"

// TestLive_RealCLI fires the installed Claude Code once. It is skipped
// unless SC_CLAUDE_LIVE=1 is set, because it needs the CLI, a login and a
// network; CI never sets it. Run it after changing Args, InheritEnv or the
// parser:
//
//	SC_CLAUDE_LIVE=1 go test -run TestLive -v ./internal/cartridge/claude/
func TestLive_RealCLI(t *testing.T) {
	if os.Getenv("SC_CLAUDE_LIVE") != "1" {
		t.Skip("set SC_CLAUDE_LIVE=1 to run against the real claude CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, err := cartridge.New(Name, config.Cartridge{})
	require.NoError(t, err)

	st, err := c.Detect(ctx)
	require.NoError(t, err, "detect: %+v", st)
	assert.True(t, st.Installed)
	assert.NotEmpty(t, st.Version)
	assert.True(t, st.Ready())
	t.Logf("detect: %+v", st)

	spec := cartridge.RunSpec{
		SystemPrompt: "You are a test fixture. Answer with the single word requested and nothing else.",
		Prompt:       "Reply with exactly the word PONG",
		Workdir:      t.TempDir(),
		Model:        claudeLiveModel,
		ToolPolicy:   cartridge.ToolReadOnly,
		MaxTurns:     1,
		Timeout:      2 * time.Minute,
	}
	s, err := c.Run(ctx, spec)
	require.NoError(t, err)
	var evs []cartridge.SessionEvent
	res, err := cartridge.Collect(s, func(ev cartridge.SessionEvent) {
		evs = append(evs, ev)
		t.Log(cartridge.Describe(ev))
	})
	require.NoError(t, err)
	require.NotEmpty(t, evs)
	assert.Equal(t, cartridge.EventDone, evs[len(evs)-1].Kind)
	assert.Contains(t, strings.ToUpper(res.Output), "PONG")
	assert.Equal(t, 0, res.ExitCode)
	assert.NotEmpty(t, res.SessionID)
	assert.True(t, res.Usage.HasCost)
	assert.Positive(t, res.Usage.OutputTokens)
	assert.Positive(t, res.Usage.Turns)
	assert.Positive(t, res.Duration)
}
