//go:build linux

package junie

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
)

// TestLiveKillReapsDescendant stops real CLI work that escaped its process group.
func TestLiveKillReapsDescendant(t *testing.T) {
	a := liveAdapter(t)
	pidPath := filepath.Join(t.TempDir(), "junie-live-child.pid")
	prompt := fmt.Sprintf("Run exactly this shell command in the foreground and wait for it to exit before replying: `setsid sh -c 'echo $$ > %s; exec sleep 300'`. Do not run it in the background, do not add a timeout, and do nothing else.", pidPath)
	s, err := a.Run(context.Background(), liveSpec(t, prompt))
	require.NoError(t, err)

	var pid int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(pidPath)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && pid > 0
	}, 90*time.Second, 100*time.Millisecond, "the real CLI did not start the requested descendant")
	t.Cleanup(func() {
		if processAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	s.Kill()
	res, err := cartridge.Collect(s, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrKilled))
	assert.NotEqual(t, 0, res.ExitCode)
	assert.Eventually(t, func() bool { return !processAlive(pid) }, 5*time.Second, 20*time.Millisecond,
		"the real CLI descendant must not survive Session.Kill")
}
