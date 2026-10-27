//go:build linux

package junie

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/config"
	"slop-cannon/internal/testutil"
)

func processAlive(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	text := string(data)
	fields := strings.Fields(text[strings.LastIndexByte(text, ')')+1:])
	return len(fields) > 0 && fields[0] != "Z"
}

// The fake CLI starts work with setsid, escaping its process group just as a
// CLI-launched service can. Killing the session must reap that tagged work;
// no gateway-stop command is available or needed for cleanup.
func TestRunKillReapsDescendantThatLeftProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not available")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	testutil.FakeBin(t, DefaultBinary, `
setsid sh -c 'echo $$ > "$1"; exec sleep 300' sh "$SC_JUNIE_CHILD_PID" >/dev/null 2>&1 &
while [ ! -s "$SC_JUNIE_CHILD_PID" ]; do sleep 0.01; done
sleep 300
`)
	a := junieAdapter(t, config.Cartridge{})
	s, err := a.Run(context.Background(), junieSpec(t, map[string]string{"SC_JUNIE_CHILD_PID": pidFile}))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(pidFile)
		return err == nil && len(strings.TrimSpace(string(data))) > 0
	}, 3*time.Second, 20*time.Millisecond)
	data, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	t.Cleanup(func() {
		if processAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	s.Kill()
	_, err = cartridge.Collect(s, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, cartridge.ErrKilled))
	assert.Eventually(t, func() bool { return !processAlive(pid) }, 3*time.Second, 20*time.Millisecond,
		"killed CLI work must not be orphaned after leaving the process group")
}
