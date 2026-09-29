package cursor

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

// TestLive_RealCLI fires the installed Cursor Agent once, on the "auto"
// model. It is skipped unless SC_CURSOR_LIVE=1 is set, because it needs the
// CLI, a login and a network; CI never sets it:
//
//	SC_CURSOR_LIVE=1 go test -run TestLive -v ./internal/cartridge/cursor/
func TestLive_RealCLI(t *testing.T) {
	if os.Getenv("SC_CURSOR_LIVE") != "1" {
		t.Skip("set SC_CURSOR_LIVE=1 to run against the real cursor-agent CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	c, err := cartridge.New(Name, config.Cartridge{Model: "auto"})
	require.NoError(t, err)

	st, err := c.Detect(ctx)
	require.NoError(t, err, "detect: %+v", st)
	assert.True(t, st.Ready())
	t.Logf("detect: %+v", st)

	s, err := c.Run(ctx, cartridge.RunSpec{
		SystemPrompt: "You are a test fixture. Answer with the single word requested and nothing else.",
		Prompt:       "Reply with exactly the word PONG",
		Workdir:      t.TempDir(),
		ToolPolicy:   cartridge.ToolReadWrite,
		Timeout:      3 * time.Minute,
	})
	require.NoError(t, err)
	res, err := cartridge.Collect(s, func(ev cartridge.SessionEvent) { t.Log(cartridge.Describe(ev)) })
	require.NoError(t, err)
	assert.Contains(t, strings.ToUpper(res.Output), "PONG")
	assert.NotEmpty(t, res.SessionID)
	assert.Positive(t, res.Usage.OutputTokens)
}
