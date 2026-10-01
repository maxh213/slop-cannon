//go:build linux

package proc

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// escapedSleeper starts, through sh, a sleep that leaves sh's process group
// with setsid and outlives sh — what `script -c "npm start"` does in an
// agent's shell — and returns its pid.
func escapedSleeper(t *testing.T, reap bool) int {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not available")
	}
	res, err := Run(context.Background(), Spec{
		Command:         "sh",
		Args:            []string{"-c", "setsid sleep 300 </dev/null >/dev/null 2>&1 & echo $!"},
		GracePeriod:     time.Second,
		ReapDescendants: reap,
	})
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

// alive reports whether pid is a live process rather than missing or a
// zombie waiting to be reaped by whoever inherited it.
func alive(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}

func TestReapDescendants_StopsGrandchildThatLeftTheGroup(t *testing.T) {
	pid := escapedSleeper(t, true)
	assert.Eventually(t, func() bool { return !alive(pid) }, 3*time.Second, 20*time.Millisecond,
		"a setsid grandchild must not outlive a child started with ReapDescendants")
}

func TestReapDescendants_OffLeavesGrandchildRunning(t *testing.T) {
	pid := escapedSleeper(t, false)
	time.Sleep(200 * time.Millisecond)
	assert.True(t, alive(pid), "without ReapDescendants nothing outside the group is touched")
}

func TestReapDescendants_TagsOnlyTheChildItStarted(t *testing.T) {
	res, err := Run(context.Background(), Spec{Command: "sh", Args: []string{"-c", "env"}, ReapDescendants: true})
	require.NoError(t, err)
	var tags []string
	for _, kv := range strings.Split(res.Stdout, "\n") {
		if strings.HasPrefix(kv, reapTagPrefix) {
			tags = append(tags, kv)
		}
	}
	require.Len(t, tags, 1)
	assert.True(t, strings.HasSuffix(tags[0], "=1"))

	res, err = Run(context.Background(), Spec{Command: "sh", Args: []string{"-c", "env"}})
	require.NoError(t, err)
	assert.NotContains(t, res.Stdout, reapTagPrefix)
}

func TestReapTagged_SparesProcessesWithoutTheTag(t *testing.T) {
	other := exec.Command("sleep", "300")
	other.Env = append(os.Environ(), newReapTag()+"=1")
	require.NoError(t, other.Start())
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })

	reapTagged(newReapTag(), 0)
	assert.True(t, alive(other.Process.Pid), "a sweep only stops processes carrying its own tag")
}
