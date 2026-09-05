package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/orchestrator"
)

// P9-15: what `sc status` says about the repository's run lock. The lock
// has six verdicts (orchestrator.InspectLock), and `sc status` used to know
// three — held, mine, alive — so a run whose pid had been recycled into an
// unrelated program was reported as "process running" while `sc run` would
// reap the same file as stale. Every helper here is prefixed "lk".

// lkWrite replaces the repository's run lock with rec.
func lkWrite(t *testing.T, runDir string, rec orchestrator.LockRecord) {
	t.Helper()
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(orchestrator.LockPath(runDir), data, 0o600))
}

// lkRecord is a lock the recorded run took, at the recorded epoch.
func lkRecord(pid int) orchestrator.LockRecord {
	return orchestrator.LockRecord{
		LockHolder: orchestrator.LockHolder{Run: statusRunID, PID: pid, TakenAt: statusEpoch},
	}
}

// lkLiveChild starts a process that outlives the test and returns its pid;
// a lock naming it is held by something that really is running.
func lkLiveChild(t *testing.T) int {
	t.Helper()
	child := exec.Command("/bin/sh", "-c", "sleep 60")
	require.NoError(t, child.Start())
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})
	return child.Process.Pid
}

// lkDeadChild returns the pid of a process that has already exited.
func lkDeadChild(t *testing.T) int {
	t.Helper()
	child := exec.Command("/bin/sh", "-c", "exit 0")
	require.NoError(t, child.Run())
	return child.Process.Pid
}

// lkNormalise makes a report golden-stable: the state directory, the temp
// base and whichever pid the test happened to get.
func lkNormalise(out, base, repo string, pids ...int) string {
	out = statusNormalise(out, base, repo)
	for _, pid := range pids {
		out = strings.ReplaceAll(out, strconv.Itoa(pid), "<pid>")
	}
	return out
}

// lkOptions installs lock-probe options for one test. The probes it fakes
// are the two that cannot be arranged here — this machine's name and a
// process's start stamp — and nothing else: InspectLock itself runs as it
// does in production.
func lkOptions(t *testing.T, opts orchestrator.LockOptions) {
	t.Helper()
	saved := statusLockOptions
	t.Cleanup(func() { statusLockOptions = saved })
	statusLockOptions = opts
}

// TestStatusLockStateNamesMatchTheProbe pins the six verdict names this
// file reports against the ones the probe spells, so a state renamed in
// orchestrator cannot leave `sc status` reporting a name nothing produces.
func TestStatusLockStateNamesMatchTheProbe(t *testing.T) {
	for _, tc := range []struct {
		state orchestrator.LockState
		name  string
	}{
		{orchestrator.LockFree, statusLockFree},
		{orchestrator.LockMine, statusLockMine},
		{orchestrator.LockLive, statusLockLive},
		{orchestrator.LockStale, statusLockStale},
		{orchestrator.LockElsewhere, statusLockElsewhere},
		{orchestrator.LockCorrupt, statusLockCorrupt},
	} {
		require.Equal(t, tc.state.String(), tc.name)
	}
}

// TestStatusLockDistinguishesTheSixVerdicts is P9-15's Done-when. Each case
// is the same recorded run with a different lock in front of it.
func TestStatusLockDistinguishesTheSixVerdicts(t *testing.T) {
	t.Run("live", func(t *testing.T) {
		_, repo := statusEnv(t)
		dir := statusRecord(t, repo, true)
		pid := lkLiveChild(t)
		lkWrite(t, dir, lkRecord(pid))

		rep, err := RunStatus(repo, statusRunID, 0)
		require.NoError(t, err)
		require.Equal(t, statusLockLive, rep.Lock.State)
		require.True(t, rep.Running())
		require.False(t, rep.Crashed())
		require.False(t, rep.Lock.PIDReused)
	})

	t.Run("mine", func(t *testing.T) {
		// The lock names this very process, which is what a run resuming
		// itself in-process finds.
		_, repo := statusEnv(t)
		statusRecord(t, repo, true) // written with os.Getpid()

		rep, err := RunStatus(repo, statusRunID, 0)
		require.NoError(t, err)
		require.Equal(t, statusLockMine, rep.Lock.State)
		require.True(t, rep.Running())
	})

	t.Run("free", func(t *testing.T) {
		_, repo := statusEnv(t)
		dir := statusRecord(t, repo, true)
		statusUnlock(t, dir)

		rep, err := RunStatus(repo, statusRunID, 0)
		require.NoError(t, err)
		require.Equal(t, statusLockFree, rep.Lock.State)
		require.False(t, rep.Lock.Held())
		require.True(t, rep.Crashed())
	})

	t.Run("stale", func(t *testing.T) {
		_, repo := statusEnv(t)
		dir := statusRecord(t, repo, true)
		lkWrite(t, dir, lkRecord(lkDeadChild(t)))

		rep, err := RunStatus(repo, statusRunID, 0)
		require.NoError(t, err)
		require.Equal(t, statusLockStale, rep.Lock.State)
		require.False(t, rep.Lock.PIDReused, "the pid is gone, not reused")
		require.True(t, rep.Crashed())
	})

	t.Run("reused pid", func(t *testing.T) {
		// The pid is alive and answers a liveness probe, but the process
		// behind it started at a different moment, so it is not the sc that
		// took the lock. This is the case `sc status` used to report as
		// "process running" while `sc run` removed the same lock as stale.
		_, repo := statusEnv(t)
		dir := statusRecord(t, repo, true)
		pid := lkLiveChild(t)
		rec := lkRecord(pid)
		rec.Started = "7"
		lkWrite(t, dir, rec)
		lkOptions(t, orchestrator.LockOptions{StartOf: func(int) string { return "9" }})

		rep, err := RunStatus(repo, statusRunID, 0)
		require.NoError(t, err)
		require.Equal(t, statusLockStale, rep.Lock.State)
		require.True(t, rep.Lock.PIDReused)
		require.False(t, rep.Running(), "a recycled pid is not the run")
		require.True(t, rep.Crashed())
		require.True(t, rep.Liveness.Alive, "the bare probe still says the pid exists")
	})

	t.Run("elsewhere", func(t *testing.T) {
		_, repo := statusEnv(t)
		dir := statusRecord(t, repo, true)
		rec := lkRecord(4242)
		rec.Host = "another-machine.invalid"
		lkWrite(t, dir, rec)

		rep, err := RunStatus(repo, statusRunID, 0)
		require.NoError(t, err)
		require.Equal(t, statusLockElsewhere, rep.Lock.State)
		require.False(t, rep.Running())
		require.True(t, rep.Unknowable())
		require.False(t, rep.Crashed(), "a run on another machine may still be firing")
	})

	t.Run("corrupt", func(t *testing.T) {
		_, repo := statusEnv(t)
		dir := statusRecord(t, repo, true)
		require.NoError(t, os.WriteFile(orchestrator.LockPath(dir), []byte("{"), 0o600)) // a half-written lock

		rep, err := RunStatus(repo, statusRunID, 0)
		require.NoError(t, err)
		require.Equal(t, statusLockCorrupt, rep.Lock.State)
		require.NotEmpty(t, rep.Lock.Error)
		require.False(t, rep.Lock.Held())
		require.True(t, rep.Crashed())
	})
}

// TestStatusLockElsewhereGolden: a lock written on another machine, which
// this one cannot probe. The phase must not claim the run crashed.
func TestStatusLockElsewhereGolden(t *testing.T) {
	base, repo := statusEnv(t)
	dir := statusRecord(t, repo, true)
	rec := lkRecord(4242)
	rec.Host = "another-machine.invalid"
	lkWrite(t, dir, rec)

	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	require.NotContains(t, out, "no live process")
	rootGolden(t, "status-lock-elsewhere", lkNormalise(out, base, repo))
}

// TestStatusLockReusedPIDGolden is the report P9-15 is about: the pid in
// the lock exists, and it is not the run.
func TestStatusLockReusedPIDGolden(t *testing.T) {
	base, repo := statusEnv(t)
	dir := statusRecord(t, repo, true)
	pid := lkLiveChild(t)
	rec := lkRecord(pid)
	rec.Started = "7"
	lkWrite(t, dir, rec)
	lkOptions(t, orchestrator.LockOptions{StartOf: func(int) string { return "9" }})

	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	require.Contains(t, out, "belongs to another program now")
	rootGolden(t, "status-lock-reused", lkNormalise(out, base, repo, pid))
}

// TestStatusLockCorruptGolden: a lock file nothing can read. It is the one
// verdict that names no run and no pid, so the report says what it can.
func TestStatusLockCorruptGolden(t *testing.T) {
	base, repo := statusEnv(t)
	dir := statusRecord(t, repo, true)
	require.NoError(t, os.WriteFile(orchestrator.LockPath(dir), []byte("{"), 0o600)) // a half-written lock

	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	rootGolden(t, "status-lock-corrupt", lkNormalise(out, base, repo))
}

// TestStatusLockStaleGolden: the lock the run left behind when it was
// killed, whose pid is simply gone.
func TestStatusLockStaleGolden(t *testing.T) {
	base, repo := statusEnv(t)
	dir := statusRecord(t, repo, true)
	pid := lkDeadChild(t)
	lkWrite(t, dir, lkRecord(pid))

	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	rootGolden(t, "status-lock-stale", lkNormalise(out, base, repo, pid))
}

// TestStatusLockAnotherRunGolden: the lock is live, and it belongs to a
// later run of the same repository — so this run is over, whatever its
// phase says.
func TestStatusLockAnotherRunGolden(t *testing.T) {
	base, repo := statusEnv(t)
	dir := statusRecord(t, repo, true)
	pid := lkLiveChild(t)
	rec := lkRecord(pid)
	rec.Run = "20260904-000000-ffff"
	lkWrite(t, dir, rec)

	out, err := rootRun(t, "--repo", repo, "status", "--events", "0")
	require.NoError(t, err)
	rootGolden(t, "status-lock-foreign", lkNormalise(out, base, repo, pid))
}

// TestStatusLockIsInTheJSON: a machine consumer gets the verdict itself,
// not only the sentence, so it need not parse English to tell a live run
// from a recycled pid.
func TestStatusLockIsInTheJSON(t *testing.T) {
	_, repo := statusEnv(t)
	dir := statusRecord(t, repo, true)
	pid := lkLiveChild(t)
	rec := lkRecord(pid)
	rec.Started = "7"
	lkWrite(t, dir, rec)
	lkOptions(t, orchestrator.LockOptions{StartOf: func(int) string { return "9" }})

	stdout, stderr, code := runExec(t, "--json", "--repo", repo, "status")
	require.Equal(t, 0, code, stderr)
	var rep StatusReport
	require.NoError(t, json.Unmarshal([]byte(stdout), &rep), stdout)
	require.Equal(t, statusLockStale, rep.Lock.State)
	require.True(t, rep.Lock.PIDReused)
	require.True(t, rep.Lock.Mine)
	require.Equal(t, pid, rep.Lock.PID)
	require.Equal(t, orchestrator.RepoLockPath(config.StateDir(repo)), rep.Lock.Path)
}
