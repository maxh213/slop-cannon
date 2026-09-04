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
//     pid left behind is taken over rather than obeyed: the resumed run's
//     own pid is in the lock while it runs, and no lock survives it;
//   - worktrees reattached — the resume's ResumeEntry for the killed bead
//     reports gitx.WorktreeAttached, not "recreated" or "missing" (P4-08);
//   - agents that were mid-flight re-fired — the resumed process fires an
//     engineer for the bead the killed process was holding, and the run
//     finishes both beads;
//   - delivered beads not re-delivered — with safety on, a bead whose pull
//     request was opened and whose gate closed before the crash gets no
//     second pull request and no second delivery, checked against the
//     branch and pull request state the gh shim recorded across both
//     processes; with safety off, a bead whose fast-forward landed on the
//     default branch before the crash is closed from the branch state
//     alone, and no second delivery touches the default branch.
//
// Three of the kill points are the hard ones, and they are what the three
// resume hand-offs exist for (P6-15, P9-03):
//
//   - killed *inside* a pull request delivery, between `gh pr create` and
//     `bd gate create`: the bead is left `delivering` with its pull request
//     recorded and no gh:pr gate, which nothing polls and nothing can ever
//     close, so its dependents can never become ready (D-20). Only
//     Deliverer.RedeliverPR gets that bead moving again;
//   - killed *before* the delivery had anything to point at — safety off,
//     inside the repository's own check command, which the delivering
//     status is written well before: no pull request, no conflict, nothing
//     landed. Nothing else in the run ships a delivering bead (the
//     conductor offers only approved ones, the gate poller watches only
//     gh:pr gates), so without Deliverer.Redeliver that bead and its
//     dependents hang for as long as the run lives;
//   - killed with a worktree left mid-rebase: the bead owes its engineer a
//     conflict round before anything may be delivered from that worktree
//     (D-10), which is Deliverer.ConflictRound.
//
// All three hand-offs reach the deliverer through
// orchestrator.Options.OnResume, and through the very type `sc run` puts
// there: [DeliveryResumeHandler], built by [e2eBuild] exactly as
// [cli.assembleRun] builds it, over the P6-37 [ResumeTracker] guard that
// checks every bead-status write of a resume against the D-07 table. The
// wiring itself is pinned in internal/cli
// (TestRunWiresTheResumeHandoffsToTheDeliverer), because a run assembled
// without it reconciles the worktrees and then does nothing about what it
// found — which is what this file's rig would otherwise hide.
//
// Two things this file deliberately does not prove, so that its claims
// match what runs. The "pull request opened before the crash" subtest runs
// against recFakeGH, which reports every pull request MERGED — the state
// the gate poller needs to close a safety-on bead — so the resume's OPEN
// branch is not taken here; it is covered by unit tests in
// internal/gitx/cleanup_test.go. And the "delivered" pair covers a crash
// after the pull request (safety on) and after the fast-forward (safety
// off), but a delivered bead whose branch was deleted, or whose pull
// request was closed without merging, is again gitx's own test's.
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
	"strconv"
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
	// recGateHitEnv names the file the bd wrapper writes when it has
	// swallowed a `bd gate create` (recModeDelivering only). It is how the
	// child knows the delivery has reached the exact instant the crash must
	// land on.
	recGateHitEnv = "SC_RECOVERY_GATE_HIT"
	// recCloseHitEnv names the file the bd wrapper writes when it has
	// swallowed a `bd close` (recModeLanded only): the merge and the push
	// happened, and the close that would have ended the delivery never
	// reached the tracker.
	recCloseHitEnv = "SC_RECOVERY_CLOSE_HIT"
	// recCheckEnv replaces the rig's trivial check command with one that
	// blocks (recModeMerging only), so the crash lands inside a delivery
	// that has written `delivering` and has nothing to show for it yet.
	recCheckEnv = "SC_RECOVERY_CHECK"
	// recCheckHitEnv names the file that blocking check command creates the
	// moment it starts, which is how the child knows the delivery has
	// reached the kill point.
	recCheckHitEnv = "SC_RECOVERY_CHECK_HIT"
)

// The kill points. Every one of them is a state of the *run*, not of the
// child process, so the parent never has to guess how far the child got.
const (
	// recModeInFlight kills the run while an engineer holds a bead: the
	// bead is in_progress with a lease and its worktree exists.
	recModeInFlight = "in-flight"
	// recModeDelivered kills the run after a bead has been shipped: its
	// pull request was opened and its gate closed, so the bead is closed
	// and must never be delivered again.
	recModeDelivered = "delivered"
	// recModeMerging kills the run in the middle of a safety-off delivery,
	// before it has anything to point at: the bead is `delivering`, its
	// check command is blocking, and there is no pull request, no conflict
	// and nothing on the default branch. It is the widest crash window in
	// the product — the delivering status is written before the rebase, so
	// it covers the check command, which is minutes long in a real
	// repository — and the only thing that can end it is
	// Deliverer.Redeliver (P9-03).
	recModeMerging = "merging"
	// recModeLanded kills a safety-off run between the fast-forward and the
	// `bd close` that ends the delivery: the work is on the default branch
	// (locally and on the remote) and the bead is still `delivering`. The
	// resume must close it from the branch state alone and must not deliver
	// it again, because a second delivery of a landed branch is a git
	// operation on the default branch.
	recModeLanded = "landed"
	// recModeDelivering kills the run in the middle of the pull request
	// delivery: the pull request has been opened and recorded on the bead
	// and the gh:pr gate that watches it does not exist, because the bd
	// wrapper answered the gate creation without letting it reach the
	// tracker — which is what a kill between those two calls leaves behind
	// (P6-15, D-20). See [recGateBlock].
	recModeDelivering = "delivering"
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
	// PRNumber and PRURL are what the delivery had recorded on Bead when
	// the process died (beads.PRMetadata, written just before the gate).
	PRNumber int    `json:"pr_number"`
	PRURL    string `json:"pr_url"`
	// Gate is the open gh:pr gate blocking Bead at the kill point, empty
	// when none does. Empty with a pull request recorded is the state only
	// the P6-15 re-delivery can repair (D-20).
	Gate domain.BeadID `json:"gate"`
	// States is every work bead's state at the kill point.
	States map[domain.BeadID]domain.BeadState `json:"states"`
	// Agents describes the agents the scheduler had live.
	Agents []string `json:"agents"`
	// PRCreates is how many `gh pr create` calls had been made, counting
	// every call rather than every head branch: two creates for one head
	// are the duplicate delivery this test exists to catch, so they must
	// count twice.
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
		recAssertLockTakenOver(t, rig, crash)
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
		assert.Equal(t, 1, crash.Sig.PRCreates, "exactly one gh pr create had been made when the process died")
		require.NotEmpty(t, crash.Sig.Branch)
		assert.Equal(t, 1, recPRCreates(t, crash.Env.GH)[crash.Sig.Branch],
			"%s's pull request was opened once", shipped)

		// --- the resume ---------------------------------------------------
		rig := e2eBuild(t, crash.Env, true, true)
		require.Equal(t, crash.Sig.Run, rig.RC.Run.ID, "the resume found the killed run")

		rig.start(ctx)
		recAssertLockTakenOver(t, rig, crash)
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

	t.Run("a delivery interrupted before it had anything to show is finished", func(t *testing.T) {
		ctx := context.Background()
		crash := recCrash(t, recModeMerging, false)
		stuck := crash.Sig.Bead
		mainBefore := crash.Env.Repo.Head(t)

		// What the crash left behind is the state that hangs for ever with
		// safety off: the bead is `delivering` — written before the rebase,
		// the check and the merge — with no pull request to re-open, no
		// conflict to resolve and nothing on the default branch. Nothing
		// else in the run ships a delivering bead, and `delivering` is
		// neither terminal nor settled, so the run cannot finish and the
		// dependent can never become ready (D-20).
		assert.Equal(t, PhaseRunning, crash.State.Phase, "the killed run never reached a finished state")
		assert.Equal(t, domain.BeadStateDelivering, crash.Killed[stuck], "%s was mid-delivery", stuck)
		assert.Zero(t, crash.Sig.PRNumber, "safety off records no pull request")
		assert.Empty(t, crash.Sig.PRURL)
		assert.Empty(t, crash.Sig.Gate, "and no gate blocks it")
		assert.Zero(t, crash.Sig.PRCreates, "gh was never asked to open one")
		assert.DirExists(t, crash.Sig.Worktree, "the killed run's worktree is still there")
		require.NotNil(t, crash.Hold)
		assert.FileExists(t, crash.Hold.Hit, "the delivery really was inside the check command")

		// --- the resume ---------------------------------------------------
		rig := e2eBuild(t, crash.Env, false, true)
		require.Equal(t, crash.Sig.Run, rig.RC.Run.ID, "the resume found the killed run")
		require.NotNil(t, rig.Run.ResumeHandler(),
			"a run must be assembled with a resume handler or this state is unreachable (P6-15)")

		rig.start(ctx)
		recAssertLockTakenOver(t, rig, crash)

		// P9-03: gitx reconciled the bead to "the delivery did not
		// complete; deliver again" and left it delivering, and the run
		// handed it to Deliverer.Redeliver, which re-ran the idempotent
		// delivery from the top.
		entry := recResumeEntry(t, rig, stuck)
		assert.Equal(t, gitx.WorktreeAttached, entry.WorktreeStatus)
		assert.False(t, entry.Conflict, "the worktree is not mid-rebase: %s", entry.Reason)
		assert.Empty(t, entry.PRState, "and there is no pull request to point at")
		assert.False(t, entry.Landed, "nothing had landed when the process died")
		assert.Equal(t, domain.BeadStateDelivering, entry.State,
			"the resume leaves it delivering: %s", entry.Reason)
		assert.False(t, entry.Changed())
		assert.Contains(t, entry.Reason, "deliver again")

		out, err := rig.wait(e2eTimeout)
		require.NoError(t, err, "problems: %v", rig.Problems())
		assert.Empty(t, rig.Problems(), "the conductor hit no problem")
		assert.Equal(t, events.FinishCompleted, out.Reason,
			"the run that could not finish before now does")

		// The interrupted delivery finished as an uninterrupted one does,
		// and the dependent — which could only become ready once the stuck
		// bead closed (D-20) — went through behind it. Both beads are read
		// back from bd rather than from anything the run remembers.
		//
		// e2eAssertBothBeadsClosed is deliberately not used here: it also
		// asserts a review round per bead in this process, and the stuck
		// bead was reviewed and approved by the process that died.
		states := rig.States(ctx)
		require.Len(t, states, 2)
		for id, st := range states {
			assert.Equalf(t, domain.BeadStateClosed, st, "%s is closed", id)
		}
		for _, o := range rig.Outcomes() {
			assert.Equal(t, DeliveryMerged, o.Kind, "safety off merges")
		}
		for _, f := range e2eWorkFiles {
			assert.FileExistsf(t, filepath.Join(rig.Repo.Dir, f), "%s landed on the default branch", f)
			assert.Equalf(t, 1, recCommitsTouching(t, rig, f),
				"exactly one commit on the default branch adds %s across both processes", f)
		}
		assert.NotEqual(t, mainBefore, rig.Repo.Head(t), "the default branch moved on, once the run could")
		assert.True(t, recIsAncestor(t, rig, mainBefore, rig.Repo.Head(t)),
			"and only forwards: the resumed run built on what the crash left")
		assert.Equal(t, rig.Repo.Head(t), e2eRemoteHead(t, rig), "and was pushed")
		assert.Zero(t, rig.Adapter.Uses("plan"), "the resumed run fired no product owner")
		recAssertResumedRun(t, rig, crash)
	})

	t.Run("a fast forward that landed before the crash is closed not delivered again", func(t *testing.T) {
		ctx := context.Background()
		crash := recCrash(t, recModeLanded, false)
		shipped := crash.Sig.Bead
		mainAfterCrash := crash.Env.Repo.Head(t)

		// The safety-off half of "delivered beads are not re-delivered":
		// the fast-forward landed and was pushed, and the crash hit before
		// the close, so bd still says `delivering` while git says the work
		// is on the default branch. A second delivery here would be a git
		// operation on the default branch (D-10's whole point), so the
		// resume must close the bead from the branch state alone.
		assert.Equal(t, PhaseRunning, crash.State.Phase, "the killed run never reached a finished state")
		assert.Equal(t, shipped, crash.Gate.Swallowed(t), "the close of %s never reached bd", shipped)
		assert.Equal(t, domain.BeadStateDelivering, crash.Killed[shipped], "so bd still has it delivering")
		assert.Zero(t, crash.Sig.PRCreates, "safety off opens no pull request")
		assert.Equal(t, mainAfterCrash, recRevParse(t, crash.Env.Repo, crash.Sig.Branch),
			"the default branch was fast-forwarded to %s's tip", shipped)
		assert.Equal(t, mainAfterCrash, recRemoteHead(t, crash.Env.Repo),
			"and pushed, before the process died")

		// --- the resume ---------------------------------------------------
		rig := e2eBuild(t, crash.Env, false, true)
		require.Equal(t, crash.Sig.Run, rig.RC.Run.ID, "the resume found the killed run")

		rig.start(ctx)
		recAssertLockTakenOver(t, rig, crash)

		// The resume read the branch, not a guess: the bead is closed with
		// sc_merge_sha, and no delivery ran for it in this process.
		entry := recResumeEntry(t, rig, shipped)
		assert.True(t, entry.Landed, "the branch is on the default branch: %s", entry.Reason)
		assert.Equal(t, domain.BeadStateClosed, entry.State)
		assert.True(t, entry.Changed())
		assert.True(t, entry.Applied)
		assert.Equal(t, mainAfterCrash, entry.MergeSHA, "recorded as the commit that landed")
		assert.Contains(t, entry.Reason, "landed on main")

		out, err := rig.wait(e2eTimeout)
		require.NoError(t, err, "problems: %v", rig.Problems())
		assert.Empty(t, rig.Problems(), "the conductor hit no problem")
		assert.Equal(t, events.FinishCompleted, out.Reason)

		assert.NotContains(t, rig.Outcomes(), shipped, "the resumed run did not deliver %s again", shipped)
		// As in the subtest above, the states are read back from bd rather
		// than through e2eAssertBothBeadsClosed, which also expects a review
		// round per bead in this process: this bead's happened in the
		// process that died.
		states := rig.States(ctx)
		require.Len(t, states, 2)
		for id, st := range states {
			assert.Equalf(t, domain.BeadStateClosed, st, "%s is closed", id)
		}
		for _, f := range e2eWorkFiles {
			assert.Equalf(t, 1, recCommitsTouching(t, rig, f),
				"exactly one commit on the default branch adds %s across both processes", f)
		}
		// The default branch only ever moved forwards: what the crash left
		// on it is still an ancestor of where the resumed run left it.
		assert.True(t, recIsAncestor(t, rig, mainAfterCrash, rig.Repo.Head(t)),
			"the resumed run built on the crash's default branch instead of rewriting it")
		assert.Equal(t, rig.Repo.Head(t), e2eRemoteHead(t, rig), "and pushed it")
		assert.Zero(t, rig.Adapter.Uses("plan"), "the resumed run fired no product owner")
		recAssertResumedRun(t, rig, crash)
	})

	t.Run("a pull request opened before the crash is finished not opened again", func(t *testing.T) {
		ctx := context.Background()
		crash := recCrash(t, recModeDelivering, true)
		stuck := crash.Sig.Bead
		mainBefore := crash.Env.Repo.Head(t)

		// What the crash left behind is the state that hangs for ever: the
		// bead is delivering, its pull request is open and recorded on it,
		// and no gh:pr gate blocks it — so nothing polls anything, the bead
		// can never close and its dependent can never become ready (D-20).
		assert.Equal(t, PhaseRunning, crash.State.Phase, "the killed run never reached a finished state")
		assert.Equal(t, stuck, crash.Gate.Swallowed(t), "the gate creation of %s never reached bd", stuck)
		assert.Equal(t, domain.BeadStateDelivering, crash.Killed[stuck], "%s was mid-delivery", stuck)
		assert.Positive(t, crash.Sig.PRNumber, "its pull request was opened and recorded on the bead")
		assert.Contains(t, crash.Sig.PRURL, "/pull/", "with its url")
		assert.Empty(t, crash.Sig.Gate, "and no gh:pr gate was ever created for it")
		assert.Equal(t, 1, crash.Sig.PRCreates, "exactly one gh pr create had been made")
		assert.DirExists(t, crash.Sig.Worktree, "the killed run's worktree is still there")

		// --- the resume ---------------------------------------------------
		rig := e2eBuild(t, crash.Env, true, true)
		require.Equal(t, crash.Sig.Run, rig.RC.Run.ID, "the resume found the killed run")
		require.NotNil(t, rig.Run.ResumeHandler(),
			"a run must be assembled with a resume handler or this state is unreachable (P6-15)")

		rig.start(ctx)
		recAssertLockTakenOver(t, rig, crash)

		// P6-15: the resume found the pull request the dead process opened
		// and handed the entry to Deliverer.RedeliverPR, which re-ran the
		// idempotent delivery — so the gate the crashed delivery never
		// managed to create exists after all, and the bead can close.
		entry := recResumeEntry(t, rig, stuck)
		assert.Equal(t, "MERGED", entry.PRState, "gh knows the pull request: %s", entry.Reason)
		require.NotNil(t, entry.PR)
		assert.Equal(t, crash.Sig.PRNumber, entry.PR.Number)

		out, err := rig.wait(e2eTimeout)
		require.NoError(t, err, "problems: %v", rig.Problems())
		assert.Empty(t, rig.Problems(), "the conductor hit no problem")
		assert.Equal(t, events.FinishCompleted, out.Reason,
			"the run that could not finish before now does")

		// The re-delivery reused the pull request rather than opening a
		// second one: gh was asked for the branch's pull request, and there
		// is still exactly one create per head across both processes.
		assert.Contains(t, recGHListHeads(t, crash.Env.GH), crash.Sig.Branch,
			"the re-delivery looked the existing pull request up by head")
		heads := recPRCreates(t, crash.Env.GH)
		assert.Equal(t, 1, heads[crash.Sig.Branch], "%s still has exactly one pull request", stuck)
		assert.Len(t, heads, 2, "one pull request per bead across both processes (got %v)", heads)
		for head, n := range heads {
			assert.Equalf(t, 1, n, "%s was opened once", head)
			assert.Truef(t, strings.HasPrefix(head, gitx.BranchPrefix), "%s is an sc/ branch", head)
		}

		// Both beads ended closed behind a resolved gate — including the
		// dependent, which could only become ready once the stuck bead
		// closed (D-20) — and safety on never moved the default branch.
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

	t.Run("a worktree left mid rebase goes back to its engineer", func(t *testing.T) {
		ctx := context.Background()
		crash := recCrash(t, recModeInFlight, false)
		held := crash.Sig.Bead
		recLeaveMidRebase(t, crash)

		// --- the resume ---------------------------------------------------
		rig := e2eBuild(t, crash.Env, false, true)
		require.Equal(t, crash.Sig.Run, rig.RC.Run.ID, "the resume found the killed run")
		require.NotNil(t, rig.Run.ResumeHandler(),
			"a run must be assembled with a resume handler or this state is unreachable (P6-15)")

		rig.start(ctx)
		recAssertLockTakenOver(t, rig, crash)

		// The resume saw the conflict rather than a deliverable worktree,
		// and left the bead where it was: a mid-rebase worktree cannot be
		// delivered from, whatever else is true of the bead (D-10).
		entry := recResumeEntry(t, rig, held)
		assert.Equal(t, gitx.WorktreeAttached, entry.WorktreeStatus)
		require.True(t, entry.Conflict, "the worktree is mid-rebase: %s", entry.Reason)
		assert.Contains(t, entry.ConflictFiles, recConflictFile)
		assert.Contains(t, entry.Reason, "mid-rebase")

		// P6-15: the entry went to Deliverer.ConflictRound, so the bead owes
		// its engineer a conflict round and the scheduler fires one.
		pending, ok := rig.deliverer().PendingFor(held)
		require.True(t, ok, "%s has a conflict round pending: %s", held, entry.Reason)
		assert.Equal(t, held, pending.Bead.ID)
		assert.Contains(t, pending.Conflict.Files, recConflictFile,
			"the engineer is told which paths conflict (P5-12)")
		assert.Equal(t, crash.Env.Repo.DefaultBranch, pending.Conflict.Target,
			"and what the rebase was onto")
		require.Eventually(t, func() bool {
			return len(recEngineerFirings(t, rig, held)) > 0
		}, e2eTimeout, recPoll, "an engineer to be fired for the conflict on %s", held)

		// The run is stopped here on purpose: what happens after the
		// engineer is handed the conflict is P6-06's loop, not this test's,
		// and the mock cartridge resolves no rebase.
		require.NoError(t, rig.Run.Kill("the conflict round is what this scenario is about"))
		stopped, err := rig.wait(e2eTimeout)
		require.NoError(t, err)
		assert.Equal(t, events.FinishStopped, stopped.Reason)
		rig.stopHelpers()
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
	assert.NoFileExists(t, LockPath(rig.RC.Run.StateDir),
		"the resumed run released the lock it took over from the dead process")

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

// recDescribePR records what the delivery had written about bead's pull
// request when the process died: the number and url beads.PRMetadata put on
// the bead, and the open gh:pr gate blocking it, if any.
func recDescribePR(t *testing.T, rig *e2eRig, bead domain.Bead, sig *recSignal) {
	t.Helper()
	sig.PRNumber, sig.PRURL = bead.PRNumber, bead.PRURL
	if gate, err := rig.Beads.GateFor(context.Background(), bead.ID); err == nil {
		sig.Gate = gate.ID
	}
}

// recAssertLockTakenOver is the lock half of the crash recovery: the run
// lock the dead process left behind was taken over rather than obeyed
// (P6-01, P9-04). Start would have refused the run outright had it been
// obeyed, and while the resumed run is live the lock names *this* process.
//
// Call it as soon as the resumed run has started. A run that has already
// finished has released the lock, and a missing lock file then proves the
// same thing, so that case is allowed for explicitly rather than raced on.
func recAssertLockTakenOver(t *testing.T, rig *e2eRig, crash *recCrashResult) {
	t.Helper()
	require.NotEqual(t, os.Getpid(), crash.Lock.PID, "the crash's lock names the dead process")
	holder, err := readLock(LockPath(rig.RC.Run.StateDir))
	if err != nil {
		select {
		case <-rig.Run.Done():
			return // finished and released before this read; nothing to check.
		default:
			t.Fatalf("the resumed run holds no run lock: %v", err)
		}
	}
	assert.Equal(t, os.Getpid(), holder.PID, "the resumed run took the dead process's lock over")
	assert.Equal(t, crash.Sig.Run, holder.Run, "and it is the same run's lock")
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

// recCommitsTouching counts the commits on the default branch that touch
// path. Every bead of the scenario adds one file, so a bead delivered twice
// — the failure both "delivered" kill points exist to catch — shows up here
// as two commits for one file, whatever the second delivery called itself.
func recCommitsTouching(t *testing.T, rig *e2eRig, path string) int {
	t.Helper()
	out := rig.Repo.Git(t, "rev-list", "--count", rig.Repo.DefaultBranch, "--", path)
	n, err := strconv.Atoi(strings.TrimSpace(out))
	require.NoErrorf(t, err, "unexpected rev-list output %q", out)
	return n
}

// recIsAncestor reports whether one commit is an ancestor of another: it is
// how a test says the default branch only moved forwards.
func recIsAncestor(t *testing.T, rig *e2eRig, ancestor, descendant string) bool {
	t.Helper()
	_, err := recGitErr(rig.Repo.Env(), rig.Repo.Dir, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// recRevParse resolves a revision in the fixture repository.
func recRevParse(t *testing.T, repo *testutil.FixtureRepo, rev string) string {
	t.Helper()
	return strings.TrimSpace(repo.Git(t, "rev-parse", rev))
}

// recRemoteHead is [e2eRemoteHead] for a repository rather than a rig, so
// the state a crash left on the remote can be read before the resumed rig
// exists.
func recRemoteHead(t *testing.T, repo *testutil.FixtureRepo) string {
	t.Helper()
	fields := strings.Fields(repo.Git(t, "ls-remote", repo.RemoteDir, "refs/heads/"+repo.DefaultBranch))
	require.NotEmpty(t, fields, "the remote has a default branch")
	return fields[0]
}

// recConflictFile is the path the mid-rebase scenario conflicts on.
const recConflictFile = "shared-note.md"

// recLeaveMidRebase puts the killed run's worktree where a crash during a
// delivery rebase leaves it: mid-rebase, with unmerged paths in the index.
//
// The state is built rather than raced for. A delivery rebase conflicts only
// when two beads touch one file, which the end-to-end scenario's two beads
// never do, and the window between `git rebase` starting and stopping on a
// conflict is a few milliseconds wide, so a kill point aimed at it would be
// a flake generator. What the resume inspects is the same either way: git's
// own rebase state in the worktree the dead process left behind.
func recLeaveMidRebase(t *testing.T, crash *recCrashResult) {
	t.Helper()
	repo, wt := crash.Env.Repo, crash.Sig.Worktree
	env := repo.Env()

	// The engineer's unfinished work is committed first: a delivery commits
	// the leftovers before it rebases (P6-16), and git refuses to start a
	// rebase in a dirty worktree.
	require.NoError(t, os.WriteFile(filepath.Join(wt, recConflictFile), []byte("the bead's own line\n"), 0o600))
	recGit(t, env, wt, "add", "-A")
	recGit(t, env, wt, "commit", "-q", "-m", "the engineer's work")

	// Something else landed on the default branch while this run was down,
	// touching the same file — which is why the rebase conflicts.
	repo.WriteFile(t, recConflictFile, "the default branch's line\n")
	repo.Commit(t, "another change to the same file")

	// The rebase the dead process was running. It stops on the conflict and
	// leaves the worktree exactly as the crash would have.
	out, err := recGitErr(env, wt, "rebase", repo.DefaultBranch)
	require.Errorf(t, err, "the rebase must conflict, not succeed:\n%s", out)
	unmerged := recGit(t, env, wt, "diff", "--name-only", "--diff-filter=U")
	require.Containsf(t, unmerged, recConflictFile,
		"the worktree is left mid-rebase with unmerged paths (rebase said:\n%s)", out)
}

// recGit runs git in dir with the fixture's isolated identity and fails the
// test when it does not succeed.
func recGit(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	out, err := recGitErr(env, dir, args...)
	require.NoErrorf(t, err, "git %s in %s:\n%s", strings.Join(args, " "), dir, out)
	return out
}

// recGitErr runs git in dir and returns its combined output with the error,
// for the one call that is meant to fail.
func recGitErr(env []string, dir string, args ...string) (string, error) {
	bin, err := testutil.RealBinary("git")
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bin, args...) //nolint:gosec // the real git, with arguments this test wrote
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
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
	// Gate is the bd seam of a recModeDelivering (`bd gate create`) or
	// recModeLanded (`bd close`) crash, disarmed by the time the crash is
	// returned; nil at every other kill point.
	Gate *recGateBlock
	// Hold is the blocking check command of a recModeMerging crash,
	// released by the time the crash is returned; nil at every other kill
	// point.
	Hold *recCheckHold
}

// recCrash sets up a machine, runs the first invocation of the run as a real
// child process, waits until that process reports it has reached mode's kill
// point, and kills it with SIGKILL. It returns once the tracker has gone
// quiet, so nothing the dead process started is still writing.
func recCrash(t *testing.T, mode string, safety bool) *recCrashResult {
	t.Helper()
	env := e2eNewEnv(t)

	// A crash inside a delivery needs a seam the plain rig does not have,
	// installed in front of the rig's own shims (FakeBin prepends to PATH,
	// so the last one installed wins). Which seam depends on the window the
	// kill point is aimed at: a `gh` that remembers the pull requests it
	// opened, because a resumed run must be able to find the one the dead
	// process opened the way the real gh would, and a `bd` whose
	// `gate create` never reaches the tracker, which is what the dead
	// process's own gate creation did; a `bd` whose `close` never reaches
	// it, which is where a crash right after the fast-forward leaves
	// things; or a check command that blocks, which holds the delivery open
	// where a real repository's checks hold it.
	var (
		block *recGateBlock
		hold  *recCheckHold
	)
	switch mode {
	case recModeDelivering:
		env.GH = recFakeGH(t)
		block = recSwallowGateCreate(t, env.BD)
	case recModeLanded:
		// The fast-forward and the push happen; the close that would end
		// the delivery never reaches the tracker, which is what a crash in
		// that window leaves behind.
		block = recSwallowClose(t, env.BD)
	case recModeMerging:
		// A check command that blocks holds the delivery open at the point
		// a real repository's check (a test suite, minutes long) holds it.
		hold = recHoldingCheck(t)
	}

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
	childEnv := map[string]string{
		recModeEnv:   mode,
		recSafetyEnv: recBoolEnv(safety),
		recRepoEnv:   env.Repo.Dir,
		recRemoteEnv: env.Repo.RemoteDir,
		recBDEnv:     env.BD.Dir,
		recGHEnv:     env.GH.Dir,
		recSignalEnv: sigPath,
	}
	if block != nil {
		switch mode {
		case recModeLanded:
			childEnv[recCloseHitEnv] = block.Hit
		default:
			childEnv[recGateHitEnv] = block.Hit
		}
	}
	if hold != nil {
		childEnv[recCheckEnv], childEnv[recCheckHitEnv] = hold.Command, hold.Hit
	}
	cmd.Env = recEnviron(childEnv, env.Repo.Env())
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
	// The bd call the dead process meant to make is gone with it; the
	// resumed run's own calls must reach the tracker.
	if block != nil {
		block.disarm(t)
	}
	// And the check command the dead process left blocking is released, so
	// the orphaned shell it spawned exits instead of idling for the rest of
	// the package's run. The resumed rig has the rig's own trivial check
	// again: recCheckEnv is set on the child's environment alone.
	if hold != nil {
		hold.release(t)
	}
	recAwaitQuiet(t, env)

	out := &recCrashResult{Env: env, Sig: sig, Gate: block, Hold: hold}
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
// The shims a crash inside a delivery needs
// ---------------------------------------------------------------------------

// recGateBlock is the seam that leaves a delivery exactly where a crash
// between `gh pr create` and `bd gate create` leaves it: the pull request
// opened and recorded on the bead, the bead delivering, and no gh:pr gate
// in the tracker — which nothing polls and nothing can ever close (D-20).
//
// The wrapper *swallows* that one call rather than holding it: it answers
// with a gate bd never made and writes the bead's id to Hit. Holding the
// call would be closer to the wall clock of a real crash and is not worth
// it — beads.Client serialises every call through one mutex, so a held
// `bd gate create` freezes the whole tracker, including the child's own
// view of it, until the 30 s call timeout. What lands on disk is the same
// either way, and that is what the resume reads.
//
// Arm exists for as long as the swallowing applies. The parent removes it
// after the crash, so the resumed run's own gate creations reach bd.
type recGateBlock struct {
	// Shim is the bd wrapper; the rig's own bd shim runs behind it.
	Shim *testutil.Shim
	// Arm and Hit are the two files the wrapper reads and writes. Hit
	// holds the id of the bead whose gate was swallowed.
	Arm, Hit string
}

// recSwallowGateCreate installs that wrapper in front of inner.
func recSwallowGateCreate(t *testing.T, inner *testutil.Shim) *recGateBlock {
	t.Helper()
	dir := t.TempDir()
	b := &recGateBlock{
		Arm: filepath.Join(dir, "armed"),
		Hit: filepath.Join(dir, "swallowed-gate"),
	}
	require.NoError(t, os.WriteFile(b.Arm, nil, 0o600))
	// The command words are not $1 and $2: every call carries beads'
	// `-C <repo>` in front of them (D-21), so the pair is looked for
	// anywhere in the arguments, along with the two values the answer has
	// to echo back.
	script := fmt.Sprintf(`_gate=0
_blocks=''
_await=''
_prev=''
for _a in "$@"; do
  if [ "$_prev" = "gate" ] && [ "$_a" = "create" ]; then _gate=1; fi
  if [ "$_prev" = "--blocks" ]; then _blocks=$_a; fi
  if [ "$_prev" = "--await-id" ]; then _await=$_a; fi
  _prev=$_a
done
if [ "$_gate" = 1 ] && [ -f %[1]s ]; then
  printf '%%s
' "$_blocks" > %[2]s.tmp
  mv %[2]s.tmp %[2]s
  printf '{"schema_version":1,"data":{"id":"gate-the-crash-swallowed","title":"Gate: gh:pr %%s","status":"open","issue_type":"gate","await_type":"gh:pr","await_id":"%%s","description":"Ad-hoc gate blocking %%s"}}
' "$_await" "$_await" "$_blocks"
  exit 0
fi
exec %[3]s "$@"
`, e2eShellQuote(b.Arm), e2eShellQuote(b.Hit), e2eShellQuote(inner.Path))
	b.Shim = testutil.FakeBin(t, "bd", script)
	return b
}

// disarm stops the wrapper swallowing gate creations, so the resumed run's
// own gates reach the tracker.
func (b *recGateBlock) disarm(t *testing.T) {
	t.Helper()
	require.NoError(t, os.Remove(b.Arm))
}

// Swallowed reports the bead whose gate creation never reached bd, or "".
func (b *recGateBlock) Swallowed(t *testing.T) domain.BeadID {
	t.Helper()
	return recBlockedGateBead(b.Hit)
}

// recSwallowClose installs a bd wrapper that answers `bd close <id>`
// itself instead of letting it reach the tracker, and records the bead it
// swallowed. It is the seam of a safety-off crash between the fast-forward
// and the close (recModeLanded): the work is on the default branch and the
// bead is still `delivering`, which is exactly what the window between
// gitx's merge stage and its close leaves behind.
//
// Every close goes through this one call — the state machine's own
// transition to closed is [beads.Client.Close] as well — so nothing writes
// the status behind the wrapper's back. Like [recSwallowGateCreate] it
// swallows rather than holds: beads.Client serialises every call through
// one mutex, and a held close would freeze the child's own view of the
// tracker, including the kill-point poll.
func recSwallowClose(t *testing.T, inner *testutil.Shim) *recGateBlock {
	t.Helper()
	dir := t.TempDir()
	b := &recGateBlock{
		Arm: filepath.Join(dir, "armed"),
		Hit: filepath.Join(dir, "swallowed-close"),
	}
	require.NoError(t, os.WriteFile(b.Arm, nil, 0o600))
	// The command word is not $1: every call carries beads' `-C <repo>` in
	// front of it (D-21), so the bead id is the word after the first
	// "close" anywhere in the arguments.
	script := fmt.Sprintf(`_id=''
_prev=''
for _a in "$@"; do
  if [ -z "$_id" ] && [ "$_prev" = "close" ]; then _id=$_a; fi
  _prev=$_a
done
if [ -n "$_id" ] && [ -f %[1]s ]; then
  printf '%%s
' "$_id" > %[2]s.tmp
  mv %[2]s.tmp %[2]s
  printf '{"schema_version":1,"data":[{"id":"%%s","title":"the close the crash swallowed","status":"closed","issue_type":"task"}]}
' "$_id"
  exit 0
fi
exec %[3]s "$@"
`, e2eShellQuote(b.Arm), e2eShellQuote(b.Hit), e2eShellQuote(inner.Path))
	b.Shim = testutil.FakeBin(t, "bd", script)
	return b
}

// recCheckHold is the seam of a crash inside a safety-off delivery
// (recModeMerging): a check command that reports it has started and then
// blocks until it is released, holding the delivery open at the point a
// real repository's check holds it. The bead is `delivering` throughout,
// with no pull request, no conflict and nothing landed.
//
// The rig runs `true` as its check command, so this is passed to the child
// alone (recCheckEnv), and released after the kill so the shell the dead
// process spawned does not idle for the rest of the package's run.
type recCheckHold struct {
	// Command is the check command itself.
	Command string
	// Hit is the file it creates the moment it starts, and Arm the file it
	// waits for the disappearance of.
	Hit, Arm string
}

// recHoldingCheck builds that check command.
func recHoldingCheck(t *testing.T) *recCheckHold {
	t.Helper()
	dir := t.TempDir()
	h := &recCheckHold{
		Arm: filepath.Join(dir, "armed"),
		Hit: filepath.Join(dir, "check-started"),
	}
	require.NoError(t, os.WriteFile(h.Arm, nil, 0o600))
	// gitx runs the check with `sh -c` in the worktree, so both paths are
	// absolute and the command touches nothing of the repository's.
	h.Command = fmt.Sprintf(`: > %s; while [ -f %s ]; do sleep 0.02; done`,
		e2eShellQuote(h.Hit), e2eShellQuote(h.Arm))
	return h
}

// release lets the held check command finish.
func (h *recCheckHold) release(t *testing.T) {
	t.Helper()
	require.NoError(t, os.Remove(h.Arm))
}

// recFileExists reports whether path is there, treating an empty path (a
// seam this kill point does not use) as absent.
func recFileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// recBlockedGateBead reads the bead id out of a [recGateBlock] hit file,
// answering "" when the file is not there (nothing has been swallowed yet)
// or when no path was given at all (this kill point does not use the
// wrapper).
func recBlockedGateBead(path string) domain.BeadID {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path) //nolint:gosec // a path this test made up
	if err != nil {
		return ""
	}
	return domain.BeadID(strings.TrimSpace(string(data)))
}

// recFakeGH is [e2eFakeGH] with the one thing a crash-and-resume test needs
// that the plain shim does not have: a memory (P8-26). The plain shim
// answers every `gh pr list` with "[]", so a resumed run that legitimately
// re-delivers a bead whose pull request is already open would open a second
// one — a failure of the shim rather than of the product. This one keeps a
// head branch → number registry, answers `gh pr list --head` from it and
// `gh pr view <n>` by number, so the resumed run finds the pull request the
// dead process opened, exactly as the real gh would.
//
// Every pull request reports MERGED, which is what the gate poller (and
// `bd gate check`, which shells out to `gh pr view <n> --json state`) needs
// to see to close a safety-on bead (D-20).
func recFakeGH(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, "gh", `
_head=''
_prev=''
for _a in "$@"; do
  if [ "$_prev" = "--head" ]; then _head=$_a; fi
  _prev=$_a
done
_key=$(printf '%s' "$_head" | tr '/' '_')
_pr() {
  printf '{"number":%s,"url":"`+e2ePRHost+`/pull/%s","state":"MERGED","title":"Pull request %s","headRefName":"%s"}' "$1" "$1" "$1" "$2"
}
case "$1 $2" in
  "label create")
    ;;
  "pr list")
    if [ -n "$_head" ] && [ -f "$SC_FAKEBIN_DIR/head.$_key" ]; then
      _n=$(cat "$SC_FAKEBIN_DIR/head.$_key")
      printf '['; _pr "$_n" "$_head"; printf ']\n'
    else
      echo '[]'
    fi
    ;;
  "pr create")
    cat > "$SC_FAKEBIN_DIR/body.$SC_FAKEBIN_SEQ"
    w=0
    while ! mkdir "$SC_FAKEBIN_DIR/.prlock" 2>/dev/null; do
      # A shim killed while it held the lock would otherwise hang every
      # later pull request for ever; steal a lock nobody released within
      # five seconds. Numbering takes microseconds, so a live holder never
      # reaches that.
      if [ "$w" -ge 500 ]; then rmdir "$SC_FAKEBIN_DIR/.prlock" 2>/dev/null; w=0; continue; fi
      w=$((w + 1))
      sleep 0.01
    done
    n=$(cat "$SC_FAKEBIN_DIR/prcount" 2>/dev/null || echo 0)
    n=$((n + 1))
    echo "$n" > "$SC_FAKEBIN_DIR/prcount"
    echo "$n" > "$SC_FAKEBIN_DIR/head.$_key"
    echo "$_head" > "$SC_FAKEBIN_DIR/num.$n"
    rmdir "$SC_FAKEBIN_DIR/.prlock"
    echo "Creating pull request in acme/widgets"
    echo "`+e2ePRHost+`/pull/$n"
    ;;
  "pr view")
    _n=$3
    _h=$(cat "$SC_FAKEBIN_DIR/num.$_n" 2>/dev/null || echo '')
    _pr "$_n" "$_h"; printf '\n'
    ;;
  *)
    echo "gh shim: unexpected command: $*" >&2
    exit 1
    ;;
esac
`)
}

// recGHListHeads returns the --head of every `gh pr list` the shims
// recorded: it is how a test says the re-delivery looked the existing pull
// request up instead of opening another one.
func recGHListHeads(t *testing.T, gh *testutil.Shim) []string {
	t.Helper()
	var out []string
	for _, inv := range gh.Invocations(t) {
		if len(inv.Args) < 2 || inv.Args[0] != "pr" || inv.Args[1] != "list" {
			continue
		}
		if head := e2eFlagValue(inv.Args, "--head"); head != "" {
			out = append(out, head)
		}
	}
	return out
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
	for _, n := range recPRCreates(t, rig.GH) {
		sig.PRCreates += n
	}
	blocked := recBlockedGateBead(os.Getenv(recGateHitEnv))
	closeSwallowed := recBlockedGateBead(os.Getenv(recCloseHitEnv))
	checking := recFileExists(os.Getenv(recCheckHitEnv))

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
			recDescribePR(t, rig, b, &sig)
			return sig, true
		case recModeMerging:
			// The delivery has written `delivering` and is inside the
			// repository's check command, which is blocking: there is no
			// pull request, no conflict and nothing on the default branch,
			// so nothing but the P9-03 re-delivery can move this bead.
			if b.State != domain.BeadStateDelivering || !checking {
				continue
			}
			sig.Bead, sig.State, sig.Assignee = b.ID, b.State, b.Assignee
			sig.Branch = gitx.BranchName(b)
			sig.Worktree = rig.Manager.PathFor(b.ID)
			recDescribePR(t, rig, b, &sig)
			return sig, true
		case recModeLanded:
			// The fast-forward landed and was pushed, and the close that
			// would have ended the delivery was swallowed: the work is on
			// the default branch and the bead is still `delivering`.
			if b.ID != closeSwallowed || b.State != domain.BeadStateDelivering {
				continue
			}
			sig.Bead, sig.State, sig.Assignee = b.ID, b.State, b.Assignee
			sig.Branch = gitx.BranchName(b)
			sig.Worktree = rig.Manager.PathFor(b.ID)
			return sig, true
		case recModeDelivering:
			// The delivery has opened the pull request, recorded it on the
			// bead and reached `bd gate create`, which the wrapper is
			// holding: the bead is delivering, the gate does not exist, and
			// killing now leaves the one state nothing but the P6-15
			// re-delivery can repair.
			if b.ID != blocked || b.State != domain.BeadStateDelivering {
				continue
			}
			sig.Bead, sig.State, sig.Assignee = b.ID, b.State, b.Assignee
			sig.Branch = gitx.BranchName(b)
			sig.Worktree = rig.Manager.PathFor(b.ID)
			recDescribePR(t, rig, b, &sig)
			if sig.PRNumber == 0 || sig.Gate != "" {
				// The pull request metadata has not landed yet, or a gate
				// blocks the bead after all: either way this is not the
				// state the kill point is about.
				return recSignal{}, false
			}
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
