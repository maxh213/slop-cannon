package gitx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/beads"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/testutil"
)

// cuTracker is an in-memory ResumeTracker recording what Resume wrote.
type cuTracker struct {
	mu       sync.Mutex
	statuses map[domain.BeadID]domain.BeadState
	meta     map[domain.BeadID]map[string]any
	closed   map[domain.BeadID]string
	calls    []string
	err      error
	closeErr error
}

func (tr *cuTracker) record(call string) {
	tr.calls = append(tr.calls, call)
}

func (tr *cuTracker) SetStatus(_ context.Context, id domain.BeadID, state domain.BeadState) (beads.Issue, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.record("status " + string(id) + " " + string(state))
	if tr.err != nil {
		return beads.Issue{}, tr.err
	}
	if tr.statuses == nil {
		tr.statuses = map[domain.BeadID]domain.BeadState{}
	}
	tr.statuses[id] = state
	return beads.Issue{ID: id, Status: string(state)}, nil
}

func (tr *cuTracker) Update(_ context.Context, id domain.BeadID, p beads.Patch) (beads.Issue, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.record("update " + string(id))
	if tr.err != nil {
		return beads.Issue{}, tr.err
	}
	if tr.meta == nil {
		tr.meta = map[domain.BeadID]map[string]any{}
	}
	if tr.meta[id] == nil {
		tr.meta[id] = map[string]any{}
	}
	for k, v := range p.Metadata {
		tr.meta[id][k] = v
	}
	return beads.Issue{ID: id}, nil
}

func (tr *cuTracker) Close(_ context.Context, id domain.BeadID, reason string) (beads.Issue, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.record("close " + string(id))
	if tr.err != nil {
		return beads.Issue{}, tr.err
	}
	if tr.closeErr != nil {
		return beads.Issue{}, tr.closeErr
	}
	if tr.closed == nil {
		tr.closed = map[domain.BeadID]string{}
	}
	tr.closed[id] = reason
	return beads.Issue{ID: id, Status: "closed", CloseReason: reason}, nil
}

// cuBead builds a bead in a state.
func cuBead(id, title string, state domain.BeadState) domain.Bead {
	return domain.Bead{ID: domain.BeadID(id), Title: title, State: state}
}

// cuWork creates the bead's worktree and commits one file in it,
// returning the commit.
func cuWork(t *testing.T, f *wtFixture, bead domain.Bead) string {
	t.Helper()
	ctx := context.Background()
	_, err := f.m.Create(ctx, bead)
	require.NoError(t, err)
	wtRepo, err := f.m.Open(ctx, bead.ID)
	require.NoError(t, err)
	acWrite(t, wtRepo.Root, "work/"+string(bead.ID)+".txt", "done by "+string(bead.ID)+"\n")
	c, err := wtRepo.CommitAll(ctx, "work for "+string(bead.ID))
	require.NoError(t, err)
	require.True(t, c.Committed)
	return c.SHA
}

// cuLand fast-forwards main to the bead's branch tip in the main
// repository, the way a delivery that crashed right after its merge stage
// leaves things.
func cuLand(t *testing.T, f *wtFixture, bead domain.Bead) string {
	t.Helper()
	f.r.Git(t, "merge", "--ff-only", "-q", BranchName(bead))
	return f.r.Head(t)
}

// cuConflict leaves the bead's worktree mid-rebase with a conflict in
// README.md.
func cuConflict(t *testing.T, f *wtFixture, bead domain.Bead) {
	t.Helper()
	ctx := context.Background()
	wtRepo, err := f.m.Open(ctx, bead.ID)
	require.NoError(t, err)
	acWrite(t, wtRepo.Root, "README.md", "branch version\n")
	_, err = wtRepo.CommitAll(ctx, "branch README")
	require.NoError(t, err)
	f.r.WriteFile(t, "README.md", "main version\n")
	f.r.Commit(t, "main README")
	_, err = wtRepo.Git(ctx, "-c", "commit.gpgsign=false", "rebase", "--quiet", "main")
	require.Error(t, err)
	require.True(t, wtRepo.RebaseInProgress())
}

// cuEntry finds the cleanup entry of a bead.
func cuEntry(t *testing.T, res *CleanupResult, id domain.BeadID) CleanupEntry {
	t.Helper()
	for _, e := range res.Entries {
		if e.BeadID == id {
			return e
		}
	}
	t.Fatalf("no cleanup entry for %s in %+v", id, res.Entries)
	return CleanupEntry{}
}

// cuRead reads a file under dir.
func cuRead(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	require.NoError(t, err)
	return string(b)
}

// cuResumeEntry finds the resume entry of a bead.
func cuResumeEntry(t *testing.T, res *ResumeResult, id domain.BeadID) ResumeEntry {
	t.Helper()
	for _, e := range res.Entries {
		if e.Bead.ID == id {
			return e
		}
	}
	t.Fatalf("no resume entry for %s", id)
	return ResumeEntry{}
}

// cuResumer builds a resumer for a fresh manager (simulated restart) with
// a quiet logger.
func cuResumer(t *testing.T, m *Manager, tracker ResumeTracker, opts ...ResumeOption) *Resumer {
	t.Helper()
	opts = append([]ResumeOption{WithResumeLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))}, opts...)
	r, err := NewResumer(m, tracker, opts...)
	require.NoError(t, err)
	return r
}

// cuGH installs a gh shim that answers the two queries resume makes.
// "pr list" prints the control file prs.json and "pr view <n>" the control
// file pr-<n>.json (a missing one is an unknown pull request), both written
// with dpSet. The "mode" control file forces a failure: "noauth" and
// "nohost" are classified by ghUnavailable, "network" is not.
func cuGH(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, "gh", `
mode=$(cat "$SC_FAKEBIN_DIR/mode" 2>/dev/null || echo ok)
case "$mode" in
  noauth) echo "You are not logged into any GitHub hosts. To log in, run:  gh auth login" >&2; exit 4 ;;
  nohost) echo "none of the git remotes configured for this repository point to a known GitHub host. To tell gh about a new GitHub host, please use 'gh auth login'" >&2; exit 1 ;;
  network) echo "HTTP 502: bad gateway" >&2; exit 1 ;;
esac
case "$1 $2" in
  "pr list") cat "$SC_FAKEBIN_DIR/prs.json" 2>/dev/null || echo '[]' ;;
  "pr view") cat "$SC_FAKEBIN_DIR/pr-$3.json" 2>/dev/null || { echo "no pull requests found for \"$3\"" >&2; exit 1; } ;;
  *) echo "gh shim: unexpected command: $*" >&2; exit 1 ;;
esac
`)
}

// cuGit installs a git shim that runs the real git for everything except
// the failures named with cuFail, so a single ref read can be broken while
// the rest of a cleanup or resume pass runs for real.
func cuGit(t *testing.T) *testutil.Shim {
	t.Helper()
	return testutil.FakeBin(t, "git", `
specs=""
if [ -f "$SC_FAKEBIN_DIR/fail" ]; then specs=$(cat "$SC_FAKEBIN_DIR/fail"); fi
for spec in $specs; do
  key=${spec%%:*}
  val=${spec#*:}
  if [ "$key" = "$spec" ]; then val=""; fi
  case "$key" in
    show-ref)
      if [ "$1" = show-ref ]; then
        for a in "$@"; do
          if [ "$a" = "refs/heads/$val" ]; then echo "fatal: unable to read the ref store" >&2; exit 128; fi
        done
      fi ;;
    rev-parse)
      if [ "$1" = rev-parse ]; then
        for a in "$@"; do
          if [ "$a" = "refs/heads/$val^{commit}" ]; then echo "fatal: unable to read the object store" >&2; exit 128; fi
        done
      fi ;;
    is-ancestor)
      if [ "$1" = merge-base ]; then echo "fatal: unable to walk the commit graph" >&2; exit 128; fi ;;
    rev-list)
      if [ "$1" = rev-list ]; then echo "not a number"; exit 0; fi ;;
    rev-list-fail)
      if [ "$1" = rev-list ]; then echo "fatal: unable to walk the commit graph" >&2; exit 128; fi ;;
    toplevel)
      if [ "$1" = rev-parse ] && [ "$2" = --show-toplevel ]; then echo "fatal: unable to read the repository" >&2; exit 128; fi ;;
    diff)
      if [ "$1" = diff ]; then echo "fatal: unable to read the index" >&2; exit 128; fi ;;
    status)
      if [ "$1" = status ]; then echo "fatal: unable to read the index" >&2; exit 128; fi ;;
    branch-D)
      if [ "$1" = branch ]; then echo "fatal: cannot lock ref" >&2; exit 128; fi ;;
    for-each-ref)
      if [ "$1" = for-each-ref ]; then echo "fatal: unable to read the ref store" >&2; exit 128; fi ;;
    worktree-list)
      if [ "$1" = worktree ] && [ "$2" = list ]; then
        n=$(cat "$SC_FAKEBIN_DIR/wtcount" 2>/dev/null || echo 0)
        n=$((n + 1))
        echo "$n" > "$SC_FAKEBIN_DIR/wtcount"
        if [ "$n" = "$val" ]; then echo "fatal: unable to read the worktree list" >&2; exit 128; fi
      fi ;;
  esac
done
exec "$SC_FAKEBIN_REAL" "$@"
`)
}

// cuFail arms the git shim with the failures to inject from now on, given
// as whitespace-separated specs, and restarts its call counter:
//
//	show-ref:<branch>    reading that branch's ref fails
//	rev-parse:<branch>   resolving that branch's tip fails
//	is-ancestor          every "git merge-base --is-ancestor" fails
//	rev-list             "git rev-list" prints something that is not a count
//	rev-list-fail        "git rev-list" fails
//	toplevel             "git rev-parse --show-toplevel" fails, so Open does
//	diff                 "git diff" fails, so ConflictedPaths does
//	status               "git status" fails, so IsClean does
//	branch-D             every "git branch" call fails
//	for-each-ref         listing the bead's branch namespace fails
//	worktree-list:<n>    the nth "git worktree list" after this call fails
func cuFail(t *testing.T, git *testutil.Shim, specs ...string) {
	t.Helper()
	_ = os.Remove(filepath.Join(git.Dir, "wtcount"))
	require.NoError(t, os.WriteFile(filepath.Join(git.Dir, "fail"), []byte(strings.Join(specs, "\n")), 0o644))
}

func TestManager_Cleanup(t *testing.T) {
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("finish with mixed states", func(t *testing.T) {
		f := wtSetup(t, nil)
		closedMerged := cuBead("fx-c1", "closed and merged", domain.BeadStateClosed)
		closedUnmerged := cuBead("fx-c2", "closed but never merged", domain.BeadStateClosed)
		closedDirty := cuBead("fx-c3", "closed with leftovers", domain.BeadStateClosed)
		closedNoWT := cuBead("fx-c4", "closed, worktree already gone", domain.BeadStateClosed)
		closedWiped := cuBead("fx-c5", "closed, worktree directory wiped", domain.BeadStateClosed)
		closedRetitled := cuBead("fx-c6", "the title it was created with", domain.BeadStateClosed)
		failed := cuBead("fx-f1", "failed", domain.BeadStateFailed)
		human := cuBead("fx-h1", "needs a human", domain.BeadStateNeedsHuman)
		inFlight := cuBead("fx-p1", "still in progress", domain.BeadStateInProgress)
		blocked := cuBead("fx-b1", "blocked by bd", domain.BeadStateBlocked)
		unknown := cuBead("fx-u1", "not in the final state", domain.BeadStateInReview)
		vanished := cuBead("fx-v1", "directory wiped", domain.BeadStateFailed)

		cuWork(t, f, closedMerged)
		cuLand(t, f, closedMerged)
		cuWork(t, f, closedUnmerged)
		cuWork(t, f, closedDirty)
		cuLand(t, f, closedDirty)
		require.NoError(t, os.WriteFile(filepath.Join(f.m.PathFor(closedDirty.ID), "scratch.txt"), []byte("wip\n"), 0o644))
		cuWork(t, f, closedNoWT)
		cuLand(t, f, closedNoWT)
		require.NoError(t, f.m.Remove(ctx, closedNoWT.ID, false))
		cuWork(t, f, closedWiped)
		cuLand(t, f, closedWiped)
		require.NoError(t, os.RemoveAll(f.m.PathFor(closedWiped.ID)))
		cuWork(t, f, closedRetitled)
		cuLand(t, f, closedRetitled)
		closedRetitledBranch := BranchName(closedRetitled)
		// The PO renamed the bead after its worktree was created.
		closedRetitled.Title = "renamed after the fact"
		require.NotEqual(t, closedRetitledBranch, BranchName(closedRetitled))
		for _, b := range []domain.Bead{failed, human, inFlight, blocked, unknown, vanished} {
			cuWork(t, f, b)
		}
		require.NoError(t, os.RemoveAll(f.m.PathFor(vanished.ID)))
		require.NoError(t, os.MkdirAll(filepath.Join(f.m.Dir(), "fx-husk"), 0o755))
		mainBefore := f.r.Head(t)

		var printed strings.Builder
		res, err := f.m.Cleanup(ctx, []domain.Bead{closedMerged, closedUnmerged, closedDirty, closedNoWT, closedWiped, closedRetitled, failed, human, inFlight, blocked, vanished}, CleanupOptions{Logger: quiet, Out: &printed})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		assert.Equal(t, "main", res.Default)
		assert.Equal(t, []string{"main"}, res.Bases, "no remote: only the local default branch")

		e := cuEntry(t, res, closedMerged.ID)
		assert.Equal(t, CleanupRemoved, e.Action)
		assert.True(t, e.BranchDeleted)
		assert.Contains(t, e.Reason, "deleted (merged into main)")
		assert.NoDirExists(t, e.Path)

		e = cuEntry(t, res, closedUnmerged.ID)
		assert.Equal(t, CleanupRemoved, e.Action)
		assert.False(t, e.BranchDeleted)
		assert.Contains(t, e.Reason, "kept (not merged into main)")
		ok, err := f.repo.BranchExists(ctx, BranchName(closedUnmerged))
		require.NoError(t, err)
		assert.True(t, ok, "an unmerged branch is never deleted")

		e = cuEntry(t, res, closedDirty.ID)
		assert.Equal(t, CleanupRemoved, e.Action, "leftovers of a closed bead do not stop the removal")
		assert.True(t, e.BranchDeleted)

		e = cuEntry(t, res, closedNoWT.ID)
		assert.Equal(t, CleanupNone, e.Action)
		assert.True(t, e.BranchDeleted, "a merged branch is deleted even without a worktree")

		e = cuEntry(t, res, closedWiped.ID)
		assert.Equal(t, CleanupPruned, e.Action)
		assert.True(t, e.BranchDeleted, "a done bead's merged branch goes even when its directory was wiped")
		assert.Contains(t, e.Reason, "deleted (merged into main)")
		ok, err = f.repo.BranchExists(ctx, BranchName(closedWiped))
		require.NoError(t, err)
		assert.False(t, ok)

		e = cuEntry(t, res, closedRetitled.ID)
		assert.Equal(t, CleanupRemoved, e.Action)
		assert.Equal(t, closedRetitledBranch, e.Branch)
		assert.True(t, e.BranchDeleted, "the branch the worktree held is deleted, not the one the new title names")
		ok, err = f.repo.BranchExists(ctx, closedRetitledBranch)
		require.NoError(t, err)
		assert.False(t, ok)

		for _, b := range []domain.Bead{failed, human, inFlight, blocked} {
			e = cuEntry(t, res, b.ID)
			assert.Equal(t, CleanupKept, e.Action, b.ID)
			assert.Equal(t, b.State, e.State)
			assert.Equal(t, "the bead is "+string(b.State), e.Reason)
			assert.Equal(t, f.m.PathFor(b.ID), e.Path)
			assert.DirExists(t, e.Path)
			assert.False(t, e.BranchDeleted)
		}
		e = cuEntry(t, res, unknown.ID)
		assert.Equal(t, CleanupKept, e.Action)
		assert.Empty(t, e.State)
		assert.Contains(t, e.Reason, "not part of this run")
		e = cuEntry(t, res, vanished.ID)
		assert.Equal(t, CleanupPruned, e.Action)
		assert.False(t, e.BranchDeleted, "a failed bead keeps its branch even when its directory is gone")
		ok, err = f.repo.BranchExists(ctx, BranchName(vanished))
		require.NoError(t, err)
		assert.True(t, ok)

		assert.Equal(t, []string{
			f.m.PathFor(blocked.ID), f.m.PathFor(failed.ID), f.m.PathFor(human.ID), f.m.PathFor(inFlight.ID), f.m.PathFor(unknown.ID),
		}, res.KeptPaths(), "sorted by bead id")
		assert.Len(t, res.Removed(), 4)
		assert.Empty(t, res.Failed())

		summary := res.Summary()
		assert.True(t, strings.HasPrefix(summary, "removed 4 worktrees, pruned 2 stale registrations, kept 5:"), summary)
		assert.Contains(t, summary, "\n  fx-b1 (blocked): "+f.m.PathFor(blocked.ID))
		assert.Contains(t, summary, "\n  fx-f1 (failed): "+f.m.PathFor(failed.ID))
		assert.Contains(t, summary, "\n  fx-h1 (needs_human): "+f.m.PathFor(human.ID))
		assert.Contains(t, summary, "\n  fx-u1 (unknown): "+f.m.PathFor(unknown.ID))
		assert.NotContains(t, summary, "failure")
		assert.Equal(t, summary+"\n", printed.String(), "the kept paths are printed for the run's final report")

		// Housekeeping: the vanished registration and the husk are gone,
		// the kept worktrees are still registered, main is untouched.
		list, err := f.m.List(ctx)
		require.NoError(t, err)
		require.Len(t, list, 5)
		for _, wt := range list {
			assert.False(t, wt.Prunable, wt.BeadID)
		}
		assert.NoDirExists(t, filepath.Join(f.m.Dir(), "fx-husk"))
		assert.Equal(t, mainBefore, f.r.Head(t))
		assert.Empty(t, f.r.Status(t))

		// A second pass is a no-op apart from the kept worktrees.
		again, err := f.m.Cleanup(ctx, []domain.Bead{closedMerged, failed}, CleanupOptions{Logger: quiet})
		require.NoError(t, err)
		assert.Empty(t, again.Removed())
		assert.Len(t, again.Kept(), 5)
	})

	t.Run("KeepBranches leaves merged branches alone", func(t *testing.T) {
		f := wtSetup(t, nil)
		bead := cuBead("fx-c1", "closed", domain.BeadStateClosed)
		cuWork(t, f, bead)
		cuLand(t, f, bead)
		res, err := f.m.Cleanup(ctx, []domain.Bead{bead}, CleanupOptions{KeepBranches: true, Logger: quiet})
		require.NoError(t, err)
		e := cuEntry(t, res, bead.ID)
		assert.Equal(t, CleanupRemoved, e.Action)
		assert.False(t, e.BranchDeleted)
		assert.Contains(t, e.Reason, "branch kept as requested")
		ok, err := f.repo.BranchExists(ctx, BranchName(bead))
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("no default branch keeps branches", func(t *testing.T) {
		f := wtSetup(t, nil)
		bead := cuBead("fx-c1", "closed", domain.BeadStateClosed)
		cuWork(t, f, bead)
		f.r.Git(t, "checkout", "-q", "-b", "trunk")
		f.r.Git(t, "branch", "-D", "-q", "main")
		res, err := f.m.Cleanup(ctx, []domain.Bead{bead}, CleanupOptions{Logger: quiet})
		require.NoError(t, err)
		assert.Empty(t, res.Default)
		assert.Empty(t, res.Bases)
		e := cuEntry(t, res, bead.ID)
		assert.Equal(t, CleanupRemoved, e.Action)
		assert.False(t, e.BranchDeleted)
		assert.Contains(t, e.Reason, "no default branch")
	})

	t.Run("nothing to do", func(t *testing.T) {
		f := wtSetup(t, nil)
		res, err := f.m.Cleanup(ctx, []domain.Bead{cuBead("fx-1", "never started", domain.BeadStateOpen)}, CleanupOptions{Logger: quiet})
		require.NoError(t, err)
		assert.Empty(t, res.Entries)
		assert.Equal(t, "removed 0 worktrees", res.Summary())
		assert.NoError(t, res.Err())
	})

	t.Run("a branch merged only on the remote is deleted", func(t *testing.T) {
		f := wtSetup(t, []testutil.FixtureOption{testutil.WithBareRemote()})
		landed := cuBead("fx-c1", "delivered with safety on", domain.BeadStateClosed)
		unmerged := cuBead("fx-c2", "closed with the PR still open", domain.BeadStateClosed)
		cuWork(t, f, landed)
		cuWork(t, f, unmerged)
		// The pull request merged on the forge: the remote default branch
		// has the work, the local one does not.
		f.r.Git(t, "push", "-q", "origin", BranchName(landed)+":main")
		f.r.Git(t, "fetch", "-q", "origin")
		localMain := f.r.Head(t)

		res, err := f.m.Cleanup(ctx, []domain.Bead{landed, unmerged}, CleanupOptions{Logger: quiet})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		assert.Equal(t, "main", res.Default)
		assert.Equal(t, []string{"origin/main", "main"}, res.Bases, "the remote-tracking branch is checked first")

		e := cuEntry(t, res, landed.ID)
		assert.Equal(t, CleanupRemoved, e.Action)
		assert.True(t, e.BranchDeleted)
		assert.Contains(t, e.Reason, "deleted (merged into origin/main)")
		ok, err := f.repo.BranchExists(ctx, BranchName(landed))
		require.NoError(t, err)
		assert.False(t, ok)

		e = cuEntry(t, res, unmerged.ID)
		assert.Equal(t, CleanupRemoved, e.Action)
		assert.False(t, e.BranchDeleted)
		assert.Contains(t, e.Reason, "kept (not merged into origin/main or main)")
		ok, err = f.repo.BranchExists(ctx, BranchName(unmerged))
		require.NoError(t, err)
		assert.True(t, ok)

		assert.Equal(t, localMain, f.r.Head(t), "cleanup never moves the default branch")
		assert.Empty(t, f.r.Status(t))
	})

	t.Run("a per-bead ref failure does not abort the pass", func(t *testing.T) {
		f := wtSetup(t, nil)
		git := cuGit(t)
		broken := cuBead("fx-c1", "closed, its ref unreadable", domain.BeadStateClosed)
		merged := cuBead("fx-c2", "closed and merged", domain.BeadStateClosed)
		kept := cuBead("fx-f1", "failed", domain.BeadStateFailed)
		cuWork(t, f, broken)
		cuLand(t, f, broken)
		require.NoError(t, f.m.Remove(ctx, broken.ID, false))
		cuWork(t, f, merged)
		cuLand(t, f, merged)
		cuWork(t, f, kept)

		cuFail(t, git, "show-ref:"+BranchName(broken))
		res, err := f.m.Cleanup(ctx, []domain.Bead{broken, merged, kept}, CleanupOptions{Logger: quiet})
		require.NoError(t, err, "one unreadable ref is a per-bead failure, not the end of the pass")

		e := cuEntry(t, res, broken.ID)
		assert.Equal(t, CleanupNone, e.Action)
		assert.Equal(t, BranchName(broken), e.Branch)
		assert.Contains(t, e.Reason, "could not be inspected")
		assert.ErrorContains(t, e.Err, "unable to read the ref store")

		// Everything else was still processed.
		e = cuEntry(t, res, merged.ID)
		assert.Equal(t, CleanupRemoved, e.Action)
		assert.True(t, e.BranchDeleted)
		assert.NoError(t, e.Err)
		e = cuEntry(t, res, kept.ID)
		assert.Equal(t, CleanupKept, e.Action)
		assert.NoError(t, e.Err)
		assert.DirExists(t, e.Path)

		assert.ErrorContains(t, res.Err(), "fx-c1")
		assert.Contains(t, res.Summary(), "1 failure:")
	})

	t.Run("a branch that cannot be checked or deleted is reported", func(t *testing.T) {
		merged := cuBead("fx-c1", "closed and merged", domain.BeadStateClosed)
		for name, spec := range map[string]string{
			"the branch check fails":   "show-ref:" + BranchName(merged),
			"the ancestor check fails": "is-ancestor",
			"the deletion fails":       "branch-D",
		} {
			t.Run(name, func(t *testing.T) {
				f := wtSetup(t, nil)
				git := cuGit(t)
				bead := merged
				cuWork(t, f, bead)
				cuLand(t, f, bead)
				cuFail(t, git, spec)
				res, err := f.m.Cleanup(ctx, []domain.Bead{bead}, CleanupOptions{Logger: quiet})
				require.NoError(t, err)
				e := cuEntry(t, res, bead.ID)
				assert.Equal(t, CleanupRemoved, e.Action, "the worktree still goes")
				assert.False(t, e.BranchDeleted)
				assert.Error(t, e.Err)
				assert.ErrorContains(t, res.Err(), "fx-c1")
				cuFail(t, git)
				ok, err := f.repo.BranchExists(ctx, BranchName(bead))
				require.NoError(t, err)
				assert.True(t, ok, "the branch is kept when the outcome is unknown")
			})
		}
	})

	t.Run("a closed bead whose branch is already gone", func(t *testing.T) {
		f := wtSetup(t, nil)
		bead := cuBead("fx-c1", "closed", domain.BeadStateClosed)
		cuWork(t, f, bead)
		// The engineer left the worktree on a detached HEAD and the branch
		// was deleted by hand before the run finished.
		wtRepo, err := f.m.Open(ctx, bead.ID)
		require.NoError(t, err)
		_, err = wtRepo.Git(ctx, "checkout", "--quiet", "--detach")
		require.NoError(t, err)
		requireDeleteBranch(t, f.m, bead)

		res, err := f.m.Cleanup(ctx, []domain.Bead{bead}, CleanupOptions{Logger: quiet})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		e := cuEntry(t, res, bead.ID)
		assert.Equal(t, CleanupRemoved, e.Action)
		assert.False(t, e.BranchDeleted)
		assert.Contains(t, e.Reason, "is already gone", "the report never claims a branch was kept or deleted")
	})

	t.Run("a closed bead whose branch cannot even be named", func(t *testing.T) {
		f := wtSetup(t, nil)
		git := cuGit(t)
		bead := cuBead("fx-c1", "closed", domain.BeadStateClosed)
		cuWork(t, f, bead)
		// A detached worktree names no branch, so cleanup has to resolve
		// one from the bead's namespace, and that read fails.
		wtRepo, err := f.m.Open(ctx, bead.ID)
		require.NoError(t, err)
		_, err = wtRepo.Git(ctx, "checkout", "--quiet", "--detach")
		require.NoError(t, err)
		requireDeleteBranch(t, f.m, bead)
		cuFail(t, git, "for-each-ref")

		res, err := f.m.Cleanup(ctx, []domain.Bead{bead}, CleanupOptions{Logger: quiet})
		require.NoError(t, err)
		e := cuEntry(t, res, bead.ID)
		assert.Equal(t, CleanupRemoved, e.Action)
		assert.False(t, e.BranchDeleted)
		assert.ErrorContains(t, e.Err, "unable to read the ref store")
	})

	t.Run("a retitled closed bead without a worktree still loses its merged branch", func(t *testing.T) {
		f := wtSetup(t, nil)
		bead := cuBead("fx-c1", "the title it was created with", domain.BeadStateClosed)
		cuWork(t, f, bead)
		cuLand(t, f, bead)
		branch := BranchName(bead)
		require.NoError(t, f.m.Remove(ctx, bead.ID, false))
		bead.Title = "renamed after the fact"
		require.NotEqual(t, branch, BranchName(bead))

		res, err := f.m.Cleanup(ctx, []domain.Bead{bead}, CleanupOptions{Logger: quiet})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		e := cuEntry(t, res, bead.ID)
		assert.Equal(t, CleanupNone, e.Action)
		assert.Equal(t, branch, e.Branch, "the branch the bead's work is on, not the one its new title names")
		assert.True(t, e.BranchDeleted)
		ok, err := f.repo.BranchExists(ctx, branch)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("the user's own worktrees keep their registrations", func(t *testing.T) {
		f := wtSetup(t, nil)
		theirs := f.wtUserWorktree(t)
		require.NoError(t, os.RemoveAll(theirs))
		wiped := cuBead("fx-c1", "closed, directory wiped", domain.BeadStateClosed)
		cuWork(t, f, wiped)
		cuLand(t, f, wiped)
		require.NoError(t, os.RemoveAll(f.m.PathFor(wiped.ID)))

		res, err := f.m.Cleanup(ctx, []domain.Bead{wiped}, CleanupOptions{Logger: quiet})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		assert.Equal(t, CleanupPruned, cuEntry(t, res, wiped.ID).Action, "the run's own stale registration is dropped")

		list := f.r.Git(t, "worktree", "list", "--porcelain")
		assert.Contains(t, list, "users-own", "cleanup never prunes worktrees the user keeps outside the run (D-23)")
		assert.Contains(t, list, "feature/theirs")
	})

	t.Run("a failed removal is reported and the worktree kept", func(t *testing.T) {
		f := wtSetup(t, nil)
		bead := cuBead("fx-c1", "closed", domain.BeadStateClosed)
		if os.Geteuid() == 0 {
			t.Skip("root can delete anything")
		}
		cuWork(t, f, bead)
		// An unwritable directory inside the worktree makes the removal
		// fail half-way.
		locked := filepath.Join(f.m.PathFor(bead.ID), "locked")
		require.NoError(t, os.MkdirAll(locked, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(locked, "pin"), []byte("x"), 0o644))
		require.NoError(t, os.Chmod(locked, 0o500))
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		res, err := f.m.Cleanup(ctx, []domain.Bead{bead}, CleanupOptions{Logger: quiet})
		require.NoError(t, err)
		e := cuEntry(t, res, bead.ID)
		assert.Equal(t, CleanupKept, e.Action)
		assert.Error(t, e.Err)
		assert.ErrorContains(t, res.Err(), "fx-c1")
		assert.Contains(t, res.Summary(), "1 failure:")
		assert.DirExists(t, e.Path)
	})
}

func TestManager_resolveBranch(t *testing.T) {
	ctx := context.Background()

	t.Run("the registered worktree branch wins over the title", func(t *testing.T) {
		f := wtSetup(t, nil)
		bead := cuBead("fx-d1", "the title it was created with", domain.BeadStateInProgress)
		cuWork(t, f, bead)
		created := BranchName(bead)
		bead.Title = "a much better title"
		branch, exists, err := f.m.resolveBranch(ctx, bead, created)
		require.NoError(t, err)
		assert.Equal(t, created, branch)
		assert.True(t, exists)
	})

	t.Run("the title names the branch when it exists", func(t *testing.T) {
		f := wtSetup(t, nil)
		bead := cuBead("fx-d1", "untouched title", domain.BeadStateInProgress)
		cuWork(t, f, bead)
		branch, exists, err := f.m.resolveBranch(ctx, bead, "")
		require.NoError(t, err)
		assert.Equal(t, BranchName(bead), branch)
		assert.True(t, exists)
	})

	t.Run("the bead's namespace is searched when the title stops naming a branch", func(t *testing.T) {
		f := wtSetup(t, nil)
		bead := cuBead("fx-d1", "the title it was created with", domain.BeadStateInProgress)
		cuWork(t, f, bead)
		created := BranchName(bead)
		// Neighbours that must never be mistaken for this bead's branch.
		f.r.Git(t, "branch", "-q", "sc/fx-d10-another-bead", "main")
		f.r.Git(t, "branch", "-q", "sc/fx-d1x-yet-another", "main")
		bead.Title = "a much better title"
		branch, exists, err := f.m.resolveBranch(ctx, bead, "")
		require.NoError(t, err)
		assert.Equal(t, created, branch)
		assert.True(t, exists)
	})

	t.Run("an ambiguous namespace falls back to the title", func(t *testing.T) {
		f := wtSetup(t, nil)
		bead := cuBead("fx-d1", "a much better title", domain.BeadStateInProgress)
		f.r.Git(t, "branch", "-q", "sc/fx-d1-one-title", "main")
		f.r.Git(t, "branch", "-q", "sc/fx-d1-another-title", "main")
		branch, exists, err := f.m.resolveBranch(ctx, bead, "")
		require.NoError(t, err)
		assert.Equal(t, BranchName(bead), branch, "guessing between two candidates would be worse than saying nothing")
		assert.False(t, exists)
	})

	t.Run("nothing at all reports the title's branch as missing", func(t *testing.T) {
		f := wtSetup(t, nil)
		bead := cuBead("fx-d1", "never started", domain.BeadStateInProgress)
		branch, exists, err := f.m.resolveBranch(ctx, bead, "")
		require.NoError(t, err)
		assert.Equal(t, BranchName(bead), branch)
		assert.False(t, exists)
	})

	t.Run("a ref failure is reported with a usable name", func(t *testing.T) {
		f := wtSetup(t, nil)
		git := cuGit(t)
		bead := cuBead("fx-d1", "unreadable", domain.BeadStateInProgress)
		cuFail(t, git, "show-ref:"+BranchName(bead))
		branch, exists, err := f.m.resolveBranch(ctx, bead, "")
		require.Error(t, err)
		assert.Equal(t, BranchName(bead), branch)
		assert.False(t, exists)
	})
}

// TestManager_BranchFor is P4-12: every gitx helper addresses the branch a
// bead's work is really on, not the one its (mutable) title spells. The
// product owner retitles a bead mid-run and the branch, the worktree, the
// commits on it and the deletion must all still follow the work.
func TestManager_BranchFor(t *testing.T) {
	ctx := context.Background()
	f := wtSetup(t, nil)
	bead := cuBead("fx-d1", "the title it was created with", domain.BeadStateInProgress)
	sha := cuWork(t, f, bead)
	original := BranchName(bead)

	retitled := bead
	retitled.Title = "a much better title the PO thought of later"
	require.NotEqual(t, original, BranchName(retitled), "the new title names a different branch")

	t.Run("while the worktree is registered", func(t *testing.T) {
		branch, exists, err := f.m.BranchFor(ctx, retitled)
		require.NoError(t, err)
		assert.Equal(t, original, branch, "git's own registration names the branch")
		assert.True(t, exists)
	})

	require.NoError(t, f.m.Remove(ctx, bead.ID, true))

	t.Run("after the worktree is gone", func(t *testing.T) {
		branch, exists, err := f.m.BranchFor(ctx, retitled)
		require.NoError(t, err)
		assert.Equal(t, original, branch, "the sc/<bead-id> namespace still names it")
		assert.True(t, exists)
	})

	t.Run("Create checks the work out instead of cutting a new branch", func(t *testing.T) {
		wt, err := f.m.Create(ctx, retitled)
		require.NoError(t, err)
		assert.Equal(t, original, wt.Branch)
		assert.Equal(t, sha, wt.Head, "the commits are still there")
		assert.Empty(t, f.r.Git(t, "for-each-ref", "--format=%(refname:short)", "refs/heads/"+BranchName(retitled)),
			"no branch was cut under the new title")
	})

	t.Run("DeleteBranch deletes the branch the work is on", func(t *testing.T) {
		require.NoError(t, f.m.Remove(ctx, bead.ID, true))
		deleted, err := f.m.DeleteBranch(ctx, retitled)
		require.NoError(t, err)
		assert.Equal(t, original, deleted)
		exists, err := f.repo.BranchExists(ctx, original)
		require.NoError(t, err)
		assert.False(t, exists)

		deleted, err = f.m.DeleteBranch(ctx, retitled)
		require.NoError(t, err, "a bead with no branch left is not an error")
		assert.Empty(t, deleted)
	})

	t.Run("a bead that never had one gets its title's branch", func(t *testing.T) {
		fresh := cuBead("fx-d9", "never started", domain.BeadStateOpen)
		branch, exists, err := f.m.BranchFor(ctx, fresh)
		require.NoError(t, err)
		assert.Equal(t, BranchName(fresh), branch)
		assert.False(t, exists)
		wt, err := f.m.Create(ctx, fresh)
		require.NoError(t, err)
		assert.Equal(t, BranchName(fresh), wt.Branch)
	})
}

func TestPRNumberFromURL(t *testing.T) {
	for url, want := range map[string]int{
		"https://github.com/acme/widgets/pull/5":              5,
		"https://github.com/acme/widgets/pull/512/files":      512,
		"https://github.com/acme/widgets/pull/7#issuecomment": 7,
		"https://github.com/acme/widgets/pull/7?w=1":          7,
		"https://github.com/acme/widgets/issues/5":            0,
		"https://github.com/acme/widgets/pull/none":           0,
		"https://github.com/acme/widgets/pull/0":              0,
		"":                                                    0,
	} {
		assert.Equal(t, want, prNumberFromURL(url), url)
	}
}

func TestGH_prByNumber(t *testing.T) {
	ctx := context.Background()
	f := wtSetup(t, []testutil.FixtureOption{testutil.WithBareRemote()})
	shim := cuGH(t)
	gh := NewGH(f.repo)

	dpSet(t, shim, "pr-5.json", `{"number":5,"url":"https://github.com/acme/widgets/pull/5","state":"open","title":"x","headRefName":"sc/fx-d1-work"}`)
	pr, state, err := gh.prByNumber(ctx, 5)
	require.NoError(t, err)
	require.NotNil(t, pr)
	assert.Equal(t, "OPEN", state, "gh's casing is not the caller's problem")
	assert.Equal(t, "sc/fx-d1-work", pr.HeadRef)

	dpSet(t, shim, "pr-6.json", `{"number":6,"url":"u","state":"CLOSED","title":"x","headRefName":"b"}`)
	pr, state, err = gh.prByNumber(ctx, 6)
	require.NoError(t, err)
	assert.Nil(t, pr, "a pull request closed without merging is as good as none")
	assert.Empty(t, state)

	dpSet(t, shim, "pr-7.json", `not json`)
	_, _, err = gh.prByNumber(ctx, 7)
	assert.ErrorContains(t, err, "unexpected output")

	_, _, err = gh.prByNumber(ctx, 8)
	assert.ErrorContains(t, err, "no pull requests found", "an unknown number is an error, never a silent \"gone\"")

	_, _, err = gh.prByNumber(ctx, 0)
	assert.ErrorContains(t, err, "not a pull request number")
}

func TestNewResumer(t *testing.T) {
	f := wtSetup(t, nil)
	_, err := NewResumer(nil, nil)
	require.Error(t, err)
	r, err := NewResumer(f.m, nil)
	require.NoError(t, err)
	assert.Same(t, f.repo, r.gh.Repo())
	gh := NewGH(f.repo)
	r, err = NewResumer(f.m, nil, WithResumeGH(gh), WithResumeGH(nil), WithResumeLogger(nil))
	require.NoError(t, err)
	assert.Same(t, gh, r.gh)
	assert.NotNil(t, r.logger)
}

func TestResumer_Resume(t *testing.T) {
	ctx := context.Background()

	t.Run("after a simulated crash without a remote", func(t *testing.T) {
		f := wtSetup(t, nil)
		landed := cuBead("fx-d1", "delivered before the crash", domain.BeadStateDelivering)
		undelivered := cuBead("fx-d2", "delivery never ran", domain.BeadStateDelivering)
		branchGone := cuBead("fx-d3", "branch deleted by hand", domain.BeadStateDelivering)
		retitled := cuBead("fx-d4", "the title it was created with", domain.BeadStateDelivering)
		wiped := cuBead("fx-p1", "worktree wiped mid-flight", domain.BeadStateInProgress)
		fresh := cuBead("fx-p2", "claimed, nothing created yet", domain.BeadStateInProgress)
		conflicted := cuBead("fx-r1", "mid-rebase", domain.BeadStateInReview)
		dirty := cuBead("fx-r2", "uncommitted edits", domain.BeadStateChangesRequested)
		done := cuBead("fx-x1", "closed, worktree not yet cleaned", domain.BeadStateClosed)
		failed := cuBead("fx-x2", "failed without a worktree", domain.BeadStateFailed)
		orphan := cuBead("fx-o1", "from another list", domain.BeadStateInProgress)

		landedSHA := cuWork(t, f, landed)
		cuLand(t, f, landed)
		undeliveredSHA := cuWork(t, f, undelivered)
		cuWork(t, f, branchGone)
		require.NoError(t, f.m.Remove(ctx, branchGone.ID, false))
		requireDeleteBranch(t, f.m, branchGone)
		retitledSHA := cuWork(t, f, retitled)
		retitledBranch := BranchName(retitled)
		// The PO rewrote the bead's title after the worktree was created, so
		// BranchName(bead) no longer names the branch the work is on.
		retitled.Title = "a much better title"
		require.NotEqual(t, retitledBranch, BranchName(retitled))
		wipedSHA := cuWork(t, f, wiped)
		require.NoError(t, os.RemoveAll(f.m.PathFor(wiped.ID)))
		cuWork(t, f, conflicted)
		cuConflict(t, f, conflicted)
		cuWork(t, f, dirty)
		require.NoError(t, os.WriteFile(filepath.Join(f.m.PathFor(dirty.ID), "edit.txt"), []byte("wip\n"), 0o644))
		cuWork(t, f, done)
		cuWork(t, f, orphan)
		mainHead := f.r.Head(t)

		// Restart: a fresh manager built only from the paths.
		m2 := f.wtReopen(t)
		tracker := &cuTracker{}
		r := cuResumer(t, m2, tracker)
		beadList := []domain.Bead{landed, undelivered, branchGone, retitled, wiped, fresh, conflicted, dirty, done, failed}
		res, err := r.Resume(ctx, beadList)
		require.NoError(t, err)
		require.NoError(t, res.Err(), res.Summary())
		assert.Equal(t, "main", res.Default)
		assert.Equal(t, "main", res.Base)
		assert.False(t, res.Fetched)
		assert.Empty(t, res.Warnings)
		require.Len(t, res.Entries, len(beadList))
		for i, b := range beadList {
			assert.Equal(t, b.ID, res.Entries[i].Bead.ID, "caller's order")
		}

		// Delivered before the crash: closed with sc_merge_sha.
		e := cuResumeEntry(t, res, landed.ID)
		assert.True(t, e.Landed)
		assert.Equal(t, 0, e.Ahead)
		assert.Equal(t, landedSHA, e.Head)
		assert.Equal(t, domain.BeadStateClosed, e.State)
		assert.True(t, e.Changed())
		assert.True(t, e.Applied)
		assert.Equal(t, landedSHA, e.MergeSHA)
		assert.Equal(t, WorktreeAttached, e.WorktreeStatus)
		assert.Contains(t, e.Reason, "landed on main")
		assert.Equal(t, map[string]any{beads.MetaMergeSHA: landedSHA}, tracker.meta[landed.ID])
		assert.Contains(t, tracker.closed[landed.ID], "merged into main as "+landedSHA[:12])

		// Delivery never ran: still delivering, worktree reattached.
		e = cuResumeEntry(t, res, undelivered.ID)
		assert.False(t, e.Landed)
		assert.Equal(t, 1, e.Ahead)
		assert.Equal(t, undeliveredSHA, e.Head)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.False(t, e.Changed())
		assert.False(t, e.Applied)
		assert.Equal(t, WorktreeAttached, e.WorktreeStatus)
		require.NotNil(t, e.Worktree)
		assert.Equal(t, m2.PathFor(undelivered.ID), e.Worktree.Path)
		assert.Contains(t, e.Reason, "deliver again")

		// Branch gone with nothing landed: a human must look.
		e = cuResumeEntry(t, res, branchGone.ID)
		assert.False(t, e.BranchExists)
		assert.Equal(t, domain.BeadStateNeedsHuman, e.State)
		assert.True(t, e.Applied)
		assert.Equal(t, WorktreeMissing, e.WorktreeStatus)
		assert.Equal(t, domain.BeadStateNeedsHuman, tracker.statuses[branchGone.ID])

		// Retitled since the worktree was created: the branch the worktree
		// holds is reconciled, not the one the new title would name.
		e = cuResumeEntry(t, res, retitled.ID)
		assert.Equal(t, WorktreeAttached, e.WorktreeStatus)
		assert.Equal(t, retitledBranch, e.Branch)
		assert.True(t, e.BranchExists, "a retitled bead's work is not reported as a vanished branch")
		assert.Equal(t, retitledSHA, e.Head)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.False(t, e.Changed(), "a retitled bead is never handed to a human for a branch that is right there")
		assert.Contains(t, e.Reason, "deliver again")

		// Wiped worktree of an in-flight bead: recreated from its branch.
		e = cuResumeEntry(t, res, wiped.ID)
		assert.Equal(t, WorktreeRecreated, e.WorktreeStatus)
		require.NotNil(t, e.Worktree)
		assert.Equal(t, wipedSHA, e.Worktree.Head, "the committed work survived on the branch")
		assert.FileExists(t, filepath.Join(e.Worktree.Path, "work", "fx-p1.txt"))
		assert.Equal(t, domain.BeadStateInProgress, e.State)
		assert.False(t, e.Changed())
		assert.Contains(t, e.Reason, "recreated")

		// Claimed but never created: a worktree is created from main.
		e = cuResumeEntry(t, res, fresh.ID)
		assert.Equal(t, WorktreeRecreated, e.WorktreeStatus)
		assert.True(t, e.BranchExists)
		assert.Equal(t, mainHead, e.Head)
		assert.False(t, e.Changed())

		// Mid-rebase: reported, not touched.
		e = cuResumeEntry(t, res, conflicted.ID)
		assert.Equal(t, WorktreeAttached, e.WorktreeStatus)
		assert.True(t, e.Conflict)
		assert.Equal(t, []string{"README.md"}, e.ConflictFiles)
		assert.True(t, e.Dirty)
		assert.False(t, e.Changed())
		assert.Contains(t, e.Reason, "conflicts in README.md")

		// Uncommitted edits: reported, kept.
		e = cuResumeEntry(t, res, dirty.ID)
		assert.True(t, e.Dirty)
		assert.False(t, e.Conflict)
		assert.False(t, e.Changed())
		assert.Equal(t, "unchanged", e.Reason)
		assert.FileExists(t, filepath.Join(m2.PathFor(dirty.ID), "edit.txt"))

		// Closed: nothing to do, the worktree waits for Cleanup.
		e = cuResumeEntry(t, res, done.ID)
		assert.Equal(t, WorktreeAttached, e.WorktreeStatus)
		assert.Equal(t, domain.BeadStateClosed, e.State)
		assert.Equal(t, "already closed", e.Reason)

		// Failed without a worktree: not recreated.
		e = cuResumeEntry(t, res, failed.ID)
		assert.Equal(t, WorktreeMissing, e.WorktreeStatus)
		assert.False(t, e.BranchExists)
		assert.False(t, e.Changed())
		assert.NoDirExists(t, m2.PathFor(failed.ID))

		// The orphan is reported and left alone.
		require.Len(t, res.Orphans, 1)
		assert.Equal(t, orphan.ID, res.Orphans[0].BeadID)
		assert.DirExists(t, m2.PathFor(orphan.ID))

		changed := res.Changed()
		require.Len(t, changed, 2)
		assert.Equal(t, landed.ID, changed[0].Bead.ID)
		assert.Equal(t, branchGone.ID, changed[1].Bead.ID)
		assert.Equal(t, []string{"update fx-d1", "close fx-d1", "status fx-d3 needs_human"}, tracker.calls)

		summary := res.Summary()
		assert.Contains(t, summary, "fx-d1: delivering -> closed [worktree attached]")
		assert.Contains(t, summary, "fx-d3: delivering -> needs_human [worktree missing]")
		assert.Contains(t, summary, "fx-r1: in_review [worktree attached, conflict]")
		assert.Contains(t, summary, "fx-r2: changes_requested [worktree attached, dirty]")
		assert.Contains(t, summary, "fx-p1: in_progress [worktree recreated]")

		// Main is untouched by the resume and the repository stays clean.
		assert.NotEqual(t, mainHead, landedSHA)
		assert.Equal(t, "main", f.r.Git(t, "symbolic-ref", "--short", "HEAD"))
		assert.Empty(t, f.r.Status(t))

		// Resuming again finds everything attached and nothing to change.
		tracker2 := &cuTracker{}
		res2, err := cuResumer(t, f.wtReopen(t), tracker2).Resume(ctx, []domain.Bead{undelivered, wiped, fresh, conflicted})
		require.NoError(t, err)
		assert.Empty(t, res2.Changed())
		assert.Empty(t, tracker2.calls)
		for _, e := range res2.Entries {
			assert.Equal(t, WorktreeAttached, e.WorktreeStatus, e.Bead.ID)
		}
	})

	t.Run("a retitled bead whose worktree vanished keeps its work", func(t *testing.T) {
		f := wtSetup(t, nil)
		// A worktree of the user's own, also wiped: recreating the bead's
		// must not drop its registration (D-23).
		theirs := f.wtUserWorktree(t)
		require.NoError(t, os.RemoveAll(theirs))
		bead := cuBead("fx-p9", "the title it was created with", domain.BeadStateInProgress)
		sha := cuWork(t, f, bead)
		branch := BranchName(bead)
		require.NoError(t, os.RemoveAll(f.m.PathFor(bead.ID)))
		// The PO retitled the bead while the run was down.
		bead.Title = "a much better title"
		require.NotEqual(t, branch, BranchName(bead))
		mainHead := f.r.Head(t)

		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{bead})
		require.NoError(t, err)
		require.NoError(t, res.Err(), res.Summary())
		e := cuResumeEntry(t, res, bead.ID)
		assert.Equal(t, branch, e.Branch, "the branch git recorded for the worktree, not the one the new title names")
		assert.True(t, e.BranchExists)
		assert.Equal(t, sha, e.Head)
		assert.Equal(t, 1, e.Ahead)
		assert.Equal(t, WorktreeRecreated, e.WorktreeStatus)
		require.NotNil(t, e.Worktree)
		assert.Equal(t, branch, e.Worktree.Branch, "the worktree is back on the branch holding the work")
		assert.Equal(t, sha, e.Worktree.Head)
		assert.FileExists(t, filepath.Join(e.Worktree.Path, "work", "fx-p9.txt"), "the committed work is not orphaned")
		assert.Contains(t, e.Reason, "recreated from branch "+branch)
		assert.False(t, e.Changed())
		ok, err := f.repo.BranchExists(ctx, BranchName(bead))
		require.NoError(t, err)
		assert.False(t, ok, "no fresh branch is cut from the default branch")
		assert.Equal(t, mainHead, f.r.Head(t))
		assert.Contains(t, f.r.Git(t, "worktree", "list", "--porcelain"), "users-own",
			"a resume never prunes worktrees the user keeps outside the run")
	})

	t.Run("a retitled bead whose registration is gone is found in its own namespace", func(t *testing.T) {
		f := wtSetup(t, nil)
		bead := cuBead("fx-p9", "the title it was created with", domain.BeadStateInProgress)
		sha := cuWork(t, f, bead)
		branch := BranchName(bead)
		// Worktree and registration both gone (a partial cleanup), branch kept.
		require.NoError(t, f.m.Remove(ctx, bead.ID, true))
		bead.Title = "a much better title"

		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{bead})
		require.NoError(t, err)
		require.NoError(t, res.Err(), res.Summary())
		e := cuResumeEntry(t, res, bead.ID)
		assert.Equal(t, branch, e.Branch, "the single branch in the bead's sc/<id> namespace")
		assert.Equal(t, sha, e.Head)
		assert.Equal(t, WorktreeRecreated, e.WorktreeStatus)
		require.NotNil(t, e.Worktree)
		assert.Equal(t, sha, e.Worktree.Head)
	})

	t.Run("a worktree parked on a foreign branch is never recreated from the default branch", func(t *testing.T) {
		f := wtSetup(t, nil)
		bead := cuBead("fx-p1", "in flight", domain.BeadStateInProgress)
		cuWork(t, f, bead)
		wtRepo, err := f.m.Open(ctx, bead.ID)
		require.NoError(t, err)
		_, err = wtRepo.Git(ctx, "checkout", "--quiet", "-b", "feature/theirs")
		require.NoError(t, err)
		require.NoError(t, os.RemoveAll(f.m.PathFor(bead.ID)))

		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{bead})
		require.NoError(t, err)
		e := cuResumeEntry(t, res, bead.ID)
		assert.Equal(t, "feature/theirs", e.Branch)
		assert.Equal(t, WorktreeMissing, e.WorktreeStatus, "recreating would leave the work on that branch behind")
		assert.ErrorContains(t, e.Err, "is not one of fx-p1's own")
		assert.ErrorIs(t, e.Err, ErrBranchInUse)
		assert.False(t, e.Changed())
		assert.Contains(t, e.Reason, "could not be inspected")
	})

	t.Run("git failures while inspecting are reported per bead", func(t *testing.T) {
		bead := cuBead("fx-p1", "in flight", domain.BeadStateInProgress)
		cases := map[string]struct {
			spec string
			want string
		}{
			"the branch lookup fails":    {"show-ref:" + BranchName(bead), "unable to read the ref store"},
			"the namespace search fails": {"for-each-ref", "unable to read the ref store"},
			"resolving the tip fails":    {"rev-parse:" + BranchName(bead), "unable to read the object store"},
			"the ancestor check fails":   {"is-ancestor", "unable to walk the commit graph"},
			"the count is not a number":  {"rev-list", "unexpected rev-list output"},
			"the count command fails":    {"rev-list-fail", "unable to walk the commit graph"},
			"the worktree list fails":    {"worktree-list:2", "unable to read the worktree list"},
			"opening the worktree fails": {"toplevel", "unable to read the repository"},
			"the status check fails":     {"status", "unable to read the index"},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				f := wtSetup(t, nil)
				git := cuGit(t)
				cuWork(t, f, bead)
				b := bead
				if tc.spec == "for-each-ref" {
					// The namespace is only searched once the worktree is
					// gone and the bead's title stops naming its branch.
					require.NoError(t, f.m.Remove(ctx, b.ID, true))
					b.Title = "a much better title"
					require.NotEqual(t, BranchName(bead), BranchName(b))
				}
				m2 := f.wtReopen(t)
				cuFail(t, git, tc.spec)
				res, err := cuResumer(t, m2, &cuTracker{}).Resume(ctx, []domain.Bead{b})
				require.NoError(t, err, "a per-bead git failure must not abort the resume")
				e := cuResumeEntry(t, res, bead.ID)
				assert.ErrorContains(t, e.Err, tc.want)
				assert.False(t, e.Changed(), "no state is invented from half the facts")
				assert.False(t, e.Applied)
				assert.Contains(t, e.Reason, "could not be inspected")
				assert.ErrorContains(t, res.Err(), string(bead.ID))
			})
		}
	})

	t.Run("a conflict list that cannot be read is reported per bead", func(t *testing.T) {
		f := wtSetup(t, nil)
		git := cuGit(t)
		bead := cuBead("fx-r1", "mid-rebase", domain.BeadStateInReview)
		cuWork(t, f, bead)
		cuConflict(t, f, bead)
		m2 := f.wtReopen(t)
		cuFail(t, git, "diff")

		res, err := cuResumer(t, m2, &cuTracker{}).Resume(ctx, []domain.Bead{bead})
		require.NoError(t, err)
		e := cuResumeEntry(t, res, bead.ID)
		assert.ErrorContains(t, e.Err, "unable to read the index")
		assert.False(t, e.Conflict, "an unreadable index is not the same as no conflict")
		assert.False(t, e.Changed())
	})

	t.Run("an active bead without a branch keeps its worktree missing", func(t *testing.T) {
		f := wtSetup(t, nil)
		// Under review, but the branch was never created and the state says
		// nothing may be cut from the default branch on its behalf.
		bead := cuBead("fx-r1", "under review, nothing on disk", domain.BeadStateInReview)
		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{bead})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		e := cuResumeEntry(t, res, bead.ID)
		assert.Equal(t, WorktreeMissing, e.WorktreeStatus)
		assert.False(t, e.BranchExists)
		assert.False(t, e.Changed())
		assert.Equal(t, "the worktree is missing", e.Reason)
		assert.NoDirExists(t, f.m.PathFor(bead.ID))
	})

	t.Run("a worktree that cannot be read is reported per bead", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can read anything")
		}
		f := wtSetup(t, nil)
		bead := cuBead("fx-p1", "in flight", domain.BeadStateInProgress)
		cuWork(t, f, bead)
		path := f.m.PathFor(bead.ID)
		require.NoError(t, os.Chmod(path, 0o000))
		t.Cleanup(func() { _ = os.Chmod(path, 0o755) })

		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{bead})
		require.NoError(t, err)
		e := cuResumeEntry(t, res, bead.ID)
		assert.Error(t, e.Err)
		assert.False(t, e.Changed())
		assert.False(t, e.Applied)
	})

	t.Run("without a tracker it is a dry run", func(t *testing.T) {
		f := wtSetup(t, nil)
		landed := cuBead("fx-d1", "delivered", domain.BeadStateDelivering)
		cuWork(t, f, landed)
		cuLand(t, f, landed)
		res, err := cuResumer(t, f.wtReopen(t), nil).Resume(ctx, []domain.Bead{landed})
		require.NoError(t, err)
		e := cuResumeEntry(t, res, landed.ID)
		assert.Equal(t, domain.BeadStateClosed, e.State)
		assert.True(t, e.Changed())
		assert.False(t, e.Applied)
		assert.Contains(t, res.Summary(), "-> closed (not applied)")
	})

	// P4-13: the crash window this covers is the one P9-03 owns — a bead
	// moved to delivering before any git work, killed before its first
	// commit. Its branch is empty, and an empty branch is trivially an
	// ancestor of the default branch, so "landed" ancestry alone would
	// close the bead with the default branch's own tip as sc_merge_sha and
	// unblock its dependents on work that does not exist (D-20). The
	// uninterrupted path refuses the same bead with ErrNothingToDeliver, so
	// a crash must not make the outcome better than no crash.
	t.Run("a delivering bead whose branch never received a commit is not closed", func(t *testing.T) {
		f := wtSetup(t, nil)
		empty := cuBead("fx-e1", "delivering, killed before the first commit", domain.BeadStateDelivering)
		landed := cuBead("fx-e2", "delivered before the crash", domain.BeadStateDelivering)
		_, err := f.m.Create(ctx, empty)
		require.NoError(t, err)
		landedSHA := cuWork(t, f, landed)
		cuLand(t, f, landed)

		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{empty, landed})
		require.NoError(t, err)
		require.NoError(t, res.Err(), res.Summary())

		e := cuResumeEntry(t, res, empty.ID)
		require.True(t, e.BranchExists, "the branch is there, it is only empty")
		assert.Zero(t, e.Ahead, "with nothing on it that the default branch has not")
		assert.False(t, e.Landed, "an empty branch landed nothing: %s", e.Reason)
		assert.Equal(t, domain.BeadStateDelivering, e.State, "so the bead is left where the crash left it")
		assert.False(t, e.Changed())
		assert.False(t, e.Applied)
		assert.Empty(t, e.MergeSHA)
		assert.Contains(t, e.Reason, "never received a commit")
		assert.Contains(t, e.Reason, "deliver again")
		assert.NotContains(t, tracker.closed, empty.ID, "nothing was closed")
		assert.NotContains(t, tracker.meta, empty.ID, "and no sc_merge_sha was recorded")

		// The same pass still closes a bead that genuinely fast-forwarded:
		// the guard reads the branch's reflog, not the ancestry both share.
		e = cuResumeEntry(t, res, landed.ID)
		assert.True(t, e.Landed)
		assert.Equal(t, domain.BeadStateClosed, e.State)
		assert.True(t, e.Applied)
		assert.Equal(t, landedSHA, e.MergeSHA)
	})

	// P4-16: the reflog rule of P4-13 catches a branch that was cut and
	// never committed to, but a rebase can also drop every commit a branch
	// carried — because the same change had already landed upstream, or
	// because the conflict was resolved in upstream's favour — which leaves
	// the branch sitting on an upstream commit with a moved ref. That
	// branch is an ancestor of the default branch and holds nothing of the
	// bead's, so a crash in that window must not close it either.
	t.Run("a delivering bead whose commits were rebased away is not closed", func(t *testing.T) {
		f := wtSetup(t, nil)
		dropped := cuBead("fx-r9", "delivering, rebased to nothing", domain.BeadStateDelivering)
		landed := cuBead("fx-r8", "delivered before the crash", domain.BeadStateDelivering)
		_, err := f.m.Create(ctx, dropped)
		require.NoError(t, err)
		wtRepo, err := f.m.Open(ctx, dropped.ID)
		require.NoError(t, err)
		acWrite(t, wtRepo.Root, "work/shared.txt", "the very same change\n")
		c, err := wtRepo.CommitAll(ctx, "work for fx-r9")
		require.NoError(t, err)
		require.True(t, c.Committed)

		// Somebody else lands the identical change on main, so the rebase
		// drops the bead's only commit.
		f.r.WriteFile(t, "work/shared.txt", "the very same change\n")
		mainTip := f.r.Commit(t, "the same change, upstream")
		_, err = wtRepo.Git(ctx, "-c", "commit.gpgsign=false", "rebase", "--quiet", "main")
		require.NoError(t, err)
		require.Equal(t, mainTip, f.r.Git(t, "rev-parse", "refs/heads/"+BranchName(dropped)),
			"the branch now sits on the upstream commit")

		landedSHA := cuWork(t, f, landed)
		cuLand(t, f, landed)

		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{dropped, landed})
		require.NoError(t, err)
		require.NoError(t, res.Err(), res.Summary())

		e := cuResumeEntry(t, res, dropped.ID)
		require.True(t, e.BranchExists)
		assert.Zero(t, e.Ahead)
		assert.False(t, e.Landed, "a branch rebased to nothing landed nothing: %s", e.Reason)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.False(t, e.Changed())
		assert.False(t, e.Applied)
		assert.Empty(t, e.MergeSHA)
		assert.Contains(t, e.Reason, "rebased away")
		assert.Contains(t, e.Reason, "deliver again")
		assert.NotContains(t, tracker.closed, dropped.ID)
		assert.NotContains(t, tracker.meta, dropped.ID)

		// And the same pass still closes a genuine fast-forward.
		e = cuResumeEntry(t, res, landed.ID)
		assert.True(t, e.Landed)
		assert.Equal(t, domain.BeadStateClosed, e.State)
		assert.True(t, e.Applied)
		assert.Equal(t, landedSHA, e.MergeSHA)
	})

	t.Run("a gate still blocking the close leaves the bead to the gate poller", func(t *testing.T) {
		f := wtSetup(t, nil)
		landed := cuBead("fx-d1", "merged by hand with a PR gate", domain.BeadStateDelivering)
		cuWork(t, f, landed)
		cuLand(t, f, landed)
		tracker := &cuTracker{closeErr: fmt.Errorf("beads: close: %w", beads.ErrBlockedOpen)}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{landed})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		e := cuResumeEntry(t, res, landed.ID)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.False(t, e.Changed())
		assert.False(t, e.Applied)
		assert.Empty(t, e.MergeSHA)
		assert.Contains(t, e.Reason, "gate poller")
	})

	t.Run("tracker failures are reported per bead", func(t *testing.T) {
		f := wtSetup(t, nil)
		landed := cuBead("fx-d1", "delivered", domain.BeadStateDelivering)
		cuWork(t, f, landed)
		cuLand(t, f, landed)
		tracker := &cuTracker{err: errors.New("bd is down")}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{landed})
		require.NoError(t, err)
		e := cuResumeEntry(t, res, landed.ID)
		assert.Equal(t, domain.BeadStateClosed, e.State)
		assert.False(t, e.Applied)
		assert.ErrorContains(t, e.Err, "bd is down")
		assert.ErrorContains(t, res.Err(), "fx-d1")
		assert.Contains(t, res.Summary(), "(error: applying closed: bd is down)")
	})

	// P4-15: a human's checkout is normally mid-edit, and Manager.Create
	// refuses a dirty repository, so a crash plus a removed worktree used
	// to leave the bead unrecoverable until the user committed or stashed.
	// Adding a linked worktree elsewhere has nothing to do with the state
	// of their working tree, so the resume recreates it regardless — and
	// leaves their work exactly where it was.
	t.Run("a wiped worktree is recreated while the user has uncommitted work", func(t *testing.T) {
		f := wtSetup(t, nil)
		wiped := cuBead("fx-p1", "wiped", domain.BeadStateInProgress)
		sha := cuWork(t, f, wiped)
		require.NoError(t, os.RemoveAll(f.m.PathFor(wiped.ID)))
		f.r.WriteFile(t, "notes.txt", "the user is editing\n")
		f.r.Git(t, "add", "notes.txt")
		f.r.WriteFile(t, "README.md", "and has staged something else\n")

		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{wiped})
		require.NoError(t, err)
		require.NoError(t, res.Err(), res.Summary())
		e := cuResumeEntry(t, res, wiped.ID)
		assert.Equal(t, WorktreeRecreated, e.WorktreeStatus)
		require.NotNil(t, e.Worktree)
		assert.Equal(t, BranchName(wiped), e.Worktree.Branch)
		assert.Equal(t, sha, e.Worktree.Head, "the bead's own commits are back")
		assert.Contains(t, e.Reason, "recreated")
		assert.False(t, e.Changed())

		assert.FileExists(t, filepath.Join(f.r.Dir, "notes.txt"), "the user's own work is untouched")
		assert.Contains(t, f.r.Status(t), "notes.txt")
		assert.Equal(t, "and has staged something else\n", cuRead(t, f.r.Dir, "README.md"))
	})

	t.Run("recreation failures are reported per bead", func(t *testing.T) {
		f := wtSetup(t, nil)
		wiped := cuBead("fx-p1", "wiped", domain.BeadStateInProgress)
		cuWork(t, f, wiped)
		path := f.m.PathFor(wiped.ID)
		require.NoError(t, os.RemoveAll(path))
		// Something else took the path in the meantime.
		require.NoError(t, os.WriteFile(path, []byte("not a worktree\n"), 0o644))
		t.Cleanup(func() { _ = os.Remove(path) })

		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{wiped})
		require.NoError(t, err)
		e := cuResumeEntry(t, res, wiped.ID)
		assert.Equal(t, WorktreeMissing, e.WorktreeStatus)
		require.Error(t, e.Err)
		assert.Contains(t, e.Err.Error(), "recreating the worktree")
		assert.False(t, e.Changed())
		assert.Contains(t, e.Reason, "could not be inspected")
		assert.ErrorContains(t, res.Err(), string(wiped.ID))
	})

	t.Run("a bead without an id is an entry error", func(t *testing.T) {
		f := wtSetup(t, nil)
		res, err := cuResumer(t, f.m, &cuTracker{}).Resume(ctx, []domain.Bead{{Title: "no id"}})
		require.NoError(t, err)
		require.Len(t, res.Entries, 1)
		assert.Error(t, res.Entries[0].Err)
		assert.Error(t, res.Err())
	})

	t.Run("no default branch", func(t *testing.T) {
		f := wtSetup(t, nil)
		f.r.Git(t, "checkout", "-q", "-b", "trunk")
		f.r.Git(t, "branch", "-D", "-q", "main")
		_, err := cuResumer(t, f.m, nil).Resume(ctx, nil)
		require.ErrorIs(t, err, ErrNoDefaultBranch)
	})
}

// TestResumer_Resume_ClearsAStaleIndexLock covers the trap a killed run
// sets for the next one: git holds .git/index.lock for the whole of an add,
// a commit or a rebase step and never clears it after a SIGKILL, so without
// this every command in that worktree fails for ever with "Another git
// process seems to be running in this repository" — and the run that
// resumes the bead re-fires its engineer, fails to commit, and spins.
func TestResumer_Resume_ClearsAStaleIndexLock(t *testing.T) {
	ctx := context.Background()
	f := wtSetup(t, nil)
	killed := cuBead("fx-k1", "killed mid-commit", domain.BeadStateInProgress)
	cuWork(t, f, killed)

	wtRepo, err := f.m.Open(ctx, killed.ID)
	require.NoError(t, err)
	gitDir, err := wtRepo.Git(ctx, "rev-parse", "--absolute-git-dir")
	require.NoError(t, err)
	lock := filepath.Join(strings.TrimSpace(gitDir), "index.lock")
	require.NoError(t, os.WriteFile(lock, []byte("<pid of the process that died>\n"), 0o600))

	// git really is stuck until the lock goes.
	acWrite(t, f.m.PathFor(killed.ID), "work/more.txt", "written after the kill\n")
	_, err = wtRepo.CommitAll(ctx, "this cannot work")
	require.Error(t, err, "git refuses to touch the index while the lock is there")

	r, err := NewResumer(f.m, nil, WithoutGH())
	require.NoError(t, err)
	res, err := r.Resume(ctx, []domain.Bead{killed})
	require.NoError(t, err)
	assert.NoFileExists(t, lock, "the resume cleared the stale lock")
	assert.Contains(t, strings.Join(res.Warnings, "\n"), "cleared a stale git index lock in fx-k1's worktree")

	c, err := wtRepo.CommitAll(ctx, "and now it works again")
	require.NoError(t, err)
	assert.True(t, c.Committed, "the bead can be worked on again")

	// A worktree with no lock is left alone and says nothing.
	clean := cuBead("fx-k2", "no lock", domain.BeadStateInProgress)
	cuWork(t, f, clean)
	res, err = r.Resume(ctx, []domain.Bead{clean})
	require.NoError(t, err)
	assert.NotContains(t, strings.Join(res.Warnings, "\n"), "stale git index lock")
}

func TestResumer_Resume_WithRemote(t *testing.T) {
	ctx := context.Background()

	// cuRemoteSetup builds a fixture with a bare remote, a gh shim, and one
	// delivering bead with a commit on its branch.
	type remoteFixture struct {
		*wtFixture
		gh   *testutil.Shim
		bead domain.Bead
		sha  string
	}
	setup := func(t *testing.T, state domain.BeadState) *remoteFixture {
		t.Helper()
		f := wtSetup(t, []testutil.FixtureOption{testutil.WithBareRemote()})
		bead := cuBead("fx-d1", "delivered with safety on", state)
		sha := cuWork(t, f, bead)
		return &remoteFixture{wtFixture: f, gh: cuGH(t), bead: bead, sha: sha}
	}

	t.Run("landed on the remote default branch only", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		// The push stage reached the remote, the local main was not moved
		// (the delivery ran from the remote-tracking branch) and then the
		// process died before the close.
		f.r.Git(t, "push", "-q", "origin", BranchName(f.bead)+":main")
		localMain := f.r.Head(t)
		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker, WithoutGH()).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		assert.True(t, res.Fetched)
		assert.Equal(t, "origin/main", res.Base)
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.True(t, e.Landed)
		assert.Equal(t, domain.BeadStateClosed, e.State)
		assert.True(t, e.Applied)
		assert.Equal(t, f.sha, e.MergeSHA)
		assert.Equal(t, localMain, f.r.Head(t), "resume never moves the local default branch")
		assert.Empty(t, f.gh.Invocations(t), "WithoutGH skips gh")
	})

	t.Run("open pull request stays delivering", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		dpSet(t, f.gh, "prs.json", `[{"number":5,"url":"https://github.com/acme/widgets/pull/5","state":"OPEN","title":"x"}]`)
		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		e := cuResumeEntry(t, res, f.bead.ID)
		require.NotNil(t, e.PR)
		assert.Equal(t, 5, e.PR.Number)
		assert.Equal(t, "OPEN", e.PRState)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.False(t, e.Changed())
		assert.Contains(t, e.Reason, "PR #5 is open")
		assert.Empty(t, tracker.calls)
		calls := f.gh.Invocations(t)
		require.Len(t, calls, 1)
		assert.Equal(t, []string{"pr", "list", "--head", BranchName(f.bead), "--state", "all", "--limit", "50", "--json", "number,url,state,title"}, calls[0].Args)
	})

	t.Run("merged pull request waits for its gate", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		dpSet(t, f.gh, "prs.json", `[{"number":5,"url":"https://github.com/acme/widgets/pull/5","state":"MERGED","title":"x"}]`)
		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.Equal(t, "MERGED", e.PRState)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.False(t, e.Changed())
		assert.Contains(t, e.Reason, "PR #5 merged")
		assert.Empty(t, tracker.calls, "bd closes the bead through the gate, never directly")
	})

	t.Run("an open pull request outlives the local branch", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		// The branch was pushed and its pull request opened; then the local
		// worktree and branch were cleaned away (a partial cleanup, or a
		// human tidying up) before the crash. The work is on the remote.
		f.r.Git(t, "push", "-q", "origin", BranchName(f.bead))
		require.NoError(t, f.m.Remove(ctx, f.bead.ID, true))
		requireDeleteBranch(t, f.m, f.bead)
		dpSet(t, f.gh, "prs.json", `[{"number":7,"url":"https://github.com/acme/widgets/pull/7","state":"OPEN","title":"x"}]`)
		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.False(t, e.BranchExists)
		assert.Empty(t, e.Head)
		require.NotNil(t, e.PR)
		assert.Equal(t, "OPEN", e.PRState)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.False(t, e.Changed(), "an open pull request is not a case for a human")
		assert.Contains(t, e.Reason, "PR #7 is open")
		assert.Empty(t, tracker.calls)
		assert.Len(t, f.gh.Invocations(t), 1, "gh is asked even without a local branch")

		// With no pull request left, the same bead does need a human.
		dpSet(t, f.gh, "prs.json", `[]`)
		res, err = cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		e = cuResumeEntry(t, res, f.bead.ID)
		assert.Nil(t, e.PR)
		assert.Equal(t, domain.BeadStateNeedsHuman, e.State)
		assert.Contains(t, e.Reason, "is gone and nothing landed on main")
	})

	t.Run("recorded pull request that is gone hands the bead to a human", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		f.bead.PRURL, f.bead.PRNumber = "https://github.com/acme/widgets/pull/5", 5
		dpSet(t, f.gh, "pr-5.json", `{"number":5,"url":"https://github.com/acme/widgets/pull/5","state":"CLOSED","title":"x","headRefName":"`+BranchName(f.bead)+`"}`)
		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.Nil(t, e.PR)
		assert.Equal(t, domain.BeadStateNeedsHuman, e.State)
		assert.True(t, e.Applied)
		assert.Contains(t, e.Reason, "no longer open or merged")
		assert.Equal(t, domain.BeadStateNeedsHuman, tracker.statuses[f.bead.ID])

		// The same for a bead that was still under review when its PR
		// (opened by a retry) was closed.
		f.bead.State = domain.BeadStateInReview
		res, err = cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		assert.Equal(t, domain.BeadStateNeedsHuman, cuResumeEntry(t, res, f.bead.ID).State)
	})

	t.Run("a recorded pull request is looked up by number, not by branch", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		branch := BranchName(f.bead)
		f.bead.PRURL, f.bead.PRNumber = "https://github.com/acme/widgets/pull/9", 9
		// The branch was pushed, the local worktree and branch cleaned away,
		// and the PO retitled the bead: nothing local names the pull
		// request's head branch any more.
		f.r.Git(t, "push", "-q", "origin", branch)
		require.NoError(t, f.m.Remove(ctx, f.bead.ID, true))
		requireDeleteBranch(t, f.m, f.bead)
		f.bead.Title = "a much better title"
		require.NotEqual(t, branch, BranchName(f.bead))
		dpSet(t, f.gh, "prs.json", `[]`)
		dpSet(t, f.gh, "pr-9.json", `{"number":9,"url":"https://github.com/acme/widgets/pull/9","state":"OPEN","title":"x","headRefName":"`+branch+`"}`)

		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.NoError(t, res.Err(), res.Summary())
		e := cuResumeEntry(t, res, f.bead.ID)
		require.NotNil(t, e.PR)
		assert.Equal(t, 9, e.PR.Number)
		assert.Equal(t, branch, e.PR.HeadRef)
		assert.Equal(t, "OPEN", e.PRState)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.False(t, e.Changed(), "a delivering bead behind an open PR is never pushed to needs_human")
		assert.Empty(t, tracker.calls)
		calls := f.gh.Invocations(t)
		require.Len(t, calls, 1)
		assert.Equal(t, []string{"pr", "view", "9", "--json", "number,url,state,title,headRefName"}, calls[0].Args,
			"a number survives a retitle; a guessed head branch does not")
	})

	t.Run("a pull request url without a number is still queried by number", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		f.bead.PRURL = "https://github.com/acme/widgets/pull/9"
		dpSet(t, f.gh, "pr-9.json", `{"number":9,"url":"https://github.com/acme/widgets/pull/9","state":"OPEN","title":"x","headRefName":"`+BranchName(f.bead)+`"}`)
		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.Equal(t, "OPEN", e.PRState)
		calls := f.gh.Invocations(t)
		require.Len(t, calls, 1)
		assert.Equal(t, "view", calls[0].Args[1])
	})

	t.Run("a merged pull request found by number waits for its gate", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		f.bead.PRNumber = 9
		dpSet(t, f.gh, "pr-9.json", `{"number":9,"url":"https://github.com/acme/widgets/pull/9","state":"MERGED","title":"x","headRefName":"`+BranchName(f.bead)+`"}`)
		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.Equal(t, "MERGED", e.PRState)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.Contains(t, e.Reason, "PR #9 merged")
		assert.Empty(t, tracker.calls)
	})

	// P4-10: a PRPushOnly or PRNoRemote delivery pushes (or does not) and
	// hands the bead to a human, recording no pull request. The human opens
	// one; the resume has to notice and take the bead back into delivery,
	// with the pull request written to its metadata so the re-run delivery
	// can gate it (D-20).
	t.Run("a hand-opened pull request takes a push-only bead back into delivery", func(t *testing.T) {
		f := setup(t, domain.BeadStateNeedsHuman)
		f.r.Git(t, "push", "-q", "origin", BranchName(f.bead))
		dpSet(t, f.gh, "prs.json", `[{"number":11,"url":"https://github.com/acme/widgets/pull/11","state":"OPEN","title":"x"}]`)
		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.NoError(t, res.Err(), res.Summary())

		e := cuResumeEntry(t, res, f.bead.ID)
		require.NotNil(t, e.PR)
		assert.Equal(t, 11, e.PR.Number)
		assert.Equal(t, "OPEN", e.PRState)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.True(t, e.Changed())
		assert.True(t, e.Applied)
		assert.Contains(t, e.Reason, "PR #11 was opened by hand")
		assert.Contains(t, e.Reason, "gate")

		// The pull request is on the bead before its status moves, so the
		// re-run delivery finds it and creates the gate.
		assert.Equal(t, map[string]any{
			beads.MetaPRURL:    "https://github.com/acme/widgets/pull/11",
			beads.MetaPRNumber: 11,
		}, tracker.meta[f.bead.ID])
		assert.Equal(t, []string{
			"update " + string(f.bead.ID),
			"status " + string(f.bead.ID) + " delivering",
		}, tracker.calls, "the metadata is written first")
		assert.Equal(t, domain.BeadStateDelivering, tracker.statuses[f.bead.ID])
	})

	t.Run("a merged pull request does the same for a no-remote bead", func(t *testing.T) {
		// PRNoRemote left the branch local; the human added the remote,
		// pushed it, opened the pull request and merged it themselves.
		f := setup(t, domain.BeadStateNeedsHuman)
		f.r.Git(t, "push", "-q", "origin", BranchName(f.bead))
		dpSet(t, f.gh, "prs.json", `[{"number":12,"url":"https://github.com/acme/widgets/pull/12","state":"MERGED","title":"x"}]`)
		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.NoError(t, res.Err(), res.Summary())
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.Equal(t, "MERGED", e.PRState)
		assert.Equal(t, domain.BeadStateDelivering, e.State, "the gate poller closes it once the gate exists")
		assert.True(t, e.Applied)
		assert.Equal(t, 12, tracker.meta[f.bead.ID][beads.MetaPRNumber])
	})

	t.Run("a needs_human bead with no pull request is left alone", func(t *testing.T) {
		f := setup(t, domain.BeadStateNeedsHuman)
		dpSet(t, f.gh, "prs.json", `[]`)
		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.Nil(t, e.PR)
		assert.Equal(t, domain.BeadStateNeedsHuman, e.State)
		assert.False(t, e.Changed())
		assert.Empty(t, tracker.calls)
		assert.Len(t, f.gh.Invocations(t), 1, "the branch was asked about once")
	})

	t.Run("a needs_human bead without a branch is never asked about", func(t *testing.T) {
		f := wtSetup(t, []testutil.FixtureOption{testutil.WithBareRemote()})
		gh := cuGH(t)
		bead := cuBead("fx-h9", "never implemented", domain.BeadStateNeedsHuman)
		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{bead})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		assert.False(t, cuResumeEntry(t, res, bead.ID).Changed())
		assert.Empty(t, gh.Invocations(t), "no branch, nothing to look a pull request up by")
	})

	t.Run("an open pull request does not pull a bead out of its review loop", func(t *testing.T) {
		f := setup(t, domain.BeadStateChangesRequested)
		f.bead.PRURL, f.bead.PRNumber = "https://github.com/acme/widgets/pull/5", 5
		dpSet(t, f.gh, "pr-5.json", `{"number":5,"url":"https://github.com/acme/widgets/pull/5","state":"OPEN","title":"x","headRefName":"`+BranchName(f.bead)+`"}`)
		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.Equal(t, "OPEN", e.PRState)
		assert.Equal(t, domain.BeadStateChangesRequested, e.State)
		assert.False(t, e.Changed(), "a stale pull request never yanks a bead into delivery")
		assert.Contains(t, e.Reason, "the bead stays changes_requested")
		assert.Empty(t, tracker.calls)
	})

	t.Run("a gh failure that is not a missing tool is an entry error", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		f.bead.PRURL, f.bead.PRNumber = "https://github.com/acme/widgets/pull/5", 5
		dpSet(t, f.gh, "mode", "network")
		tracker := &cuTracker{}
		res, err := cuResumer(t, f.wtReopen(t), tracker).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.ErrorContains(t, e.Err, "502")
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.False(t, e.Changed(), "an unreachable forge is not evidence that the PR is gone")
		assert.False(t, e.Applied)
		assert.Empty(t, res.Warnings, "only a missing or unusable gh is downgraded to a warning")
		assert.Empty(t, tracker.calls)
		assert.ErrorContains(t, res.Err(), "fx-d1")
	})

	t.Run("beads without a pull request never ask gh", func(t *testing.T) {
		f := setup(t, domain.BeadStateInReview)
		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		assert.False(t, cuResumeEntry(t, res, f.bead.ID).Changed())
		assert.Empty(t, f.gh.Invocations(t))
	})

	t.Run("gh missing is a warning, not an error", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		dpHideGH(t)
		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.NoError(t, res.Err())
		require.Len(t, res.Warnings, 1)
		assert.Contains(t, res.Warnings[0], "gh is not installed")
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.Nil(t, e.PR)
		assert.Equal(t, domain.BeadStateDelivering, e.State)
		assert.Contains(t, e.Reason, "deliver again")
		assert.Contains(t, res.Summary(), "warning: pull requests were not checked: gh is not installed")
	})

	t.Run("unauthenticated gh is a warning too", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		dpSet(t, f.gh, "mode", "noauth")
		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		require.Len(t, res.Warnings, 1)
		assert.Contains(t, res.Warnings[0], "not authenticated")
	})

	t.Run("a fetch failure is a warning and local state is used", func(t *testing.T) {
		f := setup(t, domain.BeadStateDelivering)
		cuLand(t, f.wtFixture, f.bead)
		require.NoError(t, os.RemoveAll(f.r.RemoteDir))
		res, err := cuResumer(t, f.wtReopen(t), &cuTracker{}, WithoutGH()).Resume(ctx, []domain.Bead{f.bead})
		require.NoError(t, err)
		assert.False(t, res.Fetched)
		require.Len(t, res.Warnings, 1)
		assert.Contains(t, res.Warnings[0], "fetch from origin failed")
		assert.Equal(t, "origin/main", res.Base, "the stale remote-tracking branch is still checked")
		e := cuResumeEntry(t, res, f.bead.ID)
		assert.True(t, e.Landed, "landed on the local main")
		assert.Equal(t, domain.BeadStateClosed, e.State)
	})
}
