package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/orchestrator"
	"slop-cannon/internal/testutil"
)

// The late half of P9-04's interlock, from the command's side.
//
// `sc <repo>` checks the repository's run lock twice: once in fireCannon's
// preflight, before anything is built, and once inside Run.Start, which is
// what actually takes the lock. Everything between the two is a window
// another sc can win — and the loser has by then created its run directory,
// which is what these tests are about.

// runLockRaceID is the run the other sc is pretending to be.
const runLockRaceID = "20260101-000000-aaaa"

// TestRunLosingTheLockRaceLeavesNoRunDirectory: an `sc` that passes the
// preflight and then loses the lock at Run.Start must leave nothing behind.
//
// It used to leave <state>/runs/<id>/ with a zero-byte event log and no
// state file, and config.LatestRunID takes the lexically greatest run id on
// disk — so whenever the loser's random suffix sorted after the live run's,
// `sc status` reported that empty directory ("phase new / 0 beads / 0
// events") while the lock was held by a different, live run, and `sc
// resume` adopted it and started a brand-new autopilot run under an id that
// never ran (P8-31). The refusal points the user at `sc status`, so what
// `sc status` then shows has to be the live run.
//
// The race is made deterministic rather than raced for: a `git` shim plants
// the other run's lock on the first git call that happens *after* the run
// directory exists — inside assembleRun, past the preflight, before
// Run.Start — which is exactly the window, and exactly the state (a run
// directory already created) the bug needed.
func TestRunLosingTheLockRaceLeavesNoRunDirectory(t *testing.T) {
	env := runFireSetup(t, false)

	// The other run's holder: a live process that is not this one.
	held := exec.Command("sleep", "60")
	require.NoError(t, held.Start())
	t.Cleanup(func() { _ = held.Process.Kill(); _, _ = held.Process.Wait() })

	state := config.StateDir(env.Repo.Dir)
	lock := orchestrator.RepoLockPath(state)
	host, _ := os.Hostname()
	rec, err := json.Marshal(orchestrator.LockHolder{
		Run: runLockRaceID, PID: held.Process.Pid, Host: host, TakenAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	marker := filepath.Join(t.TempDir(), "planted")
	testutil.FakeBin(t, "git", fmt.Sprintf(`if [ -d '%s' ] && [ ! -f '%s' ]; then
  : > '%s'
  printf '%%s\n' '%s' > '%s'
fi
exec "$SC_FAKEBIN_REAL" "$@"
`, filepath.Join(state, "runs"), marker, marker, rec, lock))

	_, stderr, code := runFireExec(t, env.Repo.Dir, "-m", runFireBrief, "--headless", "--cartridge", "mock")
	require.Equal(t, 1, code, stderr)
	require.FileExists(t, marker, "the lock has to be planted after the run directory exists")

	// The refusal is the preflight's own sentence, whichever check raised
	// it: it names the live run and says how to look at it.
	require.Contains(t, stderr, runLockRaceID)
	require.Contains(t, stderr, "sc status")
	require.NotContains(t, stderr, "resume it",
		"`sc resume` runs the same preflight, so the same live lock would refuse it")
	got, err := os.ReadFile(lock)
	require.NoError(t, err)
	require.Equal(t, string(rec)+"\n", string(got), "the live run's lock is left byte-identical")

	// And nothing is left for `sc status` to report or `sc resume` to adopt.
	id, found, err := config.LatestRunID(env.Repo.Dir)
	require.NoError(t, err)
	require.False(t, found, "the refused run left %s behind", id)
	entries, err := os.ReadDir(filepath.Join(state, "runs"))
	require.NoError(t, err)
	require.Empty(t, entries, "not even an empty run directory survives a refused start")
}
