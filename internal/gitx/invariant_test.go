package gitx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
	"slop-cannon/internal/testutil/gitguard"
)

// P9-02 — safety invariant tests.
//
// Almost everything a run does to the target repository goes through git,
// so the guarantee "the cannon cannot damage the target repo" is largely a
// statement about the command lines a run produces. These tests run the
// real deliverers over a real fixture repository with a bare origin behind
// a git shim that records every invocation and then execs the real git
// (cuGit), so the scenario happens for real while the exact argv is kept
// for judgement. Safety on records gh as well, because a pull request can
// land work on the default branch with no git call at all (D-11), so
// "damage in git terms" alone would miss it.
//
// What is watched, exactly: every git and gh invocation made by anything
// running with the shims on PATH. That is the deliverers and the worktree
// manager, and it is also the shell git an engineer session runs in its
// own worktree — the "agent shell git" event below and
// [TestAnAgentsOwnGitIsCaughtByTheSameChecker] put agent-issued commands
// through the same checker, because the engineer's tool policy is
// read/write (internal/agents maps events.TaskImplement,
// TaskAddressFeedback and TaskResolveConflict to cartridge.ToolReadWrite,
// which the Claude cartridge enforces with Bash allowed and no deny list),
// so the only production guard on what an engineer does with git is a
// sentence in its prompt. The resume ([Resumer.Resume]) is watched too: it
// is the one extra git-invoking component a crash adds — a fetch, ancestry
// checks, worktree add and prune — and it runs on every `sc --resume`, so
// P9-03 put it under the same checker in a scenario of its own
// ([TestAResumeNeverTouchesTheDefaultBranch]), where it runs over a
// repository a crash left mid-rebase, twice. It is deliberately not one of
// the randomised events below: a resume fetches, which changes what a
// later delivery in the same sequence has to rebase onto, and the seeds
// here are chosen so that the sequences really deliver and really
// conflict. What is still not watched: branch cleanup, the
// delivery settler and the external pull request reviewer, which are
// covered from the other side by internal/orchestrator's end-to-end tests
// asserting that neither default branch tip moves.
//
// Two invariants are asserted, one per safety mode:
//
//   - safety on (D-06's default, [PRDeliverer]): no invocation ever checks
//     out, merges or pushes the default branch, and neither the local nor
//     the remote default branch moves at all. Work reaches the human only
//     as a pushed sc/ branch and a pull request.
//   - safety off ([Manager.DeliverMerge]): the default branch is only ever
//     moved by a fast-forward — every merge carries --ff-only, no ref update
//     of the default branch is forced, no merge commit lands on it, and it
//     never moves backwards.
//
// A handful of rules hold in both modes, because they are damage rather
// than delivery: the default branch is never deleted, renamed or checked
// out, "git pull" is never run, nothing that rewrites the branch it runs
// on (commit, reset, revert, cherry-pick, rebase …) is ever run in the
// main working tree, where the default branch is the one checked out, and
// nothing that discards what is in that tree (clean, checkout of paths,
// restore, stash) is run there either — the user's uncommitted work is
// theirs. A file the test leaves untracked in the main working tree stands
// in for it and is checked after every step, so the repository half of the
// invariant sees that damage too and not only moved refs.
//
// Both are checked across the P6-09 scenario re-created here in git terms
// (two beads, one with a dependency on the other, an engineer commit and a
// review round each, a pull that lands upstream in between, then delivery)
// and across randomised event sequences, where an event picks a bead and
// does one of the things a run does to a worktree — commit, commit
// leftovers, run git as the agent would, deliver, resolve a conflict, take
// an upstream change — in an order no scenario test would think of.
//
// The invariants are asserted from the recorded argv, so a future change
// that reaches for "git checkout main", "git push --force origin main" or
// "gh pr merge" fails here even when the fixture happens to end in a
// healthy state.

// The judgement itself lives in [gitguard], which internal/orchestrator's
// end-to-end runs use too (P9-23); these aliases keep the local spelling
// this file has always used.
type (
	ivSafety = gitguard.Safety
	ivEnv    = gitguard.Env
	ivCall   = gitguard.Call
)

const (
	ivSafetyOn  = gitguard.SafetyOn
	ivSafetyOff = gitguard.SafetyOff
)

var (
	ivSplit       = gitguard.Split
	ivInspect     = gitguard.Inspect
	ivInspectGH   = gitguard.InspectGH
	ivScanGH      = gitguard.ScanGH
	ivParse       = gitguard.Parse
	ivResolvePath = gitguard.ResolvePath
	ivScan        = gitguard.Scan
	ivCount       = gitguard.Count
)

// ivAssertCommands checks every git invocation recorded so far against the
// mode's invariant and reports each violation with the command that broke
// it. It returns the calls it judged so a caller can also assert that the
// scenario really produced the commands it is supposed to.
func ivAssertCommands(t *testing.T, git *testutil.Shim, env ivEnv, when string) []ivCall {
	t.Helper()
	calls := ivParse(t, git)
	bad := ivScan(calls, env)
	assert.Empty(t, bad, "safety %s, %s: git was asked to do something a delivery may never do:\n%s",
		env.Safety, when, strings.Join(bad, "\n"))
	return calls
}

// ivStats counts what the deliveries in one sequence actually did, so a
// sequence of no-ops cannot pass for a tested one.
type ivStats struct {
	delivered  int // deliveries that returned no error
	conflicted int // deliveries that stopped on a rebase conflict (D-10)
	refused    int // deliveries refused for any other reason
}

// add folds another sequence's counts into these.
func (s *ivStats) add(o ivStats) {
	s.delivered += o.delivered
	s.conflicted += o.conflicted
	s.refused += o.refused
}

// String renders the counts for a failure message.
func (s ivStats) String() string {
	return fmt.Sprintf("%d delivered, %d conflicted, %d refused", s.delivered, s.conflicted, s.refused)
}

// ivFixture is a fixture repository with a bare origin, a recording git
// shim, a clone of the remote standing in for everyone else pushing to it,
// and (for safety on) a gh shim, a PR tracker and a deliverer.
type ivFixture struct {
	*wtFixture
	git     *testutil.Shim
	gh      *testutil.Shim
	tracker *dpTracker
	d       *PRDeliverer
	env     ivEnv
	beads   []domain.Bead
	clone   string // a working clone of the bare remote, for upstream commits

	mainSeen   string // the local default branch tip last asserted
	remoteSeen string // the remote default branch tip last asserted
	statusSeen string // "git status" in the main working tree, last asserted
	seen       int    // git invocations already returned by ivAssertStep
	stats      ivStats
}

// ivUserFile is a file the test leaves untracked in the main working tree
// and never mentions again: the user's own uncommitted work, which a run
// must leave exactly where it found it. Nothing a bead does writes to this
// path, so a merge can never legitimately collide with it.
const ivUserFile = "the-users-own-notes.txt"

// ivUserFileBody is what that file holds, so overwriting it is caught as
// well as deleting it.
const ivUserFileBody = "the user's own notes, uncommitted and untracked\n"

// ivBeads are the two beads of the P6-09 scenario: the second depends on
// the first, so it is delivered after the first has landed.
var ivBeads = []domain.Bead{
	{ID: "fx-i1", Title: "Fix the mean() bug", Type: domain.BeadTypeBug, Priority: 1},
	{ID: "fx-i2", Title: "Guard max() against an empty slice", Type: domain.BeadTypeBug, Priority: 2},
}

// ivSetup builds the fixture for one safety mode: a repository with a bare
// origin behind the recording git shim, a worktree for each bead, and the
// deliverer the mode uses. Safety on also gets the gh shim and a GitHub
// fetch URL so [PRDeliverer] takes its normal path while pushes still land
// in the local bare remote.
func ivSetup(t *testing.T, safety ivSafety) *ivFixture {
	t.Helper()
	ctx := context.Background()
	git := cuGit(t) // records every invocation, then execs the real git
	f := wtSetup(t, []testutil.FixtureOption{testutil.WithBareRemote()})

	iv := &ivFixture{
		wtFixture: f,
		git:       git,
		beads:     ivBeads,
		env: ivEnv{
			Safety:  safety,
			Default: f.r.DefaultBranch,
			Remote:  "origin",
			MainDir: ivResolvePath(f.repo.Root),
		},
	}
	iv.clone = ivClone(t, f.r)

	if safety == ivSafetyOn {
		iv.gh = dpGH(t)
		iv.tracker = &dpTracker{}
		d, err := NewPRDeliverer(f.m, iv.tracker, WithPRLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
		require.NoError(t, err)
		iv.d = d
		// gh only ever sees a GitHub remote; pushes still go to the bare
		// repository through the push URL, so nothing reaches the network.
		f.r.Git(t, "remote", "set-url", "--push", "origin", f.r.RemoteDir)
		f.r.Git(t, "remote", "set-url", "origin", "https://github.com/acme/widgets.git")
	}

	for _, bead := range iv.beads {
		_, err := f.m.Create(ctx, bead)
		require.NoError(t, err)
	}
	// The worktrees exist, so the repository may go dirty now (Create
	// refuses a dirty one): leave the user's own file in the main working
	// tree and watch it for the rest of the scenario.
	require.NoError(t, os.WriteFile(filepath.Join(f.repo.Root, ivUserFile), []byte(ivUserFileBody), 0o644))
	iv.mainSeen, iv.remoteSeen = iv.tips(t)
	iv.statusSeen = iv.mainStatus(t)
	require.Contains(t, iv.statusSeen, ivUserFile, "the user's own file is what the main working tree holds")
	return iv
}

// ivClone makes a working clone of the bare remote with the fixture's
// isolated git environment. Commits pushed from it are what everyone else
// working on the repository does while the run is going.
func ivClone(t *testing.T, r *testutil.FixtureRepo) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "upstream")
	_, err := ivRun(t, r, filepath.Dir(dir), "clone", "--quiet", r.RemoteDir, dir)
	require.NoError(t, err)
	return dir
}

// ivRun runs the real git (never the shim) in dir with the fixture's
// isolated environment, so what a test does itself is never mistaken for
// what the code under test did.
func ivRun(t *testing.T, r *testutil.FixtureRepo, dir string, args ...string) (string, error) {
	t.Helper()
	bin, err := testutil.RealBinary("git")
	require.NoError(t, err)
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = r.Env()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ivMustRun is ivRun for a command that must succeed.
func ivMustRun(t *testing.T, r *testutil.FixtureRepo, dir string, args ...string) string {
	t.Helper()
	out, err := ivRun(t, r, dir, args...)
	require.NoError(t, err, "git %s in %s: %s", strings.Join(args, " "), dir, out)
	return out
}

// tips returns the local and remote default branch tips ("" when the ref
// does not exist).
func (f *ivFixture) tips(t *testing.T) (local, remote string) {
	t.Helper()
	local, _ = ivRun(t, f.r, f.r.Dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+f.env.Default)
	remote, _ = ivRun(t, f.r, f.r.Dir, "--git-dir", f.r.RemoteDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+f.env.Default)
	return local, remote
}

// isAncestor reports whether old is an ancestor of newer in the repository
// gitDir names (empty means the fixture's own).
func (f *ivFixture) isAncestor(t *testing.T, gitDir, old, newer string) bool {
	t.Helper()
	args := []string{}
	if gitDir != "" {
		args = append(args, "--git-dir", gitDir)
	}
	args = append(args, "merge-base", "--is-ancestor", old, newer)
	_, err := ivRun(t, f.r, f.r.Dir, args...)
	return err == nil
}

// mainStatus is "git status" in the main working tree, in the porcelain
// form and including untracked files, so both the user's own file and any
// change to a tracked one show up.
func (f *ivFixture) mainStatus(t *testing.T) string {
	t.Helper()
	return ivMustRun(t, f.r, f.r.Dir, "status", "--porcelain", "--untracked-files=all")
}

// ivRepoViolations returns a reason for everything the repository itself
// says a step should not have done: safety on leaves both default branch
// tips exactly where they were, safety off may only move them forward and
// may never put a merge commit on either, and in both modes the main
// working tree — the user's, not the run's — must be untouched, the user's
// own uncommitted file included.
//
// It updates the remembered state, so the next call compares against what
// this one saw, and it returns reasons rather than asserting so that
// [TestAnAgentsOwnGitIsCaughtByTheSameChecker] can prove this half fires on
// real damage.
func (f *ivFixture) ivRepoViolations(t *testing.T) []string {
	t.Helper()
	var bad []string
	flag := func(format string, args ...any) { bad = append(bad, fmt.Sprintf(format, args...)) }
	local, remote := f.tips(t)
	switch f.env.Safety {
	case ivSafetyOn:
		if local != f.mainSeen {
			flag("the local %s moved (%s -> %s)", f.env.Default, f.mainSeen, local)
		}
		if remote != f.remoteSeen {
			flag("%s/%s moved (%s -> %s)", f.env.Remote, f.env.Default, f.remoteSeen, remote)
		}
	default:
		if local != "" && f.mainSeen != "" && local != f.mainSeen && !f.isAncestor(t, "", f.mainSeen, local) {
			flag("the local %s moved off its own history (%s -> %s)", f.env.Default, f.mainSeen, local)
		}
		if remote != "" && f.remoteSeen != "" && remote != f.remoteSeen && !f.isAncestor(t, f.r.RemoteDir, f.remoteSeen, remote) {
			flag("%s/%s moved off its own history (%s -> %s)", f.env.Remote, f.env.Default, f.remoteSeen, remote)
		}
	}
	if local != "" {
		if merges := ivMustRun(t, f.r, f.r.Dir, "log", "--merges", "--oneline", "refs/heads/"+f.env.Default); merges != "" {
			flag("a merge commit reached the local %s (D-10): %s", f.env.Default, merges)
		}
	}
	if remote != "" {
		if merges := ivMustRun(t, f.r, f.r.Dir, "--git-dir", f.r.RemoteDir, "log", "--merges", "--oneline", "refs/heads/"+f.env.Default); merges != "" {
			flag("a merge commit reached %s/%s (D-10): %s", f.env.Remote, f.env.Default, merges)
		}
	}
	if status := f.mainStatus(t); status != f.statusSeen {
		flag("the main working tree changed under the user (%q -> %q)", f.statusSeen, status)
		f.statusSeen = status
	}
	switch body, err := os.ReadFile(filepath.Join(f.repo.Root, ivUserFile)); {
	case err != nil:
		flag("the user's own uncommitted file is gone: %v", err)
	case string(body) != ivUserFileBody:
		flag("the user's own uncommitted file was rewritten: %q", body)
	}
	f.mainSeen, f.remoteSeen = local, remote
	return bad
}

// ivAssertRepo asserts that the repository half of the invariant holds.
func (f *ivFixture) ivAssertRepo(t *testing.T, when string) {
	t.Helper()
	bad := f.ivRepoViolations(t)
	assert.Empty(t, bad, "safety %s, %s: the repository is not where it should be:\n%s",
		f.env.Safety, when, strings.Join(bad, "\n"))
}

// ivAssert checks all three parts of the invariant — the git commands, the
// gh commands and the state the repository is in — and returns every git
// call recorded so far.
func (f *ivFixture) ivAssert(t *testing.T, when string) []ivCall {
	t.Helper()
	calls := ivAssertCommands(t, f.git, f.env, when)
	if f.gh != nil {
		bad := ivScanGH(t, f.gh)
		assert.Empty(t, bad, "safety %s, %s: gh was asked to do something a delivery may never do:\n%s",
			f.env.Safety, when, strings.Join(bad, "\n"))
	}
	f.ivAssertRepo(t, when)
	return calls
}

// ivAssertStep is [ivFixture.ivAssert] for one step of a sequence: it
// returns only the git invocations that step produced, so a caller can tell
// an event that really exercised the code from one that did nothing at all.
func (f *ivFixture) ivAssertStep(t *testing.T, when string) []ivCall {
	t.Helper()
	all := f.ivAssert(t, when)
	fresh := all[min(f.seen, len(all)):]
	f.seen = len(all)
	return fresh
}

// ivWork writes a file in the bead's worktree and commits it, the way an
// engineer session ends. content chooses the file: the shared one when
// shared is true, so an upstream change conflicts with it.
func (f *ivFixture) ivWork(t *testing.T, bead domain.Bead, content string, shared bool) {
	t.Helper()
	path := f.m.PathFor(bead.ID)
	rel := fmt.Sprintf("stats/%s.md", bead.ID)
	if shared {
		rel = ivSharedFile
	}
	acWrite(t, path, rel, content)
	wtRepo, err := f.m.Open(context.Background(), bead.ID)
	if err != nil {
		return // the worktree is mid-rebase or gone; the delivery reports it
	}
	if _, err := wtRepo.CommitAll(context.Background(), "work on "+string(bead.ID)); err != nil {
		t.Logf("commit in %s: %v", bead.ID, err) // tolerated: mid-rebase, nothing staged
	}
}

// ivSharedFile is the file both the beads and the upstream clone edit, so
// a delivery hits a real rebase conflict.
const ivSharedFile = "stats/NOTES.md"

// ivUpstream commits in the clone and pushes to the bare remote, which is
// what a colleague merging their own work looks like from the run's side.
// The clone is refreshed first, so its own push is always a fast-forward
// and the remote's history stays the responsibility of this test alone.
func (f *ivFixture) ivUpstream(t *testing.T, content string, shared bool) {
	t.Helper()
	if _, err := ivRun(t, f.r, f.clone, "fetch", "--quiet", "origin"); err != nil {
		return
	}
	if _, err := ivRun(t, f.r, f.clone, "reset", "--hard", "--quiet", "origin/"+f.env.Default); err != nil {
		return
	}
	rel := "upstream/" + content + ".md"
	if shared {
		rel = ivSharedFile
	}
	path := filepath.Join(f.clone, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("upstream: "+content+"\n"), 0o644))
	ivMustRun(t, f.r, f.clone, "add", "-A")
	ivMustRun(t, f.r, f.clone, "commit", "--quiet", "-m", "upstream "+content)
	ivMustRun(t, f.r, f.clone, "push", "--quiet", "origin", "HEAD:refs/heads/"+f.env.Default)
	// The remote moved because this test moved it, which is a fast-forward
	// by construction; remember it so the next check judges the code under
	// test rather than the test's own colleague.
	_, f.remoteSeen = f.tips(t)
}

// ivDeliver runs the mode's delivery for one bead, counting the outcome.
// A randomised sequence is allowed to fail a delivery — it may have left
// nothing to deliver, or a conflict for the engineer — because the
// invariant is about what git was asked to do on the way, not about the
// delivery succeeding. It returns the error for a caller that wants it.
func (f *ivFixture) ivDeliver(t *testing.T, bead domain.Bead) error {
	t.Helper()
	ctx := context.Background()
	var err error
	if f.env.Safety == ivSafetyOn {
		req := dpRequest()
		req.Bead = bead
		_, err = f.d.Deliver(ctx, req)
	} else {
		// "true" keeps the fixture's own go test out of the delivery;
		// P4-06's detection is covered by its own tests.
		_, err = f.m.DeliverMerge(ctx, bead, MergeOptions{CheckCommand: "true"})
	}
	switch {
	case err == nil:
		f.stats.delivered++
	case errors.Is(err, ErrRebaseConflict):
		f.stats.conflicted++
		t.Logf("delivering %s stopped on a conflict: %v", bead.ID, err)
	default:
		f.stats.refused++
		t.Logf("delivering %s: %v", bead.ID, err)
	}
	return err
}

// ivResume runs the crash recovery over both beads and judges what it did
// with everything else (P9-03). It is a dry run — the resumer is given no
// tracker, so nothing is written to bd — because what is being judged is
// the git it issues, not the bead states it would reconcile, which
// cleanup_test.go covers.
//
// With safety on the fixture's fetch URL is the GitHub one gh needs, and
// the resume is the one part of the product that fetches, so it is pointed
// back at the bare remote for the duration: nothing in these tests may
// reach the network. The pull request lookup still goes to the gh shim,
// which is judged as well.
func (f *ivFixture) ivResume(t *testing.T, beadList []domain.Bead) *ResumeResult {
	t.Helper()
	if f.env.Safety == ivSafetyOn {
		f.r.Git(t, "remote", "set-url", "origin", f.r.RemoteDir)
		defer f.r.Git(t, "remote", "set-url", "origin", "https://github.com/acme/widgets.git")
	}
	opts := []ResumeOption{}
	if f.env.Safety == ivSafetyOff {
		// A run with safety off never opens a pull request, and `sc run`
		// gives its resumer no gh for that reason.
		opts = append(opts, WithoutGH())
	}
	res, err := cuResumer(t, f.m, nil, opts...).Resume(context.Background(), beadList)
	require.NoError(t, err)
	return res
}

// ivDelivering returns the fixture's beads as a crash left them: claimed by
// a delivery that never finished, which is the state the resume has the
// most to do about.
func (f *ivFixture) ivDelivering() []domain.Bead {
	out := make([]domain.Bead, 0, len(f.beads))
	for _, b := range f.beads {
		b.State = domain.BeadStateDelivering
		out = append(out, b)
	}
	return out
}

// ivResolveConflict finishes what an engineer's conflict round does (D-10):
// it writes a resolution for every conflicted path and stages it, leaving
// the rebase for the next delivery attempt to continue.
func (f *ivFixture) ivResolveConflict(t *testing.T, bead domain.Bead) bool {
	t.Helper()
	wtRepo, err := f.m.Open(context.Background(), bead.ID)
	if err != nil || !wtRepo.RebaseInProgress() {
		return false
	}
	paths, err := wtRepo.ConflictedPaths(context.Background())
	if err != nil || len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		acWrite(t, wtRepo.Root, p, "resolved by the engineer\n")
	}
	ivMustRun(t, f.r, wtRepo.Root, "add", "-A")
	return true
}

// ivAgentGit runs one git command line the way an engineer session's Bash
// tool call would: through the shim on PATH, in the directory the session
// works in, with nothing in gitx involved. The engineer's tool policy is
// read/write with an unrestricted Bash, so these command lines are as real
// a part of what a run does to the repository as the deliverers' own, and
// the checker judges them the same way. The output and error are returned
// for a caller that cares; a command that fails is still recorded, which
// is what the invariant reads.
func (f *ivFixture) ivAgentGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := f.git.Command(t, args...)
	cmd.Dir = dir
	cmd.Env = f.r.Env()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ivAgentCommands are the git command lines an engineer session plausibly
// runs inside its own worktree: reading where it is and what it changed,
// committing, and throwing away its own scratch work. Every one of them is
// within the invariant, because the worktree is the agent's to write.
var ivAgentCommands = [][]string{
	{"status", "--porcelain"},
	{"diff", "--stat"},
	{"log", "--oneline", "-n", "5"},
	{"rev-parse", "--abbrev-ref", "HEAD"},
	{"commit", "--allow-empty", "--quiet", "-m", "checkpoint from the agent"},
	{"checkout", "--", "."},
}

// ivAgentSession runs one of those command lines in the bead's worktree.
// A failure is expected sometimes — the worktree may be mid-rebase — and
// tolerated, because what is judged is the command line, not its exit code.
func (f *ivFixture) ivAgentSession(t *testing.T, bead domain.Bead, args []string) {
	t.Helper()
	if out, err := f.ivAgentGit(t, f.m.PathFor(bead.ID), args...); err != nil {
		t.Logf("agent ran git %s in %s: %v: %s", strings.Join(args, " "), bead.ID, err, out)
	}
}

func TestSafetyOnNeverTouchesTheDefaultBranch(t *testing.T) {
	ctx := context.Background()
	f := ivSetup(t, ivSafetyOn)

	// The P6-09 scenario in git terms: two beads, an engineer commit and a
	// review round each, an upstream change landing in between, then the
	// dependent bead delivered after the one it depends on — and a retry of
	// the first delivery, which is what a gate poll of a bead whose pull
	// request is still open leads to.
	for i, bead := range f.beads {
		f.ivWork(t, bead, fmt.Sprintf("engineer round 1 on %s\n", bead.ID), i == 0)
		// The session's own git, run from its Bash the way an engineer
		// would, is judged with everything else.
		for _, args := range ivAgentCommands {
			f.ivAgentSession(t, bead, args)
		}
		acWrite(t, f.m.PathFor(bead.ID), "stats/leftovers.md", "left uncommitted\n")
		leftovers, err := f.m.CommitLeftovers(ctx, bead, 1)
		require.NoError(t, err)
		require.True(t, leftovers.Committed)
		f.ivAssert(t, "after the review round of "+string(bead.ID))
	}

	f.ivUpstream(t, "release-notes", false)
	f.ivAssert(t, "after an upstream commit")

	for _, bead := range f.beads {
		req := dpRequest()
		req.Bead = bead
		res, err := f.d.Deliver(ctx, req)
		require.NoError(t, err)
		assert.Equal(t, PRCreated, res.Status)
		assert.Equal(t, domain.BeadStateDelivering, res.State)
		assert.True(t, res.Pushed, "the bead's branch reaches the remote")
		f.ivAssert(t, "after delivering "+string(bead.ID))
	}
	require.NoError(t, f.ivDeliver(t, f.beads[0]), "a second delivery of the same bead is a no-op retry")

	calls := f.ivAssert(t, "at the end of the scenario")

	// The invariant must not hold merely because nothing happened: the
	// branches really are on the remote, and the pushes really ran.
	assert.GreaterOrEqual(t, ivCount(calls, "push"), len(f.beads), "every bead's branch was pushed")
	heads := ivMustRun(t, f.r, f.r.Dir, "ls-remote", "--heads", f.r.RemoteDir)
	for _, bead := range f.beads {
		assert.Contains(t, heads, "refs/heads/"+BranchName(bead), "%s was delivered as a branch", bead.ID)
	}
	assert.Len(t, f.tracker.gates, len(f.beads), "each bead ends behind its own gh:pr gate (D-20)")
	// At least one per bead: the retry above asks gh for one more, because
	// the shim's "pr list" never remembers the ones it opened.
	assert.GreaterOrEqual(t, strings.Count(strings.Join(dpCalls(t, f.gh), "\n"), "pr create"), len(f.beads),
		"a pull request per bead is what the human is given instead")
	assert.Zero(t, ivCount(calls, "merge"), "safety on never merges")
}

func TestSafetyOffOnlyFastForwardsTheDefaultBranch(t *testing.T) {
	ctx := context.Background()
	f := ivSetup(t, ivSafetyOff)
	start := f.mainSeen

	for _, bead := range f.beads {
		f.ivWork(t, bead, fmt.Sprintf("engineer round 1 on %s\n", bead.ID), false)
		acWrite(t, f.m.PathFor(bead.ID), fmt.Sprintf("stats/%s-leftovers.md", bead.ID), "left uncommitted\n")
		leftovers, err := f.m.CommitLeftovers(ctx, bead, 1)
		require.NoError(t, err)
		require.True(t, leftovers.Committed)
	}
	// Someone else lands work on the default branch first, so the local
	// branch is behind its remote and the second bead has to be rebased
	// onto a branch that moved twice.
	f.ivUpstream(t, "release-notes", false)
	f.ivAssert(t, "after an upstream commit")

	for _, bead := range f.beads {
		res, err := f.m.DeliverMerge(ctx, bead, MergeOptions{CheckCommand: "true"})
		require.NoError(t, err)
		assert.Positive(t, res.Commits, "%s landed commits on %s", bead.ID, f.env.Default)
		assert.True(t, res.Pushed, "%s reached the remote", bead.ID)
		assert.Equal(t, f.env.Default, res.Target)
		assert.True(t, res.Rebased, "%s was rebased onto a %s that moved under it", bead.ID, f.env.Default)
		f.ivAssert(t, "after delivering "+string(bead.ID))
	}

	calls := f.ivAssert(t, "at the end of the scenario")

	// Non-vacuity: the default branch really did move, through a real
	// fast-forward merge, and the remote agrees with it.
	local, remote := f.tips(t)
	assert.NotEqual(t, start, local, "the default branch received the work")
	assert.Equal(t, local, remote, "the remote default branch matches the local one")
	assert.True(t, f.isAncestor(t, "", start, local), "the default branch only moved forward")
	assert.Positive(t, ivCount(calls, "merge"), "a fast-forward merge is how the work landed")
	for _, c := range calls {
		if c.Sub == "merge" {
			assert.Contains(t, c.Rest, "--ff-only", "every merge is a fast-forward: %s", c)
		}
	}
	subjects := ivMustRun(t, f.r, f.r.Dir, "log", "--format=%s", "refs/heads/"+f.env.Default)
	for _, bead := range f.beads {
		assert.Contains(t, subjects, "work on "+string(bead.ID), "%s's work is on %s", bead.ID, f.env.Default)
	}
}

// TestAResumeNeverTouchesTheDefaultBranch is the crash-recovery half of the
// invariant (P9-03). A resume is the one extra git-invoking component a
// crash adds — it fetches, resolves branches, asks whether each branch has
// landed, recreates the worktrees whose directories are gone and prunes the
// registrations that are stale — and it runs on every `sc --resume`, over a
// repository nobody has tidied up. Everything it issues is judged by argv
// here, exactly as a delivery's is, in both safety modes.
func TestAResumeNeverTouchesTheDefaultBranch(t *testing.T) {
	for _, safety := range []ivSafety{ivSafetyOn, ivSafetyOff} {
		t.Run(string(safety), func(t *testing.T) {
			f := ivSetup(t, safety)

			// The mess a crash leaves: both beads have work committed, one
			// of them on the file the upstream commit also touches, so its
			// delivery stopped mid-rebase and its worktree is still in that
			// state — which is what the resume has to inspect, reattach and
			// report without touching anything of the user's. The other
			// bead's worktree directory is gone altogether, the way a
			// crashed machine or a tidying human leaves one, so the resume
			// has to recreate it while the fixture's working tree holds the
			// user's own uncommitted file (P4-15).
			for i, bead := range f.beads {
				f.ivWork(t, bead, fmt.Sprintf("engineer round 1 on %s\n", bead.ID), i == 0)
			}
			f.ivUpstream(t, "landed-while-the-run-was-down", true)
			_ = f.ivDeliver(t, f.beads[0])
			wiped := f.beads[len(f.beads)-1]
			require.NoError(t, os.RemoveAll(f.m.PathFor(wiped.ID)))
			f.ivAssertStep(t, "after the crash") // the setup's own calls

			res := f.ivResume(t, f.ivDelivering())

			fresh := f.ivAssertStep(t, "after the resume")
			require.NotEmpty(t, fresh, "the resume ran git at all")
			// Non-vacuity: the resume did the git work a resume does, so
			// the invariant does not hold merely because nothing happened.
			assert.Positive(t, ivCount(fresh, "fetch"), "the resume fetched: %v", fresh)
			assert.Positive(t, ivCount(fresh, "merge-base"), "and asked what had landed")
			assert.Positive(t, ivCount(fresh, "worktree"), "and worked on the worktrees")
			require.Len(t, res.Entries, len(f.beads))
			var conflicted, recreated int
			for _, e := range res.Entries {
				require.NoErrorf(t, e.Err, "%s: %s", e.Bead.ID, e.Reason)
				if e.Bead.ID == wiped.ID {
					assert.Equalf(t, WorktreeRecreated, e.WorktreeStatus,
						"%s's worktree was recreated even though the user's tree is dirty (P4-15)", e.Bead.ID)
					assert.DirExistsf(t, f.m.PathFor(e.Bead.ID), "%s has a worktree again", e.Bead.ID)
					recreated++
				} else {
					assert.Equalf(t, WorktreeAttached, e.WorktreeStatus, "%s was reattached", e.Bead.ID)
				}
				if e.Conflict {
					conflicted++
				}
			}
			assert.Equal(t, 1, recreated, "the wiped worktree came back: %s", res.Summary())
			// Safety off rebases before it merges, so the delivery above
			// really did stop mid-rebase; safety on pushes the branch and
			// opens a pull request, and leaves no rebase behind.
			want := 0
			if safety == ivSafetyOff {
				want = 1
			}
			assert.Equal(t, want, conflicted, "the worktrees were reported as they are: %s", res.Summary())

			// And a second resume, which is what a second crash gives it,
			// is judged too: nothing about the first one licenses the next.
			f.ivResume(t, f.ivDelivering())
			f.ivAssertStep(t, "after resuming twice")
		})
	}
}

// ivEvent is one thing that can happen to a run, drawn by the randomised
// sequences with the weight beside it.
type ivEvent struct {
	name   string
	weight int
	// expectGit is true when the event always reaches git through the
	// shim, so a step that recorded nothing means the event did nothing
	// and the checks after it prove nothing. It is false for the two
	// events that legitimately run no git of ours: the upstream commit
	// (deliberately made with the real git, so the test's own colleague is
	// never mistaken for the code under test) and resolving a conflict,
	// which returns immediately when there is no rebase in progress.
	expectGit bool
	do        func(t *testing.T, f *ivFixture, rng *rand.Rand, bead domain.Bead, n int)
}

// ivEvents is the alphabet the randomised sequences draw from. Everything
// a run does to a git repository between the first engineer session and
// delivery is in it, so a sequence exercises orderings no scenario test
// would write down: a delivery with nothing to deliver, two deliveries in
// a row, an upstream change landing while a worktree is mid-rebase, a
// conflict resolved and then another one opened on top of it.
var ivEvents = []ivEvent{
	{name: "engineer commit", weight: 4, expectGit: true, do: func(t *testing.T, f *ivFixture, rng *rand.Rand, bead domain.Bead, n int) {
		f.ivWork(t, bead, fmt.Sprintf("round %d\n", n), rng.IntN(2) == 0)
	}},
	{name: "agent shell git", weight: 2, expectGit: true, do: func(t *testing.T, f *ivFixture, rng *rand.Rand, bead domain.Bead, _ int) {
		f.ivAgentSession(t, bead, ivAgentCommands[rng.IntN(len(ivAgentCommands))])
	}},
	{name: "commit leftovers", weight: 2, expectGit: true, do: func(t *testing.T, f *ivFixture, _ *rand.Rand, bead domain.Bead, n int) {
		acWrite(t, f.m.PathFor(bead.ID), fmt.Sprintf("stats/left-%d.md", n), "uncommitted\n")
		if _, err := f.m.CommitLeftovers(context.Background(), bead, n); err != nil {
			t.Logf("leftovers for %s: %v", bead.ID, err) // tolerated (P6-16)
		}
	}},
	{name: "upstream commit", weight: 2, do: func(t *testing.T, f *ivFixture, rng *rand.Rand, _ domain.Bead, n int) {
		f.ivUpstream(t, fmt.Sprintf("change-%d", n), rng.IntN(2) == 0)
	}},
	{name: "deliver", weight: 4, expectGit: true, do: func(t *testing.T, f *ivFixture, _ *rand.Rand, bead domain.Bead, _ int) {
		_ = f.ivDeliver(t, bead) // the outcome is counted in f.stats
	}},
	{name: "resolve conflict", weight: 2, do: func(t *testing.T, f *ivFixture, _ *rand.Rand, bead domain.Bead, _ int) {
		f.ivResolveConflict(t, bead)
	}},
	{name: "reattach worktree", weight: 1, expectGit: true, do: func(t *testing.T, f *ivFixture, _ *rand.Rand, bead domain.Bead, _ int) {
		if _, err := f.m.Reattach(context.Background(), bead.ID); err != nil {
			t.Logf("reattaching %s: %v", bead.ID, err)
		}
	}},
	{name: "read the diff", weight: 1, expectGit: true, do: func(t *testing.T, f *ivFixture, _ *rand.Rand, bead domain.Bead, _ int) {
		if _, err := f.m.Diff(context.Background(), bead.ID); err != nil {
			t.Logf("diffing %s: %v", bead.ID, err)
		}
	}},
}

// ivPick draws one event with the weights above.
func ivPick(rng *rand.Rand) ivEvent {
	total := 0
	for _, e := range ivEvents {
		total += e.weight
	}
	n := rng.IntN(total)
	for _, e := range ivEvents {
		if n < e.weight {
			return e
		}
		n -= e.weight
	}
	return ivEvents[len(ivEvents)-1]
}

// ivEventsPerSequence is how many events one randomised sequence applies.
// Every event is followed by a full check of both invariants, so a
// sequence costs what it is long; twelve is enough for deliveries,
// conflicts, resolutions and retries to interleave.
const ivEventsPerSequence = 12

// ivSeeds are the fixed seeds the randomised sequences run with. They are
// constants rather than time-derived, so a failure is reproducible by
// running the same subtest again, and the totals asserted at the end of
// the test are the same on every machine. They are chosen against the
// event alphabet above — adding an event changes every draw, so a seed set
// that no longer reaches a rebase conflict fails the end-of-mode check
// below rather than quietly testing less.
var ivSeeds = []uint64{1, 5, 17, 1729}

func TestSafetyInvariantsUnderRandomisedEventSequences(t *testing.T) {
	for _, safety := range []ivSafety{ivSafetyOn, ivSafetyOff} {
		total := ivStats{}
		for _, seed := range ivSeeds {
			t.Run(fmt.Sprintf("safety-%s/seed-%d", safety, seed), func(t *testing.T) {
				rng := rand.New(rand.NewPCG(seed, seed))
				f := ivSetup(t, safety)
				// Every bead starts with a session behind it, the way the
				// scheduler only ever delivers a bead an engineer has
				// worked on; the shared file makes an upstream change
				// conflict with it later.
				for i, bead := range f.beads {
					f.ivWork(t, bead, fmt.Sprintf("first session on %s\n", bead.ID), i == 0)
				}
				log := []string{"0. first engineer session on every bead"}
				f.ivAssertStep(t, log[0]) // sets the mark the per-step check counts from

				for i := 1; i <= ivEventsPerSequence; i++ {
					event := ivPick(rng)
					bead := f.beads[rng.IntN(len(f.beads))]
					event.do(t, f, rng, bead, i)
					step := fmt.Sprintf("%d. %s (%s)", i, event.name, bead.ID)
					log = append(log, step)
					// Only the invocations this event produced count: the
					// log is never empty after step 0, so asserting on the
					// whole of it would prove nothing about the step.
					fresh := f.ivAssertStep(t, "after event "+step)
					if event.expectGit {
						assert.NotEmpty(t, fresh, "%s ran no git at all, so the checks after it judged nothing", step)
					}
					if t.Failed() {
						t.Fatalf("sequence so far:\n%s", strings.Join(log, "\n"))
					}
				}
				total.add(f.stats)
				t.Logf("sequence (%s):\n%s", f.stats, strings.Join(log, "\n"))
			})
		}
		// The invariants above are only worth something if the sequences
		// really delivered: a run of pure no-ops would satisfy every one
		// of them. The seeds are fixed, so these totals are stable.
		t.Run("safety-"+string(safety)+"/the sequences delivered", func(t *testing.T) {
			assert.Positive(t, total.delivered, "some delivery succeeded across the sequences (%s)", total)
			if safety == ivSafetyOff {
				// Only the merge path rebases, so only it can conflict;
				// reaching a conflict is what makes the mid-rebase
				// orderings above real (D-10).
				assert.Positive(t, total.conflicted, "some delivery stopped on a rebase conflict across the sequences (%s)", total)
			}
		})
	}
}

// TestIvTheShimCatchesAForbiddenCommand runs one of the commands the
// invariant forbids through the very shim the scenarios watch, so the
// whole path — shim, log, parser, checker — is known to catch a delivery
// that starts writing to the default branch, rather than only the
// synthetic argv of TestIvInspect.
func TestIvTheShimCatchesAForbiddenCommand(t *testing.T) {
	f := ivSetup(t, ivSafetyOn)
	before := len(ivScan(ivParse(t, f.git), f.env))
	require.Zero(t, before, "the fixture itself is within the invariant")

	cmd := f.git.Command(t, "push", "--quiet", "origin", "refs/heads/"+f.env.Default+":refs/heads/"+f.env.Default)
	cmd.Dir = f.r.Dir
	cmd.Env = f.r.Env()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "pushing the default branch to where it already is: %s", out)

	bad := ivScan(ivParse(t, f.git), f.env)
	require.Len(t, bad, 1, "the forbidden command is caught exactly once")
	assert.Contains(t, bad[0], "pushing "+f.env.Default)
}

// TestAnAgentsOwnGitIsCaughtByTheSameChecker closes the other half of the
// question the invariant answers. An engineer session runs read/write with
// an unrestricted Bash inside a linked worktree of the target repository,
// so "git push origin HEAD:refs/heads/main" or "git clean -fdx" from an
// agent is possible and the only production guard is a sentence in its
// prompt. Here the agent's own command lines go through the same shim as
// the deliverers', and the checker judges them identically: what is
// legitimate inside the bead's worktree stays legitimate, and the same
// verbs aimed at the default branch or at the user's own working tree are
// caught — argv first, and for real damage the repository half as well.
func TestAnAgentsOwnGitIsCaughtByTheSameChecker(t *testing.T) {
	f := ivSetup(t, ivSafetyOn)
	wt := f.m.PathFor(f.beads[0].ID)
	require.Empty(t, ivScan(ivParse(t, f.git), f.env), "the fixture itself is within the invariant")

	def := f.env.Default
	cases := []struct {
		name string
		dir  string
		args []string
		want string // a substring of the expected violation, "" for none
	}{
		{name: "the agent pushing the default branch from its worktree", dir: wt,
			args: []string{"push", "--quiet", "origin", "refs/heads/" + def + ":refs/heads/" + def},
			want: "pushing " + def},
		{name: "the agent checking out the default branch", dir: f.r.Dir,
			args: []string{"checkout", def}, want: "checking out " + def},
		{name: "the agent pushing with no refspec from the main working tree", dir: f.r.Dir,
			args: []string{"push", "--quiet", "origin"}, want: "pushing " + def},
		{name: "the agent throwing away the user's working tree", dir: f.r.Dir,
			args: []string{"checkout", "--", "."}, want: "discards the user's uncommitted work"},
		// --dry-run so the fixture survives to the repository check below;
		// the checker judges the command line, which is a commit in the
		// user's own working tree either way.
		{name: "the agent committing in the main working tree", dir: f.r.Dir,
			args: []string{"commit", "--dry-run", "--allow-empty", "-m", "sneaky"},
			want: "moves " + def},
		{name: "the agent committing in its own worktree", dir: wt,
			args: []string{"commit", "--allow-empty", "--quiet", "-m", "checkpoint"}},
		{name: "the agent throwing away its own scratch work", dir: wt,
			args: []string{"checkout", "--", "."}},
		{name: "the agent reading the main working tree", dir: f.r.Dir,
			args: []string{"status", "--porcelain"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.git.Reset(t)
			// The command may fail (no upstream, nothing to commit); the
			// invocation is recorded either way and that is what is judged.
			if out, err := f.ivAgentGit(t, tc.dir, tc.args...); err != nil {
				t.Logf("git %s in %s: %v: %s", strings.Join(tc.args, " "), tc.dir, err, out)
			}
			calls := ivParse(t, f.git)
			require.Len(t, calls, 1, "exactly the agent's command was recorded")
			bad := ivScan(calls, f.env)
			if tc.want == "" {
				assert.Empty(t, bad, "the agent's own worktree is the agent's to write")
				return
			}
			require.NotEmpty(t, bad, "the agent's command went unjudged")
			assert.Contains(t, strings.Join(bad, "\n"), tc.want)
		})
	}

	// Damage the repository half must see too, so this is not only a
	// statement about strings: the agent deletes the user's untracked work.
	require.Empty(t, f.ivRepoViolations(t), "the repository is still where the scenario left it")
	f.git.Reset(t)
	if out, err := f.ivAgentGit(t, f.r.Dir, "clean", "-fdx", "--quiet"); err != nil {
		t.Logf("git clean in the main working tree: %v: %s", err, out)
	}
	assert.Contains(t, strings.Join(ivScan(ivParse(t, f.git), f.env), "\n"), "clean in the main working tree")
	assert.Contains(t, strings.Join(f.ivRepoViolations(t), "\n"), "the user's own uncommitted file is gone",
		"the repository half sees the user's work vanish, not only the command that did it")
}

// TestIvTheGHShimCatchesAPullRequestMerge is the gh half of
// TestIvTheShimCatchesAForbiddenCommand: "gh pr merge" lands work on the
// default branch without running a single git command, so the git checker
// would never see it. It is caught by the gh allow list instead.
func TestIvTheGHShimCatchesAPullRequestMerge(t *testing.T) {
	f := ivSetup(t, ivSafetyOn)
	require.Empty(t, ivScanGH(t, f.gh), "the fixture itself asks gh for nothing forbidden")

	cmd := f.gh.Command(t, "pr", "merge", "12", "--squash", "--delete-branch")
	cmd.Dir = f.r.Dir
	cmd.Env = f.r.Env()
	// The shim refuses the command; that it was asked for at all is the
	// violation.
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("gh pr merge: %v: %s", err, out)
	}

	bad := ivScanGH(t, f.gh)
	require.Len(t, bad, 1, "the forbidden command is caught exactly once")
	assert.Contains(t, bad[0], "landing work on the default branch")
}
