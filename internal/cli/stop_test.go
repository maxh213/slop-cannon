package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"slop-cannon/internal/config"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/orchestrator"
)

// Tests for `sc stop` (P9-24). D-18 names it as one of the three ways a run
// ends; until now it did not exist, so the run-lock refusal and the
// troubleshooting page had to send the user to Ctrl-C or `kill`.
//
// The Done-when is TestStopStopsALiveRunFromAnotherTerminal: a real run in
// another process, stopped from this one through the lock alone. The rest
// pin what it says about a lock it must not signal.

// stopPlantLock writes a run lock for repo and returns its path.
func stopPlantLock(t *testing.T, repo string, rec orchestrator.LockRecord) string {
	t.Helper()
	path := orchestrator.RepoLockPath(config.StateDir(repo))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, '\n'), 0o600))
	return path
}

// stopHostname is this machine's name, which a lock has to carry to be
// judged live rather than [orchestrator.LockElsewhere].
func stopHostname(t *testing.T) string {
	t.Helper()
	h, err := os.Hostname()
	require.NoError(t, err)
	return h
}

// stopFastPolls shrinks the wait's poll interval so a test does not spend a
// fifth of a second per iteration.
func stopFastPolls(t *testing.T) {
	t.Helper()
	saved := stopPollInterval
	t.Cleanup(func() { stopPollInterval = saved })
	stopPollInterval = 5 * time.Millisecond
}

// stopStubSignal records the signals `sc stop` sends instead of delivering
// them.
func stopStubSignal(t *testing.T) *[]os.Signal {
	t.Helper()
	saved := stopSignal
	t.Cleanup(func() { stopSignal = saved })
	var mu sync.Mutex
	sent := &[]os.Signal{}
	stopSignal = func(_ int, sig os.Signal) error {
		mu.Lock()
		defer mu.Unlock()
		*sent = append(*sent, sig)
		return nil
	}
	return sent
}

// stopSleeper starts a child process standing in for the sc holding the
// lock, and returns its pid. ignoreInterrupt makes it a run that will not
// stop, which is what the interrupt-refused path needs.
//
// It waits for the child to say it is ready, because a shell signalled
// before it has run its `trap` dies of the default disposition and the test
// would be measuring the race rather than the command. It is also reaped as
// soon as it dies: an unreaped child is a zombie, which still answers a
// liveness probe, so the wait would never see it go.
func stopSleeper(t *testing.T, ignoreInterrupt bool) int {
	t.Helper()
	script := "echo ready; exec sleep 60"
	if ignoreInterrupt {
		script = `trap "" INT; echo ready; sleep 60`
	}
	cmd := exec.Command("sh", "-c", script)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	line, err := bufio.NewReader(out).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", line)
	reaped := make(chan struct{})
	go func() { defer close(reaped); _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-reaped })
	return cmd.Process.Pid
}

func TestStopHelpGolden(t *testing.T) {
	out, err := rootRun(t, "stop", "--help")
	require.NoError(t, err)
	rootGolden(t, "stop-help", out)
}

// TestStopStopsALiveRunFromAnotherTerminal is P9-24's Done-when: a real run
// in another process, stopped from this one with nothing but the run lock
// to go on, ending as a user stop with its lock released.
func TestStopStopsALiveRunFromAnotherTerminal(t *testing.T) {
	env := runFireSetup(t, true)
	stopFastPolls(t)

	// Autopilot never stops on its own (D-18), so nothing but `sc stop`
	// can end this run.
	var childOut, childErr bytes.Buffer
	child := runChildCommand(t, env.Repo.Dir, "--headless", "--cartridge", "mock")
	child.Stdout, child.Stderr = &childOut, &childErr
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill() })
	waited := make(chan error, 1)
	rec := runChildAwaitLock(t, env.Repo.Dir, child.Process.Pid)
	go func() { waited <- child.Wait() }()

	stdout, stderr, code := runExec(t, "stop", env.Repo.Dir)
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, "stopping run "+rec.Run.String())
	require.Contains(t, stdout, "run "+rec.Run.String()+" stopped\n")

	select {
	case err := <-waited:
		signalled, how := runChildSignalled(child)
		require.Falsef(t, signalled, "the run was killed (%s) rather than stopped:\n%s", how, runPipeStderrTail(childErr.String()))
		require.NoErrorf(t, err, "a stopped run exits 0 (D-15):\n%s", runPipeStderrTail(childErr.String()))
	case <-time.After(runFireTimeout):
		t.Fatalf("the run did not end after sc stop:\n%s", runPipeStderrTail(childErr.String()))
	}

	require.Equal(t, orchestrator.LockFree,
		orchestrator.InspectLock(config.StateDir(env.Repo.Dir), orchestrator.LockOptions{}).State,
		"the stopped run left its lock behind")
	sum, err := orchestrator.SummariseLog(config.RunDir(env.Repo.Dir, rec.Run.String()), -1)
	require.NoError(t, err)
	require.Equal(t, orchestrator.PhaseFinished, sum.Phase)
	require.Contains(t, childOut.String(), "run stopping", "the run reported the interrupt it was sent")
}

// TestStopSendsOneInterruptAndTwoWithNow: the run's own interrupt handling
// is what makes --now a kill (the second interrupt, D-18), so `sc stop`
// sends signals and nothing else.
func TestStopSendsOneInterruptAndTwoWithNow(t *testing.T) {
	_, repo := runTestEnv(t)
	stopFastPolls(t)
	pid := stopSleeper(t, false)
	stopPlantLock(t, repo, orchestrator.LockRecord{LockHolder: orchestrator.LockHolder{
		Run: "20260101-000000-aaaa", PID: pid, Host: stopHostname(t), TakenAt: time.Now().UTC(),
	}})

	sent := stopStubSignal(t)
	stdout, stderr, code := runExec(t, "stop", repo, "--wait", "0")
	require.Equal(t, 0, code, stderr)
	require.Equal(t, []os.Signal{os.Interrupt}, *sent)
	require.Contains(t, stdout, "run 20260101-000000-aaaa was asked to stop")

	*sent = nil
	_, stderr, code = runExec(t, "stop", repo, "--now", "--wait", "0")
	require.Equal(t, 0, code, stderr)
	require.Equal(t, []os.Signal{os.Interrupt, os.Interrupt}, *sent,
		"--now is the second interrupt the run escalates on")
}

// TestStopWaitsForTheHolderToGo: the wait watches the process as well as
// the lock file, because a run killed while it was stopping leaves the file
// behind and that is still a stopped run.
func TestStopWaitsForTheHolderToGo(t *testing.T) {
	_, repo := runTestEnv(t)
	stopFastPolls(t)
	// A holder that dies on the interrupt but never removes its lock.
	pid := stopSleeper(t, false)
	lock := stopPlantLock(t, repo, orchestrator.LockRecord{LockHolder: orchestrator.LockHolder{
		Run: "20260101-000000-aaaa", PID: pid, Host: stopHostname(t), TakenAt: time.Now().UTC(),
	}})

	stdout, stderr, code := runExec(t, "stop", repo, "--wait", "30s")
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, "run 20260101-000000-aaaa stopped\n")
	require.FileExists(t, lock, "sc stop never writes or removes the lock; the run owns it")
}

// TestStopReportsARunThatWillNotStop: a holder that ignores the interrupt
// is reported rather than waited on for ever, and the message says what to
// do about it.
func TestStopReportsARunThatWillNotStop(t *testing.T) {
	_, repo := runTestEnv(t)
	stopFastPolls(t)
	pid := stopSleeper(t, true)
	stopPlantLock(t, repo, orchestrator.LockRecord{LockHolder: orchestrator.LockHolder{
		Run: "20260101-000000-aaaa", PID: pid, Host: stopHostname(t), TakenAt: time.Now().UTC(),
	}})

	_, stderr, code := runExec(t, "stop", repo, "--wait", "50ms")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "sc: run 20260101-000000-aaaa did not stop within 50ms")
	require.Contains(t, stderr, "it is still going (pid ")
}

// TestStopWithNoRunGoing: the commonest case of all, and it names where it
// looked.
func TestStopWithNoRunGoing(t *testing.T) {
	_, repo := runTestEnv(t)
	stdout, stderr, code := runExec(t, "stop", repo)
	require.Equal(t, 1, code)
	require.Empty(t, stdout)
	require.Equal(t, "sc: no run is going in "+repo+": nothing holds "+
		orchestrator.RepoLockPath(config.StateDir(repo))+"\n", stderr)
}

// TestStopRefusesALockItMustNotSignal covers the three locks that name a
// process this machine may not, or need not, interrupt.
func TestStopRefusesALockItMustNotSignal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lock  func(t *testing.T, repo string)
		wants []string
	}{
		{
			name: "the holder is gone",
			lock: func(t *testing.T, repo string) {
				// A pid nothing can be running under.
				stopPlantLock(t, repo, orchestrator.LockRecord{LockHolder: orchestrator.LockHolder{
					Run: "20260101-000000-aaaa", PID: 0x7FFFFFFE, Host: stopHostname(t), TakenAt: time.Now().UTC(),
				}})
			},
			wants: []string{"run 20260101-000000-aaaa is not going any more", "nothing to stop"},
		},
		{
			name: "the run is on another machine",
			lock: func(t *testing.T, repo string) {
				stopPlantLock(t, repo, orchestrator.LockRecord{LockHolder: orchestrator.LockHolder{
					Run: "20260101-000000-bbbb", PID: 4242, Host: "another-machine", TakenAt: time.Now().UTC(),
				}})
			},
			wants: []string{"is going on another-machine", "which this machine cannot signal"},
		},
		{
			name: "the lock cannot be read",
			lock: func(t *testing.T, repo string) {
				path := orchestrator.RepoLockPath(config.StateDir(repo))
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
			},
			wants: []string{"cannot be read", "so the run holding it cannot be named"},
		},
		{
			name: "the lock is this very process",
			lock: func(t *testing.T, repo string) {
				stopPlantLock(t, repo, orchestrator.LockRecord{LockHolder: orchestrator.LockHolder{
					Run: "20260101-000000-cccc", PID: os.Getpid(), Host: stopHostname(t), TakenAt: time.Now().UTC(),
				}})
			},
			wants: []string{"is this very process", "so it cannot stop itself"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, repo := runTestEnv(t)
			tc.lock(t, repo)
			sent := stopStubSignal(t)

			_, stderr, code := runExec(t, "stop", repo)
			require.Equal(t, 1, code)
			for _, want := range tc.wants {
				require.Contains(t, stderr, want)
			}
			require.Empty(t, *sent, "nothing may be signalled on this path")
		})
	}
}

// TestStopJSONReportsWhatItDid: --json is the scriptable form, and it says
// which run was interrupted and whether it had ended by the time the wait
// was over.
func TestStopJSONReportsWhatItDid(t *testing.T) {
	_, repo := runTestEnv(t)
	stopFastPolls(t)
	pid := stopSleeper(t, false)
	stopPlantLock(t, repo, orchestrator.LockRecord{LockHolder: orchestrator.LockHolder{
		Run: "20260101-000000-aaaa", PID: pid, Host: stopHostname(t), TakenAt: time.Now().UTC(),
	}})

	stdout, stderr, code := runExec(t, "--json", "stop", repo, "--wait", "30s")
	require.Equal(t, 0, code, stderr)
	var report StopReport
	require.NoError(t, json.Unmarshal([]byte(stdout), &report), stdout)
	require.Equal(t, domain.RunID("20260101-000000-aaaa"), report.Run)
	require.Equal(t, repo, report.Repo)
	require.Equal(t, pid, report.PID)
	require.Equal(t, 1, report.Interrupts)
	require.True(t, report.Stopped)
	require.False(t, strings.Contains(stdout, "stopping run "), "--json prints one document and nothing else")
}

// TestStopTakesTheRepoFromTheFlagOrThePath keeps the argument shape of the
// other repository commands.
func TestStopTakesTheRepoFromTheFlagOrThePath(t *testing.T) {
	_, repo := runTestEnv(t)
	byPath, _, code := runExec(t, "stop", repo)
	require.Equal(t, 1, code)
	byFlag, _, code := runExec(t, "stop", "--repo", repo)
	require.Equal(t, 1, code)
	require.Equal(t, byPath, byFlag)

	// A path that is not a repository is the run form's own refusal.
	_, stderr, code := runExec(t, "stop", filepath.Join(repo, "..", "nowhere"))
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "no such directory")
}
