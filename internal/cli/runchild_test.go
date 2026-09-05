package cli

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/config"
	"slop-cannon/internal/orchestrator"
)

// A real `sc` process, for the two tests that cannot be run in this one.
//
//   - P9-20 needs file descriptor 1 to be a pipe whose reader goes away,
//     because that is the only thing the Go runtime turns into SIGPIPE.
//   - P9-24 needs a run to be *somebody else's*: `sc stop` reads a lock,
//     finds a pid that is not its own and interrupts it.
//
// Both re-execute this test binary, which is `sc` plus the mock cartridge,
// with the arguments in an environment variable. The child inherits the
// whole environment the fixture set up (HOME, XDG_*, PATH with the bd, gh
// and git shims, the GIT_* identity), so all it has to do for itself is the
// handful of package variables runFireSetup tunes in-process.

// runChildArgsEnv carries the child's argv, one argument per unit
// separator.
const runChildArgsEnv = "SC_TEST_CHILD_ARGS"

// runChildSep separates the arguments in runChildArgsEnv.
const runChildSep = "\x1f"

// TestChildRunsTheCommand is not a test: it is the body of the child
// process runChildCommand starts. It skips out of the way in every ordinary
// run of the suite.
func TestChildRunsTheCommand(t *testing.T) {
	raw, ok := os.LookupEnv(runChildArgsEnv)
	if !ok {
		t.Skip("the child half of runChildCommand; it runs only when the parent asks for it")
	}
	cartridgeAllowMock(t)
	runConductorInterval, runTickInterval, runSettleInterval = 20*time.Millisecond, 15*time.Millisecond, 20*time.Millisecond
	// The shim's self-check probes the real bd's --readonly enforcement,
	// which the fixture's stand-in does not implement (runFireSetup says
	// the same thing about its own process).
	runShimOptions = beads.ShimOptions{SkipVerify: true}

	var args []string
	if raw != "" {
		args = strings.Split(raw, runChildSep)
	}
	os.Exit(ExecuteContext(context.Background(), args, os.Stdout, os.Stderr))
}

// runChildCommand builds the command that runs `sc args...` in a child
// process of this test binary. The caller sets Stdout/Stderr and starts it.
func runChildCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(exe, "-test.run=^TestChildRunsTheCommand$", "-test.timeout=5m") //nolint:gosec // exe is this very test binary.
	cmd.Env = append(os.Environ(), runChildArgsEnv+"="+strings.Join(args, runChildSep))
	return cmd
}

// runChildSignalled reports whether the child was killed by a signal rather
// than exiting on its own, and which signal it was.
func runChildSignalled(cmd *exec.Cmd) (bool, string) {
	st := cmd.ProcessState
	if st == nil {
		return false, ""
	}
	if st.Exited() {
		return false, ""
	}
	return true, st.String()
}

// runChildAwaitLock waits for the child to take the repository's run lock
// and returns what it says. It is how a test knows the run is really going
// before it interferes with it.
func runChildAwaitLock(t *testing.T, repo string, pid int) orchestrator.LockRecord {
	t.Helper()
	state := config.StateDir(repo)
	deadline := time.Now().Add(runFireTimeout)
	for time.Now().Before(deadline) {
		p := orchestrator.InspectLock(state, orchestrator.LockOptions{})
		if p.State == orchestrator.LockLive && p.Record.PID == pid {
			return p.Record
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the child never took the run lock of %s", repo)
	return orchestrator.LockRecord{}
}

// runChildFirstLine reads one line of the child's output, so a test can
// wait for the run to be under way before it does anything.
func runChildFirstLine(t *testing.T, r io.Reader) string {
	t.Helper()
	line, err := bufio.NewReader(r).ReadString('\n')
	require.NoError(t, err, "the child printed nothing")
	return line
}
