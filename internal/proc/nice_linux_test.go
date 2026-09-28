//go:build linux

package proc

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReniceChild is the child half of TestReniceCoversEveryThreadAndChild:
// renicing is one-way for an unprivileged process, so it runs in a copy of
// the test binary rather than in the test process itself.
func TestReniceChild(t *testing.T) {
	if os.Getenv("SC_RENICE_CHILD") == "" {
		t.Skip("child half of TestReniceCoversEveryThreadAndChild")
	}
	if err := Renice(7); err != nil {
		fmt.Println("error", err)
		return
	}
	entries, _ := os.ReadDir("/proc/self/task")
	for _, e := range entries {
		tid, _ := strconv.Atoi(e.Name())
		if n, err := getNice(tid); err == nil {
			fmt.Println("thread", n)
		}
	}
	out, err := exec.Command("sh", "-c", "cut -d' ' -f19 /proc/$$/stat").Output()
	if err != nil {
		fmt.Println("error", err)
		return
	}
	fmt.Println("child", strings.TrimSpace(string(out)))
}

func TestReniceCoversEveryThreadAndChild(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestReniceChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "SC_RENICE_CHILD=1")
	out, err := cmd.Output()
	require.NoError(t, err, string(out))
	text := string(out)
	require.NotContains(t, text, "error")
	threads := 0
	for _, line := range strings.Split(text, "\n") {
		if n, ok := strings.CutPrefix(line, "thread "); ok {
			threads++
			assert.Equal(t, "7", n, "every thread of the process is reniced")
		}
	}
	assert.Greater(t, threads, 1, "a Go process runs on more than one thread")
	assert.Contains(t, text, "child 7\n", "a process started afterwards inherits the niceness")
}

func TestReniceZeroLeavesThePriorityAlone(t *testing.T) {
	before, err := getNice(os.Getpid())
	require.NoError(t, err)
	require.NoError(t, Renice(0))
	after, err := getNice(os.Getpid())
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestReniceReportsAMissingThreadList(t *testing.T) {
	old := taskDir
	taskDir = t.TempDir() + "/missing"
	t.Cleanup(func() { taskDir = old })
	err := Renice(5)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing threads")
}
