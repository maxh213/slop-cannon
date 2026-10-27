package dandelion

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
)

// TestLiveSelectedRoutes verifies real discovery and delegation with only the
// explicitly requested accounts eligible. Each firing uses a temporary workspace.
func TestLiveSelectedRoutes(t *testing.T) {
	requested := os.Getenv("SC_DANDELION_LIVE")
	if requested == "" {
		t.Skip("set SC_DANDELION_LIVE to comma-separated account ids for real route verification")
	}
	for _, account := range strings.Split(requested, ",") {
		t.Run(account, func(t *testing.T) {
			_, known := providerByID(account)
			require.True(t, known, "unknown provider %q", account)
			c, err := New(Name, config.Cartridge{
				Command: os.Getenv("SC_DANDELION_LIVE_COMMAND"), Model: string(ModeStandard),
				Env: map[string]string{"DANDELION_DISABLE": Disabled([]string{account}, "")},
			})
			require.NoError(t, err)
			var log bytes.Buffer
			adapter, ok := c.(*Adapter)
			require.True(t, ok)
			adapter.logger = slog.New(slog.NewTextHandler(&log, nil))
			session, err := c.Run(context.Background(), cartridge.RunSpec{
				Prompt:  "Reply with exactly SC-ROUTE-OK and nothing else. Do not use tools.",
				Workdir: t.TempDir(), ToolPolicy: cartridge.ToolReadExec,
				MaxTurns: 1, Timeout: 2 * time.Minute,
			})
			require.NoError(t, err)
			result, err := cartridge.Collect(session, nil)
			require.NoError(t, err)
			assert.Contains(t, result.Output, "SC-ROUTE-OK")
			assert.Contains(t, log.String(), "account="+account)
		})
	}
}
