package orchestrator

// Crash recovery (P9-03).
//
// The Phase 6 exit criterion (TestE2EKillAndResume) stops a run the way a
// second Ctrl-C stops it — in this process, through Run.Kill. That is not a
// crash: the process that held the leases is still alive, the run lock is
// still this pid's, and every goroutine gets to unwind. This file is the
// crash proper. It starts a *real second process* — this very test binary,
// re-executed with SC_RECOVERY_CHILD set, running the whole e2e rig against
// the same fixture repository, the same fake tracker and the same run
// directory — waits until the run reaches a chosen point mid-round, and
// sends it SIGKILL. Nothing in that process gets to run: no lease release,
// no worktree cleanup, no lock release, no RunFinished.
//
// A third-party observer (this test) then resumes the run in-process and
// checks the four things P9-03 asks for:
//
//   - stale leases recovered — beads.RecoverStale returns the dead
//     process's claim to the ready set (P3-05), and the run lock the dead
//     pid left behind is taken over rather than obeyed;
//   - worktrees reattached — the resume's ResumeEntry for the killed bead
//     reports gitx.WorktreeAttached, not "recreated" or "missing" (P4-08);
//   - agents that were mid-flight re-fired — the resumed process fires an
//     engineer for the bead the killed process was holding, and the run
//     finishes both beads;
//   - delivered beads not re-delivered — a bead whose pull request was
//     opened and whose gate closed before the crash gets no second pull
//     request and no second delivery, checked against the branch and pull
//     request state the gh shim recorded across both processes.
//
// Every helper here is prefixed "rec" so it cannot collide with e2e_test.go
// (whose rig it reuses) or with any other file in the package.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/cartridge"
	"slop-cannon/internal/cartridge/mock"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/testutil"
)

// The child process is configured entirely through the environment, because
// it is the same test binary and cannot be given arguments of its own.
const (
	// recModeEnv names the kill point the child must reach; it is also the
	// flag that turns TestRecoveryCrashChild from a skip into a run.
	recModeEnv = "SC_RECOVERY_CHILD"
	// recSafetyEnv is "1" when the child runs with safety on.
	recSafetyEnv = "SC_RECOVERY_SAFETY"
	// recRepoEnv, recRemoteEnv name the fixture repository the parent made.
	recRepoEnv   = "SC_RECOVERY_REPO"
	recRemoteEnv = "SC_RECOVERY_REMOTE"
	// recBDEnv, recGHEnv name the shim directories, so the child can read
	// their invocation logs the way the rig does.
	recBDEnv = "SC_RECOVERY_BD_DIR"
	recGHEnv = "SC_RECOVERY_GH_DIR"
	// recSignalEnv is the file the child writes when it reaches the kill
	// point; the parent kills it the moment the file appears.
	recSignalEnv = "SC_RECOVERY_SIGNAL"
)

// The kill points. Both are states of the *run*, not of the child process,
// so the parent never has to guess how far the child got.
const (
	// recModeInFlight kills the run while an engineer holds a bead: the
	// bead is in_progress with a lease and its worktree exists.
	recModeInFlight = "in-flight"
	// recModeDelivered kills the run after a bead has been shipped: its
	// pull request was opened and its gate closed, so the bead is closed
	// and must never be delivered again.
	recModeDelivered = "delivered"
)

const (
	// recKillPointBudget bounds how long the child may take to reach its
	// kill point. Like e2eTimeout it is a deadlock bound, not a
	// performance budget.
	recKillPointBudget = 3 * time.Minute
	// recChildLife bounds a child the parent never manages to kill, so a
	// broken handshake fails instead of hanging the package.
	recChildLife = 5 * time.Minute
	// recPoll is how often both sides look at the world.
	recPoll = 10 * time.Millisecond
	// recQuietFor is how long the fake tracker must record no call before
	// the parent believes the killed process's subprocesses are gone.
	recQuietFor = 300 * time.Millisecond
	// recQuietBudget bounds that wait.
	recQuietBudget = 30 * time.Second
)

// recSignal is the handshake the child writes at its kill point. It is
// everything the parent needs to describe the crash it is about to cause
// without reaching into the dead process.
type recSignal struct {
	// PID is the child's process id; the parent checks the run lock the
	// crash leaves behind names it.
	PID int `json:"pid"`
	// Run and RunDir locate the run the child started (D-23).
	Run    domain.RunID `json:"run"`
	RunDir string       `json:"run_dir"`
	// Epic is the run's epic bead (D-22).
	Epic domain.BeadID `json:"epic"`
	// Bead is the bead the kill point is about: the one an engineer holds
	// (recModeInFlight) or the one that was delivered (recModeDelivered).
	Bead     domain.BeadID    `json:"bead"`
	State    domain.BeadState `json:"state"`
	Assignee string           `json:"assignee"`
	Branch   string           `json:"branch"`
	Worktree string           `json:"worktree"`
	// States is every work bead's state at the kill point.
	States map[domain.BeadID]domain.BeadState `json:"states"`
	// Agents describes the agents the scheduler had live.
	Agents []string `json:"agents"`
	// PRCreates is how many pull requests had been opened.
	PRCreates int `json:"pr_creates"`
}

// ---------------------------------------------------------------------------
// The parent: cause the crash, then resume the run
// ---------------------------------------------------------------------------

// TestRecoveryKillNineAndResume is P9-03: a run killed with SIGKILL as a
// real child process, then resumed.
func TestRecoveryKillNineAndResume(t *testing.T) {
	t.Run("an engineer killed mid round comes back", func(t *testing.T) {
		ctx := context.Background()
		crash := recCrash(t, recModeInFlight, false)
		held := crash.Sig.Bead

		// What the crash left behind: a run that never finished, a lease
		// still held by a process that no longer exists, that process's
		// run lock, and the worktree it was working in.
		assert.Equal(t, PhaseRunning, crash.State.Phase, "the killed run never reached a finished state")
		assert.Empty(t, crash.State.Finish, "and never recorded an outcome")
		assert.Zero(t, crash.State.Resumes)
		assert.Equal(t, crash.Sig.PID, crash.Lock.PID, "the run lock names the dead process")
		assert.Equal(t, crash.Sig.Run, crash.Lock.Run)
		assert.Equal(t, domain.BeadStateInProgress, crash.Killed[held], "%s is still claimed", held)
		assert.NotEmpty(t, crash.Sig.Assignee, "%s is still assigned to the dead engineer", held)
		assert.DirExists(t, crash.Sig.Worktree, "the killed run's worktree is still there")
		assert.Contains(t, recWorktreeList(t, crash), recResolve(crash.Sig.Worktree),
			"and is still registered with git")
		// The scheduler's live-agent list is a snapshot of a moving target
		// — the engineer's session may have ended a millisecond before the
		// signal was written, with the bead still in_progress because its
		// report had not been posted yet — so it is reported, not asserted.
		// What must be true is asserted below: the bead was still claimed,
		// and the resumed run fires an engineer for it again.
		t.Logf("agents live when the process died: %v", crash.Sig.Agents)

		// --- the resume ---------------------------------------------------
		rig := e2eBuild(t, crash.Env, false, true)
		require.Equal(t, crash.Sig.Run, rig.RC.Run.ID, "the resume found the killed run")
		assert.Equal(t, crash.Sig.Epic, rig.Run.Epic(), "and its epic (D-22)")

		// P3-05: the dead process's lease was recovered, not waited out.
		assert.Contains(t, rig.Recovered.IDs(), held, "the stale lease on %s was recovered", held)

		rig.start(ctx)
		out, err := rig.wait(e2eTimeout)
		require.NoError(t, err, "problems: %v", rig.Problems())
		assert.Empty(t, rig.Problems(), "the conductor hit no problem")
		assert.Equal(t, events.FinishCompleted, out.Reason)

		// P4-08: the worktree the crash left was reattached, not recreated,
		// so the engineer's unfinished work was still there to build on.
		entry := recResumeEntry(t, rig, held)
		assert.Equal(t, gitx.WorktreeAttached, entry.WorktreeStatus,
			"%s: %s", held, rig.Run.ResumeResult().Summary())
		require.NotNil(t, entry.Worktree)
		assert.Equal(t, crash.Sig.Worktree, entry.Worktree.Path)
		assert.Empty(t, rig.Run.ResumeResult().Failed(), "every bead was reconciled")

		// The agent that was mid-flight was fired again, in this process.
		assert.NotEmpty(t, recEngineerFirings(t, rig, held),
			"the resumed run fired an engineer for %s", held)

		// And the run finished exactly as an uninterrupted one does.
		e2eAssertBothBeadsClosed(ctx, t, rig)
		for _, o := range rig.Outcomes() {
			assert.Equal(t, DeliveryMerged, o.Kind, "safety off merges")
		}
		for _, f := range e2eWorkFiles {
			assert.FileExistsf(t, filepath.Join(rig.Repo.Dir, f), "%s landed on the default branch", f)
		}
		assert.Equal(t, rig.Repo.Head(t), e2eRemoteHead(t, rig), "the default branch was pushed")
		assert.Equal(t, 1, rig.Planner.Status().Plans, "the brief is planned once across both processes")
		assert.Zero(t, rig.Adapter.Uses("plan"), "the resumed run fired no product owner")
		recAssertResumedRun(t, rig, crash)
	})

	t.Run("a delivered bead is not delivered twice", func(t *testing.T) {
		ctx := context.Background()
		crash := recCrash(t, recModeDelivered, true)
		shipped := crash.Sig.Bead
		mainBefore := crash.Env.Repo.Head(t)

		assert.Equal(t, PhaseRunning, crash.State.Phase, "the killed run never reached a finished state")
		assert.Equal(t, domain.BeadStateClosed, crash.Killed[shipped],
			"%s was shipped before the crash", shipped)
		assert.Equal(t, 1, crash.Sig.PRCreates, "one pull request was open when the process died")
		require.NotEmpty(t, crash.Sig.Branch)
		assert.Equal(t, 1, recPRCreates(t, crash.Env.GH)[crash.Sig.Branch],
			"%s's pull request was opened once", shipped)

		// --- the resume ---------------------------------------------------
		rig := e2eBuild(t, crash.Env, true, true)
		require.Equal(t, crash.Sig.Run, rig.RC.Run.ID, "the resume found the killed run")

		rig.start(ctx)
		out, err := rig.wait(e2eTimeout)
		require.NoError(t, err, "problems: %v", rig.Problems())
		assert.Empty(t, rig.Problems(), "the conductor hit no problem")
		assert.Equal(t, events.FinishCompleted, out.Reason)

		// The shipped bead was left alone: the resume saw a closed bead,
		// the conductor never offered it for delivery again, and gh was
		// never asked to open a second pull request for its branch.
		entry := recResumeEntry(t, rig, shipped)
		assert.False(t, entry.Changed(), "%s was already closed: %s", shipped, entry.Reason)
		assert.Equal(t, domain.BeadStateClosed, entry.State)
		assert.NotContains(t, rig.Outcomes(), shipped, "the resumed run did not deliver %s again", shipped)

		heads := recPRCreates(t, crash.Env.GH)
		assert.Equal(t, 1, heads[crash.Sig.Branch], "%s still has exactly one pull request", shipped)
		assert.Len(t, heads, 2, "one pull request per bead across both processes (got %v)", heads)
		for head, n := range heads {
			assert.Equalf(t, 1, n, "%s was opened once", head)
			assert.Truef(t, strings.HasPrefix(head, gitx.BranchPrefix), "%s is an sc/ branch", head)
		}

		// Both beads ended closed, behind resolved gates, and safety on
		// never moved the default branch in either process (D-10, D-20).
		states := rig.States(ctx)
		require.Len(t, states, 2)
		for id, st := range states {
			assert.Equalf(t, domain.BeadStateClosed, st, "%s is closed", id)
		}
		gates, err := rig.Beads.Gates(ctx, true)
		require.NoError(t, err)
		require.Len(t, gates, 2, "one gh:pr gate per bead (D-20)")
		for _, g := range gates {
			assert.Falsef(t, g.IsOpen(), "gate %s was resolved", g.ID)
		}
		assert.Equal(t, mainBefore, rig.Repo.Head(t), "the local default branch never moved (D-10)")
		assert.Equal(t, mainBefore, e2eRemoteHead(t, rig), "nor did the remote one (D-10)")
		assert.Zero(t, rig.Adapter.Uses("plan"), "the resumed run fired no product owner")
		recAssertResumedRun(t, rig, crash)
	})
}

// recAssertResumedRun checks what is true of every resumed run: state.json
// counted the resume and closed the run, and the run log is one file across
// both processes, with the second RunStarted flagged as a resume (P6-01,
// P6-08).
func recAssertResumedRun(t *testing.T, rig *e2eRig, crash *recCrashResult) {
	t.Helper()
	st, err := LoadState(rig.RC.Run.StateDir)
	require.NoError(t, err)
	assert.Equal(t, 1, st.Resumes, "state.json counted the resume")
	assert.Equal(t, PhaseFinished, st.Phase)
	assert.Equal(t, events.FinishCompleted, st.Finish)
	assert.Empty(t, st.Agents, "no agent survived the run")
	assert.Equal(t, crash.Sig.Run, st.Run.ID)

	// The run log survived the crash: the killed process's lines are still
	// there, the resumed process appended to the same file, and a line cut
	// short mid-write is tolerated rather than fatal (P8-18).
	evs, err := ReadEventLog(rig.RC.Run.StateDir)
	require.NoError(t, err, "the crashed run's log is still readable")
	require.NotEmpty(t, evs)
	assert.Equal(t, events.KindRunStarted, evs[0].Kind(), "the log still opens with RunStarted")
	assert.Equal(t, events.KindRunFinished, evs[len(evs)-1].Kind(), "and closes with RunFinished")
	var started int
	for _, ev := range evs {
		if rs, ok := ev.(events.RunStarted); ok {
			started++
			if started == 2 {
				assert.True(t, rs.Resumed, "the second RunStarted says it is a resume")
			}
		}
	}
	assert.Equal(t, 2, started, "both processes appended to the same run log")

	summary := Summarise(evs, -1)
	assert.Equal(t, PhaseFinished, summary.Phase)
	assert.Equal(t, events.FinishCompleted, summary.Finish)
	assert.LessOrEqual(t, summary.SkippedLines, 1,
		"at most the one line the kill could have cut short was skipped")
}

// recResumeEntry returns the resume's entry for one bead.
func recResumeEntry(t *testing.T, rig *e2eRig, id domain.BeadID) gitx.ResumeEntry {
	t.Helper()
	res := rig.Run.ResumeResult()
	require.NotNil(t, res, "the resumed run reconciled its worktrees")
	for _, e := range res.Entries {
		if e.Bead.ID == id {
			return e
		}
	}
	t.Fatalf("the resume has no entry for %s:\n%s", id, res.Summary())
	return gitx.ResumeEntry{}
}

// recEngineerFirings returns the engineer firings of one bead in rig's
// process, read from the mock cartridge's call log.
func recEngineerFirings(t *testing.T, rig *e2eRig, id domain.BeadID) []mock.Call {
	t.Helper()
	var out []mock.Call
	for _, c := range rig.Adapter.CallsMatching(mock.Match{Role: string(domain.RoleEngineer)}) {
		if c.Spec.Labels.Get(cartridge.LabelBead) == string(id) {
			out = append(out, c)
		}
	}
	return out
}

// recPRCreates counts the `gh pr create` calls of both processes by head
// branch. The gh shim's log is a file in the parent's temporary directory,
// so it records what the killed process did as well as what this one did.
func recPRCreates(t *testing.T, gh *testutil.Shim) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, inv := range gh.Invocations(t) {
		if len(inv.Args) < 2 || inv.Args[0] != "pr" || inv.Args[1] != "create" {
			continue
		}
		out[e2eFlagValue(inv.Args, "--head")]++
	}
	return out
}

// recWorktreeList returns the worktree paths git has registered in the
// fixture repository, with symbolic links resolved: git prints the real
// path, and a temporary directory is behind a link on some systems.
func recWorktreeList(t *testing.T, crash *recCrashResult) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(crash.Env.Repo.Git(t, "worktree", "list", "--porcelain"), "\n") {
		if path, ok := strings.CutPrefix(strings.TrimSpace(line), "worktree "); ok {
			out = append(out, recResolve(path))
		}
	}
	return out
}

// recResolve resolves symbolic links in path, leaving it alone when it
// cannot.
func recResolve(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}

// ---------------------------------------------------------------------------
// Causing the crash
// ---------------------------------------------------------------------------

// recCrashResult is everything the parent knows about the crash it caused.
type recCrashResult struct {
	// Env is the machine both processes share: the fixture repository, the
	// fake tracker, the fake gh and the state directory.
	Env *e2eEnv
	// Sig is the handshake the child wrote at its kill point.
	Sig recSignal
	// State is state.json as the killed process left it.
	State State
	// Lock is the run lock the killed process left behind.
	Lock LockHolder
	// Killed is every work bead's state as read back from the tracker
	// after the process died.
	Killed map[domain.BeadID]domain.BeadState
}

// recCrash sets up a machine, runs the first invocation of the run as a real
// child process, waits until that process reports it has reached mode's kill
// point, and kills it with SIGKILL. It returns once the tracker has gone
// quiet, so nothing the dead process started is still writing.
func recCrash(t *testing.T, mode string, safety bool) *recCrashResult {
	t.Helper()
	env := e2eNewEnv(t)

	sigPath := filepath.Join(t.TempDir(), "kill-point.json")
	logPath := filepath.Join(t.TempDir(), "child.log")
	logFile, err := os.Create(logPath) //nolint:gosec // a path this test just made up
	require.NoError(t, err)
	defer func() { _ = logFile.Close() }()

	cmd := exec.Command(os.Args[0], //nolint:gosec // this very test binary
		"-test.run", "^TestRecoveryCrashChild$", "-test.v",
		// Comfortably past the child's own guard, so a broken handshake is
		// reported by the test rather than by the testing package.
		"-test.timeout", (recChildLife + time.Minute).String())
	cmd.Env = recEnviron(map[string]string{
		recModeEnv:   mode,
		recSafetyEnv: recBoolEnv(safety),
		recRepoEnv:   env.Repo.Dir,
		recRemoteEnv: env.Repo.RemoteDir,
		recBDEnv:     env.BD.Dir,
		recGHEnv:     env.GH.Dir,
		recSignalEnv: sigPath,
	}, env.Repo.Env())
	cmd.Stdout, cmd.Stderr = logFile, logFile
	require.NoError(t, cmd.Start(), "starting the run that gets killed")

	// waitErr is written before exited is closed and read after it, so the
	// two sides never touch it at once.
	var waitErr error
	exited := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(exited) }()
	// However this test leaves, the child must not outlive it. Waiting on a
	// closed channel rather than a value means this can run after the main
	// path has already reaped the child.
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-exited
	})

	sig := recAwaitKillPoint(t, sigPath, logPath, exited)

	// SIGKILL: no signal handler, no deferred cleanup, no lease release.
	// Only this process is killed, not its process group: the git and bd
	// subprocesses it spawned are left to finish their writes, because
	// their databases are not crash-safe and half a `bd` write would be a
	// broken fixture rather than the crash this test is about.
	require.NoError(t, cmd.Process.Kill())
	<-exited
	var exit *exec.ExitError
	require.ErrorAsf(t, waitErr, &exit, "the child was killed, not exited (%v)\n%s", waitErr, recTail(t, logPath))
	assert.Equal(t, -1, exit.ExitCode(), "an exit code of -1 means a signal ended it")
	assert.Contains(t, exit.String(), "killed", "the child died of SIGKILL")
	recAwaitQuiet(t, env)

	out := &recCrashResult{Env: env, Sig: sig}
	out.State, err = LoadState(sig.RunDir)
	require.NoError(t, err, "the killed process left a state file to resume from (P6-11)")
	out.Lock, err = readLock(LockPath(sig.RunDir))
	require.NoError(t, err, "the killed process left its run lock behind")
	out.Killed = sig.States
	return out
}

// recAwaitKillPoint blocks until the child writes its handshake, and fails
// with the child's output if it dies or stalls first.
func recAwaitKillPoint(t *testing.T, sigPath, logPath string, exited <-chan struct{}) recSignal {
	t.Helper()
	deadline := time.After(recKillPointBudget)
	for {
		if sig, ok := recReadSignal(sigPath); ok {
			return sig
		}
		select {
		case <-exited:
			t.Fatalf("the run that should have been killed ended first\n%s", recTail(t, logPath))
		case <-deadline:
			t.Fatalf("the run did not reach its kill point in %s\n%s", recKillPointBudget, recTail(t, logPath))
		case <-time.After(recPoll):
		}
	}
}

// recAwaitQuiet waits until the fake tracker has recorded no call for
// recQuietFor. The killed process's `bd`, `git` and `gh` children outlive it
// by a few milliseconds; letting them finish before the resume starts keeps
// two processes from writing the fixture tracker's database at once.
func recAwaitQuiet(t *testing.T, env *e2eEnv) {
	t.Helper()
	logs := []string{env.BD.LogPath, env.GH.LogPath}
	deadline := time.Now().Add(recQuietBudget)
	last, since := recLogSizes(logs), time.Now()
	for {
		time.Sleep(recPoll)
		now := recLogSizes(logs)
		switch {
		case now != last:
			last, since = now, time.Now()
		case time.Since(since) >= recQuietFor:
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the killed run's subprocesses were still busy after %s", recQuietBudget)
		}
	}
}

// recLogSizes sums the sizes of the shim logs; a missing log counts as zero.
func recLogSizes(paths []string) int64 {
	var total int64
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil {
			total += info.Size()
		}
	}
	return total
}

// recReadSignal reads the handshake file, treating a half-written one as
// absent (the child writes it atomically, so this only guards a reader that
// arrives between create and rename).
func recReadSignal(path string) (recSignal, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // a path this test made up
	if err != nil {
		return recSignal{}, false
	}
	var sig recSignal
	if err := json.Unmarshal(data, &sig); err != nil {
		return recSignal{}, false
	}
	return sig, sig.Bead != ""
}

// recTail returns the last lines of the child's output, for a failure
// message.
func recTail(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a path this test made up
	if err != nil {
		return fmt.Sprintf("(no child output: %v)", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	const keep = 40
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	return "--- the killed run's output ---\n" + strings.Join(lines, "\n")
}

// recEnviron returns this process's environment with extra applied (later
// duplicates of the same name are removed, so the child never has to guess
// which one wins) and the fixture repository's git identity added, which is
// what gives the child's git the same isolated author as the parent's.
// HOME, PATH and XDG_CONFIG_HOME are not taken from the fixture: the child
// must keep the temporary home, the shim PATH and the state directory this
// test set up.
func recEnviron(extra map[string]string, gitEnv []string) []string {
	for _, kv := range gitEnv {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "GIT_") {
			continue
		}
		if _, taken := extra[name]; !taken {
			extra[name] = value
		}
	}
	out := make([]string, 0, len(os.Environ())+len(extra))
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok {
			if _, replaced := extra[name]; replaced {
				continue
			}
		}
		out = append(out, kv)
	}
	for name, value := range extra {
		out = append(out, name+"="+value)
	}
	return out
}

// recBoolEnv renders a flag for the child.
func recBoolEnv(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// ---------------------------------------------------------------------------
// The child: the run that gets killed
// ---------------------------------------------------------------------------

// TestRecoveryCrashChild is the run [TestRecoveryKillNineAndResume] kills.
// It is a test rather than a program of its own so that it can build the
// same rig the end-to-end scenario builds — the real planner, scheduler,
// supervisor, feedback loop, delivery handoff, gate poller and run — against
// the repository, tracker and state directory the parent set up.
//
// It skips unless the parent asked for it, so a plain `go test` runs it once
// as a no-op.
func TestRecoveryCrashChild(t *testing.T) {
	mode := os.Getenv(recModeEnv)
	if mode == "" {
		t.Skip("this test is the child process of TestRecoveryKillNineAndResume")
	}
	sigPath := os.Getenv(recSignalEnv)
	if sigPath == "" {
		t.Fatalf("%s is set but %s is not", recModeEnv, recSignalEnv)
	}

	ctx := context.Background()
	env := recAttach(t)
	rig := e2eBuild(t, env, os.Getenv(recSafetyEnv) == "1", false)
	rig.start(ctx)

	sig := recAwaitState(t, rig, mode)
	require.NoError(t, recWriteSignal(sigPath, sig))
	t.Logf("kill point %s reached: %+v", mode, sig)

	// Wait to be killed. Reaching either arm means the handshake is broken,
	// which the parent reports with this output attached.
	select {
	case <-rig.Run.Done():
		t.Fatalf("the run finished before the parent killed it\n%s", rig.diagnose())
	case <-time.After(recChildLife):
		t.Fatalf("the parent did not kill this process within %s", recChildLife)
	}
}

// recAttach rebuilds the parent's machine from the environment. Nothing is
// created here: the repository, the bare remote, the shims and the state
// directory all belong to the parent test and outlive this process, which is
// what lets the parent resume the run this process leaves half-finished.
func recAttach(t *testing.T) *e2eEnv {
	t.Helper()
	repo := os.Getenv(recRepoEnv)
	require.NotEmptyf(t, repo, "%s must name the fixture repository", recRepoEnv)
	require.DirExists(t, repo)
	return &e2eEnv{
		Home: os.Getenv("HOME"),
		Repo: &testutil.FixtureRepo{
			Dir:           repo,
			RemoteDir:     os.Getenv(recRemoteEnv),
			DefaultBranch: "main",
		},
		BD: recAttachShim("bd", os.Getenv(recBDEnv)),
		GH: recAttachShim("gh", os.Getenv(recGHEnv)),
	}
}

// recAttachShim describes a shim the parent installed. Its script is already
// on PATH, inherited; this only names the log so the rig can read it.
func recAttachShim(name, dir string) *testutil.Shim {
	return &testutil.Shim{
		Name:    name,
		Path:    filepath.Join(dir, name),
		Dir:     dir,
		LogPath: filepath.Join(dir, "invocations.log"),
	}
}

// recAwaitState polls the run until it reaches mode's kill point and
// describes it.
func recAwaitState(t *testing.T, rig *e2eRig, mode string) recSignal {
	t.Helper()
	deadline := time.Now().Add(recKillPointBudget)
	for {
		if sig, ok := recKillPoint(t, rig, mode); ok {
			return sig
		}
		select {
		case <-rig.Run.Done():
			t.Fatalf("the run finished before reaching the kill point %q\n%s", mode, rig.diagnose())
		case <-time.After(recPoll):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the kill point %q was not reached in %s\n%s", mode, recKillPointBudget, rig.diagnose())
		}
	}
}

// recKillPoint reports whether the run has reached mode's kill point, and
// describes the world at that moment.
func recKillPoint(t *testing.T, rig *e2eRig, mode string) (recSignal, bool) {
	t.Helper()
	ctx := context.Background()
	list, err := rig.beadSource(ctx)
	if err != nil {
		return recSignal{}, false
	}
	sig := recSignal{
		PID: os.Getpid(), Run: rig.RC.Run.ID, RunDir: rig.RC.Run.StateDir,
		Epic: rig.Run.Epic(), States: map[domain.BeadID]domain.BeadState{},
	}
	for _, b := range list {
		if b.Type != domain.BeadTypeEpic {
			sig.States[b.ID] = b.State
		}
	}
	for _, a := range rig.Sched.Status().Agents {
		sig.Agents = append(sig.Agents, fmt.Sprintf("%s %s/%s %s bead=%s", a.ID, a.Role, a.Lens, a.State, a.Bead))
	}
	sig.PRCreates = len(recPRCreates(t, rig.GH))

	for _, b := range list {
		if b.Type == domain.BeadTypeEpic {
			continue
		}
		switch mode {
		case recModeInFlight:
			// An engineer holds the bead and its worktree exists, so the
			// resume has both a lease and a worktree to repair.
			if b.State != domain.BeadStateInProgress || b.Assignee == "" {
				continue
			}
			wt := rig.Manager.PathFor(b.ID)
			if _, err := os.Stat(wt); err != nil {
				continue
			}
			sig.Bead, sig.State, sig.Assignee = b.ID, b.State, b.Assignee
			sig.Branch, sig.Worktree = gitx.BranchName(b), wt
			return sig, true
		case recModeDelivered:
			// The bead has been shipped: its pull request was opened and
			// its gate closed. Killing now means the resume must not ship
			// it a second time.
			if b.State != domain.BeadStateClosed || sig.PRCreates == 0 {
				continue
			}
			sig.Bead, sig.State, sig.Assignee = b.ID, b.State, b.Assignee
			sig.Branch = gitx.BranchName(b)
			sig.Worktree = rig.Manager.PathFor(b.ID)
			return sig, true
		default:
			t.Fatalf("unknown kill point %q", mode)
		}
	}
	return recSignal{}, false
}

// recWriteSignal writes the handshake atomically, so the parent never reads
// half of it and kills too early.
func recWriteSignal(path string, sig recSignal) error {
	data, err := json.MarshalIndent(sig, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the kill point: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing the kill point: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.Join(fmt.Errorf("publishing the kill point: %w", err), os.Remove(tmp))
	}
	return nil
}
